package flow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// Candidate is one discovered API candidate (PRD-2026-09-10 endpoint
// discovery): a block enclosing request reads and non-error response
// writes. Key is the stable draft reference — "c<n>" for a top-level
// condition, "c<n>.<k>" for the k-th qualifying nested branch under it.
type Candidate struct {
	Key             string   `json:"key"`
	ParentCondition int      `json:"parent_condition"`
	Cond            string   `json:"cond,omitempty"`
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Gets            []string `json:"gets,omitempty"`
	Adds            []string `json:"adds,omitempty"`
	ErrorAdds       []string `json:"error_adds,omitempty"`
	Codes           []string `json:"codes,omitempty"`
	QueryIDs        []string `json:"query_ids,omitempty"`
	Redundant       bool     `json:"redundant,omitempty"`
	// TerminalLine is the success-terminal this arm owns: a
	// tpreturn(TPSUCCESS,...) or tpforward(...) line in the arm's own
	// span (0 when the arm carries no terminal). ForwardTo names the
	// tpforward target service ("" for tpreturn outcomes). Arms whose
	// reads live in the shared entry preamble carry no Gets — the
	// terminal is what marks them convertible (return-anchored II).
	TerminalLine int    `json:"terminal_line,omitempty"`
	ForwardTo    string `json:"forward_to,omitempty"`
	// FeedsTail marks a tail-feeder: a business arm with response shape
	// that drains into a shared success tail (a tpreturn(TPSUCCESS) /
	// tpforward outside every branch) instead of owning a terminal.
	// The value is the tail's line. Tail-feeders are convergent-split
	// outcomes: one candidate per arm, each sliced with its own span
	// plus the shared entry preamble (gen unions consumed preamble
	// reads into the request).
	FeedsTail int `json:"feeds_tail,omitempty"`
}

