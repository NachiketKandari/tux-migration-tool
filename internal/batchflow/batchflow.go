// Package batchflow builds the batch-program control skeleton (PRD
// 2026-09-08 BP-2): the ordered steps of a Pro*C batch entry body — flattened
// cursor groups with their per-row DML, standalone DML/SELECT steps, loop
// extents, log sites, and the dropped Tuxedo/FML/registration constructs —
// plus the shape-rubric decision (simple cursor-batch vs stateful) that picks
// the generated Python shape. Target-language-agnostic: it consumes scanner
// facts and the IR and never invents behavior; extraction remains 100% tool
// work.
package batchflow

import (
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// DropKind classifies one dropped construct (BP-2 rubric). Batch programs
// carry no FML contract and no Tuxedo transaction scope in the Python target:
// tp* plumbing is owned by the wrapper seam, errlog arms fold into exception
// handling, and batch registration is an environment service.
type DropKind string

const (
	DropTuxedo       DropKind = "tuxedo"       // tpalloc/tpinit/tpbegin/tpcommit/…, tuxgetenv
	DropFML          DropKind = "fml"          // Fadd32/Fchg32/Fget32/MEMSET/SETNULL
	DropRegistration DropKind = "registration" // fn_rgstr_bat/fn_bat_pst_msg
	DropDebug        DropKind = "debug"        // INITBATDBGLVL
	DropErrorLog     DropKind = "errorlog"     // errlog — folded into exception handlers
)

// DropSite is one dropped construct with its source line.
type DropSite struct {
	Kind DropKind `json:"kind"`
	Call string   `json:"call"`
	Line int      `json:"line"`
}

// LogSite is one userlog/printf call site (logging-parity rubric BP-2).
type LogSite struct {
	Call       string `json:"call"`
	Line       int    `json:"line"`
	Text       string `json:"text"`        // raw argument text
	DebugGated bool   `json:"debug_gated"` // inside a DEBUG_MSG_LVL_* block
}

// Loop is one while/for loop extent inside the entry body.
type Loop struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Header    string `json:"header"`
}

// CursorGroup is the cursor-batch rubric: one flattened SELECT_MULTI cursor
// plus its paired per-row DML whose binds all come from the cursor's
// RowShape (the fetch-INTO list).
type CursorGroup struct {
	CursorName string    `json:"cursor_name"`
	Select     *ir.Query `json:"select"`
	DML        *ir.Query `json:"dml,omitempty"`
}

// Step is one query unit the cursor rubric does not cover.
type Step struct {
	Query       *ir.Query `json:"query"`
	TruncateSQL string    `json:"truncate_sql,omitempty"` // raw EXEC SQL TRUNCATE immediately preceding (rebuild pairing)
	InLoop      bool      `json:"in_loop"`
}

// Shape names the rubric outcome: "simple" (every query covered by the
// cursor-batch rubric) or "stateful" (running state or uncovered queries —
// the LLM seam fills the service body).
const (
	ShapeSimple   = "simple"
	ShapeStateful = "stateful"
)

// SQLSpan is one EXEC SQL region inside the entry body (the CodeView and the
// drop inventory need the spans, not just the IR query units).
type SQLSpan struct {
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Kind       string `json:"kind"`
	CursorName string `json:"cursor_name,omitempty"`
	Normal     string `json:"normalized"`
}

// Flow is the batch skeleton of one .pc batch program.
type Flow struct {
	Path         string         `json:"path"`
	ServiceName  string         `json:"service_name"`
	Entry        string         `json:"entry"`
	Shape        string         `json:"shape"`
	BodyStart    int            `json:"body_start"`
	BodyEnd      int            `json:"body_end"`
	Loops        []Loop         `json:"loops"`
	SQLSpans     []SQLSpan      `json:"sql_spans"`
	CursorGroups []*CursorGroup `json:"cursor_groups"`
	Steps        []*Step        `json:"steps"`
	Dropped      []DropSite     `json:"dropped"`
	Logs         []LogSite      `json:"logs"`
	Queries      []*ir.Query    `json:"queries"` // unique query units inside the entry body, IR order
	// Guards is the SQLCODE census of the entry body. Its presence is what
	// lets a target honour the source's failure contract instead of picking
	// one per query type.
	Guards Guards `json:"guards"`
}

