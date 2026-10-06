// Package config loads and validates the tuxconv run configuration
// (.tuxconv.yaml, PRD F7/R1): multi-profile OpenAI-compatible models[] with
// profile routing, run budgets, seam toggles, and artifact paths. Absent
// fields keep the documented defaults; API keys resolve from the environment
// (apiKeyEnv, R6) with a gitignored-file literal (apiKey) as the local-dev
// fallback.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default artifact-root constants (A5.3): the one home for the literals the
// cmd ladders re-declared. The config defaults and the no-config fallbacks
// both read from here, so they cannot drift apart.
const (
	DefaultStagedDir   = "conversion_logs/_staged"
	DefaultBatchpyOut  = "python_out"
	DefaultMappingsDir = "mappings"
	DefaultScenDir     = "scenarios"
)

// DefaultPaths mirrors the working-directory layout of
// configs/.tuxconv.example.yaml. Logs/audit are conventions, not config (see
// Paths).
func DefaultPaths() Paths {
	return Paths{
		Target: "../existing-go-service",
		MainGo: "",
		Ledger: "conversion_logs/ledger",
		State:  "conversion_logs/state",
		Staged: "conversion_logs/_staged",
	}
}

// Default returns the stock configuration (profiles per R1, budgets per §4.3,
// retrieval disabled per OQ1).
func Default() *Config {
	return &Config{
		Run: Run{
			Profile:             "onprem-vllm",
			Temperature:         0.1,
			MaxContextTokens:    16000,
			MaxPromptTokens:     12000,
			MaxOutputTokens:     4000,
			CharsPerToken:       4,
			TokenPolicy:         "static",
			OutputReserveTokens: 768,
			LLM:                 true,
		},
		Models: []Model{
			{
				Name:     "onprem-vllm",
				Provider: "openai-compatible",
				// The exact served model id pr-review uses against this
				// endpoint — vLLM answers only the name it was launched with.
				Model:     "<served-model-id>",
				APIBase:   "http://vllm.internal:8002/v1",
				APIKeyEnv: "VLLM_API_KEY",
				Options:   RequestOptions{Temperature: ptr(0.1), MaxTokens: 4000, Stream: true, Timeout: Duration(120 * time.Second), Retries: 3},
			},
			{
				Name:      "local-dev-openrouter",
				Provider:  "openai-compatible",
				Model:     "<openrouter-model-id>", // fill in your local .tuxconv.yaml
				APIBase:   "https://openrouter.ai/api/v1",
				APIKeyEnv: "OPENROUTER_API_KEY",
				Options:   RequestOptions{Temperature: ptr(0.1), MaxTokens: 4000, Stream: true},
			},
		},
		Elision:     Elision{Mode: "safe"},
		Concurrency: Concurrency{Workers: 1},
		ValidateCfg: ValidateCfg{MaxRetries: 3, Compile: "auto"},
		DB:          DB{WithGorm: false},
		Database:    DefaultDatabase(),
		Buffers:     DefaultBuffers(),
		Paths:       DefaultPaths(),
		Batchpy:     DefaultBatchpy(),
		Convert:     Convert{FlowDraft: ptrb(true)},
	}
}

// BufferRoleNames are the roles the pipeline understands (PF-4.1). The
// registry maps buffer variable NAMES to these roles.
var BufferRoleNames = map[string]bool{
	"input":  true, // Ibuffer — the endpoint's FML input (Fget32 source)
	"output": true, // Obuffer — the endpoint's FML output (Fadd32 target)
	"send":   true, // Sbuffer — a tpcall's send buffer
	"recv":   true, // Rbuffer — a tpcall's receive buffer
}

// DefaultBuffers returns the project's buffer naming convention registry
// (PF-4.1): keys are the lowercase name or last `_`-delimited segment
// (ptr_fml_Ibuffer → ibuffer → input) — the spelling ir's resolver matches
// case-insensitively, but the yaml convention here is the engine contract.
func DefaultBuffers() Buffers {
	return Buffers{Roles: map[string]string{
		"ibuffer": "input",
		"obuffer": "output",
		"sbuffer": "send",
		"rbuffer": "recv",
	}}
}

func ptr(f float64) *float64 { return &f }

