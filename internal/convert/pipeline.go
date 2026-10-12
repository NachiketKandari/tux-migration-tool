// Package convert is the conversion orchestrator (PRD §4.2, architecture.md
// Phase 5): it executes the deterministic plan unit by unit — models, DB
// methods, accumulated interfaces, handler glue and router render with zero
// LLM calls — and fills exactly one template-shaped gap per endpoint with
// the LLM: the controller body, generated from the query-replaced branch
// view plus the DB signatures (never raw SQL, §4.2.4/§4.3). Every unit
// transitions the ledger (resumable) and leaves an audit record (§4.7).
package convert

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/gen"
	"tux-migration-tool/internal/goast"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/ledger"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/profile"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/templates"
	"tux-migration-tool/internal/validate"
)

// receiverOf resolves the run's store-receiver convention (P1: the profile
// DB rules; goitem default).
func receiverOf(opts Options) string {
	if opts.prof != nil {
		return opts.prof.DB().StoreReceiver
	}
	return profile.Default().DB().StoreReceiver
}

// Options carries one convert run's wiring.
type Options struct {
	// prof selects the target conventions (P1); nil = goitem default.
	prof       profile.Profile
	Plan       *plan.Plan
	Main       *ir.File
	Source     string
	FnFiles    []*ir.File
	Client     llm.Client // the LLM seam — fake server in CI
	Budget     budget.Budget
	BaseDir    string // module root when the target service exists, else the staged root
	Ledger     *ledger.Ledger
	Validator  *validate.Validator
	MaxRetries int
	Audit      *audit.Recorder // per-run audit folder (§4.7); nil = skip
	Workers    int             // DB-unit render pool size (concurrency.workers); <1 → 1
	// SkipLLM is the deterministic-only mode (run.llm: false): controller
	// and fn-helper units render deterministic best-effort bodies (zero LLM
	// calls) and are marked skipped so a later LLM-enabled run upgrades
	// exactly those marker-carrying methods.
	SkipLLM bool
	// WithGorm renders the store with the legacy *gorm.DB handle alongside
	// sqlx (db.withGorm); default is the plain sqlx-only store.
	WithGorm bool
	// FlowDraft feeds the deterministic flow-tree transpilation draft
	// (PRD-2026-09-10 FLW-D7) into the controller prompt as a verified base
	// the LLM enhances; false keeps the legacy prompt. The REQUIRED-CALLS
	// gate is unchanged either way.
	FlowDraft bool
	// TxWrap deterministically repairs a combined fragment body that fails
	// the transaction gate (convert.txWrap, default on): the repair threads
	// the tx handle through every tx call site and wraps the body in
	// utils.ExecTransaction. Repair-only on would-fail output, re-gated
	// before use; false keeps the loud combined-gate failure.
	TxWrap bool
	// RetryRepair switches the LLM seams' retry methodology (retryRepair
	// A/B, default off): true sends a rejected attempt's payload back as an
	// assistant turn so the model patches it against the gate notes; false
	// regenerates from the original prompt. Only the message assembly
	// changes — gates, budgets, and the retry count are identical.
	RetryRepair bool
	// Templates is the template provider (user overlay over the embedded
	// set); nil = the embedded defaults.
	Templates templates.Provider
}

// Result summarizes one convert run.
type Result struct {
	Files        []string
	LLMCalls     int
	Stubs        []string
	Failed       []string
	Skipped      []string
	Placeholders []string
	TierB        *validate.Result
	// SQLDeviations lists the PF-6 fidelity findings: db methods whose
	// generated SQL drifted from the source Tux SQL, and SQL-free artifacts
	// that leaked SQL keywords. Flag-only — the run itself never fails.
	SQLDeviations []string
	// Warnings carries the plan's arm-coverage advisories (advisory, the
	// run never fails on them).
	Warnings []string
	// AliasedColumns counts the sanctioned computed-column `AS` aliases
	// injected across the run's db methods (Workstream A observability).
	AliasedColumns int
	// ConditionGaps lists the Workstream B transparency findings: units
	// whose accepted-or-failed bodies dropped a legacy condition after the
	// retry budget. A gap after retries fails its unit loudly.
	ConditionGaps []string
}

// fileArtifact is one deterministic whole-file output: the plan unit it
// satisfies, its display name, the base-relative target path, and how to
// render it.
type fileArtifact struct {
	id, name, path string
	render         func() (string, error)
}

// scenRun is the entry-level scenario context (G-SCEN6), computed at most
// once per convert run: the entry's flow tree and the cross-scenario diff
// the prompt's shared-block section consumes. nil when no endpoint is a
// scenario slice.
type scenRun struct {
	tree *flow.Tree
	diff *flow.ScenarioDiff
}

// scenPrompt is one endpoint's scenario-shaped prompt additions: the slice
// context, the scenarioFilter evidence (expression + matched/pruned
// assignments), the loud unfold residue, the shared-block extents (compact
// — the full report lives in scenarios/<entry>.shared.md), and the legacy
// transaction facts.
type scenPrompt struct {
	Key     string
	Filter  string
	Matched []string
	Pruned  []string
	Residue []string
	Guards  []string // FoldMixed runtime-dispatch guards (contract + evidence)
	Shared  []string
	TxNotes []string
}

