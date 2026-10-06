package ir

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// buildQueries folds query-kind SQL statements into units: plain statements
// become q<N> units (N = the statement's ordinal among query-kind
// statements, cursors included in the count); cursor DECLAREs flatten the
// DECLARE→OPEN→FETCH→CLOSE choreography into one multi-row SELECT named by
// the cursor. Duplicates stay factual via DedupKey/DuplicateOf.
func buildQueries(facts *tsscan.SourceFacts, ops []FmlOp) []*Query {
	fetchInto := fetchIntoByCursor(facts)
	var out []*Query
	counter := 0
	for _, s := range facts.Queries {
		counter++
		switch s.Kind {
		case tsscan.SQLDeclareCursor:
			out = append(out, cursorUnit(facts, s, fetchInto))
		case tsscan.SQLSelect:
			out = append(out, selectUnit(s, counter))
		case tsscan.SQLInsert, tsscan.SQLUpdate, tsscan.SQLDelete, tsscan.SQLMerge:
			out = append(out, dmlUnit(s, counter))
		default:
			counter-- // non-query kinds never consumed a number
		}
	}
	linkDuplicates(out)
	return out
}

// cursorUnit flattens one cursor's choreography into a SELECT_MULTI unit
// spanning DECLARE→last CLOSE (or the DECLARE itself when never closed —
// ghost and half cursors flatten just the same).
func cursorUnit(facts *tsscan.SourceFacts, s tsscan.ExecSQLStatement, fetchInto map[string][]string) *Query {
	rawName := cursorRawName(s.Normalized)
	body := cursorBody(s.Normalized)

	q := &Query{
		ID:              rawName,
		Type:            QuerySelectMulti,
		TemplateID:      QuerySelectMulti.TemplateID(),
		SQL:             body,
		StartLine:       s.StartLine,
		EndLine:         s.EndLine,
		OwningFunction:  s.Func,
		CursorName:      rawName,
		CursorFlattened: true,
		Sites:           []int{s.StartLine},
	}
	q.Tables, q.Aliases = parseTables(body, tsscan.SQLSelect)
	q.Binds, q.BindArity = collectBinds(scanHostRefs(body, 0, len(body)))
	q.OrderBy = parseOrderBy(body)
	q.RowShape = fetchInto[strings.ToUpper(rawName)]
	q.DedupKey = body

	// extend the span to the cursor's last choreography statement
	// (OPEN/FETCH/CLOSE — declare-only cursors end at the DECLARE)
	for _, other := range facts.AllSQL {
		if other.CursorName == "" || !SameCursor(other.CursorName, strings.ToUpper(rawName)) {
			continue
		}
		if other.Kind == tsscan.SQLOpen || other.Kind == tsscan.SQLFetch || other.Kind == tsscan.SQLClose {
			if other.EndLine > q.EndLine {
				q.EndLine = other.EndLine
			}
		}
	}
	return q
}

