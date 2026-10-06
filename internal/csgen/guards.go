package csgen

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/csplan"
	"tux-migration-tool/internal/pred"
)

// guardErrs requires every runtime-dispatch guard to stay live in the
// residual block with the same predicate (S7 CS twin): identical structure,
// identical literals, and identifiers bound to the guard's variables
// through the plan's accepted spellings. A dropped, widened, inverted, or
// mutated guard is a hard gate error, never a silent behavior change.
func guardErrs(guards []csplan.Guard, body string) []string {
	if len(guards) == 0 {
		return nil
	}
	conds := csIfConds(body)
	var errs []string
	for _, g := range guards {
		bind := func(a, b string) bool {
			ka, kb := pred.IdentKey(a), pred.IdentKey(b)
			if v, ok := g.Bind[ka]; ok && v != "" {
				ka = v
			}
			if v, ok := g.Bind[kb]; ok && v != "" {
				kb = v
			}
			return ka == kb
		}
		matched := false
		for _, want := range []string{g.Cond, g.Alt} {
			if want == "" {
				continue
			}
			w := pred.ParseCode(want)
			for i := range conds {
				if pred.Equivalent(&w, &conds[i], bind) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Sprintf("runtime dispatch guard lost: line %d condition `%s` must stay live as an if condition with the same predicate", g.Line, g.Cond))
		}
	}
	return errs
}

// csIfConds extracts the parenthesized condition of every `if` (and
// `else if`) in a C# residual block, string/char/comment aware. Parseable
// conditions are returned as pred trees; unparseable text is skipped (the
// structural checks own broken code).
func csIfConds(body string) []pred.Expr {
	var out []pred.Expr
	i := 0
	for i < len(body) {
		switch {
		case body[i] == '"' || body[i] == '\'':
			i = skipQuoted(body, i)
		case body[i] == '/' && i+1 < len(body) && body[i+1] == '/':
			for i < len(body) && body[i] != '\n' {
				i++
			}
		case body[i] == '/' && i+1 < len(body) && body[i+1] == '*':
			i += 2
			for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
				i++
			}
			i = min(i+2, len(body))
		case isWordStart(body, i, "if"):
			j := i + 2
			for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
				j++
			}
			if j < len(body) && body[j] == '(' {
				if cond, next := balancedCond(body, j); next > j {
					out = append(out, pred.ParseCode(cond))
					i = next
					continue
				}
			}
			i++
		default:
			i++
		}
	}
	return out
}

// isWordStart reports whether word sits at i as a whole token.
func isWordStart(s string, i int, word string) bool {
	if !strings.HasPrefix(s[i:], word) {
		return false
	}
	if i > 0 && isWordByte(s[i-1]) {
		return false
	}
	if i+len(word) < len(s) && isWordByte(s[i+len(word)]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// balancedCond slices the text inside a parenthesized condition starting at
// the open paren, honoring nesting and literals. Returns the inner text and
// the index after the closing paren (next <= at when unbalanced).
func balancedCond(s string, at int) (string, int) {
	depth := 0
	i := at
	for i < len(s) {
		switch s[i] {
		case '"', '\'':
			i = skipQuoted(s, i)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[at+1 : i], i + 1
			}
		}
		i++
	}
	return "", at
}

// skipQuoted returns the index just past a quoted literal starting at i.
func skipQuoted(s string, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] == q {
			return i + 1
		}
		i++
	}
	return i
}
