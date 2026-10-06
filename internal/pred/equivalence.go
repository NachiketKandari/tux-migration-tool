package pred

import (
	"strings"

	"tux-migration-tool/internal/common"
)

// Equivalent reports whether two parsed predicates are the same condition
// modulo identifier spelling (identEq), commutative && / || reordering, and
// literal normalization. It is deliberately strict otherwise: `a || b` never
// matches `a`, a negated comparison never matches its positive form, and a
// sub-term inside a comparison is never a guard. This is the guard-retention
// check's core — a model that drops, widens, inverts, or mutates a runtime
// dispatch guard must not pass.
//
// Literals compare by content: C char and string literals are equal when
// their contents match (`'F'` == `"F"`), and NULL/nil are one literal.
// Call arguments compare token-wise with the same identEq (renamed args
// still match).
func Equivalent(a, b *Expr, identEq func(a, b string) bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Foccur32 presence equivalence (logical part): a legacy existence
	// check (`Foccur32(buf, FML_X) > 0` / `== 0`, any buffer spelling)
	// equals the Go presence form (`X != ""` / `== ""`) on the same
	// field with the same polarity. Buffers never participate — the
	// request buffer vs local buffer spelling is not a semantic change.
	if af, ap, ok := PresenceOf(a); ok {
		if bf, bp, ok := PresenceOf(b); ok {
			return ap == bp && presenceFieldsEqual(af, bf, identEq)
		}
		if gi, gp, ok := GoPresenceOf(b); ok {
			return ap == gp && presenceFieldsEqual(af, gi, identEq)
		}
	}
	if ag, ap, ok := GoPresenceOf(a); ok {
		if bf, bp, ok := PresenceOf(b); ok {
			return ap == bp && presenceFieldsEqual(ag, bf, identEq)
		}
	}
	switch a.Kind {
	case KindOr, KindAnd:
		if a.Kind != b.Kind {
			return false
		}
		return itemsEquivalent(flatten(a, a.Kind), flatten(b, a.Kind), identEq)
	case KindNot:
		return b.Kind == KindNot && Equivalent(a.Inner, b.Inner, identEq)
	case KindCmp:
		if b.Kind != KindCmp || a.Op != b.Op {
			return false
		}
		if a.Op == "==" || a.Op == "!=" {
			return (Equivalent(a.L, b.L, identEq) && Equivalent(a.R, b.R, identEq)) ||
				(Equivalent(a.L, b.R, identEq) && Equivalent(a.R, b.L, identEq))
		}
		return Equivalent(a.L, b.L, identEq) && Equivalent(a.R, b.R, identEq)
	case KindCall:
		return b.Kind == KindCall && identEq(a.Name, b.Name) && argsEquivalent(a.Args, b.Args, identEq)
	case KindIdent:
		if b.Kind != KindIdent {
			return false
		}
		if isNilName(a.Name) && isNilName(b.Name) {
			return true
		}
		return identEq(a.Name, b.Name)
	case KindLit:
		return b.Kind == KindLit && normLit(a.Text) == normLit(b.Text)
	case KindRaw:
		return b.Kind == KindRaw && rawEquivalent(a.Text, b.Text, identEq)
	}
	return false
}

// flatten unpacks same-kind nested nodes: `a && (b && c)` and `a && b && c`
// are the same boolean condition, but the parser may nest either way.
func flatten(e *Expr, kind Kind) []Expr {
	if e.Kind != kind {
		return []Expr{*e}
	}
	var out []Expr
	for i := range e.Items {
		out = append(out, flatten(&e.Items[i], kind)...)
	}
	return out
}

// itemsEquivalent matches two item lists commutatively (boolean && / || are
// order-independent), backtracking over the unused items.
func itemsEquivalent(xs, ys []Expr, identEq func(a, b string) bool) bool {
	if len(xs) != len(ys) {
		return false
	}
	used := make([]bool, len(ys))
	var match func(i int) bool
	match = func(i int) bool {
		if i == len(xs) {
			return true
		}
		for j := range ys {
			if used[j] {
				continue
			}
			if !Equivalent(&xs[i], &ys[j], identEq) {
				continue
			}
			used[j] = true
			if match(i + 1) {
				return true
			}
			used[j] = false
		}
		return false
	}
	return match(0)
}

