package flow

import "fmt"

// FilterExample is one registry-derived scenarioFilter sample for a draft's
// commented examples (scenario-filter plan §5): the expression, the merged
// key a real fold produced, its matched assignments, and the honest block
// evidence. Every example is fold-verified, so a comment never claims a
// combination the source does not witness.
type FilterExample struct {
	Expr    string
	Key     string
	Matched []string
	Blocks  [][2]int
	Lines   int
	Reads   int
	Writes  int
	Queries int
}

// FilterExamples builds the commented scenarioFilter samples from the
// registry only: the primary-axis union over its first two domain values,
// plus the first reachable primary×secondary intersection. Candidates the
// plan would reject (unknown axis, unreachable conjunction) are skipped.
func FilterExamples(tree *Tree, axes []*DispatchAxis) []FilterExample {
	if tree == nil || len(axes) == 0 {
		return nil
	}
	var out []FilterExample
	primary := axes[0]
	if len(primary.Domain) >= 2 {
		expr := fmt.Sprintf("%s == '%s' || %s == '%s'",
			primary.Key(), primary.Domain[0], primary.Key(), primary.Domain[1])
		if ex, ok := filterExampleFor(tree, axes, expr); ok {
			out = append(out, ex)
		}
	}
	for _, sec := range axes[1:] {
		if sec.Kind != AxisSecondary {
			continue
		}
		found := false
		for _, pv := range primary.Domain {
			for _, sv := range sec.Domain {
				expr := fmt.Sprintf("%s == '%s' && %s == '%s'", primary.Key(), pv, sec.Key(), sv)
				if ex, ok := filterExampleFor(tree, axes, expr); ok {
					out = append(out, ex)
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			break
		}
	}
	return out
}

// filterExampleFor verifies one candidate expression with a real fold;
// candidates the plan would reject yield ok=false and are skipped.
func filterExampleFor(tree *Tree, axes []*DispatchAxis, expr string) (FilterExample, bool) {
	f, err := ParseScenarioFilter(expr)
	if err != nil {
		return FilterExample{}, false
	}
	sc, err := ScenarioForFilter(tree, axes, f)
	if err != nil {
		return FilterExample{}, false
	}
	blocks := KeptBlocks(sc, tree)
	return FilterExample{
		Expr: expr, Key: sc.Key, Matched: sc.FilterMatched, Blocks: blocks,
		Lines: KeptBlockLines(blocks), Reads: len(sc.Gets), Writes: len(sc.Adds),
		Queries: len(sc.Queries),
	}, true
}
