package flow

import (
	"sort"

	scanner "tux-migration-tool/internal/tsscan"
)

// Same-file helper functions a scenario calls are invisible to the fold: the
// fold slices the entry's own body, and a callee's definition lives in a
// different region of the file (often after the entry). A reviewer reading a
// flattened scenario therefore sees `fn_chk_foo(...)` with no body to check
// it against. MarkCommonLocalFuncs resolves those references and keeps the
// ones ≥2 scenarios share — the same "common" test DiffScenarios applies to
// a shared line block, so the two artifacts agree on what "common" means.

// localFuncDefs indexes the file's function definitions by name, excluding
// the entry itself. nil when the tree carries no facts (a hand-built test
// tree) or the file is a fragment (its braces are synthesized outside the
// source, so line spans do not address real text).
func localFuncDefs(tree *Tree, entry string) map[string]scanner.FunctionDef {
	if tree == nil || tree.facts == nil || tree.facts.Fragment {
		return nil
	}
	out := map[string]scanner.FunctionDef{}
	for _, fn := range tree.facts.Functions {
		if fn.Name == entry || fn.Name == "" {
			continue
		}
		// A definition with no closed body cannot be emitted as text.
		if fn.BodyStartLine <= 0 || fn.BodyEndLine <= 0 || fn.BodyEndLine < fn.BodyStartLine {
			continue
		}
		out[fn.Name] = fn
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MarkCommonLocalFuncs annotates each scenario with the same-file functions
// it calls that at least two scenarios call. Call once over the full scenario
// set before rendering — it is the whole point: "common" is a cross-scenario
// judgement no single scenario can make.
//
// Scenarios that call a helper from a single arm get nothing, and neither
// does an entry with one scenario (nothing can be common). Re-entrant: it
// clears LocalFuncs first, so a second call cannot accumulate.
func MarkCommonLocalFuncs(tree *Tree, scens []*Scenario) {
	for _, sc := range scens {
		if sc != nil {
			sc.LocalFuncs = nil
		}
	}
	if tree == nil || len(scens) < 2 {
		return
	}
	defs := localFuncDefs(tree, tree.Function)
	if len(defs) == 0 {
		return
	}

	// name → the scenarios that call it, in scenario order.
	keys := map[string][]string{}
	for _, sc := range scens {
		if sc == nil {
			continue
		}
		kept := scenarioKeptLines(sc)
		for _, c := range tree.facts.Calls {
			if c.Func != tree.Function || !kept[c.Line] {
				continue
			}
			if _, ok := defs[c.Name]; !ok {
				continue // external callee: no body in this file to show
			}
			list := keys[c.Name]
			if n := len(list); n == 0 || list[n-1] != sc.Key {
				keys[c.Name] = append(list, sc.Key)
			}
		}
	}
	if len(keys) == 0 {
		return
	}
	// Deterministic order: definition order in the file, so the section
	// reads top-to-bottom like the source does.
	ordered := make([]scanner.FunctionDef, 0, len(defs))
	for _, fn := range defs {
		ordered = append(ordered, fn)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StartLine != ordered[j].StartLine {
			return ordered[i].StartLine < ordered[j].StartLine
		}
		return ordered[i].Name < ordered[j].Name
	})
	byKey := map[string]*Scenario{}
	for _, sc := range scens {
		if sc != nil {
			byKey[sc.Key] = sc
		}
	}
	for _, fn := range ordered {
		callers := keys[fn.Name]
		if len(callers) < 2 {
			continue // referenced by one scenario only: not common
		}
		lf := LocalFunc{
			Name:    fn.Name,
			SigLine: fn.StartLine,
			Start:   fn.StartLine,
			End:     fn.BodyEndLine,
			Keys:    callers,
		}
		for _, k := range callers {
			if sc := byKey[k]; sc != nil {
				sc.LocalFuncs = append(sc.LocalFuncs, lf)
			}
		}
	}
}

// scenarioKeptLines is one scenario's genuinely kept lines — the same set
// the renderer emits: preamble spans plus bodyLines (surviving spans minus
// dropped lines). Deliberately not keptLineSet, which keeps a satisfied
// parent's whole span and would attribute a contradicted arm's helper call
// to this scenario.
func scenarioKeptLines(sc *Scenario) map[int]bool {
	out := map[int]bool{}
	if sc == nil {
		return out
	}
	for i := 0; i+1 < len(sc.Preamble); i += 2 {
		for l := sc.Preamble[i]; l <= sc.Preamble[i+1]; l++ {
			out[l] = true
		}
	}
	for _, l := range bodyLines(sc) {
		out[l] = true
	}
	return out
}
