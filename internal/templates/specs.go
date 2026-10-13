// Package templates shares mechanism only (Provider.Render), not data —
// but every *Data struct below is a thin per-language projection of
// contract.Service (uniform-ir plan §3.5): FieldSpec/StructSpec/ModelFileData
// project contract.Field rows, DBMethodData projects contract.QueryUnit +
// GoNamer names, and the C#/Python emitters project the same contract
// through CsNamer/PyNamer. Rule for new code: template-data constructors
// take contract.* as input, never fresh ir.Query parses.
package templates

import (
	"strconv"
	"strings"

	"tux-migration-tool/internal/contract"
)

// FieldSpec is one struct field in a models struct (PRD §4.8.3: json tags =
// raw FML field names; db tags = SELECT aliases; gin binding validators).
type FieldSpec struct {
	Name      string // Go field name, e.g. CompCode
	Type      string // string, sql.NullString, sql.NullTime, int64, ...
	JSONTag   string // raw FML name, e.g. FML_COMP_CD (request/response fields)
	OmitEmpty bool   // response fields carry ,omitempty
	DBTag     string // SELECT alias, e.g. COMP_CD (row-struct fields)
	Binding   string // gin validator list, e.g. "required,matchaccount"
	ErrMsg    string // custom validator message rendered as error:"..."
}

// Tag renders the struct tag for the field.
func (f FieldSpec) Tag() string {
	var parts []string
	switch {
	case f.JSONTag != "":
		tag := f.JSONTag
		if f.OmitEmpty {
			tag += ",omitempty"
		}
		parts = append(parts, "json:\""+tag+"\"")
	case f.DBTag != "":
		parts = append(parts, "db:\""+f.DBTag+"\"")
	}
	if f.Binding != "" {
		parts = append(parts, "binding:\""+f.Binding+"\"")
	}
	if f.ErrMsg != "" {
		parts = append(parts, "error:\""+f.ErrMsg+"\"")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// HasNullType reports whether any field uses a database/sql Null* type.
func HasNullType(structs []StructSpec) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if strings.HasPrefix(f.Type, "sql.Null") {
				return true
			}
		}
	}
	return false
}

// StructSpec is one struct in the models file.
type StructSpec struct {
	Name   string
	Fields []FieldSpec
}

// ModelFileData renders models.go.
type ModelFileData struct {
	Package string // "models"
	Structs []StructSpec
}

// HasNullTypes reports whether the file needs `import "database/sql"`.
func (d ModelFileData) HasNullTypes() bool { return HasNullType(d.Structs) }

// ParamSpec is one function parameter.
type ParamSpec struct{ Name, Type string }

// DBMethodData renders a store method (PRD §4.8.2 contract: ctx first arg,
// positional :1 binds, columns aliased AS "COL" matching db tags, GetContext /
// SelectContext / ExecContext).
type DBMethodData struct {
	Receiver   string // s — the layer-role convention (common.GoStoreRecv)
	StoreType  string // store
	Name       string // GetItemDetails
	Doc        string // optional doc comment (multi-line, already commented)
	CtxName    string // ctx (common.GoCtx)
	Params     []ParamSpec
	Query      string // backtick-free SQL body placed inside a raw string literal
	VarName    string // scan target variable, e.g. itemDetails / dateinfo / count
	RowType    string // models.ItemDetails ("" for scalar results)
	Scalar     string // int64 etc. when RowType == ""
	Multi      bool   // SelectContext into []*T
	TxParam    bool   // takes tx *sqlx.Tx as second parameter (DML / tx-variant read)
	SuccessMsg string // insert/update debug log after a successful exec
	// ErrPolicy is the failure contract the legacy SQLCODE guard declared for
	// this query: does an empty result count as an error? Empty means the
	// source said nothing, which is NOT the same as "strict" — templates must
	// keep their existing default in that case.
	ErrPolicy contract.ErrorPolicy
}

// TolerantNoRows reports whether the emitted method should swallow
// sql.ErrNoRows. Only an explicitly tolerant policy may do so; an unset policy
// defers to the template's per-type default via TolerantNoRowsDefault.
func (d DBMethodData) TolerantNoRows() bool {
	return d.ErrPolicy == contract.PolicyTolerantNoRows
}

