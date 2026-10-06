package plan

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// Kind classifies a generation unit.
type Kind string

const (
	KindModels              Kind = "models"
	KindDBMethod            Kind = "db_method"
	KindDBInterface         Kind = "db_interface"
	KindControllerMethod    Kind = "controller_method"
	KindControllerInterface Kind = "controller_interface"
	KindHandlerMethod       Kind = "handler_method"
	KindHandlerInterface    Kind = "handler_interface"
	KindRouter              Kind = "router"
	KindMocks               Kind = "mocks"
	// KindFnStub is the controller-package file carrying the panicking
	// stubs for unresolved external fns (user directive 2026-09-10: stub
	// and carry on — the calling endpoints generate against it).
	KindFnStub Kind = "fn_stub"
	// KindFnHelper is one fn-library function (PRD fn-lib mode, 2026-09-13):
	// the LLM seam translates the legacy helper into a Go method on the
	// controller struct — store calls for its SQL, the legacy status-return
	// contract otherwise.
	KindFnHelper Kind = "fn_helper"
	// KindTPCall is one tpcall site of a mapped endpoint (PF-4.4): it
	// renders as a compilable placeholder stub carrying the send/recv FML
	// contract — never invented outbound scaffolding (R8).
	KindTPCall Kind = "tpcall"
)

// Unit is one step of the decomposition plan (plan-conversion §3): what to
// generate, from which source, into which target file, with which template,
// whether the LLM fills the body, and what must exist first. TP carries the
// tpcall site's full contract for KindTPCall units. Tx carries the
// per-scenario transaction decision (G-SCEN6, SCEN-D8) for DML db units:
// any owning scenario flags a live begin→commit span → the tx-variant
// template; no scenario does → the plain (autocommit) variant. DML owned
// only by condition/conditionRef endpoints keeps the legacy tx default.
type Unit struct {
	ID            string     `json:"id"`
	Kind          Kind       `json:"kind"`
	Name          string     `json:"name"`
	SourceFile    string     `json:"source_file,omitempty"`
	SourceLines   string     `json:"source_lines,omitempty"`
	QueryIDs      []string   `json:"query_ids,omitempty"`
	TargetPath    string     `json:"target_path"`
	TemplateID    string     `json:"template_id,omitempty"`
	LLM           bool       `json:"llm"`
	Tx            bool       `json:"tx,omitempty"`
	TokenEstimate int        `json:"token_estimate"`
	Deps          []string   `json:"deps,omitempty"`
	TP            *ir.TPCall `json:"tp,omitempty"`
}

// Skipped is an IR unit deliberately not converted — a query belonging only
// to unmapped conditions (§4.2.8: conditions not mapped stay unconverted).
type Skipped struct {
	QueryID string `json:"query_id"`
	Reason  string `json:"reason"`
}

// Stub is an unresolved external fn (user directive 2026-09-10: stub and
// carry on): the generator emits a panicking package-level placeholder and
// the calling endpoints generate against it — visible in the plan, the
// ledger, and the run summary, never silent. Implementing the stub is the
// operator's follow-up; the panicking body keeps guessed semantics out of
// the generated tree. (In LLM mode the convert seam first attempts a
// one-shot synthesis per stub — pure helpers may land as real Go; anything
// declined keeps this placeholder.)
type Stub struct {
	Fn        string   `json:"fn"`
	Reason    string   `json:"reason"`
	Endpoints []string `json:"endpoints"`
}

// txHelperRole classifies an external helper fn as transaction plumbing:
// the begin/commit/abort helper trio maps onto utils.ExecTransaction (the
// convert view pass rewrites its call sites into the wrapper template), so
// these fns are dropped from the plan, never stubbed.
func txHelperRole(name string) string {
	if !strings.HasPrefix(name, "fn_") {
		return ""
	}
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "begintran") || strings.Contains(l, "begin_tran"):
		return "begin"
	case strings.Contains(l, "committran") || strings.Contains(l, "commit_tran"):
		return "commit"
	case strings.Contains(l, "aborttran") || strings.Contains(l, "abort_tran") ||
		strings.Contains(l, "rollbacktran") || strings.Contains(l, "rollback_tran"):
		return "abort"
	}
	return ""
}

