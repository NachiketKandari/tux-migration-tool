package pystmt

import (
	"strings"

	scanner "tux-migration-tool/internal/tsscan"
)

// Source-indexed lexical helpers.
//
// The statement layer needs three things the scanner's fact vocabulary does
// not carry: the byte offset behind a line/column pair, the extent of a
// statement that starts mid-line (Pro*C writes `if(x) i_row = 0;` on one line
// 24 times on dtcmpt.pc), and whether a line carries anything but whitespace
// and comments. All three are mechanical, so they live here rather than
// growing the shared scanner — tsscan is a pinned vocabulary and Phase 2 must
// not perturb it.

// source indexes a .pc file by line with the byte offset of each line's first
// byte, so a fact's 1-based line/column pair converts to a slice offset.
type source struct {
	lines  []string
	starts []int
	// comments is the scanner's comment inventory, queried per position so a
	// `;` or a `}` inside a comment never ends a statement or a block.
	comments *scanner.SourceFacts
}

func newSource(src string, facts *scanner.SourceFacts) *source {
	s := &source{lines: strings.Split(src, "\n"), comments: facts}
	s.starts = make([]int, len(s.lines)+1)
	off := 0
	for i, l := range s.lines {
		s.starts[i] = off
		off += len(l) + 1 // +1 for the newline
	}
	s.starts[len(s.lines)] = off
	return s
}

func (s *source) line(n int) string {
	if n < 1 || n > len(s.lines) {
		return ""
	}
	return s.lines[n-1]
}

// byteAt converts a 1-based line/column to a 0-based byte offset, clamped so a
// fact pointing past the end of a line (an unbalanced construct) still yields
// a sliceable offset rather than a panic.
func (s *source) byteAt(line, col int) int {
	if line < 1 || line > len(s.lines) {
		return 0
	}
	off := s.starts[line-1] + col - 1
	if col < 1 {
		off = s.starts[line-1]
	}
	if off > s.starts[line] {
		off = s.starts[line]
	}
	return off
}

func (s *source) inComment(line, col int) bool {
	if s.comments == nil {
		return false
	}
	return s.comments.InComment(line, col)
}

// codeOnly blanks every comment range on a line and trims the result. It is
// what decides whether a line carries code, and what statement text is built
// from — so a `/* Ver 1.4 */` trailing comment cannot become part of a
// statement's recorded text.
//
// Comment spans are taken from the scanner rather than re-lexed: a multi-line
// block comment opens on one line and closes on another, and only the scanner
// knows that.
func (s *source) codeOnly(line int) string {
	text := s.line(line)
	if text == "" {
		return ""
	}
	if s.comments == nil {
		return strings.TrimSpace(text)
	}
	var b strings.Builder
	col := 1
	for col <= len(text) {
		if s.inComment(line, col) {
			b.WriteByte(' ')
			col++
			continue
		}
		b.WriteByte(text[col-1])
		col++
	}
	return strings.TrimSpace(b.String())
}

// bare strips a line's trailing comment without consulting the scanner. It is
// the last-resort path for a construct the scanner never saw (a fragment
// file, a line inside an unbalanced region) where a `//` in a message string
// would otherwise truncate the text.
func bare(text string) string {
	for i := 0; i < len(text); i++ {
		if text[i] == '"' || text[i] == '\'' {
			i = scanLiteral(text, i, text[i]) - 1
			continue
		}
		if text[i] == '/' && i+1 < len(text) && (text[i+1] == '/' || text[i+1] == '*') {
			return strings.TrimSpace(text[:i])
		}
	}
	return strings.TrimSpace(text)
}

// isPreproc reports a preprocessor line. A `#` inside a function body is not a
// statement and must not be classified as one.
func isPreproc(text string) bool { return strings.HasPrefix(strings.TrimSpace(text), "#") }

// isBraceOnly reports a line whose code is nothing but block punctuation —
// braces, possibly with a trailing comment, or a lone `;`. Such a line belongs
// to no statement, so Coverage accounts for it separately rather than letting
// the statement classifier invent an Expr for it.
func isBraceOnly(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != '{' && t[i] != '}' && t[i] != ';' {
			return false
		}
	}
	return true
}

// hasCode reports whether a line carries anything other than whitespace and
// comments.
func (s *source) hasCode(line int) bool { return s.codeOnly(line) != "" }

// stmtExtent returns the last (line, col) of the statement beginning at
// (line, col): the first `;` at bracket depth ≤ 0, or — for a construct with
// no terminator — the end of the last line it occupies up to `limit`.
//
// It stops only at `;`, never at `{`. That is what lets a brace-initialised
// declaration (`int mnth_val[25][12] = {{…}};`, dtcmpt.pc:71-95, 25 lines of
// initialiser) come back as one statement spanning all of them instead of
// being cut at the first row.
//
// Bracket depth, not paren depth alone, is tracked because `mnth_val[i_row][j]`
// and `argv[1]` are ordinary here.
func (s *source) stmtExtent(line, col, limit int) (int, int) {
	depth := 0
	last := line
	for l := line; l <= limit && l <= len(s.lines); l++ {
		text := s.line(l)
		for i := col - 1; i < len(text); i++ {
			if s.inComment(l, i+1) {
				continue
			}
			switch c := text[i]; c {
			case '"', '\'':
				i = scanLiteral(text, i, c) - 1
			case '(', '[':
				depth++
			case ')', ']':
				if depth > 0 {
					depth--
				}
			case ';':
				if depth <= 0 {
					return l, i + 2
				}
			default:
				last = l
			}
		}
		col = 1
		if l > line {
			last = l
		}
	}
	return last, len(s.line(last)) + 1
}

