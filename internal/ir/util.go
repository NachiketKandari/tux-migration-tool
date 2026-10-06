package ir

import (
	"strings"
)

// equalFold is an ASCII case-insensitive compare.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// splitArgs splits a raw argument text on top-level commas (parens,
// brackets, and quoted strings do not split).
func splitArgs(args string) []string {
	var out []string
	depth := 0
	quote := byte(0)
	start := 0
	for i := 0; i < len(args); i++ {
		c := args[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, args[start:i])
				start = i + 1
			}
		}
	}
	if start <= len(args) {
		out = append(out, args[start:])
	}
	return out
}

var castRe = func(s string) string {
	for {
		t := s
		if strings.HasPrefix(t, "(") {
			if end := strings.IndexByte(t, ')'); end > 0 {
				inner := t[1:end]
				if inner != "" && isCastInner(inner) {
					t = t[end+1:]
				}
			}
		}
		t = strings.TrimLeft(t, " \t")
		if t == s {
			return s
		}
		s = t
	}
}

func isCastInner(s string) bool {
	s = strings.TrimRight(s, " \t*")
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == ' ' || c == '\t' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// baseIdent strips casts, address-of, and whitespace, returning the leading
// identifier of the expression ("" when the argument is not an identifier
// expression, e.g. a literal).
func baseIdent(arg string) string {
	s := castRe(strings.TrimSpace(arg))
	s = strings.TrimLeft(s, "& \t")
	end := 0
	for end < len(s) && isIdentByteFor(s[end]) {
		end++
	}
	return s[:end]
}

func isIdentByteFor(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// stringLit returns the unquoted contents when the argument is a string
// literal, else "".
func stringLit(arg string) string {
	s := strings.TrimSpace(arg)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return ""
}

// identifierArg is the identifier-ness of an argument: a bare name
// (optionally cast/amp-prefixed) as opposed to a literal or complex expr.
func identifierArg(arg string) (string, bool) {
	s := castRe(strings.TrimSpace(arg))
	s = strings.TrimLeft(s, "& \t")
	if s == "" {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByteFor(s[i]) {
			return "", false
		}
	}
	return s, true
}
