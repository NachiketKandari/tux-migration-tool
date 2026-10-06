// Deterministic no-llm controller and fn-helper synthesis.
//
// When the run is deterministic-only (--no-llm / run.llm: false), the
// pipeline used to skip every controller and fn-helper unit: no file was
// written and a later LLM-enabled resume filled the gap. That left --no-llm
// trees without compilable controllers at all.
//
// This file closes that gap with a best-effort deterministic synthesizer:
// every endpoint renders a Tier-A-clean method body from facts the plan and
// IR already carry — store calls in legacy order with captured results and
// error checks, row→response shaping from the FML contract, and the
// ExecTransaction wrapper when the unit's calls take tx. Anything the facts
// cannot name (unmapped bind args, unresolved helpers, tpcall sites, scalar
// placement) rides an explicit `tuxconv:TODO` comment — never a guess.
//
// The bodies carry a `tuxconv:deterministic-*` marker comment so an
// LLM-enabled resume can tell best-effort methods apart from accepted LLM
// output and upgrade exactly those. Synthesis is pure: identical inputs
// produce byte-identical bodies.
package gen

import (
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/templates"
	"tux-migration-tool/internal/walk"
)

// Deterministic markers. The endpoint/Go name rides the marker line so the
// resume probe is exact (a prefix match on "Item" must not fire on "ItemList"
// — callers match with a trailing space).
const (
	DeterministicControllerMark = "tuxconv:deterministic-controller"
	DeterministicFnHelperMark   = "tuxconv:deterministic-fnhelper"

	// NoStoreCallsMark is the reason code for a fn helper whose body
	// rendered empty. It is spelled as a reason code rather than prose
	// because internal/walkreport censuses these markers and the code is
	// what P2 will watch fall to zero as helper bodies start rendering.
	// Keep it in step with walkreport.ReasonNoStoreCalls.
	NoStoreCallsMark = "R-NO-STORE-CALLS"

	// ResponseReadKeptMark is the reason code for a read the walk keeps that
	// no response field is attributed to and that cannot source any of the
	// endpoint's unsourced fields. It is finished work, not a gap: the read
	// exists for its error check or to feed a later call's argument.
	//
	// It is a code rather than prose so the census can hold it apart from
	// R-RESPONSE-FIELD-UNRESOLVED, which is what P3C is for.
	// Keep it in step with walkreport.ReasonResponseReadKept.
	ResponseReadKeptMark = "R-RESPONSE-READ-KEPT"
)

// IsDeterministicControllerMethod reports whether src carries a
// deterministic no-llm body for endpoint name.
func IsDeterministicControllerMethod(src, name string) bool {
	return strings.Contains(src, "// "+DeterministicControllerMark+" "+name+" ")
}

// IsDeterministicFnHelper reports whether src carries a deterministic
// no-llm method for the fn helper's Go name.
func IsDeterministicFnHelper(src, goName string) bool {
	return strings.Contains(src, "// "+DeterministicFnHelperMark+" "+goName+" ")
}

// detCall is one ordered store call with its resolved query view: the canon
// query (duplicate-resolved), the call expression, the store signature
// params, and the capture contract the body emits.
type detCall struct {
	queryID   string // orig ID — the calls-map key
	canonKey  string // duplicate-resolved ID for pins/rows
	line      int    // absolute source line (legacy order)
	query     *ir.Query
	call      budget.DBCall
	params    []templates.ParamSpec
	method    string
	tx        bool
	shape     string // "error" | "single" | "scalar" | "rows"
	capture   string
	rowName   string            // models row type (reads only)
	rowByHost map[string]string // normalized host → row Go field
	rowFields []templates.FieldSpec
}

// detHelper is one same-file helper call the body needs.
type detHelper struct {
	line   int // absolute source line of first occurrence (legacy order)
	goName string
	args   []string
	retInt bool
	cap    string
}

// detFieldMap is one response-field assignment from a row field.
type detFieldMap struct {
	Resp string // response Go field
	Row  string // row Go field
}

// detProducer is one earlier read a later call's string arg can reference.
type detProducer struct {
	capture string // getItemHistoryRows
	field   string // DemoCompCd (sql.NullString — read via .String)
}

// DeterministicControllerBody synthesizes one endpoint's controller body
// (bare statements for the controller-method template) with zero LLM calls.
func (s *Service) DeterministicControllerBody(endpoint string, p *plan.Plan) (string, error) {
	e := s.endpointOf(endpoint)
	if e == nil {
		return "", fmt.Errorf("gen: no endpoint %s", endpoint)
	}
	c := s.conditionOf(*e)
	if c == nil {
		return "", fmt.Errorf("gen: no condition for endpoint %s", endpoint)
	}
	queries, calls, err := s.BranchCalls(c, p)
	if err != nil {
		return "", err
	}
	ordered, err := s.detOrderedCalls(queries, calls)
	if err != nil {
		return "", err
	}
	reqFields := s.requestFields(*e, c)
	respFields := contractFields(c.FmlOps, ir.FmlAdd)
	adds := detAdds(c)
	respType := s.responseType(endpoint)
	byName := detFieldSet(reqFields)
	reqMap := detRequestMap(s, c)
	branchSrc := s.branchSource(c.StartLine, c.EndLine)

	// Resolve every call's argument expressions in legacy order so later
	// calls can reference earlier reads: request fields by host provenance
	// or name, earlier row results for string params, zero literals
	// otherwise (with a TODO trail for the LLM upgrade).
	argTODOs := map[string][]string{}
	producers := map[string]detProducer{}
	for _, dc := range ordered {
		dc.call.Args = detCallArgs(dc, reqMap, byName, producers, argTODOs)
		detRegisterProducer(producers, dc)
	}
	helpers := detHelperCalls(p, branchSrc, c.StartLine, reqMap, byName, argTODOs)
	stubTODOs := detStubTODOs(p, branchSrc)
	tpTODOs := detTpTODOs(p, c)

	var sb strings.Builder
	fmt.Fprintf(&sb, "// %s %s — no-llm best-effort body (store calls + row→response mapping); an LLM-enabled resume upgrades it\n",
		DeterministicControllerMark, endpoint)
	fmt.Fprintf(&sb, "data = make([]*models.%s, 0)\n", respType)
	if len(ordered) == 0 {
		sb.WriteString("// tuxconv:TODO no store calls resolved — legacy logic needs the LLM seam\n")
	}
	for _, t := range stubTODOs {
		sb.WriteString(t + "\n")
	}
	for _, t := range tpTODOs {
		sb.WriteString(t + "\n")
	}
	for _, dc := range ordered {
		for _, t := range argTODOs[dc.method] {
			sb.WriteString(t + "\n")
		}
	}
	for _, h := range helpers {
		for _, t := range argTODOs["helper:"+h.goName] {
			sb.WriteString(t + "\n")
		}
	}

	inner := &strings.Builder{}
	detEmitControllerEvents(inner, s, ordered, helpers, adds, respFields, respType, detScope(ordered))

	if !detHasTx(ordered) {
		sb.WriteString(strings.TrimRight(inner.String(), "\n") + "\n")
		sb.WriteString("return data, nil")
		return sb.String(), nil
	}
	sb.WriteString("err = utils.ExecTransaction(c, s.store.GetDB(), func(tx *sqlx.Tx) error {\n")
	for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
		sb.WriteString(detClosureLine(line) + "\n")
	}
	sb.WriteString("return nil\n})")
	sb.WriteString("\nif err != nil {\n\treturn nil, err\n}\nreturn data, nil")
	return sb.String(), nil
}

