// Package budget enforces the window policy (PRD §4.3, architecture.md
// Phase 4): approximate token accounting against the configured ceilings and
// the legacy query-replacement view — every EXEC SQL block (flattened cursors
// included) rewritten to its one resolved DB call line so controller units
// never see raw SQL. Ceilings and the estimator ratio arrive as plain values
// (wired from run.* in .tuxconv.yaml by cmd); this package reads no config.
package budget

import (
	"fmt"
	"strings"
)

// Budget is the configured token policy: prompt ceiling, output ceiling, and
// the characters-per-token estimate ratio.
type Budget struct {
	MaxPromptTokens int
	MaxOutputTokens int
	CharsPerToken   int

	// Dynamic output policy (run.tokenPolicy: dynamic): the per-call output
	// room derives from the model's real context window minus the measured
	// input, clamped by the provider's per-request completion cap — a
	// static MaxOutputTokens below the model's room truncates bodies that
	// would have fit. ModelContextTokens enables the policy; zero keeps the
	// static ceilings.
	ModelContextTokens   int
	ModelMaxOutputTokens int
	OutputReserveTokens  int
}

// New returns a Budget. A non-positive charsPerToken falls back to the §4.3
// default of 4 so mis-wired callers cannot divide by zero.
func New(maxPromptTokens, maxOutputTokens, charsPerToken int) Budget {
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	return Budget{
		MaxPromptTokens: maxPromptTokens,
		MaxOutputTokens: maxOutputTokens,
		CharsPerToken:   charsPerToken,
	}
}

// Count approximates the token count of s (chars/ratio, rounded up). A
// zero-value Budget carries ratio 0 — fall back to the §4.3 default of 4 so
// a mis-built Budget cannot divide by zero.
func (b Budget) Count(s string) int {
	if s == "" {
		return 0
	}
	if b.CharsPerToken <= 0 {
		b.CharsPerToken = 4
	}
	n := len(s)
	tokens := n / b.CharsPerToken
	if n%b.CharsPerToken != 0 {
		tokens++
	}
	return tokens
}

// ErrOverBudget reports a prompt or response that exceeds its ceiling; the
// bounded-retry loop trims and re-assembles instead of silently truncating.
type ErrOverBudget struct {
	Section string // "prompt" or "output"
	Have    int
	Limit   int
}

func (e *ErrOverBudget) Error() string {
	return fmt.Sprintf("budget: %s of %d tokens exceeds the %d-token ceiling",
		e.Section, e.Have, e.Limit)
}

// CheckInput validates an assembled prompt against MaxPromptTokens.
func (b Budget) CheckInput(prompt string) error {
	if have := b.Count(prompt); have > b.MaxPromptTokens {
		return &ErrOverBudget{Section: "prompt", Have: have, Limit: b.MaxPromptTokens}
	}
	return nil
}

// CheckOutput validates a model response against MaxOutputTokens.
func (b Budget) CheckOutput(response string) error {
	if have := b.Count(response); have > b.MaxOutputTokens {
		return &ErrOverBudget{Section: "output", Have: have, Limit: b.MaxOutputTokens}
	}
	return nil
}

// Dynamic reports whether the output room derives from the model context
// rather than the static MaxOutputTokens.
func (b Budget) Dynamic() bool { return b.ModelContextTokens > 0 }

// OutputCeiling is the output ceiling for one call given the input's token
// estimate: the dynamic policy returns min(context − input − reserve,
// modelMaxOutput) — never below 1 — and the static policy returns
// MaxOutputTokens (0 when unset: no ceiling). The caller passes the input's
// estimated tokens; a 40%-skewed estimate overclaims the room, which the
// reserve absorbs after charsPerToken calibration.
func (b Budget) OutputCeiling(inputTokens int) int {
	if !b.Dynamic() {
		return b.MaxOutputTokens
	}
	if b.OutputReserveTokens < 0 {
		b.OutputReserveTokens = 0
	}
	room := b.ModelContextTokens - inputTokens - b.OutputReserveTokens
	if b.ModelMaxOutputTokens > 0 && room > b.ModelMaxOutputTokens {
		room = b.ModelMaxOutputTokens
	}
	if room < 1 {
		room = 1
	}
	return room
}

// CheckOutputCap validates a model response against an explicit ceiling —
// the dynamic OutputCeiling result at the seam call site.
func (b Budget) CheckOutputCap(response string, limit int) error {
	if have := b.Count(response); have > limit {
		return &ErrOverBudget{Section: "output", Have: have, Limit: limit}
	}
	return nil
}

// DBCall is the resolved replacement for one query unit: the plan level maps
// query IDs to calls (method naming is plan work); budget only renders them.
// Tx is the transaction handle argument the tx-variant DML signatures demand
// (G-SCEN6) — rendered between the context and the args; empty for plain
// calls.
type DBCall struct {
	Receiver string   // rendered "<Receiver>.Name(...)" — empty for a bare call
	Name     string   // GetItemDetails
	CtxName  string   // c or ctx
	Tx       string   // transaction handle arg after the context ("tx" for tx-variant DML)
	Args     []string // call-site expressions, host vars already mapped
}

// Line renders the one-line call that replaces a legacy SQL region.
func (c DBCall) Line() string {
	var sb strings.Builder
	if c.Receiver != "" {
		sb.WriteString(c.Receiver)
		sb.WriteString(".")
	}
	sb.WriteString(c.Name)
	sb.WriteString("(")
	sb.WriteString(c.CtxName)
	if c.Tx != "" {
		sb.WriteString(", ")
		sb.WriteString(c.Tx)
	}
	for _, a := range c.Args {
		sb.WriteString(", ")
		sb.WriteString(a)
	}
	sb.WriteString(")")
	return sb.String()
}
