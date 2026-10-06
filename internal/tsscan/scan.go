package tsscan

import (
	"bytes"
	"os"

	"github.com/zema1/wasitter"
)

// ScanFile scans a Pro*C source file from disk.
func ScanFile(filePath string) (*SourceFacts, error) {
	src, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return ScanBytes(src, filePath)
}

// ScanBytes scans a Pro*C source from memory. The pipeline is: pre-scan the
// raw bytes (EXEC SQL regions, comment inventory, unbalanced accounting),
// mask the regions into same-length comments, parse the masked source with
// the real C grammar, then join both sides into SourceFacts.
func ScanBytes(src []byte, path string) (*SourceFacts, error) {
	pr := prescan(src)
	brokenSpans, brokenDirs := scanBrokenDefines(src, pr.lineStarts)
	pr.brokenDefine = brokenSpans
	pr.directives = brokenDirs
	masked := mask(src, pr.regions, pr.brokenDefine, pr.debris)
	// Unbalanced files (truncations, cut-and-paste fragments) synthesize
	// their missing closing braces so the grammar sees complete functions
	// instead of collapsing in error recovery. Positions of all real facts
	// are untouched (padding is pure `}` lines past EOF); the truncated
	// function's own body end still reads as 0 through unmatchedAt.
	if deficit := pr.openDeficit; deficit > 0 || pr.openComment {
		padded := make([]byte, 0, len(masked)+2*deficit+4)
		padded = append(padded, masked...)
		if pr.openComment {
			// close the unterminated comment so the padding lands in code
			padded = append(padded, '\n', '*', '/')
		}
		for i := 0; i < deficit; i++ {
			padded = append(padded, '\n', '}')
		}
		masked = padded
	}
	sess, err := getScanSession()
	if err != nil {
		return nil, err
	}
	tree, err := sess.parser.Parse(masked)
	if err != nil {
		closeSession(sess)
		return nil, err
	}

	w := &walker{
		src:       masked,
		facts:     &SourceFacts{Path: path, NumLines: pr.numLines},
		unmatched: pr.unmatched,
	}
	w.walk(tree.RootNode())
	w.facts.ParseErrors = collectParseErrors(tree.RootNode())
	tree.Close()
	putScanSession(sess)

	if len(pr.directives) > 0 {
		w.facts.Directives = append(w.facts.Directives, pr.directives...)
		sortDirectives(w.facts.Directives)
	}

	for _, rg := range pr.regions {
		if rg.unbalanced {
			continue
		}
		rg.stmt.Func = containingFunction(w.facts.Functions, rg.stmt.StartLine)
		w.facts.AllSQL = append(w.facts.AllSQL, rg.stmt)
		if rg.stmt.Kind.IsQuery() {
			w.facts.Queries = append(w.facts.Queries, rg.stmt)
		}
	}
	w.facts.Comments = pr.comments
	w.facts.Unbalanced = pr.unbalanced
	for i := range w.facts.Calls {
		if w.facts.Calls[i].IsTpCall {
			w.facts.TpCallCount++
		}
	}
	assignIfNesting(w.facts.Branches)
	assignLoopNesting(w.facts.Loops, w.facts.Branches)
	return w.facts, nil
}

// ScanFragment scans a source fragment (a half-written file, a copied slice)
// by wrapping it in a __fragment pseudo-function so control flow parses.
// Every recorded line number is rebased back onto the fragment's own lines
// (the wrapper costs two), and facts.Fragment is set.
func ScanFragment(src []byte, path string) (*SourceFacts, error) {
	wrapped := make([]byte, 0, len(src)+len("void __fragment(void)\n{\n}\n")+1)
	wrapped = append(wrapped, "void __fragment(void)\n{\n"...)
	wrapped = append(wrapped, src...)
	if len(wrapped) == 0 || wrapped[len(wrapped)-1] != '\n' {
		wrapped = append(wrapped, '\n')
	}
	wrapped = append(wrapped, "}\n"...)

	facts, err := ScanBytes(wrapped, path)
	if err != nil {
		return nil, err
	}
	rebaseLines(facts, -2)
	// Editor-style line count of the ORIGINAL fragment text.
	numLines := bytes.Count(src, []byte{'\n'})
	if len(src) > 0 && src[len(src)-1] != '\n' {
		numLines++
	}
	facts.NumLines = numLines
	facts.Fragment = true
	if len(facts.Functions) > 0 && facts.Functions[0].Name == "__fragment" {
		facts.Functions[0].StartLine = 1
		facts.Functions[0].BodyStartLine = 1
		facts.Functions[0].BodyEndLine = numLines
	}
	return facts, nil
}