// DeterministicFnHelperBody synthesizes one fn helper's complete Go method
// (declaration + body) with zero LLM calls. The signature matches the
// prescribed helper contract so callers generated against it keep compiling.
func (s *Service) DeterministicFnHelperBody(goName string, p *plan.Plan) (string, error) {
	var h *plan.FnHelper
	for i := range p.FnHelpers {
		if p.FnHelpers[i].GoName == goName {
			h = &p.FnHelpers[i]
			break
		}
	}
	if h == nil {
		return "", fmt.Errorf("gen: no fn helper %s", goName)
	}
	structName := common.LowerFirst(s.Mapping.Service) + "Controller"
	ids := helperQueryIDs(p, goName)
	calls, err := s.StoreCallsFor(ids, p)
	if err != nil {
		return "", err
	}
	ordered, err := s.detFnCalls(ids, calls)
	if err != nil {
		return "", err
	}
	fnSrc := s.branchSource(h.StartLine, h.EndLine)
	argTODOs := map[string][]string{}
	producers := map[string]detProducer{}
	for _, dc := range ordered {
		dc.call.Args = detFnCallArgs(dc, h.Params, producers, argTODOs)
		detRegisterProducer(producers, dc)
	}
	nested := detNestedHelperCalls(p, fnSrc, h, argTODOs)
	stubTODOs := detStubTODOs(p, fnSrc)

	decl := "func (s *" + structName + ") " + h.GoName + "(c context.Context"
	for _, pr := range h.Params {
		decl += ", " + pr.Name + " " + pr.Type
	}
	decl += ")"
	if h.Return != "" {
		decl += " " + h.Return
	}
	voidRet := h.Return == ""
	outs := outParams(h.Params)

	var sb strings.Builder
	sb.WriteString(decl + " {\n")
	fmt.Fprintf(&sb, "// %s %s — no-llm best-effort body; an LLM-enabled resume upgrades it\n",
		DeterministicFnHelperMark, goName)
	for _, t := range stubTODOs {
		sb.WriteString(t + "\n")
	}
	for _, dc := range ordered {
		for _, t := range argTODOs[dc.method] {
			sb.WriteString(t + "\n")
		}
	}
	for _, nh := range nested {
		for _, t := range argTODOs["helper:"+nh.goName] {
			sb.WriteString(t + "\n")
		}
	}
	// Control-flow accounting (P2). The store-call walk above is a flat
	// statement list, so a helper whose legacy body branches or loops
	// rendered as just its SQL and helper calls — the branches were not
	// missing from the parse, they were never consulted. This makes each
	// one of them either accounted for (rendered, or elided by a named flow
	// rule) or counted as a gap.
	for _, t := range s.detFnControlFlow(h, ordered, nested).TODOs(goName) {
		sb.WriteString(t + "\n")
	}
	// tpcall accounting (P4, Option A). The entry-scoped placeholder path
	// never sees a helper's tpcalls, so without this the call into the other
	// service reaches the emitted tree as nothing at all. Each site becomes
	// a counted gap, and detFnTPCallTail below makes the helper return the
	// failure status so the gap cannot be mistaken for a success.
	tps := s.tpcallsFor(h.Name)
	for _, t := range detFnTPCallTODOs(goName, tps) {
		sb.WriteString(t + "\n")
	}

	inner := &strings.Builder{}
	detEmitFnEvents(inner, ordered, nested, outs, voidRet, false)
	body := strings.TrimRight(inner.String(), "\n")

	// A helper whose body came out empty is a helper we understood and could
	// not render. Say so, with a reason code, instead of emitting a bare
	// `return 0` that reads like an intentional no-op.
	//
	// The common cause is a helper whose C body is FML plus tpcall and no
	// SQL: helperQueryIDs finds nothing, so ordered is empty and
	// detEmitFnEvents emits nothing. That is R-NO-STORE-CALLS, not a silent
	// success — the census in internal/walkreport counts these, and a code
	// the corpus can produce is the whole reason R-NO-STORE-CALLS exists.
	//
	// The emptiness test is on the RENDERED body, not on len(ordered), so a
	// helper whose only content was an unresolvable call still gets the
	// marker if it genuinely produced no statements.
	if body == "" && len(stubTODOs) == 0 && len(argTODOs) == 0 {
		fmt.Fprintf(&sb, "// tuxconv:TODO %s: %s — the helper body rendered empty; "+
			"its statements are not represented in this method\n",
			NoStoreCallsMark, goName)
	}

	// The tail is where a skipped tpcall becomes a behavioural fact rather
	// than only a comment. Callers test the legacy status convention, so a
	// helper that did not make its inter-service call must not report the
	// legacy success value — see detFnTPCallTail for why the alternative
	// persists bad data.
	tail := detFnTail(fnSrc, h)
	if len(tps) > 0 {
		tail = detFnTPCallTail(h)
	}

	if !detHasTx(ordered) {
		sb.WriteString(body + "\n")
		sb.WriteString(tail + "\n}")
		return sb.String(), nil
	}
	sb.WriteString("err := utils.ExecTransaction(c, s.store.GetDB(), func(tx *sqlx.Tx) error {\n")
	closure := &strings.Builder{}
	detEmitFnEvents(closure, ordered, nested, outs, voidRet, true)
	for _, line := range strings.Split(strings.TrimRight(closure.String(), "\n"), "\n") {
		sb.WriteString(detClosureLineFn(line) + "\n")
	}
	sb.WriteString("return nil\n})")
	sb.WriteString("\nif err != nil {\n")
	sb.WriteString(detFnErrTail(h))
	sb.WriteString("\n}\n" + tail + "\n}")
	return sb.String(), nil
}

