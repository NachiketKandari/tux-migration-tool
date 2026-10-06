// Package namer is the deterministic per-language naming + type projection
// over the language-neutral contract model (uniform-ir plan §3.3).
//
// The contract carries raw facts (FML names, :host binds, canonical C
// types). Namers derive what each emitter needs: method/param/property
// names and host-language scalar types. Type maps are tested once here
// instead of once per backend.
package namer

import (
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/ir"
)

// Namer derives language-specific names and types from contract fields.
type Namer interface {
	// Lang identifies the target ("go", "py", "cs").
	Lang() string
	// Method derives a DB/repo method name for a query unit.
	Method(q contract.QueryUnit) string
	// Param derives a parameter/property name for a :host bind.
	Param(bind string) string
	// Prop derives a row/DTO property name for a row-shape entry.
	Prop(bind string) string
	// FieldType maps a contract field to the host-language scalar.
	FieldType(f contract.Field) string
}

// GoNamer implements Namer for the Go/sqlx emitter.
type GoNamer struct{}

// Lang implements Namer.
func (GoNamer) Lang() string { return "go" }

// Method implements Namer: Get<Table> / Insert<Table> ... with cursor-name
// preference, mirroring plan.methodName so goldens cannot drift.
func (GoNamer) Method(q contract.QueryUnit) string {
	if q.CursorName != "" {
		return "Get" + common.CamelGo(strings.TrimPrefix(q.CursorName, "cur_"))
	}
	table := "Row"
	if len(q.Tables) > 0 && q.Tables[0] != "" {
		table = tableToken(q.Tables[0])
	}
	switch q.Kind {
	case contract.QueryInsert:
		return "Insert" + common.CamelGo(table)
	case contract.QueryUpdate:
		return "Update" + common.CamelGo(table)
	case contract.QueryDelete:
		return "Delete" + common.CamelGo(table)
	case contract.QueryMerge:
		return "Merge" + common.CamelGo(table)
	default:
		return "Get" + common.CamelGo(table)
	}
}

// Param implements Namer: lowerCamel Go param names.
func (GoNamer) Param(bind string) string {
	base := strings.TrimPrefix(bind, ":")
	base = strings.TrimPrefix(base, "sql_")
	return common.CamelLowerGo(strings.ToLower(base))
}

// Prop implements Namer: exported Go struct fields.
func (GoNamer) Prop(bind string) string {
	base := rowBase(bind)
	base = strings.TrimPrefix(base, "sql_")
	return common.Export(common.CamelLowerGo(strings.ToLower(base)))
}

// FieldType implements Namer: DB-backed fields are uniformly sql.NullString
// (the gen rowFields rule); request/response scalars are string; numerics
// keep the Go scalar for stub/helper signatures.
func (GoNamer) FieldType(f contract.Field) string {
	switch f.Kind {
	case contract.FieldRow:
		return "sql.NullString"
	case contract.FieldRequest, contract.FieldResponse, contract.FieldError:
		return "string"
	case contract.FieldParam:
		return goScalar(f.CType)
	default:
		return "string"
	}
}

func goScalar(cType string) string {
	switch ir.CanonicalCType(cType) {
	case "char", "varchar", "":
		return "string"
	case "int", "short":
		return "int"
	case "long":
		return "int64"
	case "float":
		return "float32"
	case "double":
		return "float64"
	default:
		return "string"
	}
}

// PyNamer implements Namer for the Python/oracledb emitter.
type PyNamer struct{}

// Lang implements Namer.
func (PyNamer) Lang() string { return "py" }

// Method implements Namer: fetch_<cursor> / <verb>_<table>.
func (PyNamer) Method(q contract.QueryUnit) string {
	if q.CursorName != "" {
		return "fetch_" + strings.ToLower(common.PyIdent(q.CursorName))
	}
	return pyVerb(q.Kind) + "_" + strings.ToLower(common.PyIdent(firstTable(q.Tables)))
}

// Param implements Namer: bind names pass through (oracledb named binds).
func (PyNamer) Param(bind string) string { return strings.TrimPrefix(bind, ":") }

// Prop implements Namer: row-shape entries pass through for whole-row fetch.
func (PyNamer) Prop(bind string) string { return rowBase(bind) }

// FieldType implements Namer: Python scalars.
func (PyNamer) FieldType(f contract.Field) string {
	switch ir.CanonicalCType(f.CType) {
	case "int", "short", "long":
		return "int"
	case "float", "double":
		return "float"
	default:
		return "str"
	}
}

func pyVerb(k contract.QueryKind) string {
	switch k {
	case contract.QuerySelectOne, contract.QuerySelectMany:
		return "fetch"
	default:
		return strings.ToLower(string(k))
	}
}

// CsNamer implements Namer for the C#/Oracle emitter.
type CsNamer struct{}

// Lang implements Namer.
func (CsNamer) Lang() string { return "cs" }

// Method implements Namer: <Verb><Table>Query (GetCst...Query).
func (CsNamer) Method(q contract.QueryUnit) string {
	verb := "Get"
	switch q.Kind {
	case contract.QueryInsert:
		verb = "Insert"
	case contract.QueryUpdate:
		verb = "Update"
	case contract.QueryDelete:
		verb = "Delete"
	case contract.QueryMerge:
		verb = "Merge"
	}
	return verb + common.Pascal(firstTable(q.Tables)) + "Query"
}

// Param implements Namer: PascalCase request properties.
func (CsNamer) Param(bind string) string { return common.Pascal(bind) }

// Prop implements Namer: UPPER_SNAKE DTO properties.
func (CsNamer) Prop(bind string) string { return common.UpperSnake(rowBase(bind)) }

// FieldType implements Namer: C# scalars.
func (CsNamer) FieldType(f contract.Field) string {
	switch ir.CanonicalCType(f.CType) {
	case "int", "short":
		return "int"
	case "long":
		return "long"
	case "float", "double":
		return "decimal"
	default:
		return "string"
	}
}

func firstTable(tables []string) string {
	if len(tables) == 0 || tables[0] == "" {
		return "Row"
	}
	return tables[0]
}

func tableToken(t string) string {
	var sb strings.Builder
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 {
		return "Row"
	}
	return sb.String()
}

func rowBase(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, " \t"); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, ":")
}
