package convert

import (
	"regexp"
	"strconv"
	"strings"
)

// FML probe elision (deterministic view shaping): every Pro*C unpack block
// ends with a probe cycle — INIT(i_err/i_ferr), an `i_ferr[k] = Ferror32`
// after every Fget/Fadd, and an `i_err[...] == -1` check whose branches
// either seed FNOTPRES absent-input defaults or fail the call. Once
// rewriteLegacySeams has replaced the mapped Fget32 reads with direct
// request reads, the probe arrays are dead C bookkeeping, and every model
// attempt copies them (`i_ferr[0] = Ferror32`, `FNOTPRES`, `i_loop`), which
// the gates reject as undefined identifiers / legacy spelling.
//
// The pass drops the dead array lines and rewrites each probe cycle into
// the one piece of real logic it carries: FNOTPRES absent-input fallbacks
// as `if request.<Field> == "" { ... }` (index-aligned with the block's
// request reads). Cycles with no FNOTPRES fallback are pure unpack-error
// plumbing (middleware-owned) and drop whole. Anything unrecognized stays
// byte-identical — the pass is a total function over the view.

var (
	probeInitRe    = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*INIT\s*\(\s*i_(?:err|ferr)\b`)
	probeFerrRe    = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*i_ferr\s*\[[^\]]*\]\s*=\s*Ferror32\s*;`)
	probeReadRe    = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_]*\s*:=\s*request\.([A-Za-z_][A-Za-z0-9_]*)\s*;`)
	probeGetStmtRe = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*i_err\s*\[[^\]]*\]\s*=\s*Fget32\s*\(`)
	probeLoopRe    = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*for\s*\(\s*i_loop\b`)
	probeCheckRe   = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*if\s*\(\s*i_err\s*\[`)
	probeCondRe    = regexp.MustCompile(`i_err\s*\[\s*(?:i_loop|(\d+))\s*\]\s*==\s*-1`)
	probeLoopIdxRe = regexp.MustCompile(`i_loop\s*==\s*(\d+)`)
	probeFerrIdxRe = regexp.MustCompile(`i_ferr\s*\[\s*(\d+)\s*\]`)
)

// stripFMLProbes runs the elision over a seam-rewritten view. Returns the
// rewritten source and the number of source lines elided (0 → unchanged).
func stripFMLProbes(src string) (string, int) {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	var fields []string // probe index → request field ("" = unmapped source)
	collecting := false
	elided := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case probeInitRe.MatchString(line):
			fields = fields[:0]
			collecting = true
			elided++
			continue
		case probeFerrRe.MatchString(line):
			elided++
			continue
		case probeLoopRe.MatchString(line), probeCheckRe.MatchString(line):
			block, next, ok := braceBlockLines(lines, i)
			if !ok {
				out = append(out, line)
				continue
			}
			repl, n, ok := rewriteProbeBlock(block, fields)
			if !ok {
				out = append(out, block...)
			} else {
				out = append(out, repl...)
				elided += n
			}
			fields = fields[:0]
			collecting = false
			i = next - 1
			continue
		}
		if collecting {
			if m := probeReadRe.FindStringSubmatch(line); m != nil {
				fields = append(fields, m[1])
			} else if probeGetStmtRe.MatchString(line) {
				fields = append(fields, "") // unmapped Fget keeps index alignment
			}
		}
		out = append(out, line)
	}
	if elided == 0 {
		return src, 0
	}
	return strings.Join(out, "\n"), elided
}

// braceBlockLines returns lines[start:end] where end is the line index just
// past the first balanced brace block opened at or after start.
func braceBlockLines(lines []string, start int) ([]string, int, bool) {
	sc := &sliceScanner{}
	seen := false
	for j := start; j < len(lines); j++ {
		sc.scanLine(lines[j])
		if sc.depth > 0 {
			seen = true
		}
		if seen && sc.depth == 0 {
			return lines[start : j+1], j + 1, true
		}
		if sc.depth < 0 {
			return nil, start + 1, false
		}
	}
	return nil, start + 1, false
}

// probeBranch is one conditional arm of a probe check.
type probeBranch struct {
	cond string
	body string
}

