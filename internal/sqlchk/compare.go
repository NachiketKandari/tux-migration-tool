package sqlchk

import (
	"fmt"
	"strings"
)

// clauses carves a normalized token stream into the projections PF-6.3
// compares: the ordered select-list items, the table list, the
// WHERE/GROUP BY/HAVING, SET, INSERT columns/VALUES and ORDER BY token
// sequences (binds masked), plus the raw bind names and canonical ordinals.
type clauses struct {
	selectItems [][]sqlToken
	tables      []string
	where       []sqlToken
	setTail     []sqlToken
	valuesTail  []sqlToken
	orderTail   []sqlToken
	bindNames   []string
	bindOrds    []string
	hasSelect   bool
	hasWhere    bool
	hasSet      bool
	hasValues   bool
	hasOrder    bool
}

type boundary struct {
	kind string
	at   int
}

// boundaries indexes clause keywords at paren depth 0 (group/order require
// their BY).
func boundaries(toks []sqlToken) []boundary {
	var out []boundary
	depth := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == tokPunct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
			}
			continue
		}
		if t.Kind != tokWord || depth != 0 {
			continue
		}
		switch t.Text {
		case "select", "from", "where", "having", "set", "values", "into",
			"insert", "update", "delete":
			out = append(out, boundary{t.Text, i})
		case "group", "order":
			if i+1 < len(toks) && toks[i+1].Kind == tokWord && toks[i+1].Text == "by" {
				out = append(out, boundary{t.Text + "by", i})
				i++
			}
		}
	}
	return out
}

func boundaryAt(bs []boundary, kind string) (int, bool) {
	for _, b := range bs {
		if b.kind == kind {
			return b.at, true
		}
	}
	return 0, false
}

// boundaryEnd returns the exclusive end of the region starting after `at`:
// the next boundary whose kind is one of the stops (all boundaries when no
// stops are listed), or a sentinel past any token index.
func boundaryEnd(bs []boundary, at int, stops ...string) int {
	best := -1
	for _, b := range bs {
		if b.at > at && (best < 0 || b.at < best) {
			matched := false
			for _, s := range stops {
				if b.kind == s {
					matched = true
					break
				}
			}
			if matched || len(stops) == 0 {
				best = b.at
			}
		}
	}
	if best < 0 {
		return 1 << 30
	}
	return best
}

func clausesOf(toks []sqlToken) clauses {
	bs := boundaries(toks)
	var c clauses
	for _, t := range toks {
		if t.Kind == tokBind {
			c.bindNames = append(c.bindNames, strings.ToLower(strings.TrimPrefix(t.Raw, ":")))
			c.bindOrds = append(c.bindOrds, t.Text)
		}
	}

	selAt, hasSel := boundaryAt(bs, "select")
	fromAt, hasFrom := boundaryAt(bs, "from")
	if hasSel {
		c.hasSelect = true
		end := len(toks)
		if hasFrom && fromAt > selAt {
			end = fromAt
		}
		c.selectItems = selectItems(toks[selAt+1 : end])
	}
	if hasFrom {
		end := boundaryEnd(bs, fromAt, "where", "groupby", "having", "orderby", "set", "for")
		c.tables = tablesIn(toks[fromAt:min(end, len(toks))])
	}
	if uAt, ok := boundaryAt(bs, "update"); ok {
		if uAt+1 < len(toks) && toks[uAt+1].Kind == tokWord {
			c.tables = append([]string{toks[uAt+1].Text}, c.tables...)
		}
	}
	if wAt, ok := boundaryAt(bs, "where"); ok {
		c.hasWhere = true
		var segs [][]sqlToken
		end := boundaryEnd(bs, wAt, "groupby", "having", "orderby", "for")
		segs = append(segs, toks[wAt+1:min(end, len(toks))])
		if gAt, ok := boundaryAt(bs, "groupby"); ok && gAt > wAt {
			end := boundaryEnd(bs, gAt, "having", "orderby", "for")
			segs = append(segs, toks[gAt+2:min(end, len(toks))])
		}
		if hAt, ok := boundaryAt(bs, "having"); ok {
			end := boundaryEnd(bs, hAt, "orderby", "for")
			segs = append(segs, toks[hAt+1:min(end, len(toks))])
		}
		for _, s := range segs {
			c.where = append(c.where, maskBinds(s)...)
		}
	}
	if sAt, ok := boundaryAt(bs, "set"); ok {
		if uAt, ok := boundaryAt(bs, "update"); ok && uAt < sAt {
			c.hasSet = true
			end := boundaryEnd(bs, sAt, "where", "orderby")
			c.setTail = maskBinds(toks[sAt+1 : min(end, len(toks))])
		}
	}
	if iAt, ok := boundaryAt(bs, "into"); ok {
		if insAt, ok := boundaryAt(bs, "insert"); ok && insAt < iAt {
			if iAt+1 < len(toks) && toks[iAt+1].Kind == tokWord {
				c.tables = append(c.tables, toks[iAt+1].Text) // the INSERT target
			}
			c.hasValues = true
			start := iAt + 1
			if start < len(toks) && toks[start].Kind == tokWord {
				start++ // past the target table; its identity is the tables projection
			}
			c.valuesTail = maskBinds(toks[start:])
		}
	}
	if oAt, ok := boundaryAt(bs, "orderby"); ok {
		c.hasOrder = true
		c.orderTail = maskBinds(toks[oAt+2:])
	}
	return c
}

