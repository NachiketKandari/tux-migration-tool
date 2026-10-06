// Package testgen is the gentest generation engine (PRD-2026-09-09 GT-3/GT-4):
// one function = one unit = one table-driven test block. Units are
// independent, so they render on a bounded worker pool (`concurrency.workers`)
// and merge in input order — workers=1 output is byte-identical. The
// deterministic templates shape db and handler blocks and passthrough
// controllers; anything else (field-mapping controllers) is the LLM gap,
// filled inside the unit worker with parse-gated bounded retries.
package testgen

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/goast"
	"tux-migration-tool/internal/gomockgen"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/templates"
	"tux-migration-tool/internal/testscan"
	"tux-migration-tool/internal/validate"
)

// Options carries one gentest generation run's wiring.
type Options struct {
	BaseDir    string // output root (staged-first: paths.staged or -base)
	Workers    int    // per-function pool bound (concurrency.workers)
	NoLLM      bool
	MaxRetries int
	Audit      *audit.Recorder
	// LLM wiring for the gap-fill seam; zero values degrade to llm-required.
	Client llm.Client
	Budget budget.Budget
	// Templates is the template provider (user overlay over the embedded
	// set); nil = the embedded defaults.
	Templates templates.Provider
	// Log is the parsed runtime log (GT-7); nil keeps the assumed-placeholder
	// behavior. When set, fixture values come from it and every generated
	// method is tagged with its provenance.
	Log *LogData
	// NiceNames enables the optional LLM polish for names only (default off;
	// never affects assertions or structure).
	NiceNames bool
	// Stage copies the scanned non-test sources (and go.mod) into BaseDir as
	// a complete snapshot — the target tree is never modified by staging.
	Stage bool
	// FullTest runs `go test -count=1` per written package inside BaseDir
	// after the compile gate (non-fatal; flips Result.TestsFailed).
	FullTest bool
}

// provider resolves the run's template set (nil-safe embedded default).
func (o Options) provider() templates.Provider {
	if o.Templates == nil {
		return templates.NewEmbeddedProvider()
	}
	return o.Templates
}

// Per-function outcome statuses (exported for the command summary).
const (
	StatusTemplate    = "generated"
	StatusLLM         = "generated-llm"
	StatusDesign      = "skipped-by-design"
	StatusLLMNeeded   = "llm-required"
	StatusUnsupported = "unsupported"
)

// UnitResult is one function's outcome.
type UnitResult struct {
	Service string `json:"service"`
	Layer   string `json:"layer"`
	Func    string `json:"func"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// FixtureEntry is one generated method's fixture provenance (GT-7 gap
// report): "log <short-id>" when a complete trace supplied its values,
// "assumed" when the log had no hit.
type FixtureEntry struct {
	Service string `json:"service"`
	Layer   string `json:"layer"`
	Func    string `json:"func"`
	Source  string `json:"source"`
}

// Result is one run's outcome.
type Result struct {
	Files     []string       `json:"files"`
	Staged    []string       `json:"staged,omitempty"`
	Units     []UnitResult   `json:"units"`
	Fixtures  []FixtureEntry `json:"fixtures,omitempty"`
	LLMCalls  int            `json:"llm_calls"`
	Gates     []string       `json:"gates,omitempty"`
	Checklist []string       `json:"checklist,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	// TestsFailed is set when the -out full-test gate observed a failure;
	// the CLI flips its summary line on it (non-fatal by design).
	TestsFailed bool `json:"tests_failed,omitempty"`
}

// unit is one function's generation work item.
type unit struct {
	sc       *serviceCtx
	layer    testscan.Layer
	dir      string
	outFile  string // base name, e.g. item_test.go
	suite    string // suite struct name for the block
	fn       testscan.Func
	db       *dbFact
	ctrl     *ctrlFact
	handler  *handlerFact
	respType string // controller response type the handler test asserts on
}

// block is one rendered unit outcome.
type block struct {
	unit    *unit
	method  string // the rendered suite-method source
	status  string
	detail  string
	llmCall int
}

type serviceCtx struct {
	name       string
	dir        string
	moduleRoot string
	module     string
	models     *modelsInfo
	ctrlIface  map[string]ctrlIfaceSig
	// dbIface is the store interface's declared per-method shape. Controller
	// EXPECTs need it: a no-arg method takes no matcher, and GetDB() must hand
	// back a live *sqlx.DB rather than a fixture value.
	dbIface      map[string]dbIfaceSig
	fixtures     FixtureSource
	dbFacts      *layerFacts
	ctrlFacts    *layerFacts
	handlerFacts *layerFacts
	tpl          templates.Provider
}

// provider resolves the service's template set (nil-safe embedded default).
func (sc *serviceCtx) provider() templates.Provider {
	if sc.tpl == nil {
		return templates.NewEmbeddedProvider()
	}
	return sc.tpl
}

