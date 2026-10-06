package tsscan

import (
	"strings"

	"github.com/zema1/wasitter"
)

func nodeText(src []byte, n wasitter.Node) string {
	if n.IsNull() {
		return ""
	}
	return string(src[n.StartByte():n.EndByte()])
}

type walker struct {
	src       []byte
	facts     *SourceFacts
	depth     int
	fn        string
	inBody    bool
	unmatched []pos
}

func childByKind(n wasitter.Node, kinds ...string) wasitter.Node {
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		for _, k := range kinds {
			if c.Type() == k {
				return c
			}
		}
	}
	return wasitter.Node{}
}

// typeText joins the leading type-ish children of a declaration (struct
// bodies are trimmed away).
func typeText(src []byte, n wasitter.Node, stopAt int) string {
	parts := make([]string, 0, 2)
	for i := 0; i < stopAt; i++ {
		c := n.Child(i)
		switch c.Type() {
		case "primitive_type", "type_identifier", "sized_type_specifier",
			"struct_specifier", "union_specifier", "enum_specifier":
			t := nodeText(src, c)
			if idx := strings.IndexByte(t, '{'); idx >= 0 {
				t = t[:idx]
			}
			parts = append(parts, strings.TrimSpace(t))
		}
	}
	return strings.Join(parts, " ")
}

var declaratorKinds = map[string]bool{
	"identifier": true, "init_declarator": true, "pointer_declarator": true,
	"array_declarator": true, "function_declarator": true, "parenthesized_declarator": true,
	"attributed_declarator": true,
}

// walkDeclarator extracts (name, array, nameNode) from a declarator chain.
func walkDeclarator(src []byte, n wasitter.Node) (string, bool, wasitter.Node) {
	switch n.Type() {
	case "identifier":
		return nodeText(src, n), false, n
	case "init_declarator", "pointer_declarator", "parenthesized_declarator", "attributed_declarator":
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if declaratorKinds[c.Type()] {
				return walkDeclarator(src, c)
			}
		}
	case "array_declarator":
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if declaratorKinds[c.Type()] {
				name, _, idn := walkDeclarator(src, c)
				return name, true, idn
			}
		}
	case "function_declarator":
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if c.Type() == "parameter_list" {
				continue
			}
			if declaratorKinds[c.Type()] {
				return walkDeclarator(src, c)
			}
		}
	}
	return "", false, wasitter.Node{}
}

// recordDeclaration records one declaration's VarDecls (one per declarator).
// Works for both statement declarations and parameter declarations.
func (w *walker) recordDeclaration(n wasitter.Node) {
	stop, typeDone := 0, false
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if declaratorKinds[c.Type()] {
			stop, typeDone = i, true
			break
		}
		switch c.Type() {
		case "struct_specifier", "enum_specifier", "union_specifier":
			if !childByKind(c, "field_declaration_list", "enumerator_list").IsNull() {
				stop, typeDone = i+1, true
			}
		}
		if typeDone {
			break
		}
	}
	if !typeDone {
		return
	}
	base := typeText(w.src, n, stop)
	for i := stop; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Type() == "function_declarator" {
			// a prototype declaration: its parameters still type host vars
			w.recordParams(c)
			continue
		}
		if hasPointerDeclarator(c) {
			continue // pointer declarations are not VarDecls (pinned vocabulary)
		}
		name, array, idn := walkDeclarator(w.src, c)
		if name == "" || idn.IsNull() {
			continue
		}
		sp := idn.StartPoint()
		w.facts.VarDecls = append(w.facts.VarDecls, VarDecl{
			Type: base, Name: name,
			Line: int(sp.Row) + 1, Col: int(sp.Column) + 1,
			Array: array, Func: w.fnCtx(),
		})
	}
}

// hasPointerDeclarator reports whether the declarator chain dereferences.
func hasPointerDeclarator(n wasitter.Node) bool {
	if n.IsNull() {
		return false
	}
	if n.Type() == "pointer_declarator" {
		return true
	}
	for i := 0; i < n.ChildCount(); i++ {
		if hasPointerDeclarator(n.Child(i)) {
			return true
		}
	}
	return false
}

// fnCtx is the function context: file-scope declarations and parameters
// record with "".
func (w *walker) fnCtx() string {
	if w.inBody {
		return w.fn
	}
	return ""
}

