package flow

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/pred"
)

// Resolver supplies plan-level naming to the renderer (FLW-D5). When nil,
// or when a lookup misses, the renderer emits deterministic placeholders —
// the draft stays honest about what only the plan/LLM step can name.
type Resolver interface {
	// StoreCall returns the Go call expression for a query's store method,
	// e.g. `s.store.GetItemHistory(c, request.CompCd)`.
	StoreCall(queryID string) (expr string, ok bool)
	// RowType returns the Go row type a multi-row query yields,
	// e.g. `*models.ItemHistoryRow`.
	RowType(queryID string) (typ string, ok bool)
}

// Render is the deterministic transpilation draft: a conservative Go
// skeleton with explicit TODO residue. Elided counts dropped Tuxedo
// constructs; TODOs list the gaps the AI-enhance step must fill.
// Conditions is the transparency census: every legacy if/elseif the draft
// walk renders (after the same elisions), so the seam gate can require the
// LLM body to carry each condition and its effects.
type Render struct {
	Body       string
	Elided     int
	TODOs      []string
	Conditions []CensusCond
}

// droppedCallees are Tuxedo/FML buffer-management, Pro*C helper, and
// logging calls the Go controller never reproduces (the method template and
// store layer own their effect).
var droppedCallees = map[string]bool{
	"tpalloc": true, "tprealloc": true, "tpfree": true,
	"MEMSET": true, "SETNULL": true, "SETLEN": true,
	"Funused32": true, "Fsizeof32": true, "Fneeded32": true,
	"userlog": true, "errlog": true,
	"strcpy": true, "INITDBGLVL": true, "DEBUG_MSG_LVL": true,
}

// RenderSpan renders the nodes whose span sits inside [from, to] — one
// endpoint's condition slice — at the given indent level (1-based tabs).
func RenderSpan(tree *Tree, res Resolver, from, to, indent int) Render {
	r := &renderer{res: res, indent: indent, introduced: map[string]bool{}}
	r.nodes(tree.Root, from, to)
	if r.elided > 0 {
		r.linef("// %d C declarations/buffer-management/logging constructs elided", r.elided)
	}
	return Render{Body: r.sb.String(), Elided: r.elided, TODOs: r.todos, Conditions: r.conds}
}

type renderer struct {
	res        Resolver
	sb         strings.Builder
	indent     int
	elided     int
	todos      []string
	conds      []CensusCond
	fetchDepth int

	// introduced records every name this render has already declared with
	// `:=`. It is flat across the whole render, not per-scope, on purpose —
	// see assignOp. A later assignment to a name in here is `=`, which is
	// what stops a nested-scope assignment from silently shadowing.
	introduced map[string]bool
}

func (r *renderer) linef(format string, args ...any) {
	r.sb.WriteString(strings.Repeat("\t", r.indent))
	fmt.Fprintf(&r.sb, format, args...)
	r.sb.WriteString("\n")
}

func (r *renderer) todo(line int, what string) {
	r.todos = append(r.todos, fmt.Sprintf("line %d: %s", line, what))
	r.linef("// TODO(line %d): %s", line, what)
}

// nodes renders a sibling list; an if/elseif whose next sibling is an
// elseif/else skips its closing brace (the chain's last node closes it).
func (r *renderer) nodes(ns []*Node, from, to int) {
	for i, n := range ns {
		if n.EndLine < from || n.Line > to {
			continue
		}
		chainNext := i+1 < len(ns) && (ns[i+1].Sub == "elseif" || ns[i+1].Sub == "else")
		r.node(n, from, to, chainNext)
	}
}

func (r *renderer) node(n *Node, from, to int, chainNext bool) {
	switch n.Kind {
	case KindBranch:
		r.branch(n, from, to, chainNext)
	case KindLoop:
		r.loop(n)
	case KindSQL:
		r.sql(n)
	case KindReturn:
		r.elided++ // the method template owns returns
	case KindDecl:
		r.elided++ // C declarations → the Go method template owns locals
	case KindStmt:
		r.stmt(n)
	case KindUnknown:
		r.todo(n.Line, n.Text)
	}
}