// Generate runs the per-function pipeline over the scanned target: build
// units from the scan gaps, render on the worker pool, assemble per-layer
// test files, parse-gate, write under BaseDir, regenerate mocks.
func Generate(ctx context.Context, tgt *testscan.Target, rep *testscan.Report, opts Options) (*Result, error) {
	log := telemetry.Log(ctx)
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	res := &Result{}
	if opts.Log != nil {
		res.Warnings = append(res.Warnings, opts.Log.Warnings...)
	}

	svcs := buildServiceCtxs(rep, opts.provider(), opts)
	if len(svcs) == 0 {
		return res, nil
	}
	if opts.Log != nil {
		for _, sc := range svcs {
			sc.logOnlyWarnings(res, opts.Log)
		}
	}

	var units []*unit
	for _, sc := range svcs {
		for _, sr := range rep.Services {
			if sr.Dir != sc.dir {
				continue
			}
			for _, lr := range sr.Layers {
				lf := sc.factsFor(lr.Layer)
				outFile, suite := outNames(sc, lr.Layer, lr.Dir)
				for i := range lr.Funcs {
					if u := buildUnit(sc, lr.Layer, lr.Dir, outFile, suite, lr.Funcs[i], lf, res, opts); u != nil {
						units = append(units, u)
					}
				}
			}
		}
	}

	// Per-function worker pool: units are independent; results merge in
	// input order so workers=1 and workers=N produce identical bytes.
	blocks := make([]*block, len(units))
	common.RunIndexed(len(units), opts.Workers, func(i int) {
		blocks[i] = renderUnit(ctx, units[i], opts)
	})

	for _, b := range blocks {
		if b == nil {
			continue
		}
		res.Units = append(res.Units, UnitResult{
			Service: b.unit.sc.name, Layer: string(b.unit.layer), Func: b.unit.fn.Name,
			Status: b.status, Detail: b.detail,
		})
		res.LLMCalls += b.llmCall
	}

	for _, sc := range svcs {
		for _, layer := range []testscan.Layer{testscan.LayerDB, testscan.LayerController, testscan.LayerHandler} {
			var lb []*block
			for _, b := range blocks {
				if b == nil || b.unit.sc != sc || b.unit.layer != layer {
					continue
				}
				if b.status == StatusTemplate || b.status == StatusLLM {
					lb = append(lb, b)
				}
			}
			if len(lb) == 0 {
				continue
			}
			methods := make([]string, 0, len(lb))
			tags := make([]string, 0, len(lb))
			for _, b := range lb {
				methods = append(methods, b.method)
				if opts.Log != nil {
					tags = append(tags, sc.fixtureTag(layer, b.unit.fn.Name))
				}
			}
			content, err := composeFile(sc, layer, lb[0].unit.outFile, lb[0].unit.suite, methods, provenanceLine(tags))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s/%s: compose failed: %v", sc.name, layer, err))
				continue
			}
			outPath, err := outPathFor(opts.BaseDir, sc, lb[0].unit.dir, lb[0].unit.outFile)
			if err != nil {
				res.Warnings = append(res.Warnings, err.Error())
				continue
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("mkdir %s: %v", filepath.Dir(outPath), err))
				continue
			}
			if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("write %s: %v", outPath, err))
				continue
			}
			res.Files = append(res.Files, outPath)
			res.Checklist = append(res.Checklist, checklistFile(layer, content)...)
			log.Info("gentest file written", "path", outPath, "methods", len(methods), "suite", lb[0].unit.suite)
		}
	}

	stageSources(res, opts, svcs)
	runServiceMocks(ctx, svcs, res, opts)
	compileGate(res, opts)
	fullTestGate(res, opts)
	archiveSummary(opts.Audit, res)
	return res, nil
}

// provenanceLine renders the per-file fixture provenance comment, e.g.
// "// fixture: log ebb010eb | assumed" (deduplicated, method order).
func provenanceLine(tags []string) string {
	seen := map[string]bool{}
	var parts []string
	for _, t := range tags {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return ""
	}
	return "// fixture: " + strings.Join(parts, " | ")
}

