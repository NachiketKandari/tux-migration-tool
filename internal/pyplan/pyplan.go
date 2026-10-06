// Package pyplan turns the batch flow skeleton into the Python generation
// plan (PRD 2026-09-08 BP-3): mechanical, deterministic naming for the SQL
// constants, the DAL functions / repository methods, and the service class —
// plus the CodeView (SQL regions replaced by call placeholders, dropped
// constructs elided) that the LLM seam consumes for stateful service bodies.
// Identical inputs produce a byte-identical plan.
package pyplan

import (
	"strconv"
	"strings"

	"tux-migration-tool/internal/batchflow"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/sqltext"
)

// Wrapper carries the configurable db-router conventions (BP-7): the import
// path, the router class, and the read/write mode spellings the generated
// code uses. Transactions are wrapper-owned (BP-5) — the generated code
// never calls commit/rollback itself.
type Wrapper struct {
	Import      string
	RouterClass string
	ReadMode    string
	WriteMode   string
}

// Options are the run's shape/loop knobs (BP-4) and naming conventions.
type Options struct {
	Shape        string // auto | repo (forced repository shape)
	DMLLoop      string // batch | rowbyrow
	ChunkSize    int
	LoggerPrefix string
	Entrypoint   string
	Wrapper      Wrapper
}

// QueryConst is one module-level SQL constant.
type QueryConst struct {
	ID    string
	Name  string
	Kind  string
	SQL   string
	Binds []string // binds of the executable SQL (INTO stripped), textual order, unique
}

// DALFn is one pure cursor function of the simple shape (mockable with a
// plain cursor double, the local-only reference shape).
type DALFn struct {
	Name    string
	Kind    string // fetch | dml
	Const   string
	BindIdx []int // DML: fetch-row indexes for the bind projection
	// RowShape is RESERVED as data on the DAL (the fetch template renders
	// whole rows); the repo-shape twin feeds the seam prompt.
	RowShape []string
	// QueryIDs are the IR query units this DAL function serves. One today —
	// Phase 1's consolidation is repository-shape only — but the field is
	// what lets the §6.1 census account for a simple-shape plan's queries
	// without a second, DAL-only accounting path.
	QueryIDs []string
}

// Phase is one cursor-batch phase of the simple-shape service.
type Phase struct {
	Index   int
	Method  string
	FetchFn string
	DMLFn   string
	Cursor  string
	Table   string
}

// RepoMethod is one repository method of the repository shape.
type RepoMethod struct {
	Name      string
	Kind      string // fetch | dml | rebuild
	QueryKind string // ir.QueryType of the backing query ("" for truncate halves)
	Consts    []string
	Binds     []string // kwarg bind names of the executable SQL
	RowShape  []string
	// InLoop is RESERVED as data — block assignment runs off SrcLine
	// ranges (Orchestration); InLoop stays for the diff/plan JSON review
	// surface (engine-wiring audit Tier-2 note).
	InLoop  bool
	SrcLine int // source line of the backing query (orchestration-block assignment)
	// MergedSrcLines are the source lines of the query units that folded
	// into this method (Phase 1 consolidation): identical statement, spelled
	// with different inter-token whitespace. Orchestration reads them so a
	// method whose sites sit in different dispatch arms is still required in
	// every block that calls it — on dtcmpt.pc one holiday probe folds from
	// lines 450, 608 and 965, i.e. from three different case arms.
	MergedSrcLines []int
	// MergedQueryIDs are the IR query ids folded into this method, the
	// retained one first. They are the fold's audit trail: every query id in
	// the flow must appear in exactly one method or one date-math entry.
	MergedQueryIDs []string
}

// calledIn reports whether any of the method's source sites falls inside the
// inclusive line range. It reads the folded sites as well as the retained one,
// which is what makes Phase 1's consolidation invisible to the seam contract:
// a method may serve several dispatch arms, and each of them must still list
// it under "must call".
func (m RepoMethod) calledIn(start, end int) bool {
	if m.SrcLine >= start && m.SrcLine <= end {
		return true
	}
	for _, l := range m.MergedSrcLines {
		if l >= start && l <= end {
			return true
		}
	}
	return false
}

