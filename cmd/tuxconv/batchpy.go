package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/batchflow"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pycensus"
	"tux-migration-tool/internal/pygen"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/sqlchk"
	"tux-migration-tool/internal/telemetry"
	scanner "tux-migration-tool/internal/tsscan"
)

// runBatchpy implements `tuxconv batchpy <file|dir>` (PRD-2026-09-08): Pro*C
// batch programs → Python service modules reusing the tux parse stack. The
// deterministic scaffold (SQL constants, DAL/repository, service shell)
// always generates; the service body of repository-shape batches fills
// through the LLM seam unless -no-llm. Every module passes the pychk syntax
// gate + SQL fidelity check and reports its logic retention per mode.
func runBatchpy(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("convertbatchpy", flag.ContinueOnError)
	outDir := fs.String("out", "", "Output directory for generated modules (default: batchpy.outDir from config)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip the service-body LLM seam (overrides run.llm)")
	shape := fs.String("shape", "", "auto|repo — shape rubric override (default: batchpy.shape from config)")
	dmlLoop := fs.String("dml-loop", "", "batch|rowbyrow — cursor-DML semantics, simple shape only (repo shape renders row-by-row; default: batchpy.dmlLoop from config)")
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

	target, err := resolveBatchInput(positional, cfg)
	if err != nil {
		return fmt.Errorf("batchpy: %w", err)
	}

	b := cfg.Batchpy
	for _, o := range []struct {
		v, name string
		allowed []string
	}{
		{*shape, "batchpy.shape", []string{"auto", "repo"}},
		{*dmlLoop, "batchpy.dmlLoop", []string{"batch", "rowbyrow"}},
	} {
		if o.v == "" {
			continue
		}
		ok := false
		for _, a := range o.allowed {
			if o.v == a {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("-%s: got %q, want one of %v", strings.TrimPrefix(o.name, "batchpy."), o.v, o.allowed)
		}
	}
	if *shape != "" {
		b.Shape = *shape
	}
	if *dmlLoop != "" {
		b.DMLLoop = *dmlLoop
	}

	out := *outDir
	if out == "" {
		out = b.OutDir
	}
	if out == "" {
		out = config.DefaultBatchpyOut
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("batchpy: create output dir %s: %w", out, err)
	}

	paths, err := batchTargets(ctx, target, cfg.Batchpy.FileFilter)
	if err != nil {
		return err
	}

	client := resolveLLMClient(ctx, cfg, *noLLM, "batch service bodies")
	llmEnabled := client != nil
	wiring := newWiring(ctx, cfg)
	bg := wiring.budget
	rec := wiring.audit

	// Per-file worker fan-out (PRD-2026-09-08 BP-2.1): each worker converts
	// one batch file end-to-end — scan → flow → plan → generate → gates →
	// write — bounded by concurrency.workers (default 1 keeps runs serial;
	// workers>1 parallelizes the per-file pipeline including LLM calls, so
	// endpoint rate limits are the ceiling). Results print in input order.
	workers := cfg.Concurrency.Workers
	log.Info("batchpy fan-out", "target", target, "files", len(paths),
		"workers", workers, "out", out)
	type fileResult struct {
		path  string
		res   pygen.Result
		plan  *pyplan.Plan
		name  string
		start time.Time
		skip  string
		err   error
	}
	results := make([]fileResult, len(paths))
	common.RunIndexed(len(paths), workers, func(i int) {
		path := paths[i]
		start := time.Now()
		telemetry.Log(ctx).Info("batch file started", "source", path)
		res, plan, name, err := convertBatchFile(ctx, path, b, irOptions(cfg, false), pygen.Options{
			NoLLM: !llmEnabled || client == nil, Client: client, Budget: bg, MaxRetries: cfg.ValidateCfg.MaxRetries,
			Audit: rec, Templates: tpl,
		})
		var wp *wrongPipelineError
		if errors.As(err, &wp) {
			results[i] = fileResult{path: path, skip: err.Error(), start: start}
			return
		}
		results[i] = fileResult{path: path, res: res, plan: plan, name: name, start: start, err: err}
	})

	// Module-collision gate: the module name comes from the batch's
	// c_ServiceName literal, so distinct files can declare the same module —
	// concurrent writes to one .py would silently mix generations. Hard
	// error before anything is written (deterministic-first, loud).
	seen := map[string]string{}
	for _, r := range results {
		if r.err != nil || r.skip != "" {
			continue
		}
		if prev, dup := seen[r.name]; dup {
			return fmt.Errorf("batchpy: %s and %s both produce module %s.py — batch service names must be unique within one directory run (convert the files individually if intentional)", prev, r.path, r.name)
		}
		seen[r.name] = r.path
	}

	summary := 0
	skipped := 0
	var firstErr error
	for _, r := range results {
		if r.skip != "" {
			log.Warn("wrong pipeline — file skipped (severity F4 guard)", "source", r.path, "reason", r.skip)
			fmt.Printf("skipped: %s — %s\n", filepath.Base(r.path), r.skip)
			skipped++
			continue
		}
		if r.err != nil {
			log.Error("batchpy file failed", "source", r.path, "error", r.err,
				"duration_s", elapsedSeconds(r.start))
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		dst := filepath.Join(out, r.name+".py")
		if werr := os.WriteFile(dst, []byte(r.res.Content), 0o644); werr != nil {
			log.Error("batchpy file failed", "source", r.path, "error", werr)
			if firstErr == nil {
				firstErr = fmt.Errorf("batchpy: write %s: %w", dst, werr)
			}
			continue
		}
		// Support files the module imports (the db-router wrapper — pygentest
		// plan F4). Same content every run, so concurrent workers and repeat
		// runs overwrite identically; write failures fail the run loudly.
		for _, sf := range r.res.SupportFiles {
			sp := filepath.Join(out, sf.Path)
			if dir := filepath.Dir(sp); dir != "." {
				if werr := os.MkdirAll(dir, 0o755); werr != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("batchpy: create support dir %s: %w", dir, werr)
					}
					continue
				}
			}
			if werr := os.WriteFile(sp, []byte(sf.Content), 0o644); werr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("batchpy: write support file %s: %w", sp, werr)
				}
				continue
			}
		}
		log.Info("batch module written", "source", r.path, "output", dst,
			"shape", r.res.Retention.Shape, "dml_loop", r.res.Retention.DMLLoop,
			"retention_pct", fmt.Sprintf("%.1f", r.res.Retention.Percent()),
			"sql_deviations", r.res.Retention.SQLDeviations, "llm_calls", r.res.LLMCalls,
			"duration_s", elapsedSeconds(r.start))
		archiveBatchArtifacts(ctx, rec, r.name, r.plan, r.res)
		summary += r.res.Retention.SQLDeviations
		writeBatchModuleReport(os.Stdout, r.name, r.res, out)
	}
	fmt.Printf("batchpy: %d module(s) written under %s — %d sql deviations total", len(paths)-skipped, out, summary)
	if skipped > 0 {
		fmt.Printf(", %d skipped", skipped)
	}
	fmt.Println()
	return firstErr
}