// cursorRawName recovers the cursor's raw-case name from normalized text.
func cursorRawName(norm string) string {
	fields := strings.Fields(norm)
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

// cursorBody is the SELECT body after "CURSOR FOR".
func cursorBody(norm string) string {
	up := strings.ToUpper(norm)
	idx := strings.Index(up, "CURSOR FOR")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(norm[idx+len("CURSOR FOR"):])
}

// fetchIntoByCursor maps uppercased cursor names to the INTO list of their
// first FETCH.
func fetchIntoByCursor(facts *tsscan.SourceFacts) map[string][]string {
	out := map[string][]string{}
	for _, s := range facts.AllSQL {
		if s.Kind != tsscan.SQLFetch {
			continue
		}
		name := strings.ToUpper(s.CursorName)
		if _, ok := out[name]; ok {
			continue
		}
		if into := parseIntoList(s.Normalized); len(into) > 0 {
			out[name] = into
		}
	}
	return out
}

// selectUnit builds a plain SELECT unit. The INTO span is positional: host
// refs inside it are the row shape, never binds; the same name bound
// elsewhere (e.g. ROUND(:x,-2) INTO :x) still counts as a bind.
func selectUnit(s tsscan.ExecSQLStatement, counter int) *Query {
	binds, into := splitSelectRefs(s.Normalized)
	qtype := QuerySelectMulti
	if len(into) > 0 {
		qtype = QuerySelectSingle
	}
	q := &Query{
		ID:             fmt.Sprintf("q%d", counter),
		Type:           qtype,
		TemplateID:     qtype.TemplateID(),
		SQL:            s.Normalized,
		StartLine:      s.StartLine,
		EndLine:        s.EndLine,
		OwningFunction: s.Func,
		Sites:          []int{s.StartLine},
	}
	q.Tables, q.Aliases = parseTables(s.Normalized, tsscan.SQLSelect)
	q.Binds, q.BindArity = collectBinds(binds)
	q.OrderBy = parseOrderBy(s.Normalized)
	q.RowShape = into
	q.DedupKey = s.Normalized
	return q
}

// dmlUnit builds INSERT/UPDATE/DELETE/MERGE units; every :name is a bind.
func dmlUnit(s tsscan.ExecSQLStatement, counter int) *Query {
	qtype := QueryInsert
	switch s.Kind {
	case tsscan.SQLUpdate:
		qtype = QueryUpdate
	case tsscan.SQLDelete:
		qtype = QueryDelete
	case tsscan.SQLMerge:
		qtype = QueryMerge
	}
	q := &Query{
		ID:             fmt.Sprintf("q%d", counter),
		Type:           qtype,
		TemplateID:     qtype.TemplateID(),
		SQL:            s.Normalized,
		StartLine:      s.StartLine,
		EndLine:        s.EndLine,
		OwningFunction: s.Func,
		Sites:          []int{s.StartLine},
	}
	q.Tables, q.Aliases = parseTables(s.Normalized, s.Kind)
	q.Binds, q.BindArity = collectBinds(scanHostRefs(s.Normalized, 0, len(s.Normalized)))
	q.DedupKey = s.Normalized
	return q
}

// linkDuplicates marks later units with identical dedup keys as duplicates
// of the first unit in the group (never removed — dedup stays factual).
func linkDuplicates(qs []*Query) {
	first := map[string]string{}
	for _, q := range qs {
		if q.DedupKey == "" {
			continue
		}
		if id, ok := first[q.DedupKey]; ok {
			q.DuplicateOf = id
			continue
		}
		first[q.DedupKey] = q.ID
	}
}

// parseIntoList extracts the INTO target list (`:a, :b`) of a SELECT or
// FETCH — the row shape; INTO outputs are never binds. Array-occurrence
// targets keep their subscript ("px_jthldr[0]").
func parseIntoList(norm string) []string {
	idx := topKeywordIndex(norm, "INTO")
	if idx < 0 {
		return nil
	}
	rest := norm[idx+len("INTO"):]
	return intoNames(rest)
}

// intoNames captures :name entries (with optional dotted struct members,
// [subscripts], and indicator variables — glued, spaced, or the INDICATOR
// keyword form) until a clause keyword. Indicators are not row columns.
func intoNames(s string) []string {
	var out []string
	depth := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '(':
			depth++
		case s[i] == ')':
			if depth > 0 {
				depth--
			}
		case depth == 0 && s[i] == ':':
			// ": name" — whitespace between the colon and the host name is
			// legal Pro*C (reformatted sources ship it); scan past it.
			k := i + 1
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			j := k
			for j < len(s) && isIdentByteFor(s[j]) {
				j++
			}
			if j == k {
				continue
			}
			start := k
			name := s[start:j]
			for j+1 < len(s) && s[j] == '.' && isIdentByteFor(s[j+1]) {
				k := j + 1
				for k < len(s) && isIdentByteFor(s[k]) {
					k++
				}
				name, j = s[start:k], k
			}
			if j < len(s) && s[j] == '[' {
				k2 := j
				for k2 < len(s) && s[k2] != ']' {
					k2++
				}
				if k2 < len(s) {
					name = s[start : k2+1]
					j = k2 + 1
				}
			}
			j = skipIndicator(s, j)
			out = append(out, name)
			i = j - 1
		case depth == 0 && wordStarts(s, i):
			w := wordAt(s, i)
			up := strings.ToUpper(w)
			if up == "FROM" || up == "WHERE" {
				return out
			}
		}
	}
	return out
}

