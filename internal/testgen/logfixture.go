// GT-7 log-backed fixtures: LogFixtureSource implements FixtureSource with
// real values parsed from the target app's runtime log. It is scoped per
// (service, layer, method) — ForUnit returns a copy carrying that method's
// selected traces — and every value lookup degrades to the assumed
// placeholder on a miss, so a log can never invent structure (plan
// "Fixture values" / "Fallbacks").
package testgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/testscan"
)

// ScopedFixtureSource is the GT-7 seam: a fixture source that can scope
// itself to one service method, returning the values for that method. The
// engine calls ForUnit per rendered function; sources that cannot scope
// simply keep returning themselves.
type ScopedFixtureSource interface {
	FixtureSource
	ForUnit(service, layer, method string) FixtureSource
}

// ResponseFieldSource is implemented by sources holding logged response
// values distinct from request values (the same json tag can carry a request
// value and a different response value in one trace).
type ResponseFieldSource interface {
	ResponseFieldValues(structName string) [][2]string
}

// MethodLogValues is implemented by scoped log sources: it exposes the
// selected traces so renderers can add the logged second row, mock the
// controller's store calls with real values, and tag provenance.
type MethodLogValues interface {
	FixtureSource
	SuccessTrace() *LogTrace
	FailedTrace() *LogTrace
	ResponseRaw() json.RawMessage
	ErrorMsg() string
	Provenance() string
}

// LogFixtureSource is the log-driven FixtureSource backend (GT-7). Build one
// per service with NewLogFixtureSource and scope it per method via ForUnit.
type LogFixtureSource struct {
	Models *modelsInfo
	data   *LogData

	service string
	layer   string
	method  string
	vals    *logMethodValues
}

// logMethodValues is one scoped method's selected values.
type logMethodValues struct {
	success *LogTrace
	failed  *LogTrace
	req     map[string]json.RawMessage
	resp    map[string]json.RawMessage
	row     map[string]json.RawMessage // first logged row object (field name → raw)
	scalar  json.RawMessage            // first logged scalar result
	hasRow  bool
	hasScal bool
	errMsg  string
}

// NewLogFixtureSource builds the log backend for one service.
func NewLogFixtureSource(data *LogData, models *modelsInfo, service string) *LogFixtureSource {
	return &LogFixtureSource{Models: models, data: data, service: service}
}

// ForUnit scopes the source to one method: success/failed traces are selected
// with the plan's rules (first successful complete trace; first failed
// complete trace), and request/response/db values are derived from the
// primary trace.
func (l *LogFixtureSource) ForUnit(service, layer, method string) FixtureSource {
	if l == nil || l.data == nil {
		return l
	}
	scoped := &LogFixtureSource{Models: l.Models, data: l.data, service: service, layer: layer, method: method}
	v := &logMethodValues{}
	v.success = l.data.Trace(service, method, false)
	v.failed = l.data.Trace(service, method, true)
	if v.failed == v.success {
		v.failed = nil
	}
	primary := v.success
	if primary == nil {
		primary = v.failed
	}
	if primary != nil {
		v.req = requestJSON(primary.RequestBody)
		v.resp, _ = responseData(primary)
		v.row, v.scalar, v.hasRow, v.hasScal = firstDBValue(primary, method)
	}
	if v.failed != nil {
		v.errMsg, _ = v.failed.ErrorForLayer(service, layer, method)
		if v.req == nil {
			v.req = requestJSON(v.failed.RequestBody)
		}
		if v.resp == nil {
			v.resp, _ = responseData(v.failed)
		}
	}
	scoped.vals = v
	return scoped
}

// SuccessTrace implements MethodLogValues.
func (l *LogFixtureSource) SuccessTrace() *LogTrace {
	if l == nil || l.vals == nil {
		return nil
	}
	return l.vals.success
}

// FailedTrace implements MethodLogValues.
func (l *LogFixtureSource) FailedTrace() *LogTrace {
	if l == nil || l.vals == nil {
		return nil
	}
	return l.vals.failed
}

// ResponseRaw implements MethodLogValues.
func (l *LogFixtureSource) ResponseRaw() json.RawMessage {
	if l == nil || l.vals == nil {
		return nil
	}
	if raw, ok := responseDataRaw(l.vals.success, l.vals.failed); ok {
		return raw
	}
	return nil
}

