// Computed-column alias helpers: the deterministic select-list analysis the
// Go emitter uses to give every computed select item an Oracle-visible name.
//
// Oracle returns the expression text as the column name for unaliased
// computed items (NVL(...), TO_CHAR(...), DECODE(...), literals, ...), so a
// row struct scanning by the INTO host-var name (STR_DESC for a DECODE)
// fails at runtime. The fix is a sanctioned `AS <alias>` per computed item,
// wired to the same name in the row struct's db tag. sqlchk tolerates the
// aliases by design (stripSelectAlias on both sides), so fidelity holds.
package sqltext

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// SelectItems splits the top-level (paren-depth-0, literal-aware) comma list
// of the SELECT clause, using the same boundary discipline as
// ir/query.go:topKeywordIndex and sqlchk/compare.go:selectItems. Each entry
// is trimmed; empty entries are dropped. nil when the text carries no
// SELECT list (no SELECT keyword or an empty list).
func SelectItems(sql string) []string {
	sel, selEnd, ok := findTopKeyword(sql, "SELECT", 0)
	if !ok {
		return nil
	}
	_ = sel
	from, _, ok := findTopKeyword(sql, "FROM", selEnd)
	end := len(sql)
	if ok {
		end = from
	} else {
		// No FROM (SELECT without a table): stop at the next top-level
		// clause keyword so ORDER/GROUP/UNION tails never join the list.
		if cut := topClauseEnd(sql, selEnd); cut >= 0 {
			end = cut
		}
	}
	list := sql[selEnd:end]
	var out []string
	depth := 0
	start := 0
	flush := func(piece string) {
		if t := strings.TrimSpace(piece); t != "" {
			out = append(out, t)
		}
	}
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '\'':
			i = skipLitIn(list, i)
		case '"':
			i = skipDqIn(list, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush(list[start:i])
				start = i + 1
			}
		}
	}
	flush(list[start:])
	return out
}

// IsComputedItem reports whether a select-list item needs a sanctioned
// alias: true when the item is not a single identifier (bare `col` or
// qualified `t.col`). Functions (NVL, TO_CHAR, DECODE), operators, CASE,
// literals and subqueries are computed; `*` and `t.*` never are. Items that
// already carry a trailing alias (`expr AS x`, `expr x`) return false —
// aliasing is idempotent and the insertion pass skips them.
func IsComputedItem(item string) bool {
	t := strings.TrimSpace(item)
	if t == "" {
		return false
	}
	// A leading DISTINCT/ALL/UNIQUE belongs to the SELECT, not the item.
	t = stripDistinctPrefix(t)
	if hasTrailingAlias(t) {
		return false
	}
	if t == "*" {
		return false
	}
	if strings.HasSuffix(t, ".*") {
		if isQualifiedIdent(strings.TrimSpace(t[:len(t)-2])) {
			return false
		}
	}
	if isQuotedIdent(t) {
		return false
	}
	if isBareIdentList(t) {
		return false
	}
	return true
}

// AliasFor renders the deterministic alias for a computed column:
// TUXC_<SERVICE>_<UNIT>_<N> with N the 1-based select-list ordinal,
// sanitized (non-identifier bytes → `_`, so fn-namespaced ids like
// `fn_gene_otp:q2` stay safe) and capped at 30 bytes (the Oracle identifier
// limit) with a deterministic hash suffix when truncating. Pure function,
// no registry — worker-pool safe. Uniqueness across a staged run falls out
// of (service, canonical unit, ordinal): dedup resolves duplicates to one
// canonical unit, so both arms share one alias set.
func AliasFor(service, canonicalUnitID string, ordinal int) string {
	full := "TUXC_" + sanitizeIdent(service) + "_" + sanitizeIdent(canonicalUnitID) + "_" + strconv.Itoa(ordinal)
	if len(full) <= 30 {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(full))
	suffix := fmt.Sprintf("_%08X", h.Sum32())
	keep := 30 - len(suffix)
	if keep <= 0 {
		return suffix[len(suffix)-30:]
	}
	return full[:keep] + suffix
}

