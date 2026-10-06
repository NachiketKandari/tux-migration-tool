// Package walkreport is the P0 instrument of the walk-faithful deterministic
// controller plan (docs/deterministic-walk-plan.md): it answers "how much of
// this service did we actually convert, and what is still a gap?" without
// changing a single byte of generated output.
//
// Two independent measurements, deliberately kept apart:
//
//   - Coverage: per-function flow coverage, from internal/flow. This is the
//     parser-accuracy guard — it says how much of the C source we UNDERSTAND.
//   - Gap census: every `tuxconv:TODO` in an already-emitted controller tree,
//     bucketed by a stable reason code. This says how much of what we
//     understood we could RENDER.
//
// The gap census reads the emitted .go files as text rather than asking the
// emitter to re-report. That is the whole design: a census derived from the
// generator's own return values would agree with a broken generator for the
// same reason it agreed with a correct one. Reading the artifact means the
// number can only ever describe what was really written to disk.
//
// Nothing here is a converter. It writes one report and mutates no pipeline
// state; it never calls an LLM, so -no-llm applies by construction.
package walkreport

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Reason is a stable, greppable code for WHY a gap was left. The vocabulary
// is the plan's (docs/deterministic-walk-plan.md §Design), narrowed to the
// codes the corpus actually produces today and pinned by tests below. A code
// is an API: it appears in committed reports and is compared across phases,
// so renaming one is a breaking change and adding one is additive.
//
// Codes are assigned by SHAPE OF MESSAGE, not by which emitter produced it.
// Two emitters that leave the same kind of hole get the same code, so a later
// phase that fixes one and not the other shows a partial drop rather than
// hiding both under a single bucket.
const (
	// ReasonHelperArgUnresolved is a fn-helper call argument passed a zero
	// value because no request field matched the helper's declared
	// parameter name. detHelperCalls in internal/gen emits these; it
	// resolves by parameter name only and never consults the call site.
	//
	// This is the corpus's dominant gap: 345 of the 423 TODOs on
	// dummyPipelineTest — all 15 params of FnFindDummyProfile and
	// FnSaveDummyProfile across 23 endpoints. P1's provenance index targets
	// exactly this code, so its count is the primary KPI for that phase.
	ReasonHelperArgUnresolved = "R-HELPER-ARG-UNRESOLVED"

	// ReasonStoreArgUnresolved is a store-call argument passed a zero value
	// because nothing established where it came from. detCallArgs and
	// detFnCallArgs emit these. Same underlying gap as
	// ReasonHelperArgUnresolved — "this value has no provenance" — but a
	// different site, and P1 fixes the two call sites through different code
	// paths, so they stay separate codes.
	ReasonStoreArgUnresolved = "R-STORE-ARG-UNRESOLVED"

	// ReasonResponseFieldUnresolved is a response field with no row match,
	// so it is emitted as a zero value. Two shapes carry it: the per-field
	// list form ("response fields without row match") and the per-call form
	// ("has no response-field match — kept for its error check"). P3's
	// provenance-driven shaping targets this code.
	ReasonResponseFieldUnresolved = "R-RESPONSE-FIELD-UNRESOLVED"

	// ReasonNoStoreCalls is a fn helper whose body rendered empty: the
	// emitter understood the helper and produced no statements for it.
	// internal/gen emits this as NoStoreCallsMark; on the corpus it is
	// FnFindDummyProfile, whose C body is FML plus tpcall with no SQL, so
	// there is no store call to project and the old output was a bare
	// `return 0` that read like an intentional no-op.
	//
	// P2 is what drives this to zero, by rendering the helper's statements
	// rather than skipping them. Until then the count is the honest
	// measure of how many helpers are stubs.
	ReasonNoStoreCalls = "R-NO-STORE-CALLS"

	// ReasonNestedHelperArgUnresolved is an argument to a helper called FROM
	// another helper's body, passed a zero because nothing established where
	// it came from. detFnCallArgs emits these; detHelperCalls emits the
	// caller-side equivalent above.
	//
	// Separate from ReasonHelperArgUnresolved for the same reason P1 split
	// helper-arg from store-arg: the two are fixed by different code paths
	// (detFnCallArgs vs detHelperCalls), so a merged bucket would let P2
	// report a drop that is really only one of the two.
	//
	// These only appeared once P2 rendered FnSaveDummyProfile's body — before
	// that the nested call was invisible, so this code had no population at
	// all. The census surfaced the shape as R-UNCLASSIFIED rather than
	// quietly absorbing it, which is the escape hatch earning its keep.
	ReasonNestedHelperArgUnresolved = "R-NESTED-HELPER-ARG-UNRESOLVED"

	// ReasonControlFlowNotRendered is a branch or loop in a fn helper's
	// legacy body that the deterministic path does not project as Go
	// control flow. internal/gen emits one per construct.
	//
	// This code is P2's actual measure. Before it existed, a helper with a
	// 200-line branching body rendered as its three SQL/helper calls and
	// looked finished — the branches were not dropped by a failed parse,
	// they were never consulted, so nothing anywhere recorded that the walk
	// was partial. The count is what P2 drives to zero.
	//
	// Only constructs that no named flow rule accounts for land here; a
	// debug-logging branch or an Fadd-result loop is elided by a rule
	// internal/flow already states, and counting those as gaps would
	// overstate the work.
	ReasonControlFlowNotRendered = "R-CONTROL-FLOW-NOT-RENDERED"

	// ReasonCallOutsideItsGuard is a call emitted unconditionally that the
	// legacy source conditionally guards.
	//
	// Kept apart from ReasonControlFlowNotRendered because it is worse. An
	// unrendered branch is missing work; a call hoisted out of its guard
	// makes the emitted Go assert something the source does not — it runs
	// where the C does not. Severity should not hide inside a count.
	ReasonCallOutsideItsGuard = "R-CALL-OUTSIDE-ITS-GUARD"

	// ReasonTPCallNotRendered is a tpcall site — a call into another
	// Tuxedo service — that the deterministic path does not render.
	// internal/gen emits one per site, in the fn helper that contains it.
	//
	// It is the P4/Option-A code (docs/deterministic-walk-plan.md, "Pinned:
	// TPCall is Option A"). Before it existed the site was not merely
	// unrendered, it was absent: the tpcall placeholder path is
	// entry-scoped and the corpus entry has zero tpcalls, so a helper's
	// tpcall reached the emitted tree as nothing at all. A count is the
	// minimum honest record of a call that was dropped.
	//
	// Option B would render these as a typed outbound client call. It is
	// deferred because the callee's source is not in this repository, so a
	// generated client could not be verified against a real implementation.
	ReasonTPCallNotRendered = "R-TPCALL"

	// ReasonResponseReadKept is a store read the walk keeps but that no
	// response field is attributed to, and whose row cannot source any of
	// the endpoint's unsourced fields.
	//
	// It is NOT a defect. It is the honest classification of a read the C
	// performs for its own sake: a lookup whose value feeds a later store
	// call's argument or a helper, or whose result exists only for its
	// error check. getUacUsrAccnts is the corpus example — 22 of the 23
	// endpoints read it, and every one of them then passes
	// getUacUsrAccnts.UrfUsrId.String into a later call.
	//
	// It exists because "has no response-field match" was doing two jobs.
	// A read that feeds nothing looked identical to a read that should have
	// fed the response and lost its value — the second is a real gap the
	// LLM seam must fill, the first is finished work. P3C separates them so
	// the remaining R-RESPONSE-FIELD-UNRESOLVED count means what it says.
	ReasonResponseReadKept = "R-RESPONSE-READ-KEPT"

	// ReasonUnclassified is the escape hatch, and it is deliberately not a
	// bucket to make the total come out even. A TODO whose shape matches no
	// registered pattern lands here, so adding a new emitter gap without
	// registering its code shows up as a non-zero count instead of silently
	// inflating one of the codes above.
	//
	// The census reports it separately and (*WalkReport).Tidy fails on it.
	ReasonUnclassified = "R-UNCLASSIFIED"
)

