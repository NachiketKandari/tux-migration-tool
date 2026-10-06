// Package validate implements the two-tier validation design
// (plan-conversion §2): Tier A — stdlib go/parser + gofmt idempotence — runs
// on every generated file on every laptop; Tier B — go build / go vet /
// go test (and an optional smoke run) — runs only where the target Go
// service exists, anchored by paths.mainGo. A missing target degrades to
// Tier A with a WARN: the environment is never a run failure unless
// validate.compile is "always".
package validate

import (
	"context"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options carries the validator wiring from .tuxconv.yaml.
type Options struct {
	MainGo   string // a file inside the target module; "" = Tier A only
	Compile  string // auto | always | never
	RunSmoke bool   // `go run .` after a green Tier B
}

// Result is one validation outcome.
type Result struct {
	Tier          string // "syntax" | "compile"
	OK            bool
	Errors        []string // trimmed, ready for a bounded retry loop
	DegradeReason string   // why Tier B was skipped ("" when it ran)
	Summary       string   // one-line human summary for the ledger
}

// Validator executes the validation tiers.
type Validator struct {
	opts Options
}

// New returns a Validator for the run's options.
func New(opts Options) *Validator { return &Validator{opts: opts} }

// ResolveModuleRoot walks up from anchor (a file inside the module) to the
// nearest go.mod — the Tier-B working directory.
func ResolveModuleRoot(anchor string) (string, error) {
	if anchor == "" {
		return "", fmt.Errorf("validate: no anchor file")
	}
	abs, err := filepath.Abs(anchor)
	if err != nil {
		return "", fmt.Errorf("validate: resolve %s: %w", anchor, err)
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		abs = filepath.Join(abs, "main.go") // allow directory anchors
	}
	dir := filepath.Dir(abs)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("validate: no go.mod above %s", anchor)
		}
		dir = parent
	}
}

// TierBEnabled reports whether the compile tier can run and, when it cannot
// under mode "auto", why (the degrade reason surfaced as one WARN).
func (v *Validator) TierBEnabled() (bool, string) {
	switch v.opts.Compile {
	case "never":
		return false, "validate.compile is \"never\""
	case "always":
		if _, err := ResolveModuleRoot(v.opts.MainGo); err != nil {
			return false, fmt.Sprintf("validate.compile is \"always\" but the target is missing: %v", err)
		}
		return true, ""
	default: // auto
		if v.opts.MainGo == "" {
			return false, "paths.mainGo is empty — the target service is absent on this machine; validating syntax-only"
		}
		if _, err := ResolveModuleRoot(v.opts.MainGo); err != nil {
			return false, fmt.Sprintf("paths.mainGo %q does not resolve to a module: %v — validating syntax-only", v.opts.MainGo, err)
		}
		return true, ""
	}
}

// Syntax runs Tier A on one generated Go file: it must parse and be gofmt
// idempotent. Everything else about the file is untouched.
func (v *Validator) Syntax(path string) Result {
	res := Result{Tier: "syntax"}
	src, err := os.ReadFile(path)
	if err != nil {
		res.Summary = "unreadable"
		res.Errors = append(res.Errors, fmt.Sprintf("read %s: %v", path, err))
		return res
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution); err != nil {
		res.Errors = append(res.Errors, TrimGoErrors(err.Error())...)
	}
	formatted, ferr := format.Source(src)
	if ferr != nil {
		res.Errors = append(res.Errors, TrimGoErrors(ferr.Error())...)
	} else if !strings.HasSuffix(string(src), "\n") || string(formatted) != string(src) {
		res.Errors = append(res.Errors, path+": not gofmt-clean (run gofmt -w)")
	}
	res.OK = len(res.Errors) == 0
	res.Summary = "syntax ok"
	if !res.OK {
		res.Summary = fmt.Sprintf("syntax: %d error(s)", len(res.Errors))
	}
	return res
}

// CompileAll runs Tier B against the target module: build, vet, test, and —
// when opts.RunSmoke — a smoke run. Batched after conversion completes.
// Its Errors surface in the run summary (cmd) — they do NOT feed any
// per-unit retry loop: the LLM seams' gates are per-body structural checks
// (goast/parse), and Tier B has no unit to retry by the time it runs
// (engine-wiring audit Tier-1 #6: the old doc claimed a retry loop no
// code path fed).
func (v *Validator) CompileAll(ctx context.Context) Result {
	res := Result{Tier: "compile"}
	enabled, degrade := v.TierBEnabled()
	if !enabled {
		res.DegradeReason = degrade
		res.Summary = "skipped: " + degrade
		res.OK = v.opts.Compile != "always" // "always" + missing target is an error state
		return res
	}
	root, err := ResolveModuleRoot(v.opts.MainGo)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		res.Summary = "module resolution failed"
		return res
	}
	steps := []struct {
		name string
		args []string
	}{
		{"build", []string{"build", "./..."}},
		{"vet", []string{"vet", "./..."}},
		{"test", []string{"test", "./..."}},
	}
	if v.opts.RunSmoke {
		steps = append(steps, struct {
			name string
			args []string
		}{"run", []string{"run", "."}})
	}
	for _, step := range steps {
		out, err := goCmd(ctx, root, step.args...)
		if err != nil {
			res.Errors = append(res.Errors, TrimGoErrors(fmt.Sprintf("go %s: %s", step.name, out))...)
			res.Summary = fmt.Sprintf("go %s failed", step.name)
			res.OK = false
			return res
		}
	}
	res.OK = true
	res.Summary = "build/vet/test ok"
	return res
}

// goCmd runs one go toolchain command in dir and returns combined output.
func goCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TrimGoErrors keeps compiler-shaped lines from a parse/build error,
// bounded so retry prompts stay small (G6). Exported as the one bounded
// error-trimmer — the convert path's byte-twin copy was deleted (A3.3).
func TrimGoErrors(out string) []string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		kept = append(kept, line)
		if len(kept) >= 40 {
			kept = append(kept, "… (output trimmed)")
			break
		}
	}
	return kept
}
