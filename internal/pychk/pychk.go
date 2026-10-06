// Package pychk is the batchpy validation gate (PRD-2026-09-08 BP-8): a
// structural Python syntax check (indentation blocks, bracket/quote balance)
// plus the SQL fidelity check, which extracts the generated module's
// triple-quoted string literals and reuses sqlchk.Compare unchanged — the
// same normalized structural projection the Go target is gated with. When a
// python3 interpreter is available, the ast.parse check upgrades the
// structural gate; both are degrade-safe observations.
package pychk

import (
	"os"
	"os/exec"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/sqlchk"
)

// Issue is one structural syntax problem with its 1-based line.
type Issue struct {
	Line int
	Msg  string
}

// Check performs the structural syntax scan: indentation blocks, bracket and
// quote balance, compound-statement headers. A clean result is necessary but
// not sufficient — CheckWithInterpreter upgrades the gate when python3 is on
// PATH.
func Check(src string) []Issue {
	var issues []Issue
	issue := func(line int, msg string) { issues = append(issues, Issue{Line: line, Msg: msg}) }

	lines := strings.Split(src, "\n")
	quote := ""            // active triple-quote marker
	depth := 0             // bracket depth carried across continuation lines
	prevOpener := false    // previous statement line opened a block
	prevOpenerIndent := "" // indent of the block-opening line
	blocks := []string{""} // open block body levels; the module level ("") is the outermost
	// pendingOpener records a block header whose parentheses do not close on
	// the header line — a wrapped def signature. At the header line itself the
	// bracket depth is non-zero, so statement-level analysis (and with it
	// isBlockOpener) is skipped; the header is re-examined when the depth
	// returns to zero, using the header line's own indent.
	pendingOpener := false
	pendingOpenerIndent := ""

	for i, raw := range lines {
		lineNo := i + 1
		startedInQuote := quote != ""
		code, newQuote, d := scanLine(raw, quote)
		quote = newQuote

		// A line that begins inside a triple-quoted string carries no
		// block structure — even when the string closes mid-line, the
		// remainder is a statement continuation at most.
		if startedInQuote {
			continue
		}
		preDepth := depth
		depth += d

		indent := common.Leading(raw)
		trimmed := strings.TrimSpace(code)
		// A triple-quoted literal opened on this line: the line is a statement
		// (a docstring is the first statement of its block) even though its
		// code portion is blanked to whitespace.
		quoteOpened := !startedInQuote && quote != ""

		// Inside a bracket continuation the line belongs to the statement that
		// opened it — Python's own rule: newlines inside (), [] and {} do not
		// end a logical line. The closer is never a fresh statement. It only
		// has to answer one question: did the logical line turn out to be a
		// block header (`def f(` … `):`)? Decide on the closing line's tail,
		// keeping the header line's indent for the body comparison. Without
		// this, a wrapped signature is invisible as a block opener and its
		// body is flagged as an unexpected indent.
		if preDepth > 0 {
			if depth == 0 && pendingOpener {
				pendingOpener = false
				if trimmed != "" && !strings.HasPrefix(trimmed, "#") && strings.HasSuffix(trimmed, ":") {
					prevOpener = true
					prevOpenerIndent = pendingOpenerIndent
				}
			}
			continue
		}

		// Statement-level analysis only applies to lines that begin a logical
		// line: no brackets open above, none open below.
		if depth == 0 && (trimmed != "" || quoteOpened) {
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if prevOpener {
				if indent <= prevOpenerIndent {
					issue(lineNo, "block opener not followed by deeper indent")
				}
				prevOpener = false
				// The block's body level is the next statement's indent —
				// pushed here, not the opener's own line indent.
				blocks = append(blocks, indent)
			} else if len(blocks) > 0 {
				top := blocks[len(blocks)-1]
				switch {
				case indent > top:
					// Deeper than the innermost open block without an
					// opener — Python raises IndentationError here.
					issue(lineNo, "unexpected indent")
				case indent < top:
					for len(blocks) > 0 && indent < blocks[len(blocks)-1] {
						blocks = blocks[:len(blocks)-1]
					}
					if len(blocks) == 0 || indent != blocks[len(blocks)-1] {
						issue(lineNo, "unindent does not match any outer indentation level")
					}
				}
			}
			if strings.ContainsRune(indent, '\t') {
				issue(lineNo, "tab in indentation")
			}
			if isBlockOpener(trimmed) {
				prevOpener = true
				prevOpenerIndent = indent
			} else if d > 0 {
				// The logical line continues inside brackets. If it turns out
				// to be a block header (the continuation closes with ':'),
				// the pending record restores the opener — with this line's
				// indent, not the closer's.
				pendingOpener = true
				pendingOpenerIndent = indent
			}
		} else if depth > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			// A statement that begins here continues inside brackets. It is
			// also the block body a pending opener was waiting for — an
			// `if not (...):` followed by a wrapped `raise` — so the opener
			// is consumed exactly as it would be for a single-line body,
			// before the statement is recorded as possibly-a-header.
			if prevOpener {
				if indent <= prevOpenerIndent {
					issue(lineNo, "block opener not followed by deeper indent")
				}
				prevOpener = false
				blocks = append(blocks, indent)
			}
			// The logical line may itself turn out to be a block header —
			// decide when the continuation closes.
			pendingOpener = true
			pendingOpenerIndent = indent
		}
	}

	last := len(lines)
	if quote != "" {
		issue(last, "unterminated triple-quoted string")
	}
	if depth != 0 {
		issue(last, "unbalanced brackets at end of file")
	}
	if prevOpener {
		issue(last, "file ends with an unterminated block")
	}
	return issues
}