// Discover walks the tree and returns the API candidates sorted by line:
// top-level branches matched against the condition inventory (key c<n>),
// plus qualifying nested branches (key c<n>.<k>). An if whose adds are all
// error emissions fails the rubric — it is a guard (the read collapses into
// the request struct), never an endpoint.
//
// Qualification is return-anchored (experiment exp/return-anchored-detection,
// now wired): a branch qualifies on request reads AND non-error response
// writes, OR on non-error response writes plus a success terminal the arm
// itself owns (tpreturn(TPSUCCESS,...) / tpforward(...)). The second leg
// covers dispatch arms whose reads live in the shared entry preamble
// (24 TPSUCCESS arms in the large dispatch file, most with no in-arm
// Fget32) — the terminal
// is the outcome evidence, and gen's requestFields unions the consumed
// preamble reads back into the request contract, so the candidate's
// request never renders empty for lack of in-arm Gets.
func Discover(tree *Tree, conditions []ir.Condition) []Candidate {
	var fn string
	var facts *scanner.SourceFacts
	if tree != nil {
		fn = tree.Function
		facts = tree.facts
	}
	// qualifies is the single candidate test shared by emission and by
	// ConditionFor's DFS replay (kthQualifyingChild) — the two must never
	// drift or refs resolve to the wrong block.
	qualifies := func(n *Node) (bool, int, string) {
		return branchQualifies(n, facts, fn)
	}
	var out []Candidate
	emitted := map[int]bool{}
	// walkChildren examines branch children under a qualifying parent key.
	// The qualifier counter k is shared across the parent's whole subtree
	// walk — non-branch children and census-failing branches recurse inside
	// the same walk (a fresh invocation per subtree would renumber them and
	// collide with later siblings' keys, and ConditionFor's DFS replay
	// would then resolve a ref to a different block than the draft
	// described). Nested candidates under a qualifying branch start their
	// own counter under that branch's key.
	var walkChildren func(n *Node, parentKey string, parentIdx int, parentCand *Node)
	walkChildren = func(n *Node, parentKey string, parentIdx int, parentCand *Node) {
		k := 0
		var walk func(n *Node)
		walk = func(n *Node) {
			for _, c := range n.Children {
				if c.Kind != KindBranch {
					walk(c)
					continue
				}
				ok, termLine, fwd := qualifies(c)
				if !ok {
					// Guard or logic-only — not a candidate, but its subtree can
					// hold candidates under the same parent.
					walk(c)
					continue
				}
				k++
				census := fmlCensus(c)
				cand := Candidate{
					Key:             fmt.Sprintf("%s.%d", parentKey, k),
					ParentCondition: parentIdx,
					Cond:            c.Cond,
					StartLine:       c.Line,
					EndLine:         c.EndLine,
					Gets:            census.gets,
					Adds:            census.adds,
					ErrorAdds:       census.errorAdds,
					Codes:           fmlCodes(c),
					QueryIDs:        c.QueryIDs,
					TerminalLine:    termLine,
					ForwardTo:       fwd,
				}
				if parentCand != nil {
					pc := fmlCensus(parentCand)
					cand.Redundant = subset(census.gets, pc.gets) && subset(census.adds, pc.adds)
				}
				out = append(out, cand)
				walkChildren(c, cand.Key, parentIdx, c)
			}
		}
		walk(n)
	}
	for _, root := range tree.Root {
		if root.Kind != KindBranch {
			walkChildren(root, "", 0, nil)
			continue
		}
		census := fmlCensus(root)
		idx := conditionIndexOf(root, conditions)
		ok, termLine, fwd := qualifies(root)
		if !ok || idx == 0 {
			// Guard/logic-only root — not a candidate itself, but its
			// subtree can hold candidates. Anchor them to the condition
			// inventory when the root maps to one (DIS-D3 keys are c<n>.<k>;
			// an empty anchor yields unloadable ".<k>" refs).
			key := ""
			if idx > 0 {
				key = "c" + strconv.Itoa(idx)
			}
			walkChildren(root, key, idx, nil)
			continue
		}
		out = append(out, Candidate{
			Key:             "c" + strconv.Itoa(idx),
			ParentCondition: idx,
			Cond:            root.Cond,
			StartLine:       root.Line,
			EndLine:         root.EndLine,
			Gets:            census.gets,
			Adds:            census.adds,
			ErrorAdds:       census.errorAdds,
			Codes:           fmlCodes(root),
			QueryIDs:        root.QueryIDs,
			TerminalLine:    termLine,
			ForwardTo:       fwd,
		})
		emitted[idx] = true
		walkChildren(root, "c"+strconv.Itoa(idx), idx, root)
	}
	// Tail-feeder promotion (convergent split): a shared success tail —
	// a terminal outside every branch — multiplexes the business arms
	// that drain into it. Each such arm becomes a c<n> candidate marked
	// FeedsTail, so the fallback draft covers convergent files too.
	// v1 scope: top-level inventory-mapped arms only; arms owning their
	// own terminal are already candidates above and never re-emitted.
	for _, tail := range convergentTails(tree, facts, fn) {
		for _, root := range tree.Root {
			if root.Kind != KindBranch || root.EndLine >= tail {
				continue
			}
			idx := conditionIndexOf(root, conditions)
			if idx == 0 || emitted[idx] {
				continue
			}
			if IsNoiseCond(root.Cond) {
				continue
			}
			if termLine, _ := successTerminal(root, facts, fn); termLine != 0 {
				continue
			}
			// An arm ending in its own failure exit (the unconditional
			// trap `Fadd(err); tpreturn(TPFAIL)`) never reaches the tail.
			if endsInExit(root, facts, fn) {
				continue
			}
			census := fmlCensus(root)
			if len(census.adds) == 0 && len(root.QueryIDs) == 0 {
				continue
			}
			out = append(out, Candidate{
				Key:             "c" + strconv.Itoa(idx),
				ParentCondition: idx,
				Cond:            root.Cond,
				StartLine:       root.Line,
				EndLine:         root.EndLine,
				Gets:            census.gets,
				Adds:            census.adds,
				ErrorAdds:       census.errorAdds,
				Codes:           fmlCodes(root),
				QueryIDs:        root.QueryIDs,
				FeedsTail:       tail,
			})
			emitted[idx] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartLine < out[j].StartLine })
	return out
}

// conditionIndexOf matches a root branch node to its 1-based condition
// inventory index. An exact header-line match wins first: a chain header
// may share its line with the previous arm's closing brace (`} else if …`
// on one line), which sits inside both spans — and a smallest-span
// tie-break alone would steal the header for the previous arm whenever
// that arm is the smaller one. The inventory's preorder order (an arm's
// own condition precedes any same-line nested condition) makes the first
// exact match the node's own. Containment with the smallest span remains
// the fallback for shapes the header line alone cannot decide.
func conditionIndexOf(n *Node, conditions []ir.Condition) int {
	for i := range conditions {
		if conditions[i].StartLine == n.Line {
			return conditions[i].Index
		}
	}
	best := 0
	bestSize := 1 << 30
	for i := range conditions {
		c := &conditions[i]
		if n.Line >= c.StartLine && n.Line <= c.EndLine {
			if size := c.EndLine - c.StartLine; size < bestSize {
				best, bestSize = c.Index, size
			}
		}
	}
	return best
}