// AllReasons is the census key order: the real codes first, R-UNCLASSIFIED
// last so a reader scanning the table sees the healthy rows above the
// alarming one.
var AllReasons = []string{
	ReasonHelperArgUnresolved,
	ReasonStoreArgUnresolved,
	ReasonResponseFieldUnresolved,
	ReasonNestedHelperArgUnresolved,
	ReasonControlFlowNotRendered,
	ReasonCallOutsideItsGuard,
	ReasonTPCallNotRendered,
	ReasonNoStoreCalls,
	ReasonResponseReadKept,
	ReasonUnclassified,
}

var (
	// helperArgRe matches "FnSaveDummyProfile(sql_urf_eq_growth_asset_prcnt):
	// no request-field provenance — …". Anchored on the parenthesised
	// snake_case parameter, which is what separates a helper call site from
	// a store call site: the store form spells the same parameter as
	// Go.Param in lowerCamel.
	helperArgRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*\([a-z][a-z0-9_]*\): no request-field provenance`)

	// storeArgRe matches "UpdateUrfUsrDummyProf.dUrfDebtPrsrvAssetPrcnt: no
	// request-field provenance for "d_urf_debt_prsrv_asset_prcnt" — …".
	// Go method name, then the parameter as the emitter spells it.
	//
	// The parameter class is [A-Za-z] rather than [a-z] because the emitter
	// passes the C name through verbatim, and the corpus contains store
	// params that are not lowerCamel at all — GetGetUrfDtls.MI and
	// GetGetUrfDtls.SS, where MI and SS are the C identifiers. A [a-z]
	// anchor here silently pushed those two into R-UNCLASSIFIED.
	storeArgRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*\.[A-Za-z][A-Za-z0-9_]*: no request-field provenance`)

	// rowSourceRe matches "response fields without row source (zero values): …".
	// The wording changed in P3A and the change is load-bearing: the old
	// "without row match" was emitted once per READ, so one unsourced field
	// was reported by every read in the endpoint that failed to produce it.
	// "without row source" is emitted once per ENDPOINT, naming the fields no
	// read in the walk produces. The gap is the same zero value either way,
	// which is why the code is unchanged; the count is not, which is why the
	// message is.
	rowSourceRe = regexp.MustCompile(`^response fields without row source\b`)

	// responseRoleRe matches "getUacUsrAccnts (UacUsrAccnts) has no
	// response-field match — kept for its error check; …".
	responseRoleRe = regexp.MustCompile(`^[a-z][A-Za-z0-9]* \([A-Za-z][A-Za-z0-9]*\) has no response-field match`)

	// noStoreCallsRe matches the empty-helper-body marker internal/gen emits
	// as "R-NO-STORE-CALLS: FnFindDummyProfile — the helper body rendered
	// empty; …". It is anchored on the code itself rather than on the prose
	// after it: the prose is the human-readable half and may be reworded
	// without the gap changing, whereas the code is what the census tracks.
	noStoreCallsRe = regexp.MustCompile(`^` + ReasonNoStoreCalls + `\b`)

	// nestedHelperArgRe matches "FnInsertIntoUra(c_ura_user_id): no
	// helper-param provenance — zero value passed; LLM maps it". It is the
	// nested-call counterpart of helperArgRe and differs only in that last
	// phrase, which is deliberate: the emitter distinguishes the two sites
	// in the message, so the census must as well.
	nestedHelperArgRe = regexp.MustCompile(
		`^[A-Za-z][A-Za-z0-9_]*\([a-zA-Z][a-zA-Z0-9_]*\): no helper-param provenance`)

	// controlFlowRe matches the summary line gen emits as
	// "R-CONTROL-FLOW-NOT-RENDERED: FnSaveDummyProfile — 21 of 32 branch/loop
	// constructs …". The per-construct lines beneath it start with
	// "//   legacy", so they read as detail under this one gap rather than as
	// gaps of their own: a caller wants "this method is missing 21
	// branches", once, and the file already says which ones.
	controlFlowRe = regexp.MustCompile(`^` + ReasonControlFlowNotRendered + `\b`)

	// callOutsideGuardRe matches gen's "R-CALL-OUTSIDE-ITS-GUARD:
	// FnInsertIntoUra at line 4707 is emitted unconditionally, but the legacy
	// source guards it with the branch at line 4704 (c_flg_using == 'A') …".
	// The guard's line and condition live in the message rather than the
	// marker so one gap can name the specific misplacement instead of only
	// the callee.
	callOutsideGuardRe = regexp.MustCompile(`^` + ReasonCallOutsideItsGuard + `\b`)

	// tpcallRe matches gen's `R-TPCALL: tpcall calls "SVC_NETWORTH" at legacy
	// line 4393 — a call into another service is not rendered; …`. The callee
	// and line ride in the message so a gap names the specific dropped call
	// rather than only the helper it sits in.
	tpcallRe = regexp.MustCompile(`^` + ReasonTPCallNotRendered + `\b`)

	// responseReadKeptRe matches gen's "R-RESPONSE-READ-KEPT: getDual
	// (Dual) feeds no response field — the walk keeps it for its own sake;
	// …". Anchored on the code, like the other register-marked shapes, so
	// the prose after the colon can be reworded without moving the count.
	responseReadKeptRe = regexp.MustCompile(`^` + ReasonResponseReadKept + `\b`)
)