// tablesIn collects the table names of a FROM region: every word directly
// after from/join or a comma (subquery FROMs included — a drifted inner
// table must flag).
func tablesIn(toks []sqlToken) []string {
	var out []string
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if t.Kind != tokWord {
			continue
		}
		prev := toks[i-1]
		if (prev.Kind == tokWord && (prev.Text == "from" || prev.Text == "join")) ||
			(prev.Kind == tokPunct && prev.Text == ",") {
			out = append(out, t.Text)
		}
	}
	return out
}

// selectItems splits a select region at depth-0 commas and strips each
// item's trailing column alias (`AS "X"`, `AS x`, bare `"X"`, bare `x`) —
// alias names may differ, the compared expression may not (PF-6.2).
func selectItems(toks []sqlToken) [][]sqlToken {
	var items [][]sqlToken
	depth := 0
	start := 0
	for i := 0; i < len(toks); i++ {
		if toks[i].Kind == tokPunct {
			switch toks[i].Text {
			case "(":
				depth++
			case ")":
				depth--
			case ",":
				if depth == 0 {
					items = appendItem(items, toks[start:i])
					start = i + 1
				}
			}
		}
	}
	items = appendItem(items, toks[start:])
	return items
}

func appendItem(items [][]sqlToken, item []sqlToken) [][]sqlToken {
	if len(item) == 0 {
		return items
	}
	return append(items, stripSelectAlias(item))
}

// aliasEligible reports whether prev can precede a bare trailing alias:
// an expression end (word/number/quoted ident/closing paren) — never a
// comma, dot or operator (those make the last word part of the expression).
func aliasEligible(prev sqlToken) bool {
	switch prev.Kind {
	case tokWord, tokNum, tokQIdent:
		return true
	case tokPunct:
		return prev.Text == ")"
	}
	return false
}

func stripSelectAlias(item []sqlToken) []sqlToken {
	n := len(item)
	if n < 2 {
		return item
	}
	last, prev := item[n-1], item[n-2]
	lastIsAlias := last.Kind == tokQIdent || (last.Kind == tokWord && !isStopWord(last))
	if prev.Kind == tokWord && prev.Text == "as" && lastIsAlias {
		return item[:n-2]
	}
	if lastIsAlias && aliasEligible(prev) {
		return item[:n-1]
	}
	return item
}

func maskBinds(toks []sqlToken) []sqlToken {
	out := make([]sqlToken, len(toks))
	for i, t := range toks {
		if t.Kind == tokBind {
			t.Text = ":b"
			t.Raw = "" // the masked stream compares structure, not bind spelling
		}
		out[i] = t
	}
	return out
}