// compileGate is the degrade-safe compile pass (PRD-2026-09-09 GT-D6): for
// every written package inside a module, `go vet` then a test-binary
// compile (`go test -run '^$'` — builds the _test.go files without running
// anything) run with a timeout; outcomes are recorded gate lines — never
// run failures (staged trees without the target module's deps degrade
// visibly here).
//
// Under -out the module root is a *staged* copy, so it arrives carrying the
// target module's go.mod but none of its dependency graph: every command
// below would then report "missing go.sum entry" and no code error could
// ever surface. resolveDeps runs once per root first to close that gap, as
// its own gate line so a dependency failure stays distinguishable from a
// code failure. It writes go.mod/go.sum, which is why it is staged-only —
// an in-place run must never touch the user's module files
// (docs/RULES.md §6 never-write-target).
func compileGate(res *Result, opts Options) {
	pkgs := map[string]string{} // package dir → module root
	// Collapse the file list to distinct directories, in first-appearance
	// order. moduleRootOf walks to the filesystem root stat-ing go.mod at
	// each level and hasGoFiles ReadDirs the package, and both were being
	// repeated for every file in a directory rather than once per
	// directory. Order is preserved so the gate lines below keep their
	// existing order.
	var dirs []string
	seenDir := map[string]bool{}
	for _, f := range append(append([]string{}, res.Files...), res.Staged...) {
		d := filepath.Dir(f)
		if !seenDir[d] {
			seenDir[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, dir := range dirs {
		root, ok := moduleRootOf(dir)
		if !ok {
			// Outside any module the vet/compile gates cannot run —
			// record the degrade visibly instead of skipping silently.
			// Each dir now appears once, so no dedupe is needed here.
			res.Gates = append(res.Gates, "gate: skipped ("+dir+" is outside any Go module)")
			continue
		}
		if hasGoFiles(dir) {
			pkgs[dir] = root
		}
	}
	// One resolution per module root, before that root's first code gate,
	// so the vet/test lines below report on code rather than on plumbing.
	resolved := map[string]bool{}
	for dir, root := range pkgs {
		if !resolved[root] {
			resolved[root] = true
			if opts.Stage {
				res.Gates = append(res.Gates, resolveDeps(root)...)
			}
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		if rel == "." {
			pkg = "."
		}
		res.Gates = append(res.Gates, gateOne(root, "go", "vet", pkg)...)
		res.Gates = append(res.Gates, gateOne(root, "go", "test", "-count=1", "-run", "^$", pkg)...)
	}
}

// resolveDeps materializes a staged module's dependency graph so the code
// gates that follow can see real compile errors: `go mod tidy` first (it
// both resolves requirements and writes go.sum from the copied go.mod),
// falling back to `GOFLAGS=-mod=mod go mod download` when tidy cannot run —
// offline, or a go.mod whose requirements cannot be re-resolved.
//
// Best-effort by design: a failure is reported, never swallowed, and the
// caller still runs the code gates — a broken dependency graph must not
// disable the gate that would have explained it.
func resolveDeps(root string) []string {
	detail, failed := gateCmd(root, "go", "mod", "tidy")
	if !failed {
		return []string{"go mod tidy [" + root + "]: clean"}
	}
	if _, dlFailed := gateCmdEnv(root, []string{"GOFLAGS=-mod=mod"}, "go", "mod", "download"); !dlFailed {
		return []string{"go mod download [-mod=mod] [" + root + "]: clean"}
	}
	// Report the tidy diagnostic: producing go.sum is tidy's contract, so
	// its failure is the one that explains the missing entries.
	return []string{"go mod tidy [" + root + "]: FAILED — " + detail}
}

// fullTestGate is the -out full run (GT-7): `go test -count=1` per written
// package inside the output tree. Non-fatal, but a failure flips the visible
// Result.TestsFailed summary flag (the CLI prints "gentest: tests FAILED").
func fullTestGate(res *Result, opts Options) {
	// Both lists, not just res.Files. A staged run writes nothing to Files —
	// it populates Staged — and the CLI only ever sets FullTest together with
	// Stage (`Stage: explicit, FullTest: explicit`), so reading Files alone
	// meant this gate returned immediately in every real invocation. It was
	// dead: the one gate that actually RUNS the generated tests never ran.
	// compileGate a few lines up already merges both; this now matches it.
	files := append(append([]string{}, res.Files...), res.Staged...)
	if !opts.FullTest || len(files) == 0 {
		return
	}
	dirs := map[string]bool{}
	for _, f := range files {
		if dir := filepath.Dir(f); hasGoFiles(dir) {
			dirs[dir] = true
		}
	}
	for dir := range dirs {
		root, ok := moduleRootOf(dir)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		if rel == "." {
			pkg = "."
		}
		detail, failed := gateCmd(root, "go", "test", "-count=1", pkg)
		label := "go test -count=1 " + pkg + " [" + dir + "]"
		if failed {
			res.TestsFailed = true
			res.Gates = append(res.Gates, label+": FAILED — "+detail)
			continue
		}
		res.Gates = append(res.Gates, label+": PASS")
	}
}

// gateTimeout bounds every gate command. One bound for the whole gate
// (vet, compile, dependency resolution) so a wedged toolchain degrades at a
// predictable point instead of hanging the run.
const gateTimeout = 90 * time.Second

// gateOne runs one gate command (bounded) and renders its outcome line.
func gateOne(dir, name string, args ...string) []string {
	detail, failed := gateCmd(dir, name, args...)
	label := name + " " + strings.Join(args, " ") + " [" + dir + "]"
	if failed {
		return []string{label + ": FAILED — " + detail}
	}
	return []string{label + ": clean"}
}

// gateCmd runs one bounded gate command; the returned detail is the clipped
// combined output on failure ("" on success).
func gateCmd(dir string, name string, args ...string) (string, bool) {
	return gateCmdEnv(dir, nil, name, args...)
}

// gateCmdEnv is gateCmd with extra environment entries layered over the
// inherited environment (e.g. GOFLAGS=-mod=mod), used where a command's
// behaviour depends on a mode flag rather than an argument.
func gateCmdEnv(dir string, env []string, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return "", false
	}
	detail := strings.TrimSpace(string(out))
	if len(detail) > 400 {
		detail = detail[:400]
	}
	if detail == "" {
		detail = err.Error()
	}
	return detail, true
}

// buildUnit classifies one scanned function into a unit, recording skip
// outcomes on the run result; nil return = no work item.
func buildUnit(sc *serviceCtx, layer testscan.Layer, dir, outFile, suite string, fn testscan.Func, lf *layerFacts, res *Result, opts Options) *unit {
	skip := func(status, detail string) *unit {
		res.Units = append(res.Units, UnitResult{Service: sc.name, Layer: string(layer), Func: fn.Name, Status: status, Detail: detail})
		return nil
	}
	if fn.HasTest {
		return skip("skipped-covered", "existing test detected by scan")
	}
	if !fn.Exported {
		return skip(StatusDesign, "unexported")
	}
	if fn.Ctor {
		return skip(StatusDesign, "constructor (mocks cover construction)")
	}
	// The wiring constructor (ItemController(repo repo.DataObject) — same
	// name as the controller interface) composes the service; it carries no
	// testable branching, mocks cover construction.
	if fn.Recv == "" && fn.Name == ctrlIfaceName(sc) {
		return skip(StatusDesign, "wiring constructor (mocks cover construction)")
	}
	u := &unit{sc: sc, layer: layer, dir: dir, outFile: outFile, suite: suite, fn: fn}
	switch layer {
	case testscan.LayerDB:
		f := lf.DB[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no sqlx query call recognized (want SelectContext/GetContext/ExecContext, plain or tx)")
		}
		if issue := dbFactIssue(sc, f); issue != "" {
			return skip(StatusUnsupported, issue)
		}
		u.db = f
	case testscan.LayerController:
		f := lf.Ctrl[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no controller shape recognized")
		}
		u.ctrl = f
	case testscan.LayerHandler:
		f := lf.Handler[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no handler shape recognized")
		}
		// Response type: the controller body fact is authoritative; the
		// controller interface declaration is the fallback for trees whose
		// controller bodies are the LLM seam (no-llm runs) — handler bodies
		// and shapes are deterministic either way.
		resp := ""
		if cf := sc.ctrlFacts.Ctrl[f.CtrlCall]; cf != nil && cf.ResponseType != "" {
			resp = cf.ResponseType
		} else if sig, ok := sc.ctrlIface[f.CtrlCall]; ok {
			resp = sig.Response
		}
		if resp == "" {
			return skip(StatusUnsupported, "controller signature for "+f.CtrlCall+" not found (response type unknown)")
		}
		u.handler = f
		u.respType = resp
	default:
		return skip(StatusUnsupported, "layer not testable")
	}
	if opts.Log != nil {
		res.Fixtures = append(res.Fixtures, FixtureEntry{
			Service: sc.name, Layer: string(layer), Func: fn.Name,
			Source: sc.fixtureTag(layer, fn.Name),
		})
	}
	return u
}

// dbFactIssue reports why one db fact cannot render a valid block ("" =
// renderable). Reads need their resolved row/scan type and a models struct
// carrying db tags (the mock row and the expected literal both derive from
// them): composing a block with an empty type name would only fail later at
// the parse gate, so the method is skipped with a reason here instead.
func dbFactIssue(sc *serviceCtx, f *dbFact) string {
	switch f.Shape {
	case "multi", "single":
		if f.RowType == "" {
			return "db row type not recognized (want a scan target declared []*models.T / *models.T / models.T)"
		}
		if len(dbCols(sc, f)) == 0 {
			return "db row type " + structBase(f.RowType) + " has no db-tagged fields (cannot synthesize the mock row)"
		}
	case "scalar":
		if !scalarishType(f.Scalar) {
			return "db scalar scan type not recognized (want int64/string/sql.Null* scanned into a var)"
		}
	case "dml":
		// Exec contract needs no scan target; the query literal is optional
		// (regex falls back to a permissive anchor).
	default:
		return "db query shape not recognized"
	}
	return ""
}

// scalarishType reports whether a scan-target type is a scalar database/sql
// can Scan into (or a Null* wrapper) — a slice or struct here means the
// declaration was misread and the block would be invalid.
func scalarishType(t string) bool {
	switch t {
	case "string", "bool",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64",
		"sql.NullString", "sql.NullInt64", "sql.NullInt32", "sql.NullInt16",
		"sql.NullBool", "sql.NullFloat64", "sql.NullTime",
		"time.Time":
		return true
	}
	return false
}

// renderUnit renders one function's test block on a worker: deterministic
// template when the shape allows, else the LLM seam, else llm-required.
func renderUnit(ctx context.Context, u *unit, opts Options) *block {
	b := &block{unit: u}
	template := func(m string) *block {
		b.method = opts.polish(ctx, m)
		b.status = StatusTemplate
		return b
	}
	unsupported := func(err error) *block {
		b.status = StatusUnsupported
		b.detail = err.Error()
		return b
	}
	switch u.layer {
	case testscan.LayerDB:
		m, err := renderDBMethod(u)
		if err != nil {
			return unsupported(err)
		}
		return template(m)
	case testscan.LayerHandler:
		m, err := renderHandlerMethod(u)
		if err != nil {
			return unsupported(err)
		}
		return template(m)
	case testscan.LayerController:
		if u.ctrl.Passthrough && len(u.ctrl.StoreCalls) == 1 {
			m, err := renderCtrlMethod(u)
			if err != nil {
				return unsupported(err)
			}
			return template(m)
		}
		// GT-7: a complete log trace for the method makes the field-mapping
		// shape deterministic — request values, store mock returns and the
		// expected response all come from the log, so no LLM is needed.
		if _, mv := u.sc.fixtureFor(u.layer, u.fn.Name); mv != nil && mv.SuccessTrace() != nil {
			m, err := renderCtrlMethod(u)
			if err != nil {
				return unsupported(err)
			}
			return template(m)
		}
		if opts.NoLLM || opts.Client == nil {
			b.status = StatusLLMNeeded
			b.detail = "field-mapping controller body needs the LLM seam (-no-llm skips it; -log-file supplies deterministic values)"
			return b
		}
		m, calls, err := fillCtrlMethod(ctx, u, opts)
		b.llmCall = calls
		if err != nil {
			b.status = StatusLLMNeeded
			b.detail = fmt.Sprintf("llm fill failed: %v", err)
			return b
		}
		b.method, b.status = m, StatusLLM
		return b
	}
	b.status = StatusUnsupported
	b.detail = "unknown layer"
	return b
}

// outNames picks the output test file and suite name for a layer: the
// reference shape (<service>.go → <service>_test.go, <Svc><Noun>Suite —
// the controller layer's reference appends the layer noun again,
// <Svc>ControllerSuiteController) when the layer has no test files; a
// gentest-suffixed twin when it does, so existing suites are never touched.
func outNames(sc *serviceCtx, layer testscan.Layer, dir string) (file, suite string) {
	noun := map[testscan.Layer]string{
		testscan.LayerDB:         "Store",
		testscan.LayerController: "Controller",
		testscan.LayerHandler:    "Handler",
	}[layer]
	tail := "Suite"
	if layer == testscan.LayerController {
		tail = "SuiteController"
	}
	base := strings.ToUpper(sc.name[:1]) + sc.name[1:]
	if len(testFiles(dir)) > 0 {
		if layer == testscan.LayerController {
			return sc.name + "_gentest_test.go", base + noun + tail + "Gen"
		}
		return sc.name + "_gentest_test.go", base + noun + "GenSuite"
	}
	stem := sc.name
	if src := sourceFiles(dir); len(src) > 0 {
		for _, s := range src {
			if s != "interface.go" {
				stem = strings.TrimSuffix(s, ".go")
				break
			}
		}
		if stem == sc.name && strings.HasSuffix(src[0], ".go") && src[0] != "interface.go" {
			stem = strings.TrimSuffix(src[0], ".go")
		}
	}
	return stem + "_test.go", base + noun + tail
}

// outPathFor computes the output path for one layer test file. An empty
// baseDir means in-place: the file lands in the same folder as the converted
// code that was scanned (dir itself). Otherwise the path mirrors the
// module-root-relative layout under baseDir (staged-first): module trees
// stay relocatable while the layer structure is preserved. Both operands are
// made absolute first — sc.moduleRoot is absolute (validate.ResolveModuleRoot)
// while dir may be relative (A3.3: keep Rel well-formed either way).
func outPathFor(baseDir string, sc *serviceCtx, dir, outFile string) (string, error) {
	if baseDir == "" {
		return filepath.Join(dir, outFile), nil
	}
	absDir := dir
	if abs, aerr := filepath.Abs(dir); aerr == nil {
		absDir = abs
	}
	rel, err := filepath.Rel(sc.moduleRoot, absDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = ""
	}
	return filepath.Join(baseDir, rel, outFile), nil
}

// composeFile renders the full test file: scaffold + suite + method blocks,
// parse-gated and gofmt-canonicalized. provLine is the optional per-file
// fixture-provenance comment (GT-7).
func composeFile(sc *serviceCtx, layer testscan.Layer, outFile, suite string, methods []string, provLine string) (string, error) {
	prov := sc.provider()
	mockCmd, covCmd := headerCmds(sc, layer)
	var out string
	var err error
	switch layer {
	case testscan.LayerDB:
		joined := strings.Join(methods, "\n")
		out, err = prov.Render(templates.TestDBFile, templates.TestDBFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "db",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			UtilsPkg:       sc.module + "/pkg/utils",
			SuiteName:      suite,
			StoreVar:       strings.ToLower(sc.name) + "Store",
			IfaceName:      dbIfaceName(sc),
			CtorCall:       dbCtorCall(sc),
			NeedsSQL:       strings.Contains(joined, "sql.Null") || strings.Contains(joined, "database/sql"),
			NeedsModels:    strings.Contains(joined, "models."),
			NeedsRegexp:    strings.Contains(joined, "regexp.QuoteMeta"),
			Methods:        methods,
		})
	case testscan.LayerController:
		joined := strings.Join(methods, "\n")
		out, err = prov.Render(templates.TestControllerFile, templates.TestControllerFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "controller",
			DBPkg:          sc.module + "/pkg/services/" + sc.name + "/db",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			// The SAME decision mockgen is given below, so the test's import and
			// the generated mock cannot land on different gomock majors.
			GomockPkg: treeGomock(sc),
			SuiteName: suite, StoreVar: strings.ToLower(sc.name) + "Store",
			CtrlVar:   strings.ToLower(sc.name) + "Controller",
			CtrlIface: ctrlIfaceName(sc),
			MockType:  "db.Mock" + dbIfaceName(sc),
			MockCtor:  "db.NewMock" + dbIfaceName(sc) + "(suite.mockController)",
			CtorCall:  ctrlCtorCall(sc),
			NeedsSQL:  strings.Contains(joined, "sql.Null"),
			NeedsTime: strings.Contains(joined, "time."),
			Methods:   methods,
		})
	case testscan.LayerHandler:
		out, err = prov.Render(templates.TestHandlerFile, templates.TestHandlerFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "handler",
			ControllerPkg:  sc.module + "/pkg/services/" + sc.name + "/controller",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			NetworkPkg:     sc.module + "/pkg/network",
			UtilsPkg:       sc.module + "/pkg/utils",
			// Read, not assumed: the template's SetupSuite only calls
			// RegisterValidations when the target's utils package declares it.
			// The import path is the hardcoded one above, so this looks there.
			HasRegisterValidations: declaresFunc(filepath.Join(sc.moduleRoot, "pkg", "utils"), "RegisterValidations"),
			// The SAME decision mockgen is given in runServiceMocks.
			GomockPkg:    treeGomock(sc),
			SuiteName:    suite,
			CtrlMockVar:  strings.ToLower(sc.name) + "Controller",
			CtrlMockType: "controller.Mock" + ctrlIfaceName(sc),
			MockCtor:     "controller.NewMock" + ctrlIfaceName(sc) + "(gomock.NewController(suite.T()))",
			HandlerVar:   strings.ToLower(sc.name) + "Handler",
			IfaceName:    handlerIfaceName(sc),
			CtorCall:     handlerCtorCall(sc),
			Methods:      methods,
		})
	default:
		return "", fmt.Errorf("unknown layer %s", layer)
	}
	if err != nil {
		return "", err
	}
	if provLine != "" {
		out = provLine + "\n" + out
	}
	formatted, ferr := goast.Emit("gentest: "+outFile, out)
	if ferr != nil {
		return "", ferr
	}
	return formatted, nil
}