// patterns maps a reason code to the message shapes that carry it. The four
// shapes are mutually exclusive, so map order is irrelevant to matching;
// Classify still walks AllReasons so the reporting order is deterministic.
var patterns = map[string][]*regexp.Regexp{
	ReasonHelperArgUnresolved:       {helperArgRe},
	ReasonStoreArgUnresolved:        {storeArgRe},
	ReasonResponseFieldUnresolved:   {rowSourceRe, responseRoleRe},
	ReasonNoStoreCalls:              {noStoreCallsRe},
	ReasonResponseReadKept:          {responseReadKeptRe},
	ReasonNestedHelperArgUnresolved: {nestedHelperArgRe},
	ReasonControlFlowNotRendered:    {controlFlowRe},
	ReasonCallOutsideItsGuard:       {callOutsideGuardRe},
	ReasonTPCallNotRendered:         {tpcallRe},
}

// TODOPrefix is the marker every emitted gap comment carries. It is the
// single token that separates a gap comment from ordinary generated prose,
// which is what makes counting them from the artifact safe.
const TODOPrefix = "tuxconv:TODO"

// todoRe finds a TODO comment line and captures its message, dropping the
// leading "//" and any space after the marker.
var todoRe = regexp.MustCompile(`(?m)^[ \t]*//[ \t]*` + TODOPrefix + `[ \t]+(.*)$`)

