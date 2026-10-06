package main

import (
	"context"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/templates"
)

// runWiring is the run-wide collaborator bundle (A5.2): one budget and one
// audit recorder cover every service/file in a run — the budget is
// immutable, the recorder mutex-guarded. Built once per command from the
// run config; the LLM client stays per-purpose (resolveLLMClient) because
// its disable log names the seam. The DB handle stays optional
// (resolveDBHandle): nil/disabled offline, never fatal.
type runWiring struct {
	cfg    *config.Config
	budget budget.Budget
	audit  *audit.Recorder // nil when the archive folder is unavailable
	db     dbHandle
}

// dbHandle is the minimal surface runWiring keeps from internal/db —
// Enabled/Source only, so wiring never imports database/sql directly and
// the DB stays a separate module.
type dbHandle interface {
	Enabled() bool
	Source() string
}

// runBudget resolves the run's budget from the config: the static ceilings
// plus the dynamic output policy when run.tokenPolicy selects it — the
// per-call output room then derives from the model's real context instead of
// a configured cap (config validate enforces the dynamic knobs).
func runBudget(r config.Run) budget.Budget {
	b := budget.New(r.MaxPromptTokens, r.MaxOutputTokens, r.CharsPerToken)
	if r.TokenPolicy == "dynamic" {
		b.ModelContextTokens = r.ModelContextTokens
		b.ModelMaxOutputTokens = r.ModelMaxOutputTokens
		b.OutputReserveTokens = r.OutputReserveTokens
	}
	return b
}

// newWiring resolves the bundle. Audit degradation is visible (WARN), never
// fatal — the audit trail is best-effort by contract (§4.7). The DB handle
// resolves the same way: offline when no DSN is configured, never fatal —
// conversion runs deterministic without live Oracle.
func newWiring(ctx context.Context, cfg *config.Config) *runWiring {
	log := telemetry.Log(ctx)
	w := &runWiring{
		cfg:    cfg,
		budget: runBudget(cfg.Run),
	}
	if h, err := resolveDBHandle(ctx, cfg); err != nil {
		log.Warn("database handle unavailable — continuing offline", "error", err)
	} else {
		w.db = h
		if h != nil && h.Enabled() {
			// The pool stays open for the run; call sites that need live
			// verification use their own resolveDBHandle. Closing here
			// would drop it — lifecycle belongs to the holder.
			_ = h
		}
	}
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		return w
	}
	w.audit = rec
	return w
}

// auditRecorder resolves the run's audit recorder with the standard degrade
// (WARN + nil) for call sites that carry no config — archive helpers.
func auditRecorder(ctx context.Context) *audit.Recorder {
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		telemetry.Log(ctx).Warn("audit archive unavailable", "error", err)
		return nil
	}
	return rec
}

// templateProvider resolves the run's template set (flag > config > embedded):
// a non-empty dir activates the file overlay, where <id>.tmpl files override
// the embedded templates and missing IDs keep the embedded defaults. The
// routing decision is logged once per run so staged output stays auditable.
func templateProvider(ctx context.Context, cfg *config.Config, flagDir string) (templates.Provider, error) {
	dir := flagDir
	if dir == "" {
		dir = cfg.Templates.Dir
	}
	prov, err := templates.Resolve(dir)
	if err != nil {
		return nil, err
	}
	if _, ok := prov.(*templates.FileProvider); ok {
		overridden := 0
		for _, info := range templates.List(prov) {
			if info.Overridden {
				overridden++
			}
		}
		telemetry.Log(ctx).Info("templates: override dir active",
			"dir", dir, "overridden", overridden, "known", len(templates.AllIDs))
	}
	return prov, nil
}