// Guards is the post-query status-guard inventory of the entry body: every
// `if (SQLCODE …)` branch paired with the query it follows, and the polarity
// that branch declares.
//
// Flow has no branch field today, so this is the batch path's only record that
// a query's failure contract was ever stated. Without it the guard reaches the
// LLM seam as untranslated C — a bare `if(SQLCODE != 0)` header whose body has
// already been replaced by `# dropped [errorlog]` — and an empty result is
// indistinguishable from a failure.
type Guards struct {
	// Strict counts guards where an empty result IS a failure
	// (`if (SQLCODE != 0)`).
	Strict int `json:"strict"`
	// TolerantNoRows counts guards where an empty result is a SUCCESS
	// (`if (SQLCODE != 0 && SQLCODE != NO_DATA_FOUND)`).
	TolerantNoRows int `json:"tolerant_no_rows"`
	// Unclassified counts guards whose polarity could not be read with
	// confidence. Non-zero is a coverage signal, not a failure: those queries
	// keep the per-type default and the count says so.
	Unclassified int `json:"unclassified"`
	// Benign counts `== NO_DATA_FOUND` arms — success arms that declare no
	// failure contract of their own.
	Benign int `json:"benign"`
	// ByQuery is the per-query verdict, so an emitter reads the polarity off
	// the query it is emitting rather than re-deriving it.
	ByQuery map[string]contract.ErrorPolicy `json:"by_query,omitempty"`
	// Views is the same inventory as one span per guard, for the CodeView.
	// The seam needs the guard's own line and extent, not just the tally:
	// without it the guard header reaches the model as untranslated C with
	// its body already elided.
	Views []GuardView `json:"views,omitempty"`
}

// GuardView is one status guard as the seam's CodeView must see it: the legacy
// line it occupies, and the contract it states. It is deliberately a
// translation of the guard's *meaning*, not of its spelling — the CodeView's
// job is to be implemented, and `SQLCODE` cannot be implemented in Python.
type GuardView struct {
	Line        int                  `json:"line"`
	EndLine     int                  `json:"end_line,omitempty"`
	Policy      contract.ErrorPolicy `json:"policy"`
	Benign      bool                 `json:"benign,omitempty"`
	Compensates bool                 `json:"compensates,omitempty"`
	ErrCode     string               `json:"err_code,omitempty"`
}

