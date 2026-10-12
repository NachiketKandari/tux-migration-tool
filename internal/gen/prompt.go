package gen

import (
	"fmt"
	"slices"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/templates"
)

// ConditionOf returns the condition backing an endpoint name.
func (s *Service) ConditionOf(endpoint string) *ir.Condition {
	for _, e := range s.Mapping.Endpoints {
		if e.Name == endpoint {
			return s.conditionOf(e)
		}
	}
	return nil
}

// ScenarioOf returns the scenario slice backing an endpoint name (nil for
// condition/conditionRef endpoints) — the flattened-view input the
// controller prompt consumes (G-SCEN6). Memoized: one fold per endpoint.
func (s *Service) ScenarioOf(endpoint string) *flow.Scenario {
	e := s.endpointOf(endpoint)
	if e == nil || (e.ScenarioRef == "" && e.ScenarioFilter == "") {
		return nil
	}
	if sc, ok := s.scenMemo[endpoint]; ok {
		return sc
	}
	sc := s.scenarioOf(*e)
	if s.scenMemo == nil {
		s.scenMemo = map[string]*flow.Scenario{}
	}
	s.scenMemo[endpoint] = sc
	return sc
}

func (s *Service) endpointOf(endpoint string) *plan.Endpoint {
	for i := range s.Mapping.Endpoints {
		if s.Mapping.Endpoints[i].Name == endpoint {
			return &s.Mapping.Endpoints[i]
		}
	}
	return nil
}

// scenarioOf resolves a scenarioRef/scenarioFilter endpoint's slice from the
// flow tree (SCEN-D7: re-derivation from source — the stale-artifact-proof
// pattern). Degrades to nil on any mismatch; the plan already validated the
// ref, so this is defensive only.
func (s *Service) scenarioOf(e plan.Endpoint) *flow.Scenario {
	t := s.treeFor()
	if t == nil {
		return nil
	}
	if e.ScenarioFilter != "" {
		filter, err := flow.ParseScenarioFilter(e.ScenarioFilter)
		if err != nil {
			return nil
		}
		sc, err := flow.ScenarioForFilter(t, t.AxesFor([]byte(s.source)), filter)
		if err != nil {
			return nil
		}
		return sc
	}
	key, value, err := plan.ParseScenarioRef(e.ScenarioRef)
	if err != nil {
		return nil
	}
	axis := t.DispatchAxisFor([]byte(s.source))
	if axis == nil || axis.Key() != key {
		return nil
	}
	if !slices.Contains(axis.Domain, value) && !(axis.HasDefault && value == axis.DefaultKey()) {
		return nil
	}
	return flow.ScenarioFor(t, axis, value)
}

// treeFor builds the entry's flow tree once (the shared flow.TreeFor
// derivation — conditionRef and scenarioRef limbs share the memo).
func (s *Service) treeFor() *flow.Tree {
	if s.flowTree != nil {
		return s.flowTree
	}
	if strings.TrimSpace(s.source) == "" {
		return nil
	}
	t, err := flow.TreeFor(s.source, s.Main)
	if err != nil {
		return nil
	}
	s.flowTree = t
	return t
}

// BranchCalls returns the condition's canonical queries re-based to the
// branch slice plus their resolved DBCall map — the input budget.ReplaceQueries
// needs to rewrite the branch view (plan-conversion §4 step 4).
func (s *Service) BranchCalls(c *ir.Condition, p *plan.Plan) ([]*ir.Query, map[string]budget.DBCall, error) {
	// Collect the branch's queries (explicit references, else line overlap).
	var ids []string
	seen := map[string]bool{}
	ids = c.QueryIDs
	if len(ids) == 0 {
		for _, q := range s.Main.Queries {
			if q.StartLine >= c.StartLine && q.StartLine <= c.EndLine {
				ids = append(ids, q.ID)
			}
		}
	}
	calls, err := s.StoreCallsFor(ids, p)
	if err != nil {
		return nil, nil, err
	}
	// The physical SQL region replaced in THIS branch is the referenced
	// site's — a shared query (q5→q3) has its own lines in each branch.
	var queries []*ir.Query
	for _, id := range ids {
		orig := s.queries[id]
		if orig == nil || seen[orig.ID] {
			continue
		}
		seen[orig.ID] = true
		lq := *orig
		lq.StartLine -= c.StartLine - 1
		lq.EndLine -= c.StartLine - 1
		queries = append(queries, &lq)
	}
	return queries, calls, nil
}

