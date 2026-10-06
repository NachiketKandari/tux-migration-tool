package tsscan

import (
	"regexp"
	"sort"
	"strings"
)

type pos struct {
	line int
	col  int
}

// sqlRegion is the pre-scan's record of one EXEC SQL construct. start/end are
// byte offsets (end exclusive) covering the whole "EXEC SQL ... ;" region.
type sqlRegion struct {
	start      int
	end        int
	startLine  int
	startCol   int
	endLine    int
	stmt       ExecSQLStatement
	unbalanced bool // ran to EOF; recorded as an UnbalancedRegion, never a statement
}

type prescanResult struct {
	regions      []sqlRegion
	comments     []Comment
	unbalanced   []UnbalancedRegion
	unmatched    []pos
	lineStarts   []int
	numLines     int
	brokenDefine [][2]int    // byte spans of #define lines with unbalanced parens
	debris       [][2]int    // comment-tail debris between an early '*/' and a dangling one
	directives   []Directive // facts preserved from masked broken defines
	openDeficit  int         // unclosed '{' count at EOF (the padding budget)
	openComment  bool        // the file ends inside an unterminated block comment
}

var (
	bannerVerRe  = regexp.MustCompile(`(?i)\bver\.?\s*\d`)
	bannerWordRe = regexp.MustCompile(`(?i)(added|comment|start|end)`)
)

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// matchWord reports whether src[i:] starts with word w delimited by
// non-identifier bytes, ASCII case-insensitively.
func matchWord(src []byte, i int, w string) bool {
	if i+len(w) > len(src) {
		return false
	}
	for k := 0; k < len(w); k++ {
		c := src[i+k]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != w[k] {
			return false
		}
	}
	if i > 0 && isIdentByte(src[i-1]) {
		return false
	}
	if i+len(w) < len(src) && isIdentByte(src[i+len(w)]) {
		return false
	}
	return true
}

// execMarkerEnd returns the end offset of the "EXEC <ws> SQL" marker when
// src[i:] starts with the keyword pair on word boundaries.
func execMarkerEnd(src []byte, i int) (int, bool) {
	if !matchWord(src, i, "exec") {
		return 0, false
	}
	j := i + 4
	for j < len(src) && isSpaceByte(src[j]) {
		j++
	}
	if !matchWord(src, j, "sql") {
		return 0, false
	}
	return j + 3, true
}

