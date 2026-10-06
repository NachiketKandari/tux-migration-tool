package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/telemetry"
)

// SeamInput describes one bounded-retry LLM seam call. RunSeam owns the
// skeleton every LLM seam shares (attempt clamping, budget ceilings, the
// chat call, exchange archiving, gate-feedback accumulation, the retry
// loop); the caller owns its per-seam policy: message assembly, payload
// extraction, gates, and the error posture (AbortOnChatError).
type SeamInput struct {
	// Unit, Kind, Name identify the call in the audit trail; Template and
	// LLM are optional plan-backed extras. Attempt, Prompt, Response,
	// Errors, and Outcome are owned by the runner.
	Unit     string
	Kind     string
	Name     string
	Template string
	LLM      bool

	// File overrides the audit artifact name per attempt; nil keeps the
	// default "<kind>-<name>-attempt<n>.json" convention.
	File func(attempt int) string

	// Audit archives every attempt when set; nil skips archiving.
	Audit *audit.Recorder

	Client      Client
	Budget      budget.Budget
	MaxRetries  int     // additional attempts beyond the first
	Temperature float64 // passed through verbatim (endpoint defaults are the client's wiring)

	// MaxTokens overrides the endpoint's per-request output cap when > 0
	// (0 = endpoint default). Thinking-mode models spend part of the cap on
	// reasoning that never reaches content, so seams that need code room
	// raise it; the Budget.CheckOutput ceiling on the extracted content is
	// unaffected.
	MaxTokens int

	// Prompt builds this attempt's messages. prev is the prior attempt's
	// gated payload ("" on the first attempt, and after chat/over-cap
	// failures where there is nothing to patch); repair-mode callers send
	// it back as an assistant turn. notes carries the accumulated
	// rejection notes from earlier attempts (empty on the first). The
	// first return value is the prompt text archived in the audit trail.
	Prompt func(prev string, notes []string) (archived string, messages []Message)

	// Repair records the caller's retry methodology in the audit trail:
	// true = rejected attempts ride back as an assistant turn and the
	// model patches its own output; false = the model regenerates from
	// the original prompt. RunSeam does not enforce the strategy (Prompt
	// owns message assembly) — the flag exists so tuxconv retrystats can
	// compare A/B audit folders.
	Repair bool

	// Extract turns the raw response content into the seam payload (fence
	// stripping, JSON selection); nil = identity.
	Extract func(content string) string

	// Gate validates the extracted payload; the returned strings are the
	// rejection reasons archived and accumulated into notes for the next
	// attempt. nil = any in-budget response is accepted.
	Gate func(payload string) []string

	// Rejected, when set, receives the extracted payload of every gated
	// attempt. Callers use it as a visible fallback artifact when all
	// attempts fail (a rejected body kept in comments beats an empty
	// method); on success the last call is moot. Never called for chat
	// errors or over-cap responses (no payload exists).
	Rejected func(payload string)

	// AbortOnChatError returns the chat error immediately (after archiving)
	// instead of feeding it back and retrying. Transport failures usually
	// mean the endpoint is down, so seams that cannot make progress abort;
	// advisory seams (convert controllers, discover naming) retry.
	AbortOnChatError bool
}

