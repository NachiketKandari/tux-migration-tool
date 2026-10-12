// Package tsscan is the tree-sitter based structural scanner for Pro*C/Tuxedo
// sources. It produces a pinned fact vocabulary through a real C grammar
// instead of character heuristics.
//
// The strategy is mask-and-parse: a small, auditable pre-scan locates the
// Pro*C-only constructs (EXEC SQL statements), masks each region into a
// same-length block comment — every byte offset, line and column of the
// surrounding C code is preserved exactly — and stock tree-sitter-c parses
// the masked source. SQL statement facts come from the pre-scan; every C
// structural fact comes from the AST. Comments are classified by the pinned
// banner rules, and unbalanced regions fail loud.
package tsscan

// SQLKind classifies an EXEC SQL statement. The numeric order is pinned
// (golden-checked) so downstream JSON stays
// stable.
type SQLKind int

const (
	SQLUnknown SQLKind = iota
	SQLSelect
	SQLDeclareCursor
	SQLInsert
	SQLUpdate
	SQLDelete
	SQLMerge
	SQLOpen
	SQLFetch
	SQLClose
	SQLDeclareSection
	SQLInclude
	SQLCommit
	SQLRollback
	SQLConnect
	SQLOther
	// SQLOpenCursorFor is the EXECUTE-wrapped cursor: a declare and an open
	// fused into one embedded block, so the cursor's SELECT rides along with
	// the open instead of standing as its own DECLARE. It is appended last so
	// the pinned numeric values above never shift.
	SQLOpenCursorFor
)

// IsQuery reports whether the kind is a logical database query (the SELECT,
// DECLARE CURSOR and DML families; cursor choreography and plumbing are not).
func (k SQLKind) IsQuery() bool {
	switch k {
	case SQLSelect, SQLDeclareCursor, SQLInsert, SQLUpdate, SQLDelete, SQLMerge,
		SQLOpenCursorFor:
		return true
	}
	return false
}

// String renders the pinned kind vocabulary.
func (k SQLKind) String() string {
	switch k {
	case SQLSelect:
		return "SELECT"
	case SQLDeclareCursor:
		return "DECLARE_CURSOR"
	case SQLInsert:
		return "INSERT"
	case SQLUpdate:
		return "UPDATE"
	case SQLDelete:
		return "DELETE"
	case SQLMerge:
		return "MERGE"
	case SQLOpen:
		return "OPEN"
	case SQLFetch:
		return "FETCH"
	case SQLClose:
		return "CLOSE"
	case SQLDeclareSection:
		return "DECLARE_SECTION"
	case SQLInclude:
		return "INCLUDE"
	case SQLCommit:
		return "COMMIT"
	case SQLRollback:
		return "ROLLBACK"
	case SQLConnect:
		return "CONNECT"
	case SQLOther:
		return "OTHER"
	case SQLOpenCursorFor:
		return "OPEN_CURSOR_FOR"
	}
	return "UNKNOWN"
}

// ExecSQLStatement is one EXEC SQL region, classified by the pre-scan.
// StartLine/StartCol locate the EXEC keyword; EndLine the terminating ';'.
// Raw is the statement text after the EXEC SQL marker; Normalized collapses
// whitespace runs to single spaces. CursorName is uppercased (pinned
// convention) for cursor choreography statements.
type ExecSQLStatement struct {
	// Raw is the byte-exact source statement — the audit-trail twin of
	// Normalized. Every engine reads Normalized; Raw exists so the scan
	// goldens/JSON can prove the normalization lost nothing
	// (engine-wiring audit Tier-2 note).
	Raw        string
	Normalized string
	Kind       SQLKind
	StartLine  int
	StartCol   int
	EndLine    int
	CursorName string
	Func       string
}

// FunctionDef is a C function definition. StartLine/Col locate the function
// name; BodyStartLine is the line of the opening brace and BodyEndLine the
// line of its matching close (0 when the brace never closes — an unbalanced
// file resolves what it can and reports the mismatch).
type FunctionDef struct {
	Name          string
	ReturnType    string
	StartLine     int
	Col           int
	BodyStartLine int
	BodyEndLine   int
}

// FunctionCall is a call site. Args is the raw text between the call's
// parens ("" when the parenthesization is unbalanced).
// IsTpCall is true for both tpcall (sync) and tpacall (async): tpacall is
// the async variant with no inline reply buffer, but for discovery,
// analysis scoring, buffer roles, and placeholder generation it carries
// the same outbound-service dependency signal. Use Name to distinguish
// sync ("tpcall") from async ("tpacall") when the difference matters
// (e.g. tpacall has no recv-buffer arg — args[3] is flags, and the reply
// arrives via a later tpgetrply).
type FunctionCall struct {
	Name      string
	Line      int
	Col       int
	Args      string
	IsTpCall  bool
	IsFnPref  bool
	IsChkPref bool
	Func      string
}