// DateMathStmt is one `FROM DUAL` statement routed out of the repository
// (Phase 1 of docs/pybatchpy-logic-plan.md §7). It is a pure function of the
// values its binds already hold — `LAST_DAY`, `NEXT_DAY`, `±1 day`,
// `DECODE(SIGN(…))` — so it becomes `datetime`/`calendar` arithmetic in the
// service rather than a round trip to the database per occurrence.
//
// The entry keeps the statement's SQL, binds and the reason the elision was
// safe. It is not emitted as a constant, so nothing is lost: it stays in the
// plan and in the audit archive, and the §6.1 SQL accounting reconciles
// constants + date-math + folds against the query units the flow found.
type DateMathStmt struct {
	SQL      string
	Binds    []string // executable-SQL bind names, textual order
	QueryID  string
	SrcLine  int
	InLoop   bool
	Reason   string
	Op       string // recognised idiom family (last_day | next_day | offset | sign_decode | scalar)
	QueryIDs []string
}

// Plan is the deterministic Python generation plan for one batch program.
type Plan struct {
	Module      string
	ServiceName string
	ClassName   string
	RepoName    string
	RepoUsed    bool
	Shape       string // simple | repo
	DMLLoop     string
	ChunkSize   int
	LoggerName  string
	Entrypoint  string
	Wrapper     Wrapper
	Consts      []QueryConst
	DAL         []DALFn
	Phases      []Phase
	Repo        []RepoMethod
	// DateMath holds the `FROM DUAL` statements the repository does not own.
	// It is empty for the simple shape and for a batch with no dual queries.
	DateMath []DateMathStmt
	Flow     *batchflow.Flow

	constByQuery map[string]string // IR query id → constant name
	callByQuery  map[string]string // IR query id → DAL fn or repo method name
}

