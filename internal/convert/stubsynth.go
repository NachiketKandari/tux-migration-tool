package convert

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/goast"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/validate"
)

// Stub synthesis (2026-09-16): unresolved external fns (plan.Stubs) render
// into controller/fnstubs.go as panicking placeholders by default
// (stub-and-carry-on). Before that render, each stub gets one best-effort
// LLM attempt in its own seam call: the call shows the inferred input/output
// signature plus every call-site line, and the model either implements the
// helper as real idiomatic Go or explicitly declines with
// CANNOT_SYNTHESIZE. A decline — or any gate failure — keeps the panicking
// stub, never a guessed body. Deterministic-only runs (SkipLLM / nil client)
// skip synthesis silently so -no-llm output is unchanged.

// declineMarker is the model's explicit "not implementable" verdict. The
// gate accepts it without retry; anything else must pass the purity gate.
const declineMarker = "CANNOT_SYNTHESIZE"

// stubSynthSystem is the one-shot seam's fixed policy: pure-Go helpers only,
// the transpiler's int-status convention, no new surface.
const stubSynthSystem = `You implement one unresolved legacy Pro*C/Tuxedo helper as ONE idiomatic Go function.
Rules:
- Emit ONLY one complete Go func declaration with the fixed name given — no package clause, no imports, no extra helpers or types.
- Keep the inferred parameter order and count; fix a parameter TYPE only when the call sites clearly demand it. An argument the legacy passes by address (&x, an out-param) stays a pointer parameter.
- Keep the legacy int status return: -1 on the failure paths, the legacy success value otherwise. Never add a Go error return.
- Pure logic only: strconv, strings, time, unicode and plain control flow. Never SQL, never Tuxedo/atmi/FML/userlog/errlog calls, never logging statements, never panic.
- Translate the C idiom, don't transliterate it: C strings become Go strings, manual length loops become strconv/strings, every declared parameter is used (blank-assign what the logic genuinely ignores).
- The function must be gofmt-clean and parse as written.
- When the helper needs SQL, Tuxedo/FML, file/network I/O, or its semantics are undecidable from the call sites shown, do NOT guess — reply with exactly: CANNOT_SYNTHESIZE: <one-line reason>`

// stubCall is one call-site line of an unresolved fn.
type stubCall struct {
	line int
	text string
	args []string
}

// stubEvidence is what the seam shows the model for one stub: the fixed Go
// name, the inferred signature, every call-site line, and the return-usage
// convention read off those lines.
type stubEvidence struct {
	fn         string
	goName     string
	calls      []stubCall
	signature  string
	returnNote string
	hostTypes  []string // "name ctype" declarations backing the inference
}

// synthesizeStubs attempts one LLM synthesis per plan stub and returns the
// accepted bodies keyed by legacy fn name, plus a short per-fn mark for the
// run summary ("synthesized" or "stubbed: <reason>"). Best-effort by
// contract: any decline, gate failure, or transport error keeps the
// panicking stub and never fails the run. One shot per stub (MaxRetries 0)
// — controllers own the retry budget; stubs must not multiply LLM calls.
func synthesizeStubs(ctx context.Context, opts Options, res *Result) (map[string]string, map[string]string) {
	bodies := map[string]string{}
	marks := map[string]string{}
	if len(opts.Plan.Stubs) == 0 || opts.SkipLLM || opts.Client == nil {
		return bodies, marks
	}
	hostIdx := hostTypeIndex(opts.Main)
	srcLines := strings.Split(opts.Source, "\n")
	for _, st := range opts.Plan.Stubs {
		ev := collectStubEvidence(opts.Main, srcLines, hostIdx, st.Fn)
		if len(ev.calls) == 0 {
			marks[st.Fn] = "stubbed: no call sites in source"
			continue
		}
		prompt := buildStubPrompt(ev)
		accepted, calls, notes, err := llm.RunSeam(ctx, llm.SeamInput{
			Unit: "fnstub", Kind: "fn_stub_synth", Name: ev.goName,
			Template: "fn_stub_file", LLM: true, Repair: opts.RetryRepair,
			Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: 0,
			Temperature: 0.1,
			Prompt: func(_ string, attemptNotes []string) (string, []llm.Message) {
				msgs := []llm.Message{
					{Role: "system", Content: stubSynthSystem},
					{Role: "user", Content: prompt},
				}
				return prompt, msgs
			},
			Extract: cleanBody,
			Gate: func(body string) []string {
				return stubSynthGate(body, ev.goName)
			},
		})
		res.LLMCalls += calls
		switch {
		case err == nil && isDeclined(accepted):
			reason := strings.TrimSpace(strings.TrimPrefix(accepted, declineMarker))
			reason = strings.TrimPrefix(reason, ":")
			marks[st.Fn] = "stubbed: LLM declined" + shortSuffix(reason)
			telemetry.Log(ctx).Info("stub synthesis declined", "fn", st.Fn, "reason", reason)
		case err == nil:
			bodies[st.Fn] = accepted
			marks[st.Fn] = "synthesized"
			telemetry.Log(ctx).Info("stub synthesized", "fn", st.Fn, "callsites", len(ev.calls))
		default:
			marks[st.Fn] = "stubbed: synthesis rejected"
			telemetry.Log(ctx).Warn("stub synthesis failed — keeping panic stub",
				"fn", st.Fn, "error", strings.Join(notes, "; "))
		}
	}
	return bodies, marks
}