// FnHelper is one fn-library function's conversion anchor: the legacy fn
// name, the Go method name the unit renders, and the fn's source span —
// the side-table the convert seam reads for KindFnHelper units. Params and
// Return carry the deterministic Go signature (same-file helpers): both
// the helper seam and its callers receive it verbatim, so the two
// independent LLM outputs agree.
type FnHelper struct {
	Name      string `json:"name"`    // legacy fn name (fn_is_demo_active)
	GoName    string `json:"go_name"` // the unit's method name (FnIsDemoActive)
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// Params is the kept legacy parameter list in order, with Go types.
	// Empty when the declaration could not be read (fn-lib mode keeps its
	// legacy behavior then).
	Params []FnParam `json:"params,omitempty"`
	// Return is the Go return type ("int" for the legacy status contract,
	// "" for void).
	Return string `json:"return,omitempty"`
	// Fixed marks a service-run helper whose signature is prescribed to
	// both seams (the caller and the helper) because independent LLM
	// outputs must agree. Fn-lib helpers have no in-tree callers, so their
	// parameter list stays the seam's choice.
	Fixed bool `json:"fixed_signature,omitempty"`
}

// Plan is the deterministic decomposition of one conversion run.
type Plan struct {
	Service string    `json:"service"`
	Module  string    `json:"module"`
	Source  string    `json:"source"`
	Mapping *Mapping  `json:"mapping"`
	Units   []Unit    `json:"units"`
	Skipped []Skipped `json:"skipped,omitempty"`
	Stubs   []Stub    `json:"stubs,omitempty"`
	Dropped []string  `json:"dropped,omitempty"`
	// FnLib marks a fn-library plan (no entry service): units are the
	// library's own queries (db methods) and functions (helper bodies) —
	// no endpoints, no handler glue, no mapping draft needed.
	FnLib bool `json:"fn_lib,omitempty"`
	// FnHelpers carries one record per KindFnHelper unit, keyed by GoName.
	FnHelpers []FnHelper `json:"fn_helpers,omitempty"`
	// Warnings lists non-fatal coverage findings (advisory, never fatal —
	// §4.2.8 lets the user omit conditions): dispatch arms the mapping
	// leaves unmapped. Omission is a legitimate choice; silence about an
	// arm is not (G-SCEN1 spirit).
	Warnings []string `json:"warnings,omitempty"`
}

// Options carries the plan inputs: the main file's IR and source text, the
// fn-file IRs backing resolved external fns, the user mapping, and the
// budget used for token estimates.
type Options struct {
	Main    *ir.File
	Source  string
	FnFiles []*ir.File
	Mapping *Mapping
	Budget  budget.Budget
}

