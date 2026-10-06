package convert

import (
	"strings"

	"tux-migration-tool/internal/llm"
)

// Retry fix hints — the closing instruction each convert seam appends to the
// accumulated gate notes. One home: the retry methodology experiment keeps
// roll and repair modes byte-identical on the feedback turn.
const (
	fixHintMethodBody = "\nFix these errors and emit only the corrected method body."
	fixHintMethod     = "\nFix these errors and emit only the corrected method."
	fixHintFragment   = "\nFix these errors and emit only the corrected fragment statements."
	fixHintMergedBody = "\nFix these errors and emit only the corrected merged body."
)

// seamMessages assembles one convert seam attempt's messages. Gate notes
// always ride a final user turn; in repair mode the prior attempt's gated
// payload (prev) rides an assistant turn before them, so the model patches
// its own output instead of regenerating from scratch. prev is empty on the
// first attempt and after transport/over-cap failures.
func seamMessages(system, prompt, prev, fixHint string, notes []string, repair bool) []llm.Message {
	msgs := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}
	if len(notes) == 0 {
		return msgs
	}
	if repair && strings.TrimSpace(prev) != "" {
		msgs = append(msgs, llm.Message{Role: "assistant", Content: prev})
	}
	return append(msgs, llm.Message{
		Role:    "user",
		Content: "Your previous output failed validation:\n" + strings.Join(notes, "\n") + fixHint,
	})
}