// Build derives the flow skeleton from the file IR, the scanner facts, and
// the source text. Deterministic: identical inputs produce an identical Flow.
func Build(f *ir.File, facts *scanner.SourceFacts, src string) *Flow {
	entry, body := entryBody(facts)
	flow := &Flow{
		Path:        f.Path,
		Entry:       entry,
		BodyStart:   body.BodyStartLine,
		BodyEnd:     bodyEnd(facts, body),
		ServiceName: serviceName(facts, body, f.Path),
	}
	lines := strings.Split(src, "\n")
	flow.Loops = findLoops(lines, facts, entry, flow.BodyStart, flow.BodyEnd)

	for _, q := range f.UniqueQueries() {
		if q.StartLine >= flow.BodyStart && q.StartLine <= flow.BodyEnd {
			flow.Queries = append(flow.Queries, q)
		}
	}

	// Cursor rubric: pair each non-cursor DML whose binds ⊆ a cursor's
	// RowShape with the cursor declared before it (max overlap, nearest).
	var cursorSelects []*ir.Query
	for _, q := range flow.Queries {
		if q.CursorFlattened && q.CursorName != "" {
			cursorSelects = append(cursorSelects, q)
		}
	}
	claimed := map[*ir.Query]*CursorGroup{}
	for _, q := range flow.Queries {
		if q.CursorFlattened || !isDML(q) || len(q.Binds) == 0 {
			continue
		}
		best, bestScore := (*ir.Query)(nil), 0
		for _, c := range cursorSelects {
			if c.StartLine >= q.StartLine {
				continue
			}
			score := overlap(q.Binds, c.RowShape)
			if score != len(uniqueNames(q.Binds)) {
				continue // the rubric requires every DML bind to be a fetched column
			}
			if score > bestScore || (score == bestScore && best != nil && c.StartLine > best.StartLine) {
				best, bestScore = c, score
			}
		}
		if best != nil {
			g := claimed[best]
			if g == nil {
				g = &CursorGroup{CursorName: best.CursorName, Select: best}
				claimed[best] = g
			}
			if g.DML == nil {
				g.DML = q
			}
		}
	}
	for _, c := range cursorSelects {
		if g := claimed[c]; g != nil {
			flow.CursorGroups = append(flow.CursorGroups, g)
		} else {
			flow.Steps = append(flow.Steps, &Step{Query: c, InLoop: InLoop(flow.Loops, c.StartLine)})
		}
	}
	for _, q := range flow.Queries {
		if q.CursorFlattened || isDMLPaired(q, claimed) {
			continue
		}
		flow.Steps = append(flow.Steps, &Step{Query: q, TruncateSQL: truncateBefore(facts, q.StartLine), InLoop: InLoop(flow.Loops, q.StartLine)})
	}

	for _, s := range facts.AllSQL {
		if s.StartLine < flow.BodyStart || s.StartLine > flow.BodyEnd {
			continue
		}
		flow.SQLSpans = append(flow.SQLSpans, SQLSpan{
			StartLine: s.StartLine, EndLine: s.EndLine, Kind: s.Kind.String(),
			CursorName: s.CursorName, Normal: s.Normalized,
		})
	}

	flow.Dropped = droppedSites(facts, flow.BodyStart, flow.BodyEnd)
	flow.Logs = logSites(facts, flow.BodyStart, flow.BodyEnd)
	flow.Guards = censusGuards(facts, f, flow.Queries, flow.BodyStart, flow.BodyEnd)
	flow.Shape = rubric(flow)
	return flow
}

// censusGuards pairs every query in the flow with the SQLCODE guard that
// immediately follows it and tallies the polarities.
//
// The pairing is the same adjacency rule the Go path uses (flow.GuardAfter):
// the nearest status branch below the query's last line, within the guard
// window. Queries with no guard contribute nothing, which is correct — the
// source stated no contract there, so the emitter keeps its default.
//
// Tallying is what makes this a census rather than a side table. A guard whose
// polarity could not be read lands in Unclassified instead of being guessed,
// so a corpus spelling the classifier does not know shows up as a number
// rather than as quietly-wrong error handling.
func censusGuards(facts *scanner.SourceFacts, f *ir.File, queries []*ir.Query, start, end int) Guards {
	g := Guards{ByQuery: map[string]contract.ErrorPolicy{}}
	for _, q := range queries {
		if q.StartLine < start || q.StartLine > end {
			continue
		}
		guard, ok := flow.GuardAfter(facts, q)
		if !ok {
			continue
		}
		switch {
		case guard.Policy.IsSet():
			g.ByQuery[q.ID] = guard.Policy
			g.Views = append(g.Views, viewOf(guard))
		case guard.Benign:
			g.Benign++
			g.Views = append(g.Views, viewOf(guard))
		default:
			// A status-referencing branch that is neither a declared policy
			// nor a pure success arm: a no-rows test fused with business
			// logic. It declares no failure contract, so it does not become a
			// query verdict — but it is not silently dropped either, and the
			// seam still gets a translation of it.
			g.Unclassified++
			g.Views = append(g.Views, viewOf(guard))
		}
		switch guard.Policy {
		case contract.PolicyStrict:
			g.Strict++
		case contract.PolicyTolerantNoRows:
			g.TolerantNoRows++
		}
	}
	return g
}