// Build assembles the plan from the flow and the run options. Shape: auto
// follows the rubric (simple → function DAL, stateful → repository); repo
// forces the repository shape (BP-4).
func Build(flow *batchflow.Flow, opts Options) *Plan {
	p := &Plan{
		Module:       flow.ServiceName,
		ServiceName:  flow.ServiceName,
		ClassName:    common.CamelPy(flow.ServiceName) + "Service",
		RepoName:     common.CamelPy(flow.ServiceName) + "Repository",
		Shape:        flow.Shape,
		DMLLoop:      opts.DMLLoop,
		ChunkSize:    opts.ChunkSize,
		LoggerName:   opts.LoggerPrefix + flow.ServiceName,
		Entrypoint:   opts.Entrypoint,
		Wrapper:      opts.Wrapper,
		Flow:         flow,
		constByQuery: map[string]string{},
		callByQuery:  map[string]string{},
	}
	if opts.Shape == "repo" || flow.Shape == batchflow.ShapeStateful {
		p.Shape = "repo"
	}
	if p.ChunkSize <= 0 {
		p.ChunkSize = 1000
	}

	used := map[string]bool{}
	take := func(base string) string {
		name := base
		for i := 2; used[strings.ToLower(name)]; i++ {
			name = base + "_" + strconv.Itoa(i)
		}
		used[strings.ToLower(name)] = true
		return name
	}

	for _, g := range flow.CursorGroups {
		sel := g.Select
		sc := take("SELECT_" + strings.ToUpper(g.CursorName) + "_QUERY")
		p.Consts = append(p.Consts, QueryConst{ID: sel.ID, Name: sc, Kind: string(sel.Type), SQL: emitSQL(sel), Binds: bindsOf(sel)})
		p.constByQuery[sel.ID] = sc
		if g.DML == nil {
			continue
		}
		dc := take(strings.ToUpper(verbOf(g.DML.Type)) + "_" + strings.ToUpper(g.CursorName) + "_QUERY")
		p.Consts = append(p.Consts, QueryConst{ID: g.DML.ID, Name: dc, Kind: string(g.DML.Type), SQL: emitSQL(g.DML), Binds: bindsOf(g.DML)})
		p.constByQuery[g.DML.ID] = dc
		if p.Shape == "repo" {
			continue // groups become repository methods below, not DAL phases
		}
		fname := "fetch_" + g.CursorName
		dname := verbOf(g.DML.Type) + "_" + g.CursorName
		p.DAL = append(p.DAL,
			DALFn{Name: fname, Kind: "fetch", Const: sc, RowShape: sel.RowShape, QueryIDs: []string{sel.ID}},
			DALFn{Name: dname, Kind: "dml", Const: dc, BindIdx: bindIdx(g.DML.Binds, sel.RowShape), QueryIDs: []string{g.DML.ID}},
		)
		p.callByQuery[sel.ID] = fname
		p.callByQuery[g.DML.ID] = dname
		p.Phases = append(p.Phases, Phase{
			Index: len(p.Phases) + 1, Method: "process_" + g.CursorName + "_phase",
			FetchFn: fname, DMLFn: dname, Cursor: g.CursorName, Table: firstTable(g.DML),
		})
	}

	if p.Shape != "repo" {
		return p
	}
	p.RepoUsed = true
	// Repository consolidation (Phase 1, docs/pybatchpy-logic-plan.md §7):
	// every query unit goes through repoBuilder, which folds units that are
	// the same statement and routes `FROM DUAL` arithmetic into the DateMath
	// bucket. Before this, each unit got its own constant and method.
	rb := newRepoBuilder(p, take)
	// Cursor groups → repository methods (forced-repo over a simple rubric
	// flow keeps every query, but the orchestration becomes the LLM seam's).
	for _, g := range flow.CursorGroups {
		rb.add(g.Select, "", false, strings.ToLower(g.CursorName))
		if g.DML != nil {
			rb.add(g.DML, "", batchflow.InLoop(flow.Loops, g.DML.StartLine), strings.ToLower(g.CursorName))
		}
	}
	for _, s := range flow.Steps {
		rb.add(s.Query, s.TruncateSQL, s.InLoop, firstTable(s.Query))
	}
	rb.finish()
	return p
}

// repoBuilder accumulates the repository-shape entries under Phase 1's two
// consolidation rules: one entry per distinct statement, and `FROM DUAL`
// arithmetic kept out of the repository entirely.
//
// Entries are built out of line and appended to the Plan only at finish, so a
// fold can record itself on the retained entry without aliasing a slice
// element that a later append would move.
type repoBuilder struct {
	p      *Plan
	take   func(base string) string
	byKey  map[string]*repoEntry // fold key → retained entry
	truncs map[string]string     // truncate text key → constant name
	order  []*repoEntry          // source order, which is the emitted order
}

// repoEntry is one retained statement, under construction. It is held here
// rather than in Plan.Repo until finish, so a fold can append to it without
// aliasing a slice element that a later append would move.
type repoEntry struct {
	method RepoMethod
}

func newRepoBuilder(p *Plan, take func(base string) string) *repoBuilder {
	return &repoBuilder{
		p: p, take: take,
		byKey:  map[string]*repoEntry{},
		truncs: map[string]string{},
	}
}

// add folds one query unit into the plan. truncate is the raw paired TRUNCATE
// statement when there is one, inLoop the flow's loop-membership flag, and
// scope the naming base the caller derived (a cursor name, or the table).
func (b *repoBuilder) add(q *ir.Query, truncate string, inLoop bool, scope string) {
	sql := emitSQL(q)
	binds := bindsOf(q)
	if isDateMath(q, sql, binds) {
		b.addDateMath(q, sql, binds, inLoop)
		return
	}
	truncConst := b.truncateConst(q, truncate)
	key := foldKey(sql, rowShapeOf(q), string(q.Type), truncConst != "")
	if e, ok := b.byKey[key]; ok {
		b.foldInto(e, q)
		return
	}
	e := &repoEntry{method: b.newMethod(q, sql, binds, truncConst, inLoop, scope)}
	b.byKey[key] = e
	b.order = append(b.order, e)
}

