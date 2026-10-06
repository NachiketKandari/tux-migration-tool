// Package pycensus is the batchpy accounting census (docs/pybatchpy-logic-plan.md
// §6.1). It answers one question with counters instead of judgement: did every
// EXEC SQL region and every non-SQL statement the batch entry body carries
// land in exactly one accounted bucket?
//
// Accounting proves nothing was *lost*. It cannot prove nothing was *changed*
// — that is the differential oracle's job (§6.2). What it does buy is turning
// today's silent loss into a number: the drop table (`batchflow.dropKinds`) is
// undifferentiated and silent, so a construct nobody classified — `exit` on
// dtcmpt.pc — leaves no trace at all. This package is where it leaves one.
//
// Phase 0 of the plan lands it report-only: Compute never gates and never
// changes emitted output. Phase 3 promotes Findings into hard failures.
//
// It imports only the Python path's own packages (batchflow, pyplan) plus the
// shared read-only scanner/IR, and nothing in internal/gen, internal/csgen or
// internal/csplan can reach it — the same additive-isolation discipline
// docs/uniform-ir-plan.md sets for shared IR fields. That is what keeps the Go
// and C# targets regression-free while the batch path evolves.
package pycensus

import (
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/batchflow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pyplan"
	scanner "tux-migration-tool/internal/tsscan"
)

// Bucket is the accounting home of one non-SQL statement (§6.1 gate 2). Every
// recorded call must land in exactly one of these. Unknown is permitted to
// exist in the type so the census can *report* it; it is never permitted to be
// non-zero at promotion time — that is precisely where today's silent loss
// lives.
type Bucket string

const (
	// BucketTranslated: the statement becomes service logic — control flow,
	// assignments, string arithmetic, local function calls. Recognised via
	// the CodeView, which keeps these lines verbatim.
	BucketTranslated Bucket = "translated"
	// BucketElided: deliberately dropped with a stated justification (FML
	// buffer plumbing, debug-level config, process exit). The reason is
	// carried per site so the elision is disclosed, not silent.
	BucketElided Bucket = "elided-justified"
	// BucketTuxedoRuntime: tp* transaction/lifecycle plumbing, owned by the
	// wrapper seam (BP-5) rather than the generated body.
	BucketTuxedoRuntime Bucket = "tuxedo-runtime"
	// BucketLogOnly: userlog/errlog/printf — logging parity, not logic.
	BucketLogOnly Bucket = "log-only"
	// BucketUnknown: no rule matched. Must be zero.
	BucketUnknown Bucket = "unknown"
)

// buckets is the canonical report order — the four accounted buckets plus the
// residue, so a reader sees the sum before the failure.
var buckets = []Bucket{BucketTranslated, BucketElided, BucketTuxedoRuntime, BucketLogOnly, BucketUnknown}

// SQLClass is the accounting class of one EXEC SQL region (§6.1 gate 1).
type SQLClass string

const (
	// SQLQuery is a logical statement: SELECT, DECLARE CURSOR, and DML.
	SQLQuery SQLClass = "query"
	// SQLChoreo is cursor movement (OPEN/FETCH/CLOSE) — structure, not a
	// statement; it becomes a `for row in repo.fetch…()`.
	SQLChoreo SQLClass = "cursor-choreo"
	// SQLPlumbing is transaction/session scope (COMMIT/ROLLBACK/CONNECT/
	// DECLARE SECTION) — owned by the router context manager.
	SQLPlumbing SQLClass = "plumbing"
	// SQLInclude is `EXEC SQL INCLUDE`, a text splice, not a statement.
	SQLInclude SQLClass = "include"
)

var sqlClasses = []SQLClass{SQLQuery, SQLChoreo, SQLPlumbing, SQLInclude}

// SQLAccounting is gate 1: every EXEC SQL region in the entry body is
// classified, and the query sites are reconciled against what the plan
// actually emitted. Residue must be zero — a query site with no emitted
// constant is a lost statement.
type SQLAccounting struct {
	Sites      int            `json:"sites"`       // in-body EXEC SQL regions
	ByClass    map[string]int `json:"by_class"`    // sites per SQLClass
	ByKind     map[string]int `json:"by_kind"`     // sites per scanner kind
	QuerySites int            `json:"query_sites"` // sites in the query classes
	// QueryUnique is the distinct query units the flow kept (the IR
	// already folded byte-identical text); QuerySites-QueryUnique counts
	// the folds.
	QueryUnique int `json:"query_unique"`
	// Constants is the emitted SQL-constant count, RepoMethods the
	// repository methods built from them, DateMath the `FROM DUAL`
	// side-effect-free statements Phase 1 routed to the date-math bucket
	// instead of a repository method.
	Constants   int `json:"constants"`
	RepoMethods int `json:"repo_methods"`
	DateMath    int `json:"date_math"`
	// DateMathByOp counts the elided statements per recognised idiom family
	// (§5's `FROM DUAL` row) — the completion counter Phase 3 promotes.
	DateMathByOp map[string]int `json:"date_math_by_op,omitempty"`
	// Folded is the number of query units that share a retained entry's
	// statement identity (Phase 1 consolidation: identical token sequence,
	// different inter-token whitespace).
	Folded int `json:"folded"`
	// Accounted is the number of query units the plan placed somewhere — a
	// repository method or a date-math entry, counting a folded unit once
	// per entry that claims it. It must equal QueryUnique.
	//
	// Unaccounted and Duplicated are the two ways it can fail, and both are
	// invisible to a constant count: a fold can under-count by dropping a
	// unit, and a mis-scoped fold can over-count by claiming one twice.
	Accounted   int      `json:"accounted"`
	Unaccounted []string `json:"unaccounted,omitempty"`
	Duplicated  []string `json:"duplicated,omitempty"`
	// Residue is a query site that reached no constant. Must be zero.
	Residue int `json:"residue"`
}

