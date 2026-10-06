package testgen

import (
	"go/ast"
	"go/token"
	"strings"
)

// This file is the answer to a question the log route could not answer.
//
// Every generated controller case used to be built from what the log recorded
// on ONE captured request: `trace.DBCalls(method)` says which store calls ran
// on that trace, and that set was asserted for EVERY synthesized case. It is
// the wrong source, for two reasons the corpus demonstrates:
//
//   - GetDB() is a store call that runs no SQL, so it can never appear in a
//     log. It was therefore never expected, and the generated Success case
//     died on "Unexpected call to GetDB" before reaching AddQuestion, whose
//     own expectation then went unmet. The fix is not a special case: GetDB
//     is an ordinary step of the success path, and a path model includes it
//     only on the paths that reach it.
//
//   - Which calls run is PATH-DEPENDENT. In AddQuestion the body returns
//     before ExecTransaction when QuestionNumberExists reports a duplicate,
//     so a globally-always-expect-GetDB rule is exactly as wrong as the
//     current never-expect-it rule. Only the path decides.
//
// Everything needed is in the method body: the guards, their order, and the
// loops that repeat a call. This file reads that and enumerates the paths.

// pathCall is one store call as it happens on one path.
type pathCall struct {
	Method   string
	Args     []string // rendered verbatim, ctx arg dropped
	ArgCount int      // declared arity, ctx included; -1 when unread

	// Times is the gomock .Times() count as source text, empty for the
	// default of one. It is an expression, not a number, because the trip
	// count is a property of the CASE: `for index := range len(request.AnswerID)`
	// runs once per element, and the case decides how many there are.
	Times string

	// ErrBranch marks the call that fails on this path: it returns a non-nil
	// error and the caller takes its error branch.
	ErrBranch bool
	// ErrLiteral is what the failing call returns. It is empty for a generic
	// failure (the renderer substitutes the suite's own sentinel) and set to
	// "sql.ErrNoRows" when the body's guard distinguishes that sentinel.
	ErrLiteral string

	// BoolTrue marks a call whose boolean result sends the caller into a
	// `if <ident> { return <business error> }` body.
	BoolTrue bool

	// GuardMsg is the business-error literal the body returns on BoolTrue,
	// when it writes one instead of propagating a store error.
	GuardMsg string
}

// ctrlPath is one enumerated execution path through a controller method.
type ctrlPath struct {
	Desc  string
	Calls []pathCall
	// Final marks the path that runs to the method's success return.
	Final bool
	// Response is the success value on Final paths that build one.
	Response string
}

// ctrlPathBuilder walks one method body and accumulates paths.
//
// The walk is a linear prefix enumeration, not symbolic execution. Every
// guard yields one path that stops there, and execution continues past it
// along the success edge; the final path runs to the end. That is the classic
// "one case per error point" shape of a hand-written Go table test, and it is
// exactly what gomock can express: a case that fails call N expects calls
// 1..N and nothing after.
type ctrlPathBuilder struct {
	fset    *token.FileSet
	dbIface map[string]dbIfaceSig
	out     []ctrlPath
	cur     []pathCall
}

// enumerateCtrlPaths returns every path through a controller method body.
//
// respLit is the success value the body returns, when it is a literal; it is
// attached to the final path so the renderer need not look again.
func enumerateCtrlPaths(fd *ast.FuncDecl, fset *token.FileSet, dbIface map[string]dbIfaceSig, respLit string) []ctrlPath {
	if fd == nil || fd.Body == nil {
		return nil
	}
	b := &ctrlPathBuilder{fset: fset, dbIface: dbIface}
	b.block(fd.Body.List, "")
	b.finishFinal(respLit)
	return b.out
}

// finishFinal emits the path that reached the end of the method.
func (b *ctrlPathBuilder) finishFinal(respLit string) {
	b.out = append(b.out, ctrlPath{
		Desc:     "Success",
		Calls:    b.snapshot(),
		Final:    true,
		Response: respLit,
	})
}

func (b *ctrlPathBuilder) snapshot() []pathCall {
	out := make([]pathCall, len(b.cur))
	copy(out, b.cur)
	return out
}

