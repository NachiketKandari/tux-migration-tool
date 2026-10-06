package csplan

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/namer"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/pred"
	"tux-migration-tool/internal/sqltext"
)

// Param is one named Oracle parameter of a repo method: the Oracle param
// name, the bind host var it came from, and the request property that
// supplies it ("" when the mapping leaves it unmapped — the service body
// must supply the value, which is LLM-seam/TODO territory).
type Param struct {
	Name        string `json:"name"`
	Bind        string `json:"bind"`
	RequestProp string `json:"request_prop,omitempty"`
}

// Prop is one DTO property of a response class, derived from the query's
// row shape (the INTO host var, indicator dropped, type prefixes stripped,
// upper-snake named like the reference convention).
type Prop struct {
	Name string `json:"name"`
	Bind string `json:"bind"`
}

// QueryPlan is one database unit's C# shape: the NamedQueries const, the
// repo method kind, the ordered named parameters, and the DTO row
// properties for SELECTs. Line is the query's source span — the cursor
// units' span covers the whole DECLARE→CLOSE choreography — and anchors
// the LLM seam's arm view (SQL regions replaced by the deterministic
// repo calls).
type QueryPlan struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // select-single | select-multi | dml | merge
	Name     string  `json:"name"` // const name (GetCUSTDetailsQuery)
	SQL      string  `json:"sql"`
	Params   []Param `json:"params"`
	RowProps []Prop  `json:"row_props,omitempty"`
	DML      bool    `json:"dml,omitempty"`
	Line     [2]int  `json:"line,omitempty"`
}

// Guard is one runtime-dispatch guard the arm must keep live (the S7 CS
// twin): the residual predicate plus the C# spellings that bind to its
// identifiers (legacy name, mapping request field, suggested prop, and the
// FML-derived request name). The CS seam gates its accepted residual block
// against these with predicate equivalence.
type Guard struct {
	Line int    `json:"line"`
	Cond string `json:"cond"`
	// Alt is the original pre-fold predicate when it differs (single-value
	// slices may keep the full legacy condition). Empty for merged filters.
	Alt  string            `json:"alt,omitempty"`
	Bind map[string]string `json:"bind,omitempty"` // spellings (lowercased camel) → legacy ident
}

// EndpointPlan is one action's C# shape: the controller action, its
// response DTO, the request properties it reads, and its queries.
type EndpointPlan struct {
	Name     string `json:"name"`
	Route    string `json:"route"`
	Scenario string `json:"scenario,omitempty"` // scenarioRef key when arm-sliced
	// Filter is the scenarioFilter expression when the arm comes from a
	// filter endpoint ("" for scenarioRef / condition arms); Matched/Pruned
	// are its surviving and reachability-pruned assignment keys.
	Filter        string   `json:"filter,omitempty"`
	FilterMatched []string `json:"filter_matched,omitempty"`
	FilterPruned  []string `json:"filter_pruned,omitempty"`
	DTOName       string   `json:"dto_name"`
	Requests      []string `json:"requests"` // request properties read, in bind order
	QueryIDs      []string `json:"query_ids"`
	Residue       []string `json:"residue,omitempty"` // loud slice residue (SCEN-D4)
	Guards        []Guard  `json:"guards,omitempty"`  // FoldMixed runtime-dispatch guards (S7)
	LineSpan      [2]int   `json:"line_span"`         // source span of the arm (the LLM seam's code view)
	SourceSpan    string   `json:"source_span,omitempty"`
}

// Plan is the deterministic convertcs decomposition for one entry file.
type Plan struct {
	Source string `json:"source"`
	Entry  string `json:"entry"`

	Namespace  string `json:"namespace"`
	Area       string `json:"area"`
	Component  string `json:"component"`
	ApiVersion string `json:"api_version"`
	RequestDTO string `json:"request_dto"`
	Validator  string `json:"validator,omitempty"`

	Controller string `json:"controller"` // <Component>Controller
	Service    string `json:"service"`    // <Component>Service
	Repo       string `json:"repo"`       // <Component>Repository
	QueriesCls string `json:"queries_cls"`
	DTOCls     string `json:"dto_cls"`

	Endpoints []EndpointPlan `json:"endpoints"`
	Queries   []QueryPlan    `json:"queries"`

	// Warnings are the arm-coverage advisories (D4): every condition the
	// inventory carries that no endpoint covers — the tool never invents
	// endpoints, and it never stays silent about a reachable dispatch arm.
	Warnings []string `json:"warnings,omitempty"`
}