// Build constructs the plan. Deterministic: identical inputs produce a
// byte-identical plan.
func Build(opts Options) (*Plan, error) {
	if opts.Main == nil || opts.Mapping == nil {
		return nil, fmt.Errorf("plan: main IR and mapping are required")
	}
	m := opts.Mapping
	p := &Plan{Service: m.Service, Module: m.Module, Source: opts.Main.Path, Mapping: m}

	// Endpoint resolution (PRD-2026-09-10/12): conditionRef endpoints resolve
	// through the flow tree, scenarioRef endpoints through the flow fold;
	// both trees build lazily and at most once (flow.TreeFor is the shared
	// derivation; plan's policy is to hard-error). Each endpoint resolves
	// exactly once per build — the memo feeds the query, stub, controller,
	// and tpcall loops alike.
	var flowTree *flow.Tree
	treeFor := func(ref string) (*flow.Tree, error) {
		if flowTree != nil {
			return flowTree, nil
		}
		if strings.TrimSpace(opts.Source) == "" {
			return nil, fmt.Errorf("plan: endpoint references %s but no source is available to re-derive the flow tree", ref)
		}
		t, err := flow.TreeFor(opts.Source, opts.Main)
		if err != nil {
			return nil, fmt.Errorf("plan: flow tree for %s: %w", ref, err)
		}
		flowTree = t
		return t, nil
	}

	// epResolved is one endpoint's resolved shape: the condition-shaped value
	// the machinery consumes, the scenario slice behind a scenarioRef
	// endpoint (nil otherwise), and the scenario's flattened code slice —
	// the controller unit's source and token-estimate input.
	type epResolved struct {
		cond *ir.Condition
		scen *flow.Scenario
		text string
	}
	epMemo := map[string]*epResolved{}
	resolveEp := func(e Endpoint) (*epResolved, error) {
		if r, ok := epMemo[e.Name]; ok {
			return r, nil
		}
		r := &epResolved{}
		switch {
		case e.ScenarioRef != "":
			key, value, err := ParseScenarioRef(e.ScenarioRef)
			if err != nil {
				return nil, fmt.Errorf("plan: endpoint %s: %w", e.Name, err)
			}
			t, err := treeFor(e.ScenarioRef)
			if err != nil {
				return nil, err
			}
			axis := t.DispatchAxisFor([]byte(opts.Source))
			if axis == nil {
				return nil, fmt.Errorf("plan: endpoint %s references scenario %s but the entry function has no dispatch axis", e.Name, e.ScenarioRef)
			}
			if axis.Key() != key {
				return nil, fmt.Errorf("plan: endpoint %s references scenario key %q — the detected axis key is %q", e.Name, key, axis.Key())
			}
			domain := strings.Join(axis.Domain, ",")
			if axis.HasDefault {
				domain += " (default arm: " + axis.DefaultKey() + ")"
			}
			if !slices.Contains(axis.Domain, value) && !(axis.HasDefault && value == axis.DefaultKey()) {
				return nil, fmt.Errorf("plan: endpoint %s references scenario value %q — the detected %s domain is [%s]", e.Name, value, key, domain)
			}
			r.scen = flow.ScenarioFor(t, axis, value)
			r.cond = flow.ScenarioCondition(r.scen, t)
			text, _ := flow.ScenarioSource(r.scen, t, []byte(opts.Source))
			r.text = text
		case e.ScenarioFilter != "":
			// Scenario filter (scenario-filter plan §4-5): a boolean over
			// the ranked axes registry re-folds the tree under the
			// matching assignments. Parse errors were rejected at load;
			// axis/value existence and reachability are checked here,
			// against the file that actually dispatches.
			t, err := treeFor(e.ScenarioFilter)
			if err != nil {
				return nil, err
			}
			registry := t.AxesFor([]byte(opts.Source))
			if len(registry) == 0 {
				return nil, fmt.Errorf("plan: endpoint %s references scenarioFilter %q but the entry function has no dispatch axes", e.Name, e.ScenarioFilter)
			}
			filter, err := flow.ParseScenarioFilter(e.ScenarioFilter)
			if err != nil {
				return nil, fmt.Errorf("plan: endpoint %s: %w", e.Name, err)
			}
			sc, err := flow.ScenarioForFilter(t, registry, filter)
			if err != nil {
				return nil, fmt.Errorf("plan: endpoint %s: %w", e.Name, err)
			}
			r.scen = sc
			r.cond = flow.ScenarioCondition(sc, t)
			text, _ := flow.ScenarioSource(sc, t, []byte(opts.Source))
			r.text = text
		case e.ConditionRef != "":
			t, err := treeFor(e.ConditionRef)
			if err != nil {
				return nil, err
			}
			c, err := flow.ConditionFor(t, opts.Main.Conditions, e.ConditionRef)
			if err != nil {
				return nil, fmt.Errorf("plan: %w", err)
			}
			r.cond = c
		default:
			c := opts.Main.Condition(e.Condition)
			if c == nil {
				err := fmt.Errorf("plan: endpoint %s maps condition %d — inventory has %d conditions",
					e.Name, e.Condition, len(opts.Main.Conditions))
				if n := len(opts.Main.Unbalanced); n > 0 {
					// The condition inventory may be short because the parse
					// was truncated (severity F3) — say so, never a bare count.
					err = fmt.Errorf("%w — the file carries %d unbalanced region(s) (%s), so the parse may be truncated",
						err, n, unbalancedSummary(opts.Main.Unbalanced))
				}
				return nil, err
			}
			r.cond = c
		}
		epMemo[e.Name] = r
		return r, nil
	}
	cond := func(e Endpoint) (*ir.Condition, error) {
		r, err := resolveEp(e)
		if err != nil {
			return nil, err
		}
		return r.cond, nil
	}

	queryByID := make(map[string]*ir.Query, len(opts.Main.Queries))
	for _, q := range opts.Main.Queries {
		queryByID[q.ID] = q
	}

	// Queries per mapped endpoint, deduplicated to canonical units. Tx votes
	// ride along (G-SCEN6): every scenario endpoint owning a query votes its
	// census tx flag; the db unit takes the OR when any scenario endpoint
	// references the query, the legacy tx default otherwise.
	canonical := map[string]*ir.Query{}
	var dbQueries []*ir.Query
	seen := map[string]bool{}
	txVotes := map[string][]bool{}                                 // canonical query ID → owning scenarios' Tx flags
	endpointQueries := make(map[string][]string, len(m.Endpoints)) // endpoint name -> query IDs
	for _, e := range m.Endpoints {
		r, err := resolveEp(e)
		if err != nil {
			return nil, err
		}
		ids := r.cond.QueryIDs
		if len(ids) == 0 {
			ids = queriesIn(opts.Main, r.cond)
		}
		scenTx := map[string]bool{}
		if r.scen != nil {
			for _, qy := range r.scen.Queries {
				scenTx[qy.ID] = qy.Tx
			}
		}
		for _, id := range ids {
			q := queryByID[id]
			if q == nil {
				return nil, fmt.Errorf("plan: endpoint %s references unknown query %q (%s)", e.Name, id, e.RefOrIndex())
			}
			if q.DuplicateOf != "" {
				q = queryByID[q.DuplicateOf]
			}
			endpointQueries[e.Name] = append(endpointQueries[e.Name], q.ID)
			if tx, ok := scenTx[id]; ok {
				txVotes[q.ID] = append(txVotes[q.ID], tx)
			}
			if !seen[q.ID] {
				seen[q.ID] = true
				canonical[q.ID] = q
				dbQueries = append(dbQueries, q)
			}
		}
	}

	// Same-file helper fns (user directive 2026-09-17): a fn_* function the
	// main file itself defines and its own code calls is a helper — it
	// plans as a KindFnHelper unit (controller/fns.go) with a deterministic
	// Go signature, and its call sites call the generated method. The entry
	// function and chk_* session plumbing are not helpers; a helper whose
	// declaration cannot be read confidently keeps its legacy handling.
	var sameFile []localHelper
	if strings.TrimSpace(opts.Source) != "" {
		facts, ferr := scanner.ScanBytes([]byte(opts.Source), opts.Main.Path)
		if ferr != nil {
			return nil, fmt.Errorf("plan: same-file helper scan: %w", ferr)
		}
		called := map[string]bool{}
		for _, c := range facts.Calls {
			called[c.Name] = true
		}
		goNames := map[string]bool{}
		for _, def := range facts.Functions {
			if def.Name == opts.Main.Entry || strings.HasPrefix(def.Name, "chk_") || !called[def.Name] {
				continue
			}
			params, ret, ok := helperSignature(def, opts.Source)
			if !ok {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"%s is a same-file helper but its declaration could not be read — its calls keep the legacy drop handling", def.Name))
				continue
			}
			end := def.BodyEndLine
			if end <= def.BodyStartLine {
				end = facts.NumLines
			}
			goName := common.Export(common.CamelGo(def.Name))
			for goNames[goName] {
				goName += "X"
			}
			goNames[goName] = true
			h := localHelper{name: def.Name, goName: goName, start: def.BodyStartLine, end: end, params: params, ret: ret}
			for _, q := range opts.Main.Queries {
				if q.OwningFunction != def.Name {
					continue
				}
				canon := q
				if q.DuplicateOf != "" {
					if cq := queryByID[q.DuplicateOf]; cq != nil {
						canon = cq
					}
				}
				h.qids = append(h.qids, canon.ID)
				if !seen[canon.ID] {
					seen[canon.ID] = true
					canonical[canon.ID] = canon
					dbQueries = append(dbQueries, canon)
				}
			}
			sameFile = append(sameFile, h)
		}
	}

	// Queries belonging to no mapped endpoint are deliberate skips.
	mappedIDs := map[string]bool{}
	for _, q := range dbQueries {
		mappedIDs[q.ID] = true
	}
	for _, q := range opts.Main.Queries {
		if q.DuplicateOf != "" || mappedIDs[q.ID] {
			continue
		}
		p.Skipped = append(p.Skipped, Skipped{QueryID: q.ID, Reason: "belongs only to unmapped conditions"})
	}

	// Arm-coverage advisory (G-SCEN1 spirit): every condition-inventory
	// entry no endpoint covers gets a warning — the tool never invents
	// endpoints, but it never stays silent about a reachable dispatch arm
	// either. Coverage is line-level: an arm is covered when some
	// endpoint's resolved slice keeps a line of its body (scenario slices)
	// or its resolved span contains the header (condition/conditionRef
	// slices). The header line itself can't carry the scenario check: a
	// chain header may share its line with the previous arm's closing
	// brace, which the previous arm's span keeps by construction.
	//
	// The scenarioRef hint is derived once before the loop:
	// DispatchAxisFor re-harvests the axes (a full-file line split plus four
	// regexes over every line) and depends on nothing inside the loop.
	axisHint := ""
	if flowTree != nil {
		if axis := flowTree.DispatchAxisFor([]byte(opts.Source)); axis != nil && axis.HasDefault {
			axisHint = "scenarioRef: " + axis.Key() + "=" + axis.DefaultKey()
		}
	}
	for i := range opts.Main.Conditions {
		c := &opts.Main.Conditions[i]
		check := c.StartLine
		if i > 0 && opts.Main.Conditions[i-1].EndLine >= c.StartLine && c.StartLine < c.EndLine {
			check = c.StartLine + 1 // shared brace line — probe the body
		}
		covered := false
		for _, e := range m.Endpoints {
			r, ok := epMemo[e.Name]
			if !ok || r.cond == nil {
				continue
			}
			if r.scen != nil && flowTree != nil {
				// KeepsLine, not KeptLines(...)[check]: the map form
				// inserted an entry per surviving line of the slice on
				// every (arm × endpoint) probe.
				if flow.KeepsLine(r.scen, check) {
					covered = true
					break
				}
				continue
			}
			if r.cond.StartLine <= c.StartLine && c.StartLine <= r.cond.EndLine {
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
		hint := fmt.Sprintf("condition: %d", c.Index)
		if axisHint != "" {
			hint = axisHint
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%s %d (lines %d-%d) has no endpoint — map it (%s) or it stays logic-only",
			arm, c.Index, c.StartLine, c.EndLine, hint))
	}

	// External fns: chk_* are dropped constructs at conversion time
	// (§4.8.4.1); resolved SQL-bearing fns contribute their own db units;
	// everything else blocks the endpoints that call it (§4.2.9.4).
	fnIRByPath := make(map[string]*ir.File, len(opts.FnFiles))
	for _, f := range opts.FnFiles {
		fnIRByPath[f.Path] = f
	}
	var fnQueries []*ir.Query
	for _, fn := range opts.Main.ExternalFns {
		if strings.HasPrefix(fn.Name, "chk_") {
			p.Dropped = append(p.Dropped, fn.Name+" (session/error plumbing — middleware owns it, §4.8.4.1)")
			continue
		}
		if role := txHelperRole(fn.Name); role != "" {
			// Utility helpers route through the ExecTransaction template
			// (convert's txtemplate pass rewrites their call sites), so a
			// stub would only invite the model to call the legacy name.
			p.Dropped = append(p.Dropped, fn.Name+" (transaction "+role+" plumbing — utils.ExecTransaction owns begin/commit/rollback)")
			continue
		}
		switch {
		case fn.Resolved && fn.HasSQL && fnIRByPath[fn.DefinedIn] != nil:
			f := fnIRByPath[fn.DefinedIn]
			for _, q := range f.Queries {
				ns := fn.Name + ":" + q.ID
				if seen[ns] {
					continue
				}
				seen[ns] = true
				qq := *q
				qq.ID = ns
				fnQueries = append(fnQueries, &qq)
			}
		case fn.Resolved:
			p.Dropped = append(p.Dropped, fn.Name+" (no SQL in its body — pure logic, inlined by the controller)")
		default:
			b := Stub{Fn: fn.Name, Reason: "defining file not provided — converted as a panicking stub; implement before relying on the calling endpoints"}
			for _, e := range m.Endpoints {
				if c, err := cond(e); err == nil && callsiteIn(fn.Callsites, c) {
					b.Endpoints = append(b.Endpoints, e.Name)
				}
			}
			p.Stubs = append(p.Stubs, b)
		}
	}

	// Units — generation order: models → db methods → db interface →
	// controllers → controller interface → handlers → handler interface →
	// router → mocks.
	add := func(u Unit) { p.Units = append(p.Units, u) }
	dbUnitIDs := []string{}

	// dbMethodName resolves a db unit's name and keeps it unique: the
	// deterministic fallback (cursor/table-derived) can collide across
	// distinct queries (many SELECTs from one table), so a colliding name
	// gains its query id — traceable and loader-legal. Pins stay verbatim.
	dbNames := map[string]bool{}
	dbMethodName := func(pinName, id, fallback string) string {
		name := fallback
		if pinName != "" {
			name = pinName
		}
		return uniqueName(name, id, dbNames)
	}

	add(Unit{
		ID: "u01", Kind: KindModels, Name: m.Service + "Models",
		SourceFile: opts.Main.Path, TargetPath: m.ImportPath("models") + "/" + m.Service + ".go",
		TemplateID: "model_file", TokenEstimate: opts.Budget.Count(modelsRepr(opts.Main)),
	})
	for _, q := range dbQueries {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		pin := m.DBMethods[q.ID]
		name := dbMethodName(pin.Name, q.ID, methodName(m, q))
		add(Unit{
			ID: id, Kind: KindDBMethod, Name: name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(q.StartLine, q.EndLine),
			QueryIDs:   []string{q.ID},
			TargetPath: m.ImportPath("db") + "/" + m.Service + ".go",
			TemplateID: q.TemplateID, LLM: false,
			// Tx is the scenario vote (G-SCEN6): any owning scenario's tx
			// census flag wins; queries owned only by condition/conditionRef
			// endpoints keep the legacy tx default (decision 27).
			Tx:            dmlTxOf(txVotes, q.ID),
			TokenEstimate: opts.Budget.Count(q.SQL),
			Deps:          []string{"u01"},
		})
		dbUnitIDs = append(dbUnitIDs, id)
	}
	for _, q := range fnQueries {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		pin := m.DBMethods[q.ID]
		name := dbMethodName(pin.Name, q.ID, methodName(m, q))
		add(Unit{
			ID: id, Kind: KindDBMethod, Name: name,
			SourceFile: definedInOf(opts, q.ID), SourceLines: lineSpan(q.StartLine, q.EndLine),
			QueryIDs:   []string{q.ID},
			TargetPath: m.ImportPath("db") + "/" + m.Service + ".go",
			TemplateID: q.TemplateID, LLM: false,
			TokenEstimate: opts.Budget.Count(q.SQL),
			Deps:          []string{"u01"},
		})
		dbUnitIDs = append(dbUnitIDs, id)
	}
	ifaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: ifaceID, Kind: KindDBInterface, Name: common.Export(m.Service) + "Store",
		TargetPath: m.ImportPath("db") + "/interface.go",
		TemplateID: "db_interface_file",
		Deps:       dbUnitIDs,
	})

	ctrlIDs := []string{}
	for _, e := range m.Endpoints {
		r, err := resolveEp(e)
		if err != nil {
			return nil, err
		}
		c := r.cond
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		// Scenario endpoints estimate from the flattened slice (the kept
		// lines — dropped branches never ride the prompt); condition
		// endpoints keep the raw branch-slice estimate.
		estimate := opts.Budget.Count(branchSource(opts.Source, c.StartLine, c.EndLine))
		if r.scen != nil {
			estimate = opts.Budget.Count(r.text)
		}
		add(Unit{
			ID: id, Kind: KindControllerMethod, Name: e.Name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(c.StartLine, c.EndLine),
			QueryIDs:   endpointQueries[e.Name],
			TargetPath: m.ImportPath("controller") + "/" + m.Service + ".go",
			TemplateID: "controller_method", LLM: true,
			TokenEstimate: estimate,
			Deps:          []string{ifaceID},
		})
		ctrlIDs = append(ctrlIDs, id)
	}
	ctrlIfaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: ctrlIfaceID, Kind: KindControllerInterface, Name: common.Export(m.Service) + "Controller",
		TargetPath: m.ImportPath("controller") + "/interface.go",
		TemplateID: "controller_interface_file",
		Deps:       ctrlIDs,
	})
	// Same-file helper units: one LLM gap per helper fn (fns.go), the
	// controller-method seams call the generated methods.
	for _, h := range sameFile {
		estimate := opts.Budget.Count(branchSource(opts.Source, h.start, h.end))
		add(Unit{
			ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindFnHelper, Name: h.goName,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(h.start, h.end),
			QueryIDs:   h.qids,
			TargetPath: m.ImportPath("controller") + "/fns.go",
			TemplateID: "fn_helper", LLM: true,
			TokenEstimate: estimate,
			Deps:          []string{ifaceID},
		})
		p.FnHelpers = append(p.FnHelpers, FnHelper{
			Name: h.name, GoName: h.goName, StartLine: h.start, EndLine: h.end,
			Params: h.params, Return: h.ret, Fixed: true,
		})
	}
	if len(p.Stubs) > 0 {
		add(Unit{
			ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindFnStub, Name: "fnstubs.go",
			TargetPath: m.ImportPath("controller") + "/fnstubs.go",
			TemplateID: "fn_stub_file", LLM: false,
			Deps: []string{},
		})
	}

	// TPCall units — one per call site under the owning endpoint (PF-4.4).
	// Sites outside every mapped condition are recorded skips (§4.2.8: only
	// mapped conditions convert).
	tpcallContract := func(tp *ir.TPCall) string {
		var sb strings.Builder
		for _, op := range tp.SendFML {
			fmt.Fprintf(&sb, "send %s %s\n", op.Field, op.Target)
		}
		for _, op := range tp.RecvFML {
			fmt.Fprintf(&sb, "recv %s %s\n", op.Field, op.Target)
		}
		return sb.String()
	}
	tpNameSeen := map[string]int{}
	for i := range opts.Main.TPCalls {
		tp := &opts.Main.TPCalls[i]
		owner := ""
		for _, e := range m.Endpoints {
			if c, err := cond(e); err == nil && c.ContainsLine(tp.StartLine) {
				owner = e.Name
				break
			}
		}
		if owner == "" {
			p.Skipped = append(p.Skipped, Skipped{QueryID: "tpcall:" + tp.Service, Reason: "belongs only to unmapped conditions"})
			continue
		}
		name := "TPCall" + common.CamelGo(tp.Service)
		tpNameSeen[name]++
		if n := tpNameSeen[name]; n > 1 {
			name = fmt.Sprintf("%s%d", name, n)
		}
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		add(Unit{
			ID: id, Kind: KindTPCall, Name: name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(tp.StartLine, tp.EndLine),
			TargetPath: m.ImportPath("controller") + "/tpcall_placeholders.go",
			TemplateID: "tpcall_placeholder", LLM: false,
			TokenEstimate: opts.Budget.Count(tpcallContract(tp)),
			Deps:          []string{ctrlIfaceID},
			TP:            tp,
		})
	}

	handlerIDs := []string{}
	for _, e := range m.Endpoints {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		add(Unit{
			ID: id, Kind: KindHandlerMethod, Name: e.Name,
			TargetPath: m.ImportPath("handler") + "/" + m.Service + ".go",
			TemplateID: "handler_method", LLM: false,
			Deps: []string{ctrlIfaceID},
		})
		handlerIDs = append(handlerIDs, id)
	}
	handlerIfaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: handlerIfaceID, Kind: KindHandlerInterface, Name: common.Export(m.Service) + "Handler",
		TargetPath: m.ImportPath("handler") + "/interface.go",
		TemplateID: "handler_interface_file",
		Deps:       handlerIDs,
	})
	add(Unit{
		ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindRouter, Name: m.RouteGroup,
		TargetPath: m.ImportPath("handler") + "/router_snippet.txt",
		TemplateID: "router_snippet",
		Deps:       []string{handlerIfaceID},
	})
	add(Unit{
		ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindMocks, Name: "mockgen " + common.Export(m.Service) + "Store/" + common.Export(m.Service) + "Controller",
		TargetPath: m.ImportPath("db") + "/mock_store.go",
		TemplateID: "(mockgen)", LLM: false,
		Deps: []string{ifaceID, ctrlIfaceID},
	})

	return p, nil
}