// Classify assigns a reason code to one TODO message. It returns
// ReasonUnclassified when no registered pattern matches, which callers must
// treat as a finding rather than a success.
func Classify(message string) string {
	msg := strings.TrimSpace(message)
	for _, code := range AllReasons {
		for _, re := range patterns[code] {
			if re.MatchString(msg) {
				return code
			}
		}
	}
	return ReasonUnclassified
}

// Gap is one TODO found in an emitted tree.
type Gap struct {
	// File is the emitted file's path relative to the tree root, slash-
	// separated, so a report is comparable across checkouts.
	File string `json:"file"`
	// Line is the 1-based line number within File.
	Line int `json:"line"`
	// Method is the enclosing Go method name, or "" when the comment is not
	// inside one.
	Method string `json:"method,omitempty"`
	// Reason is the stable code from Classify.
	Reason string `json:"reason"`
	// Message is the TODO text with the marker stripped, kept verbatim so
	// the report can be read without re-reading the artifact.
	Message string `json:"message"`
}

// Census is the gap tally. ByCode is the primary KPI; the rest are cuts of
// the same population, so a phase can show WHERE a drop happened without
// re-deriving anything.
type Census struct {
	Total    int            `json:"total"`
	ByCode   map[string]int `json:"by_code"`
	ByMethod map[string]int `json:"by_method,omitempty"`
	ByFile   map[string]int `json:"by_file,omitempty"`
	// Gaps keeps every finding so -json is enough to diff two phases
	// without re-reading both trees.
	Gaps []Gap `json:"gaps,omitempty"`
}

// Add folds one gap into the census.
func (c *Census) Add(g Gap) {
	if c.ByCode == nil {
		c.ByCode = map[string]int{}
	}
	if c.ByMethod == nil {
		c.ByMethod = map[string]int{}
	}
	if c.ByFile == nil {
		c.ByFile = map[string]int{}
	}
	c.Total++
	c.ByCode[g.Reason]++
	c.ByMethod[g.Method]++
	c.ByFile[g.File]++
	c.Gaps = append(c.Gaps, g)
}

