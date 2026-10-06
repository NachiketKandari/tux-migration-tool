// Per-function fact extraction (PRD-2026-09-09 GT-D3): gentest reads the
// converted Go code itself — never the conversion IR — so it works on any
// tree, including hand-edited ones. Extraction is parse-only (no type
// checking): shapes are recognized by the reference conventions (store
// receivers, `query := `+backtick literals, sqlx call names, request
// selectors).
package testgen

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Param is one function parameter (name + type as written).
type Param struct {
	Name string
	Type string
}

// dbFact is one store method's extracted shape.
type dbFact struct {
	Name       string
	CtxName    string
	Params     []Param
	Query      string
	Tables     []string // FROM list in source order (DML: target table)
	Shape      string   // multi | single | scalar | dml
	RowType    string   // models.ItemDetails (as written)
	Scalar     string   // int64 / string / float64
	Args       []string // call args after the query arg, rendered verbatim
	IsTx       bool     // takes tx *sqlx.Tx (tx-variant: runs on tx, not g.db)
	Recv       string   // sqlx receiver: "g.db" | "tx" (as written)
	ReturnType string   // first non-error result type (tx scalar reads)
	// NoRowsContract classifies what the method does when the query call
	// fails, which is what a no-rows mock actually exercises:
	//
	//	propagate — `if err != nil { return nil, err }`, so the caller sees
	//	            sql.ErrNoRows and the no-rows case must expect it
	//	tolerate  — `if err != nil { return &T{}, nil }` or an explicit
	//	            errors.Is(err, sql.ErrNoRows) branch, so the zero value
	//	            arrives with a nil error
	//
	// This was previously assumed to be `tolerate` for every read, which is
	// wrong for the common converted shape: every corpus GetContext read
	// propagates err unchanged, so the generated no-rows case asserted a nil
	// error against a method that returns one.
	NoRowsContract string
	// NoRowsError is the error a DML method returns when RowsAffected == 0,
	// as written in its body: a domain message ("unable to add the question")
	// for the count-check shape, or "sql: no rows in result set" for the
	// `return sql.ErrNoRows` shape. Empty means the method tolerates zero
	// rows (the DELETE-tx variant discards the result) and no case is emitted.
	NoRowsError string
}

// storeCall is one dependency call inside a controller body.
type storeCall struct {
	Method string
	Args   []string // rendered verbatim, ctx arg dropped
	// ArgCount is the call site's total argument count including ctx, or -1
	// when the store interface's declaration for Method could not be read.
	// The template needs it because a no-argument store method (GetDB())
	// takes no gomock.Any() matcher: emitting one was over-arity, and five
	// occurrences of it appeared in the corpus's log-route output.
	//
	// -1 means "unknown", and the template then keeps the matcher — guessing
	// the arity from the call site alone is what this field replaces.
	ArgCount int
}

// dbIfaceSig is one store interface method's declared shape. The bodies are
// not enough: GetDB() has no body worth reading (it returns a field) yet its
// EXPECT must both take no matcher and hand back a live *sqlx.DB.
type dbIfaceSig struct {
	ArgCount int    // declared parameter count, ctx included
	Result   string // first result type as written (*sqlx.DB, []*models.X, …)
	// Results is the declared result COUNT. A DML method that returns only
	// `error` takes Return(nil), not Return(nil, nil) — the generated
	// controller suite failed with "wrong number of arguments to Return for
	// MockDummyProfileStore.EditMarks: got 2, want 1". The first result's TYPE
	// cannot answer this: `error` is a type like any other, and knowing it is
	// the only result is what the arity depends on.
	Results int
}

// extractDBIface parses the db layer's interface declarations into per-method
// signatures. Arity and result type are properties of the declaration, not of
// any body, so reading them here is data rather than inference.
func extractDBIface(serviceDir string) map[string]dbIfaceSig {
	out := map[string]dbIfaceSig{}
	dir := filepath.Join(serviceDir, "db")
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				for _, m := range it.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 {
						continue
					}
					sig := dbIfaceSig{ArgCount: -1}
					if ft.Params != nil {
						sig.ArgCount = ft.Params.NumFields()
					}
					if ft.Results != nil {
						sig.Results = ft.Results.NumFields()
						if sig.Results > 0 {
							sig.Result = renderExpr(ft.Results.List[0].Type, fset)
						}
					}
					out[m.Names[0].Name] = sig
				}
			}
		}
	}
	return out
}

// ctrlFact is one controller endpoint's extracted shape.
type ctrlFact struct {
	Name         string
	CtxName      string
	RequestType  string // models.ItemRequest
	ResponseType string
	StoreCalls   []storeCall
	// Paths is the control-flow path model of this body: one entry per way
	// through it, each carrying the store calls that happen, which one fails,
	// and how many times a looped call runs. It is read from the body, not
	// from a captured log, so it is the same on both routes.
	Paths       []ctrlPath
	Passthrough bool // body returns the store result without field mapping
	// ScalarResponse is a basic-literal value the body returns on SUCCESS,
	// e.g. `return "Marks Edited Successfully", nil`. It is only set when
	// ResponseType is a basic scalar, where a struct-mapping expectation is
	// meaningless and the only alternative was the zero value.
	//
	// Without it the deterministic route asserted "" against a method that
	// returns a constant string, because the tool knew the response TYPE and
	// nothing about the VALUE. The log route already supplies the value; this
	// is the third source, and the body's own literal outranks a guess.
	ScalarResponse string
	Src            string // verbatim function source (LLM prompt input)
}

