// Package flow derives a statement-level flow tree from scanner facts
// (PRD-2026-09-10): branches, loops, SQL spans, returns, and residual
// statement runs nested by block extents, each annotated with FML ops,
// query IDs, and callee names, plus a coverage metric that measures how
// much of a function's live code the deterministic parse classified. It is
// a read-only consumer of the parse stack — nothing here changes
// scanner/IR behavior (F3).
package flow

import (
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pred"
	scanner "tux-migration-tool/internal/tsscan"
)

// Kind names the statement role of a flow node.
type Kind string

const (
	KindBranch  Kind = "branch"
	KindLoop    Kind = "loop"
	KindSQL     Kind = "sql"
	KindStmt    Kind = "stmt"
	KindDecl    Kind = "decl"
	KindReturn  Kind = "return"
	KindUnknown Kind = "unknown"
)

// StmtSub names the classified statement shape.
const (
	SubAssign = "assign"
	SubCall   = "call"
	SubExpr   = "expr"
)

// Node is one statement-level flow element. Children nest by block extents
// (a branch/loop's block interior); Text carries the trimmed source of
// stmt/unknown nodes; FmlOps/QueryIDs/Calls annotate the node's line span.
// BufRoles maps buffer variable names to their IR data-flow roles
// (input/output/send/recv) when the IR file was linked.
type Node struct {
	Kind      Kind              `json:"kind"`
	Sub       string            `json:"sub,omitempty"`
	Line      int               `json:"line"`
	EndLine   int               `json:"end_line,omitempty"`
	Cond      string            `json:"cond,omitempty"`
	Text      string            `json:"text,omitempty"`
	Predicate *pred.Expr        `json:"predicate,omitempty"`
	FmlOps    []ir.FmlOp        `json:"fml_ops,omitempty"`
	QueryIDs  []string          `json:"query_ids,omitempty"`
	Calls     []string          `json:"calls,omitempty"`
	BufRoles  map[string]string `json:"buf_roles,omitempty"`
	Children  []*Node           `json:"children,omitempty"`
}

// Coverage measures how much of a function's live code the deterministic
// parse classified (the parser-accuracy instrument).
type Coverage struct {
	CodeLines  int   `json:"code_lines"`
	Classified int   `json:"classified_lines"`
	Unknown    int   `json:"unknown_lines"`
	Residue    []int `json:"residue_lines,omitempty"`
}