// buildServiceCtxs builds per-service extraction contexts from the scan.
// With a parsed log (GT-7) the fixture source is the log backend; without
// one it stays the assumed-placeholder backend.
func buildServiceCtxs(rep *testscan.Report, prov templates.Provider, opts Options) []*serviceCtx {
	seen := map[string]bool{}
	var svcs []*serviceCtx
	for _, sr := range rep.Services {
		if seen[sr.Dir] {
			continue
		}
		seen[sr.Dir] = true
		sc := &serviceCtx{name: sr.Name, dir: sr.Dir, tpl: prov}
		if root, ok := moduleRootOf(sr.Dir); ok {
			sc.moduleRoot = root
		}
		sc.module = moduleName(sc.moduleRoot, sr.Dir)
		sc.models = extractModels(sr.Dir)
		sc.ctrlIface = extractCtrlIface(sr.Dir)
		sc.dbIface = extractDBIface(sr.Dir)
		if opts.Log != nil {
			sc.fixtures = NewLogFixtureSource(opts.Log, sc.models, sr.Name)
		} else {
			sc.fixtures = &AssumedFixtureSource{Models: sc.models}
		}
		sc.dbFacts = extractLayer(filepath.Join(sr.Dir, "db"), "db", sc.dbIface)
		sc.ctrlFacts = extractLayer(filepath.Join(sr.Dir, "controller"), "controller", sc.dbIface)
		sc.handlerFacts = extractLayer(filepath.Join(sr.Dir, "handler"), "handler", sc.dbIface)
		svcs = append(svcs, sc)
	}
	return svcs
}

