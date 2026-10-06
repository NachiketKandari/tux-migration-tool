package gen

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/sqltext"
)

// canonicalAliasID resolves a namespaced query key to its canonical alias
// identity: dedup duplicates (q5 → DuplicateOf=q3) share the canonical
// unit's alias set so both scenario arms carry one deterministic name
// group. The key is the plan's namespaced query ID (`q1` for main-file
// queries, `fn:<name>:<qID>` for fn files) — raw q.ID alone collides across
// files (the main q1 and every fn q1). Pure function — worker-pool safe.
func canonicalAliasID(queryKey string, q *ir.Query) string {
	if q.DuplicateOf != "" {
		if j := strings.LastIndex(queryKey, ":"); j >= 0 {
			return queryKey[:j+1] + q.DuplicateOf
		}
		return q.DuplicateOf
	}
	return queryKey
}

// oracleBareFuncs are zero-argument Oracle pseudo-functions that parse as a
// lone identifier but never name a table column (SELECT sysdate FROM dual
// returns SYSDATE, never the INTO host var). They alias like any computed
// item so the row scan sees a sanctioned name.
var oracleBareFuncs = map[string]bool{
	"SYSDATE": true, "SYSTIMESTAMP": true, "CURRENT_DATE": true,
	"CURRENT_TIMESTAMP": true, "LOCALTIMESTAMP": true, "USER": true,
	"ROWNUM": true, "ROWID": true, "UID": true,
}

// columnAliases computes the deterministic computed-column alias set for one
// query: 0-based select-list ordinal → alias. queryKey is the plan's
// namespaced query ID (unit.QueryIDs[0]). Only SELECT single/multi
// queries with a row shape participate; scalar COUNT queries and DML never
// do. The alignment guard requires len(selectItems) == len(RowShape) — the
// INTO list is positional — otherwise injection is skipped and the returned
// warning explains loudly, never silently.
func (s *Service) columnAliases(queryKey string, q *ir.Query) (map[int]string, string) {
	if q.Type != ir.QuerySelectSingle && q.Type != ir.QuerySelectMulti {
		return nil, ""
	}
	if isCountQuery(q) {
		return nil, ""
	}
	if len(q.RowShape) == 0 {
		return nil, ""
	}
	items := sqltext.SelectItems(sqltext.CanonicalSQL(q.SQL))
	if len(items) != len(q.RowShape) {
		return nil, fmt.Sprintf("gen: query %s selects %d items for %d INTO hosts — computed-column aliases skipped (positional INTO requires equal counts)", q.ID, len(items), len(q.RowShape))
	}
	service := ""
	if s.Mapping != nil {
		service = s.Mapping.Service
	}
	canon := canonicalAliasID(queryKey, q)
	out := map[int]string{}
	for i, item := range items {
		needs := sqltext.IsComputedItem(item)
		if !needs {
			// Bare-column mismatch: the expression names one column while
			// the INTO host names another (`sysdate` into :c_to_date).
			// Without an alias Oracle returns the expression name and the
			// scan fails. Only the known Oracle pseudo-functions alias
			// here — any other bare mismatch is a mapping-shape problem
			// the pipeline flags elsewhere; silently renaming it would
			// hide the mismatch instead of surfacing it. Matching bare
			// columns keep today's tag derivation (zero golden churn).
			if bare, ok := bareIdentOf(item); ok {
				if hostTagOf(q.RowShape[i]) != strings.ToUpper(lastSegment(bare)) && oracleBareFuncs[strings.ToUpper(lastSegment(bare))] {
					needs = true
				}
			}
		}
		if !needs {
			continue
		}
		out[i] = sqltext.AliasFor(service, canon, i+1)
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// bareIdentOf returns the dotted identifier text of a lone select item
// (`col`, `t.col`), false for anything else.
func bareIdentOf(item string) (string, bool) {
	t := strings.TrimSpace(item)
	for _, kw := range []string{"DISTINCT ", "ALL ", "UNIQUE "} {
		if len(t) > len(kw) && strings.EqualFold(t[:len(kw)], kw) {
			t = strings.TrimSpace(t[len(kw):])
			break
		}
	}
	if t == "" || t == "*" || strings.HasSuffix(t, ".*") {
		return "", false
	}
	parts := strings.Split(t, ".")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || !isAliasIdentStart(p[0]) {
			return "", false
		}
		for i := 1; i < len(p); i++ {
			if !isAliasIdentByte(p[i]) && p[i] != '$' && p[i] != '#' {
				return "", false
			}
		}
	}
	if strings.ContainsAny(t, " \t\n\r(),;") {
		return "", false
	}
	return t, true
}

func isAliasIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isAliasIdentByte(b byte) bool {
	return isAliasIdentStart(b) || (b >= '0' && b <= '9')
}

// hostTagOf derives the bare-column db tag for one INTO host var — the same
// uppercased sql_-stripped derivation rowFields uses.
func hostTagOf(hvName string) string {
	if j := strings.LastIndex(hvName, "."); j >= 0 {
		hvName = hvName[j+1:]
	}
	if j := strings.IndexAny(hvName, " \t"); j > 0 {
		hvName = hvName[:j]
	}
	return strings.ToUpper(strings.TrimPrefix(hvName, "sql_"))
}

// lastSegment returns the text after the final dot (`t.col` → `col`).
func lastSegment(bare string) string {
	if j := strings.LastIndex(bare, "."); j >= 0 {
		return bare[j+1:]
	}
	return bare
}

// isBareMatch reports whether a bare select identifier names the same column
// as the host var (case-insensitive, last-dot-segment on both sides).
func isBareMatch(bare, hvName string) bool {
	b := bare
	if j := strings.LastIndex(b, "."); j >= 0 {
		b = b[j+1:]
	}
	h := hvName
	if j := strings.LastIndex(h, "."); j >= 0 {
		h = h[j+1:]
	}
	if j := strings.IndexAny(h, " \t"); j > 0 {
		h = h[:j]
	}
	h = strings.TrimPrefix(h, "sql_")
	return strings.EqualFold(b, h)
}

// AliasReport aggregates the alias sets of every db unit in the plan: the
// total aliased-column count, the alignment-skip warnings, and the per-unit
// item → alias trail for the sql-aliases.json audit record.
func (s *Service) AliasReport(p *plan.Plan) (count int, warnings []string, trail map[string]map[string]string) {
	trail = map[string]map[string]string{}
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil {
			continue
		}
		aliases, warn := s.columnAliases(u.QueryIDs[0], q)
		if warn != "" {
			warnings = append(warnings, u.Name+": "+warn)
		}
		if len(aliases) == 0 {
			continue
		}
		items := sqltext.SelectItems(sqltext.CanonicalSQL(q.SQL))
		per := map[string]string{}
		for pos, alias := range aliases {
			item := ""
			if pos >= 0 && pos < len(items) {
				item = items[pos]
			}
			per[alias] = item
			count++
		}
		trail[u.Name] = per
	}
	return count, warnings, trail
}