// helperQueryIDs returns the plan unit's query IDs for a fn helper Go name.
func helperQueryIDs(p *plan.Plan, goName string) []string {
	for _, u := range p.Units {
		if u.Kind == plan.KindFnHelper && u.Name == goName {
			return u.QueryIDs
		}
	}
	return nil
}

// detOrderedCalls resolves every branch query into legacy-ordered detCalls
// with capture names assigned.
func (s *Service) detOrderedCalls(queries []*ir.Query, calls map[string]budget.DBCall) ([]*detCall, error) {
	var out []*detCall
	for _, q := range queries {
		call, ok := calls[q.ID]
		if !ok {
			continue
		}
		dc, err := s.detResolveCall(q.ID, call)
		if err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	used := map[string]int{}
	for _, dc := range out {
		dc.capture = detCaptureName(dc.method, dc.shape, used)
	}
	return out, nil
}

// detFnCalls resolves a fn helper's query IDs into legacy-ordered detCalls.
func (s *Service) detFnCalls(ids []string, calls map[string]budget.DBCall) ([]*detCall, error) {
	var out []*detCall
	for _, id := range ids {
		call, ok := calls[id]
		if !ok {
			continue
		}
		dc, err := s.detResolveCall(id, call)
		if err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	used := map[string]int{}
	for _, dc := range out {
		dc.capture = detCaptureName(dc.method, dc.shape, used)
	}
	return out, nil
}

// detResolveCall builds one detCall: canon query, store params, result
// shape, and row contract for reads.
func (s *Service) detResolveCall(origID string, call budget.DBCall) (*detCall, error) {
	orig := s.Query(origID)
	canon, canonKey := orig, origID
	if orig != nil && orig.DuplicateOf != "" {
		if alias := canonicalAliasID(origID, orig); s.Query(alias) != nil {
			canon, canonKey = s.Query(alias), alias
		} else if cq := s.Query(orig.DuplicateOf); cq != nil {
			canon, canonKey = cq, orig.DuplicateOf
		}
	}
	method := call.Name
	dc := &detCall{queryID: origID, canonKey: canonKey, query: canon, call: call, method: method, tx: call.Tx != ""}
	if abs := s.Query(origID); abs != nil {
		dc.line = abs.StartLine
	}
	if canon == nil {
		// No IR record (should not happen — the caller resolved it):
		// keep the call error-only so the body still checks it.
		dc.shape = "error"
		return dc, nil
	}
	pin, _ := s.Pin(canonKey)
	params, err := s.dbParams(canon, pin)
	if err != nil {
		return nil, err
	}
	dc.params = params
	switch {
	case canon.Type.IsDML():
		dc.shape = "error"
	case canon.Type == ir.QuerySelectSingle && isCountQuery(canon):
		dc.shape = "scalar"
	case canon.Type == ir.QuerySelectSingle:
		dc.shape = "single"
	default:
		dc.shape = "rows"
	}
	if dc.shape == "rows" || dc.shape == "single" {
		dc.rowName = s.RowName(canonKey, method)
		if fields, ferr := s.rowFields(canonKey, canon); ferr == nil {
			dc.rowFields = fields
			byHost := map[string]string{}
			for i, hv := range canon.RowShape {
				if i < len(fields) {
					byHost[normHost(hv)] = fields[i].Name
				}
			}
			dc.rowByHost = byHost
		}
	}
	return dc, nil
}

// detScope builds the endpoint-wide provenance index over the body's reads
// (plan P3A).
//
// Shaping is handed one read at a time, but a response field is a write to one
// host variable, and that host is produced by exactly one read. Without the
// index, every read is asked about every response field, so a field belonging
// to read A is reported unmapped by reads B, C and D as well — the same gap
// counted once per read in the endpoint. The index answers the question the
// renderer actually has: which read produces this host?
//
// Reads are indexed in walk order, which is what makes the first-wins owner
// rule deterministic. Reads with no row shape still take a slot so the index
// of every other read keeps matching the call order.
func detScope(ordered []*detCall) *walk.Scope {
	reads := make([]walk.Read, 0, len(ordered))
	for _, dc := range ordered {
		r := walk.Read{
			QueryID: dc.queryID,
			Capture: dc.capture,
			RowType: dc.rowName,
		}
		if dc.query != nil {
			r.Hosts = dc.query.RowShape
		}
		for _, f := range dc.rowFields {
			r.Fields = append(r.Fields, f.Name)
		}
		reads = append(reads, r)
	}
	return walk.Index(reads)
}

// detCaptureName assigns the deterministic per-body capture variable:
// lowerFirst(method)[+Rows], numeric suffix on repeats (mirrors the seam's
// capture contract the gates already pin).
func detCaptureName(method, shape string, used map[string]int) string {
	base := common.LowerFirst(method)
	if shape == "rows" {
		base += "Rows"
	}
	if n := used[base]; n > 0 {
		used[base]++
		return fmt.Sprintf("%s%d", base, n+1)
	}
	used[base]++
	return base
}

// detAdds returns the condition's contract FML adds (response drivers).
//
// Output-buffer only. A response field is a write to the endpoint's Obuffer;
// an Fadd32 against the Ibuffer writes the *request* buffer, which is a read
// guard or a default, never the response value. Admitting Ibuffer adds here
// is what let the shared preamble's `Fadd32(ptr_fml_Ibuffer, FML_POINT_TYPE,
// &sql_urf_mm_opt_stts_2)` shadow the real per-branch
// `Fadd32(ptr_fml_Obuffer, FML_POINT_TYPE, &sql_rpam_answer_id)` and leave
// PointType unmapped on every endpoint (P3B). flow.resolveResponses already
// draws the same line (SCEN-D9); this makes the shaping path agree with it.
func detAdds(c *ir.Condition) []ir.FmlOp {
	var out []ir.FmlOp
	for _, op := range c.FmlOps {
		if op.Kind == ir.FmlAdd && !op.Dropped && !op.Error && strings.HasSuffix(op.Buffer, "Obuffer") {
			out = append(out, op)
		}
	}
	return out
}

// detFieldSet indexes request Go names (lowercased) for the name-fallback
// arg match.
func detFieldSet(fields []templates.FieldSpec) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		out[strings.ToLower(f.Name)] = f.Name
	}
	return out
}

