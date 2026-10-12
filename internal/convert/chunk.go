package convert

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/gen"
	"tux-migration-tool/internal/goast"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/ledger"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
	"tux-migration-tool/internal/validate"
)

// Chunked controller generation (2026-09-13, user directive): when one
// endpoint's assembled prompt exceeds the budget ceiling, the legacy view is
// split into contiguous statement-balanced fragments sized per chunk; each
// fragment is one bounded-retry llm.RunSeam call (audit artifacts suffixed
// #chunk<k>); the accepted fragments concatenate into the method body the
// full REQUIRED-CALLS + parse gates judge. Deterministic chunking — the LLM
// only translates a fragment, never decides where it ends.

// chunkSafetyPct keeps each chunk's estimated tokens at this share of the
// prompt ceiling: the chars/token estimator undercounts dense C code (a
// dense monolith slice measured ~2.8 chars/token at the provider), so the
// margin absorbs the estimation error.
const chunkSafetyPct = 70

// outputExpansionPct is the Go-translation inflation over the legacy view's
// own token estimate: the Go body carries the same statements plus error
// checks per store call and struct-literal shaping per output field, while
// the Pro*C error choreography (errlog/Fadd/tpfree/tpreturn) shrinks to
// `if err != nil`. Calibrated on the 2026-09-14 dense-C run — the F branch's
// completion measured ~1.2–1.4× its post-replacement view's tokens — 130 is
// mid-range and errs toward splitting earlier.
const outputExpansionPct = 130

// outputTriggerPct routes to fragments when the estimate reaches this share
// of the output ceiling: the chars/token ratio is configured, not measured,
// and the provider's tokenizer on dense C code ran ~2.8 chars/token against
// the default 4 (2026-09-14 dense-C run — the estimate read 3360 of 4000,
// the call truncated at exactly 4000). Splitting a branch that would have
// fit costs one extra call; a truncated body costs the call plus every
// retry — the margin errs the same way. dummy.pc evidence (2026-09-17: a
// single-shot body estimated under the 80% line still truncated at the
// 4096 cap) tightened the margin to 70%.
const outputTriggerPct = 70

// outputChunkReason names the output-ceiling split trigger, empty when the
// estimate fits with margin.
func outputChunkReason(b budget.Budget, viewSrc string, maxOutputTokens int) string {
	if maxOutputTokens <= 0 {
		return ""
	}
	est := outputTokenEstimate(b, viewSrc)
	if est > maxOutputTokens*outputTriggerPct/100 {
		return fmt.Sprintf("expected output of ~%d tokens approaches the %d-token ceiling", est, maxOutputTokens)
	}
	return ""
}

// outputTokenEstimate projects the controller body's token size from the
// post-replacement view source: view tokens inflated by the translation
// factor. It drives the output-ceiling routing decision — an estimate over
// MaxOutputTokens routes to the chunked path before a single call can
// truncate (finish_reason=length, gates reject, retries burn).
func outputTokenEstimate(b budget.Budget, viewSrc string) int {
	return b.Count(viewSrc) * outputExpansionPct / 100
}

// minFragmentChars is the smallest slice a chunk may carry; below it the
// scaffolding alone starves the fragment and the run fails loudly instead of
// fanning out hundreds of calls.
const minFragmentChars = 2000

// chunkFramingChars is head-room for the per-chunk framing the single-call
// scaffold cannot see (fragment banner, locals list, filtered sections).
const chunkFramingChars = 600

// chunkCtx carries everything the chunked path needs from controllerBody —
// the single-call path already computed these pieces.
type chunkCtx struct {
	ctx     context.Context
	opts    Options
	res     *Result
	svc     *gen.Service
	unit    plan.Unit
	db      map[string]dbOut
	cond    *ir.Condition
	view    budget.View
	scen    *scenPrompt
	axisVar string // dispatch axis variable (arm-wrapper repair; "" = no scenario)
	prompt  string // the full single-call prompt (base-scaffold accounting)
	calls   map[string]budget.DBCall
	// census is the Workstream B transparency census for the endpoint's
	// condition span (nil when the flow draft is off — scenario slices and
	// draft-off runs). The combined gate requires it; per-fragment gates
	// stay census-free (fragments are partial by construction).
	census []flow.CensusCond
	// guards are the scenario slice's FoldMixed runtime-dispatch guards
	// (S7) and guardBind maps their identifiers to the endpoint's request
	// spellings. Like the census they gate the combined body only — a guard
	// may live in any fragment, so fragments are exempt by construction.
	guards    []flow.MixedGuard
	guardBind *guardBindings
}

// systemPromptFragment is the fragment-framed system prompt: the same rules
// as systemPrompt, scoped to one contiguous fragment of the method body.
// The input is already flattened (every SQL block replaced by its store
// call) — translate that logic into idiomatic Go. Output is code only to
// keep token spend on the body: no prose, no markdown fences, no SQL.
const systemPromptFragment = `You emit the statements of ONE fragment of a larger Go controller method body. Fragments arrive in order, possibly cut mid-block: emit exactly the braces shown. Input is flattened (SQL already store calls); the template owns the wrapper + START/END logs.
OUTPUT: bare Go statements of this fragment only — no wrapper/package/imports/helpers/types, no prose/fences/comments (S-codes live in error text), minimal blank lines. Stay terse: fit the output ceiling. Never re-emit other fragments.
SIGNATURE (fixed, verbatim): ctx, request, named returns data and err. Never req/resp/Response. Never shadow data/err with :=.
INPUTS: request.<Field> exactly as defined.
STORE: every c.store.* call shown appears exactly once, exact params/order, results captured; c.<Name>(...) calls are generated helpers — call as shown. No other c.* calls. Never SQL.
FIELDS: verbatim struct/row names; sql.NullString via row.X.String.
LITERALS (Go only): "Y" never 'Y'; 0 never '\0'; == never =.
ERRORS: after every err-returning call: if err != nil { return nil, err }. A variable error message uses errors.New(msg) — never fmt.Errorf(variable). Return (return data, err / return nil, err) ONLY where this fragment's view shows one; otherwise fall through.
JOINTS: a closing brace closes an earlier fragment's block; an if header open at the end is closed later. Listed earlier-fragment locals are reused, never redeclared; := only on first use in THIS fragment.
LEGACY MAP (intent, never spelling): FML pack (Fadd32) → append shaped rows to data; tpreturn(TPSUCCESS) → return data, err (only where shown); error legs → return nil + S-code; CLOSE/SETNULL/SETLEN/MEMSET/buffer-math/DEBUG userlog → drop; session prologue (Fget32/chk_sssn/tpalloc) → drop. Never emit tpreturn/tpalloc/Fadd32/Fget32/errlog/userlog/EXEC SQL/FBFR32/unsafe.
FLOW: same loops/branches/order; dead-looking branches still implemented; every shown store call appears under its condition.`