// rebaseLines shifts every recorded line number by delta (columns unchanged;
// nesting is containment-based and therefore shift-invariant).
func rebaseLines(f *SourceFacts, delta int) {
	for i := range f.Functions {
		f.Functions[i].StartLine += delta
		f.Functions[i].BodyStartLine += delta
		if f.Functions[i].BodyEndLine > 0 {
			f.Functions[i].BodyEndLine += delta
		}
	}
	for i := range f.Calls {
		f.Calls[i].Line += delta
	}
	for i := range f.AllSQL {
		f.AllSQL[i].StartLine += delta
		f.AllSQL[i].EndLine += delta
	}
	for i := range f.Queries {
		f.Queries[i].StartLine += delta
		f.Queries[i].EndLine += delta
	}
	for i := range f.Branches {
		f.Branches[i].StartLine += delta
		if f.Branches[i].BlockStart > 0 {
			f.Branches[i].BlockStart += delta
			f.Branches[i].BlockEnd += delta
		}
	}
	for i := range f.Loops {
		f.Loops[i].StartLine += delta
		if f.Loops[i].BlockStart > 0 {
			f.Loops[i].BlockStart += delta
			f.Loops[i].BlockEnd += delta
		}
		if f.Loops[i].WhileLine > 0 {
			f.Loops[i].WhileLine += delta
		}
	}
	for i := range f.Returns {
		f.Returns[i].Line += delta
	}
	for i := range f.VarDecls {
		f.VarDecls[i].Line += delta
	}
	for i := range f.Comments {
		f.Comments[i].StartLine += delta
		f.Comments[i].EndLine += delta
	}
	for i := range f.Unbalanced {
		f.Unbalanced[i].StartLine += delta
	}
	for i := range f.ParseErrors {
		f.ParseErrors[i].Line += delta
	}
	for i := range f.Switches {
		f.Switches[i].StartLine += delta
		f.Switches[i].BlockStart += delta
		f.Switches[i].BlockEnd += delta
	}
}

// containingFunction names the function whose body spans the line ("" when
// the line belongs to no function body). A function with an unresolved body
// end owns everything until the next function starts.
func containingFunction(fns []FunctionDef, line int) string {
	best := -1
	for i := range fns {
		f := &fns[i]
		if f.BodyStartLine > 0 && f.BodyStartLine <= line && (best < 0 || fns[i].BodyStartLine >= fns[best].BodyStartLine) {
			if f.BodyEndLine > 0 && line > f.BodyEndLine {
				continue
			}
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return fns[best].Name
}

// assignIfNesting computes each branch's NestDepth: the number of braced
// if/else-if blocks strictly containing the branch's header position. Else
// arms never contribute and never nest (pinned convention: their NestDepth
// stays 0); unbraced arms create no nesting level.
func assignIfNesting(bs []Branch) {
	for i := range bs {
		if bs[i].Kind == BranchElse {
			continue
		}
		n := 0
		for j := range bs {
			if j == i || bs[j].BlockStart == 0 {
				continue
			}
			if bs[j].Kind != BranchIf && bs[j].Kind != BranchElseIf {
				continue
			}
			if containsSpan(bs[j].BlockStart, bs[j].BlockStartCol, bs[j].BlockEnd, bs[j].BlockEndCol,
				bs[i].StartLine, bs[i].StartCol) {
				n++
			}
		}
		bs[i].NestDepth = n
	}
}

// assignLoopNesting computes each loop's NestDepth: enclosing braced
// if/else-if blocks plus enclosing loops.
func assignLoopNesting(ls []Loop, bs []Branch) {
	for i := range ls {
		n := 0
		for j := range bs {
			b := &bs[j]
			if b.BlockStart == 0 || (b.Kind != BranchIf && b.Kind != BranchElseIf) {
				continue
			}
			if containsSpan(b.BlockStart, b.BlockStartCol, b.BlockEnd, b.BlockEndCol, ls[i].StartLine, ls[i].StartCol) {
				n++
			}
		}
		for j := range ls {
			if j == i || ls[j].BlockStart == 0 {
				continue
			}
			if containsSpan(ls[j].BlockStart, ls[j].BlockStartCol, ls[j].BlockEnd, ls[j].BlockEndCol, ls[i].StartLine, ls[i].StartCol) {
				n++
			}
		}
		ls[i].NestDepth = n
	}
}

// containsSpan reports whether position (l,c) lies inside the block span
// [start..end): start <= pos < end, compared as (line, column) tuples.
func containsSpan(sL, sC, eL, eC, l, c int) bool {
	beforeStart := sL > l || (sL == l && sC > c)
	atOrAfterEnd := l > eL || (l == eL && c >= eC)
	return !beforeStart && !atOrAfterEnd
}

// collectParseErrors surfaces every node the grammar could not recover from
// (fail-loud: a parse error is a recorded fact, never a silent drop).
func collectParseErrors(root wasitter.Node) []ParseError {
	if root.IsNull() {
		return nil
	}
	var out []ParseError
	var visit func(n wasitter.Node)
	visit = func(n wasitter.Node) {
		if n.IsError() {
			sp := n.StartPoint()
			out = append(out, ParseError{Kind: "error", Node: n.Type(), Line: int(sp.Row) + 1, Col: int(sp.Column) + 1})
		} else if n.IsMissing() {
			sp := n.StartPoint()
			out = append(out, ParseError{Kind: "missing", Node: n.Type(), Line: int(sp.Row) + 1, Col: int(sp.Column) + 1})
		}
		for i := 0; i < n.ChildCount(); i++ {
			visit(n.Child(i))
		}
	}
	visit(root)
	return out
}

// sortDirectives orders directives by line (stable) after merging facts
// recovered by the pre-scan.
func sortDirectives(ds []Directive) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j].Line < ds[j-1].Line; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}