// headerEnd returns the column just past the closing paren of a control
// header on one line — `if (a == b)` → the column after `)`. It is what
// separates an unbraced arm's body from its condition, 24 times over on
// dtcmpt.pc:361-384 (`if(strcmp(…,"M00") == 0) i_row=0;`).
//
// It returns 0 when the header does not close on this line, which is the
// caller's signal that the body starts on a later line and that the header
// line itself carries no statement.
func (s *source) headerEnd(line, col int) int {
	text := s.line(line)
	depth := 0
	for i := col - 1; i < len(text); i++ {
		if s.inComment(line, i+1) {
			continue
		}
		switch c := text[i]; c {
		case '"', '\'':
			i = scanLiteral(text, i, c) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 2
			}
		}
	}
	return 0
}

// closeParen returns the 1-based column of the `)` that closes the header
// starting at `col`, or 0 when it does not close within `limit`.
func (s *source) closeParen(line, col, limit int) int {
	if n := s.headerEnd(line, col); n > 0 {
		return n - 1
	}
	depth := 0
	for l := line; l <= limit && l <= len(s.lines); l++ {
		text := s.line(l)
		start := 0
		if l == line {
			start = col - 1
		}
		for i := start; i < len(text); i++ {
			if s.inComment(l, i+1) {
				continue
			}
			switch c := text[i]; c {
			case '"', '\'':
				i = scanLiteral(text, i, c) - 1
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	return 0
}

// firstCodeCol returns the 1-based column of the first non-space byte of a
// line that is not a comment, or 0 when the line carries no code.
func (s *source) firstCodeCol(line int) int {
	text := s.line(line)
	for i := 0; i < len(text); i++ {
		if s.inComment(line, i+1) {
			continue
		}
		if text[i] != ' ' && text[i] != '\t' {
			return i + 1
		}
	}
	return 0
}

// braceBlock matches braces from the first `{` at or after (line, col) and
// returns the lines of the opening and closing brace. String literals and
// comments are skipped so a brace inside a userlog message cannot end a block
// early — the same rule pycensus applies, for the same reason.
func (s *source) braceBlock(line, col, limit int) (open, close int, ok bool) {
	depth := 0
	for l := line; l <= limit && l <= len(s.lines); l++ {
		text := s.line(l)
		start := 0
		if l == line {
			start = col - 1
		}
		for i := start; i < len(text); i++ {
			if s.inComment(l, i+1) {
				continue
			}
			switch c := text[i]; c {
			case '"', '\'':
				i = scanLiteral(text, i, c) - 1
			case '{':
				if depth == 0 {
					open = l
				}
				depth++
			case '}':
				depth--
				if depth == 0 && open > 0 {
					return open, l, true
				}
			}
		}
	}
	return 0, 0, false
}

// blockBraceCol returns the 1-based column of the `{` that opens a control
// header's body on the header line itself, or 0 when the brace is not there
// (the `{` is on the next line — the shape dtcmpt.pc uses for every `if`).
func (s *source) blockBraceCol(line, col int) int {
	text := s.line(line)
	for i := col - 1; i < len(text); i++ {
		if s.inComment(line, i+1) {
			continue
		}
		switch c := text[i]; c {
		case '"', '\'':
			i = scanLiteral(text, i, c) - 1
		case '{':
			return i + 1
		case ';':
			return 0
		}
	}
	return 0
}

// cutKeyword removes a leading C keyword when it appears as a whole word,
// returning the remainder and whether the keyword was there. Whole-word
// matters: `do` must not match a variable named `double_cnt`, and `case` must
// not match `case_no`.
func cutKeyword(s, kw string) (string, bool) {
	s = strings.TrimLeft(s, " \t")
	if len(s) < len(kw) || !strings.EqualFold(s[:len(kw)], kw) {
		return s, false
	}
	if len(s) > len(kw) && isIdentByte(s[len(kw)]) {
		return s, false
	}
	return strings.TrimLeft(s[len(kw):], " \t"), true
}

// splitTop splits on a separator that is at bracket depth 0 and outside
// literals. The for header needs it: `for (a = 0; a < n && v[i] > 0; a++)`
// has to become three clauses, and neither a plain `strings.Split` on `;` nor
// a naive scan survives the `;` inside a nested call or the `;` inside a
// string.
func splitTop(text, sep string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '"', '\'':
			i = scanLiteral(text, i, c) - 1
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && strings.HasPrefix(text[i:], sep) {
				out = append(out, strings.TrimSpace(text[start:i]))
				i += len(sep) - 1
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(text[start:]))
}

// collapse normalises whitespace for a statement's recorded Text, so two
// spellings of one statement are comparable and a multi-line statement is one
// line in the linear stream.
func collapse(text string) string { return strings.Join(strings.Fields(text), " ") }