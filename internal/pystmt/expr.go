package pystmt

import (
	"strings"
)

// Expression IR — the "missing piece" Phase 2 names
// (docs/pybatchpy-logic-plan.md §7: expressions "today live raw in
// `Calls[].Args`").
//
// `pred.Expr` is the right shape for a *condition*: a C-precedence boolean
// tree that degrades to Raw. It is the wrong shape for everything else in
// this corpus — a call argument list, an assignment right-hand side, a for
// header. Its grammar rejects arithmetic continuation by design
// (`pred.parseCmp` fails on `+ - * / ? :`), so
// `TO_DATE(:tmp_strt_date,'DD-MM-YYYY')+1` or `mnth_val[i_row][j]>0` would
// arrive as one opaque Raw string. So conditions keep `pred.Expr` and
// everything else gets the tree below.
//
// The parser is a precedence climber over C's operator table, with two
// constructs that this corpus cannot do without:
//
//   - **Casts.** `(char *)c_strt_date`, `(TPINIT*)tpalloc(…)`, `(long)i`. Without
//     cast handling roughly half the argument expressions here degrade to Raw,
//     which would make the IR a string table again. A parenthesised run of
//     type words and `*` is a cast; anything with an operator or a comma
//     inside is a grouping paren.
//   - **Subscript and member chains.** `mnth_val[i_row][j]`, `c_Event_msg`,
//     `ptr_tpinfo->grpname`, `sql_msi_intvl_start_dt.arr` are how this batch
//     reads arrays and Pro*C structs. They are the shape every loop bound and
//     debug log in the file is written in.
//
// Degrade direction: any parse that does not consume its whole input returns
// `Raw{normalized text}`. A partial tree is worse than an honest one — it
// looks structured and is not.
type ExprKind string

const (
	ExprIdent   ExprKind = "ident"
	ExprLit     ExprKind = "lit"
	ExprIndex   ExprKind = "index"
	ExprMember  ExprKind = "member"
	ExprCall    ExprKind = "call"
	ExprCast    ExprKind = "cast"
	ExprUnary   ExprKind = "unary"
	ExprBinary  ExprKind = "binary"
	ExprAssign  ExprKind = "assign"
	ExprTernary ExprKind = "ternary"
	ExprRaw     ExprKind = "raw"
)

// Expr is one node. Which fields are set depends on Kind:
//
//	ident  Name              lit     Text
//	index  L, Args[0]        member  L, Name (`.arr` / `->grpname`)
//	call   Name, Args        cast    Name (type), L
//	unary  Op, L             binary  Op, L, R
//	assign Op, L, R          ternary L, Args[0] (then), Args[1] (else)
//	raw    Text
//
// L and R are pointers so the JSON serialises as a nested object rather than
// a value copy at every level; Args is a slice for the variadic positions.
type Expr struct {
	Kind ExprKind `json:"kind"`
	Name string   `json:"name,omitempty"`
	Op   string   `json:"op,omitempty"`
	Text string   `json:"text,omitempty"`
	L    *Expr    `json:"l,omitempty"`
	R    *Expr    `json:"r,omitempty"`
	Args []Expr   `json:"args,omitempty"`
}

// Raw builds the lossless degrade node.
func Raw(text string) Expr {
	return Expr{Kind: ExprRaw, Text: strings.Join(strings.Fields(text), " ")}
}

// IsRaw reports the degrade.
func (e *Expr) IsRaw() bool { return e != nil && e.Kind == ExprRaw }

