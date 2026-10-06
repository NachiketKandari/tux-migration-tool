package convert

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/gen"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
)

// Deterministic no-llm bodies (gen.DeterministicControllerBody /
// DeterministicFnHelperBody) land through the same append/validate/ledger
// path as LLM output, minus the seam: the body is synthesized, gated with
// the parse-level subset of the seam gates, and recorded as skipped (an
// LLM-enabled resume upgrades exactly these marker-carrying methods).

// deterministicController renders and gates one endpoint's deterministic
// body. Any error degrades to the legacy skip — a deterministic gap must
// never fail a --no-llm run.
func deterministicController(ctx context.Context, opts Options, svc *gen.Service, unitName string) (string, error) {
	body, err := svc.DeterministicControllerBody(unitName, opts.Plan)
	if err != nil {
		return "", err
	}
	c := svc.ConditionOf(unitName)
	if c == nil {
		return "", fmt.Errorf("convert: no condition for endpoint %s", unitName)
	}
	_, calls, err := svc.BranchCalls(c, opts.Plan)
	if err != nil {
		return "", err
	}
	if errs := detControllerGates(opts, body, c, calls); len(errs) > 0 {
		telemetry.Log(ctx).Warn("deterministic controller failed its gates — leaving skipped",
			"unit", unitName, "errors", strings.Join(errs, "; "))
		return "", fmt.Errorf("convert: deterministic body for %s failed its gates: %s", unitName, strings.Join(errs, "; "))
	}
	return body, nil
}

// detControllerGates runs the parse-level subset of the seam gates over a
// deterministic body: parse/shape, required calls (from the resolved call
// map, not a prompt view), tuxedo, tx, and the empty-if transparency gate.
// The condition-census gate stays LLM-only (no draft is rendered here).
func detControllerGates(opts Options, body string, c *ir.Condition, calls map[string]budget.DBCall) []string {
	var errs []string
	errs = append(errs, validateBody(opts, body)...)
	receiver := receiverOf(opts)
	for _, want := range detRequiredStoreCalls(calls, receiver) {
		if !strings.Contains(body, want) {
			errs = append(errs, "orchestration contract: "+strings.TrimSuffix(want, "(")+" missing from the deterministic body")
		}
	}
	for _, want := range detRequiredHelpers(opts, c) {
		if !strings.Contains(body, want) {
			errs = append(errs, "orchestration contract: "+strings.TrimSuffix(want, "(")+" missing from the deterministic body")
		}
	}
	errs = append(errs, controllerTuxedoErrs(body)...)
	errs = append(errs, txGateErrs(body, calls)...)
	errs = append(errs, emptyIfErrs(body)...)
	return errs
}

// deterministicFnHelper renders and gates one fn helper's deterministic
// method. Any error degrades to the legacy skip.
func deterministicFnHelper(ctx context.Context, opts Options, svc *gen.Service, goName string) (string, error) {
	body, err := svc.DeterministicFnHelperBody(goName, opts.Plan)
	if err != nil {
		return "", err
	}
	h, ok := fnHelperOf(opts.Plan, goName)
	if !ok {
		return "", fmt.Errorf("convert: no fn record for helper %s", goName)
	}
	calls, err := svc.StoreCallsFor(detHelperQueryIDs(opts.Plan, goName), opts.Plan)
	if err != nil {
		return "", err
	}
	structName := common.LowerFirst(svc.Mapping.Service) + "Controller"
	view := detSynthView(calls)
	if errs := fnHelperGate(body, h, structName, view, receiverOf(opts)); len(errs) > 0 {
		telemetry.Log(ctx).Warn("deterministic fn helper failed its gates — leaving skipped",
			"unit", goName, "errors", strings.Join(errs, "; "))
		return "", fmt.Errorf("convert: deterministic helper %s failed its gates: %s", goName, strings.Join(errs, "; "))
	}
	return body, nil
}

// detHelperQueryIDs returns the plan unit's query IDs for a fn helper.
func detHelperQueryIDs(p *plan.Plan, goName string) []string {
	for _, u := range p.Units {
		if u.Kind == plan.KindFnHelper && u.Name == goName {
			return u.QueryIDs
		}
	}
	return nil
}

// detControllerState reports whether the controller file carries a live
// method for the unit and whether that method is a deterministic no-llm
// body (the LLM-upgrade signal).
func detControllerState(path, name string) (landed, deterministic bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	src := string(data)
	return hasLiveMethod(src, name), gen.IsDeterministicControllerMethod(src, name)
}

// detControllerLanded reports whether the controller file's live method
// for the unit is deterministic.
func detControllerLanded(path, name string) bool {
	_, det := detControllerState(path, name)
	return det
}

// detFnHelperState is the fn-helper counterpart of detControllerState.
func detFnHelperState(path, name string) (landed, deterministic bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	src := string(data)
	return hasLiveMethod(src, name), gen.IsDeterministicFnHelper(src, name)
}

// detFnHelperLanded reports whether the fn file's live method for the
// helper is deterministic.
func detFnHelperLanded(path, name string) bool {
	_, det := detFnHelperState(path, name)
	return det
}

