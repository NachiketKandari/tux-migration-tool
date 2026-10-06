package convert

import (
	"regexp"
	"sort"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/ir"
)

// Deterministic legacy-seam rewrites (micro-chunk step 2, measured run
// 16092026_184233): the two fragment-level killers left after scaffold
// elision were the model echoing FML unpack/error-append spellings the
// tuxedo gate forbids — Fget32 unpack blocks, Fadd32(FML_ERR_MSG) error
// legs, chk_sssn session checks. All three carry a single mechanical intent
// with a deterministic Go shape, so the VIEW carries the mapped Go instead
// of the legacy spelling — the same pass-through pattern ReplaceQueries
// established for SQL → store calls. The model copies; the tuxedo gate
// never has to reject.

// seamCounts tallies one rewriteLegacySeams pass for telemetry.
type seamCounts struct {
	Gets    int // Fget32 unpack lines rewritten to request reads
	ErrAdds int // Fadd32(ERR-field) error legs rewritten to err = fmt.Errorf
	Ssn     int // chk_sssn session checks neutralized
}

var (
	// seamStmtRe matches a statement-leading call: optional provenance
	// markers, then either `Fget32(`/`Fadd32(` bare or `lhs = Call(`.
	// Branch conditions (`if (Fget32(...) == -1)`) never match — their
	// `if` prefix blocks the anchor.
	seamStmtRe = regexp.MustCompile(`^\s*(?:/\*[^*]*\*/\s*)*(?:[A-Za-z_][A-Za-z0-9_]*(?:\s*\[[^\]]*\])?\s*=\s*)?(Fget32|Fadd32)\s*\(`)
	// seamAssignRe strips a trailing `lhs = ` assignment from pre-call text.
	seamAssignRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\s*\[[^\]]*\])?\s*=\s*$`)
	// seamCastRe strips a C cast prefix on a call argument.
	seamCastRe = regexp.MustCompile(`(?i)\(\s*[a-zA-Z_][a-zA-Z0-9_ ]*\*+\s*\)`)
	// errlogLineRe anchors an errlog statement (optional provenance
	// markers); errlogErrCodeRe pulls its S-code out.
	errlogLineRe    = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*errlog\s*\(`)
	errlogErrCodeRe = regexp.MustCompile(`"(S\d{5})"`)
	// errBufferNames are the shared error-message buffers the error legs
	// consume; an adjacent errlog S-code repaints only these.
	errBufferNames = map[string]bool{"c_errmsg": true, "c_err_msg": true, "errmsg": true, "err_msg": true}
)

// rewriteLegacySeams maps the deterministic FML/session seams in a
// flattened branch view before prompting:
//
//   - `i_err[k] = Fget32(buf, FML_X, 0, (char*)&tgt, 0)` with FML_X in the
//     endpoint's request map → `tgt := request.<GoField>;` (verbatim
//     normalized target — the model's own accepted bodies spell it exactly
//     this way). Unmapped fields stay legacy; the model still owns them.
//   - `Fadd32(inbuf, FML_ERR_MSG, expr, 0);` error-append statements →
//     `err = errors.New("S31005");` when the adjacent `errlog(...)` line
//     supplies an S-code and expr is the shared error buffer, else
//     `err = fmt.Errorf("%s", expr);`. Response-shaping adds (non-ERR
//     fields, Obuffer fan-outs) and condition-context adds are left for the
//     AI to shape.
//   - `x = chk_sssn(...)` → `x = 0;` — session prologue the prompt already
//     orders dropped; the timeout guard becomes dead, which is correct
//     (Go middleware owns sessions).
//
// The errlog S-code pairing is why this pass runs before the scaffold
// strip: the scaffold pass deletes the errlog line, so the code must be
// captured while both lines are still in the view.
//
// Returns the rewritten source, the counts, and whether anything changed.
func rewriteLegacySeams(src string, reqMap map[string]string) (string, seamCounts, bool) {
	var out []string
	var c seamCounts
	changed := false
	pendingCode := ""
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if errlogLineRe.MatchString(line) {
			if m := errlogErrCodeRe.FindStringSubmatch(line); m != nil {
				pendingCode = m[1]
			}
			out = append(out, line)
			continue
		}
		rewritten, kind := rewriteSeamLine(line, reqMap, pendingCode)
		// The S-code window is the adjacent line only: the corpus pairs
		// errlog directly with its consuming Fadd32 error leg.
		pendingCode = ""
		if kind == seamSsn {
			c.Ssn++
			changed = true
			// The neutralized check (`x = 0;`) turns its `if (x == -1)`
			// guard provably dead: drop the guard block, and the dead
			// assignment too when nothing else reads the temp.
			if target := seamSsnTarget(line); target != "" {
				if end, ok := deadSessionGuard(lines, i+1, target); ok {
					if !identUsedElsewhere(lines, target, i, end) {
						i = end - 1
						continue
					}
					out = append(out, rewritten)
					i = end - 1
					continue
				}
			}
			out = append(out, rewritten)
			continue
		}
		switch kind {
		case seamGet:
			c.Gets++
			changed = true
		case seamErrAdd:
			c.ErrAdds++
			changed = true
		}
		out = append(out, rewritten)
	}
	return strings.Join(out, "\n"), c, changed
}