// newMethod builds a fresh repository entry: its constant (plus the truncate
// half when paired) and the method that owns them.
func (b *repoBuilder) newMethod(q *ir.Query, sql string, binds []string, truncConst string, inLoop bool, scope string) RepoMethod {
	nameScope := scope
	if dualOnly(q) {
		// `FROM DUAL` is not a table: name after the value the statement
		// returns so the repository never grows a `fetch_dual*` family.
		nameScope = common.PyIdent(dualScope(q))
	}
	cn := b.take(strings.ToUpper(verbOf(q.Type)) + "_" + strings.ToUpper(nameScope) + "_QUERY")
	b.p.Consts = append(b.p.Consts, QueryConst{ID: q.ID, Name: cn, Kind: string(q.Type), SQL: sql, Binds: binds})
	b.p.constByQuery[q.ID] = cn
	m := RepoMethod{
		Name: b.take(verbOf(q.Type) + "_" + nameScope),
		Kind: verbOf(q.Type), QueryKind: string(q.Type), Consts: []string{cn},
		Binds: binds, RowShape: q.RowShape, InLoop: inLoop, SrcLine: q.StartLine,
		MergedQueryIDs: []string{q.ID},
	}
	if truncConst != "" {
		m.Kind = "rebuild"
		m.Consts = []string{truncConst, cn}
		m.Name = b.take("rebuild_" + nameScope)
	}
	b.p.callByQuery[q.ID] = m.Name
	return m
}

// foldInto merges a query unit into a retained entry. The folded unit shares
// the entry's constant and method — same token sequence by construction — so
// the CodeView placeholder, the retention stubs and the orchestration contract
// still name a real call at the folded site's line. What is *recorded* is the
// unit's id and source line: the fold is visible, not silent.
func (b *repoBuilder) foldInto(e *repoEntry, q *ir.Query) {
	e.method.MergedSrcLines = append(e.method.MergedSrcLines, q.StartLine)
	e.method.MergedQueryIDs = append(e.method.MergedQueryIDs, q.ID)
	// The folded unit's own constant is the entry's *statement* constant,
	// which is the last one: a rebuild method's first entry is its truncate
	// half.
	b.p.constByQuery[q.ID] = e.method.Consts[len(e.method.Consts)-1]
	b.p.callByQuery[q.ID] = e.method.Name
}

// addDateMath records one `FROM DUAL` arithmetic statement as a date-math
// entry. No constant and no method: the service computes it. The query id is
// deliberately left out of callByQuery so the retention report and the seam
// contract stop asking for a repository call that does not exist.
func (b *repoBuilder) addDateMath(q *ir.Query, sql string, binds []string, inLoop bool) {
	b.p.DateMath = append(b.p.DateMath, DateMathStmt{
		SQL: sql, Binds: binds, QueryID: q.ID, SrcLine: q.StartLine, InLoop: inLoop,
		Op: dateMathOp(sql), Reason: dateMathReason(binds), QueryIDs: []string{q.ID},
	})
}

// truncateConst returns the constant name for a paired TRUNCATE half,
// creating it once per distinct statement. A truncate half carries no query
// id of its own, so it folds on its text alone.
func (b *repoBuilder) truncateConst(q *ir.Query, truncate string) string {
	if truncate == "" || q.Type != ir.QueryInsert {
		return ""
	}
	key := foldKey(truncate, "", "TRUNCATE", false)
	if name, ok := b.truncs[key]; ok {
		return name
	}
	name := b.take("TRUNCATE_" + strings.ToUpper(firstTable(q)) + "_QUERY")
	b.p.Consts = append(b.p.Consts, QueryConst{ID: q.ID + "-trunc", Name: name, Kind: "TRUNCATE", SQL: truncate})
	b.truncs[key] = name
	return name
}