// systemPromptComposer merges independently converted fragment bodies into
// one coherent controller method body. The fragments already fit in context
// individually; the composer applies the applicable controller template shape
// across them (single body, one declaration per local, balanced braces) and
// emits code only.
const systemPromptComposer = `You stitch ordered Go fragment bodies into ONE coherent controller method body — merge, don't re-translate. The template owns the wrapper + START/END logs.
OUTPUT: bare merged Go statements only — no wrapper/package/imports/helpers/types, no prose/fences/comments, minimal blank lines. Stay terse: fit the output ceiling.
KEEP: every shown c.store.* call exactly once, same condition and order, exact params, results captured; c.<Name>(...) calls are generated helpers — keep as shown. No other c.* calls. Never SQL.
UNIFY: one declaration per local (reuse earlier, := on first use only); brace joints join without adding/dropping braces; data/err never shadowed with :=.
SIGNATURE: c, request, data and err; return nil, err on error paths, data, err on success — where shown, else fall through.
LEGACY MAP (intent, never spelling): FML pack → append shaped rows to data; tpreturn(TPSUCCESS) → return data, err; error legs → return nil + S-code; CLOSE/SETNULL/buffer bookkeeping/userlog → drop. Never emit tpreturn/tpalloc/Fadd32/Fget32/errlog/userlog/EXEC SQL/FBFR32/unsafe.
FIELDS verbatim; sql.NullString via row.X.String. LITERALS Go-only: "Y", 0 for \0, ==. No logging. Check every error before use. A variable error message uses errors.New(msg), never fmt.Errorf(variable).`

// sliceScanner tracks brace/string/comment state across the flattened view's
// lines so statement boundaries land only at true depth-0 statement ends —
// braces inside string literals ("Folio # is {%s}") and comment text never
// shift the depth.
type sliceScanner struct {
	depth    int
	inBlock  bool // inside a /* */ comment (may span lines)
	inString bool
	inChar   bool
	escaped  bool
	sawCode  bool // the current line carried at least one code character
}

func (sc *sliceScanner) scanLine(line string) {
	sc.sawCode = false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case sc.inBlock:
			if ch == '*' && i+1 < len(line) && line[i+1] == '/' {
				sc.inBlock = false
				i++
			}
		case sc.inString:
			switch {
			case sc.escaped:
				sc.escaped = false
			case ch == '\\':
				sc.escaped = true
			case ch == '"':
				sc.inString = false
			}
		case sc.inChar:
			switch {
			case sc.escaped:
				sc.escaped = false
			case ch == '\\':
				sc.escaped = true
			case ch == '\'':
				sc.inChar = false
			}
		default:
			switch {
			case ch == '/' && i+1 < len(line) && line[i+1] == '*':
				sc.inBlock = true
				i++
			case ch == '/' && i+1 < len(line) && line[i+1] == '/':
				return // line comment — nothing code-shaped after this
			case ch == '"':
				sc.inString = true
			case ch == '\'':
				sc.inChar = true
			case ch == '{':
				sc.depth++
			case ch == '}':
				sc.depth--
			}
			if ch != ' ' && ch != '\t' && ch != '\r' {
				sc.sawCode = true
			}
		}
	}
}