func ptrb(b bool) *bool { return &b }

// Load reads path (typically .tuxconv.yaml in the working directory), overlays
// it on the defaults (absent sections keep their default values), and
// validates. Unknown keys are errors: a typo'd config must fail loudly.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Config is the root schema of .tuxconv.yaml.
type Config struct {
	Run         Run         `yaml:"run"`
	Models      []Model     `yaml:"models"`
	Retrieval   Retrieval   `yaml:"retrieval"`
	Elision     Elision     `yaml:"elision"`
	Concurrency Concurrency `yaml:"concurrency"`
	ValidateCfg ValidateCfg `yaml:"validate"`
	DB          DB          `yaml:"db"`
	Database    Database    `yaml:"database"`
	Convert     Convert     `yaml:"convert"`
	Buffers     Buffers     `yaml:"buffers"`
	Paths       Paths       `yaml:"paths"`
	Batchpy     Batchpy     `yaml:"batchpy"`
	Convertcs   Convertcs   `yaml:"convertcs"`
	Templates   Templates   `yaml:"templates"`
}

// Templates carries the user template overlay (the user-tunable template
// seam): Dir points at a directory of <template_id>.tmpl files that override
// matching embedded templates; IDs without a file keep the embedded default,
// so a partial override always works. Empty (default) = embedded set alone.
// A missing dir, an unknown id, or an unparseable override fails at wire-up;
// `tuxconv templates list|dump|verify` inspect, export, and check a set.
type Templates struct {
	Dir string `yaml:"dir"`
}

// Convertcs carries the convertcs (.NET Core) run defaults: the output
// root, the deterministic-only kill switch, and the namespace/area
// defaults the discover -target cs drafts fill their placeholders from.
type Convertcs struct {
	// Input is the .pc/.pcf file converted when the CLI passes no
	// positional target (same seam as convert.input).
	Input string `yaml:"input"`
	// Mapping is the convertcs mapping YAML used when -mapping is absent.
	Mapping string `yaml:"mapping"`
	// Out is the default output root for the generated component tree
	// (the -out flag overrides; the fallback is paths.staged's convention).
	Out string `yaml:"out"`
	// NoLLM forces the deterministic-only run: service bodies keep their
	// tuxconv:TODO seams. The -no-llm flag ORs on top of it (flag > config).
	NoLLM bool `yaml:"noLLM"`
	// Namespace is the draft-time root-namespace default: discover
	// -target cs fills the draft's namespace placeholder from here when
	// set (empty keeps the editable TODO placeholder).
	Namespace string `yaml:"namespace"`
	// Area is the draft-time dotted-area default under the root
	// (convertcs.area: AcmeApplication.CustomerAuthenticate).
	Area string `yaml:"area"`
}

// Batchpy carries the batch→Python conventions (PRD-2026-09-08 BP-7):
// wrapper-seam spellings the generated code imports, the entrypoint name,
// and the shape/DML-loop defaults the CLI flags override.
// Batchpy carries the batch→Python conventions (PRD-2026-09-08 BP-7).
type Batchpy struct {
	// Input is the .pc/.pcf file or directory converted when the CLI passes
	// no positional target (same seam as convert.input).
	Input string `yaml:"input"`
	// WrapperModule is the generated import path of the db-router wrapper.
	WrapperModule string `yaml:"wrapperModule"`
	// RouterClass is the wrapper class exposed by WrapperModule.
	RouterClass string `yaml:"routerClass"`
	// ReadMode/WriteMode are the wrapper's connection-mode spellings.
	ReadMode  string `yaml:"readMode"`
	WriteMode string `yaml:"writeMode"`
	// LoggerPrefix prefixes the generated logger name ("app.<module>").
	LoggerPrefix string `yaml:"loggerPrefix"`
	// Entrypoint is the unified runner method the service exposes.
	Entrypoint string `yaml:"entrypoint"`
	// Shape is the default shape rubric override: auto | repo.
	Shape string `yaml:"shape"`
	// DMLLoop is the default cursor-DML semantics: batch | rowbyrow.
	// Applies to the SIMPLE shape only — repo-shape DML always renders
	// row-by-row (the repository contract; engine-wiring audit Tier-2).
	DMLLoop string `yaml:"dmlLoop"`
	// ChunkSize is the executemany chunk size (batch mode, simple shape
	// only — repo-shape modules carry no CHUNK_SIZE).
	ChunkSize int `yaml:"chunkSize"`
	// OutDir is the default output directory for generated modules.
	OutDir string `yaml:"outDir"`
	// FileFilter scopes directory targets to .pc/.pcf files whose base name
	// contains the substring (case-insensitive) — e.g. "bat_px_" picks only
	// that family. Empty (default) converts every file in the directory.
	// Applies only when the target is a directory; an explicitly passed file
	// always converts.
	FileFilter string `yaml:"fileFilter"`
}

