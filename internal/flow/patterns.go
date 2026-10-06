package flow

import (
	"fmt"
	"sort"
	"strings"
	"tux-migration-tool/internal/ir"
)

// HintKind names a recognized corpus idiom (PRD-2026-09-10 FLW-D4).
type HintKind string

const (
	HintFetchIterate   HintKind = "fetch-then-iterate"
	HintRequestGuard   HintKind = "request-guard"
	HintResponseFanout HintKind = "response-fanout"
	HintErrOpLoop      HintKind = "err-op-check-loop"
	HintDebugIf        HintKind = "debug-only-if"
	HintOccDecode      HintKind = "occurrence-decode"
	HintDoWhile        HintKind = "do-while"
)

// Hint marks one recognized idiom at a source line.
type Hint struct {
	Kind   HintKind `json:"kind"`
	Line   int      `json:"line"`
	Detail string   `json:"detail"`
}

// Match walks the tree and returns the recognized idioms sorted by line.
func Match(tree *Tree) []Hint {
	var out []Hint
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, h := range matchNode(n) {
			out = append(out, h)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range tree.Root {
		walk(r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func matchNode(n *Node) []Hint {
	var out []Hint
	switch {
	case n.Kind == KindLoop && n.Sub == "do":
		out = append(out, Hint{Kind: HintDoWhile, Line: n.Line, Detail: "do-while → Go for {} with a trailing break check"})
	case n.Kind == KindLoop && isFetchLoop(n):
		qids := strings.Join(n.QueryIDs, ", ")
		out = append(out, Hint{Kind: HintFetchIterate, Line: n.Line, Detail: fmt.Sprintf(
			"cursor fetch loop → one multi-row store call + range over rows (queries: %s)", orNone(qids))})
	case n.Kind == KindLoop && isOccDecodeLoop(n):
		out = append(out, Hint{Kind: HintOccDecode, Line: n.Line,
			Detail: "occurrence-bound decode loop → request decode is implicit in the request struct; the loop collapses"})
	case n.Kind == KindLoop && isErrOpLoop(n):
		out = append(out, Hint{Kind: HintErrOpLoop, Line: n.Line,
			Detail: "Fadd32 result-check loop → Go error handling covers it; droppable"})
	case n.Kind == KindBranch && isRequestGuard(n):
		out = append(out, Hint{Kind: HintRequestGuard, Line: n.Line,
			Detail: "request read guard → collapses in Go (request struct field + error return)"})
	case n.Kind == KindBranch && isDebugIf(n):
		out = append(out, Hint{Kind: HintDebugIf, Line: n.Line,
			Detail: "debug-logging branch → elide (the method template owns logging)"})
	}
	if n.Kind == KindLoop || n.Kind == KindBranch {
		for buf, fields := range fanoutAdds(n) {
			if len(fields) >= 2 {
				out = append(out, Hint{Kind: HintResponseFanout, Line: n.Line, Detail: fmt.Sprintf(
					"%d response fields added to %s → map rows/request to the response struct (%s)",
					len(fields), buf, strings.Join(fields, ", "))})
			}
		}
	}
	return out
}

// isFetchLoop reports whether a FETCH SQL span sits inside the loop (not
// crossing into a nested loop's own fetch loop).
func isFetchLoop(n *Node) bool {
	var has func(ns []*Node) bool
	has = func(ns []*Node) bool {
		for _, c := range ns {
			if c.Kind == KindSQL && c.Sub == "FETCH" {
				return true
			}
			if c.Kind == KindLoop {
				continue // a nested loop owns its own fetch
			}
			if has(c.Children) {
				return true
			}
		}
		return false
	}
	return has(n.Children)
}

// isOccDecodeLoop reports a loop bounded by an FML occurrence count.
func isOccDecodeLoop(n *Node) bool {
	c := strings.ToLower(n.Cond)
	return strings.Contains(c, "foccur32") || strings.Contains(c, "foccur")
}

// isErrOpLoop reports the corpus's Fadd-result check loop: an indexed loop
// whose body tests a result slot against -1 (the `[i]` slot may appear in
// the body rather than the header).
func isErrOpLoop(n *Node) bool {
	var has func(ns []*Node) bool
	has = func(ns []*Node) bool {
		for _, c := range ns {
			if c.Kind == KindLoop {
				continue // a nested loop owns its own result checks
			}
			if (c.Kind == KindStmt && strings.Contains(c.Text, "-1") && strings.Contains(c.Text, "[")) ||
				(c.Kind == KindBranch && strings.Contains(c.Cond, "-1") && strings.Contains(c.Cond, "[")) {
				return true
			}
			if has(c.Children) {
				return true
			}
		}
		return false
	}
	return has(n.Children)
}

// isRequestGuard reports the request-read guard shape: an if whose condition
// contains an Fget32 call, whose body adds an error field or error-message
// value and returns (tpreturn or C return).
func isRequestGuard(n *Node) bool {
	if !strings.Contains(n.Cond, "Fget32") {
		return false
	}
	errAdd := false
	for _, op := range n.FmlOps {
		if op.Kind == ir.FmlAdd && (ir.IsErrField(op.Field) || ir.IsErrValue(op.Target)) {
			errAdd = true
		}
	}
	if !errAdd {
		return false
	}
	for _, c := range n.Calls {
		if c == "tpreturn" {
			return true
		}
	}
	for _, c := range n.Children {
		if c.Kind == KindReturn {
			return true
		}
	}
	return false
}

// isDebugIf reports a branch that only logs: DEBUG-flag condition, every
// call in span a logging call.
func isDebugIf(n *Node) bool {
	if !strings.Contains(n.Cond, "DEBUG") {
		return false
	}
	if len(n.Calls) == 0 {
		return false
	}
	for _, c := range n.Calls {
		if c != "userlog" && c != "errlog" {
			return false
		}
	}
	return true
}

// fanoutAdds groups the node's Fadd32 ops by buffer, returning field names
// per buffer with ≥2 adds (the response-mapping idiom). Error emissions are
// not response mapping — an ERR field or an error-message value (the
// "fadd c_errmsg = returning error" idiom) is excluded. The buffer plays no
// part: the reply is often the request buffer reused in place.
func fanoutAdds(n *Node) map[string][]string {
	byBuf := map[string][]string{}
	for _, op := range n.FmlOps {
		if op.Kind != ir.FmlAdd {
			continue
		}
		if ir.IsErrField(op.Field) || ir.IsErrValue(op.Target) {
			continue
		}
		byBuf[op.Buffer] = append(byBuf[op.Buffer], op.Field)
	}
	out := map[string][]string{}
	for buf, fields := range byBuf {
		if len(fields) >= 2 {
			out[buf] = fields
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none linked"
	}
	return s
}