// methodName resolves a DB method name: the mapping pin when present, else
// the deterministic fallback (cursor/table-derived, §3 of plan-conversion).
func methodName(m *Mapping, q *ir.Query) string {
	if pin, ok := m.DBMethods[q.ID]; ok && pin.Name != "" {
		return pin.Name
	}
	if q.CursorName != "" {
		return "Get" + common.CamelGo(strings.TrimPrefix(q.CursorName, "cur_"))
	}
	table := "Row"
	if len(q.Tables) > 0 && q.Tables[0] != "" {
		table = tableName(q.Tables[0])
	}
	switch q.Type {
	case ir.QueryInsert:
		return "Insert" + common.CamelGo(table)
	case ir.QueryUpdate:
		return "Update" + common.CamelGo(table)
	case ir.QueryDelete:
		return "Delete" + common.CamelGo(table)
	case ir.QueryMerge:
		return "Merge" + common.CamelGo(table)
	default:
		return "Get" + common.CamelGo(table)
	}
}

// uniqueName keeps a db method name unique the way Build does: a colliding
// name gains its query id (traceable, loader-legal), further collisions
// grow X suffixes. Pins and fallbacks go through the same rule.
func uniqueName(name, id string, taken map[string]bool) string {
	if !taken[name] {
		taken[name] = true
		return name
	}
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		}
	}
	cand := name + sb.String()
	for taken[cand] {
		cand += "X"
	}
	taken[cand] = true
	return cand
}