func (r *renderer) branch(n *Node, from, to int, chainNext bool) {
	if isRequestGuard(n) {
		// Collapses in Go: the field is a request struct member and the
		// error path is the method's own error return.
		r.elided++
		return
	}
	if isDebugIf(n) {
		r.elided++
		return
	}
	// Transparency census: collected on the same walk that emits the
	// headers, after the same elisions — zero divergence risk with the
	// draft. Plumbing predicates filter here (Go error handling owns
	// them); non-transpilable call conditions still census (the TODO
	// carries them, the LLM must implement the branch).
	if n.Sub != "else" && !censusSkipped(n, r.fetchDepth) {
		r.conds = append(r.conds, CensusCond{
			Line:     n.Line,
			Cond:     n.Cond,
			Skeleton: Skeleton(n.Predicate),
			Idents:   ExprIdents(n.Predicate),
			Effects:  censusEffects(n.Children),
			HasElse:  chainNext,
		})
	}
	switch n.Sub {
	case "else":
		r.linef("} else {")
		r.children(n, from, to)
		if !chainNext {
			r.linef("}")
		}
		return
	case "elseif":
		if strings.TrimSpace(exprGo(n.Predicate)) == "" {
			r.todo(n.Line, "condition not transpilable: "+n.Cond)
			return
		}
		r.linef("} else if %s {", exprGo(n.Predicate))
		r.children(n, from, to)
		if !chainNext {
			r.linef("}")
		}
		return
	}
	if strings.TrimSpace(exprGo(n.Predicate)) == "" {
		r.todo(n.Line, "condition not transpilable: "+n.Cond)
		return
	}
	r.linef("if %s {", exprGo(n.Predicate))
	r.children(n, from, to)
	if !chainNext {
		r.linef("}")
	}
}

func (r *renderer) children(n *Node, from, to int) {
	saved := r.indent
	r.indent++
	r.nodes(n.Children, from, to)
	r.indent = saved
}

func (r *renderer) loop(n *Node) {
	if isErrOpLoop(n) {
		r.elided++ // Go error handling covers the Fadd-result checks
		return
	}
	if isFetchLoop(n) && len(n.QueryIDs) > 0 {
		qid := n.QueryIDs[0]
		call, ok := "", false
		if r.res != nil {
			call, ok = r.res.StoreCall(qid)
		}
		if !ok {
			call = "s.store./* TODO name for " + qid + " */"
			r.todo(n.Line, "store method name for query "+qid)
		}
		row, okRow := "", false
		if r.res != nil {
			row, okRow = r.res.RowType(qid)
		}
		if !okRow {
			row = "/* TODO row type for " + qid + " */"
		}
		r.linef("rows, err := %s", call)
		r.linef("if err != nil {")
		r.linef("\treturn nil, err")
		r.linef("}")
		r.linef("for _, row := range rows { // []%s", row)
		r.fetchDepth++
		for _, c := range n.Children {
			if c.Kind == KindSQL || (c.Kind == KindBranch && strings.Contains(c.Cond, "SQLCODE")) {
				continue // consumed by the fetch-then-iterate shape
			}
			r.node(c, 0, 1<<30, false)
		}
		r.fetchDepth--
		r.linef("}")
		return
	}
	switch n.Sub {
	case "do":
		r.linef("for {")
		r.children(n, 0, 1<<30)
		if n.Cond != "" && n.Predicate != nil {
			r.linef("if !(%s) {", exprGo(n.Predicate))
			r.linef("\tbreak")
			r.linef("}")
		} else {
			r.todo(n.EndLine, "do-while condition not transpilable: "+n.Cond)
		}
		r.linef("}")
	default:
		cond := exprGo(n.Predicate)
		if cond == "1" || strings.EqualFold(strings.TrimSpace(n.Cond), "1") {
			cond = "" // while(1) → bare for
		}
		if n.Cond != "" && cond == "" {
			r.todo(n.Line, "loop condition not transpilable: "+n.Cond)
		}
		r.linef("for %s {", cond)
		r.children(n, 0, 1<<30)
		r.linef("}")
	}
}

func (r *renderer) sql(n *Node) {
	switch n.Sub {
	case "OPEN", "CLOSE", "DECLARE_CURSOR", "DECLARE_SECTION", "INCLUDE":
		r.elided++ // cursor plumbing — the store layer owns it
		return
	}
	if len(n.QueryIDs) > 0 {
		r.todo(n.Line, fmt.Sprintf("standalone SQL (%s) → store call for %s", n.Sub, strings.Join(n.QueryIDs, ", ")))
		return
	}
	r.todo(n.Line, "unlinked SQL: "+n.Text)
}