// logOnlyWarnings records methods the log references but the disk tree does
// not define — the log cannot invent code (plan "Caller ↔ code matching"),
// so these are ignored visibly instead of silently.
func (sc *serviceCtx) logOnlyWarnings(res *Result, data *LogData) {
	seen := map[string]bool{}
	for _, t := range data.Traces {
		for _, c := range t.Callers {
			if !strings.EqualFold(c.Service, sc.name) {
				continue
			}
			key := c.Layer + "." + c.Method
			if seen[key] {
				continue
			}
			seen[key] = true
			known := false
			switch c.Layer {
			case "db":
				known = sc.dbFacts.DB[c.Method] != nil
			case "controller":
				known = sc.ctrlFacts.Ctrl[c.Method] != nil
			case "handler":
				known = sc.handlerFacts.Handler[c.Method] != nil
			}
			if !known {
				res.Warnings = append(res.Warnings, fmt.Sprintf("log-only %s method %s/%s ignored (no code match)", c.Layer, sc.name, c.Method))
			}
		}
	}
}

func (sc *serviceCtx) factsFor(l testscan.Layer) *layerFacts {
	switch l {
	case testscan.LayerDB:
		return sc.dbFacts
	case testscan.LayerController:
		return sc.ctrlFacts
	case testscan.LayerHandler:
		return sc.handlerFacts
	}
	return &layerFacts{DB: map[string]*dbFact{}, Ctrl: map[string]*ctrlFact{}, Handler: map[string]*handlerFact{}}
}