// rewriteProbeBlock parses one probe cycle and returns its Go-shaped
// replacement (one `if request.<Field> == ""` per FNOTPRES fallback), the
// number of source lines elided, and whether the rewrite applies. ok=false
// leaves the block byte-identical (conservative total-function contract).
func rewriteProbeBlock(block []string, fields []string) ([]string, int, bool) {
	text := strings.Join(block, "\n")
	m := probeCondRe.FindStringSubmatchIndex(text)
	if m == nil {
		return nil, 0, false
	}
	condIdx := -1 // wrapper index; -1 → take each branch's own index
	if m[2] >= 0 {
		if n, err := strconv.Atoi(text[m[2]:m[3]]); err == nil {
			condIdx = n
		}
	}
	open := skipSpaceComments(text, m[1])
	if open < len(text) && text[open] == ')' {
		open = skipSpaceComments(text, open+1)
	}
	body, _, ok := stringBraceBody(text, open)
	if !ok {
		return nil, 0, false
	}
	inner := skipSpaceComments(body, 0)
	if !strings.HasPrefix(body[inner:], "if") {
		// The check body is the plain unpack-failure path (no FNOTPRES
		// fallback structure) — middleware-owned, drops whole.
		return nil, len(block), true
	}
	branches, _, ok := parseIfChain(body, inner)
	if !ok {
		return nil, 0, false
	}
	var repl []string
	fallbacks := 0
	for _, b := range branches {
		if strings.TrimSpace(b.cond) == "" {
			continue // trailing error leg (unpack failure — middleware-owned)
		}
		if !strings.Contains(b.cond, "FNOTPRES") {
			// A non-FNOTPRES conditional arm carries semantics beyond
			// absent-input defaults — leave the whole cycle untouched.
			return nil, 0, false
		}
		idx, ok := fallbackIndex(b.cond)
		if !ok {
			return nil, 0, false
		}
		if idx < 0 {
			idx = condIdx
		}
		if idx < 0 || idx >= len(fields) || fields[idx] == "" {
			return nil, 0, false // unmapped source: cannot express the guard
		}
		body := trimProbeBody(b.body)
		if len(body) == 0 {
			continue
		}
		repl = append(repl, `if request.`+fields[idx]+` == "" {`)
		repl = append(repl, body...)
		repl = append(repl, "}")
		fallbacks++
	}
	if fallbacks == 0 {
		// Pure unpack-error plumbing: the whole cycle drops.
		return nil, len(block), true
	}
	return repl, len(block), true
}

// fallbackIndex extracts the probe index a FNOTPRES condition guards:
// `i_loop == K && i_ferr[...] == FNOTPRES` or `i_ferr[K] == FNOTPRES`.
func fallbackIndex(cond string) (int, bool) {
	if m := probeLoopIdxRe.FindStringSubmatch(cond); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	if m := probeFerrIdxRe.FindStringSubmatch(cond); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	return 0, false
}

// trimProbeBody splits a branch body into lines with blank edges removed.
func trimProbeBody(body string) []string {
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// skipSpaceComments advances past whitespace and comments.
func skipSpaceComments(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r':
			i++
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return len(s)
			}
			i += 2 + j + 2
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return len(s)
			}
			i += j + 1
		default:
			return i
		}
	}
	return i
}

// stringBraceBody returns the content between the brace at i and its match.
func stringBraceBody(s string, i int) (string, int, bool) {
	if i >= len(s) || s[i] != '{' {
		return "", i, false
	}
	depth := 0
	inStr, inChar, esc, inBlock := false, false, false, false
	for j := i; j < len(s); j++ {
		ch := s[j]
		switch {
		case inBlock:
			if ch == '*' && j+1 < len(s) && s[j+1] == '/' {
				inBlock = false
				j++
			}
		case inStr:
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
		case inChar:
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '\'':
				inChar = false
			}
		default:
			switch {
			case ch == '/' && j+1 < len(s) && s[j+1] == '*':
				inBlock = true
				j++
			case ch == '"':
				inStr = true
			case ch == '\'':
				inChar = true
			case ch == '{':
				depth++
			case ch == '}':
				depth--
				if depth == 0 {
					return s[i+1 : j], j + 1, true
				}
			}
		}
	}
	return "", i, false
}

// parseIfChain parses `if (cond) { body } [else if (cond) { body }]*
// [else { body }]` starting at i. The trailing else branch carries an empty
// cond. ok=false on any other shape.
func parseIfChain(s string, i int) ([]probeBranch, int, bool) {
	var branches []probeBranch
	i = skipSpaceComments(s, i)
	if !strings.HasPrefix(s[i:], "if") {
		return nil, i, false
	}
	i += 2
	cond, body, next, ok := parseIfArm(s, i)
	if !ok {
		return nil, i, false
	}
	branches = append(branches, probeBranch{cond: cond, body: body})
	i = next
	for {
		i = skipSpaceComments(s, i)
		if !strings.HasPrefix(s[i:], "else") {
			break
		}
		i += 4
		i = skipSpaceComments(s, i)
		if strings.HasPrefix(s[i:], "if") {
			i += 2
			cond, body, next, ok = parseIfArm(s, i)
			if !ok {
				return nil, i, false
			}
			branches = append(branches, probeBranch{cond: cond, body: body})
			i = next
			continue
		}
		body, next, ok := stringBraceBody(s, i)
		if !ok {
			return nil, i, false
		}
		branches = append(branches, probeBranch{body: body})
		i = next
		break
	}
	return branches, i, true
}

// parseIfArm parses `(cond) { body }` after an `if`.
func parseIfArm(s string, i int) (cond, body string, next int, ok bool) {
	i = skipSpaceComments(s, i)
	cond, close, ok := balancedParens(s, i)
	if !ok {
		return "", "", i, false
	}
	i = skipSpaceComments(s, close+1)
	body, next, ok = stringBraceBody(s, i)
	if !ok {
		return "", "", i, false
	}
	return cond, body, next, true
}