// skipIndicator consumes an indicator variable after a host target: the
// glued ":var:ind" form, the spaced ":var :ind" form, or the INDICATOR
// keyword form ":var INDICATOR :ind".
func skipIndicator(s string, j int) int {
	k := j
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		k++
	}
	if k < len(s) && s[k] == ':' {
		e := k + 1
		for e < len(s) && isIdentByteFor(s[e]) {
			e++
		}
		if e > k+1 {
			return e
		}
		return j
	}
	if k+len("INDICATOR") <= len(s) && equalFold(s[k:k+len("INDICATOR")], "INDICATOR") &&
		isWordBoundaryAfter(s, k+len("INDICATOR")) {
		k2 := k + len("INDICATOR")
		for k2 < len(s) && (s[k2] == ' ' || s[k2] == '\t') {
			k2++
		}
		if k2 < len(s) && s[k2] == ':' {
			e := k2 + 1
			for e < len(s) && isIdentByteFor(s[e]) {
				e++
			}
			if e > k2+1 {
				return e
			}
		}
		return k2
	}
	return j
}

// collectBinds dedups host refs in first-appearance order.
func collectBinds(parts ...[]string) ([]string, int) {
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		for _, n := range part {
			if seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, len(out)
}

// splitSelectRefs splits a SELECT's host references by the INTO span: refs
// inside it are outputs (the row shape), everything else is a bind.
func splitSelectRefs(norm string) (binds, into []string) {
	intoStart := topKeywordIndex(norm, "INTO")
	if intoStart < 0 {
		return scanHostRefs(norm, 0, len(norm)), nil
	}
	intoEnd := clauseEndAfter(norm, intoStart+len("INTO"))
	binds = append(scanHostRefs(norm, 0, intoStart), scanHostRefs(norm, intoEnd, len(norm))...)
	into = intoNames(norm[intoStart+len("INTO") : intoEnd])
	return binds, into
}

// scanHostRefs captures the base names of :host refs in s[from:to];
// array-occurrence subscripts are consumed and stripped. Whitespace between
// the colon and the name is legal Pro*C (": var") and is scanned past.
func scanHostRefs(s string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		if s[i] != ':' {
			continue
		}
		k := i + 1
		for k < to && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		j := k
		for j < to && isIdentByteFor(s[j]) {
			j++
		}
		if j == k {
			continue
		}
		name := s[k:j]
		if j < to && s[j] == '[' {
			k := j
			for k < to && s[k] != ']' {
				k++
			}
			if k < to {
				j = k + 1
			}
		}
		i = j - 1
		out = append(out, name)
	}
	return out
}

// clauseEndAfter finds the next top-level clause keyword at or after from.
func clauseEndAfter(s string, from int) int {
	depth := 0
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && isWordBoundaryBefore(s, i) {
				for _, kw := range []string{"FROM", "WHERE", "ORDER", "GROUP"} {
					if i+len(kw) <= len(s) && equalFold(s[i:i+len(kw)], kw) && isWordBoundaryAfter(s, i+len(kw)) {
						return i
					}
				}
			}
		}
	}
	return len(s)
}

// parseTables extracts the table names (and aliases) for the statement
// kind, from the clause the kind owns.
func parseTables(norm string, kind tsscan.SQLKind) ([]string, []string) {
	var from string
	up := strings.ToUpper(norm)
	switch kind {
	case tsscan.SQLSelect, tsscan.SQLDelete:
		from = "FROM"
	case tsscan.SQLInsert:
		from = "INTO"
		if idx := strings.Index(up, "INSERT INTO"); idx >= 0 {
			return tableList(norm[idx+len("INSERT INTO"):])
		}
		if idx := topKeywordIndex(norm, "INTO"); idx >= 0 {
			return tableList(norm[idx+len("INTO"):])
		}
		return nil, nil
	case tsscan.SQLUpdate:
		if idx := strings.Index(up, "UPDATE"); idx >= 0 {
			return tableList(norm[idx+len("UPDATE"):])
		}
		return nil, nil
	case tsscan.SQLMerge:
		if idx := strings.Index(up, "MERGE INTO"); idx >= 0 {
			// single token: "MERGE INTO <table>" (the pinned convention drops
			// the merge alias)
			rest := strings.Fields(norm[idx+len("MERGE INTO"):])
			if len(rest) > 0 {
				return []string{rest[0]}, nil
			}
			return nil, nil
		}
		from = "INTO"
	}
	idx := topKeywordIndex(norm, from)
	if idx < 0 {
		return nil, nil
	}
	return tablesAfterFrom(norm[idx+len(from):])
}

// tablesAfterFrom parses the clause after FROM; a parenthesized subquery
// recurses into its own FROM list.
func tablesAfterFrom(clause string) ([]string, []string) {
	trimmed := strings.TrimLeft(clause, " \t\r\n")
	if strings.HasPrefix(trimmed, "(") {
		if inner, ok := balancedPrefix(trimmed); ok {
			if fromIdx := topKeywordIndex(inner, "FROM"); fromIdx >= 0 {
				return tablesAfterFrom(inner[fromIdx+len("FROM"):])
			}
		}
	}
	return tableList(clause)
}