// Options carries the plan inputs: the entry IR, the source text, and the
// user mapping.
type Options struct {
	Main    *ir.File
	Source  string
	Mapping *Mapping
}

// Build constructs the plan. Deterministic: identical inputs produce a
// byte-identical plan.
func Build(opts Options) (*Plan, error) {
	if opts.Main == nil || opts.Mapping == nil {
		return nil, fmt.Errorf("csplan: main IR and mapping are required")
	}
	if strings.TrimSpace(opts.Source) == "" {
		return nil, fmt.Errorf("csplan: %s needs its source text", opts.Main.Path)
	}
	m := opts.Mapping
	p := &Plan{
		Source:     opts.Main.Path,
		Entry:      opts.Main.Entry,
		Namespace:  m.Namespace,
		Area:       strings.Trim(m.Area, "."),
		Component:  m.Component,
		ApiVersion: m.ApiVersion,
		RequestDTO: m.RequestDTO,
		Validator:  m.Validator,
		Controller: m.Component + "Controller",
		Service:    m.Component + "Service",
		Repo:       m.Component + "Repository",
		QueriesCls: m.Component + "Queries",
		DTOCls:     m.Component + "DTO",
	}
	if p.ApiVersion == "" {
		p.ApiVersion = "1.0"
	}
	if p.RequestDTO == "" {
		p.RequestDTO = "object"
	}

	// The flow tree powers both reference forms: scenarioRef endpoints fold
	// through the dispatch axis, conditionRef endpoints through the tree.
	var tree *flow.Tree
	treeFor := func() (*flow.Tree, error) {
		if tree != nil {
			return tree, nil
		}
		t, err := flow.TreeFor(opts.Source, opts.Main)
		if err != nil {
			return nil, fmt.Errorf("csplan: flow tree for %s: %w", opts.Main.Path, err)
		}
		tree = t
		return t, nil
	}

	queryByID := make(map[string]*ir.Query, len(opts.Main.Queries))
	for _, q := range opts.Main.Queries {
		queryByID[q.ID] = q
	}
	hostVars := map[string]bool{}
	for _, hv := range opts.Main.HostVars {
		// Only typed host vars count: the extractor's format-literal leak
		// (the `:mi:ss` inside 'dd-Mon-yyyy hh24:mi:ss' surfacing as binds)
		// carries no declaration, so those pseudo-binds never reach the
		// repo signature.
		if hv.CType != "" {
			hostVars[hv.Name] = true
		}
	}

	canonical := map[string]bool{}
	seenEp := map[string]bool{}
	// Per-endpoint coverage evidence for the arm-coverage advisory (D4).
	type epCover struct {
		scen *flow.Scenario
		cond *ir.Condition
	}
	var covers []epCover
	for _, e := range m.Endpoints {
		ep := EndpointPlan{
			Name:    e.Name,
			Route:   e.Route,
			DTOName: e.Name + "Response",
		}
		var cond *ir.Condition
		var scen *flow.Scenario
		switch {
		case e.ScenarioRef != "":
			t, err := treeFor()
			if err != nil {
				return nil, err
			}
			axis := t.DispatchAxisFor([]byte(opts.Source))
			key, value, err := plan.ParseScenarioRef(e.ScenarioRef)
			if err != nil {
				// A malformed ref is a mapping bug — the error names the
				// axis the file actually dispatches on (the fix needs
				// both facts).
				return nil, fmt.Errorf("csplan: endpoint %s: %w (the file dispatches on %s)",
					e.Name, err, axisRefSummary(t, opts.Source))
			}
			if axis == nil || axis.Key() != key {
				return nil, fmt.Errorf("csplan: endpoint %s references scenario axis %q — the file dispatches on %s",
					e.Name, key, axisRefSummary(t, opts.Source))
			}
			scen = flow.ScenarioFor(t, axis, value)
			if scen == nil {
				return nil, fmt.Errorf("csplan: endpoint %s: no scenario slice for %s", e.Name, e.ScenarioRef)
			}
			ep.Scenario = scen.Key
			ep.Residue = scen.Residue
			ep.Filter, ep.FilterMatched, ep.FilterPruned = scen.Filter, scen.FilterMatched, scen.FilterPruned
			cond = flow.ScenarioCondition(scen, t)
			if cond == nil {
				return nil, fmt.Errorf("csplan: endpoint %s: scenario %s has no condition", e.Name, e.ScenarioRef)
			}
		case e.ScenarioFilter != "":
			t, err := treeFor()
			if err != nil {
				return nil, err
			}
			registry := t.AxesFor([]byte(opts.Source))
			if len(registry) == 0 {
				return nil, fmt.Errorf("csplan: endpoint %s references scenarioFilter %q but the entry function has no dispatch axes", e.Name, e.ScenarioFilter)
			}
			filter, err := flow.ParseScenarioFilter(e.ScenarioFilter)
			if err != nil {
				return nil, fmt.Errorf("csplan: endpoint %s: %w", e.Name, err)
			}
			scen, err = flow.ScenarioForFilter(t, registry, filter)
			if err != nil {
				return nil, fmt.Errorf("csplan: endpoint %s: %w", e.Name, err)
			}
			ep.Scenario = scen.Key
			ep.Residue = scen.Residue
			ep.Filter, ep.FilterMatched, ep.FilterPruned = scen.Filter, scen.FilterMatched, scen.FilterPruned
			cond = flow.ScenarioCondition(scen, t)
			if cond == nil {
				return nil, fmt.Errorf("csplan: endpoint %s: scenarioFilter %s has no condition", e.Name, e.ScenarioFilter)
			}
		case e.ConditionRef != "":
			t, err := treeFor()
			if err != nil {
				return nil, err
			}
			c, err := flow.ConditionFor(t, opts.Main.Conditions, e.ConditionRef)
			if err != nil {
				return nil, fmt.Errorf("csplan: endpoint %s: %w", e.Name, err)
			}
			cond = c
		default:
			cond = opts.Main.Condition(e.Condition)
			if cond == nil {
				return nil, fmt.Errorf("csplan: endpoint %s maps condition %d — inventory has %d conditions",
					e.Name, e.Condition, len(opts.Main.Conditions))
			}
		}

		if scen != nil {
			ep.Guards = buildGuards(scen, cond, m)
		}

		ids := cond.QueryIDs
		if len(ids) == 0 {
			ids = queriesInSpan(opts.Main, cond)
		}
		ep.LineSpan = [2]int{cond.StartLine, cond.EndLine}
		ep.SourceSpan = fmt.Sprintf("%d-%d", cond.StartLine, cond.EndLine)
		seenQuery := map[string]bool{}
		for _, id := range ids {
			q := queryByID[id]
			if q == nil {
				return nil, fmt.Errorf("csplan: endpoint %s references unknown query %q", e.Name, id)
			}
			if q.DuplicateOf != "" {
				q = queryByID[q.DuplicateOf]
			}
			// One entry per canonical query: a merged filter arm can reach
			// the same query through two sites (an F and an I count), and
			// the generator emits one const/method per entry.
			if seenQuery[q.ID] {
				continue
			}
			seenQuery[q.ID] = true
			ep.QueryIDs = append(ep.QueryIDs, q.ID)
			if !canonical[q.ID] {
				canonical[q.ID] = true
				qp, err := buildQueryPlan(q, m, hostVars)
				if err != nil {
					return nil, fmt.Errorf("csplan: query %s: %w", q.ID, err)
				}
				p.Queries = append(p.Queries, qp)
			}
		}
		if len(ep.QueryIDs) == 0 {
			return nil, fmt.Errorf("csplan: endpoint %s (%s) owns no SQL — map an arm that carries queries, or convert the file as a fn library", e.Name, ep.Scenario)
		}
		// Request properties: the endpoint's params that the request
		// supplies, in bind order of its first query.
		seenReq := map[string]bool{}
		for _, id := range ep.QueryIDs {
			for _, qp := range p.Queries {
				if qp.ID != id {
					continue
				}
				for _, prm := range qp.Params {
					if prm.RequestProp != "" && !seenReq[prm.RequestProp] {
						seenReq[prm.RequestProp] = true
						ep.Requests = append(ep.Requests, prm.RequestProp)
					}
				}
			}
		}
		if seenEp[e.Name] {
			return nil, fmt.Errorf("csplan: endpoint %s mapped twice", e.Name)
		}
		seenEp[e.Name] = true
		covers = append(covers, epCover{scen: scen, cond: cond})
		p.Endpoints = append(p.Endpoints, ep)
	}

	// Unpinned const names must be unique among the planned queries (plan
	// order): a merged scenarioFilter arm reaches two arms whose SQL can
	// derive the same DefaultQueryName (two shapes named after one first
	// table), and duplicate consts/repo methods do not compile. Pins are
	// reserved first and never rewritten; a collision renames only the
	// later unpinned occurrence, so single-arm mappings keep their names.
	assignUniqueQueryNames(p.Queries, m.DBMethods)

	// Arm-coverage advisory (D4): every reachable dispatch arm no endpoint
	// covers gets a warning — the same style convertgo prints. The tool
	// never invents endpoints, but it never stays silent about a reachable
	// arm either. Dispatch-axis files (the corpus's separate-if and chain
	// shapes) are covered through the axis domain; files without an axis
	// fall back to the condition-inventory pass. Coverage is line-level:
	// scenario endpoints own their slice key; condition/conditionRef
	// endpoints cover by span over the arm's condition anchor.
	var flowTree *flow.Tree
	if t, err := treeFor(); err == nil {
		flowTree = t
	}
	if flowTree != nil {
		if axis := flowTree.DispatchAxisFor([]byte(opts.Source)); axis != nil {
			values := append([]string(nil), axis.Domain...)
			if axis.HasDefault {
				values = append(values, axis.DefaultKey())
			}
			for _, v := range values {
				scen := flow.ScenarioFor(flowTree, axis, v)
				ext := scen.BodyExtent()
				// The arm header line may share the previous arm's closing
				// brace (kept by that arm's slice span), so probe the body
				// line where one exists.
				check := ext[0]
				if ext[0] < ext[1] {
					check = ext[0] + 1
				}
				covered := false
				for _, cov := range covers {
					if cov.scen != nil {
						// Kept-lines coverage: a scenarioRef slice keeps
						// exactly its own arm, and a scenarioFilter slice
						// keeps every arm its matching assignments reach —
						// the line check covers both without key-equality
						// (which a merged filter key can never satisfy).
						// The span fallback must never judge a scenario
						// endpoint: a slice's extent encloses dropped
						// sibling arms.
						// KeepsLine, not KeptLines(...)[check]: the map
						// form built an entry per surviving line on
						// every (axis value × cover) probe.
						if flow.KeepsLine(cov.scen, check) {
							covered = true
							break
						}
						continue
					}
					if cov.cond != nil && cov.cond.StartLine <= check && check <= cov.cond.EndLine {
						covered = true
						break
					}
				}
				if covered {
					continue
				}
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"dispatch arm %s=%s (lines %d-%d) has no endpoint — map it (scenarioRef: %s) or it stays logic-only",
					axis.Key(), v, ext[0], ext[1], scen.Key))
			}
		}
	} else {
		for i := range opts.Main.Conditions {
			c := &opts.Main.Conditions[i]
			check := c.StartLine
			if i > 0 && opts.Main.Conditions[i-1].EndLine >= c.StartLine && c.StartLine < c.EndLine {
				check = c.StartLine + 1 // shared brace line — probe the body
			}
			covered := false
			for _, cov := range covers {
				if cov.cond != nil && cov.cond.StartLine <= check && check <= cov.cond.EndLine {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
			arm := "condition"
			if c.IsDefault {
				arm = "else arm"
			}
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s %d (lines %d-%d) has no endpoint — map it (condition: %d) or it stays logic-only",
				arm, c.Index, c.StartLine, c.EndLine, c.Index))
		}
	}
	return p, nil
}

