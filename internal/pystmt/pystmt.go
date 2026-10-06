// Package pystmt is the batch statement IR — Phase 2 of
// docs/pybatchpy-logic-plan.md §7.
//
// ## What it is for
//
// `pyplan.Plan` carries SQL text and method signatures and nothing else
// (§3.1: "no expression tree, no statement list, no types, no conditions").
// Everything downstream is template filling, which is why the `--no-llm`
// output for a stateful batch is `raise NotImplementedError` (§3.3): there is
// nothing in the plan to emit service logic *from*. This package is that
// missing structure: per function, a linear statement stream with nesting and
// byte spans, so a later phase can emit code instead of asking an LLM to
// comprehend 1,332 lines of Pro*C with no IR (§7 Phase 5).
//
// ## Where the facts come from
//
// The skeleton reuses the shared scanner rather than re-parsing: function
// bodies, if/else-if/else chains, loops, returns, declarations and calls all
// come from `tsscan.SourceFacts`, and every `EXEC SQL` region from the IR the
// batch flow already carries. Three gaps are filled here, and each is
// recorded rather than papered over:
//
//   - **Switches.** `SourceFacts.Switches` is declared "additive" but is empty
//     on dtcmpt.pc even though the file has `switch(sql_msi_intvl_sch_typ)` at
//     :386 with four arms. Trusting the field would silently drop the exact
//     dispatch whose 'D' arm is the §6.2 defect. So switches are located by a
//     contained source scan (findSwitches) — the same decision pycensus makes,
//     for the same reason.
//   - **Expressions.** §7 says so explicitly: they "today live raw in
//     `Calls[].Args`". Call arguments, assignment right-hand sides and for
//     headers are parsed here (expr.go) into a tree with a lossless Raw
//     degrade.
//   - **Statements the scanner has no fact for.** `break`, `continue`, `goto`
//     and plain assignments are not in the scanner's pinned vocabulary, and
//     pointer declarations are explicitly skipped by it (`walk.go`: "pointer
//     declarations are not VarDecls"), so `TPINIT* ptr_tpinfo;` at :101 has no
//     fact at all. A line-scope classifier supplies them; Coverage below is
//     what proves nothing fell between the two.
//
// ## Conditions
//
// Every condition — if, while, do-while tail, for middle clause, switch — is a
// `pred.Expr` (`internal/pred`), reused unmodified. It is already a
// C-precedence boolean tree and already degrades to Raw instead of failing.
//
// ## Proof, not assertion
//
// `Unit.Coverage` partitions every source line of every scanned function body
// into exactly one of: blank, comment, preprocessor, block delimiter, control
// header, or a statement span. `Unit.Uncovered` is what is left — a line of
// real code that no statement claims. That is the §6.1 gate-2 question
// ("every non-SQL statement lands in exactly one bucket") asked of the IR
// itself, and it is a number rather than a judgement. `Unit.Residue` closes
// the loop from the other side: a scanner fact (call, declaration, return)
// inside a scanned function that no statement claims. Both are measured, not
// asserted, and both are reported rather than gating — Phase 3 promotes them.
//
// ## Isolation
//
// py-path only. It imports the shared read-only scanner, the IR and `pred`,
// and nothing in `internal/gen`, `internal/csplan` or `internal/csgen`. This
// is the additive-isolation discipline docs/uniform-ir-plan.md sets, and it is
// why the Go and C# targets cannot regress from this file.
package pystmt

import (
	"strconv"
	"strings"

	"tux-migration-tool/internal/pred"
)

// Kind discriminates a statement. The vocabulary is closed: a batch Pro*C
// program has a small fixed statement set, and a construct that fits none of
// these degrades to Expr with its text preserved verbatim rather than being
// dropped. That is deliberate — §3.2's complaint is about undifferentiated
// silent loss, so an unrecognised statement must still be *visible*.
type Kind string

