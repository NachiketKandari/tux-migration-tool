// Package flow — return-anchored discovery experiment (exp/return-anchored-detection).
//
// Hypothesis under test: success-return sites (tpreturn(TPSUCCESS,...))
// are outcome anchors — one site ≈ one observable service outcome — and
// the ancestor branch chain above each site names "which conditions caused
// this service". The existing Discover groups by condition (cause); this
// file groups by return (effect) and labels with the cause.
//
// EXPERIMENT STATUS: additive only, behind no flag in code but not wired
// into any production path (no caller in cmd/, plan/, or convert/). Safe
// to evaluate and delete. See return_anchored_test.go for the pinned
// synthetic cases and docs for the real-file scorecard.
package flow

import (
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// ReturnOutcome is one tpreturn(TPSUCCESS) site with its causing guard
// chain and response shape. Convergent is true when the return sits
// outside every branch body (function-tail return): every dispatch arm
// flows into it, so the outcome alone under-splits and Arms carries the
// per-arm split the condition layer must supply (the convergent-tail
// shape: several content arms, one shared tail return).
type ReturnOutcome struct {
	ReturnLine   int      `json:"return_line"`
	Kind         string   `json:"kind,omitempty"` // "return" (default) or "forward"
	ForwardTo    string   `json:"forward_to,omitempty"`
	Buffer       string   `json:"buffer,omitempty"`
	InsideBranch bool     `json:"inside_branch"`
	GuardChain   []string `json:"guard_chain,omitempty"`
	GuardLines   []int    `json:"guard_lines,omitempty"`
	TopCondition int      `json:"top_condition,omitempty"`
	Gets         []string `json:"gets,omitempty"`
	Adds         []string `json:"adds,omitempty"`
	ErrorAdds    []string `json:"error_adds,omitempty"`
	QueryIDs     []string `json:"query_ids,omitempty"`
	// Convergent marks a tail return fed by a whole dispatch chain.
	Convergent bool        `json:"convergent,omitempty"`
	Arms       []ReturnArm `json:"arms,omitempty"`
}

// ReturnArm is one dispatch arm feeding a convergent (tail) return.
type ReturnArm struct {
	Cond      string   `json:"cond,omitempty"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Gets      []string `json:"gets,omitempty"`
	Adds      []string `json:"adds,omitempty"`
	ErrorAdds []string `json:"error_adds,omitempty"`
	QueryIDs  []string `json:"query_ids,omitempty"`
}

// noiseCondSubstr marks guard predicates that are plumbing, not business
// dispatch: SQL status, FML unpack/error checks, debug switches,
// buffer-alloc NULL checks. These dominate raw if-counts (e.g. ~1800 ifs
// in the large transaction file, most of them SQLCODE/DEBUG/Ferror guards)
// but never delimit services.
//
// Foccur32/Foccur existence checks (`Foccur32(buf, FML_X) > 0`) are
// deliberately absent here: which optional request flag arrived is business
// dispatch, not plumbing — they stay in guard chains, the census, and the
// draft as Go presence checks.
var noiseCondSubstr = []string{
	"SQLCODE", "sqlca", "Ferror", "FNOTPRES", "Fget32",
	"DEBUG", "debug", "tpalloc", "tprealloc", "tpfree",
}

// IsNoiseCond reports whether a branch condition is plumbing noise rather
// than business dispatch. Exported for the scorecard and for future
// filtering inside Discover.
func IsNoiseCond(cond string) bool {
	for _, s := range noiseCondSubstr {
		if strings.Contains(cond, s) {
			return true
		}
	}
	return false
}

// SuccessReturnSites lists tpreturn(TPSUCCESS,...) call lines in fn.
// tpreturn rides in facts.Calls (not facts.Returns, which is C return
// only); success is arg0 == TPSUCCESS. TPFAIL legs are error exits, not
// outcomes.
func SuccessReturnSites(facts *scanner.SourceFacts, fn string) []scanner.FunctionCall {
	return terminalSites(facts, fn, false)
}

// ForwardSites lists tpforward(...) call lines in fn. tpforward ends the
// service by delegating the request to another service — a success
// terminal with no local reply, invisible to TPSUCCESS-only scans.
func ForwardSites(facts *scanner.SourceFacts, fn string) []scanner.FunctionCall {
	return terminalSites(facts, fn, true)
}

func terminalSites(facts *scanner.SourceFacts, fn string, forward bool) []scanner.FunctionCall {
	var out []scanner.FunctionCall
	for _, c := range facts.Calls {
		if c.Func != fn {
			continue
		}
		if forward && c.Name == "tpforward" {
			out = append(out, c)
		}
		if !forward && c.Name == "tpreturn" && strings.Contains(c.Args, "TPSUCCESS") {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// returnBuffer extracts the reply-buffer ident from
// tpreturn(TPSUCCESS, rval, buffer, len, flags): third comma-separated
// arg, casts and address operators stripped.
func returnBuffer(args string) string {
	parts := splitCallArgs(args)
	if len(parts) < 3 {
		return ""
	}
	b := strings.TrimSpace(parts[2])
	// strip C casts "(char *)" repeatedly, then &, *, parens
	for {
		t := strings.TrimSpace(b)
		if strings.HasPrefix(t, "(") {
			if i := strings.Index(t, ")"); i >= 0 {
				b = t[i+1:]
				continue
			}
		}
		break
	}
	b = strings.Trim(b, "&*() \t")
	if i := strings.Fields(b); len(i) > 0 {
		b = i[0]
	}
	return b
}

// forwardBuffer extracts tpforward's delegated buffer (arg1):
// tpforward(svc, data, len, flags).
func forwardBuffer(args string) string {
	parts := splitCallArgs(args)
	if len(parts) < 2 {
		return ""
	}
	b := strings.TrimSpace(parts[1])
	for {
		t := strings.TrimSpace(b)
		if strings.HasPrefix(t, "(") {
			if i := strings.Index(t, ")"); i >= 0 {
				b = t[i+1:]
				continue
			}
		}
		break
	}
	b = strings.Trim(b, "&*() \t")
	if i := strings.Fields(b); len(i) > 0 {
		b = i[0]
	}
	return b
}

// splitCallArgs splits raw call args on top-level commas (no nesting
// tracking beyond paren depth — enough for tpreturn's flat arg list).
func splitCallArgs(args string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, args[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, args[start:])
	return parts
}

// branchPath returns the outer→inner chain of branch nodes containing
// line, via DFS over the tree.
func branchPath(tree *Tree, line int) []*Node {
	var best []*Node
	var cur []*Node
	var dfs func(ns []*Node)
	dfs = func(ns []*Node) {
		for _, n := range ns {
			if n.Kind == KindBranch && line >= n.Line && line <= n.EndLine {
				cur = append(cur, n)
				if len(cur) > len(best) {
					best = append([]*Node(nil), cur...)
				}
				dfs(n.Children)
				cur = cur[:len(cur)-1]
			} else if len(n.Children) > 0 {
				dfs(n.Children)
			}
		}
	}
	if tree != nil {
		dfs(tree.Root)
	}
	return best
}

// DiscoverByReturn groups by success-terminal site (effect) and labels each
// outcome with its causing guard chain (cause). tpforward delegations are
// outcomes of kind "forward" (no local reply shape — Adds stays empty and
// ForwardTo names the delegate). Tail returns outside any branch are marked
// Convergent with per-arm censuses so the caller can apply the condition
// split inside the outcome.
func DiscoverByReturn(tree *Tree, conditions []ir.Condition, facts *scanner.SourceFacts, fn string) []ReturnOutcome {
	sites := SuccessReturnSites(facts, fn)
	for _, s := range ForwardSites(facts, fn) {
		sites = append(sites, s)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Line < sites[j].Line })
	out := make([]ReturnOutcome, 0, len(sites))
	for _, s := range sites {
		path := branchPath(tree, s.Line)
		oc := ReturnOutcome{
			ReturnLine:   s.Line,
			InsideBranch: len(path) > 0,
		}
		if s.Name == "tpforward" {
			oc.Kind = "forward"
			oc.ForwardTo = forwardService(s.Args)
			oc.Buffer = forwardBuffer(s.Args)
		} else {
			oc.Kind = "return"
			oc.Buffer = returnBuffer(s.Args)
		}
		for _, b := range path {
			if IsNoiseCond(b.Cond) {
				continue
			}
			oc.GuardChain = append(oc.GuardChain, b.Cond)
			oc.GuardLines = append(oc.GuardLines, b.Line)
		}
		if len(path) > 0 {
			inner := path[len(path)-1]
			census := fmlCensus(inner)
			oc.Gets, oc.Adds, oc.ErrorAdds = census.gets, census.adds, census.errorAdds
			oc.QueryIDs = append([]string(nil), inner.QueryIDs...)
			oc.TopCondition = conditionIndexOf(path[0], conditions)
		} else {
			// Tail return: every top-level dispatch arm flows here.
			// Record each arm's census so the hybrid can split inside.
			oc.Convergent = true
			for _, root := range tree.Root {
				if root.Kind != KindBranch {
					continue
				}
				census := fmlCensus(root)
				if len(census.gets) == 0 && len(census.adds) == 0 && len(census.errorAdds) == 0 && len(root.QueryIDs) == 0 {
					continue
				}
				oc.Arms = append(oc.Arms, ReturnArm{
					Cond:      root.Cond,
					StartLine: root.Line,
					EndLine:   root.EndLine,
					Gets:      census.gets,
					Adds:      census.adds,
					ErrorAdds: census.errorAdds,
					QueryIDs:  append([]string(nil), root.QueryIDs...),
				})
			}
			sort.Slice(oc.Arms, func(i, j int) bool { return oc.Arms[i].StartLine < oc.Arms[j].StartLine })
		}
		out = append(out, oc)
	}
	return out
}