// ErrorMsg implements MethodLogValues.
func (l *LogFixtureSource) ErrorMsg() string {
	if l == nil || l.vals == nil {
		return ""
	}
	return l.vals.errMsg
}

// Provenance implements MethodLogValues: "log <short-id>" when the method has
// a complete trace, "assumed" otherwise.
func (l *LogFixtureSource) Provenance() string {
	if t := l.SuccessTrace(); t != nil {
		return "log " + t.ShortID()
	}
	if t := l.FailedTrace(); t != nil {
		return "log " + t.ShortID()
	}
	return "assumed"
}

// assumed falls back to the deterministic placeholder source.
func (l *LogFixtureSource) assumed() *AssumedFixtureSource {
	return &AssumedFixtureSource{Models: l.Models}
}

// LogCoverageEntry is one scanned function's log-fixture provenance:
// "log <short-id>" when a complete trace supplied its values, "assumed"
// when the log had no hit (same tagging the generator writes per method).
type LogCoverageEntry struct {
	Service string `json:"service"`
	Layer   string `json:"layer"`
	Func    string `json:"func"`
	Source  string `json:"source"`
}

// LogCoverage summarizes how a parsed log covers a scan report.
type LogCoverage struct {
	Entries []LogCoverageEntry `json:"entries"`
	FromLog int                `json:"from_log"`
	Assumed int                `json:"assumed"`
}

// CoverLog computes per-function fixture provenance for a scan report
// without rendering anything — the preflight behind
// `gentest -check-only -log-file`. It mirrors LogFixtureSource.Provenance
// (success trace first, failed trace as fallback) so the counts match what
// a generate run would tag. Models-layer directories are skipped: they are
// never a test target.
func CoverLog(rep *testscan.Report, data *LogData) LogCoverage {
	cov := LogCoverage{}
	if rep == nil || data == nil {
		return cov
	}
	for _, svc := range rep.Services {
		src := NewLogFixtureSource(data, nil, svc.Name)
		for _, lr := range svc.Layers {
			if lr.Layer == testscan.LayerModels {
				continue
			}
			for _, fn := range lr.Funcs {
				tag := "assumed"
				if scoped, ok := src.ForUnit(svc.Name, string(lr.Layer), fn.Name).(MethodLogValues); ok {
					tag = scoped.Provenance()
				}
				cov.Entries = append(cov.Entries, LogCoverageEntry{
					Service: svc.Name, Layer: string(lr.Layer), Func: fn.Name, Source: tag,
				})
				if tag == "assumed" {
					cov.Assumed++
				} else {
					cov.FromLog++
				}
			}
		}
	}
	return cov
}

// RowValues implements FixtureSource: logged values by db column, assumption
// for columns the trace did not log. Values are pre-escaped for embedding in
// the templates' double-quoted string literals.
func (l *LogFixtureSource) RowValues(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = l.assumed().RowValues([]string{c})[0]
		if l == nil || l.vals == nil || !l.vals.hasRow {
			continue
		}
		if raw, ok := l.fieldRawForColumn(c); ok {
			if prim, ok := logPrimitiveOf(raw); ok {
				out[i] = goStringBody(prim.Text)
			}
		}
	}
	return out
}

// ColExpr implements FixtureSource: the logged value rendered as the column
// field's Go literal, assumption on any miss.
func (l *LogFixtureSource) ColExpr(col, typ string) string {
	if l != nil && l.vals != nil && l.vals.hasRow {
		if raw, ok := l.fieldRawForColumn(col); ok {
			if lit := colExprFromRaw(typ, raw); lit != "" {
				return lit
			}
		}
	}
	return l.assumed().ColExpr(col, typ)
}

// ArgValue implements FixtureSource: store-call bind literals keep the
// assumed placeholder (the log does not carry Go parameter names).
func (l *LogFixtureSource) ArgValue(name, typ string) string {
	return l.assumed().ArgValue(name, typ)
}

// FieldValues implements FixtureSource: request values from the method's
// logged requestBody, mapped through the models json tags.
func (l *LogFixtureSource) FieldValues(structName string) [][2]string {
	return l.fieldValues(structName, false)
}

