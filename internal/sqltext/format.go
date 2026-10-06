package sqltext

import "strings"

// formatIndent is one pretty-print indent level: select-list entries and
// top-level AND/OR continuations.
const formatIndent = "    "

// formatClauseHeads are the top-level keywords that start a fresh line at
// column zero. INTO is deliberately absent (INSERT INTO / MERGE INTO stay on
// their statement head); FROM after DELETE and SET after UPDATE are held on
// the same line through the prevWord exceptions in Format.
var formatClauseHeads = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "HAVING": true,
	"GROUP": true, "ORDER": true, "CONNECT": true, "START": true,
	"UNION": true, "MINUS": true, "INTERSECT": true,
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true,
	"USING": true, "ON": true, "SET": true, "VALUES": true,
	"WHEN": true, "RETURNING": true, "FOR": true,
}

// formatSelectPrefixes ride the SELECT head line (`SELECT DISTINCT a, …`).
var formatSelectPrefixes = map[string]bool{"DISTINCT": true, "ALL": true, "UNIQUE": true}

// Format renders SQL in the shared pretty layout: top-level clauses start at
// column zero, select-list entries, SET assignments, RETURNING items,
// multi-row VALUES tuples and top-level AND/OR continuations are indented
// one level, INSERT column lists and VALUES tuples split one item per line
// inside their parens, and BETWEEN's AND never splits. Whitespace outside
// string literals, quoted identifiers and comments is normalized; token
// text, casing and literals are untouched, so the result stays executable
// SQL and Format is idempotent. Empty input stays empty.
func Format(sql string) string {
	toks := formatTokens(sql)
	if len(toks) == 0 {
		return ""
	}
	var w formatWriter
	w.atLineStart = true
	depth := 0
	inSelect := false
	selectHead := false
	caseDepth := 0
	between := 0
	prevWord := ""
	// lastClause tracks the most recent top-level clause head so paren
	// lists can tell INSERT/VALUES tuples (split) from function args,
	// ON/USING parens and subqueries (kept inline). parenStack remembers
	// that clause per open paren; only depth-1 commas under INSERT/VALUES
	// split, deeper function commas never do.
	lastClause := ""
	var parenStack []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				// A paren opening a subquery (`(SELECT …)`, as in
				// INSERT..SELECT or USING (SELECT …)) keeps the old inline
				// nested-select layout; only real INSERT/VALUES item lists
				// split. Peek the next token so the SELECT's own commas
				// (depth 1 under an INSERT) don't get claimed as list
				// items while FROM/WHERE inside stay inline.
				top := lastClause
				if i+1 < len(toks) && toks[i+1].kind == tokWord && strings.EqualFold(toks[i+1].text, "SELECT") {
					top = "QUERY"
				}
				parenStack = append(parenStack, top)
				depth++
			case ")":
				if len(parenStack) > 0 {
					parenStack = parenStack[:len(parenStack)-1]
				}
				if depth > 0 {
					depth--
				}
			}
		}
		up := ""
		if t.kind == tokWord {
			up = strings.ToUpper(t.text)
		}
		// `SELECT DISTINCT` rides the head line; the first other token opens
		// the indented select list.
		if selectHead && !(t.kind == tokWord && formatSelectPrefixes[up]) {
			w.newline(formatIndent)
			selectHead = false
		}
		if t.kind == tokWord && depth == 0 {
			switch {
			case up == "SELECT":
				w.newline("")
				inSelect = true
				selectHead = true
				lastClause = "SELECT"
			case formatClauseHeads[up] &&
				!(up == "FROM" && prevWord == "DELETE") &&
				!(up == "SET" && prevWord == "UPDATE") &&
				!(up == "WHEN" && caseDepth > 0):
				w.newline("")
				inSelect = false
				lastClause = up
			case formatClauseHeads[up]:
				// Held on the same line (DELETE FROM, MERGE's UPDATE SET,
				// CASE's WHEN) but still a new list context.
				inSelect = false
				lastClause = up
			}
			switch up {
			case "CASE":
				caseDepth++
			case "END":
				if caseDepth > 0 {
					caseDepth--
				}
			case "BETWEEN":
				between++
			case "AND":
				if between > 0 {
					between--
				} else {
					w.newline(formatIndent)
				}
			case "OR":
				w.newline(formatIndent)
			}
		}
		w.write(t)
		if t.kind == tokPunct && t.text == "," {
			switch {
			case depth == 0 && inSelect:
				w.newline(formatIndent)
			case depth == 0 && (lastClause == "SET" || lastClause == "RETURNING" || lastClause == "VALUES"):
				// UPDATE SET assignments, RETURNING items and
				// inter-tuple VALUES commas: one entry per line.
				w.newline(formatIndent)
			case depth == 1 && len(parenStack) > 0 && (parenStack[len(parenStack)-1] == "INSERT" || parenStack[len(parenStack)-1] == "VALUES"):
				// INSERT column lists and VALUES tuples: one item per
				// line. Deeper commas (function args like
				// NVL(a, b) inside the list) stay inline.
				w.newline(formatIndent)
			}
		}
		if t.kind == tokLineComment {
			w.newline("")
		}
		if t.kind == tokWord {
			prevWord = up
		}
	}
	return strings.TrimRight(w.sb.String(), " \t\n\r")
}