// scalarSuccessReturn finds the literal a controller body returns on success:
// a `return <basic literal>, nil`. The nil error is what distinguishes it from
// the method's failure returns, which pair the same shape with a real error.
//
// It reports false for anything else — a variable, a call, a composite literal,
// or a model response — so an unrecognised body keeps the zero value rather
// than gaining an invented expectation.
func scalarSuccessReturn(fd *ast.FuncDecl, fset *token.FileSet, responseType string) (string, bool) {
	if !isScalarType(responseType) {
		return "", false
	}
	for _, r := range returnsIn(fd.Body) {
		if len(r.results) < 2 || !isNilIdent(r.results[len(r.results)-1]) {
			continue
		}
		lit := r.results[0]
		switch v := lit.(type) {
		case *ast.BasicLit:
			return renderExpr(v, fset), true
		case *ast.Ident:
			// A named constant: true/false and the predeclared zero values.
			switch v.Name {
			case "true", "false", "nil", "iota":
				return v.Name, true
			}
		}
	}
	return "", false
}

// handlerFact is one gin handler's extracted shape.
type handlerFact struct {
	Name        string
	RequestType string
	// CtrlCall is the controller method the handler actually invokes, which
	// is not always the handler's own name: handler.GetCustomerDummyProfile
	// calls controller.GetCustomerRP. The generated EXPECT() has to name the
	// method that exists on the mock.
	CtrlCall string
	// CtrlCallArgs is the controller call's arguments after ctx, as written.
	// It is read from the call site rather than inferred from the handler's
	// local `var request models.X`, because those two disagree: DisplayMarks
	// binds a request the controller method never receives, and emitting
	// EXPECT().DisplayMarks(ctx, &request) was over-arity.
	CtrlCallArgs []string
	// CtrlBranches is every controller call site in this handler, each with the
	// request-field comparison that guards it.
	//
	// A handler may route one request field to SEVERAL controller methods:
	//
	//	if request.RequestType == "B" { data, err = f.controller.ViewQuestions(c, &request) }
	//	if request.RequestType == "L" { data, err = f.controller.ListSection(c, &request) }
	//
	// CtrlCall keeps only one of them, so the generated EXPECT names a method
	// the request might not reach. The fixture value is then chosen without
	// knowing which branch it selects, and the run dies with
	//
	//	Unexpected call to Mock…Controller.ViewQuestions: there are no
	//	expected calls of the method "ViewQuestions"
	//
	// Each branch's guard is what lets the renderer pick a value that takes
	// the branch the EXPECT was written for.
	CtrlBranches []ctrlBranch
	// Envelope is every response this body writes, read from its own source.
	// It decides which cases exist: a body that never writes FailureJSON has
	// no controller-error case to generate.
	Envelope []envelopeCall
}

// ctrlCallArgsOf returns a controller call's arguments after ctx, as written.
func ctrlCallArgsOf(call *ast.CallExpr, fset *token.FileSet) []string {
	var args []string
	for i, a := range call.Args {
		if i == 0 {
			continue // ctx
		}
		args = append(args, renderExpr(a, fset))
	}
	return args
}

// blockHasControllerCall reports whether a block reaches a controller call.
func blockHasControllerCall(b *ast.BlockStmt) bool {
	found := false
	ast.Inspect(b, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "controller" {
			found = true
			return false
		}
		return true
	})
	return found
}

// guardFieldAndValue splits a routing condition into the field it reads and the
// literal it compares against.
func guardFieldAndValue(cond ast.Expr) (field, value string) {
	if un, ok := cond.(*ast.UnaryExpr); ok && un.Op == token.NOT {
		cond = un.X
	}
	be, ok := cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.EQL {
		return "", ""
	}
	sel, ok := be.X.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "request" {
		return "", ""
	}
	lit, ok := be.Y.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", ""
	}
	unq, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", ""
	}
	return sel.Sel.Name, unq
}

// isRequestFieldCompare reports whether a condition is
// `request.<Field> == "<Value>"` or its negation.
func isRequestFieldCompare(cond ast.Expr) bool {
	if un, ok := cond.(*ast.UnaryExpr); ok && un.Op == token.NOT {
		cond = un.X
	}
	be, ok := cond.(*ast.BinaryExpr)
	return ok && be.Op == token.EQL && isRequestField(be.X) && isStringLit(be.Y)
}

func isRequestField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "request"
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

// ctrlBranch is one controller call site and the request-field comparison that
// guards it.
type ctrlBranch struct {
	Method string
	Args   []string // arguments after ctx, as written
	// Field is the request field the guard compares, e.g. "RequestType".
	Field string
	// Value is the literal the guard compares it against, e.g. "B".
	Value string
	// Guarded is false for an unguarded call — a handler that calls exactly one
	// controller method with no branch in front of it.
	Guarded bool
}

// envelopeCall is one response a handler body writes, as the gCtx method that
// writes it. GinContext owns the status code, so the generated case asserts the
// envelope the body actually chose instead of a status table the template
// invented.
type envelopeCall struct {
	// Method is the GinContext method: SuccessJSON, FailureJSON, BadRequestJSON.
	Method string
	// Reached is the path condition under which the body writes it, as written
	// in the source ("err != nil", "" when unconditional).
	Reached string
}

