// Package testscan implements the gentest scan phase (PRD-2026-09-09
// GT-D1): it resolves a gentest target inside a converted Go service tree
// (pkg/services/<svc>/{db,controller,handler,models} — or any directory
// shaped like it), inventories the functions each layer defines, and
// detects which functions already have tests. Detection is two-pass
// heuristic evidence — exact Test<Fn> names and call-site invocations —
// recorded per function so the gap report stays auditable. The package is
// read-only over the target tree.
package testscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// layerOf classifies a directory base name into its service layer; "model"
// and "models" both classify as the models layer (convert writes models/,
// the item reference corpus uses model/).
func layerOf(name string) Layer {
	switch name {
	case "db":
		return LayerDB
	case "controller":
		return LayerController
	case "handler":
		return LayerHandler
	case "models", "model":
		return LayerModels
	}
	return ""
}

// isServiceDir reports whether dir holds at least one testable layer
// subdirectory — the shape of a converted service.
func isServiceDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch layerOf(e.Name()) {
		case LayerDB, LayerController, LayerHandler:
			return true
		}
	}
	return false
}

// normalizeFilter defaults the nil filter to every testable layer and
// rejects non-testable layers in an explicit one.
func normalizeFilter(layers []Layer) ([]Layer, error) {
	if len(layers) == 0 {
		return []Layer{LayerDB, LayerController, LayerHandler}, nil
	}
	var out []Layer
	for _, l := range layers {
		switch l {
		case LayerDB, LayerController, LayerHandler:
			if !filterHas(out, l) {
				out = append(out, l)
			}
		default:
			return nil, fmt.Errorf("gentest: -layers: %q is not a testable layer (want db, controller, handler)", string(l))
		}
	}
	return out, nil
}

func filterHas(filter []Layer, l Layer) bool {
	for _, f := range filter {
		if f == l {
			return true
		}
	}
	return false
}

func checkFilter(l Layer, filter []Layer, path string) error {
	if !filterHas(filter, l) {
		return fmt.Errorf("gentest: %s is in the %s layer, excluded by -layers", path, l)
	}
	return nil
}

// Resolve classifies target — a .go file, a layer dir, a service dir, or a
// services root — into the concrete layer directories gentest scans,
// restricted to the layers filter. A module root works as a services root
// through a bounded pkg/services (or services) descent. Unrecognized shapes
// error loudly; the models layer is never a test target.
func Resolve(target string, layers []Layer) (*Target, error) {
	filter, err := normalizeFilter(layers)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("gentest: cannot access target %s: %w", target, err)
	}
	if !fi.IsDir() {
		return resolveFile(target, filter)
	}
	switch l := layerOf(filepath.Base(target)); l {
	case LayerDB, LayerController, LayerHandler:
		if err := checkFilter(l, filter, target); err != nil {
			return nil, err
		}
		return &Target{
			Path: target, Mode: ModeLayer, Root: filepath.Dir(target),
			LayerDirs: []LayerDir{{
				Service: filepath.Base(filepath.Dir(target)), ServiceDir: filepath.Dir(target),
				Layer: l, Dir: target,
			}},
		}, nil
	}
	if isServiceDir(target) {
		dirs, err := serviceLayerDirs(target, filter)
		if err != nil {
			return nil, err
		}
		return &Target{Path: target, Mode: ModeService, Root: target, LayerDirs: dirs}, nil
	}
	if services := serviceRoots(target); len(services) > 0 {
		return resolveRoot(target, target, services, filter)
	}
	for _, cand := range []string{filepath.Join(target, "pkg", "services"), filepath.Join(target, "services")} {
		if services := serviceRoots(cand); len(services) > 0 {
			return resolveRoot(target, cand, services, filter)
		}
	}
	return nil, fmt.Errorf("gentest: %s does not look like a converted service tree — want a .go file, a db/controller/handler layer dir, a service dir with layer subdirectories, or a services root", target)
}

// serviceRoots returns the child directories of dir that are service dirs,
// in directory order.
func serviceRoots(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && isServiceDir(filepath.Join(dir, e.Name())) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// resolveRoot builds layer dirs for every service under root that has at
// least one filter-matching layer directory; a root where no service
// matches errors.
func resolveRoot(target, root string, services []string, filter []Layer) (*Target, error) {
	var dirs []LayerDir
	for _, svc := range services {
		sdirs, err := serviceLayerDirs(svc, filter)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, sdirs...)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("gentest: no service under %s has a %v layer directory (check -layers)", root, filter)
	}
	return &Target{Path: target, Mode: ModeRoot, Root: root, LayerDirs: dirs}, nil
}

// serviceLayerDirs maps one service dir onto its filter-matching testable
// layer directories.
func serviceLayerDirs(serviceDir string, filter []Layer) ([]LayerDir, error) {
	name := filepath.Base(serviceDir)
	var dirs []LayerDir
	for _, l := range []Layer{LayerDB, LayerController, LayerHandler} {
		if !filterHas(filter, l) {
			continue
		}
		dir := filepath.Join(serviceDir, string(l))
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		dirs = append(dirs, LayerDir{Service: name, ServiceDir: serviceDir, Layer: l, Dir: dir})
	}
	return dirs, nil
}