// DefaultBatchpy returns the reference-codebase conventions (the
// local-only reference conversions' wrapper contract).
func DefaultBatchpy() Batchpy {
	return Batchpy{
		WrapperModule: "core.db_router",
		RouterClass:   "DatabaseRouter",
		ReadMode:      "DbMode.READ",
		WriteMode:     "DbMode.WRITE",
		LoggerPrefix:  "app.",
		Entrypoint:    "process_daily_batch",
		Shape:         "auto",
		DMLLoop:       "batch",
		ChunkSize:     1000,
		OutDir:        "python_out",
	}
}

// Buffers is the config-extensible FML buffer-role registry (PF-4.1):
// variable names → roles (input/output/send/recv). The defaults are the
// project naming convention; renamed or additional buffer kinds register
// here instead of in code.
type Buffers struct {
	Roles map[string]string `yaml:"roles"`
}

// Run carries the generation-wide budget and the default profile name.
// CharsPerToken is the estimator ratio (§4.3): the budgeter approximates
// tokens as characters/CharsPerToken — tune it per model without a code
// change.
type Run struct {
	Profile          string  `yaml:"profile"`
	Temperature      float64 `yaml:"temperature"`
	MaxContextTokens int     `yaml:"maxContextTokens"`
	MaxPromptTokens  int     `yaml:"maxPromptTokens"`
	MaxOutputTokens  int     `yaml:"maxOutputTokens"`
	CharsPerToken    int     `yaml:"charsPerToken"`
	// TokenPolicy selects the output-ceiling policy: "static" (default)
	// keeps MaxOutputTokens; "dynamic" derives the per-call output room
	// from the model's real context minus the measured input (best output:
	// no configured-ceiling truncation, fragment stitching only when the
	// prompt itself nears the context window). Dynamic requires
	// modelContextTokens and modelMaxOutputTokens.
	TokenPolicy          string `yaml:"tokenPolicy"`
	ModelContextTokens   int    `yaml:"modelContextTokens"`
	ModelMaxOutputTokens int    `yaml:"modelMaxOutputTokens"`
	// OutputReserveTokens is the headroom dynamic mode subtracts from the
	// context when claiming output room — the estimator skew residual
	// (default 768).
	OutputReserveTokens int `yaml:"outputReserveTokens"`
	// LLM gates the generation seam (default true). false = deterministic-only
	// run: models/db/interfaces/handler/router generate, controller bodies
	// render deterministic best-effort drafts (marked skipped for a later
	// LLM-enabled resume to upgrade).
	LLM bool `yaml:"llm"`
}

// Duration is a yaml time span ("120s", "2m") decoding into time.Duration.
type Duration time.Duration