// tokKind classifies one lexical token; kind drives keyword handling and
// comment newlines, never token text.
type tokKind uint8

const (
	tokWord tokKind = iota
	tokLiteral
	tokQuoted
	tokBind
	tokPunct
	tokLineComment
	tokBlockComment
)

type formatToken struct {
	text  string
	space bool
	kind  tokKind
}

// formatTokens splits SQL into whitespace-normalized tokens. Whitespace runs
// collapse to a single pending space per token; literal, quoted-identifier,
// bind and comment spans stay byte-identical.
func formatTokens(sql string) []formatToken {
	var out []formatToken
	space := false
	for i := 0; i < len(sql); {
		c := sql[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			space = true
			i++
			continue
		}
		start := i
		var kind tokKind
		switch {
		case c == '\'':
			i = skipLit(sql, i) + 1
			kind = tokLiteral
		case c == '"':
			i = skipDq(sql, i) + 1
			kind = tokQuoted
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			kind = tokLineComment
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(sql)
			}
			kind = tokBlockComment
		case c == ':' && i+1 < len(sql) && isIdentStart(sql[i+1]):
			i += 2
			for i < len(sql) && isIdentByte(sql[i]) {
				i++
			}
			kind = tokBind
		case isIdentByte(c):
			i++
			for i < len(sql) && isIdentByte(sql[i]) {
				i++
			}
			kind = tokWord
		default:
			i++
			kind = tokPunct
		}
		out = append(out, formatToken{text: sql[start:i], space: space, kind: kind})
		space = false
	}
	return out
}

// formatWriter accumulates the laid-out SQL, owning line starts, indentation
// and inter-token spacing.
type formatWriter struct {
	sb          strings.Builder
	atLineStart bool
	indent      string
	prev        formatToken
	hasPrev     bool
}

// newline ends the current line; on an empty line it just retargets the
// pending indent (so a clause head overrides a select-item break).
func (w *formatWriter) newline(indent string) {
	if w.atLineStart {
		w.indent = indent
		return
	}
	w.sb.WriteByte('\n')
	w.atLineStart = true
	w.indent = indent
	w.hasPrev = false
}

// write emits one token: indent on a fresh line, one space when the source
// had whitespace and the pair tolerates it, nothing when the source had the
// tokens adjacent (`NVL(`, `t.col`, `:bind`).
func (w *formatWriter) write(t formatToken) {
	switch {
	case w.atLineStart:
		w.sb.WriteString(w.indent)
		w.atLineStart = false
		w.indent = ""
	case w.spaceBefore(t):
		w.sb.WriteByte(' ')
	}
	w.sb.WriteString(t.text)
	w.prev = t
	w.hasPrev = true
	if t.kind == tokBlockComment && strings.ContainsAny(t.text, "\n\r") {
		w.atLineStart = true
		w.indent = ""
		w.hasPrev = false
	}
}

func (w *formatWriter) spaceBefore(t formatToken) bool {
	if !w.hasPrev || !t.space {
		return false
	}
	if w.prev.text == "(" || w.prev.text == "." {
		return false
	}
	switch t.text {
	case ")", ",", ";", ".":
		return false
	}
	return true
}

// skipDq mirrors skipLit for double-quoted identifiers (`""` escapes).
func skipDq(sql string, i int) int {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] == '"' {
			if j+1 < len(sql) && sql[j+1] == '"' {
				j++
				continue
			}
			return j
		}
	}
	return len(sql) - 1
}