// StoreCallsFor resolves the DBCall per referenced query site: the canonical
// plan unit names the method, the pin + binds name the args, and tx-variant
// DML units insert the tx handle the template signature demands (G-SCEN6).
func (s *Service) StoreCallsFor(ids []string, p *plan.Plan) (map[string]budget.DBCall, error) {
	dbUnitByQuery := map[string]plan.Unit{}
	for _, u := range p.Units {
		if u.Kind == plan.KindDBMethod && len(u.QueryIDs) > 0 {
			dbUnitByQuery[u.QueryIDs[0]] = u
		}
	}
	calls := make(map[string]budget.DBCall, len(ids))
	for _, id := range ids {
		orig := s.queries[id]
		if orig == nil {
			return nil, fmt.Errorf("gen: condition references unknown query %q", id)
		}
		if _, dup := calls[orig.ID]; dup {
			continue
		}
		canon := orig
		if orig.DuplicateOf != "" {
			canon = s.queries[orig.DuplicateOf]
		}
		u, ok := dbUnitByQuery[canon.ID]
		if !ok {
			return nil, fmt.Errorf("gen: query %s has no db plan unit", canon.ID)
		}
		pin, _ := s.Pin(canon.ID)
		params, err := s.dbParams(canon, pin)
		if err != nil {
			return nil, err
		}
		args := make([]string, len(params))
		for j, pp := range params {
			args[j] = pp.Name
		}
		call := budget.DBCall{
			Receiver: common.GoCtrlRecv + "." + common.GoStoreField,
			Name:     u.Name,
			CtxName:  common.GoCtx,
			Args:     args,
		}
		if q := s.Query(canon.ID); u.Tx && q != nil && q.Type.IsDML() && q.Type != ir.QueryMerge {
			call.Tx = common.GoTx
		}
		calls[orig.ID] = call
	}
	return calls, nil
}