// writeBatchModuleReport renders one written module's human report — the
// summary line (including the log-site parity the Retention contract
// promises alongside, engine-wiring audit Tier-1 #10), seam notes,
// structural issues, per-const SQL fidelity detail, and the interpreter
// gate's ast detail. Every outcome the engine computed reaches the
// operator here (Tier-1 #1/#11: no silent writes, no transcript-only
// fidelity).
func writeBatchModuleReport(w io.Writer, name string, res pygen.Result, out string) {
	ret := res.Retention
	fmt.Fprintf(w, "%s: shape=%s dml=%s consts=%d phases=%d syntax=%s sql_deviations=%d retention=%.0f%% llm_calls=%d log_sites=%d/%d -> %s\n",
		name, ret.Shape, ret.DMLLoop, ret.SelectTotal+ret.DMLTotal, ret.PhasesTotal, ret.PyMode,
		ret.SQLDeviations, ret.Percent(), ret.LLMCalls, ret.LogCallsEmitted, ret.LogSitesTotal,
		filepath.Join(out, name+".py"))
	writeBatchCensus(w, res.Census)
	for _, n := range res.Notes {
		fmt.Fprintln(w, "  note:", n)
	}
	for _, iss := range res.Structure {
		fmt.Fprintf(w, "  structure: line %d: %s\n", iss.Line, iss.Msg)
	}
	for _, d := range res.Fidelity {
		switch d.Status {
		case sqlchk.StatusDeviated:
			for _, dv := range d.Deviations {
				fmt.Fprintf(w, "  sql deviation: %s [%s] %s\n", d.Method, dv.Kind, dv.Detail)
			}
		case sqlchk.StatusUnverifiable:
			fmt.Fprintln(w, "  sql unverifiable:", d.Method)
		}
	}
	if !res.PyOK && ret.PyMode == "ast" {
		fmt.Fprintln(w, "  python syntax:", res.PyDetail)
	}
}