const (
	// KindExpr is a statement whose effect is not one of the others: a bare
	// expression, an unrecognised control transfer (`goto`), a cast-and-call
	// expression. Text always carries the original so nothing is lost.
	//
	// It is the one Kind constant that does not carry its own name. `Expr` is
	// the package's expression *type* (expr.go), and a constant and a type
	// cannot share an identifier — that collision is why the package sat
	// uncompilable while the IR was being written. The type is the more
	// fundamental of the two (it is the type of Stmt.LHS, Stmt.RHS and
	// Stmt.Args), so the constant yields.
	KindExpr Kind = "expr"
	// Assign writes a target: `x = e`, `x += e`, `x++`, `x--`.
	Assign Kind = "assign"
	// If is a branch. Chain siblings (else / else if) hang off an If's Else
	// field rather than sitting beside it, so the emitted Python is an
	// if/elif/else and not a flat bag of arms.
	If Kind = "if"
	// While is a while loop.
	While Kind = "while"
	// For is a counted for loop. Init/Step carry the first and third header
	// clauses; Cond carries the middle one.
	For Kind = "for"
	// DoWhile is a do/while loop. Cond comes from the tail `while`, not the
	// header, so CondText and the WhileLine are both recorded.
	DoWhile Kind = "do-while"
	// Switch is a dispatch. Arms carries the case/default labels in source
	// order.
	Switch Kind = "switch"
	// Break leaves the innermost loop or switch.
	Break Kind = "break"
	// Continue starts the next iteration of the innermost loop.
	Continue Kind = "continue"
	// Return leaves the function; RHS is the returned expression when the
	// source has one.
	Return Kind = "return"
	// Decl is a local declaration. Names carries every declarator on the
	// line (`int i, j, k;` is one statement with three names) because that is
	// one source statement and accounting must count it as one.
	Decl Kind = "decl"
	// SQLRef is one EXEC SQL region. It is a statement, not a repository
	// method — the §4 split ("a statement whose byte span touches a cursor
	// becomes a repository method") is a *consumer* of this IR, not a
	// producer of it.
	SQLRef Kind = "sqlref"
	// Call is a bare call statement whose result is discarded
	// (`tpterm();`, `rtrim(x);`). A call that produces a value is an
	// Assign; a call inside a condition is part of the condition.
	Call Kind = "call"
)

// kinds is the canonical report order, so a count map always prints the same
// way and an absent kind is visibly absent rather than silently merged.
var kinds = []Kind{
	KindExpr, Assign, If, While, For, DoWhile, Switch,
	Break, Continue, Return, Decl, SQLRef, Call,
}

// Chain discriminates an arm's position in an if/else-if/else chain. It is
// recorded on the Stmt so the chain can be rebuilt after the region tree is
// assembled (siblings only become an else after their order is known).
type Chain string

const (
	ChainHead Chain = ""      // a plain `if`
	ChainElse Chain = "else"  // `else`
	ChainElseIf Chain = "elseif" // `else if`
)

// Span is a statement's byte extent. Line/Col are 1-based and byte-faithful to
// the original source (the scanner's convention); StartByte/EndByte are
// 0-based offsets into the same source, so a consumer can slice the exact
// bytes without re-deriving them. EndCol is exclusive of the terminator's
// newline: for `x = 1;` on one line it is the column just past the `;`.
type Span struct {
	StartLine int `json:"start_line"`
	StartCol  int `json:"start_col"`
	EndLine   int `json:"end_line"`
	EndCol    int `json:"end_col"`
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
}

// Arm is one switch label and the statements under it. An unbraced arm
// (`default:` at dtcmpt.pc:1259 followed by two statements and no braces)
// is exactly the shape where a parser that assumes braces loses statements,
// so Arm records its own span and SpanValid says whether that extent is known.
type Arm struct {
	Label     string `json:"label"`
	IsDefault bool   `json:"default,omitempty"`
	Span      Span   `json:"span"`
	Body      []Stmt `json:"body,omitempty"`
}