// detRequestMap builds the normalized-host → request-field map from the
// same FmlGet derivation the request struct uses (branch gets plus consumed
// preamble gets — mirrors the prompt's provenance).
func detRequestMap(s *Service, c *ir.Condition) map[string]string {
	out := map[string]string{}
	branchSrc := ""
	if s.source != "" && c.EndLine > c.StartLine {
		if lines := s.sourceLines(); c.EndLine <= len(lines) && c.StartLine >= 1 {
			branchSrc = strings.Join(lines[c.StartLine-1:c.EndLine], "\n")
		}
	}
	for _, op := range c.FmlOps {
		if op.Kind == ir.FmlGet && !op.Dropped && !op.Error && op.Target != "" {
			out[normHost(op.Target)] = fieldFromFML(op.Field)
		}
	}
	if s.Main == nil || branchSrc == "" {
		return out
	}
	for _, op := range s.Main.FmlOps {
		if op.Kind != ir.FmlGet || op.Dropped || op.Target == "" || !strings.Contains(branchSrc, op.Target) {
			continue
		}
		if _, ok := out[normHost(op.Target)]; !ok {
			out[normHost(op.Target)] = fieldFromFML(op.Field)
		}
	}
	return out
}

// branchSource slices absolute [from, to] lines out of the entry source. It
// reads the memoized split (sourceLines) rather than re-splitting the whole
// .pc per call — both callers are per-endpoint, so the old body re-split
// once per endpoint and per fn helper.
func (s *Service) branchSource(from, to int) string {
	lines := s.sourceLines()
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// detRegisterProducer indexes one read's row fields (normalized host →
// capture/field) for later string args. First producer wins — deterministic.
func detRegisterProducer(producers map[string]detProducer, dc *detCall) {
	if dc.shape != "rows" && dc.shape != "single" {
		return
	}
	for norm, field := range dc.rowByHost {
		if norm == "" || field == "" {
			continue
		}
		if _, ok := producers[norm]; !ok {
			producers[norm] = detProducer{capture: dc.capture, field: field}
		}
	}
}

// detCallArgs resolves one store call's argument expressions: request
// fields by host provenance, then by param-name match, then an earlier
// read's row field for string params, else the type's zero literal with a
// TODO trail for the LLM upgrade.
func detCallArgs(dc *detCall, reqMap, byName map[string]string, producers map[string]detProducer, todos map[string][]string) []string {
	binds := []string{}
	if dc.query != nil {
		binds = dc.query.Binds
	}
	out := make([]string, 0, len(dc.params))
	for i, pr := range dc.params {
		var bind string
		if i < len(binds) {
			bind = binds[i]
		}
		if bind != "" {
			if f, ok := reqMap[normHost(bind)]; ok && f != "" {
				out = append(out, "request."+f)
				continue
			}
		}
		if f, ok := byName[strings.ToLower(pr.Name)]; ok {
			out = append(out, "request."+f)
			continue
		}
		if pr.Type == "string" && bind != "" {
			if prd, ok := producers[normHost(bind)]; ok {
				out = append(out, prd.capture+"."+prd.field+".String")
				continue
			}
		}
		out = append(out, detZero(pr.Type))
		todos[dc.method] = append(todos[dc.method], fmt.Sprintf(
			"// tuxconv:TODO %s.%s: no request-field provenance for %q — zero value passed; LLM maps it",
			dc.method, pr.Name, bind))
	}
	return out
}

// detFnCallArgs resolves one store call's args against a fn helper's own
// params (bare identifiers — no request struct here), then earlier reads
// for string params, else zero literals with a TODO trail.
func detFnCallArgs(dc *detCall, params []plan.FnParam, producers map[string]detProducer, todos map[string][]string) []string {
	binds := []string{}
	if dc.query != nil {
		binds = dc.query.Binds
	}
	var declared []string
	for _, pr := range params {
		declared = append(declared, pr.Name)
	}
	out := make([]string, 0, len(dc.params))
	for i, pr := range dc.params {
		if name := detMatchIdent(pr.Name, declared); name != "" {
			out = append(out, name)
			continue
		}
		var bind string
		if i < len(binds) {
			bind = binds[i]
		}
		if name := detMatchIdent(bind, declared); name != "" {
			out = append(out, name)
			continue
		}
		if pr.Type == "string" && bind != "" {
			if prd, ok := producers[normHost(bind)]; ok {
				out = append(out, prd.capture+"."+prd.field+".String")
				continue
			}
		}
		out = append(out, detZero(pr.Type))
		todos[dc.method] = append(todos[dc.method], fmt.Sprintf(
			"// tuxconv:TODO %s.%s: no helper-param provenance for %q — zero value passed; LLM maps it",
			dc.method, pr.Name, bind))
	}
	return out
}

// detMatchIdent matches a db param (or bind host) to a declared identifier:
// normalized-host equality first, then lowercased-name equality.
func detMatchIdent(want string, declared []string) string {
	if want == "" {
		return ""
	}
	if nw := normHost(want); nw != "" {
		for _, d := range declared {
			if normHost(d) == nw {
				return d
			}
		}
	}
	lw := strings.ToLower(want)
	for _, d := range declared {
		if strings.ToLower(d) == lw {
			return d
		}
	}
	return ""
}

// detZero renders the Go zero literal for a store/param type. time.Time{}
// needs only the content-gated time import.
func detZero(typ string) string {
	switch strings.TrimSpace(typ) {
	case "string":
		return `""`
	case "int", "int64", "int32", "float64", "float32":
		return "0"
	case "bool":
		return "false"
	case "time.Time":
		return "time.Time{}"
	default:
		if t := strings.TrimSpace(typ); strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") {
			return "nil"
		}
		return `""`
	}
}