// moduleRootOf walks up from dir for the nearest go.mod via the shared
// validate.ResolveModuleRoot; ok=false when the dir sits outside any module
// (staged trees may sit outside any module). A package AT the module root
// (go.mod in dir itself) resolves fine — only a walk failure skips.
func moduleRootOf(dir string) (root string, ok bool) {
	root, err := validate.ResolveModuleRoot(dir)
	if err != nil {
		return "", false
	}
	return root, true
}

// moduleName resolves the target module: the go.mod module line, else the
// prefix of any source import path above /pkg/. An empty root (outside any
// module) skips the go.mod read — Join("", "go.mod") would otherwise read
// "go.mod" relative to the working directory, a cwd-dependent accident.
func moduleName(root, dir string) string {
	if root != "" {
		if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "module "))
				}
			}
		}
	}
	for _, name := range sourceFiles(dir) {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "\"") && strings.Contains(line, "/pkg/") {
				p := strings.Trim(line, "\"")
				if i := strings.Index(p, "/pkg/"); i > 0 {
					return p[:i]
				}
			}
		}
	}
	return "module"
}

func dbIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "db", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "db")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "db", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Store"
}

func ctrlIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "controller", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "controller")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "controller", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Controller"
}

func handlerIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "handler", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "handler")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "handler", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Handler"
}

// ifaceFromFile returns the first interface type declared in the file.
func ifaceFromFile(path string) string {
	src, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	for _, d := range af.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					return ts.Name.Name
				}
			}
		}
	}
	return ""
}

// dbCtorCall renders the suite's store construction.
//
// A service may hold more than one sqlx.DB — a read handle and a write handle,
// e.g. NewDummyProfileStore(db, writeDb *sqlx.DB) — and the old code emitted a
// single argument, so every read ran against a nil *sqlx.DB and the suite
// panicked before its first assertion.
//
// EVERY sqlx handle parameter gets the live mock connection. That is not a
// refinement of a position heuristic; it is what makes the suite correct. A
// sqlmock suite has exactly one connection, and both handles are the same
// logical database reached through different pools — so pointing them at the
// same mock is the only arrangement under which every method in the store can
// be driven.
//
// The fix plan proposed picking ONE handle by majority vote over the db facts'
// Recv field and nil-ing the rest, on the reasoning that "a suite exercising
// the majority path never touches them". That reasoning is wrong, and the
// grown fixture is what proved it: a generated db suite exercises EVERY method,
// so a store with a write handle has its write method nil-panic. Measured, not
// assumed — AddOrder (on g.writeDb) panicked at demo.go:89 with a nil
// *sqlx.DB while the reads on g.db passed.
//
// A non-sqlx parameter (a config struct, a logger) still gets nil, which
// matches the controller layer's rule for a dependency the generator owns no
// mock for.
func dbCtorCall(sc *serviceCtx) string {
	name := sc.dbFacts.DBCtor
	if name == "" {
		name = "New" + dbIfaceName(sc)
	}
	params := sc.dbFacts.DBCtorParams
	if len(params) <= 1 {
		return name + "(suite.sqlDB)"
	}
	args := make([]string, len(params))
	for i, p := range params {
		if isSQLXHandle(p.Type) {
			args[i] = "suite.sqlDB"
		} else {
			args[i] = ctorZeroValue(p.Type, sc.models)
		}
	}
	return name + "(" + strings.Join(args, ", ") + ")"
}

// isSQLXHandle reports whether a declared parameter type is a sqlx connection
// handle. The spelling varies (`*sqlx.DB`, `sqlx.DB`, and an aliased import),
// so the check is on the type's final segment rather than an exact string.
func isSQLXHandle(typ string) bool {
	t := strings.TrimPrefix(strings.TrimSpace(typ), "*")
	return t == "sqlx.DB" || t == "sqlx.ExtContext" || strings.HasSuffix(t, ".sqlx.DB")
}