// scanLine returns the code portion of one line (string/comment segments
// blanked), the triple-quote state after the line, and the net bracket delta.
func scanLine(raw, quote string) (code string, newQuote string, depth int) {
	var sb strings.Builder
	i := 0
	for i < len(raw) {
		// Inside a triple-quoted string: scan for the closer.
		if quote != "" {
			if end := strings.Index(raw[i:], quote); end >= 0 {
				i += end + len(quote)
				quote = ""
				continue
			}
			return sb.String(), quote, depth
		}
		c := raw[i]
		switch {
		case (i+2 < len(raw)+1) && (strings.HasPrefix(raw[i:], `"""`) || strings.HasPrefix(raw[i:], "'''")):
			q := raw[i : i+3]
			rest := raw[i+3:]
			if end := strings.Index(rest, q); end >= 0 {
				// "str" marks the blanked literal: a line that carries only a
				// string (a docstring, a bare literal statement) must still
				// read as a statement. Blanking to pure whitespace made the
				// opener line invisible, so its block's body indent was never
				// pushed and every statement after it mis-indent-reported.
				sb.WriteString("str")
				sb.WriteString(strings.Repeat(" ", end)) // blank the content
				i = i + 3 + end + 3
				continue
			}
			// The literal opens and runs past this line: blank the remainder
			// (it is all string content) and hand the quote state back.
			sb.WriteString("str")
			quote = q
			sb.WriteString(strings.Repeat(" ", len(raw)-(i+3)))
			return sb.String(), quote, depth
		case c == '#':
			return sb.String(), quote, depth // comment to EOL
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(raw) {
				if raw[j] == '\\' {
					j += 2
					continue
				}
				if raw[j] == c {
					break
				}
				j++
			}
			if n := j - i + 1; n >= 3 {
				sb.WriteString("str")
				sb.WriteString(strings.Repeat(" ", n-3))
			} else {
				sb.WriteString(strings.Repeat(" ", n))
			}
			i = j + 1
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		}
		sb.WriteByte(c)
		i++
	}
	return sb.String(), quote, depth
}

// isBlockOpener recognizes compound-statement headers (the trimmed code ends
// with ':' and starts with a block keyword).
func isBlockOpener(trimmed string) bool {
	if !strings.HasSuffix(trimmed, ":") {
		return false
	}
	for _, kw := range []string{"if ", "elif ", "else", "for ", "while ", "def ", "class ", "try", "except", "finally", "with ", "match "} {
		if strings.HasPrefix(trimmed, kw) {
			return true
		}
	}
	return false
}