// walk dispatches on node kind; every handler is responsible for recursing
// into exactly the children it owns.
func (w *walker) walk(n wasitter.Node) {
	if n.IsNull() {
		return
	}
	switch n.Type() {
	case "function_definition":
		w.walkFunctionDefinition(n)
	case "compound_statement":
		w.depth++
		w.walkChildren(n)
		w.depth--
	case "declaration":
		w.recordDeclaration(n)
		w.walkChildrenFrom(n, firstDeclaratorIndex(n))
	case "if_statement":
		w.walkIf(n, BranchIf)
	case "while_statement":
		w.walkWhile(n)
	case "for_statement":
		w.walkFor(n)
	case "do_statement":
		w.walkDo(n)
	case "ERROR":
		w.recoverTailLessDo(n)
		w.walkChildren(n)
	case "return_statement":
		sp := n.StartPoint()
		w.facts.Returns = append(w.facts.Returns, Return{
			Line: int(sp.Row) + 1, Col: int(sp.Column) + 1, Func: w.fnCtx(),
		})
		w.walkChildren(n)
	case "call_expression":
		w.recordCall(n)
		w.walkChildren(n)
	case "preproc_include":
		w.recordInclude(n)
	case "preproc_def", "preproc_function_def", "preproc_call", "preproc_if",
		"preproc_ifdef", "preproc_else", "preproc_elif", "preproc_endif":
		w.recordDirective(n)
		w.walkChildren(n)
	default:
		w.walkChildren(n)
	}
}

func firstDeclaratorIndex(n wasitter.Node) int {
	for i := 0; i < n.ChildCount(); i++ {
		if declaratorKinds[n.Child(i).Type()] {
			return i
		}
	}
	return int(n.ChildCount())
}

// nextNonComment returns the first child at or after index i that is not a
// comment node (comments are extra named children in the tree).
func nextNonComment(n wasitter.Node, i int) wasitter.Node {
	for ; i < int(n.ChildCount()); i++ {
		c := n.Child(i)
		if c.Type() != "comment" {
			return c
		}
	}
	return wasitter.Node{}
}

func (w *walker) walkChildren(n wasitter.Node) {
	for i := 0; i < n.ChildCount(); i++ {
		w.walk(n.Child(i))
	}
}

func (w *walker) walkChildrenFrom(n wasitter.Node, from int) {
	for i := from; i < int(n.ChildCount()); i++ {
		w.walk(n.Child(i))
	}
}

// walkFunctionDefinition records the function and its parameters, then the
// body at brace depth 1 with the function context active. BodyEndLine is 0
// when the body's opening brace never closes (unbalanced file).
func (w *walker) walkFunctionDefinition(n wasitter.Node) {
	var decl wasitter.Node
	var typeStop int
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Type() == "function_declarator" {
			decl, typeStop = c, i
			break
		}
	}
	if decl.IsNull() {
		w.walkChildren(n)
		return
	}
	retType := typeText(w.src, n, typeStop)
	name, _, idn := walkDeclarator(w.src, decl)
	if name == "" || idn.IsNull() {
		w.walkChildren(n)
		return
	}
	sp := idn.StartPoint()
	fnDef := FunctionDef{
		Name: name, ReturnType: retType,
		StartLine: int(sp.Row) + 1, Col: int(sp.Column) + 1,
	}
	w.facts.Functions = append(w.facts.Functions, fnDef)

	prevFn, prevIn := w.fn, w.inBody
	w.recordParams(decl)
	body := childByKind(n, "compound_statement")
	w.fn, w.inBody = name, true
	if !body.IsNull() {
		fnDef.BodyStartLine = int(body.StartPoint().Row) + 1
		if w.unmatchedAt(body.StartPoint()) {
			fnDef.BodyEndLine = 0
		} else {
			fnDef.BodyEndLine = int(body.EndPoint().Row) + 1
		}
		w.facts.Functions[len(w.facts.Functions)-1] = fnDef
		w.walk(body)
	}
	w.fn, w.inBody = prevFn, prevIn
}

// recordParams records parameter declarations in the Params side-channel
// (the pinned vocabulary keeps parameters out of VarDecls but consumers
// still need them for host-variable typing).
func (w *walker) recordParams(decl wasitter.Node) {
	pl := childByKind(decl, "parameter_list")
	if pl.IsNull() {
		return
	}
	for i := 0; i < pl.NamedChildCount(); i++ {
		if pd := pl.NamedChild(i); pd.Type() == "parameter_declaration" {
			w.recordParamDecl(pd)
		}
	}
}

