package flow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/sqltext"
	scanner "tux-migration-tool/internal/tsscan"
)

// Query reuse report (<entry>.qreuse.md / <entry>.qreuse.json).
//
// The scenario diff answers "which scenarios keep which LINES". This answers
// the question a reviewer actually asks of a DB method: of the query units two
// arms both reach, are they the same statement, and if not, where do they
// differ?
//
// Two keys, deliberately different:
//
//   - the REUSE key is sqltext.SemanticKey — the statement itself. Equal keys
//     mean one DB method, full stop.
//   - the SHAPE key is the structural envelope (tables, select-item count, row
//     shape, binds, ORDER BY). Equal shape with unequal reuse key is the
//     interesting case: the same query under a varying predicate, which is
//     usually one method plus one parameter rather than two methods.
//
// Read-only over the IR: no IR field changes, no codegen decision is taken.
// Where the IR itself is lossy the report says so in Known blind spots rather
// than resolving it silently.

// Reuse family verdicts.
const (
	// ReuseSingle: the family holds one unit, so there is nothing to compare
	// and nothing to reuse.
	ReuseSingle = "single"
	// ReuseIdentical: every member carries the same reuse key.
	ReuseIdentical = "identical"
	// ReuseParam: members differ only by literal-valued predicates on columns
	// every member filters on — one method plus one parameter per column.
	ReuseParam = "one-method-plus-param"
	// ReuseVarying: same shape, but the difference is structural (extra or
	// dropped predicates, select-list drift). Review before collapsing.
	ReuseVarying = "varying-predicate"
)

// ReuseFamily is one shape family: the query units agreeing on tables,
// select-item count, row shape, binds and ORDER BY. Distinct families are
// always separate DB methods.
type ReuseFamily struct {
	ID      string   `json:"id"`
	Verdict string   `json:"verdict"`
	Shape   string   `json:"shape"`
	Units   []string `json:"units"`
	// Scenarios is the sorted union of the members' scenario keys.
	Scenarios []string       `json:"scenarios"`
	Members   []*ReuseMember `json:"members"`
	// Common lists the WHERE predicates every member shares, so the delta is
	// read in context.
	Common []string `json:"common_predicates,omitempty"`
	// VaryingColumns names the columns whose literals differ between members
	// and that reduce to a plain `col = lit` / `col IN (…)` predicate.
	VaryingColumns []*VaryingColumn `json:"varying_columns,omitempty"`
	// StructuralDrift lists, per member, the extra predicates that are NOT a
	// simple literal comparison — why a family may not collapse blindly.
	StructuralDrift []MemberDrift `json:"structural_drift,omitempty"`
	// SelectDrift lists, per member, the select items no sibling shares.
	SelectDrift []MemberDrift `json:"select_drift,omitempty"`
	// Identical names the members that repeat the family's first member under
	// the same reuse key.
	Identical []string `json:"identical_repeats,omitempty"`
}

// ReuseMember is one unit inside a family.
type ReuseMember struct {
	ID          string   `json:"id"`
	Line        int      `json:"line"`
	EndLine     int      `json:"end_line"`
	Scenarios   []string `json:"scenarios,omitempty"`
	Key         string   `json:"reuse_key"`
	DuplicateOf string   `json:"duplicate_of,omitempty"`
	// Extra lists the WHERE predicates this member has beyond Common.
	Extra []string `json:"extra_predicates,omitempty"`
}

// MemberDrift names the member a drift list belongs to.
type MemberDrift struct {
	Unit  string   `json:"unit"`
	Items []string `json:"items"`
}

// VaryingColumn is a column whose literal values differ between a family's
// members — the parameterization candidate.
type VaryingColumn struct {
	Column string `json:"column"`
	// Values is ordered by member line. A slice, not a map, so the JSON order
	// is the IR order.
	Values []ColumnValue `json:"values"`
	// Counted is how many members actually filter on the column. Only a
	// full count makes the column parameterizable: folding a predicate into a
	// parameter for some arms and not others would change a result set.
	Counted int `json:"members_filtering"`
}

// ColumnValue is one member's literals for a varying column. A member that
// does not filter on the column carries Unfiltered, so "no filter" never
// reads as "filters on nothing".
type ColumnValue struct {
	Unit       string   `json:"unit"`
	Literals   []string `json:"literals,omitempty"`
	Unfiltered bool     `json:"unfiltered,omitempty"`
}