// writeBatchCensus renders the §6.1 accounting census: the counters that
// prove every EXEC SQL region and every non-SQL statement reached exactly one
// accounted bucket. Phase 0 of docs/pybatchpy-logic-plan.md reports this
// without gating on it, so the residue lines are printed as evidence rather
// than as errors — Phase 3 promotes them.
//
// `unique` and `accounted` must be equal: every query unit the flow found
// reached exactly one repository method or one date-math entry. `folded` is
// how many units share an entry's statement identity and `date_math` how many
// were moved out of the repository — the two are what let `constants` shrink
// while `accounted` does not.
//
// The point is that a construct the drop table ignores can no longer be
// invisible: `exit` appears on 38 sites of the dt_cmpt fixture and, before
// this census, appeared in no report at all.
func writeBatchCensus(w io.Writer, c pycensus.Census) {
	if c.SQL.Sites == 0 && c.Statements.Total == 0 {
		return // no facts supplied — nothing measured, nothing claimed
	}
	fmt.Fprintf(w, "  census sql: sites=%d query=%d unique=%d accounted=%d constants=%d repo_methods=%d date_math=%d folded=%d residue=%d\n",
		c.SQL.Sites, c.SQL.QuerySites, c.SQL.QueryUnique, c.SQL.Accounted,
		c.SQL.Constants, c.SQL.RepoMethods, c.SQL.DateMath, c.SQL.Folded, c.SQL.Residue)
	if len(c.SQL.DateMathByOp) > 0 {
		fmt.Fprintf(w, "  census date math by idiom: %v\n", c.SQL.DateMathByOp)
	}
	fmt.Fprintf(w, "  census stmts: total=%d translated=%d elided=%d tuxedo=%d log_only=%d unknown=%d\n",
		c.Statements.Total,
		c.Statements.ByBucket[string(pycensus.BucketTranslated)],
		c.Statements.ByBucket[string(pycensus.BucketElided)],
		c.Statements.ByBucket[string(pycensus.BucketTuxedoRuntime)],
		c.Statements.ByBucket[string(pycensus.BucketLogOnly)],
		c.Statements.ByBucket[string(pycensus.BucketUnknown)])
	if c.Dispatch.Switches > 0 {
		fmt.Fprintf(w, "  census dispatch: switches=%d arms=%v\n", c.Dispatch.Switches, c.Dispatch.CaseLabels)
	}
	if c.Loops.Total > 0 {
		fmt.Fprintf(w, "  census loops: total=%d bounded=%d guarded=%d indefinite=%d\n",
			c.Loops.Total, c.Loops.Bounded, c.Loops.Guarded, c.Loops.Indefinite)
	}
	if len(c.Statements.Guards) > 0 {
		fmt.Fprintf(w, "  census guards: %d registration return value(s) compared (concurrency controls, not logging)\n",
			len(c.Statements.Guards))
	}
	for _, f := range c.Findings {
		fmt.Fprintf(w, "  census finding: %s\n", f)
	}
}

// wrongPipelineError marks a Tuxedo service entry (SVC_*) fed to the batch
// pipeline (severity F4) — a visible skip, never a plausible-looking empty
// module.
type wrongPipelineError struct{ entry string }

func (e *wrongPipelineError) Error() string {
	return fmt.Sprintf("%s is a Tuxedo service entry, not a batch program — wrong pipeline; convert it with `tuxconv convert`", e.entry)
}