// splitStatements cuts the flattened view into statement units at ANY
// nesting depth: each unit is a maximal run of lines representing one
// statement — ending at its `;` or block-closing `}`, `} else` chain
// continuations attached to their if, unbraced headers glued to their body,
// preprocessor continuations held open. A unit may end inside a braced
// block (mid-block fragments); validateFragment compensates the brace
// delta when parsing. Concatenating the units reproduces the view
// byte-for-byte — scenario views never carry the entry's own brace lines
// (the MethodTemplate owns them), so a trailing block close is real code
// and stays.
func splitStatements(view string) []string {
	lines := strings.Split(view, "\n")
	sc := &sliceScanner{}
	var units []string
	var cur []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		sc.scanLine(line)
		cur = append(cur, line)
		trimmed := strings.TrimSpace(line)
		endsStmt := strings.HasSuffix(trimmed, ";") || strings.HasSuffix(trimmed, "}")
		continuation := strings.HasSuffix(trimmed, "\\")
		if !sc.inBlock && sc.sawCode && endsStmt && !continuation {
			// Lookahead: an else-chain continuation (`} else ...`) belongs
			// to this statement's unit, so the unit stays open. Provenance
			// markers prefix every flattened line (`/*L21716*/}`), so the
			// peek strips them before classifying. Blank lines between the
			// close and the continuation must not break the glue — peek
			// past them, else the next unit starts mid-chain and its
			// fragment makes the model emit a dangling `else`.
			j := i + 1
			for j < len(lines) && strings.TrimSpace(stripLeadingComments(lines[j])) == "" {
				j++
			}
			if j < len(lines) {
				next := stripLeadingComments(lines[j])
				if strings.HasPrefix(next, "}") || strings.HasPrefix(next, "else") {
					continue
				}
			}
			units = append(units, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	if len(cur) > 0 {
		units = append(units, strings.Join(cur, "\n"))
	}
	return units
}

// startsChainContinuation reports whether a statement unit continues an
// if/elseif/else chain opened by an earlier unit: its first code line
// (provenance markers and blanks skipped) opens with `else`, or with `}`
// carrying an `else` on the same line (`} else {`). Such a unit can never
// stand alone — a fragment starting there makes the model emit a dangling
// `else` (audit 2026-09-16: `expected statement, found 'else'`). A lone
// closing `}` (genuine block end, no else) is NOT a continuation: cutting
// before it is the mid-block case bracePads already covers.
func startsChainContinuation(unit string) bool {
	for _, line := range strings.Split(unit, "\n") {
		s := stripLeadingComments(line)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "else") {
			return true
		}
		if strings.HasPrefix(s, "}") && strings.Contains(s, "else") {
			return true
		}
		return false
	}
	return false
}

// glueChainUnits merges chain-continuation units into the unit that opened
// their chain, so groupFragments (which only cuts BETWEEN units) can never
// split an if/elseif/else chain across fragments. Merging preserves the
// byte-for-byte reassembly contract; a merged chain larger than the slice
// budget rides alone and fails loudly downstream like any oversized unit.
func glueChainUnits(units []string) []string {
	var out []string
	for _, u := range units {
		if len(out) > 0 && startsChainContinuation(u) {
			out[len(out)-1] += "\n" + u
			continue
		}
		out = append(out, u)
	}
	return out
}

// stripLeadingComments trims leading block comments (the flattened render's
// `/*L<n>*​/` provenance markers) and whitespace from a line, so prefix
// classification sees the code beneath them.
func stripLeadingComments(line string) string {
	s := strings.TrimSpace(line)
	for strings.HasPrefix(s, "/*") {
		end := strings.Index(s, "*/")
		if end < 0 {
			break
		}
		s = strings.TrimSpace(s[end+2:])
	}
	return s
}

// groupFragments packs statement units into N balanced contiguous chunks
// under the per-chunk slice budget (chars), where N is the minimal count
// that fits. Units are split at statement boundaries into roughly equal
// char mass (total/N per chunk) so fragment outputs are equal-sized: output
// 1, output 2, ... then feed one composer pass whose inputs all fit in
// context. A unit larger than the budget becomes its own oversized chunk —
// the per-chunk budget check then fails loudly with the chunk named, never
// a silent truncation.
func groupFragments(units []string, sliceBudget int, sigChars func(string) int) []string {
	if len(units) == 0 {
		return nil
	}
	if sliceBudget < 1 {
		sliceBudget = 1
	}
	weights := make([]int, len(units))
	total := 0
	for i, u := range units {
		w := len(u) + 1 + sigChars(u)
		weights[i] = w
		total += w
	}
	n := (total + sliceBudget - 1) / sliceBudget
	if n < 1 {
		n = 1
	}
	if n > len(units) {
		n = len(units)
	}
	// Single oversized unit rides alone (callers fail it loudly downstream).
	if len(units) == 1 {
		return []string{units[0]}
	}
	target := total / n
	if target < 1 {
		target = 1
	}
	var chunks []string
	var cur []string
	curChars := 0
	for i, u := range units {
		_ = i
		// Never exceed the hard slice budget when the chunk is non-empty
		// and more chunks remain — the balanced target is soft, the budget
		// is hard. Chunk count stays capped at n via the guard.
		overBudget := len(cur) > 0 && curChars+weights[i] > sliceBudget && len(chunks)+1 < n
		overTarget := len(cur) > 0 && curChars+weights[i] > target && len(chunks)+1 < n
		if overBudget || (overTarget && curChars >= target/2) {
			chunks = append(chunks, strings.Join(cur, "\n"))
			cur = nil
			curChars = 0
		}
		cur = append(cur, u)
		curChars += weights[i]
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "\n"))
	}
	return chunks
}

// fragmentLocals extracts the local names one body's Go declares (:= LHS and
// var statements) so later fragments can reuse them without redeclaring.
// Names are order-stable. Callers dispatching several fragments should use
// fragmentLocalsInto with a running accumulator instead.
func fragmentLocals(body string) []string {
	return fragmentLocalsInto(nil, body)
}

// fragmentLocalsInto appends the local names body declares (:= LHS and var
// statements) to dst, skipping names already in dst. Passing the running
// accumulator means each fragment is scanned once: re-deriving the set from
// strings.Join of every prior body made the dispatch O(chunks² × lines).
func fragmentLocalsInto(dst []string, body string) []string {
	var names []string
	seen := map[string]bool{}
	for _, n := range dst {
		seen[n] = true
	}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, ":=") {
			lhs := strings.TrimSpace(strings.Split(line, ":=")[0])
			lhs = strings.TrimPrefix(lhs, "for ")
			for _, m := range identRe.FindAllString(lhs, -1) {
				add(m)
			}
			continue
		}
		if m := varDeclRe.FindStringSubmatch(line); m != nil {
			add(m[1])
		}
	}
	return append(dst, names...)
}