// Stmt is one statement. Which fields are set depends on Kind:
//
//	KindExpr  Text
//	Assign    LHS, Op, RHS
//	If        Cond, Body, Else
//	While     Cond, Body
//	For       Init, Cond, Step, Body
//	DoWhile   Cond, Body, TailLine
//	Switch    Cond, Arms
//	Break     —
//	Continue  —
//	Return    RHS
//	Decl      Name (base type), Names (declarators), Init
//	SQLRef    SQLKind, QueryID
//	Call      Name, Args
type Stmt struct {
	Kind Kind `json:"kind"`
	Span Span `json:"span"`
	// Text is the statement's own source with comments blanked and
	// whitespace collapsed. It is the lossless half of the IR: whatever the
	// classifier did not understand is still here.
	Text string `json:"text"`
	// Chain positions an if arm within its chain (see Chain).
	Chain Chain `json:"chain,omitempty"`
	// Name is the call callee, the declaration's base type, or the label of
	// an unrecognised control transfer.
	Name string `json:"name,omitempty"`
	// Names is a declaration's declarator list, source order.
	Names []string `json:"names,omitempty"`
	// Cond is the parsed condition. Always a pred.Expr, which degrades to
	// Raw rather than failing.
	Cond     *pred.Expr `json:"cond,omitempty"`
	CondText string     `json:"cond_text,omitempty"`
	// LHS is an assignment target (also the operand of ++/--).
	LHS *Expr `json:"lhs,omitempty"`
	// Op is the assignment operator: "=", "+=", "-=", "++" or "--".
	Op string `json:"op,omitempty"`
	// RHS is an assignment value or a returned expression.
	RHS *Expr `json:"rhs,omitempty"`
	// Init and Step are a for header's first and third clauses.
	Init *Expr `json:"init,omitempty"`
	Step *Expr `json:"step,omitempty"`
	// Args are a call's parsed arguments, source order.
	Args []Expr `json:"args,omitempty"`
	// Initializer is a declaration's `= …` right-hand side, which is where
	// dtcmpt.pc's 25x12 `mnth_val` frequency table lives (:71-95) — the §5
	// row that becomes a `FREQUENCY_MONTHS` dict.
	Initializer *Expr `json:"initializer,omitempty"`
	// SQLKind is the scanner's SQL kind ("SELECT", "INSERT", "FETCH", …) and
	// QueryID the IR query unit this region became, when it became one.
	SQLKind string `json:"sql_kind,omitempty"`
	QueryID string `json:"query_id,omitempty"`
	// Body is the statement list of a block-bearing statement.
	Body []Stmt `json:"body,omitempty"`
	// Else carries an if chain's later arms. An `else if` appears here as a
	// nested If, so a reader (or an emitter) sees the chain, not a bag.
	Else []Stmt `json:"else,omitempty"`
	// Arms are a switch's labels in source order.
	Arms []Arm `json:"arms,omitempty"`
	// TailLine is the line of a do-while's `while (…)` tail.
	TailLine int `json:"tail_line,omitempty"`
	// Depth is the block nesting level, 0 at a function body.
	Depth int `json:"depth"`
}

// Param is one function parameter.
type Param struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Array bool   `json:"array,omitempty"`
	Span  Span   `json:"span"`
}

// Func is one function's statement stream.
type Func struct {
	Name       string `json:"name"`
	ReturnType string `json:"return_type,omitempty"`
	// Header is the signature line's span; Body is the braced block's.
	Header Span   `json:"header"`
	Body   Span   `json:"body"`
	Params []Param `json:"params,omitempty"`
	Stmts  []Stmt  `json:"stmts"`
	// Counts is this function's statement histogram.
	Counts map[Kind]int `json:"counts,omitempty"`
}

// Walk calls fn for every statement in pre-order: a statement, then its body,
// then its else arms, then its switch arms. The stream is therefore both the
// nested form and a linear one — Phase 3's per-idiom completion counters need
// the linear form, the emitter needs the nested form, and deriving one from
// the other twice is how the two drift apart.
func Walk(stmts []Stmt, fn func(*Stmt)) {
	for i := range stmts {
		s := &stmts[i]
		fn(s)
		Walk(s.Body, fn)
		Walk(s.Else, fn)
		for a := range s.Arms {
			Walk(s.Arms[a].Body, fn)
		}
	}
}

// Line renders a span's extent as `line` or `a-b` — the form every message in
// this stack uses to point at source.
func (s Span) Line() string {
	if s.StartLine == s.EndLine {
		return strconv.Itoa(s.StartLine)
	}
	return strconv.Itoa(s.StartLine) + "-" + strconv.Itoa(s.EndLine)
}

// String renders the statement as one line of the linear stream: `line kind
// text`, truncated to keep a census readable. It is the debug/report form;
// Text is still the lossless field.
func (s Stmt) String() string {
	text := s.Text
	if len(text) > 60 {
		text = text[:57] + "..."
	}
	kind := string(s.Kind)
	if s.Chain != "" {
		kind = string(s.Chain) + "-" + kind
	}
	return s.Span.Line() + " " + kind + " " + text
}

// Summary renders the statement stream as text, one statement per line, with
// indentation from Depth. Phase 3 uses it to show a reviewer exactly what the
// IR believes the program does; a reviewer reading it is the cheapest
// available check that the IR is not lying.
func (f Func) Summary() string {
	var b strings.Builder
	Walk(f.Stmts, func(s *Stmt) {
		b.WriteString(strings.Repeat("  ", s.Depth))
		b.WriteString(s.String())
		b.WriteString("\n")
	})
	return b.String()
}