// resolveFile classifies a single .go target by its containing directory:
// a layer dir pins the layer and the service (its parent); any other
// location degrades to the "other" layer with a bounded walk up (≤3 levels)
// for the nearest service dir. Models files are never a test target.
func resolveFile(path string, filter []Layer) (*Target, error) {
	dir := filepath.Dir(path)
	l := layerOf(filepath.Base(dir))
	if l == LayerModels {
		return nil, fmt.Errorf("gentest: %s is in the models layer — models are not a test target", path)
	}
	if l == "" {
		l = LayerOther
	} else if err := checkFilter(l, filter, path); err != nil {
		return nil, err
	}
	serviceDir := ""
	if l != LayerOther {
		serviceDir = filepath.Dir(dir)
	} else {
		// Bounded walk-up: 3 levels, so a stray .go file never reaches the
		// filesystem root hunting for a service dir.
		for d, i := dir, 0; i < 3 && d != filepath.Dir(d); d, i = filepath.Dir(d), i+1 {
			if isServiceDir(d) {
				serviceDir = d
				break
			}
		}
	}
	ld := LayerDir{Layer: l, Dir: dir}
	if serviceDir != "" {
		ld.Service = filepath.Base(serviceDir)
		ld.ServiceDir = serviceDir
	}
	return &Target{Path: path, Mode: ModeFile, Root: dir, LayerDirs: []LayerDir{ld}}, nil
}

// Scan inventories the target's functions and detects existing test
// coverage. Files that fail to parse are skipped with a recorded warning —
// never silent.
func (t *Target) Scan() (*Report, error) {
	rep := &Report{Target: t.Path, Mode: t.Mode}
	byService := map[string]int{}
	for _, ld := range t.LayerDirs {
		lr := scanLayer(ld, rep)
		if i, ok := byService[ld.ServiceDir]; ok {
			rep.Services[i].Layers = append(rep.Services[i].Layers, lr)
			continue
		}
		byService[ld.ServiceDir] = len(rep.Services)
		rep.Services = append(rep.Services, ServiceReport{Name: ld.Service, Dir: ld.ServiceDir, Layers: []LayerReport{lr}})
	}
	return rep, nil
}

// scanLayer inventories one layer directory: non-test, non-mock Go files
// supply the function inventory; *_test.go files supply the detection
// corpus. An existing but empty layer dir warns (nothing to scan is a
// fact worth surfacing, e.g. a .txt reference corpus).
func scanLayer(ld LayerDir, rep *Report) LayerReport {
	lr := LayerReport{Layer: ld.Layer, Dir: ld.Dir}
	entries, err := os.ReadDir(ld.Dir)
	if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("unreadable %s: %v", ld.Dir, err))
		return lr
	}
	var srcs, tests []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, "_test.go"):
			tests = append(tests, name)
		case strings.HasSuffix(name, "_mock.go") || strings.HasPrefix(name, "mock_"):
			// mockgen output — mock methods are not test targets
		default:
			srcs = append(srcs, name)
		}
	}
	if len(srcs) == 0 && len(tests) == 0 {
		rep.Warnings = append(rep.Warnings, ld.Dir+": no Go files found")
		return lr
	}
	fset := token.NewFileSet()
	lr.Funcs, rep.Warnings = inventory(ld.Dir, srcs, fset, rep.Warnings)
	nameEv, callEv, warns := testEvidence(ld.Dir, tests, fset)
	rep.Warnings = append(rep.Warnings, warns...)
	for i := range lr.Funcs {
		f := &lr.Funcs[i]
		if ev, ok := nameEv["Test"+f.Name]; ok {
			f.HasTest = true
			f.Evidence = append(f.Evidence, *ev)
		}
		if ev, ok := callEv[f.Name]; ok {
			f.HasTest = true
			f.Evidence = append(f.Evidence, *ev)
		}
	}
	return lr
}

// inventory parses the layer's source files into Func entries (methods and
// standalone funcs; constructors flagged by the New-prefix + single-result
// shape).
func inventory(dir string, names []string, fset *token.FileSet, warns []string) ([]Func, []string) {
	var funcs []Func
	for _, name := range names {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			warns = append(warns, fmt.Sprintf("skipped %s: %v", filepath.Join(dir, name), err))
			continue
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcs = append(funcs, Func{
				Name:     fd.Name.Name,
				Recv:     receiverName(fd),
				Exported: fd.Name.IsExported(),
				Ctor:     fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1,
				File:     name,
				Line:     fset.Position(fd.Pos()).Line,
			})
		}
	}
	return funcs, warns
}

func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	switch t := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// testEvidence scans the layer's test files: every Test-prefixed function
// or method name feeds the name pass, and every call expression inside a
// Test-prefixed body — selector or plain identifier — feeds the call-site
// pass. Calls in non-Test helpers never count as evidence. First evidence
// per name wins (deterministic directory order).
func testEvidence(dir string, names []string, fset *token.FileSet) (nameEv, callEv map[string]*Evidence, warns []string) {
	nameEv = map[string]*Evidence{}
	callEv = map[string]*Evidence{}
	for _, name := range names {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			warns = append(warns, fmt.Sprintf("skipped %s: %v", filepath.Join(dir, name), err))
			continue
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fd.Name.Name, "Test") || fd.Body == nil {
				continue
			}
			tn := fd.Name.Name
			if _, exists := nameEv[tn]; !exists {
				nameEv[tn] = &Evidence{Kind: "name", Test: tn, File: name, Line: fset.Position(fd.Pos()).Line}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				key := ""
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					key = f.Sel.Name
				case *ast.Ident:
					key = f.Name
				}
				if key != "" {
					if _, exists := callEv[key]; !exists {
						callEv[key] = &Evidence{Kind: "call", Test: tn, File: name, Line: fset.Position(call.Pos()).Line}
					}
				}
				return true
			})
		}
	}
	return nameEv, callEv, warns
}