// handlerEnvelope reads which response envelopes a handler body writes, and the
// condition guarding each one.
//
// The template used to assert a fixed 500/204/200 table, but the mapping lives
// in GinContext, not in the handler: the corpus answers Success→200,
// Failure→404 and BadRequest→400, and every generated case asserted the wrong
// one. Reading the body is also what distinguishes a genuine controller error
// from a request the binder refused before the controller ran.
func handlerEnvelope(fd *ast.FuncDecl, fset *token.FileSet) []envelopeCall {
	var out []envelopeCall
	if fd == nil || fd.Body == nil {
		return out
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "SuccessJSON", "FailureJSON", "BadRequestJSON":
			out = append(out, envelopeCall{Method: sel.Sel.Name})
		}
		return true
	})
	return out
}

// fieldInfo is one struct field of the models layer.
type fieldInfo struct {
	Name string
	Type string
	JSON string
	DB   string
	// Validate is the field's validator constraint as written, from either
	// the `validate` or `binding` tag. A handler binds its request through
	// c.BindJSON, which enforces it, so a synthesized value that violates it
	// is refused with 400 before the controller is called — which is why the
	// value has to be chosen against the tag rather than against the type.
	Validate string
}

// modelsInfo is the service's models inventory (model/ and models/ both).
type modelsInfo struct {
	Structs map[string][]fieldInfo
}

// ctrlIfaceSig is one controller interface method's request/response shape.
type ctrlIfaceSig struct {
	Request  string // models.ItemHistoryRequest (pointer stripped)
	Response string // []*models.ItemHistoryResponse (first result, as written)
	// Results is the declared result COUNT, for the same reason the store
	// interface records it: a handler EXPECT's Return payload has to match
	// the method's arity, and `[]any{nil, nil}` is wrong for a controller
	// method that returns only `error`. The first result's TYPE cannot answer
	// it — `error` is a type like any other.
	Results int
}

// layerFacts is one layer directory's extraction outcome.
type layerFacts struct {
	DB      map[string]*dbFact
	Ctrl    map[string]*ctrlFact
	Handler map[string]*handlerFact
	DBCtor  string
	// DBCtorParams are the db constructor's parameters as written (name +
	// type, declaration order). The names are what decide which argument
	// receives the live handle: a service may expose more than one sqlx.DB
	// (a read handle and a write handle) and only the names tie a call's
	// receiver back to the parameter it came from. The count alone was not
	// enough — a two-handle ctor gave the connection to the wrong argument
	// and every read nil-panicked.
	DBCtorParams []Param
	CtrlCtor     string
	// CtrlCtorParams is the controller constructor's parameters as written.
	// A converted controller often takes more than its own store — the corpus
	// ctor is NewDummyProfileController(store db.DummyProfileStore, userStore
	// commonDB.UserStore) — and emitting only the first produced "not enough
	// arguments in call" for every controller suite.
	CtrlCtorParams []Param
	HandlerCtor    string
}

// sourceFiles lists the layer's non-test, non-mock .go files.
func sourceFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" ||
			strings.HasSuffix(name, "_test.go") ||
			strings.HasSuffix(name, "_mock.go") || strings.HasPrefix(name, "mock_") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// testFiles lists the layer's *_test.go files.
func testFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".go" && strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// extractLayer parses one layer directory into facts; unparseable files are
// skipped (the scan already warned).
func extractLayer(dir string, layer string, dbIface map[string]dbIfaceSig) *layerFacts {
	lf := &layerFacts{DB: map[string]*dbFact{}, Ctrl: map[string]*ctrlFact{}, Handler: map[string]*handlerFact{}}
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			switch {
			case layer == "db":
				if fd.Recv != nil {
					if f := extractDBFact(fd, fset); f != nil {
						lf.DB[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 {
					if lf.DBCtor == "" {
						lf.DBCtor = fd.Name.Name
						lf.DBCtorParams = paramsOf(fd, fset)
					}
				}
			case layer == "controller":
				if fd.Recv != nil {
					if f := extractCtrlFact(fd, fset, dbIface); f != nil {
						lf.Ctrl[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 && lf.CtrlCtor == "" {
					lf.CtrlCtor = fd.Name.Name
					lf.CtrlCtorParams = paramsOf(fd, fset)
				}
			case layer == "handler":
				if fd.Recv != nil {
					if f := extractHandlerFact(fd, fset); f != nil {
						lf.Handler[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 && lf.HandlerCtor == "" {
					lf.HandlerCtor = fd.Name.Name
				}
			}
		}
	}
	return lf
}

// extractCtrlIface parses the controller layer's interface declarations into
// per-method signatures. Handler tests need the controller's response type;
// the interface declaration is the authoritative fallback (it carries the
// type even when bodies are still the LLM seam), so the handler gate can stay
// deterministic instead of degrading to unsupported.
func extractCtrlIface(serviceDir string) map[string]ctrlIfaceSig {
	out := map[string]ctrlIfaceSig{}
	dir := filepath.Join(serviceDir, "controller")
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				for _, m := range it.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 {
						continue
					}
					sig := ctrlIfaceSig{}
					if ft.Params != nil && ft.Params.NumFields() > 0 {
						last := ft.Params.List[len(ft.Params.List)-1]
						if rt := renderExpr(last.Type, fset); strings.HasPrefix(rt, "*models.") {
							sig.Request = strings.TrimPrefix(rt, "*")
						}
					}
					if ft.Results != nil && ft.Results.NumFields() > 0 {
						sig.Results = ft.Results.NumFields()
						sig.Response = renderExpr(ft.Results.List[0].Type, fset)
					}
					if sig.Response != "" {
						out[m.Names[0].Name] = sig
					}
				}
			}
		}
	}
	return out
}

// extractModels parses the service's models dir (model/ or models/).
// backtickArg returns the first raw-string literal passed as an argument to a
// call, or "" if there is none.
//
// A raw string is the SQL convention in this codebase, and an interpreted
// "SELECT …" is not, so the backtick is the discriminator rather than any
// attempt to recognise SQL by shape.
func backtickArg(call *ast.CallExpr) string {
	for _, a := range call.Args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
			continue
		}
		if q, err := strconv.Unquote(lit.Value); err == nil {
			return q
		}
	}
	return ""
}

// declaresFunc reports whether the package in dir declares a top-level function
// with this name.
//
// It exists because the handler template's SetupSuite used to call
// utils.RegisterValidations unconditionally. That is an assumption about the
// target, and it is wrong for any repo whose pkg/utils has no
// validator-registration helper: the suite references a symbol that does not
// exist and will not compile. The declaration has to be READ, not assumed.
//
// A package that is absent, empty, or unparseable reports false. That is the
// safe direction: emitting no call compiles against a package that has the
// symbol only if something else needs it, whereas emitting a call to a missing
// symbol never compiles. It also means the hardcoded pkg/utils path (a separate
// known limitation) degrades to "no validation setup" instead of a crash.
func declaresFunc(dir, name string) bool {
	fset := token.NewFileSet()
	for _, f := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, f), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name != nil && fd.Name.Name == name {
				return true
			}
		}
	}
	return false
}