// balancedPrefix returns the text between the leading '(' and its match.
func balancedPrefix(s string) (string, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// tableList parses a comma-separated table list, stopping at the first
// clause keyword; entries may carry aliases. Aliases compact to the
// non-empty ones (the field is omitted when none exist, per the goldens).
func tableList(s string) ([]string, []string) {
	var tables, all []string
	depth := 0
	start := 0
	flush := func(piece string) {
		fields := strings.Fields(piece)
		if len(fields) == 0 {
			return
		}
		tables = append(tables, fields[0])
		if len(fields) >= 2 && !clauseKeyword(fields[1]) && fields[1][0] != '(' {
			all = append(all, fields[1])
		}
	}
	ret := func() ([]string, []string) {
		var aliases []string
		for _, a := range all {
			if a != "" {
				aliases = append(aliases, a)
			}
		}
		return tables, aliases
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && wordStarts(s, i):
			w := wordAt(s, i)
			if clauseKeyword(w) {
				if piece := strings.TrimSpace(s[start:i]); piece != "" {
					flush(strings.TrimRight(piece, " \t,"))
				}
				return ret()
			}
		case depth == 0 && c == ',':
			flush(s[start:i])
			start = i + 1
		}
	}
	if piece := strings.TrimSpace(s[start:]); piece != "" {
		flush(strings.TrimRight(piece, " \t;"))
	}
	return ret()
}

// parseOrderBy extracts the first ORDER BY's text at any depth (the inner
// select owns the ordering of a ROWNUM-wrapped query); the span ends at the
// relative-depth-zero ')' or the next clause keyword.
func parseOrderBy(norm string) string {
	idx := orderAtIndex(norm)
	if idx < 0 {
		return ""
	}
	rest := norm[idx+len("ORDER BY"):]
	end := len(rest)
	depth := 0
done:
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				end = i
				break done
			}
			depth--
		default:
			if depth == 0 && isWordBoundaryBefore(rest, i) {
				for _, kw := range []string{"WHERE", "ORDER", "GROUP", "HAVING", "UNION", "MINUS", "INTERSECT", "CONNECT", "FOR", ";"} {
					if i+len(kw) <= len(rest) && equalFold(rest[i:i+len(kw)], kw) && isWordBoundaryAfter(rest, i+len(kw)) {
						end = i
						break done
					}
				}
			}
		}
	}
	return strings.TrimSpace(rest[:end])
}

// orderAtIndex finds the first ORDER BY at any nesting depth.
func orderAtIndex(s string) int {
	for i := 0; i+len("ORDER BY") <= len(s); i++ {
		if equalFold(s[i:i+len("ORDER BY")], "ORDER BY") &&
			isWordBoundaryBefore(s, i) && isWordBoundaryAfter(s, i+len("ORDER BY")) {
			return i
		}
	}
	return -1
}

// topKeywordIndex finds a keyword at top nesting level with word boundaries.
func topKeywordIndex(s, kw string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i+len(kw) <= len(s) && equalFold(s[i:i+len(kw)], kw) &&
				isWordBoundaryBefore(s, i) && isWordBoundaryAfter(s, i+len(kw)) {
				return i
			}
		}
	}
	return -1
}

func wordStarts(s string, i int) bool {
	if i > 0 && isIdentByteFor(s[i-1]) {
		return false
	}
	return i < len(s) && isIdentByteFor(s[i])
}

func wordAt(s string, i int) string {
	j := i
	for j < len(s) && isIdentByteFor(s[j]) {
		j++
	}
	return s[i:j]
}

func isWordBoundaryBefore(s string, i int) bool {
	return i == 0 || !isIdentByteFor(s[i-1])
}

func isWordBoundaryAfter(s string, i int) bool {
	return i >= len(s) || !isIdentByteFor(s[i])
}

// clauseKeyword terminates a table list.
func clauseKeyword(w string) bool {
	switch strings.ToUpper(w) {
	case "WHERE", "ON", "ORDER", "GROUP", "HAVING", "CONNECT", "START", "UNION",
		"MINUS", "INTERSECT", "SET", "VALUES", "SELECT", "WHEN", "USING", "FOR",
		"NOT", "MATCHED", "RETURNING", "INTO", "FROM":
		return true
	}
	return false
}