// Run executes the plan. Deterministic units regenerate byte-identically on
// resume; only pending LLM units consume the client.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Plan == nil || opts.Main == nil || opts.Ledger == nil || opts.Validator == nil {
		return nil, fmt.Errorf("convert: plan, main IR, ledger and validator are required")
	}
	svc, err := gen.NewService(gen.Options{Plan: opts.Plan, Main: opts.Main, FnFiles: opts.FnFiles, WithGorm: opts.WithGorm, Source: opts.Source, Templates: opts.Templates})
	if err != nil {
		return nil, err
	}
	res := &Result{}

	// Scenario context (G-SCEN6): computed once when any endpoint is a
	// scenario slice — the flow tree the flattened views fold from plus the
	// cross-scenario diff.
	var sr *scenRun
	for _, e := range opts.Plan.Mapping.Endpoints {
		if e.ScenarioRef != "" || e.ScenarioFilter != "" {
			sr = scenRunOf(opts)
			break
		}
	}

	// Staged-collision guard (live-eval finding, 2026-09-08): a fresh run
	// must not write into a staged tree left by a previous run —
	// controller/db files accumulate, so the generations would silently mix
	// (observed: two generations of the same methods in one file). A fresh
	// ledger plus an existing target file is a hard error: clear the
	// previous run's tree or resume with its ledger.
	if opts.Ledger.Fresh() {
		probe, perr := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("models")+"/"+svc.Mapping.Service+".go")
		if perr == nil {
			if _, serr := os.Stat(probe); serr == nil {
				return nil, fmt.Errorf("convert: output already exists (%s) but the ledger is fresh — clear the previous run's tree or resume with its ledger", probe)
			}
		}
	}

	// Generation-change advisory (audit 2026-09-16): plan unit IDs are
	// positional, so a mapping rename reuses an ID for a different
	// endpoint/method. Ledger.Get resets such units to planned, but files
	// already on disk (controller bodies, the conversion map) still carry
	// the old generation. Say so loudly — for a clean tree, clear the
	// staged dir and the service ledger and re-run.
	if renamed := renamedUnits(opts.Plan, opts.Ledger); len(renamed) > 0 {
		msg := "mapping rename detected since the last run (" + strings.Join(renamed, ", ") + ") — stale artifacts from the old generation may remain in the staged tree; for a clean tree clear the staged dir and the service ledger and re-run"
		telemetry.Log(ctx).Warn("generation change", "units", strings.Join(renamed, ", "))
		res.Warnings = append(res.Warnings, msg)
	}

	// Stubbed helpers (unresolved external fns, stub-and-carry-on
	// 2026-09-10): the endpoints that call them generate against the
	// stub in controller/fnstubs.go, visibly. Each stub first gets one
	// best-effort LLM synthesis attempt (own seam, input/output signatures
	// shown — easy pure helpers like fn_long_to_int land as real idiomatic
	// Go; anything the model declines or that fails the gate keeps the
	// panicking stub, never a guessed body). A resume whose fnstubs.go
	// already landed skips the seam outright — synthesis is a first-run
	// cost, never a per-resume LLM call.
	var stubSynth, stubMarks map[string]string
	if fnStubLanded(opts) {
		stubSynth, stubMarks = map[string]string{}, map[string]string{}
	} else {
		stubSynth, stubMarks = synthesizeStubs(ctx, opts, res)
	}
	for _, st := range opts.Plan.Stubs {
		entry := st.Fn + " → " + strings.Join(st.Endpoints, ", ")
		if m, ok := stubMarks[st.Fn]; ok && m != "" {
			entry += " (" + m + ")"
		}
		res.Stubs = append(res.Stubs, entry)
	}
	// Arm-coverage advisories ride the result summary — omission is the
	// user's choice; silence about an unmapped arm is not.
	res.Warnings = append(res.Warnings, opts.Plan.Warnings...)

	// 1. Models — deterministic. A fn library without queries has no row
	// shapes; the plan carries no models unit and the file is skipped.
	if unitID(opts.Plan, plan.KindModels) != "" {
		modelsPath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("models")+"/"+svc.Mapping.Service+".go")
		if err != nil {
			return nil, err
		}
		if err := generateFile(ctx, opts, res, "u01", "models", "models.go", modelsPath, func() (string, error) { return svc.ModelFile(opts.Plan) }); err != nil {
			return nil, err
		}
	}

	// 2. DB methods — deterministic. Renders run through a bounded worker
	// pool (concurrency.workers); the ledger and the interface accumulate
	// serially in unit order, so bytes are identical to a workers=1 run.
	// A pure-logic fn library has no queries — no db surface at all.
	dbBodies := map[string]dbOut{}
	// dbFilePath rides Run scope: the sql-free negative gate excludes it at
	// every call site (the fn-helpers call site sits outside the db gate's
	// scope).
	var dbFilePath string
	if dbUnits := unitsOf(opts.Plan, plan.KindDBMethod); len(dbUnits) > 0 {
		var err error
		dbBodies, err = renderDBUnits(svc, dbUnits, opts.workerCount())
		if err != nil {
			return nil, err
		}
		for _, u := range dbUnits {
			e := opts.Ledger.Get(u.ID, string(u.Kind), u.Name)
			if e.Status == ledger.StatusAppended {
				continue // resume: already recorded
			}
			opts.Ledger.Set(u.ID, ledger.StatusGenerated, "")
			addMap(opts, res, u, []string{u.TargetPath})
		}
		dbFilePath, err = opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("db")+"/"+svc.Mapping.Service+".go")
		if err != nil {
			return nil, err
		}
		dbFile, err := svc.DBMethodsFile(opts.Plan)
		if err != nil {
			return nil, err
		}
		if err := writeFileValidated(ctx, opts, res, dbFilePath, dbFile, "db-methods"); err != nil {
			return nil, err
		}
		for _, u := range dbUnits {
			opts.Ledger.Set(u.ID, ledger.StatusAppended, "", relPath(opts.BaseDir, dbFilePath))
		}
		checkDBFidelity(ctx, opts, res, svc, dbFilePath, dbUnits)
		// Deterministic computed-column aliases (Workstream A
		// observability): the count rides the run summary, alignment
		// skips ride the warnings, and the item → alias trail rides the
		// sql-aliases.json audit record (never write-only).
		if n, warns, trail := svc.AliasReport(opts.Plan); len(trail) > 0 || len(warns) > 0 {
			res.AliasedColumns += n
			res.Warnings = append(res.Warnings, warns...)
			if len(trail) > 0 {
				writeAuditJSON(ctx, opts, "sql-aliases.json", trail)
			}
		}

		ifacePath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("db")+"/interface.go")
		if err != nil {
			return nil, err
		}
		// Rebuild the interface from scratch every run (audit 2026-09-16):
		// AccumulateDBInterface dedups same-name/same-signature lines, but a
		// mapping rename leaves the old generation's methods in the file
		// alongside the new ones (observed: 12 methods after a rename), and
		// a resume would restack them. Rebuilding from the skeleton keeps
		// single-run and resume bytes identical.
		if err := os.Remove(ifacePath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("convert: clear stale db interface: %w", err)
		}
		for _, u := range unitsOf(opts.Plan, plan.KindDBMethod) {
			if err := svc.AccumulateDBInterface(ifacePath, dbBodies[u.ID].sig); err != nil {
				return nil, fmt.Errorf("convert: accumulate db interface: %w", err)
			}
		}
		if err := validateFile(ctx, opts, ifacePath); err != nil {
			return nil, err
		}
		res.Files = append(res.Files, ifacePath)
		checkSQLFreeArtifacts(ctx, opts, res, dbFilePath)
	}

	// 3. Controller + handler glue — service-shaped plans only. A fn
	// library has no endpoints: no controller interface, no handlers, no
	// router (the fn helpers carry their own struct scaffold below).
	if len(unitsOf(opts.Plan, plan.KindControllerMethod)) > 0 {
		handlerIfaceID := unitID(opts.Plan, plan.KindHandlerInterface)
		artifacts := []fileArtifact{
			{unitID(opts.Plan, plan.KindControllerInterface), "controller-interface.go",
				svc.Mapping.ImportPath("controller") + "/interface.go",
				func() (string, error) { return svc.ControllerInterface(opts.Plan) }},
		}
		if len(opts.Plan.Stubs) > 0 {
			artifacts = append(artifacts, fileArtifact{unitID(opts.Plan, plan.KindFnStub), "fnstubs.go",
				svc.Mapping.ImportPath("controller") + "/fnstubs.go",
				func() (string, error) { return svc.FnStubFileWithSynth(opts.Plan, stubSynth) }})
		}
		artifacts = append(artifacts,
			fileArtifact{handlerIfaceID, "handler-interface.go",
				svc.Mapping.ImportPath("handler") + "/interface.go",
				func() (string, error) { return svc.HandlerInterface(opts.Plan) }},
			fileArtifact{handlerIfaceID + "+methods", "handler-methods.go",
				svc.Mapping.ImportPath("handler") + "/" + svc.Mapping.Service + ".go",
				func() (string, error) { return svc.HandlerMethodsFile() }},
			fileArtifact{unitID(opts.Plan, plan.KindRouter), "router-snippet",
				svc.Mapping.ImportPath("handler") + "/router_snippet.txt",
				func() (string, error) { return svc.Router() }},
		)
		for _, a := range artifacts {
			path, err := opts.absPath(opts.BaseDir, a.path)
			if err != nil {
				return nil, err
			}
			var render = a.render
			if err := generateFile(ctx, opts, res, a.id, "file", a.name, path, render); err != nil {
				return nil, err
			}
		}
	} else if len(opts.Plan.Stubs) > 0 {
		// fn-lib mode still stubs its unresolved external calls.
		path, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("controller")+"/fnstubs.go")
		if err != nil {
			return nil, err
		}
		if err := generateFile(ctx, opts, res, unitID(opts.Plan, plan.KindFnStub), "file", "fnstubs.go", path,
			func() (string, error) { return svc.FnStubFileWithSynth(opts.Plan, stubSynth) }); err != nil {
			return nil, err
		}
	}

	// 4. Controller bodies — the one LLM gap per endpoint (§4.2.4).
	ctrlFilePath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("controller")+"/"+svc.Mapping.Service+".go")
	if err != nil {
		return nil, err
	}
	for _, u := range unitsOf(opts.Plan, plan.KindControllerMethod) {
		entry := opts.Ledger.Get(u.ID, string(u.Kind), u.Name) // register before any transition
		if entry.Status == ledger.StatusAppended {
			if landed, det := detControllerState(ctrlFilePath, u.Name); landed && !det {
				continue // resume: accepted LLM body already on disk
			} else if landed {
				telemetry.Log(ctx).Info("deterministic controller body queued for LLM upgrade",
					"unit", u.Name, "path", ctrlFilePath)
			} else {
				telemetry.Log(ctx).Info("controller method missing despite appended ledger — regenerating",
					"unit", u.Name, "path", ctrlFilePath)
			}
		}
		if opts.SkipLLM {
			// Deterministic-only mode: render the best-effort body (zero
			// LLM calls). An accepted LLM body on disk always wins — never
			// downgrade it — and a synthesis gap keeps the legacy skip.
			if entry.Status == ledger.StatusAppended && !detControllerLanded(ctrlFilePath, u.Name) && methodLanded(ctrlFilePath, u.Name) {
				continue
			}
			body, derr := deterministicController(ctx, opts, svc, u.Name)
			if derr != nil {
				opts.Ledger.Set(u.ID, ledger.StatusSkipped, "llm disabled (run.llm: false)")
				res.Skipped = append(res.Skipped, u.Name)
				continue
			}
			if err := appendControllerMethod(ctx, opts, res, svc, u, ctrlFilePath, body); err != nil {
				opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
				res.Failed = append(res.Failed, u.Name)
				continue
			}
			opts.Ledger.Set(u.ID, ledger.StatusSkipped, "deterministic no-llm body (LLM resume upgrades)")
			res.Skipped = append(res.Skipped, u.Name)
			addMap(opts, res, u, []string{relPath(opts.BaseDir, ctrlFilePath)})
			continue
		}
		if opts.Client == nil {
			return nil, fmt.Errorf("convert: endpoint %s needs the LLM client but none is configured", u.Name)
		}
		body, rejected, _, err := controllerBody(ctx, opts, res, svc, u, dbBodies, sr, ctrlFilePath)
		if err != nil {
			// Keep the last rejected output visible (commented, behind a
			// panicking placeholder) — an empty controller helps nobody.
			appendRejectedController(ctx, opts, res, svc, u, ctrlFilePath, rejected, err)
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
			continue
		}
		if err := appendControllerMethod(ctx, opts, res, svc, u, ctrlFilePath, body); err != nil {
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
			continue
		}
		opts.Ledger.Set(u.ID, ledger.StatusAppended, "", relPath(opts.BaseDir, ctrlFilePath))
		addMap(opts, res, u, []string{relPath(opts.BaseDir, ctrlFilePath)})
	}

	// 4b. TPCall placeholders — deterministic, compilable stubs (PF-4.5):
	// one `tuxconv:TODO` stub per site, ledger status placeholder, mapped in
	// the conversion map like every other unit.
	if err := renderTPCallPlaceholders(ctx, opts, res, svc); err != nil {
		return nil, err
	}

	// 4c. Fn-library helper bodies — one LLM gap per fn (fn-lib mode):
	// each legacy helper translates into a Go method on the controller
	// struct, store calls for its SQL, the legacy status-return contract
	// otherwise. The file assembles method by method, resume-safe like
	// the controllers; -no-llm renders deterministic best-effort helpers
	// marked skipped for a later resume to upgrade.
	if fnUnits := unitsOf(opts.Plan, plan.KindFnHelper); len(fnUnits) > 0 {
		fnFilePath, ferr := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("controller")+"/fns.go")
		if ferr != nil {
			return nil, ferr
		}
		for _, u := range fnUnits {
			entry := opts.Ledger.Get(u.ID, string(u.Kind), u.Name) // register before any transition
			if entry.Status == ledger.StatusAppended {
				if landed, det := detFnHelperState(fnFilePath, u.Name); landed && !det {
					continue // resume: accepted LLM helper already on disk
				} else if landed {
					telemetry.Log(ctx).Info("deterministic fn helper queued for LLM upgrade",
						"unit", u.Name, "path", fnFilePath)
				} else {
					telemetry.Log(ctx).Info("fn helper missing despite appended ledger — regenerating",
						"unit", u.Name, "path", fnFilePath)
				}
			}
			if opts.SkipLLM {
				// Deterministic-only mode: same posture as the controllers —
				// an accepted LLM helper wins, a synthesis gap keeps the skip.
				if entry.Status == ledger.StatusAppended && !detFnHelperLanded(fnFilePath, u.Name) && methodLanded(fnFilePath, u.Name) {
					continue
				}
				body, derr := deterministicFnHelper(ctx, opts, svc, u.Name)
				if derr != nil {
					// A deterministic helper that fails its gates used to
					// vanish from fns.go here while tux.go kept calling
					// it — the emitted package then did not compile, with
					// nothing in the tree saying so. Emit a compiling
					// placeholder carrying the prescribed signature (which
					// the caller was generated against) and a reason-coded
					// TODO, the same posture appendRejectedFnHelper takes on
					// the LLM path. The census then counts it like any
					// other gap instead of the file losing a method.
					if perr := appendPlaceholderFnHelper(ctx, opts, res, svc, u, fnFilePath, derr); perr != nil {
						telemetry.Log(ctx).Warn("gated-out helper placeholder not written",
							"unit", u.Name, "error", perr.Error())
					}
					opts.Ledger.Set(u.ID, ledger.StatusSkipped, "llm disabled (run.llm: false); deterministic body failed its gates")
					res.Skipped = append(res.Skipped, u.Name)
					res.Warnings = append(res.Warnings, u.Name+" deterministic body failed its gates — a compiling placeholder with the prescribed signature is in fns.go")
					continue
				}
				if err := appendFnHelper(ctx, opts, res, svc, fnFilePath, u.Name, body); err != nil {
					opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
					res.Failed = append(res.Failed, u.Name)
					continue
				}
				opts.Ledger.Set(u.ID, ledger.StatusSkipped, "deterministic no-llm helper (LLM resume upgrades)")
				res.Skipped = append(res.Skipped, u.Name)
				addMap(opts, res, u, []string{relPath(opts.BaseDir, fnFilePath)})
				continue
			}
			if opts.Client == nil {
				return nil, fmt.Errorf("convert: fn helper %s needs the LLM client but none is configured", u.Name)
			}
			body, rejected, _, err := fnHelperBody(ctx, opts, res, svc, u, dbBodies, fnFilePath)
			if err != nil {
				// Same visible-fallback posture as the controller path: the
				// rejected helper stays commented behind its panicking
				// placeholder instead of vanishing from fns.go.
				appendRejectedFnHelper(ctx, opts, res, svc, u, fnFilePath, rejected, err)
				opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
				res.Failed = append(res.Failed, u.Name)
				continue
			}
			if err := appendFnHelper(ctx, opts, res, svc, fnFilePath, u.Name, body); err != nil {
				opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
				res.Failed = append(res.Failed, u.Name)
				continue
			}
			opts.Ledger.Set(u.ID, ledger.StatusAppended, "", relPath(opts.BaseDir, fnFilePath))
			addMap(opts, res, u, []string{relPath(opts.BaseDir, fnFilePath)})
		}
		checkSQLFreeArtifacts(ctx, opts, res, dbFilePath, fnFilePath)
	}

	if err := opts.Ledger.Save(); err != nil {
		return nil, err
	}

	// 5. Tier B — batched when the target service exists (plan-conversion §2).
	tb := opts.Validator.CompileAll(ctx)
	res.TierB = &tb
	if tb.DegradeReason != "" {
		telemetry.Log(ctx).Warn("tier B validation skipped", "reason", tb.DegradeReason)
	}
	return res, nil
}

