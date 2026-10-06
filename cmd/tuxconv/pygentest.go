package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tux-migration-tool/internal/batchflow"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pygentest"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/templates"
	scanner "tux-migration-tool/internal/tsscan"
)

// runPygentest implements `tuxconv pygentest <file|dir>` (docs/pygentest-plan.md
// §4 Phase 2c): generate the assertion suite for every batch module that
// convertbatchpy already wrote, then RUN it.
//
// The oracle judges the artifact on disk, never a freshly generated one: it
// re-derives the plan from the same .pc (that is what says what the module was
// supposed to do) and loads the module the pipeline left in the module
// directory (that is what it did). Regenerating here would audit a file nobody
// is going to ship, and would silently pass a module the pipeline never wrote.
//
// Exit status is the gate: a suite with findings fails the command, because
// "the generated code is right" has to be a measured claim rather than a text
// diff. -no-run writes the suite without executing it — inspection only, and
// the tool says so in its summary.
func runPygentest(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("pygentest", flag.ContinueOnError)
	moduleDir := fs.String("module-dir", "", "Directory holding the generated module and its support files (default: batchpy.outDir from config)")
	outDir := fs.String("out", "", "Where the suite and its two support modules are written (default: the module dir)")
	noRun := fs.Bool("no-run", false, "Write the suite without executing it (inspection only — an unexecuted suite is not a gate)")
	python := fs.String("python", "", "Interpreter to run the suite with (default: python3 on PATH)")
	strict := fs.Bool("strict", false, "Also fail when the suite reports a not-asserted claim (skips), not only on failed checks")
	shape := fs.String("shape", "", "auto|repo — shape rubric override (must match the convertbatchpy run; default: batchpy.shape from config)")
	dmlLoop := fs.String("dml-loop", "", "batch|rowbyrow — cursor-DML semantics (must match the convertbatchpy run; default: batchpy.dmlLoop from config)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	templatesDir := fs.String("templates", "", "Directory of <template_id>.tmpl overrides (flag > templates.dir config; missing ids keep the embedded set)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("pygentest: must provide a target (.pc/.pcf file, or a directory of them; several are fine)")
	}

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	tpl, err := templateProvider(ctx, cfg, *templatesDir)
	if err != nil {
		return err
	}

	b := cfg.Batchpy
	if *shape != "" {
		b.Shape = *shape
	}
	if *dmlLoop != "" {
		b.DMLLoop = *dmlLoop
	}
	modDir := *moduleDir
	if modDir == "" {
		modDir = b.OutDir
	}
	if modDir == "" {
		modDir = config.DefaultBatchpyOut
	}
	suiteDir := *outDir
	if suiteDir == "" {
		suiteDir = modDir
	}
	if err := os.MkdirAll(suiteDir, 0o755); err != nil {
		return fmt.Errorf("pygentest: create output dir %s: %w", suiteDir, err)
	}

	// Several targets are accepted, and each is expanded. batchpy's own
	// resolver keeps only the first positional; doing the same here would
	// silently ignore the rest and report a smaller run than asked for.
	paths, err := pygentestTargets(ctx, positional, cfg)
	if err != nil {
		return err
	}
	log.Info("pygentest invoked", "targets", positional, "files", len(paths),
		"module_dir", modDir, "out", suiteDir, "no_run", *noRun, "strict", *strict)

	green, findings, incomplete, notAudited := 0, 0, 0, 0
	var firstErr error
	for _, path := range paths {
		out, err := pygentestOne(ctx, path, b, modDir, suiteDir, tpl,
			pygentest.Python(*python), *noRun, *strict)
		// A suite that RAN is counted before the error is taken: "0 green,
		// 0 with findings" beside a failing module would be the report
		// miscounting its own result.
		switch {
		case out.Ran && out.Green:
			green++
		case out.Ran && out.Verdict == verdictIncomplete:
			incomplete++
		case out.Ran:
			findings++
		}
		if err != nil {
			// No verdict at all (no module on disk, no interpreter, a
			// template error) is counted apart: it is not a clean run and
			// not a finding, and folding it into either would hide it. A
			// red suite also returns an error, so this is keyed on Ran —
			// otherwise every finding would be counted twice.
			if !out.Ran {
				notAudited++
			}
			log.Error("pygentest target failed", "source", path, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("pygentest: %s: %w", filepath.Base(path), err)
			}
		}
	}
	fmt.Printf("pygentest: %d target(s) under %s — %d green, %d with findings, %d incomplete, %d not audited%s\n",
		len(paths), suiteDir, green, findings, incomplete, notAudited, executedNote(*noRun))
	return firstErr
}

