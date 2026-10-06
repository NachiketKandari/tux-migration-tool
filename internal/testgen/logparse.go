// GT-7 runtime-log parsing (docs/gt7-log-fixtures-plan.md): gentest reads the
// edited converted app's own runtime log and derives fixture values per
// (service, method). Parsing is line-based and degrade-safe — ANSI escapes are
// stripped, `requestID: null` boot lines and stack-trace continuations are
// skipped, and a single malformed line becomes a warning instead of failing
// the run. Traces are grouped by requestID; the `API Call End` line supplies
// path/status/request/response bodies, the service-layer caller lines supply
// the method inventory and db results, and ERROR lines supply the failed
// cases.
package testgen

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// logLineRe matches one structured runtime log line:
//
//	MM-DD-YYYY HH:MM:SS <ANSI LEVEL> <file:line> <caller> <MSG> <JSON>
//
// The caller is a fully qualified Go package symbol (the app logs its own
// package path), which is what ties a line back to a service method.
var logLineRe = regexp.MustCompile(`^(\d{2}-\d{2}-\d{4} \d{2}:\d{2}:\d{2}) ([A-Z]+) (\S+) (\S+) (.*)$`)

// ansiRe strips CSI color sequences (zap's development console encoder).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// logCallerRe recognizes service callers:
//
//	root-folder/pkg/services/dummyprofile/db.(*store).ViewQuestions
//	root-folder/pkg/services/item/controller.(itemController).ItemList
//
// Service segment and layer are captured for matching; receiver name and
// pointer-ness are deliberately ignored (pointer/value receivers and
// receiver names are equivalent — plan "Caller ↔ code matching").
var logCallerRe = regexp.MustCompile(`pkg/services/([^/]+)/(db|controller|handler)\.(?:\(\*?([A-Za-z0-9_]+)\)|([A-Za-z0-9_]+))\.([A-Za-z0-9_]+)$`)

// LogCaller is one service-layer caller line inside a trace.
type LogCaller struct {
	Service string // service directory segment (case-insensitive match)
	Layer   string // db | controller | handler
	Method  string // function name (case-sensitive)
	Msg     string // message head before the JSON payload
	Level   string // DEBUG | INFO | ERROR | ...
	Line    int    // 1-based log line
}

// LogDBCall is one db-layer call inside a trace. HasVal reports whether the
// line carried a result payload; Key names the payload key (`result:`,
// `QuestionID:`, `Exists:`, …) and Raw is its JSON value.
type LogDBCall struct {
	Method string
	Key    string
	Raw    json.RawMessage
	HasVal bool
	Msg    string
	Line   int
}

// LogError is one ERROR line inside a trace. Caller is zero-valued for
// framework errors that carry no service caller (e.g. the network layer's
// internalServerFailureJSON).
type LogError struct {
	Caller LogCaller
	Msg    string
}

// LogTrace is one requestID's grouped lines.
type LogTrace struct {
	ID           string
	Order        int // first-line order across the log (deterministic)
	Path         string
	HMethod      string
	Status       int
	RequestBody  string
	ResponseBody string
	LatencyMS    int
	End          bool
	Callers      []LogCaller
	DB           map[string][]LogDBCall // db method → calls in line order
	Errors       []LogError
	Data         json.RawMessage // SuccessJSON `data` payload (expected response)
	Lines        int
}

// Complete reports the plan's completeness rule:
// API Call End + ≥1 layer caller.
func (t *LogTrace) Complete() bool { return t != nil && t.End && len(t.Callers) > 0 }

// Successful is the plan's success rule: status == 200 and no ERROR line in
// the trace (regardless of which layer logged it).
func (t *LogTrace) Successful() bool { return t.Complete() && t.Status == 200 && len(t.Errors) == 0 }

// HasCaller reports whether the trace carries a caller for (service, method):
// service segment case-insensitive, method name case-sensitive.
func (t *LogTrace) HasCaller(service, method string) bool {
	for _, c := range t.Callers {
		if strings.EqualFold(c.Service, service) && c.Method == method {
			return true
		}
	}
	return false
}

