// Package ir is the deterministic extraction layer: it folds tsscan source
// facts into the file-level IR every downstream consumer reads. It never
// decides — no endpoint picking, no renaming, no silent dedup — and it never
// drops facts silently: unbalanced regions ride on File, ambiguity is
// recorded (Ambiguous tpcalls), and duplicates stay factual
// (DedupKey/DuplicateOf; UniqueQueries is the planning-time view).
package ir

import (
	"strings"

	"tux-migration-tool/internal/pred"
)

// QueryType classifies a database unit by shape.
type QueryType string

const (
	QuerySelectSingle QueryType = "SELECT_SINGLE"
	QuerySelectMulti  QueryType = "SELECT_MULTI"
	QueryInsert       QueryType = "INSERT"
	QueryUpdate       QueryType = "UPDATE"
	QueryDelete       QueryType = "DELETE"
	QueryMerge        QueryType = "MERGE"
)

// Template identifiers mirrored as plain strings.
//
// Deprecated: the template registry is a consumer concern. New code must use
// gen.TemplateFor (Go) or the per-language namer instead of reading these
// from the IR. The constants and QueryType.TemplateID stay only as a
// compatibility shim until Phase 5 removes them (uniform-ir plan §3.2).
const (
	TemplateSelectSingle = "db_method_select_single"
	TemplateSelectMulti  = "db_method_select_multi"
	TemplateInsertTx     = "db_method_insert_tx"
	TemplateUpdateTx     = "db_method_update_tx"
	TemplateDeleteTx     = "db_method_delete_tx"
	TemplateMerge        = "db_method_merge"
)

// TemplateID maps the query type to its generation template id.
//
// Deprecated: use gen.TemplateFor instead. Kept for compatibility while
// backends migrate to contract + namer (uniform-ir plan §3.2).
func (q QueryType) TemplateID() string {
	switch q {
	case QuerySelectSingle:
		return TemplateSelectSingle
	case QuerySelectMulti:
		return TemplateSelectMulti
	case QueryInsert:
		return TemplateInsertTx
	case QueryUpdate:
		return TemplateUpdateTx
	case QueryDelete:
		return TemplateDeleteTx
	case QueryMerge:
		return TemplateMerge
	}
	return ""
}

// IsDML reports whether the type is a mutating statement.
func (q QueryType) IsDML() bool {
	switch q {
	case QueryInsert, QueryUpdate, QueryDelete, QueryMerge:
		return true
	}
	return false
}

// IsErrField is the one home of the ERR-field rule: any FML field whose name
// carries "ERR" is an error carrier.
func IsErrField(field string) bool {
	return containsFieldToken(field, "ERR")
}

// IsErrValue is the value side of the error rule: the host variable an
// Fadd32 writes is the service's error-message carrier (c_errmsg and
// friends). Error-ness is decided by the field and the value written —
// never the buffer: the reply is frequently the request buffer reused
// in place, so the buffer a message lands in says nothing. An add that
// is not an error emission is a response write — something is returned
// either way, which is what makes the branch convertible into an API.
func IsErrValue(target string) bool {
	if target == "" {
		return false
	}
	flat := strings.ReplaceAll(strings.ToLower(target), "_", "")
	return strings.Contains(flat, "errmsg")
}

func containsFieldToken(field, token string) bool {
	if len(token) == 0 {
		return false
	}
	for i := 0; i+len(token) <= len(field); i++ {
		if field[i:i+len(token)] == token {
			return true
		}
	}
	return false
}

// FmlOpKind splits FML traffic by direction: Fget32 reads the request in,
// Fadd32 writes the response out, Fdel32 takes a field back OUT of a buffer.
//
// Fdel is a first-class kind rather than an omission because a buffer's
// contents are not the list of adds that precede it — a deleted field is
// absent, and reporting it as present would hand a callee a value the
// caller never sent. See TPField for the folded buffer state.
type FmlOpKind string

const (
	FmlGet FmlOpKind = "get"
	FmlAdd FmlOpKind = "add"
	FmlDel FmlOpKind = "del"
)