// seamSsnTarget extracts the assignment target of a `target = chk_sssn(...)`
// line ("" for any other shape).
func seamSsnTarget(line string) string {
	i := strings.Index(line, "chk_sssn(")
	if i < 0 {
		return ""
	}
	eq := assignmentEq(line, i)
	if eq < 0 {
		return ""
	}
	target := strings.TrimSpace(line[:eq])
	if !isPlainIdent(target) {
		return ""
	}
	return target
}

// deadSessionGuard finds the provably-dead `if (target == -1) { ... }`
// block a neutralized session check leaves behind, starting at line start
// (blank lines skipped). Returns the index just past the block.
func deadSessionGuard(lines []string, start int, target string) (int, bool) {
	re := common.CachedRegexp(`^\s*(?:/\*.*?\*/\s*)*if\s*\(\s*` + regexp.QuoteMeta(target) + `\s*==\s*-1\s*\)`)
	j := start
	for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
		j++
	}
	if j >= len(lines) || !re.MatchString(lines[j]) {
		return 0, false
	}
	_, end, ok := braceBlockLines(lines, j)
	if !ok {
		return 0, false
	}
	return end, true
}

// identUsedElsewhere reports whether ident has a real use outside lines
// [skipFrom, skipTo): plain declarations and `= 0` initializations are
// bookkeeping, not reads.
func identUsedElsewhere(lines []string, ident string, skipFrom, skipTo int) bool {
	use := common.CachedRegexp(`\b` + regexp.QuoteMeta(ident) + `\b`)
	bookkeeping := common.CachedRegexp(`^\s*(?:/\*.*?\*/\s*)*(?:(?:int|long|short|char|float|double|unsigned)\b[^=]*\b` +
		regexp.QuoteMeta(ident) + `\b|` + regexp.QuoteMeta(ident) + `\s*=\s*0\s*;)`)
	for i, ln := range lines {
		if i >= skipFrom && i < skipTo {
			continue
		}
		if !use.MatchString(ln) {
			continue
		}
		if bookkeeping.MatchString(ln) {
			continue
		}
		return true
	}
	return false
}

type seamKind int

const (
	seamNone seamKind = iota
	seamGet
	seamErrAdd
	seamSsn
)

// rewriteSeamLine applies the three seam rewrites to one line, returning
// the (possibly original) line and which rewrite fired. pendingCode is the
// S-code captured from the preceding errlog line ("" when none) — it turns
// a variable error leg into errors.New(<code>) instead of fmt.Errorf.
func rewriteSeamLine(line string, reqMap map[string]string, pendingCode string) (string, seamKind) {
	// chk_sssn first: the unpack block's `l_sssn_id_chk = chk_sssn(...)`
	// precedes any Fget line ordering — order within a line never collides
	// (one call per line in the flattened views).
	if i := strings.Index(line, "chk_sssn("); i >= 0 {
		if eq := assignmentEq(line, i); eq >= 0 {
			if _, close, ok := balancedParens(line, i+len("chk_sssn")); ok && lineEndsWithStmt(line, close) {
				return line[:eq+1] + " 0;" + trailingSuffix(line, close), seamSsn
			}
		}
		return line, seamNone
	}
	m := seamStmtRe.FindStringSubmatchIndex(line)
	if m == nil {
		return line, seamNone
	}
	name := line[m[2]:m[3]]
	args, close, ok := callArgs(line, m[1]-1)
	if !ok || len(args) < 4 {
		return line, seamNone
	}
	// Prefix keeps provenance markers and whitespace, drops any
	// `lhs = ` assignment — the rewritten statement replaces it whole.
	head := seamAssignRe.ReplaceAllString(line[:m[2]], "")
	switch name {
	case "Fget32":
		field := strings.TrimSpace(args[1])
		goField, mapped := reqMap[field]
		if !mapped {
			return line, seamNone
		}
		target := normalizeSeamTarget(args[3])
		if target == "" || !isPlainIdent(target) {
			return line, seamNone
		}
		return head + target + " := request." + goField + ";" + trailingSuffix(line, close), seamGet
	case "Fadd32":
		field := strings.TrimSpace(args[1])
		if !ir.IsErrField(field) {
			return line, seamNone
		}
		expr := normalizeSeamTarget(args[2])
		if expr == "" {
			return line, seamNone
		}
		if pendingCode != "" && isErrBuffer(expr) {
			return head + "err = errors.New(\"" + pendingCode + "\");" + trailingSuffix(line, close), seamErrAdd
		}
		return head + "err = fmt.Errorf(\"%s\", " + expr + ");" + trailingSuffix(line, close), seamErrAdd
	}
	return line, seamNone
}