func extractModels(serviceDir string) *modelsInfo {
	mi := &modelsInfo{Structs: map[string][]fieldInfo{}}
	dir := ""
	for _, cand := range []string{"models", "model"} {
		if fi, err := os.Stat(filepath.Join(serviceDir, cand)); err == nil && fi.IsDir() {
			dir = filepath.Join(serviceDir, cand)
			break
		}
	}
	if dir == "" {
		return mi
	}
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				var fields []fieldInfo
				for _, f := range st.Fields.List {
					info := fieldInfo{Type: renderExpr(f.Type, fset)}
					for _, n := range f.Names {
						info.Name = n.Name
						break
					}
					if f.Tag != nil {
						tag, _ := strconv.Unquote(f.Tag.Value)
						info.JSON = tagValue(tag, "json")
						info.DB = tagValue(tag, "db")
						info.Validate = tagValue(tag, "validate")
						if info.Validate == "" {
							info.Validate = tagValue(tag, "binding")
						}
					}
					if info.Name != "" {
						fields = append(fields, info)
					}
				}
				mi.Structs[ts.Name.Name] = fields
			}
		}
	}
	return mi
}

// tagValue reads one key out of a Go struct tag.
//
// It used to split the tag on spaces and unquote the remainder, which silently
// truncated any tag whose value contains a space — and the corpus is full of
// them: `validate:"oneof=W X Y"` was read as `oneof=W`, so the allowed set
// collapsed to a single option and the two fields that most often fail
// validation were never adjusted at all. reflect.StructTag is the parser that
// handles quoting correctly.
func tagValue(tag, key string) string {
	return reflect.StructTag(tag).Get(key)
}

// paramsOf returns a function's parameters in declaration order, one entry
// per name (a grouped `a, b string` yields two). Names matter as much as
// types here: a constructor's parameter names are the only link from a call
// site back to the handle it should receive.
func paramsOf(fd *ast.FuncDecl, fset *token.FileSet) []Param {
	if fd.Type.Params == nil {
		return nil
	}
	var out []Param
	for _, p := range fd.Type.Params.List {
		typ := renderExpr(p.Type, fset)
		for _, n := range p.Names {
			out = append(out, Param{Name: n.Name, Type: typ})
		}
	}
	return out
}

