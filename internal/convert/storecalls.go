package convert

import (
	"regexp"
	"strconv"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/gen"
	"tux-migration-tool/internal/ir"
)

// Store-call capture rendering (deterministic view shaping): ReplaceQueries
// inserts bare calls (`s.store.UpdateDummyProfile(c, tx, …)`), and models
// copy that shape — the following `if err != nil` then checks a stale err.
// The gates reject a discarded result, but the view should teach the
// accepted shape up front: error-only calls become `err = s.store.X(…)`,
// row/single-value calls become `<capture>, err := s.store.X(…)` with a
// deterministic per-view capture name.

// storeCallShapes classifies each store method's return shape from its query
// type: DML → error-only, select-single → single value, else a row slice.
func storeCallShapes(svc *gen.Service, calls map[string]budget.DBCall) map[string]string {
	out := make(map[string]string, len(calls))
	for id, call := range calls {
		q := svc.Query(id)
		if q == nil || call.Name == "" {
			continue
		}
		switch {
		case q.Type.IsDML():
			out[call.Name] = "error"
		case q.Type == ir.QuerySelectSingle:
			out[call.Name] = "single"
		default:
			out[call.Name] = "rows"
		}
	}
	return out
}

// assignStoreCalls rewrites whole-line bare store calls into the captured
// shape. Duplicate calls to one method gain a numeric suffix; lines whose
// parens do not close on the line stay untouched.
func assignStoreCalls(src, receiver string, shape map[string]string) (string, int) {
	if len(shape) == 0 {
		return src, 0
	}
	recv := strings.TrimSuffix(receiver, ".")
	if recv == "" {
		recv = "s.store"
	}
	callRe := common.CachedRegexp(`^(\s*)` + regexp.QuoteMeta(recv) + `\.([A-Za-z0-9_]+)\(`)
	used := map[string]int{}
	lines := strings.Split(src, "\n")
	changed := 0
	for i, line := range lines {
		m := callRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[2]
		kind, ok := shape[name]
		if !ok {
			continue
		}
		open := strings.Index(line, name+"(") + len(name)
		_, close, balanced := balancedParens(line, open)
		if !balanced || strings.TrimSpace(line[close+1:]) != "" {
			continue
		}
		indent := m[1]
		call := line[len(indent):]
		if kind == "error" {
			lines[i] = indent + "err = " + call
			changed++
			continue
		}
		capture := common.LowerFirst(name)
		if kind == "rows" {
			capture += "Rows"
		}
		if n := used[capture]; n > 0 {
			capture += strconv.Itoa(n + 1)
		}
		used[capture]++
		lines[i] = indent + capture + ", err := " + call
		changed++
	}
	if changed == 0 {
		return src, 0
	}
	return strings.Join(lines, "\n"), changed
}