// The per-module verdicts the summary counts.
const (
	verdictGreen      = "GREEN"
	verdictFindings   = "FINDINGS"
	verdictIncomplete = "INCOMPLETE"
)

// pygentestTargets expands every positional target (or batchpy.input when
// none was given) into the .pc/.pcf files to audit, de-duplicated and in the
// order given.
func pygentestTargets(ctx context.Context, positional []string, cfg *config.Config) ([]string, error) {
	targets := positional
	if len(targets) == 0 {
		if cfg.Batchpy.Input == "" {
			return nil, fmt.Errorf("pygentest: no input target: pass a .pc/.pcf file or directory (several are fine), or set batchpy.input in .tuxconv.yaml")
		}
		targets = []string{cfg.Batchpy.Input}
	}
	var paths []string
	seen := map[string]bool{}
	for _, target := range targets {
		found, err := batchTargets(ctx, target, cfg.Batchpy.FileFilter)
		if err != nil {
			return nil, fmt.Errorf("pygentest: %w", err)
		}
		for _, p := range found {
			key := filepath.Clean(p)
			if seen[key] {
				continue
			}
			seen[key] = true
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// executedNote keeps the summary honest about what the command actually did:
// an unexecuted suite is a written file, not a verdict.
func executedNote(noRun bool) string {
	if noRun {
		return " (WRITTEN ONLY — -no-run: nothing was executed, so this run proves nothing)"
	}
	return ""
}

// suiteOutcome is one target's result. Ran distinguishes "the suite executed
// and said something" from "the command could not get that far" (no module on
// disk, no interpreter), which the summary counts differently.
type suiteOutcome struct {
	Module  string
	Suite   string
	Ran     bool
	Green   bool
	Verdict string
}

// pygentestOne generates and runs the suite for one batch file.
func pygentestOne(ctx context.Context, path string, b config.Batchpy, modDir, suiteDir string,
	tpl templates.Provider, python pygentest.Python, noRun, strict bool) (suiteOutcome, error) {
	log := telemetry.Log(ctx)

	plan, err := pygentestPlan(path, b)
	if err != nil {
		return suiteOutcome{}, err
	}
	if plan.Shape != "repo" {
		return suiteOutcome{}, fmt.Errorf("the %s shape has no suite yet — pygentest asserts the repository shape's method contract (cursor-DAL functions take a cursor the plan does not describe)", plan.Shape)
	}

	modulePath := filepath.Join(modDir, plan.Module+".py")
	moduleSrc, err := os.ReadFile(modulePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return suiteOutcome{}, fmt.Errorf("no generated module at %s — run `convertbatchpy -out %s %s` first; pygentest audits the artifact that is on disk and does not regenerate it",
				modulePath, modDir, path)
		}
		return suiteOutcome{}, fmt.Errorf("read generated module %s: %w", modulePath, err)
	}

	harness, err := pygentest.Harness(tpl, templates.NewPyDBRouterData(
		plan.Wrapper.RouterClass, plan.Wrapper.ReadMode, plan.Wrapper.WriteMode))
	if err != nil {
		return suiteOutcome{}, err
	}
	rules, err := pygentest.SQLRules(tpl)
	if err != nil {
		return suiteOutcome{}, err
	}
	proc, err := os.ReadFile(path)
	if err != nil {
		return suiteOutcome{}, fmt.Errorf("read %s: %w", path, err)
	}
	data := pygentest.NewSuiteData(plan, pygentest.Source{
		Module: string(moduleSrc), ProC: string(proc), Path: path,
	})
	content, err := pygentest.Suite(tpl, data)
	if err != nil {
		return suiteOutcome{}, err
	}

	suitePath := filepath.Join(suiteDir, pygentest.SuiteFile(plan.Module))
	for _, f := range []struct{ name, body string }{
		{pygentest.File, harness},
		{pygentest.SQLRulesFile, rules},
		{filepath.Base(suitePath), content},
	} {
		dst := filepath.Join(suiteDir, f.name)
		if err := os.WriteFile(dst, []byte(f.body), 0o644); err != nil {
			return suiteOutcome{}, fmt.Errorf("write %s: %w", dst, err)
		}
	}
	fmt.Printf("pygentest: %s: suite written -> %s\n", plan.Module, suitePath)
	if noRun {
		return suiteOutcome{Module: plan.Module, Suite: suitePath, Green: true}, nil
	}

	res, err := pygentest.Run(python, suitePath)
	if err != nil {
		return suiteOutcome{}, err
	}
	archiveSuite(ctx, plan, res)

	verdict := verdictGreen
	green := res.OK()
	if !green {
		verdict = verdictFindings
	}
	if strict && green && res.Skipped > 0 {
		verdict, green = verdictIncomplete, false
	}
	log.Info("pygentest suite run", "module", plan.Module, "verdict", verdict,
		"checks", res.Checks, "failed", res.Failed, "skipped", res.Skipped,
		"body", data.BodyState)
	fmt.Printf("%s: %s — %s, service body=%s -> %s\n",
		plan.Module, verdict, res.Summary(), data.BodyState, suitePath)
	for _, f := range res.Findings {
		// The suite records what it found and what it could not decide. A
		// rule violation is a finding; a "body is a stub" row is a caveat.
		// Printing both under one heading would train the reader to skip it.
		if isRuleFinding(f.Rule) {
			fmt.Printf("  finding: %s %s — %s\n", f.Rule, f.Where, f.Detail)
			continue
		}
		fmt.Printf("  note: %s %s — %s\n", f.Rule, f.Where, f.Detail)
	}
	if !green {
		return suiteOutcome{Module: plan.Module, Suite: suitePath, Ran: true, Verdict: verdict}, fmt.Errorf(
			"%s: %s — %d finding(s), %d not asserted (the suite output above is the evidence)", plan.Module, verdict, res.Failed, res.Skipped)
	}
	return suiteOutcome{Module: plan.Module, Green: true, Ran: true, Verdict: verdict, Suite: suitePath}, nil
}

// isRuleFinding reports whether a recorded item is a rule violation (R1-R5,
// the Tier-1 well-formedness rules) as opposed to the suite narrating its own
// state.
func isRuleFinding(rule string) bool { return len(rule) == 2 && rule[0] == 'R' }

// archiveSuite persists one run's read-out next to the module's other audit
// artifacts (best-effort, never fatal — a findings report must not become a
// second, quieter failure).
func archiveSuite(ctx context.Context, plan *pyplan.Plan, res pygentest.Result) {
	rec := auditRecorder(ctx)
	if rec == nil {
		return
	}
	if _, err := rec.WriteJSON(plan.Module+".pygentest.json", res); err != nil {
		telemetry.Log(ctx).Warn("audit archive write failed", "error", err)
	}
}

// pygentestPlan rebuilds the deterministic plan for one batch file — the same
// construction convertbatchpy performs, without emitting a module.
func pygentestPlan(path string, b config.Batchpy) (*pyplan.Plan, error) {
	facts, err := scanner.ScanFile(path)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	irf, err := ir.ExtractFile(path)
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", path, err)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return pyplan.Build(batchflow.Build(irf, facts, string(src)), pyplan.Options{
		Shape: b.Shape, DMLLoop: b.DMLLoop, ChunkSize: b.ChunkSize,
		LoggerPrefix: b.LoggerPrefix, Entrypoint: b.Entrypoint,
		Wrapper: pyplan.Wrapper{
			Import: b.WrapperModule, RouterClass: b.RouterClass,
			ReadMode: b.ReadMode, WriteMode: b.WriteMode,
		},
	}), nil
}