// ControllerPromptContext renders the fixed contract a controller prompt
// consumes: the exact method signature (named returns data/err) plus the
// endpoint's request/response/row shapes as compact name lists — exact Go
// names (the live-smoke finding: the model guesses ActiveFlg vs CActiveFlag
// when names aren't quoted), FML source per contract field, uniform types
// stated once (contract fields are all string; row fields are all
// sql.NullString). No struct tags or per-field types: the body never emits
// them, so they are pure token cost.
func (s *Service) ControllerPromptContext(endpoint string, p *plan.Plan, storeMethods []string) (string, error) {
	c := s.ConditionOf(endpoint)
	if c == nil {
		return "", fmt.Errorf("gen: no condition for endpoint %s", endpoint)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "func (%s *%s) %s(%s context.Context, request *models.%s) (data []*models.%s, err error)\n",
		common.GoCtrlRecv, common.LowerFirst(s.Mapping.Service)+"Controller", endpoint,
		common.GoCtx, s.requestType(endpoint), s.responseType(endpoint))
	// The request contract mirrors ModelFile's derivation (§4.8.3 —
	// requestFields): branch FmlGet ops, unioned with the entry-preamble
	// Fgets whose host vars the branch consumes (live-run fix).
	var ep plan.Endpoint
	for _, e := range s.Mapping.Endpoints {
		if e.Name == endpoint {
			ep = e
			break
		}
	}
	sb.WriteString("request " + s.requestType(endpoint) + " (all string): " + compactFields(s.requestFields(ep, c)) + "\n")
	sb.WriteString("response " + s.responseType(endpoint) + " (all string): " + compactFields(contractFields(c.FmlOps, ir.FmlAdd)) + "\n")

	rows := map[string]bool{}
	rowQuery := map[string]*ir.Query{}
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 || !slices.Contains(storeMethods, u.Name) {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil || q.Type.IsDML() || isCountQuery(q) {
			// DML units (incl. MERGE) return no rows; COUNT singles return
			// scalars — neither contributes a row struct to the contract
			// (live-run fix: a MERGE endpoint previously hard-errored here).
			continue
		}
		row := s.RowName(u.QueryIDs[0], u.Name)
		if rows[row] {
			continue
		}
		fields, err := s.rowFields(u.QueryIDs[0], q)
		if err != nil {
			return "", err
		}
		rows[row] = true
		rowQuery[row] = q
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.Name)
		}
		sb.WriteString("row " + row + " (all sql.NullString, read row.X.String): " + strings.Join(names, ", ") + "\n")
	}

	// Response shaping + input provenance (item-golden parity): the SQL
	// replacement elides the FETCH/Fadd loop, so the view shows a bare
	// store call with no loop and no field mapping — without this block
	// the model must guess both. The block restates the known seams:
	// the db interface (store input/output), the row structs (every
	// field sql.NullString, read via .String) vs the controller
	// response (every field string), the per-field row→response map
	// derived from the FML ops' host-var targets, and each store arg's
	// provenance (request field vs prior store result vs local).
	sb.WriteString(responseShaping(s, c, p, endpoint, storeMethods, rowQuery))

	// External interactions surface as compilable placeholders (PF-4.5): the
	// body calls the stub instead of inventing an outbound call. Endpoints
	// without tpcalls keep prompts unchanged.
	if sigs := s.PlaceholderSignatures(c, p); len(sigs) > 0 {
		sb.WriteString("\nexternal service call placeholders (call these instead of the legacy tpcall; each returns an error — handle it like a store error and return nil, err):\n")
		for _, sig := range sigs {
			sb.WriteString(sig + "\n")
		}
	}
	return sb.String(), nil
}