// ctrlCtorCall renders the suite's controller construction.
//
// A converted controller frequently takes more than its own store — the corpus
// ctor is NewDummyProfileController(store db.DummyProfileStore, userStore
// commonDB.UserStore) — so hardcoding the single argument produced "not enough
// arguments in call" and no controller suite compiled at all.
//
// One argument is rendered per declared parameter, mirroring dbCtorCall so both
// layers read the same way. The parameter whose type is this service's own
// store interface gets the generated mock; every other dependency gets nil.
//
// nil rather than a discovered mock is a deliberate limit, not an oversight.
// gentest only generates mocks for the service's own two interfaces, so a
// cross-package collaborator (commonDB.UserStore) has no mock to hand over.
// nil compiles, and a method that actually dereferences it fails loudly at the
// call rather than silently asserting against a stub the tool invented.
// Generating those mocks is a feature; discovering cross-package interfaces to
// mock is follow-up work, not a bug fix.
func ctrlCtorCall(sc *serviceCtx) string {
	name := sc.ctrlFacts.CtrlCtor
	if name == "" {
		name = "New" + ctrlIfaceName(sc)
	}
	params := sc.ctrlFacts.CtrlCtorParams
	store := "suite." + strings.ToLower(sc.name) + "Store"
	if len(params) == 0 {
		// No params captured — the pre-F7 shape. The store is the only
		// dependency the generator owns, so it is the only safe guess.
		return name + "(" + store + ")"
	}
	want := ownStoreNames(sc)
	args := make([]string, len(params))
	for i, p := range params {
		if want[ifaceBaseName(p.Type)] {
			args[i] = store
		} else {
			args[i] = ctorZeroValue(p.Type, sc.models)
		}
	}
	return name + "(" + strings.Join(args, ", ") + ")"
}

// ctorZeroValue returns the source literal to pass for a constructor parameter
// the generator has no value for.
//
// It used to be a bare `nil` for every parameter that was neither a sqlx handle
// nor one of this service's own store interfaces. That is right for an interface
// or a pointer, and every corpus and fixture constructor takes exactly those
// (*sqlx.DB, db.DummyProfileStore, commonDB.UserStore) — which is why the bug
// never showed up in a measurement.
//
// For a struct it is the worst failure shape available. `nil` compiles, so
// every vet and compile gate passes, and the suite nil-panics the first time the
// constructor touches a field. For an array or a basic type it does not even
// compile. Both are worse than passing the type's zero value, which is what this
// returns.
//
// The zero value is used rather than a synthesised plausible one on purpose: a
// zero struct has no field that can be dereferenced, so the failure mode becomes
// "the test asserts something empty" instead of a panic.
func ctorZeroValue(typ string, mi *modelsInfo) string {
	t := strings.TrimSpace(typ)
	switch {
	case t == "":
		return "nil" // no declared type — the pre-F7 guess, unchanged
	case strings.HasPrefix(t, "*"),
		strings.HasPrefix(t, "[]"),
		strings.HasPrefix(t, "map["),
		strings.HasPrefix(t, "chan "),
		t == "chan",
		strings.HasPrefix(t, "func("),
		strings.HasPrefix(t, "interface{"),
		t == "interface{}",
		t == "any",
		t == "error":
		return "nil"
	}
	switch t {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "byte", "rune", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128":
		return "0"
	}
	// An array is a composite literal, never nil.
	if strings.HasPrefix(t, "[") {
		return t + "{}"
	}
	// A named type is the hard case: `models.Store` is an interface and
	// `models.Request` is a struct, and the written type says nothing about
	// which. The models table is the only thing that knows, so ask it. An
	// unknown name falls through to nil, which is the status quo and compiles
	// for the interface case that dominates every constructor seen so far.
	if mi != nil {
		if _, isStruct := mi.Structs[unqualify(t)]; isStruct {
			return t + "{}"
		}
	}
	return "nil"
}

// ownStoreNames is the set of names this service's store interface may be
// spelled with in the controller constructor's signature.
//
// Both sources are consulted because each fails in a different situation.
// dbIfaceName reads the db layer's interface declaration and is the
// authoritative answer, but it synthesizes a fallback name when the
// declaration cannot be read. The db constructor's own name carries the same
// information independently: NewDummyProfileStore implies DummyProfileStore, and
// the converted code always derives one from the other.
//
// Matching only one of them is how a correct signature still rendered as nil:
// with the declaration unread, the synthesized name did not match the real one
// and the suite got a nil store — a failure at run time, far from the cause.
func ownStoreNames(sc *serviceCtx) map[string]bool {
	out := map[string]bool{ifaceBaseName(dbIfaceName(sc)): true}
	if sc.dbFacts != nil {
		if ctor := sc.dbFacts.DBCtor; ctor != "" && strings.HasPrefix(ctor, "New") && len(ctor) > 3 {
			out[ctor[3:]] = true
		}
		if len(sc.dbFacts.DBCtorParams) > 0 {
			// A *sqlx.DB parameter is a handle, not the store interface, so
			// it must not be mistaken for one.
			delete(out, "DB")
		}
	}
	return out
}

// ifaceBaseName strips the package qualifier from a possibly-qualified type
// name, so `db.DummyProfileStore` and `DummyProfileStore` compare equal. A
// constructor may declare its own store either way depending on whether the
// layer files import their sibling package, and matching on the qualified form
// alone would silently fall through to nil for the one argument that matters.
func ifaceBaseName(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 {
		return t[i+1:]
	}
	return t
}

func handlerCtorCall(sc *serviceCtx) string {
	name := sc.handlerFacts.HandlerCtor
	if name == "" {
		name = "New" + handlerIfaceName(sc)
	}
	return name + "(suite." + strings.ToLower(sc.name) + "Controller)"
}