// CursorReport is one cursor's lifecycle across the entry: the opens that
// produced units and the FETCH choreography behind them.
//
// The statement counts and the FETCH line census come from the scan, because
// they are properties of the choreography as written — a cursor no unit ended
// up modelling still has fetches worth counting. What each unit actually
// resolved to — the INTO lists and whether they disagreed — is read from the
// IR (Query.FetchSites / Query.RowShapeDivergent), so the report no longer
// has to reconstruct a fact the IR now carries.
type CursorReport struct {
	Name  string   `json:"name"`
	Units []string `json:"units,omitempty"`
	// Declares / Opens / Fetches / Closes count the scanned choreography
	// statements. Opens counts both the plain OPEN and the EXECUTE-wrapped
	// `OPEN … FOR` form, the latter being what most reopens actually are.
	Declares      int `json:"declares"`
	Opens         int `json:"opens"`
	EmbeddedOpens int `json:"embedded_opens"`
	Fetches       int `json:"fetches"`
	Closes        int `json:"closes"`
	// FetchSites is every FETCH line, ascending.
	FetchSites []int `json:"fetch_sites,omitempty"`
	// FetchVariants counts the distinct FETCH INTO lists the IR recorded for
	// this cursor. >1 means one cursor is fed into more than one set of host
	// variables.
	FetchVariants int  `json:"fetch_variants"`
	Divergent     bool `json:"row_shape_divergent,omitempty"`
	// DivergentUnits are units where two fetches INSIDE the unit's own open
	// window disagree — the row shape is one of several and the others reach
	// nothing. This is the case to review.
	DivergentUnits []string `json:"divergent_units,omitempty"`
	// WidenedUnits are units whose open window held no fetch, so the row
	// shape was borrowed from the cursor's first fetch. Their variant count
	// spans sibling opens and their own row shape is not in doubt, but the
	// provenance is worth a glance.
	WidenedUnits []string `json:"widened_units,omitempty"`
}

// QueryReuseReport is the <entry>.qreuse.json record.
type QueryReuseReport struct {
	Entry    string          `json:"entry"`
	Keys     []string        `json:"keys,omitempty"`
	Units    int             `json:"units"`
	Families []*ReuseFamily  `json:"families"`
	Cursors  []*CursorReport `json:"cursors,omitempty"`
	// Reusable counts units that share a family with at least one other unit.
	Reusable int `json:"reusable_units"`
	// SeparateMethods is how many DB methods the entry needs if each family
	// becomes one method.
	SeparateMethods int      `json:"separate_methods"`
	Summary         string   `json:"summary"`
	BlindSpots      []string `json:"blind_spots,omitempty"`
}