// StatementAccounting is gate 2: every recorded call in the entry body has
// exactly one bucket. Unlisted names the constructs no rule covered — that
// list is the whole point of the census.
type StatementAccounting struct {
	Total    int            `json:"total"`
	ByBucket map[string]int `json:"by_bucket"`
	// Unlisted is the sorted set of call names that matched no rule. Each
	// one is a construct the drop table silently ignores.
	Unlisted []string `json:"unlisted,omitempty"`
	// Guards lists registration calls whose return value is compared —
	// concurrency controls that share a bucket with plain logging and must
	// not be lost inside it (§3.2).
	Guards []string `json:"guards,omitempty"`
	// ElidedBy names the per-kind justification recorded for elided sites.
	ElidedBy map[string]string `json:"elided_by,omitempty"`
}

// DispatchAccounting is gate 3: every `case` label in the C switch must
// appear as a key of the emitted handler mapping. A 'D' arm that silently
// vanishes must fail.
type DispatchAccounting struct {
	Switches    int      `json:"switches"`
	CaseLabels  []string `json:"case_labels,omitempty"`
	EmittedKeys []string `json:"emitted_keys,omitempty"`
	Missing     []string `json:"missing,omitempty"`
}

// LoopAccounting is gate 4: a loop is only safe to emit as `range()`-bounded
// or explicitly capped with a recorded reason. The C guards this plan is
// worried about terminate by luck, not by proof.
type LoopAccounting struct {
	Total      int      `json:"total"`
	Bounded    int      `json:"bounded"`    // for-header with a countable trip
	Guarded    int      `json:"guarded"`    // while(cond): terminates on data
	Indefinite int      `json:"indefinite"` // while(TRUE)-style: needs a break/exit
	Uncapped   []string `json:"uncapped,omitempty"`
	ExitSites  int      `json:"exit_sites"` // break/return/goto inside loop bodies
}

// UninitRead is one variable declared in the entry body, never assigned, and
// read before any write — the `i_row` class of defect (§6.1 gate 5). A row
// that inherits the previous iteration's value is a silent correctness bug no
// syntax gate can see.
type UninitRead struct {
	Var  string `json:"var"`
	Line int    `json:"line"`
	Type string `json:"type,omitempty"`
}

// Loop is one counted loop site.
type Loop struct {
	Header  string
	Line    int
	EndLine int
	Kind    string
	Bounded bool
	Capped  bool
	Reason  string
}

// Switch is one counted switch site.
type Switch struct {
	Cond     string
	Line     int
	EndLine  int
	CaseKeys []string
}

// Call is one counted call site with its bucket and justification.
type Call struct {
	Name   string
	Line   int
	Bucket Bucket
	Reason string
}

// Census is the whole report-only picture for one batch program.
type Census struct {
	SQL        SQLAccounting       `json:"sql"`
	Statements StatementAccounting `json:"statements"`
	Dispatch   DispatchAccounting  `json:"dispatch"`
	Loops      LoopAccounting      `json:"loops"`
	Uninit     []UninitRead        `json:"uninitialised_reads,omitempty"`
	// Calls and LoopsSites/SwitchesSites keep the per-site detail for the
	// audit archive; Counts is what a human reads.
	Calls       []Call   `json:"calls,omitempty"`
	LoopSites   []Loop   `json:"loop_sites,omitempty"`
	SwitchSites []Switch `json:"switch_sites,omitempty"`
	Findings    []string `json:"findings,omitempty"`
}

// Input is the one bundle Compute needs. Every field is already produced by
// the existing pipeline — this package invents no new extraction.
type Input struct {
	Flow  *batchflow.Flow
	Plan  *pyplan.Plan
	Facts *scanner.SourceFacts
	Lines []string
}