// FieldLit implements FixtureSource.
//
// The declared type always comes from the models inventory — the log records
// JSON, which carries no Go type — but the VALUE comes from the log when it
// holds one. That is the whole point of the log route: a logged
// AnswerID: ["a","b"] is real fixture data, and rendering it as a scalar
// placeholder would throw away the realism the log exists to supply.
//
// A logged array whose elements do not fit the declared element type is
// refused rather than coerced: an invented value that fails to compile is worse
// than the assumed placeholder, which at least has the right shape.
func (l *LogFixtureSource) FieldLit(structName, field string) (string, string) {
	typ, lit := l.assumed().FieldLit(structName, field)
	if typ == "" || l == nil || l.vals == nil {
		return typ, lit
	}
	var f fieldInfo
	for _, cand := range l.Models.Structs[structName] {
		if cand.Name == field {
			f = cand
			break
		}
	}
	if f.JSON == "" {
		return typ, lit
	}
	raw, ok := pickJSONKey(l.vals.req, jsonBase(f.JSON))
	if !ok {
		return typ, lit
	}
	elem := unqualify(strings.TrimPrefix(f.Type, "[]"))
	if !strings.HasPrefix(f.Type, "[]") || elem == "" {
		return typ, lit // scalar field: FieldValues already carries the value
	}
	if lit, ok := l.jsonSliceLit(raw, elem); ok {
		return typ, lit
	}
	return typ, lit
}

// knownStruct reports whether the models inventory declares this type, which is
// what distinguishes a struct element (rendered as a composite literal over its
// fields) from a scalar one.
func (l *LogFixtureSource) knownStruct(name string) bool {
	_, ok := l.Models.Structs[name]
	return ok
}

// jsonSliceLit renders a logged JSON array as a Go slice literal over elem,
// or reports false when the logged elements are not of that type.
func (l *LogFixtureSource) jsonSliceLit(raw json.RawMessage, elem string) (string, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", false
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if !l.knownStruct(elem) {
			// A scalar element: the logged JSON must actually be one, or the
			// value would not compile against the declared type.
			prim, ok := logPrimitiveOf(item)
			if !ok {
				return "", false
			}
			switch elem {
			case "string":
				if prim.Kind != "string" {
					return "", false
				}
				parts = append(parts, strconv.Quote(prim.Text))
			case "int", "int8", "int16", "int32", "int64",
				"uint", "uint8", "uint16", "uint32", "uint64",
				"float32", "float64":
				if prim.Kind != "number" {
					return "", false
				}
				parts = append(parts, prim.Text)
			case "bool":
				if prim.Kind != "bool" {
					return "", false
				}
				parts = append(parts, prim.Text)
			default:
				return "", false
			}
			continue
		}
		// A struct element: map the logged object onto the model's fields by
		// json tag, the same mapping the scalar path uses, so a nested request
		// stays honest about what it actually logged.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return "", false
		}
		lit, ok := l.jsonStructLit(obj, elem, 0)
		if !ok {
			return "", false
		}
		parts = append(parts, lit)
	}
	if len(parts) == 0 {
		return "", false
	}
	return "[]" + l.assumed().qualified(elem) + "{" + strings.Join(parts, ", ") + "}", true
}

// jsonStructLit renders a logged JSON object as a composite literal over the
// model's fields, falling back to the assumed placeholder per field the log
// does not carry.
func (l *LogFixtureSource) jsonStructLit(obj map[string]json.RawMessage, structName string, depth int) (string, bool) {
	if depth > 2 {
		return "", false
	}
	fields := l.Models.Structs[structName]
	if len(fields) == 0 {
		return "", false
	}
	assumed := l.assumed()
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f.Name+": "+l.jsonFieldLit(obj, f, depth))
	}
	return assumed.qualified(structName) + "{" + strings.Join(parts, ", ") + "}", true
}