// withLast returns a snapshot whose final call carries mutate applied.
func (b *ctrlPathBuilder) withLast(mutate func(*pathCall)) []pathCall {
	snap := b.snapshot()
	if len(snap) == 0 {
		return snap
	}
	mutate(&snap[len(snap)-1])
	return snap
}

// emit appends one enumerated path.
func (b *ctrlPathBuilder) emit(desc string, calls []pathCall) {
	b.out = append(b.out, ctrlPath{Desc: desc, Calls: calls})
}

// block walks a statement list. trip is the loop trip-count expression that
// applies to calls inside it, empty outside a loop.
//
// It reports whether the block always returns, so the caller knows whether
// execution can continue past it.
func (b *ctrlPathBuilder) block(stmts []ast.Stmt, trip string) bool {
	for _, s := range stmts {
		switch st := s.(type) {
		case *ast.ReturnStmt:
			// A tail `return utils.ExecTransaction(ctx, c.store.GetDB(), fn)`
			// is where a method with no other work puts its transaction, so
			// the calls inside the returned expression are still steps of the
			// path. Skipping them dropped GetDB from every such method.
			for _, r := range st.Results {
				call, ok := r.(*ast.CallExpr)
				if !ok {
					continue
				}
				if pc, ok := b.storeCall(call, trip); ok {
					b.cur = append(b.cur, pc)
					continue
				}
				b.execTransaction(call, trip)
			}
			// The success value is emitted by the caller as the Final path;
			// a return inside the body means later statements are dead.
			return true

		case *ast.AssignStmt:
			if b.assignment(st, trip) {
				return true
			}

		case *ast.ExprStmt:
			if b.execTransaction(st.X, trip) {
				return true
			}

		case *ast.IfStmt:
			if b.branch(st, trip) {
				return true
			}

		case *ast.RangeStmt:
			// The trip count is the length of whatever is ranged over, so it
			// becomes `len(testCase.<Field>)` in the generated test. A range
			// over something the case does not carry falls back to one.
			if b.execTransaction(st.X, trip) {
				return true
			}
			if b.block(st.Body.List, tripExpr(st.X)) {
				return true
			}
		}
	}
	return false
}

// assignment handles `x, err := c.store.Foo(...)` and
// `err = utils.ExecTransaction(...)`.
func (b *ctrlPathBuilder) assignment(st *ast.AssignStmt, trip string) bool {
	for _, rhs := range st.Rhs {
		if call, ok := rhs.(*ast.CallExpr); ok {
			if pc, ok := b.storeCall(call, trip); ok {
				b.cur = append(b.cur, pc)
				continue
			}
			if b.execTransaction(call, trip) {
				return true
			}
		}
	}
	return false
}

// execTransaction handles utils.ExecTransaction(ctx, c.store.GetDB(), fn).
//
// GetDB is an ARGUMENT, evaluated before the transaction body runs, so it is
// a step of the path like any other. Descending into the closure is what puts
// the body's calls after it.
func (b *ctrlPathBuilder) execTransaction(e ast.Expr, trip string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if !isExecTransaction(call) {
		return false
	}
	for _, a := range call.Args {
		if pc, ok := b.storeCallFrom(a, trip); ok {
			b.cur = append(b.cur, pc)
		}
		if lit, ok := a.(*ast.FuncLit); ok && lit.Body != nil {
			if b.block(lit.Body.List, trip) {
				return true
			}
		}
	}
	return false
}

// isExecTransaction reports whether a call is the transaction helper. The
// check is on the last path segment so a same-named method elsewhere cannot
// be mistaken for it.
func isExecTransaction(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "ExecTransaction"
}