// argsEquivalent compares call arguments token-wise: identifiers through
// identEq, literals normalized, everything else verbatim.
func argsEquivalent(xs, ys []string, identEq func(a, b string) bool) bool {
	if len(xs) != len(ys) {
		return false
	}
	for i := range xs {
		if !tokensEquivalent(argTokens(xs[i]), argTokens(ys[i]), identEq) {
			return false
		}
	}
	return true
}

// rawEquivalent compares raw predicate text token-wise (identifiers through
// identEq, whitespace-insensitive).
func rawEquivalent(a, b string, identEq func(a, b string) bool) bool {
	return tokensEquivalent(argTokens(a), argTokens(b), identEq)
}

func tokensEquivalent(xs, ys []string, identEq func(a, b string) bool) bool {
	if len(xs) != len(ys) {
		return false
	}
	for i := range xs {
		x, y := xs[i], ys[i]
		switch {
		case isIdentText(x) && isIdentText(y):
			if !identEq(x, y) {
				return false
			}
		case isNilName(x) && isNilName(y):
		case normLit(x) != normLit(y):
			return false
		}
	}
	return true
}

// argTokens splits call-argument (or raw) text into identifier, literal, and
// symbol tokens, dropping whitespace. Operators/punctuation runs stay one
// token so `a.b` vs `a . b` cannot drift.
func argTokens(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"' || c == '\'':
			q := c
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == q {
					break
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			out = append(out, normLit(s[i:j+1]))
			i = j + 1
		case isIdentChar(c):
			// Consume a dotted chain but keep the FIRST segment: host-var
			// array idioms (`ls_match_acc.arr`) and legacy struct member
			// reads both name the same variable the Go side spells as one
			// identifier (`lsMatchAcc`).
			j := i
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			out = append(out, s[i:j])
			i = j
			for i < len(s) && s[i] == '.' && i+1 < len(s) && isIdentChar(s[i+1]) {
				i++
				for i < len(s) && isIdentChar(s[i]) {
					i++
				}
			}
		default:
			j := i
			for j < len(s) && !isIdentChar(s[j]) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' && s[j] != '"' && s[j] != '\'' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

func isIdentText(s string) bool {
	return s != "" && isIdentChar(s[0]) && (s[0] < '0' || s[0] > '9')
}

// normLit normalizes a literal token: C char literals become double-quoted
// strings with the same content, NULL/nil collapse to one spelling, and
// numeric text loses underscore separators.
func normLit(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 2 && (t[0] == '\'' || t[0] == '"') && t[len(t)-1] == t[0] {
		inner := t[1 : len(t)-1]
		return `"` + inner + `"`
	}
	if isNilName(t) {
		return "nil"
	}
	return strings.ReplaceAll(t, "_", "")
}

func isNilName(s string) bool { return s == "nil" || s == "NULL" || s == "null" }

// presenceFieldsEqual compares an FML presence field against a Go presence
// ident: FML spellings normalize through FieldFromFML (`FML_FOO` → `Foo`)
// so the legacy field and the request-struct field meet under the caller's
// identEq (bindings or spelling keys). Non-FML names compare directly.
func presenceFieldsEqual(a, b string, identEq func(a, b string) bool) bool {
	return identEq(presenceGo(a), presenceGo(b))
}

func presenceGo(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 4 && (t[:4] == "FML_" || t[:4] == "fml_") {
		return common.FieldFromFML(t)
	}
	// FML field constants sometimes arrive lowercased through arg-token
	// normalization — match case-insensitively before giving up.
	upper := strings.ToUpper(t)
	if strings.HasPrefix(upper, "FML_") {
		return common.FieldFromFML(upper)
	}
	return t
}