func (w *walker) recordParamDecl(n wasitter.Node) {
	stop, typeDone := 0, false
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if declaratorKinds[c.Type()] {
			stop, typeDone = i, true
			break
		}
	}
	if !typeDone {
		return
	}
	base := typeText(w.src, n, stop)
	for i := stop; i < n.ChildCount(); i++ {
		// pointer parameters keep their base type (the pointer itself is
		// dropped, matching the pinned host-variable typing)
		name, array, idn := walkDeclarator(w.src, n.Child(i))
		if name == "" || idn.IsNull() {
			continue
		}
		sp := idn.StartPoint()
		w.facts.Params = append(w.facts.Params, VarDecl{
			Type: base, Name: name,
			Line: int(sp.Row) + 1, Col: int(sp.Column) + 1,
			Array: array,
		})
	}
}

func (w *walker) unmatchedAt(p wasitter.Point) bool {
	for _, up := range w.unmatched {
		if up.line == int(p.Row)+1 && up.col == int(p.Column)+1 {
			return true
		}
	}
	return false
}

func condNodeOf(n wasitter.Node) wasitter.Node {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c.Type() == "parenthesized_expression" {
			return c
		}
	}
	return wasitter.Node{}
}

// condText is the condition text between the outer parens, whitespace-collapsed.
func condText(src []byte, n wasitter.Node) string {
	if n.IsNull() || n.ChildCount() < 2 {
		return ""
	}
	return collapseWS(string(src[n.StartByte()+1 : n.EndByte()-1]))
}

// walkIf records an if/else-if arm and flattens its else-if/else chain so
// chain members stay siblings.
func (w *walker) walkIf(n wasitter.Node, kind BranchKind) {
	condIdx := -1
	for i := 0; i < n.ChildCount(); i++ {
		if n.Child(i).Type() == "parenthesized_expression" {
			condIdx = int(i)
			break
		}
	}
	var condNode wasitter.Node
	if condIdx >= 0 {
		condNode = n.Child(condIdx)
	}
	br := Branch{Kind: kind, Cond: condText(w.src, condNode)}
	sp := n.StartPoint()
	br.StartLine, br.StartCol = int(sp.Row)+1, int(sp.Column)+1
	if consequence := nextNonComment(n, condIdx+1); !consequence.IsNull() && consequence.Type() == "compound_statement" {
		bsp, bep := consequence.StartPoint(), consequence.EndPoint()
		br.BlockStart, br.BlockStartCol = int(bsp.Row)+1, int(bsp.Column)+1
		br.BlockEnd, br.BlockEndCol = int(bep.Row)+1, int(bep.Column)
	}
	br.Depth = w.depth
	br.Function = w.fnCtx()
	w.facts.Branches = append(w.facts.Branches, br)

	if !condNode.IsNull() {
		w.walk(condNode) // calls inside the condition are recorded
	}
	// consequence: the statement between the condition and the else clause
	if consequence := nextNonComment(n, condIdx+1); !consequence.IsNull() && consequence.Type() != "else_clause" {
		w.walk(consequence)
	}

	var elseTok, alt wasitter.Node
	if ec := childByKind(n, "else_clause"); !ec.IsNull() {
		for i := 0; i < ec.ChildCount(); i++ {
			c := ec.Child(i)
			switch c.Type() {
			case "else":
				elseTok = c
			case "comment":
				// comments ride inside the else clause; skip them
			default:
				if alt.IsNull() {
					alt = c
				}
			}
		}
	}
	if alt.IsNull() {
		return
	}
	switch alt.Type() {
	case "if_statement":
		w.walkIf(alt, BranchElseIf)
	default:
		els := Branch{Kind: BranchElse, Depth: w.depth, Function: w.fnCtx()}
		if !elseTok.IsNull() {
			esp := elseTok.StartPoint()
			els.StartLine, els.StartCol = int(esp.Row)+1, int(esp.Column)+1
		}
		if alt.Type() == "compound_statement" {
			asp, aep := alt.StartPoint(), alt.EndPoint()
			els.BlockStart, els.BlockStartCol = int(asp.Row)+1, int(asp.Column)+1
			els.BlockEnd, els.BlockEndCol = int(aep.Row)+1, int(aep.Column)
		}
		w.facts.Branches = append(w.facts.Branches, els)
		w.walk(alt)
	}
}