func subset(child, parent []string) bool {
	if len(parent) == 0 {
		return false
	}
	pm := map[string]bool{}
	for _, s := range parent {
		pm[s] = true
	}
	for _, s := range child {
		if !pm[s] {
			return false
		}
	}
	return true
}

// fmlCensus splits a node's FML ops into distinct request reads, distinct
// non-error response writes, and the error emissions (DIS-D2).
func fmlCensus(n *Node) struct {
	gets      []string
	adds      []string
	errorAdds []string
} {
	seenGet := map[string]bool{}
	seenAdd := map[string]bool{}
	seenErr := map[string]bool{}
	var out struct {
		gets      []string
		adds      []string
		errorAdds []string
	}
	for _, op := range n.FmlOps {
		switch {
		case op.Kind == ir.FmlGet:
			if !seenGet[op.Field] {
				seenGet[op.Field] = true
				out.gets = append(out.gets, op.Field)
			}
		case op.Kind == ir.FmlAdd && isErrorAdd(op):
			if !seenErr[op.Field] {
				seenErr[op.Field] = true
				out.errorAdds = append(out.errorAdds, op.Field)
			}
		case op.Kind == ir.FmlAdd:
			if !seenAdd[op.Field] {
				seenAdd[op.Field] = true
				out.adds = append(out.adds, op.Field)
			}
		}
	}
	return out
}

// fmlCodes lists the distinct legacy error codes the node's FML ops carry
// (PRD-2026-09-10 defines pass, G-DEF6) — the draft's per-candidate
// retention comment.
func fmlCodes(n *Node) []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range n.FmlOps {
		if op.Code == "" || seen[op.Code] {
			continue
		}
		seen[op.Code] = true
		out = append(out, op.Code)
	}
	return out
}

// isErrorAdd reports an error-emission add: the field is an ERR field or
// the value written is the service's error-message variable (the "fadd
// c_errmsg = returning error" idiom). The buffer plays no part — the reply
// is often the request buffer reused in place, so a role-based rule would
// swallow genuine response writes (FML_VLME into the reused input buffer).
// Every non-error add is a response write: something is returned either
// way, which is what makes the branch convertible into an API. Gets never
// qualify: a read-guard node shares the input buffer, and marking its GETs
// as error emissions would erase the branch's request fields from the
// contract.
func isErrorAdd(op ir.FmlOp) bool {
	if op.Kind != ir.FmlAdd {
		return false
	}
	return ir.IsErrField(op.Field) || ir.IsErrValue(op.Target)
}

// Census summarizes a condition's FML ops with the discovery error-add rule
// (DIS-D2, ERR-field/value convention — the error emissions the draft
// comments use). It works for any condition, qualifying or not.
func Census(c *ir.Condition) (gets, adds, errs []string) {
	seen := map[string]map[string]bool{"/g": {}, "/a": {}, "/e": {}}
	add := func(k, s string) []string {
		if !seen[k][s] {
			seen[k][s] = true
			switch k {
			case "/g":
				gets = append(gets, s)
			case "/a":
				adds = append(adds, s)
			default:
				errs = append(errs, s)
			}
		}
		return nil
	}
	for _, op := range c.FmlOps {
		switch {
		case op.Kind == ir.FmlGet:
			add("/g", op.Field)
		case op.Kind == ir.FmlAdd && (ir.IsErrField(op.Field) || ir.IsErrValue(op.Target)):
			add("/e", op.Field)
		case op.Kind == ir.FmlAdd:
			add("/a", op.Field)
		}
	}
	return gets, adds, errs
}