// collectStubEvidence reads one stub's call sites straight from the entry
// source (the IR keeps call-site lines, not argument text) and infers a Go
// signature from the argument shapes plus the file's host declarations.
func collectStubEvidence(main *ir.File, srcLines []string, hostIdx map[string]ir.HostVar, fn string) stubEvidence {
	ev := stubEvidence{fn: fn, goName: common.CamelLowerGo(fn)}
	var sites []int
	for _, ext := range main.ExternalFns {
		if ext.Name == fn {
			sites = ext.Callsites
			break
		}
	}
	seen := map[string]bool{}
	for _, ln := range sites {
		if ln < 1 || ln > len(srcLines) {
			continue
		}
		text := strings.TrimSpace(srcLines[ln-1])
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		ev.calls = append(ev.calls, stubCall{line: ln, text: text, args: extractCallArgs(text, fn)})
	}
	if len(ev.calls) == 0 {
		return ev
	}
	// Signature inference uses the call with the most arguments (overloads
	// do not exist in the corpus; arity disagreements surface in the
	// prompt as multiple call-site lines for the model to reconcile).
	best := ev.calls[0]
	for _, c := range ev.calls[1:] {
		if len(c.args) > len(best.args) {
			best = c
		}
	}
	params := make([]string, 0, len(best.args))
	usedNames := map[string]bool{}
	for i, raw := range best.args {
		name, typ := inferStubParam(raw, i, hostIdx)
		if usedNames[name] {
			name = fmt.Sprintf("p%d", i)
		}
		usedNames[name] = true
		params = append(params, name+" "+typ)
		if h, ok := lookupHost(hostIdx, baseIdentOf(raw)); ok {
			ev.hostTypes = append(ev.hostTypes, h.Name+" "+strings.TrimSpace(h.CType))
		}
	}
	ev.signature = "func " + ev.goName + "(" + strings.Join(params, ", ") + ") int"
	ev.returnNote = returnUsageNote(ev.calls)
	return ev
}

