package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Validate enforces the schema invariants: exactly one default profile,
// unique names, OpenAI-compatible endpoints with resolvable URLs, positive
// budgets, and known enum values. Errors name the offending field so a
// mis-edited yaml fails at load, never mid-run.
func (c *Config) Validate() error {
	if c.Run.Profile == "" {
		return fmt.Errorf("run.profile must not be empty")
	}
	for _, bad := range []struct {
		name string
		v    int
	}{
		{"run.maxContextTokens", c.Run.MaxContextTokens},
		{"run.maxPromptTokens", c.Run.MaxPromptTokens},
		{"run.maxOutputTokens", c.Run.MaxOutputTokens},
		{"run.charsPerToken", c.Run.CharsPerToken},
	} {
		if bad.v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", bad.name, bad.v)
		}
	}
	if c.Run.MaxPromptTokens > c.Run.MaxContextTokens {
		return fmt.Errorf("run.maxPromptTokens (%d) exceeds the context window (%d)", c.Run.MaxPromptTokens, c.Run.MaxContextTokens)
	}
	switch c.Run.TokenPolicy {
	case "", "static":
		// The static policy ignores the dynamic knobs — a stray
		// modelContextTokens on a static run stays inert.
	case "dynamic":
		if c.Run.ModelContextTokens <= 0 {
			return fmt.Errorf("run.tokenPolicy \"dynamic\" requires run.modelContextTokens (the provider's context length)")
		}
		if c.Run.ModelMaxOutputTokens <= 0 {
			return fmt.Errorf("run.tokenPolicy \"dynamic\" requires run.modelMaxOutputTokens (the provider's per-request completion cap)")
		}
		if c.Run.ModelContextTokens < c.Run.ModelMaxOutputTokens {
			return fmt.Errorf("run.modelContextTokens (%d) is below run.modelMaxOutputTokens (%d)", c.Run.ModelContextTokens, c.Run.ModelMaxOutputTokens)
		}
		if c.Run.OutputReserveTokens < 0 {
			return fmt.Errorf("run.outputReserveTokens must not be negative, got %d", c.Run.OutputReserveTokens)
		}
	default:
		return fmt.Errorf("run.tokenPolicy %q: only \"static\" or \"dynamic\"", c.Run.TokenPolicy)
	}
	if c.Run.Temperature < 0 || c.Run.Temperature > 2 {
		return fmt.Errorf("run.temperature %v out of range [0,2]", c.Run.Temperature)
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("models[] must not be empty")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Name == "" {
			return fmt.Errorf("models[%d].name must not be empty", i)
		}
		if seen[m.Name] {
			return fmt.Errorf("duplicate model profile %q", m.Name)
		}
		seen[m.Name] = true
		if m.Provider != "openai-compatible" {
			return fmt.Errorf("models[%d].provider %q: only \"openai-compatible\" is supported (R1)", i, m.Provider)
		}
		if m.Model == "" {
			return fmt.Errorf("models[%d].model must not be empty", i)
		}
		u, err := url.Parse(m.APIBase)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("models[%d].apiBase %q is not a valid http(s) URL", i, m.APIBase)
		}
		// No apiKeyEnv/apiKey is a deliberate keyless endpoint; a *named*
		// env var that is unset with no literal fallback errors at
		// Model.APIKey() time so a typo'd name never silently goes keyless.
		if m.Options.Temperature != nil && (*m.Options.Temperature < 0 || *m.Options.Temperature > 2) {
			return fmt.Errorf("models[%d].requestOptions.temperature %v out of range [0,2]", i, *m.Options.Temperature)
		}
		if m.Options.MaxTokens < 0 {
			return fmt.Errorf("models[%d].requestOptions.maxTokens must not be negative", i)
		}
		if m.Options.Retries < 0 {
			return fmt.Errorf("models[%d].requestOptions.retries must not be negative", i)
		}
	}

	// The default profile must route to an existing entry.
	if _, err := c.Route(""); err != nil {
		return fmt.Errorf("run.profile: %w", err)
	}

	switch c.Elision.Mode {
	case "safe", "off":
	default:
		return fmt.Errorf("elision.mode %q: only \"safe\" or \"off\"", c.Elision.Mode)
	}
	if c.Concurrency.Workers < 1 {
		return fmt.Errorf("concurrency.workers must be >= 1, got %d", c.Concurrency.Workers)
	}
	if c.ValidateCfg.MaxRetries < 0 {
		return fmt.Errorf("validate.maxRetries must not be negative, got %d", c.ValidateCfg.MaxRetries)
	}
	switch c.ValidateCfg.Compile {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("validate.compile %q: only \"auto\", \"always\" or \"never\"", c.ValidateCfg.Compile)
	}
	if c.ValidateCfg.Compile == "always" && strings.TrimSpace(c.Paths.MainGo) == "" {
		return fmt.Errorf("validate.compile \"always\" requires paths.mainGo (the target module anchor)")
	}

	for _, p := range []struct{ name, v string }{
		{"paths.ledger", c.Paths.Ledger},
		{"paths.state", c.Paths.State},
		{"paths.staged", c.Paths.Staged},
	} {
		if strings.TrimSpace(p.v) == "" {
			return fmt.Errorf("%s must not be empty", p.name)
		}
	}

	// Buffer-role registry (PF-4.1): roles are a fixed vocabulary — a typo
	// must fail at load, never degrade silently mid-run.
	for name, role := range c.Buffers.Roles {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("buffers.roles: buffer name must not be empty")
		}
		if !BufferRoleNames[role] {
			return fmt.Errorf("buffers.roles[%q]: unknown role %q (want input, output, send or recv)", name, role)
		}
	}

	// batchpy conventions (PRD-2026-09-08 BP-7): enum fields are a fixed
	// vocabulary; the wrapper spellings must be present or the generated
	// module cannot compile.
	b := c.Batchpy
	for _, p := range []struct{ name, v string }{
		{"batchpy.wrapperModule", b.WrapperModule},
		{"batchpy.routerClass", b.RouterClass},
		{"batchpy.readMode", b.ReadMode},
		{"batchpy.writeMode", b.WriteMode},
		{"batchpy.entrypoint", b.Entrypoint},
	} {
		if strings.TrimSpace(p.v) == "" {
			return fmt.Errorf("%s must not be empty", p.name)
		}
	}
	switch b.Shape {
	case "auto", "repo":
	default:
		return fmt.Errorf("batchpy.shape %q: only \"auto\" or \"repo\"", b.Shape)
	}
	switch b.DMLLoop {
	case "batch", "rowbyrow":
	default:
		return fmt.Errorf("batchpy.dmlLoop %q: only \"batch\" or \"rowbyrow\"", b.DMLLoop)
	}
	if b.ChunkSize < 1 {
		return fmt.Errorf("batchpy.chunkSize must be >= 1, got %d", b.ChunkSize)
	}
	// database (optional live-Oracle access, internal/db): disabled by
	// default (no DSN), which is valid — the pipeline runs offline. Only
	// the pool knobs are validated; a missing DSN never fails the run.
	if c.Database.MaxOpenConns < 0 {
		return fmt.Errorf("database.maxOpenConns must not be negative, got %d", c.Database.MaxOpenConns)
	}
	if c.Database.MaxIdleConns < 0 {
		return fmt.Errorf("database.maxIdleConns must not be negative, got %d", c.Database.MaxIdleConns)
	}
	if time.Duration(c.Database.ConnMaxLifetime) < 0 {
		return fmt.Errorf("database.connMaxLifetime must not be negative")
	}
	return nil
}
