package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/convert"
	"tux-migration-tool/internal/gomockgen"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/ledger"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/templates"
	"tux-migration-tool/internal/validate"
)

// runConvert implements `tuxconv convert <file|dir> -mapping <yaml>` — the
// Phase 5 execution gate: deterministic plan units generate first (models,
// DB methods, interfaces, handler glue, router), then each mapped endpoint's
// controller body is filled through the LLM seam from the query-replaced
// branch view. Output lands in the target module when paths.mainGo resolves,
// else under paths.staged (the two-laptop degrade); the ledger makes runs
// resumable; every unit leaves an audit record. When the target directory
// holds several Tuxedo entry files, one worker converts each service
// end-to-end in parallel (convert_dir.go), each in its own output subtree.
func runConvert(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convertgo", flag.ContinueOnError)
	mappingFlag := fs.String("mapping", "", "User mapping YAML, or a directory of per-service yamls (each with source: <entry file>) when the target dir holds multiple services (default: convert.mapping from config, else the mappings/ convention)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	baseDir := fs.String("base", "", "Output base directory override (default: target module root when paths.mainGo resolves, else paths.staged; dir fan-out appends each service name)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: controllers render deterministic best-effort bodies (overrides run.llm)")
	fragment := fs.Bool("fragment", false, "Force fragment mode on a single-file input (PF-3.1)")
	noInline := fs.Bool("no-inline-fns", false, "Do not materialize cross-file fn_* helpers into their caller (directory mode only; they keep the legacy stub/TODO handling)")
	retryRepair := fs.Bool("retry-repair", false, "Retry methodology A/B: rejected LLM attempts are sent back as an assistant turn for patching instead of regenerated (default off)")
	templatesDir := fs.String("templates", "", "Directory of <template_id>.tmpl overrides (flag > templates.dir config; missing ids keep the embedded set)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
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

	target, err := resolveInput(positional, cfg)
	if err != nil {
		return err
	}

	files, mains, excluded, err := extractPlanIR(ctx, target, cfg, *fragment)
	if err != nil {
		return err
	}

	// Run-wide wiring: one budget/audit bundle plus the per-purpose client
	// cover every service in the run (the bundle is immutable/mutex-guarded;
	// the client is stateless) — A5.2's shared runWiring base.
	wiring := newWiring(ctx, cfg)
	v := validate.New(validate.Options{MainGo: cfg.Paths.MainGo, Compile: cfg.ValidateCfg.Compile, RunSmoke: cfg.ValidateCfg.Run})
	client := resolveLLMClient(ctx, cfg, *noLLM, "controller bodies")
	w := &convertWiring{
		cfg: wiring.cfg, budget: wiring.budget, validator: v, client: client,
		audit: wiring.audit, llmEnabled: client != nil,
		templates:   tpl,
		retryRepair: *retryRepair || (cfg.Convert.RetryRepair != nil && *cfg.Convert.RetryRepair),
	}

	baseRoot, degrade := resolveBaseRoot(*baseDir, cfg)

	// Fn-library mode: targets whose IR has no entry (fn libraries of
	// helper functions) convert directly — there are no endpoints to map,
	// so the draft-and-stop detour would be pure friction (nothing to
	// tag, no names to choose). Each library roots its own output subtree.
	fnLibMode := len(mains) > 0 && len(excluded) == 0
	for _, m := range mains {
		if !isFnLibFile(m) {
			fnLibMode = false
			break
		}
	}
	if fnLibMode {
		for _, main := range mains {
			if err := runConvertFnLib(ctx, w, main, baseRoot, cfg.Concurrency.Workers, degrade); err != nil {
				return err
			}
		}
		return nil
	}

	// Mapping resolution order: -mapping flag → convert.mapping → the
	// mappings/ convention (discover/auto-draft output). Nothing found →
	// scan the target, write drafts, and stop: the user reviews names,
	// then re-runs this same command to convert (§4.2.8 — the tool never
	// invents endpoints; it proposes and waits).
	mappingPath := mappingFor(*mappingFlag, cfg)
	if mappingPath == "" {
		return draftAndStop(ctx, target, cfg, *noLLM)
	}

	// Fan-out covers multi-entry dirs and filtered runs alike: when
	// convert.fileFilter dropped entries, the mapping path must be a
	// directory (per-service yamls) so the excluded services' mappings can
	// be exempted from the orphan check deliberately.
	if len(mains) > 1 || len(excluded) > 0 {
		return runConvertFanout(ctx, w, target, mains, files, excluded, mappingPath, baseRoot, degrade, *noInline)
	}

	// A directory mapping for a single-entry target: pick the yaml whose
	// source:/stem matches the entry (the mappings/ convention). Nothing
	// matches — including a convention dir holding only foreign drafts —
	// and the run drafts-and-stops instead.
	if fi, serr := os.Stat(mappingPath); serr == nil && fi.IsDir() {
		mappingPath, err = mappingForEntry(mappingPath, filepath.Base(mains[0].Path))
		if err != nil {
			return err
		}
		if mappingPath == "" {
			return draftAndStop(ctx, target, cfg, *noLLM)
		}
	}
	mapping, err := plan.LoadMapping(mappingPath)
	if err != nil {
		return err
	}
	// Derived module (module == service): the staged tree roots at the
	// service folder, matching the dir fan-out's per-service subtrees —
	// absPath drops the target path's first segment (the module root), so
	// the base carries it.
	base := baseRoot
	if mapping.Module == mapping.Service {
		base = filepath.Join(base, mapping.Service)
	}
	res, led, err := convertOneService(ctx, w, mains[0], files, mapping, base, cfg.Concurrency.Workers, *noInline, nil)
	if err != nil {
		return err
	}
	printServiceSummary(os.Stdout, mapping.Service, res, led, base, degrade)
	return nil
}

// isFnLibFile reports the fn-library shape: no Tuxedo entry, not a
// fragment, at least one locally-defined function (a fragment has none —
// PF-3's rubric keeps those a different concern).
func isFnLibFile(f *ir.File) bool {
	return f != nil && f.Entry == "" && !f.Fragment && len(f.Functions) > 0
}

// runConvertFnLib runs the fn-library pipeline: plan (db methods for the
// library's queries, an LLM seam per helper fn), ledger load, convert.Run
// — no mapping, no handler glue, no mocks. The output subtree roots at the
// library's service name (the file stem), like a dir fan-out service.
func runConvertFnLib(ctx context.Context, w *convertWiring, main *ir.File, baseRoot string, workers int, degrade string) error {
	log := telemetry.Log(ctx)
	start := time.Now()
	svcName := plan.FnLibServiceName(main.Path)
	log.Info("convert fn library started", "source", main.Path, "service", svcName, "fns", len(main.Functions))
	for _, u := range main.Unbalanced {
		log.Warn("unbalanced region in source — parse continues leniently, generated output may be incomplete",
			"service", svcName, "kind", u.Kind, "line", u.Line, "col", u.Col)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		return fmt.Errorf("convert: read source: %w", err)
	}
	p, err := plan.BuildFnLib(plan.Options{Main: main, Source: string(src), Budget: w.budget})
	if err != nil {
		return err
	}

	led, err := ledger.Load(w.cfg.Paths.Ledger, svcName)
	if err != nil {
		return err
	}

	base := baseRoot
	if p.Mapping.Module == p.Mapping.Service {
		base = filepath.Join(base, p.Mapping.Service)
	}
	res, err := convert.Run(ctx, convert.Options{
		Plan: p, Main: main, Source: string(src), FnFiles: nil,
		Client: w.client, Budget: w.budget, BaseDir: base,
		Ledger: led, Validator: w.validator, MaxRetries: w.cfg.ValidateCfg.MaxRetries, Audit: w.audit,
		Workers: workers,
		SkipLLM: !w.llmEnabled, WithGorm: w.cfg.DB.WithGorm,
		Templates:   w.templates,
		FlowDraft:   false,
		RetryRepair: w.retryRepair,
	})
	if err != nil {
		return err
	}
	if _, err := w.audit.WriteJSON(svcName+"_ledger.json", led); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	log.Info("convert fn library completed",
		"service", svcName, "base", base,
		"files", len(res.Files), "llm_calls", res.LLMCalls,
		"duration_s", elapsedSeconds(start))
	printServiceSummary(os.Stdout, svcName, res, led, base, degrade)
	return nil
}

// convertWiring carries one convert invocation's shared, concurrency-safe
// collaborators: the LLM client (stateless), token budget (immutable
// value), validator (stateless options holder), and audit recorder
// (mutex-guarded Write).
type convertWiring struct {
	cfg        *config.Config
	budget     budget.Budget
	validator  *validate.Validator
	client     llm.Client
	audit      *audit.Recorder
	llmEnabled bool
	// templates is the resolved template set (flag > config > embedded)
	// every service in the run renders from.
	templates templates.Provider
	// retryRepair is the resolved retry methodology (CLI flag OR config
	// convert.retryRepair) every service in the run uses.
	retryRepair bool
}

// draftAndStop is convert's no-mapping fallback (draft, then stop): the
// target is scanned for API candidates and editable mapping drafts land in
// mappings/ — AI-named when the model is reachable, deterministic names
// otherwise — and the run exits before generating anything, so the user's
// review is a free re-run (no tree cleanup, no half-converted state).
func draftAndStop(ctx context.Context, target string, cfg *config.Config, noLLM bool) error {
	client := resolveLLMClient(ctx, cfg, noLLM, "endpoint naming")
	bd := runBudget(cfg.Run)
	n, err := discoverCore(ctx, target, discoverOutDir(""), false, cfg, client, bd)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("convert: no mapping found — drafts written, conversion deferred to the re-run",
		"target", target, "drafts", n)
	return nil
}

// resolveBaseRoot resolves the run's output base: the -base override wins,
// else the target module root when paths.mainGo resolves, else the staged
// tree with the two-laptop degrade note.
func resolveBaseRoot(baseFlag string, cfg *config.Config) (root, degrade string) {
	if baseFlag != "" {
		return baseFlag, ""
	}
	if root, err := validate.ResolveModuleRoot(cfg.Paths.MainGo); err == nil && cfg.Paths.MainGo != "" {
		return root, ""
	}
	return cfg.Paths.Staged, "generated code staged under " + cfg.Paths.Staged + " (target service absent: set paths.mainGo to compile there)"
}

// convertOneService runs the full per-service pipeline: plan build, ledger
// load, convert.Run (deterministic scaffold + LLM controller bodies), the
// ledger audit copy, and mock regeneration. base is this service's isolated
// output root (the shared baseRoot in single-service mode, a per-service
// subtree in dir fan-out); workers sizes the inner DB-render pool.
//
// Cross-file fn_* helpers are materialized into this service first (see
// inline.go), so they plan as KindFnHelper units and their bodies render
// into controller/fns.go for the controller to call — instead of arriving
// as a stub plus a "TODO external fn call" at the call site. The expanded
// IR and source are threaded into BOTH plan.Build and convert.Run: they must
// agree, because each slices helper bodies out of the source text.
func convertOneService(ctx context.Context, w *convertWiring, main *ir.File, files []*ir.File, mapping *plan.Mapping, base string, workers int, noInline bool, corpusText map[string]string) (*convert.Result, *ledger.Ledger, error) {
	log := telemetry.Log(ctx)
	start := time.Now()
	log.Info("convert service started", "service", mapping.Service, "source", main.Path, "base", base)
	for _, u := range main.Unbalanced {
		log.Warn("unbalanced region in source — parse continues leniently, generated output may be incomplete",
			"service", mapping.Service, "kind", u.Kind, "line", u.Line, "col", u.Col)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("convert: read source: %w", err)
	}
	mainIR, mainSrc, err := expandFns(ctx, main, files, string(src), noInline, corpusText)
	if err != nil {
		return nil, nil, err
	}
	main, src = mainIR, []byte(mainSrc)
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: files, Mapping: mapping, Budget: w.budget})
	if err != nil {
		return nil, nil, err
	}

	led, err := ledger.Load(w.cfg.Paths.Ledger, mapping.Service)
	if err != nil {
		return nil, nil, err
	}

	res, err := convert.Run(ctx, convert.Options{
		Plan: p, Main: main, Source: string(src), FnFiles: files,
		Client: w.client, Budget: w.budget, BaseDir: base,
		Ledger: led, Validator: w.validator, MaxRetries: w.cfg.ValidateCfg.MaxRetries, Audit: w.audit,
		Workers: workers,
		SkipLLM: !w.llmEnabled, WithGorm: w.cfg.DB.WithGorm,
		Templates:   w.templates,
		FlowDraft:   w.cfg.Convert.FlowDraft == nil || *w.cfg.Convert.FlowDraft,
		TxWrap:      w.cfg.Convert.TxWrap == nil || *w.cfg.Convert.TxWrap,
		RetryRepair: w.retryRepair,
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := w.audit.WriteJSON(mapping.Service+"_ledger.json", led); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}

	runMocks(ctx, base, p)
	log.Info("convert service completed",
		"service", mapping.Service, "base", base,
		"files", len(res.Files), "llm_calls", res.LLMCalls,
		"duration_s", elapsedSeconds(start))
	return res, led, nil
}

// printServiceSummary reports one service's run outcome — the same lines in
// single-service and fan-out mode.
func printServiceSummary(w io.Writer, service string, res *convert.Result, led *ledger.Ledger, base, degrade string) {
	appended, failed, _, skipped, placeholders, deviated := led.Counts()
	fmt.Fprintf(w, "%s: %d files written under %s — units: %d appended, %d failed, %d skipped, %d placeholders, %d stubbed fns, %d sql deviations, %d llm calls, %d aliased columns, %d condition gaps\n",
		service, len(res.Files), base, appended, failed, skipped, placeholders, len(res.Stubs), deviated, res.LLMCalls, res.AliasedColumns, len(res.ConditionGaps))
	if degrade != "" {
		fmt.Fprintln(w, "  note:", degrade)
	}
	if res.TierB != nil && res.TierB.DegradeReason != "" {
		fmt.Fprintln(w, "  tier B:", res.TierB.DegradeReason)
	} else if res.TierB != nil {
		fmt.Fprintln(w, "  tier B:", res.TierB.Summary)
		// Engine-wiring audit Tier-1 #6: a failed batched Tier B must show
		// its trimmed compiler/vet/test output, not just the one-line
		// summary — the Errors slice was computed and dropped.
		for _, e := range res.TierB.Errors {
			fmt.Fprintln(w, "  tier B error:", e)
		}
	}
	for _, f := range res.Failed {
		fmt.Fprintln(w, "  failed:", f)
	}
	for _, s := range res.Skipped {
		fmt.Fprintln(w, "  skipped:", s)
	}
	for _, d := range res.SQLDeviations {
		fmt.Fprintln(w, "  sql deviation:", d)
	}
	for _, g := range res.ConditionGaps {
		fmt.Fprintln(w, "  condition gap:", g)
	}
	for _, st := range res.Stubs {
		fmt.Fprintln(w, "  stubbed fn (panics until implemented):", st)
	}
	for _, cw := range res.Warnings {
		fmt.Fprintln(w, "  coverage:", cw)
	}
}

// runMocks regenerates the uber-go/mock doubles when the mockgen binary and
// the target module are both available; otherwise it is a WARN + skip —
// never a run failure (plan-conversion §4.7). The runner lives in
// internal/gen (shared with `gentest`, PRD-2026-09-09 GT-D7); this wrapper
// computes the plan's two interface targets.
func runMocks(ctx context.Context, base string, p *plan.Plan) {
	targets := gomockgen.MockTargetsFor(base, p.Service)
	for i := range targets {
		// The plan's unit target paths may override the conventional
		// service subtree (custom layouts) — keep the mockRel resolution.
		folder := "db"
		if i == 1 {
			folder = "controller"
		}
		targets[i].Source = filepath.Join(base, mockRel(p, folder, "interface.go"))
		targets[i].Dest = filepath.Join(base, mockRel(p, folder, filepath.Base(targets[i].Dest)))
	}
	gomockgen.RunMocks(ctx, targets)
}

func mockRel(p *plan.Plan, folder, file string) string {
	for _, u := range p.Units {
		if strings.HasSuffix(u.TargetPath, "/"+folder+"/"+file) {
			parts := strings.SplitN(u.TargetPath, "/", 2)
			if len(parts) == 2 {
				return parts[1]
			}
		}
	}
	return folder + "/" + file
}
