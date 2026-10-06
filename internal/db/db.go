// Package db is the optional Oracle access seam (database/sql).
//
// Separate module by design: the conversion pipeline (parse → IR → plan →
// gen) never imports an Oracle driver and never requires DB credentials.
// This package wraps database/sql so operators can point the tool at a live
// Oracle instance for verification/inspection without making the DB a hard
// dependency.
//
// Disabled by default: when no DSN resolves (neither the literal nor the
// env var), Open returns a disabled Handle and nil error — every method on
// a disabled Handle returns ErrDisabled, and callers degrade to
// deterministic/offline behavior with a visible WARN, never a failure.
// The same contract covers the LLM seam: no API key → deterministic-only
// run, no DB DSN → offline run.
//
// Driver note: database/sql needs a registered driver name. The default is
// "oracle" (github.com/sijms/go-ora/v2, pure Go, no cgo). godror registers
// "godror" (cgo + Oracle client libs). This package does NOT import either
// driver — the operator's binary (or a tools-tagged file) registers the
// driver they use. When a DSN is set but the driver name is unregistered,
// Open fails loudly naming the driver, so a typo never silently disables.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrDisabled is returned by every Handle method when the handle is
// disabled (no DSN resolved). Callers treat it as "offline mode", not a
// failure.
var ErrDisabled = errors.New("db: disabled — no DSN configured (set database.dsn or the env var)")

// Config carries the Oracle connection settings. DSNEnv names the env var
// holding the DSN (never logged); DSN is the literal fallback for local dev
// only (never commit it). Empty DSN + unset env = disabled, which is valid.
type Config struct {
	// Driver is the database/sql driver name ("oracle" for go-ora, "godror"
	// for godror). Defaults to "oracle" when empty and a DSN resolves.
	Driver string
	// DSN is the literal data source name (local-dev fallback).
	DSN string
	// DSNEnv is the env-var NAME holding the DSN (env wins over literal).
	DSNEnv string
	// MaxOpenConns / MaxIdleConns size the pool (0 = driver default).
	MaxOpenConns int
	MaxIdleConns int
	// ConnMaxLifetime caps connection reuse ("5m", "0s" = unlimited).
	ConnMaxLifetime time.Duration
}

// DefaultConfig returns the stock disabled-by-default settings.
func DefaultConfig() Config {
	return Config{
		Driver:          "oracle",
		DSN:             "",
		DSNEnv:          "ORACLE_DSN",
		MaxOpenConns:    0,
		MaxIdleConns:    0,
		ConnMaxLifetime: 0,
	}
}

// ResolveDSN returns the effective DSN and where it came from ("env:NAME",
// "literal", or "none"). The value itself is never logged by this package.
func (c Config) ResolveDSN() (dsn, source string) {
	if c.DSNEnv != "" {
		if v := strings.TrimSpace(os.Getenv(c.DSNEnv)); v != "" {
			return v, "env:" + c.DSNEnv
		}
		// Named env var unset with no literal fallback: caller decides —
		// Open treats it as disabled only when DSN is also empty; a typo'd
		// name with an empty literal is indistinguishable from "not
		// configured" by design (DB is optional, unlike the LLM key which
		// errors on a named-but-unset var). Operators run `dbcheck` to see
		// the resolution.
	}
	if strings.TrimSpace(c.DSN) != "" {
		return c.DSN, "literal"
	}
	return "", "none"
}

// Enabled reports whether a DSN resolves (i.e. Open would try to connect).
func (c Config) Enabled() bool {
	dsn, _ := c.ResolveDSN()
	return strings.TrimSpace(dsn) != ""
}

// Handle is an optional Oracle connection. Disabled handles carry nil DB
// and every method returns ErrDisabled. Enabled handles wrap *sql.DB.
type Handle struct {
	db     *sql.DB
	driver string
	source string
}

// Open resolves the DSN and — when one exists — opens the pool. When no
// DSN resolves it returns a disabled Handle and nil error: the application
// works offline (deterministic conversion, no live verification).
// A DSN with an unregistered driver name is a hard error (typo guard).
func Open(cfg Config) (*Handle, error) {
	dsn, source := cfg.ResolveDSN()
	if strings.TrimSpace(dsn) == "" {
		return &Handle{source: "none"}, nil
	}
	driver := strings.TrimSpace(cfg.Driver)
	if driver == "" {
		driver = "oracle"
	}
	known := false
	for _, name := range sql.Drivers() {
		if name == driver {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("db: driver %q is not registered (import your Oracle driver, e.g. go-ora registers \"oracle\", godror registers \"godror\")", driver)
	}
	sqldb, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open driver %q: %w", driver, err)
	}
	if cfg.MaxOpenConns > 0 {
		sqldb.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqldb.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqldb.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	return &Handle{db: sqldb, driver: driver, source: source}, nil
}

// Enabled reports whether the handle carries a live pool.
func (h *Handle) Enabled() bool { return h != nil && h.db != nil }

// Driver reports the driver name ("" when disabled).
func (h *Handle) Driver() string {
	if h == nil {
		return ""
	}
	return h.driver
}

// Source reports where the DSN came from ("env:NAME", "literal", "none") —
// never the DSN value itself.
func (h *Handle) Source() string {
	if h == nil {
		return "none"
	}
	if h.source == "" {
		return "none"
	}
	return h.source
}

// Status is the machine-readable connectivity summary for logs/UI.
// It never includes the DSN value.
func (h *Handle) Status() map[string]string {
	if !h.Enabled() {
		return map[string]string{"enabled": "false", "source": h.Source()}
	}
	return map[string]string{"enabled": "true", "driver": h.driver, "source": h.source}
}

// Ping verifies the connection (ErrDisabled when offline).
func (h *Handle) Ping(ctx context.Context) error {
	if !h.Enabled() {
		return ErrDisabled
	}
	return h.db.PingContext(ctx)
}

// Close releases the pool (no-op when disabled).
func (h *Handle) Close() error {
	if !h.Enabled() {
		return nil
	}
	return h.db.Close()
}

// Query runs a read query (ErrDisabled when offline). The caller owns rows.
func (h *Handle) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !h.Enabled() {
		return nil, ErrDisabled
	}
	return h.db.QueryContext(ctx, query, args...)
}

// Exec runs a DML/DDL statement (ErrDisabled when offline).
func (h *Handle) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !h.Enabled() {
		return nil, ErrDisabled
	}
	return h.db.ExecContext(ctx, query, args...)
}

// DB exposes the raw pool for call sites that need Tx/Prepare (nil when
// disabled — check Enabled() first).
func (h *Handle) DB() *sql.DB {
	if h == nil {
		return nil
	}
	return h.db
}