// jsonFieldLit renders one field's value from a logged object, falling back to
// the assumed literal when the log does not carry it.
func (l *LogFixtureSource) jsonFieldLit(obj map[string]json.RawMessage, f fieldInfo, depth int) string {
	if f.JSON == "" {
		return l.assumed().fieldLiteralOf(f, depth)
	}
	raw, ok := pickJSONKey(obj, jsonBase(f.JSON))
	if !ok {
		return l.assumed().fieldLiteralOf(f, depth)
	}
	elem := unqualify(strings.TrimPrefix(f.Type, "[]"))
	switch {
	case strings.HasPrefix(f.Type, "[]") && elem == "string":
		var items []string
		if err := json.Unmarshal(raw, &items); err == nil {
			quoted := make([]string, len(items))
			for i, s := range items {
				quoted[i] = strconv.Quote(s)
			}
			return "[]string{" + strings.Join(quoted, ", ") + "}"
		}
	case strings.HasPrefix(f.Type, "[]"):
		if lit, ok := l.jsonSliceLit(raw, elem); ok {
			return lit
		}
	case f.Type == "string":
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "string" {
			return strconv.Quote(prim.Text)
		}
	}
	return l.assumed().fieldLiteralOf(f, depth)
}

// ResponseFieldValues implements ResponseFieldSource: response values from
// the logged SuccessJSON data / responseBody.
func (l *LogFixtureSource) ResponseFieldValues(structName string) [][2]string {
	return l.fieldValues(structName, true)
}

// FailedRequestValues renders the failed trace's request values for a struct,
// falling back per field to the success trace (the controller's logged-error
// case can carry a different request than its success case).
func (l *LogFixtureSource) FailedRequestValues(structName string) [][2]string {
	if l == nil || l.vals == nil || l.vals.failed == nil {
		return nil
	}
	raw := requestJSON(l.vals.failed.RequestBody)
	if raw == nil {
		return nil
	}
	assumed := l.assumed()
	// FieldValues allocates a fresh [][2]string over every field of the
	// struct (running the validated-body derivation per field). It depends
	// only on structName, so hoist it out of the field loop and index it —
	// the old linear re-scan per field was O(fields²) with one allocation
	// per outer iteration.
	assumedByField := map[string][2]string{}
	for _, fv := range assumed.FieldValues(structName) {
		if _, seen := assumedByField[fv[0]]; !seen {
			assumedByField[fv[0]] = fv
		}
	}
	out := make([][2]string, 0, len(l.Models.Structs[structName]))
	for _, f := range l.Models.Structs[structName] {
		if f.JSON == "" {
			continue
		}
		if r, ok := pickJSONKey(raw, jsonBase(f.JSON)); ok {
			if prim, ok := logPrimitiveOf(r); ok {
				out = append(out, [2]string{f.Name, goStringBody(prim.Text)})
				continue
			}
		}
		if fv, ok := assumedByField[f.Name]; ok {
			out = append(out, fv)
		}
	}
	return out
}

// ZeroExpr implements FixtureSource: zeros are shape, not values — the
// assumed source supplies them.
func (l *LogFixtureSource) ZeroExpr(scalar string) string {
	return l.assumed().ZeroExpr(scalar)
}

// fieldValues maps logged json values onto the struct's json-tagged fields,
// falling back per field to the assumed placeholder.
func (l *LogFixtureSource) fieldValues(structName string, response bool) [][2]string {
	assumed := l.assumed()
	if l == nil || l.vals == nil {
		return assumed.FieldValues(structName)
	}
	values := l.vals.req
	if response {
		values = l.vals.resp
	}
	out := make([][2]string, 0, len(l.Models.Structs[structName]))
	for _, f := range l.Models.Structs[structName] {
		if f.JSON == "" {
			continue
		}
		key := jsonBase(f.JSON)
		if raw, ok := pickJSONKey(values, key); ok {
			if prim, ok := logPrimitiveOf(raw); ok {
				// A logged value was accepted by the service's own binder, so
				// it already satisfies the validator. Only the synthesized
				// fallback below has to be chosen against the tag.
				out = append(out, [2]string{f.Name, goStringBody(prim.Text)})
				continue
			}
		}
		for _, fv := range assumed.FieldValues(structName) {
			if fv[0] == f.Name {
				out = append(out, fv)
				break
			}
		}
	}
	// Fields whose JSON tag is empty were skipped above; merge any assumed
	// pair the loop above did not reach (defensive: struct name unknown to
	// the assumed source yields nothing either).
	if len(out) == 0 {
		return assumed.FieldValues(structName)
	}
	return out
}

