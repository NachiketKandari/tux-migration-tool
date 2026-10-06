package flow

import (
	"fmt"
	"sort"
	"strings"
)

// LineBlock is a maximal run of consecutive source lines with one shared
// membership: the scenarios keeping them (G-SCEN4, SCEN-D5 — exact line-set
// algebra, no fuzzy diffing).
type LineBlock struct {
	Start     int      `json:"start"`
	End       int      `json:"end"`
	Scenarios []string `json:"scenarios"` // keys of the scenarios keeping the block, in diff key order
}

// QueryMembers is one query's scenario membership (G-SCEN4): which
// scenarios reach it, and which of those decide its DML runs inside a
// transaction. TxMixed surfaces the tx-contradiction — tx in one scenario,
// non-tx in another — the per-query fact the plan's tx vote resolves.
type QueryMembers struct {
	ID      string   `json:"id"`
	DML     bool     `json:"dml,omitempty"`
	Keys    []string `json:"scenarios"`
	TxKeys  []string `json:"tx_scenarios,omitempty"`
	TxMixed bool     `json:"tx_mixed,omitempty"`
}

// ScenarioDiff is the cross-scenario reuse report (G-SCEN4): shared blocks
// (kept by ≥2 scenarios — reusable helpers emitted once), per-scenario
// unique blocks (the per-transaction logic), per-query membership (db-method
// dedup + tx-contradiction surfacing), and the first divergence point (the
// controller's dispatch delta). Pure function over the scenario IRs.
type ScenarioDiff struct {
	Entry      string                 `json:"entry"`
	Keys       []string               `json:"keys"`
	Shared     []LineBlock            `json:"shared,omitempty"`
	Unique     map[string][]LineBlock `json:"unique,omitempty"`
	Queries    []QueryMembers         `json:"queries,omitempty"`
	Divergence int                    `json:"divergence,omitempty"` // first line where scenarios disagree (0 = none)
}

// DiffScenarios computes the set algebra over the scenarios' kept line sets.
// Blocks are maximal runs of consecutive lines with identical membership;
// shared = ≥2 scenarios, unique = exactly one. The first divergence is the
// smallest line not kept by every scenario (preamble lines are kept by all,
// so it lands in the body). Deterministic: key order = input order.
func DiffScenarios(entry string, scens []*Scenario) *ScenarioDiff {
	d := &ScenarioDiff{Entry: entry, Unique: map[string][]LineBlock{}}
	keys := make([]string, len(scens))
	for i, sc := range scens {
		keys[i] = sc.Key
	}
	d.Keys = keys
	kept := map[int][]int{} // line → scenario indexes keeping it
	for i, sc := range scens {
		addKept(kept, i, sc)
	}
	lines := make([]int, 0, len(kept))
	for l := range kept {
		lines = append(lines, l)
	}
	sort.Ints(lines)

	// Maximal runs of consecutive lines with identical membership.
	sameSet := func(a, b []int) bool {
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
	flush := func(from, to int) {
		idxs := kept[lines[from]]
		block := LineBlock{Start: lines[from], End: lines[to], Scenarios: keyNames(idxs, keys)}
		switch {
		case len(idxs) >= 2:
			d.Shared = append(d.Shared, block)
		case len(idxs) == 1:
			k := keys[idxs[0]]
			d.Unique[k] = append(d.Unique[k], block)
		}
	}
	start := 0
	for i := 1; i <= len(lines); i++ {
		if i < len(lines) && lines[i] == lines[i-1]+1 && sameSet(kept[lines[i-1]], kept[lines[i]]) {
			continue // the run continues
		}
		flush(start, i-1)
		start = i
	}

	d.Queries = queryMembership(keys, scens)
	d.Divergence = firstDivergence(lines, kept, len(scens))
	return d
}

// addKept registers one scenario's kept lines (preamble + body spans) under
// its membership index. Parent and child spans overlap by construction, so
// a line already registered for the index stays single-member.
func addKept(kept map[int][]int, idx int, sc *Scenario) {
	var add func(l int)
	add = func(l int) {
		for _, have := range kept[l] {
			if have == idx {
				return
			}
		}
		kept[l] = append(kept[l], idx)
	}
	for i := 0; i+1 < len(sc.Preamble); i += 2 {
		for l := sc.Preamble[i]; l <= sc.Preamble[i+1]; l++ {
			add(l)
		}
	}
	var walk func(nodes []*SliceNode)
	walk = func(nodes []*SliceNode) {
		for _, sn := range nodes {
			for l := sn.Line; l <= sn.EndLine; l++ {
				add(l)
			}
			walk(sn.Children)
		}
	}
	walk(sc.Body)
}

// keyNames renders the scenario keys for a membership index list.
func keyNames(idxs []int, keys []string) []string {
	out := make([]string, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, keys[i])
	}
	return out
}