// controllerBody assembles the controller unit's prompt (rewritten branch +
// DB signatures + FML contract, never raw SQL), calls the LLM with bounded
// retries feeding trimmed validation errors, and returns the accepted body.
// The retry/budget/audit skeleton is the shared llm.RunSeam; convert's
// per-seam policy (retry through chat errors, the separate validation-
// feedback message) stays local. Scenario endpoints (sr != nil for a
// scenario slice) swap the branch view for the flattened slice (G-SCEN6).
func controllerBody(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, dbBodies map[string]dbOut, sr *scenRun, ctrlFile string) (body, rejected, prompt string, err error) {
	c := svc.ConditionOf(u.Name)
	if c == nil {
		return "", "", "", fmt.Errorf("convert: no condition for endpoint %s", u.Name)
	}
	queries, calls, err := svc.BranchCalls(c, opts.Plan)
	if err != nil {
		return "", "", "", err
	}
	view := budget.View{Source: branchSource(opts.Source, c.StartLine, c.EndLine)}
	scen := (*scenPrompt)(nil)
	axisVar := ""
	draft := ""
	var census []flow.CensusCond
	var guards []flow.MixedGuard
	var guardBind *guardBindings
	if sr != nil {
		if sc := svc.ScenarioOf(u.Name); sc != nil {
			view, err = scenarioView(opts, svc, sc, sr.tree, calls)
			if err != nil {
				return "", "", "", err
			}
			axisVar = sc.Var
			scen = scenPromptOf(sc, sr.diff)
			// The FoldMixed guards are the slice's runtime dispatch contract
			// (S7): the same specs feed the prompt facts and the guard gate,
			// with the endpoint's request-field spellings bound.
			guards = flow.MixedGuards(sc)
			guardBind = guardBindingsFor(guards, u.Name, c, svc)
			// The flattened slice IS the deterministic base for a scenario
			// endpoint — a span-limited Go draft would render dropped
			// branches, so the flowDraft seam stays off here.
		}
	}
	if scen == nil {
		view, err = budget.ReplaceQueries(view.Source, queries, calls)
		if err != nil {
			return "", "", "", fmt.Errorf("convert: query replacement for %s: %w", u.Name, err)
		}
		if opts.FlowDraft {
			draft, census = flowDraft(opts, svc, c)
		}
	}
	// Dead commented-out code never reaches the model: the ver-2.2 D2U
	// SELECT rode a /* ... **/ block into fragment prompts as live SQL and
	// every retry echoed it back (found SQL). Comment-only lines carry no
	// store calls, so gating against the stripped view is equivalent.
	view.Source = stripDeadComments(view.Source)
	// Legacy-seam rewrite (micro-chunk step 2): the FML unpack block, the
	// FML_ERR_MSG error legs, and the chk_sssn session check carry a single
	// mechanical intent each — the view carries the mapped Go (request
	// reads, err = errors.New(S-code), neutralized session check) so the
	// model copies instead of echoing the spellings the tuxedo gate
	// forbids. Runs before the scaffold strip: the error leg's S-code
	// rides an errlog line that pass deletes, so the pairing is consumed
	// while both lines are still in the view.
	rewritten, seams, _ := rewriteLegacySeams(view.Source, svc.FMLRequestMap(u.Name, c))
	if seams.Gets+seams.ErrAdds+seams.Ssn > 0 {
		view.Source = rewritten
		telemetry.Log(ctx).Info("legacy seams rewritten",
			"unit", u.Name, "fml_gets", seams.Gets, "err_legs", seams.ErrAdds, "chk_sssn", seams.Ssn)
	}
	// Transaction template: the legacy fn_*tran/tp* begin/commit calls are
	// the two ends of the Go utils.ExecTransaction wrapper — the view
	// carries the template lines verbatim so the model copies the accepted
	// shape (and `tx` is defined) instead of calling the stubs.
	if txed, n := rewriteTxTemplate(view.Source); n > 0 {
		view.Source = txed
		telemetry.Log(ctx).Info("transaction template applied", "unit", u.Name, "sites", n)
	}
	// FML probe elision: the INIT/i_ferr/Ferror32 arrays and the
	// i_err[i_loop]/FNOTPRES checks are dead once the unpack reads became
	// direct request reads — models copy them verbatim (undefined C name
	// rejects). The pass keeps each FNOTPRES fallback as a Go presence
	// guard and drops the unpack-error plumbing.
	if probed, elided := stripFMLProbes(view.Source); elided > 0 {
		view.Source = probed
		telemetry.Log(ctx).Info("fml probe scaffolding elided", "unit", u.Name, "lines", elided)
	}
	// Session-arg scrub (micro-chunk step 3): unresolved-fn calls keep the
	// middleware-owned session/buffer args in the view, and the model
	// copies them into outputs the gate rejects (undefined C names). The
	// generated stub is variadic; the accepted shape omits them. Runs on
	// the seam-rewritten view so the strcpy prologue is still present.
	if scrubbed, dropped := stripSessionArgs(view.Source, stubFnNames(opts.Plan)); dropped > 0 {
		view.Source = scrubbed
		telemetry.Log(ctx).Info("session args dropped from unresolved-fn views", "unit", u.Name, "dropped", dropped)
	}
	// Same-file helper calls (user directive 2026-09-17): the view carries
	// the generated controller method call s.<GoName>(...) with exactly the
	// args the helper signature keeps — the model copies it, and the
	// REQUIRED-CALLS gate demands it like every store call.
	if renamed, n := rewriteHelperCalls(view.Source, helperFns(opts.Plan)); n > 0 {
		view.Source = renamed
		telemetry.Log(ctx).Info("same-file helper calls rewritten", "unit", u.Name, "calls", n)
	}
	// Deterministic scaffold elision (micro-chunk step 1): pure
	// Tuxedo/Pro*C scaffold the tuxedo gate would reject in output never
	// reaches the model — fewer echo-failures, smaller prompts. Dropped
	// lines carry no store calls, so the orchestration gate is unaffected.
	view.Source, _ = stripLegacyScaffold(view.Source)
	// Prelude hoist: the leading run of context-only C declarations the
	// template owns never reaches the model either. Stops at first live
	// line; store-call lines never match.
	view.Source, _ = stripPreludeDecls(view.Source)
	// Legacy #define inlining: the view's macro names (`DUMMY_PROFILE_LIST`)
	// are undefined Go identifiers when copied; substitute the effective
	// literal values so the model keeps only names the Go body can declare.
	if inlined, n := inlineLegacyConstants(view.Source, effectiveDefines(opts.Main, c, opts.Main.Entry)); n > 0 {
		view.Source = inlined
		telemetry.Log(ctx).Info("legacy constants inlined into view", "unit", u.Name, "substitutions", n)
	}
	// Store-call capture rendering: the accepted shape assigns every result
	// (error-only → `err = …`; rows → `<name>Rows, err := …`). The view
	// carries it so models copy the captured form instead of the bare call
	// the stale-err gate rejects.
	if assigned, n := assignStoreCalls(view.Source, receiverOf(opts), storeCallShapes(svc, calls)); n > 0 {
		view.Source = assigned
		telemetry.Log(ctx).Info("store calls captured in view", "unit", u.Name, "calls", n)
	}
	// §4.7 query-replacement accounting (engine-wiring audit Tier-2: the
	// budget engine computed this on every seam call and nothing recorded
	// it). One line per unit in the run log.
	telemetry.Log(ctx).Info("sql replaced by store calls", "unit", u.Name,
		"queries", len(view.Report), "shrink_pct", fmt.Sprintf("%.0f", view.ShrinkPct()))
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Name)
	}
	sort.Strings(methods)
	contract, err := svc.ControllerPromptContext(u.Name, opts.Plan, methods)
	if err != nil {
		return "", "", "", err
	}
	prompt = buildPrompt(view, dbSignaturesFor(opts.Plan, dbBodies, methods), contract, u.Name, draft, opts.Plan.Stubs, legacyHelpers(opts.Plan, view.Source),
		legacyConstants(opts.Main, c, opts.Main.Entry), legacyErrorCodes(c), scen)
	// Oversized-endpoint routing (2026-09-13 prompt-driven; 2026-09-14
	// output-driven): chunked generation when either ceiling projects to
	// break — the assembled prompt over maxPromptTokens, or the body's
	// estimated translation over maxOutputTokens (a single call truncates
	// mid-body there: finish_reason=length, gates reject, retries burn).
	// Both triggers share the fragment path; the reason rides the run log.
	chunkReason := ""
	if opts.Budget.MaxPromptTokens > 0 && opts.Budget.Count(prompt) > opts.Budget.MaxPromptTokens {
		chunkReason = fmt.Sprintf("prompt of %d tokens exceeds the %d-token ceiling",
			opts.Budget.Count(prompt), opts.Budget.MaxPromptTokens)
	} else if reason := outputChunkReason(opts.Budget, view.Source, opts.Budget.OutputCeiling(opts.Budget.Count(prompt))); reason != "" {
		chunkReason = reason
	}
	if chunkReason != "" {
		telemetry.Log(ctx).Info("endpoint split into statement fragments", "unit", u.Name, "reason", chunkReason)
		// Statement-boundary fragments, one bounded seam call per fragment,
		// combined body through the full gates.
		body, chunkRej, cerr := controllerBodyChunked(chunkCtx{
			ctx: ctx, opts: opts, res: res, svc: svc, unit: u, db: dbBodies,
			cond: c, view: view, scen: scen, axisVar: axisVar, prompt: prompt, calls: calls,
			census: census, guards: guards, guardBind: guardBind,
		})
		if cerr != nil {
			writeConditionCensusAudit(ctx, opts, u, census, []string{cerr.Error()})
			recordConditionGaps(res, u, []string{cerr.Error()})
			return "", chunkRej, prompt, cerr
		}
		writeConditionCensusAudit(ctx, opts, u, census, nil)
		return body, "", prompt, nil
	}
	// Upgrade resume: a landed deterministic body rides as the repair
	// baseline (budget-gated) — the model edits a valid solution instead
	// of rediscovering the constraint puzzle. After the chunk routing
	// above, so fragment prompts never carry a whole-body skeleton.
	prompt = detSkeletonFor(ctx, opts, prompt, ctrlFile, u.Name, detControllerLanded)
	rej := ""
	accepted, chatCalls, notes, err := llm.RunSeam(ctx, llm.SeamInput{
		Unit: u.ID, Kind: string(u.Kind), Name: u.Name,
		Template: u.TemplateID, LLM: u.LLM, Repair: opts.RetryRepair,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		Temperature: 0.1,
		Prompt: func(prev string, attemptNotes []string) (string, []llm.Message) {
			return prompt, seamMessages(systemPrompt, prompt, prev, fixHintMethodBody, attemptNotes, opts.RetryRepair)
		},
		Extract: func(content string) string {
			return repairControllerBody(ctx, u.Name, axisVar, content)
		},
		Gate: func(body string) []string {
			verr := validateBody(opts, body)
			verr = append(verr, requiredCallErrs(view.Source, body, receiverOf(opts))...)
			verr = append(verr, controllerTuxedoErrs(body)...)
			verr = append(verr, txGateErrs(body, calls)...)
			// Condition transparency (Workstream B): the empty-if reject
			// is unconditional; the census gate runs only when the flow
			// draft is non-empty (additive/never-fatal degrade contract).
			verr = append(verr, emptyIfErrs(body)...)
			if draft != "" {
				verr = append(verr, conditionPresenceErrs(census, body)...)
			}
			// S7 guard retention: scenario slices carry no census (the
			// flattened slice replaces the draft), so the FoldMixed
			// dispatch guards need their own deterministic gate.
			verr = append(verr, scenarioGuardErrs(guards, guardBind, body)...)
			return verr
		},
		Rejected: func(payload string) { rej = payload },
	})
	res.LLMCalls += chatCalls
	opts.Ledger.Get(u.ID, string(u.Kind), u.Name).Attempts += chatCalls
	if err != nil {
		if chatCalls == 0 {
			// The prompt was rejected before any chat call: over the input
			// ceiling — a wiring problem, not a validation failure.
			return "", rej, prompt, fmt.Errorf("convert: %w — trim the mapping or raise run.maxPromptTokens", err)
		}
		writeConditionCensusAudit(ctx, opts, u, census, notes)
		recordConditionGaps(res, u, notes)
		return "", rej, prompt, fmt.Errorf("validation failed after %d attempts: %s", chatCalls, strings.Join(notes, "; "))
	}
	writeConditionCensusAudit(ctx, opts, u, census, nil)
	opts.Ledger.Set(u.ID, ledger.StatusValidated, "")
	return accepted, "", prompt, nil
}

const systemPrompt = `You emit the body of a Go controller method translated from a legacy Pro*C/Tuxedo branch.
OUTPUT: bare Go statements only — no package/imports/func wrapper/helpers/types, no prose/fences/comments (S-codes live in error text), minimal blank lines. Stay terse: fit the output ceiling.
SIGNATURE (fixed, verbatim): ctx context.Context, request *models.X, named returns data and err. Never req/resp/Response. Never shadow data/err with :=.
HEADER: a leading else-if header is the method's own condition — never emit it or a leading }. Start at the first statement under it.
INPUTS: request.<Field> exactly as defined — the only input source.
STORE: every c.store.* call appears exactly once, exact params/order, results captured to locals — or _ = name // tuxconv:TODO when unmappable; c.<Name>(...) calls are generated helpers — call as shown. No other c.* calls. Never SQL.
FIELDS: verbatim struct/row names (CToDateString is not CToDate + String); sql.NullString via its .String field.
LITERALS (Go only): "Y" never 'Y'; 0 never '\0'; == never =.
ERRORS: after every err-returning call: if err != nil { return nil, err }. Every path ends return data, err / return nil, err. A variable error message uses errors.New(msg) — fmt.Errorf takes a constant format string, never a variable.
LEGACY MAP (intent, never spelling): FML pack (Fadd32) → append shaped rows to data; tpreturn(TPSUCCESS) → return data, err; error legs → return nil + S-code; CLOSE/SETNULL/SETLEN/MEMSET/buffer-math/DEBUG userlog → drop; session prologue (Fget32/chk_sssn/tpalloc/INCLUDEs) → drop. Never emit tpreturn/tpalloc/Fadd32/Fget32/errlog/userlog/EXEC SQL/FBFR32/unsafe.
FLOW: same loops/branches/order; dead-looking branches still implemented.`

// fnHelperSystem is the fn-library translation seam's contract: one
// legacy helper becomes one complete Go method on the controller struct —
// the store is s.store, the legacy status-return contract is kept, and no
// SQL appears anywhere.
const fnHelperSystem = `You convert one legacy Pro*C/Tuxedo helper function (a fn library) into ONE Go method.
Rules:
- Emit ONLY one complete Go method declaration: func (s *Receiver) Name(...) ... — no package clause, no imports, no helper functions or types.
- The receiver and method name are fixed and provided verbatim in the prompt. When the prompt shows the complete fixed signature, declare exactly that parameter list and return type — same names, types, and order: callers are generated against it.
- The first parameter is ctx context.Context (the store calls need it); the legacy parameters follow in order: char*/varchar value params become string, long becomes int64, int stays int; a parameter the legacy writes through a pointer (an out-param) becomes a pointer parameter (*string, *int64).
- A call shown as s.<Name>(...) is a generated controller helper method — call it exactly as shown, passing your declared identifiers in that order.
- Keep the legacy int status return: -1 on the failure paths, the legacy success value otherwise. Never add a Go error return.
- Call the database exclusively through the store signatures provided — exactly the parameters each signature shows, same count and order, once per legacy SQL statement, in the legacy order. Never write SQL anywhere (no raw strings, no string literals containing SQL).
- Copy row struct field names character-exact from the definitions provided — never fuse names and never invent fields; a sql.NullString reads through its .String FIELD (no parentheses: row.X.String).
- On a store error, follow the legacy failure path: set out-params to zero values when the legacy did, then return the legacy failure status (-1).
- When the legacy logged an error code (S followed by digits) before failing, set the error-message out-param (when present) to a string containing that code; otherwise keep the code in a comment.
- Legacy tpcall sites have no outbound Go convention here — leave a // tuxconv:TODO tpcall comment at the site and return the legacy failure status.
- String comparisons use double-quoted literals: flag == "Y", never 'Y'. No logging statements. Every declared local is used. A store result you cannot otherwise use stays kept as _ = name with a // tuxconv:TODO comment noting its role — never dropped, never left unused. The method already ends with the legacy return — never fall off the end.`

