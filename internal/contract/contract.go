// Package contract is the uniform language-agnostic service model every
// emitter plugs into (uniform-ir plan §3.1).
//
// It is a deterministic projection of ir.File (+ flow slicing for endpoint
// boundaries): endpoints, request/response fields, query units, params, row
// fields, tpcall dependencies, and loud residue. It carries zero language
// names — no GoHint, no db_method_* template ids, no PascalCase, no
// sql.NullString, no OracleParameter, no gin tags. Those live in namer and
// the per-language template-data projections.
//
// The builder arranges facts; it never invents them: ambiguity stays loud
// (Ambiguous, Residue, Warnings, Skipped), duplicates resolve to canonical
// units exactly like plan does (DuplicateOf → first unit).
package contract

import (
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/sqltext"
)

// QueryKind classifies a database unit by shape. It mirrors ir.QueryType
// with language-neutral spellings (underscores, no template ids).
type QueryKind string

const (
	QuerySelectOne  QueryKind = "select_one"
	QuerySelectMany QueryKind = "select_many"
	QueryInsert     QueryKind = "insert"
	QueryUpdate     QueryKind = "update"
	QueryDelete     QueryKind = "delete"
	QueryMerge      QueryKind = "merge"
)

// QueryKindOf maps the IR query type to the contract kind.
func QueryKindOf(t ir.QueryType) QueryKind {
	switch t {
	case ir.QuerySelectSingle:
		return QuerySelectOne
	case ir.QuerySelectMulti:
		return QuerySelectMany
	case ir.QueryInsert:
		return QueryInsert
	case ir.QueryUpdate:
		return QueryUpdate
	case ir.QueryDelete:
		return QueryDelete
	case ir.QueryMerge:
		return QueryMerge
	default:
		return QueryKind(strings.ToLower(string(t)))
	}
}

// IsDML reports whether the kind mutates.
func (k QueryKind) IsDML() bool {
	switch k {
	case QueryInsert, QueryUpdate, QueryDelete, QueryMerge:
		return true
	}
	return false
}

// FieldKind classifies a contract field by contract role.
type FieldKind string

const (
	FieldRequest  FieldKind = "request"
	FieldResponse FieldKind = "response"
	FieldRow      FieldKind = "row"
	FieldParam    FieldKind = "param"
	FieldError    FieldKind = "error"
)

// Field is one language-neutral contract field: the raw FML identity, the
// :host bind, and the canonical C type. Name derivation (Go/C#/Python) is
// a namer concern, never stored here.
type Field struct {
	FMLName  string    `json:"fml_name,omitempty"`
	HostVar  string    `json:"host_var,omitempty"`
	CType    string    `json:"c_type,omitempty"`
	Nullable bool      `json:"nullable,omitempty"`
	Array    bool      `json:"array,omitempty"`
	Kind     FieldKind `json:"kind"`
	IsErr    bool      `json:"is_err,omitempty"`
	Code     string    `json:"code,omitempty"`
}

// QueryUnit is one database unit's language-neutral shape.
type QueryUnit struct {
	ID         string    `json:"id"`
	Kind       QueryKind `json:"kind"`
	SQL        string    `json:"sql"`
	Tables     []string  `json:"tables"`
	Binds      []string  `json:"binds"`
	RowFields  []Field   `json:"row_fields,omitempty"`
	Params     []Field   `json:"params,omitempty"`
	CursorName string    `json:"cursor_name,omitempty"`
	Line       [2]int    `json:"line"`
	Tx         bool      `json:"tx,omitempty"`
}

// Request is an endpoint's input contract.
type Request struct {
	Fields []Field `json:"fields,omitempty"`
}

// Response is an endpoint's output contract (errors carried separately).
type Response struct {
	Fields []Field `json:"fields,omitempty"`
	Errors []Field `json:"errors,omitempty"`
}

// TPDependency is one tpcall site's language-neutral contract.
type TPDependency struct {
	Service   string  `json:"service"`
	Send      []Field `json:"send,omitempty"`
	Recv      []Field `json:"recv,omitempty"`
	Ambiguous bool    `json:"ambiguous,omitempty"`
	Line      [2]int  `json:"line"`
}