// exitCalls are process-termination calls. They are absent from
// batchflow.dropKinds — 38 sites on dtcmpt.pc — which is precisely why the
// census exists: an unclassified construct that leaves no trace anywhere.
var exitCalls = map[string]bool{"exit": true, "_exit": true, "abort": true, "longjmp": true}

// idiomCalls are the batch Pro*C vocabulary (§5): plain C library calls that
// become service logic through the CodeView, not runtime plumbing.
var idiomCalls = map[string]bool{
	"strcpy": true, "strncpy": true, "strcmp": true, "strncmp": true, "strlen": true,
	"sprintf": true, "snprintf": true, "strcat": true, "strstr": true, "strtok": true,
	"rtrim": true, "ltrim": true, "trim": true, "atol": true, "atoi": true, "atof": true,
	"strtol": true, "strtoul": true, "toupper": true, "tolower": true, "abs": true,
	"memcmp": true, "memset": true, "malloc": true, "free": true, "calloc": true,
}

// Compute derives the census. Deterministic: identical inputs produce an
// identical Census, and nothing here consults the generated module — Phase 0
// changes no output.
func Compute(in Input) Census {
	c := Census{
		SQL:        SQLAccounting{ByClass: map[string]int{}, ByKind: map[string]int{}},
		Statements: StatementAccounting{ByBucket: map[string]int{}, ElidedBy: map[string]string{}},
	}
	if in.Flow == nil {
		return c
	}
	c.SQL = sqlAccounting(in)
	c.Statements, c.Calls = statementAccounting(in)
	c.Loops, c.LoopSites = loopAccounting(in)
	c.Dispatch, c.SwitchSites = dispatchAccounting(in)
	c.Uninit = uninitReads(in)
	c.Findings = findings(c)
	return c
}

// sqlAccounting classifies every in-body EXEC SQL region and reconciles the
// query sites against the emitted constants.
func sqlAccounting(in Input) SQLAccounting {
	a := SQLAccounting{ByClass: map[string]int{}, ByKind: map[string]int{}}
	for _, s := range in.Flow.SQLSpans {
		a.Sites++
		a.ByKind[s.Kind]++
		class := classOf(s.Kind)
		a.ByClass[string(class)]++
		if class == SQLQuery {
			a.QuerySites++
		}
	}
	a.QueryUnique = len(in.Flow.Queries)
	if a.QuerySites > a.QueryUnique {
		a.Folded += a.QuerySites - a.QueryUnique
	}
	if in.Plan != nil {
		a.Constants = len(in.Plan.Consts)
		a.RepoMethods = len(in.Plan.Repo)
		a.DateMath = len(in.Plan.DateMath)
		a.DateMathByOp = dateMathOps(in.Plan.DateMath)
		a.Folded += queryUnits(in.Plan) - a.Constants - a.DateMath
		placed := placedQueryIDs(in.Plan)
		a.Accounted = len(placed)
		a.Unaccounted = unplaced(in.Flow.Queries, placed)
		a.Duplicated = duplicated(placed)
		// Residue is the count a human reads: a query unit that reached no
		// plan entry, or that two entries claimed. Both mean a statement was
		// lost. It is deliberately NOT `query_unique - constants`: once Phase 1
		// folds identical statements, one repository entry legitimately
		// covers several query units, and a date-math entry legitimately
		// covers none.
		a.Residue = len(a.Unaccounted) + len(a.Duplicated)
	}
	return a
}

// queryUnits is the number of IR query units the plan accounted for, counting
// a folded unit once per entry that claims it.
func queryUnits(p *pyplan.Plan) int {
	n := 0
	for _, fn := range p.DAL {
		n += len(fn.QueryIDs)
	}
	for _, m := range p.Repo {
		n += len(m.MergedQueryIDs)
	}
	for _, d := range p.DateMath {
		n += len(d.QueryIDs)
	}
	return n
}

// placedQueryIDs collects the query ids every plan entry claims — DAL
// function (simple shape), repository method (repo shape) or date-math entry —
// together with how many entries claimed each. Two entries claiming one unit
// is a fold that crossed a real difference.
func placedQueryIDs(p *pyplan.Plan) map[string]int {
	out := map[string]int{}
	for _, fn := range p.DAL {
		for _, id := range fn.QueryIDs {
			out[id]++
		}
	}
	for _, m := range p.Repo {
		for _, id := range m.MergedQueryIDs {
			out[id]++
		}
	}
	for _, d := range p.DateMath {
		for _, id := range d.QueryIDs {
			out[id]++
		}
	}
	return out
}

// unplaced lists the flow's query units no entry claimed.
func unplaced(queries []*ir.Query, placed map[string]int) []string {
	var out []string
	for _, q := range queries {
		if placed[q.ID] == 0 {
			out = append(out, q.ID)
		}
	}
	sort.Strings(out)
	return out
}

