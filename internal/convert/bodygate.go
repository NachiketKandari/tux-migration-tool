package convert

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/telemetry"
)

// The seam's parse-only body gate (validateBody) let bodies through that
// parse but cannot compile: an unused local, a leaked legacy C identifier,
// a rune literal, or a missing terminal return only surfaces on a wired
// target's Tier B, which the local syntax-only runs skip entirely. The
// checks below close that gap stage-time, over the same synthetic wrap
// validateBody uses, so line numbers come back body-relative.

// parseBodyWrapped parses the wrapped body with object resolution and
// returns the method's block plus the prelude line offset.
func parseBodyWrapped(body string) (*token.FileSet, *ast.BlockStmt, int, bool) {
	wrapped := bodyParseWrap(body)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "body.go", wrapped, 0)
	if err != nil {
		return nil, nil, 0, false // validateBody owns parse errors
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		return fset, fd.Body, wrapPrefixLines(wrapped, body), true
	}
	return nil, nil, 0, false
}

// unusedLocalErrs keeps go/types' declared-and-not-used findings for the
// body. Unresolvable imports (models, the store receiver) produce their own
// errors and are ignored; the compiler's unused rule is independent of
// them.
func unusedLocalErrs(body string) []string {
	wrapped := bodyParseWrap(body)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "body.go", wrapped, 0)
	if err != nil {
		return nil
	}
	offset := wrapPrefixLines(wrapped, body)
	var errs []string
	conf := types.Config{
		Importer: importer.Default(),
		Error: func(err error) {
			var terr types.Error
			if !errors.As(err, &terr) || !strings.Contains(terr.Msg, "declared and not used") {
				return
			}
			errs = append(errs, fmt.Sprintf("line %d: %s", fset.Position(terr.Pos).Line-offset, terr.Msg))
		},
	}
	_, _ = conf.Check("controller", fset, []*ast.File{f}, nil)
	return errs
}

// repairUnusedLocals inserts the `_ = name` the compiler's declared-and-not-used
// rule demands. A local the model declares and never reads is not a translation
// defect worth a retry note: `_ = name` is already the pipeline's documented
// escape hatch for a value that must be kept, and this is the same go/types
// finding unusedLocalErrs reports — so the pipeline can own the shape instead
// of asking the model to. The statement lands directly after the declaration,
// the name counts as used, the body compiles, and the gate accepts the model's
// code on its first attempt.
//
// Conservative by construction: an insertion only happens at a statement
// boundary (end of line, or before a trailing comment). A `:=` that is an
// if/for/switch init has no legal insertion point of its own — placing one
// there would rewrite the wrapper header — so it is left for the gate, exactly
// as before. Parse failures and unresolved names return the body unchanged, so
// the repair can only ever remove a gate note, never add one. Idempotent: once
// `_ = x` is present, x is used and a second pass finds nothing.
func repairUnusedLocals(body string) string {
	wrapped := bodyParseWrap(body)
	base := strings.Index(wrapped, body)
	if base < 0 {
		return body
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "body.go", wrapped, 0)
	if err != nil {
		return body
	}
	var block *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			block = fd.Body
			break
		}
	}
	if block == nil {
		return body
	}

	// Collect the compiler's unused-local findings (the same filter
	// unusedLocalErrs reports); the type checker's own Defs table then names
	// each one, with the message as the fallback.
	type finding struct {
		pos  token.Pos
		name string
	}
	var findings []finding
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{
		Importer: importer.Default(),
		Error: func(err error) {
			var terr types.Error
			if !errors.As(err, &terr) || !strings.Contains(terr.Msg, "declared and not used") {
				return
			}
			findings = append(findings, finding{pos: terr.Pos, name: unusedName(terr.Msg)})
		},
	}
	_, _ = conf.Check("controller", fset, []*ast.File{f}, info)
	if len(findings) == 0 {
		return body
	}
	byPos := make(map[token.Pos]string, len(info.Defs))
	for id := range info.Defs {
		byPos[id.Pos()] = id.Name
	}

	insertions := map[int][]string{}
	for _, fd := range findings {
		name := byPos[fd.pos]
		if name == "" {
			name = fd.name
		}
		if name == "" || name == "_" {
			continue
		}
		decl := deepestStmt(block, fd.pos)
		if decl == nil {
			continue
		}
		end := fset.Position(decl.End()).Offset
		if end < base || end > base+len(body) || !stmtBoundary(wrapped, end) {
			continue
		}
		insertions[end-base] = append(insertions[end-base],
			lineIndent(wrapped, fset.Position(decl.Pos()).Offset)+"_ = "+name)
	}
	if len(insertions) == 0 {
		return body
	}
	offsets := make([]int, 0, len(insertions))
	for off := range insertions {
		offsets = append(offsets, off)
	}
	sort.Ints(offsets)
	for i := len(offsets) - 1; i >= 0; i-- { // right-to-left keeps offsets valid
		off := offsets[i]
		body = body[:off] + "\n" + strings.Join(insertions[off], "\n") + body[off:]
	}
	return body
}