// extractDBFact recognizes the store-method conventions: ctx first param, a
// backtick query literal (assigned, var or const), one sqlx call
// (SelectContext/GetContext/ExecContext and their aliases on g.db or tx),
// row/scalar type from the scan target's declaration. Tx-variant methods
// (tx *sqlx.Tx second param, tx.ExecContext / tx.GetContext receiver) set
// IsTx — the db test layer renders them with the Beginx handle instead of
// skipping them as unsupported.
func extractDBFact(fd *ast.FuncDecl, fset *token.FileSet) *dbFact {
	f := &dbFact{Name: fd.Name.Name}
	if fd.Type.Params != nil && fd.Type.Params.NumFields() > 0 {
		if names := fd.Type.Params.List[0].Names; len(names) > 0 {
			f.CtxName = names[0].Name
		}
	}
	for _, p := range paramsOf(fd, fset) {
		f.Params = append(f.Params, p)
		if p.Type == "*sqlx.Tx" || p.Type == "sqlx.Tx" || (p.Name == "tx" && strings.Contains(p.Type, "Tx")) {
			f.IsTx = true
		}
	}
	// ReturnType is the first non-error result (tx scalar reads scan into
	// sql.Null* but return the extracted Go type — e.g. GetMarks scans
	// sql.NullString, returns string).
	if fd.Type.Results != nil {
		for _, r := range fd.Type.Results.List {
			t := renderExpr(r.Type, fset)
			if t == "error" {
				continue
			}
			f.ReturnType = t
			break
		}
	}
	var queryLit string
	var call *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range x.Rhs {
				if lit, ok := rhs.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(lit.Value, "`") {
					q, err := strconv.Unquote(lit.Value)
					if err == nil {
						queryLit = q
					}
				}
			}
		case *ast.GenDecl:
			// `var query = `…`` / `const query = `…`` package or local
			// declarations carry the literal outside an assignment.
			if x.Tok != token.VAR && x.Tok != token.CONST {
				return true
			}
			for _, spec := range x.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(lit.Value, "`") {
						q, err := strconv.Unquote(lit.Value)
						if err == nil {
							queryLit = q
						}
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if dbCallShape(sel.Sel.Name) != "" {
					call = x
					// The SQL can also arrive as an argument rather than
					// through a variable:
					//
					//	s.tx.ExecContext(ctx, `DELETE FROM … WHERE ID = ?`, id)
					//
					// A fallback only — a variable or declaration found above is
					// the more deliberate statement of the same query, and
					// overriding it could change behaviour that already works.
					//
					// This is not cosmetic. isDeleteTx tests the query for a
					// DELETE prefix, so an empty Query made a DELETE read as a
					// non-DELETE and the generated case asserted the wrong
					// thing, with nothing failing to say so.
					if queryLit == "" {
						queryLit = backtickArg(x)
					}
				}
			}
		}
		return true
	})
	if call == nil {
		return nil
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		f.Recv = renderExpr(sel.X, fset)
		if f.Recv == "tx" {
			f.IsTx = true
		}
		f.Shape = dbCallShape(sel.Sel.Name)
	}
	for i, a := range call.Args {
		if i < 2 {
			continue // ctx, scan target
		}
		if lit, ok := a.(*ast.Ident); ok && lit.Name == "query" {
			continue
		}
		f.Args = append(f.Args, renderExpr(a, fset))
	}
	if queryLit != "" {
		f.Query = queryLit
		f.Tables = tablesOf(queryLit)
	}
	if f.Shape == "multi" || f.Shape == "scan" {
		target := ""
		if len(call.Args) > 1 {
			if star, ok := call.Args[1].(*ast.UnaryExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok {
					target = id.Name
				}
			}
		}
		if target == "" {
			// QueryRowx/QueryRow shapes pass the query where the scan
			// target sits on Select/Get — the destination is the Scan arg.
			target = scanVarOf(fd)
		}
		f.RowType, f.Scalar = scanTargetType(fd, target, fset)
		if f.Shape == "scan" {
			if f.Scalar != "" {
				f.Shape = "scalar"
			} else if f.RowType != "" {
				f.Shape = "single"
			}
		}
	}
	f.NoRowsContract = noRowsContractOf(fd, fset)
	if f.Shape == "dml" {
		f.NoRowsError = noRowsErrorOf(fd, fset)
	}
	return f
}

// returnStmt is one `return …` statement's result expressions.
type returnStmt struct{ results []ast.Expr }

// returnsErr reports whether the statement returns the query's error
// unchanged — `return …, err`, the shape of a propagating read.
func (r returnStmt) returnsErr() bool {
	if len(r.results) == 0 {
		return false
	}
	id, ok := r.results[len(r.results)-1].(*ast.Ident)
	return ok && id.Name == "err"
}

// returnsNilError reports whether the statement swallows the error —
// `return &T{}, nil`, the shape of a tolerating read.
func (r returnStmt) returnsNilError() bool {
	if len(r.results) == 0 {
		return false
	}
	id, ok := r.results[len(r.results)-1].(*ast.Ident)
	return ok && id.Name == "nil"
}

// returnsIn collects every `return` statement under n. The caller decides
// which of them belong to the branch it is classifying: an `err != nil` guard
// is judged on the returns directly inside its own body, so a return nested in
// a deeper if stays that inner branch's decision instead of being attributed
// to the guard.
func returnsIn(n ast.Node) []returnStmt {
	if n == nil {
		return nil
	}
	var out []returnStmt
	ast.Inspect(n, func(x ast.Node) bool {
		if rs, ok := x.(*ast.ReturnStmt); ok {
			out = append(out, returnStmt{results: rs.Results})
		}
		return true
	})
	return out
}

// isErrNilCheck reports whether a condition is the `err != nil` guard over a
// query call's error. `!(err == nil)` counts too — the converted code uses
// both spellings and they mean the same thing.
func isErrNilCheck(cond ast.Expr) bool {
	un, negated := cond.(*ast.UnaryExpr)
	if negated && un.Op == token.NOT {
		if be, ok := un.X.(*ast.BinaryExpr); ok && be.Op == token.EQL {
			return isErrIdent(be.X) && isNilIdent(be.Y)
		}
		return false
	}
	be, ok := cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return false
	}
	return isErrIdent(be.X) && isNilIdent(be.Y)
}

func isErrIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "err"
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// noRowsContractOf classifies a read's error branch. The default is
// `tolerate`, which is what the no-rows case assumed before the contract was
// read from the body; defaulting to it means an unrecognised shape keeps the
// bytes it already had rather than silently flipping to the other contract.
func noRowsContractOf(fd *ast.FuncDecl, fset *token.FileSet) string {
	// An explicit sql.ErrNoRows check is the clearest statement of intent:
	// the author decided what no-rows means for this method.
	explicit := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		// errors.Is(err, sql.ErrNoRows) — the Fun is a SelectorExpr
		// (errors.Is), not a bare identifier.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Is" {
			return true
		}
		if isErrNoRowsSentinel(call.Args[1], fset) {
			explicit = true
		}
		return true
	})

	tolerating, propagating := false, false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Cond == nil {
			return true
		}
		// Only the guard over the query call's own error matters.
		if !isErrNilCheck(ifs.Cond) {
			return true
		}
		for _, r := range returnsIn(ifs.Body) {
			switch {
			case r.returnsErr():
				propagating = true
			case r.returnsNilError():
				tolerating = true
			}
		}
		return true
	})

	switch {
	case explicit:
		// The author singled out sql.ErrNoRows, so they decided what no-rows
		// means here. Checked first: a method that special-cases the sentinel
		// and also propagates other errors still tolerates the no-rows case,
		// which is the only scenario this contract describes.
		return "tolerate"
	case propagating && !tolerating:
		return "propagate"
	default:
		// tolerating-only, both, or nothing recognisable. All three default to
		// tolerate, which is the behaviour that predated body-reading, so an
		// unrecognised shape keeps its existing bytes.
		return "tolerate"
	}
}

// noRowsErrorOf returns the error text a DML method produces when
// RowsAffected == 0, or "" when the body has no such branch.
//
// Two converted shapes exist and both must be recognised:
//
//	count, err := g.db.ExecContext(...)
//	if count > 0 { return nil }
//	return errors.New("unable to add the question")
//
// and the direct
//
//	if count == 0 { return sql.ErrNoRows }
//	return nil
//
// Anything else yields "" and the caller keeps the existing behaviour rather
// than guessing a message the code never states — an invented expectation
// would fail for a reason unrelated to the method under test.
func noRowsErrorOf(fd *ast.FuncDecl, fset *token.FileSet) string {
	var found string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		for _, r := range returnsIn(n) {
			msg, ok := returnedErrorLiteral(r, fset)
			if ok && msg != "" {
				found = msg
				return false
			}
		}
		return true
	})
	return found
}

// returnedErrorLiteral extracts the message from a `return errors.New("…")`
// or a `return sql.ErrNoRows` — the two converted shapes for a zero-rows
// branch. Both are returned as a bare expression rather than inside a call,
// so both spellings are handled at the top level of the switch.
func returnedErrorLiteral(r returnStmt, fset *token.FileSet) (string, bool) {
	for _, res := range r.results {
		// sql.ErrNoRows — the sentinel itself, rendered to its canonical text.
		if isErrNoRowsSentinel(res, fset) {
			return "sql: no rows in result set", true
		}
		call, ok := res.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		// errors.New("…") — the domain-message shape.
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "errors" && sel.Sel.Name == "New" {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					return s, true
				}
			}
		}
	}
	return "", false
}

// isErrNoRowsSentinel reports whether an expression names the sql.ErrNoRows
// sentinel. Both the qualified form and a package-level alias denote the same
// error, whose message is fixed by database/sql.
//
// The alias match is case-INSENSITIVE on purpose. Converted code conventionally
// spells it `errNoRows` — lowercase e, since it is a local — and a
// case-sensitive HasSuffix("ErrNoRows") misses every one of those. The
// repo's own dbtx fixture used it, so the pin for this failed before the
// method was reached.
func isErrNoRowsSentinel(e ast.Expr, fset *token.FileSet) bool {
	if renderExpr(e, fset) == "sql.ErrNoRows" {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && strings.HasSuffix(strings.ToLower(id.Name), "norows")
}

// dbCallShape maps a sqlx / database-sql call name onto the db block shape
// it renders through: multi (Select), single/scalar (Get/QueryRow, resolved
// by the scan target) and dml (Exec/NamedExec). "" = not a query call.
func dbCallShape(name string) string {
	switch name {
	case "SelectContext", "Select":
		return "multi"
	case "GetContext", "Get", "QueryRowxContext", "QueryRowx", "QueryRowContext", "QueryRow":
		return "scan"
	case "ExecContext", "Exec", "NamedExecContext", "NamedExec":
		return "dml"
	}
	return ""
}

// scanVarOf finds the destination of a row Scan call (`row.Scan(&x)`) — the
// QueryRowx/QueryRow shapes carry no scan-target argument.
func scanVarOf(fd *ast.FuncDecl) string {
	var name string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Scan" {
			return true
		}
		if un, ok := call.Args[0].(*ast.UnaryExpr); ok {
			if id, ok := un.X.(*ast.Ident); ok {
				name = id.Name
			}
		}
		return true
	})
	return name
}