// FmlOp is one FML field access. Target is the host variable written (add)
// or filled (get); Code is the legacy error-message code correlated through
// the errlog/strcpy/sprintf writer convention; Optional marks an
// FNOTPRES-guarded read; Dropped marks fields the conversion deliberately
// drops (session/error plumbing).
//
// Composite marks an access whose value/destination is not a bare host
// variable — `c_buf[i]`, `*p`, a call. Target then carries only the leading
// identifier, so a consumer must not read it as the whole expression. A
// string literal is NOT composite: its value is fully known.
type FmlOp struct {
	Kind      FmlOpKind `json:"kind"`
	Field     string    `json:"field"`
	Target    string    `json:"target,omitempty"`
	Buffer    string    `json:"buffer,omitempty"`
	Line      int       `json:"line"`
	Code      string    `json:"code,omitempty"`
	Optional  bool      `json:"optional,omitempty"`
	Dropped   bool      `json:"dropped,omitempty"`
	Error     bool      `json:"error,omitempty"`
	Composite bool      `json:"composite,omitempty"`
}

// FmlBufferRole is the resolved role of an FML buffer variable.
type FmlBufferRole string

const (
	BufferInput   FmlBufferRole = "input"
	BufferOutput  FmlBufferRole = "output"
	BufferSend    FmlBufferRole = "send"
	BufferRecv    FmlBufferRole = "recv"
	BufferUnknown FmlBufferRole = "unknown-role"
)

// BufferRole records one buffer variable and its resolved role. Unknown
// roles are recorded, never guessed.
type BufferRole struct {
	Name string        `json:"name"`
	Role FmlBufferRole `json:"role"`
}

// TPField is one field in a tpcall buffer AT THE CALL LINE — the answer to
// "what is in the S buffer when tpcall runs" and "what does the caller take
// out of the R buffer, into what".
//
// This is the folded view over the SendFML/RecvFML op log, in the same
// relationship UniqueQueries has to Query: the log stays factual and
// duplicate-preserving (every add, every read, as written), the field list
// is the state. The fold is a REPLAY, not a filter, because FML buffers are
// last-write-wins per field and Fdel32 removes a field outright — so the
// contents at the call line are not the adds that precede it. Two
// consequences the log alone cannot express:
//
//   - A field added, deleted, then re-added carries only the re-add.
//   - A buffer tpfree'd or memset between two calls starts EMPTY at the
//     second call; the adds that fed the first are not in it.
//
// That replay is what makes the list trustworthy enough to bind a callee's
// parameters against, which is the whole prerequisite for inlining a service
// call rather than stubbing it.
type TPField struct {
	Field string `json:"field"`
	// Target is the host variable on the other side of the call: the
	// source expression's variable for a send field, the destination
	// variable for a recv field. This is the binding an inline rewrite
	// needs — the callee's parameter becomes the caller's Target.
	Target string `json:"target,omitempty"`
	Line   int    `json:"line"`
	// Composite mirrors FmlOp.Composite: Target is only the leading
	// identifier of a larger expression, so the binding is partial.
	Composite bool `json:"composite,omitempty"`
	// Optional marks an FNOTPRES-guarded read (recv side only).
	Optional bool `json:"optional,omitempty"`
	// Unchecked marks a recv read whose FML error the caller never
	// tested. The field may be absent, leaving Target at whatever it
	// held before — a real hazard for a caller that feeds Target into
	// SQL or a response, and one a bare Fget32 statement makes silently.
	Unchecked bool `json:"unchecked,omitempty"`
	// Note: there is deliberately no Error field here. FmlOp.Error looks
	// like the obvious thing to mirror, but it is never populated by
	// FmlOpOf, so mirroring it would add a second always-false flag. The
	// underlying gap is in the classifier, not here.
}

// TPCall is one correlated service call: the service, the buffers handed
// across it, and the FML contracts on both sides gathered from the
// surrounding block. Empty contracts with identified buffers are ambiguous
// — recorded loudly, never guessed away.
type TPCall struct {
	Service     string  `json:"service"`
	ServiceFile string  `json:"service_file,omitempty"`
	SendBuffer  string  `json:"send_buffer,omitempty"`
	RecvBuffer  string  `json:"recv_buffer,omitempty"`
	SendFML     []FmlOp `json:"send_fml,omitempty"`
	RecvFML     []FmlOp `json:"recv_fml,omitempty"`
	// SendFields/RecvFields are the buffer states at StartLine, folded
	// from the op logs above (see TPField). A consumer that needs to know
	// what actually crosses the call reads these; the FML logs stay the
	// audit trail of the ops that produced them.
	SendFields []TPField `json:"send_fields,omitempty"`
	RecvFields []TPField `json:"recv_fields,omitempty"`
	StartLine  int       `json:"start_line"`
	EndLine    int       `json:"end_line"`
	// Async marks the tpacall (async, no inline reply) variant. Sync
	// tpcall carries service+send+recv buffers; tpacall carries
	// service+send only and the reply arrives via a later tpgetrply,
	// so RecvBuffer/RecvFML stay empty by construction — never flags.
	Async     bool   `json:"async,omitempty"`
	Function  string `json:"function,omitempty"`
	Ambiguous bool   `json:"ambiguous,omitempty"`
	// Callee is the resolved callee's own contract, joined against the
	// buffers above on FML field name (see TPCallee, tpcalljoin.go). It is
	// nil unless exactly ONE corpus file defined the service: a nil Callee
	// is the signal that the KindTPCall placeholder is the right output,
	// and it is what every tpcall in the tracked corpus produces today.
	Callee *TPCallee `json:"callee,omitempty"`
	// JoinRefusal is set when a callee file WAS located but the join could
	// not be made sound (see TPJoinRefusal). A tpcall with no callee file
	// at all sets neither field: that is the long-standing placeholder
	// path, not a refusal, and its output stays byte-identical.
	JoinRefusal TPJoinRefusal `json:"join_refusal,omitempty"`
}

