package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Exchange is one LLM call's full trace — the assembled prompt the model saw
// and the raw response it produced — so every seam's AI step (convert
// controllers, batchpy service bodies, gentest gap-fill, discover naming) is
// reproducible from the run's audit folder. Outcome is derivable from Errors;
// it is carried for human readers of the artifact.
type Exchange struct {
	Unit     string   `json:"unit,omitempty"`     // unit / step identifier (u01, module name, entry fn)
	Kind     string   `json:"kind,omitempty"`     // what the call generated (controller, batchpy, gentest, discover…)
	Name     string   `json:"name"`               // function / endpoint / candidate the call was for
	Attempt  int      `json:"attempt"`            // 0-based retry attempt
	Template string   `json:"template,omitempty"` // template ID when the caller is plan-backed
	LLM      bool     `json:"llm,omitempty"`      // whether the caller unit is LLM-shaped
	Prompt   string   `json:"prompt"`
	Response string   `json:"response"`
	Errors   []string `json:"errors,omitempty"` // gate/validation errors fed back into the next attempt
	Outcome  string   `json:"outcome"`          // ok | failed
	// RetryRepair marks the seam's retry methodology: true = rejected
	// attempts rode back as an assistant turn (patch-in-place), false =
	// fresh regeneration. Lets tuxconv retrystats compare A/B audit runs.
	RetryRepair bool `json:"retry_repair,omitempty"`
	// FinishReason is the provider's finish_reason for this attempt
	// ("stop", "length", …). Anything but "stop"/"" means the payload may
	// be cut mid-statement — the audit trail must say so (engine-wiring
	// audit Tier-1 #8: truncation was indistinguishable from success).
	FinishReason string `json:"finish_reason,omitempty"`
	// Token usage for this attempt (provider-reported; estimated when the
	// endpoint omits usage — UsageEstimated says which).
	PromptTokens     int  `json:"prompt_tokens,omitempty"`
	CompletionTokens int  `json:"completion_tokens,omitempty"`
	TotalTokens      int  `json:"total_tokens,omitempty"`
	UsageEstimated   bool `json:"usage_estimated,omitempty"`
	// File overrides the default "<kind>-<name>-attempt<attempt>.json"
	// artifact name (convert pins its unit-<id>-attempt<n> convention).
	File string `json:"-"`
}

// WriteJSON marshals v (indent-2) and archives it under name — the one
// helper for the marshal→Write→warn pattern every command's artifact
// archive repeated (A5.2). A nil receiver or marshal error is a no-op; the
// write error is returned.
func (r *Recorder) WriteJSON(name string, v any) (string, error) {
	if r == nil {
		return "", nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("audit: %s: %w", name, err)
	}
	return r.Write(name, func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
}

// WriteExchange archives one LLM call's full trace and returns the written
// path. Safe for concurrent callers (Write is mutex-guarded).
func (r *Recorder) WriteExchange(e Exchange) (string, error) {
	name := e.File
	if name == "" {
		name = sanitizeArtifact(fmt.Sprintf("%s-%s-attempt%d.json", e.Kind, e.Name, e.Attempt))
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("audit: exchange %s: %w", name, err)
	}
	return r.Write(name, func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
}

// sanitizeArtifact keeps an Exchange's derived file name acceptable to
// Write (no path separators, no spaces).
func sanitizeArtifact(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(name)
}