// fnHelperBody assembles one fn-library helper's prompt (fn-lib mode): the
// fn's source slice with its SQL regions replaced by store calls — the
// same no-raw-SQL contract the branch path enforces — plus the DB
// signatures, the legacy error codes, and the stub list; then runs the LLM
// seam with the parse/name/required-call gates.
func fnHelperBody(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, dbBodies map[string]dbOut, fnFile string) (body, rejected, prompt string, err error) {
	h, ok := fnHelperOf(opts.Plan, u.Name)
	if !ok {
		return "", "", "", fmt.Errorf("convert: no fn record for helper %s", u.Name)
	}
	fnSource := branchSource(opts.Source, h.StartLine, h.EndLine)
	if strings.TrimSpace(fnSource) == "" {
		return "", "", "", fmt.Errorf("convert: fn helper %s has an empty source span (%d-%d)", u.Name, h.StartLine, h.EndLine)
	}
	calls, err := svc.StoreCallsFor(u.QueryIDs, opts.Plan)
	if err != nil {
		return "", "", "", err
	}
	queries := make([]*ir.Query, 0, len(u.QueryIDs))
	for _, id := range u.QueryIDs {
		orig := svc.Query(id)
		if orig == nil {
			continue
		}
		lq := *orig
		lq.StartLine -= h.StartLine - 1
		lq.EndLine -= h.StartLine - 1
		queries = append(queries, &lq)
	}
	view, err := budget.ReplaceQueries(fnSource, queries, calls)
	if err != nil {
		return "", "", "", fmt.Errorf("convert: query replacement for fn %s: %w", u.Name, err)
	}
	// Same deterministic view passes as the controller seam (a helper body
	// carries the same legacy seams — unpack probes, err legs, scaffold):
	// the model copies mapped Go instead of echoing spellings the gates
	// reject. The Fget32 request unpack map is nil here — a helper's gets
	// read the tpcall reply buffer, never the request.
	view.Source = stripDeadComments(view.Source)
	if rewritten, seams, _ := rewriteLegacySeams(view.Source, nil); seams.Gets+seams.ErrAdds+seams.Ssn > 0 {
		view.Source = rewritten
	}
	if txed, n := rewriteTxTemplate(view.Source); n > 0 {
		view.Source = txed
	}
	if probed, elided := stripFMLProbes(view.Source); elided > 0 {
		view.Source = probed
	}
	// Session-arg scrub (micro-chunk step 3): same contract as the
	// controller seam — unresolved-fn calls lose the middleware-owned
	// session/buffer args the stub signature is not meant to carry.
	if scrubbed, dropped := stripSessionArgs(view.Source, stubFnNames(opts.Plan)); dropped > 0 {
		view.Source = scrubbed
		telemetry.Log(ctx).Info("session args dropped from unresolved-fn views", "unit", u.Name, "dropped", dropped)
	}
	// Nested same-file helper calls (same rewrite as the controller seam):
	// the view carries s.<GoName>(...) so the required-call contract holds
	// inside helper bodies too.
	if renamed, n := rewriteHelperCalls(view.Source, helperFns(opts.Plan)); n > 0 {
		view.Source = renamed
		telemetry.Log(ctx).Info("same-file helper calls rewritten", "unit", u.Name, "calls", n)
	}
	view.Source, _ = stripLegacyScaffold(view.Source)
	view.Source, _ = stripPreludeDecls(view.Source)
	if inlined, n := inlineLegacyConstants(view.Source, effectiveDefines(opts.Main, &ir.Condition{EndLine: h.EndLine}, h.Name)); n > 0 {
		view.Source = inlined
	}
	// Store-call capture rendering: same accepted shape as the controller
	// seam (every result assigned) — the view carries it so the model
	// copies instead of the bare call the stale-err gate rejects.
	if assigned, n := assignStoreCalls(view.Source, receiverOf(opts), storeCallShapes(svc, calls)); n > 0 {
		view.Source = assigned
	}
	telemetry.Log(ctx).Info("sql replaced by store calls", "unit", u.Name,
		"queries", len(view.Report), "shrink_pct", fmt.Sprintf("%.0f", view.ShrinkPct()))
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Name)
	}
	sort.Strings(methods)
	structName := common.LowerFirst(svc.Mapping.Service) + "Controller"
	prompt = buildFnPrompt(h, structName, view.Source,
		dbSignaturesFor(opts.Plan, dbBodies, methods), svc.FnRowContracts(opts.Plan, u.QueryIDs),
		fnErrorCodes(view.Source), legacyHelpers(opts.Plan, view.Source), opts.Plan.Stubs)
	// Upgrade resume: same repair baseline as the controller seam.
	prompt = detSkeletonFor(ctx, opts, prompt, fnFile, u.Name, detFnHelperLanded)
	rej := ""
	accepted, chatCalls, notes, err := llm.RunSeam(ctx, llm.SeamInput{
		Unit: u.ID, Kind: string(u.Kind), Name: u.Name,
		Template: u.TemplateID, LLM: u.LLM, Repair: opts.RetryRepair,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		Temperature: 0.1,
		Prompt: func(prev string, attemptNotes []string) (string, []llm.Message) {
			return prompt, seamMessages(fnHelperSystem, prompt, prev, fixHintMethod, attemptNotes, opts.RetryRepair)
		},
		Extract: cleanBody,
		Gate: func(body string) []string {
			return fnHelperGate(body, h, structName, view.Source, receiverOf(opts))
		},
		Rejected: func(payload string) { rej = payload },
	})
	res.LLMCalls += chatCalls
	if err != nil {
		if chatCalls == 0 {
			return "", rej, prompt, fmt.Errorf("convert: %w — raise run.maxPromptTokens", err)
		}
		return "", rej, prompt, fmt.Errorf("validation failed after %d attempts: %s", chatCalls, strings.Join(notes, "; "))
	}
	opts.Ledger.Get(u.ID, string(u.Kind), u.Name).Attempts += chatCalls
	return accepted, "", prompt, nil
}

// fnHelperGate validates one seam attempt: the fixed receiver/name shape,
// the prescribed parameter list (callers were generated against it), a
// parse-clean complete method, and the required-call contract.
func fnHelperGate(body string, h plan.FnHelper, structName, view, receiver string) []string {
	var errs []string
	sig := "func (" + common.GoCtrlRecv + " *" + structName + ") " + h.GoName + "("
	if !strings.Contains(body, sig) {
		errs = append(errs, "the method must be declared with the fixed receiver and name: "+sig+"...) — emit one complete Go method, nothing else")
	} else if want := fnHelperDecl(h, structName); h.Fixed && (len(h.Params) > 0 || h.Return != "") {
		if got, ok := parsedFnSignature(body); ok && got != normalizeSignature(want) {
			errs = append(errs, "signature contract: the method must be declared exactly as "+want+" — callers are generated against this parameter list")
		}
	}
	if _, ferr := goast.Emit("convert: fn helper", "package controller\n\n"+body); ferr != nil {
		errs = append(errs, validate.TrimGoErrors(ferr.Error())...)
	}
	return append(errs, requiredCallErrs(view, body, receiver)...)
}

// fnHelperDecl renders the prescribed method declaration for one helper:
// the context parameter, the derived legacy parameters in order, and the
// legacy status return (empty for void).
func fnHelperDecl(h plan.FnHelper, structName string) string {
	parts := make([]string, 0, len(h.Params)+1)
	parts = append(parts, common.GoCtx+" context.Context")
	for _, p := range h.Params {
		parts = append(parts, p.Name+" "+p.Type)
	}
	decl := "func (" + common.GoCtrlRecv + " *" + structName + ") " + h.GoName + "(" + strings.Join(parts, ", ") + ")"
	if h.Return != "" {
		decl += " " + h.Return
	}
	return decl
}

// parsedFnSignature renders the first method declaration in the body as a
// whitespace-normalized signature string ("" when the body does not parse
// or declares no method — parse errors are reported by the caller's gate).
func parsedFnSignature(body string) (string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "helper.go", "package controller\n\n"+body, 0)
	if err != nil {
		return "", false
	}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil {
			continue
		}
		var buf bytes.Buffer
		if perr := printer.Fprint(&buf, fset, fd); perr != nil {
			return "", false
		}
		got := buf.String()
		if i := strings.IndexByte(got, '{'); i >= 0 {
			got = got[:i]
		}
		return normalizeSignature(got), true
	}
	return "", false
}

// normalizeSignature collapses whitespace so printer output and the
// prescribed declaration compare structurally.
func normalizeSignature(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// buildFnPrompt assembles the fn helper's deterministic context: the
// prescribed signature, the SQL-replaced fn view, the DB contract for its
// queries, the row struct definitions, the required-call list, the legacy
// error codes, nested helper calls, and the stub section.
func buildFnPrompt(h plan.FnHelper, structName, view, dbContract, rowContracts string, errCodes, helpers []string, stubs []plan.Stub) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Legacy helper function — emit the complete Go method (s *%s) %s.\n\n", structName, h.GoName)
	if h.Fixed && (len(h.Params) > 0 || h.Return != "") {
		sb.WriteString("Signature (fixed, verbatim): " + fnHelperDecl(h, structName) + "\n" +
			"Declare exactly this method — same parameter names, types, and order: callers are generated against this signature.\n\n")
	}
	sb.WriteString("Legacy function, with every SQL block already replaced by its store call:\n\n" + view + "\n\n")
	if dbContract != "" {
		writeDBContract(&sb, dbContract)
	}
	if rowContracts != "" {
		sb.WriteString("Row struct definitions the store calls return — copy field names character-exact, never invent fields; a sql.NullString reads through its .String FIELD (no parentheses: row.X.String):\n" + rowContracts + "\n\n")
	}
	if calls := requiredCalls(view, common.GoStoreCallPrefix()); len(calls) > 0 {
		sb.WriteString("REQUIRED CALLS — every one must appear in the method, in the legacy order: " +
			strings.Join(calls, ", ") + "\n\n")
	}
	if len(errCodes) > 0 {
		sb.WriteString("Legacy error codes — retain them (error-message out-param text, else a comment): " +
			strings.Join(errCodes, ", ") + "\n\n")
	}
	if len(helpers) > 0 {
		sb.WriteString("Same-file helper calls in this function — generated controller methods: call each as shown, with the mapped arguments in order:\n")
		for _, hl := range helpers {
			sb.WriteString("  - " + hl + "\n")
		}
		sb.WriteString("\n")
	}
	if len(stubs) > 0 {
		sb.WriteString("Stubbed helpers — legacy fns with no source in the corpus. Each has a generated package-level stub (variadic args, int return, panics at runtime); call the RIGHT stub for each legacy fn and pass only identifiers your method declares (declare zero-value locals for C-only names; out-pointers become &local):\n")
		writeStubEntries(&sb, stubs)
		sb.WriteString("\n")
	}
	return sb.String()
}

// fnErrCodeRe matches the legacy error codes a fn's source carries
// ("S31240" shapes) — the FML-op census has no conditions to read here.
var fnErrCodeRe = regexp.MustCompile(`\bS\d{5}\b`)