// DefaultMethodNames resolves the db method names a query list gets when
// the mapping pins nothing: the deterministic fallback (cursor/table
// derived), collisions resolved exactly as Build resolves them. The
// discover drafts pre-fill their dbMethods block from here, so the names
// a reviewer sees in the draft are the names conversion produces — the
// draft and the plan cannot drift.
func DefaultMethodNames(queries []*ir.Query) map[string]string {
	taken := map[string]bool{}
	out := make(map[string]string, len(queries))
	for _, q := range queries {
		if q.DuplicateOf != "" {
			continue // pins key canonical ids only
		}
		out[q.ID] = uniqueName(methodName(&Mapping{}, q), q.ID, taken)
	}
	return out
}

// tableName renders a query's first table as an identifier-safe token: the
// corpus's inline views extract as "(SELECT …" shapes — non-identifier
// bytes drop, so the deterministic Get<Table> method (and its row struct)
// stays a valid Go identifier. An all-symbol table falls back to "Row".
func tableName(t string) string {
	var sb strings.Builder
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 {
		return "Row"
	}
	return sb.String()
}

// dmlTxOf applies the scenario tx votes (G-SCEN6): a query owned by at
// least one scenario endpoint takes the OR of the owning scenarios' census
// tx flags; an unvoted query keeps the legacy tx default (true — decision
// 27), so condition/conditionRef mappings are byte-unchanged.
func dmlTxOf(votes map[string][]bool, id string) bool {
	v, ok := votes[id]
	if !ok {
		return true
	}
	tx := false
	for _, b := range v {
		tx = tx || b
	}
	return tx
}

