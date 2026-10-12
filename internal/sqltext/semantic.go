package sqltext

import "strings"

// SemanticKey folds a statement to the comparison key two query units must
// share to count as *the same query*. It removes exactly the differences that
// cannot change which rows a statement returns:
//
//   - comments (both /* */ and the -- line form), literal-aware so a `--`
//     inside a string literal survives;
//   - the Pro*C `INTO :a, :b` host-variable list, which is an output target,
//     not a predicate (StripInto);
//   - the `: name` bind spelling Pro*C tolerates and Oracle does not
//     (CollapseBinds);
//   - unquoted identifier case — Oracle folds it, so `nvl(a_col,' ')` and
//     `NVL(A_COL,' ')` are one column;
//   - insignificant whitespace, including the stray space before a comma that
//     separates two byte-identical statements in practice;
//   - one trailing statement terminator.
//
// What survives is clause structure, string literals, column names and bind
// names: everything that decides the result set. Two units with equal keys are
// the same statement as far as a DB method is concerned; unequal keys still
// need not mean *different* queries (see flow.BuildQueryReuse, which reports
// the varying predicates rather than only the verdict).
//
// It is idempotent — SemanticKey(SemanticKey(s)) == SemanticKey(s) — so it can
// be applied to already-normalized text without compounding.
func SemanticKey(sql string) string {
	s := stripSQLComments(sql)
	s = StripInto(s)
	s = CollapseBinds(s)
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	s = strings.ToUpper(s)
	return tighten(s)
}

// stripSQLComments blanks /* */ and `-- …` comment runs to a single space,
// skipping over string literals so a literal containing comment openers is
// left intact.
func stripSQLComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); {
		switch {
		case sql[i] == '\'':
			k := skipLit(sql, i)
			b.WriteString(sql[i : k+1])
			i = k + 1
		case sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < len(sql) {
				i += 2
			} else {
				i = len(sql)
			}
			b.WriteByte(' ')
		case sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(sql[i])
			i++
		}
	}
	return b.String()
}

// tighten collapses whitespace runs to one space and drops the spaces that
// pad punctuation, so `NVL( a , b )` and `NVL(a,b)` fold together. A space is
// kept whenever it separates two tokens that would otherwise merge (never
// between a closing paren and a following word — `)AND` would still read back
// the same, but `) AND` costs nothing and reads better in a diff). Literal
// bodies pass through untouched: whitespace inside `'a  b'` is data.
func tighten(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false // a whitespace run is buffered, not yet written
	last := byte(0)  // last byte written to b, 0 at the start
	write := func(c byte) {
		b.WriteByte(c)
		last = c
		pending = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			pending = true
			continue
		}
		if c == '\'' {
			if pending && !isPadByte(last) {
				b.WriteByte(' ')
			}
			pending = false
			k := skipLit(s, i)
			b.WriteString(s[i : k+1])
			last = ')'
			i = k
			continue
		}
		// Whitespace before these carries no meaning; writing directly
		// discards whatever run was buffered.
		if isPadByte(c) {
			write(c)
			continue
		}
		if pending && !isPadByte(last) {
			b.WriteByte(' ')
		}
		write(c)
	}
	return b.String()
}

// isPadByte reports whether whitespace around this byte is pure padding in
// SQL: brackets, the comma, and the comparison operators. Arithmetic is left
// out on purpose — `-` next to `-` could read as a line-comment opener, and
// spacing inside an arithmetic expression never separates two identifiers.
func isPadByte(c byte) bool {
	switch c {
	case '(', ')', ',', '=', '<', '>', '!':
		return true
	}
	return false
}
