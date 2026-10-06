// Package goast implements the mechanical go/ast append model (PRD §4.2.3,
// OQ14, architecture.md Phase 3): interface method accumulation, signature
// inspection, and import merging on generated Go files. All edits are offset
// splices over the original bytes — no printer re-emission — so formatting
// and comments outside the inserted lines survive byte-for-byte. Callers
// serialize appends (plan-conversion §4); the package itself is stateless.
package goast

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Errors surfaced by AccumulateInterface and InspectInterface.
var (
	ErrInterfaceNotFound = errors.New("goast: interface not found")
	ErrSignatureConflict = errors.New("goast: method already present with a different signature")
)

// Emit is the one emission gate (A4.1): parse a Go source string, normalize
// it with go/format, and return the formatted bytes or a parse error. Every
// generated-Go write path (template render, assembled interface files,
// appended controller methods, composed test files) normalizes through this
// one function; the error prefix names the origin for the retry loop.
func Emit(origin, src string) (string, error) {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return "", fmt.Errorf("%s: output does not parse: %w", origin, err)
	}
	return string(formatted), nil
}

// Signature is one method clause of an interface as written in the source.
type Signature struct {
	Name string
	Text string // verbatim clause text, whitespace-trimmed
}

// InspectInterface returns the methods of ifaceName in source order.
// Embedded interfaces (nameless clauses) are skipped — the pipeline only
// generates named methods.
func InspectInterface(path, ifaceName string) ([]Signature, error) {
	f, err := load(path)
	if err != nil {
		return nil, err
	}
	it, err := f.findInterface(ifaceName)
	if err != nil {
		return nil, err
	}
	sigs := make([]Signature, 0, len(it.Methods.List))
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			continue
		}
		sigs = append(sigs, Signature{
			Name: m.Names[0].Name,
			Text: strings.TrimSpace(string(f.src[f.offset(m.Pos()):f.offset(m.End())])),
		})
	}
	return sigs, nil
}

// AccumulateInterface appends sigLine to ifaceName in the Go file at path,
// the accumulating-interface contract of PRD §4.2.3: a method with the same
// name and a structurally identical signature (parameter names are
// documentation, not identity) is a no-op returning added=false — resume
// safe — while the same name with any other shape is an ErrSignatureConflict.
// A missing file (and its parent directories) is created, in which case
// pkgName supplies the package clause. The insertion is a splice: every byte
// outside the inserted line is written back unchanged.
func AccumulateInterface(path, pkgName, ifaceName, sigLine string) (bool, error) {
	if !token.IsIdentifier(pkgName) {
		return false, fmt.Errorf("goast: invalid package name %q", pkgName)
	}
	if !token.IsIdentifier(ifaceName) {
		return false, fmt.Errorf("goast: invalid interface name %q", ifaceName)
	}
	sig, err := parseSigLine(sigLine)
	if err != nil {
		return false, err
	}
	src, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("goast: read %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, fmt.Errorf("goast: create %s: %w", filepath.Dir(path), err)
		}
		body := "package " + pkgName + "\n\ntype " + ifaceName + " interface {\n\t" + sig.text + "\n}\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return false, fmt.Errorf("goast: write %s: %w", path, err)
		}
		return true, nil
	}
	f, err := parse(path, src)
	if err != nil {
		return false, err
	}
	it, err := f.findInterface(ifaceName)
	if err != nil {
		return false, err
	}
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 || m.Names[0].Name != sig.name {
			continue
		}
		if sigEqual(m, f, sig) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s in %s: existing %q, new %q",
			ErrSignatureConflict, sig.name, path,
			strings.TrimSpace(string(f.src[f.offset(m.Pos()):f.offset(m.End())])), sig.text)
	}
	off, layout := f.insertOffset(it)
	out := string(f.src[:off]) + fmt.Sprintf(layout, sig.text) + string(f.src[off:])
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return false, fmt.Errorf("goast: write %s: %w", path, err)
	}
	return true, nil
}

// parsedSig is a validated signature line with the AST pieces needed for
// structural comparison.
type parsedSig struct {
	name  string
	text  string
	field *ast.Field
	fset  *token.FileSet
	src   []byte
}

// parseSigLine validates that line is a single interface-method clause and
// returns its method name. Whitespace runs are collapsed so multi-line
// caller input normalizes to the canonical one-line template shape.
func parseSigLine(line string) (*parsedSig, error) {
	text := strings.Join(strings.Fields(line), " ")
	if text == "" {
		return nil, fmt.Errorf("goast: empty signature")
	}
	src := "package __goast_sig\n\ntype __goast_iface interface {\n\t" + text + "\n}\n"
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "sig.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("goast: parse signature %q: %w", text, err)
	}
	var it *ast.InterfaceType
	for _, d := range af.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		ts := gd.Specs[0].(*ast.TypeSpec)
		it, ok = ts.Type.(*ast.InterfaceType)
		if !ok {
			return nil, fmt.Errorf("goast: signature %q is not an interface method", text)
		}
	}
	if it.Methods == nil || len(it.Methods.List) != 1 || len(it.Methods.List[0].Names) != 1 {
		return nil, fmt.Errorf("goast: signature %q is not a single named method", text)
	}
	fld := it.Methods.List[0]
	return &parsedSig{
		name:  fld.Names[0].Name,
		text:  text,
		field: fld,
		fset:  fset,
		src:   []byte(src),
	}, nil
}

