package flow

// MixedGuard is one live runtime-dispatch guard of a scenario slice: the
// residual predicate a FoldMixed node keeps. For a merged scenarioFilter the
// residual is the legacy predicate verbatim (axis terms stay for runtime
// dispatch); for a single-value slice it is the axis-stripped remainder, and
// Alt carries the original predicate (the other exact implementation a
// translation may keep). Both convert seams (Go and CS) gate their accepted
// bodies against these.
type MixedGuard struct {
	Line int    `json:"line"`
	Cond string `json:"cond"`
	// Alt is the original (pre-fold) predicate when it differs from Cond —
	// a single-value slice may keep the full legacy condition instead of
	// the stripped residual. Empty for merged-filter guards.
	Alt string `json:"alt,omitempty"`
}

// MixedGuards collects a scenario slice's live dispatch guards: every
// FoldMixed node whose surviving subtree carries behavior (a statement,
// loop, query, return — not just empty branch wrappers). Deduped by
// condition pair so repeated identical guards report once.
func MixedGuards(sc *Scenario) []MixedGuard {
	if sc == nil {
		return nil
	}
	var out []MixedGuard
	seen := map[[2]string]bool{}
	var walk func(nodes []*SliceNode)
	walk = func(nodes []*SliceNode) {
		for _, sn := range nodes {
			if sn == nil {
				continue
			}
			if sn.Fold == FoldMixed && sn.FoldedCond != "" && guardCarriesBehavior(sn) {
				g := MixedGuard{Line: sn.Line, Cond: sn.FoldedCond}
				if sn.Cond != "" && sn.Cond != sn.FoldedCond {
					g.Alt = sn.Cond
				}
				key := [2]string{g.Cond, g.Alt}
				if !seen[key] {
					seen[key] = true
					out = append(out, g)
				}
			}
			walk(sn.Children)
		}
	}
	walk(sc.Body)
	return out
}

// guardCarriesBehavior reports whether a mixed node's surviving subtree
// contains any non-branch node — a guard whose body folded to empty branch
// wrappers gates no behavior and needs no live condition.
func guardCarriesBehavior(sn *SliceNode) bool {
	for _, c := range sn.Children {
		if c == nil {
			continue
		}
		if c.Kind != KindBranch || guardCarriesBehavior(c) {
			return true
		}
	}
	return false
}