// detHelperCalls finds the same-file helper calls a branch body needs: plan
// helpers whose legacy name appears as a call in the branch source, ordered
// by first occurrence.
//
// Args resolve against reqMap, the same normalized-host → request-field map
// the store calls use. That matters more than it looks: a helper parameter
// keeps its LEGACY C spelling (plan/helperSignature preserves it so the
// helper body reads naturally), so FnFindDummyProfile's parameter is
// `c_user_id`, not `UsrId`. Matching that against the request struct's Go
// field names — which is what this did before — never matches anything,
// because `c_user_id` and `UsrId` have nothing in common but their meaning.
// The provenance map is keyed on the same legacy spelling the FML_GET target
// uses, so the two sides line up.
//
// byName stays as a second attempt for the legacy shapes where a parameter
// really is named after a Go request field. reqMap is tried first because it
// is provenance-backed and byName is a coincidence of naming.
func detHelperCalls(p *plan.Plan, branchSrc string, baseLine int, reqMap, byName map[string]string, todos map[string][]string) []detHelper {
	if p == nil || branchSrc == "" {
		return nil
	}
	lines := strings.Split(branchSrc, "\n")
	var out []detHelper
	used := map[string]int{}
	for _, h := range p.FnHelpers {
		line := -1
		for i, ln := range lines {
			if strings.Contains(ln, h.Name+"(") {
				line = baseLine + i
				break
			}
		}
		if line < 0 {
			continue
		}
		dh := detHelper{line: line, goName: h.GoName, retInt: h.Return == "int"}
		for _, pr := range h.Params {
			if f, ok := reqMap[normHost(pr.Name)]; ok && f != "" {
				dh.args = append(dh.args, "request."+f)
				continue
			}
			if f, ok := byName[strings.ToLower(pr.Name)]; ok {
				dh.args = append(dh.args, "request."+f)
				continue
			}
			dh.args = append(dh.args, detZero(pr.Type))
			todos["helper:"+h.GoName] = append(todos["helper:"+h.GoName], fmt.Sprintf(
				"// tuxconv:TODO %s(%s): no request-field provenance — zero value passed; LLM maps it", h.GoName, pr.Name))
		}
		cap := common.LowerFirst(h.GoName) + "St"
		if n := used[cap]; n > 0 {
			cap = fmt.Sprintf("%s%d", cap, n+1)
		}
		used[cap]++
		dh.cap = cap
		out = append(out, dh)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// detNestedHelperCalls finds same-file helper calls inside a fn helper's
// own source (args resolve against the helper's params).
func detNestedHelperCalls(p *plan.Plan, fnSrc string, h *plan.FnHelper, todos map[string][]string) []detHelper {
	if p == nil || fnSrc == "" {
		return nil
	}
	var declared []string
	for _, pr := range h.Params {
		declared = append(declared, pr.Name)
	}
	lines := strings.Split(fnSrc, "\n")
	var out []detHelper
	used := map[string]int{}
	for _, nh := range p.FnHelpers {
		if nh.GoName == h.GoName {
			continue
		}
		line := -1
		for i, ln := range lines {
			if strings.Contains(ln, nh.Name+"(") {
				line = h.StartLine + i
				break
			}
		}
		if line < 0 {
			continue
		}
		dh := detHelper{line: line, goName: nh.GoName, retInt: nh.Return == "int"}
		for _, pr := range nh.Params {
			if name := detMatchIdent(pr.Name, declared); name != "" {
				dh.args = append(dh.args, name)
				continue
			}
			dh.args = append(dh.args, detZero(pr.Type))
			todos["helper:"+nh.GoName] = append(todos["helper:"+nh.GoName], fmt.Sprintf(
				"// tuxconv:TODO %s(%s): no helper-param provenance — zero value passed; LLM maps it", nh.GoName, pr.Name))
		}
		cap := common.LowerFirst(nh.GoName) + "St"
		if n := used[cap]; n > 0 {
			cap = fmt.Sprintf("%s%d", cap, n+1)
		}
		used[cap]++
		dh.cap = cap
		out = append(out, dh)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// detStubTODOs lists the unresolved-helper TODOs for stubs the source
// references: omitted from the deterministic body by design (the package
// stub's out-pointer contract needs the LLM), visible for the upgrade.
func detStubTODOs(p *plan.Plan, src string) []string {
	if p == nil || len(p.Stubs) == 0 || src == "" {
		return nil
	}
	var out []string
	for _, st := range p.Stubs {
		if strings.Contains(src, st.Fn) {
			out = append(out, "// tuxconv:TODO unresolved helper "+st.Fn+" call omitted — LLM maps it to the package stub")
		}
	}
	sort.Strings(out)
	return out
}

// detTpTODOs lists the tpcall-site TODOs for placeholders owned by the
// tpcall units inside the condition span.
func detTpTODOs(p *plan.Plan, c *ir.Condition) []string {
	if p == nil || c == nil {
		return nil
	}
	var out []string
	for _, u := range p.Units {
		if u.Kind != plan.KindTPCall || u.TP == nil {
			continue
		}
		if u.TP.StartLine >= c.StartLine && u.TP.EndLine <= c.EndLine {
			svc := u.TP.Service
			if svc == "" {
				svc = u.Name
			}
			out = append(out, "// tuxconv:TODO tpcall "+svc+" site omitted — placeholder in tpcall_placeholders.go")
		}
	}
	sort.Strings(out)
	return out
}

// detHasTx reports whether any call takes the tx handle.
func detHasTx(ordered []*detCall) bool {
	for _, dc := range ordered {
		if dc.tx {
			return true
		}
	}
	return false
}

// detCallLine renders one store invocation (capture assignment excluded).
func detCallLine(dc *detCall) string {
	var sb strings.Builder
	sb.WriteString(dc.call.Receiver + "." + dc.call.Name + "(" + dc.call.CtxName)
	if dc.call.Tx != "" {
		sb.WriteString(", " + dc.call.Tx)
	}
	for _, a := range dc.call.Args {
		sb.WriteString(", " + a)
	}
	sb.WriteString(")")
	return sb.String()
}

// detEmitControllerEvents renders the interleaved helper + store calls in
// legacy order plus the response shaping. outerErr reports whether an err
// variable is already in scope (the controller's named return): without
// one, the first error-only call declares it; reads always capture with :=.
func detEmitControllerEvents(sb *strings.Builder, s *Service, ordered []*detCall, helpers []detHelper, adds []ir.FmlOp, respFields []templates.FieldSpec, respType string, scope *walk.Scope) {
	type event struct {
		line int
		kind int // 0 = store, 1 = helper
		dc   *detCall
		h    *detHelper
	}
	var evs []event
	for _, dc := range ordered {
		evs = append(evs, event{line: dc.line, kind: 0, dc: dc})
	}
	for i := range helpers {
		evs = append(evs, event{line: helpers[i].line, kind: 1, h: &helpers[i]})
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].line != evs[j].line {
			return evs[i].line < evs[j].line
		}
		return evs[i].kind < evs[j].kind
	})
	errDecl := false
	for _, ev := range evs {
		if ev.kind == 1 {
			detEmitHelper(sb, ev.h)
			continue
		}
		detEmitStoreCall(sb, ev.dc, &errDecl, true)
	}
	detEmitShaping(sb, ordered, adds, respFields, respType, scope)
}

// detEmitStoreCall renders one checked store call: error-only assigns err,
// reads capture with := (reused err after the first capture).
func detEmitStoreCall(sb *strings.Builder, dc *detCall, errDecl *bool, outerErr bool) {
	call := detCallLine(dc)
	if dc.shape == "error" {
		if outerErr || *errDecl {
			sb.WriteString("err = " + call + "\n")
		} else {
			sb.WriteString("err := " + call + "\n")
			*errDecl = true
		}
		sb.WriteString("if err != nil {\n\treturn nil, err\n}\n")
		return
	}
	sb.WriteString(dc.capture + ", err := " + call + "\n")
	*errDecl = true
	sb.WriteString("if err != nil {\n\treturn nil, err\n}\n")
}

// detEmitHelper renders one same-file helper call: int-status helpers check
// -1 with a named error, void helpers are fire-and-forget.
func detEmitHelper(sb *strings.Builder, h *detHelper) {
	call := "s." + h.goName + "(c"
	for _, a := range h.args {
		call += ", " + a
	}
	call += ")"
	if !h.retInt {
		sb.WriteString(call + "\n")
		return
	}
	sb.WriteString(h.cap + " := " + call + "\n")
	sb.WriteString("if " + h.cap + " == -1 {\n\treturn nil, errors.New(\"" + h.goName + " failed\")\n}\n")
}

// detRowSources assigns each response field to the row that actually produces
// its value, once for the whole endpoint (plan P3A).
//
// The question "which read sources this field?" has exactly one answer, and it
// is not read-local. A field's FML write names a host variable, and the
// provenance index says which read produces that host. The old code asked
// every read about every field, so a field owned by read A was reported
// unmapped by reads B, C and D too — one gap per read in the endpoint, and the
// same list of names repeated under each.
//
// The returned map is field name → the read index that owns it. A field absent
// from the map is one no read in the endpoint produces: a real gap, reported
// once.
func detRowSources(adds []ir.FmlOp, respFields []templates.FieldSpec, scope *walk.Scope) map[string]int {
	owned := map[string]int{}
	if scope == nil {
		return owned
	}
	// First add wins per response name, matching the target rule the rest of
	// shaping uses.
	seen := map[string]bool{}
	for _, op := range adds {
		name := fieldFromFML(op.Field)
		if seen[name] || op.Target == "" {
			continue
		}
		seen[name] = true
		if o, ok := scope.OwnerFor(op.Target); ok {
			owned[name] = o.Read
		}
	}
	return owned
}

// detUnsourced returns the response fields no read in the endpoint produces,
// in response order and deduplicated. These are the endpoint's genuine shaping
// gaps — a field whose value is written from a host that no read yields, so
// the renderer has nothing to emit and says so once rather than once per read.
func detUnsourced(owned map[string]int, respFields []templates.FieldSpec) []string {
	var out []string
	for _, rf := range respFields {
		if _, ok := owned[rf.Name]; !ok {
			out = append(out, rf.Name)
		}
	}
	return out
}

// detReadFeedsResponse reports whether any response field is attributed to
// this read. It is the honest form of "this read shapes something": a read
// whose only apparent pairs came from fields another read owns feeds nothing,
// and saying it "has no response-field match" is then true rather than a
// side effect of losing a cross-read guess.
func detReadFeedsResponse(dc *detCall, owned map[string]int, scope *walk.Scope) bool {
	if dc == nil {
		return false
	}
	idx, ok := scope.ReadFor(dc.capture)
	if !ok {
		return false
	}
	for _, owner := range owned {
		if owner == idx {
			return true
		}
	}
	return false
}

// detReadCouldSourceUnsourced reports whether this read's row shape carries
// the host variable of any response field the endpoint could not source.
//
// It splits the reads that feed no response field into the two cases that used
// to share one message and are not the same thing:
//
//   - FALSE — the read's row cannot produce any missing value, so keeping it
//     is the whole job. getUacUsrAccnts is the corpus instance: 22 endpoints
//     read it and every one passes getUacUsrAccnts.UrfUsrId.String into a
//     later call, so it is finished work, reported as R-RESPONSE-READ-KEPT.
//
//   - TRUE — the read's row DOES carry the host an unsourced response write
//     names, so the value exists and shaping failed to place it. That is a
//     genuine R-RESPONSE-FIELD-UNRESOLVED and must stay in the LLM seam's
//     queue.
//
// Splitting them is what makes the remaining R-RESPONSE-FIELD-UNRESOLVED count
// mean "a response value the walk could not place" rather than "a read we
// chose not to use".
func detReadCouldSourceUnsourced(dc *detCall, adds []ir.FmlOp, unsourced []string) bool {
	if dc == nil || dc.query == nil {
		return false
	}
	shape := map[string]bool{}
	for _, hv := range dc.query.RowShape {
		shape[normHost(hv)] = true
	}
	if len(shape) == 0 {
		return false
	}
	want := map[string]bool{}
	for _, name := range unsourced {
		want[name] = true
	}
	// First add wins per response name, the same rule attribution uses.
	seen := map[string]bool{}
	for _, op := range adds {
		name := fieldFromFML(op.Field)
		if seen[name] || op.Target == "" || !want[name] {
			continue
		}
		seen[name] = true
		if shape[normHost(op.Target)] {
			return true
		}
	}
	return false
}

// detEmitShaping renders the data appends: one range loop per multi-row
// read, one nil-guarded append per single-row read, the scalar guess, or
// the read-less empty append. A response field no read can source rides a
// single TODO for the endpoint.
func detEmitShaping(sb *strings.Builder, ordered []*detCall, adds []ir.FmlOp, respFields []templates.FieldSpec, respType string, scope *walk.Scope) {
	var rows, singles []*detCall
	var scalars []*detCall
	for _, dc := range ordered {
		switch dc.shape {
		case "rows":
			rows = append(rows, dc)
		case "single":
			singles = append(singles, dc)
		case "scalar":
			scalars = append(scalars, dc)
		}
	}
	if len(rows) == 0 && len(singles) == 0 && len(scalars) == 0 {
		detEmitReadless(sb, ordered, respFields, respType)
		return
	}
	// Endpoint-wide attribution, computed once.
	owned := detRowSources(adds, respFields, scope)
	unsourced := detUnsourced(owned, respFields)
	for _, dc := range rows {
		pairs := detRowPairs(dc, adds, respFields, scope, owned)
		sb.WriteString("for _, row := range " + dc.capture + " {\n")
		sb.WriteString("\tdata = append(data, " + detLiteral(respType, pairs, "row") + ")\n")
		sb.WriteString("}\n")
	}
	for _, dc := range singles {
		pairs := detRowPairs(dc, adds, respFields, scope, owned)
		if !detReadFeedsResponse(dc, owned, scope) {
			// No response field is attributed to this read. Two very
			// different situations wear this shape, and P3C splits them:
			// a read whose row could have carried a missing value is a
			// real gap; a read that could not is finished work.
			//
			// The test is ATTRIBUTION, not "did any pair come out". Those
			// differ exactly when a read's only pairs came from fields
			// another read owns — the cross-read reading P3A exists to
			// stop. Testing pairs would let P3A turn a silently-wrong
			// mapping into a spurious note, and it did: this is the one
			// gap the ownership rule added before the test was made
			// attribution-aware.
			if detReadCouldSourceUnsourced(dc, adds, unsourced) {
				sb.WriteString("// tuxconv:TODO response fields without row source (zero values): " +
					strings.Join(unsourced, ", ") + " — " + dc.capture + " (" + dc.rowName +
					") carries the value but shaping did not place it\n")
				sb.WriteString("_ = " + dc.capture + "\n")
				continue
			}
			sb.WriteString("// tuxconv:TODO " + ResponseReadKeptMark + ": " + dc.capture + " (" + dc.rowName +
				") feeds no response field — the walk keeps it for its own sake; LLM need not map it\n")
			sb.WriteString("_ = " + dc.capture + "\n")
			continue
		}
		if len(pairs) == 0 {
			// Attributed a field, but nothing renderable came out — this
			// read's row shape has no host column for it. The endpoint
			// unsourced note names the field; keep the capture used.
			sb.WriteString("_ = " + dc.capture + "\n")
			continue
		}
		sb.WriteString("if " + dc.capture + " != nil {\n")
		sb.WriteString("\tdata = append(data, " + detLiteral(respType, pairs, dc.capture) + ")\n")
		sb.WriteString("}\n")
	}
	for i, dc := range scalars {
		if i == 0 && len(rows) == 0 && len(singles) == 0 && len(respFields) > 0 {
			sb.WriteString("// tuxconv:TODO scalar " + dc.capture + " mapped to " + respFields[0].Name + " by position — verify\n")
			sb.WriteString("data = append(data, &models." + respType + "{" + respFields[0].Name + ": fmt.Sprintf(\"%d\", " + dc.capture + ")})\n")
			continue
		}
		// Every capture must be used: a merged/multi-count slice reaches
		// several scalars, and only the first can ride the by-position
		// guess — the rest stay kept for their error checks.
		sb.WriteString("// tuxconv:TODO scalar " + dc.capture + " unused in shaping — LLM maps its branch role\n")
		sb.WriteString("_ = " + dc.capture + "\n")
	}
	// One gap for the endpoint, naming the fields nothing in the walk
	// produces. Emitted after the appends so it reads as the summary it is.
	if len(unsourced) > 0 {
		sb.WriteString("// tuxconv:TODO response fields without row source (zero values): " + strings.Join(unsourced, ", ") + "\n")
	}
}

// detRowPairs maps the response fields THIS read sources onto its row fields:
// FML-add target → row field via the provenance index, substring fallback,
// deterministic response order.
//
// It returns only pairs this read can actually render — a field owned by
// another read is not this read's to emit, and is not this read's gap either.
// The endpoint-level gap is detUnsourced's, reported once by the caller.
func detRowPairs(dc *detCall, adds []ir.FmlOp, respFields []templates.FieldSpec, scope *walk.Scope, owned map[string]int) []detFieldMap {
	byHost := map[string]string{}
	if dc != nil {
		byHost = dc.rowByHost
	}
	idx, _ := scope.ReadFor(dc.capture)
	targetOf := map[string]string{} // response Go name → FML-add target
	for _, op := range adds {
		name := fieldFromFML(op.Field)
		if _, ok := targetOf[name]; !ok && op.Target != "" {
			targetOf[name] = op.Target
		}
	}
	var pairs []detFieldMap
	for _, rf := range respFields {
		// Attributed elsewhere: not this read's field.
		if owner, ok := owned[rf.Name]; ok && owner != idx {
			continue
		}
		rowField := ""
		if t, ok := targetOf[rf.Name]; ok {
			rowField = byHost[normHost(t)]
		}
		if rowField == "" && dc != nil {
			// No authoritative host match. The substring fallback is the
			// only signal left, and it is a guess — but it is the same
			// guess the renderer always made, and removing it here would
			// turn working field maps into gaps. What P3A removes is the
			// fallback's reach ACROSS reads: a field already attributed to
			// another read never reaches this line.
			rowField = detFuzzyRowField(dc.rowFields, rf.Name)
		}
		if rowField == "" {
			continue
		}
		pairs = append(pairs, detFieldMap{Resp: rf.Name, Row: rowField})
	}
	return pairs
}

// detLiteral renders one `&models.Resp{...}` literal: mapped row fields
// read through .String, the receiver selecting row vs single captures.
func detLiteral(respType string, pairs []detFieldMap, recv string) string {
	var sb strings.Builder
	sb.WriteString("&models." + respType + "{")
	for i, pr := range pairs {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(pr.Resp + ": " + recv + "." + pr.Row + ".String")
	}
	sb.WriteString("}")
	return sb.String()
}

// detEmitReadless renders the read-less append: every response field is a
// zero value with a TODO (no row source exists to map from).
func detEmitReadless(sb *strings.Builder, ordered []*detCall, respFields []templates.FieldSpec, respType string) {
	_ = ordered
	if len(respFields) == 0 {
		return
	}
	sb.WriteString("// tuxconv:TODO no row source — response fields are zero values; LLM shapes them\n")
	sb.WriteString("data = append(data, &models." + respType + "{})\n")
}

// detClosureLine maps one method-body line into the tx-closure form:
// method two-value returns collapse to the error (data rides the outer
// named return), bare returns carry err.
func detClosureLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "return") {
		return line
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "return"))
	if rest == "" {
		return indent + "return err"
	}
	if i := strings.LastIndex(rest, ","); i >= 0 {
		return indent + "return " + strings.TrimSpace(rest[i+1:])
	}
	return line
}