var (
	identRe   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	varDeclRe = regexp.MustCompile(`^\s*var\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

// filterByPresence keeps only the endpoint-level prompt items (constants
// `NAME = value`, error codes) whose key the chunk's own text mentions —
// the full lists (hundreds of legacy codes in a monolith) would starve
// every chunk.
func filterByPresence(items []string, text string) []string {
	var out []string
	for _, it := range items {
		key := it
		if i := strings.Index(it, " = "); i > 0 {
			key = it[:i]
		}
		if strings.Contains(text, key) {
			out = append(out, it)
		}
	}
	return out
}

// filterHelpers keeps the helper-mapping lines whose fn the chunk text
// calls — either the legacy spelling (external fns) or the rewritten Go
// call (same-file helpers, `fn_x(...) → c.FnX(...)`).
func filterHelpers(helpers []string, text string) []string {
	var out []string
	for _, h := range helpers {
		if helperLineMentions(h, text) {
			out = append(out, h)
		}
	}
	return out
}

// helperLineMentions reports whether the chunk text calls the helper the
// line maps: the legacy fn name, or the Go call after the arrow.
func helperLineMentions(line, text string) bool {
	if fn := strings.SplitN(line, "(", 2)[0]; strings.Contains(text, fn) {
		return true
	}
	arrow := "→ " + common.GoCtrlRecv + "."
	if i := strings.Index(line, arrow); i >= 0 {
		rest := line[i+len("→ "):]
		if j := strings.Index(rest, "("); j > 0 && strings.Contains(text, rest[:j]) {
			return true
		}
	}
	return false
}

// filterStubs keeps the stubbed-helper entries the chunk text calls.
func filterStubs(stubs []plan.Stub, text string) []plan.Stub {
	var out []plan.Stub
	for _, st := range stubs {
		if strings.Contains(text, st.Fn) {
			out = append(out, st)
		}
	}
	return out
}

// buildChunkPrompt assembles one fragment's prompt: controller template +
// flattened logic (DB contract, REQUIRED CALLS, struct definitions), scoped
// to the fragment, plus the fragment framing and the declared-locals
// continuation context. Small by design — convert this flattened logic to
// Go, code only.
func buildChunkPrompt(cx chunkCtx, k, n int, chunkText, dbContract, contract string, helpers, constants, errCodes []string, stubs []plan.Stub, locals []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Endpoint: %s\n\n", cx.unit.Name)
	fmt.Fprintf(&sb, "Fragment %d of %d of one controller body (template owns the wrapper + START/END logs). Convert this flattened logic to Go: emit ONLY this fragment's statements (code only, no fences) — no wrapper, no package lines, no other fragments, no closing `}` for the method.\n\n", k+1, n)
	writePromptFacts(&sb, fragmentWording, cx.scen, k == 0, chunkText, common.GoStoreCallPrefix(), dbContract, contract, helpers, constants, errCodes, stubs)
	if k > 0 && len(locals) > 0 {
		sb.WriteString("Locals already declared by earlier fragments — never redeclare, reuse them: " +
			strings.Join(locals, ", ") + "\n\n")
	}
	fmt.Fprintf(&sb, "Legacy fragment %d of %d (SQL already replaced by store calls):\n\n%s\n", k+1, n, chunkText)
	return sb.String()
}

// controllerBodyChunked runs the oversized-endpoint path: statement-boundary
// chunks, one bounded-retry seam call per chunk, fragment gates, then the
// combined body through the full parse + REQUIRED-CALLS gates. Any chunk or
// the combined gate failing fails the unit loudly with the chunk named; the
// last rejected payload rides back so the caller can keep it visible.
func controllerBodyChunked(cx chunkCtx) (string, string, error) {
	opts := cx.opts
	receiver := receiverOf(opts)
	methods := requiredCalls(cx.view.Source, receiver)
	for i, m := range methods {
		methods[i] = bareStoreCall(m) // contracts match on bare names (single-call parity)
	}
	fullSigs := dbSignaturesFor(opts.Plan, cx.db, methods)

	sigLen := map[string]int{}
	for _, ln := range strings.Split(fullSigs, "\n") {
		if m := storeCallRe(common.GoStoreCallPrefix()).FindStringSubmatch(ln); m != nil {
			sigLen[m[1]] = len(ln) + 1
		}
	}
	helpersAll := legacyHelpers(opts.Plan, cx.view.Source)
	constantsAll := legacyConstants(opts.Main, cx.cond, opts.Main.Entry)
	errCodesAll := legacyErrorCodes(cx.cond)
	contract, err := cx.svc.ControllerPromptContext(cx.unit.Name, opts.Plan, methods)
	if err != nil {
		return "", "", err
	}

	// The per-chunk scaffold is what a chunk's prompt actually carries: the
	// shared contract plus the sections filtered to that chunk's own text
	// (its signature subset, the constants/codes/helpers/stubs it mentions).
	// Grouping budgets fragment + those extras — never the endpoint-level
	// unfiltered lists, which would starve every chunk.
	scenExtras := 0
	if cx.scen != nil {
		for _, tn := range cx.scen.TxNotes {
			scenExtras += len(tn) + 8
		}
	}
	extrasOf := func(unit string) int {
		seen := map[string]bool{}
		total := 0
		for _, call := range requiredCalls(unit, receiver) {
			// sigLen is keyed by bare method name — strip the receiver
			// before the lookup (audit 2026-09-16: prefixed lookups missed
			// every entry and starved the slice budget accounting).
			call = bareStoreCall(call)
			if !seen[call] {
				seen[call] = true
				total += sigLen[call]
			}
		}
		for _, it := range filterByPresence(constantsAll, unit) {
			total += len(it) + 6
		}
		for _, it := range filterByPresence(errCodesAll, unit) {
			total += len(it) + 2
		}
		for _, h := range filterHelpers(helpersAll, unit) {
			total += len(h) + 6
		}
		for _, st := range filterStubs(opts.Plan.Stubs, unit) {
			total += len(st.Fn) + 48
		}
		return total
	}

	chunkTokens := opts.Budget.MaxPromptTokens * chunkSafetyPct / 100
	if chunkTokens < 1 {
		chunkTokens = 1
	}
	chunkChars := chunkTokens * opts.Budget.CharsPerToken
	sliceBudget := chunkChars - len(contract) - scenExtras - chunkFramingChars
	// The translated fragment must also fit the OUTPUT ceiling: a fragment
	// whose Go translation overflows the response cap truncates mid-body
	// (dropping trailing required calls into a gate loop). Cap the slice at
	// 75% of the output budget's char equivalent.
	outputRoom := opts.Budget.MaxOutputTokens * 3 / 4 * opts.Budget.CharsPerToken
	if outputRoom > 0 && sliceBudget > outputRoom {
		sliceBudget = outputRoom
	}
	if sliceBudget < minFragmentChars {
		return "", "", fmt.Errorf("convert: budget: prompt of %d tokens exceeds the %d-token ceiling and the prompt scaffolding leaves no fragment room (output ceiling %d tokens) — trim the mapping or raise run.maxPromptTokens/run.maxOutputTokens",
			opts.Budget.Count(cx.prompt), opts.Budget.MaxPromptTokens, opts.Budget.MaxOutputTokens)
	}

	chunks := groupFragments(glueChainUnits(splitStatements(cx.view.Source)), sliceBudget, extrasOf)
	n := len(chunks)
	telemetry.Log(cx.ctx).Info("fragment plan",
		"unit", cx.unit.Name, "fragments", n, "slice_budget_chars", sliceBudget)
	// Running set of locals declared by the fragments emitted so far. Each
	// fragment appends its own names, so the prompt sees the same accumulated
	// list without re-scanning every prior body.
	var declared []string
	var bodies []string
	for k, chunkText := range chunks {
		telemetry.Log(cx.ctx).Info("fragment dispatch",
			"unit", cx.unit.Name, "fragment", k+1, "of", n, "chars", len(chunkText))
		chunkMethods := requiredCalls(chunkText, receiver)
		dbContract := dbSignaturesFor(opts.Plan, cx.db, chunkMethods)
		prompt := buildChunkPrompt(cx, k, n, chunkText, dbContract, contract,
			filterHelpers(helpersAll, chunkText), filterByPresence(constantsAll, chunkText),
			filterByPresence(errCodesAll, chunkText), filterStubs(opts.Plan.Stubs, chunkText),
			declared)
		fragRej := ""
		fragment, chatCalls, _, err := llm.RunSeam(cx.ctx, llm.SeamInput{
			Unit: cx.unit.ID, Kind: string(cx.unit.Kind), Name: fmt.Sprintf("%s#chunk%d", cx.unit.Name, k+1),
			Template: cx.unit.TemplateID, LLM: cx.unit.LLM, Repair: opts.RetryRepair,
			Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
			Temperature: 0.1,
			Prompt: func(prev string, attemptNotes []string) (string, []llm.Message) {
				return prompt, seamMessages(systemPromptFragment, prompt, prev, fixHintFragment, attemptNotes, opts.RetryRepair)
			},
			Extract: cleanBody,
			Gate: func(body string) []string {
				verr := validateFragment(body)
				verr = append(verr, uncapturedStoreErrs(body, receiver)...)
				verr = append(verr, requiredCallErrs(chunkText, body, receiver)...)
				return append(verr, controllerTuxedoErrs(body)...)
			},
			Rejected: func(payload string) { fragRej = payload },
		})
		opts.Ledger.Get(cx.unit.ID, string(cx.unit.Kind), cx.unit.Name).Attempts += chatCalls
		cx.res.LLMCalls += chatCalls
		if err != nil {
			return "", fragRej, fmt.Errorf("fragment %d/%d: %w", k+1, n, err)
		}
		bodies = append(bodies, fragment)
		// Extend the running local-name set with this fragment's own
		// declarations, so the next prompt carries the same accumulated
		// list the old strings.Join(bodies) re-derivation produced.
		declared = fragmentLocalsInto(declared, fragment)
	}

	combined := strings.Join(bodies, "\n")
	// The fragments passed their own parse-only gates, but the stitch can
	// leave a local the compiler calls unused across fragment scopes — a
	// shape the pipeline owns, not a translation defect. Neutralize it before
	// the combined gate so the seam never spends a retry on it.
	combined = repairUnusedLocals(combined)
	// rejected is the last bad payload the caller keeps visible when the
	// stitch cannot be rescued: the combined body itself, or the repair
	// seam's final rejected payload when one existed.
	rejected := combined
	parseErrs := validateBody(opts, combined)
	reqErrs := requiredCallErrs(cx.view.Source, combined, receiver)
	txErrs := txGateErrs(combined, cx.calls)
	condErrs := emptyIfErrs(combined)
	if len(cx.census) > 0 {
		condErrs = append(condErrs, conditionPresenceErrs(cx.census, combined)...)
	}
	condErrs = append(condErrs, scenarioGuardErrs(cx.guards, cx.guardBind, combined)...)
	repaired := false
	if len(parseErrs) == 0 && len(reqErrs) == 0 && len(condErrs) == 0 && len(txErrs) > 0 && cx.opts.TxWrap {
		// Deterministic tx-wrap repair: the systematic stitch gap —
		// fragments omit the ExecTransaction wrapper the combined gate
		// demands. Repair-only on would-fail output; re-gated below and
		// never bypassed. A successful repair skips the composer pass,
		// which would risk dropping the wrapper again.
		if fixed, ok := wrapTxBody(combined, cx.calls, receiver); ok {
			rerrs := validateBody(opts, fixed)
			rerrs = append(rerrs, requiredCallErrs(cx.view.Source, fixed, receiver)...)
			rerrs = append(rerrs, controllerTuxedoErrs(fixed)...)
			rerrs = append(rerrs, txGateErrs(fixed, cx.calls)...)
			rerrs = append(rerrs, emptyIfErrs(fixed)...)
			if len(cx.census) > 0 {
				rerrs = append(rerrs, conditionPresenceErrs(cx.census, fixed)...)
			}
			rerrs = append(rerrs, scenarioGuardErrs(cx.guards, cx.guardBind, fixed)...)
			if len(rerrs) == 0 {
				telemetry.Log(cx.ctx).Info("tx-wrap repair stitched fragments",
					"unit", cx.unit.Name, "fragments", n)
				combined = fixed
				txErrs = nil
				repaired = true
			}
		}
	}
	var fullErrs []string
	fullErrs = append(fullErrs, parseErrs...)
	fullErrs = append(fullErrs, reqErrs...)
	fullErrs = append(fullErrs, txErrs...)
	fullErrs = append(fullErrs, condErrs...)
	if len(fullErrs) > 0 {
		// Combined-body repair: the fragments passed their own gates but the
		// stitch still breaks the full contract (an undefined name across
		// fragment scopes, a dropped required call, a stray else). One
		// repair seam over the stitched body is the difference between a
		// converted endpoint and a lost one — re-gated by the same combined
		// contract, never bypassed.
		if fixed, repairRej, ok := repairCombinedBody(cx, contract, fullSigs, combined, fullErrs); ok {
			telemetry.Log(cx.ctx).Info("combined fragment body repaired",
				"unit", cx.unit.Name, "errors", len(fullErrs))
			combined = fixed
			fullErrs = nil
			repaired = true // the fixed body owns the final shape
		} else if repairRej != "" {
			rejected = repairRej
		}
	}
	if len(fullErrs) > 0 {
		return "", rejected, fmt.Errorf("combined fragment body failed validation: %s", strings.Join(fullErrs, "; "))
	}
	// Composer pass: one stitch call over the fragment outputs + the
	// applicable controller template shape. Fragment outputs fit in context
	// by construction (each capped at 75% of the output ceiling), so the
	// composer sees output 1, output 2, ... plus the template and the full
	// contract, and returns the single merged body. Best-effort: a composer
	// rejection keeps the concatenated body, never fails the unit. Skipped
	// after a successful tx-wrap repair, which already owns the final shape.
	if !repaired {
		if composed, ok := composeFragments(cx, contract, fullSigs, bodies); ok {
			combined = composed
		}
	}
	if cx.axisVar != "" {
		if fixed, ok := repairArmWrapper(combined, cx.axisVar); ok {
			telemetry.Log(cx.ctx).Info("arm wrapper unwrapped", "unit", cx.unit.Name, "axis", cx.axisVar)
			combined = fixed
		}
	}
	combined = ensureTerminalReturn(combined)
	opts.Ledger.Set(cx.unit.ID, ledger.StatusValidated, "")
	return combined, "", nil
}

// buildComposerPrompt assembles the stitch call: the controller template
// shape, the ordered fragment outputs, the full DB contract, REQUIRED
// CALLS, and the verbatim signature + struct definitions. Dynamic context
// rides the same filtered extras the fragment path uses (tx notes).
func buildComposerPrompt(cx chunkCtx, contract, dbContract string, bodies []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Endpoint: %s\n\n", cx.unit.Name)
	fmt.Fprintf(&sb, "Stitch %d fragment outputs into ONE controller method body (template owns the wrapper + START/END logs). The fragments already translate the flattened logic (SQL already replaced) — merge them, code only.\n\n", len(bodies))
	writePromptFacts(&sb, composerWording, cx.scen, false, cx.view.Source, receiverOf(cx.opts), dbContract, contract, nil, nil, nil, nil)
	for i, b := range bodies {
		fmt.Fprintf(&sb, "Fragment output %d of %d:\n%s\n\n", i+1, len(bodies), b)
	}
	sb.WriteString("Emit ONLY the merged Go body statements (code only, no prose, no fences).")
	return sb.String()
}

// composeFragments runs the stitch call. It returns (body, true) when the
// composer passes the full body gates; otherwise ("", false) and the caller
// keeps the concatenated fragments. The prompt is budget-checked first — an
// over-ceiling composer prompt skips the call instead of failing loudly.
func composeFragments(cx chunkCtx, contract, fullSigs string, bodies []string) (string, bool) {
	if len(bodies) < 2 {
		return "", false
	}
	opts := cx.opts
	receiver := receiverOf(opts)
	prompt := buildComposerPrompt(cx, contract, fullSigs, bodies)
	if opts.Budget.MaxPromptTokens > 0 {
		if berr := opts.Budget.CheckInput(prompt); berr != nil {
			telemetry.Log(cx.ctx).Warn("composer skipped — prompt over ceiling, keeping concatenated fragments",
				"unit", cx.unit.Name, "error", berr.Error())
			return "", false
		}
	}
	composed, chatCalls, _, err := llm.RunSeam(cx.ctx, llm.SeamInput{
		Unit: cx.unit.ID, Kind: string(cx.unit.Kind), Name: cx.unit.Name + "#composer",
		Template: cx.unit.TemplateID, LLM: cx.unit.LLM, Repair: opts.RetryRepair,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		Temperature: 0.1,
		Prompt: func(prev string, attemptNotes []string) (string, []llm.Message) {
			return prompt, seamMessages(systemPromptComposer, prompt, prev, fixHintMergedBody, attemptNotes, opts.RetryRepair)
		},
		Extract: cleanBody,
		Gate: func(body string) []string {
			verr := validateBody(opts, body)
			verr = append(verr, requiredCallErrs(cx.view.Source, body, receiver)...)
			verr = append(verr, controllerTuxedoErrs(body)...)
			verr = append(verr, txGateErrs(body, cx.calls)...)
			verr = append(verr, emptyIfErrs(body)...)
			if len(cx.census) > 0 {
				verr = append(verr, conditionPresenceErrs(cx.census, body)...)
			}
			verr = append(verr, scenarioGuardErrs(cx.guards, cx.guardBind, body)...)
			return verr
		},
	})
	opts.Ledger.Get(cx.unit.ID, string(cx.unit.Kind), cx.unit.Name).Attempts += chatCalls
	cx.res.LLMCalls += chatCalls
	if err != nil {
		telemetry.Log(cx.ctx).Warn("composer rejected — keeping concatenated fragments",
			"unit", cx.unit.Name, "error", err.Error())
		return "", false
	}
	telemetry.Log(cx.ctx).Info("composer stitched fragments", "unit", cx.unit.Name, "fragments", len(bodies))
	return composed, true
}

// repairCombinedBody runs one repair seam over a stitched body that failed
// the combined gates: the prompt carries the full contract, the gate errors,
// and the body, and the result must pass the same combined contract. The
// prompt is budget-checked first; any rejection keeps the loud failure.
func repairCombinedBody(cx chunkCtx, contract, fullSigs, body string, errs []string) (string, string, bool) {
	opts := cx.opts
	if opts.Client == nil || cx.ctx.Err() != nil {
		return "", "", false
	}
	receiver := receiverOf(opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Endpoint: %s\n\n", cx.unit.Name)
	sb.WriteString("The Go controller body below failed validation. Fix exactly the listed problems and emit the corrected full body (code only, no fences, no prose).\n\n")
	sb.WriteString("Validation errors:\n  - " + strings.Join(errs, "\n  - ") + "\n\n")
	writePromptFacts(&sb, composerWording, cx.scen, false, cx.view.Source, receiver, fullSigs, contract, nil, nil, nil, nil)
	sb.WriteString("\nBody to fix:\n" + body + "\n")
	prompt := sb.String()
	if opts.Budget.MaxPromptTokens > 0 {
		if berr := opts.Budget.CheckInput(prompt); berr != nil {
			telemetry.Log(cx.ctx).Warn("combined repair skipped — prompt over ceiling",
				"unit", cx.unit.Name, "error", berr.Error())
			return "", "", false
		}
	}
	rej := ""
	fixed, chatCalls, _, err := llm.RunSeam(cx.ctx, llm.SeamInput{
		Unit: cx.unit.ID, Kind: string(cx.unit.Kind), Name: cx.unit.Name + "#repair",
		Template: cx.unit.TemplateID, LLM: cx.unit.LLM, Repair: true,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: 1,
		Temperature: 0.1,
		Prompt: func(prev string, attemptNotes []string) (string, []llm.Message) {
			return prompt, seamMessages(systemPrompt, prompt, prev, fixHintMethodBody, attemptNotes, true)
		},
		Extract: func(content string) string {
			return repairControllerBody(cx.ctx, cx.unit.Name, cx.axisVar, content)
		},
		Gate: func(b string) []string {
			verr := validateBody(opts, b)
			verr = append(verr, requiredCallErrs(cx.view.Source, b, receiver)...)
			verr = append(verr, controllerTuxedoErrs(b)...)
			verr = append(verr, txGateErrs(b, cx.calls)...)
			verr = append(verr, emptyIfErrs(b)...)
			if len(cx.census) > 0 {
				verr = append(verr, conditionPresenceErrs(cx.census, b)...)
			}
			verr = append(verr, scenarioGuardErrs(cx.guards, cx.guardBind, b)...)
			return verr
		},
		Rejected: func(payload string) { rej = payload },
	})
	opts.Ledger.Get(cx.unit.ID, string(cx.unit.Kind), cx.unit.Name).Attempts += chatCalls
	cx.res.LLMCalls += chatCalls
	if err != nil {
		telemetry.Log(cx.ctx).Warn("combined repair rejected",
			"unit", cx.unit.Name, "error", err.Error())
		return "", rej, false
	}
	return fixed, "", true
}

// txLocalDeclRe spots a var-declared tx handle the repair's closure
// parameter would shadow.
var txLocalDeclRe = regexp.MustCompile(`(?m)^\s*var\s+tx\b`)

// hasTxLocal reports whether the body declares its own tx handle (var tx
// or any := with tx on the LHS) — wrapping it in a func(tx ...) closure
// would silently rebind those uses, so the repair bails instead.
func hasTxLocal(body string) bool {
	if txLocalDeclRe.MatchString(body) {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, ":="); i >= 0 {
			for _, m := range identRe.FindAllString(line[:i], -1) {
				if m == "tx" {
					return true
				}
			}
		}
	}
	return false
}

// isReturnLine reports whether a trimmed line is a return statement (bare
// or valued) rather than an identifier that merely starts with "return".
func isReturnLine(t string) bool {
	return t == "return" || strings.HasPrefix(t, "return ") || strings.HasPrefix(t, "return\t")
}

// depthZeroComma returns the index of the first comma at paren depth zero,
// honoring string/char literals and all bracket kinds — the split point
// between a return's value list. -1 when the line holds a single value.
func depthZeroComma(s string) int {
	depth := 0
	inStr, inChar, esc := false, false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
		case inChar:
			if ch == '\\' {
				esc = true
			} else if ch == '\'' {
				inChar = false
			}
		case ch == '"':
			inStr = true
		case ch == '\'':
			inChar = true
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
		case ch == ',' && depth == 0:
			return i
		}
	}
	return -1
}

// rewriteClosureReturn maps one method-body return line into the
// tx-closure form: `return A, B` → `return B` (named data is already
// populated by the appends; the error propagates to the wrapper, which
// rolls back), bare `return` → `return err`. Non-return lines pass through
// untouched.
func rewriteClosureReturn(line string) string {
	trimmed := strings.TrimSpace(line)
	if !isReturnLine(trimmed) {
		return line
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	rest := strings.TrimSpace(trimmed[len("return"):])
	if rest == "" {
		return indent + "return err"
	}
	if i := depthZeroComma(rest); i >= 0 {
		return indent + "return " + strings.TrimSpace(rest[i+1:])
	}
	return line
}

// wrapTxBody deterministically repairs a combined fragment body that fails
// the transaction gate: the unit's store calls take tx but the fragments
// omitted the ExecTransaction wrapper. The repair threads the tx handle
// through every tx call site (Recv.Name(ctx, → Recv.Name(ctx, tx, —
// already-threaded sites are left alone) and wraps the whole body in the
// wrapper the gate names, with method returns mapped into closure returns
// and a `return data, nil` tail delivering the accumulated rows.
//
// Bail-outs (ok=false — the caller keeps today's loud gate failure):
//   - the body already contains ExecTransaction (partial wrapper —
//     ambiguous, never second-guessed)
//   - no tx-variant calls (gate inert — nothing to repair)
//   - the body declares its own tx local (the closure parameter would
//     shadow it)
//
// The result is re-gated by the caller (parse + REQUIRED-CALLS + tuxedo +
// tx); the repair never bypasses validation.
func wrapTxBody(body string, calls map[string]budget.DBCall, receiver string) (string, bool) {
	if strings.Contains(body, "ExecTransaction(") {
		return "", false
	}
	type txCall struct{ recv, name, ctx, tx string }
	var tcs []txCall
	for _, call := range calls {
		if call.Tx == "" {
			continue
		}
		recv := call.Receiver
		if recv == "" {
			recv = strings.TrimSuffix(receiver, ".")
		}
		ctx := call.CtxName
		if ctx == "" {
			ctx = "c"
		}
		tcs = append(tcs, txCall{recv, call.Name, ctx, call.Tx})
	}
	if len(tcs) == 0 {
		return "", false
	}
	sort.Slice(tcs, func(i, j int) bool { return tcs[i].name < tcs[j].name })
	if hasTxLocal(body) {
		return "", false
	}
	// Wrapper context: the most common CtxName among tx calls (deterministic
	// tie-break on sorted order — tcs is name-sorted above).
	ctxName := tcs[0].ctx
	best := 0
	seen := map[string]bool{}
	for _, tc := range tcs {
		if seen[tc.ctx] {
			continue
		}
		seen[tc.ctx] = true
		n := 0
		for _, other := range tcs {
			if other.ctx == tc.ctx {
				n++
			}
		}
		if n > best {
			best, ctxName = n, tc.ctx
		}
	}
	rewritten := body
	for _, tc := range tcs {
		// Thread tx after the context argument. Already-threaded sites
		// (`Name(ctx, tx, ...`) are left byte-identical — RE2 has no
		// lookahead, so the skip is an explicit following-text check.
		pat := common.CachedRegexp(regexp.QuoteMeta(tc.recv + "." + tc.name + "(" + tc.ctx + ","))
		txLead := common.CachedRegexp(`^\s*` + regexp.QuoteMeta(tc.tx) + `\s*,`)
		var sb strings.Builder
		prev := 0
		for _, loc := range pat.FindAllStringIndex(rewritten, -1) {
			if txLead.MatchString(rewritten[loc[1]:]) {
				continue
			}
			sb.WriteString(rewritten[prev:loc[1]])
			sb.WriteString(" " + tc.tx + ",")
			prev = loc[1]
			for prev < len(rewritten) && (rewritten[prev] == ' ' || rewritten[prev] == '\t') {
				prev++
			}
			sb.WriteString(" ")
		}
		sb.WriteString(rewritten[prev:])
		rewritten = sb.String()
	}
	cls := make([]string, 0, len(rewritten)/40+3)
	for _, line := range strings.Split(rewritten, "\n") {
		cls = append(cls, rewriteClosureReturn(line))
	}
	tail := ""
	for i := len(cls) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(cls[i]); t != "" {
			tail = t
			break
		}
	}
	if !isReturnLine(tail) {
		cls = append(cls, "return nil")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "if err := utils.ExecTransaction(%s, %s.GetDB(), func(tx *sqlx.Tx) error {\n",
		ctxName, tcs[0].recv)
	sb.WriteString(strings.Join(cls, "\n"))
	sb.WriteString("\n}); err != nil {\n\treturn nil, err\n}\nreturn data, nil")
	return sb.String(), true
}

// validateFragment parses a fragment in the same synthetic-method wrap
// validateBody uses, compensating the fragment's brace balance: a fragment
// cut mid-block closes deeper than it opens (or vice versa), so the parse
// check pads with synthetic braces — the pad is parse-only, never emitted;
// the combined body's full parse gate stays the real check.
func validateFragment(fragment string) []string {
	prefix, suffix := bracePads(fragment)
	wrapped := bodyParseWrap(strings.Repeat("{", prefix) + "\n" + fragment + "\n" + strings.Repeat("}", suffix))
	if _, ferr := goast.Emit("convert: controller fragment", wrapped); ferr != nil {
		return validate.TrimGoErrors(ferr.Error())
	}
	return nil
}

// bracePads returns the synthetic brace padding a fragment needs to parse in
// isolation: prefix braces when the text closes blocks opened before it
// (negative running depth), suffix braces when it ends with blocks still
// open. The pads never appear in the emitted body.
func bracePads(fragment string) (prefix, suffix int) {
	sc := &sliceScanner{}
	minDepth := 0
	for _, line := range strings.Split(fragment, "\n") {
		sc.scanLine(line)
		if sc.depth < minDepth {
			minDepth = sc.depth
		}
	}
	if minDepth < 0 {
		prefix = -minDepth
	}
	suffix = prefix + sc.depth
	if suffix < 0 {
		suffix = 0
	}
	return prefix, suffix
}