// fnErrorCodes lists the distinct legacy error codes in the fn view.
func fnErrorCodes(view string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range fnErrCodeRe.FindAllString(view, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// fnHelperOf finds the fn record a KindFnHelper unit renders.
func fnHelperOf(p *plan.Plan, goName string) (plan.FnHelper, bool) {
	for _, h := range p.FnHelpers {
		if h.GoName == goName {
			return h, true
		}
	}
	return plan.FnHelper{}, false
}

// appendFnHelper appends the accepted fn helper method to
// controller/fns.go, creating the deterministic scaffold on first use — in
// fn-lib mode the controller struct (store field) plus constructor, while
// a service run's struct + constructor already live in
// controller/interface.go (the controller-interface unit) and fns.go must
// not redeclare them. Imports are derived from the file text — only what
// it references — so the file never carries unused imports for Tier B.
func appendFnHelper(ctx context.Context, opts Options, res *Result, svc *gen.Service, path, name, body string) error {
	_, err := writeFnArtifact(ctx, opts, res, svc, path, name, body)
	return err
}

// writeFnArtifact merges one complete helper method into controller/fns.go
// and reports whether it wrote: a rejected placeholder for the same method
// is replaced, a live method with that name wins, and the scaffold (struct
// + constructor) is created on first use only for fn-lib plans — a service
// run's struct lives in controller/interface.go and fns.go must not
// redeclare it. Imports are derived from the file text so the file never
// carries unused imports for Tier B.
func writeFnArtifact(ctx context.Context, opts Options, res *Result, svc *gen.Service, path, name, body string) (bool, error) {
	structName := common.LowerFirst(svc.Mapping.Service) + "Controller"
	dbImport := svc.Mapping.ImportPath("db")
	dbQual := dbImport[strings.LastIndexByte(dbImport, '/')+1:]
	storeIface := common.Export(svc.Mapping.Service) + "Store"
	var merged string
	if _, statErr := os.Stat(path); statErr == nil {
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return false, rerr
		}
		src := string(existing)
		if stripped, ok := stripRejectedBlock(src, name); ok {
			src = stripped
		}
		if gen.IsDeterministicFnHelper(src, name) {
			// LLM upgrade: same replace-instead-of-stack contract as the
			// controller path.
			if stripped, ok := stripMethodByName(src, name); ok {
				src = stripped
			}
		}
		if hasLiveMethod(src, name) {
			return false, nil
		}
		merged = src + "\n" + strings.TrimRight(body, "\n") + "\n"
	} else {
		merged = "package controller\n\n"
		if opts.Plan == nil || opts.Plan.FnLib {
			merged += "type " + structName + " struct {\n\tstore " + dbQual + "." + storeIface + "\n}\n\n" +
				"func New" + common.Export(svc.Mapping.Service) + "Controller(store " + dbQual + "." + storeIface + ") *" + structName + " {\n" +
				"\treturn &" + structName + "{store: store}\n}\n\n"
		}
		merged += strings.TrimRight(body, "\n") + "\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
	}
	formatted, ferr := goast.Emit("convert: fn helpers file", merged)
	if ferr != nil {
		return false, ferr
	}
	if err := os.WriteFile(path, []byte(formatted), 0o644); err != nil {
		return false, err
	}
	if imports := deriveFnImports(formatted, dbImport, svc.ModelsPkg); len(imports) > 0 {
		for i, imp := range imports {
			// deriveFnImports flags the tx wrapper without the module
			// path (text-only view) — qualify it to this service's utils.
			if imp == "utils:ExecTransaction" {
				imports[i] = svc.Module + "/pkg/utils"
			}
		}
		if err := goast.AddImports(path, imports...); err != nil {
			return false, err
		}
		if final, rerr := os.ReadFile(path); rerr == nil {
			if norm, nerr := goast.Emit("convert: fn helpers file", string(final)); nerr == nil {
				if werr := os.WriteFile(path, []byte(norm), 0o644); werr != nil {
					return false, werr
				}
			}
		}
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return false, err
	}
	res.Files = append(res.Files, path)
	return true, nil
}

// appendPlaceholderFnHelper writes a helper method that carries the prescribed
// signature and does nothing, so a caller generated against that signature
// still compiles.
//
// It exists because the deterministic path had no equivalent of
// appendRejectedFnHelper's placeholder. A deterministic helper that failed its
// gates was simply skipped, and since the caller in tux.go is generated against
// the signature rather than against what survived, the emitted package stopped
// compiling with no marker anywhere explaining why. Silence here is the
// failure mode; a placeholder that says so is not.
//
// The body returns the legacy failure status rather than panicking: this is a
// deterministic gap the LLM resume is expected to fill, not a rejected
// human-authored attempt, and a panic would take the whole service down on a
// path the operator believes compiles.
func appendPlaceholderFnHelper(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, path string, cause error) error {
	h, ok := fnHelperOf(opts.Plan, u.Name)
	if !ok || !h.Fixed {
		// Fn-lib helpers have no in-tree callers, so a placeholder with a
		// prescribed signature buys nothing; keep the legacy skip.
		return nil
	}
	structName := common.LowerFirst(svc.Mapping.Service) + "Controller"
	method := placeholderFnHelperMethod(h, structName, cause)
	_, err := writeFnArtifact(ctx, opts, res, svc, path, h.GoName, method)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("gated-out helper kept as a compiling placeholder",
		"unit", u.Name, "path", path, "reason", cause.Error())
	return nil
}

// placeholderFnHelperMethod renders the placeholder's Go method. It is
// separated from the file write so its shape — and especially that it parses
// — is testable without a pipeline.
func placeholderFnHelperMethod(h plan.FnHelper, structName string, cause error) string {
	var b strings.Builder
	b.WriteString("\t// tuxconv:TODO " + gen.NoStoreCallsMark + ": " + h.GoName +
		" — the deterministic body failed its gates and was not emitted.\n")
	// The reason is a goast parse error, so it is already newline-free by
	// construction — but oneLine keeps that a property of the renderer
	// rather than a property of its current caller.
	b.WriteString("\t// Reason: " + oneLine(cause.Error()) + "\n")
	// Zero the out-params so a caller that passes addresses gets defined
	// values rather than whatever it held, matching the int-status contract
	// the real bodies implement on their error paths.
	for _, p := range h.Params {
		if !strings.HasPrefix(strings.TrimSpace(p.Type), "*") {
			continue
		}
		b.WriteString("\tif " + p.Name + " != nil {\n\t\t*" + p.Name + " = 0\n\t}\n")
	}
	if h.Return == "" {
		b.WriteString("\treturn\n")
	} else {
		b.WriteString("\treturn -1\n")
	}
	return fnHelperDecl(h, structName) + " {\n" + b.String() + "}\n"
}

// oneLine collapses a message to a single line so it can sit in a Go comment
// without breaking the file's parse.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// appendRejectedFnHelper keeps a failed service helper's last rejected
// output visible the same way the controller path does: commented inside a
// panicking placeholder with the prescribed signature (which callers were
// generated against). Fn-lib helpers keep their legacy posture — no
// callers exist, so nothing is gained by a placeholder.
func appendRejectedFnHelper(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, path, rejected string, cause error) {
	h, ok := fnHelperOf(opts.Plan, u.Name)
	if !ok || !h.Fixed || strings.TrimSpace(rejected) == "" {
		return
	}
	structName := common.LowerFirst(svc.Mapping.Service) + "Controller"
	body := rejectedBodyComment(h.GoName, rejected, cause) +
		"\n\tpanic(\"tuxconv: " + h.GoName + " rejected — the commented output above is the last rejected attempt; fix the view or rerun\")"
	method := fnHelperDecl(h, structName) + " {\n" + body + "\n}\n"
	wrote, err := writeFnArtifact(ctx, opts, res, svc, path, h.GoName, rejectedBlock(h.GoName, method))
	if err != nil {
		telemetry.Log(ctx).Warn("rejected helper output not kept", "unit", u.Name, "error", err.Error())
		return
	}
	if !wrote {
		return
	}
	telemetry.Log(ctx).Info("rejected helper output kept commented", "unit", u.Name)
	res.Warnings = append(res.Warnings, u.Name+" failed validation — the last rejected output is kept commented in fns.go (panicking placeholder)")
}

// deriveFnImports lists the import paths the fn file's text references:
// the db package (only when the struct's store field is declared or a body
// names a db type — a service run's fns.go has neither, so the import stays
// out), context, models, fmt, errors, and the tx/time trio deterministic
// no-llm helpers may reference (utils/sqlx for the ExecTransaction wrapper,
// time for time.Time zeroes).
func deriveFnImports(text, dbPkg, modelsPkg string) []string {
	var out []string
	dbQual := dbPkg[strings.LastIndexByte(dbPkg, '/')+1:]
	if strings.Contains(text, dbQual+".") {
		out = append(out, dbPkg)
	}
	if strings.Contains(text, "context.Context") {
		out = append(out, "context")
	}
	if modelsPkg != "" && strings.Contains(text, "models.") {
		out = append(out, modelsPkg)
	}
	if strings.Contains(text, "fmt.") {
		out = append(out, "fmt")
	}
	if strings.Contains(text, "errors.") {
		out = append(out, "errors")
	}
	if strings.Contains(text, "time.") {
		out = append(out, "time")
	}
	if strings.Contains(text, "ExecTransaction(") {
		// The module-qualified utils path rides the caller: deriveFnImports
		// sees only text, so flag it and let writeFnArtifact qualify.
		out = append(out, "utils:ExecTransaction")
	}
	if strings.Contains(text, "sqlx.") {
		out = append(out, "github.com/jmoiron/sqlx")
	}
	return out
}

// flowDraft renders the deterministic transpilation draft for one endpoint's
// condition slice (PRD-2026-09-10 FLW-D7): the entry function's flow tree
// span-limited to the condition, with plan-backed store calls — plus the
// transparency census collected on the same walk (zero divergence). Any
// failure degrades to an empty draft with no census — the seam is additive,
// never fatal.
func flowDraft(opts Options, svc *gen.Service, c *ir.Condition) (string, []flow.CensusCond) {
	if opts.Main == nil || opts.Source == "" {
		return "", nil
	}
	tree := flowTreeOf(opts.Source, opts.Main)
	if len(tree.Root) == 0 {
		return "", nil
	}
	_, calls, err := svc.BranchCalls(c, opts.Plan)
	if err != nil {
		calls = nil // placeholder store calls, never a run failure
	}
	out := flow.RenderSpan(tree, planResolver{svc: svc, calls: calls}, c.StartLine, c.EndLine, 2)
	return strings.TrimRight(out.Body, "\n"), out.Conditions
}

// flowTreeOf re-derives the entry function's flow tree via the shared
// flow.TreeFor (fragments wrap via ScanFragment there); any failure yields
// an empty tree, never a panic.
func flowTreeOf(src string, f *ir.File) *flow.Tree {
	t, err := flow.TreeFor(src, f)
	if err != nil {
		return &flow.Tree{}
	}
	return t
}

// scenRunOf builds the run-level scenario context (G-SCEN6): the entry's
// flow tree plus the cross-scenario diff. Any failure degrades to nil — the
// seam is additive, never fatal.
func scenRunOf(opts Options) *scenRun {
	if opts.Main == nil || opts.Source == "" {
		return nil
	}
	tree := flowTreeOf(opts.Source, opts.Main)
	if len(tree.Root) == 0 {
		return nil
	}
	axis := tree.DispatchAxisFor([]byte(opts.Source))
	if axis == nil {
		return nil
	}
	scens := flow.Scenarios(tree, axis)
	return &scenRun{tree: tree, diff: flow.DiffScenarios(opts.Main.Entry, scens)}
}

// scenarioView assembles a scenario endpoint's prompt base (G-SCEN6): the
// flattened kept-lines slice with every reachable query's SQL region
// replaced by its resolved store call — the same no-raw-SQL contract the
// branch path enforces, over the folded slice instead of a contiguous span.
func scenarioView(opts Options, svc *gen.Service, sc *flow.Scenario, tree *flow.Tree, calls map[string]budget.DBCall) (budget.View, error) {
	text, regions := flow.ScenarioSource(sc, tree, []byte(opts.Source))
	queries := make([]*ir.Query, 0, len(regions))
	for id, span := range regions {
		orig := svc.Query(id)
		if orig == nil {
			return budget.View{}, fmt.Errorf("convert: scenario query %s has no IR record", id)
		}
		q := *orig
		q.StartLine, q.EndLine = span[0], span[1]
		queries = append(queries, &q)
	}
	return budget.ReplaceQueries(text, queries, calls)
}

// scenPromptOf renders one endpoint's scenario prompt additions (G-SCEN6):
// the slice context, a compact shared-block note (the full report lives in
// scenarios/<entry>.shared.md), and the legacy transaction facts (SCEN-D8
// evidence) with the ExecTransaction wrap pattern.
func scenPromptOf(sc *flow.Scenario, diff *flow.ScenarioDiff) *scenPrompt {
	p := &scenPrompt{Key: sc.Key, Filter: sc.Filter, Matched: sc.FilterMatched, Pruned: sc.FilterPruned, Residue: sc.Residue}
	for _, g := range flow.MixedGuards(sc) {
		p.Guards = append(p.Guards, fmt.Sprintf("L%d: %s", g.Line, g.Cond))
	}
	if diff != nil {
		samples := diff.Shared
		if len(samples) > sharedPromptSamples {
			samples = samples[:sharedPromptSamples]
		}
		var extents []string
		for _, b := range samples {
			extents = append(extents, fmt.Sprintf("%d-%d", b.Start, b.End))
		}
		if len(diff.Shared) > 0 {
			p.Shared = append(p.Shared,
				fmt.Sprintf("%d block(s) are byte-identical across the sibling scenarios (e.g. lines %s) — shared init/response shaping, already present in this slice",
					len(diff.Shared), strings.Join(extents, ", ")))
		}
	}
	var txIDs []string
	for _, q := range sc.Queries {
		if q.DML && q.Tx {
			txIDs = append(txIDs, q.ID)
		}
	}
	if len(txIDs) > 0 {
		for _, s := range sc.TxSpans {
			p.TxNotes = append(p.TxNotes, fmt.Sprintf("legacy tx span %d→%d (%s) — the view carries the utils.ExecTransaction open/close template; the wrapper owns begin/commit", s.BeginLine, s.CommitLine, txKindName(s.Kind)))
		}
		p.TxNotes = append(p.TxNotes,
			fmt.Sprintf("DML queries %s ride those transactions and their store calls take tx — wrap each call in utils.ExecTransaction(ctx, c.store.GetDB(), func(tx *sqlx.Tx) error { ...; return nil }); the wrapper owns begin/commit/rollback",
				strings.Join(txIDs, ", ")))
	}
	if len(sc.TxAborts) > 0 {
		lines := make([]string, 0, len(sc.TxAborts))
		for _, l := range sc.TxAborts {
			lines = append(lines, strconv.Itoa(l))
		}
		p.TxNotes = append(p.TxNotes,
			fmt.Sprintf("legacy abort/rollback calls (lines %s) are elided from the slice — never emit a stub call for them: translate their error paths as returning the error, and utils.ExecTransaction rolls the transaction back when the closure errors",
				strings.Join(lines, ", ")))
	}
	return p
}

// sharedPromptSamples caps the shared-block examples a prompt carries (the
// count stays exact; only the sample list is bounded).
const sharedPromptSamples = 5

// txKindName renders a tx span's kind for the prompt evidence line.
func txKindName(kind string) string {
	switch kind {
	case "tp":
		return "tpbegin→tpcommit"
	case "helper":
		return "fn_equ_begintran→committran"
	}
	return kind
}

// planResolver adapts the plan's store calls and row names to the flow
// renderer's Resolver (FLW-D5).
type planResolver struct {
	svc   *gen.Service
	calls map[string]budget.DBCall
}

func (r planResolver) StoreCall(qid string) (string, bool) {
	c, ok := r.calls[qid]
	if !ok {
		return "", false
	}
	// Build straight into the result instead of appending CtxName onto a
	// fresh []string and handing it to strings.Join — that allocated a
	// slice header array plus a join buffer on every rendered store call.
	var sb strings.Builder
	sb.Grow(len(c.Receiver) + len(c.Name) + len(c.CtxName) + 2)
	sb.WriteString(c.Receiver)
	sb.WriteByte('.')
	sb.WriteString(c.Name)
	sb.WriteByte('(')
	sb.WriteString(c.CtxName)
	for i, a := range c.Args {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(a)
	}
	sb.WriteByte(')')
	return sb.String(), true
}

func (r planResolver) RowType(qid string) (string, bool) {
	c, ok := r.calls[qid]
	if !ok {
		return "", false
	}
	return "*models." + r.svc.RowName(qid, c.Name), true
}

// buildPrompt assembles the deterministic context: rewritten branch view,
// DB contract, the fixed signature + verbatim struct definitions, the
// required-call contract (every store call the view shows is a must-call —
// a dropped sub-flow is a gate rejection, never a silent gap), the
// stubbed-helper section when the plan stubbed unresolved fns, and — when
// rendered — the deterministic flow draft the LLM enhances.
// fnCallRe extracts the legacy fn_* calls a branch view shows (the
// resolved-helper prompt mapping; case-insensitive, fn-name-shaped only).
var fnCallRe = regexp.MustCompile(`(?i)\b(fn_[a-z0-9_]+)\s*\(`)

// legacyHelpers maps the legacy fn_* calls a branch view shows to their Go
// equivalents: SQL-bearing fns name the db method that owns their lookup
// (from the plan's fn-namespaced units), same-file helpers name the
// generated method on this controller (controller/fns.go) with its exact
// signature, pure-logic fns are named for inlining. Unresolved fns ride the
// stubs section instead. One line per fn, deterministic order.
func legacyHelpers(p *plan.Plan, view string) []string {
	fnMethod := map[string]string{}
	for _, u := range p.Units {
		if len(u.QueryIDs) == 0 || !strings.HasPrefix(u.QueryIDs[0], "fn_") {
			continue
		}
		ns := u.QueryIDs[0]
		fn := ns[:strings.LastIndex(ns, ":")]
		fnMethod[fn] = u.Name
	}
	pureLogic := map[string]bool{}
	for _, d := range p.Dropped {
		if name, _, ok := strings.Cut(d, " "); ok && strings.HasPrefix(name, "fn_") {
			pureLogic[name] = true
		}
	}
	stubbed := map[string]bool{}
	for _, st := range p.Stubs {
		stubbed[st.Fn] = true
	}
	var out []string
	seen := map[string]bool{}
	// Same-file helpers first (deterministic plan order): the call-site
	// rewrite turns the view's call into s.<GoName>(...), so presence
	// accepts either spelling.
	for _, h := range p.FnHelpers {
		fn := strings.ToLower(h.Name)
		if seen[fn] {
			continue
		}
		if !strings.Contains(view, h.Name) && !strings.Contains(view, common.GoCtrlRecv+"."+h.GoName+"(") {
			continue
		}
		seen[fn] = true
		out = append(out, helperCallLine(h))
	}
	for _, m := range fnCallRe.FindAllStringSubmatch(view, -1) {
		fn := strings.ToLower(m[1])
		if seen[fn] || stubbed[fn] {
			continue
		}
		seen[fn] = true
		if method, ok := fnMethod[fn]; ok {
			out = append(out, fmt.Sprintf("%s(...) → %s%s(...) — its SQL lookup is this store method (see the DB contract); translate the fn's remaining logic inline, declaring any locals you need", fn, common.GoStoreCallPrefix(), method))
			continue
		}
		if pureLogic[fn] {
			out = append(out, fmt.Sprintf("%s(...) → pure logic resolved in the corpus — inline its behavior from the call-site usage", fn))
			continue
		}
		out = append(out, fmt.Sprintf("%s(...) → session/error plumbing, middleware-owned — drop the call", fn))
	}
	return out
}

// helperCallLine renders the prompt line for one same-file helper: the
// legacy call maps to the generated controller method, with the exact
// deterministic signature both seams were given.
func helperCallLine(h plan.FnHelper) string {
	parts := make([]string, 0, len(h.Params)+1)
	parts = append(parts, common.GoCtx+" context.Context")
	for _, p := range h.Params {
		parts = append(parts, p.Name+" "+p.Type)
	}
	sig := common.GoCtrlRecv + "." + h.GoName + "(" + strings.Join(parts, ", ") + ")"
	if h.Return != "" {
		sig += " " + h.Return
	}
	return fmt.Sprintf("%s(...) → %s — generated controller method (controller/fns.go): call it with the mapped arguments in order, middleware-owned args dropped; the legacy status return is kept", h.Name, sig)
}

// stubFnNames collects the unresolved fns' legacy spellings (plan.Stub.Fn)
// — the callees stripSessionArgs scrubs middleware-owned args from.
func stubFnNames(p *plan.Plan) map[string]bool {
	if len(p.Stubs) == 0 {
		return nil
	}
	names := make(map[string]bool, len(p.Stubs))
	for _, st := range p.Stubs {
		names[st.Fn] = true
	}
	return names
}

// helperFns indexes the plan's same-file helpers by their legacy fn name
// (the spelling the views carry before the call-site rewrite).
func helperFns(p *plan.Plan) map[string]plan.FnHelper {
	if p == nil || len(p.FnHelpers) == 0 {
		return nil
	}
	m := make(map[string]plan.FnHelper, len(p.FnHelpers))
	for _, h := range p.FnHelpers {
		m[h.Name] = h
	}
	return m
}

func buildPrompt(view budget.View, dbContract, contract, endpoint, draft string, stubs []plan.Stub, helpers, constants, errCodes []string, scen *scenPrompt) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Endpoint: %s\n\n", endpoint)
	writePromptFacts(&sb, viewWording, scen, true, view.Source, common.GoStoreCallPrefix(), dbContract, contract, helpers, constants, errCodes, stubs)
	sb.WriteString("Legacy branch (SQL already replaced by store calls):\n\n" + view.Source + "\n")
	if draft != "" {
		sb.WriteString("\nDeterministic flow draft (parsed from the control flow — verify it, fix field mappings, keep the flow and every store call):\n" + draft + "\n")
	}
	return sb.String()
}