// RunSeam executes one bounded-retry LLM seam call. It returns the accepted
// payload (Extract applied to the raw response), the number of chat calls
// made, the accumulated rejection notes, and the failure error when every
// attempt was rejected or a caller-policy abort fired.
func RunSeam(ctx context.Context, in SeamInput) (payload string, calls int, notes []string, err error) {
	maxAttempts := in.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	extract := in.Extract
	if extract == nil {
		extract = func(content string) string { return content }
	}
	record := func(attempt int, prompt string, resp Response, errs []string) {
		if in.Audit == nil {
			return
		}
		e := audit.Exchange{
			Unit: in.Unit, Kind: in.Kind, Name: in.Name, Attempt: attempt,
			Template: in.Template, LLM: in.LLM, RetryRepair: in.Repair,
			Prompt: prompt, Response: resp.Content, Errors: errs, Outcome: "ok",
			FinishReason: resp.FinishReason,
		}
		if resp.Usage.TotalTokens > 0 || resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
			e.PromptTokens = resp.Usage.PromptTokens
			e.CompletionTokens = resp.Usage.CompletionTokens
			e.TotalTokens = resp.Usage.TotalTokens
			e.UsageEstimated = resp.Usage.Estimated
		}
		if len(errs) > 0 {
			e.Outcome = "failed"
		}
		if in.File != nil {
			e.File = in.File(attempt)
		}
		if _, werr := in.Audit.WriteExchange(e); werr != nil {
			telemetry.Log(ctx).Warn("audit exchange failed", "kind", in.Kind, "name", in.Name, "error", werr)
		}
	}
	var lastErr error
	// prev is the payload the previous attempt was gated on — the repair
	// context callers may send back as an assistant turn. Cleared on
	// failures that produced no usable payload (chat error, over-cap).
	var prev string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		archived, messages := in.Prompt(prev, notes)
		// The archived prompt is the base prompt; retry turns (the feedback
		// notes, and the prior payload in repair mode) are real input too —
		// count them so the ceiling reflects what the model actually sees.
		counted := archived
		if len(messages) > 2 {
			for _, m := range messages[2:] {
				counted += "\n" + m.Content
			}
		}
		inputTokens := in.Budget.Count(counted)
		telemetry.Log(ctx).Info("llm attempt",
			"name", in.Name, "attempt", attempt+1, "max", maxAttempts, "prompt_tokens_est", inputTokens)
		if in.Budget.MaxPromptTokens > 0 {
			if berr := in.Budget.CheckInput(counted); berr != nil {
				return "", calls, notes, berr
			}
		}
		// The output ceiling for this attempt: the dynamic policy derives it
		// from the model context minus the measured input (clamped by the
		// provider's completion cap); the static policy returns
		// MaxOutputTokens (0 = no ceiling). A room the reserve collapsed to
		// near zero means the prompt over-consumed the context — loud, not
		// a one-token generation.
		cap := in.Budget.OutputCeiling(inputTokens)
		if in.Budget.Dynamic() && cap < 256 {
			return "", calls, notes, fmt.Errorf("budget: prompt of ~%d tokens leaves only %d tokens of output room against the %d-token context — shrink the prompt, raise the context, or lower the reserve",
				inputTokens, cap, in.Budget.ModelContextTokens)
		}
		maxTokens := in.MaxTokens // the seam's explicit override wins (thinking-mode room)
		if maxTokens == 0 && in.Budget.Dynamic() {
			maxTokens = cap
		}
		resp, cerr := in.Client.Chat(ctx, ChatRequest{
			Model:       "", // endpoint default (resolved by the client's wiring)
			Messages:    messages,
			Temperature: in.Temperature,
			MaxTokens:   maxTokens,
		})
		calls++
		if cerr != nil {
			lastErr = cerr
			telemetry.Log(ctx).Warn("llm call failed",
				"name", in.Name, "attempt", attempt+1, "max", maxAttempts, "error", cerr.Error())
			record(attempt, archived, Response{}, []string{cerr.Error()})
			prev = "" // no payload to patch
			if in.AbortOnChatError {
				return "", calls, notes, fmt.Errorf("llm chat: %w", cerr)
			}
			notes = append(notes, cerr.Error())
			continue
		}
		// Truncation signal (engine-wiring audit Tier-1 #8): a
		// finish_reason other than "stop" means the provider cut the
		// completion — the payload may end mid-statement even when the
		// gates happen to pass. The archived Exchange carries
		// finish_reason + usage; the note rides into the next attempt's
		// prompt whenever a later gate rejects.
		if resp.FinishReason != "" && resp.FinishReason != "stop" {
			telemetry.Log(ctx).Warn("llm response truncated", "kind", in.Kind, "name", in.Name,
				"finish_reason", resp.FinishReason, "total_tokens", resp.Usage.TotalTokens)
			notes = append(notes, "response truncated (finish_reason "+resp.FinishReason+") — the payload may be incomplete")
		}
		if cap > 0 {
			if berr := in.Budget.CheckOutputCap(resp.Content, cap); berr != nil {
				lastErr = berr
				telemetry.Log(ctx).Warn("llm output over cap",
					"name", in.Name, "attempt", attempt+1, "max", maxAttempts, "error", berr.Error())
				record(attempt, archived, resp, []string{berr.Error()})
				prev = "" // over-cap content is not a repair base
				notes = append(notes, "output over budget: "+berr.Error())
				continue
			}
		}
		payload = extract(resp.Content)
		var gateErrs []string
		if in.Gate != nil {
			gateErrs = in.Gate(payload)
		}
		if len(gateErrs) > 0 {
			if in.Rejected != nil {
				in.Rejected(payload)
			}
			lastErr = errors.New(strings.Join(gateErrs, "; "))
			telemetry.Log(ctx).Warn("llm attempt rejected — retrying",
				"name", in.Name, "attempt", attempt+1, "max", maxAttempts,
				"errors", len(gateErrs), "first", firstErrSummary(gateErrs))
			record(attempt, archived, resp, gateErrs)
			prev = payload // the repair base for the next attempt
			notes = append(notes, gateErrs...)
			continue
		}
		record(attempt, archived, resp, nil)
		return payload, calls, notes, nil
	}
	if lastErr == nil {
		lastErr = errors.New("all attempts rejected")
	}
	return "", calls, notes, lastErr
}

// firstErrSummary keeps the rejection log line readable: the first gate
// error, truncated — the full list lives in the audit exchange and the
// next attempt's notes.
func firstErrSummary(errs []string) string {
	s := strings.Join(errs, "; ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