// finish publishes the accumulated entries onto the plan in source order.
func (b *repoBuilder) finish() {
	for _, e := range b.order {
		b.p.Repo = append(b.p.Repo, e.method)
	}
}

// ConstName returns the SQL constant backing an IR query id.
func (p *Plan) ConstName(queryID string) string { return p.constByQuery[queryID] }

// CallName returns the DAL fn / repo method name backing an IR query id.
func (p *Plan) CallName(queryID string) string { return p.callByQuery[queryID] }

// verbOf maps a query type to its Python verb (fetch for SELECTs).
// It delegates to the contract kind's shared verb (uniform-ir plan
// §3.3/Phase 4): select_one/select_many → fetch, DML kinds → their
// lowercase name — identical to the old switch by construction.
func verbOf(t ir.QueryType) string {
	switch contract.QueryKindOf(t) {
	case contract.QuerySelectOne, contract.QuerySelectMany:
		return "fetch"
	default:
		return strings.ToLower(string(contract.QueryKindOf(t)))
	}
}

func firstTable(q *ir.Query) string {
	if len(q.Tables) == 0 {
		return "row"
	}
	// DB-link-qualified tables (SCHEME@CONTENT_DB) and schema-qualified
	// names sanitize to Python-safe identifiers.
	return strings.ToLower(common.PyIdent(q.Tables[0]))
}

// pyIdent coerces a C/SQL identifier fragment into a Python-safe one:
// non-alphanumerics become underscores.
// emitSQL renders the IR's canonical SQL as executable oracledb text (the
// documented BP-3 rubric transforms, tolerance-matched in sqlchk's
// normalizer): Pro*C host-variable INTO lists are stripped from SELECTs and
// `: name` collapses to `:name`. It delegates to sqltext.CanonicalSQL
// (uniform-ir plan §3.2: one canonicalizer for every backend);
// CanonicalSQL additionally trims one trailing semicolon, which oracledb
// forbids in executable text.
func emitSQL(q *ir.Query) string {
	return sqltext.CanonicalSQL(q.SQL)
}

// bindsOf returns the bind names of the EXECUTABLE SQL (emitSQL: the INTO
// host-target list is stripped from SELECTs) — the exact names a caller
// must supply for oracledb's named binds. Binds derived from the raw
// source SQL would count INTO targets as binds and mismatch the const at
// runtime (ORA-01008 class). It delegates to sqltext.ExecutableBinds
// (uniform-ir plan §3.2).
func bindsOf(q *ir.Query) []string { return sqltext.ExecutableBinds(emitSQL(q)) }

// bindOrder returns the unique bind names in textual order of appearance
// (`: name` with whitespace counts — Pro*C tolerates the space).
// It delegates to sqltext.ExecutableBinds; kept as a named wrapper while
// callers migrate (uniform-ir plan §3.2).
func bindOrder(sql string) []string {
	return sqltext.ExecutableBinds(sql)
}

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// bindIdx maps the DML's textual bind order onto the fetch row tuple.
func bindIdx(binds, rowShape []string) []int {
	idx := map[string]int{}
	for i, r := range rowShape {
		idx[strings.ToLower(r)] = i
	}
	out := make([]int, 0, len(binds))
	for _, b := range binds {
		out = append(out, idx[strings.ToLower(b)])
	}
	return out
}