// InjectAliases inserts ` AS <alias>` for every position in aliases
// (0-based select-list ordinal → alias name) into sql's SELECT list. The
// insertion lands before the item's depth-0 comma/FROM so the emitted query
// stays executable. Items that already carry a trailing alias are skipped,
// making the pass idempotent. Positions outside the select list are
// ignored. sql is otherwise untouched (spacing, casing, line breaks kept).
func InjectAliases(sql string, aliases map[int]string) string {
	if len(aliases) == 0 {
		return sql
	}
	sel, selEnd, ok := findTopKeyword(sql, "SELECT", 0)
	if !ok {
		return sql
	}
	_ = sel
	from, _, ok := findTopKeyword(sql, "FROM", selEnd)
	listEnd := len(sql)
	if ok {
		listEnd = from
	} else if cut := topClauseEnd(sql, selEnd); cut >= 0 {
		listEnd = cut
	}
	type insert struct {
		pos   int
		alias string
	}
	var inserts []insert
	depth := 0
	itemIdx := 0
	itemStart := selEnd
	consider := func(itemEnd int) {
		if itemIdx < 0 {
			return
		}
		alias, want := aliases[itemIdx]
		if !want || alias == "" {
			return
		}
		raw := strings.TrimSpace(sql[itemStart:itemEnd])
		if raw == "" || hasTrailingAlias(stripDistinctPrefix(raw)) {
			return
		}
		// Insert after the item's last non-space byte so trailing gaps
		// before the comma/FROM survive (`x   FROM` → `x AS A   FROM`).
		trueEnd := itemEnd
		for trueEnd > itemStart {
			c := sql[trueEnd-1]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				trueEnd--
				continue
			}
			break
		}
		inserts = append(inserts, insert{pos: trueEnd, alias: alias})
	}
	for i := selEnd; i < listEnd; i++ {
		switch sql[i] {
		case '\'':
			i = skipLitIn(sql, i)
		case '"':
			i = skipDqIn(sql, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				consider(i)
				itemIdx++
				itemStart = i + 1
			}
		}
	}
	consider(listEnd)
	if len(inserts) == 0 {
		return sql
	}
	// Right-to-left keeps earlier offsets valid.
	for i := len(inserts) - 1; i >= 0; i-- {
		ins := inserts[i]
		sql = sql[:ins.pos] + " AS " + ins.alias + sql[ins.pos:]
	}
	return sql
}

// sanitizeIdent uppercases s and maps every non [A-Z0-9_] byte to `_`.
// Empty input becomes "X" so the alias never carries an empty segment.
func sanitizeIdent(s string) string {
	up := strings.ToUpper(s)
	var sb strings.Builder
	for i := 0; i < len(up); i++ {
		c := up[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('_')
	}
	if sb.Len() == 0 {
		return "X"
	}
	return sb.String()
}

func stripDistinctPrefix(t string) string {
	up := strings.ToUpper(t)
	for _, kw := range []string{"DISTINCT", "ALL", "UNIQUE"} {
		if strings.HasPrefix(up, kw) {
			rest := t[len(kw):]
			if rest != "" && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
				return strings.TrimSpace(rest)
			}
		}
	}
	return t
}

// hasTrailingAlias reports whether item ends with an explicit column alias:
// `expr AS x` or bare `expr x` at depth 0 outside literals/parens. A single
// bare identifier is the column itself, never an alias.
func hasTrailingAlias(item string) bool {
	t := strings.TrimSpace(item)
	if t == "" {
		return false
	}
	// `AS <alias>` at depth 0: scan for the last top-level AS whose suffix
	// is a lone identifier.
	depth := 0
	lastAS := -1
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '\'':
			i = skipLitIn(t, i)
		case '"':
			i = skipDqIn(t, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && (i == 0 || !isIdentByte(t[i-1])) {
				if i+2 <= len(t) && strings.EqualFold(t[i:min(i+2, len(t))], "AS") &&
					(i+2 >= len(t) || !isIdentByte(t[i+2])) {
					lastAS = i
				}
			}
		}
	}
	if lastAS >= 0 {
		suffix := strings.TrimSpace(t[lastAS+2:])
		prefix := strings.TrimSpace(t[:lastAS])
		if prefix != "" && isAliasName(suffix) {
			return true
		}
	}
	// Bare `expr alias`: the last top-level space-separated word is a lone
	// identifier following an expression end — never a lone word (the
	// column itself) and never a clause keyword.
	depth = 0
	lastSpace := -1
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '\'':
			i = skipLitIn(t, i)
		case '"':
			i = skipDqIn(t, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ' ', '\t', '\n', '\r':
			if depth == 0 {
				lastSpace = i
			}
		}
	}
	if lastSpace < 0 {
		return false
	}
	last := strings.TrimSpace(t[lastSpace+1:])
	prev := strings.TrimSpace(t[:lastSpace])
	if prev == "" || !isAliasName(last) || isClauseWord(last) {
		return false
	}
	// The word before the alias must end an expression (identifier, number,
	// quoted ident or closing paren) — an operator or dot means the last
	// word is still part of the expression (`a || b`, `t.col`).
	pe := lastNonSpaceByte(prev)
	return pe == ')' || pe == '"' || isIdentByte(pe) || (pe >= '0' && pe <= '9')
}

