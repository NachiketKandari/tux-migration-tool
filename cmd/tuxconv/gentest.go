package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/testgen"
	"tux-migration-tool/internal/testscan"
	"tux-migration-tool/internal/validate"
)

// gentestLayers enumerates the service layers gentest can target; the
// layout contract is pkg/services/<svc>/{db,controller,handler,models}
// (PRD-2026-09-09 GT-D1).
var gentestLayers = []string{"db", "controller", "handler"}

// runGentest implements `tuxconv gentest <file|dir>` (PRD-2026-09-09): the
// post-conversion function → Go test pipeline over a converted Go service
// tree. The scan phase (GT-1) resolves the target, inventories functions,
// and detects existing coverage; -check-only reports the gap and writes
// nothing. Templates (GT-2) and generation (GT-3+) are pending.
func runGentest(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("gentest", flag.ContinueOnError)
	layers := fs.String("layers", "", "Comma-separated subset of db,controller,handler (default: all layers found in the target)")
	checkOnly := fs.Bool("check-only", false, "Report the test gap (functions without tests) and exit without writing")
	baseDir := fs.String("base", "", "Output base directory override (alias of -out; default: the converted tree's own module root, so tests land in the same folder as the converted code)")
	outDir := fs.String("out", "", "Explicit output root: tests plus a complete non-test source snapshot (collision-renamed) land here; the target tree is never modified")
	inPlace := fs.Bool("in-place", false, "Write each test file into the same folder as the converted code it covers (ignores -out/-base and paths.staged)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip the LLM gap-fill seam (overrides run.llm)")
	logFile := fs.String("log-file", "", "Parse a runtime log and take fixture values from it (empty = assumed placeholders; same folder + same log = byte-identical output)")
	niceNames := fs.Bool("nice-names", false, "Optional LLM polish for test names only (default off; never affects assertions or structure)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	templatesDir := fs.String("templates", "", "Directory of <template_id>.tmpl overrides (flag > templates.dir config; missing ids keep the embedded set)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if len(positional) == 0 {
		return fmt.Errorf("gentest: must provide a target (.go file, layer dir, service dir, or services root)")
	}
	target := positional[0]

	if *outDir != "" && *baseDir != "" && *outDir != *baseDir {
		return fmt.Errorf("gentest: -out and -base are aliases — give one of them (got %q and %q)", *outDir, *baseDir)
	}
	explicitOut := *outDir
	if explicitOut == "" {
		explicitOut = *baseDir
	}
	if *outDir != "" {
		baseDir = outDir
	}

	var layerFilter []testscan.Layer
	if *layers != "" {
		for _, l := range strings.Split(*layers, ",") {
			l = strings.TrimSpace(l)
			known := false
			for _, k := range gentestLayers {
				if l == k {
					known = true
					break
				}
			}
			if !known {
				return fmt.Errorf("gentest: -layers: got %q, want one of %v", l, gentestLayers)
			}
			layerFilter = append(layerFilter, testscan.Layer(l))
		}
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

	tgt, err := testscan.Resolve(target, layerFilter)
	if err != nil {
		return err
	}
	log.Info("gentest invoked", "target", target, "mode", string(tgt.Mode),
		"layers", strings.Join(layerNames(layerFilter), ","), "check_only", *checkOnly,
		"base", *baseDir, "no_llm", *noLLM)
	rep, err := tgt.Scan()
	if err != nil {
		return err
	}
	total, tested, gaps := rep.Counts()
	log.Info("gentest scan complete", "target", tgt.Path, "mode", string(tgt.Mode),
		"services", len(rep.Services), "functions", total, "tested", tested, "gaps", gaps,
		"warnings", len(rep.Warnings))

	if *checkOnly {
		if err := rep.WriteText(os.Stdout); err != nil {
			return err
		}
		// Check-only also honors -log-file: the log is parsed and its
		// fixture coverage is reported (which methods carry real values,
		// which fall back to assumed) without writing anything. This is
		// the preflight the web Tests step shows before a generate run.
		if *logFile != "" {
			logData, err := testgen.ParseLogFile(*logFile)
			if err != nil {
				return err
			}
			log.Info("gentest log parsed", "path", logData.Path, "traces", len(logData.Traces),
				"skipped_null", logData.SkippedNull, "stack_lines", logData.StackLines, "warnings", len(logData.Warnings))
			complete := 0
			for _, tr := range logData.Traces {
				if tr.Complete() {
					complete++
				}
			}
			fmt.Printf("log: %s — %d lines, %d traces (%d complete), %d warnings, %d requestID:null skipped, %d stack lines\n",
				logData.Path, logData.Lines, len(logData.Traces), complete, len(logData.Warnings), logData.SkippedNull, logData.StackLines)
			cov := testgen.CoverLog(rep, logData)
			fmt.Printf("log: fixtures — %d scanned functions with log values, %d assumed (of %d)\n",
				cov.FromLog, cov.Assumed, len(cov.Entries))
		}
		fmt.Println("check-only: no test files written")
		archiveGapReport(ctx, rep)
		return nil
	}
	// Scan warnings reach the operator in generate mode too (engine-wiring
	// audit Tier-1 #9: pre-fix they were logged as a count only; an
	// unreadable dir or unparseable file silently shrank the gap report).
	for _, wn := range rep.Warnings {
		log.Warn("gentest scan warning", "detail", wn)
		fmt.Println("  scan warning:", wn)
	}

	// Same-folder-first output: -in-place writes next to the converted
	// code, -base stages elsewhere, otherwise the converted tree's own
	// module root is the base — outPathFor then resolves each test file
	// into the same folder as the converted layer it covers. Trees
	// outside any module fall back to paths.staged (never implicit CWD).
	base := *baseDir
	if *inPlace {
		base = ""
	}
	if base == "" && !*inPlace {
		if root, rerr := validate.ResolveModuleRoot(target); rerr == nil {
			base = root
		}
	}
	if base == "" && !*inPlace {
		base = cfg.Paths.Staged
	}
	if base == "" && !*inPlace {
		base = config.DefaultStagedDir
	}

	client := resolveLLMClient(ctx, cfg, *noLLM, "controller gap-fill")
	llmEnabled := client != nil
	if *niceNames && client == nil {
		log.Warn("nice-names requested but no LLM client is available; names stay as generated")
		fmt.Println("  nice-names: no LLM client available — names left as generated")
	}

	wiring := newWiring(ctx, cfg)

	var logData *testgen.LogData
	if *logFile != "" {
		logData, err = testgen.ParseLogFile(*logFile)
		if err != nil {
			return err
		}
		log.Info("gentest log parsed", "path", logData.Path, "traces", len(logData.Traces),
			"skipped_null", logData.SkippedNull, "stack_lines", logData.StackLines, "warnings", len(logData.Warnings))
	}
	// -out/-base are explicit snapshots: stage the non-test sources next to
	// the generated tests and run the full test gate inside the out tree.
	explicit := explicitOut != "" && !*inPlace

	res, err := testgen.Generate(ctx, tgt, rep, testgen.Options{
		BaseDir: base, Workers: cfg.Concurrency.Workers, NoLLM: !llmEnabled,
		Client: client, Budget: wiring.budget,
		MaxRetries: cfg.ValidateCfg.MaxRetries, Audit: wiring.audit, Templates: tpl,
		Log: logData, NiceNames: *niceNames, Stage: explicit, FullTest: explicit,
	})
	if err != nil {
		return err
	}
	printGentestSummary(base, res)
	return nil
}

// printGentestSummary reports one run's per-function outcomes in input
// order, convert-summary style.
func printGentestSummary(base string, res *testgen.Result) {
	counts := map[string]int{}
	for _, u := range res.Units {
		counts[u.Status]++
	}
	fmt.Printf("gentest: %d functions under %s — %d generated (template), %d generated (llm), %d llm-required, %d skipped-covered, %d skipped-by-design, %d unsupported, %d llm calls\n",
		len(res.Units), base, counts[testgen.StatusTemplate], counts[testgen.StatusLLM], counts[testgen.StatusLLMNeeded],
		counts["skipped-covered"], counts[testgen.StatusDesign], counts[testgen.StatusUnsupported], res.LLMCalls)
	if len(res.Fixtures) > 0 {
		fromLog, assumed := 0, 0
		for _, f := range res.Fixtures {
			if f.Source == "assumed" {
				assumed++
			} else {
				fromLog++
			}
		}
		fmt.Printf("gentest: fixtures — %d methods from log values, %d assumed\n", fromLog, assumed)
	}
	for _, f := range res.Files {
		fmt.Println("  wrote:", f)
	}
	for _, f := range res.Staged {
		fmt.Println("  staged:", f)
	}
	for _, u := range res.Units {
		switch u.Status {
		case testgen.StatusLLMNeeded, testgen.StatusUnsupported, testgen.StatusDesign:
			fmt.Printf("  %s: %s — %s\n", u.Status, u.Func, u.Detail)
		}
	}
	for _, g := range res.Gates {
		fmt.Println("  gate:", g)
	}
	for _, c := range res.Checklist {
		fmt.Println("  ", c)
	}
	for _, w := range res.Warnings {
		fmt.Println("  warning:", w)
	}
	if res.TestsFailed {
		fmt.Println("gentest: tests FAILED (see gates)")
	}
}

// layerNames renders a layer filter for logs; nil means every testable
// layer.
func layerNames(layers []testscan.Layer) []string {
	if len(layers) == 0 {
		return gentestLayers
	}
	names := make([]string, 0, len(layers))
	for _, l := range layers {
		names = append(names, string(l))
	}
	return names
}

// archiveGapReport persists the scan outcome (machine twin of the stdout
// gap report) into the run's audit trail — best-effort, never fatal.
func archiveGapReport(ctx context.Context, rep *testscan.Report) {
	log := telemetry.Log(ctx)
	if _, err := auditRecorder(ctx).WriteJSON("gentest_gap_report.json", rep); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
}