// buildQueryPlan derives one query's C# shape: kind, const name, params
// (binds filtered to declared host variables — the extractor's
// format-literal leak like `:mi` never reaches the repo signature), and
// DTO row props for SELECTs.
func buildQueryPlan(q *ir.Query, m *Mapping, hostVars map[string]bool) (QueryPlan, error) {
	qp := QueryPlan{ID: q.ID, SQL: cleanSQL(q.SQL), Line: [2]int{q.StartLine, q.EndLine}}
	switch q.Type {
	case ir.QuerySelectSingle:
		qp.Kind = "select-single"
	case ir.QuerySelectMulti:
		qp.Kind = "select-multi"
	case ir.QueryInsert, ir.QueryUpdate, ir.QueryDelete:
		qp.Kind = "dml"
		qp.DML = true
	case ir.QueryMerge:
		qp.Kind = "merge"
		qp.DML = true
	default:
		return QueryPlan{}, fmt.Errorf("unsupported query type %q", q.Type)
	}
	if pin, ok := m.DBMethods[q.ID]; ok && pin.Name != "" {
		qp.Name = pin.Name
	} else {
		qp.Name = DefaultQueryName(q, qp.DML)
	}

	// Params: first-seen bind order, host-var filtered, deduped.
	seen := map[string]bool{}
	for _, b := range q.Binds {
		if !hostVars[b] || seen[b] {
			continue
		}
		seen[b] = true
		prm := Param{Name: b, Bind: b}
		if n, ok := m.ParamNames[b]; ok && n != "" {
			prm.Name = n
		} else if n, ok := m.RequestFields[b]; ok && n != "" {
			prm.Name = n
		} else {
			prm.Name = pascalOf(b)
		}
		prm.RequestProp = m.RequestFields[b]
		qp.Params = append(qp.Params, prm)
	}

	// Row props: the INTO shape with indicator variables already dropped
	// by the IR; type prefixes stripped, upper-snake named.
	if !qp.DML {
		for _, rs := range q.RowShape {
			bind := rs
			if i := strings.Index(rs, "["); i >= 0 {
				bind = rs[:i]
			}
			if bind == "" {
				continue
			}
			qp.RowProps = append(qp.RowProps, Prop{Name: propNameOf(bind), Bind: bind})
		}
	}
	return qp, nil
}