// Endpoint is one convertible arm's language-neutral shape.
type Endpoint struct {
	Name      string         `json:"name"`
	Condition int            `json:"condition,omitempty"`
	Scenario  string         `json:"scenario,omitempty"`
	Request   Request        `json:"request"`
	Response  Response       `json:"response"`
	Queries   []QueryUnit    `json:"queries,omitempty"`
	TPCalls   []TPDependency `json:"tpcalls,omitempty"`
	Residue   []string       `json:"residue,omitempty"`
	LineSpan  [2]int         `json:"line_span"`
	Warnings  []string       `json:"warnings,omitempty"`
}

// Skipped is an IR unit deliberately not converted.
type Skipped struct {
	QueryID string `json:"query_id"`
	Reason  string `json:"reason"`
}

// Service is the deterministic conversion input for one entry file.
type Service struct {
	Name      string     `json:"name"`
	Entry     string     `json:"entry,omitempty"`
	Source    string     `json:"source,omitempty"`
	Endpoints []Endpoint `json:"endpoints"`
	Warnings  []string   `json:"warnings,omitempty"`
	Skipped   []Skipped  `json:"skipped,omitempty"`
}

// QueryInput carries a query plus the tx vote for its unit.
type QueryInput struct {
	Query *ir.Query
	Tx    bool
}

// EndpointInput carries one endpoint's resolved shape for Build.
type EndpointInput struct {
	Name      string
	Condition *ir.Condition
	Scenario  string
	Queries   []QueryInput
	TPCalls   []*ir.TPCall
	Residue   []string
	LineSpan  [2]int
}

// BuildOptions carries the builder inputs: the service identity, the full
// IR (host types, FML ops), and the resolved endpoints (condition slices
// with their query/tpcall membership — resolved once by the caller from
// ir + flow, so endpoint-resolution policy has one home here as it grows).
type BuildOptions struct {
	Name      string
	Entry     string
	Source    string
	Main      *ir.File
	Endpoints []EndpointInput
}

// Build arranges IR facts into the language-neutral service model.
// Deterministic: identical inputs produce byte-identical output.
func Build(opts BuildOptions) *Service {
	svc := &Service{Name: opts.Name, Entry: opts.Entry, Source: opts.Source}
	hostByName := map[string]ir.HostVar{}
	if opts.Main != nil {
		for _, hv := range opts.Main.HostVars {
			hostByName[hv.Name] = hv
			hostByName[strings.ToLower(hv.Name)] = hv
		}
	}
	for _, in := range opts.Endpoints {
		ep := Endpoint{Name: in.Name, Scenario: in.Scenario, LineSpan: in.LineSpan, Residue: in.Residue}
		if in.Condition != nil {
			ep.Condition = in.Condition.Index
			if ep.LineSpan == ([2]int{}) {
				ep.LineSpan = [2]int{in.Condition.StartLine, in.Condition.EndLine}
			}
			ep.Request, ep.Response = contractFromOps(in.Condition.FmlOps)
		}
		for _, qi := range in.Queries {
			if qi.Query == nil {
				continue
			}
			ep.Queries = append(ep.Queries, QueryUnitFor(qi.Query, hostByName, qi.Tx))
		}
		for _, tp := range in.TPCalls {
			if tp == nil {
				continue
			}
			ep.TPCalls = append(ep.TPCalls, TPDependencyFor(tp))
		}
		// Merge scenario/request fields with query params so the request
		// contract covers binds the FML reads alone don't mention.
		ep.Request = mergeRequestParams(ep.Request, ep.Queries)
		svc.Endpoints = append(svc.Endpoints, ep)
	}
	sort.SliceStable(svc.Endpoints, func(i, j int) bool { return svc.Endpoints[i].Name < svc.Endpoints[j].Name })
	return svc
}

