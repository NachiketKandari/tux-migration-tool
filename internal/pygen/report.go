package pygen

import (
	"strings"

	"tux-migration-tool/internal/pychk"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/sqlchk"
)

// Retention is the logic-retention report (PRD-2026-09-08 BP-10): how much
// of the source batch's trackable logic the generated module actually
// carries, per mode (shape × DML loop × llm on/off). Trackable items:
// SELECT constants, DML constants, rubric phases, and (LLM mode) CodeView
// call stubs represented in the service body. Log-site parity is reported
// alongside, not folded into the percent — logging is parity, not logic.
type Retention struct {
	Shape           string `json:"shape"`
	DMLLoop         string `json:"dml_loop"`
	SelectTotal     int    `json:"select_total"`
	SelectMatched   int    `json:"select_matched"`
	DMLTotal        int    `json:"dml_total"`
	DMLMatched      int    `json:"dml_matched"`
	PhasesTotal     int    `json:"phases_total"`
	PhasesEmitted   int    `json:"phases_emitted"`
	StubTotal       int    `json:"stub_total"`
	StubRepresented int    `json:"stub_represented"`
	LogSitesTotal   int    `json:"log_sites_total"`
	LogCallsEmitted int    `json:"log_calls_emitted"`
	SyntaxOK        bool   `json:"syntax_ok"`
	PyMode          string `json:"py_mode"`
	SQLDeviations   int    `json:"sql_deviations"`
	LLMCalls        int    `json:"llm_calls"`
	LLMFilled       bool   `json:"llm_filled"`
}

// Percent is the retained-logic ratio over the trackable items.
func (r Retention) Percent() float64 {
	total := r.SelectTotal + r.DMLTotal + r.PhasesTotal + r.StubTotal
	if total == 0 {
		return 100
	}
	matched := r.SelectMatched + r.DMLMatched + r.PhasesEmitted + r.StubRepresented
	return float64(matched) / float64(total) * 100
}

// retentionOf computes the report for one generated module. serviceBody is
// the service-class region only — stub representation counts there, not in
// the repo scaffold that defines the methods.
func retentionOf(p *pyplan.Plan, content, serviceBody string, fidelity []sqlchk.Result, llmCalls int, structure []pychk.Issue, pyOK bool, pyMode string, llmFilled bool) Retention {
	r := Retention{
		Shape: p.Shape, DMLLoop: p.DMLLoop,
		PhasesTotal: len(p.Phases), LogSitesTotal: len(p.Flow.Logs),
		SyntaxOK: len(structure) == 0 && pyOK, PyMode: pyMode,
		SQLDeviations: 0, LLMCalls: llmCalls, LLMFilled: llmFilled,
	}
	byQuery := map[string]sqlchk.Result{}
	for _, res := range fidelity {
		byQuery[res.QueryID] = res
		if res.Status == sqlchk.StatusDeviated {
			r.SQLDeviations++
		}
	}
	isSelect := func(kind string) bool { return strings.HasPrefix(kind, "SELECT_") }
	for _, c := range p.Consts {
		res, ok := byQuery[c.ID]
		matched := ok && res.Status == sqlchk.StatusMatch
		if isSelect(c.Kind) {
			r.SelectTotal++
			if matched {
				r.SelectMatched++
			}
			continue
		}
		if c.Kind == "TRUNCATE" {
			continue // structural plumbing, not logic
		}
		r.DMLTotal++
		if matched {
			r.DMLMatched++
		}
	}
	for _, ph := range p.Phases {
		if strings.Contains(content, "def "+ph.Method+"(") {
			r.PhasesEmitted++
		}
	}
	if p.Shape == "repo" {
		for _, q := range p.Flow.Queries {
			call := p.CallName(q.ID)
			if call == "" {
				continue
			}
			r.StubTotal++
			if strings.Contains(serviceBody, call) {
				r.StubRepresented++
			}
		}
	}
	r.LogCallsEmitted = strings.Count(content, "logger.")
	return r
}