// IsTpCallName reports whether a callee name is a Tuxedo service call —
// tpcall or its async sibling tpacall. Case-sensitive: ATMI names are
// lowercase in every corpus sample.
func IsTpCallName(name string) bool {
	return name == "tpcall" || name == "tpacall"
}

// Directive is a raw preprocessor fact, recorded but never interpreted.
// IsHeader/IsSystem classify include directives; RESERVED as data — no
// engine reads the classification yet (engine-wiring audit Tier-2 note).
type Directive struct {
	Kind     string
	Arg      string
	Line     int
	IsHeader bool
	IsSystem bool
}

// BranchKind discriminates the flattened if/else-if/else chain members.
type BranchKind string

const (
	BranchIf     BranchKind = "if"
	BranchElseIf BranchKind = "elseif"
	BranchElse   BranchKind = "else"
)

// Branch is one member of an if/else-if/else chain, flattened so chain
// siblings stay siblings. Block extents are the braces' lines and columns
// (0 when the arm is unbraced). Depth is the brace depth at the keyword
// (function body top level == 1). NestDepth counts enclosing braced
// if/else-if blocks — the branching-factor doubling input.
type Branch struct {
	Kind          BranchKind
	Cond          string
	StartLine     int
	StartCol      int
	BlockStart    int
	BlockEnd      int
	BlockStartCol int
	BlockEndCol   int
	Depth         int
	NestDepth     int
	Function      string
}

// LoopKind discriminates loop headers.
type LoopKind string

const (
	LoopFor   LoopKind = "for"
	LoopWhile LoopKind = "while"
	LoopDo    LoopKind = "do"
)

// Loop is one loop header. A do-while is a single record whose tail while()
// fills Cond and WhileLine. NestDepth counts enclosing braced if/else-if
// blocks plus enclosing loops.
type Loop struct {
	Kind          LoopKind
	Cond          string
	StartLine     int
	StartCol      int
	BlockStart    int
	BlockEnd      int
	BlockStartCol int
	BlockEndCol   int
	Depth         int
	NestDepth     int
	WhileLine     int
	Function      string
}

// Return is a C return statement (tpreturn rides in Calls, not here).
type Return struct {
	Line int
	Col  int
	Func string
}

// VarDecl is a variable declaration. Type is the base type text; pointers
// are not part of Type. Func names the enclosing function ("" for
// file-scope declarations; parameters ride in Params, not VarDecls).
type VarDecl struct {
	Type  string
	Name  string
	Line  int
	Col   int
	Array bool
	Func  string
}

// CommentKind discriminates the comment inventory.
type CommentKind string

const (
	CommentBlock  CommentKind = "block"
	CommentLine   CommentKind = "line"
	CommentBanner CommentKind = "banner"
)

// Comment is one comment span. Banner comments (the "Ver N added/comment"
// convention) delimit live regions: single-line banners are live, multi-line
// banner blocks bury commented-out code and are dead.
type Comment struct {
	Kind      CommentKind
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	Live      bool
}

// UnbalancedRegion is a loud record of a construct the pre-scan could not
// close: an unterminated block comment, an EXEC SQL running to EOF, or an
// unmatched brace.
type UnbalancedRegion struct {
	Kind      string
	StartLine int
	StartCol  int
}

// ParseError is additive: a node the C grammar could not recover from, or a
// token it had to synthesize. The scanner never drops these silently.
type ParseError struct {
	Kind string // "error" | "missing"
	Node string
	Line int
	Col  int
}

// Switch is additive: a switch statement header with its block extents.
// It is an additive fact beyond the pinned vocabulary.
type Switch struct {
	Cond          string
	StartLine     int
	BlockStart    int
	BlockEnd      int
	BlockStartCol int
	BlockEndCol   int
	Depth         int
	NestDepth     int
	Function      string
}

// SourceFacts is the aggregate structural record for one source file. All
// positions are 1-based line/column and byte-faithful to the original file,
// masking included.
type SourceFacts struct {
	Path        string
	NumLines    int
	Directives  []Directive
	Functions   []FunctionDef
	Calls       []FunctionCall
	AllSQL      []ExecSQLStatement
	Queries     []ExecSQLStatement
	Branches    []Branch
	Loops       []Loop
	Returns     []Return
	VarDecls    []VarDecl
	Params      []VarDecl
	Comments    []Comment
	Unbalanced  []UnbalancedRegion
	TpCallCount int
	Fragment    bool
	ParseErrors []ParseError
	Switches    []Switch
}

// InComment reports whether the position lies inside any recorded comment
// span — the queryable "is this position code?" check.
func (f *SourceFacts) InComment(line, col int) bool {
	for i := range f.Comments {
		c := &f.Comments[i]
		if before(c.StartLine, c.StartCol, line, col) || after(c.EndLine, c.EndCol, line, col) {
			continue
		}
		return true
	}
	return false
}

func before(l1, c1, l2, c2 int) bool {
	return l1 > l2 || (l1 == l2 && c1 > c2)
}

func after(l1, c1, l2, c2 int) bool {
	return l1 < l2 || (l1 == l2 && c1 < c2)
}