func lineStartIndex(src []byte) []int {
	starts := make([]int, 0, 64)
	starts = append(starts, 0)
	for i, b := range src {
		if b == '\n' && i+1 < len(src) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func posAt(lineStarts []int, off int) pos {
	l := sort.SearchInts(lineStarts, off)
	if l < len(lineStarts) && lineStarts[l] == off {
		return pos{line: l + 1, col: 1}
	}
	l--
	if l < 0 {
		l = 0
	}
	return pos{line: l + 1, col: off - lineStarts[l] + 1}
}

// collapseWS collapses whitespace runs to single spaces and trims.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// classifySQL maps normalized statement text to its kind, capturing the
// cursor name (uppercased) for cursor choreography. Keywords match
// case-insensitively; the corpus mixes EXEC SQL include with uppercase.
func classifySQL(norm string) (SQLKind, string) {
	fields := strings.Fields(strings.ToUpper(norm))
	if len(fields) == 0 {
		return SQLOther, ""
	}
	switch fields[0] {
	case "SELECT":
		return SQLSelect, ""
	case "INSERT":
		return SQLInsert, ""
	case "UPDATE":
		return SQLUpdate, ""
	case "DELETE":
		return SQLDelete, ""
	case "MERGE":
		return SQLMerge, ""
	case "COMMIT":
		return SQLCommit, ""
	case "ROLLBACK":
		return SQLRollback, ""
	case "CONNECT":
		return SQLConnect, ""
	case "INCLUDE":
		return SQLInclude, ""
	case "BEGIN", "END":
		if len(fields) >= 3 && fields[1] == "DECLARE" && fields[2] == "SECTION" {
			return SQLDeclareSection, ""
		}
	case "DECLARE":
		if len(fields) >= 4 && fields[2] == "CURSOR" && fields[3] == "FOR" {
			return SQLDeclareCursor, fields[1]
		}
	case "OPEN", "FETCH", "CLOSE":
		if len(fields) >= 2 {
			name := strings.TrimRight(fields[1], ";,")
			switch fields[0] {
			case "OPEN":
				return SQLOpen, name
			case "FETCH":
				return SQLFetch, name
			default:
				return SQLClose, name
			}
		}
	}
	return SQLOther, ""
}

// prescan walks the raw bytes once, collecting: EXEC SQL regions (with the
// documented lenient ';' close), the comment inventory (block/line/banner
// with the live-flag rules), unbalanced regions, and unmatched braces.
func prescan(src []byte) prescanResult {
	starts := lineStartIndex(src)
	r := prescanResult{lineStarts: starts, numLines: len(starts)}
	var stack []pos
	var extraCloses []pos
	lastCommentEnd := -1
	i := 0
	for i < len(src) {
		b := src[i]
		switch {
		case b == '/' && i+1 < len(src) && src[i+1] == '*':
			i = scanBlockComment(src, i, &r)
			lastCommentEnd = i
		case b == '/' && i+1 < len(src) && src[i+1] == '/':
			i = scanLineComment(src, i, &r)
		case b == '"' || b == '\'':
			i = skipString(src, i)
		case b == '*' && i+1 < len(src) && src[i+1] == '/' && lastCommentEnd >= 0:
			// a dangling '*/' in code state: everything since the last real
			// comment ended is comment-tail debris — mask it for the parse
			r.debris = append(r.debris, [2]int{lastCommentEnd, i + 2})
			lastCommentEnd = -1
			i += 2
		case (b == 'E' || b == 'e') && startsExec(src, i):
			i = scanExecSQL(src, i, &r)
		case b == '{':
			stack = append(stack, posAt(r.lineStarts, i))
			i++
		case b == '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			} else {
				extraCloses = append(extraCloses, posAt(r.lineStarts, i))
			}
			i++
		default:
			i++
		}
	}
	// Brace facts follow any exec_sql facts (pinned ordering), and
	// collapse to a single loud record at the first offending position —
	// one defect is one fact, however many unmatched braces trail it.
	r.openDeficit = len(stack)
	if len(stack) > 0 {
		p := stack[0]
		r.unmatched = append(r.unmatched, p)
		r.unbalanced = append(r.unbalanced, UnbalancedRegion{Kind: "braces", StartLine: p.line, StartCol: p.col})
	} else {
		for _, p := range extraCloses {
			r.unmatched = append(r.unmatched, p)
			r.unbalanced = append(r.unbalanced, UnbalancedRegion{Kind: "braces", StartLine: p.line, StartCol: p.col})
		}
	}
	return r
}

func scanBlockComment(src []byte, i int, r *prescanResult) int {
	sPos := posAt(r.lineStarts, i)
	j := i + 2
	closed := false
	for j+1 < len(src) {
		if src[j] == '*' && src[j+1] == '/' {
			j += 2
			closed = true
			break
		}
		j++
	}
	if !closed {
		j = len(src)
	}
	ePos := posAt(r.lineStarts, j-1)
	text := src[i:j]
	kind, live := CommentBlock, false
	if closed && bannerVerRe.Match(text) && bannerWordRe.Match(text) {
		kind, live = CommentBanner, sPos.line == ePos.line
	}
	r.comments = append(r.comments, Comment{
		Kind: kind, StartLine: sPos.line, StartCol: sPos.col,
		EndLine: ePos.line, EndCol: ePos.col, Live: live,
	})
	if !closed {
		r.unbalanced = append(r.unbalanced, UnbalancedRegion{Kind: "block_comment", StartLine: sPos.line, StartCol: sPos.col})
		r.openComment = true
	}
	return j
}

func scanLineComment(src []byte, i int, r *prescanResult) int {
	sPos := posAt(r.lineStarts, i)
	j := i + 2
	for j < len(src) && src[j] != '\n' {
		j++
	}
	last := j - 1
	if last < i {
		last = i
	}
	ePos := posAt(r.lineStarts, last)
	r.comments = append(r.comments, Comment{
		Kind: CommentLine, StartLine: sPos.line, StartCol: sPos.col,
		EndLine: ePos.line, EndCol: ePos.col,
	})
	return j
}

// skipString advances past a C string or character literal (unterminated
// literals end at the newline, like C).
func skipString(src []byte, i int) int {
	quote := src[i]
	j := i + 1
	for j < len(src) {
		switch src[j] {
		case '\\':
			j += 2
			continue
		case quote:
			return j + 1
		case '\n':
			return j
		}
		j++
	}
	return j
}

func startsExec(src []byte, i int) bool {
	_, ok := execMarkerEnd(src, i)
	return ok
}

// scanExecSQL consumes one EXEC SQL construct. The terminator is the first
// ';' outside string/comment contexts; without one, the region closes
// leniently at the next EXEC SQL keyword, and only when that also never
// arrives does the region run to EOF — recorded as an unbalanced exec_sql
// fact and never passed off as a statement. A lenient close is itself a
// defect fact (exec_sql_lenient): the region's span may absorb content
// between the two statements, so downstream consumers see it, never a
// silent merge. Comments found inside the region
// are recorded in the comment inventory and replaced by a single space in
// the statement's Raw text (pinned convention).
func scanExecSQL(src []byte, i int, r *prescanResult) int {
	markerEnd, _ := execMarkerEnd(src, i)
	sPos := posAt(r.lineStarts, i)
	end := len(src)
	endLine := r.numLines
	terminated, lenient := false, false
	k := markerEnd
	quote := byte(0)
	var spans [][2]int
	for k < len(src) {
		c := src[k]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				k += 2
				continue
			}
			if c == quote {
				quote = 0
			}
			k++
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			k++
		case c == '/' && k+1 < len(src) && src[k+1] == '*':
			cs := k
			k += 2
			closed := false
			for k+1 < len(src) {
				if src[k] == '*' && src[k+1] == '/' {
					k += 2
					closed = true
					break
				}
				k++
			}
			if !closed {
				k = len(src)
			}
			recordRegionComment(src, cs, k, closed, r)
			spans = append(spans, [2]int{cs, k})
		case c == ';':
			k++
			terminated = true
		case (c == 'E' || c == 'e') && startsExec(src, k):
			end = k
			endLine = posAt(r.lineStarts, k-1).line
			lenient = true
		default:
			k++
		}
		if terminated || lenient {
			break
		}
	}
	switch {
	case terminated:
		end = k
		endLine = posAt(r.lineStarts, k-1).line
	case lenient:
		// end/endLine already set to the next EXEC keyword. Loud: without
		// the ';', this region's span absorbs whatever sits between the two
		// statements, so the close is recorded as a defect fact.
		r.unbalanced = append(r.unbalanced, UnbalancedRegion{Kind: "exec_sql_lenient", StartLine: sPos.line, StartCol: sPos.col})
	default:
		r.unbalanced = append(r.unbalanced, UnbalancedRegion{Kind: "exec_sql", StartLine: sPos.line, StartCol: sPos.col})
		r.regions = append(r.regions, sqlRegion{start: i, end: end, unbalanced: true, startLine: sPos.line, startCol: sPos.col})
		return end
	}
	rawEnd := end
	if terminated {
		rawEnd = end - 1 // exclude the terminating ';'
	}
	raw := blankedText(src, markerEnd, rawEnd, spans)
	raw = strings.Trim(raw, " \t\r\n")
	norm := collapseWS(raw)
	kind, cursor := classifySQL(norm)
	r.regions = append(r.regions, sqlRegion{
		start: i, end: end,
		startLine: sPos.line, startCol: sPos.col, endLine: endLine,
		stmt: ExecSQLStatement{
			Raw: raw, Normalized: norm, Kind: kind,
			StartLine: sPos.line, StartCol: sPos.col, EndLine: endLine,
			CursorName: cursor,
		},
	})
	return end
}

