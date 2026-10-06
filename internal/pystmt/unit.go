package pystmt

import (
	"sort"
	"strconv"
	"strings"

	scanner "tux-migration-tool/internal/tsscan"
)

// Unit is one file's statement IR: every function the scanner found a
// definition for, in source order, plus the two accounting reports that make
// the IR checkable rather than merely plausible.
type Unit struct {
	// Path is the source file, for the audit archive.
	Path string `json:"path"`
	// Funcs are the functions, in source order.
	Funcs []Func `json:"funcs"`
	// Coverage partitions every source line of every scanned function body.
	Coverage Coverage `json:"coverage"`
	// Residue is the other half of the proof: a scanner fact that no
	// statement claims.
	Residue Residue `json:"residue"`
	// Counts is the whole-unit statement histogram.
	Counts map[Kind]int `json:"counts,omitempty"`
}

// Coverage is the §6.1 gate-2 question asked of the IR itself: does every
// line of real code belong to exactly one statement?
//
// Every line of every scanned body lands in exactly one reason. `Uncovered`
// is what is left, and a non-empty Uncovered is a statement the classifier
// failed to see — the same silent loss §3.2 complains about, except now it
// has a number.
//
// The reasons are deliberately explicit rather than "covered or not":
//
//	stmt    — inside some statement's byte span
//	header  — a control keyword line (`if (…)`, `while (…)`, `case 'A':`)
//	brace   — a block delimiter, with or without a trailing comment
//	blank   — no code
//	comment — code-free once comments are blanked
//	preproc — a `#` directive
type Coverage struct {
	// Scanned is the number of function bodies the IR covered.
	Scanned int `json:"scanned"`
	// Lines is every source line inside those bodies.
	Lines int `json:"lines"`
	// ByReason is the partition. Its sum must equal Lines.
	ByReason map[string]int `json:"by_reason"`
	// Uncovered lists the lines that reached no reason at all.
	Uncovered []int `json:"uncovered,omitempty"`
	// ByKind is the statement histogram.
	ByKind map[Kind]int `json:"by_kind,omitempty"`
	// Depth is the deepest block nesting reached — a zero here on a
	// stateful batch would mean the skeleton never nested.
	Depth int `json:"depth"`
}

// Clean reports that every line of every scanned body was accounted for.
func (c Coverage) Clean() bool { return len(c.Uncovered) == 0 }

// Residue is the inverse check: a fact the scanner recorded inside a scanned
// function that no statement claimed. It catches the failure a line-coverage
// count cannot — a statement span that is too wide, swallowing a call it
// should have named.
type Residue struct {
	// Calls are `name@line` for a call site no statement carries.
	Calls []string `json:"calls,omitempty"`
	// Decls are `name@line` for a declaration no statement carries.
	Decls []string `json:"decls,omitempty"`
	// Returns are source lines of a return no statement carries.
	Returns []int `json:"returns,omitempty"`
	// Funcs are functions the scanner found but the IR did not emit, and
	// vice versa — the §1 complaint that `func_spill_over_date` "does not
	// appear anywhere in the output" stated as a check.
	MissingFuncs []string `json:"missing_funcs,omitempty"`
	ExtraFuncs   []string `json:"extra_funcs,omitempty"`
}

// Clean reports that every scanner fact reached a statement.
func (r Residue) Clean() bool {
	return len(r.Calls) == 0 && len(r.Decls) == 0 && len(r.Returns) == 0 &&
		len(r.MissingFuncs) == 0 && len(r.ExtraFuncs) == 0
}

// Total is the number of residue items, for a one-number report.
func (r Residue) Total() int {
	return len(r.Calls) + len(r.Decls) + len(r.Returns) + len(r.MissingFuncs) + len(r.ExtraFuncs)
}

// QueryRef binds one EXEC SQL region to the IR query unit it became. The
// caller supplies it rather than this package reaching for the plan, so the
// statement IR stays free of a dependency on the generation plan — the plan
// consumes the IR, not the other way round.
type QueryRef struct {
	// StartLine is the region's first line, which is how a region is matched
	// back to its unit (the batch flow already keys on it).
	StartLine int `json:"start_line"`
	// ID is the IR query id, empty for a region that is not a query
	// (`EXEC SQL include`, cursor choreography).
	ID string `json:"id,omitempty"`
	// Kind is the scanner's SQL kind.
	Kind string `json:"kind,omitempty"`
}

