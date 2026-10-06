// FixtureSource seam (PRD-2026-09-09 GT-D5): the values tests assert on come
// from a pluggable backend. v0 is AssumedFixtureSource — deterministic
// placeholder values synthesized from the extracted shapes — so -no-llm
// runs are byte-identical everywhere. The real backend (GT-7, on user logs)
// swaps realism in without touching the pipeline.
package testgen

import (
	"fmt"
	"strconv"
	"strings"
)

// FixtureSource provides assumed fixture values for one service.
type FixtureSource interface {
	// FieldLit returns the Go type of a json-tagged field and a literal of that
	// type for the case struct, e.g. ("[]string", `[]string{"answerid"}`).
	//
	// It exists because FieldValues cannot express a non-string field: the
	// generated case struct declared every field `string`, so a slice-typed
	// request field produced "cannot use testCase.AnswerID (variable of type
	// string) as []string value". A literal rather than a bare value because
	// the template embeds it unquoted, which a []string cannot survive.
	//
	// ("", "") means "no declared type" and the caller keeps its previous
	// string behaviour — the conservative reading, since a wrong literal is a
	// compile error whereas the string form at least renders.
	FieldLit(structName, field string) (typ, lit string)
	// RowValues returns one mock row for the db-tag columns, in order.
	RowValues(cols []string) []string
	// ColExpr renders the expected-struct field literal for a db column of
	// the given Go type, e.g. sql.NullString{String: "comp_cd", Valid: true}.
	ColExpr(col, typ string) string
	// ArgValue renders a store-call bind literal for a param of the given
	// type, e.g. "comp_cd" / time.Now() / 0.
	ArgValue(name, typ string) string
	// FieldValues returns the json-tagged fields of a models struct as
	// (field name, assumed value) pairs, in declaration order.
	FieldValues(structName string) [][2]string
	// ZeroExpr renders the zero value literal for a scalar Go type.
	ZeroExpr(scalar string) string
}

// AssumedFixtureSource synthesizes deterministic placeholders: db values are
// the lowercased column name, bind literals the lowercased param name, json
// values the lowercased field name. Every consumer of a value derives both
// sides (mock row and expected expr) from the same source, so asserted
// equality always matches.
type AssumedFixtureSource struct {
	Models *modelsInfo
}

func placeholder(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 32)
		}
	}
	if b.Len() == 0 {
		return "val"
	}
	return b.String()
}

// RowValues implements FixtureSource.
func (a *AssumedFixtureSource) RowValues(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = placeholder(c)
	}
	return out
}

// ColExpr implements FixtureSource.
func (a *AssumedFixtureSource) ColExpr(col, typ string) string {
	v := placeholder(col)
	switch typ {
	case "sql.NullString":
		return fmt.Sprintf("sql.NullString{String: %q, Valid: true}", v)
	case "sql.NullInt64":
		return fmt.Sprintf("sql.NullInt64{Int64: 0, Valid: true}")
	case "sql.NullTime":
		return "sql.NullTime{Time: time.Date(2025, 4, 28, 0, 0, 0, 0, time.UTC), Valid: true}"
	default:
		return fmt.Sprintf("%q", v)
	}
}

// ArgValue implements FixtureSource.
func (a *AssumedFixtureSource) ArgValue(name, typ string) string {
	switch typ {
	case "string":
		return fmt.Sprintf("%q", placeholder(name))
	case "time.Time":
		return "time.Now()"
	case "bool":
		return "false"
	case "int", "int32", "int64", "float32", "float64":
		return "0"
	case "sql.NullString", "sql.NullInt64", "sql.NullTime":
		return typ + "{}"
	default:
		return "nil"
	}
}

// FieldLit implements FixtureSource.
//
// Only slices and struct-valued fields get a typed literal. A string field
// deliberately returns ("", "") so the case struct's declaration and value stay
// byte-identical to what they were before this method existed — the majority of
// fields are strings, and re-rendering them buys nothing.
//
// A slice becomes a one-element literal over its element type, and a struct
// element becomes a composite literal over that struct's own fields, so
// []models.QnA expands into the fields QnA actually declares rather than a
// placeholder that would not compile.
func (a *AssumedFixtureSource) FieldLit(structName, field string) (string, string) {
	typ, elem, isSlice := a.fieldType(structName, field)
	switch {
	case isSlice && elem == "string":
		return typ, "[]string{" + strconv.Quote(placeholder(field)) + "}"
	case isSlice:
		// The TYPE is qualified too, not just the literal. The generated
		// case struct lives in the db/controller/handler package and the
		// models file declares its own types unqualified (`QnA []QnA`), so a
		// raw `[]QnA` as the field declaration is "undefined: QnA" in the
		// generated suite. Qualifying only the value would have left the
		// declaration broken while the literal looked right.
		q := a.qualified(elem)
		decl := "[]" + q
		if lit, ok := a.elemLiteral(elem, 0); ok {
			return decl, decl + "{{" + lit + "}}"
		}
		return decl, decl + "{}"
	case typ != "" && typ != "string":
		q := a.qualified(elem)
		if lit, ok := a.elemLiteral(elem, 0); ok {
			return q, q + "{" + lit + "}"
		}
		return q, q + "{}"
	}
	return "", ""
}

// qualified renders a model type name as the generated test must spell it.
// A models file declares its own types unqualified (`QnA []QnA`), but the test
// lives in the db/controller package and refers to them as models.QnA — so an
// unqualified name that the inventory knows about gets the package prefix.
// A name the inventory does not know is already qualified and passes through.
func (a *AssumedFixtureSource) qualified(name string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	if a.Models == nil {
		return name
	}
	if _, ok := a.Models.Structs[name]; ok {
		return "models." + name
	}
	return name
}