// buildStubPrompt renders the one-shot user message: fixed name, inferred
// signature, every call-site line, host declarations, return convention.
func buildStubPrompt(ev stubEvidence) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Unresolved legacy helper — decide if it is implementable as pure Go, and if so implement it.\n\n")
	fmt.Fprintf(&sb, "Legacy symbol: %s\nGo name (fixed — use exactly): %s\n\n", ev.fn, ev.goName)
	fmt.Fprintf(&sb, "Call sites (%d):\n", len(ev.calls))
	for _, c := range ev.calls {
		fmt.Fprintf(&sb, "  L%d: %s\n", c.line, c.text)
	}
	sb.WriteString("\nInferred signature (from call sites + host declarations — fix a TYPE only when the call sites clearly demand it; never reorder, add, or drop parameters):\n  " + ev.signature + "\n\n")
	if len(ev.hostTypes) > 0 {
		sb.WriteString("Host declarations:\n")
		for _, h := range ev.hostTypes {
			sb.WriteString("  " + h + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Return convention: " + ev.returnNote + "\n\n")
	sb.WriteString("Reply with exactly `" + declineMarker + ": <reason>` when the helper needs SQL, Tuxedo/FML, I/O, or is otherwise undecidable. Otherwise emit ONLY the complete Go func declaration.")
	return sb.String()
}

// stubSynthGate validates one synthesis attempt: the decline marker passes
// through, anything else must be a parse-clean, panic-free, SQL-free func
// with the fixed name and an int return.
func stubSynthGate(body, goName string) []string {
	if isDeclined(body) {
		return nil
	}
	var errs []string
	if !strings.Contains(body, "func "+goName+"(") {
		errs = append(errs, "the function must be declared with the fixed name: func "+goName+"(...) — emit one complete Go func, nothing else")
	}
	if _, ferr := goast.Emit("convert: stub synthesis", "package controller\n\n"+body); ferr != nil {
		errs = append(errs, validate.TrimGoErrors(ferr.Error())...)
	}
	if strings.Contains(body, "panic(") {
		errs = append(errs, "synthesized helpers must implement the logic — never panic")
	}
	if loc := sqlTuxedoRef.FindString(body); loc != "" {
		errs = append(errs, "synthesized helpers must be pure Go — no SQL/Tuxedo/FML reference ("+loc+")")
	}
	if !stubIntReturn.MatchString(body) {
		errs = append(errs, "the function must keep the legacy int status return (func "+goName+"(...) int)")
	}
	return errs
}

var (
	sqlTuxedoRef  = regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|MERGE|CURSOR|EXEC\s+SQL|tpcall|tpacall|tpgetrply|Fadd32|Fget32|FNOTPRES|tpalloc|tpfree|tpreturn|userlog|errlog|sqlca)\b`)
	stubIntReturn = regexp.MustCompile(`\)\s*int\s*\{`)
)

func isDeclined(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), declineMarker)
}

func shortSuffix(reason string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		if len(reason) > 80 {
			reason = reason[:80] + "…"
		}
		return ": " + reason
	}
	return ""
}

// returnUsageNote reads the failure-check convention off the call-site
// lines (the corpus checks `== -1`).
func returnUsageNote(calls []stubCall) string {
	for _, c := range calls {
		switch {
		case strings.Contains(c.text, "== -1"):
			return "int status; -1 = failure (call sites check `== -1`), non-negative = success — keep it"
		case strings.Contains(c.text, "!= -1"), strings.Contains(c.text, "!= 0"):
			return "int status; -1/0 compared at call sites — keep the -1-on-failure convention"
		}
	}
	return "int status; -1 = failure, non-negative = success (corpus convention) — keep it"
}

// inferStubParam maps one raw call argument to a Go parameter name and type.
// Address-of (&x) marks an out-param → pointer type. Host declarations win;
// C-name prefixes (c_/i_/l_/d_) and literal shapes are the fallback. The
// model sees the result and fixes types — inference stays honest, never
// inventive.
func inferStubParam(raw string, i int, hostIdx map[string]ir.HostVar) (string, string) {
	t := strings.TrimSpace(raw)
	out := strings.HasPrefix(t, "&")
	base := baseIdentOf(t)
	name := fmt.Sprintf("p%d", i)
	if base != "" {
		name = common.CamelLowerGo(strings.ToLower(base))
	}
	typ := ""
	if h, ok := lookupHost(hostIdx, base); ok {
		typ = goTypeOf(strings.ToLower(strings.TrimSpace(h.CType)), strings.TrimSpace(h.GoHint))
	} else {
		typ = guessTypeOf(base, t)
	}
	if typ == "" {
		typ = "string"
	}
	if out && !strings.HasPrefix(typ, "*") && !strings.HasPrefix(typ, "[]") {
		typ = "*" + typ
	}
	return name, typ
}

func lookupHost(idx map[string]ir.HostVar, base string) (ir.HostVar, bool) {
	if base == "" {
		return ir.HostVar{}, false
	}
	h, ok := idx[strings.ToLower(base)]
	return h, ok
}

func hostTypeIndex(main *ir.File) map[string]ir.HostVar {
	idx := map[string]ir.HostVar{}
	if main == nil {
		return idx
	}
	for _, h := range main.HostVars {
		idx[strings.ToLower(h.Name)] = h
	}
	return idx
}

// goTypeOf maps a C declaration type (plus the IR's Go hint when present)
// to the transpiler's Go scalar.
func goTypeOf(ctype, goHint string) string {
	if goHint != "" && isBareGoType(goHint) {
		return goHint
	}
	switch {
	case strings.Contains(ctype, "char"):
		return "string"
	case strings.Contains(ctype, "double"), strings.Contains(ctype, "float"):
		return "float64"
	case strings.Contains(ctype, "long"):
		return "int64"
	case strings.Contains(ctype, "short"):
		return "int"
	case strings.Contains(ctype, "int"):
		return "int"
	case strings.Contains(ctype, "time"):
		return "string"
	default:
		return ""
	}
}

// guessTypeOf falls back to literal shapes and the corpus's Hungarian
// prefixes (c_ char, i_ int, l_ long, d_ double) for undeclared names.
func guessTypeOf(base, raw string) string {
	t := strings.TrimSpace(raw)
	if len(t) >= 2 && ((t[0] == '"' && t[len(t)-1] == '"') || (t[0] == '\'' && t[len(t)-1] == '\'')) {
		return "string"
	}
	if isNumLit(strings.TrimPrefix(t, "&")) {
		return "int64"
	}
	lb := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lb, "c_"):
		return "string"
	case strings.HasPrefix(lb, "l_"):
		return "int64"
	case strings.HasPrefix(lb, "i_"), strings.HasPrefix(lb, "n_"):
		return "int"
	case strings.HasPrefix(lb, "d_"), strings.HasPrefix(lb, "f_"):
		return "float64"
	default:
		return ""
	}
}

func isNumLit(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "+")
	dots := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
		case s[i] == '.' && dots == 0:
			dots++
		default:
			return false
		}
	}
	return true
}

func isBareGoType(s string) bool {
	switch s {
	case "string", "int", "int64", "float64", "bool",
		"*string", "*int", "*int64", "*float64", "time.Time":
		return true
	default:
		return false
	}
}

// baseIdentOf strips casts and address-of, returning the leading identifier
// ("" for literals/complex expressions) — the local mirror of ir.baseIdent,
// which is unexported.
func baseIdentOf(arg string) string {
	s := strings.TrimSpace(arg)
	for strings.HasPrefix(s, "(") {
		if end := strings.IndexByte(s, ')'); end > 0 {
			s = strings.TrimSpace(s[end+1:])
		} else {
			break
		}
	}
	s = strings.TrimLeft(s, "& \t")
	end := 0
	for end < len(s) && isStubIdentByte(s[end]) {
		end++
	}
	return s[:end]
}

func isStubIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// extractCallArgs pulls the top-level comma-separated arguments of fn's call
// on one source line (""- and ”-aware, paren/bracket-depth-aware). It
// returns nil when the line carries no balanced call — the prompt then shows
// the raw line alone.
func extractCallArgs(line, fn string) []string {
	idx := strings.Index(line, fn)
	if idx < 0 {
		return nil
	}
	open := strings.Index(line[idx+len(fn):], "(")
	if open < 0 {
		return nil
	}
	start := idx + len(fn) + open + 1
	depth := 1
	quote := byte(0)
	for i := start; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return splitStubArgs(line[start:i])
			}
		}
	}
	return nil
}

// splitStubArgs splits raw argument text on top-level commas.
func splitStubArgs(args string) []string {
	var out []string
	depth := 0
	quote := byte(0)
	start := 0
	for i := 0; i < len(args); i++ {
		c := args[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(args[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}
