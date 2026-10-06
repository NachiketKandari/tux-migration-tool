package flow

import (
	"bytes"
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
	scanner "tux-migration-tool/internal/tsscan"
)

// This file is the dispatch-axis registry (scenario-filter plan §3):
// DispatchAxisFor's harvest and ranking rubric live here, exposed as an
// all-candidates view so `scenarioFilter` expressions can intersect axes
// (`c_flag == 'H' && new_flag == 'K'`) without a second detector drifting
// from the first. DispatchAxisFor's own recognizer cascade is unchanged —
// the registry's rank 0 is structurally forced to its result.

// axisHarvest is one entry's raw axis evidence: the strcmp/char/defined
// constant predicate stats (pass 1) and the distinct branch-guard lines
// their values sit in (pass 2). The single home both DispatchAxisFor and
// AxesFor rank from.
type axisHarvest struct {
	facts    *scanner.SourceFacts
	lines    [][]byte
	from, to int

	refs   map[string]*axisStats
	idents map[string]*axisStats
	symbs  map[string]*axisStats     // ident == defined-constant compares
	links  map[string]map[string]int // ref → alias → linked count
	// guardNest maps a branch-guard start line to its nest depth, for the
	// guards this entry harvests. Depth is what separates the entry's
	// API-level spine from per-arm detail (see nestOf).
	guardNest map[int]int
}

// harvestAxes runs the two recognition passes over the entry's body span
// (comment-masked, so commented-out predicates never pollute a domain).
// nil when the entry has no open body span.
func harvestAxes(src []byte, facts *scanner.SourceFacts, entry string, irFile *ir.File) *axisHarvest {
	if facts == nil {
		return nil
	}
	span, ok := entrySpan(facts, entry)
	if !ok {
		return nil
	}
	lines := bytes.Split(src, []byte("\n"))
	from, to := span[0], span[1]
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return nil
	}
	h := &axisHarvest{
		facts:     facts,
		lines:     lines,
		from:      from,
		to:        to,
		refs:      map[string]*axisStats{},
		idents:    map[string]*axisStats{},
		symbs:     map[string]*axisStats{},
		links:     map[string]map[string]int{},
		guardNest: map[int]int{},
	}

	// Pass 1: strcmp + normalize + char-compare harvest (comment-masked).
	for i := from; i <= to; i++ {
		text := string(lines[i-1])
		if lineCommentMasked(facts, i, text) {
			continue
		}
		for _, m := range strcmpSiteRe.FindAllStringSubmatch(text, -1) {
			ref, val := m[1], m[2]
			st, ok := h.refs[ref]
			if !ok {
				st = newAxisStats(ref)
				h.refs[ref] = st
			}
			st.vals[val] = true
			st.sites++
		}
		for _, m := range charAssignRe.FindAllStringSubmatch(text, -1) {
			ident, ch := m[1], m[2]
			st, ok := h.idents[ident]
			if !ok {
				st = newAxisStats(ident)
				h.idents[ident] = st
			}
			st.cvals[ch] = true
			// Normalization link: the nearest preceding strcmp guarding
			// this assignment (unbraced if-bodies keep them adjacent).
			linkNormal(h.links, h.refs, i, ident, ch, lines, from)
		}
		for _, m := range charCompareRe.FindAllStringSubmatch(text, -1) {
			ident, ch := m[1], m[2]
			st, ok := h.idents[ident]
			if !ok {
				st = newAxisStats(ident)
				h.idents[ident] = st
			}
			st.compares++
			st.cvals[ch] = true
		}
		for _, m := range identCompareRe.FindAllStringSubmatch(text, -1) {
			ident, konst := m[1], m[2]
			val, defined := axisSymbolValue(irFile, entry, i, konst)
			if !defined {
				continue
			}
			if _, isConst := axisSymbolValue(irFile, entry, i, ident); isConst {
				continue // constant==constant folds at compile time — never dispatch
			}
			st, ok := h.symbs[ident]
			if !ok {
				st = newAxisStats(ident)
				h.symbs[ident] = st
			}
			st.sites++
			st.vals[val] = true
		}
	}

	// Pass 2: guard structure. An axis is dispatch structure, not string
	// similarity: its values must sit in the guards of (mutually exclusive)
	// branch arms. One branch record is ONE guard however many values its
	// compound condition tests — a membership strcmp chain (`!=0 && !=0`)
	// is flag derivation inside one arm, never a dispatch spine.
	for bi := range facts.Branches {
		b := &facts.Branches[bi]
		if b.Function != entry || b.Cond == "" || b.StartLine < from || b.StartLine > to {
			continue
		}
		h.guardNest[b.StartLine] = b.NestDepth
		for _, m := range strcmpSiteRe.FindAllStringSubmatch(b.Cond, -1) {
			if st := h.refs[m[1]]; st != nil {
				st.guards[b.StartLine] = true
			}
		}
		for _, m := range charCompareRe.FindAllStringSubmatch(b.Cond, -1) {
			if st := h.idents[m[1]]; st != nil {
				st.guards[b.StartLine] = true
			}
		}
		for _, m := range identCompareRe.FindAllStringSubmatch(b.Cond, -1) {
			if st := h.symbs[m[1]]; st != nil {
				st.guards[b.StartLine] = true
			}
		}
	}
	return h
}

