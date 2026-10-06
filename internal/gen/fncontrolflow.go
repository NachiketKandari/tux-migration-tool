package gen

import (
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/plan"
)

// ControlFlowNotRendered is the reason code for a legacy control-flow
// construct inside a fn helper that the deterministic path cannot yet
// project. It is counted by internal/walkreport.
//
// The alternative was to keep rendering helper bodies as a flat statement
// list, which is what made a 200-line legacy function come out as three
// statements that looked complete. Silence is the failure mode; a counted
// gap is not.
const ControlFlowNotRendered = "R-CONTROL-FLOW-NOT-RENDERED"

// CallOutsideItsGuard is the reason code for a call that the deterministic
// path emitted unconditionally even though the legacy source guarded it.
//
// It is a separate code from ControlFlowNotRendered because it is a strictly
// worse defect: an unrendered branch is missing work, whereas a call hoisted
// out of its guard asserts something the source does not — the emitted Go
// says "always", the C says "when c_flg_using == 'A'". A reader of the Go
// would be misled about behaviour, not merely left without detail. Counting
// them apart keeps the severity visible.
const CallOutsideItsGuard = "R-CALL-OUTSIDE-ITS-GUARD"

// elidableHints are the flow idioms whose own matchNode detail says the
// construct collapses in Go, so a helper body that omits them is complete
// with respect to them.
//
// This set is deliberately NOT "every hint flow.Match can return". Two of the
// hints describe a rendering rather than a disappearance:
//
//   - HintFetchIterate: "cursor fetch loop → one multi-row store call +
//     range over rows" — the loop must become a range. Until it does, an
//     omitted fetch loop is missing work.
//   - HintDoWhile: "do-while → Go for {} with a trailing break check" —
//     likewise; the loop must become a for.
//
// Treating those as elidable would be the exact smoothing-over this project
// forbids: the hint would license a gap that no code has yet filled. The
// four below are the ones whose detail text says the construct itself goes
// away.
var elidableHints = map[flow.HintKind]bool{
	flow.HintDebugIf:      true, // "elide (the method template owns logging)"
	flow.HintErrOpLoop:    true, // "Go error handling covers it; droppable"
	flow.HintOccDecode:    true, // "request decode is implicit ... collapses"
	flow.HintRequestGuard: true, // "collapses in Go (request struct field + error return)"
}

// fnControlFlow is one control-flow construct in a helper's legacy body
// together with the verdict the deterministic path reached about it.
type fnControlFlow struct {
	line     int
	endLine  int
	kind     string
	cond     string
	verdict  string // human-readable, already says what was done
	rendered bool
}

// fnGuardLeak records a call emitted outside the branch or loop that
// conditionally guards it in the legacy source.
type fnGuardLeak struct {
	what      string
	line      int
	guardLine int
	guardCond string
}

// fnControlFlowReport is the full accounting for one helper body.
type fnControlFlowReport struct {
	items    []fnControlFlow
	leaks    []fnGuardLeak
	total    int
	rendered int
	elided   int
	missing  int
}

// detFnControlFlow walks a helper's flow tree and decides, for every branch
// and loop in it, whether the emitted body accounts for it.
//
// It returns a zero report when there is no tree to consult — an absent tree
// is a parse failure, not a claim that the body is complete, so the caller
// must not treat empty as "nothing to say". The helper's existing
// R-NO-STORE-CALLS marker covers the case where nothing at all rendered.
func (s *Service) detFnControlFlow(h *plan.FnHelper, ordered []*detCall, nested []detHelper) fnControlFlowReport {
	var rep fnControlFlowReport
	tree := s.helperFlowTree(h.Name)
	if tree == nil {
		return rep
	}
	hints := map[int]flow.HintKind{}
	for _, hint := range flow.Match(tree) {
		// First hint wins: matchNode's switch is ordered, so the first is
		// the classification it considered primary.
		if _, seen := hints[hint.Line]; !seen {
			hints[hint.Line] = hint.Kind
		}
	}
	// Lines of store calls that actually rendered. A SQLCODE guard directly
	// after one of these is the call's own error check, already emitted by
	// detEmitFnStoreCall.
	sqlAt := map[int]bool{}
	for _, dc := range ordered {
		sqlAt[dc.line] = true
	}
	// Nested helper calls are also statements; their lines matter for the
	// guard check even though they are not SQL.
	nestedAt := map[int]string{}
	for _, nh := range nested {
		nestedAt[nh.line] = nh.goName
	}

	var walk func(nodes []*flow.Node)
	walk = func(nodes []*flow.Node) {
		for i, n := range nodes {
			if n.Kind != flow.KindBranch && n.Kind != flow.KindLoop {
				walk(n.Children)
				continue
			}
			item := fnControlFlow{
				line:    n.Line,
				endLine: n.EndLine,
				kind:    string(n.Kind),
				cond:    n.Cond,
			}
			switch {
			case isSQLCodeGuard(n) && i > 0 && sqlAt[nodes[i-1].Line]:
				// The emitter's `if err != nil { ... return -1 }` IS this
				// check: the legacy int-status contract carries it. The
				// branch's errlog does not come across, which is the
				// logging posture the debug-only-if hint already
				// establishes for the whole file.
				item.verdict = "covered by the store call's error check"
				item.rendered = true
			case elidableHints[hints[n.Line]]:
				item.verdict = "elided: " + string(hints[n.Line]) + " collapses in Go"
			default:
				item.verdict = "not rendered"
			}
			rep.items = append(rep.items, item)
			walk(n.Children)
		}
	}
	walk(tree.Root)

	sort.SliceStable(rep.items, func(i, j int) bool { return rep.items[i].line < rep.items[j].line })
	for i := range rep.items {
		rep.total++
		switch {
		case rep.items[i].rendered:
			rep.rendered++
		case rep.items[i].verdict == "not rendered":
			rep.missing++
		default:
			rep.elided++
		}
	}
	rep.leaks = fnFindGuardLeaks(rep.items, ordered, nested)
	return rep
}