// TPJoinRefusal is why the cross-call join declined to run for a tpcall
// whose callee file was found. Every code here means the corpus is
// ambiguous or the callee could not be understood — never that the two sides
// disagree, which is a TPIssue instead. The distinction matters: a refusal
// says "look at this by hand", an issue says "here is what the code does".
type TPJoinRefusal string

const (
	// RefuseAmbiguousCallee: two or more corpus files define the service
	// (entry name or base name). Picking one would be a guess.
	RefuseAmbiguousCallee TPJoinRefusal = "ambiguous_callee"
	// RefuseCalleeNoEntry: the matched file has no SVC_ entry function, so
	// its ops cannot be attributed to the service — they may belong to a
	// helper. Scoping them to "the whole file" is exactly the wrong answer.
	RefuseCalleeNoEntry TPJoinRefusal = "callee_has_no_entry_function"
	// RefuseCalleeNoBuffers: the callee resolves no input or output FML
	// buffer role, so which ops are reads and which are writes is unknown.
	RefuseCalleeNoBuffers TPJoinRefusal = "callee_buffer_roles_unresolved"
)

// TPIssueCode classifies one cross-call join finding.
//
// The two FNOTPRES codes are NOT errors, and the distinction is load-bearing:
// an absent field in an FML buffer does not produce garbage, it produces a
// FNOTPRES and an untouched destination. See fnotpresNote in tpcalljoin.go
// for the mechanism, which is attached to both as their Note.
type TPIssueCode string

const (
	// IssueCalleeExpectsUnsent: the callee Fget32's a field the caller's
	// send buffer does not carry at the call line.
	IssueCalleeExpectsUnsent TPIssueCode = "callee_expects_unsent"
	// IssueCallerReadsUnwritten: the caller Fget32's a reply field the
	// callee never writes.
	IssueCallerReadsUnwritten TPIssueCode = "caller_reads_unwritten"
	// IssueSentUnread: the caller sends a field the callee never reads.
	// Dead traffic, not a fault.
	IssueSentUnread TPIssueCode = "sent_unread"
	// IssueWrittenUnread: the callee writes a field the caller never reads.
	// Dead traffic, not a fault.
	IssueWrittenUnread TPIssueCode = "written_unread"
	// IssueFieldSplitTarget: one side touches the field through more than
	// one variable, so there is no single binding to report. Recorded
	// rather than silently resolved to the first one in source order.
	IssueFieldSplitTarget TPIssueCode = "field_split_across_targets"
)

// TPBinding is one FML field both sides touch, joined on the FIELD NAME.
//
// The field name is the only exact key available. The host variable is a
// lossy projection of the source expression — `(char*)sql_item_date.arr`
// becomes `sql_item_date`, a varchar whose payload lives in `.arr` — so
// joining on it would bind unrelated values whenever the two sides happened
// to reuse a name, or miss every binding whenever they did not. The field
// name is identical on both sides or it is not a binding at all.
type TPBinding struct {
	Field     string `json:"field"`
	CallerVar string `json:"caller_var,omitempty"`
	CalleeVar string `json:"callee_var,omitempty"`
	// Direction is "in" (caller writes, callee reads), "out" (callee
	// writes, caller reads) or "inout" (both).
	Direction string `json:"direction"`
	// CallerUnchecked carries the caller's own TPField.Unchecked for a
	// reply field: present, but consumed without testing Ferror32.
	CallerUnchecked bool `json:"caller_unchecked,omitempty"`
	// Composite marks either side as a partial projection of a larger
	// expression, so the binding is not a whole-variable rename.
	Composite bool `json:"composite,omitempty"`
}