// axisCandidate is one qualifying stats entry plus its rank inputs.
type axisCandidate struct {
	axis   *DispatchAxis
	weight int // recognizer weight (max link count / value count / compare count)
	sites  int // predicate-site count inside its class
	key    string
}

// collectAxes composes every qualifying candidate from one stats map and
// ranks them with the recognizer's weight, breaking ties by site count then
// identifier (the exact rig DispatchAxisFor's pickAxis always used — one
// implementation now).
//
// Qualification is per CANDIDATE, never per cascade result: a candidate
// needs ≥2 distinct values as well as ≥2 guard sites. Gating the value
// count here (rather than only on DispatchAxisFor's final winner) keeps a
// sub-threshold candidate in an earlier recognizer from short-circuiting
// the cascade past a qualifying spine in a later one — `strcmp(x, "")`
// emptiness probes harvest one value and sit in several guards, which is a
// validation check, not a dispatch.
func collectAxes(stats map[string]*axisStats, links map[string]map[string]int, idents map[string]*axisStats, weightOf func(*axisStats) (string, int)) []axisCandidate {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cands []axisCandidate
	for _, k := range keys {
		st := stats[k]
		alias, weight := weightOf(st)
		if weight <= 0 {
			continue
		}
		if guardSitesOf(st, alias, idents) < 2 {
			continue
		}
		axis := composeAxis(st, alias, links, idents)
		if len(axis.Domain) < 2 {
			continue
		}
		cands = append(cands, axisCandidate{
			axis:   axis,
			weight: weight,
			sites:  st.sites + st.compares,
			key:    k,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].weight != cands[j].weight {
			return cands[i].weight > cands[j].weight
		}
		if cands[i].sites != cands[j].sites {
			return cands[i].sites > cands[j].sites
		}
		return cands[i].key < cands[j].key
	})
	return cands
}

// normalizeWeightOf ranks a ref by its strongest normalization link
// (ref → alias → count). Equal link counts tie-break lexicographically:
// map iteration is not an order, and the registry promises repeat-run
// byte identity.
func normalizeWeightOf(links map[string]map[string]int) func(*axisStats) (string, int) {
	return func(st *axisStats) (string, int) {
		aliases := make([]string, 0, len(links[st.ref]))
		for al := range links[st.ref] {
			aliases = append(aliases, al)
		}
		sort.Strings(aliases)
		maxAlias, maxN := "", 0
		for _, al := range aliases {
			if n := links[st.ref][al]; n > maxN {
				maxAlias, maxN = al, n
			}
		}
		if maxAlias == "" {
			return "", 0
		}
		return maxAlias, maxN
	}
}

// byValuesWeight ranks a direct candidate by its distinct value count (the
// direct-strcmp ref and the defined-constant ident compete on this).
func byValuesWeight(st *axisStats) (string, int) { return "", len(st.vals) }

// charCompareWeight ranks a char-compare scalar by its compare count.
func charCompareWeight(st *axisStats) (string, int) { return st.ref, st.compares }