// TolerantNoRowsDefault is the fallback when the source declared no policy.
// It is the pre-existing template behaviour, preserved verbatim so adopting
// the policy cannot change output for queries whose guard was not read.
//
// The switch is over every set policy rather than testing for one: an
// unclassified policy is a verdict (a guard that was read but not understood)
// and must not quietly resolve to a default, or the counted gap would be
// papered over at exactly the point the census is watching.
func (d DBMethodData) TolerantNoRowsDefault(fallback bool) bool {
	switch d.ErrPolicy {
	case contract.PolicyTolerantNoRows:
		return true
	case contract.PolicyStrict, contract.PolicyUnclassified:
		return false
	default:
		return fallback
	}
}

// ArgList renders the comma-separated bind argument names.
func (d DBMethodData) ArgList() string {
	names := make([]string, len(d.Params))
	for i, p := range d.Params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// ReturnType renders the Go return type pair.
func (d DBMethodData) ReturnType() string {
	switch {
	case d.Multi && d.RowType != "":
		return "[]*" + d.RowType
	case d.RowType != "":
		return "*" + d.RowType
	default:
		return d.Scalar
	}
}

// DBInterfaceData renders db/interface.go (store struct + <Name>Store
// interface + constructor; the interface accretes one line per generated
// method — PRD §4.2.3).
type DBInterfaceData struct {
	Package      string   // "db"
	StoreType    string   // "store"
	IfaceName    string   // "ItemStore"
	CtorName     string   // "NewItemStore"
	WithGorm     bool     // store carries the legacy *gorm.DB handle alongside sqlx
	ReadDBs      []string // logical read-DB names; one *sqlx.DB param each
	ModelsPkg    string   // models package import path
	ExtraImports []string
	Methods      []string // rendered signatures, one line each
}

// DefaultReadDB is the logical read-DB name used when a mapping leaves readDBs
// unset. It exists to keep the handler wiring and the store constructor in
// agreement: the store ctor takes one *sqlx.DB per read DB, so an empty list
// would render NewXStore() against a one-param ctor and not compile. Point it
// at a field your target repo actually has (repo.Databases.ReadDatabase.<name>),
// or set readDBs in the mapping to override it per service.
const DefaultReadDB = "PX"

// ReadDBsOrDefault returns the configured read DBs, falling back to the single
// DefaultReadDB so the store ctor and its call site can never disagree on
// arity.
func ReadDBsOrDefault(dbs []string) []string {
	if len(dbs) == 0 {
		return []string{DefaultReadDB}
	}
	return dbs
}

// DBFieldDecls renders the store struct's handle fields, tab-indented, so the
// struct declares exactly the handles DBParams accepts and DBFields assigns.
func (d DBInterfaceData) DBFieldDecls() string {
	dbs := ReadDBsOrDefault(d.ReadDBs)
	lines := make([]string, 0, len(dbs)+1)
	if d.WithGorm {
		lines = append(lines, "\toracle *gorm.DB")
	}
	for i := range dbs {
		name := "db"
		if i > 0 {
			name = "db" + strconv.Itoa(i+1)
		}
		lines = append(lines, "\t"+name+" *sqlx.DB")
	}
	return strings.Join(lines, "\n")
}

// DBParams renders the store constructor's *sqlx.DB parameter list: one per
// read DB, so the signature always matches ReadDBArgList at the call site.
func (d DBInterfaceData) DBParams() string {
	dbs := ReadDBsOrDefault(d.ReadDBs)
	parts := make([]string, 0, len(dbs)+1)
	if d.WithGorm {
		parts = append(parts, "oracle *gorm.DB")
	}
	for i := range dbs {
		name := "db"
		if i > 0 {
			name = "db" + strconv.Itoa(i+1)
		}
		parts = append(parts, name+" *sqlx.DB")
	}
	return strings.Join(parts, ", ")
}

// DBFields renders the struct literals the constructor assigns from DBParams.
func (d DBInterfaceData) DBFields() string {
	dbs := ReadDBsOrDefault(d.ReadDBs)
	parts := make([]string, 0, len(dbs)+1)
	if d.WithGorm {
		parts = append(parts, "oracle: oracle")
	}
	for i := range dbs {
		name := "db"
		if i > 0 {
			name = "db" + strconv.Itoa(i+1)
		}
		parts = append(parts, name+": "+name)
	}
	return strings.Join(parts, ", ")
}

// ControllerInterfaceData renders controller/interface.go (PRD §4.8.6).
type ControllerInterfaceData struct {
	Package    string // "controller"
	StructName string // "itemController"
	IfaceName  string // "ItemController"
	CtorName   string // "NewItemController"
	DBPkg      string // db package import path
	ModelsPkg  string // models package import path
	StoreIface string // "db.ItemStore"
	Methods    []string
}

// FnStub is one placeholder for an unresolved external fn (stub-and-carry-on,
// 2026-09-10): variadic args, int return matching the corpus's -1 error
// convention. Body carries an LLM-synthesized implementation when the stub
// synthesis seam accepted one (2026-09-16) — otherwise the panicking
// placeholder renders.
type FnStub struct {
	Name     string // Go name, e.g. fnLongToInt
	Original string // corpus symbol, e.g. fn_long_to_int
	Body     string // synthesized Go func decl; "" = panicking stub
}

// FnStubFileData renders controller/fnstubs.go.
type FnStubFileData struct {
	Package string // "controller"
	Stubs   []FnStub
}

// ControllerMethodData renders one controller endpoint method (pure business
// logic + store calls; the body slot is filled by the pipeline/LLM).
type ControllerMethodData struct {
	StructName   string // "itemController"
	Name         string // "ItemList"
	Receiver     string // "c" — the layer-role convention (common.GoCtrlRecv)
	CtxName      string // "ctx" (common.GoCtx)
	RequestType  string // "models.ItemRequest"
	ResponseType string // "models.ItemResponse"
	Body         string // logic including any return statements
}

// HandlerInterfaceData renders handler/interface.go: handler struct + interface
// + constructor + the wiring function building the store from the existing repo
// (PRD §4.8.6, OQ11).
type HandlerInterfaceData struct {
	Package         string   // "handler"
	Module          string   // "root-folder"
	Service         string   // "item"
	StructName      string   // "itemHandler"
	IfaceName       string   // "ItemHandler"
	CtorName        string   // "NewItemHandler"
	WiringFnName    string   // "ItemController" (repo.DataObject wiring function)
	ControllerIface string   // "ItemController"
	ControllerCtor  string   // "NewItemController"
	StoreCtor       string   // "NewItemStore"
	ReadDBs         []string // "EBATEST", "PX"
	Methods         []string
	CtxName         string // "ctx" (common.GoCtx) — the gin context parameter
}

// ReadDBArgList renders the repo.Databases.ReadDatabase.* constructor args,
// one per read DB. It falls back to DefaultReadDB when the mapping leaves
// readDBs unset, so the argument count here always matches the store
// constructor's parameter count (DBInterfaceData.DBParams) — otherwise the
// wiring was emitted as NewXStore() against a one-param ctor and did not
// compile.
func (h HandlerInterfaceData) ReadDBArgList() string {
	dbs := ReadDBsOrDefault(h.ReadDBs)
	args := make([]string, len(dbs))
	for i, db := range dbs {
		args[i] = "repo.Databases.ReadDatabase." + db
	}
	return strings.Join(args, ", ")
}

// HandlerMethodData renders one gin handler method (PRD §4.8.6 handler shape).
type HandlerMethodData struct {
	StructName      string // "itemHandler"
	Name            string // "ItemList"
	RequestType     string // "models.ItemRequest"
	Receiver        string // "h" — the layer-role convention (common.GoHandlerRecv)
	CtxName         string // "ctx" (common.GoCtx) — the gin context parameter
	ControllerField string // "controller"
}

// DBSelectTxData renders a tx-variant single-row read (GetMarks shape in
// examples/dbTransactionEx.txt): a mid-flow read inside a sequential-crux flow
// runs on tx, takes `tx *sqlx.Tx` as its second parameter (PRD §4.2.3 variant
// rule) and propagates every error — including sql.ErrNoRows — to the flow.
type DBSelectTxData struct {
	Receiver  string // g
	StoreType string // store
	Name      string // GetMarks
	CtxName   string // ctx
	Params    []ParamSpec
	Query     string
	VarName   string // "marks"
	ScanType  string // "sql.NullString"
	Extract   string // "marks.String"
	Zero      string // `""` — zero-value return on the error path
	Return    string // "string"
	// ErrPolicy is the failure contract the legacy SQLCODE guard declared.
	// Unset defers to this template's existing strict propagation.
	ErrPolicy contract.ErrorPolicy
}

// TolerantNoRows reports whether this tx read swallows sql.ErrNoRows. The
// template's default is strict (it propagates every error); only an explicitly
// tolerant policy relaxes that.
func (d DBSelectTxData) TolerantNoRows() bool {
	return d.ErrPolicy == contract.PolicyTolerantNoRows
}

// ArgList renders the comma-separated bind argument names.
func (d DBSelectTxData) ArgList() string {
	names := make([]string, len(d.Params))
	for i, p := range d.Params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// ControllerTxMethodData renders a controller endpoint that wraps a
// sequential-crux flow in utils.ExecTransaction (AssessQnA shape in
// examples/controllerTxSignature.txt; decision 27): pre-flow reads run outside
// the tx, every call inside the closure takes tx, post-commit reads assemble
// the response.
type ControllerTxMethodData struct {
	Receiver     string // "c"
	StructName   string // "controller"
	Name         string // "AssessQnA"
	CtxName      string // "ctx"
	RequestType  string // "models.AssessQnARequest"
	ResponseType string // "models.AssessQnAResponse"
	PreFlow      string // code before the tx opens (errors → return nil, err)
	TxBody       string // crux sequence; ends with `return nil`
	PostFlow     string // code after the wrapper; ends with the return statement
}

// RouteSpec is one router entry.
type RouteSpec struct {
	Path    string // "/pxitemhistory"
	Handler string // "ItemHistory"
}

// RouterData renders the router snippet (examples/router.txt shape).
type RouterData struct {
	Service string // "item"
	Routes  []RouteSpec
}

// ---- Python batch db-router support file (pygentest plan G2) ----

// PyDBRouterFileData renders py_db_router_file — the `core/db_router.py`
// wrapper every generated batchpy module imports. The enum members are
// derived from the plan's ReadMode/WriteMode *expressions* (`DbMode.READ`)
// rather than hardcoded, so a config that redefines the modes keeps the enum
// and the emitters' references consistent instead of one drifting from the
// other.
type PyDBRouterFileData struct {
	RouterClass   string   // DatabaseRouter
	ModeClass     string   // DbMode — the enum's name, parsed from the mode exprs
	ModeMembers   []string // READ, WRITE — the referenced members, first-seen order
	WriteModeExpr string   // the full WRITE expression, for the commit branch
}

// NewPyDBRouterData derives the router template's data from the plan's
// wrapper config. `readMode`/`writeMode` are the mode expressions the plan
// carries (config's batchpy.readMode / writeMode, e.g. `DbMode.READ`). The
// last dotted segment is the member, the run before it the class; a mode
// spelled as a bare member (`WRITE`) is taken to belong to the other mode's
// class, or to a `DbMode`-named enum when neither carries one.
//
// WriteModeExpr is always a full `Class.Member` expression: the templates
// compare `mode == WriteModeExpr`, so an empty or bare config must still
// render executable Python (an empty expression rendered `if mode == :`,
// which ast.parse rejects).
func NewPyDBRouterData(routerClass, readMode, writeMode string) PyDBRouterFileData {
	readClass, readMember := splitModeExpr(readMode)
	writeClass, writeMember := splitModeExpr(writeMode)

	class := writeClass
	if class == "" {
		class = readClass
	}
	if class == "" {
		class = "DbMode"
	}
	if writeMember == "" {
		writeMember = "WRITE"
	}

	var members []string
	seen := map[string]bool{}
	for _, member := range []string{readMember, writeMember} {
		if member != "" && !seen[member] {
			seen[member] = true
			members = append(members, member)
		}
	}

	return PyDBRouterFileData{
		RouterClass:   routerClass,
		ModeClass:     class,
		ModeMembers:   members,
		WriteModeExpr: class + "." + writeMember,
	}
}

// splitModeExpr splits `DbMode.READ` into its class and member; a bare
// `READ` has no class and an empty expression has neither.
func splitModeExpr(expr string) (class, member string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", ""
	}
	if i := strings.LastIndexByte(expr, '.'); i >= 0 {
		return expr[:i], expr[i+1:]
	}
	return "", expr
}

// ---- Test templates (PRD-2026-09-09 GT-2, distilled from examples/item) ----

// TestHeaderData renders the mockgen + coverage comment header carried by
// every generated test file (examples/item/*_test.txt convention).
type TestHeaderData struct {
	MockGenCmd  string // full mockgen command line
	CoverageCmd string // full go test + cover command line
}

// TestDBFileData renders a db layer test file: suite struct + runner +
// SetupSuite + per-method test blocks (examples/item/db/item_test.txt shape).
type TestDBFileData struct {
	TestHeaderData
	Package     string // db
	LoggerPkg   string // <module>/pkg/logger
	ModelsPkg   string // models import path
	UtilsPkg    string // <module>/pkg/utils
	SuiteName   string // ItemStoreSuite
	StoreVar    string // itemStore
	IfaceName   string // ItemStore
	CtorCall    string // NewItemStore(nil, suite.sqlDB) — pre-rendered
	NeedsSQL    bool   // database/sql import (sql.Null* expected exprs)
	NeedsModels bool   // models import (row-struct expected exprs)
	NeedsRegexp bool   // regexp import (QuoteMeta expectations over the query var)
	Methods     []string
}

// TestDBMethodData renders one suite method: table-driven sqlmock cases
// (SQLError / [Success-NoRows] / Success) over one store call. Braced
// literals arrive pre-rendered (ExpectExpr/NoRowsExpr) so templates never
// fight text/template's {{ parsing.
//
// Tx/DML variants (PRD §4.2.3 decision 27): IsDML switches the block to the
// Exec contract (ExpectExec + sqlmock.NewResult, error-only signature);
// IsTx adds the Beginx tx handle (ExpectBegin + `tx, _ := suite.sqlDB.Beginx()`
// + tx as the second call arg). The non-tx SELECT shape (both false) is the
// reference GT-2 shape and must stay byte-stable.
type TestDBMethodData struct {
	SuiteName  string   // ItemStoreSuite
	StoreVar   string   // itemStore
	Name       string   // GetItemDetails
	Query      string   // store SQL literal (backtick-free); empty = use Regex fallback
	Regex      string   // fallback ExpectQuery/ExpectExec regex (no literal in body)
	Shape      string   // multi | single | scalar | dml
	Cols       []string // mock row columns (db tags)
	Row        []string // Success row values (assumed fixture source)
	NoRows     bool     // single/scalar GetContext: extra Success-NoRows case
	NoRowsExpr string   // expectedOutput literal for the NoRows case
	// NoRowsError is what the method itself returns when the query finds
	// nothing, read from the method's body rather than assumed:
	//
	//	read — "sql: no rows in result set" for a read that propagates err
	//	       unchanged, "" for one that tolerates it. Asserting "" against a
	//	       propagating read is what made every generated no-rows case fail.
	//	DML — the error returned when RowsAffected == 0: a domain message for
	//	       the count-check shape, or the sql.ErrNoRows text for the
	//	       sentinel shape. "" means the method tolerates zero rows and the
	//	       case asserts no error.
	//
	// Empty is the pre-existing behaviour for every unrecognised body: the
	// tool asserts only what the code states.
	NoRowsError string
	ExpectType  string   // []*models.ItemDetails / int64
	ExpectExpr  string   // Success expectedOutput literal
	CallArgs    []string // rendered store-call args after ctx (tx excluded)
	IsDML       bool     // Exec contract: error-only, ExpectExec + NewResult
	IsTx        bool     // tx handle: ExpectBegin + Beginx + tx second arg
	// ToleratesNoRows selects the zero-rows branch: true emits
	// Success-NoRows with no expected error, false emits NoRows with the
	// method's own error. It is decided by the body's zero-rows error and
	// nothing else — the field used to be called DeleteTx and derived from
	// the SQL verb, which the corpus contradicts (dummyprofile's DeleteQuestion
	// is a DELETE-tx that returns an error on zero rows).
	ToleratesNoRows bool
	// GT-7: the first failed complete trace's row as a second case.
	HasAlt    bool
	AltDesc   string   // "Logged#2"
	AltRow    []string // mock row values from the failed trace
	AltExpect string   // expectedOutput literal for the logged row
}

// TestControllerFileData renders a controller test file
// (examples/item/controller/item_test.txt shape): gomock store mock built in
// SetupTest, torn down with Finish, suite fields ctx/mockController/store/
// controller.
type TestControllerFileData struct {
	TestHeaderData
	Package   string // controller
	DBPkg     string // db package import (Mock<StoreIface> lives there)
	LoggerPkg string
	ModelsPkg string
	SuiteName string // DemoControllerSuiteController
	StoreVar  string // demoStore (the db.Mock<StoreIface> field)
	CtrlVar   string // demoController (controller under test)
	CtrlIface string // DemoController
	MockType  string // db.MockDemoStore
	MockCtor  string // db.NewMockDemoStore(suite.mockController)
	CtorCall  string // NewDemoController(suite.demoStore)
	NeedsSQL  bool   // database/sql import (sql.Null* mock-row literals)
	NeedsTime bool   // time import (sql.NullTime / time.Now literals)
	// GomockPkg is the gomock import path, or "" for the uber-go default. See
	// TestHandlerFileData.GomockPkg — same value, same reason, both file
	// templates previously hardcoded the legacy path.
	GomockPkg string
	Methods   []string
}

// CtrlCall is one store-call occurrence inside a controller method: the
// mock method, the EXPECT args after the ctx matcher, and the generated case
// field carrying its Return payload.
type CtrlCall struct {
	Method string   // GetOrderDetails
	Args   []string // args after the ctx matcher (concrete literal or gomock.Any())
	Field  string   // mockInput, mockInput2, ...
	// TakesCtx reports whether the store method declares a ctx parameter. A
	// no-arg method takes no matcher at all: EXPECT().GetDB(gomock.Any()) was
	// over-arity against `GetDB() *sqlx.DB`. Unknown arity is true, so an
	// unread declaration keeps the previous behaviour rather than dropping a
	// matcher that may be required.
	TakesCtx bool
}

// CtrlCaseField is one request-field value for one controller case (the
// failed trace can carry a different request than the success trace).
type CtrlCaseField struct {
	Name  string
	Value string // pre-escaped body text (embedded between quotes)
	// Type is the field's declared Go type for the case struct's declaration,
	// empty meaning string. Literal, when set, replaces the quoted Value
	// entirely — a slice or struct value cannot survive being embedded
	// between quotes, which is how a []string field became the compile error
	// "cannot use testCase.AnswerID (variable of type string) as []string
	// value".
	Type    string
	Literal string
}

// Expr renders the case-struct initialiser for this field: the typed literal
// when one was resolved, else the quoted scalar value. Both routes are byte-
// identical to the pre-F5 output for a string field, which is the common case
// and the one the goldens pin.
func (f CtrlCaseField) Expr() string {
	if f.Literal != "" {
		return f.Literal
	}
	return `"` + f.Value + `"`
}

// CtrlCase is one row of the controller test's case table, aligned with the
// method's CtrlCall list.
type CtrlCase struct {
	Desc           string
	ReqFields      []CtrlCaseField
	Inputs         []string // one Return payload per call; "nil" = skip EXPECT
	ExpectedError  string
	ExpectedOutput string
}

// HandlerCase is one row of the handler test's case table.
type HandlerCase struct {
	Desc          string
	ReqFields     []CtrlCaseField
	Input         string // the mockInput literal
	ExpectedError string // "" = success path
	// ExpectFailure marks a case that must produce the failure envelope
	// WITHOUT an expected message.
	//
	// The binder's error is a go-playground validator rendering, so its text
	// is a third-party library's wording. Asserting on it would break the
	// generated test on a validator upgrade while testing nothing about this
	// service; the envelope's own Status string is this service's choice, via
	// the BadRequestJSON call in its body, and that is asserted instead.
	ExpectFailure bool
	// HTTPCode used to be a pre-rendered status literal asserted directly.
	// It is gone: the code is the host GinContext's choice, not the handler
	// body's, so it is not knowable from the service. The envelope's own
	// Status string and error description are asserted instead.
	HTTPCode string
}

// CasesOrDerived returns Cases when the GT-7 case table was built; the
// legacy two-case table from the pre-GT-7 fields otherwise — template-data
// compatibility for external callers.
//
// The legacy "Failure" row mocked (nil, nil) and then asserted a "No Data
// Found" error, which no path produces: a nil error takes the handler's
// success edge. It is now the empty-result success case it actually was.
func (d TestHandlerMethodData) CasesOrDerived() []HandlerCase {
	if len(d.Cases) > 0 {
		return d.Cases
	}
	if d.SuccessInput == "" && d.Name == "" {
		return nil
	}
	req := make([]CtrlCaseField, 0, len(d.ReqFields))
	for _, f := range d.ReqFields {
		req = append(req, CtrlCaseField{Name: f.Name, Value: f.Value})
	}
	return []HandlerCase{
		{Desc: d.Name + "Error", ReqFields: req, Input: `[]any{nil, errors.New("error while fetching data")}`, ExpectedError: "error while fetching data"},
		{Desc: "EmptyResult", ReqFields: req, Input: "[]any{nil, nil}"},
		{Desc: "Success", ReqFields: req, Input: d.SuccessInput},
	}
}

// TestControllerMethodData renders one suite method: request fields as case
// fields, guarded EXPECT (gomock.Any() ctx + concrete args) in body-call
// order, request built from the case fields, ErrorContains / NoError+Equal
// validations.
type TestControllerMethodData struct {
	SuiteName string     // DemoControllerSuiteController
	StoreVar  string     // demoStore
	CtrlVar   string     // demoController
	Name      string     // OrderDirect
	StoreCall string     // store dependency (GetOrderDetails)
	StoreArgs []string   // EXPECT args after ctx (concrete literals)
	ReqFields []ReqField // request fields driving the case struct
	ReqExpr   string     // models.OrderRequest{CompCode: testCase.CompCode} — pointer added by the template
	// NoRequest marks a controller method that takes no request. The template
	// then builds no request and calls it with ctx alone.
	//
	// This is not cosmetic. extractCtrlFact used to return nil for such a
	// method, so the case never arose; now it yields a fact with an empty
	// RequestType, and emitting `request := &{}` for that produces output that
	// does not parse at all — the whole controller suite was lost, not just
	// this method. The same principle as F9 on the handler side: a method that
	// takes no request must not be given one.
	NoRequest  bool
	MockReturn string // []any{<store row literal>, nil} — pre-rendered
	ExpectType string // []*models.OrderResponse
	ExpectExpr string // success expectedOutput literal — pre-rendered
	// GT-7: multi-call deterministic rendering. Calls is one entry per store
	// call occurrence; Cases is the case table (StoreError / Success / and
	// the logged business-error case when the log has one).
	Calls []CtrlCall
	Cases []CtrlCase
}

// CallsOrDerived returns Calls when the GT-7 multi-call data was built; a
// single-call projection of the legacy StoreCall/StoreArgs fields otherwise
// (template-data compatibility).
func (d TestControllerMethodData) CallsOrDerived() []CtrlCall {
	if len(d.Calls) > 0 {
		return d.Calls
	}
	if d.StoreCall == "" {
		return nil
	}
	// TakesCtx is true here: the legacy single-call path predates the store
	// interface's arity being readable, and defaulting to the matcher keeps
	// every byte it produced before.
	return []CtrlCall{{Method: d.StoreCall, Args: d.StoreArgs, Field: "mockInput", TakesCtx: true}}
}

// CasesOrDerived returns Cases or the legacy two-case table (StoreError /
// Success) from MockReturn/ExpectExpr.
func (d TestControllerMethodData) CasesOrDerived() []CtrlCase {
	if len(d.Cases) > 0 {
		return d.Cases
	}
	calls := d.CallsOrDerived()
	if len(calls) == 0 {
		return nil
	}
	req := make([]CtrlCaseField, 0, len(d.ReqFields))
	for _, f := range d.ReqFields {
		req = append(req, CtrlCaseField{Name: f.Name, Value: f.Value})
	}
	storeErr := make([]string, len(calls))
	for i := range storeErr {
		storeErr[i] = "nil"
	}
	storeErr[0] = `[]any{nil, errors.New("store error")}`
	return []CtrlCase{
		{Desc: "StoreError", ReqFields: req, Inputs: storeErr, ExpectedError: "store error", ExpectedOutput: expectZero(d.ExpectType)},
		{Desc: "Success", ReqFields: req, Inputs: []string{d.MockReturn}, ExpectedOutput: d.ExpectExpr},
	}
}

// expectZero renders the zero expectation for a Go type name.
func expectZero(t string) string {
	switch {
	case t == "":
		return "nil"
	case strings.HasPrefix(t, "[]"), strings.HasPrefix(t, "*"),
		strings.HasPrefix(t, "map["), strings.HasPrefix(t, "chan "), t == "any", t == "interface{}":
		return "nil"
	case t == "string":
		return `""`
	case t == "bool":
		return "false"
	case t == "int", t == "int8", t == "int16", t == "int32", t == "int64",
		t == "uint", t == "uint8", t == "uint16", t == "uint32", t == "uint64",
		t == "float32", t == "float64":
		return "0"
	default:
		return t + "{}"
	}
}

// TestHandlerFileData renders a handler test file
// (examples/item/handler/item_test.txt shape).
type TestHandlerFileData struct {
	TestHeaderData
	Package       string // handler
	ControllerPkg string // controller import (MockItemController)
	LoggerPkg     string
	ModelsPkg     string
	NetworkPkg    string // <module>/pkg/network
	UtilsPkg      string
	SuiteName     string // ItemHandlerSuite
	CtrlMockVar   string // itemController
	CtrlMockType  string // controller.MockItemController
	MockCtor      string // controller.NewMockItemController(gomock.NewController(suite.T()))
	HandlerVar    string // itemHandler
	IfaceName     string // ItemHandler
	CtorCall      string // NewItemHandler(suite.itemController)
	// HasRegisterValidations reports whether the target's utils package
	// actually declares RegisterValidations.
	//
	// It gates the SetupSuite call AND the gin/binding + validator/v10 imports,
	// which appear nowhere else in the file. Setting it unconditionally meant a
	// suite that could not compile against any repo whose pkg/utils has no
	// validator-registration helper — and gating only the call would have traded
	// an undefined-symbol error for an unused-import one.
	HasRegisterValidations bool
	// GomockPkg is the gomock import path, or "" for the uber-go default.
	//
	// It was a hardcoded "github.com/golang/mock/gomock" in the template while
	// the major handed to mockgen came from treeGomock — two decisions about one
	// value that never consulted each other. A module vendoring
	// go.uber.org/mock then got an uber mock and a legacy-importing test, and a
	// package cannot hold both majors, so nothing compiled.
	GomockPkg string
	Methods   []string
}

// ReqField is one request-struct field mirrored into the handler test's
// case struct (GT-D2: the example's per-request case fields, derived).
// Type and Literal mirror CtrlCaseField: a slice-typed request field cannot be
// declared `string` or embedded between quotes, and both defaults are what
// produced F5's compile error on the corpus's []string request fields.
type ReqField struct {
	Name    string // CompCode
	Value   string // assumed fixture value
	Type    string // declared Go type; empty means string
	Literal string // pre-rendered literal replacing the quoted Value
}

// Expr renders the case-struct initialiser, mirroring CtrlCaseField.Expr.
func (f ReqField) Expr() string {
	if f.Literal != "" {
		return f.Literal
	}
	return `"` + f.Value + `"`
}

// TestHandlerMethodData renders one suite method: gin-context table cases
// (Error 500 / Failure 204 / Success 200) over the mocked controller. With a
// log, Cases carries the per-case request values and the logged-error case.
type TestHandlerMethodData struct {
	SuiteName   string // ItemHandlerSuite
	CtrlMockVar string // itemController
	HandlerVar  string // itemHandler
	Name        string // ItemList — the HANDLER's own name
	// CtrlMethod is the controller method the handler invokes, which the
	// mock's EXPECT() must name. It usually equals Name but not always:
	// handler.GetCustomerDummyProfile calls controller.GetCustomerRP, and
	// expecting the handler's name produced a method the mock does not have.
	CtrlMethod string
	// CtrlCallArgs are the arguments the controller call actually receives,
	// rendered. Empty for a controller method that takes no request, which is
	// why it cannot be inferred from the handler's local request var.
	CtrlCallArgs []string
	ReqFields    []ReqField    // request fields driving the case struct
	ReqInit      string        // models.ItemRequest{CompCode: testCase.CompCode}
	SuccessInput string        // []any{<response literal>, nil} — pre-rendered
	RespType     string        // []*models.ItemResponse
	Cases        []HandlerCase // GT-7 case table
}