// unusedName extracts the identifier from go/types' "declared and not used: x".
func unusedName(msg string) string {
	const prefix = "declared and not used"
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg[i+len(prefix):]), ":"))
}

// deepestStmt returns the innermost statement containing pos — the statement
// that declares the name. Inspect is pre-order, so the last statement whose
// span contains pos is the deepest.
func deepestStmt(root ast.Node, pos token.Pos) ast.Stmt {
	var best ast.Stmt
	ast.Inspect(root, func(n ast.Node) bool {
		if s, ok := n.(ast.Stmt); ok && s.Pos() <= pos && pos < s.End() {
			best = s
		}
		return true
	})
	return best
}

// stmtBoundary reports whether off ends a statement in wrapped: only
// whitespace to the end of the line, or a trailing comment. A `:=` that is an
// if/for/switch init is followed by `; …` and is deliberately not a boundary.
func stmtBoundary(wrapped string, off int) bool {
	i := off
	for i < len(wrapped) && (wrapped[i] == ' ' || wrapped[i] == '\t') {
		i++
	}
	if i >= len(wrapped) {
		return false
	}
	switch wrapped[i] {
	case '\n', '\r':
		return true
	}
	return strings.HasPrefix(wrapped[i:], "//") || strings.HasPrefix(wrapped[i:], "/*")
}

// lineIndent returns the leading whitespace of the line holding off.
func lineIndent(wrapped string, off int) string {
	start := off
	for start > 0 && wrapped[start-1] != '\n' {
		start--
	}
	i := start
	for i < len(wrapped) && (wrapped[i] == ' ' || wrapped[i] == '\t') {
		i++
	}
	return wrapped[start:i]
}

// nonConstFormatErrs rejects fmt.Errorf calls whose format argument is not
// a string literal: go vet (Tier B) fails those, and a variable containing
// format verbs corrupts the error. errors.New is the variable-message home.
func nonConstFormatErrs(body string) []string {
	fset, block, offset, ok := parseBodyWrapped(body)
	if !ok {
		return nil
	}
	var errs []string
	ast.Inspect(block, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Errorf" || len(call.Args) == 0 {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return true
		}
		errs = append(errs, fmt.Sprintf("line %d: fmt.Errorf uses a non-constant format string — use errors.New(<message>) for a variable message",
			fset.Position(call.Pos()).Line-offset))
		return true
	})
	return errs
}

// runeLiteralErrs rejects rune literals: FML flags and status values are
// strings/ints in Go, and `cFlag == 'Y'` (a rune) never compares against a
// string — the classic transliteration slip.
func runeLiteralErrs(body string) []string {
	fset, block, offset, ok := parseBodyWrapped(body)
	if !ok {
		return nil
	}
	var errs []string
	ast.Inspect(block, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.CHAR {
			return true
		}
		errs = append(errs, fmt.Sprintf("line %d: rune literal %s — use a double-quoted string (\"Y\") or an int", fset.Position(lit.Pos()).Line-offset, lit.Value))
		return true
	})
	return errs
}

