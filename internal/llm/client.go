// Package llm is the OpenAI-compatible generation client (PRD Phase 1, R1):
// chat completions with SSE streaming, bounded retries, token accounting,
// and run-id-correlated logging. The Client interface is the pipeline seam —
// on-prem vLLM and the OpenRouter dev profile are the same wire protocol; the
// httptest fake (fake.go) stands in for CI.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/telemetry"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is a completion request. The endpoint's defaults are filled by
// the caller (config.Merged); MaxTokens 0 means endpoint default.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int
	Stream      bool
}

// Usage is the token accounting for one call. Estimated marks a derived
// count (chars/4) for endpoints that do not report usage.
type Usage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated,omitempty"`
}

// Response is the assembled completion.
type Response struct {
	Content      string
	FinishReason string
	Usage        Usage
}

// Client is the LLM seam consumed by the pipeline. Stream invokes onDelta
// per token delta and returns the assembled Response.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (Response, error)
	Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Response, error)
}

// Endpoint is the resolved connection parameters for one model profile.
type Endpoint struct {
	ProfileName string
	Model       string
	APIBase     string
	APIKey      string // empty = keyless endpoint; never logged
	Temperature float64
	MaxTokens   int
	Stream      bool
	Timeout     time.Duration
	Retries     int
}

var retryBaseDelay = 500 * time.Millisecond

type openaiClient struct {
	endpoint Endpoint
	http     *http.Client
}

// New builds the OpenAI-compatible client for one endpoint.
func New(endpoint Endpoint) Client {
	c := &openaiClient{endpoint: endpoint}
	if endpoint.Timeout > 0 {
		c.http = &http.Client{Timeout: endpoint.Timeout}
	} else {
		c.http = &http.Client{}
	}
	return c
}

type chatCompletionPayload struct {
	Model         string    `json:"model"`
	Messages      []Message `json:"messages"`
	Temperature   float64   `json:"temperature"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	Stream        bool      `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// Chat performs a non-streaming completion.
func (c *openaiClient) Chat(ctx context.Context, req ChatRequest) (Response, error) {
	req.Stream = false
	body, err := json.Marshal(c.payload(req))
	if err != nil {
		return Response{}, err
	}
	raw, err := c.doWithRetries(ctx, body)
	if err != nil {
		return Response{}, err
	}
	defer raw.Close()

	var wire struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsage `json:"usage"`
	}
	if err := json.NewDecoder(raw).Decode(&wire); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: response carried no choices")
	}
	resp := Response{Content: wire.Choices[0].Message.Content, FinishReason: wire.Choices[0].FinishReason}
	resp.Usage = usageOf(wire.Usage, len(resp.Content))
	c.logCall(ctx, req, resp.Usage)
	return resp, nil
}