// duplicated lists the query units more than one entry claimed.
func duplicated(placed map[string]int) []string {
	var out []string
	for id, n := range placed {
		if n > 1 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// dateMathOps counts the elided date-math statements per recognised idiom.
func dateMathOps(stmts []pyplan.DateMathStmt) map[string]int {
	if len(stmts) == 0 {
		return nil
	}
	out := map[string]int{}
	for _, d := range stmts {
		out[d.Op]++
	}
	return out
}

// classOf maps a scanner SQL kind to its accounting class.
func classOf(kind string) SQLClass {
	switch kind {
	case "SELECT", "DECLARE_CURSOR", "INSERT", "UPDATE", "DELETE", "MERGE":
		return SQLQuery
	case "OPEN", "FETCH", "CLOSE":
		return SQLChoreo
	case "INCLUDE":
		return SQLInclude
	default:
		return SQLPlumbing
	}
}

// statementAccounting buckets every call site in the entry body. The rule
// table is deliberately explicit: an unmatched name lands in Unlisted rather
// than being quietly tolerated.
func statementAccounting(in Input) (StatementAccounting, []Call) {
	a := StatementAccounting{ByBucket: map[string]int{}, ElidedBy: map[string]string{}}
	var calls []Call
	if in.Facts == nil {
		return a, calls
	}
	// Pre-seed every bucket to zero so a report always shows the full
	// partition — an absent key would read as "not measured".
	for _, b := range buckets {
		a.ByBucket[string(b)] = 0
	}
	// A registration call whose result is assigned to a variable that a
	// branch then compares is a guard, not a bare statement:
	//
	//	i_returncode = fn_rgstr_bat(...);   // :157
	//	if (i_returncode != 1) { … }        // :159
	//
	// §3.2: that is a single-instance concurrency control, and it must not
	// hide inside the same silent bucket as the log call. Matching on the
	// call name alone would miss this shape entirely, so the assignment is
	// resolved to its target first.
	guarded := guardedCalls(in)
	for _, c := range in.Facts.Calls {
		if c.Line < in.Flow.BodyStart || c.Line > in.Flow.BodyEnd {
			continue
		}
		site := Call{Name: c.Name, Line: c.Line}
		switch {
		case c.Name == "userlog" || c.Name == "printf":
			site.Bucket = BucketLogOnly
		case exitCalls[c.Name]:
			// Process termination: a C `exit(0)` on an error path becomes
			// raise; the success `exit(1)` becomes return. Disclosed, not
			// silent.
			site.Bucket = BucketElided
			site.Reason = "process termination becomes raise/return"
			a.ElidedBy[c.Name] = site.Reason
		case scanner.IsTpCallName(c.Name):
			site.Bucket = BucketTuxedoRuntime
			site.Reason = "tpcall becomes a direct repository/service call"
		case guarded[c.Name]:
			site.Bucket = BucketElided
			site.Reason = "registration return value compared — concurrency guard, not logging"
			a.ElidedBy[c.Name] = site.Reason
		default:
			kind, ok := dropKindOf(c.Name)
			if !ok {
				// A function defined in this same file is part of the
				// batch's own logic — it becomes a service method
				// (func_spill_over_date on dtcmpt.pc). A C library
				// idiom becomes service logic too. Anything else is
				// genuinely unaccounted for.
				if idiomCalls[c.Name] || locallyDefined(in.Facts, c.Name) {
					site.Bucket = BucketTranslated
					break
				}
				site.Bucket = BucketUnknown
				a.Unlisted = appendUnique(a.Unlisted, c.Name)
				break
			}
			site.Bucket = bucketOfDropKind(kind)
			if site.Bucket == BucketElided {
				site.Reason = elisionReason(kind)
				a.ElidedBy[c.Name] = site.Reason
			}
		}
		calls = append(calls, site)
		a.Total++
		a.ByBucket[string(site.Bucket)]++
	}
	for _, d := range in.Flow.Dropped {
		if d.Line < in.Flow.BodyStart || d.Line > in.Flow.BodyEnd {
			continue
		}
		if guarded[d.Call] {
			a.Guards = append(a.Guards, strconv.Itoa(d.Line)+": "+d.Call)
		}
	}
	sort.Strings(a.Unlisted)
	sort.Strings(a.Guards)
	return a, calls
}

// guardedCalls returns the registration call names whose return value a
// branch condition compares, directly or through an assignment target.
func guardedCalls(in Input) map[string]bool {
	out := map[string]bool{}
	// Variables a registration call writes, so `if (v != 1)` can be
	// attributed back to the call.
	assigned := map[string]string{}
	if len(in.Lines) > 0 {
		for l := in.Flow.BodyStart; l <= in.Flow.BodyEnd && l-1 < len(in.Lines); l++ {
			text := in.Lines[l-1]
			for _, name := range registrationNames {
				at := strings.Index(text, name)
				if at < 0 {
					continue
				}
				// `lhs = name(` — the identifier immediately left of the
				// `=` is the assignment target.
				left := strings.TrimRight(text[:at], " \t")
				if !strings.HasSuffix(left, "=") || strings.HasSuffix(left, "==") {
					continue
				}
				left = strings.TrimRight(left[:len(left)-1], " \t")
				if v, ok := lastIdent(left); ok {
					assigned[v] = name
				}
			}
		}
	}
	for _, br := range in.Facts.Branches {
		for _, name := range registrationNames {
			if mentions(br.Cond, name) {
				out[name] = true
			}
		}
		for v, name := range assigned {
			if mentions(br.Cond, v) {
				out[name] = true
			}
		}
	}
	return out
}

// lastIdent returns the final identifier token of a string.
func lastIdent(s string) (string, bool) {
	s = strings.TrimRight(s, " \t")
	i := len(s)
	for i > 0 && isIdentByte(s[i-1]) {
		i--
	}
	if i == len(s) || i == 0 {
		return "", false
	}
	return s[i:], true
}

// registrationNames is the deterministic ordering of the registration entry
// points, so the census is order-independent.
var registrationNames = []string{"fn_rgstr_bat", "fn_bat_pst_msg"}

// locallyDefined reports whether the name is a function this file defines.
// Those calls are the batch's own logic (func_spill_over_date), not an
// unresolved external — §3.2's "already in SourceFacts.Functions" case.
func locallyDefined(facts *scanner.SourceFacts, name string) bool {
	for _, f := range facts.Functions {
		if f.Name == name {
			return true
		}
	}
	return false
}

// registrationCalls are the batch-registration entry points whose return
// value is the single-instance guard.
var registrationCalls = map[string]bool{"fn_rgstr_bat": true, "fn_bat_pst_msg": true}

// bucketOfDropKind maps a batchflow drop kind onto its census bucket. The
// split is the §3.2 fix: the drop table lumps four different decisions under
// one silent bucket.
func bucketOfDropKind(k batchflow.DropKind) Bucket {
	switch k {
	case batchflow.DropTuxedo:
		return BucketTuxedoRuntime
	case batchflow.DropErrorLog:
		return BucketLogOnly
	default:
		return BucketElided
	}
}

// elisionReason states why a dropped kind may be dropped — the disclosure
// §6.4 demands, so the deviation is declared instead of silent.
func elisionReason(k batchflow.DropKind) string {
	switch k {
	case batchflow.DropFML:
		return "FML buffer field plumbing has no Python contract"
	case batchflow.DropDebug:
		return "debug-level configuration knob, environment-owned"
	case batchflow.DropRegistration:
		return "batch registration is an environment service"
	case batchflow.DropTuxedo:
		return "tp* lifecycle owned by the wrapper seam"
	case batchflow.DropErrorLog:
		return "error log folds into the exception handler"
	}
	return "no justification recorded"
}

// loopAccounting classifies each loop's termination proof (§6.1 gate 4).
func loopAccounting(in Input) (LoopAccounting, []Loop) {
	var a LoopAccounting
	var out []Loop
	if in.Facts == nil {
		return a, out
	}
	for _, l := range in.Facts.Loops {
		if l.StartLine < in.Flow.BodyStart || l.StartLine > in.Flow.BodyEnd {
			continue
		}
		site := Loop{Line: l.StartLine, EndLine: l.BlockEnd, Kind: string(l.Kind), Header: l.Cond}
		switch {
		case l.Kind == scanner.LoopFor && countable(l.Cond):
			site.Bounded = true
			site.Reason = "for-header trip count is countable"
		case l.Kind == scanner.LoopWhile && indefinite(l.Cond):
			// while(TRUE) — terminates only via an in-body exit. Counted
			// as indefinite; the cursor idiom carries a `break`.
			site.Reason = "while(TRUE) — terminates on an in-body exit"
		case l.Kind == scanner.LoopWhile:
			site.Reason = "while(cond) — terminates on a data condition"
		default:
			site.Reason = "termination not classified"
		}
		out = append(out, site)
		a.Total++
		switch {
		case site.Bounded:
			a.Bounded++
		case site.Kind == string(scanner.LoopWhile) && indefinite(l.Cond):
			a.Indefinite++
			a.Uncapped = append(a.Uncapped, strconv.Itoa(site.Line)+": "+site.Header)
		default:
			a.Guarded++
		}
	}
	sort.Strings(a.Uncapped)
	return a, out
}

// countable reports whether a for-header condition has a trip count the
// emitter can turn into a Python range(). The single-index C loop is the
// countable shape.
func countable(cond string) bool {
	c := strings.ReplaceAll(cond, " ", "")
	return strings.Contains(c, "<") && strings.Contains(c, "++")
}

// indefinite reports the while(TRUE) header.
func indefinite(cond string) bool {
	c := strings.ToUpper(strings.ReplaceAll(cond, " ", ""))
	return c == "TRUE" || c == "1" || c == ""
}

// dispatchAccounting reads each switch's case labels out of the source.
//
// It does NOT use SourceFacts.Switches: that field is declared "additive" in
// the scanner's pinned vocabulary but is empty on dtcmpt.pc even though the
// file has `switch(sql_msi_intvl_sch_typ)` at line 386 with four arms. A
// census that trusted an unpopulated field would report "0 switches, nothing
// to account for" on exactly the fixture whose case-'D' arm is the defect
// §6.2 records. Phase 0 adds no scanner field, so the switches are located
// here by a contained source scan.
//
// Missing stays empty while no handler mapping exists to compare against
// (-no-llm emits a placeholder body), because reporting every C arm as
// "missing" before the emitter has a mapping to miss would be a false
// failure. Phase 3 fills EmittedKeys and starts the comparison.
func dispatchAccounting(in Input) (DispatchAccounting, []Switch) {
	var a DispatchAccounting
	var out []Switch
	for _, s := range findSwitches(in) {
		s.CaseKeys = caseKeys(in, s)
		a.Switches++
		a.CaseLabels = append(a.CaseLabels, s.CaseKeys...)
		out = append(out, s)
	}
	a.CaseLabels = uniq(a.CaseLabels)
	return a, out
}

// findSwitches locates the entry body's switch statements by a contained
// brace match: the header line opens the block, and the block runs until the
// depth returns to zero. Depth counting skips string and char literals and
// line comments so a brace inside a message string cannot end a switch early.
func findSwitches(in Input) []Switch {
	var out []Switch
	for l := in.Flow.BodyStart; l <= in.Flow.BodyEnd && l-1 < len(in.Lines); l++ {
		cond, ok := switchHeader(in.Lines[l-1])
		if !ok {
			continue
		}
		end, ok := blockEnd(in.Lines, l, in.Flow.BodyEnd)
		if !ok {
			// Unbalanced block: record the header with an unknown extent
			// so the switch still shows up rather than vanishing.
			out = append(out, Switch{Cond: cond, Line: l})
			continue
		}
		out = append(out, Switch{Cond: cond, Line: l, EndLine: end})
	}
	return out
}

// switchHeader reports whether the line opens a switch and returns its
// condition text.
func switchHeader(line string) (string, bool) {
	t := strings.TrimSpace(line)
	rest, ok := cutKeyword(t, "switch")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return "", false
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(rest[1:i]), true
			}
		}
	}
	return "", false
}