// Coverage is one function's flow coverage — the parser-accuracy guard.
type Coverage struct {
	Function string `json:"function"`
	// Classified and CodeLines come from flow.Coverage. Unknown is the
	// residue count and ResidueLines says where, so a drop in coverage is
	// traceable to lines rather than only to a number.
	Classified   int   `json:"classified"`
	CodeLines    int   `json:"code_lines"`
	Unknown      int   `json:"unknown"`
	ResidueLines []int `json:"residue_lines,omitempty"`
	// Percent is Classified/CodeLines as an integer. A function with no code
	// lines reports 100 rather than 0: nothing was missed, because there
	// was nothing to classify.
	Percent int `json:"percent"`
}

// NewCoverage builds a Coverage from raw counts, computing the percentage and
// truncating the residue list to keep a report readable. It is exported
// because the CLI measures coverage with internal/flow while this package
// owns the report shape.
func NewCoverage(fn string, classified, codeLines, unknown int, residue []int) Coverage {
	c := Coverage{
		Function:   fn,
		Classified: classified,
		CodeLines:  codeLines,
		Unknown:    unknown,
		Percent:    100,
	}
	if codeLines > 0 {
		c.Percent = classified * 100 / codeLines
	}
	// Cap the residue list: a function with 200 unclassified lines should
	// not bury the table. The full count stays in Unknown.
	if len(residue) > 20 {
		c.ResidueLines = residue[:20]
	} else {
		c.ResidueLines = residue
	}
	return c
}

// CoverageReport is the coverage roll-up for one analysed file.
type CoverageReport struct {
	Path      string     `json:"path"`
	Functions []Coverage `json:"functions"`
}

// WalkReport is the whole P0 measurement. Rendered as text and emitted as
// JSON with -json; both come from this one struct, so the two views cannot
// disagree.
type WalkReport struct {
	// Target is the corpus root (or file) that was analysed.
	Target string `json:"target"`
	// Controller is the emitted tree the census was read from, empty when
	// no census was requested.
	Controller string           `json:"controller,omitempty"`
	Coverage   []CoverageReport `json:"coverage,omitempty"`
	Census     *Census          `json:"census,omitempty"`
}

// Tidy returns findings that mean the INSTRUMENT needs attention, as opposed
// to the gaps it is measuring. A non-empty result is a bug in the census or
// an unregistered TODO shape — never a gap in the conversion.
//
// The distinction matters: P0's job is to make the existing gaps measurable,
// and a measurement that silently absorbs an unknown shape is worse than no
// measurement, because the number it reports is then wrong in a direction
// nobody can see.
func (r *WalkReport) Tidy() []string {
	var findings []string
	if r.Census == nil {
		return []string{"no census: pass -controller to measure the emitted tree"}
	}
	if n := r.Census.ByCode[ReasonUnclassified]; n > 0 {
		findings = append(findings, fmt.Sprintf(
			"%d TODO(s) matched no registered reason code — register the shape in internal/walkreport "+
				"rather than letting it inflate an existing code", n))
	}
	if r.Census.Total == 0 {
		findings = append(findings, "census is empty: the tree carries no "+TODOPrefix+
			" markers at all, which usually means the wrong directory was passed")
	}
	return findings
}

// CensusFile returns every TODO in one emitted file, in source order,
// attributing each to its enclosing Go method.
//
// Method attribution parses the file rather than scanning for "func ": a
// receiver splits "func (s *tuxController) FnX(" into two fields under naive
// scanning, and mis-attributing a gap to the wrong method would corrupt the
// per-method cut that the phases diff against. A file that does not parse is
// not an error — generated output can legitimately be a fragment — it just
// yields empty method names.
func CensusFile(name string, data []byte) []Gap {
	src := string(data)
	spans := methodSpans(src)

	var out []Gap
	for _, m := range todoRe.FindAllStringSubmatchIndex(src, -1) {
		line := lineOf(data, m[0])
		out = append(out, Gap{
			File:    name,
			Line:    line,
			Method:  methodAt(spans, line),
			Reason:  Classify(src[m[2]:m[3]]),
			Message: strings.TrimSpace(src[m[2]:m[3]]),
		})
	}
	return out
}

// span is an inclusive 1-based line range owned by one Go method.
type span struct {
	name       string
	start, end int
}