// viewOf projects a flow.Guard onto the CodeView shape.
func viewOf(g flow.Guard) GuardView {
	return GuardView{
		Line: g.Line, EndLine: g.EndLine, Policy: g.Policy,
		Benign: g.Benign, Compensates: g.Compensates, ErrCode: g.ErrCode,
	}
}

// rubric decides the shape: simple only when the cursor-batch rubric covers
// every query (each cursor paired with exactly one DML, no leftover steps).
func rubric(flow *Flow) string {
	if len(flow.Steps) == 0 && len(flow.CursorGroups) > 0 {
		for _, g := range flow.CursorGroups {
			if g.DML == nil {
				return ShapeStateful
			}
		}
		return ShapeSimple
	}
	return ShapeStateful
}

func isDML(q *ir.Query) bool {
	return q.Type.IsDML()
}

func isDMLPaired(q *ir.Query, claimed map[*ir.Query]*CursorGroup) bool {
	for _, g := range claimed {
		if g.DML == q {
			return true
		}
	}
	return false
}

func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		k := strings.ToLower(n)
		if !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	return out
}

func overlap(binds, rowShape []string) int {
	idx := map[string]bool{}
	for _, r := range rowShape {
		idx[strings.ToLower(r)] = true
	}
	n := 0
	for _, b := range uniqueNames(binds) {
		if idx[strings.ToLower(b)] {
			n++
		}
	}
	return n
}

// InLoop reports whether the line falls inside one of the flow's loop
// extents. The one containment check for the batch path (pyplan consumes it).
func InLoop(loops []Loop, line int) bool {
	for _, l := range loops {
		if line >= l.StartLine && line <= l.EndLine {
			return true
		}
	}
	return false
}

// entryBody picks the batch entry: the function named main, else the SVC_*
// entry, else the first function, else the whole file (fragment-style).
func entryBody(facts *scanner.SourceFacts) (string, scanner.FunctionDef) {
	var main, svc, first *scanner.FunctionDef
	for i := range facts.Functions {
		fn := &facts.Functions[i]
		if first == nil {
			first = fn
		}
		if fn.Name == "main" && main == nil {
			main = fn
		}
		if strings.HasPrefix(fn.Name, "SVC_") && svc == nil {
			svc = fn
		}
	}
	switch {
	case main != nil:
		return "main", *main
	case svc != nil:
		return svc.Name, *svc
	case first != nil:
		return first.Name, *first
	default:
		return "", scanner.FunctionDef{BodyStartLine: 1}
	}
}

func bodyEnd(facts *scanner.SourceFacts, body scanner.FunctionDef) int {
	if body.BodyEndLine > 0 {
		return body.BodyEndLine
	}
	end := facts.NumLines
	for _, fn := range facts.Functions {
		if fn.StartLine > body.StartLine && fn.StartLine-1 < end {
			end = fn.StartLine - 1
		}
	}
	return end
}