// fieldType resolves a field's declared Go type. elem is the element type
// unqualified and isSlice reports whether a slice prefix was present.
//
// elem stays as written in models; FieldLit qualifies it before rendering,
// because the generated case struct lives in another package and needs
// `models.QnA` where the model says `QnA`.
func (a *AssumedFixtureSource) fieldType(structName, field string) (typ, elem string, isSlice bool) {
	if a.Models == nil {
		return "", "", false
	}
	for _, f := range a.Models.Structs[structName] {
		if f.Name != field {
			continue
		}
		t := f.Type
		if strings.HasPrefix(t, "[]") {
			// The element stays as DECLARED. FieldLit qualifies it itself
			// before rendering, and pre-qualifying here made it look up
			// "models.QnA" in the struct table, miss, and return an empty
			// composite literal where the element's own fields belong.
			return t, t[2:], true
		}
		return f.Type, unqualify(t), false
	}
	return "", "", false
}

// unqualify strips a package qualifier from a type name.
func unqualify(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 {
		return t[i+1:]
	}
	return t
}

// elemLiteral renders a struct type's fields as `Name: value, …` pairs.
// Recursion is bounded: a model that refers to itself through a slice would
// otherwise expand forever, and a self-referential request is not a shape this
// generator can honour anyway.
func (a *AssumedFixtureSource) elemLiteral(structName string, depth int) (string, bool) {
	if a.Models == nil || depth > 2 {
		return "", false
	}
	fields := a.Models.Structs[structName]
	if len(fields) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f.Name+": "+a.fieldLiteralOf(f, depth+1))
	}
	return strings.Join(parts, ", "), true
}

// fieldLiteralOf renders one fieldInfo's value at the given recursion depth.
func (a *AssumedFixtureSource) fieldLiteralOf(f fieldInfo, depth int) string {
	typ := f.Type
	elem := unqualify(typ)
	isSlice := strings.HasPrefix(typ, "[]")
	if isSlice {
		elem = typ[2:]
	}
	switch {
	case isSlice && elem == "string":
		return "[]string{" + validatedLit(f.Validate, "string", strconv.Quote(placeholder(f.Name)), false) + "}"
	case typ == "string":
		return validatedLit(f.Validate, "string", strconv.Quote(placeholder(f.Name)), false)
	case typ == "bool":
		return "false"
	case typ == "int" || typ == "int8" || typ == "int16" || typ == "int32" || typ == "int64" ||
		typ == "uint" || typ == "uint8" || typ == "uint16" || typ == "uint32" || typ == "uint64" ||
		typ == "float32" || typ == "float64":
		// A zero is a legal number but not a legal positive one, so a
		// positivenum/gte tag has to move it.
		return validatedLit(f.Validate, typ, "0", false)
	case strings.HasPrefix(typ, "sql.Null") || typ == "time.Time":
		return typ + "{}"
	case elem != "" && depth <= 2 && a.Models != nil && a.knownStruct(elem):
		q := a.qualified(elem)
		if isSlice {
			if lit, ok := a.elemLiteral(elem, depth); ok {
				return "[]" + q + "{{" + lit + "}}"
			}
			return "[]" + q + "{}"
		}
		if lit, ok := a.elemLiteral(elem, depth); ok {
			return q + "{" + lit + "}"
		}
		return q + "{}"
	}
	// An interface, a pointer, or a type the models inventory does not
	// describe: nil is the only value guaranteed to compile.
	return "nil"
}

// knownStruct reports whether the models inventory declares this type as a
// struct, which is what makes a composite literal over its fields meaningful.
func (a *AssumedFixtureSource) knownStruct(name string) bool {
	_, ok := a.Models.Structs[name]
	return ok
}

// FieldValues implements FixtureSource. Tag options (,omitempty) are
// stripped before placeholdering — values derive from the field name only.
func (a *AssumedFixtureSource) FieldValues(structName string) [][2]string {
	fields := a.Models.Structs[structName]
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		if f.JSON == "" {
			continue
		}
		name := f.JSON
		if i := strings.Index(name, ","); i >= 0 {
			name = name[:i]
		}
		out = append(out, [2]string{f.Name, validatedBody(f, name)})
	}
	return out
}

// validatedBody returns a request field's body text — the value a case struct
// carries, unquoted — chosen to satisfy the field's validator tag.
//
// The body is escaped for embedding between quotes by the template, so the
// tag is applied to the escaped form and the result is escaped back. A tag
// with no rule this file models leaves the placeholder alone.
func validatedBody(f fieldInfo, name string) string {
	body := placeholder(name)
	if f.Validate == "" {
		return body
	}
	// validatedLit works on quoted literals for string fields; strip the
	// quotes so a tag like oneof=A B reaches it as text.
	lit := validatedLit(f.Validate, "string", strconv.Quote(body), false)
	if unq, err := strconv.Unquote(lit); err == nil {
		return unq
	}
	return body
}

// ZeroExpr implements FixtureSource.
func (a *AssumedFixtureSource) ZeroExpr(scalar string) string {
	switch scalar {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64":
		return "0"
	case "sql.NullString", "sql.NullInt64", "sql.NullInt32",
		"sql.NullBool", "sql.NullFloat64", "sql.NullTime":
		return scalar + "{}"
	case "time.Time":
		return "time.Time{}"
	default:
		return "0"
	}
}