// fieldRawForColumn finds the row field carrying the db column tag and
// returns its logged raw value.
func (l *LogFixtureSource) fieldRawForColumn(col string) (json.RawMessage, bool) {
	for _, name := range sortedStructNames(l.Models.Structs) {
		for _, f := range l.Models.Structs[name] {
			if !strings.EqualFold(f.DB, col) {
				continue
			}
			if raw, ok := l.vals.row[f.Name]; ok {
				return raw, true
			}
			if raw, ok := l.vals.row[col]; ok {
				return raw, true
			}
		}
	}
	return nil, false
}

func sortedStructNames(structs map[string][]fieldInfo) []string {
	names := make([]string, 0, len(structs))
	for name := range structs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---- logged value helpers ------------------------------------------------

// logPrimitive is one scalar extracted from a logged JSON value.
type logPrimitive struct {
	Text string // unquoted text: string content, number text, true/false
	Kind string // string | number | bool
}

// logPrimitiveOf unwraps a logged value into a scalar: `"abc"`, `3`, `true`,
// or a database/sql Null* object ({"String":"abc","Valid":true}).
func logPrimitiveOf(raw json.RawMessage) (logPrimitive, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return logPrimitive{}, false
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return logPrimitive{}, false
		}
		return logPrimitive{Text: s, Kind: "string"}, true
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return logPrimitive{}, false
		}
		return logPrimitive{Text: strconv.FormatBool(b), Kind: "bool"}, true
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return logPrimitive{}, false
		}
		for _, key := range []string{"String", "Int64", "Int32", "Int16", "Float64", "Bool", "Time"} {
			if inner, ok := m[key]; ok {
				if prim, ok := logPrimitiveOf(inner); ok {
					return prim, true
				}
			}
		}
		return logPrimitive{}, false
	default:
		if _, err := strconv.ParseFloat(trimmed, 64); err != nil {
			return logPrimitive{}, false
		}
		return logPrimitive{Text: trimmed, Kind: "number"}, true
	}
}

// colExprFromRaw renders the Go literal for one db-tagged model field from
// its logged raw value; "" = cannot render (caller falls back).
func colExprFromRaw(typ string, raw json.RawMessage) string {
	switch typ {
	case "sql.NullString":
		if prim, ok := logPrimitiveOf(raw); ok {
			return fmt.Sprintf("sql.NullString{String: %q, Valid: true}", prim.Text)
		}
	case "sql.NullInt64", "sql.NullInt32", "sql.NullInt16":
		field := strings.TrimPrefix(typ, "sql.Null")
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "number" {
			return fmt.Sprintf("%s{%s: %s, Valid: true}", typ, field, prim.Text)
		}
	case "sql.NullFloat64":
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "number" {
			return fmt.Sprintf("sql.NullFloat64{Float64: %s, Valid: true}", prim.Text)
		}
	case "sql.NullBool":
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "bool" {
			return fmt.Sprintf("sql.NullBool{Bool: %s, Valid: true}", prim.Text)
		}
	case "string":
		if prim, ok := logPrimitiveOf(raw); ok {
			return fmt.Sprintf("%q", prim.Text)
		}
	case "bool":
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "bool" {
			return prim.Text
		}
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64":
		if prim, ok := logPrimitiveOf(raw); ok && prim.Kind == "number" {
			return prim.Text
		}
	}
	return ""
}

// primitiveExpect renders a scalar expectation from a logged value using the
// method's resolved return type (bool flag reads infer from the logged
// number, the corpus's exists-check convention).
func primitiveExpect(typ string, raw json.RawMessage) (string, bool) {
	if strings.HasPrefix(typ, "sql.") {
		if lit := colExprFromRaw(typ, raw); lit != "" {
			return lit, true
		}
		return "", false
	}
	prim, ok := logPrimitiveOf(raw)
	if !ok {
		return "", false
	}
	switch typ {
	case "string":
		return strconv.Quote(prim.Text), true
	case "bool":
		switch prim.Kind {
		case "bool":
			return prim.Text, true
		case "number":
			return strconv.FormatBool(prim.Text != "0"), true
		case "string":
			switch prim.Text {
			case "true", "false":
				return prim.Text, true
			case "1":
				return "true", true
			case "0":
				return "false", true
			}
		}
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		if prim.Kind == "number" {
			return prim.Text, true
		}
		if prim.Kind == "string" {
			if _, err := strconv.ParseInt(prim.Text, 10, 64); err == nil {
				return prim.Text, true
			}
		}
	case "float32", "float64":
		if prim.Kind == "number" {
			return prim.Text, true
		}
		if prim.Kind == "string" {
			if _, err := strconv.ParseFloat(prim.Text, 64); err == nil {
				return prim.Text, true
			}
		}
	}
	return "", false
}