// serviceName resolves the batch's service name: the literal assigned to
// c_ServiceName (strcpy/sprintf), else the file basename.
func serviceName(facts *scanner.SourceFacts, body scanner.FunctionDef, path string) string {
	for _, c := range facts.Calls {
		if c.Name != "strcpy" && c.Name != "sprintf" {
			continue
		}
		if c.Line < body.StartLine || (body.BodyEndLine > 0 && c.Line > body.BodyEndLine) {
			continue
		}
		if !strings.Contains(c.Args, "c_ServiceName") {
			continue
		}
		if lit := firstQuoted(c.Args); lit != "" {
			return lit
		}
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return strings.ToLower(base)
}

func firstQuoted(args string) string {
	i := strings.IndexByte(args, '"')
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(args[i+1:], '"')
	if j < 0 {
		return ""
	}
	return args[i+1 : i+1+j]
}

var dropKinds = map[string]DropKind{
	"tpalloc": DropTuxedo, "tpfree": DropTuxedo, "tpinit": DropTuxedo, "tpterm": DropTuxedo,
	"tpopen": DropTuxedo, "tpclose": DropTuxedo, "tpbegin": DropTuxedo, "tpcommit": DropTuxedo,
	"tpabort": DropTuxedo, "tuxgetenv": DropTuxedo,
	"Fadd32": DropFML, "Fchg32": DropFML, "Fget32": DropFML, "MEMSET": DropFML, "SETNULL": DropFML,
	"fn_rgstr_bat": DropRegistration, "fn_bat_pst_msg": DropRegistration,
	"INITBATDBGLVL": DropDebug,
	"errlog":        DropErrorLog,
}

func droppedSites(facts *scanner.SourceFacts, start, end int) []DropSite {
	var out []DropSite
	for _, c := range facts.Calls {
		kind, ok := dropKinds[c.Name]
		if !ok || c.Line < start || c.Line > end {
			continue
		}
		out = append(out, DropSite{Kind: kind, Call: c.Name, Line: c.Line})
	}
	return out
}

func logSites(facts *scanner.SourceFacts, start, end int) []LogSite {
	var out []LogSite
	for _, c := range facts.Calls {
		if (c.Name != "userlog" && c.Name != "printf") || c.Line < start || c.Line > end {
			continue
		}
		out = append(out, LogSite{Call: c.Name, Line: c.Line, Text: c.Args, DebugGated: debugGated(facts, c.Line)})
	}
	return out
}

func debugGated(facts *scanner.SourceFacts, line int) bool {
	for _, b := range facts.Branches {
		if !strings.Contains(b.Cond, "DEBUG_MSG_LVL") {
			continue
		}
		if line >= b.StartLine && (b.BlockEnd == 0 || line <= b.BlockEnd) {
			return true
		}
	}
	return false
}

// findLoops maps the scanner's loop records onto the batch Loop shape: the
// entry body's braced for/while loops, in source order. The scanner's loop
// inventory (FLW-1) is the single loop engine — batchflow carries no
// brace matcher of its own. Do-loops are excluded (their extent is the
// do-tail merge, not a while-header block) and unbraced bodies are dropped,
// both matching the previous line-scan behavior. Headers are re-derived
// from the source line so the JSON shape stays byte-identical.
func findLoops(lines []string, facts *scanner.SourceFacts, entry string, start, end int) []Loop {
	var out []Loop
	for _, l := range facts.Loops {
		if l.Kind == scanner.LoopDo || l.BlockEnd == 0 || l.Function != entry {
			continue
		}
		if l.StartLine < start || l.StartLine > end || l.StartLine-1 >= len(lines) {
			continue
		}
		if facts.InComment(l.StartLine, l.StartCol) { // defensive: the scanner never records these
			continue
		}
		trimmed := strings.TrimSpace(lines[l.StartLine-1])
		out = append(out, Loop{StartLine: l.StartLine, EndLine: l.BlockEnd, Header: loopHeader(trimmed)})
	}
	return out
}

func loopHeader(trimmed string) string {
	if i := strings.IndexByte(trimmed, '{'); i >= 0 {
		return strings.TrimRight(trimmed[:i], " \t")
	}
	return trimmed
}

// truncateBefore returns the raw EXEC SQL TRUNCATE statement that sits
// immediately before the query starting at startLine (the truncate+insert
// rebuild pairing). Comment lines between the two are tolerated; another SQL
// statement between them breaks the pairing.
func truncateBefore(facts *scanner.SourceFacts, startLine int) string {
	var trunc *scanner.ExecSQLStatement
	for i := range facts.AllSQL {
		s := &facts.AllSQL[i]
		if s.EndLine >= startLine {
			continue
		}
		if strings.Contains(strings.ToUpper(s.Normalized), "TRUNCATE") && strings.Contains(strings.ToUpper(s.Normalized), "TABLE") {
			if startLine-s.EndLine <= 15 && (trunc == nil || s.EndLine > trunc.EndLine) {
				trunc = s
			}
			continue
		}
		if trunc != nil && s.StartLine > trunc.EndLine && s.StartLine < startLine {
			return "" // another statement in between — not a rebuild pair
		}
	}
	if trunc == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trunc.Raw, "EXEC SQL"), ";"))
}
