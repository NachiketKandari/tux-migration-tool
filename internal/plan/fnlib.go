// fnlib plans a fn-library conversion (PRD fn-lib mode, 2026-09-13): a
// file of legacy helper functions with no Tuxedo entry service. There are
// no endpoints to map — the tool never invents endpoints, and none are
// needed here — so the plan converts the library directly: one db method
// per local SQL query (the deterministic machinery services use), one
// LLM-seam helper unit per local function (chk_* session plumbing dropped,
// parity with the service path), and the same stub-and-carry-on policy for
// the library's own unresolved external calls.
package plan

import (
	"fmt"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// FnLibServiceName derives a fn library's service identity from its file
// stem ("fn_demo_lib.pc" → "fn_demo_lib"): identifier-safe, lowercase — the
// same convention the mapping drafts use for service names.
func FnLibServiceName(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var sb strings.Builder
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	name := strings.ToLower(sb.String())
	if name == "" || name == "_" {
		name = "fnlib"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	return name
}

// BuildFnLib constructs a fn-library plan. Deterministic: identical inputs
// produce a byte-identical plan.
func BuildFnLib(opts Options) (*Plan, error) {
	if opts.Main == nil {
		return nil, fmt.Errorf("plan: main IR is required")
	}
	f := opts.Main
	if f.Entry != "" || f.Fragment || len(f.Functions) == 0 {
		return nil, fmt.Errorf("plan: %s is not a fn library (entry %q, fragment %v, functions %d) — convert it as a service",
			filepath.Base(f.Path), f.Entry, f.Fragment, len(f.Functions))
	}
	if strings.TrimSpace(opts.Source) == "" {
		return nil, fmt.Errorf("plan: fn library %s needs its source text", filepath.Base(f.Path))
	}
	m := &Mapping{Service: FnLibServiceName(f.Path)}
	if opts.Mapping != nil {
		if opts.Mapping.Service != "" {
			m.Service = opts.Mapping.Service
		}
		if opts.Mapping.DBMethods != nil {
			m.DBMethods = opts.Mapping.DBMethods
		}
	}
	m.Module = m.Service
	p := &Plan{Service: m.Service, Module: m.Module, Source: f.Path, Mapping: m, FnLib: true}

	// Fn spans: the scanner is the span authority (the IR keeps names
	// only). An unbalanced body covers to EOF — the fault-tolerance
	// posture (F3), loud in the IR's unbalanced record.
	facts, err := scanner.ScanBytes([]byte(opts.Source), f.Path)
	if err != nil {
		return nil, fmt.Errorf("plan: fn library scan: %w", err)
	}
	spans := map[string][2]int{}
	defs := map[string]scanner.FunctionDef{}
	order := make([]string, 0, len(facts.Functions))
	for _, fn := range facts.Functions {
		end := fn.BodyEndLine
		if end <= fn.BodyStartLine {
			end = facts.NumLines
		}
		spans[fn.Name] = [2]int{fn.BodyStartLine, end}
		defs[fn.Name] = fn
		order = append(order, fn.Name)
	}

	// Local queries, canonical: every query the library itself owns.
	var local []*ir.Query
	for _, q := range f.Queries {
		if q.DuplicateOf != "" {
			continue
		}
		local = append(local, q)
	}

	add := func(u Unit) { p.Units = append(p.Units, u) }
	dbNames := map[string]bool{}
	dbMethodName := func(pinName, id, fallback string) string {
		name := fallback
		if pinName != "" {
			name = pinName
		}
		if dbNames[name] {
			var sb strings.Builder
			for _, r := range id {
				switch {
				case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
					sb.WriteRune(r)
				}
			}
			cand := name + sb.String()
			for dbNames[cand] {
				cand += "X"
			}
			name = cand
		}
		dbNames[name] = true
		return name
	}

	dbUnitIDs := []string{}
	if len(local) > 0 {
		add(Unit{
			ID: "u01", Kind: KindModels, Name: m.Service + "Models",
			SourceFile: f.Path, TargetPath: m.ImportPath("models") + "/" + m.Service + ".go",
			TemplateID: "model_file", TokenEstimate: opts.Budget.Count(modelsRepr(f)),
		})
		for _, q := range local {
			id := fmt.Sprintf("u%02d", len(p.Units)+1)
			pin := m.DBMethods[q.ID]
			name := dbMethodName(pin.Name, q.ID, methodName(m, q))
			add(Unit{
				ID: id, Kind: KindDBMethod, Name: name,
				SourceFile: f.Path, SourceLines: lineSpan(q.StartLine, q.EndLine),
				QueryIDs:   []string{q.ID},
				TargetPath: m.ImportPath("db") + "/" + m.Service + ".go",
				TemplateID: q.TemplateID, LLM: false,
				// No scenarios exist in a fn library — the plain
				// (autocommit) variant renders (G-SCEN6 default).
				Tx:            false,
				TokenEstimate: opts.Budget.Count(q.SQL),
				Deps:          []string{"u01"},
			})
			dbUnitIDs = append(dbUnitIDs, id)
		}
		add(Unit{
			ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindDBInterface, Name: common.Export(m.Service) + "Store",
			TargetPath: m.ImportPath("db") + "/interface.go",
			TemplateID: "db_interface_file",
			Deps:       dbUnitIDs,
		})
	}
	ifaceID := ""
	if len(dbUnitIDs) > 0 {
		ifaceID = fmt.Sprintf("u%02d", len(p.Units))
	}

	// Helper units: one per locally-defined function, chk_* dropped with
	// the session-plumbing note (§4.8.4.1 parity). Each unit owns the
	// queries its fn body runs.
	for _, name := range order {
		start, end := spans[name][0], spans[name][1]
		if strings.HasPrefix(name, "chk_") {
			p.Dropped = append(p.Dropped, name+" (session/error plumbing — middleware owns it, §4.8.4.1)")
			continue
		}
		var qids []string
		for _, q := range local {
			if q.OwningFunction == name {
				qids = append(qids, q.ID)
			}
		}
		goName := common.Export(common.CamelGo(name))
		params, ret, _ := helperSignature(defs[name], opts.Source)
		p.FnHelpers = append(p.FnHelpers, FnHelper{Name: name, GoName: goName, StartLine: start, EndLine: end, Params: params, Return: ret})
		deps := []string{}
		if ifaceID != "" {
			deps = []string{ifaceID}
		}
		estimate := 0
		if end >= start {
			estimate = opts.Budget.Count(branchSource(opts.Source, start, end))
		}
		add(Unit{
			ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindFnHelper, Name: goName,
			SourceFile: f.Path, SourceLines: lineSpan(start, end),
			QueryIDs:   qids,
			TargetPath: m.ImportPath("controller") + "/fns.go",
			TemplateID: "fn_helper", LLM: true,
			TokenEstimate: estimate,
			Deps:          deps,
		})
	}

	// External fns the library calls: chk_* dropped, everything else
	// stub-and-carry-on (single-file mode ingests no defining files, so
	// nothing can resolve here — the calling helpers generate against the
	// panicking stub, visibly).
	for _, fn := range f.ExternalFns {
		if strings.HasPrefix(fn.Name, "chk_") {
			p.Dropped = append(p.Dropped, fn.Name+" (session/error plumbing — middleware owns it, §4.8.4.1)")
			continue
		}
		if role := txHelperRole(fn.Name); role != "" {
			p.Dropped = append(p.Dropped, fn.Name+" (transaction "+role+" plumbing — utils.ExecTransaction owns begin/commit/rollback)")
			continue
		}
		b := Stub{Fn: fn.Name, Reason: "defining file not provided — converted as a panicking stub; implement before relying on the calling helpers"}
		for _, h := range p.FnHelpers {
			span := [2]int{h.StartLine, h.EndLine}
			if callsiteInFn(fn.Callsites, span) {
				b.Endpoints = append(b.Endpoints, h.GoName)
			}
		}
		p.Stubs = append(p.Stubs, b)
	}
	if len(p.Stubs) > 0 {
		add(Unit{
			ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindFnStub, Name: "fnstubs.go",
			TargetPath: m.ImportPath("controller") + "/fnstubs.go",
			TemplateID: "fn_stub_file", LLM: false,
			Deps: []string{},
		})
	}

	return p, nil
}

// callsiteInFn reports whether any call site line sits inside the fn's
// span (the helper-attribution check for Stub.Endpoints).
func callsiteInFn(sites []int, span [2]int) bool {
	for _, s := range sites {
		if s >= span[0] && s <= span[1] {
			return true
		}
	}
	return false
}