// compactFields renders contract fields as bare names with their FML source:
// "CompCd [FML_COMP_CD], Account [FML_ACCOUNT]" — exact Go names for the
// body, FML tags for traceability against the view's Fget/Fadd lines.
func compactFields(fields []templates.FieldSpec) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.JSONTag != "" {
			parts = append(parts, f.Name+" ["+f.JSONTag+"]")
		} else {
			parts = append(parts, f.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// normHost normalizes a Pro*C host-var reference for target matching:
// qualified/indicator/array noise stripped (ST.TBL.col → col,
// "sql_x INDICATOR" → sql_x, s.arr → s), the sql_ prefix dropped,
// lowercased. RowShape entries and FML-op targets normalize to the same
// key, so an Fadd target finds its FETCH-INTO column even when the row
// struct names the field from the SELECT alias.
func normHost(s string) string {
	s = strings.TrimSpace(s)
	if j := strings.IndexAny(s, " \t"); j > 0 {
		s = s[:j]
	}
	if j := strings.LastIndex(s, "."); j >= 0 {
		s = s[j+1:]
	}
	s = strings.TrimSuffix(s, ".arr")
	s = strings.TrimSuffix(s, "[0]")
	if j := strings.Index(s, "["); j >= 0 {
		s = s[:j]
	}
	s = strings.TrimSuffix(s, ".arr")
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sql_")
	return strings.Trim(s, "_")
}

// responseShaping renders the compact seam block the flattened view cannot
// carry: store input/output per method, the row-vs-response type rule, the
// row→response field map, and each store arg's provenance. Short by design
// — a few lines per method, not a second copy of the structs above.
func responseShaping(s *Service, c *ir.Condition, p *plan.Plan, endpoint string, storeMethods []string, rowQuery map[string]*ir.Query) string {
	// Endpoint's methods in plan order (deterministic, matches the view's
	// REQUIRED-CALLS order only when the view lists them in legacy order —
	// plan order is the stable choice here).
	var units []plan.Unit
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 || !slices.Contains(storeMethods, u.Name) {
			continue
		}
		units = append(units, u)
	}
	if len(units) == 0 {
		return ""
	}
	// Request field lookup: normalized host var → request Go field, from
	// the same FmlGet derivation the request struct uses (branch gets
	// plus consumed preamble gets).
	var ep plan.Endpoint
	for _, e := range s.Mapping.Endpoints {
		if e.Name == endpoint {
			ep = e
			break
		}
	}
	reqByHost := map[string]string{}
	for _, f := range s.requestFields(ep, c) {
		reqByHost["_by_name_"+strings.ToLower(f.Name)] = f.Name
	}
	getOps := []ir.FmlOp{}
	for _, op := range c.FmlOps {
		if op.Kind == ir.FmlGet && !op.Dropped && !op.Error {
			getOps = append(getOps, op)
		}
	}
	if s.Main != nil && s.source != "" {
		branchSrc := ""
		if lines := s.sourceLines(); c.EndLine <= len(lines) && c.StartLine >= 1 && c.EndLine > c.StartLine {
			branchSrc = strings.Join(lines[c.StartLine-1:c.EndLine], "\n")
		}
		for _, op := range s.Main.FmlOps {
			if op.Kind != ir.FmlGet || op.Dropped || op.Target == "" || !strings.Contains(branchSrc, op.Target) {
				continue
			}
			dup := false
			for _, g := range getOps {
				if g.Field == op.Field {
					dup = true
					break
				}
			}
			if !dup {
				getOps = append(getOps, op)
			}
		}
	}
	for _, op := range getOps {
		if op.Target != "" {
			reqByHost[normHost(op.Target)] = fieldFromFML(op.Field)
		}
	}
	// Row-shape lookup across the endpoint's queries: normalized host var
	// → producing method, so a bind fed by another store result names it.
	shapeByHost := map[string]string{}
	for _, u := range units {
		q := s.Query(u.QueryIDs[0])
		if q == nil {
			continue
		}
		for _, hv := range q.RowShape {
			if k := normHost(hv); k != "" {
				if _, ok := shapeByHost[k]; !ok {
					shapeByHost[k] = u.Name
				}
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("\nResponse shaping — the FETCH/Fadd loop the SQL replacement elided (implement exactly this; the view shows no loop):\n")
	respFields := contractFields(c.FmlOps, ir.FmlAdd)
	for _, u := range units {
		q := s.Query(u.QueryIDs[0])
		if q == nil {
			continue
		}
		pin, _ := s.Pin(u.QueryIDs[0])
		params, _ := s.dbParams(q, pin)
		argNames := make([]string, len(params))
		for i, pp := range params {
			argNames[i] = pp.Name + " " + pp.Type
		}
		storeCall := common.GoStoreCallPrefix() + u.Name
		ctxArgs := append([]string{common.GoCtx}, argNames...)
		switch {
		case q.Type.IsDML():
			fmt.Fprintf(&sb, "- %s(%s) returns error only.\n", storeCall, strings.Join(ctxArgs, ", "))
			continue
		case isCountQuery(q):
			fmt.Fprintf(&sb, "- %s(%s) returns int64 scalar — use directly, no struct, no loop.\n", storeCall, strings.Join(ctxArgs, ", "))
		case q.Type == ir.QuerySelectSingle:
			row := s.RowName(u.QueryIDs[0], u.Name)
			fmt.Fprintf(&sb, "- %s(%s) returns *models.%s (single row — read fields directly, no loop).\n", storeCall, strings.Join(ctxArgs, ", "), row)
		default:
			row := s.RowName(u.QueryIDs[0], u.Name)
			fmt.Fprintf(&sb, "- %s(%s) returns []*models.%s → one %s per row, in order.\n", storeCall, strings.Join(ctxArgs, ", "), row, s.responseType(endpoint))
		}
		// Arg provenance.
		if len(params) > 0 && len(q.Binds) > 0 {
			parts := make([]string, 0, len(params))
			for i, pp := range params {
				if i >= len(q.Binds) {
					break
				}
				bk := normHost(q.Binds[i])
				switch {
				case bk != "" && reqByHost[bk] != "":
					parts = append(parts, pp.Name+" ← request."+reqByHost[bk])
				case bk != "" && shapeByHost[bk] != "" && shapeByHost[bk] != u.Name:
					parts = append(parts, pp.Name+" ← "+shapeByHost[bk]+" result")
				default:
					parts = append(parts, pp.Name+" ← local (see view)")
				}
			}
			sb.WriteString("  args: " + strings.Join(parts, ", ") + "\n")
		}
	}
	// Row→response map for the multi-row result(s): FML add target → row
	// field (via RowShape position, alias-proof) → response field.
	mapped := false
	// Response field name → the first matching FML add's target and raw
	// field. Built once from c.FmlOps: the lookup below ran per (store
	// method × response field) over the same op list, which is O(units ×
	// respFields × ops) per endpoint. First match wins, matching the
	// break-on-first the scan used.
	fmlAddByField := map[string][2]string{}
	for _, op := range c.FmlOps {
		if op.Kind != ir.FmlAdd || op.Dropped || op.Error {
			continue
		}
		name := fieldFromFML(op.Field)
		if _, seen := fmlAddByField[name]; !seen {
			fmlAddByField[name] = [2]string{op.Target, op.Field}
		}
	}
	for _, u := range units {
		q := s.Query(u.QueryIDs[0])
		if q == nil || q.Type != ir.QuerySelectMulti {
			continue
		}
		row := s.RowName(u.QueryIDs[0], u.Name)
		fields, err := s.rowFields(u.QueryIDs[0], q)
		if err != nil || len(respFields) == 0 {
			continue
		}
		byHost := map[string]string{}
		for i, hv := range q.RowShape {
			if i < len(fields) {
				byHost[normHost(hv)] = fields[i].Name
			}
		}
		sb.WriteString("  map " + row + " → " + s.responseType(endpoint) + ":\n")
		for _, rf := range respFields {
			var target string
			var fml string
			if hit, ok := fmlAddByField[rf.Name]; ok {
				target, fml = hit[0], hit[1]
			}
			rowField := ""
			if target != "" {
				rowField = byHost[normHost(target)]
			}
			if rowField == "" {
				// Fallback: response name contains row name or vice
				// versa (alias-shaped rows where the host var left no
				// trace) — still pins the intended pair explicitly.
				rl, sl := strings.ToLower(rf.Name), ""
				for _, f := range fields {
					fl := strings.ToLower(f.Name)
					if strings.Contains(fl, rl) || strings.Contains(rl, fl) {
						sl = f.Name
						break
					}
				}
				rowField = sl
			}
			if rowField != "" {
				fmt.Fprintf(&sb, "    %s ← %s.String\n", rf.Name, rowField)
				mapped = true
			} else if fml != "" {
				fmt.Fprintf(&sb, "    %s ← ? (%s, no row column matched — use the closest row field)\n", rf.Name, fml)
			}
		}
	}
	if !mapped {
		fmt.Fprintf(&sb, "  init: data = make([]*models.%s, 0), then append the shaped element directly (no loop)\n", s.responseType(endpoint))
	} else {
		fmt.Fprintf(&sb, "  init: data = make([]*models.%s, 0); then append the shaped element once per returned row\n  (range over the slice you captured from the %s call above; declare the capture)\n", s.responseType(endpoint), common.GoStoreCallPrefix())
	}
	return sb.String()
}

// RenderControllerMethod wraps an accepted LLM body in the controller
// method template — the template owns the shape, the LLM owns only the
// template-shaped gap.
func (s *Service) RenderControllerMethod(endpoint, body string) (string, error) {
	return s.render(templates.ControllerMethod, templates.ControllerMethodData{
		StructName:   common.LowerFirst(s.Mapping.Service) + "Controller",
		Name:         endpoint,
		Receiver:     common.GoCtrlRecv,
		CtxName:      common.GoCtx,
		RequestType:  "models." + s.requestType(endpoint),
		ResponseType: "models." + s.responseType(endpoint),
		Body:         strings.TrimRight(body, "\n"),
	})
}