// legacyConstants renders the #define constants effective at the endpoint's
// span end (PRD-2026-09-10 defines pass, G-DEF6): file-scope plus entry-
// function defines, #undef tombstones honored, macros excluded. The
// unresolvable-in-Go compound values ride here verbatim so the LLM inlines
// them without guessing.
func legacyConstants(f *ir.File, c *ir.Condition, entry string) []string {
	effective := effectiveDefines(f, c, entry)
	if len(effective) == 0 {
		return nil
	}
	out := make([]string, 0, len(effective))
	for name, value := range effective {
		out = append(out, name+" = "+value)
	}
	sort.Strings(out)
	return out
}

// effectiveDefines resolves the #define names effective at the endpoint's
// span end (file-scope plus entry-function defines, #undef tombstones
// honored, macros excluded) — the single source both the prompt's constants
// section and the view's constant inlining read.
func effectiveDefines(f *ir.File, c *ir.Condition, entry string) map[string]string {
	if f == nil || c == nil {
		return nil
	}
	effective := map[string]string{}
	for _, d := range f.Defines {
		if d.Line > c.EndLine || (d.Function != "" && d.Function != entry) {
			continue
		}
		if d.Undef {
			delete(effective, d.Name)
			continue
		}
		if d.Macro {
			continue
		}
		effective[d.Name] = d.Value
	}
	return effective
}

// inlineLegacyConstants substitutes the endpoint's effective #define names in
// a view with their literal values. Models otherwise copy the macro names
// (`c_rqst_typ == DUMMY_PROFILE_LIST`) and the undeclared-identifier gate
// rejects the body; the prompt's constants section still lists each name for
// provenance. Returns the rewritten view and the substitution count.
func inlineLegacyConstants(src string, defines map[string]string) (string, int) {
	if len(defines) == 0 {
		return src, 0
	}
	names := make([]string, 0, len(defines))
	res := make(map[string]*regexp.Regexp, len(defines))
	for name, value := range defines {
		if name == "" || value == "" {
			continue
		}
		names = append(names, name)
		res[name] = common.CachedRegexp(`\b` + regexp.QuoteMeta(name) + `\b`)
	}
	if len(names) == 0 {
		return src, 0
	}
	sort.Strings(names)
	count := 0
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		for _, name := range names {
			re := res[name]
			if !strings.Contains(line, name) {
				continue
			}
			// One pass instead of Match + FindAllStringIndex +
			// ReplaceAll: the pattern is `\b<name>\b`, so every match
			// equals name and the replacement is always defines[name].
			// Counting inside the callback drops the []int the counting
			// pass allocated and a whole re-scan of the line.
			// ReplaceAllStringFunc substitutes the return value
			// literally (no Expand), so this is the old
			// ReplaceAllLiteralString, not a $-interpolating replace.
			n := 0
			repl := defines[name]
			line = re.ReplaceAllStringFunc(line, func(string) string {
				n++
				return repl
			})
			if n == 0 {
				continue
			}
			count += n
		}
		lines[i] = line
	}
	if count == 0 {
		return src, 0
	}
	return strings.Join(lines, "\n"), count
}

// legacyErrorCodes lists the distinct legacy error-message codes the
// endpoint's FML ops ship (G-DEF6) — the deterministic retention rule.
func legacyErrorCodes(c *ir.Condition) []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range c.FmlOps {
		if op.Code == "" || seen[op.Code] {
			continue
		}
		seen[op.Code] = true
		out = append(out, op.Code)
	}
	return out
}

// storeCallRe extracts the store calls the rewritten view requires; the
// receiver prefix is per-profile (P1: goitem's "s.store." is the reference).
func storeCallRe(receiver string) *regexp.Regexp {
	return common.CachedRegexp(regexp.QuoteMeta(receiver) + `([A-Za-z0-9_]+)\(`)
}

// helperCallRe matches a bare s.<Name>( selector call: the same-file
// helper methods (controller/fns.go) the view shows after the call-site
// rewrite. Store-receiver calls never match — their selector has a dot
// before the method name.
var helperCallRe = regexp.MustCompile(`\b` + common.GoCtrlRecv + `\.([A-Za-z0-9_]+)\(`)

// requiredCalls lists the unique calls the view requires, in order: every
// store method with the profile's receiver prefix, plus every bare
// s.<Name>( helper call. The body must contain each one.
func requiredCalls(view string, receiver string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(call string) {
		if call == "" || seen[call] {
			return
		}
		seen[call] = true
		out = append(out, call)
	}
	for _, m := range storeCallRe(receiver).FindAllStringSubmatch(view, -1) {
		add(receiver + m[1])
	}
	for _, m := range helperCallRe.FindAllStringSubmatch(view, -1) {
		add(common.GoCtrlRecv + "." + m[1])
	}
	return out
}

// requiredCallErrs is the orchestration-contract gate: a body that drops a
// store call the view shows is rejected and the omission fed back.
func requiredCallErrs(view, body string, receiver string) []string {
	var errs []string
	for _, call := range requiredCalls(view, receiver) {
		if !strings.Contains(body, call+"(") {
			errs = append(errs, "orchestration contract: "+call+" appears in the branch view but is missing from the body — every REQUIRED CALL is mandatory under its view condition")
		}
	}
	return errs
}