// Tree is the flow of one function: root statements (the function body's
// top level) plus the coverage rollup.
type Tree struct {
	Function  string   `json:"function"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Root      []*Node  `json:"root"`
	Coverage  Coverage `json:"coverage"`

	// facts is the scanner source the tree was built from — the exact-line
	// record the scenario layer reads for call-site spans (unexported: not
	// part of the serialized IR).
	facts *scanner.SourceFacts
	// irFile is the extracted IR the tree was built from — the scoped
	// define table (G-DEF3) the scenario layer resolves constants through,
	// so the axis harvest and the predicate fold share one define universe.
	irFile *ir.File
}

// Build derives the flow tree of one function (fn == "" on a fragment file
// picks the synthesized __fragment). irFile (optional) links query IDs by
// line overlap — flattened cursor units attach to the loop containing their
// FETCH span.
func Build(src []byte, facts *scanner.SourceFacts, fn string, irFile *ir.File) *Tree {
	def, entry := pickFunction(facts, fn)
	tree := &Tree{Function: entry, facts: facts, irFile: irFile}
	if def == nil {
		return tree
	}
	tree.StartLine = def.BodyStartLine
	tree.EndLine = def.BodyEndLine
	if def.BodyStartLine == 0 {
		return tree // body never opened — nothing to cover
	}
	end := def.BodyEndLine
	if end == 0 {
		// Fault tolerance (F3 spirit): the scanner never closed this body
		// (unbalanced braces) — cover whatever is open instead of bailing,
		// and let the unbalanced fact stay loud in the coverage.
		end = facts.NumLines
	}

	lines := splitLines(src)
	code := maskedLines(facts, lines)
	anchors := collectAnchors(facts, entry, irFile)
	sort.SliceStable(anchors, func(i, j int) bool {
		if anchors[i].node.Line != anchors[j].node.Line {
			return anchors[i].node.Line < anchors[j].node.Line
		}
		return anchors[i].spanEnd < anchors[j].spanEnd
	})
	// Root span = the function's brace lines inclusive: bare braces filter
	// out as non-code, a fragment's first statement shares the body's
	// opening-brace line (BodyStartLine == first statement line there), and
	// a definition whose brace shares the signature line excludes that
	// signature from statement runs.
	sig := 0
	if !facts.Fragment && def.StartLine == def.BodyStartLine {
		sig = def.BodyStartLine
	}
	tree.Root = nest(code, facts, anchors, def.BodyStartLine, end, sig, entry, irFile)
	tree.Coverage = coverage(code, def.BodyStartLine, end, sig, tree.Root)
	return tree
}

// maskedLines strips comment content from every line using the recorded
// comment spans: interiors of multi-line comments vanish, comment-end lines
// keep only the content after the closer, single-line comments strip
// lexically. The result is the code-only line image the tree is built on.
func maskedLines(facts *scanner.SourceFacts, lines []string) []string {
	out := make([]string, len(lines))
	copy(out, lines)
	for _, c := range facts.Comments {
		if c.EndLine > c.StartLine {
			for l := c.StartLine + 1; l < c.EndLine && l-1 < len(out); l++ {
				out[l-1] = ""
			}
			if c.EndLine-1 < len(out) {
				out[c.EndLine-1] = tailAfterCol(out[c.EndLine-1], c.EndCol)
			}
			continue
		}
		if c.StartLine-1 < len(out) {
			out[c.StartLine-1] = stripComments(out[c.StartLine-1])
		}
	}
	return out
}

// tailAfterCol keeps only the content after a 1-based column (the text
// following a comment's closing */).
func tailAfterCol(line string, col int) string {
	if col < 0 || col >= len(line) {
		return ""
	}
	return line[col:]
}

// ScanForIR scans source text the way the extraction path did: a fragment
// file (no entry function — a lone block/branch, PF-3) is wrapped by
// ScanFragment so the __fragment function def and rebased line numbers
// exist; otherwise the plain scan applies. Consumers re-deriving flow trees
// from an ir.File (plan, gen, the flow/discover commands) must use this —
// a plain ScanBytes on fragment text sees no function body and yields an
// empty tree even though the IR is complete.
func ScanForIR(src string, f *ir.File) (*scanner.SourceFacts, error) {
	if f.Fragment {
		return scanner.ScanFragment([]byte(src), f.Path)
	}
	return scanner.ScanBytes([]byte(src), f.Path)
}

// TreeFor scans source the way the extraction path did (ScanForIR — a
// fragment wraps via ScanFragment) and builds the entry function's flow
// tree. This is the one home for consumers re-deriving a tree from an
// ir.File (plan, gen, convert, the flow command); each caller keeps its own
// memo and failure policy (AD8): plan hard-errors, gen/convert degrade.
func TreeFor(src string, f *ir.File) (*Tree, error) {
	facts, err := ScanForIR(src, f)
	if err != nil {
		return nil, err
	}
	return Build([]byte(src), facts, f.Entry, f), nil
}

// pickFunction resolves the function to build: the named function, the
// fragment's synthesized pseudo-function, or a single-function file.
func pickFunction(facts *scanner.SourceFacts, fn string) (*scanner.FunctionDef, string) {
	if fn == "" && facts.Fragment && len(facts.Functions) > 0 {
		return &facts.Functions[0], facts.Functions[0].Name
	}
	if fn == "" && !facts.Fragment && len(facts.Functions) == 1 {
		// Empty-name convenience: a single-function file (the common
		// service/fragment shape) is unambiguous — pick it. An explicit
		// name that matches nothing still returns nil, never a guess.
		return &facts.Functions[0], facts.Functions[0].Name
	}
	for i := range facts.Functions {
		if facts.Functions[i].Name == fn {
			return &facts.Functions[i], fn
		}
	}
	return nil, fn
}

func splitLines(src []byte) []string {
	return strings.Split(string(src), "\n")
}

// anchor is one recorded construct with a line span, pending conversion to
// a node once nesting is decided. endConsumed marks a spanEnd line that is
// real code (SQL tail, do-while tail, unbraced header) — the next anchor
// scan must resume after it; brace-line spanEnds are re-scanned harmlessly
// (they filter as non-code) so chain siblings on the `} else if` line still
// match.
type anchor struct {
	node        *Node
	spanEnd     int
	braced      bool
	endConsumed bool
}

func collectAnchors(facts *scanner.SourceFacts, fn string, irFile *ir.File) []anchor {
	var out []anchor
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.Function != fn {
			continue
		}
		end := b.StartLine
		if b.BlockEnd > end {
			end = b.BlockEnd
		}
		n := &Node{Kind: KindBranch, Sub: string(b.Kind), Line: b.StartLine, EndLine: end, Cond: b.Cond}
		if b.Kind != scanner.BranchElse {
			e := pred.Parse(b.Cond)
			if res := defineResolver(irFile, fn, b.StartLine); res != nil {
				s := pred.Substitute(&e, res)
				e = s
			}
			n.Predicate = &e
		}
		braced := b.BlockStart != 0 && b.BlockEnd != 0
		out = append(out, anchor{node: n, spanEnd: end, braced: braced, endConsumed: !braced})
	}
	for i := range facts.Loops {
		l := &facts.Loops[i]
		if l.Function != fn {
			continue
		}
		end := l.StartLine
		for _, e := range []int{l.BlockEnd, l.WhileLine} {
			if e > end {
				end = e
			}
		}
		n := &Node{Kind: KindLoop, Sub: string(l.Kind), Line: l.StartLine, EndLine: end, Cond: l.Cond}
		if l.Cond != "" {
			e := pred.Parse(l.Cond)
			if res := defineResolver(irFile, fn, l.StartLine); res != nil {
				s := pred.Substitute(&e, res)
				e = s
			}
			n.Predicate = &e
		}
		braced := l.BlockStart != 0 && l.BlockEnd != 0
		consumed := !braced || (l.Kind == scanner.LoopDo && l.WhileLine >= l.BlockEnd)
		out = append(out, anchor{node: n, spanEnd: end, braced: braced, endConsumed: consumed})
	}
	for i := range facts.AllSQL {
		s := &facts.AllSQL[i]
		if s.Func != fn {
			continue
		}
		n := &Node{Kind: KindSQL, Sub: s.Kind.String(), Line: s.StartLine, EndLine: s.EndLine, Text: s.Normalized}
		out = append(out, anchor{node: n, spanEnd: s.EndLine, braced: false, endConsumed: true})
	}
	for i := range facts.Returns {
		r := &facts.Returns[i]
		if r.Func != fn {
			continue
		}
		out = append(out, anchor{node: &Node{Kind: KindReturn, Line: r.Line, EndLine: r.Line}, spanEnd: r.Line, braced: false, endConsumed: true})
	}
	for i := range facts.VarDecls {
		d := &facts.VarDecls[i]
		if d.Func != fn {
			continue
		}
		n := &Node{Kind: KindDecl, Line: d.Line, EndLine: d.Line}
		out = append(out, anchor{node: n, spanEnd: d.Line, braced: false, endConsumed: true})
	}
	return out
}

// nest builds the node tree for the line span [from, to]: braced anchors
// fully inside become children of the previous smaller enclosing anchor via
// recursion; gaps between anchors classify as stmt/unknown runs. Chain
// siblings (`} else if …` sharing the previous block's close line) start
// exactly at cur, so the cur bound is inclusive. An unbraced branch's C
// body is the single statement following its header — the statement run
// after it splits at the first ';'-terminated line, the first statement
// nesting under the branch (its span extends to cover the body), the rest
// staying a sibling run.
func nest(code []string, facts *scanner.SourceFacts, anchors []anchor, from, to, sig int, fn string, irFile *ir.File) []*Node {
	var nodes []*Node
	var unbraced *Node // the last unbraced branch pending its body statement
	cur := from
	for i := 0; i < len(anchors); i++ {
		a := anchors[i]
		if a.node.Line < cur || a.node.Line > to || a.spanEnd > to {
			continue
		}
		if run := residualRun(code, cur, a.node.Line-1, sig, fn, facts, irFile); run != nil {
			nodes = append(nodes, adoptUnbracedBody(code, run, unbraced, fn, facts, irFile)...)
			unbraced = nil
		}
		inner := a.node
		if a.braced && a.spanEnd > a.node.Line {
			// The block interior: anchors strictly inside this one.
			inner.Children = nest(code, facts, anchors[i+1:], a.node.Line+1, a.spanEnd-1, sig, fn, irFile)
		}
		annotate(inner, facts, fn, irFile)
		nodes = append(nodes, inner)
		if !a.braced && a.node.Kind == KindBranch {
			unbraced = inner
		}
		cur = a.spanEnd
		if a.endConsumed {
			cur++
		}
	}
	if run := residualRun(code, cur, to, sig, fn, facts, irFile); run != nil {
		nodes = append(nodes, adoptUnbracedBody(code, run, unbraced, fn, facts, irFile)...)
	}
	return nodes
}

// adoptUnbracedBody splits a statement run following an unbraced branch:
// the first ';'-terminated statement becomes the branch's body child (the
// branch span extends to cover it — C semantics: one statement); the
// remainder stays a sibling run. host == nil returns the run untouched.
func adoptUnbracedBody(code []string, run *Node, host *Node, fn string, facts *scanner.SourceFacts, irFile *ir.File) []*Node {
	if host == nil {
		return []*Node{run}
	}
	k := 0
	for l := run.Line; l <= run.EndLine && l-1 < len(code); l++ {
		if strings.HasSuffix(strings.TrimSpace(code[l-1]), ";") {
			k = l
			break
		}
	}
	if k == 0 {
		k = run.EndLine // no terminator in the run — the whole run is the body
	}
	var text []string
	for l := run.Line; l <= k && l-1 < len(code); l++ {
		if lineIsCode(code[l-1]) {
			text = append(text, strings.TrimSpace(code[l-1]))
		}
	}
	body := &Node{Kind: run.Kind, Sub: run.Sub, Line: run.Line, EndLine: k, Text: strings.Join(text, "\n")}
	annotate(body, facts, fn, irFile)
	host.Children = []*Node{body}
	host.EndLine = k
	if k >= run.EndLine {
		return nil
	}
	rest := residualRun(code, k+1, run.EndLine, 0, fn, facts, irFile)
	if rest == nil {
		return nil
	}
	return []*Node{rest}
}

// residualRun classifies the consecutive live-code lines [from, to] that no
// anchor covers: define/undef directive runs classify as elided
// declarations (G-DEF4 — constants the IR already records); other
// control-flow-led or preprocessor-led runs are loud unknown; an
// assignment/call run is a stmt; anything else is expr.
func residualRun(code []string, from, to, sig int, fn string, facts *scanner.SourceFacts, irFile *ir.File) *Node {
	var text []string
	var lines []int
	for l := from; l <= to && l-1 < len(code); l++ {
		if l == sig || !lineIsCode(code[l-1]) {
			continue
		}
		text = append(text, strings.TrimSpace(code[l-1]))
		lines = append(lines, l)
	}
	if len(text) == 0 {
		return nil
	}
	joined := strings.Join(text, "\n")
	// Define/undef runs are the IR's own constants (G-DEF4): classified,
	// never loud residue. Mixed runs (any other directive or statement)
	// keep the loud-unknown rule — the fault-tolerance contract.
	allDefine := true
	for _, l := range lines {
		if !isDefineLine(facts, l) {
			allDefine = false
			break
		}
	}
	if allDefine {
		return &Node{Kind: KindDecl, Sub: "define", Line: from, EndLine: to, Text: joined}
	}
	// Control-flow keywords and other preprocessor lines in a residual run
	// mean the structural parse failed to anchor them — loud unknown, never
	// a confidently-classified fake statement.
	for _, l := range text {
		if isControlResidue(l) {
			return &Node{Kind: KindUnknown, Line: from, EndLine: to, Text: joined}
		}
	}
	sub := SubExpr
	if hasTopLevelAssign(joined) {
		sub = SubAssign
	} else if _, isCall := leadingCall(joined); isCall {
		sub = SubCall
	}
	run := &Node{Kind: KindStmt, Sub: sub, Line: from, EndLine: to, Text: joined}
	annotate(run, facts, fn, irFile)
	return run
}

// isDefineLine reports whether the line carries a recorded #define or
// #undef directive — the only preprocessor lines the classify-as-decl rule
// absorbs (G-DEF4).
func isDefineLine(facts *scanner.SourceFacts, line int) bool {
	for i := range facts.Directives {
		d := &facts.Directives[i]
		if d.Line == line {
			return d.Kind == "define" || d.Kind == "undef"
		}
	}
	return false
}

// defineResolver returns the ident→literal resolver for one function line
// (G-DEF3): DefineAt lookups chained through bare-identifier values with a
// cycle guard; only pure literals resolve (DEF-D2 — compound values keep
// the ident in Go, the prompt's constants section carries them raw).
// nil irFile → nil resolver (no substitution).
func defineResolver(f *ir.File, fn string, line int) func(string) (string, bool) {
	if f == nil {
		return nil
	}
	return func(name string) (string, bool) {
		seen := map[string]bool{}
		cur := name
		for {
			if seen[cur] {
				return "", false
			}
			seen[cur] = true
			d, ok := f.DefineAt(fn, line, cur)
			if !ok {
				return "", false
			}
			v := strings.TrimSpace(d.Value)
			switch {
			case pred.IsLitText(v):
				return v, true
			case pred.IsBareIdent(v):
				cur = v
			default:
				return "", false
			}
		}
	}
}

// controlResidueWords are the keywords that, leading a residual line, mark
// unanchored control flow (the accuracy metric must count them as unknown).
var controlResidueWords = map[string]bool{
	"if": true, "else": true, "elseif": true, "while": true, "for": true,
	"do": true, "switch": true, "case": true, "default": true, "goto": true,
	"break": true, "continue": true, "return": true,
}

func isControlResidue(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return true
	}
	w := t
	if i := strings.IndexAny(t, " \t({;"); i >= 0 {
		w = t[:i]
	}
	return controlResidueWords[w]
}

// lineIsCode reports whether a comment-masked line carries live code.
func lineIsCode(masked string) bool {
	t := strings.TrimSpace(masked)
	if t == "" || t == "{" || t == "}" || t == "};" {
		return false
	}
	return true
}

// stripComments removes /* */ and // spans from one line (no multi-line
// carry: multi-line block comment interiors are dropped line-wise because
// their interior lines have no terminator).
func stripComments(line string) string {
	var sb strings.Builder
	for i := 0; i < len(line); i++ {
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '*' {
			for j := i + 2; j < len(line); j++ {
				if j+1 <= len(line) && line[j] == '*' && j+1 < len(line) && line[j+1] == '/' {
					i = j + 1
					goto next
				}
			}
			return sb.String() // unterminated on this line: rest is comment
		}
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
			return sb.String()
		}
		if line[i] == '"' || line[i] == '\'' {
			q := line[i]
			sb.WriteByte(line[i])
			for i++; i < len(line); i++ {
				sb.WriteByte(line[i])
				if line[i] == '\\' && i+1 < len(line) {
					i++
					sb.WriteByte(line[i])
					continue
				}
				if line[i] == q {
					break
				}
			}
			continue
		}
		sb.WriteByte(line[i])
	next:
	}
	return sb.String()
}

// hasTopLevelAssign reports whether the text contains an assignment '='
// outside ==, !=, <=, >= and outside parens/brackets.
func hasTopLevelAssign(text string) bool {
	return topLevelAssignIndex(text) >= 0
}

func isIdentStartByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentByte(b byte) bool {
	return isIdentStartByte(b) || (b >= '0' && b <= '9')
}

// annotate fills FmlOps/QueryIDs/Calls for a node's line span.
func annotate(n *Node, facts *scanner.SourceFacts, fn string, irFile *ir.File) {
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if call.Func != fn || call.Line < n.Line || call.Line > n.EndLine {
			continue
		}
		n.Calls = append(n.Calls, call.Name)
		if op, ok := ir.FmlOpOf(call, facts); ok {
			n.FmlOps = append(n.FmlOps, op)
		}
	}
	if irFile == nil {
		return
	}
	if len(irFile.Buffers) > 0 && len(n.FmlOps) > 0 {
		n.BufRoles = make(map[string]string, len(irFile.Buffers))
		for _, b := range irFile.Buffers {
			n.BufRoles[b.Name] = string(b.Role)
		}
	}
	for _, q := range irFile.Queries {
		if q.StartLine >= n.Line && q.StartLine <= n.EndLine {
			n.QueryIDs = append(n.QueryIDs, q.ID)
			continue
		}
		// Flattened cursor units start at DECLARE (outside the fetch loop);
		// attach them to the loop holding their FETCH span via cursor name.
		if q.CursorFlattened && q.CursorName != "" && fetchesCursor(facts, fn, n, q.CursorName) {
			n.QueryIDs = append(n.QueryIDs, q.ID)
		}
	}
}

// fetchesCursor reports whether any FETCH SQL span inside n's extent reads
// the named cursor (case-insensitive: the scanner uppercases cursor names,
// the IR keeps the DECLARE's raw casing).
func fetchesCursor(facts *scanner.SourceFacts, fn string, n *Node, cursor string) bool {
	for i := range facts.AllSQL {
		s := &facts.AllSQL[i]
		if s.Func != fn || s.Kind != scanner.SQLFetch || !ir.SameCursor(s.CursorName, cursor) {
			continue
		}
		if s.StartLine >= n.Line && s.StartLine <= n.EndLine {
			return true
		}
	}
	return false
}

// coverage rolls up the metric: live code lines in the body span vs lines
// classified by nodes vs residue (unknown lines). Each node contributes its
// span minus what its descendants already cover, so a parent's broad span
// never "classifies" lines an unknown child disowns.
func coverage(code []string, from, to, sig int, roots []*Node) Coverage {
	cov := Coverage{}
	codeSet := map[int]bool{}
	for l := from; l <= to && l-1 < len(code); l++ {
		if l != sig && lineIsCode(code[l-1]) {
			codeSet[l] = true
		}
	}
	cov.CodeLines = len(codeSet)
	covered := map[int]bool{}
	// walk returns all code lines inside n's span (self + descendants);
	// classified nodes claim the lines their descendants don't.
	var walk func(n *Node) map[int]bool
	walk = func(n *Node) map[int]bool {
		span := map[int]bool{}
		for l := n.Line; l <= n.EndLine && l-1 < len(code); l++ {
			if codeSet[l] {
				span[l] = true
			}
		}
		childAll := map[int]bool{}
		for _, c := range n.Children {
			for l := range walk(c) {
				childAll[l] = true
			}
		}
		if n.Kind != KindUnknown {
			for l := range span {
				if !childAll[l] {
					covered[l] = true
				}
			}
		}
		for l := range childAll {
			span[l] = true
		}
		return span
	}
	for _, n := range roots {
		walk(n)
	}
	cov.Classified = len(covered)
	cov.Unknown = cov.CodeLines - cov.Classified
	for l := from; l <= to && l-1 < len(code); l++ {
		if codeSet[l] && !covered[l] {
			cov.Residue = append(cov.Residue, l)
		}
	}
	return cov
}