// scanTargetType finds how the scan target variable is declared inside the
// method body: `var x []*models.T`, `var x models.T`, `var x int64`,
// `x := []*models.T{}`, `x := models.T{}`, `x := &models.T{}`, or
// `x := make([]*models.T, 0)`. Unresolvable declarations leave both results
// empty — the build gate skips the method instead of composing a block with
// an empty type name.
func scanTargetType(fd *ast.FuncDecl, varName string, fset *token.FileSet) (rowType, scalar string) {
	if varName == "" {
		return "", ""
	}
	var typ string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if typ != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.DeclStmt:
			gd, ok := x.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR || len(gd.Specs) == 0 {
				return true
			}
			vs, ok := gd.Specs[0].(*ast.ValueSpec)
			if !ok || vs.Type == nil {
				return true
			}
			for _, name := range vs.Names {
				if name.Name == varName {
					typ = renderExpr(vs.Type, fset)
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != varName || i >= len(x.Rhs) {
					continue
				}
				if t := declaredTypeOf(x.Rhs[i], fset); t != "" {
					typ = t
				}
			}
		}
		return true
	})
	switch {
	case strings.HasPrefix(typ, "[]*"):
		if inner := strings.TrimPrefix(typ, "[]*"); structishType(inner) {
			rowType = inner
		} else {
			scalar = typ
		}
	case strings.HasPrefix(typ, "[]"):
		if inner := strings.TrimPrefix(typ, "[]"); structishType(inner) {
			rowType = inner
		} else {
			scalar = typ
		}
	case strings.HasPrefix(typ, "*"):
		if inner := strings.TrimPrefix(typ, "*"); structishType(inner) {
			rowType = inner
		} else {
			scalar = typ
		}
	case structishType(typ):
		rowType = typ
	case typ != "":
		scalar = typ
	}
	return rowType, scalar
}

// structishType reports whether a declared type names a struct (exported or
// package-qualified) rather than a builtin scalar — `models.T`, `DateInfo`
// vs `int64`, `string`, `[]byte`, `sql.NullString`, `time.Time`.
func structishType(t string) bool {
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "sql.") || strings.HasPrefix(t, "time.") {
		return false
	}
	if strings.Contains(t, ".") {
		return true
	}
	r := rune(t[0])
	return r >= 'A' && r <= 'Z'
}

// declaredTypeOf renders the static type of a scan-target initializer:
// composite literals (with or without the address-of) and make() calls.
func declaredTypeOf(e ast.Expr, fset *token.FileSet) string {
	switch x := e.(type) {
	case *ast.CompositeLit:
		return renderExpr(x.Type, fset)
	case *ast.UnaryExpr:
		if t := declaredTypeOf(x.X, fset); t != "" {
			return "*" + t
		}
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "make" && len(x.Args) > 0 {
			return renderExpr(x.Args[0], fset)
		}
	}
	return ""
}

// tablesOf extracts the FROM table list from a SQL query (source order,
// aliases dropped). DML statements carry no FROM: INSERT INTO / UPDATE /
// DELETE FROM / MERGE INTO yield their single target table so the Exec
// regex still anchors on it. The DML target wins over any FROM inside a
// subquery (MERGE ... USING (SELECT ... FROM DUAL) targets the MERGE table,
// not DUAL).
func tablesOf(query string) []string {
	up := strings.ToUpper(strings.TrimSpace(query))
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "MERGE"} {
		if strings.HasPrefix(up, verb) {
			for _, prefix := range []string{"INSERT INTO ", "MERGE INTO ", "DELETE FROM ", "UPDATE "} {
				if idx := strings.Index(up, prefix); idx >= 0 {
					rest := strings.TrimSpace(query[idx+len(prefix):])
					if rest == "" {
						return nil
					}
					// Table is the first token; cut at "(" for paren-glued
					// targets (INSERT INTO T(col,...)) then strip punctuation.
					tok := strings.Fields(rest)[0]
					if i := strings.Index(tok, "("); i >= 0 {
						tok = tok[:i]
					}
					tok = strings.Trim(tok, "(),;")
					if tok != "" {
						return []string{tok}
					}
				}
			}
			return nil
		}
	}
	up2 := strings.ToUpper(query)
	i := strings.Index(up2, " FROM ")
	if i >= 0 {
		rest := query[i+len(" FROM "):]
		end := len(rest)
		for _, kw := range []string{"\nWHERE", " WHERE", "\nORDER", " ORDER", "\nGROUP", " GROUP", "\nJOIN", " JOIN", "\nHAVING", " HAVING"} {
			if j := strings.Index(strings.ToUpper(rest), kw); j >= 0 && j < end {
				end = j
			}
		}
		segment := rest[:end]
		var out []string
		for _, t := range strings.Split(segment, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if sp := strings.Fields(t); len(sp) > 0 {
				t = sp[0]
			}
			out = append(out, t)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// extractCtrlFact recognizes controller endpoints: (ctx context.Context,
// request *models.X) → (…, error), collecting `recv.store.M(...)` calls in
// body order.
func extractCtrlFact(fd *ast.FuncDecl, fset *token.FileSet, dbIface map[string]dbIfaceSig) *ctrlFact {
	f := &ctrlFact{Name: fd.Name.Name, Passthrough: true}
	if fd.Type.Params == nil || fd.Type.Params.NumFields() < 1 {
		return nil
	}
	first := fd.Type.Params.List[0]
	if renderExpr(first.Type, fset) != "context.Context" {
		return nil
	}
	if len(first.Names) > 0 {
		f.CtxName = first.Names[0].Name
	}
	// A request parameter is optional. DisplayMarks(ctx) is a real endpoint in
	// the corpus and returning nil for it made the controller unit `unsupported`,
	// which is how the one method the log route missed stayed missing. An empty
	// RequestType is the honest shape: no request, and the renderer emits a
	// call with no request argument.
	if len(fd.Type.Params.List) > 1 {
		reqField := fd.Type.Params.List[len(fd.Type.Params.List)-1]
		rt := renderExpr(reqField.Type, fset)
		if strings.HasPrefix(rt, "*models.") {
			f.RequestType = strings.TrimPrefix(rt, "*")
		}
	}
	if fd.Type.Results != nil && fd.Type.Results.NumFields() > 0 {
		f.ResponseType = renderExpr(fd.Type.Results.List[0].Type, fset)
	}
	if lit, ok := scalarSuccessReturn(fd, fset, f.ResponseType); ok {
		f.ScalarResponse = lit
	}
	// The path model is read from the body here, while the FuncDecl is in
	// hand. Every consumer downstream shares it, so the two routes cannot
	// disagree about how many times a call happens.
	f.Paths = enumerateCtrlPaths(fd, fset, dbIface, f.ScalarResponse)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "store" {
			return true
		}
		sc := storeCall{Method: sel.Sel.Name, ArgCount: -1}
		if sig, ok := dbIface[sel.Sel.Name]; ok {
			sc.ArgCount = sig.ArgCount
		}
		for i, a := range call.Args {
			if i == 0 {
				continue // ctx
			}
			sc.Args = append(sc.Args, renderExpr(a, fset))
		}
		f.StoreCalls = append(f.StoreCalls, sc)
		return true
	})
	// Field mapping (composite literals over models types or .String
	// projections) is what the deterministic template cannot shape.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if t := renderExpr(x.Type, fset); strings.Contains(t, "models.") {
				f.Passthrough = false
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "String" {
				f.Passthrough = false
			}
		}
		return true
	})
	if len(f.StoreCalls) == 0 {
		return nil
	}
	f.Src = renderNode(fd, fset)
	return f
}