// txGateErrs enforces the transaction contract the prompt instructs: when
// the unit's store calls take tx, the body wraps the flow in
// utils.ExecTransaction and each tx-variant call carries the tx handle.
// String-level by design (pre-Tier-B): a parse-only gate cannot see
// undefined symbols, and Tier B is skipped on syntax-only runs — without
// this check a body that never opens the transaction passes as
// transactional. Guard-ordering (call inside the closure) stays Tier B's
// type-check job when the target service is wired.
func txGateErrs(body string, calls map[string]budget.DBCall) []string {
	txCalls := map[string]budget.DBCall{}
	for _, call := range calls {
		if call.Tx != "" {
			txCalls[call.Name] = call
		}
	}
	if len(txCalls) == 0 {
		return nil
	}
	names := make([]string, 0, len(txCalls))
	for name := range txCalls {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	if !strings.Contains(body, "ExecTransaction(") {
		errs = append(errs, "transaction contract: the branch's DML calls take tx — the body must wrap the flow in utils.ExecTransaction(ctx, c.store.GetDB(), func(tx *sqlx.Tx) error { ...; return nil }); the wrapper owns begin/commit/rollback")
	}
	for _, name := range names {
		call := txCalls[name]
		if !strings.Contains(body, name+"("+call.CtxName+", "+call.Tx) {
			errs = append(errs, "transaction contract: "+name+" must be called inside the ExecTransaction closure with the tx handle: "+call.Receiver+"."+name+"("+call.CtxName+", "+call.Tx+", ...)")
		}
	}
	return errs
}

// bareStoreCall strips a store receiver prefix (s.store.GetDateRange →
// GetDateRange); bare names pass through unchanged.
func bareStoreCall(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// tuxedoCallRe matches legacy Tuxedo/FML runtime calls — bare or
// s.-prefixed transliterations — that must never appear in a Go controller
// body (audit 2026-09-16: fragment outputs transliterated tpalloc/errlog/
// Fadd32/tpreturn with s. receivers and passed the parse gates).
var tuxedoCallRe = regexp.MustCompile(`(?i)(?:s\.)?\b(tpreturn|tpalloc|tpfree|tprealloc|Fadd32|Fget32|Ferror32|Fsizeof32|Funused32|Finit32|Fneeded32|errlog|userlog|chk_sssn)\s*\(`)

// tuxedoRefRe matches legacy runtime references (directives, buffer types,
// indicator macros, cursor state) with no Go equivalent in a controller.
var tuxedoRefRe = regexp.MustCompile(`(?i)(EXEC\s+SQL|FBFR32|SQLCODE|SQLCA|SETNULL\s*\(|SETLEN\s*\(|MEMSET\s*\(|unsafe\.Pointer|tpacall\s*\(|tpcall\s*\()`)

// controllerTuxedoErrs rejects transliterated Tuxedo/FML runtime usage in a
// controller body: the view still shows the legacy runtime calls, but the
// system prompt maps them to response shaping / returns / nothing — their
// spelling must never reach the body. One note per category keeps retry
// feedback compact.
func controllerTuxedoErrs(body string) []string {
	seenCall := map[string]bool{}
	var calls []string
	for _, m := range tuxedoCallRe.FindAllStringSubmatch(body, -1) {
		name := strings.ToLower(m[1])
		if !seenCall[name] {
			seenCall[name] = true
			calls = append(calls, name)
		}
	}
	seenRef := map[string]bool{}
	var refs []string
	for _, m := range tuxedoRefRe.FindAllString(body, -1) {
		key := strings.ToLower(strings.Join(strings.Fields(m), " "))
		if !seenRef[key] {
			seenRef[key] = true
			refs = append(refs, strings.TrimSpace(m))
		}
	}
	var errs []string
	if len(calls) > 0 {
		errs = append(errs, "tuxedo runtime: translate intent, never spelling — drop these legacy calls and use the mapped Go instead (FML packing → append shaped rows to data; tpreturn(TPSUCCESS) → return data, err; error legs → return nil with the S-code in the error text): "+strings.Join(calls, ", "))
	}
	if len(refs) > 0 {
		errs = append(errs, "tuxedo runtime: these legacy references have no Go equivalent in a controller — drop cursor CLOSE/buffer bookkeeping, read inputs as request.<Field>, never write SQL: "+strings.Join(refs, ", "))
	}
	return errs
}

// stripDeadComments drops comment-only lines (// full-line and /* */
// regions, including multi-line block comments) from a branch view before
// prompting. Dead code lives in comments — the ver-2.2 D2U SELECT rode a
// /* ... **/ block into fragment prompts as live SQL and every retry echoed
// it back (found SQL). Lines carrying code (even with trailing comments)
// are kept byte-for-byte; string/char literals never start comments.
func stripDeadComments(src string) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		i := 0
		code := false
		inString := false
		inChar := false
		escaped := false
		for i < len(line) {
			ch := line[i]
			switch {
			case inBlock:
				if ch == '*' && i+1 < len(line) && line[i+1] == '/' {
					inBlock = false
					i += 2
					continue
				}
				i++
			case inString:
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inString = false
				}
				i++
			case inChar:
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '\'' {
					inChar = false
				}
				i++
			default:
				switch {
				case ch == '/' && i+1 < len(line) && line[i+1] == '*':
					inBlock = true
					i += 2
				case ch == '/' && i+1 < len(line) && line[i+1] == '/':
					i = len(line) // line comment — nothing code-shaped after this
				case ch == '"':
					inString = true
					code = true
					i++
				case ch == '\'':
					inChar = true
					code = true
					i++
				default:
					if ch != ' ' && ch != '\t' && ch != '\r' {
						code = true
					}
					i++
				}
			}
		}
		if code {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// legacyScaffoldRe matches whole view lines that are pure Tuxedo/Pro*C
// scaffold with no Go intent: buffer-management macros, buffer alloc/free,
// FML size helpers, logging calls, cursor CLOSE, and EXEC SQL INCLUDE /
// DECLARE-SECTION directives. The controller tuxedo gate (controllerTuxedoErrs) rejects all
// of these spellings in LLM output — stripping them from the view removes
// the echo temptation and the prompt tax before any LLM spend.
//
// Conservative by design: only lines STARTING with the scaffold token
// (after optional /*...*/ provenance markers) are dropped. Mixed-intent
// lines (Fadd32/Fget32 field mapping, tpreturn legs, chk_sssn guards,
// strcpy assignments) are kept — the LLM still maps their intent.
var legacyScaffoldRe = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*(?:EXEC\s+SQL\s+(?:[Ii][Nn][Cc][Ll][Uu][Dd][Ee]|[Bb][Ee][Gg][Ii][Nn]|[Ee][Nn][Dd])\b|FBFR32\b|MEMSET\s*\(|SETNULL\s*\(|SETLEN\s*\(|CLOSE\s+[A-Za-z_]|userlog\s*\(|errlog\s*\(|tpalloc\s*\(|tpfree\s*\(|tprealloc\s*\(|INITDBGLVL\s*\(|Fsizeof32\s*\(|Funused32\s*\(|Fneeded32\s*\(|Finit32\s*\(|[A-Za-z_][A-Za-z0-9_.]*\s*=\s*(?:\([^)]*\)\s*)*(?:tpalloc|tprealloc)\s*\()`)

// stripLegacyScaffold drops pure-scaffold lines from a branch view before
// prompting. It runs after stripDeadComments/ReplaceQueries: query regions
// are already store calls (requiredCalls unaffected), and only dead scaffold
// — never store calls, branches, or FML mapping — is removed. Returns the
// stripped source and the dropped line count for telemetry.
func stripLegacyScaffold(src string) (string, int) {
	var out []string
	dropped := 0
	for _, line := range strings.Split(src, "\n") {
		if legacyScaffoldRe.MatchString(line) {
			dropped++
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), dropped
}

// preludeDeclRe matches uninitialized C declaration lines: a storage type
// (or varchar host-var) declaring names with no initializer and no call —
// `int i_cnt;`, `char c_user_id[19];`. Anything with `=`, `(`, or `{` keeps
// its line: initializers and call-shaped decls may carry intent the prompt
// still needs.
var preludeDeclRe = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*(?:(?:int|long|short|char|float|double|varchar|TPSVCINFO|FBFR32)\b[^=({;]*;\s*(?:/\*.*?\*/\s*)?)$`)

// stripPreludeDecls drops the view's leading run of context-only lines —
// blanks, uninitialized C declarations, and EXEC SQL DECLARE markers —
// before prompting. The Go template owns declarations (fragmentLocals tracks
// Go-side decls; C decls never translate), so the shared init prelude every
// endpoint repeats is prompt tax with echo risk. Stops at the first
// intent-bearing line; a view whose first code line is live keeps every
// byte. Store-call lines always carry `(` and never match. Returns the
// stripped source and the dropped line count.
func stripPreludeDecls(src string) (string, int) {
	lines := strings.Split(src, "\n")
	i := 0
	for i < len(lines) {
		t := strings.TrimSpace(stripLeadingComments(lines[i]))
		if t == "" || preludeDeclRe.MatchString(lines[i]) {
			i++
			continue
		}
		break
	}
	return strings.Join(lines[i:], "\n"), i
}

// branchSource slices the 1-based inclusive line range out of src.
func branchSource(src string, from, to int) string {
	lines := strings.Split(src, "\n")
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// bodyParseWrap wraps a body (full or fragment) in the synthetic method the
// parse gate compiles — one home for the wrap both validateBody and
// validateFragment reuse.
func bodyParseWrap(body string) string {
	// The signature mirrors the rendered method (request/data/err declared)
	// so the undeclared-identifier gate sees only genuinely unknown names.
	return "package controller\n\nimport (\n\t\"context\"\n\tmodels \"root-folder/pkg/services/item/models\"\n)\n\ntype t struct{}\n\nfunc (t) Check(ctx context.Context, request *models.Request) (data []*models.Response, err error) {\n" + body + "\n}\n"
}

// validateBody checks the body wrapped in a synthetic method. gofmt
// normalization is deterministic — space-vs-tab indentation from the model
// is auto-fixed downstream (appendControllerMethod re-formats the file), so
// only parse errors reject an attempt.
func validateBody(opts Options, body string) []string {
	if t := strings.TrimSpace(body); strings.HasPrefix(t, "}") {
		return []string{"the body must not start with a branch-chain continuation (`} else ...`) — the view's leading `else if (...) {` header is the condition this method already represents; emit only the statements under it"}
	}
	if _, ferr := goast.Emit("convert: controller body", bodyParseWrap(body)); ferr != nil {
		return validate.TrimGoErrors(ferr.Error())
	}
	// Parse-clean is not compile-clean: reject the compiler-level shapes the
	// local syntax-only runs cannot see (Tier B absent), so the retry loop
	// pays them out in notes instead of staging broken code.
	errs := nonConstFormatErrs(body)
	errs = append(errs, runeLiteralErrs(body)...)
	errs = append(errs, undeclaredIdentErrs(body, stubNames(opts))...)
	errs = append(errs, uncapturedStoreErrs(body, receiverOf(opts))...)
	return append(errs, unusedLocalErrs(body)...)
}

// nullStringCallRe matches a two-level .String() call (row.X.String(),
// dateRange.CFromDate.String()) — the model's spelling of the NullString
// .String FIELD. Single-level x.String() (e.g. time.Time) is left alone,
// so legitimate String() methods survive the fixup.
var nullStringCallRe = regexp.MustCompile(`(\.[A-Za-z_][A-Za-z0-9_]*)\.String\(\)`)

// dataShadowRe matches a body-level `data := make(` — the template fixes
// data as a named return (same scope as the body), so := is a redeclaration;
// the init the prompt instructs must assign with =.
var dataShadowRe = regexp.MustCompile(`(?m)^(\s*)data := make\(`)

// cleanBody strips code fences and stray blank lines the model may add —
// line-based, so the body's own indentation (which gofmt normalizes) is
// preserved exactly. It also normalizes NullString method-call spelling to
// the field access, so no gate or retry burns on it.
func cleanBody(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t == "```" || t == "```go" {
			continue
		}
		out = append(out, ln)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	cleaned := dataShadowRe.ReplaceAllString(
		nullStringCallRe.ReplaceAllString(strings.Join(out, "\n"), "${1}.String"),
		"${1}data = make(")
	return fixRuneLiterals(cleaned)
}

// appendControllerMethod appends the rendered method to the controller file
// (text append + import header on creation — bodies are LLM artifacts, the
// go/ast append model stays reserved for interfaces). The file is normalized
// with go/format after every append so Tier A passes deterministically.
func appendControllerMethod(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, path, body string) error {
	method, err := svc.RenderControllerMethod(u.Name, body)
	if err != nil {
		return err
	}
	_, err = writeControllerArtifact(ctx, opts, res, svc, path, u.Name, method)
	return err
}

// writeControllerArtifact merges one rendered method into the controller
// file and reports whether it wrote. A rejected placeholder for the same
// method is replaced (never duplicated); a live method with that name wins
// (resume guard: the ledger is the resume record, but a cleared/stale
// ledger against an existing file would restack the method — generation is
// deterministic, so bytes would be identical). Imports stay content-gated.
func writeControllerArtifact(ctx context.Context, opts Options, res *Result, svc *gen.Service, path, name, method string) (bool, error) {
	var merged string
	if _, statErr := os.Stat(path); statErr == nil {
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return false, rerr
		}
		src := string(existing)
		if stripped, ok := stripRejectedBlock(src, name); ok {
			src = stripped
		}
		if gen.IsDeterministicControllerMethod(src, name) {
			// LLM upgrade: the deterministic placeholder yields to the
			// accepted body instead of stacking a duplicate.
			if stripped, ok := stripMethodByName(src, name); ok {
				src = stripped
			}
		}
		if hasLiveMethod(src, name) {
			return false, nil
		}
		merged = src + "\n" + strings.TrimRight(method, "\n") + "\n"
	} else {
		// The base header carries only what every method needs; errors/fmt
		// (and the tx/sqlx pair below) are added content-gated so a body
		// that never uses them cannot stage an unused import — the local
		// syntax-only runs would keep the gap latent until Tier B.
		header := "package controller\n\nimport (\n\t\"context\"\n\n\t\"" + svc.Module + "/pkg/logger\"\n\t\"" + svc.ModelsPkg + "\"\n)\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		merged = header + "\n" + strings.TrimRight(method, "\n") + "\n"
	}
	formatted, ferr := goast.Emit("convert: controller file", merged)
	if ferr != nil {
		return false, ferr
	}
	if err := os.WriteFile(path, []byte(formatted), 0o644); err != nil {
		return false, err
	}
	// Tx bodies need imports the fixed header never carried: utils for the
	// ExecTransaction wrapper, sqlx for the tx handle. Previously every
	// such body was Tier-B-uncompilable on a wired target (the runs here
	// are syntax-only, so the gap stayed latent). Content-gated — files
	// without tx shapes keep their exact header.
	var wantImports []string
	if strings.Contains(merged, "errors.") {
		wantImports = append(wantImports, "errors")
	}
	if strings.Contains(merged, "fmt.") {
		wantImports = append(wantImports, "fmt")
	}
	if strings.Contains(merged, "time.") {
		wantImports = append(wantImports, "time")
	}
	if strings.Contains(merged, "ExecTransaction(") {
		wantImports = append(wantImports, svc.Module+"/pkg/utils")
	}
	if strings.Contains(merged, "sqlx.") {
		wantImports = append(wantImports, "github.com/jmoiron/sqlx")
	}
	if len(wantImports) > 0 {
		if err := goast.AddImports(path, wantImports...); err != nil {
			return false, err
		}
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return false, err
	}
	res.Files = append(res.Files, path)
	return true, nil
}

// appendRejectedController keeps a failed seam's last rejected output
// visible: the output rides commented inside a panicking placeholder method
// (the stub-and-carry-on posture of the fn stubs) instead of the method
// disappearing. Best-effort: the unit still fails in the ledger, so resume
// retries, and writeControllerArtifact replaces the placeholder when a
// later attempt succeeds.
func appendRejectedController(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, path, rejected string, cause error) {
	if strings.TrimSpace(rejected) == "" {
		return
	}
	body := rejectedBodyComment(u.Name, rejected, cause) +
		"\n\tpanic(\"tuxconv: " + u.Name + " rejected — the commented output above is the last rejected attempt; fix the view or rerun\")"
	method, err := svc.RenderControllerMethod(u.Name, body)
	if err != nil {
		telemetry.Log(ctx).Warn("rejected output not kept", "unit", u.Name, "error", err.Error())
		return
	}
	wrote, err := writeControllerArtifact(ctx, opts, res, svc, path, u.Name, rejectedBlock(u.Name, method))
	if err != nil {
		telemetry.Log(ctx).Warn("rejected output not kept", "unit", u.Name, "error", err.Error())
		return
	}
	if !wrote {
		return
	}
	telemetry.Log(ctx).Info("rejected output kept commented", "unit", u.Name)
	res.Warnings = append(res.Warnings, u.Name+" failed validation — the last rejected output is kept commented in the controller (panicking placeholder)")
}

// rejectedBlock wraps a placeholder method in strip markers so a later
// accepted body replaces it instead of stacking a duplicate.
func rejectedBlock(name, method string) string {
	return "// tuxconv:REJECTED-BEGIN " + name + "\n" + method + "\n// tuxconv:REJECTED-END " + name + "\n"
}

// stripRejectedBlock removes a previously kept rejected placeholder for
// name (markers included) from src.
func stripRejectedBlock(src, name string) (string, bool) {
	begin := "// tuxconv:REJECTED-BEGIN " + name
	end := "// tuxconv:REJECTED-END " + name
	i := strings.Index(src, begin)
	if i < 0 {
		return src, false
	}
	j := strings.Index(src[i:], end)
	if j < 0 {
		return src, false
	}
	j += i + len(end)
	if j < len(src) && src[j] == '\n' {
		j++
	}
	return src[:i] + src[j:], true
}

// hasLiveMethod reports whether src declares a real method named name —
// line-anchored, so commented rejected signatures never count.
func hasLiveMethod(src, name string) bool {
	re := common.CachedRegexp(`(?m)^func \([^)]*\) ` + regexp.QuoteMeta(name) + `\(`)
	return re.MatchString(src)
}

// methodLanded reports whether path exists and declares a live method named
// name — the merged-file resume guard. Ledger-appended controller/fn-helper
// units skip regeneration only when their method is still on disk; a cleared
// staged tree with a kept ledger must regenerate instead of silently leaving
// the tree incomplete. Any read failure is a miss (conservative: regenerate
// and let the write surface the real error).
func methodLanded(path, name string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hasLiveMethod(string(data), name)
}

// rejectedBodyComment renders the header + commented payload of a kept
// rejected output: every payload line becomes a `//` comment (any content
// stays inert), and the marker text inside the payload is neutralized so it
// can never collide with the block markers.
func rejectedBodyComment(name, rejected string, cause error) string {
	rejected = strings.ReplaceAll(rejected, "tuxconv:REJECTED", "tuxconv_REJECTED")
	var b strings.Builder
	fmt.Fprintf(&b, "// tuxconv:REJECTED — %s: every attempt failed validation; the last rejected output is kept below, compiled out.\n", name)
	if cause != nil {
		fmt.Fprintf(&b, "// rejection: %s\n", strings.ReplaceAll(strings.TrimSpace(cause.Error()), "\n", " "))
	}
	b.WriteString("//\n")
	for _, ln := range strings.Split(strings.TrimRight(rejected, "\n"), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			b.WriteString("//\n")
			continue
		}
		b.WriteString("// " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTPCallPlaceholders writes controller/tpcall_placeholders.go for the
// plan's tpcall units and records each as a ledger placeholder with its
// conversion-map entry (PF-4.4/4.5/4.6). No-op for plans without tpcalls.
func renderTPCallPlaceholders(ctx context.Context, opts Options, res *Result, svc *gen.Service) error {
	units := unitsOf(opts.Plan, plan.KindTPCall)
	if len(units) == 0 {
		return nil
	}
	path, err := opts.absPath(opts.BaseDir, units[0].TargetPath)
	if err != nil {
		return err
	}
	for _, u := range units {
		opts.Ledger.Get(u.ID, string(u.Kind), u.Name) // register before transitions
	}
	if e := opts.Ledger.Get(units[0].ID, string(units[0].Kind), units[0].Name); e.Status == ledger.StatusPlaceholder {
		if _, serr := os.Stat(path); serr == nil {
			return nil // resume: the placeholder file already landed
		} else if !os.IsNotExist(serr) {
			return fmt.Errorf("convert: stat tpcall placeholders: %w", serr)
		}
		telemetry.Log(ctx).Info("placeholder file missing despite ledger — regenerating", "path", path)
	}
	content, err := svc.PlaceholderFile(opts.Plan)
	if err != nil {
		for _, u := range units {
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
		}
		return fmt.Errorf("convert: render tpcall placeholders: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write tpcall placeholders: %w", err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return err
	}
	res.Files = append(res.Files, path)
	for _, u := range units {
		reason := "external service call rendered as a tuxconv:TODO placeholder (no outbound-call convention, R8)"
		if u.TP != nil && u.TP.Ambiguous {
			reason = "external service call rendered as a tuxconv:TODO placeholder (ambiguous send/recv window)"
		}
		opts.Ledger.Set(u.ID, ledger.StatusPlaceholder, reason, relPath(opts.BaseDir, path))
		addMap(opts, res, u, []string{relPath(opts.BaseDir, path)})
		res.Placeholders = append(res.Placeholders, u.Name)
	}
	return nil
}

// dbOut is one rendered DB method: its body and interface signature.
type dbOut struct {
	body string
	sig  string
}

// dbSignaturesFor renders the store contract lines a controller prompt
// consumes: only the methods the endpoint's view calls (scenario services
// carry hundreds of store methods — the whole-interface contract would
// exceed the prompt ceiling; the REQUIRED-CALLS gate already pins the
// endpoint's own set). Names may arrive bare (GetDateDetails, single-call
// path) or receiver-prefixed (s.store.GetDateRange, fragment path) — the
// match is on the bare method name either way (audit 2026-09-16: prefixed
// names matched nothing, so every fragment prompt carried an empty
// contract next to its REQUIRED CALLS).
func dbSignaturesFor(p *plan.Plan, bodies map[string]dbOut, methods []string) string {
	want := map[string]bool{}
	for _, m := range methods {
		want[bareStoreCall(m)] = true
	}
	var lines []string
	for _, u := range unitsOf(p, plan.KindDBMethod) {
		if !want[u.Name] {
			continue
		}
		b := bodies[u.ID]
		lines = append(lines, common.GoStoreCallPrefix()+b.sig)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// --- helpers ---

// renderDBUnits renders every DB unit through a bounded worker pool.
// svc.DBMethod is a pure read of the shared IR (queries, host vars, mapping
// pins), so parallel renders are race-free; results land indexed by unit
// position and the first unit-order error wins, so output and failures match
// a workers=1 run byte for byte.
func renderDBUnits(svc *gen.Service, units []plan.Unit, workers int) (map[string]dbOut, error) {
	out := make([]dbOut, len(units))
	errs := make([]error, len(units))
	common.RunIndexed(len(units), workers, func(i int) {
		body, sig, _, err := svc.DBMethod(units[i])
		if err != nil {
			errs[i] = err
			return
		}
		out[i] = dbOut{body, sig}
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	bodies := make(map[string]dbOut, len(units))
	for i, u := range units {
		bodies[u.ID] = out[i]
	}
	return bodies, nil
}

func (o Options) workerCount() int {
	return max(o.Workers, 1)
}

func unitsOf(p *plan.Plan, k plan.Kind) []plan.Unit {
	var out []plan.Unit
	for _, u := range p.Units {
		if u.Kind == k {
			out = append(out, u)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func unitID(p *plan.Plan, k plan.Kind) string {
	for _, u := range p.Units {
		if u.Kind == k {
			return u.ID
		}
	}
	return ""
}

// renamedUnits lists plan units whose ledger entry already exists under a
// different kind/name — the mapping-rename signal. Callers warn; Ledger.Get
// resets those units to planned so they regenerate. Only semantic units
// (db/controller/fn-helper/tpcall) are compared: whole-file artifacts
// (models, interfaces, handlers, router, fnstubs) register in the ledger
// under their display kind/name ("file"/filename), so comparing them
// against the plan identity would flag every run.
func renamedUnits(p *plan.Plan, l *ledger.Ledger) []string {
	var out []string
	for _, u := range p.Units {
		switch u.Kind {
		case plan.KindDBMethod, plan.KindControllerMethod, plan.KindFnHelper, plan.KindTPCall:
		default:
			continue
		}
		if e, ok := l.Units[u.ID]; ok && e.Kind != "" && e.Name != "" &&
			(e.Kind != string(u.Kind) || e.Name != u.Name) {
			out = append(out, u.ID+" ("+e.Name+"→"+u.Name+")")
		}
	}
	return out
}

// fnStubLanded reports whether the fnstub file unit already converted — the
// resume shortcut that keeps stub synthesis a first-run cost. It reads the
// ledger map directly (no Get) so the probe never mutates resume state. A
// cleared staged tree with a kept ledger must re-run synthesis: otherwise
// generateFile would rewrite panicking stubs over the synthesized ones with
// an empty synth map.
func fnStubLanded(opts Options) bool {
	id := unitID(opts.Plan, plan.KindFnStub)
	if id == "" {
		return true // no stubs — nothing to synthesize
	}
	e, ok := opts.Ledger.Units[id]
	if !ok || e.Status != ledger.StatusAppended {
		return false
	}
	if opts.Plan == nil || opts.Plan.Mapping == nil {
		return true
	}
	path, err := opts.absPath(opts.BaseDir, opts.Plan.Mapping.ImportPath("controller")+"/fnstubs.go")
	if err != nil {
		return true
	}
	if _, serr := os.Stat(path); serr == nil {
		return true
	} else if !os.IsNotExist(serr) {
		return true // let generateFile surface the stat error without burning synthesis
	}
	return false
}

// generateFile renders a deterministic artifact, writes it (unless the
// ledger already marked it appended AND the file is still on disk —
// resume), validates Tier A, and records the ledger + audit trail. A resume
// whose staged tree was cleared must regenerate: an appended ledger entry
// with no file falls through instead of silently leaving the tree
// incomplete.
func generateFile(ctx context.Context, opts Options, res *Result, id, kind, name, path string, render func() (string, error)) error {
	e := opts.Ledger.Get(id, kind, name)
	if e.Status == ledger.StatusAppended {
		if _, serr := os.Stat(path); serr == nil {
			return nil // resume: artifact already on disk
		} else if !os.IsNotExist(serr) {
			return fmt.Errorf("convert: stat %s: %w", path, serr)
		}
		telemetry.Log(ctx).Info("staged artifact missing despite appended ledger — regenerating", "unit", name, "path", path)
	}
	content, err := render()
	if err != nil {
		opts.Ledger.Set(id, ledger.StatusFailed, err.Error())
		return fmt.Errorf("convert: render %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write %s: %w", path, err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		opts.Ledger.Set(id, ledger.StatusFailed, err.Error())
		return err
	}
	opts.Ledger.Set(id, ledger.StatusAppended, "", relPath(opts.BaseDir, path))
	res.Files = append(res.Files, path)
	return nil
}

// writeFileValidated writes a whole-file artifact and validates it.
func writeFileValidated(ctx context.Context, opts Options, res *Result, path, content, label string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write %s: %w", label, err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return err
	}
	res.Files = append(res.Files, path)
	return nil
}

// validateFile runs Tier A on one generated file. Only .go sources go
// through the parser + gofmt check; other artifacts (e.g. the router
// snippet for the user's transport layer) need only exist.
func validateFile(ctx context.Context, opts Options, path string) error {
	if !strings.HasSuffix(path, ".go") {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("convert: %s missing", path)
		}
		return nil
	}
	res := opts.Validator.Syntax(path)
	if !res.OK {
		return fmt.Errorf("convert: %s: %s", path, strings.Join(res.Errors, "; "))
	}
	return nil
}

// addMap records the unit's conversion-map entries (§4.6).
func addMap(opts Options, res *Result, u plan.Unit, targets []string) {
	src := u.SourceFile
	if src == "" {
		src = opts.Plan.Source
	}
	span := ""
	if u.SourceLines != "" {
		span = " (L" + u.SourceLines + ")"
	}
	for _, t := range targets {
		opts.Ledger.AddMap(fmt.Sprintf("%s :: %s%s", filepath.Base(src), u.Name, span), t)
	}
}

// absPath resolves a plan unit's target path (an import path) to a file on
// disk under the run's base directory: the module's first segment maps to
// the base itself, so the same relative layout holds for the real target
// service and the staged fallback.
func (o Options) absPath(base, targetPath string) (string, error) {
	parts := strings.SplitN(targetPath, "/", 2)
	rel := targetPath
	if len(parts) == 2 {
		rel = parts[1]
	}
	return filepath.Join(base, rel), nil
}

func relPath(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}