// branch handles an if statement, which is where every path decision lives.
//
// Three shapes are recognised, and they cover the whole corpus:
//
//	err != nil        -> one path where the guarded call fails
//	<ident>           -> one path where the guarded call returns true
//	errors.Is(err, X) inside a failing guard -> the sentinel sub-branch
//
// The body of a recognised guard always returns, so execution continues
// after the if along the success edge. A guard whose body does NOT return is
// not a path decision we can honour, so its body is walked inline and the
// call is left unmarked: that is worse than knowing, but it is not a lie.
func (b *ctrlPathBuilder) branch(st *ast.IfStmt, trip string) bool {
	// `if err := c.store.X(...); err != nil {` puts the call in Init.
	if st.Init != nil {
		if as, ok := st.Init.(*ast.AssignStmt); ok {
			b.assignment(as, trip)
		}
	}

	if isErrNilCheck(st.Cond) && b.bodyReturns(st) {
		b.emitErrPaths(st)
		return false
	}

	if id, ok := bareIdent(st.Cond); ok && b.bodyReturns(st) {
		msg := businessErrorMsg(st.Body)
		b.emit(id+"True", b.withLast(func(pc *pathCall) {
			pc.BoolTrue = true
			pc.GuardMsg = msg
		}))
		return false
	}

	// Not a recognised decision point: fall through and treat the body as
	// unconditional. An Else branch is dropped, because honouring one edge
	// and not the other would over-claim.
	return b.block(st.Body.List, trip)
}

// emitErrPaths emits the paths taken when the guarded call returns an error.
//
// A guard that tests `errors.Is(err, sql.ErrNoRows)` has two distinct error
// paths, not one. That is the upsert shape the corpus uses in AssessQnA, and
// collapsing it to a single generic failure loses the recovery calls:
// emitting ONLY the generic path drops the Insert call the body really makes,
// and emitting ONLY the sentinel path claims the method stops there when it
// carries on. The sentinel edge therefore advances b.cur and lets the walk
// continue, while the generic edge is the one path that stops at the guard.
func (b *ctrlPathBuilder) emitErrPaths(st *ast.IfStmt) {
	name := "StoreError"
	if len(b.cur) > 0 {
		name = b.cur[len(b.cur)-1].Method + "Error"
	}
	branch, hasNoRows := b.errNoRowsBranch(st)

	if !hasNoRows {
		b.emit(name, b.errPathSnapshot(""))
		return
	}

	saved := b.snapshot()
	b.cur = b.errPathSnapshot("sql.ErrNoRows")
	b.block(branch.Body.List, "")
	// Only the non-sentinel error terminates here, so this is the sole path
	// built from the pre-guard prefix. b.cur keeps the recovered state so the
	// remaining statements continue along the success edge.
	b.emit(name, errBranchCalls(saved, ""))
}

// errPathSnapshot copies the accumulated calls with the guarded one marked as
// failing with lit.
func (b *ctrlPathBuilder) errPathSnapshot(lit string) []pathCall {
	return errBranchCalls(b.snapshot(), lit)
}

// errBranchCalls marks the last call of a prefix as failing with lit.
//
// The loop trip count is cleared: a call inside `for … range` fails on its
// FIRST iteration, because the guard returns out of the loop. Leaving the
// count on would ask gomock for N calls where the body makes one.
func errBranchCalls(prefix []pathCall, lit string) []pathCall {
	out := make([]pathCall, len(prefix))
	copy(out, prefix)
	if n := len(out); n > 0 {
		out[n-1].ErrBranch = true
		out[n-1].ErrLiteral = lit
		out[n-1].Times = ""
	}
	return out
}

// errNoRowsBranch returns the `if errors.Is(err, sql.ErrNoRows)` statement
// nested inside a failing guard, when there is exactly one.
func (b *ctrlPathBuilder) errNoRowsBranch(st *ast.IfStmt) (*ast.IfStmt, bool) {
	var found *ast.IfStmt
	ast.Inspect(st.Body, func(n ast.Node) bool {
		inner, ok := n.(*ast.IfStmt)
		if !ok || found != nil {
			return true
		}
		if b.isErrNoRowsCheck(inner.Cond) {
			found = inner
			return false
		}
		return true
	})
	return found, found != nil
}

// isErrNoRowsCheck recognises `errors.Is(err, sql.ErrNoRows)`.
func (b *ctrlPathBuilder) isErrNoRowsCheck(cond ast.Expr) bool {
	call, ok := cond.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Is" {
		return false
	}
	if len(call.Args) < 2 {
		return false
	}
	return b.fsetRender(call.Args[1]) == "sql.ErrNoRows"
}

// businessErrorMsg returns the literal a guard body returns, e.g.
// `return "", errors.New("Question number must be unique...")`.
func businessErrorMsg(body *ast.BlockStmt) string {
	if body == nil {
		return ""
	}
	for _, s := range body.List {
		rs, ok := s.(*ast.ReturnStmt)
		if !ok {
			continue
		}
		for _, r := range rs.Results {
			call, ok := r.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				continue
			}
			if len(call.Args) == 1 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return lit.Value
				}
			}
		}
	}
	return ""
}

