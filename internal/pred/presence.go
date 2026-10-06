package pred

import (
	"strconv"
	"strings"
)

// Foccur32 presence semantics (logical-part home):
//
//	Foccur32(buf, FIELD) returns the occurrence count of FIELD in buf.
//	`Foccur32(fml_ibuffer, FML_X) > 0` means FIELD was present in the
//	request; `== 0` means absent. The 16-bit `Foccur` spelling carries the
//	same existence meaning. Branch conditions on this shape are business
//	logic (which optional flag arrived), never plumbing — the census,
//	draft, and guard gates must keep them live as Go presence checks
//	(`request.<Field> != ""` / `== ""`).
//
// This file is the one home for recognizing that shape so every logical
// consumer (flow census/skeleton/render, guard/condition equivalence,
// convert bindings) shares one definition.

// IsFoccurName reports whether a call name is an FML occurrence count:
// Foccur32 (case-insensitive) or the Foccur shorthand.
func IsFoccurName(name string) bool {
	return strings.EqualFold(name, "Foccur32") || strings.EqualFold(name, "Foccur")
}

// FoccurField extracts the FML field a foccur call counts: the 2nd
// argument (`Foccur32(buf, FML_X)` → `FML_X`). The buffer (1st arg) is
// deliberately ignored — request vs local buffer spellings name the same
// presence fact.
func FoccurField(call *Expr) (string, bool) {
	if call == nil || call.Kind != KindCall || !IsFoccurName(call.Name) {
		return "", false
	}
	if len(call.Args) < 2 {
		return "", false
	}
	field := strings.TrimSpace(call.Args[1])
	if field == "" {
		return "", false
	}
	return field, true
}

// FoccurFields collects every FML field counted by foccur calls in e,
// in first-appearance order — the binding/census scan for presence
// guards nested inside && / || / ! compounds.
func FoccurFields(e *Expr) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(x *Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}
		switch x.Kind {
		case KindOr, KindAnd:
			for i := range x.Items {
				walk(&x.Items[i])
			}
		case KindNot:
			walk(x.Inner)
		case KindCmp:
			walk(x.L)
			walk(x.R)
		case KindCall:
			if f, ok := FoccurField(x); ok && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	walk(e)
	return out
}

// PresenceOf reports whether e is an FML presence check: a foccur count
// compared against the 0/1 boundary, a bare foccur call (C truthiness —
// present when nonzero), or its negation.
//
// Returns the counted FML field and present=true for the
// present-shapes (`> 0`, `!= 0`, `>= 1`, bare call), present=false for
// the absent-shapes (`== 0`, `<= 0`, `< 1`, `!call`). Exact-count
// comparisons (`== 1`, `> 1`, …) are occurrence arithmetic, not
// existence — ok=false so callers keep the honest TODO.
func PresenceOf(e *Expr) (field string, present bool, ok bool) {
	if e == nil {
		return "", false, false
	}
	switch e.Kind {
	case KindCall:
		if f, ok := FoccurField(e); ok {
			return f, true, true
		}
		return "", false, false
	case KindNot:
		if f, p, ok := PresenceOf(e.Inner); ok {
			// `!present` ↔ absent; double negation composes via recursion.
			// Only invert when the inner is a bare/call presence — a
			// `!(x > 0)` comparison negation stays a comparison, handled
			// by the caller's Not rendering, not by flipping presence.
			if e.Inner != nil && e.Inner.Kind == KindCall {
				return f, !p, true
			}
			// `!(Foccur(...) > 0)` is logically absent — accept the flip.
			if e.Inner != nil && e.Inner.Kind == KindCmp {
				return f, !p, true
			}
		}
		return "", false, false
	case KindCmp:
		return cmpPresence(e)
	}
	return "", false, false
}

// cmpPresence folds one comparison with a foccur side against a numeric
// literal on the other side (either order: `Foccur(...) > 0` or
// `0 < Foccur(...)`).
func cmpPresence(e *Expr) (string, bool, bool) {
	if e == nil || e.Kind != KindCmp {
		return "", false, false
	}
	if f, ok := FoccurField(e.L); ok {
		if n, ok := numericLit(e.R.TextOf()); ok {
			return fieldPresence(f, e.Op, n, false)
		}
		return "", false, false
	}
	if f, ok := FoccurField(e.R); ok {
		if n, ok := numericLit(e.L.TextOf()); ok {
			return fieldPresence(f, e.Op, n, true)
		}
	}
	return "", false, false
}

// TextOf renders a leaf's comparable text (ident name or literal text).
func (e *Expr) TextOf() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case KindIdent:
		return e.Name
	case KindLit:
		return e.Text
	}
	return ""
}

// numericLit parses a C numeric literal with an optional L/l suffix
// (`0`, `0L`, `1`) — the only comparands a presence check carries.
func numericLit(text string) (int, bool) {
	t := strings.TrimSpace(text)
	t = strings.TrimSuffix(t, "L")
	t = strings.TrimSuffix(t, "l")
	if t == "" {
		return 0, false
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, false
	}
	return n, true
}

// fieldPresence applies the comparison operator to the occurrence count:
// swapped inverts the operator's direction (`0 < Foccur` ≡ `Foccur > 0`).
func fieldPresence(field, op string, n int, swapped bool) (string, bool, bool) {
	if swapped {
		op = swapCmp(op)
	}
	switch op {
	case ">":
		if n == 0 {
			return field, true, true
		}
	case "!=":
		if n == 0 {
			return field, true, true
		}
	case ">=":
		if n == 1 {
			return field, true, true
		}
	case "==":
		if n == 0 {
			return field, false, true
		}
	case "<=":
		if n == 0 {
			return field, false, true
		}
	case "<":
		if n == 1 {
			return field, false, true
		}
	}
	return "", false, false
}

func swapCmp(op string) string {
	switch op {
	case ">":
		return "<"
	case "<":
		return ">"
	case ">=":
		return "<="
	case "<=":
		return ">="
	}
	return op
}

// GoPresenceOf reports whether e is the Go presence form the draft and
// the gates accept for a legacy foccur check: `X != ""` (present) or
// `X == ""` (absent), with X a bare identifier (ParseCode already
// collapsed `request.X` to `X`).
func GoPresenceOf(e *Expr) (ident string, present bool, ok bool) {
	if e == nil || e.Kind != KindCmp {
		return "", false, false
	}
	if e.L != nil && e.L.Kind == KindIdent && e.R != nil && e.R.Kind == KindLit && isEmptyStringLit(e.R.Text) {
		switch e.Op {
		case "!=":
			return e.L.Name, true, true
		case "==":
			return e.L.Name, false, true
		}
		return "", false, false
	}
	if e.R != nil && e.R.Kind == KindIdent && e.L != nil && e.L.Kind == KindLit && isEmptyStringLit(e.L.Text) {
		switch e.Op {
		case "!=":
			return e.R.Name, true, true
		case "==":
			return e.R.Name, false, true
		}
	}
	return "", false, false
}

func isEmptyStringLit(text string) bool {
	t := strings.TrimSpace(text)
	return t == `""` || t == `''`
}