// outParams lists the pointer (out) params of a fn helper.
func outParams(params []plan.FnParam) []plan.FnParam {
	var out []plan.FnParam
	for _, pr := range params {
		if strings.HasPrefix(strings.TrimSpace(pr.Type), "*") {
			out = append(out, pr)
		}
	}
	return out
}

// detEmitFnEvents renders a fn helper's store + nested-helper calls with
// the legacy int-status contract: store errors zero the out-params and
// return -1, row results stay visible via the blank identifier. Inside a tx
// closure (inClosure) failure returns carry err (the closure returns error
// only); tails are mapped by detClosureLineFn.
func detEmitFnEvents(sb *strings.Builder, ordered []*detCall, nested []detHelper, outs []plan.FnParam, voidRet, inClosure bool) {
	type event struct {
		line int
		kind int
		dc   *detCall
		h    *detHelper
	}
	var evs []event
	for _, dc := range ordered {
		evs = append(evs, event{line: dc.line, kind: 0, dc: dc})
	}
	for i := range nested {
		evs = append(evs, event{line: nested[i].line, kind: 1, h: &nested[i]})
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].line != evs[j].line {
			return evs[i].line < evs[j].line
		}
		return evs[i].kind < evs[j].kind
	})
	errDecl := false
	for _, ev := range evs {
		if ev.kind == 1 {
			detEmitFnHelper(sb, ev.h, outs, voidRet, inClosure)
			continue
		}
		detEmitFnStoreCall(sb, ev.dc, outs, voidRet, inClosure, &errDecl)
	}
}