// methodSpans parses src and returns every top-level method's line range,
// sorted by start line. Unparseable input yields no spans.
func methodSpans(src string) []span {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []span
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		if end < start {
			end = start
		}
		out = append(out, span{name: fn.Name.Name, start: start, end: end})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// methodAt returns the name of the span containing line, or "".
func methodAt(spans []span, line int) string {
	for _, s := range spans {
		if line >= s.start && line <= s.end {
			return s.name
		}
	}
	return ""
}

// CensusDir walks root and censuses every .go file beneath it. File names in
// the result are slash-separated paths relative to root, so two runs from
// different checkouts produce comparable reports.
//
// Non-.go files are skipped silently: an emitted controller tree legitimately
// contains non-Go assets, and failing on them would make the instrument
// unusable on a real tree. A .go file that does not parse still contributes
// its TODOs — the census reads comments, not syntax — it just has no method
// attribution.
func CensusDir(root string) (Census, error) {
	var c Census
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("walkreport: read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("walkreport: relativise %s: %w", path, err)
		}
		for _, g := range CensusFile(filepath.ToSlash(rel), data) {
			c.Add(g)
		}
		return nil
	})
	if err != nil {
		return c, err
	}
	return c, nil
}

// MarshalJSON renders the report as indented JSON — the -json form, and the
// one P5's byte-identical rerun check will diff across phases. It lives here
// rather than in the CLI so the schema is owned by the package that defines
// it and a golden test can pin it without going through a subcommand.
func MarshalJSON(r *WalkReport) ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("walkreport: marshal: %w", err)
	}
	return append(data, '\n'), nil
}

// lineOf returns the 1-based line number of byte offset off.
func lineOf(data []byte, off int) int {
	if off > len(data) {
		off = len(data)
	}
	return 1 + strings.Count(string(data[:off]), "\n")
}

// Render writes the human-readable report to w. It lives beside the struct
// so the text and JSON views come from one traversal.
func (r *WalkReport) Render(w *strings.Builder) {
	fmt.Fprintf(w, "\nwalk report: %s\n", r.Target)
	if r.Controller != "" {
		fmt.Fprintf(w, "  census from: %s\n", r.Controller)
	}

	for _, cf := range r.Coverage {
		fmt.Fprintf(w, "\n%s\n", cf.Path)
		for _, f := range cf.Functions {
			if f.CodeLines == 0 {
				fmt.Fprintf(w, "  %-32s no code lines\n", f.Function)
				continue
			}
			fmt.Fprintf(w, "  %-32s %d/%d code lines classified (%d%%), unknown %d",
				f.Function, f.Classified, f.CodeLines, f.Percent, f.Unknown)
			if len(f.ResidueLines) > 0 {
				fmt.Fprintf(w, " at lines %s", joinInts(f.ResidueLines, 8))
			}
			fmt.Fprintln(w)
		}
	}

	if r.Census == nil {
		fmt.Fprintln(w, "\nno census requested")
		return
	}

	fmt.Fprintf(w, "\ngap census: %d TODO(s)\n", r.Census.Total)
	for _, code := range AllReasons {
		n := r.Census.ByCode[code]
		if n == 0 && code != ReasonUnclassified {
			continue
		}
		pct := 0
		if r.Census.Total > 0 {
			pct = n * 100 / r.Census.Total
		}
		fmt.Fprintf(w, "  %-28s %5d  %3d%%\n", code, n, pct)
	}

	printTally(w, "by method", r.Census.ByMethod)
	printTally(w, "by file", r.Census.ByFile)

	if findings := r.Tidy(); len(findings) > 0 {
		fmt.Fprintln(w, "\nfindings:")
		for _, f := range findings {
			fmt.Fprintf(w, "  ! %s\n", f)
		}
	}
}

// printTally prints a count map, skipping the single-entry case where it
// would say nothing the total did not already say.
func printTally(w *strings.Builder, label string, m map[string]int) {
	if len(m) <= 1 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", label)
	for _, k := range sortedKeys(m) {
		fmt.Fprintf(w, "  %5d  %s\n", m[k], k)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinInts(xs []int, max int) string {
	if len(xs) > max {
		xs = xs[:max]
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