// ErrorFor returns the first ERROR message logged for (service, method) (any
// layer); ok=false when the trace logged no such error.
func (t *LogTrace) ErrorFor(service, method string) (string, bool) {
	return t.ErrorForLayer(service, "", method)
}

// ErrorForLayer is ErrorFor restricted to one layer when layer != "" — the
// same method name lives in every layer (ViewQuestions is handler,
// controller and db), so method-level error attribution must pin the layer.
func (t *LogTrace) ErrorForLayer(service, layer, method string) (string, bool) {
	for _, e := range t.Errors {
		if e.Caller.Service == "" {
			continue
		}
		if layer != "" && e.Caller.Layer != layer {
			continue
		}
		if strings.EqualFold(e.Caller.Service, service) && e.Caller.Method == method {
			return e.Msg, true
		}
	}
	return "", false
}

// DBCalls returns every recorded db call for the method, in line order.
func (t *LogTrace) DBCalls(method string) []LogDBCall {
	if t == nil {
		return nil
	}
	return t.DB[method]
}

// LogData is one parsed log: traces in first-appearance order plus the
// tolerance counters the plan asks for (skipped lines are counted, never
// fatal).
type LogData struct {
	Path        string
	Traces      []*LogTrace
	Warnings    []string
	Lines       int // total lines read
	SkippedNull int // requestID: null boot/db lines skipped
	StackLines  int // continuation lines (Go stacktraces) tolerated
}

// ParseLogFile reads and parses one runtime log (extension-blind: the plan
// detects by content, not suffix).
func ParseLogFile(path string) (*LogData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gentest: cannot read log file %s: %w", path, err)
	}
	return ParseLog(path, b), nil
}