// requestJSON parses a logged requestBody (a JSON string inside the log's
// JSON) into raw field values; nil on any parse miss.
func requestJSON(body string) map[string]json.RawMessage {
	body = strings.TrimSpace(body)
	if body == "" || body[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil
	}
	return m
}

// responseDataRaw returns the trace's logged success payload: the
// SuccessJSON `data` value, falling back to responseBody's `data` member.
func responseDataRaw(success, failed *LogTrace) (json.RawMessage, bool) {
	for _, t := range []*LogTrace{success, failed} {
		if t == nil {
			continue
		}
		if len(t.Data) > 0 {
			return t.Data, true
		}
		if raw, ok := responseBodyData(t.ResponseBody); ok {
			return raw, true
		}
	}
	return nil, false
}

func responseBodyData(body string) (json.RawMessage, bool) {
	body = strings.TrimSpace(body)
	if body == "" || body[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, false
	}
	data, ok := m["data"]
	return data, ok
}

// responseData maps the logged success payload onto a raw field map (first
// array element for list responses).
func responseData(t *LogTrace) (map[string]json.RawMessage, bool) {
	raw, ok := responseDataRaw(t, nil)
	if !ok {
		return nil, false
	}
	return rowRawOf(raw)
}

// firstDBValue finds the first valued db call for the method and classifies
// it: a row object (or first array element) versus a scalar result.
func firstDBValue(t *LogTrace, method string) (row map[string]json.RawMessage, scalar json.RawMessage, hasRow, hasScalar bool) {
	for _, call := range t.DBCalls(method) {
		if !call.HasVal {
			continue
		}
		if m, ok := rowRawOf(call.Raw); ok {
			return m, nil, true, false
		}
		if _, ok := logPrimitiveOf(call.Raw); ok {
			return nil, call.Raw, false, true
		}
	}
	return nil, nil, false, false
}

// rowRawOf normalizes a logged db result into raw row fields: an object is
// returned as-is unless it is a database/sql Null* wrapper; an array takes
// its first object element.
func rowRawOf(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false
	}
	switch trimmed[0] {
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
			return nil, false
		}
		return rowRawOf(list[0])
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, false
		}
		if isNullWrapper(m) {
			return nil, false
		}
		return m, true
	}
	return nil, false
}

// isNullWrapper reports whether an object is a database/sql Null* value
// rather than a row struct ({"String":"x","Valid":true}).
func isNullWrapper(m map[string]json.RawMessage) bool {
	if len(m) == 0 {
		return false
	}
	for key := range m {
		switch key {
		case "String", "Int64", "Int32", "Int16", "Int", "Float64", "Bool", "Time", "Valid":
		default:
			return false
		}
	}
	return true
}

// jsonBase strips tag options (",omitempty") from a json tag.
func jsonBase(tag string) string {
	if i := strings.Index(tag, ","); i >= 0 {
		return tag[:i]
	}
	return tag
}

// pickJSONKey looks a json key up case-sensitively first, then
// case-insensitively (log producers are consistent, but tags drift).
func pickJSONKey(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if m == nil {
		return nil, false
	}
	if raw, ok := m[key]; ok {
		return raw, true
	}
	for k, raw := range m {
		if strings.EqualFold(k, key) {
			return raw, true
		}
	}
	return nil, false
}