// Stream performs an SSE completion, invoking onDelta per content token.
func (c *openaiClient) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Response, error) {
	req.Stream = true
	body, err := json.Marshal(c.payload(req))
	if err != nil {
		return Response{}, err
	}
	raw, err := c.doWithRetries(ctx, body)
	if err != nil {
		return Response{}, err
	}
	defer raw.Close()

	var resp Response
	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
			Usage *wireUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Response{}, fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Usage != nil && chunk.Usage.PromptTokens+chunk.Usage.CompletionTokens > 0 {
			resp.Usage = usageOf(chunk.Usage, len(resp.Content))
		}
		for _, ch := range chunk.Choices {
			if s, ok := chunk.Choices[0].FinishReason.(string); ok && s != "" {
				resp.FinishReason = s
			}
			if ch.Delta.Content == "" {
				continue
			}
			resp.Content += ch.Delta.Content
			if onDelta != nil {
				if err := onDelta(ch.Delta.Content); err != nil {
					return Response{}, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Response{}, fmt.Errorf("llm: stream read: %w", err)
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage = estimateUsage(req, resp.Content)
	}
	c.logCall(ctx, req, resp.Usage)
	return resp, nil
}

func (c *openaiClient) payload(req ChatRequest) chatCompletionPayload {
	model := req.Model
	if model == "" {
		// The endpoint's profile model is the default — callers may leave
		// Model empty to follow the routed config (OpenRouter 400s without it).
		model = c.endpoint.Model
	}
	// Request-level values win; the endpoint's config defaults fill the
	// gaps (run.temperature / models[].requestOptions in .tuxconv.yaml).
	// Stream stays request-owned: Chat pins it false, Stream pins it true.
	temperature := req.Temperature
	if temperature == 0 {
		temperature = c.endpoint.Temperature
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.endpoint.MaxTokens
	}
	p := chatCompletionPayload{
		Model:       model,
		Messages:    req.Messages,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Stream:      req.Stream,
	}
	if req.Stream {
		p.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	return p
}

// doWithRetries posts the payload, retrying transport errors and transient
// statuses with exponential backoff (bounded by the endpoint's retries).
func (c *openaiClient) doWithRetries(ctx context.Context, body []byte) (io.ReadCloser, error) {
	attempts := c.endpoint.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * time.Duration(1<<uint(min(attempt-1, 4)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		raw, retryable, err := c.post(ctx, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *openaiClient) post(ctx context.Context, body []byte) (io.ReadCloser, bool, error) {
	url := strings.TrimRight(c.endpoint.APIBase, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.endpoint.APIKey != "" {
		// Same signature pr-review uses against the on-prem vLLM endpoint:
		// both header shapes together (client.go:132-133 there). OpenRouter
		// reads Bearer; on-prem reads api-key — sending both is harmless and
		// keeps the two tools on one working credential convention.
		req.Header.Set("Authorization", "Bearer "+c.endpoint.APIKey)
		req.Header.Set("api-key", c.endpoint.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("llm: call %s: %w", url, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024))
		return nil, resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusInternalServerError ||
				resp.StatusCode == http.StatusBadGateway ||
				resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusGatewayTimeout,
			fmt.Errorf("llm: endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return resp.Body, false, nil
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageOf(w *wireUsage, contentLen int) Usage {
	if w == nil || w.TotalTokens == 0 {
		return Usage{PromptTokens: 0, CompletionTokens: contentLen / 4, TotalTokens: contentLen / 4, Estimated: true}
	}
	u := Usage{PromptTokens: w.PromptTokens, CompletionTokens: w.CompletionTokens, TotalTokens: w.TotalTokens}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

func estimateUsage(req ChatRequest, content string) Usage {
	promptChars := 0
	for _, m := range req.Messages {
		promptChars += len(m.Content)
	}
	u := Usage{PromptTokens: promptChars / 4, CompletionTokens: len(content) / 4, Estimated: true}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

func (c *openaiClient) logCall(ctx context.Context, req ChatRequest, u Usage) {
	telemetry.Log(ctx).Info("llm call complete",
		"profile", c.endpoint.ProfileName,
		"model", req.Model,
		"stream", req.Stream,
		"prompt_tokens", u.PromptTokens,
		"completion_tokens", u.CompletionTokens,
		"total_tokens", u.TotalTokens,
		"usage_estimated", u.Estimated,
	)
}

// NewFromConfig resolves the run's profile (override when non-empty), its
// merged request options, and its API key (R6: env first, gitignored-file
// literal as fallback) into a ready client. The key value is never logged.
func NewFromConfig(ctx context.Context, cfg *config.Config, override string) (Client, error) {
	m, err := cfg.Route(override)
	if err != nil {
		return nil, err
	}
	key, _, err := m.ResolveKey()
	if err != nil {
		return nil, err
	}
	temperature, maxTokens, stream, timeout, retries := cfg.Merged(m)
	return New(Endpoint{
		ProfileName: m.Name,
		Model:       m.Model,
		APIBase:     m.APIBase,
		APIKey:      key,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Stream:      stream,
		Timeout:     timeout,
		Retries:     retries,
	}), nil
}