// camel turns snake/dotted segments into exported CamelCase (DEMO_ACC_X → DemoAccX).
// queriesIn derives a condition's query IDs by line overlap when the
// inventory's explicit references are absent.
func queriesIn(f *ir.File, c *ir.Condition) []string {
	var ids []string
	for _, q := range f.Queries {
		if c.ContainsLine(q.StartLine) {
			ids = append(ids, q.ID)
		}
	}
	return ids
}

func callsiteIn(sites []int, c *ir.Condition) bool {
	for _, s := range sites {
		if c.ContainsLine(s) {
			return true
		}
	}
	return false
}

func lineSpan(from, to int) string {
	if from == to {
		return strconv.Itoa(from)
	}
	return fmt.Sprintf("%d-%d", from, to)
}

// unbalancedSummary renders the IR's unbalanced regions as a compact
// "kind@line" list for error messages (severity F3).
func unbalancedSummary(u []ir.Unbalanced) string {
	parts := make([]string, 0, len(u))
	for _, r := range u {
		parts = append(parts, fmt.Sprintf("%s@%d", r.Kind, r.Line))
	}
	return strings.Join(parts, ", ")
}

// branchSource slices the 1-based inclusive line range out of src.
func branchSource(src string, from, to int) string {
	if src == "" {
		return ""
	}
	lines := strings.Split(src, "\n")
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

// modelsRepr is the deterministic token-estimate input for the models unit:
// the FML field inventory plus every row shape.
func modelsRepr(f *ir.File) string {
	var sb strings.Builder
	for _, op := range f.FmlOps {
		if op.Dropped {
			continue
		}
		fmt.Fprintf(&sb, "%s %s %s\n", op.Kind, op.Field, op.Target)
	}
	for _, c := range f.Conditions {
		for _, op := range c.FmlOps {
			if op.Dropped {
				continue
			}
			fmt.Fprintf(&sb, "%s %s %s\n", op.Kind, op.Field, op.Target)
		}
	}
	for _, q := range f.UniqueQueries() {
		sb.WriteString(strings.Join(q.RowShape, " "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// definedInOf finds the source file backing a namespaced fn-query ID.
func definedInOf(opts Options, nsID string) string {
	fn := strings.SplitN(nsID, ":", 2)[0]
	for _, ext := range opts.Main.ExternalFns {
		if ext.Name == fn && ext.DefinedIn != "" {
			return ext.DefinedIn
		}
	}
	return ""
}

// SortUnits keeps plan output stable regardless of builder internals.
func SortUnits(units []Unit) {
	sort.SliceStable(units, func(i, j int) bool { return units[i].ID < units[j].ID })
}