// UnmarshalYAML implements yaml.v3 strict duration parsing.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML renders the duration in Go notation.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// RequestOptions are the per-profile generation parameters.
type RequestOptions struct {
	Temperature *float64 `yaml:"temperature"` // absent → inherit run.temperature
	MaxTokens   int      `yaml:"maxTokens"`
	Stream      bool     `yaml:"stream"`
	Timeout     Duration `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
}

// Model is one OpenAI-compatible endpoint profile.
type Model struct {
	Name      string         `yaml:"name"`
	Provider  string         `yaml:"provider"`
	Model     string         `yaml:"model"`
	APIBase   string         `yaml:"apiBase"`
	APIKeyEnv string         `yaml:"apiKeyEnv"` // env-var NAME, never the value (R6)
	APIKey    string         `yaml:"apiKey"`    // literal fallback for the gitignored local .tuxconv.yaml only
	Options   RequestOptions `yaml:"requestOptions"`
}

// Route resolves the model profile a run uses: override when non-empty, else
// run.profile. This is the routing seam every LLM-bound command goes through.
func (c *Config) Route(override string) (*Model, error) {
	name := override
	if name == "" {
		name = c.Run.Profile
	}
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i], nil
		}
	}
	names := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		names = append(names, m.Name)
	}
	return nil, fmt.Errorf("profile %q not found in models[] (have %v)", name, names)
}

// APIKey resolves the profile's key: the env var wins; the literal apiKey
// (local gitignored config only) is the fallback. The source is returned for
// audit logging — never log the key value itself. Surrounding whitespace is
// ignored on both channels (pasted keys commonly carry quotes/spaces/newlines;
// internal/db ResolveDSN and the web viewer already trim), so a
// whitespace-only value counts as unset and the returned key is trimmed.
func (m *Model) ResolveKey() (key, source string, err error) {
	if m.APIKeyEnv != "" {
		if v := strings.TrimSpace(os.Getenv(m.APIKeyEnv)); v != "" {
			return v, "env:" + m.APIKeyEnv, nil
		}
		if strings.TrimSpace(m.APIKey) == "" {
			return "", "", fmt.Errorf("profile %s: env var %s is not set", m.Name, m.APIKeyEnv)
		}
	}
	if strings.TrimSpace(m.APIKey) != "" {
		return strings.TrimSpace(m.APIKey), "literal", nil
	}
	return "", "none", nil
}

// Merged returns the effective request options: profile values where set,
// run-level budget otherwise, stock 120s timeout when neither applies.
func (c *Config) Merged(m *Model) (temperature float64, maxTokens int, stream bool, timeout time.Duration, retries int) {
	temperature = c.Run.Temperature
	if m.Options.Temperature != nil {
		temperature = *m.Options.Temperature
	}
	maxTokens = c.Run.MaxOutputTokens
	if m.Options.MaxTokens > 0 {
		maxTokens = m.Options.MaxTokens
	}
	stream = m.Options.Stream
	timeout = time.Duration(m.Options.Timeout)
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	retries = m.Options.Retries
	return temperature, maxTokens, stream, timeout, retries
}

// Retrieval toggles the deferred retriever seam (OQ1). RESERVED — no
// engine reads this section; the schema is validated so existing configs
// load, and the README documents it as disabled. Wiring it is a deliberate
// later-version decision.
type Retrieval struct {
	Enabled          bool `yaml:"enabled"`
	MaxContextTokens int  `yaml:"maxContextTokens"`
}

// Elision selects the body-elision mode (G3). RESERVED — validated
// ("safe" | "off") but no engine honors it yet; elision behavior is fixed
// in the deterministic generators. Wire it or remove it in a later version.
type Elision struct {
	Mode string `yaml:"mode"`
}

// Concurrency is the opt-in DB-unit worker count (pipeline-three-parts 2b).
type Concurrency struct {
	Workers int `yaml:"workers"`
}

// DB carries the generated DB-layer shape options.
type DB struct {
	// WithGorm makes the store carry the legacy *gorm.DB handle alongside
	// sqlx (the item-example variant: NewXStore(oracle, db)). Default false —
	// the plain sqlx-only store (NewXStore(db)) is the standard shape.
	WithGorm bool `yaml:"withGorm"`
}

// Database carries the optional live-Oracle access settings (internal/db).
// Empty (default) = offline: the pipeline runs fully deterministic without
// any DSN, and `tuxconv dbcheck` reports disabled. Set dsn (local-dev
// literal, never commit) or the env var named by dsnEnv to enable live
// verification. The DSN value is never logged.
type Database struct {
	// Driver is the database/sql driver name: "oracle" (go-ora, pure Go)
	// or "godror" (cgo + Oracle client libs). Default "oracle".
	Driver string `yaml:"driver"`
	// DSN is the literal data source name (local-dev fallback only).
	DSN string `yaml:"dsn"`
	// DSNEnv names the env var holding the DSN (env wins over literal).
	DSNEnv string `yaml:"dsnEnv"`
	// MaxOpenConns / MaxIdleConns size the pool (0 = driver default).
	MaxOpenConns int `yaml:"maxOpenConns"`
	MaxIdleConns int `yaml:"maxIdleConns"`
	// ConnMaxLifetime caps connection reuse ("5m", "0s" = unlimited).
	ConnMaxLifetime Duration `yaml:"connMaxLifetime"`
}

// DefaultDatabase returns the disabled-by-default Oracle access settings.
func DefaultDatabase() Database {
	return Database{
		Driver: "oracle",
		DSN:    "",
		DSNEnv: "ORACLE_DSN",
	}
}

// Convert carries the plan/convert commands' default inputs so repeat runs
// need no CLI arguments; explicit CLI flags override these.
type Convert struct {
	// Input is the .pc/.pcf file or directory converted when the CLI passes
	// no positional target.
	Input string `yaml:"input"`
	// Mapping is the user endpoint-mapping YAML used when -mapping is absent.
	Mapping string `yaml:"mapping"`
	// FileFilter scopes directory targets to .pc/.pcf files whose base name
	// contains the substring (case-insensitive) — e.g. "px_" picks up
	// SVC_PX_*.pc and px_*.pc alike. Empty (default) converts every file in
	// the directory. Applies only when the target is a directory; an
	// explicitly passed file always converts.
	FileFilter string `yaml:"fileFilter"`
	// FlowDraft feeds the deterministic flow-tree transpilation draft
	// (PRD-2026-09-10) into the controller prompt as a verified base the
	// LLM enhances instead of translating the raw C from scratch. The
	// REQUIRED-CALLS gate is unchanged; kill-switch for prompt A/B runs.
	FlowDraft *bool `yaml:"flowDraft"`
	// TxWrap deterministically repairs a combined fragment body that fails
	// the transaction gate: when the unit's store calls take tx but the
	// fragments omitted the ExecTransaction wrapper (the systematic stitch
	// gap), the repair threads the tx handle through every tx call site
	// and wraps the body in the wrapper the gate names. Repair-only: it
	// runs solely on would-fail gate output and is re-gated before use —
	// passing bodies are never rewritten. Nil (default) enables it; false
	// keeps the loud combined-gate failure for gate-behavior A/B runs.
	TxWrap *bool `yaml:"txWrap"`
	// RetryRepair is the retry-methodology A/B switch (default off): false
	// = a rejected attempt regenerates from the original prompt; true = the
	// rejected payload rides back as an assistant turn and the model
	// patches it. Compare the two with `tuxconv retrystats <auditA> <auditB>`.
	RetryRepair *bool `yaml:"retryRepair"`
}

// ValidateCfg configures the bounded gofmt/build/vet/test retry loop (G6)
// and its two tiers (plan-conversion §2): Tier A (parse + gofmt) always runs;
// Tier B (build/vet/test in the target module) runs when paths.mainGo
// resolves — compile: auto follows presence, always errors when the target
// is missing, never skips Tier B even when it could run.
type ValidateCfg struct {
	MaxRetries int    `yaml:"maxRetries"`
	Compile    string `yaml:"compile"`
	Run        bool   `yaml:"run"`
}

// Paths are the run's artifact roots, relative to the config file. Logs and
// audit are deliberately NOT configurable: `conversion_logs/logs` and
// `conversion_logs/audit` are load-bearing conventions (gitignore, PRDs,
// tooling) owned by the `-log-dir` global flag and the hardcoded audit root.
type Paths struct {
	// Target is RESERVED — no engine reads it (the convert target comes
	// from the CLI positional / convert.input; Tier-B anchors on MainGo).
	// Kept so existing configs load; defaulted to the documented layout.
	Target string `yaml:"target"`
	// MainGo is the target service's main.go (or any file inside the module)
	// — the anchor Tier-B validation walks up from to the go.mod. Empty means
	// the target service is absent on this machine: conversion degrades to
	// syntax-only validation and stages generated code under paths.staged
	// (the two-laptop constraint, plan-conversion §2).
	MainGo string `yaml:"mainGo"`
	Ledger string `yaml:"ledger"`
	State  string `yaml:"state"`
	Staged string `yaml:"staged"`
}