// goStringBody escapes text for embedding inside a double-quoted Go string
// literal (the templates insert case values between quotes).
func goStringBody(s string) string {
	if !strings.ContainsAny(s, "\\\"\n\r\t") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- renderer helpers ------------------------------------------------------

// loggedDBRow renders the mock-row values for one logged db call, falling
// back per field to the assumed placeholder.
func loggedDBRow(sc *serviceCtx, rowType string, call LogDBCall) ([]string, bool) {
	base := structBase(rowType)
	assumed := &AssumedFixtureSource{Models: sc.models}
	m, hasRow := rowRawOf(call.Raw)
	var out []string
	for _, f := range sc.models.Structs[base] {
		if f.DB == "" {
			continue
		}
		val := assumed.RowValues([]string{f.DB})[0]
		if hasRow {
			raw, ok := m[f.Name]
			if !ok {
				raw, ok = m[f.DB]
			}
			if ok {
				if prim, ok := logPrimitiveOf(raw); ok {
					val = goStringBody(prim.Text)
				}
			}
		}
		out = append(out, val)
	}
	return out, len(out) > 0
}

// loggedDBExpect renders the expected output literal for one logged db call.
func loggedDBExpect(sc *serviceCtx, f *dbFact, call LogDBCall) (string, bool) {
	if f.RowType == "" {
		if lit, ok := primitiveExpect(dbExpectType(f), call.Raw); ok {
			return lit, true
		}
		return "", false
	}
	rowType := f.RowType
	switch f.Shape {
	case "multi":
		rowType = "[]*" + f.RowType
	case "single":
		rowType = "*" + f.RowType
	}
	return loggedRowLiteral(sc, rowType, call)
}

// loggedScalarRow renders the mock-row value for a logged scalar result
// (scan-compatible primitive text).
func loggedScalarRow(call LogDBCall) (string, bool) {
	prim, ok := logPrimitiveOf(call.Raw)
	if !ok {
		return "", false
	}
	return prim.Text, true
}

// loggedReturn renders a gomock Return payload for one store method from a
// logged db call; "" = no logged value (caller falls back).
func loggedReturn(sc *serviceCtx, method string, call LogDBCall) string {
	df := sc.dbFacts.DB[method]
	if df == nil || !call.HasVal {
		return ""
	}
	// Every shape below used to spell out `[]any{<lit>, nil}` itself, four
	// copies of the same arity assumption inside one function. They go through
	// storeReturnLiteral so the declared result count decides the shape — which
	// is what GetDB() *sqlx.DB needs, since the log route was still handing it
	// two elements.
	switch df.Shape {
	case "multi", "single", "scalar":
		if lit, ok := loggedDBExpect(sc, df, call); ok {
			return storeReturnLiteral(sc, method, lit, "nil")
		}
	case "dml":
		return storeReturnLiteral(sc, method, "nil", "nil")
	}
	return ""
}

// loggedScalarResponseLiteral renders a scalar response expectation from the
// method's logged success payload; "" when unusable.
func loggedScalarResponseLiteral(responseType string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if strings.HasPrefix(responseType, "*") || strings.HasPrefix(responseType, "[]") {
		return ""
	}
	if prim, ok := logPrimitiveOf(raw); ok {
		if lit, ok := primitiveExpect(responseType, json.RawMessage(strconv.Quote(prim.Text))); ok {
			return lit
		}
	}
	return ""
}

// loggedRowLiteral renders a row-struct literal for one logged db call,
// falling back per missing field to the assumed fixture value.
func loggedRowLiteral(sc *serviceCtx, rowType string, call LogDBCall) (string, bool) {
	base := structBase(rowType)
	m, ok := rowRawOf(call.Raw)
	if !ok {
		return "", false
	}
	var fields []string
	for _, fl := range sc.models.Structs[base] {
		if fl.DB == "" {
			continue
		}
		raw, found := m[fl.Name]
		if !found {
			raw, found = m[fl.DB]
		}
		lit := ""
		if found {
			lit = colExprFromRaw(fl.Type, raw)
		}
		if lit == "" {
			lit = sc.fixtures.ColExpr(fl.DB, fl.Type)
		}
		fields = append(fields, fl.Name+": "+lit)
	}
	joined := strings.Join(fields, ", ")
	switch {
	case strings.HasPrefix(rowType, "[]*"):
		return rowType + "{{" + joined + "}}", true
	case strings.HasPrefix(rowType, "*"):
		return "&" + rowType[1:] + "{" + joined + "}", true
	default:
		return rowType + "{" + joined + "}", true
	}
}

// isScalarType reports whether a Go type name renders from the ZeroExpr
// table (a non-struct response type).
func isScalarType(t string) bool {
	switch t {
	case "string", "bool",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64",
		"sql.NullString", "sql.NullInt64", "sql.NullInt32", "sql.NullInt16",
		"sql.NullBool", "sql.NullFloat64", "sql.NullTime", "time.Time":
		return true
	}
	return false
}
