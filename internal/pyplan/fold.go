package pyplan

import (
	"strconv"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/ir"
)

// Repository consolidation — Phase 1 of docs/pybatchpy-logic-plan.md.
//
// The pre-Phase-1 plan emitted one SQL constant and one repository method per
// IR query unit, which on dtcmpt.pc meant 16 constants and 16 methods for 16
// distinct statements: eight named `fetch_dual*` (named after `FROM DUAL`,
// which is not a table), four near-identical `fetch_hlm_hldy_mstr*` probes,
// and two byte-different spellings of one `INSERT`. This file is the fix and
// the two rules it needs:
//
//  1. One entry per *distinct statement*. The identity is a token stream, not
//     a byte string (foldKey), so it folds exactly the differences the Pro*C
//     corpus carries between semantically identical statements —
//     inter-token whitespace — and nothing else.
//  2. `FROM DUAL` date arithmetic is not a repository concern. A statement
//     whose only table is Oracle's dual pseudo-table, that binds at least one
//     host variable and calls nothing but recognised Oracle builtins is a
//     pure function of values the caller already holds; it becomes a
//     DateMath entry (dateMathStmt) instead of a repository method
//     (isDateMath).
//
// Both decisions are recorded, not silent. A folded site lands in
// RepoMethod.MergedSrcLines with the source lines it shares an identity
// with, and every elision carries the reason it was safe. That is the
// disclosure §6.4 asks for and the "log every elision" guard of §8 asks for;
// Phase 1 only produces the data, Phase 3 prints it into the module.

// foldSep separates the fold key's parts. A single NUL would be ambiguous
// only for a statement whose text could contain NUL, which SQL source cannot;
// it is escaped inside literals anyway (escapeFold).
const foldSep = "\x00"

// foldKey is the consolidation identity of one repository entry.
//
// The plan names the key `(CanonicalSQL(q.SQL), sorted ExecutableBinds)`
// (§4). That pair reduces to the token stream below — `sqltext.ExecutableBinds`
// is itself derived from the canonical text and every bind appears in it as a
// `:name` token — but the two non-SQL facts a method's *contract* carries are
// not in the SQL and must not be crossed by a fold:
//
//   - rowShape: the positional tuple the caller unpacks. Two statements with
//     identical text but different INTO targets are the same query returning
//     differently-shaped rows.
//   - kind: a SELECT_SINGLE and a SELECT_MULTI over identical text are
//     different methods — one returns a row, one iterates.
//   - truncate: an INSERT with a paired TRUNCATE half is a `rebuild` method
//     carrying two constants, not a single-statement insert.
//
// The key is deliberately *stronger* than "equivalent SQL": two statements
// share it only when their token sequences are identical. The live `-1`
// holiday probe (dtcmpt.pc:790, case 'B') and the plain probe carry different
// tokens and therefore keep different methods — which is exactly the §6.2
// defect-1 class a loose key would happily collapse.
func foldKey(sql, rowShape, kind string, truncate bool) string {
	var b strings.Builder
	toks := foldTokens(sql)
	for i, t := range toks {
		if i > 0 {
			b.WriteString(foldSep)
		}
		b.WriteString(t)
	}
	b.WriteString(foldSep + foldSep)
	b.WriteString("row=" + rowShape)
	b.WriteString(foldSep + "kind=" + kind)
	b.WriteString(foldSep + "trunc=" + boolWord(truncate))
	return b.String()
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// rowShapeOf renders a query's INTO-target list as a comparable string.
func rowShapeOf(q *ir.Query) string {
	return strings.ToLower(strings.Join(q.RowShape, ","))
}

// foldTokens splits a statement into its significant tokens: comments
// removed, string literals and quoted identifiers kept verbatim, bare
// identifiers upper-cased, binds case-sensitive, punctuation one byte each.
// Inter-token whitespace produces no token, so two statements tokenise
// identically exactly when they differ only in whitespace.
//
// Upper-casing bare identifiers is sound because an unquoted SQL identifier
// is case-insensitive; string literals and `"quoted"` identifiers keep their
// bytes, so `'PX'` and `'MI'` never merge. Bind names stay case-sensitive
// because a Pro*C host variable is a C identifier and the emitted Python
// takes it as a keyword argument name.
func foldTokens(sql string) []string {
	var out []string
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
			continue
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(sql)
			}
			continue
		case c == '\'' || c == '"':
			j := literalEnd(sql, i)
			out = append(out, escapeFold(sql[i:j]))
			i = j
			continue
		case isIdentByte(c):
			j := i
			for j < len(sql) && isIdentByte(sql[j]) {
				j++
			}
			out = append(out, strings.ToUpper(sql[i:j]))
			i = j
			continue
		case c == ':':
			// A bind, tolerating Pro*C's `: name` spacing. The canonicalizer
			// has already collapsed that space by the time the plan sees
			// production SQL, but the key must be correct on its own input:
			// otherwise `: name` splits into a `:` token and a bare word, and
			// the word gets upper-cased — giving a host variable a different
			// identity from its own `:name` spelling. A `:` that names
			// nothing stays a lone punctuation token.
			j := i + 1
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
				j++
			}
			bindStart := j
			if j < len(sql) && isIdentByte(sql[j]) {
				for j < len(sql) && isIdentByte(sql[j]) {
					j++
				}
				// Emit the collapsed `:name` spelling (the rule
				// sqltext.CollapseBinds applies), so the internal space
				// cannot reach the key.
				out = append(out, escapeFold(":"+sql[bindStart:j]))
				i = j
				continue
			}
			out = append(out, string(c))
			i++
			continue
		}
		out = append(out, string(c))
		i++
	}
	return out
}