// CheckWithInterpreter runs the real syntax gate: python3's ast.parse over
// the written file. mode reports "ast" (interpreter ran), or "unavailable"
// with the structural gate as the only evidence.
func CheckWithInterpreter(path string) (ok bool, mode string, detail string) {
	py, err := exec.LookPath("python3")
	if err != nil {
		return false, "unavailable", "python3 not found on PATH"
	}
	cmd := exec.Command(py, "-c", "import ast,sys; ast.parse(open(sys.argv[1], encoding='utf-8').read())", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "ast", strings.TrimSpace(string(out))
	}
	return true, "ast", ""
}

// CheckSource is the content-level syntax gate (A2.7): the one owner of the
// temp-file + interpreter dance — callers pass module content and never
// touch the interpreter themselves.
func CheckSource(content string) (ok bool, mode string, detail string) {
	if _, err := exec.LookPath("python3"); err != nil {
		return false, "unavailable", "python3 not found on PATH"
	}
	tmp, err := os.CreateTemp("", "batchpy-*.py")
	if err != nil {
		return false, "unavailable", err.Error()
	}
	path := tmp.Name()
	if _, werr := tmp.WriteString(content); werr != nil {
		tmp.Close()
		os.Remove(path)
		return false, "unavailable", werr.Error()
	}
	tmp.Close()
	defer os.Remove(path)
	return CheckWithInterpreter(path)
}

// Literal is one extracted triple-quoted string with its assignment name
// ("" for inline literals).
type Literal struct {
	Name string
	SQL  string
}

// SQLLiterals extracts the module's triple-quoted string literals in source
// order, capturing the assignment target when the literal opens a
// `NAME = """..."""` statement (the py_batch_const shape).
func SQLLiterals(src string) []Literal {
	lines := strings.Split(src, "\n")
	var out []Literal
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		name := ""
		for _, q := range []string{`"""`, "'''"} {
			idx := strings.Index(trimmed, q)
			if idx < 0 {
				continue
			}
			head := strings.TrimSpace(trimmed[:idx])
			if eq := strings.LastIndex(head, "="); eq >= 0 && isIdentStr(strings.TrimSpace(head[:eq])) {
				name = strings.TrimSpace(head[:eq])
			}
			body, end := capture(lines, i, idx+len(q), q)
			out = append(out, Literal{Name: name, SQL: body})
			i = end
			break
		}
	}
	return out
}

// capture reads a triple-quoted body starting at (line, col) until the
// closing marker, returning the body text and the closing line index.
func capture(lines []string, line, col int, q string) (string, int) {
	var sb strings.Builder
	first := lines[line][col:]
	if end := strings.Index(first, q); end >= 0 {
		return first[:end], line
	}
	sb.WriteString(first)
	for l := line + 1; l < len(lines); l++ {
		if end := strings.Index(lines[l], q); end >= 0 {
			sb.WriteString("\n")
			sb.WriteString(lines[l][:end])
			return sb.String(), l
		}
		sb.WriteString("\n")
		sb.WriteString(lines[l])
	}
	return sb.String(), len(lines) - 1
}

func isIdentStr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// FidelityTarget is one SQL constant's fidelity input (const name, IR query
// id, source SQL).
type FidelityTarget struct {
	Const   string
	QueryID string
	Source  string
}

// Fidelity compares every target's source SQL against the generated module's
// literal of the same const name through sqlchk.Compare — the same
// structural projection used for the Go target (PF-6 semantics, BP-8).
// The compare skeleton is the shared sqlchk.CompareTargets; only the
// Python triple-quoted literal extractor is local.
func Fidelity(targets []FidelityTarget, generated string) []sqlchk.Result {
	byName := map[string]string{}
	for _, lit := range SQLLiterals(generated) {
		if lit.Name != "" {
			if _, dup := byName[lit.Name]; !dup {
				byName[lit.Name] = lit.SQL
			}
		}
	}
	return sqlchk.CompareTargets(targets,
		func(t FidelityTarget) (string, bool) { gen, ok := byName[t.Const]; return gen, ok },
		func(t FidelityTarget) string { return t.Const },
		func(t FidelityTarget) string { return t.QueryID },
		func(t FidelityTarget) string { return t.Source })
}