// detEmitFnStoreCall renders one checked store call under the int-status
// contract (no named err in scope — the first error-only call declares it).
func detEmitFnStoreCall(sb *strings.Builder, dc *detCall, outs []plan.FnParam, voidRet, inClosure bool, errDecl *bool) {
	call := detCallLine(dc)
	if dc.shape == "error" {
		if *errDecl {
			sb.WriteString("err = " + call + "\n")
		} else {
			sb.WriteString("err := " + call + "\n")
			*errDecl = true
		}
		sb.WriteString("if err != nil {\n")
		sb.WriteString(detZeroOuts(outs, "\t"))
		sb.WriteString(detFnErrReturn(voidRet, inClosure, "\t") + "\n}")
		return
	}
	sb.WriteString(dc.capture + ", err := " + call + "\n")
	*errDecl = true
	sb.WriteString("if err != nil {\n")
	sb.WriteString(detZeroOuts(outs, "\t"))
	sb.WriteString(detFnErrReturn(voidRet, inClosure, "\t") + "\n}")
	if dc.shape == "rows" || dc.shape == "single" || dc.shape == "scalar" {
		// Both newlines. The leading one separates this from the error
		// check above; the trailing one is what keeps the NEXT statement
		// off this line. Without it a store call followed by anything else
		// fused into `_ = capcapturename := ...`, which does not parse —
		// and because a rejected helper is dropped rather than emitted,
		// that surfaced as the whole method missing from fns.go while the
		// caller still called it. It only ever bit when the store call was
		// NOT the last statement, which is why a single-call helper looked
		// fine.
		sb.WriteString("\n_ = " + dc.capture + "\n")
	}
}