// literalEnd returns the index just past the quoted literal starting at i,
// honouring both the C backslash escape Pro*C inherits and the SQL doubled
// quote. An unterminated literal runs to end of input.
func literalEnd(sql string, i int) int {
	q := sql[i]
	for j := i + 1; j < len(sql); j++ {
		switch {
		case sql[j] == '\\' && j+1 < len(sql):
			j++
		case sql[j] == q:
			if j+1 < len(sql) && sql[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(sql)
}

// escapeFold makes a literal token safe to join on NUL: backslash and NUL are
// the two characters the key could otherwise not round-trip.
func escapeFold(tok string) string {
	if !strings.ContainsAny(tok, "\\\x00") {
		return tok
	}
	var b strings.Builder
	for i := 0; i < len(tok); i++ {
		switch tok[i] {
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(tok[i])
		}
	}
	return b.String()
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ---------------------------------------------------------------------------
// FROM DUAL classification
// ---------------------------------------------------------------------------

// dualOnly reports whether a query's only table is Oracle's dual
// pseudo-table. DUAL has no columns and no rows of its own, so a statement
// over it cannot read or write user data — the first half of the §8
// "provably side-effect-free" guard.
func dualOnly(q *ir.Query) bool {
	return len(q.Tables) == 1 && strings.EqualFold(q.Tables[0], "DUAL")
}

// oracleBuiltins is the call allowlist a `FROM DUAL` statement must stay
// inside for the elision to be provably side-effect-free.
//
// It is an allowlist rather than a denylist on purpose: an unrecognised name
// means the statement is NOT elided, so it stays a repository method. The
// failure direction is wrong-but-working, never wrong-and-gone.
var oracleBuiltins = map[string]bool{
	"ABS": true, "ADD_MONTHS": true, "CEIL": true, "COALESCE": true,
	"COUNT": true, "DECODE": true, "EXTRACT": true, "FLOOR": true,
	"GREATEST": true, "INSTR": true, "LAST_DAY": true, "LEAST": true,
	"LENGTH": true, "LOWER": true, "MONTHS_BETWEEN": true,
	"NEXT_DAY": true, "NVL": true, "NULLIF": true, "POWER": true,
	"ROUND": true, "SIGN": true, "SQRT": true, "SUBSTR": true,
	"TO_CHAR": true, "TO_DATE": true, "TO_NUMBER": true, "TRUNC": true,
	"UPPER": true,
}

// dualReserved are the identifier tokens a `FROM DUAL` statement may carry
// that are not calls: Oracle's clause words and the naked date values. DUAL
// exposes no columns, so a bare identifier in a dual projection is one of
// these and nothing else — which is what lets the check be an enumeration
// rather than a heuristic.
var dualReserved = map[string]bool{
	"ALL": true, "AND": true, "AS": true, "ASC": true, "BETWEEN": true,
	"BY": true, "CASE": true, "CURRENT_DATE": true, "CURRENT_TIMESTAMP": true,
	"DESC": true, "DISTINCT": true, "DUAL": true, "ELSE": true, "END": true,
	"FROM": true, "IN": true, "INTERVAL": true, "IS": true, "LIKE": true,
	"LOCALTIMESTAMP": true, "NOT": true, "NULL": true, "OR": true,
	"SELECT": true, "SYSDATE": true, "SYSTIMESTAMP": true, "THEN": true,
	"TO": true, "WHEN": true, "WHERE": true,
}

// isDateMath reports whether a statement is database-side date arithmetic
// that Python's `datetime`/`calendar` replaces exactly, and therefore
// belongs in the DateMath bucket rather than the repository method list.
//
// The test has three conjuncts, all mechanical:
//
//	FROM DUAL and nothing else   → no table, no columns, nothing to read
//	binds at least one host var  → a pure function of caller-held values
//	only builtin calls           → no user function with hidden side effects
//
// **The bind conjunct is load-bearing, and it is why `SELECT
// TO_NUMBER(TO_CHAR(SYSDATE,'YYYY'))` (:196) stays a repository method.** A
// bind-free dual query reads the *database clock*. That is real state owned
// by the database — its timezone, its notion of "now" — and swapping it for
// the Python host clock would silently change the run's epoch. It is not
// date arithmetic; it is a dependency, and a dependency is what a repository
// method is for. Every statement that IS arithmetic is a transformation of a
// date the caller already holds, and those are the ones that move.
func isDateMath(q *ir.Query, sql string, binds []string) bool {
	if !dualOnly(q) {
		return false
	}
	if len(binds) == 0 {
		return false // reads the database clock — a repository dependency
	}
	return onlyBuiltinCalls(sql)
}

// onlyBuiltinCalls reports whether every call in a statement names a
// recognised Oracle builtin and every bare identifier is a reserved word or a
// naked date value.
func onlyBuiltinCalls(sql string) bool {
	toks := foldTokens(sql)
	for i, t := range toks {
		if !bareWord(t) {
			continue
		}
		upper := strings.ToUpper(t)
		// A word immediately followed by `(` is a call.
		if i+1 < len(toks) && toks[i+1] == "(" {
			if !oracleBuiltins[upper] {
				return false
			}
			continue
		}
		if !dualReserved[upper] {
			return false
		}
	}
	return true
}

// bareWord reports whether a fold token is a bare SQL identifier, which is
// what the builtin walk has to classify. Everything else is a value, a
// literal or punctuation: Oracle numeric literals share the identifier byte
// class with host variables (`COUNT(1)`, `+7*(…),-2`), so a digit-leading
// token is a number rather than a name; `:b` starts with a bind marker, and
// `'x'` / `"x"` are quoted. None of them can name a function.
func bareWord(tok string) bool {
	if tok == "" {
		return false
	}
	if c := tok[0]; c >= '0' && c <= '9' {
		return false
	}
	return isIdentByte(tok[0])
}

// ---------------------------------------------------------------------------
// Recognising the elided idiom (Phase 3 promotes Op into a completion counter)
// ---------------------------------------------------------------------------

// dateMathOp names the idiom family a date-math statement belongs to (§5's
// `FROM DUAL` row). It is the completion counter Phase 3 promotes: a run that
// recognised 7 of 7 date-math statements is a different claim from one that
// recognised 5.
func dateMathOp(sql string) string {
	upper := strings.ToUpper(sql)
	switch {
	case strings.Contains(upper, "SIGN(") && strings.Contains(upper, "DECODE("):
		return "sign_decode"
	case strings.Contains(upper, "NEXT_DAY("):
		return "next_day"
	case strings.Contains(upper, "LAST_DAY("):
		return "last_day"
	case dayOffset(upper):
		return "offset"
	default:
		return "scalar"
	}
}

// dayOffset reports a bare ±N day shift (`…+1`, `…-1`) applied to a date.
func dayOffset(upper string) bool {
	for _, op := range []string{"+", "-"} {
		if !strings.Contains(upper, ")"+op) {
			continue
		}
		for _, n := range []string{"0", "1", "2", "3", "4", "5", "6", "7"} {
			if strings.Contains(upper, ")"+op+n) {
				return true
			}
		}
	}
	return false
}

// dateMathReason states, per statement, why the elision was safe. §6.4 asks
// for the deviation to be *declared* with a rationale rather than silent, and
// the rationale is the whole content of the classification: the statement
// reads no table, takes every input from a bind the caller already holds, and
// calls only Oracle builtins.
func dateMathReason(binds []string) string {
	return "FROM DUAL only; " + itoa(len(binds)) + " bound input(s) (" +
		strings.Join(binds, ", ") + "); no table read, no DML, no unrecognised call — " +
		"evaluated by datetime/calendar in the service"
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// Naming after the value a statement returns
// ---------------------------------------------------------------------------

// hostVarPrefixes are the Pro*C host-variable type prefixes this corpus uses.
// Stripping them makes a derived name describe the value rather than the
// C variable (`l_currnt_year` → `currnt_year`).
var hostVarPrefixes = []string{"sql_", "tmp_", "rev_", "l_", "c_", "n_", "d_", "v_", "i_", "s_"}

// dualScope names a `FROM DUAL` statement after the host variable its scalar
// lands in, instead of after `FROM DUAL` (which is not a table and made eight
// methods on dtcmpt.pc read `fetch_dual_1` … `fetch_dual_8`).
//
// The reference port calls the :196 statement `get_current_year()`. That name
// is a reading of what `TO_NUMBER(TO_CHAR(SYSDATE,'YYYY'))` *means*, and
// mechanical naming cannot recover meaning from a projection — so this
// returns the honest mechanical name instead.
func dualScope(q *ir.Query) string {
	for _, target := range q.RowShape {
		if v := stripHostVarPrefix(target); v != "" {
			return v
		}
	}
	return "scalar"
}

func stripHostVarPrefix(name string) string {
	low := strings.ToLower(name)
	for _, p := range hostVarPrefixes {
		if strings.HasPrefix(low, p) && len(low) > len(p) {
			return low[len(p):]
		}
	}
	return low
}

// scopeOf returns the naming scope for a statement's constant and method:
// its first table, or — when that table is `FROM DUAL` — the value it
// returns.
func scopeOf(q *ir.Query) string {
	if dualOnly(q) {
		return common.PyIdent(dualScope(q))
	}
	return firstTable(q)
}