// ConditionFor re-derives the candidate a draft reference names (DIS-D3):
// "c<n>" resolves to the top-level branch matching condition n's span,
// "c<n>.<k>" to its k-th qualifying nested branch. The returned condition
// is synthesized (Index 0 — never collides with the 1-based inventory) with
// Error-flagged FML ops so contract derivation can exclude error emissions.
func ConditionFor(tree *Tree, conditions []ir.Condition, ref string) (*ir.Condition, error) {
	segments := strings.Split(ref, ".")
	if len(segments) == 0 || !strings.HasPrefix(segments[0], "c") {
		return nil, fmt.Errorf("flow: malformed candidate reference %q (want c<n> or c<n>.<k>)", ref)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(segments[0], "c"))
	if err != nil || n < 1 || n > len(conditions) {
		return nil, fmt.Errorf("flow: candidate reference %q — condition inventory has %d condition(s)", ref, len(conditions))
	}
	c := &conditions[n-1]
	current := rootFor(tree, c)
	if current == nil {
		return nil, fmt.Errorf("flow: candidate reference %q — no branch node matches condition %d's span", ref, n)
	}
	for _, seg := range segments[1:] {
		k, err := strconv.Atoi(seg)
		if err != nil || k < 1 {
			return nil, fmt.Errorf("flow: malformed candidate reference %q", ref)
		}
		found := kthQualifyingChild(tree, current, k)
		if found == nil {
			return nil, fmt.Errorf("flow: candidate reference %q — branch at line %d has fewer qualifying nested branch(es)", ref, current.Line)
		}
		current = found
	}
	return synthCondition(current), nil
}

// rootFor matches a condition's span to its root branch node — the same
// matcher conditionIndexOf uses in reverse: exact header-line match first
// (chain headers can share the previous arm's brace line), then the first
// root whose line the span contains.
func rootFor(tree *Tree, c *ir.Condition) *Node {
	for _, root := range tree.Root {
		if root.Kind == KindBranch && root.Line == c.StartLine {
			return root
		}
	}
	for _, root := range tree.Root {
		if root.Kind == KindBranch && root.Line >= c.StartLine && root.Line <= c.EndLine {
			return root
		}
	}
	return nil
}

// branchQualifies is the one candidate test: a branch is an API candidate
// iff it shapes a non-error response (adds, optionally queries — queries
// ride the tail-feeder pass) AND proves it is an outcome. Proof, in order:
// own a success terminal (tpreturn TPSUCCESS / tpforward), else carry
// request reads — unless the arm is an unconditional trap (shapes a
// response nobody receives: failure exit with no owned code after it).
// Guards (error-only adds), logic-only arms, and terminal-less write arms
// fail. TPFAIL legs are error exits, never proof.
func branchQualifies(n *Node, facts *scanner.SourceFacts, fn string) (bool, int, string) {
	census := fmlCensus(n)
	if len(census.adds) == 0 {
		return false, 0, ""
	}
	line, fwd := successTerminal(n, facts, fn)
	if line != 0 {
		return true, line, fwd
	}
	if len(census.gets) == 0 {
		return false, 0, ""
	}
	if endsInExit(n, facts, fn) {
		return false, 0, ""
	}
	return true, 0, ""
}

// kthQualifyingChild replays Discover's key assignment: the k-th qualifying
// branch in the same depth-first order the walk used for keys, using the
// shared branchQualifies test — the two must never drift or refs resolve
// to the wrong block.
func kthQualifyingChild(tree *Tree, n *Node, k int) *Node {
	count := 0
	var fn string
	var facts *scanner.SourceFacts
	if tree != nil {
		fn = tree.Function
		facts = tree.facts
	}
	var walk func(n *Node) *Node
	walk = func(n *Node) *Node {
		for _, c := range n.Children {
			if c.Kind != KindBranch {
				if f := walk(c); f != nil {
					return f
				}
				continue
			}
			if ok, _, _ := branchQualifies(c, facts, fn); !ok {
				if f := walk(c); f != nil {
					return f
				}
				continue
			}
			count++
			if count == k {
				return c
			}
		}
		return nil
	}
	return walk(n)
}

// convergentTails lists success-terminal lines outside every branch body
// (function-tail tpreturn(TPSUCCESS)/tpforward sites): shared tails that
// multiplex the arms draining into them. Sorted ascending.
func convergentTails(tree *Tree, facts *scanner.SourceFacts, fn string) []int {
	if tree == nil || facts == nil {
		return nil
	}
	var tails []int
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Func != fn {
			continue
		}
		var isTail bool
		switch {
		case c.Name == "tpreturn" && strings.Contains(c.Args, "TPSUCCESS"):
			isTail = true
		case c.Name == "tpforward":
			isTail = true
		}
		if !isTail {
			continue
		}
		if len(branchPath(tree, c.Line)) == 0 {
			tails = append(tails, c.Line)
		}
	}
	sort.Ints(tails)
	return tails
}