// assignmentEq returns the offset of the `=` that assigns the call whose
// name starts at i (the RHS target), or -1 when the call is not a plain
// assignment's RHS (comparison, condition, bare statement).
func assignmentEq(line string, callStart int) int {
	head := line[:callStart]
	eq := strings.LastIndex(head, "=")
	if eq < 0 {
		return -1
	}
	if j := strings.LastIndex(head[:eq], "=="); j >= 0 && eq >= j && eq <= j+1 {
		return -1 // `x == chk_sssn(` — a comparison, never rewrite
	}
	lhs := strings.TrimSpace(head[:eq])
	if lhs == "" || strings.ContainsAny(lhs, "(=<>!&|") {
		return -1
	}
	return eq
}

// lineEndsWithStmt reports whether the statement after the call's closing
// paren at close ends the line (a `;` — optionally followed by a comment).
func lineEndsWithStmt(line string, close int) bool {
	rest := strings.TrimSpace(line[close+1:])
	return rest == "" || rest == ";" || strings.HasPrefix(rest, ";") || strings.HasPrefix(rest, "/*")
}

// trailingSuffix keeps a line's trailing comment (provenance markers or a
// C comment the flattened render may suffix) after a rewritten statement.
// close is the offset of the call's closing paren; the scan starts past it.
func trailingSuffix(line string, close int) string {
	rest := line[close+1:]
	if i := strings.Index(rest, ";"); i >= 0 {
		if j := strings.Index(rest[i:], "/*"); j >= 0 {
			return " " + rest[i+j:]
		}
		return ""
	}
	if j := strings.Index(rest, "/*"); j >= 0 {
		return " " + rest[j:]
	}
	return ""
}

// isPlainIdent guards the rewrite targets: a bare Go-safe identifier only.
var isPlainIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func isPlainIdent(s string) bool { return isPlainIdentRe.MatchString(s) }

// isErrBuffer reports whether a normalized seam target names a shared
// error-message buffer — the only exprs an errlog S-code repaints.
func isErrBuffer(expr string) bool { return errBufferNames[expr] }

// sessionArgNames are the middleware-owned identifiers an unresolved-fn
// call in the view must not carry: Go middleware owns the service name,
// the session/user ids, and the shared error buffers. The generated stub
// is variadic and the accepted body shape omits them (measured A/B: the
// model copied the session plumbing verbatim and the gate rejected the
// undefined C names).
var sessionArgNames = map[string]bool{
	"c_ServiceName": true, "c_errmsg": true, "c_err_msg": true,
	"errmsg": true, "err_msg": true, "c_user_id": true, "c_userid": true,
	"li_session_id": true, "l_sssn_id": true, "DEF_USR": true, "DEF_SSSN": true,
}

