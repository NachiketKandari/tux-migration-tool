// Package sqlchk is the SQL fidelity gate (PF-6): it proves every generated
// db method carries the source Tux SQL — same columns, same tables, same
// conditions — tolerant only of alias renames and bind-style changes. The
// comparison is a normalized structural projection over stdlib tokenization
// (no SQL-parser dependency, R4); deviations are typed facts surfaced
// flag-only through the ledger and run summary — verification observes,
// never gates.
package sqlchk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// Status is one fidelity check outcome.
type Status string

const (
	StatusMatch        Status = "match"
	StatusDeviated     Status = "deviated"
	StatusUnverifiable Status = "unverifiable"
)

// DeviationKind classifies one difference between source and generated SQL.
type DeviationKind string

const (
	DevColumns DeviationKind = "columns"
	DevTables  DeviationKind = "tables"
	DevWhere   DeviationKind = "where"
	DevSet     DeviationKind = "set"
	DevValues  DeviationKind = "values"
	DevOrder   DeviationKind = "order"
	DevBinds   DeviationKind = "binds"
	// DevSQLLeak marks SQL keyword shapes inside a string literal of an
	// artifact that must be SQL-free (controller/handler/view, PF-6.5).
	DevSQLLeak DeviationKind = "sql-leak"
)

// Deviation is one typed difference with a bounded detail for the run log
// and audit trail; Fn carries the enclosing Go func for SQL-leak
// attribution.
type Deviation struct {
	Kind   DeviationKind `json:"kind"`
	Detail string        `json:"detail,omitempty"`
	Fn     string        `json:"fn,omitempty"`
}

// Target is one db method's fidelity input: the plan unit's Go method name,
// the canonical IR query ID, and the source SQL (ir.Query.SQL).
type Target struct {
	Method  string `json:"method"`
	QueryID string `json:"query_id"`
	Source  string `json:"source"`
}

// Result is one check outcome.
type Result struct {
	Method     string      `json:"method"`
	QueryID    string      `json:"query_id"`
	Status     Status      `json:"status"`
	Deviations []Deviation `json:"deviations,omitempty"`
}

// CompareTargets is the shared fidelity skeleton (A2.7): for each target,
// a missing literal is unverifiable (visible, never a silent pass — PF-6.6),
// otherwise the source and generated SQL compare through the normalizer.
// The literal extractor is the caller's language-specific half (Go backtick
// literals, Python triple-quoted constants). The projected identity fields
// are extracted by the accessor funcs; the lookup returns the generated SQL.
func CompareTargets[T any](targets []T, lookup func(t T) (gen string, ok bool),
	method, queryID, source func(t T) string) []Result {
	results := make([]Result, 0, len(targets))
	for _, t := range targets {
		gen, ok := lookup(t)
		if !ok {
			results = append(results, Result{
				Method:  method(t),
				QueryID: queryID(t),
				Status:  StatusUnverifiable,
			})
			continue
		}
		devs := Compare(source(t), gen)
		status := StatusMatch
		if len(devs) > 0 {
			status = StatusDeviated
		}
		results = append(results, Result{
			Method:     method(t),
			QueryID:    queryID(t),
			Status:     status,
			Deviations: devs,
		})
	}
	return results
}

// CheckDBFile parses the written db-methods file and compares every target
// method's embedded SQL raw literal against the source SQL through the same
// normalizer. A method with no extractable literal is unverifiable — a
// visible fact, never a silent pass (PF-6.6).
func CheckDBFile(path string, targets []Target) ([]Result, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("sqlchk: parse %s: %w", path, err)
	}
	literals := sqlLiterals(f)
	return CompareTargets(targets,
		func(t Target) (string, bool) { gen, ok := literals[t.Method]; return gen, ok },
		func(t Target) string { return t.Method },
		func(t Target) string { return t.QueryID },
		func(t Target) string { return t.Source }), nil
}

// sqlLiterals maps each top-level func name to its first backtick raw-string
// literal — the db templates embed exactly one per method (`query := `<SQL>“).
func sqlLiterals(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.HasPrefix(lit.Value, "`") {
				if _, dup := out[fd.Name.Name]; !dup {
					out[fd.Name.Name] = strings.TrimSuffix(strings.TrimPrefix(lit.Value, "`"), "`")
				}
			}
			return true
		})
	}
	return out
}
