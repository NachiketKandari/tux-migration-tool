// Package sqltext carries the Pro*C SQL text transforms shared by every
// emitter (Go, Python, C#): the SELECT host-variable INTO list is stripped
// (the IR's RowShape carries the targets; the emitted query must be
// executable SQL), `: name` bind spellings collapse to `:name` (Pro*C
// tolerates the space, oracledb named binds do not) and the statement is
// pretty-printed by Format. CanonicalSQL is the one home for the
// executable-SQL derivation — backends must call it instead of
// reimplementing INTO-stripping locally.
package sqltext

import "strings"

// StripInto removes the `INTO :a, :b …` span from a SELECT's SQL text.
// The host-var INTO list is a Pro*C construct, not SQL (BP-8, the same
// tolerance the fidelity gate applies): the driver scans columns into the
// row struct by name, and the IR's RowShape already carries the INTO
// targets, so the emitted query string must not contain the clause. The
// leading-`:` guard keeps `INSERT INTO t` untouched; the FROM scan is
// paren-aware and literal-aware.
func StripInto(sql string) string {
	lower := strings.ToLower(sql)
	for i := 0; i+4 <= len(lower); i++ {
		if lower[i:i+4] != "into" || !wordBoundary(sql, i, 4) {
			continue
		}
		j := i + 4
		for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t' || sql[j] == '\n' || sql[j] == '\r') {
			j++
		}
		if j >= len(sql) || sql[j] != ':' {
			continue // INSERT INTO t — not a host-var list
		}
		depth := 0
		for k := j; k < len(sql); k++ {
			switch sql[k] {
			case '\'':
				k = skipLit(sql, k)
			case '(':
				depth++
			case ')':
				depth--
			case 'f', 'F':
				if depth == 0 && k+4 < len(sql) && strings.EqualFold(sql[k:k+4], "from") && wordBoundary(sql, k, 4) {
					return strings.TrimSpace(sql[:i] + sql[k:])
				}
			}
		}
		return strings.TrimSpace(sql[:i])
	}
	return sql
}

// CollapseBinds rewrites `: name` → `:name` (Pro*C tolerates the space;
// oracledb named binds do not). String literals are left untouched.
func CollapseBinds(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == ':' && i+1 < len(sql) {
			j := i + 1
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
				j++
			}
			if j > i+1 && j < len(sql) && isIdentByte(sql[j]) {
				b.WriteByte(':')
				i = j - 1
				continue
			}
		}
		if c == '\'' {
			k := skipLit(sql, i)
			b.WriteString(sql[i : k+1])
			i = k
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// CanonicalSQL renders executable SQL for code generation: SELECT
// host-variable INTO lists are stripped, `: name` collapses to `:name`,
// surrounding whitespace is trimmed and one trailing statement semicolon is
// removed (the host language syntax carries its own terminator). The result
// is then pretty-printed by Format, the shared indent layout every backend
// embeds. It is idempotent and safe for every statement kind: non-SELECT
// INTO clauses (INSERT INTO t) are untouched by StripInto, so DML passes
// through except for bind collapsing, semicolon trimming and formatting.
func CanonicalSQL(sql string) string {
	s := CollapseBinds(StripInto(sql))
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	return Format(strings.TrimSpace(s))
}

// ExecutableBinds returns the unique bind names of executable SQL in textual
// order of appearance (`: name` with whitespace counts — Pro*C tolerates the
// space). Positional `:1`/`:2` binds keep no name. String literals are
// ignored. It must be called on CanonicalSQL output (or it applies the same
// derivation internally by scanning the given text literally).
func ExecutableBinds(sql string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(sql); i++ {
		if sql[i] != ':' {
			if sql[i] == '\'' {
				i = skipLit(sql, i)
			}
			continue
		}
		j := i + 1
		for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
			j++
		}
		nameStart := j
		for j < len(sql) && isIdentByte(sql[j]) {
			j++
		}
		if j == nameStart {
			continue // positional :1/:2 binds keep no name
		}
		name := sql[nameStart:j]
		// Positional :1/:2 binds keep no name (leading digit = positional).
		if name[0] >= '0' && name[0] <= '9' {
			i = j - 1
			continue
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}

func wordBoundary(sql string, i, n int) bool {
	before := i == 0 || !isIdentByte(sql[i-1])
	after := i+n >= len(sql) || !isIdentByte(sql[i+n])
	return before && after
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func skipLit(sql string, i int) int {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] == '\'' {
			if j+1 < len(sql) && sql[j+1] == '\'' {
				j++
				continue
			}
			return j
		}
	}
	return len(sql) - 1
}