// convertBatchFile runs the batchpy pipeline for one .pc file — scan →
// flow → plan → generate → gates — and returns the generated module; the
// caller owns the ordered write phase (collision gate + input-order
// writes), so workers never race one output file. irOpts threads the run
// config's buffer-role registry (audit Tier-1 #4) — batchflow's census and
// the IR archive stay consistent with the extract/convertgo paths.
func convertBatchFile(ctx context.Context, path string, b config.Batchpy, irOpts ir.Options, gOpts pygen.Options) (pygen.Result, *pyplan.Plan, string, error) {
	facts, err := scanner.ScanFile(path)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: scan %s: %w", path, err)
	}
	irf, err := ir.ExtractFileOpts(path, irOpts)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: extract %s: %w", path, err)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: read %s: %w", path, err)
	}

	flow := batchflow.Build(irf, facts, string(src))
	if strings.HasPrefix(flow.Entry, "SVC_") {
		// Wrong-pipeline guard (severity F4): the flow entry fell back to a
		// Tuxedo service function — no batch main exists here.
		return pygen.Result{}, nil, "", &wrongPipelineError{entry: flow.Entry}
	}
	if len(flow.Queries) == 0 {
		telemetry.Log(ctx).Warn("batch carries no SQL — the module will have no constants and no DAL (degenerate but visible)",
			"source", path)
	}
	plan := pyplan.Build(flow, pyplan.Options{
		Shape: b.Shape, DMLLoop: b.DMLLoop, ChunkSize: b.ChunkSize,
		LoggerPrefix: b.LoggerPrefix, Entrypoint: b.Entrypoint,
		Wrapper: pyplan.Wrapper{
			Import: b.WrapperModule, RouterClass: b.RouterClass,
			ReadMode: b.ReadMode, WriteMode: b.WriteMode,
		},
	})
	gOpts.Plan = plan
	gOpts.Source = string(src)
	gOpts.SourcePath = path
	// The §6.1 accounting census reads the scanner facts, so it is handed
	// the ones this file already produced. Report-only — it changes no
	// emitted bytes and gates nothing yet.
	gOpts.Facts = facts

	res, err := pygen.Generate(ctx, gOpts)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: generate %s: %w", path, err)
	}
	return res, plan, plan.Module, nil
}

// batchTargets resolves the input to a list of .pc/.pcf files (file or dir).
// A directory target is scoped by the batchpy.fileFilter yaml when set —
// base names containing the substring (case-insensitive) survive; an empty
// filter keeps every file. An explicitly passed file always converts.
func batchTargets(ctx context.Context, target string, filter string) ([]string, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("batchpy: cannot access %s: %w", target, err)
	}
	if !fi.IsDir() {
		return []string{target}, nil
	}
	var paths []string
	err = filepath.Walk(target, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() {
			switch strings.ToLower(filepath.Ext(p)) {
			case ".pc", ".pcf":
				paths = append(paths, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter) != "" {
		needle := strings.ToLower(filter)
		var kept []string
		for _, p := range paths {
			if strings.Contains(strings.ToLower(filepath.Base(p)), needle) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("batchpy: no .pc/.pcf file in %s matches batchpy.fileFilter %q", target, filter)
		}
		telemetry.Log(ctx).Info("batchpy.fileFilter applied",
			"filter", filter, "matched", len(kept), "of", len(paths))
		paths = kept
	}
	return paths, nil
}

// archiveBatchArtifacts writes the module, its plan, its retention report,
// and its per-const fidelity detail into the run's audit trail (best-effort,
// never fatal). JSON artifacts go through the shared WriteJSON
// (nil-receiver tolerated, A5.2).
func archiveBatchArtifacts(ctx context.Context, rec *audit.Recorder, name string, plan *pyplan.Plan, res pygen.Result) {
	if rec == nil {
		return
	}
	log := telemetry.Log(ctx)
	if _, err := rec.Write(name+".py", func(w io.Writer) error {
		_, werr := w.Write([]byte(res.Content))
		return werr
	}); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if _, err := rec.WriteJSON(name+".batchplan.json", plan); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if _, err := rec.WriteJSON(name+".retention.json", res.Retention); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if _, err := rec.WriteJSON(name+".fidelity.json", res.Fidelity); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	// The §6.1 accounting census archives next to the fidelity detail: it
	// is the evidence for "nothing was lost", where fidelity is the
	// evidence for "nothing was changed".
	if _, err := rec.WriteJSON(name+".census.json", res.Census); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if len(res.Notes) > 0 {
		if _, err := rec.Write(name+".notes.txt", func(w io.Writer) error {
			for _, n := range res.Notes {
				if _, werr := w.Write([]byte(n + "\n")); werr != nil {
					return werr
				}
			}
			return nil
		}); err != nil {
			log.Warn("audit archive write failed", "error", err)
		}
	}
}