// Camel converts snake/dashed identifiers to CamelCase (bat_px_demo_rt →
// BatPxDemoRt).
// CodeView renders the entry body for the LLM seam (BP-6, the batch analogue
// of the query-replaced branch view): every EXEC SQL span becomes one
// placeholder line naming the deterministic call or cursor op, dropped
// constructs collapse to marked comments, and the surrounding control flow
// (loops, branches, assignments, logs) stays verbatim.
func (p *Plan) CodeView(src string) string {
	entries := p.codeViewEntries(src)
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Text
	}
	return strings.Join(texts, "\n")
}

// viewEntry is one CodeView line with its source line number.
type viewEntry struct {
	Line int
	Text string
}

// OrchestrationBlock is one phase of the orchestration contract (BP-6): a
// contiguous control region of the entry body — the setup before any loop,
// then each loop — with the repository methods whose source lines fall
// inside it. The seam must implement every block, in order, calling every
// listed method under the condition the view shows.
type OrchestrationBlock struct {
	Index   int
	Name    string
	Methods []string
	View    string
}

// Orchestration slices the CodeView into the per-block contract.
func (p *Plan) Orchestration(src string) []OrchestrationBlock {
	entries := p.codeViewEntries(src)
	type rng struct {
		start, end int
		name       string
	}
	var ranges []rng
	if len(p.Flow.Loops) == 0 {
		ranges = append(ranges, rng{p.Flow.BodyStart, p.Flow.BodyEnd, "body"})
	} else {
		if first := p.Flow.Loops[0].StartLine; first-1 >= p.Flow.BodyStart {
			ranges = append(ranges, rng{p.Flow.BodyStart, first - 1, "setup (before any loop)"})
		}
		for i, l := range p.Flow.Loops {
			if i > 0 {
				prev := p.Flow.Loops[i-1].EndLine
				if l.StartLine-1 > prev {
					ranges = append(ranges, rng{prev + 1, l.StartLine - 1,
						"between loops (source " + strconv.Itoa(prev+1) + "-" + strconv.Itoa(l.StartLine-1) + ")"})
				}
			}
			ranges = append(ranges, rng{l.StartLine, l.EndLine,
				"loop `" + l.Header + "` (source " + strconv.Itoa(l.StartLine) + "-" + strconv.Itoa(l.EndLine) + ")"})
		}
		if tail := p.Flow.Loops[len(p.Flow.Loops)-1].EndLine; tail+1 <= p.Flow.BodyEnd {
			ranges = append(ranges, rng{tail + 1, p.Flow.BodyEnd,
				"epilogue (source " + strconv.Itoa(tail+1) + "-" + strconv.Itoa(p.Flow.BodyEnd) + ")"})
		}
	}
	blocks := make([]OrchestrationBlock, 0, len(ranges))
	for i, r := range ranges {
		b := OrchestrationBlock{Index: i + 1, Name: r.name}
		var texts []string
		for _, e := range entries {
			if e.Line >= r.start && e.Line <= r.end {
				texts = append(texts, e.Text)
			}
		}
		b.View = strings.Join(texts, "\n")
		for _, m := range p.Repo {
			// A folded method must be required in every block that calls it,
			// not just the block holding its retained site's line: on
			// dtcmpt.pc the holiday probe folds from three lines in three
			// different switch arms.
			if m.calledIn(r.start, r.end) {
				b.Methods = append(b.Methods, m.Name)
			}
		}
		if b.View != "" || len(b.Methods) > 0 {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// codeViewEntries renders the entry body line-by-line with EXEC SQL spans
// replaced by placeholders and dropped statements elided; Line is the source
// line each output line came from.
func (p *Plan) codeViewEntries(src string) []viewEntry {
	lines := strings.Split(src, "\n")
	placeholder := map[int]string{}
	sqlLine := map[int]bool{}
	for _, s := range p.Flow.SQLSpans {
		if s.StartLine < p.Flow.BodyStart || s.StartLine > p.Flow.BodyEnd {
			continue
		}
		if text := p.sqlPlaceholder(s); text != "" {
			placeholder[s.StartLine] = text
		}
		for l := s.StartLine; l <= s.EndLine; l++ {
			sqlLine[l] = true
		}
	}
	hidden := map[int]bool{}
	for _, d := range p.Flow.Dropped {
		start, end := dropSpan(lines, d.Line)
		for l := start; l <= end; l++ {
			hidden[l] = true
		}
		placeholder[start] = "# dropped [" + string(d.Kind) + "]: " + d.Call
	}
	var out []viewEntry
	for l := p.Flow.BodyStart; l <= p.Flow.BodyEnd && l-1 < len(lines); l++ {
		if sqlLine[l] || hidden[l] {
			continue
		}
		if text, ok := placeholder[l]; ok {
			out = append(out, viewEntry{Line: l, Text: common.Leading(lines[l-1]) + text})
			continue
		}
		out = append(out, viewEntry{Line: l, Text: lines[l-1]})
	}
	return out
}

// sqlPlaceholder maps one EXEC SQL span to its CodeView placeholder line.
func (p *Plan) sqlPlaceholder(s batchflow.SQLSpan) string {
	switch s.Kind {
	case "DECLARE_CURSOR":
		q := p.cursorSelect(s.CursorName)
		if q == nil {
			return "# cursor " + s.CursorName + ": DECLARE"
		}
		return "# cursor " + s.CursorName + ": DECLARE → const " + p.ConstName(q.ID) +
			" (row: " + rowNames(q) + ")"
	case "OPEN":
		return "# cursor " + s.CursorName + ": OPEN"
	case "FETCH":
		q := p.cursorSelect(s.CursorName)
		call := ""
		if q != nil {
			call = " (calls " + p.CallName(q.ID) + ")"
		}
		return "# cursor " + s.CursorName + ": FETCH row" + call
	case "CLOSE":
		return "# cursor " + s.CursorName + ": CLOSE"
	case "COMMIT", "ROLLBACK":
		return "# " + strings.ToLower(s.Kind) + " — owned by " + p.Wrapper.RouterClass + " context manager"
	}
	// Query spans: match the IR unit by start line.
	for _, q := range p.Flow.Queries {
		if q.StartLine != s.StartLine {
			continue
		}
		if call := p.CallName(q.ID); call != "" {
			return "# " + call + "(" + strings.Join(kwargBinds(bindsOf(q)), ", ") + ")"
		}
		return "# SQL " + string(q.Type)
	}
	if strings.Contains(strings.ToUpper(s.Normal), "TRUNCATE") {
		return "# TRUNCATE — folded into the paired rebuild method"
	}
	return ""
}

func rowNames(q *ir.Query) string {
	if len(q.RowShape) == 0 {
		return "row"
	}
	return strings.Join(q.RowShape, ", ")
}

func kwargBinds(binds []string) []string {
	out := make([]string, len(binds))
	for i, b := range binds {
		out[i] = b + "=" + b
	}
	return out
}

func (p *Plan) cursorSelect(name string) *ir.Query {
	for _, g := range p.Flow.CursorGroups {
		if g.CursorName == name {
			return g.Select
		}
	}
	return nil
}

// dropSpan returns the statement extent of a dropped call: from its line
// until parens rebalance and a statement terminator appears.
func dropSpan(lines []string, line int) (int, int) {
	depth := 0
	seen := false
	for l := line; l <= len(lines); l++ {
		text := lines[l-1]
		for i := 0; i < len(text); i++ {
			switch text[i] {
			case '"', '\'':
				i = skipQuoted(text, i)
			case '(':
				depth++
				seen = true
			case ')':
				depth--
			case ';':
				if seen && depth <= 0 {
					return line, l
				}
			}
		}
		if seen && depth <= 0 {
			return line, l
		}
	}
	return line, line
}

func skipQuoted(text string, i int) int {
	q := text[i]
	for j := i + 1; j < len(text); j++ {
		if text[j] == '\\' {
			j++
			continue
		}
		if text[j] == q {
			return j
		}
	}
	return len(text) - 1
}