// bodyReturns reports whether a guarded body ends the path.
//
// The last statement being a return is not enough, because a converted guard
// often reads as an if/else whose BOTH edges return:
//
//	if err != nil {
//	    if errors.Is(err, sql.ErrNoRows) {
//	        …recover…
//	    } else {
//	        return err
//	    }
//	}
//
// Requiring a trailing return statement here missed that shape entirely: the
// outer guard was treated as "not a path decision", so the enumerator walked
// its body unconditionally and emitted a path that both recovered and
// returned, which is no path at all.
func (b *ctrlPathBuilder) bodyReturns(st *ast.IfStmt) bool {
	if st.Body == nil || len(st.Body.List) == 0 {
		return false
	}
	return listReturns(st.Body.List)
}

// listReturns reports whether control definitely leaves a statement list.
//
// Statements before the deciding one are skipped rather than disqualifying it:
// `[logger.Debug(…), return x]` returns. A statement that falls through is
// followed, which is what makes a bare `if err := X(); err != nil { return err }`
// at the end of a block count as terminal despite having no else.
func listReturns(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	switch s := stmts[0].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.IfStmt:
		if ifReturns(s) {
			return true
		}
		return listReturns(stmts[1:])
	default:
		return listReturns(stmts[1:])
	}
}

// ifReturns reports whether every edge of a guard leaves the block.
func ifReturns(x *ast.IfStmt) bool {
	if x.Body == nil || !listReturns(x.Body.List) {
		return false
	}
	// The then-branch always returns and there is no else, so the guard is
	// terminal on its own.
	if x.Else == nil {
		return true
	}
	switch e := x.Else.(type) {
	case *ast.BlockStmt:
		return listReturns(e.List)
	case *ast.IfStmt:
		return ifReturns(e)
	default:
		return false
	}
}

// bareIdent returns the identifier of a `if <ident> {` condition.
func bareIdent(cond ast.Expr) (string, bool) {
	id, ok := cond.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// tripExpr renders the trip count for a range statement as source text, or ""
// when the ranged expression is not something a case can count.
func tripExpr(x ast.Expr) string {
	if x == nil {
		return ""
	}
	// `for index := range len(request.AnswerID)` and
	// `for _, q := range request.QnA` both range over request fields.
	var e ast.Expr = x
	if call, ok := x.(*ast.CallExpr); ok {
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "len" && len(call.Args) == 1 {
			e = call.Args[0]
		}
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	field := sel.Sel.Name
	if field == "" {
		return ""
	}
	return "len(testCase." + field + ")"
}

// storeCall recognises a `c.store.Foo(...)` right-hand side.
func (b *ctrlPathBuilder) storeCall(call *ast.CallExpr, trip string) (pathCall, bool) {
	return b.storeCallFrom(call, trip)
}

// storeCallFrom builds a pathCall from an expression that may be a store call.
func (b *ctrlPathBuilder) storeCallFrom(e ast.Expr, trip string) (pathCall, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return pathCall{}, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return pathCall{}, false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || !isStoreField(inner.Sel.Name) {
		return pathCall{}, false
	}
	pc := pathCall{Method: sel.Sel.Name, ArgCount: -1, Times: trip}
	if sig, ok := b.dbIface[sel.Sel.Name]; ok {
		pc.ArgCount = sig.ArgCount
	}
	for i, a := range call.Args {
		if i == 0 {
			continue // ctx
		}
		pc.Args = append(pc.Args, b.fsetRender(a))
	}
	return pc, true
}

// isStoreField recognises the store field on a controller receiver. The corpus
// uses more than one name: AddQuestion calls c.store, AssessQnA calls both
// c.store and c.userStore, and the suite mocks both through one field.
func isStoreField(name string) bool {
	return name == "store" || strings.HasSuffix(name, "Store")
}

func (b *ctrlPathBuilder) fsetRender(e ast.Expr) string {
	if b.fset == nil {
		return renderNode(e, nil)
	}
	return renderExpr(e, b.fset)
}