// Input is everything Build needs. Nothing here is invented by this package:
// every field is something the existing pipeline already produced.
type Input struct {
	// Facts is the shared scanner's structural record, read-only.
	Facts *scanner.SourceFacts
	// Src is the source text the facts' positions index into.
	Src string
	// SQL is the in-body EXEC SQL regions, in source order.
	SQL []QueryRef
	// Limit bounds every extent walk. Zero means "the source's line count" —
	// a function whose closing brace never arrives (an unbalanced file
	// resolves what it can) must not walk past the end of the file.
	Limit int
}

// Build is the whole IR for one file. Deterministic: identical inputs produce
// an identical Unit, which is what lets a golden test pin it.
func Build(in Input) Unit {
	u := Unit{}
	b := newBuilder(in)
	u.Path = b.factsPath()
	u.Funcs = b.functions()
	u.Coverage = b.coverage()
	u.Residue = b.residue()
	u.Counts = map[Kind]int{}
	for i := range u.Funcs {
		Walk(u.Funcs[i].Stmts, func(s *Stmt) { u.Counts[s.Kind]++ })
	}
	if len(u.Counts) == 0 {
		u.Counts = nil
	}
	return u
}

// Statements returns every statement in the unit in pre-order.
func (u Unit) Statements() []*Stmt {
	var out []*Stmt
	for i := range u.Funcs {
		Walk(u.Funcs[i].Stmts, func(s *Stmt) { out = append(out, s) })
	}
	return out
}

// Summary renders the unit's whole statement stream as indented text. It is
// the artefact a reviewer reads to decide whether the IR believes what the
// source says; a machine agreeing with itself is not evidence.
func (u Unit) Summary() string {
	var b strings.Builder
	for _, f := range u.Funcs {
		b.WriteString("function " + f.Name + " (source " + f.Body.Line() + ")\n")
		b.WriteString(f.Summary())
	}
	return b.String()
}

// report renders the two accounting reports as lines, in the same
// census voice the batchpy report already uses.
func (u Unit) report() []string {
	var out []string
	if u.Coverage.Scanned == 0 {
		return out
	}
	out = append(out, "stmt ir: functions="+strconv.Itoa(len(u.Funcs))+
		" statements="+strconv.Itoa(countTotal(u.Counts))+
		" lines="+strconv.Itoa(u.Coverage.Lines)+
		" depth="+strconv.Itoa(u.Coverage.Depth))
	if len(u.Coverage.Uncovered) > 0 {
		out = append(out, "stmt ir: "+strconv.Itoa(len(u.Coverage.Uncovered))+
			" code line(s) reached no statement — "+lineList(u.Coverage.Uncovered))
	}
	if u.Residue.Total() > 0 {
		var parts []string
		if len(u.Residue.Calls) > 0 {
			parts = append(parts, "calls "+strings.Join(u.Residue.Calls, ", "))
		}
		if len(u.Residue.Decls) > 0 {
			parts = append(parts, "decls "+strings.Join(u.Residue.Decls, ", "))
		}
		if len(u.Residue.Returns) > 0 {
			parts = append(parts, "returns "+lineList(u.Residue.Returns))
		}
		if len(u.Residue.MissingFuncs) > 0 {
			parts = append(parts, "missing functions "+strings.Join(u.Residue.MissingFuncs, ", "))
		}
		out = append(out, "stmt ir: "+strconv.Itoa(u.Residue.Total())+
			" scanner fact(s) reached no statement — "+strings.Join(parts, "; "))
	}
	return out
}

// Report is the exported form of report(): the statement-IR accounting lines
// for the batchpy operator report.
func (u Unit) Report() []string { return u.report() }

func countTotal(m map[Kind]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func lineList(lines []int) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = strconv.Itoa(l)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}