// TPIssue is one finding from the join. Note always carries the MECHANISM,
// never just a label, so a consumer can act on it without re-deriving the
// FML semantics.
type TPIssue struct {
	Code      TPIssueCode `json:"code"`
	Field     string      `json:"field,omitempty"`
	CallerVar string      `json:"caller_var,omitempty"`
	CalleeVar string      `json:"callee_var,omitempty"`
	Note      string      `json:"note"`
}

// TPCallee is the callee's own contract, projected from its IR and joined
// against the caller's buffers. Expects is every field the callee's entry
// function reads out of an input buffer; Produces every field it writes into
// an output buffer. Both are unions over the entry function's body, not a
// replay: a service "expects" a field if ANY path in it reads that field.
type TPCallee struct {
	Service  string      `json:"service"`
	Path     string      `json:"path"`
	Entry    string      `json:"entry"`
	Expects  []TPField   `json:"expects,omitempty"`
	Produces []TPField   `json:"produces,omitempty"`
	Bindings []TPBinding `json:"bindings,omitempty"`
	Issues   []TPIssue   `json:"issues,omitempty"`
}

// HostVar is a host variable referenced by queries or FML traffic, typed
// from the file's own declarations when possible. CType is the canonical
// source fact (see CanonicalCType); GoHint is a deprecated Go-side
// projection kept for compatibility — new code must use gen.GoTypeFor or
// namer.GoNamer instead (uniform-ir plan §3.2).
type HostVar struct {
	Name             string `json:"name"`
	CType            string `json:"c_type,omitempty"`
	GoHint           string `json:"go_hint,omitempty"`
	Array            bool   `json:"array,omitempty"`
	Nullable         bool   `json:"nullable,omitempty"`
	FromHeader       bool   `json:"from_header,omitempty"`
	InDeclareSection bool   `json:"in_declare_section,omitempty"`
}

// Query is one database unit. Cursor units carry the cursor's raw-case name
// as both ID and CursorName, with CursorFlattened marking the
// DECLARE→CLOSE choreography folded into a single SELECT_MULTI. Duplicates
// stay in the file (DuplicateOf points at the first unit of the group);
// UniqueQueries is the planning-time view.
type Query struct {
	ID              string    `json:"id"`
	Type            QueryType `json:"type"`
	TemplateID      string    `json:"template_id"`
	SQL             string    `json:"sql"`
	Aliases         []string  `json:"aliases,omitempty"`
	StartLine       int       `json:"start_line"`
	EndLine         int       `json:"end_line"`
	OwningFunction  string    `json:"owning_function"`
	CursorName      string    `json:"cursor_name,omitempty"`
	CursorFlattened bool      `json:"cursor_flattened,omitempty"`
	Tables          []string  `json:"tables"`
	Binds           []string  `json:"binds"`
	BindArity       int       `json:"bind_arity"`
	RowShape        []string  `json:"row_shape,omitempty"`
	OrderBy         string    `json:"order_by,omitempty"`
	Sites           []int     `json:"sites"`
	DedupKey        string    `json:"dedup_key"`
	DuplicateOf     string    `json:"duplicate_of,omitempty"`
	DefinedBy       string    `json:"defined_by,omitempty"`
}

// Condition is one member of the entry function's top-level dispatch chain.
// Predicate is the parsed C-precedence tree of the raw Expr text; FlagVars
// are the predicate identifiers that are FML read-targets; FmlOps and
// QueryIDs collect the traffic that lives inside the branch's span.
type Condition struct {
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	Expr  string `json:"expr,omitempty"`
	// Predicate is the parsed form of Expr. RESERVED as data — flow
	// re-parses Expr with define substitution and does NOT read this
	// field, so the two parses can diverge by construction; keep them in
	// sync or drop this one in a later version (engine-wiring audit
	// Tier-2 note).
	Predicate *pred.Expr `json:"predicate,omitempty"`
	FlagVars  []string   `json:"flag_vars,omitempty"`
	StartLine int        `json:"start_line"`
	EndLine   int        `json:"end_line"`
	FmlOps    []FmlOp    `json:"fml_ops,omitempty"`
	QueryIDs  []string   `json:"query_ids,omitempty"`
	IsDefault bool       `json:"is_default,omitempty"`
}