func (r *renderer) stmt(n *Node) {
	// Response fan-out: several Fadd32 ops into one buffer become response
	// mapping — the AI-enhance step fills the struct literal.
	for buf, fields := range fanoutAdds(n) {
		if len(fields) >= 2 {
			r.todo(n.Line, fmt.Sprintf("map %s to the response struct (%s)", buf, strings.Join(fields, ", ")))
			return
		}
	}
	// Per-statement processing: residual runs may carry several C
	// statements, each classified independently.
	for _, line := range strings.Split(n.Text, "\n") {
		r.stmtLine(strings.TrimSpace(line), n)
	}
}

// stmtLine renders one C statement: dropped constructs elide, ATMI exits
// map to Go returns, external fn calls and FML ops become TODOs, plain
// assignments transpile, everything else stays honest residue.
func (r *renderer) stmtLine(line string, n *Node) {
	if line == "" || line == ";" {
		return
	}
	name, isCall := leadingCall(line)
	if isCall {
		switch {
		case name == "tpreturn":
			if strings.Contains(line, "TPSUCCESS") {
				r.linef("return data, nil")
			} else {
				r.linef("return nil, err%s", r.codeComment(n))
			}
			return
		case droppedCallees[name]:
			r.elided++
			return
		case strings.HasPrefix(name, "fn_") || strings.HasPrefix(name, "chk_"):
			r.todo(n.Line, "external fn call (plan resolves): "+firstLine(line))
			return
		case name == "Fadd32" || name == "Fget32":
			r.todo(n.Line, "FML op → request/response mapping: "+firstLine(line)+r.codeComment(n))
			return
		}
	}
	if hasTopLevelAssign(line) {
		lhs, rhs := splitAssign(line)
		r.linef("%s %s %s", lhs, r.assignOp(lhs), goLitRHS(rhs))
		return
	}
	if isCall {
		r.todo(n.Line, "call: "+firstLine(line))
		return
	}
	r.todo(n.Line, firstLine(line))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// assignOp picks the Go assignment operator for one C assignment.
//
// The renderer's design elides the C declaration (KindDecl: "the Go method
// template owns locals") and lets the variable's first assignment introduce
// it, so `:=` is correct for exactly one assignment per name. Emitting it for
// all of them was wrong in two distinct ways, and the second is the one that
// matters:
//
//   - a second assignment in the SAME scope is a redeclaration — a compile
//     error, loud and harmless.
//   - a second assignment in a NESTED scope is not an error at all. Go
//     happily declares a new variable that shadows the outer one, so
//     `d_tot_amt := d_tot_amt + val` inside the CSH branch compiles, runs,
//     and throws the accumulation away. The risk-profile totals would be
//     permanently zero, and nothing anywhere would say so.
//
// That second case is the reason this is a correctness fix and not a
// cosmetic one, and the reason the tracking below is flat across the whole
// render rather than per-scope: once a name exists, no later assignment may
// re-declare it. Being flat can only ever turn a shadow into a compile error,
// never the reverse.
//
// A non-identifier lhs is never `:=`. Go cannot declare through a deref
// (`*out = 0` is an assignment) or through a field or index
// (`row.Field = x` likewise), so those are always `=`.
func (r *renderer) assignOp(lhs string) string {
	name := strings.TrimSpace(lhs)
	if !isGoIdent(name) {
		return "="
	}
	if r.introduced[name] {
		return "="
	}
	r.introduced[name] = true
	return ":="
}

// codeComment renders the legacy error codes the node's FML ops carry
// (PRD-2026-09-10 defines pass, G-DEF6): the draft retains the codes the
// legacy runtime maps to real messages. Empty when no op carries one.
func (r *renderer) codeComment(n *Node) string {
	var codes []string
	seen := map[string]bool{}
	for _, op := range n.FmlOps {
		if op.Code == "" || seen[op.Code] {
			continue
		}
		seen[op.Code] = true
		codes = append(codes, op.Code)
	}
	if len(codes) == 0 {
		return ""
	}
	return " // legacy error code(s): " + strings.Join(codes, ", ")
}

// goLitRHS cleans a C literal right-hand side for the draft: char literals
// become Go strings; anything else passes through untouched.
func goLitRHS(s string) string {
	if v, ok := charLitToGo(s); ok {
		return v
	}
	return s
}

// leadingCall extracts the callee of the first call in the text.
func leadingCall(text string) (string, bool) {
	for i := 0; i < len(text); i++ {
		if isIdentStartByte(text[i]) {
			j := i
			for j < len(text) && isIdentByte(text[j]) {
				j++
			}
			if j < len(text) && text[j] == '(' {
				return text[i:j], true
			}
			i = j
		}
	}
	return "", false
}

// topLevelAssignIndex returns the byte index of the first top-level
// assignment '=' — outside ==, !=, <=, >= and outside parens/brackets —
// or -1. One scanner for both the boolean test and the split.
func topLevelAssignIndex(text string) int {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '"', '\'':
			q := text[i]
			for i++; i < len(text) && text[i] != q; i++ {
			}
		case '=':
			if depth == 0 && i > 0 && !strings.ContainsRune("=!<>+-*/%&|^", rune(text[i-1])) &&
				(i+1 >= len(text) || text[i+1] != '=') {
				return i
			}
		}
	}
	return -1
}

