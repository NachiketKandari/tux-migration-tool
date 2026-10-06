package testgen

import (
	"strconv"
	"strings"
)

// A handler binds its request with c.BindJSON, which runs the model's
// validator tags. A synthesized value that violates one is rejected before the
// controller is ever called, so the handler answers 400 and the case asserts
// nothing about the code under test. That is why nearly every generated
// handler case failed, including Success:
//
//	Key: 'AddQuestionRequest.CustomerType' … failed on the 'oneof' tag
//	Key: 'AddQuestionRequest.QuestionNo' … failed on the 'positivenum' tag
//
// The constraint is written on the field. This file reads it and produces a
// value that satisfies it, rather than guessing a placeholder and hoping.

// validatedLit returns a literal for a field that satisfies its validation
// tag, falling back to the caller's own value when the tag is absent or is one
// this file does not model.
//
// typ is the field's declared Go type, because the same rule renders
// differently for a string ("1") and a number (1). A tagged slice is
// constrained by its element, so isSlice passes the element type through.
func validatedLit(tag, typ, fallback string, isSlice bool) string {
	if tag == "" {
		return fallback
	}
	if isSlice {
		typ = strings.TrimPrefix(typ, "[]")
	}
	for _, rule := range strings.Split(tag, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" || rule == "-" {
			continue
		}
		name, arg := rule, ""
		if i := strings.Index(rule, "="); i >= 0 {
			name, arg = rule[:i], rule[i+1:]
		}
		if v, ok := satisfyRule(name, arg, typ, fallback); ok {
			return v
		}
	}
	return fallback
}

// satisfyRule maps one validator rule to a value that passes it.
func satisfyRule(name, arg, typ, fallback string) (string, bool) {
	isNum := typ != "" && typ != "string" && !strings.HasPrefix(typ, "[]")
	quoted := func(s string) string { return strconv.Quote(s) }

	switch name {
	case "oneof":
		// The allowed set is the argument, space separated. The first option
		// is the least surprising choice: it is the value the author listed
		// first, and any of them passes.
		opts := strings.Fields(arg)
		if len(opts) == 0 {
			return "", false
		}
		if isNum {
			if n, err := strconv.Atoi(opts[0]); err == nil {
				return strconv.Itoa(n), true
			}
			return "", false
		}
		return quoted(opts[0]), true

	case "positivenum", "numeric", "matchaccount":
		// A positive number, as digits. The corpus writes these on string
		// fields, so the digits are quoted for those and bare for numbers.
		if isNum {
			return "1", true
		}
		return quoted("1"), true

	case "email":
		return quoted("test@example.com"), true

	case "alphanum":
		return quoted("abc123"), true

	case "uuid", "uuid4":
		return quoted("123e4567-e89b-12d3-a456-426614174000"), true

	case "required", "required_if", "required_with", "required_without":
		// NOT a rule to satisfy, so it must not consume the value.
		//
		// The corpus writes `binding:"required,oneof=W R"`, and the
		// placeholder is already non-empty, so returning it here returned a
		// value that then FAILED the oneof that followed. Every oneof and
		// positivenum field in the corpus is behind a `required`, so this one
		// case suppressed the whole fix. An unmodelled rule returns false and
		// the loop continues to the next one; required must do the same.
		return "", false

	case "len":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return "", false
		}
		return sizedLiteral(fallback, n, isNum), true

	case "min":
		n, err := strconv.Atoi(arg)
		if err != nil {
			return "", false
		}
		if isNum {
			return strconv.Itoa(n), true
		}
		return sizedLiteral(fallback, n, false), true

	case "gte", "gt":
		n, err := strconv.Atoi(arg)
		if err != nil {
			return "", false
		}
		if name == "gt" {
			n++
		}
		if isNum {
			return strconv.Itoa(n), true
		}
		return quoted(strings.Repeat("a", maxInt(n, 1))), true

	case "lte", "lt", "max":
		// The fallback is not known to violate an upper bound, and padding it
		// to satisfy one would be a guess. Leave it alone.
		return "", false
	}
	return "", false
}

// sizedLiteral returns fallback padded or trimmed to exactly n characters,
// which is what a len= constraint asks for.
func sizedLiteral(fallback string, n int, isNum bool) string {
	if isNum {
		return fallback
	}
	body := fallback
	if q := strings.TrimSpace(fallback); len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
		if unq, err := strconv.Unquote(q); err == nil {
			body = unq
		}
	}
	for len(body) < n {
		body += "a"
	}
	if len(body) > n {
		body = body[:n]
	}
	return strconv.Quote(body)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