// ContainsLine is the one ownership predicate for associating facts with a
// condition's branch span.
func (c *Condition) ContainsLine(line int) bool {
	return line >= c.StartLine && line <= c.EndLine
}

// ExternalFn is a project-convention symbol (fn_*/chk_* prefix) called but
// not defined locally; directory mode resolves it against the corpus.
type ExternalFn struct {
	Name      string   `json:"name"`
	Resolved  bool     `json:"resolved,omitempty"`
	DefinedIn string   `json:"defined_in,omitempty"`
	HasSQL    bool     `json:"has_sql,omitempty"`
	Callsites []int    `json:"callsites"`
	QueryIDs  []string `json:"query_ids,omitempty"`
}

// Unbalanced is a loud record of a construct the scanner could not close.
type Unbalanced struct {
	Kind string `json:"kind"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// Define is one #define/#undef fact with its lexical scope: file scope when
// Function is empty, function scope (from its line onward, shadowing file
// scope) otherwise. Macros are audit-only and never substituted.
type Define struct {
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Line     int    `json:"line"`
	Function string `json:"function,omitempty"`
	Macro    bool   `json:"macro,omitempty"`
	Undef    bool   `json:"undef,omitempty"`
}

// File is the extracted IR for one source file.
type File struct {
	Path            string       `json:"path"`
	Entry           string       `json:"entry,omitempty"`
	Fragment        bool         `json:"fragment,omitempty"`
	Functions       []string     `json:"functions"`
	Defines         []Define     `json:"defines,omitempty"`
	BranchCount     int          `json:"branch_count"`
	BranchingFactor int          `json:"branching_factor"`
	Conditions      []Condition  `json:"conditions,omitempty"`
	FmlOps          []FmlOp      `json:"fml_ops,omitempty"`
	Buffers         []BufferRole `json:"buffers,omitempty"`
	TPCalls         []TPCall     `json:"tpcalls,omitempty"`
	Queries         []*Query     `json:"queries"`
	HostVars        []HostVar    `json:"host_vars"`
	ExternalFns     []ExternalFn `json:"external_fns,omitempty"`
	Unbalanced      []Unbalanced `json:"unbalanced,omitempty"`
	// FunctionSpans gives each declared function's body line range.
	// Functions is the name list and stays as it is; this is the span, and
	// it exists because FmlOp records no owning function. Anything that has
	// to reason about ONE function's ops — the cross-call join projecting a
	// callee's contract — has to bound them by line instead. Scoping to
	// "the whole file" is not a safe approximation: a helper's Fget32 would
	// be reported as something the service expects, and a binding could
	// name the wrong variable.
	FunctionSpans []FuncSpan `json:"function_spans,omitempty"`
	// ParseErrors carries the grammar's recovery nodes — source the parser
	// could not fully understand (engine-wiring audit Tier-2: the scanner
	// computed them, no stage surfaced them; "never a silent drop" is a
	// README promise).
	ParseErrors []ParseError `json:"parse_errors,omitempty"`
}

// ParseError is one grammar-recovery site, mirrored from tsscan so the IR
// archive carries it without importing the scanner's fact types.
type ParseError struct {
	Kind string `json:"kind"` // "error" | "missing"
	Node string `json:"node"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// FuncSpan is one function's body line range, inclusive. A zero BodyEnd
// means the scanner never closed the body; treat the span as open rather
// than as empty, since a zero-width span would silently drop real ops.
type FuncSpan struct {
	Name      string `json:"name"`
	BodyStart int    `json:"body_start"`
	BodyEnd   int    `json:"body_end"`
}

// Condition returns the condition with the given 1-based index — the one
// lookup for plan (map building) and gen (linear scans).
func (f *File) Condition(index int) *Condition {
	for i := range f.Conditions {
		if f.Conditions[i].Index == index {
			return &f.Conditions[i]
		}
	}
	return nil
}

// SameCursor compares cursor names case-insensitively (the scanner
// uppercases, the IR keeps raw casing).
func SameCursor(a, b string) bool {
	return equalFold(a, b)
}

// UniqueQueries returns the first unit of every DedupKey group, in IR order.
func (f *File) UniqueQueries() []*Query {
	seen := make(map[string]bool, len(f.Queries))
	out := make([]*Query, 0, len(f.Queries))
	for _, q := range f.Queries {
		if seen[q.DedupKey] {
			continue
		}
		seen[q.DedupKey] = true
		out = append(out, q)
	}
	return out
}