// cleanSQL prepares a query's SQL for the C# verbatim const: the Pro*C
// host plumbing (the `INTO :host:ind` clause) is stripped — the C# side
// reads columns by name — and the trailing statement semicolon goes
// (the const's C# syntax carries its own terminator). Binds in the
// surviving predicate text stay verbatim: the OracleParameter names must
// match them exactly.
//
// It delegates to sqltext.CanonicalSQL (uniform-ir plan §3.2: one SQL
// canonicalizer for every backend). CanonicalSQL additionally collapses
// `: name` to `:name`; that matches the OracleParameter derivation which
// already strips whitespace around bind names.
func cleanSQL(sql string) string {
	return sqltext.CanonicalSQL(sql)
}

// propNameOf derives a DTO property name from a row-shape host var.
// It delegates to common.UpperSnake (uniform-ir plan §3.3/Phase 4: one
// shared rule promoted from here, so the plan and CsNamer cannot drift).
func propNameOf(bind string) string { return common.UpperSnake(bind) }

// pascalOf derives a parameter name from a host var when the mapping
// leaves it unset (sql_cst_pan_no → CstPanNo).
// It delegates to common.Pascal (uniform-ir plan §3.3/Phase 4: one shared
// rule promoted from here, so the plan and CsNamer cannot drift).
func pascalOf(bind string) string { return common.Pascal(bind) }