// sigEqual compares two interface-method clauses structurally: parameter and
// result types must match pairwise, ignoring parameter names.
func sigEqual(existing *ast.Field, f *file, sig *parsedSig) bool {
	efn, ok := existing.Type.(*ast.FuncType)
	if !ok {
		return false
	}
	nfn, ok := sig.field.Type.(*ast.FuncType)
	if !ok {
		return false
	}
	ep := flatten(efn.Params)
	np := flatten(nfn.Params)
	if len(ep) != len(np) {
		return false
	}
	er := flatten(efn.Results)
	nr := flatten(nfn.Results)
	if len(er) != len(nr) {
		return false
	}
	for i := range ep {
		if !exprEqual(ep[i], f.fset, f.src, np[i], sig.fset, sig.src) {
			return false
		}
	}
	for i := range er {
		if !exprEqual(er[i], f.fset, f.src, nr[i], sig.fset, sig.src) {
			return false
		}
	}
	return true
}

// flatten expands grouped fields — (a, b string) is one Field with two
// names — into one type entry per parameter, or nil for an absent list.
func flatten(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	out := make([]ast.Expr, 0, len(fl.List))
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, f.Type)
		}
	}
	return out
}

// exprEqual compares two type expressions that may originate from different
// parse trees (and file sets), falling back to source text for the rarer
// shapes the pipeline never generates.
func exprEqual(a ast.Expr, afset *token.FileSet, asrc []byte, b ast.Expr, bfset *token.FileSet, bsrc []byte) bool {
	switch x := a.(type) {
	case *ast.Ident:
		y, ok := b.(*ast.Ident)
		return ok && x.Name == y.Name
	case *ast.SelectorExpr:
		y, ok := b.(*ast.SelectorExpr)
		return ok && x.Sel.Name == y.Sel.Name && exprEqual(x.X, afset, asrc, y.X, bfset, bsrc)
	case *ast.StarExpr:
		y, ok := b.(*ast.StarExpr)
		return ok && exprEqual(x.X, afset, asrc, y.X, bfset, bsrc)
	case *ast.ArrayType:
		y, ok := b.(*ast.ArrayType)
		if !ok {
			return false
		}
		if (x.Len == nil) != (y.Len == nil) {
			return false
		}
		if x.Len != nil && textOf(x.Len, afset, asrc) != textOf(y.Len, bfset, bsrc) {
			return false
		}
		return exprEqual(x.Elt, afset, asrc, y.Elt, bfset, bsrc)
	case *ast.Ellipsis:
		y, ok := b.(*ast.Ellipsis)
		return ok && exprEqual(x.Elt, afset, asrc, y.Elt, bfset, bsrc)
	case *ast.MapType:
		y, ok := b.(*ast.MapType)
		return ok && exprEqual(x.Key, afset, asrc, y.Key, bfset, bsrc) &&
			exprEqual(x.Value, afset, asrc, y.Value, bfset, bsrc)
	default:
		return textOf(a, afset, asrc) == textOf(b, bfset, bsrc)
	}
}

func textOf(e ast.Expr, fset *token.FileSet, src []byte) string {
	return string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
}

// file is one parsed Go source with its original bytes kept for splicing.
type file struct {
	path string
	fset *token.FileSet
	ast  *ast.File
	src  []byte
}

func load(path string) (*file, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("goast: read %s: %w", path, err)
	}
	return parse(path, src)
}

func parse(path string, src []byte) (*file, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("goast: parse %s: %w", path, err)
	}
	return &file{path: path, fset: fset, ast: af, src: src}, nil
}

func (f *file) offset(p token.Pos) int { return f.fset.Position(p).Offset }

func (f *file) findInterface(name string) (*ast.InterfaceType, error) {
	for _, d := range f.ast.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return nil, fmt.Errorf("goast: %s in %s is not an interface", name, f.path)
			}
			return it, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrInterfaceNotFound, name)
}

// insertOffset returns the splice point for a new method clause plus the
// exact text to insert there. After the last existing clause — skipping past
// any trailing same-line comment — for a populated interface; on its own
// line before the closing brace for an empty one (either the template's
// multiline shape or a one-liner). For an interface type the brace positions
// are the Methods field list's Opening/Closing.
func (f *file) insertOffset(it *ast.InterfaceType) (int, string) {
	if it.Methods == nil || len(it.Methods.List) == 0 {
		rbrace := f.offset(it.Methods.Closing)
		lineStart := rbrace
		for lineStart > 0 && f.src[lineStart-1] != '\n' {
			lineStart--
		}
		if strings.TrimSpace(string(f.src[lineStart:rbrace])) == "" {
			return lineStart, "\t%s\n"
		}
		return rbrace, "\n\t%s\n"
	}
	end := f.offset(it.Methods.List[len(it.Methods.List)-1].End())
	if i := bytes.IndexByte(f.src[end:], '\n'); i >= 0 {
		return end + i + 1, "\t%s\n"
	}
	return len(f.src), "\n\t%s\n"
}