// cutKeyword removes a leading keyword when it appears as a whole word.
func cutKeyword(s, kw string) (string, bool) {
	if len(s) < len(kw) || !strings.EqualFold(s[:len(kw)], kw) {
		return s, false
	}
	if len(s) > len(kw) && isIdentByte(s[len(kw)]) {
		return s, false
	}
	return s[len(kw):], true
}

// blockEnd returns the line on which the brace opened at or after startLine
// closes.
func blockEnd(lines []string, startLine, limit int) (int, bool) {
	depth := 0
	opened := false
	for l := startLine; l <= limit && l-1 < len(lines); l++ {
		depth += braceDelta(lines[l-1])
		if depth > 0 {
			opened = true
		}
		if opened && depth == 0 {
			return l, true
		}
	}
	return 0, false
}

// braceDelta is the net brace depth of a line, ignoring literals and comments.
func braceDelta(line string) int {
	depth := 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '"', '\'':
			i = skipCString(line, i)
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return depth // line comment to end of line
			}
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth
}

// skipCString returns the index of the closing quote of the literal at i.
func skipCString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == q {
			return j
		}
	}
	return len(s) - 1
}

// caseKeys returns the switch's case labels in source order. `default` is the
// fallthrough, not a dispatch key, so it is not one. Labels are stripped of
// their trailing comment (`case 'A': /* first or last N days */` → `'A'`),
// because a label has to be comparable to an emitted dict key.
func caseKeys(in Input, sw Switch) []string {
	var keys []string
	end := sw.EndLine
	if end < sw.Line {
		end = sw.Line
	}
	for l := sw.Line; l <= end && l-1 < len(in.Lines); l++ {
		rest, ok := cutKeyword(strings.TrimSpace(in.Lines[l-1]), "case")
		if !ok {
			continue
		}
		rest = stripComments(rest)
		rest = strings.TrimSpace(rest)
		if i := strings.Index(rest, ":"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		rest = strings.TrimSuffix(strings.TrimSpace(rest), ":")
		if rest == "" || strings.EqualFold(rest, "default") {
			continue
		}
		keys = append(keys, rest)
	}
	return keys
}

// stripComments removes a trailing // or /* */ comment from a source fragment.
func stripComments(s string) string {
	for {
		i := strings.Index(s, "//")
		j := strings.Index(s, "/*")
		cut := -1
		switch {
		case i >= 0 && (j < 0 || i < j):
			cut = i
		case j >= 0:
			if end := strings.Index(s[j+2:], "*/"); end >= 0 {
				s = s[:j] + s[j+2+end+2:]
				continue
			}
			cut = j // unterminated block comment
		}
		if cut < 0 {
			return s
		}
		return s[:cut]
	}
}

// uninitReads finds variables declared in the entry body, never assigned, and
// read — the `i_row` class of defect (§6.1 gate 5). Assignment detection is
// a token scan over the body's own lines, so it needs no scanner change.
func uninitReads(in Input) []UninitRead {
	var out []UninitRead
	if in.Facts == nil {
		return out
	}
	body := in.Lines
	sqlOut := sqlOutBinds(body, in.Flow)
	for _, d := range in.Facts.VarDecls {
		if d.Func != in.Flow.Entry || d.Name == "" {
			continue
		}
		if d.Line < in.Flow.BodyStart || d.Line > in.Flow.BodyEnd {
			continue
		}
		// Parameters are initialised by the caller.
		if isParam(in.Facts, d.Name, d.Line) {
			continue
		}
		assigned, readAt := scanAssignRead(body, d, in.Flow, sqlOut)
		if assigned || readAt == 0 {
			continue
		}
		out = append(out, UninitRead{Var: d.Name, Line: d.Line, Type: d.Type})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// isParam reports whether a VarDecl names one of the enclosing function's
// parameters — those arrive initialised.
func isParam(facts *scanner.SourceFacts, name string, line int) bool {
	for _, p := range facts.Params {
		if p.Name == name && p.Line <= line {
			return true
		}
	}
	return false
}

// scanAssignRead walks the body for the first write and the first read of the
// declared name. A read before any write is the uninitialised-read defect.
func scanAssignRead(lines []string, d scanner.VarDecl, flow *batchflow.Flow, sqlOut map[string]bool) (assigned bool, firstRead int) {
	for l := flow.BodyStart; l <= flow.BodyEnd && l-1 < len(lines); l++ {
		if l == d.Line {
			// A declaration with an initializer writes the variable
			// (`int mnth_val[25][12] = {{…}}` spans lines, but the `=`
			// is on the declaration line).
			if hasInitializer(lines[l-1], d.Name) {
				return true, 0
			}
			continue // otherwise the declaration is not a read
		}
		text := lines[l-1]
		if sqlOut[strings.ToLower(d.Name)] {
			return true, 0
		}
		if isAssignTo(text, d.Name) {
			return true, 0
		}
		if firstRead == 0 && mentions(text, d.Name) {
			firstRead = l
		}
	}
	return assigned, firstRead
}

// hasInitializer reports whether a declaration line initialises the name
// (`int x = 0;` / `int m[2][2] = {{…}}`).
func hasInitializer(text, name string) bool {
	at := strings.Index(text, name)
	if at < 0 {
		return false
	}
	rest := strings.TrimLeft(text[at+len(name):], " \t")
	// Skip the array subscripts a 2-D initializer carries.
	for strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return false
		}
		rest = strings.TrimLeft(rest[end+1:], " \t")
	}
	return strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==")
}

// sqlOutBinds returns the lowercased host variables the body's `INTO` clauses
// write. One forward pass over the whole body, because a Pro*C INTO list runs
// across many lines (`SELECT … INTO :a,\n :b,\n FROM …`) and a per-line test
// would classify every out-parameter after the first as an uninitialised read.
func sqlOutBinds(lines []string, flow *batchflow.Flow) map[string]bool {
	out := map[string]bool{}
	inInto := false
	for l := flow.BodyStart; l <= flow.BodyEnd && l-1 < len(lines); l++ {
		i := 0
		text := lines[l-1]
		for i < len(text) {
			if !inInto {
				at := indexWord(text, i, "INTO")
				if at < 0 {
					break
				}
				inInto = true
				i = at + len("INTO")
				continue
			}
			// Inside an INTO list: consume `,` whitespace and `:name`
			// binds. Anything else ends the list.
			c := text[i]
			switch {
			case c == ' ' || c == '\t' || c == ',' || c == '\r':
				i++
			case c == ':' && i+1 < len(text) && isIdentByte(text[i+1]):
				j := i + 1
				for j < len(text) && isIdentByte(text[j]) {
					j++
				}
				out[strings.ToLower(text[i+1:j])] = true
				i = j
			case c == '\n':
				i++
			default:
				inInto = false
			}
		}
	}
	return out
}

// indexWord finds a whole-word occurrence of w at or after i.
func indexWord(text string, i int, w string) int {
	for ; i+len(w) <= len(text); i++ {
		if !strings.EqualFold(text[i:i+len(w)], w) {
			continue
		}
		if i > 0 && isIdentByte(text[i-1]) {
			continue
		}
		if i+len(w) < len(text) && isIdentByte(text[i+len(w)]) {
			continue
		}
		return i
	}
	return -1
}

// isAssignTo reports whether the line writes the name. Three shapes count:
// a C assignment `name = …` (excluding the `==` of a comparison), a SQL
// `INTO :name` target, and the destination of a known writer call.
func isAssignTo(text, name string) bool {
	if callDstArg(text, name) {
		return true
	}
	return cAssign(text, name)
}

// callDstArg reports whether the line passes the name as the first argument
// of a call that writes through its destination (`strcpy(dst, …)`,
// `sprintf(dst, …)`).
func callDstArg(text, name string) bool {
	for _, w := range dstWriterCalls {
		open := strings.Index(text, w+"(")
		if open < 0 {
			continue
		}
		rest := text[open+len(w)+1:]
		// The first argument ends at the first comma or closing paren,
		// whichever comes first.
		end := strings.IndexAny(rest, ",)")
		if end < 0 {
			end = len(rest)
		}
		if strings.TrimSpace(rest[:end]) == name {
			return true
		}
	}
	return false
}

// dstWriterCalls write their first argument.
var dstWriterCalls = []string{"strcpy", "strncpy", "strcat", "sprintf", "snprintf", "strncat"}

// cAssign reports a bare `name = …` assignment, excluding comparisons.
func cAssign(text, name string) bool {
	i := 0
	for {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		i = at + len(name)
		if at > 0 && (isIdentByte(text[at-1]) || text[at-1] == ':') {
			continue
		}
		if at+len(name) < len(text) && isIdentByte(text[at+len(name)]) {
			continue
		}
		rest := strings.TrimLeft(text[at+len(name):], " \t")
		if !strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "==") {
			continue
		}
		return true
	}
}