// SuggestRequestProp is the draft-time view of pascalOf: the request
// property the discover -target cs drafts suggest for a bind host var —
// exactly the name the plan derives when the mapping leaves it unset.
func SuggestRequestProp(bind string) string { return pascalOf(bind) }

// buildGuards derives the arm's runtime-dispatch guards and their accepted
// C# spellings: the legacy identifier, the mapping's request field, the
// suggested prop (pascalOf), and the FML-derived request name for the Fget
// that fills the identifier. Never nil-element, deterministic.
func buildGuards(scen *flow.Scenario, cond *ir.Condition, m *Mapping) []Guard {
	mixed := flow.MixedGuards(scen)
	if len(mixed) == 0 {
		return nil
	}
	out := make([]Guard, 0, len(mixed))
	for _, g := range mixed {
		idents := map[string]bool{}
		for _, text := range []string{g.Cond, g.Alt} {
			if text == "" {
				continue
			}
			pe := pred.Parse(text)
			for _, id := range flow.ExprIdents(&pe) {
				idents[id] = true
			}
		}
		names := make([]string, 0, len(idents))
		for id := range idents {
			names = append(names, id)
		}
		sort.Strings(names)
		bind := map[string]string{}
		claim := func(spelling, ident string) {
			k := pred.IdentKey(spelling)
			if k == "" {
				return
			}
			if prev, ok := bind[k]; ok {
				if prev != ident {
					bind[k] = ""
				}
				return
			}
			bind[k] = ident
		}
		for _, id := range names {
			claim(id, id)
			if n := m.RequestFields[id]; n != "" {
				claim(n, id)
			}
			claim(SuggestRequestProp(id), id)
		}
		if cond != nil {
			for _, op := range cond.FmlOps {
				if op.Kind != ir.FmlGet || op.Dropped || op.Error {
					continue
				}
				base := strings.TrimSuffix(op.Target, ".arr")
				base = strings.TrimSuffix(base, ".len")
				for _, id := range names {
					if base == id || strings.HasSuffix(base, id) {
						claim(common.FieldFromFML(op.Field), id)
					}
				}
			}
		}
		out = append(out, Guard{Line: g.Line, Cond: g.Cond, Alt: g.Alt, Bind: bind})
	}
	return out
}