// fixRuneLiterals rewrites plain single-letter rune literals to their
// string form ('Y' → "Y") before the gates run: the Pro*C char-flag
// transliteration slip is mechanical, and making the model burn a retry on
// it is waste. Only 3-byte letter literals convert (escapes like '\n' and
// digit/symbol runes may be intentional int math), and never inside an
// arithmetic or index/slice expression — those stay for runeLiteralErrs to
// reject so the model decides string vs int. Parse failures pass through
// untouched (the parse gate owns them).
func fixRuneLiterals(body string) string {
	wrapped := bodyParseWrap(body)
	base := strings.Index(wrapped, body)
	if base < 0 {
		return body
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "body.go", wrapped, 0)
	if err != nil {
		return body
	}
	unsafePos := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if isArithOp(x.Op) {
				markCharLits(x, unsafePos)
			}
		case *ast.IndexExpr:
			markCharLits(x.Index, unsafePos)
		case *ast.SliceExpr:
			// Missing bounds are legal Go (s[i:], s[:n], s[:]) and their
			// AST fields are nil — Inspect(nil) panics.
			if x.Low != nil {
				markCharLits(x.Low, unsafePos)
			}
			if x.High != nil {
				markCharLits(x.High, unsafePos)
			}
		}
		return true
	})
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.CHAR || unsafePos[lit.Pos()] {
			return true
		}
		v := lit.Value
		if len(v) != 3 || !isASCIILetter(v[1]) {
			return true
		}
		off := fset.Position(lit.Pos()).Offset - base
		edits = append(edits, edit{start: off, end: off + len(v), text: `"` + string(v[1]) + `"`})
		return true
	})
	for i := len(edits) - 1; i >= 0; i-- { // right-to-left keeps offsets valid
		e := edits[i]
		if e.start < 0 || e.end > len(body) {
			continue
		}
		body = body[:e.start] + e.text + body[e.end:]
	}
	return body
}

// isArithOp reports whether a binary operator computes a value (any context
// where a letter rune could be intended as an int) rather than compares.
func isArithOp(op token.Token) bool {
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.GTR, token.LEQ, token.GEQ, token.LAND, token.LOR:
		return false
	}
	return true
}

// markCharLits records every rune literal inside n as unsafe to rewrite.
func markCharLits(n ast.Node, mark map[token.Pos]bool) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.CHAR {
			mark[lit.Pos()] = true
		}
		return true
	})
}

// isASCIILetter reports whether b is a plain ASCII letter (the rune literal
// shapes the flag fix targets).
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// terminatingReturnErr rejects a body whose last statement cannot terminate
// the method: named results do not make falling off the end legal.
func terminatingReturnErr(body string) []string {
	fset, block, offset, ok := parseBodyWrapped(body)
	if !ok || len(block.List) == 0 {
		return nil
	}
	last := block.List[len(block.List)-1]
	if terminates(last) {
		return nil
	}
	return []string{fmt.Sprintf("line %d: the body can fall off the end — end every path with return data, err / return nil, err",
		fset.Position(last.End()).Line-offset)}
}

// terminates reports whether s is a terminating statement (the Go spec's
// conservative subset the controller template produces).
func terminates(s ast.Stmt) bool {
	switch x := s.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		if call, ok := x.X.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" {
				return true
			}
		}
	case *ast.BlockStmt:
		return len(x.List) > 0 && terminates(x.List[len(x.List)-1])
	case *ast.IfStmt:
		if x.Else == nil || len(x.Body.List) == 0 {
			return false
		}
		return terminates(x.Body.List[len(x.Body.List)-1]) && terminates(x.Else)
	case *ast.ForStmt:
		return x.Cond == nil // for {} never falls through
	case *ast.LabeledStmt:
		return terminates(x.Stmt)
	}
	return false
}

// undeclaredIdentErrs reports identifiers the parser cannot resolve to a
// declaration in the body or the wrap — exactly the leaked legacy C names
// (`c_flag`, `c_errmsg`) and typos. allow carries the package-level seam
// names the generated package declares outside the body (fn stubs).
func undeclaredIdentErrs(body string, allow map[string]bool) []string {
	fset, block, offset, ok := parseBodyWrapped(body)
	if !ok {
		return nil
	}
	// Field/method names, struct-literal keys and labels are not uses.
	skip := map[*ast.Ident]bool{}
	ast.Inspect(block, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			skip[x.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok {
				skip[id] = true
			}
		case *ast.LabeledStmt:
			skip[x.Label] = true
		case *ast.BranchStmt:
			if x.Label != nil {
				skip[x.Label] = true
			}
		}
		return true
	})
	var errs []string
	seen := map[string]bool{}
	ast.Inspect(block, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Obj != nil || skip[id] || id.Name == "_" || seen[id.Name] {
			return true
		}
		seen[id.Name] = true
		if identAllowed(id.Name, allow) {
			return true
		}
		errs = append(errs, fmt.Sprintf("line %d: undefined identifier %q — legacy C names do not exist here; read request.<Field> or declare a local",
			fset.Position(id.Pos()).Line-offset, id.Name))
		return true
	})
	return errs
}

