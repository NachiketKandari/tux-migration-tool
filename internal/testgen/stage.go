// GT-7 -out staging: with an explicit output root, gentest stages the
// scanned service's non-test Go sources (and the module's go.mod) into the
// out tree so the directory becomes a complete, compatible snapshot. The
// target tree is never modified by staging, never overwritten either: a
// destination name that already exists gets the `_convertgo` suffix (the
// plan's collision rule, e.g. interface.go → interface_convertgo.go).
package testgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/testscan"
)

// stageSources copies the scanned services' non-test sources into the run's
// BaseDir. It is a no-op unless Options.Stage is set (explicit -out/-base)
// and the output root differs from the module root (an in-place run has
// nothing to snapshot).
func stageSources(res *Result, opts Options, svcs []*serviceCtx) {
	if !opts.Stage || opts.BaseDir == "" || len(svcs) == 0 {
		return
	}
	absBase, err := filepath.Abs(opts.BaseDir)
	if err != nil {
		absBase = opts.BaseDir
	}
	for _, sc := range svcs {
		if sc.moduleRoot != "" {
			absRoot, rerr := filepath.Abs(sc.moduleRoot)
			if rerr == nil && absRoot == absBase {
				continue // same tree — never copy onto itself
			}
		}
		for _, sub := range []string{"db", "controller", "handler", "models", "model"} {
			dir := filepath.Join(sc.dir, sub)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
					continue
				}
				stageOne(res, opts.BaseDir, sc, dir, name)
			}
		}
		stageSupport(res, opts.BaseDir, sc)
		if sc.moduleRoot != "" {
			stageOne(res, opts.BaseDir, sc, sc.moduleRoot, "go.mod")
		}
	}
}

// stageSupport copies the module's host packages into the out tree, keeping
// their module-relative layout.
//
// Staging used to copy only the scanned service's own db/controller/handler/
// models directories. A generated suite also imports the module's support
// packages — the handler test imports the network package for HttpResponse,
// and a controller suite that opens a transaction imports utils for the
// sqlmock handle — so without them the staged tree does not compile:
//
//	package gentestgate/pkg/network is not in std
//
// That is a missing file, not a bad import: the target tree is never modified,
// so the out tree has to carry what the generated code references.
//
// Everything outside the service dir is copied verbatim, with no collision
// renaming: these files are being restored at their own paths, not merged into
// the service's packages.
func stageSupport(res *Result, baseDir string, sc *serviceCtx) {
	if sc.moduleRoot == "" {
		return
	}
	absSvc, err := filepath.Abs(sc.dir)
	if err != nil {
		absSvc = sc.dir
	}
	absRoot, err := filepath.Abs(sc.moduleRoot)
	if err != nil || absRoot == absSvc {
		return
	}
	filepath.Walk(sc.moduleRoot, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // a stat failure just skips the entry
		}
		if filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		// The service's own packages were staged above, into their renamed
		// destinations; copying them again would duplicate every symbol.
		absP, aerr := filepath.Abs(p)
		if aerr == nil && strings.HasPrefix(absP, absSvc+string(filepath.Separator)) {
			return nil
		}
		rel, rerr := filepath.Rel(sc.moduleRoot, p)
		if rerr != nil {
			return nil //nolint:nilerr // outside the module: not ours to stage
		}
		dst := filepath.Join(baseDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil //nolint:nilerr // best effort, as stageOne is
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil //nolint:nilerr // best effort, as stageOne is
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return nil //nolint:nilerr // best effort, as stageOne is
		}
		res.Staged = append(res.Staged, dst)
		return nil
	})
}

// stageOne writes one source file into the out tree, renaming on collision.
func stageOne(res *Result, baseDir string, sc *serviceCtx, dir, name string) {
	dst, err := outPathFor(baseDir, sc, dir, name)
	if err != nil {
		return
	}
	dst = collisionName(dst)
	src := filepath.Join(dir, name)
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return
	}
	res.Staged = append(res.Staged, dst)
}

// hasGoFiles reports whether dir holds at least one .go file (a staged
// go.mod sits at the module root, where vet/compile gates must not run).
func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".go" {
			return true
		}
	}
	return false
}

// collisionName returns the plan's collision-safe destination: the natural
// name when free, else `<stem>_convertgo<ext>` (numbered deterministically
// when repeated).
func collisionName(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	cand := stem + "_convertgo" + ext
	for n := 2; ; n++ {
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
		cand = fmt.Sprintf("%s_convertgo%d%s", stem, n, ext)
	}
}

// checklistFile is the structural checklist against the human reference
// suites (dummyPipelineTest/rpdbtest.txt, rptestcontroller.txt,
// handler_test.txt): the template-shape markers a generated file must carry
// for its layer. Findings are advisory only — the plan asks for guidelines,
// not byte comparison — and never fail a run.
func checklistFile(layer testscan.Layer, content string) []string {
	var markers []string
	switch layer {
	case testscan.LayerDB:
		markers = []string{"testCases :=", "for _, testCase := range", "assert.ErrorContains(", "assert.NoError("}
		if !strings.Contains(content, "ExpectQuery(") && !strings.Contains(content, "ExpectExec(") {
			return []string{"checklist db: missing ExpectQuery/ExpectExec mock expectation"}
		}
	case testscan.LayerController:
		markers = []string{"testCases :=", "EXPECT()", "gomock.Any()", "assert.ErrorContains(", "assert.NoError("}
	case testscan.LayerHandler:
		markers = []string{"CreateTestGinContext", "assert.Equal("}
	}
	var missing []string
	for _, m := range markers {
		if !strings.Contains(content, m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("checklist %s: missing %s", layer, strings.Join(missing, ", "))}
}