// Compare checks generated SQL against the source SQL — both sides through
// the same normalizer — and returns the typed deviations; empty means
// fidelity (PF-6.3).
func Compare(source, generated string) []Deviation {
	var devs []Deviation
	sb, gb := clausesOf(normalizeSQL(source)), clausesOf(normalizeSQL(generated))

	if d := diffBinds(sb, gb); d != "" {
		devs = append(devs, Deviation{Kind: DevBinds, Detail: d})
	}
	if sb.hasSelect || gb.hasSelect {
		switch {
		case !sb.hasSelect:
			devs = append(devs, Deviation{Kind: DevColumns, Detail: "generated has a SELECT list, source does not"})
		case !gb.hasSelect:
			devs = append(devs, Deviation{Kind: DevColumns, Detail: "source has a SELECT list, generated does not"})
		default:
			if d := diffItems(sb.selectItems, gb.selectItems); d != "" {
				devs = append(devs, Deviation{Kind: DevColumns, Detail: d})
			}
		}
	}
	if d := diffTables(sb.tables, gb.tables); d != "" {
		devs = append(devs, Deviation{Kind: DevTables, Detail: d})
	}
	switch {
	case sb.hasWhere != gb.hasWhere:
		devs = append(devs, Deviation{Kind: DevWhere, Detail: "WHERE clause presence differs"})
	case sb.hasWhere:
		if d := diffToks(sb.where, gb.where, "where"); d != "" {
			devs = append(devs, Deviation{Kind: DevWhere, Detail: d})
		}
	}
	switch {
	case sb.hasSet != gb.hasSet:
		devs = append(devs, Deviation{Kind: DevSet, Detail: "SET clause presence differs"})
	case sb.hasSet:
		if d := diffToks(sb.setTail, gb.setTail, "set"); d != "" {
			devs = append(devs, Deviation{Kind: DevSet, Detail: d})
		}
	}
	switch {
	case sb.hasValues != gb.hasValues:
		devs = append(devs, Deviation{Kind: DevValues, Detail: "INSERT columns/VALUES presence differs"})
	case sb.hasValues:
		if d := diffToks(sb.valuesTail, gb.valuesTail, "insert columns/values"); d != "" {
			devs = append(devs, Deviation{Kind: DevValues, Detail: d})
		}
	}
	switch {
	case sb.hasOrder != gb.hasOrder:
		devs = append(devs, Deviation{Kind: DevOrder, Detail: "ORDER BY presence differs"})
	case sb.hasOrder:
		if d := diffToks(sb.orderTail, gb.orderTail, "order by"); d != "" {
			devs = append(devs, Deviation{Kind: DevOrder, Detail: d})
		}
	}
	return devs
}

// diffBinds compares the bind projection (PF-6.2): matching styles compare
// the raw bind spellings positionally (a swap or a rename is a real drift);
// a style change (:1 ↔ :sql_x) falls back to first-appearance ordinals so
// only count/order changes flag.
func diffBinds(sb, gb clauses) string {
	if len(sb.bindNames) == 0 && len(gb.bindNames) == 0 {
		return ""
	}
	if hasPositional(sb.bindNames) == hasPositional(gb.bindNames) {
		if equalStrings(sb.bindNames, gb.bindNames) {
			return ""
		}
		return fmt.Sprintf("binds differ: source %v vs generated %v", sb.bindNames, gb.bindNames)
	}
	if !equalStrings(sb.bindOrds, gb.bindOrds) {
		return fmt.Sprintf("bind sequence differs: source %v vs generated %v", sb.bindOrds, gb.bindOrds)
	}
	return ""
}

func hasPositional(names []string) bool {
	for _, n := range names {
		if n != "" && isDigits(n) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

func diffItems(a, b [][]sqlToken) string {
	if len(a) != len(b) {
		return fmt.Sprintf("select list has %d columns vs %d", len(a), len(b))
	}
	for i := range a {
		if d := diffToks(a[i], b[i], fmt.Sprintf("select column %d", i+1)); d != "" {
			return d
		}
	}
	return ""
}

func diffTables(a, b []string) string {
	if len(a) != len(b) {
		return fmt.Sprintf("table list differs: source %v vs generated %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("table %d differs: source %q vs generated %q", i+1, a[i], b[i])
		}
	}
	return ""
}

func diffToks(a, b []sqlToken, label string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("%s differs at token %d: source %q vs generated %q", label, i+1, snippetAt(a, i), snippetAt(b, i))
		}
	}
	if len(a) != len(b) {
		longer := a
		if len(b) > len(a) {
			longer = b
		}
		return fmt.Sprintf("%s differs: %d tokens vs %d — extra %q", label, len(a), len(b), snippetAt(longer, n))
	}
	return ""
}

// snippetAt renders up to six tokens from at, ellipsized — a bounded,
// human-readable diff detail for the run log.
func snippetAt(toks []sqlToken, at int) string {
	var parts []string
	for i := at; i < at+6 && i < len(toks); i++ {
		parts = append(parts, toks[i].Text)
	}
	s := strings.Join(parts, " ")
	if at+6 < len(toks) {
		s += " …"
	}
	return s
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