// fnFindGuardLeaks pairs each emitted statement with the smallest unrendered
// branch or loop whose span contains it. Smallest, because spans nest: the
// innermost guard is the one whose condition the call actually depends on.
func fnFindGuardLeaks(items []fnControlFlow, ordered []*detCall, nested []detHelper) []fnGuardLeak {
	var out []fnGuardLeak
	check := func(what string, line int) {
		best := -1
		for i, it := range items {
			if it.verdict != "not rendered" {
				continue
			}
			if line < it.line || line > it.endLine {
				continue
			}
			if best < 0 || it.endLine-it.line < items[best].endLine-items[best].line {
				best = i
			}
		}
		if best >= 0 {
			out = append(out, fnGuardLeak{
				what: what, line: line,
				guardLine: items[best].line, guardCond: items[best].cond,
			})
		}
	}
	for _, dc := range ordered {
		check(dc.method, dc.line)
	}
	for _, nh := range nested {
		check(nh.goName, nh.line)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// isSQLCodeGuard reports the legacy post-SQL error check: the branch that
// tests SQLCODE immediately after an embedded SQL statement. It is the one
// branch the deterministic emitter already renders, as the Go error check
// the store call emits.
func isSQLCodeGuard(n *flow.Node) bool {
	c := strings.ToLower(n.Cond)
	return strings.Contains(c, "sqlcode") && strings.Contains(c, "!=")
}

// detFnControlFlowTODOs renders the report as tuxconv:TODO comments.
//
// The accounting is a contiguous block rather than interleaved with the
// statements, for two reasons. The bodies here go through a transaction
// closure transform (detClosureLineFn re-splits on newlines), and a comment
// landing between the `if err != nil` line and its braces would be read as
// commented-out code. And the whole point is to be read as a list: the
// counts are the deliverable, and every entry carries its legacy line, so
// the original order is recoverable without relying on where the comment
// happened to be placed.
func (r fnControlFlowReport) TODOs(goName string) []string {
	if r.total == 0 {
		return nil
	}
	var out []string
	if r.missing > 0 {
		out = append(out, fmt.Sprintf(
			"// tuxconv:TODO %s: %s — %d of %d branch/loop constructs in the legacy "+
				"body are not rendered as Go control flow; the statements below are "+
				"this method's whole walk, so every construct listed here is work "+
				"this method does not do",
			ControlFlowNotRendered, goName, r.missing, r.total))
		for _, it := range r.items {
			if it.verdict != "not rendered" {
				continue
			}
			out = append(out, fmt.Sprintf("//   legacy %s at line %d: %s — not rendered",
				it.kind, it.line, detCondText(it.cond)))
		}
	}
	for _, lk := range r.leaks {
		out = append(out, fmt.Sprintf(
			"// tuxconv:TODO %s: %s at line %d is emitted unconditionally, but the legacy "+
				"source guards it with the %s at line %d (%s) — the call runs on paths "+
				"where the C does not",
			CallOutsideItsGuard, lk.what, lk.line, "branch", lk.guardLine, detCondText(lk.guardCond)))
	}
	return out
}

// detCondText renders a legacy condition for a comment, one line, truncated.
// The truncation is deliberate: a comment that ran to the screen width would
// wrap unpredictably, and the condition's head is what identifies it. The
// line number is the exact handle.
func detCondText(cond string) string {
	c := strings.Join(strings.Fields(cond), " ")
	if c == "" {
		return "(no condition — an else arm)"
	}
	const max = 90
	if len(c) > max {
		return c[:max] + "…"
	}
	return c
}