// splitAssign splits the first top-level assignment into lhs/rhs with casts
// stripped, for the conservative `lhs := rhs` draft.
func splitAssign(text string) (lhs, rhs string) {
	i := topLevelAssignIndex(text)
	if i < 0 {
		return text, ""
	}
	lhs = strings.TrimSpace(text[:i])
	rhs = strings.TrimSpace(text[i+1:])
	lhs = strings.TrimSuffix(lhs, ";")
	rhs = strings.TrimSuffix(rhs, ";")
	return stripCasts(lhs), stripCasts(rhs)
}

// stripCasts removes C cast expressions (`(char *)`, `(FBFR32*)`).
func stripCasts(s string) string {
	for {
		i := strings.Index(s, "(")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], ")")
		if j < 0 {
			return s
		}
		inner := strings.TrimSpace(s[i+1 : i+j])
		if inner == "" || strings.ContainsAny(inner, "()") {
			return s
		}
		first := inner
		for k := 0; k < len(first); k++ {
			if !isIdentByte(first[k]) && first[k] != ' ' && first[k] != '*' {
				return s // not a cast — a real parenthesized expr
			}
		}
		s = s[:i] + s[i+j+1:]
	}
}

// exprGo renders a best-effort Go condition; "" when the expression is not
// transpilable (calls stay honest TODOs, never fake Go).
//
// Foccur32 presence checks transliterate to Go presence checks:
// `Foccur32(buf, FML_X) > 0` → `FmlX != ""`, `== 0` → `FmlX == ""`
// (FieldFromFML naming — the request-struct policy). The buffer never
// renders: presence is a property of the request field, not of which
// local buffer variable carried it.
func exprGo(e *pred.Expr) string {
	if e == nil {
		return ""
	}
	if field, present, ok := pred.PresenceOf(e); ok {
		goField := common.FieldFromFML(field)
		if goField == "" {
			return ""
		}
		if present {
			return goField + ` != ""`
		}
		return goField + ` == ""`
	}
	if ident, present, ok := pred.GoPresenceOf(e); ok {
		if present {
			return ident + ` != ""`
		}
		return ident + ` == ""`
	}
	// `strcmp(V, "LIT") == 0` is the corpus's dispatch idiom and is exactly
	// a Go string comparison, so it is checked before the Kind switch —
	// the cmp case would otherwise assemble it as
	// `V == "LIT" == 0`, because the call and the trailing 0 are two
	// separate nodes and neither is transpilable alone.
	if s, ok := strcmpEquality(e); ok {
		return s
	}
	switch e.Kind {
	case "and", "or":
		sep := " && "
		if e.Kind == "or" {
			sep = " || "
		}
		parts := make([]string, 0, len(e.Items))
		// Index, not value: `for _, item := range` copies the whole
		// 136-byte pred.Expr onto the stack and `&item` then forces it to
		// escape, so every conjunct/disjunct allocated. &e.Items[i] points
		// straight into the tree — the pattern the rest of the package uses.
		for i := range e.Items {
			s := exprGo(&e.Items[i])
			if s == "" {
				return ""
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, sep)
	case "not":
		s := exprGo(e.Inner)
		if s == "" {
			return ""
		}
		if e.Inner != nil && e.Inner.Kind == "cmp" {
			// `!` binds tighter than `<` — `!(c < d)` must keep its parens
			// or the rendered Go flips the semantics.
			return "!(" + s + ")"
		}
		return "!" + s
	case "cmp":
		l, r := exprGo(e.L), exprGo(e.R)
		if l == "" || r == "" {
			return ""
		}
		return l + " " + e.Op + " " + r
	case "ident":
		if e.Name == "NULL" {
			return "nil"
		}
		return e.Name
	case "lit":
		return goLit(e.Text)
	default:
		return ""
	}
}

// charLitToGo maps a C char literal ('x') to a Go string literal ("x").
func charLitToGo(t string) (string, bool) {
	t = strings.TrimSpace(t)
	if len(t) >= 3 && t[0] == '\'' && t[len(t)-1] == '\'' {
		return `"` + t[1:len(t)-1] + `"`, true
	}
	return "", false
}

// strcmpEquality renders the corpus's dispatch idiom — `strcmp(V, "LIT") == 0`
// and its `!= 0` negation — as the Go string comparison it is exactly
// equivalent to. C's strcmp returns 0 iff the two strings are byte-equal; Go's
// `==` on strings is byte-equal. Nothing is approximated.
//
// It is deliberately stricter than the axis harvester, which matches
// `strn?cmp` with one regex (internal/flow/scenario.go strcmpSiteRe). For
// harvest that is right — strncmp(a,"CSH",3) and strcmp(a,"CSH") do select the
// same dispatch value — but for CODE it would be a silent bug: strncmp stops
// at a length, so `strncmp(a,"CSH",3)==0` is not `a == "CSH"`, and rendering
// it as one would compile, run, and pick the wrong arm for every longer value.
// Same for strcasecmp. So the callee must be exactly "strcmp".
//
// The `== 0` matters too. `strcmp(a,b) < 0` is an ordering test that Go's `==`
// cannot express, so anything other than a comparison against literal 0 is
// declined and stays a TODO.
func strcmpEquality(e *pred.Expr) (string, bool) {
	if e == nil || e.Kind != pred.KindCmp {
		return "", false
	}
	if e.Op != "==" && e.Op != "!=" {
		return "", false
	}
	if e.L == nil || e.L.Kind != pred.KindCall || e.L.Name != "strcmp" {
		return "", false
	}
	if e.R == nil || e.R.Kind != pred.KindLit || strings.TrimSpace(e.R.Text) != "0" {
		return "", false
	}
	if len(e.L.Args) != 2 {
		return "", false
	}
	lhs, ok := strcmpOperand(e.L.Args[0])
	if !ok {
		return "", false
	}
	rhs, ok := strcmpOperand(e.L.Args[1])
	if !ok {
		return "", false
	}
	return lhs + " " + e.Op + " " + rhs, true
}

// strcmpOperand renders one strcmp argument: a bare identifier or a string
// literal, and nothing else.
//
// A member access (`sql_ovp_lvl1_desc.arr`) is declined rather than
// best-effort rendered. `.arr` is a Tuxedo struct member, not a Go field, so
// the Go spelling of the operand depends on the target struct's shape — which
// is exactly the kind of fact this project refuses to guess. Declining leaves
// an honest TODO; rendering `sql_ovp_lvl1_desc.arr` would not compile, and
// rendering `sql_ovp_lvl1_desc` would compile and be wrong.
func strcmpOperand(arg string) (string, bool) {
	a := strings.TrimSpace(arg)
	if a == "" || strings.ContainsAny(a, "()[]*&+-/ ") {
		return "", false
	}
	if strings.HasPrefix(a, `"`) {
		if !strings.HasSuffix(a, `"`) || len(a) < 2 {
			return "", false
		}
		return a, true
	}
	if v, ok := charLitToGo(a); ok {
		return v, true
	}
	if !isGoIdent(a) {
		return "", false
	}
	return a, true
}

// isGoIdent reports whether s is a plain Go identifier: a letter or underscore
// followed by letters, digits, and underscores. Dots are excluded on purpose —
// a member access is not an identifier (see strcmpOperand).
func isGoIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return s != ""
}

// goLit maps C literals to Go: char literals become strings, numbers pass.
func goLit(t string) string {
	if v, ok := charLitToGo(t); ok {
		return v
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	for _, r := range t {
		if r != '.' && (r < '0' || r > '9') {
			return ""
		}
	}
	return t
}