// identAllowed is the identifier allowlist for the body wrap: predeclared
// names plus the packages the controller file assembly can import (context,
// models, logger, errors, fmt, time, sqlx, utils) and the receiver.
func identAllowed(name string, allow map[string]bool) bool {
	if types.Universe.Lookup(name) != nil || allow[name] {
		return true
	}
	switch name {
	case "s", "context", "models", "logger", "errors", "fmt", "time", "sqlx", "utils":
		return true
	}
	return false
}

// uncapturedStoreErrs rejects store calls whose result is discarded: the
// model's bare-call shape (`s.store.UpdateDummyProfile(c, tx, …)`) drops the
// returned error, and the `if err != nil` that follows then checks a stale
// err — a real logic drop Tier A cannot see. Accepted bodies assign or test
// the result (`err = s.store.X(…)`, `rows, err := s.store.X(…)`,
// `if err := s.store.X(…); err != nil`).
func uncapturedStoreErrs(body, receiver string) []string {
	if receiver == "" {
		return nil
	}
	fset, block, offset, ok := parseBodyWrapped(body)
	if !ok {
		return nil
	}
	recv := strings.TrimSuffix(receiver, ".")
	var errs []string
	ast.Inspect(block, func(n ast.Node) bool {
		es, isStmt := n.(*ast.ExprStmt)
		if !isStmt {
			return true
		}
		call, isCall := es.X.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || selectorBase(sel.X) != recv {
			return true
		}
		errs = append(errs, fmt.Sprintf("line %d: store call result discarded — capture it and check the error (rows, err := %s.%s(…) / err = %s.%s(…))",
			fset.Position(es.Pos()).Line-offset, recv, sel.Sel.Name, recv, sel.Sel.Name))
		return true
	})
	return errs
}

// selectorBase renders a selector chain's receiver (`s.store` for
// `s.store.GetDB`); "" for anything else.
func selectorBase(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if base := selectorBase(x.X); base != "" {
			return base + "." + x.Sel.Name
		}
	}
	return ""
}

// stubNames lists the fn-stub helper functions the generated package
// declares (fnstubs.go): bodies may call them, the wrap cannot resolve them.
func stubNames(opts Options) map[string]bool {
	out := map[string]bool{}
	if opts.Plan == nil {
		return out
	}
	for _, st := range opts.Plan.Stubs {
		out[common.CamelLowerGo(st.Fn)] = true
	}
	return out
}

// repairControllerBody is the extract-side deterministic repair for
// controller bodies: cleanBody, the arm-wrapper unwrap, the unused-local
// neutralizer, and the terminal return. The repairs run before the gates so
// the model is not asked to fix shapes the pipeline can own deterministically.
// The rune-literal fix is parse-dependent, so it re-runs after the structural
// strips: a body that re-adds the leading arm header does not parse,
// cleanBody's fix no-ops on it, and without the second pass the gate burns a
// retry on literals the pipeline could have owned.
func repairControllerBody(ctx context.Context, unit, axisVar, content string) string {
	body := cleanBody(content)
	stripped := false
	if fixed, ok := stripLeadingArmChain(body); ok {
		telemetry.Log(ctx).Info("leading arm chain stripped", "unit", unit)
		body = fixed
		stripped = true
	}
	if axisVar != "" {
		if fixed, ok := repairArmWrapper(body, axisVar); ok {
			telemetry.Log(ctx).Info("arm wrapper unwrapped", "unit", unit, "axis", axisVar)
			body = fixed
			stripped = true
		}
	}
	if stripped {
		if fixed := fixRuneLiterals(body); fixed != body {
			telemetry.Log(ctx).Info("rune literals rewritten after strip", "unit", unit)
			body = fixed
		}
	}
	// A local the compiler calls unused is the pipeline's shape to own
	// (`_ = name` is the prompt's own escape hatch), not a retry note.
	if fixed := repairUnusedLocals(body); fixed != body {
		telemetry.Log(ctx).Info("unused locals neutralized", "unit", unit)
		body = fixed
	}
	return ensureTerminalReturn(body)
}