// isAliasName reports whether s is a lone alias identifier (bare or
// double-quoted).
func isAliasName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\n\r(),;") {
		return false
	}
	if isQuotedIdent(s) {
		return true
	}
	if !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentByte(s[i]) && s[i] != '$' && s[i] != '#' {
			return false
		}
	}
	return !isClauseWord(s)
}

func isClauseWord(s string) bool {
	switch strings.ToUpper(s) {
	case "FROM", "WHERE", "ORDER", "GROUP", "HAVING", "UNION", "MINUS",
		"INTERSECT", "CONNECT", "START", "FOR", "INTO", "SELECT", "AS",
		"AND", "OR", "NOT", "CASE", "WHEN", "THEN", "ELSE", "END",
		"BY", "ASC", "DESC", "NULL", "IS", "IN", "BETWEEN", "LIKE",
		"DISTINCT", "ALL", "UNIQUE":
		return true
	}
	return false
}

// isBareIdentList reports whether t is one dotted identifier path
// (`col`, `t.col`, `s.t.col`) with no operators, spaces or parens.
func isBareIdentList(t string) bool {
	if t == "" {
		return false
	}
	parts := strings.Split(t, ".")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || !isIdentStart(p[0]) {
			return false
		}
		for i := 1; i < len(p); i++ {
			if !isIdentByte(p[i]) && p[i] != '$' && p[i] != '#' {
				return false
			}
		}
	}
	return true
}

func isQualifiedIdent(t string) bool { return isBareIdentList(t) }

func isQuotedIdent(t string) bool {
	t = strings.TrimSpace(t)
	return len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"'
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func lastNonSpaceByte(s string) byte {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			return s[i]
		}
	}
	return 0
}

// findTopKeyword finds kw at paren depth 0 outside string literals,
// case-insensitively with word boundaries, at or after from. It returns the
// keyword's start, the end (start+len) and true.
func findTopKeyword(sql, kw string, from int) (int, int, bool) {
	depth := 0
	for i := 0; i < len(sql); i++ {
		switch sql[i] {
		case '\'':
			i = skipLitIn(sql, i)
			continue
		case '"':
			i = skipDqIn(sql, i)
			continue
		case '(':
			depth++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 || i < from {
			continue
		}
		if i+len(kw) > len(sql) || !strings.EqualFold(sql[i:i+len(kw)], kw) {
			continue
		}
		before := i == 0 || !isIdentByte(sql[i-1])
		after := i+len(kw) >= len(sql) || !isIdentByte(sql[i+len(kw)])
		if before && after {
			return i, i + len(kw), true
		}
	}
	return 0, 0, false
}

// topClauseEnd finds the next top-level clause keyword ending a FROM-less
// SELECT list (ORDER/GROUP/HAVING/UNION/.../FOR), or -1.
func topClauseEnd(sql string, from int) int {
	best := -1
	for _, kw := range []string{"ORDER", "GROUP", "HAVING", "UNION", "MINUS", "INTERSECT", "CONNECT", "FOR", "WHERE", "LIMIT", "OFFSET", "FETCH"} {
		if s, _, ok := findTopKeyword(sql, kw, from); ok {
			if best < 0 || s < best {
				best = s
			}
		}
	}
	return best
}

func skipLitIn(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\'' {
			if j+1 < len(s) && s[j+1] == '\'' {
				j++
				continue
			}
			return j
		}
	}
	return len(s) - 1
}

func skipDqIn(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == '"' {
			if j+1 < len(s) && s[j+1] == '"' {
				j++
				continue
			}
			return j
		}
	}
	return len(s) - 1
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