// treeGomock reports which gomock major the scanned module vendors, so the
// generated mock lands in the same major the templates' test files import.
// A converted service inherits its host's dependencies and the tool must not
// override them: one package cannot hold both majors, since
// go.uber.org/mock/gomock and github.com/golang/mock/gomock declare distinct
// Controller types and a mock from one satisfies neither in the other.
//
// The module's go.mod is the authority; a gomock import already present in
// the tree's test files is the corroborating signal. "" = no evidence, which
// keeps the existing uber-go default rather than guessing a new import.
func treeGomock(sc *serviceCtx) string {
	legacy := gomockgen.LegacyGomockPath
	for _, root := range []string{sc.moduleRoot, filepath.Dir(sc.dir)} {
		if root == "" {
			continue
		}
		if declaresRequire(filepath.Join(root, "go.mod"), legacy) {
			return legacy
		}
		if declaresRequire(filepath.Join(root, "go.mod"), "go.uber.org/mock") {
			return ""
		}
	}
	if importsGomock(filepath.Join(sc.dir, "db"), legacy) {
		return legacy
	}
	return ""
}

// declaresRequire reports whether a go.mod requires the given module path.
// It matches the require block textually rather than parsing it: the go.mod
// grammar the tool needs is one substring, and a parse failure on a
// partially-written or toolchain-extended go.mod must not read as "absent".
// Both spellings count — a bare `path v1.2.3` line inside require ( ... ),
// and the single-line `require path v1.2.3` form.
func declaresRequire(goMod, path string) bool {
	b, err := os.ReadFile(goMod)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		if strings.HasPrefix(line, path+" ") || line == path || strings.HasPrefix(line, path+"/v") {
			return true
		}
	}
	return false
}

// importsGomock reports whether any Go file under dir imports the gomock
// module path.
func importsGomock(dir, path string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	fset := token.NewFileSet()
	needle := strconv.Quote(path + "/gomock")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		af, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, imp := range af.Imports {
			if imp.Path != nil && imp.Path.Value == needle {
				return true
			}
		}
	}
	return false
}

// runServiceMocks regenerates db/controller mocks for every touched service
// (best-effort, shared runner; the target tree's interfaces only). The
// target derivation is the shared gomockgen.MockTargetsFor table (A4.3).
//
// Two tree facts decide whether a mock is written at all, because writing one
// is a mutation of the user's own module and never a safe default:
//
//   - The package may already declare Mock<Iface>. A converted service that
//     shipped its own doubles (the dummyprofile corpus hand-writes
//     db/interface_mock.go) would get a second declaration of the same type
//     and stop compiling — "MockDummyProfileStore redeclared in this block".
//     The existing mock already satisfies what the generated tests import, so
//     skipping is both correct and cheaper. Note this is not a *filename*
//     collision: mock_store.go and interface_mock.go are different files in
//     one package, which is why stage.go's collisionName rename cannot help.
//
//   - In-place runs must not write into the scanned tree at all
//     (docs/RULES.md §6 never-write-target). Without -out there is nowhere
//     else to put a mock, so the run reports the mockgen command it would
//     have run and leaves the tree byte-identical apart from the _test.go
//     files it was asked for.
func runServiceMocks(ctx context.Context, svcs []*serviceCtx, res *Result, opts Options) {
	var targets []gomockgen.MockTarget
	for _, sc := range svcs {
		base := dbIfaceName(sc)
		base = strings.TrimSuffix(base, "Store")
		gomockMajor := treeGomock(sc)
		for _, t := range gomockgen.MockTargetsFor(sc.dir, base) {
			t.Gomock = gomockMajor
			if _, err := os.Stat(t.Source); err != nil {
				continue
			}
			if mockAlreadyDeclared(t.Dest, t.Name) {
				continue
			}
			if !opts.Stage {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s: no %s in %s — run the MockGen command in the generated file's header, or re-run with -out to stage one",
					filepath.Base(filepath.Dir(t.Dest)), "Mock"+t.Name, filepath.Base(filepath.Dir(t.Dest))))
				continue
			}
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return
	}
	gomockgen.RunMocks(ctx, targets)
}

// mockAlreadyDeclared reports whether dir's package already declares a type
// named `Mock`+iface (mockgen's own naming). It parses rather than
// substring-matches: a comment or an unrelated identifier mentioning the
// mock must not suppress generation, and the same package can carry a
// hand-written double under a filename gentest would not guess.
func mockAlreadyDeclared(dest, iface string) bool {
	dir := filepath.Dir(dest)
	want := "Mock" + iface
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		af, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == want {
					return true
				}
			}
		}
	}
	return false
}

// archiveSummary persists the run summary into the audit trail (best-effort).
func archiveSummary(rec *audit.Recorder, res *Result) {
	_, _ = rec.WriteJSON("gentest_summary.json", res) // nil-tolerant
}

// headerCmds renders the mockgen + coverage header comments for a layer.
func headerCmds(sc *serviceCtx, layer testscan.Layer) (mockCmd, covCmd string) {
	rel := "pkg/services/" + sc.name + "/" + string(layer)
	test := sc.name + "_test.go"
	switch layer {
	case testscan.LayerDB:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/mock_store.go -package=%s", rel, rel, layer)
	case testscan.LayerController:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/mock_controller.go -package=%s", rel, rel, layer)
	case testscan.LayerHandler:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/interface_mock.go -package=%s", rel, rel, layer)
	}
	covCmd = fmt.Sprintf("go test %s/%s %s/%s.go %s/interface.go -v -coverprofile=coverage.txt -covermode count && go tool cover -html=coverage.txt", rel, test, rel, sc.name, rel)
	return mockCmd, covCmd
}