// queryMembership folds the per-query scenario membership in file query order.
func queryMembership(keys []string, scens []*Scenario) []QueryMembers {
	order := []string{}
	info := map[string]*QueryMembers{}
	get := func(id string) *QueryMembers {
		m, ok := info[id]
		if !ok {
			m = &QueryMembers{ID: id}
			info[id] = m
			order = append(order, id)
		}
		return m
	}
	for i, sc := range scens {
		for _, q := range sc.Queries {
			m := get(q.ID)
			m.Keys = append(m.Keys, keys[i])
			if q.DML {
				m.DML = true
				if q.Tx {
					m.TxKeys = append(m.TxKeys, keys[i])
				}
			}
		}
	}
	sort.Strings(order)
	out := make([]QueryMembers, 0, len(order))
	for _, id := range order {
		m := info[id]
		m.TxMixed = m.DML && len(m.TxKeys) > 0 && len(m.TxKeys) < len(m.Keys)
		out = append(out, *m)
	}
	return out
}

// firstDivergence is the smallest kept line whose membership misses any
// scenario — the first point where the scenarios' bodies disagree (0 when
// the slices are identical).
func firstDivergence(lines []int, kept map[int][]int, n int) int {
	for _, l := range lines {
		if len(kept[l]) < n {
			return l
		}
	}
	return 0
}

// RenderSharedMD renders the human-readable half of the shared report
// (SCEN-D6 twin: <entry>.shared.md beside <entry>.shared.json).
func RenderSharedMD(d *ScenarioDiff) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — scenario diff/reuse report\n\n", d.Entry)
	fmt.Fprintf(&sb, "Scenarios (%d): %s\n\n", len(d.Keys), strings.Join(d.Keys, ", "))

	sb.WriteString("## Shared blocks (kept by ≥2 scenarios — reusable helpers)\n\n")
	if len(d.Shared) == 0 {
		sb.WriteString("none\n\n")
	}
	for _, b := range d.Shared {
		fmt.Fprintf(&sb, "- lines %d-%d — %d scenario(s): %s\n", b.Start, b.End, len(b.Scenarios), strings.Join(b.Scenarios, ", "))
	}
	sb.WriteString("\n")

	sb.WriteString("## Scenario-unique blocks (per-scenario logic)\n\n")
	if len(d.Unique) == 0 {
		sb.WriteString("none\n\n")
	}
	for _, k := range d.Keys {
		blocks := d.Unique[k]
		if len(blocks) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "### %s\n\n", k)
		for _, b := range blocks {
			fmt.Fprintf(&sb, "- lines %d-%d\n", b.Start, b.End)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Query membership\n\n")
	if len(d.Queries) == 0 {
		sb.WriteString("none\n\n")
	} else {
		sb.WriteString("| query | dml | scenarios | tx scenarios |\n")
		sb.WriteString("|---|---|---|---|\n")
		for _, m := range d.Queries {
			tx := "—"
			if len(m.TxKeys) > 0 {
				tx = strings.Join(m.TxKeys, ", ")
				if m.TxMixed {
					tx += " (MIXED — non-tx in: " + strings.Join(diffKeys(m.Keys, m.TxKeys), ", ") + ")"
				}
			}
			dml := ""
			if m.DML {
				dml = "yes"
			}
			fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", m.ID, dml, strings.Join(m.Keys, ", "), tx)
		}
		sb.WriteString("\n")
	}

	if d.Divergence > 0 {
		fmt.Fprintf(&sb, "## First divergence\n\nline %d — the first kept line not shared by every scenario (the dispatch delta's start)\n", d.Divergence)
	} else {
		sb.WriteString("## First divergence\n\nnone — every kept line is shared (slices are identical)\n")
	}
	return sb.String()
}

// diffKeys returns the keys of a not present in b (stable order).
func diffKeys(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if !set[s] {
			out = append(out, s)
		}
	}
	return out
}