// literalPred recognizes a predicate that filters one column on literals:
// `COL = 'x'`, `COL <> 3`, `COL IN ('a','b')`, optionally table-qualified and
// optionally negated. The patterns tolerate absent spacing because they run on
// semantic keys, where `COL ='x'` and `COL='x'` are one predicate. Anything
// involving a function call — NVL, CASE, DECODE — deliberately does not match:
// those land in StructuralDrift rather than being dressed up as
// parameterizable.
var (
	simplePred = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*(\.[A-Z_][A-Z0-9_]*)*)\s*(<>|!=|>=|<=|=|<|>)\s*(.+)$`)
	inPred     = regexp.MustCompile(`^(NOT\s+)?([A-Z_][A-Z0-9_]*(\.[A-Z_][A-Z0-9_]*)*)\s+IN\s*\((.+)\)$`)
)

// BuildQueryReuse folds the IR query units into shape families and judges each
// family's reuse verdict. irFile supplies the unit facts, diff the scenario
// membership (the QueryMembers the shared report already prints), tree the
// scan facts behind the cursor lifecycle. Pure and read-only.
func BuildQueryReuse(entry string, irFile *ir.File, diff *ScenarioDiff, tree *Tree) *QueryReuseReport {
	r := &QueryReuseReport{Entry: entry}
	if irFile == nil {
		r.Summary = "no IR — nothing to compare"
		return r
	}
	// Scenario membership, keyed by unit id, taken from the shared report so
	// the two artifacts cannot disagree about who reaches what.
	scen := map[string][]string{}
	if diff != nil {
		r.Keys = diff.Keys
		for _, m := range diff.Queries {
			scen[m.ID] = m.Keys
		}
	}

	byShape := map[string][]*ir.Query{}
	var shapeOrder []string
	for _, q := range irFile.Queries {
		k := reuseShape(q)
		if _, seen := byShape[k]; !seen {
			shapeOrder = append(shapeOrder, k)
		}
		byShape[k] = append(byShape[k], q)
	}
	// Seed in IR order, then order families by their first member's line so
	// the report reads in source order.
	sort.SliceStable(shapeOrder, func(i, j int) bool {
		return byShape[shapeOrder[i]][0].StartLine < byShape[shapeOrder[j]][0].StartLine
	})

	r.Units = len(irFile.Queries)
	for i, sk := range shapeOrder {
		fam := buildFamily(fmt.Sprintf("F%d", i+1), byShape[sk], scen)
		r.Families = append(r.Families, fam)
		if len(fam.Units) > 1 {
			r.Reusable += len(fam.Units)
		}
	}
	r.SeparateMethods = len(r.Families)
	r.Cursors = buildCursors(irFile, tree)
	r.BlindSpots = bindBlindSpots(irFile)
	r.Summary = reuseSummary(r)
	return r
}

// reuseShape is the structural envelope: the facts that decide whether two
// units can ever be one DB method. It deliberately excludes the WHERE text
// (that is the reuse key's job) and includes the row shape, which the
// extractor's DedupKey omits.
func reuseShape(q *ir.Query) string {
	tabs := make([]string, 0, len(q.Tables))
	for _, t := range q.Tables {
		tabs = append(tabs, strings.ToUpper(t))
	}
	sort.Strings(tabs)
	return strings.Join([]string{
		"tables=" + strings.Join(tabs, ","),
		fmt.Sprintf("cols=%d", len(sqltext.SelectItems(q.SQL))),
		"row=" + strings.Join(upperAll(q.RowShape), ","),
		"binds=" + strings.Join(sortedUpper(setOf(execBinds(q.SQL))), ","),
		"order=" + sqltext.SemanticKey(q.OrderBy),
	}, "|")
}

// buildFamily folds one shape group into a family record and its verdict.
func buildFamily(id string, qs []*ir.Query, scen map[string][]string) *ReuseFamily {
	sort.SliceStable(qs, func(i, j int) bool { return qs[i].StartLine < qs[j].StartLine })
	f := &ReuseFamily{ID: id, Verdict: ReuseSingle, Shape: familyShape(qs[0])}

	keys := make([]string, len(qs))
	preds := make([][]string, len(qs))
	items := make([]map[string]string, len(qs))
	for i, q := range qs {
		keys[i] = sqltext.SemanticKey(q.SQL)
		preds[i] = wherePredicates(q.SQL)
		items[i] = selectItems(q.SQL)
		f.Units = append(f.Units, q.ID)
		f.Members = append(f.Members, &ReuseMember{
			ID:          q.ID,
			Line:        q.StartLine,
			EndLine:     q.EndLine,
			Scenarios:   scen[q.ID],
			Key:         keys[i],
			DuplicateOf: q.DuplicateOf,
		})
	}
	for _, m := range f.Members {
		f.Scenarios = append(f.Scenarios, m.Scenarios...)
	}
	f.Scenarios = sortedUnique(f.Scenarios)

	if len(qs) == 1 {
		// Nothing to compare: a one-unit family carries no reuse verdict.
		return f
	}

	allSame := true
	for _, k := range keys {
		if k != keys[0] {
			allSame = false
			break
		}
	}
	if allSame {
		f.Verdict = ReuseIdentical
		f.Common = preds[0]
		for _, m := range f.Members[1:] {
			f.Identical = append(f.Identical, m.ID)
		}
		return f
	}

	f.Common = intersectAll(preds)
	// For each member: the predicates beyond Common, each classified as either
	// a simple literal comparison (a parameterization candidate) or structural
	// drift.
	colVals := map[string]map[string][]string{}
	simple := true
	for i, q := range qs {
		extras := difference(preds[i], f.Common)
		f.Members[i].Extra = extras
		var drift []string
		for _, p := range extras {
			col, lits, ok := literalSignature(p)
			if !ok {
				simple = false
				drift = append(drift, p)
				continue
			}
			if colVals[col] == nil {
				colVals[col] = map[string][]string{}
			}
			colVals[col][q.ID] = lits
		}
		if len(drift) > 0 {
			f.StructuralDrift = append(f.StructuralDrift, MemberDrift{Unit: q.ID, Items: drift})
		}
	}
	for _, col := range sortedMapKeys(colVals) {
		byUnit := colVals[col]
		vc := &VaryingColumn{Column: col}
		for _, q := range qs {
			lits, ok := byUnit[q.ID]
			if !ok {
				vc.Values = append(vc.Values, ColumnValue{Unit: q.ID, Unfiltered: true})
				continue
			}
			vc.Values = append(vc.Values, ColumnValue{Unit: q.ID, Literals: lits})
			vc.Counted++
		}
		f.VaryingColumns = append(f.VaryingColumns, vc)
	}
	// One method plus a parameter only holds when every difference is a plain
	// literal comparison — and even then only the columns every member filters
	// on are safe to lift; the rest are reported for review. Anything else
	// leaves the family structurally varying.
	if simple && len(colVals) > 0 && allColumnsCounted(f.VaryingColumns, len(qs)) {
		f.Verdict = ReuseParam
	} else {
		f.Verdict = ReuseVarying
	}
	f.SelectDrift = selectDrift(qs, items)
	return f
}

// allColumnsCounted reports whether every varying column is filtered on by
// every member.
func allColumnsCounted(vcs []*VaryingColumn, n int) bool {
	for _, vc := range vcs {
		if vc.Counted != n {
			return false
		}
	}
	return true
}

// literalSignature recognizes `COL = lit` and `COL IN (lit, …)` and returns
// the column with its literals. The comparison operator is not returned: the
// caller needs to know which column is filtered and on what, and two members
// using `=` vs `IN` on one column is exactly the shape that collapses to one
// parameterized method.
func literalSignature(pred string) (string, []string, bool) {
	if m := inPred.FindStringSubmatch(pred); m != nil {
		lits := splitTopLevel(m[4], ",")
		if len(lits) == 0 {
			return "", nil, false
		}
		return m[2], lits, true
	}
	m := simplePred.FindStringSubmatch(pred)
	if m == nil {
		return "", nil, false
	}
	// A function call on either side is not a plain literal comparison.
	if strings.ContainsAny(m[4], "()") {
		return "", nil, false
	}
	return m[1], []string{m[4]}, true
}

// selectDrift lists, per member, the select items no sibling shares.
func selectDrift(qs []*ir.Query, items []map[string]string) []MemberDrift {
	if len(qs) < 2 {
		return nil
	}
	counts := map[string]int{}
	for _, m := range items {
		for k := range m {
			counts[k]++
		}
	}
	var out []MemberDrift
	for i, q := range qs {
		var uniq []string
		for k, v := range items[i] {
			if counts[k] == 1 {
				uniq = append(uniq, v)
			}
		}
		if len(uniq) == 0 {
			continue
		}
		sort.Strings(uniq)
		out = append(out, MemberDrift{Unit: q.ID, Items: uniq})
	}
	return out
}

// buildCursors reads the scanned choreography per cursor name. The counts and
// the FETCH line census come from the scan; the INTO variants and the
// divergence come from the IR's per-unit FetchSites.
func buildCursors(irFile *ir.File, tree *Tree) []*CursorReport {
	unitsByCursor := map[string][]string{}
	variants := map[string]map[string]bool{}
	divergent := map[string][]string{}
	widened := map[string][]string{}
	for _, q := range irFile.Queries {
		if q.CursorName == "" {
			continue
		}
		k := upperName(q.CursorName)
		unitsByCursor[k] = append(unitsByCursor[k], q.ID)
		if variants[k] == nil {
			variants[k] = map[string]bool{}
		}
		for _, fs := range q.FetchSites {
			if len(fs.Into) == 0 {
				continue
			}
			variants[k][strings.Join(fs.Into, ",")] = true
		}
		if q.FetchSitesWidened {
			widened[k] = append(widened[k], q.ID)
		}
		if q.RowShapeDivergent() {
			divergent[k] = append(divergent[k], q.ID)
		}
	}
	if tree == nil || tree.facts == nil {
		return nil
	}
	byCursor := map[string]*CursorReport{}
	for _, s := range tree.facts.AllSQL {
		name := upperName(s.CursorName)
		if name == "" {
			continue
		}
		rep := byCursor[name]
		if rep == nil {
			rep = &CursorReport{Name: name}
			byCursor[name] = rep
		}
		switch s.Kind {
		case scanner.SQLDeclareCursor:
			rep.Declares++
		case scanner.SQLOpen:
			rep.Opens++
		case scanner.SQLOpenCursorFor:
			rep.EmbeddedOpens++
		case scanner.SQLClose:
			rep.Closes++
		case scanner.SQLFetch:
			rep.Fetches++
			rep.FetchSites = append(rep.FetchSites, s.StartLine)
		}
	}
	out := make([]*CursorReport, 0, len(byCursor))
	for _, name := range sortedMapKeys(byCursor) {
		rep := byCursor[name]
		rep.Units = unitsByCursor[name]
		rep.FetchVariants = len(variants[name])
		rep.Divergent = rep.FetchVariants > 1
		// Split the divergent units by provenance: only those that saw two
		// disagreeing fetches inside their own open window have a row shape
		// in doubt. The rest borrowed their shape from outside their window.
		for _, id := range divergent[name] {
			if listHas(widened[name], id) {
				continue
			}
			rep.DivergentUnits = append(rep.DivergentUnits, id)
		}
		rep.WidenedUnits = widened[name]
		out = append(out, rep)
	}
	return out
}

// bindBlindSpots reports units whose IR bind list disagrees with the
// literal-aware scan. The IR's scanHostRefs is not literal-aware, so an Oracle
// time mask like 'hh:mi:ss' inside the statement harvests as the host
// variables :mi and :ss — inflating BindCount and, on the codegen paths that
// read Query.Binds directly, the generated parameter list.
func bindBlindSpots(irFile *ir.File) []string {
	var out []string
	for _, q := range irFile.Queries {
		real := execBinds(q.SQL)
		var surplus []string
		for _, b := range q.Binds {
			if !listHasFold(real, b) {
				surplus = append(surplus, b)
			}
		}
		if len(surplus) == 0 {
			continue
		}
		sort.Strings(surplus)
		out = append(out, fmt.Sprintf(
			"`%s` (line %d): the IR records %s (%s) but only %s (%s); the surplus (%s) is harvested from string literals, not binds",
			q.ID, q.StartLine, countNoun(len(q.Binds), "bind", "binds"), strings.Join(q.Binds, ", "),
			countNoun(len(real), "real host variable", "real host variables"), strings.Join(real, ", "),
			strings.Join(surplus, ", ")))
	}
	return out
}

func reuseSummary(r *QueryReuseReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s → %s; ", countNoun(r.Units, "query unit", "query units"),
		countNoun(r.SeparateMethods, "shape family", "shape families"))
	if r.Reusable == 0 {
		b.WriteString("no two units share a shape, so every unit needs its own DB method.")
		return b.String()
	}
	fmt.Fprintf(&b, "%s share a shape with at least one other", countNoun(r.Reusable, "unit", "units"))
	var identical, param, varying int
	for _, f := range r.Families {
		if len(f.Units) < 2 {
			continue
		}
		switch f.Verdict {
		case ReuseIdentical:
			identical++
		case ReuseParam:
			param++
		default:
			varying++
		}
	}
	if identical > 0 {
		fmt.Fprintf(&b, "; %s are byte-identical repeats", countNoun(identical, "family", "families"))
	}
	if param > 0 {
		fmt.Fprintf(&b, "; %s collapse to one method plus parameters", countNoun(param, "family", "families"))
	}
	if varying > 0 {
		fmt.Fprintf(&b, "; %s share a shape but differ structurally — review before collapsing",
			countNoun(varying, "family", "families"))
	}
	b.WriteString(".")
	return b.String()
}

// countNoun renders "1 query unit" / "3 query units".
func countNoun(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// RenderQueryReuseMD renders the human half of the reuse report.
func RenderQueryReuseMD(r *QueryReuseReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — query reuse report\n\n", r.Entry)
	if len(r.Keys) > 0 {
		fmt.Fprintf(&sb, "Scenarios (%d): %s\n\n", len(r.Keys), strings.Join(r.Keys, ", "))
	}
	fmt.Fprintf(&sb, "%s\n\n", r.Summary)
	sb.WriteString("A **shape family** is the set of units agreeing on tables, select-item\ncount, row shape, binds and ORDER BY. Members of one family are candidates\nfor a single DB method; distinct families are always separate methods.\n\n")

	sb.WriteString("## Families\n\n")
	if len(r.Families) == 0 {
		sb.WriteString("none\n\n")
	}
	if len(r.Families) > 0 {
		sb.WriteString("| family | verdict | units | scenarios | shape |\n")
		sb.WriteString("|---|---|---|---|---|\n")
	}
	for _, f := range r.Families {
		units := strings.Join(f.Units, ", ")
		if len(f.Units) > 3 {
			units = fmt.Sprintf("%s … (%d total)", strings.Join(f.Units[:3], ", "), len(f.Units))
		}
		fmt.Fprintf(&sb, "| %s | %s | %d (%s) | %d | %s |\n",
			f.ID, f.Verdict, len(f.Units), units, len(f.Scenarios), mdCell(f.Shape))
	}
	if len(r.Families) > 0 {
		sb.WriteString("\n")
	}

	sb.WriteString("## Family detail\n\n")
	for _, f := range r.Families {
		if len(f.Units) == 1 {
			fmt.Fprintf(&sb, "### %s — %s (single unit)\n\n%s\n\n", f.ID, f.Units[0], mdCell(f.Shape))
			continue
		}
		fmt.Fprintf(&sb, "### %s — %d units, verdict `%s`\n\n", f.ID, len(f.Units), f.Verdict)
		fmt.Fprintf(&sb, "Shape (shared by every member): %s\n\n", mdCell(f.Shape))
		sb.WriteString("| unit | lines | scenarios | reuse key | extractor dedup |\n")
		sb.WriteString("|---|---|---|---|---|\n")
		for _, m := range f.Members {
			dup := "—"
			if m.DuplicateOf != "" {
				dup = "duplicate of `" + m.DuplicateOf + "`"
			}
			scen := "—"
			if len(m.Scenarios) > 0 {
				scen = strings.Join(m.Scenarios, ", ")
			}
			fmt.Fprintf(&sb, "| %s | %d-%d | %s | `%s` | %s |\n",
				m.ID, m.Line, m.EndLine, scen, shortKey(m.Key), dup)
		}
		sb.WriteString("\n")

		if len(f.Identical) > 0 {
			fmt.Fprintf(&sb, "Byte-identical repeats of `%s`: %s\n\n", f.Members[0].ID, strings.Join(f.Identical, ", "))
		}
		if len(f.Common) > 0 {
			fmt.Fprintf(&sb, "Shared WHERE predicates (%d): %s\n\n", len(f.Common), bulletList(f.Common))
		}
		if hasExtras(f) {
			sb.WriteString("Where they differ — the predicates each member adds on top of the shared set:\n\n")
			for _, m := range f.Members {
				if len(m.Extra) == 0 {
					fmt.Fprintf(&sb, "- `%s`: nothing beyond the shared predicates\n", m.ID)
					continue
				}
				fmt.Fprintf(&sb, "- `%s`: %s\n", m.ID, bulletInline(m.Extra))
			}
			sb.WriteString("\n")
		}
		for _, vc := range f.VaryingColumns {
			note := ""
			if vc.Counted != len(f.Members) {
				note = fmt.Sprintf(" — **only %d of %d members filter on it, so it is NOT safe to lift**", vc.Counted, len(f.Members))
			}
			fmt.Fprintf(&sb, "Varying literal column `%s` (%d of %d members filter on it%s):\n\n",
				vc.Column, vc.Counted, len(f.Members), note)
			for _, v := range vc.Values {
				if v.Unfiltered {
					fmt.Fprintf(&sb, "- `%s`: not filtered\n", v.Unit)
					continue
				}
				fmt.Fprintf(&sb, "- `%s`: %s\n", v.Unit, strings.Join(v.Literals, " | "))
			}
			sb.WriteString("\n")
		}
		// Only worth a second listing when some difference WAS reducible to a
		// parameter — otherwise "Where they differ" already printed the whole
		// story and repeating it says nothing new.
		if len(f.StructuralDrift) > 0 && len(f.VaryingColumns) > 0 {
			sb.WriteString("Structural drift — the remaining differences, NOT reducible to a\nparameter, which is why this family does not collapse blindly:\n\n")
			for _, d := range f.StructuralDrift {
				fmt.Fprintf(&sb, "- `%s`: %s\n", d.Unit, bulletInline(d.Items))
			}
			sb.WriteString("\n")
		}
		if len(f.SelectDrift) > 0 {
			sb.WriteString("Select items unique to one member:\n\n")
			for _, d := range f.SelectDrift {
				fmt.Fprintf(&sb, "- `%s`: %s\n", d.Unit, bulletInline(d.Items))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("## Cursor lifecycle\n\n")
	if len(r.Cursors) == 0 {
		sb.WriteString("none\n\n")
	} else {
		sb.WriteString("| cursor | units | declare | open | embedded open | fetch | close | fetch variants | divergent |\n")
		sb.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, c := range r.Cursors {
			div := "—"
			if c.Divergent {
				div = fmt.Sprintf("**YES — %d distinct FETCH INTO lists**", c.FetchVariants)
			}
			fmt.Fprintf(&sb, "| %s | %s | %d | %d | %d | %d | %d | %d | %s |\n",
				c.Name, mdCell(strings.Join(c.Units, ", ")), c.Declares, c.Opens, c.EmbeddedOpens,
				c.Fetches, c.Closes, c.FetchVariants, div)
		}
		sb.WriteString("\n")
		for _, c := range r.Cursors {
			if len(c.DivergentUnits) > 0 {
				fmt.Fprintf(&sb,
					"- `%s`: two FETCHes inside the same open's window scan into different host\n  variables, so %s resolved its row shape from the first and the other INTO list\n  reaches nothing. Fetch lines: %s.\n",
					c.Name, bulletInline(c.DivergentUnits), joinInts(c.FetchSites))
			}
		}
		if widened := widenedCursors(r.Cursors); len(widened) > 0 {
			sb.WriteString("\n")
			for _, c := range widened {
				fmt.Fprintf(&sb,
					"- `%s`: %s have no FETCH inside their own open window, so the row shape was\n  borrowed from the cursor's first FETCH. The shape is right for all of them — the\n  note is provenance, not a loss.\n",
					c.Name, bulletInline(c.WidenedUnits))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Known blind spots\n\n")
	sb.WriteString("This report is read-only. The following are losses in the IR itself; they\nare surfaced here rather than fixed.\n\n")
	// A fixed problem must not keep being reported as a blind spot: the
	// cursor DedupKey now folds in the row shape, so two opens that differ
	// only in their FETCH INTO targets no longer fold together.
	spots := append([]string(nil), r.BlindSpots...)
	spots = append(spots,
		"FETCH INTO targets are recorded per unit (`Query.FetchSites`) but only the first one\n  resolves into `RowShape`, which is what codegen reads. A cursor whose fetches disagree\n  keeps the rest as data no emitter consults — see Cursor lifecycle.",
		"The FETCH line census above is a scan fact, not an IR fact: `Query.Sites` records only\n  the DECLARE/OPEN line. The INTO lists and the divergence are IR facts; where the lines\n  are is still reconstructed from the scan.",
	)
	for _, s := range spots {
		fmt.Fprintf(&sb, "- %s\n", s)
	}
	return sb.String()
}

// widenedCursors are cursors with at least one unit that borrowed its row
// shape from outside its own open window.
func widenedCursors(cs []*CursorReport) []*CursorReport {
	var out []*CursorReport
	for _, c := range cs {
		if len(c.WidenedUnits) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func hasExtras(f *ReuseFamily) bool {
	for _, m := range f.Members {
		if len(m.Extra) > 0 {
			return true
		}
	}
	return false
}

func familyShape(q *ir.Query) string {
	return fmt.Sprintf("tables [%s], %s, %s, %s, ORDER BY %s",
		strings.Join(q.Tables, ", "), countNoun(len(sqltext.SelectItems(q.SQL)), "select item", "select items"),
		countNoun(len(q.RowShape), "row-shape column", "row-shape columns"),
		countNoun(len(execBinds(q.SQL)), "bind", "binds"), orDash(q.OrderBy))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func bulletList(v []string) string {
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = "`" + s + "`"
	}
	return strings.Join(parts, ", ")
}

// bulletInline renders a possibly long list inline, capped so one wide
// predicate cannot swamp the report.
func bulletInline(v []string) string {
	const maxInline = 8
	if len(v) <= maxInline {
		return bulletList(v)
	}
	return bulletList(v[:maxInline]) + fmt.Sprintf(" … (%d more)", len(v)-maxInline)
}

// shortKey abbreviates a reuse key for the table; the full key is in the JSON.
func shortKey(k string) string {
	if len(k) <= 44 {
		return k
	}
	return k[:41] + "…"
}

func upperAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func setOf(v []string) map[string]bool {
	out := make(map[string]bool, len(v))
	for _, s := range v {
		out[s] = true
	}
	return out
}

func sortedUpper(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strings.ToUpper(k))
	}
	sort.Strings(out)
	return out
}

func sortedUnique(v []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(v))
	for _, s := range v {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func listHasFold(v []string, s string) bool {
	for _, x := range v {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func upperName(s string) string {
	return strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(s), ":"))
}

// execBinds is the literal-aware bind scan — the count a generated method
// should have, as opposed to the IR's Query.Binds.
func execBinds(sql string) []string {
	return sqltext.ExecutableBinds(sqltext.StripInto(sql))
}

// intersectAll returns the predicates every member carries, in the first
// member's order.
func intersectAll(preds [][]string) []string {
	if len(preds) == 0 {
		return nil
	}
	first := preds[0]
	var out []string
	for _, p := range first {
		inAll := true
		for _, other := range preds[1:] {
			if !listHas(other, p) {
				inAll = false
				break
			}
		}
		if inAll {
			out = append(out, p)
		}
	}
	return out
}

func difference(v, drop []string) []string {
	d := setOf(drop)
	var out []string
	for _, s := range v {
		if !d[s] {
			out = append(out, s)
		}
	}
	return out
}

func listHas(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

// sortedMapKeys returns the sorted keys of any string-keyed map. The package's
// sortedKeys covers the map[string]bool sets; this one covers the report's
// map[string][]string and map[string]*CursorReport.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// selectItems indexes a statement's select list by semantic key, keeping the
// whitespace-normalized original text for display.
func selectItems(sql string) map[string]string {
	out := map[string]string{}
	for _, it := range sqltext.SelectItems(sql) {
		out[sqltext.SemanticKey(it)] = strings.Join(strings.Fields(it), " ")
	}
	return out
}

// wherePredicates splits the top-level WHERE clause on top-level AND and
// returns each predicate under its semantic key. The clause ends at the first
// top-level ordering/aggregation/composition keyword.
func wherePredicates(sql string) []string {
	clause := whereClause(sql)
	if clause == "" {
		return nil
	}
	parts := splitTopLevel(clause, "AND")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// Under the semantic key, so the shared/extra set algebra below is
		// insensitive to `= 'C'` vs `='C'` and to identifier case — a
		// difference the extractor sees but which cannot change the rows.
		out = append(out, sqltext.SemanticKey(p))
	}
	return out
}

func whereClause(sql string) string {
	start := topKeyword(sql, "WHERE")
	if start < 0 {
		return ""
	}
	s := sql[start+len("WHERE"):]
	end := len(s)
	for _, kw := range []string{"ORDER BY", "GROUP BY", "HAVING", "UNION", "MINUS", "INTERSECT", "CONNECT BY", "START WITH"} {
		if i := topKeyword(s, kw); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimSpace(s[:end])
}

// topKeyword finds kw at relative paren depth zero, outside string literals,
// on word boundaries.
func topKeyword(s, kw string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			i = skipLiteral(s, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i+len(kw) <= len(s) && s[i:i+len(kw)] == kw &&
				(i == 0 || !isIdentByte(s[i-1])) &&
				(i+len(kw) == len(s) || !isIdentByte(s[i+len(kw)])) {
				return i
			}
		}
	}
	return -1
}

// splitTopLevel splits on sep at paren depth zero, outside string literals.
func splitTopLevel(s, sep string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			i = skipLiteral(s, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i+len(sep) <= len(s) && s[i:i+len(sep)] == sep &&
				(i == 0 || !isIdentByte(s[i-1])) &&
				(i+len(sep) == len(s) || !isIdentByte(s[i+len(sep)])) {
				out = append(out, s[start:i])
				start = i + len(sep)
				i += len(sep) - 1
			}
		}
	}
	out = append(out, s[start:])
	kept := make([]string, 0, len(out))
	for _, p := range out {
		if t := strings.TrimSpace(p); t != "" {
			kept = append(kept, t)
		}
	}
	return kept
}

// skipLiteral returns the index of the closing quote of the literal opened at
// s[i], honouring the doubled-quote escape.
func skipLiteral(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\'' {
			if j+1 < len(s) && s[j+1] == '\'' {
				j++
				continue
			}
			return j
		}
	}
	return len(s) - 1
}