// strcpyServiceRe anchors the session-prologue strcpy whose destination is
// the middleware-owned service name.
var strcpyServiceRe = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*strcpy\s*\(\s*c_ServiceName\s*,`)

// stripSessionArgs drops middleware-owned identifiers (session service
// name, error buffers, user/session ids) from calls to unresolved legacy
// fns in a view — the Go stub is variadic and the accepted shape omits
// them. Also drops the strcpy(c_ServiceName, rqst->name) prologue line.
// fnNames carries the legacy fn spellings (plan.Stub.Fn); other callees
// keep every arg, including out-params like &c_d2u_active_flg. Returns the
// rewritten source and the number of args/lines dropped.
func stripSessionArgs(src string, fnNames map[string]bool) (string, int) {
	if len(fnNames) == 0 {
		return src, 0
	}
	callRe := stubCallRe(fnNames)
	var out []string
	dropped := 0
	for _, line := range strings.Split(src, "\n") {
		rewritten, n := stripSessionArgLine(line, callRe)
		dropped += n
		out = append(out, rewritten)
	}
	if dropped == 0 {
		return src, 0
	}
	return strings.Join(out, "\n"), dropped
}

// stubCallRe matches a call to any unresolved fn name; the names are
// legacy C identifiers, so a word boundary anchors them.
func stubCallRe(fnNames map[string]bool) *regexp.Regexp {
	names := make([]string, 0, len(fnNames))
	for n := range fnNames {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Strings(names)
	key := strings.Join(names, "|")
	return common.CachedRegexp(`\b(?:` + key + `)\s*\(`)
}

// stripSessionArgLine scrubs one line: a statement-level
// strcpy(c_ServiceName, ...) line drops whole (empty result), and the
// first call to a known unresolved fn keeps only its non-session args.
func stripSessionArgLine(line string, fnCall *regexp.Regexp) (string, int) {
	if strcpyServiceRe.MatchString(line) {
		if open := strings.Index(line, "("); open >= 0 {
			if _, close, ok := balancedParens(line, open); ok && lineEndsWithStmt(line, close) {
				return "", 1
			}
		}
	}
	loc := fnCall.FindStringIndex(line)
	if loc == nil {
		return line, 0
	}
	open := loc[0] + strings.Index(line[loc[0]:loc[1]], "(")
	content, close, ok := balancedParens(line, open)
	if !ok {
		return line, 0
	}
	args := splitTopArgs(content)
	kept := make([]string, 0, len(args))
	dropped := 0
	for _, a := range args {
		if sessionArgNames[normalizeSeamTarget(a)] {
			dropped++
			continue
		}
		kept = append(kept, strings.TrimSpace(a))
	}
	if dropped == 0 {
		return line, 0
	}
	return line[:open+1] + strings.Join(kept, ", ") + line[close:], dropped
}

// normalizeSeamTarget reduces a call argument to the bare host-var name the
// Go assignment writes: `(char *)&c_user_id` → c_user_id,
// `(char *)sql_rpd_prdt_id.arr` → sql_rpd_prdt_id. The verbatim C spelling
// is deliberate — the model transliterates view identifiers byte-for-byte,
// so the unpack declaration stays consistent with the condition uses of the
// same name.
func normalizeSeamTarget(arg string) string {
	s := strings.TrimSpace(arg)
	if j := strings.Index(s, "/*"); j >= 0 { // trailing provenance marker
		if k := strings.Index(s[j:], "*/"); k >= 0 {
			s = s[:j] + s[j+k+2:]
		}
	}
	s = strings.TrimSpace(seamCastRe.ReplaceAllString(s, ""))
	s = strings.TrimPrefix(s, "&")
	if j := strings.LastIndex(s, "."); j >= 0 && strings.HasSuffix(s, ".arr") {
		s = s[:j]
	}
	if j := strings.Index(s, "["); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// callArgs extracts the top-level comma-split arguments of the call whose
// opening paren sits at offset open (the index of `(`). Honors string and
// char literals and all bracket kinds. ok=false when the parens never
// close on this line.
func callArgs(line string, open int) (args []string, close int, ok bool) {
	content, c, ok := balancedParens(line, open)
	if !ok {
		return nil, 0, false
	}
	return splitTopArgs(content), c, true
}

// balancedParens returns the content between the paren at start and its
// matching close, honoring string/char literals and nested brackets.
func balancedParens(s string, start int) (string, int, bool) {
	depth := 0
	inStr, inChar, esc := false, false, false
	for i := start; i < len(s); i++ {
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
			if depth == 0 {
				return s[start+1 : i], i, true
			}
		}
	}
	return "", 0, false
}

// splitTopArgs splits on commas at bracket depth zero, honoring literals.
func splitTopArgs(s string) []string {
	var parts []string
	depth := 0
	inStr, inChar, esc := false, false, false
	last := 0
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
			parts = append(parts, s[last:i])
			last = i + 1
		}
	}
	parts = append(parts, s[last:])
	return parts
}