// walkWhile records a while loop.
func (w *walker) walkWhile(n wasitter.Node) {
	sp := n.StartPoint()
	body := childByKind(n, "compound_statement")
	lp := Loop{
		Kind:      LoopWhile,
		StartLine: int(sp.Row) + 1, StartCol: int(sp.Column) + 1,
		Depth: w.depth, Function: w.fnCtx(),
	}
	if cond := condNodeOf(n); !cond.IsNull() {
		lp.Cond = condText(w.src, cond)
	}
	if !body.IsNull() {
		bsp, bep := body.StartPoint(), body.EndPoint()
		lp.BlockStart, lp.BlockStartCol = int(bsp.Row)+1, int(bsp.Column)+1
		lp.BlockEnd, lp.BlockEndCol = int(bep.Row)+1, int(bep.Column)
	}
	w.facts.Loops = append(w.facts.Loops, lp)
	w.walkChildren(n) // condition calls and body
}

// walkFor records a for loop; Cond is the full header between the parens
// with all whitespace stripped.
func (w *walker) walkFor(n wasitter.Node) {
	sp := n.StartPoint()
	body := childByKind(n, "compound_statement")
	lp := Loop{
		Kind:      LoopFor,
		StartLine: int(sp.Row) + 1, StartCol: int(sp.Column) + 1,
		Depth: w.depth, Function: w.fnCtx(),
	}
	if open, close := childToken(n, "(", firstPick), childToken(n, ")", lastPick); !open.IsNull() && !close.IsNull() {
		lp.Cond = stripWS(string(w.src[open.EndByte():close.StartByte()]))
	}
	if !body.IsNull() {
		bsp, bep := body.StartPoint(), body.EndPoint()
		lp.BlockStart, lp.BlockStartCol = int(bsp.Row)+1, int(bsp.Column)+1
		lp.BlockEnd, lp.BlockEndCol = int(bep.Row)+1, int(bep.Column)
	}
	w.facts.Loops = append(w.facts.Loops, lp)
	w.walkChildren(n)
}

// walkDo records a do-while as one loop whose tail while() fills Cond and
// WhileLine.
func (w *walker) walkDo(n wasitter.Node) {
	sp := n.StartPoint()
	body := childByKind(n, "compound_statement")
	lp := Loop{
		Kind:      LoopDo,
		StartLine: int(sp.Row) + 1, StartCol: int(sp.Column) + 1,
		Depth: w.depth, Function: w.fnCtx(),
	}
	if cond := condNodeOf(n); !cond.IsNull() {
		lp.Cond = condText(w.src, cond)
		csp := cond.StartPoint()
		lp.WhileLine = int(csp.Row) + 1
	}
	if !body.IsNull() {
		bsp, bep := body.StartPoint(), body.EndPoint()
		lp.BlockStart, lp.BlockStartCol = int(bsp.Row)+1, int(bsp.Column)+1
		lp.BlockEnd, lp.BlockEndCol = int(bep.Row)+1, int(bep.Column)
	}
	w.facts.Loops = append(w.facts.Loops, lp)
	w.walkChildren(n)
}

// recoverTailLessDo repairs the tail-less do the grammar cannot parse
// (pinned behavior): an ERROR node holding a bare `do` token
// followed by its compound body still records one do loop — the block
// extents are the body's, the tail condition stays empty.
func (w *walker) recoverTailLessDo(n wasitter.Node) {
	var doTok wasitter.Node
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Type() == "do" {
			doTok = c
			break
		}
	}
	if doTok.IsNull() {
		return
	}
	body := firstCompoundAfterByte(n, doTok.StartByte())
	if body.IsNull() {
		return
	}
	sp := doTok.StartPoint()
	lp := Loop{
		Kind: LoopDo, StartLine: int(sp.Row) + 1, StartCol: int(sp.Column) + 1,
		Depth: w.depth, Function: w.fnCtx(),
	}
	bsp, bep := body.StartPoint(), body.EndPoint()
	lp.BlockStart, lp.BlockStartCol = int(bsp.Row)+1, int(bsp.Column)+1
	lp.BlockEnd, lp.BlockEndCol = int(bep.Row)+1, int(bep.Column)
	w.facts.Loops = append(w.facts.Loops, lp)
}