// blankedText reassembles the region text with each comment span collapsed
// to a single space.
func blankedText(src []byte, from, to int, spans [][2]int) string {
	var b strings.Builder
	b.Grow(to - from)
	k := from
	for _, sp := range spans {
		if sp[0] >= to {
			break
		}
		if sp[0] > k {
			b.Write(src[k:sp[0]])
		}
		b.WriteByte(' ')
		k = sp[1]
		if k > to {
			k = to
		}
	}
	if k < to {
		b.Write(src[k:to])
	}
	return b.String()
}

// recordRegionComment records a comment found inside an EXEC SQL region with
// the same banner rules as code comments.
func recordRegionComment(src []byte, start, end int, closed bool, r *prescanResult) {
	sPos := posAt(r.lineStarts, start)
	ePos := posAt(r.lineStarts, end-1)
	kind, live := CommentBlock, false
	if closed && bannerVerRe.Match(src[start:end]) && bannerWordRe.Match(src[start:end]) {
		kind, live = CommentBanner, sPos.line == ePos.line
	}
	r.comments = append(r.comments, Comment{
		Kind: kind, StartLine: sPos.line, StartCol: sPos.col,
		EndLine: ePos.line, EndCol: ePos.col, Live: live,
	})
}

// mask rewrites each EXEC SQL region into a same-length block comment, every
// empty char literal (” — invalid C that the corpus uses freely) into "0 ",
// and each broken #define line into a comment so the grammar sees valid C.
// Every newline is preserved, so all downstream line/column positions stay
// identical to the original file.
func mask(src []byte, regions []sqlRegion, broken, debris [][2]int) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	for _, rg := range regions {
		if rg.end-rg.start < 4 {
			continue
		}
		out[rg.start] = '/'
		out[rg.start+1] = '*'
		for k := rg.start + 2; k < rg.end; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
		e := rg.end
		for e > rg.start+4 && (out[e-1] == '\n' || out[e-1] == '\r') {
			e--
		}
		out[e-2] = '*'
		out[e-1] = '/'
	}
	broken = append(append([][2]int(nil), broken...), debris...)
	for _, sp := range broken {
		if sp[1]-sp[0] < 4 {
			continue
		}
		out[sp[0]] = '/'
		out[sp[0]+1] = '*'
		for k := sp[0] + 2; k < sp[1]; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
		e := sp[1]
		for e > sp[0]+4 && (out[e-1] == '\n' || out[e-1] == '\r') {
			e--
		}
		out[e-2] = '*'
		out[e-1] = '/'
	}
	maskEmptyCharLiterals(out)
	return out
}