// String renders the tree back to C-shaped text. It is the round-trip check:
// Parse then String must reproduce the input modulo whitespace, and the tests
// assert that for every shape in this corpus.
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case ExprIdent:
		return e.Name
	case ExprLit:
		return e.Text
	case ExprIndex:
		return e.L.String() + "[" + arg(e.Args, 0) + "]"
	case ExprMember:
		op := "."
		if e.Op == "->" {
			op = "->"
		}
		return e.L.String() + op + e.Name
	case ExprCall:
		return e.Name + "(" + strings.Join(argTexts(e.Args), ", ") + ")"
	case ExprCast:
		return "(" + e.Name + ")" + e.L.String()
	case ExprUnary:
		return e.Op + e.L.String()
	case ExprBinary:
		// Parenthesise a child that binds looser than this operator, or the
		// round trip silently changes the meaning: `(a + b) * c` renders as
		// `a + b * c`, which is a different program. The tree is right and the
		// rendering would be wrong, so the rendering has to recover the parens.
		return paren(e.L, e.Op, true) + " " + e.Op + " " + paren(e.R, e.Op, false)
	case ExprAssign:
		return e.L.String() + " " + e.Op + " " + e.R.String()
	case ExprTernary:
		return e.L.String() + " ? " + arg(e.Args, 0) + " : " + arg(e.Args, 1)
	case ExprRaw:
		return e.Text
	}
	return e.Text
}

// precedence returns the binding strength of an operator, matching binLevels'
// order (higher binds tighter). An unknown operator binds tightest, which is
// the safe direction: paren only ever adds parentheses that were there.
func precedence(op string) int {
	for i, level := range binLevels {
		if hasOp(level, op) {
			return i
		}
	}
	return -1
}

// paren wraps a binary operand in parentheses when it binds looser than the
// surrounding operator. Equal precedence on the left needs no parens (C
// left-associativity already groups it); equal precedence on the right does,
// which is what makes `a - (b - c)` render correctly.
func paren(e *Expr, op string, left bool) string {
	if e == nil || e.Kind != ExprBinary {
		return e.String()
	}
	p, q := precedence(e.Op), precedence(op)
	if p < q || (p == q && !left) {
		return "(" + e.String() + ")"
	}
	return e.String()
}

func arg(args []Expr, i int) string {
	if i < len(args) {
		return args[i].String()
	}
	return ""
}

func argTexts(args []Expr) []string {
	out := make([]string, len(args))
	for i := range args {
		out[i] = args[i].String()
	}
	return out
}

// Ident builds an identifier leaf (used by the statement layer for an
// assignment target it resolved itself).
func Ident(name string) Expr { return Expr{Kind: ExprIdent, Name: name} }

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tChar
	tOp
)

type token struct {
	kind tokKind
	text string
}

// ops is C's operator table, longest first so `>>=` never lexes as `>>` then
// `=`, and `->` never as `-` then `>`.
var ops = []string{
	">>=", "<<=",
	"->", "++", "--", "==", "!=", "<=", ">=", "&&", "||",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "::",
	"+", "-", "*", "/", "%", "=", "<", ">", "!", "~", "&", "|", "^",
	"?", ":", ".", ",", "(", ")", "[", "]", "{", "}", ";", "#",
}

func lex(src string) []token {
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '"' || c == '\'':
			j := scanLiteral(src, i, c)
			k := tString
			if c == '\'' {
				k = tChar
			}
			out = append(out, token{kind: k, text: src[i:j]})
			i = j
		case isIdentByte(c):
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			out = append(out, token{kind: tIdent, text: src[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := scanNumber(src, i)
			out = append(out, token{kind: tNumber, text: src[i:j]})
			i = j
		default:
			matched := ""
			for _, o := range ops {
				if strings.HasPrefix(src[i:], o) {
					matched = o
					break
				}
			}
			if matched == "" {
				// An unknown byte: surface it as punctuation so the
				// parser fails loudly into Raw rather than looping.
				matched = string(c)
			}
			out = append(out, token{kind: tOp, text: matched})
			i += len(matched)
		}
	}
	return append(out, token{kind: tEOF})
}

func scanLiteral(src string, i int, quote byte) int {
	for j := i + 1; j < len(src); j++ {
		if src[j] == '\\' {
			j++
			continue
		}
		if src[j] == quote {
			return j + 1
		}
	}
	return len(src)
}

// scanNumber consumes a C numeric literal: decimal, float, hex, with the
// unsigned/long/float suffixes Pro*C inherits.
func scanNumber(src string, i int) int {
	j := i
	if src[j] == '0' && j+1 < len(src) && (src[j+1] == 'x' || src[j+1] == 'X') {
		j += 2
		for j < len(src) && isHexByte(src[j]) {
			j++
		}
	} else {
		for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
			j++
		}
	}
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && src[k] >= '0' && src[k] <= '9' {
			j = k
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
		}
	}
	for j < len(src) && strings.IndexByte("uUlLfF", src[j]) >= 0 {
		j++
	}
	return j
}

