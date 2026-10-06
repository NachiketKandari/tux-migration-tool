package sqlchk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
)

// leakRe matches the SQL keyword shapes that must never appear inside a
// string literal of a SQL-free artifact (PF-6.5): SELECT..FROM, INSERT INTO,
// UPDATE .. SET, DELETE FROM. Requiring the keyword pair keeps ordinary
// messages ("please select an option") out of the findings.
var leakRe = regexp.MustCompile(
	`(?is)\bselect\b[\s\S]*\bfrom\b|\binsert\s+into\b|\bupdate\s+[\w."]+\s+set\b|\bdelete\s+from\b`)

// CheckSQLFree parses Go files that must never contain SQL — controller,
// handler and view artifacts (the pipeline's prompts are SQL-free by
// contract; the artifacts now are checked too) — and reports every string
// literal carrying a SQL keyword shape, attributed to its enclosing
// function when one exists.
func CheckSQLFree(paths []string) ([]Result, error) {
	var results []Result
	for _, path := range paths {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("sqlchk: parse %s: %w", path, err)
		}
		var devs []Deviation
		scan := func(fnName string, body ast.Node) {
			ast.Inspect(body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !leakRe.MatchString(lit.Value) {
					return true
				}
				devs = append(devs, Deviation{
					Kind:   DevSQLLeak,
					Fn:     fnName,
					Detail: fmt.Sprintf("%s:%d: SQL keywords in string literal (%s)", filepath.Base(path), fset.Position(lit.Pos()).Line, fnName),
				})
				return true
			})
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				scan(fd.Name.Name, fd.Body)
				continue
			}
			scan("<package-level>", decl)
		}
		if len(devs) > 0 {
			results = append(results, Result{Method: path, Status: StatusDeviated, Deviations: devs})
		}
	}
	return results, nil
}

// Kinds renders the deviation kinds of a result as a compact list for
// ledger reasons and summary lines.
func Kinds(devs []Deviation) string {
	parts := make([]string, 0, len(devs))
	for _, d := range devs {
		parts = append(parts, string(d.Kind))
	}
	return strings.Join(parts, ",")
}