// ParseLog parses log bytes. name is only used for provenance/warnings.
func ParseLog(name string, data []byte) *LogData {
	d := &LogData{Path: name}
	byID := map[string]*LogTrace{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		d.Lines++
		line := strings.TrimRight(ansiRe.ReplaceAllString(raw, ""), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := logLineRe.FindStringSubmatch(line)
		if m == nil {
			// Multi-line Go stacktraces after ERROR lines (and any other
			// continuation) are tolerated, not warned.
			d.StackLines++
			continue
		}
		level, callerSym, rest := m[2], m[4], m[5]
		msg, payload, ok := splitLogPayload(rest)
		if !ok {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s:%d: unparsable payload", name, lineNo))
			continue
		}
		rid, isStr := payloadRequestID(payload)
		if !isStr {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s:%d: missing requestID", name, lineNo))
			continue
		}
		if rid == "" {
			d.SkippedNull++
			continue
		}
		t := byID[rid]
		if t == nil {
			t = &LogTrace{ID: rid, Order: len(d.Traces), DB: map[string][]LogDBCall{}}
			byID[rid] = t
			d.Traces = append(d.Traces, t)
		}
		t.Lines++
		if strings.Contains(msg, "API Call End") {
			t.End = true
			t.Path = jsonString(payload["path"])
			t.HMethod = jsonString(payload["method"])
			t.RequestBody = jsonString(payload["requestBody"])
			t.ResponseBody = jsonString(payload["responseBody"])
			t.Status = jsonInt(payload["status"])
			t.LatencyMS = jsonInt(payload["latency"])
		}
		if strings.Contains(callerSym, "SuccessJSON") {
			if raw, ok := payload["data"]; ok {
				t.Data = raw
			}
		}
		caller, ok := parseLogCaller(callerSym)
		if !ok {
			if level == "ERROR" {
				t.Errors = append(t.Errors, LogError{Msg: msg})
			}
			continue
		}
		caller.Msg, caller.Level, caller.Line = msg, level, lineNo
		t.Callers = append(t.Callers, caller)
		if caller.Layer == "db" {
			call := LogDBCall{Method: caller.Method, Msg: msg, Line: lineNo}
			if key, raw, ok := logDBValue(payload); ok {
				call.Key, call.Raw, call.HasVal = key, raw, true
			}
			t.DB[caller.Method] = append(t.DB[caller.Method], call)
		}
		if level == "ERROR" {
			t.Errors = append(t.Errors, LogError{Caller: caller, Msg: msg})
		}
	}
	for _, t := range d.Traces {
		for method, calls := range t.DB {
			t.DB[method] = mergeDBCalls(method, calls)
		}
	}
	return d
}

// mergeDBCalls folds a method's consecutive log lines into one call: the
// entry debug line (message == method name) opens a call, a result line
// attaches its payload, and the next entry or the next valued line opens the
// following call. DML calls log only entry + progress messages, repeat-result
// reads (GetMarks) log only payload lines — both come out one-per-call.
func mergeDBCalls(method string, calls []LogDBCall) []LogDBCall {
	var out []LogDBCall
	for _, c := range calls {
		if len(out) == 0 || c.Msg == method || out[len(out)-1].HasVal {
			out = append(out, c)
			continue
		}
		last := &out[len(out)-1]
		if c.HasVal && !last.HasVal {
			last.Key, last.Raw, last.HasVal = c.Key, c.Raw, true
		}
	}
	return out
}

// Trace returns the method's selected trace: for failed=false the first
// successful complete trace, falling back to the first complete trace when
// none succeeded; for failed=true the first complete trace that is not
// successful. nil when no complete trace carries the method.
func (d *LogData) Trace(service, method string, failed bool) *LogTrace {
	var fallback *LogTrace
	for _, t := range d.Traces {
		if !t.HasCaller(service, method) {
			continue
		}
		if failed {
			if t.Complete() && !t.Successful() {
				return t
			}
			continue
		}
		if !t.Complete() {
			continue
		}
		if t.Successful() {
			return t
		}
		if fallback == nil {
			fallback = t
		}
	}
	return fallback
}

// parseLogCaller splits a caller symbol into its service/layer/method parts.
func parseLogCaller(sym string) (LogCaller, bool) {
	m := logCallerRe.FindStringSubmatch(sym)
	if m == nil {
		return LogCaller{}, false
	}
	return LogCaller{Service: m[1], Layer: m[2], Method: m[5]}, true
}

// splitLogPayload separates the message head from the trailing structured
// JSON object. The JSON starts at the first ` {` whose remainder parses —
// messages may themselves contain braces, so each candidate is tried in
// order and the first parse wins.
func splitLogPayload(rest string) (msg string, payload map[string]json.RawMessage, ok bool) {
	for i := 0; i < len(rest); i++ {
		if rest[i] != '{' || (i > 0 && rest[i-1] != ' ') {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(rest[i:]), &m); err == nil {
			return strings.TrimSpace(rest[:i]), m, true
		}
	}
	return "", nil, false
}

// payloadRequestID returns the requestID value and whether the payload
// carried the key at all. `null` yields ("", true) — the boot/db lines the
// plan skips.
func payloadRequestID(payload map[string]json.RawMessage) (string, bool) {
	raw, ok := payload["requestID"]
	if !ok {
		return "", false
	}
	if string(raw) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// logDBValue picks the result-carrying key of a db caller line. The known
// keys win in a fixed order (deterministic); any other single non-control key
// is accepted so method-specific probes (a future `Count:` etc.) still work.
func logDBValue(payload map[string]json.RawMessage) (string, json.RawMessage, bool) {
	for _, key := range []string{"result:", "QuestionID:", "Exists:"} {
		if raw, ok := payload[key]; ok {
			return key, raw, true
		}
	}
	var extra []string
	for k := range payload {
		switch k {
		case "requestID", "matchAccount":
		default:
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return "", nil, false
	}
	sort.Strings(extra)
	return extra[0], payload[extra[0]], true
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func jsonInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// ShortID is the provenance tag for a trace (first 8 hex characters).
func (t *LogTrace) ShortID() string {
	if t == nil {
		return ""
	}
	if len(t.ID) > 8 {
		return t.ID[:8]
	}
	return t.ID
}