// firstCompoundAfterByte finds the nearest compound_statement at or under n
// starting after byte offset from (the do body is a sibling of the `do`
// token, often one error-recovery level down).
func firstCompoundAfterByte(n wasitter.Node, from uint32) wasitter.Node {
	if n.StartByte() > from && n.Type() == "compound_statement" {
		return n
	}
	for i := 0; i < n.ChildCount(); i++ {
		if c := firstCompoundAfterByte(n.Child(i), from); !c.IsNull() {
			return c
		}
	}
	return wasitter.Node{}
}

func childToken(n wasitter.Node, tok string, pick func(a, b wasitter.Node) wasitter.Node) wasitter.Node {
	var found wasitter.Node
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() && c.Type() == tok {
			found = pick(found, c)
		}
	}
	return found
}

func firstPick(a, b wasitter.Node) wasitter.Node {
	if a.IsNull() {
		return b
	}
	return a
}

func lastPick(a, b wasitter.Node) wasitter.Node { return b }

func stripWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if !isSpaceByte(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// recordCall records one call site; nested calls are covered by the generic
// recursion into the arguments.
func (w *walker) recordCall(n wasitter.Node) {
	fnNode := childByKind(n, "identifier")
	if fnNode.IsNull() {
		return
	}
	name := nodeText(w.src, fnNode)
	args := ""
	if argList := childByKind(n, "argument_list"); !argList.IsNull() && argList.ChildCount() >= 2 {
		args = string(w.src[argList.StartByte()+1 : argList.EndByte()-1])
	}
	sp := fnNode.StartPoint()
	w.facts.Calls = append(w.facts.Calls, FunctionCall{
		Name: name,
		Line: int(sp.Row) + 1, Col: int(sp.Column) + 1,
		Args:      args,
		IsTpCall:  IsTpCallName(name),
		IsFnPref:  strings.HasPrefix(name, "fn_"),
		IsChkPref: strings.HasPrefix(name, "chk_"),
		Func:      w.fnCtx(),
	})
}

func (w *walker) recordInclude(n wasitter.Node) {
	arg := ""
	isSystem := false
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Type() == "system_lib_string" {
			arg, isSystem = nodeText(w.src, c), true
			break
		}
		if c.Type() == "string_literal" {
			arg = nodeText(w.src, c)
			break
		}
	}
	sp := n.StartPoint()
	w.facts.Directives = append(w.facts.Directives, Directive{
		Kind: "include", Arg: arg, Line: int(sp.Row) + 1, IsHeader: true, IsSystem: isSystem,
	})
}

// recordDirective records raw preprocessor facts. #define arguments are the
// reconstructed rest-of-line text ("NAME VALUE" / "NAME(params) VALUE");
// every other directive keeps its raw spelling and is never interpreted.
func (w *walker) recordDirective(n wasitter.Node) {
	sp := n.StartPoint()
	line := int(sp.Row) + 1
	switch n.Type() {
	case "preproc_def", "preproc_function_def":
		// the raw rest-of-line after "#define" is the fact ("NAME VALUE" /
		// "NAME(params) VALUE"), exactly as pinned in the goldens
		text := nodeText(w.src, n)
		if idx := strings.Index(text, "define"); idx >= 0 {
			text = text[idx+len("define"):]
		}
		if arg := strings.Trim(stripTrailingComment(text), " \t\r\n"); arg != "" {
			w.facts.Directives = append(w.facts.Directives, Directive{Kind: "define", Arg: arg, Line: line})
		}
	case "preproc_call":
		kind, arg := "", ""
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if i == 0 {
				kind = strings.TrimPrefix(nodeText(w.src, c), "#")
				continue
			}
			if c.Type() == "preproc_arg" {
				arg = strings.TrimSpace(nodeText(w.src, c))
			}
		}
		if kind != "" {
			w.facts.Directives = append(w.facts.Directives, Directive{Kind: kind, Arg: arg, Line: line})
		}
	case "preproc_ifdef", "preproc_ifndef", "preproc_if":
		kind := map[string]string{"preproc_ifdef": "ifdef", "preproc_ifndef": "ifndef", "preproc_if": "if"}[n.Type()]
		arg := ""
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c.Type() == "identifier" {
				arg = nodeText(w.src, c)
				break
			}
		}
		w.facts.Directives = append(w.facts.Directives, Directive{Kind: kind, Arg: arg, Line: line})
	case "preproc_else", "preproc_elif", "preproc_endif":
		kind := map[string]string{"preproc_else": "else", "preproc_elif": "elif", "preproc_endif": "endif"}[n.Type()]
		w.facts.Directives = append(w.facts.Directives, Directive{Kind: kind, Line: line})
	}
}
