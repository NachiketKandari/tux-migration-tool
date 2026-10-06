package goast

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

// AddImports merges importPaths into the file's import declaration,
// preserving named imports. Paths are grouped gofmt-style — first path
// segment without a dot (stdlib and project-local) before dotted external
// paths — and sorted within each group, matching the template-rendered
// shape (db_interface_file.tmpl). The declaration is rebuilt as text
// between its original bounds; a file without one gets a block inserted
// after the package clause.
func AddImports(path string, importPaths ...string) error {
	if len(importPaths) == 0 {
		return nil
	}
	f, err := load(path)
	if err != nil {
		return err
	}
	existing := map[string]string{}
	var decl *ast.GenDecl
	for _, d := range f.ast.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if decl != nil {
			return fmt.Errorf("goast: multiple import declarations in %s", path)
		}
		decl = gd
		for _, s := range gd.Specs {
			is := s.(*ast.ImportSpec)
			p, err := strconv.Unquote(textOf(is.Path, f.fset, f.src))
			if err != nil {
				return fmt.Errorf("goast: import path in %s: %w", path, err)
			}
			name := ""
			if is.Name != nil {
				name = is.Name.Name
			}
			existing[p] = name
		}
	}
	for _, p := range importPaths {
		if p == "" {
			return fmt.Errorf("goast: empty import path")
		}
		if _, ok := existing[p]; !ok {
			existing[p] = ""
		}
	}
	var local, external []string
	for p := range existing {
		if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
			external = append(external, p)
		} else {
			local = append(local, p)
		}
	}
	sort.Strings(local)
	sort.Strings(external)

	render := func(paths []string) []string {
		lines := make([]string, 0, len(paths))
		for _, p := range paths {
			if name := existing[p]; name != "" {
				lines = append(lines, "\t"+name+" "+strconv.Quote(p))
			} else {
				lines = append(lines, "\t"+strconv.Quote(p))
			}
		}
		return lines
	}
	var sb strings.Builder
	sb.WriteString("import (\n")
	for _, l := range render(local) {
		sb.WriteString(l + "\n")
	}
	if len(local) > 0 && len(external) > 0 {
		sb.WriteString("\n")
	}
	for _, l := range render(external) {
		sb.WriteString(l + "\n")
	}
	sb.WriteString(")")
	block := sb.String()

	var out string
	if decl == nil {
		pkgEnd := f.offset(f.ast.Name.End())
		lineEnd := pkgEnd
		if i := indexByteFrom(f.src, pkgEnd); i >= 0 {
			lineEnd = i + 1
		}
		out = string(f.src[:lineEnd]) + "\n" + block + "\n" + string(f.src[lineEnd:])
	} else {
		start := f.offset(decl.Pos())
		end := f.offset(decl.End())
		out = string(f.src[:start]) + block + string(f.src[end:])
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("goast: write %s: %w", path, err)
	}
	return nil
}

func indexByteFrom(src []byte, from int) int {
	for i := from; i < len(src); i++ {
		if src[i] == '\n' {
			return i
		}
	}
	return -1
}