// maskEmptyCharLiterals rewrites code-state ” occurrences to "0 " in place
// (same length, valid C, same value: an empty char is '\0' is 0).
func maskEmptyCharLiterals(out []byte) {
	i := 0
	for i < len(out) {
		switch {
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '*':
			i += 2
			for i+1 < len(out) && !(out[i] == '*' && out[i+1] == '/') {
				i++
			}
			i += 2
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			for i < len(out) && out[i] != '\n' {
				i++
			}
		case out[i] == '\'' && i+1 < len(out) && out[i+1] == '\'':
			out[i], out[i+1] = '0', ' '
			i += 2
		case out[i] == '"' || out[i] == '\'':
			q := out[i]
			i++
			for i < len(out) {
				if out[i] == '\\' {
					i += 2
					continue
				}
				if out[i] == q || out[i] == '\n' {
					i++
					break
				}
				i++
			}
		default:
			i++
		}
	}
}

// scanBrokenDefines finds #define lines whose parenthesization is unbalanced
// at end of line — malformed macros that would derail the grammar. Their
// byte spans are returned for masking and their directive facts preserved.
func scanBrokenDefines(src []byte, starts []int) ([][2]int, []Directive) {
	var spans [][2]int
	var dirs []Directive
	i := 0
	for i < len(src) {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case src[i] == '"' || src[i] == '\'':
			i = skipString(src, i)
		case src[i] == '#':
			// directive start: only ws between line start and '#'
			lineStart := i
			k := lineStart - 1
			for k >= 0 && (src[k] == ' ' || src[k] == '\t') {
				k--
			}
			if k >= 0 && src[k] != '\n' {
				i++
				continue
			}
			eol := i
			for eol < len(src) && src[eol] != '\n' {
				eol++
			}
			line := src[i:eol]
			if _, rest, ok := cutDefineHead(line); ok {
				balanced, _ := parenBalance(rest)
				if !balanced {
					spans = append(spans, [2]int{lineStart, eol})
					full := string(line)
					if idx := strings.Index(full, "define"); idx >= 0 {
						full = full[idx+len("define"):]
					}
					dirs = append(dirs, Directive{
						Kind: "define",
						Arg:  strings.Trim(stripTrailingComment(full), " \t\r"),
						Line: posAt(starts, lineStart).line,
					})
				}
			}
			i = eol
		default:
			i++
		}
	}
	return spans, dirs
}

// cutDefineHead splits "#define NAME" from the rest of the line.
func cutDefineHead(line []byte) (name string, rest []byte, ok bool) {
	s := line
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	if len(s) > 0 && s[0] == '#' {
		s = s[1:]
		for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
			s = s[1:]
		}
	}
	if !matchWord(s, 0, "define") {
		return "", nil, false
	}
	s = s[6:]
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	n := 0
	for n < len(s) && isIdentByte(s[n]) {
		n++
	}
	if n == 0 {
		return "", nil, false
	}
	return string(s[:n]), s[n:], true
}

// parenBalance scans text (strings skipped) for paren balance.
func parenBalance(s []byte) (bool, int) {
	depth, i := 0, 0
	for i < len(s) {
		switch s[i] {
		case '"', '\'':
			i = skipString(s, i)
			continue
		case '(':
			depth++
		case ')':
			depth--
		}
		i++
	}
	return depth == 0, depth
}

// stripTrailingComment cuts a trailing // or /* */ comment from a directive
// line (comments inside string literals are respected).
func stripTrailingComment(s string) string {
	quote := byte(0)
	last := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '"' || c == '\'':
			quote = c
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			last = i
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			return strings.TrimRight(s[:i], " \t")
		}
	}
	if last >= 0 {
		return strings.TrimRight(s[:last], " \t")
	}
	return s
}