// QueryUnitFor projects one IR query to its language-neutral unit.
func QueryUnitFor(q *ir.Query, hosts map[string]ir.HostVar, tx bool) QueryUnit {
	u := QueryUnit{
		ID:         q.ID,
		Kind:       QueryKindOf(q.Type),
		SQL:        sqltext.CanonicalSQL(q.SQL),
		Tables:     append([]string(nil), q.Tables...),
		CursorName: q.CursorName,
		Line:       [2]int{q.StartLine, q.EndLine},
		Tx:         tx,
	}
	u.Binds = sqltext.ExecutableBinds(u.SQL)
	// Fall back to the IR's bind order when canonicalization yields none
	// but the extractor recorded binds (defensive: keeps params loud).
	if len(u.Binds) == 0 && len(q.Binds) > 0 {
		seen := map[string]bool{}
		for _, b := range q.Binds {
			key := strings.ToLower(b)
			if !seen[key] {
				seen[key] = true
				u.Binds = append(u.Binds, b)
			}
		}
	}
	for _, b := range u.Binds {
		hv := hosts[b]
		if hv.Name == "" {
			hv = hosts[strings.ToLower(b)]
		}
		u.Params = append(u.Params, Field{
			HostVar: b, CType: hv.CType, Nullable: hv.Nullable, Array: hv.Array, Kind: FieldParam,
		})
	}
	for _, rs := range q.RowShape {
		base := rowBase(rs)
		if base == "" {
			continue
		}
		hv := hosts[base]
		if hv.Name == "" {
			hv = hosts[strings.ToLower(base)]
		}
		u.RowFields = append(u.RowFields, Field{
			HostVar: base, CType: hv.CType, Nullable: hv.Nullable, Array: hv.Array, Kind: FieldRow,
		})
	}
	return u
}

// TPDependencyFor projects one IR tpcall to its language-neutral contract.
func TPDependencyFor(tp *ir.TPCall) TPDependency {
	d := TPDependency{
		Service:   tp.Service,
		Ambiguous: tp.Ambiguous,
		Line:      [2]int{tp.StartLine, tp.EndLine},
	}
	for _, op := range tp.SendFML {
		d.Send = append(d.Send, fieldFromFmlOp(op, FieldRequest))
	}
	for _, op := range tp.RecvFML {
		d.Recv = append(d.Recv, fieldFromFmlOp(op, FieldResponse))
	}
	return d
}

func contractFromOps(ops []ir.FmlOp) (Request, Response) {
	var req Request
	var resp Response
	seenReq := map[string]bool{}
	seenResp := map[string]bool{}
	seenErr := map[string]bool{}
	for _, op := range ops {
		if op.Dropped {
			continue
		}
		switch op.Kind {
		case ir.FmlGet:
			if seenReq[op.Field] {
				continue
			}
			seenReq[op.Field] = true
			req.Fields = append(req.Fields, fieldFromFmlOp(op, FieldRequest))
		case ir.FmlAdd:
			f := fieldFromFmlOp(op, FieldResponse)
			if f.IsErr {
				if !seenErr[op.Field] {
					seenErr[op.Field] = true
					resp.Errors = append(resp.Errors, Field{
						FMLName: op.Field, HostVar: op.Target, Kind: FieldError, IsErr: true, Code: op.Code,
					})
				}
				continue
			}
			if seenResp[op.Field] {
				continue
			}
			seenResp[op.Field] = true
			resp.Fields = append(resp.Fields, f)
		}
	}
	return req, resp
}

func fieldFromFmlOp(op ir.FmlOp, kind FieldKind) Field {
	return Field{
		FMLName: op.Field, HostVar: op.Target, Kind: kind,
		IsErr: ir.IsErrField(op.Field) || ir.IsErrValue(op.Target), Code: op.Code,
	}
}

func mergeRequestParams(req Request, queries []QueryUnit) Request {
	seen := map[string]bool{}
	for _, f := range req.Fields {
		if f.FMLName != "" {
			seen["fml:"+f.FMLName] = true
		}
		if f.HostVar != "" {
			seen["hv:"+strings.ToLower(f.HostVar)] = true
		}
	}
	for _, q := range queries {
		for _, p := range q.Params {
			if p.HostVar == "" || seen["hv:"+strings.ToLower(p.HostVar)] {
				continue
			}
			seen["hv:"+strings.ToLower(p.HostVar)] = true
			req.Fields = append(req.Fields, p)
		}
	}
	return req
}

func rowBase(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, " \t"); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, ":")
}