// stripMethodByName removes the live method named name from a Go source
// file (offsets from the parse tree — no brace counting). It lets an
// LLM-accepted body replace a deterministic placeholder instead of stacking
// a duplicate; formatting is normalized downstream by goast.Emit.
func stripMethodByName(src, name string) (string, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ctrl.go", src, parser.SkipObjectResolution)
	if err != nil {
		return src, false
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != name {
			continue
		}
		start := fset.Position(fd.Pos()).Offset
		end := fset.Position(fd.End()).Offset
		if start < 0 || end > len(src) || start >= end {
			return src, false
		}
		rest := strings.TrimLeft(src[end:], "\n")
		head := strings.TrimRight(src[:start], " \t")
		return head + rest, true
	}
	return src, false
}

// detRequiredStoreCalls lists the `recv.Name(` spellings every store call
// contributes (deterministic order).
func detRequiredStoreCalls(calls map[string]budget.DBCall, receiver string) []string {
	var names []string
	seen := map[string]bool{}
	for _, call := range calls {
		recv := call.Receiver
		if recv == "" {
			recv = strings.TrimSuffix(receiver, ".")
		}
		want := recv + "." + call.Name + "("
		if !seen[want] {
			seen[want] = true
			names = append(names, want)
		}
	}
	sort.Strings(names)
	return names
}

// detRequiredHelpers lists the `s.GoName(` spellings the branch's
// same-file helper calls contribute: plan helpers whose legacy name the
// branch source calls.
func detRequiredHelpers(opts Options, c *ir.Condition) []string {
	if opts.Plan == nil || c == nil {
		return nil
	}
	src := branchSource(opts.Source, c.StartLine, c.EndLine)
	var out []string
	seen := map[string]bool{}
	for _, h := range opts.Plan.FnHelpers {
		if !strings.Contains(src, h.Name+"(") {
			continue
		}
		want := "s." + h.GoName + "("
		if !seen[want] {
			seen[want] = true
			out = append(out, want)
		}
	}
	sort.Strings(out)
	return out
}

// detSynthView renders the synthetic view the fn-helper required-call gate
// consumes: one call line per resolved store call (the gate extracts the
// same required set the synthesizer emitted).
func detSynthView(calls map[string]budget.DBCall) string {
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, call.Line())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// detMethodSource extracts the live method named name from a gofmt-emitted
// Go source file ("" when absent). Line-anchored like hasLiveMethod —
// commented rejected placeholders never match — and it degrades to "" on
// hand-edited (non-gofmt) files rather than guessing boundaries.
func detMethodSource(src, name string) string {
	lines := strings.Split(src, "\n")
	start := -1
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "func (") {
			continue
		}
		if !strings.Contains(ln, ") "+name+"(") {
			continue
		}
		start = i
		break
	}
	if start < 0 {
		return ""
	}
	end := -1
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			end = i
			break
		}
	}
	if end < 0 {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// detSkeletonBody reduces a landed deterministic method to the bare
// statements the seam OUTPUT rule demands: the fixed signature line and
// the closing brace go (the signature is already declared — re-emitting
// the wrapper teaches the model the wrong output shape and the gates
// reject it). The marker + TODO comments stay as the edit map.
func detSkeletonBody(method string) string {
	lines := strings.Split(strings.TrimRight(method, "\n"), "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "func (") {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && lines[len(lines)-1] == "}" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// detSkeletonSection renders the deterministic-baseline prompt section for
// one upgrade unit: the landed deterministic body plus the edit contract
// (improve, don't rebuild; the blank-identifier TODO idiom is the legal
// residue for unmappable results). Empty input renders nothing — the prompt
// stays exactly as before.
func detSkeletonSection(method string) string {
	body := detSkeletonBody(method)
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nDeterministic baseline (compiles; satisfies every REQUIRED CALL) — improve these bare statements, do not rebuild them:\n```go\n")
	sb.WriteString(body)
	sb.WriteString("\n```\nThe method signature is fixed and already declared — emit bare statements only, no func wrapper. Replace each // tuxconv:TODO residue with the real legacy mapping or branch logic. Keep every store call with exact params/order and every capture name. A store result whose branch role you cannot map stays kept as _ = name // tuxconv:TODO <role> — never dropped, never left unused.\n")
	return sb.String()
}

// detSkeletonFor attaches the landed deterministic method for name (when
// any) to the upgrade prompt — budget-gated: the section rides only when
// prompt+section still fits MaxPromptTokens (0 = unbounded), so a huge
// endpoint degrades to the view-only prompt instead of tripping the
// oversized-endpoint wiring. isDet is detControllerLanded/detFnHelperLanded.
func detSkeletonFor(ctx context.Context, opts Options, prompt, path, name string, isDet func(string, string) bool) string {
	if path == "" || !isDet(path, name) {
		return prompt
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return prompt
	}
	section := detSkeletonSection(detMethodSource(string(raw), name))
	if section == "" {
		return prompt
	}
	if max := opts.Budget.MaxPromptTokens; max > 0 && opts.Budget.Count(prompt+section) > max {
		telemetry.Log(ctx).Info("deterministic baseline omitted from upgrade prompt — over budget",
			"unit", name, "prompt_tokens", opts.Budget.Count(prompt),
			"section_tokens", opts.Budget.Count(section), "max", max)
		return prompt
	}
	telemetry.Log(ctx).Info("deterministic baseline attached to upgrade prompt", "unit", name)
	return prompt + section
}
