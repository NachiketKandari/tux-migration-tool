package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/db"
	"tux-migration-tool/internal/telemetry"
)

// runDBCheck implements `tuxconv dbcheck` — the optional live-Oracle probe.
//
// The DB is a separate module (internal/db, database/sql) and is never
// required: with no DSN this command reports `enabled=false source=none`
// and exits 0, and every other command runs offline. With a DSN it opens
// the pool and (with -ping) verifies connectivity. The DSN value is never
// printed or logged — only its source (env:NAME / literal / none).
func runDBCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dbcheck", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	ping := fs.Bool("ping", false, "Ping the Oracle instance (default: report resolution only, no connection)")

	flagArgs, _ := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	h, err := resolveDBHandle(ctx, cfg)
	if err != nil {
		return err
	}
	if h != nil && h.Enabled() {
		defer h.Close()
	}
	st := map[string]string{"enabled": "false", "source": "none", "driver": ""}
	if h != nil {
		st = h.Status()
	}
	fmt.Printf("database: enabled=%s driver=%s source=%s config=%s\n",
		st["enabled"], firstNonEmpty(st["driver"], cfg.Database.Driver), firstNonEmpty(st["source"], "none"), cfgSource)
	if !h.Enabled() {
		fmt.Println("database: offline — no DSN configured (set database.dsn or the env var); conversion runs deterministic without it")
		return nil
	}
	if !*ping {
		fmt.Println("database: DSN resolves — pass -ping to verify connectivity")
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := h.Ping(pctx); err != nil {
		return fmt.Errorf("db: ping failed (driver %q source %s): %w", h.Driver(), h.Source(), err)
	}
	fmt.Printf("database: ping ok (driver %s source %s)\n", h.Driver(), h.Source())
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveDBHandle opens the optional Oracle handle: disabled (no DSN) is a
// visible WARN + nil-error offline handle, never a failure. Only an
// unregistered driver name with a DSN set is a hard error (typo guard in
// internal/db). Every other command uses this so the app works with zero DB
// keys — same contract as the LLM seam (no API key → deterministic-only).
func resolveDBHandle(ctx context.Context, cfg *config.Config) (*db.Handle, error) {
	log := telemetry.Log(ctx)
	dcfg := db.Config{
		Driver:          cfg.Database.Driver,
		DSN:             cfg.Database.DSN,
		DSNEnv:          cfg.Database.DSNEnv,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.Database.ConnMaxLifetime),
	}
	// Empty Database section (zero value from an old yaml overlay) still
	// resolves to the disabled default — never force-enable.
	if dcfg.Driver == "" {
		dcfg.Driver = config.DefaultDatabase().Driver
	}
	if dcfg.DSNEnv == "" && dcfg.DSN == "" {
		dcfg.DSNEnv = config.DefaultDatabase().DSNEnv
	}
	h, err := db.Open(dcfg)
	if err != nil {
		return nil, err
	}
	if !h.Enabled() {
		log.Warn("database offline — running without live Oracle (set database.dsn or the env var to enable)", "source", h.Source())
		return h, nil
	}
	log.Info("database enabled", "driver", h.Driver(), "source", h.Source())
	return h, nil
}