// stripLeadingArmChain removes a leading branch-continuation header the
// model occasionally re-adds around a scenario arm — `else if (…) { … }`,
// `} else if (…) { … }`, `else { … }`, `} else { … }`. A Go body can never
// legally start with `else`, and the method's own condition already
// represents the arm, so the header and its matching closing brace drop;
// the inner text plus anything after the block becomes the body. The piece
// must match exactly (header + one block); anything else is returned
// unchanged for the gate to reject.
func stripLeadingArmChain(body string) (string, bool) {
	t := strings.TrimSpace(body)
	rest := t
	if strings.HasPrefix(rest, "}") {
		rest = strings.TrimSpace(rest[1:])
	}
	if !strings.HasPrefix(rest, "else") {
		return body, false
	}
	rest = strings.TrimSpace(rest[len("else"):])
	if rest == "" {
		return body, false
	}
	if strings.HasPrefix(rest, "if") {
		rest = strings.TrimSpace(rest[len("if"):])
		if rest == "" {
			return body, false
		}
		if rest[0] == '(' {
			_, close, ok := balancedParens(rest, 0)
			if !ok {
				return body, false
			}
			rest = strings.TrimSpace(rest[close+1:])
		} else {
			open := strings.IndexByte(rest, '{')
			if open < 0 {
				return body, false
			}
			rest = rest[open:]
		}
	}
	if rest == "" || rest[0] != '{' {
		return body, false
	}
	inner, next, ok := stringBraceBody(rest, 0)
	if !ok {
		return body, false
	}
	tail := strings.TrimSpace(rest[next:])
	if tail == "" {
		return strings.TrimSpace(inner), true
	}
	return strings.TrimRight(inner, " \t\n") + "\n" + tail, true
}

// ensureTerminalReturn appends the terminal `return data, err` when the
// body's last statement cannot terminate the method: named results do not
// make falling off the end legal, and the return is a no-op on the paths
// that already returned. Idempotent.
func ensureTerminalReturn(body string) string {
	if len(terminatingReturnErr(body)) == 0 {
		return body
	}
	return strings.TrimRight(body, " \t\n") + "\nreturn data, err"
}

// repairArmWrapper removes a leading `if <axisVar> == '<value>' { … }`
// wrapper the model occasionally re-adds around a scenario arm (the method
// already represents that arm) and appends the terminal `return data, err`
// the unwrap exposes. Returns the body unchanged when no wrapper matches.
func repairArmWrapper(body, axisVar string) (string, bool) {
	if axisVar == "" {
		return body, false
	}
	wrapped := bodyParseWrap(body)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "body.go", wrapped, 0)
	if err != nil {
		return body, false
	}
	var block *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			block = fd.Body
			break
		}
	}
	if block == nil || len(block.List) != 1 {
		return body, false
	}
	ifs, ok := block.List[0].(*ast.IfStmt)
	if !ok || ifs.Else != nil || ifs.Init != nil || !condMentions(ifs.Cond, axisVar) {
		return body, false
	}
	start := fset.Position(ifs.Body.Lbrace).Offset + 1
	end := fset.Position(ifs.Body.Rbrace).Offset
	if start < 0 || end > len(wrapped) || start >= end {
		return body, false
	}
	inner := strings.TrimSpace(wrapped[start:end])
	if inner == "" {
		return body, false
	}
	if len(terminatingReturnErr(inner)) > 0 {
		inner += "\nreturn data, err"
	}
	return inner, true
}

// condMentions reports whether the condition references the identifier.
func condMentions(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// wrapPrefixLines counts the wrapper's prelude lines so reported positions
// map back to the body's own line numbers.
func wrapPrefixLines(wrapped, body string) int {
	i := strings.Index(wrapped, body)
	if i < 0 {
		return 0
	}
	return strings.Count(wrapped[:i], "\n")
}