// descendantSpans lists every branch span strictly below n.
func descendantSpans(n *Node) [][2]int {
	var spans [][2]int
	var collect func(x *Node)
	collect = func(x *Node) {
		for _, c := range x.Children {
			if c.Kind == KindBranch {
				spans = append(spans, [2]int{c.Line, c.EndLine})
			}
			collect(c)
		}
	}
	collect(n)
	return spans
}

// insideSpans reports whether line sits in any of the spans.
func insideSpans(spans [][2]int, line int) bool {
	for _, s := range spans {
		if line >= s[0] && line <= s[1] {
			return true
		}
	}
	return false
}

// ownedBy reports whether line is n's own code: inside n's span but
// outside every descendant branch's span (nested code belongs to the
// nested arm, never the parent).
func ownedBy(n *Node, spans [][2]int, line int) bool {
	return line >= n.Line && line <= n.EndLine && !insideSpans(spans, line)
}

// endsInExit reports whether the arm never falls through: it owns a
// failure exit (tpreturn(TPFAIL) or a bare C return) with no owned code
// after it — e.g. the unconditional-trap idiom `Fadd(err); tpreturn(TPFAIL)`
// as the arm body. Conditional exits nested in sub-branches don't count:
// the arm still feeds whatever follows on the pass-through path.
func endsInExit(n *Node, facts *scanner.SourceFacts, fn string) bool {
	if n == nil || facts == nil {
		return false
	}
	spans := descendantSpans(n)
	var exits []int
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Func != fn || !ownedBy(n, spans, c.Line) {
			continue
		}
		if c.Name == "tpreturn" && strings.Contains(c.Args, "TPFAIL") {
			exits = append(exits, c.Line)
		}
	}
	for i := range facts.Returns {
		r := &facts.Returns[i]
		if r.Func != fn || !ownedBy(n, spans, r.Line) {
			continue
		}
		exits = append(exits, r.Line)
	}
	for _, e := range exits {
		followed := false
		for i := range facts.Calls {
			c := &facts.Calls[i]
			if c.Func == fn && c.Line > e && ownedBy(n, spans, c.Line) {
				followed = true
				break
			}
		}
		if !followed {
			for i := range facts.Returns {
				r := &facts.Returns[i]
				if r.Func == fn && r.Line > e && ownedBy(n, spans, r.Line) {
					followed = true
					break
				}
			}
		}
		if !followed {
			for i := range facts.AllSQL {
				s := &facts.AllSQL[i]
				if s.Func == fn && s.StartLine > e && ownedBy(n, spans, s.StartLine) {
					followed = true
					break
				}
			}
		}
		if !followed {
			return true
		}
	}
	return false
}

// successTerminal finds the success terminal the branch itself owns: a
// tpreturn(TPSUCCESS,...) or tpforward(...) line of the node's own code
// (a nested terminal belongs to the nested arm, never the parent).
// Returns the line and, for tpforward, the target service ("" for tpreturn
// outcomes). Zero line = none owned. TPFAIL legs are error exits, never
// outcomes.
func successTerminal(n *Node, facts *scanner.SourceFacts, fn string) (line int, forwardTo string) {
	if n == nil || facts == nil {
		return 0, ""
	}
	spans := descendantSpans(n)
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Func != fn || !ownedBy(n, spans, c.Line) {
			continue
		}
		switch {
		case c.Name == "tpreturn" && strings.Contains(c.Args, "TPSUCCESS"):
			return c.Line, ""
		case c.Name == "tpforward":
			return c.Line, forwardService(c.Args)
		}
	}
	return 0, ""
}

// forwardService extracts tpforward's target service (arg0): a string
// literal in every corpus sample, trimmed defensively.
func forwardService(args string) string {
	parts := splitCallArgs(args)
	if len(parts) == 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(parts[0]), "\" \t")
}

// synthCondition converts a branch node into the condition-shaped value the
// plan/gen machinery consumes. Index 0 marks it synthesized (DIS-D4).
func synthCondition(n *Node) *ir.Condition {
	c := &ir.Condition{
		Index:     0,
		Kind:      n.Sub,
		Expr:      n.Cond,
		StartLine: n.Line,
		EndLine:   n.EndLine,
		QueryIDs:  n.QueryIDs,
	}
	if n.Predicate != nil {
		e := *n.Predicate
		c.Predicate = &e
	}
	for _, op := range n.FmlOps {
		o := op
		if isErrorAdd(o) {
			o.Error = true
		}
		c.FmlOps = append(c.FmlOps, o)
	}
	return c
}