func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
}

// ParseExpr parses a C expression. It never fails: text the grammar does not
// cover, or that does not parse to completion, comes back as Raw.
func ParseExpr(text string) Expr {
	if strings.TrimSpace(text) == "" {
		return Raw(text)
	}
	p := &parser{toks: lex(text)}
	e, ok := p.parseAssign()
	if !ok || p.peek().kind != tEOF {
		return Raw(text)
	}
	return e
}

func (p *parser) peek() token  { return p.toks[p.pos] }
func (p *parser) next() token  { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool  { return p.toks[p.pos].kind == tEOF }
func (p *parser) at(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == s
}

func (p *parser) accept(s string) bool {
	if p.at(s) {
		p.pos++
		return true
	}
	return false
}

// binLevels is C's binary precedence, loosest first. `=` is deliberately
// absent: assignment is handled by parseAssign, which is right-associative,
// and `==` is a distinct token so the two can never be confused.
var binLevels = [][]string{
	{"||"},
	{"&&"},
	{"|"},
	{"^"},
	{"&"},
	{"==", "!="},
	{"<", ">", "<=", ">="},
	{"<<", ">>"},
	{"+", "-"},
	{"*", "/", "%"},
}

var assignOps = []string{"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>="}

// parseAssign := parseTernary [assignOp parseAssign]
func (p *parser) parseAssign() (Expr, bool) {
	l, ok := p.parseTernary()
	if !ok {
		return Expr{}, false
	}
	t := p.peek()
	if t.kind != tOp {
		return l, true
	}
	for _, op := range assignOps {
		if t.text == op {
			p.next()
			r, ok := p.parseAssign()
			if !ok {
				return Expr{}, false
			}
			return Expr{Kind: ExprAssign, Op: op, L: &l, R: &r}, true
		}
	}
	return l, true
}

// parseTernary := parseBin(0) ['?' parseAssign ':' parseTernary]
func (p *parser) parseTernary() (Expr, bool) {
	c, ok := p.parseBin(0)
	if !ok {
		return Expr{}, false
	}
	if !p.accept("?") {
		return c, true
	}
	then, ok := p.parseAssign()
	if !ok || !p.accept(":") {
		return Expr{}, false
	}
	els, ok := p.parseTernary()
	if !ok {
		return Expr{}, false
	}
	return Expr{Kind: ExprTernary, L: &c, Args: []Expr{then, els}}, true
}

func (p *parser) parseBin(level int) (Expr, bool) {
	if level >= len(binLevels) {
		return p.parseUnary()
	}
	l, ok := p.parseBin(level + 1)
	if !ok {
		return Expr{}, false
	}
	for {
		t := p.peek()
		if t.kind != tOp || !hasOp(binLevels[level], t.text) {
			return l, true
		}
		p.next()
		r, ok := p.parseBin(level + 1)
		if !ok {
			return Expr{}, false
		}
		// `cur` is a fresh variable each iteration, not the loop's own `l`.
		// `&l` would alias the accumulator, so `l = Expr{... L: &l ...}` makes
		// the new node's own left child point back at itself: `a + b + c` builds
		// a cyclic tree and every walk over it recurses until the stack dies.
		cur := l
		l = Expr{Kind: ExprBinary, Op: t.text, L: &cur, R: &r}
	}
}

// parseUnary := ('-'|'+'|'!'|'~'|'*'|'&') parseUnary | parsePostfix
func (p *parser) parseUnary() (Expr, bool) {
	t := p.peek()
	if t.kind == tOp {
		switch t.text {
		case "-", "+", "!", "~", "*", "&":
			p.next()
			inner, ok := p.parseUnary()
			if !ok {
				return Expr{}, false
			}
			return Expr{Kind: ExprUnary, Op: t.text, L: &inner}, true
		case "++", "--":
			p.next()
			inner, ok := p.parseUnary()
			if !ok {
				return Expr{}, false
			}
			return Expr{Kind: ExprUnary, Op: t.text, L: &inner}, true
		}
	}
	return p.parsePostfix()
}

// parsePostfix := parsePrimary { '(' args ')' | '[' expr ']' | ('.'|'->') ident }
func (p *parser) parsePostfix() (Expr, bool) {
	e, ok := p.parsePrimary()
	if !ok {
		return Expr{}, false
	}
	for {
		t := p.peek()
		if t.kind != tOp {
			return e, true
		}
		// `cur` is a fresh variable per iteration for the same reason as in
		// parseBin: `&e` aliases the loop's own accumulator, so `e = Expr{
		// ..., L: &e}` builds a node whose child is itself. `a[i][j]` and
		// `p->q.r` are exactly the shapes that hit it, and every walk over the
		// resulting cycle recurses until the stack dies.
		cur := e
		switch t.text {
		case "(", "[":
			open := t.text
			p.next()
			var items []Expr
			for {
				if p.accept(")") && open == "(" {
					e = Expr{Kind: ExprCall, Name: cur.String(), Args: items}
					goto done
				}
				if p.accept("]") && open == "[" {
					e = Expr{Kind: ExprIndex, L: &cur, Args: items}
					goto done
				}
				item, ok := p.parseAssign()
				if !ok {
					return Expr{}, false
				}
				items = append(items, item)
				if p.accept(",") {
					continue
				}
				if (open == "(" && p.accept(")")) || (open == "[" && p.accept("]")) {
					if open == "(" {
						e = Expr{Kind: ExprCall, Name: cur.String(), Args: items}
					} else {
						e = Expr{Kind: ExprIndex, L: &cur, Args: items}
					}
					goto done
				}
				return Expr{}, false
			}
		case ".", "->":
			p.next()
			name := p.next()
			if name.kind != tIdent {
				return Expr{}, false
			}
			e = Expr{Kind: ExprMember, Op: t.text, L: &cur, Name: name.text}
		default:
			return e, true
		}
	done:
	}
}

// cTypeWords are the type names a parenthesised run may hold for it to be a
// cast rather than a grouping paren. Kept short on purpose: the fallback
// rules (a `*`, or two or more words) carry the corpus, so this list only has
// to disambiguate the bare single-word case `(long)x`.
var cTypeWords = map[string]bool{
	"void": true, "char": true, "short": true, "int": true, "long": true,
	"float": true, "double": true, "signed": true, "unsigned": true,
	"struct": true, "union": true, "enum": true, "const": true,
	"volatile": true, "size_t": true, "time_t": true, "bool": true,
}

// parsePrimary := cast | '(' expr ')' | literal | ident
//
// The cast test runs before the grouping paren because `(char *)x` parses
// perfectly well as a grouping paren over a binary `char * x` — as a *tree*
// it is wrong, and the wrong tree would turn `(TPINIT*)tpalloc(…)` into a
// multiplication. It is recognised by shape: only type words and `*`, and then
// followed by something an operand can start with.
func (p *parser) parsePrimary() (Expr, bool) {
	t := p.peek()
	switch {
	case t.kind == tString || t.kind == tChar || t.kind == tNumber:
		p.next()
		return Expr{Kind: ExprLit, Text: t.text}, true
	case t.kind == tIdent:
		p.next()
		return Expr{Kind: ExprIdent, Name: t.text}, true
	case t.kind == tOp && t.text == "(":
		if name, ok := p.castAhead(); ok {
			operand, ok := p.parseUnary()
			if !ok {
				return Expr{}, false
			}
			return Expr{Kind: ExprCast, Name: name, L: &operand}, true
		}
		p.next()
		inner, ok := p.parseAssign()
		if !ok || !p.accept(")") {
			return Expr{}, false
		}
		return inner, true
	}
	return Expr{}, false
}

// castAhead reports whether the `(` at the cursor opens a cast, and returns the
// type text if so, having consumed the whole `(type)` run.
func (p *parser) castAhead() (string, bool) {
	start := p.pos
	depth := 0
	var words []string
	stars := 0
	other := false
	for i := p.pos; i < len(p.toks); i++ {
		t := p.toks[i]
		if t.kind == tEOF {
			break
		}
		if t.kind == tOp && t.text == "(" {
			depth++
			if depth == 1 {
				continue
			}
		}
		if t.kind == tOp && t.text == ")" {
			depth--
			if depth == 0 {
				break
			}
		}
		if depth != 1 {
			other = true
			break
		}
		switch {
		case t.kind == tIdent:
			words = append(words, t.text)
		case t.kind == tOp && t.text == "*":
			stars++
		case t.kind == tOp && t.text == "[":
			// `(char [8])` never appears; give up cleanly.
			other = true
		default:
			other = true
		}
		if other {
			break
		}
	}
	if other || len(words) == 0 {
		p.pos = start
		return "", false
	}
	// The run must end here, and an operand must follow.
	j := p.pos
	for p.toks[j].kind != tEOF && !(p.toks[j].kind == tOp && p.toks[j].text == ")") {
		j++
	}
	if p.toks[j].kind == tEOF {
		p.pos = start
		return "", false
	}
	after := p.toks[j+1]
	operandFollows := false
	switch after.kind {
	case tIdent, tNumber, tString, tChar:
		operandFollows = true
	case tOp:
		switch after.text {
		case "(", "!", "~", "*", "&", "-", "+":
			operandFollows = true
		}
	}
	isType := stars > 0 || len(words) > 1
	if len(words) == 1 && cTypeWords[strings.ToLower(words[0])] {
		isType = true
	}
	if !operandFollows || !isType {
		p.pos = start
		return "", false
	}
	// Consume `( … )` and the operator tokens of the run.
	p.pos = j + 1
	// No space before the stars: `(TPINIT*)tpalloc(…)` is how the source
	// spells it and how the corpus spells it. Rendering `TPINIT *` round-trips
	// to a spelling nobody writes, which makes the round-trip check fail on a
	// tree that is actually correct.
	name := strings.Join(words, " ")
	if stars > 0 {
		name += strings.Repeat("*", stars)
	}
	return strings.TrimSpace(name), true
}

// hasOp reports whether a token list holds an operator. Named `hasOp` rather
// than `contains` because build.go has an unrelated interval `contains` for
// region nesting — two same-named unexported helpers in one package is the
// kind of collision that only shows up at build time.
func hasOp(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// SplitArgs splits a call's raw argument text on top-level commas, honouring
// nesting and literals. It is the path for the argument text the scanner
// records (`Calls[].Args`), so the statement layer can parse each argument
// even when it never sees the call's own parens.
func SplitArgs(args string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(args); i++ {
		switch c := args[i]; c {
		case '"', '\'':
			i = scanLiteral(args, i, c) - 1
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				if s := strings.TrimSpace(args[start:i]); s != "" {
					out = append(out, s)
				}
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(args[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// ParseArgs parses each top-level argument of a recorded argument list.
func ParseArgs(args string) []Expr {
	parts := SplitArgs(args)
	out := make([]Expr, 0, len(parts))
	for _, p := range parts {
		out = append(out, ParseExpr(p))
	}
	return out
}