// DefaultQueryName derives the NamedQueries const when unpinned:
// <Verb><Table>Query (GetCstMblAccopnRqstQuery / Update…Query). Exported
// because the discover -target cs drafts pin exactly these names — the
// draft's dbMethods and the plan's unpinned fallback cannot drift.
// It delegates to CsNamer over the contract unit (uniform-ir plan
// §3.3/Phase 4): the verb comes from the contract kind, the table from the
// shared Pascal rule.
func DefaultQueryName(q *ir.Query, dml bool) string {
	return namer.CsNamer{}.Method(contract.QueryUnit{
		Kind:   contract.QueryKindOf(q.Type),
		Tables: q.Tables,
	})
}

// QueryNamesInOrder assigns every query its default const name walking the
// given order: a name already taken gains a numeric suffix. The plan passes
// its planned canonical queries in plan order, and the discover draft
// passes the same order (its endpoint slices first), so emitted pins and
// the unpinned fallback agree.
func QueryNamesInOrder(queries []*ir.Query) map[string]string {
	out := make(map[string]string, len(queries))
	used := map[string]bool{}
	for _, q := range queries {
		if q == nil || q.DuplicateOf != "" {
			continue
		}
		name := DefaultQueryName(q, q.Type.IsDML())
		base := name
		for i := 2; used[name]; i++ {
			name = base + strconv.Itoa(i)
		}
		used[name] = true
		out[q.ID] = name
	}
	return out
}

// assignUniqueQueryNames rewrites colliding unpinned defaults in place.
// Pinned names are reserved first and used verbatim; only the later
// unpinned duplicate moves.
func assignUniqueQueryNames(queries []QueryPlan, pins map[string]MethodPin) {
	used := map[string]bool{}
	for i := range queries {
		if pin, ok := pins[queries[i].ID]; ok && pin.Name != "" {
			used[pin.Name] = true
		}
	}
	for i := range queries {
		qp := &queries[i]
		if pin, ok := pins[qp.ID]; ok && pin.Name != "" {
			continue
		}
		base := qp.Name
		for n := 2; used[qp.Name]; n++ {
			qp.Name = base + strconv.Itoa(n)
		}
		used[qp.Name] = true
	}
}

// queriesInSpan collects the queries whose span sits inside the
// condition's branch span (the fallback when the IR's QueryIDs are
// unset).
func queriesInSpan(f *ir.File, cond *ir.Condition) []string {
	var ids []string
	for _, q := range f.Queries {
		if q.DuplicateOf != "" {
			continue
		}
		if q.StartLine >= cond.StartLine && q.EndLine <= cond.EndLine {
			ids = append(ids, q.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// axisRefSummary names the file's dispatch axis for the mapping-mismatch
// error (or "no axis" when the entry has no dispatch spine).
func axisRefSummary(t *flow.Tree, src string) string {
	axis := t.DispatchAxisFor([]byte(src))
	if axis == nil {
		return "no dispatch axis"
	}
	return axis.Key() + " " + strings.Join(axis.Domain, "/")
}
