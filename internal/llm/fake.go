package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// FakeResponse is one scripted completion. Status non-zero simulates an
// error response (retry scripts); Chunks drives SSE mode; Usage is optional
// — when nil the client estimates.
type FakeResponse struct {
	Content string
	Chunks  []string
	Usage   *Usage
	Status  int
}

// FakeServer is an in-process OpenAI-compatible endpoint for tests: it
// records every request body and header and plays back the script in order
// (the last entry repeats).
type FakeServer struct {
	URL      string
	Requests []map[string]any
	Headers  []http.Header

	mu     sync.Mutex
	script []FakeResponse
	i      int
	srv    *httptest.Server
}

// NewFakeServer starts the scripted endpoint. Call Close when done.
func NewFakeServer(script ...FakeResponse) *FakeServer {
	s := &FakeServer{script: script}
	if len(s.script) == 0 {
		s.script = []FakeResponse{{Content: "ok"}}
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	return s
}

// Close shuts the server down.
func (s *FakeServer) Close() { s.srv.Close() }

// RequestCount returns how many requests the fake has served.
func (s *FakeServer) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

// Reset replaces the playback script and rewinds it (request history kept).
func (s *FakeServer) Reset(script ...FakeResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = script
	s.i = 0
}

func (s *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	resp := s.script[min(s.i, len(s.script)-1)]
	s.i++
	s.Requests = append(s.Requests, body)
	s.Headers = append(s.Headers, r.Header.Clone())
	s.mu.Unlock()

	if resp.Status != 0 {
		http.Error(w, "scripted failure", resp.Status)
		return
	}

	stream, _ := body["stream"].(bool)
	w.Header().Set("Content-Type", "application/json")
	if stream && len(resp.Chunks) > 0 {
		flusher := w.(http.Flusher)
		for _, chunk := range resp.Chunks {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
				"choices": []any{map[string]any{
					"delta":         map[string]any{"content": chunk},
					"finish_reason": nil,
				}},
			}))
			flusher.Flush()
		}
		if resp.Usage != nil {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
				"choices": []any{},
				"usage": map[string]any{
					"prompt_tokens":     resp.Usage.PromptTokens,
					"completion_tokens": resp.Usage.CompletionTokens,
					"total_tokens":      resp.Usage.TotalTokens,
				},
			}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	out := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": resp.Content},
			"finish_reason": "stop",
		}},
	}
	if resp.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return strings.TrimSpace(string(b))
}