// composeAxis builds the axis from one stats entry and the chosen alias:
// domain = compare values ∪ assignment values, sorted.
func composeAxis(st *axisStats, alias string, links map[string]map[string]int, idents map[string]*axisStats) *DispatchAxis {
	domain := map[string]bool{}
	for v := range st.vals {
		domain[v] = true
	}
	for v := range st.cvals {
		domain[v] = true
	}
	vals := make([]string, 0, len(domain))
	for v := range domain {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	name := alias
	if name == "" {
		name = st.ref
	}
	if dot := strings.Index(name, "."); dot > 0 {
		name = name[:dot]
	}
	// Guard lines: the candidate's own guards plus its alias's (the
	// normalize idiom splits the test between the strcmp ref and the
	// scalar compare).
	guards := map[int]bool{}
	for l := range st.guards {
		guards[l] = true
	}
	if alias != "" {
		if is := idents[alias]; is != nil {
			for l := range is.guards {
				guards[l] = true
			}
		}
	}
	lines := make([]int, 0, len(guards))
	for l := range guards {
		lines = append(lines, l)
	}
	sort.Ints(lines)
	return &DispatchAxis{
		Ref:        st.ref,
		RefName:    name,
		Alias:      alias,
		Domain:     vals,
		Sites:      st.sites + st.compares,
		Normalized: alias != "" && len(links[st.ref]) > 0,
		GuardLines: lines,
	}
}

// nestOf reports the shallowest branch-nest depth among an axis's guards —
// the API-level test. 0 is the entry's top level; deeper is per-arm detail.
// A candidate whose guard lines cannot be located reports 0: the registry
// never demotes on missing evidence.
func (h *axisHarvest) nestOf(a *DispatchAxis) int {
	best := -1
	for _, l := range a.GuardLines {
		if d, ok := h.guardNest[l]; ok && (best < 0 || d < best) {
			best = d
		}
	}
	if best < 0 {
		return 0
	}
	return best
}

// candidates returns every qualifying candidate in dispatch-axis order —
// the single home for the ranking both the slicer and the registry read.
//
// The recognizer cascade's three groups come first, in priority order
// (normalize-chain, direct compare, char-compare), each ranked by its own
// rubric; the concatenation is then STABLY re-ordered so a shallower-nested
// guard wins. Depth leads because a nested chain is per-arm API detail, not
// the entry's spine: it must not outrank a top-level candidate however many
// values it carries. Stability is what preserves the cascade — within a
// group the group's own rubric still decides, and across groups at equal
// depth the earlier (higher-priority) recognizer still wins, exactly as
// before depth entered the rubric.
func (h *axisHarvest) candidates() []axisCandidate {
	// Recognizer 2's two spellings contest on one rubric: most distinct
	// values, then sites, then identifier (betterDirectAxis's order).
	direct := append(
		collectAxes(h.refs, h.links, h.idents, byValuesWeight),
		collectAxes(h.symbs, h.links, h.idents, byValuesWeight)...,
	)
	sort.SliceStable(direct, func(i, j int) bool {
		di, dj := len(direct[i].axis.Domain), len(direct[j].axis.Domain)
		if di != dj {
			return di > dj
		}
		if direct[i].axis.Sites != direct[j].axis.Sites {
			return direct[i].axis.Sites > direct[j].axis.Sites
		}
		return direct[i].axis.RefName < direct[j].axis.RefName
	})
	all := make([]axisCandidate, 0, len(direct)+4)
	all = append(all, collectAxes(h.refs, h.links, h.idents, normalizeWeightOf(h.links))...)
	all = append(all, direct...)
	all = append(all, collectAxes(h.idents, h.links, h.idents, charCompareWeight)...)
	sort.SliceStable(all, func(i, j int) bool {
		return h.nestOf(all[i].axis) < h.nestOf(all[j].axis)
	})
	return all
}

// best is the entry's dispatch axis: the first candidate — the API-level
// (shallowest) spine, recognizer priority and each recognizer's own rubric
// breaking ties. nil when nothing qualifies.
func (h *axisHarvest) best() *DispatchAxis {
	c := h.candidates()
	if len(c) == 0 {
		return nil
	}
	return c[0].axis
}

// ranked returns every qualifying axis in registry order — the same
// candidate ordering the slicer reads (candidates), so rank 0 and
// DispatchAxisFor cannot disagree. One logical axis appears once: the first
// spelling wins and later candidates sharing its scenario key, ref, or alias
// are the same spine (the normalize ref, its alias, and their char compares
// collapse to one entry).
func (h *axisHarvest) ranked() []*DispatchAxis {
	var out []*DispatchAxis
	seenKeys := map[string]bool{}
	seenRefs := map[string]bool{}
	add := func(a *DispatchAxis) {
		if a == nil || len(a.Domain) < 2 || a.Key() == "" {
			return
		}
		if seenKeys[a.Key()] || seenRefs[a.Ref] {
			return
		}
		if a.Alias != "" && seenRefs[a.Alias] {
			return
		}
		seenKeys[a.Key()] = true
		seenRefs[a.Ref] = true
		if a.Alias != "" {
			seenRefs[a.Alias] = true
		}
		if a.Normalized {
			// Every alias of the normalize ref spells the same spine —
			// collapse the alternates with it.
			for al := range h.links[a.Ref] {
				seenRefs[al] = true
			}
		}
		out = append(out, a)
	}

	for _, c := range h.candidates() {
		add(c.axis)
	}
	return out
}

// AxesFor returns the entry's ranked dispatch-axis registry (scenario-filter
// plan §3): rank 0 is exactly DispatchAxisFor's result when that detects an
// axis (structurally forced, so the filter feature can never drift from the
// existing slicer), and every other qualifying axis follows — secondary
// axes nested inside a primary arm's span included (e.g. `new_flag` inside
// `c_flag == 'H'`). Every entry carries GuardLines, the tree-derived
// HasDefault mark, and Kind. nil when nothing qualifies.
func (t *Tree) AxesFor(src []byte) []*DispatchAxis {
	if t == nil || t.facts == nil || t.Function == "" {
		return nil
	}
	h := harvestAxes(src, t.facts, t.Function, t.irFile)
	if h == nil {
		return nil
	}
	ranked := h.ranked()
	primary := t.DispatchAxisFor(src)
	if primary != nil {
		out := make([]*DispatchAxis, 0, len(ranked)+1)
		out = append(out, primary) // carries DispatchAxisFor's own HasDefault
		for _, a := range ranked {
			if a.Ref == primary.Ref && a.Alias == primary.Alias {
				continue
			}
			out = append(out, a)
		}
		ranked = out
	}
	if len(ranked) == 0 {
		return nil
	}
	for i, a := range ranked {
		if i == 0 && primary != nil {
			continue // rank 0 is DispatchAxisFor's result verbatim
		}
		// Secondary axes dispatch inside another arm's span; the
		// top-level-only mark would miss their else arm.
		a.HasDefault = hasAxisDefaultDeep(t.Root, a.Ref, a.Alias)
	}
	classifyAxisKinds(ranked, t.facts.Branches, t.Function)
	return ranked
}

// hasAxisDefaultDeep is hasAxisDefault generalized to nested chains (the
// registry's secondary axes live there): the same dispatch-chain test,
// recursing into every arm's children. hasAxisDefault itself stays
// top-level-only — it is pinned to DispatchAxisFor's existing behavior.
func hasAxisDefaultDeep(nodes []*Node, ref, alias string) bool {
	touches := func(n *Node) bool {
		return n.Predicate != nil && exprTouchesAxis(n.Predicate, ref, alias)
	}
	for i := 0; i < len(nodes); {
		end := chainExtent(nodes, i)
		if nodes[i].Kind == KindBranch && nodes[i].Sub == string(scanner.BranchIf) {
			dispatch := false
			for _, m := range nodes[i:end] {
				if touches(m) {
					dispatch = true
					break
				}
			}
			if dispatch && nodes[end-1].Sub == string(scanner.BranchElse) {
				return true
			}
		}
		for _, m := range nodes[i:end] {
			if hasAxisDefaultDeep(m.Children, ref, alias) {
				return true
			}
		}
		i = end
	}
	return false
}

// classifyAxisKinds marks rank 0 primary and any axis whose shallowest
// guard sits nested deeper than the primary's (facts.Branches NestDepth) as
// secondary. A candidate with no locatable guard line stays primary: the
// registry never demotes on missing evidence.
func classifyAxisKinds(axes []*DispatchAxis, branches []scanner.Branch, entry string) {
	nest := map[int]int{}
	for i := range branches {
		b := &branches[i]
		if b.Function == entry {
			nest[b.StartLine] = b.NestDepth
		}
	}
	minNest := func(a *DispatchAxis) (int, bool) {
		best, found := 0, false
		for _, l := range a.GuardLines {
			n, ok := nest[l]
			if !ok {
				continue
			}
			if !found || n < best {
				best, found = n, true
			}
		}
		return best, found
	}
	pNest, pFound := 0, false
	if len(axes) > 0 {
		pNest, pFound = minNest(axes[0])
	}
	for i, a := range axes {
		if i == 0 {
			a.Kind = AxisPrimary
			continue
		}
		if n, ok := minNest(a); ok && pFound && n > pNest {
			a.Kind = AxisSecondary
		} else {
			a.Kind = AxisPrimary
		}
	}
}