// extractHandlerFact recognizes gin handlers: (c *gin.Context), a request
// var decl, and one controller call.
func extractHandlerFact(fd *ast.FuncDecl, fset *token.FileSet) *handlerFact {
	f := &handlerFact{Name: fd.Name.Name}
	if fd.Type.Params == nil || fd.Type.Params.NumFields() != 1 || renderExpr(fd.Type.Params.List[0].Type, fset) != "*gin.Context" {
		return nil
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) > 0 {
				if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && vs.Type != nil {
					t := renderExpr(vs.Type, fset)
					if strings.HasPrefix(t, "models.") {
						f.RequestType = strings.TrimPrefix(t, "models.")
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "controller" {
					f.CtrlCall = sel.Sel.Name
					f.CtrlBranches = append(f.CtrlBranches, ctrlBranch{
						Method: sel.Sel.Name,
						Args:   ctrlCallArgsOf(x, fset),
					})
					// The call site's own argument list is the only
					// trustworthy statement of what the controller method
					// receives. A handler may bind a request and not pass
					// it, and one that does pass it may pass something
					// else entirely.
					//
					// REPLACED, not appended. A handler can route to more
					// than one controller method:
					//
					//	if request.RequestType == "B" { …ViewQuestions(c, &request) }
					//	if request.RequestType == "L" { …ListSection(c, &request) }
					//
					// which is in the corpus. Appending merged both call
					// sites' arguments and produced
					// ListSection(c, &request, &request) — three arguments
					// to a method that takes two. Each call site's name and
					// arguments must be overwritten together so they always
					// describe the same call.
					f.CtrlCallArgs = nil
					for i, a := range x.Args {
						if i == 0 {
							continue // ctx
						}
						f.CtrlCallArgs = append(f.CtrlCallArgs, renderExpr(a, fset))
					}
				}
			}
		}
		return true
	})
	f.Envelope = handlerEnvelope(fd, fset)
	f.CtrlBranches = matchBranchesToCalls(f.CtrlBranches, fd.Body.List)
	if f.CtrlCall == "" {
		return nil
	}
	return f
}

// matchBranchesToCalls attaches each routing guard to the call site it guards.
//
// Both are collected in source order — the calls by ast.Inspect over the body,
// the guards by scanning its top-level statements — so they line up. A guard
// with no call of its own, or a call no guard claims, is left unguarded rather
// than paired by guesswork.
func matchBranchesToCalls(branches []ctrlBranch, stmts []ast.Stmt) []ctrlBranch {
	guards := routingGuards(stmts)
	if len(branches) == 0 || len(guards) == 0 {
		return branches
	}
	for i := range branches {
		if i >= len(guards) {
			break
		}
		field, value := guardFieldAndValue(guards[i].Cond)
		if field == "" {
			continue
		}
		branches[i].Field = field
		branches[i].Value = value
		branches[i].Guarded = true
	}
	return branches
}

// routingGuards returns the top-level if statements that route to a controller
// call, in source order.
func routingGuards(stmts []ast.Stmt) []*ast.IfStmt {
	var out []*ast.IfStmt
	for _, s := range stmts {
		ifs, ok := s.(*ast.IfStmt)
		if !ok || ifs.Body == nil || !isRequestFieldCompare(ifs.Cond) {
			continue
		}
		if blockHasControllerCall(ifs.Body) {
			out = append(out, ifs)
		}
	}
	return out
}

// renderExpr prints an expression/type back to Go source.
func renderExpr(e ast.Expr, fset *token.FileSet) string {
	if e == nil {
		return ""
	}
	return renderNode(e, fset)
}

// renderNode prints any AST node back to Go source.
func renderNode(n ast.Node, fset *token.FileSet) string {
	if n == nil {
		return ""
	}
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}