// mentions reports whether the line references the name as a token.
func mentions(text, name string) bool {
	for i := 0; i+len(name) <= len(text); i++ {
		if text[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isIdentByte(text[i-1]) {
			continue
		}
		if i+len(name) < len(text) && isIdentByte(text[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// dropKindOf mirrors batchflow's drop table so the census can bucket a site
// without exporting the map. Kept in lockstep by
// TestDropKindTableMatchesBatchflow, which fails if the two ever diverge.
func dropKindOf(name string) (batchflow.DropKind, bool) {
	switch name {
	case "tpalloc", "tpfree", "tpinit", "tpterm", "tpopen", "tpclose",
		"tpbegin", "tpcommit", "tpabort", "tuxgetenv":
		return batchflow.DropTuxedo, true
	case "Fadd32", "Fchg32", "Fget32", "MEMSET", "SETNULL":
		return batchflow.DropFML, true
	case "fn_rgstr_bat", "fn_bat_pst_msg":
		return batchflow.DropRegistration, true
	case "INITBATDBGLVL":
		return batchflow.DropDebug, true
	case "errlog":
		return batchflow.DropErrorLog, true
	}
	return "", false
}

// findings renders the residue checks as human-readable lines. Each is a
// Phase 3 promotion candidate; Phase 0 only reports them.
func findings(c Census) []string {
	var out []string
	if c.SQL.Residue > 0 {
		out = append(out, "sql: "+itoa(c.SQL.Residue)+" query unit(s) reached no plan entry — "+
			strings.Join(append(append([]string{}, c.SQL.Unaccounted...), c.SQL.Duplicated...), ", "))
	}
	if c.Statements.ByBucket[string(BucketUnknown)] > 0 {
		out = append(out, "statements: "+itoa(c.Statements.ByBucket[string(BucketUnknown)])+
			" call(s) matched no accounting rule — "+strings.Join(c.Statements.Unlisted, ", "))
	}
	if len(c.Dispatch.Missing) > 0 {
		out = append(out, "dispatch: switch arm(s) absent from the emitted mapping — "+strings.Join(c.Dispatch.Missing, ", "))
	}
	if len(c.Uninit) > 0 {
		for _, u := range c.Uninit {
			out = append(out, "uninitialised read: "+u.Var+" (declared line "+itoa(u.Line)+", read before any write)")
		}
	}
	for _, u := range c.Loops.Uncapped {
		out = append(out, "loop: uncapped while(TRUE) at "+u+" — needs an explicit cap with a recorded reason")
	}
	return out
}

// Clean reports whether every accounting residue is zero — the single
// predicate Phase 3 turns into a hard failure.
func (c Census) Clean() bool {
	return c.SQL.Residue == 0 &&
		c.Statements.ByBucket[string(BucketUnknown)] == 0 &&
		len(c.Dispatch.Missing) == 0 &&
		len(c.Uninit) == 0
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func appendUnique(in []string, s string) []string {
	for _, x := range in {
		if x == s {
			return in
		}
	}
	return append(in, s)
}

func itoa(n int) string { return strconv.Itoa(n) }