// detEmitFnHelper renders one nested helper call under the int-status
// contract.
func detEmitFnHelper(sb *strings.Builder, h *detHelper, outs []plan.FnParam, voidRet, inClosure bool) {
	call := "s." + h.goName + "(c"
	for _, a := range h.args {
		call += ", " + a
	}
	call += ")"
	if !h.retInt {
		sb.WriteString(call + "\n")
		return
	}
	sb.WriteString(h.cap + " := " + call + "\n")
	sb.WriteString("if " + h.cap + " == -1 {\n")
	sb.WriteString(detZeroOuts(outs, "\t"))
	sb.WriteString(detFnErrReturn(voidRet, inClosure, "\t") + "\n}")
}

// detZeroOuts renders the out-param zeroing block (nil-guarded).
func detZeroOuts(outs []plan.FnParam, indent string) string {
	var sb strings.Builder
	for _, o := range outs {
		elem := strings.TrimSpace(strings.TrimPrefix(o.Type, "*"))
		sb.WriteString(indent + "if " + o.Name + " != nil {\n")
		sb.WriteString(indent + "\t*" + o.Name + " = " + detZero(elem) + "\n")
		sb.WriteString(indent + "}\n")
	}
	return sb.String()
}

// detFnErrReturn renders the failure return for the int-status contract:
// -1 (bare return for void), or the closure's err inside tx.
func detFnErrReturn(voidRet, inClosure bool, indent string) string {
	if inClosure {
		return indent + "return err"
	}
	if voidRet {
		return indent + "return"
	}
	return indent + "return -1"
}

// detFnTail renders the success tail for the int-status contract: the
// legacy success value read off the fn source (0 when undecidable).
func detFnTail(fnSrc string, h *plan.FnHelper) string {
	if h.Return == "" {
		return "return"
	}
	return "return " + detFnSuccessRet(fnSrc)
}

// detFnSuccessRet reads the legacy success value off the fn source: the
// single distinct non-(-1) integer return literal, else 0. Callers compare
// `== -1` for failure (the corpus convention), so the exact success value
// is advisory — but carrying it keeps status checks byte-faithful.
func detFnSuccessRet(fnSrc string) string {
	seen := map[string]bool{}
	var vals []string
	for _, line := range strings.Split(fnSrc, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "return") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "return"))
		rest = strings.TrimSpace(strings.TrimSuffix(rest, ";"))
		// C writes the literal either bare or parenthesised, and this
		// corpus uses both: `return 1;` in fn_insert_into_ura,
		// `return(1);` in both tpcall helpers. Without this the
		// parenthesised form fails the digit check below, contributes no
		// value, and the function silently falls back to 0 — a status the
		// legacy code never returns.
		rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")"))
		rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")"))
		if rest == "" || rest == "-1" {
			continue
		}
		v := strings.TrimSpace(rest)
		if strings.HasPrefix(v, "-") {
			continue // only the -1 failure literal is negative
		}
		ok := v != ""
		for _, r := range v {
			if r < '0' || r > '9' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	if len(vals) == 1 {
		return vals[0]
	}
	return "0"
}

// detFnErrTail renders the post-wrapper failure tail.
func detFnErrTail(h *plan.FnHelper) string {
	if h.Return == "" {
		return "\treturn"
	}
	if outs := detZeroOuts(outParams(h.Params), "\t"); outs != "" {
		return outs + "\treturn -1"
	}
	return "\treturn -1"
}

// detClosureLineFn maps one fn-helper body line into the tx-closure form
// (the closure returns error only): failure returns carry err, every other
// single-value return is success (nil), bare void returns are nil.
func detClosureLineFn(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "return") {
		return line
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "return"))
	switch rest {
	case "", "0":
		return indent + "return nil"
	case "-1":
		return indent + "return err"
	case "err", "nil":
		return line
	}
	if i := strings.LastIndex(rest, ","); i >= 0 {
		return indent + "return " + strings.TrimSpace(rest[i+1:])
	}
	// Any other single value (e.g. the legacy success literal) is the
	// success path inside the closure.
	return indent + "return nil"
}

// detFuzzyRowField falls back to a substring match between response and row
// names (alias-shaped rows where the host var left no trace) — mirrors the
// prompt's responseShaping fallback.
func detFuzzyRowField(fields []templates.FieldSpec, resp string) string {
	rl := strings.ToLower(resp)
	for _, f := range fields {
		fl := strings.ToLower(f.Name)
		if strings.Contains(fl, rl) || strings.Contains(rl, fl) {
			return f.Name
		}
	}
	return ""
}
