package flow

import (
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/pred"
	scanner "tux-migration-tool/internal/tsscan"
)

// This file implements the scenarioFilter feature (docs/scenario-filter-plan.md
// §1-§4): a boolean over registered dispatch axes that resolves to a set of
// matching assignments, and a re-fold of the tree under that set — never the
// union of separately-folded slices. `&&` intersects (one combined
// assignment), `||` unions (guards kept per-assignment), `!=`/`!` compose.
// A branch guard is satisfied only when true under EVERY matching
// assignment, contradicted only when false under every one; anything else
// stays live verbatim. A matching assignment is kept only when the source
// structure witnesses each asserted `var=value` (a guard of that value, or
// the reachable default arm) — the reachability prune.

const (
	// maxFilterTerms caps the filter's DNF size (distribution blow-up).
	maxFilterTerms = 32
	// maxFilterAssignments caps the matching-assignment enumeration.
	maxFilterAssignments = 25
)

// ScenarioFilter is a parsed, validated scenarioFilter expression. The v1
// grammar: axis identifiers, char/string/number literals, ==, !=, &&, ||,
// !, and parens — everything else is a hard reject.
type ScenarioFilter struct {
	// Text is the user's expression verbatim (the audit trail).
	Text string
	// Vars are the axis identifiers referenced, first-use order.
	Vars  []string
	terms []filterTerm
}

// filterTerm is one AND term of the filter's DNF: the literals that must
// hold together.
type filterTerm struct {
	literals []filterLiteral
}

// filterLiteral is one `var ==/!= value` atom.
type filterLiteral struct {
	varName string
	op      string // "==" or "!="
	value   string
}

// ParseScenarioFilter parses and validates a scenarioFilter expression.
// Grammar failures and unsupported shapes (>, <, arithmetic, calls, bare
// idents) are positioned rejects naming the offending atom.
func ParseScenarioFilter(text string) (*ScenarioFilter, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("scenarioFilter must not be empty")
	}
	e := pred.Parse(trimmed)
	if e.IsRaw() {
		return nil, fmt.Errorf("scenarioFilter %q: unsupported expression (v1: ident ==/!= literal, &&, ||, !, parens)", trimmed)
	}
	terms, err := filterDNF(&e, trimmed)
	if err != nil {
		return nil, err
	}
	f := &ScenarioFilter{Text: trimmed, terms: terms}
	seen := map[string]bool{}
	var walk func(e *pred.Expr)
	walk = func(e *pred.Expr) {
		if e == nil {
			return
		}
		switch e.Kind {
		case pred.KindOr, pred.KindAnd:
			for i := range e.Items {
				walk(&e.Items[i])
			}
		case pred.KindNot:
			walk(e.Inner)
		case pred.KindCmp:
			walk(e.L)
			walk(e.R)
		case pred.KindIdent:
			if !seen[e.Name] {
				seen[e.Name] = true
				f.Vars = append(f.Vars, e.Name)
			}
		}
	}
	walk(&e)
	return f, nil
}

// filterDNF rewrites e to disjunctive normal form of `var ==/!= literal`
// atoms. `!` is pushed to the atoms (De Morgan); the term count is capped.
func filterDNF(e *pred.Expr, text string) ([]filterTerm, error) {
	if e == nil {
		return nil, fmt.Errorf("scenarioFilter %q: empty expression", text)
	}
	switch e.Kind {
	case pred.KindOr:
		var out []filterTerm
		for i := range e.Items {
			ts, err := filterDNF(&e.Items[i], text)
			if err != nil {
				return nil, err
			}
			out = append(out, ts...)
			if len(out) > maxFilterTerms {
				return nil, fmt.Errorf("scenarioFilter %q expands beyond %d terms — simplify it", text, maxFilterTerms)
			}
		}
		return out, nil
	case pred.KindAnd:
		return filterCrossDNF(e.Items, text)
	case pred.KindNot:
		return filterNotDNF(e.Inner, text)
	case pred.KindCmp:
		lit, err := filterAtom(e, text)
		if err != nil {
			return nil, err
		}
		return []filterTerm{{literals: []filterLiteral{lit}}}, nil
	default:
		return nil, filterUnsupported(e, text)
	}
}

// filterCrossDNF distributes && over the already-DNF items.
func filterCrossDNF(items []pred.Expr, text string) ([]filterTerm, error) {
	acc := []filterTerm{{}}
	for i := range items {
		ts, err := filterDNF(&items[i], text)
		if err != nil {
			return nil, err
		}
		var next []filterTerm
		for _, a := range acc {
			for _, t := range ts {
				lits := make([]filterLiteral, 0, len(a.literals)+len(t.literals))
				lits = append(lits, a.literals...)
				lits = append(lits, t.literals...)
				next = append(next, filterTerm{literals: lits})
				if len(next) > maxFilterTerms {
					return nil, fmt.Errorf("scenarioFilter %q expands beyond %d terms — simplify it", text, maxFilterTerms)
				}
			}
		}
		acc = next
	}
	return acc, nil
}

// filterNotDNF pushes a negation down to the atoms: !(a&&b) = !a||!b,
// !(a||b) = !a&&!b, !(x==v) = x!=v, !(x!=v) = x==v, !!a = a.
func filterNotDNF(e *pred.Expr, text string) ([]filterTerm, error) {
	if e == nil {
		return nil, fmt.Errorf("scenarioFilter %q: empty negation", text)
	}
	switch e.Kind {
	case pred.KindAnd:
		var out []filterTerm
		for i := range e.Items {
			ts, err := filterNotDNF(&e.Items[i], text)
			if err != nil {
				return nil, err
			}
			out = append(out, ts...)
			if len(out) > maxFilterTerms {
				return nil, fmt.Errorf("scenarioFilter %q expands beyond %d terms — simplify it", text, maxFilterTerms)
			}
		}
		return out, nil
	case pred.KindOr:
		neg := make([]pred.Expr, len(e.Items))
		for i := range e.Items {
			not := pred.Expr{Kind: pred.KindNot, Inner: &e.Items[i]}
			neg[i] = not
		}
		return filterCrossDNF(neg, text)
	case pred.KindNot:
		return filterDNF(e.Inner, text)
	case pred.KindCmp:
		lit, err := filterAtom(e, text)
		if err != nil {
			return nil, err
		}
		if lit.op == "==" {
			lit.op = "!="
		} else {
			lit.op = "=="
		}
		return []filterTerm{{literals: []filterLiteral{lit}}}, nil
	default:
		return nil, filterUnsupported(e, text)
	}
}

// filterAtom validates one atom: `ident ==/!= literal` (either side order).
func filterAtom(e *pred.Expr, text string) (filterLiteral, error) {
	if e.Kind != pred.KindCmp || (e.Op != "==" && e.Op != "!=") {
		return filterLiteral{}, filterUnsupported(e, text)
	}
	l, r := e.L, e.R
	if l.Kind == pred.KindLit && r.Kind == pred.KindIdent {
		l, r = r, l
	}
	if l.Kind != pred.KindIdent || r.Kind != pred.KindLit || !pred.IsLitText(r.Text) {
		return filterLiteral{}, fmt.Errorf("scenarioFilter %q: unsupported atom %q (v1: ident ==/!= literal only)",
			text, oneLineC(e.String()))
	}
	return filterLiteral{varName: l.Name, op: e.Op, value: unquote(r.Text)}, nil
}

func filterUnsupported(e *pred.Expr, text string) error {
	return fmt.Errorf("scenarioFilter %q: unsupported atom %q (v1: ident ==/!= literal, &&, ||, !, parens)",
		text, oneLineC(e.String()))
}

// filterAssignment is one matching combination of axis values that must be
// folded together. env carries the fold bindings; witness collects, during
// the dry walk, which asserted values the source structure actually reaches.
type filterAssignment struct {
	key        string
	vars       []string // asserted identifiers, sorted
	values     map[string]string
	env        foldAssumptions
	witness    map[string]bool
	hasDefault bool
}

// missingWitness returns an unwitnessed asserted identifier ("" when the
// assignment is reachable).
func (a *filterAssignment) missingWitness() string {
	for _, v := range a.vars {
		if !a.witness[v] {
			return v
		}
	}
	return ""
}

// filterConstraint is one term var's admitted-value set, intersected across
// the term's literals.
type filterConstraint struct {
	name        string
	axis        *DispatchAxis
	allowed     map[string]bool
	initialized bool
}

// assignments expands the filter's DNF terms into matching assignments over
// the registry: each term's vars enumerate the values satisfying its
// literals (== pins, != excludes), cartesian across vars, capped. Unknown
// axes, out-of-domain values, contradictory literals and overflow are hard
// errors naming the offender — never an empty endpoint.
func (f *ScenarioFilter) assignments(registry []*DispatchAxis) ([]*filterAssignment, error) {
	byKey := map[string]*DispatchAxis{}
	for _, a := range registry {
		byKey[a.Key()] = a
	}
	var out []*filterAssignment
	index := map[string]int{}
	for _, t := range f.terms {
		var order []string
		cons := map[string]*filterConstraint{}
		for _, lit := range t.literals {
			axis := byKey[lit.varName]
			if axis == nil {
				return nil, fmt.Errorf("scenarioFilter %q: unknown axis %q%s",
					f.Text, lit.varName, suggestAxisVar(lit.varName, registry))
			}
			c, ok := cons[lit.varName]
			if !ok {
				c = &filterConstraint{name: lit.varName, axis: axis, allowed: map[string]bool{}}
				cons[lit.varName] = c
				order = append(order, lit.varName)
			}
			vals, err := filterConstraintValues(axis, lit)
			if err != nil {
				return nil, fmt.Errorf("scenarioFilter %q: %w", f.Text, err)
			}
			if !c.initialized {
				for v := range vals {
					c.allowed[v] = true
				}
				c.initialized = true
				continue
			}
			for v := range c.allowed {
				if !vals[v] {
					delete(c.allowed, v)
				}
			}
		}
		for _, name := range order {
			if len(cons[name].allowed) == 0 {
				return nil, fmt.Errorf("scenarioFilter %q: contradictory literals for %s (domain %s)",
					f.Text, name, axisDomainDisplay(cons[name].axis))
			}
		}
		total := 1
		for _, name := range order {
			total *= len(cons[name].allowed)
		}
		if len(out)+total > maxFilterAssignments {
			return nil, fmt.Errorf("scenarioFilter %q expands to more than %d assignments — narrow it",
				f.Text, maxFilterAssignments)
		}
		valLists := make([][]string, len(order))
		for i, name := range order {
			valLists[i] = sortedKeys(cons[name].allowed)
		}
		idx := make([]int, len(order))
		for {
			values := map[string]string{}
			for i, name := range order {
				values[name] = valLists[i][idx[i]]
			}
			key, vars, env, hasDefault := filterAssignmentShape(order, values, cons)
			if _, ok := index[key]; !ok {
				index[key] = len(out)
				out = append(out, &filterAssignment{
					key: key, vars: vars, values: values, env: env,
					witness: map[string]bool{}, hasDefault: hasDefault,
				})
			}
			i := len(order) - 1
			for i >= 0 {
				idx[i]++
				if idx[i] < len(valLists[i]) {
					break
				}
				idx[i] = 0
				i--
			}
			if i < 0 {
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// filterConstraintValues returns the values one literal admits within the
// axis universe (domain ∪ default key).
func filterConstraintValues(axis *DispatchAxis, lit filterLiteral) (map[string]bool, error) {
	universe := axisUniverse(axis)
	if !universe[lit.value] {
		return nil, fmt.Errorf("%s %s %q matches nothing (domain %s)",
			lit.varName, lit.op, lit.value, axisDomainDisplay(axis))
	}
	vals := map[string]bool{}
	if lit.op == "==" {
		vals[lit.value] = true
		return vals, nil
	}
	for v := range universe {
		if v != lit.value {
			vals[v] = true
		}
	}
	return vals, nil
}

// filterAssignmentShape composes the canonical key, sorted asserted vars,
// fold environment and default mark for one value combination.
func filterAssignmentShape(order []string, values map[string]string, cons map[string]*filterConstraint) (string, []string, foldAssumptions, bool) {
	sorted := append([]string(nil), order...)
	sort.Strings(sorted)
	var keyParts []string
	var env foldAssumptions
	hasDefault := false
	for _, name := range sorted {
		v := values[name]
		axis := cons[name].axis
		keyParts = append(keyParts, name+"="+v)
		env = append(env, axisAssumption{Key: name, Ref: axis.Ref, Alias: axis.Alias, Value: v})
		if axis.HasDefault && v == axis.DefaultKey() {
			hasDefault = true
		}
	}
	return strings.Join(keyParts, " && "), sorted, env, hasDefault
}

// axisUniverse is the axis's scenario values: the enumerated domain plus
// the default arm's key when the chain ends in an else.
func axisUniverse(axis *DispatchAxis) map[string]bool {
	out := map[string]bool{}
	for _, v := range axis.Domain {
		out[v] = true
	}
	if axis.HasDefault {
		out[axis.DefaultKey()] = true
	}
	return out
}

// axisDomainDisplay renders the domain for error messages, default
// included ("F,H,I,default").
func axisDomainDisplay(axis *DispatchAxis) string {
	vals := append([]string(nil), axis.Domain...)
	if axis.HasDefault {
		vals = append(vals, axis.DefaultKey())
	}
	return strings.Join(vals, ",")
}

// suggestAxisVar offers the closest registry key for an unknown identifier
// (edit distance ≤ 2, case-insensitive), with its domain.
func suggestAxisVar(name string, registry []*DispatchAxis) string {
	lower := strings.ToLower(name)
	bestKey, bestDist := "", 3
	for _, a := range registry {
		d := filterEditDistance(lower, strings.ToLower(a.Key()))
		if d < bestDist {
			bestKey, bestDist = a.Key(), d
		}
	}
	if bestKey == "" {
		if len(registry) == 0 {
			return " (no dispatch axes detected)"
		}
		keys := make([]string, 0, len(registry))
		for _, a := range registry {
			keys = append(keys, a.Key())
		}
		sort.Strings(keys)
		return " (registry: " + strings.Join(keys, ", ") + ")"
	}
	for _, a := range registry {
		if a.Key() == bestKey {
			return fmt.Sprintf(" — did you mean %q (domain %s)?", bestKey, axisDomainDisplay(a))
		}
	}
	return ""
}

// filterEditDistance is the Levenshtein distance (small identifiers only).
func filterEditDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// envTruth is one environment's fold verdict for a predicate.
type envTruth struct {
	truth tribool
	ok    bool
}

// filterFold is the env-aware scenario walk: one fold pass over the tree
// with several simultaneous assignments.
type filterFold struct {
	tree    *Tree
	sc      *Scenario
	assigns []*filterAssignment
	envs    []foldAssumptions
}

func newFilterFold(tree *Tree, assigns []*filterAssignment) *filterFold {
	envs := make([]foldAssumptions, len(assigns))
	for i, a := range assigns {
		envs[i] = a.env
	}
	return &filterFold{tree: tree, assigns: assigns, envs: envs}
}

// touchesAny reports whether the predicate references any assumed axis.
func (ff *filterFold) touchesAny(e *pred.Expr) bool {
	for _, fs := range ff.envs {
		if exprTouchesAny(e, fs) {
			return true
		}
	}
	return false
}

// truths evaluates the predicate under every assignment.
func (ff *filterFold) truths(e *pred.Expr) []envTruth {
	out := make([]envTruth, len(ff.envs))
	for k := range ff.envs {
		_, t, ok := foldExpr(e, ff.envs[k])
		out[k] = envTruth{truth: t, ok: ok}
	}
	return out
}

// walk folds one sibling run. The chain rules mirror ScenarioFor, lifted
// per assignment: an arm is dropped only when unreachable under every
// assignment, the default arm is dropped only when every assignment is
// already taken, and a guard is commented only when provably true under
// every assignment. reach[k] tracks whether the subtree is actually
// reachable under assignment k — fold verdicts stay global (that is what
// keeps every union arm's guard live), but witness evidence and the
// chain-exclusivity marks are gated by it, so a value compared in an arm
// another assignment reaches can never witness the arm's own assignment
// (the cross-talk that let `(H && K) || (default && K)` pass the prune).
func (ff *filterFold) walk(nodes []*Node, reach []bool) []*SliceNode {
	var out []*SliceNode
	for i := 0; i < len(nodes); {
		end := chainExtent(nodes, i)
		dispatch := false
		if nodes[i].Kind == KindBranch && nodes[i].Sub == string(scanner.BranchIf) {
			for _, m := range nodes[i:end] {
				if m.Predicate != nil && ff.touchesAny(m.Predicate) {
					dispatch = true
					break
				}
			}
		}
		// chainVars[k] = asserted vars this chain tests under assignment k
		// (the default arm's witness when it is the only reached arm).
		chainVars := make([]map[string]bool, len(ff.envs))
		for k := range chainVars {
			chainVars[k] = map[string]bool{}
		}
		if dispatch {
			for _, m := range nodes[i:end] {
				if m.Predicate == nil {
					continue
				}
				for k := range ff.envs {
					for _, a := range ff.envs[k] {
						if exprTouchesAxis(m.Predicate, a.Ref, a.Alias) {
							chainVars[k][a.Key] = true
						}
					}
				}
			}
		}
		taken := make([]bool, len(ff.envs))
		for _, m := range nodes[i:end] {
			armReach := reachedUnder(reach, taken)
			if dispatch && m.Sub == string(scanner.BranchElse) {
				if !anyTrue(armReach) {
					ff.sc.Counts.Dropped++
					ff.sc.Counts.DroppedLines = append(ff.sc.Counts.DroppedLines, spanLines(m.Line, m.EndLine)...)
					continue
				}
				for k := range ff.envs {
					if !armReach[k] {
						continue
					}
					for v := range chainVars[k] {
						ff.assigns[k].witness[v] = true
					}
				}
				sn := &SliceNode{Kind: m.Kind, Sub: m.Sub, Line: m.Line, EndLine: m.EndLine, Cond: m.Cond, Fold: FoldKept}
				sn.Children = ff.walk(m.Children, armReach)
				ff.sc.Counts.Kept++
				out = append(out, sn)
				continue
			}
			sn, trueEnvs := ff.foldArm(m, armReach)
			if sn != nil {
				out = append(out, sn)
			}
			for k, t := range trueEnvs {
				if t {
					taken[k] = true
				}
			}
		}
		i = end
	}
	return out
}

// reachedUnder returns the arms still reachable per assignment: the outer
// reach mask minus the assignments whose earlier chain arm already matched.
func reachedUnder(reach, taken []bool) []bool {
	out := make([]bool, len(reach))
	for k := range reach {
		out[k] = reach[k] && !taken[k]
	}
	return out
}

// reachAll is the entry reach mask: every assignment can reach the root.
func reachAll(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

// anyTrue reports whether the mask has at least one set entry.
func anyTrue(mask []bool) bool {
	for _, v := range mask {
		if v {
			return true
		}
	}
	return false
}

// foldArm folds one chain member under every assignment. reach[k] marks the
// assignments whose control flow actually arrives at this member; the fold
// verdict is global (a guard that is not true under every assignment stays
// live), while witness evidence and the chain-exclusivity signal are
// per-assignment and reach-gated. A dropped arm returns nil.
func (ff *filterFold) foldArm(n *Node, reach []bool) (*SliceNode, []bool) {
	sn := &SliceNode{Kind: n.Kind, Sub: n.Sub, Line: n.Line, EndLine: n.EndLine, Cond: n.Cond}
	trueEnvs := make([]bool, len(ff.envs))
	switch {
	case n.Kind != KindBranch || n.Predicate == nil:
		sn.Fold = FoldKept
		sn.Children = ff.walk(n.Children, reach)
		ff.sc.Counts.Kept++
		return sn, trueEnvs
	case !ff.touchesAny(n.Predicate):
		// Never touches an assumed axis — verbatim, never residue.
		sn.Fold = FoldKept
		sn.Children = ff.walk(n.Children, reach)
		ff.sc.Counts.Kept++
		return sn, trueEnvs
	}
	truths := ff.truths(n.Predicate)
	allTrue, allFalse, anyUnknown := true, true, false
	bodyReach := make([]bool, len(ff.envs))
	for k, et := range truths {
		if !et.ok {
			anyUnknown = true
		}
		if !(et.ok && et.truth == triTrue) {
			allTrue = false
		} else if reach[k] {
			// An arm can be provably true under one assignment only — the
			// per-assignment chain-exclusivity signal.
			trueEnvs[k] = true
		}
		if !(et.ok && et.truth == triFalse) {
			allFalse = false
		}
		if reach[k] && !(et.ok && et.truth == triFalse) {
			bodyReach[k] = true
		}
	}
	// Witness: a non-contradicted verdict in a reachable context under an
	// assignment is structural evidence that the assignment's values occur
	// there. An unreachable context proves nothing — its guards may belong
	// to another assignment's arm.
	for k, et := range truths {
		if !reach[k] || (et.ok && et.truth == triFalse) {
			continue
		}
		for _, a := range ff.envs[k] {
			if exprTouchesAxis(n.Predicate, a.Ref, a.Alias) {
				ff.assigns[k].witness[a.Key] = true
			}
		}
	}
	switch {
	case allTrue:
		sn.Fold = FoldSatisfied
		sn.Children = ff.walk(n.Children, bodyReach)
		ff.sc.Counts.Kept++
		return sn, trueEnvs
	case allFalse:
		ff.sc.Counts.Dropped++
		ff.sc.Counts.DroppedLines = append(ff.sc.Counts.DroppedLines, spanLines(n.Line, n.EndLine)...)
		return nil, trueEnvs
	case anyUnknown:
		sn.Fold = FoldUnrecognized
		ff.sc.Counts.Unfolded++
		ff.sc.Residue = append(ff.sc.Residue, fmt.Sprintf("L%d: %s", n.Line, oneLineC(n.Cond)))
		sn.Children = ff.walk(n.Children, bodyReach)
		ff.sc.Counts.Kept++
		return sn, trueEnvs
	default:
		// Mixed across the assignments: the guard must stay live, verbatim
		// (axis terms are never stripped — runtime dispatch is preserved).
		sn.Fold = FoldMixed
		sn.FoldedCond = n.Cond
		sn.Children = ff.walk(n.Children, bodyReach)
		ff.sc.Counts.Kept++
		return sn, trueEnvs
	}
}

// ScenarioForFilter slices the tree under a parsed scenarioFilter (see the
// file header). registry is Tree.AxesFor's ranked registry; assignments are
// pruned to those the source structure witnesses (a guard of each asserted
// value, or the reachable default arm), and the surviving set folds the
// tree. Every-pruned and no-body cases are hard errors naming the filter —
// never an empty endpoint.
func ScenarioForFilter(tree *Tree, registry []*DispatchAxis, filter *ScenarioFilter) (*Scenario, error) {
	if tree == nil || filter == nil {
		return nil, fmt.Errorf("scenarioFilter: no tree or filter")
	}
	assigns, err := filter.assignments(registry)
	if err != nil {
		return nil, err
	}
	if len(assigns) == 0 {
		return nil, fmt.Errorf("scenarioFilter %q matches no assignment over the detected axes", filter.Text)
	}
	// Pass 1: dry walk collects per-assignment witnesses.
	dry := newFilterFold(tree, assigns)
	dry.sc = &Scenario{}
	_ = dry.walk(tree.Root, reachAll(len(assigns)))

	var live []*filterAssignment
	var lost []string
	for _, a := range assigns {
		if missing := a.missingWitness(); missing != "" {
			lost = append(lost, fmt.Sprintf("%s (no reachable %s)", a.key, missing))
			continue
		}
		live = append(live, a)
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("scenarioFilter %q matches no reachable arm in %s — every matching assignment is contradicted: %s",
			filter.Text, tree.Function, strings.Join(lost, ", "))
	}

	// Pass 2: fold under the surviving assignments.
	sc := &Scenario{}
	ff := newFilterFold(tree, live)
	ff.sc = sc
	key, varName, value := filterScenarioIdentity(live)
	sc.Key, sc.Var, sc.Value = key, varName, value
	sc.Filter = filter.Text
	for _, a := range live {
		sc.FilterMatched = append(sc.FilterMatched, a.key)
		if a.hasDefault {
			sc.Default = true
		}
	}
	sc.FilterPruned = lost
	split := len(tree.Root)
	for i := 0; i < len(tree.Root) && split == len(tree.Root); {
		end := chainExtent(tree.Root, i)
		for _, m := range tree.Root[i:end] {
			if nodeTouchesAny(m, ff.envs) {
				split = i
				break
			}
		}
		i = end
	}
	for _, n := range tree.Root[:split] {
		sc.Preamble = append(sc.Preamble, n.Line, n.EndLine)
	}
	sc.Body = ff.walk(tree.Root[split:], reachAll(len(live)))
	sc.Tail = tailSpans(sc, tree.Root, split, func(e *pred.Expr) bool {
		for _, fs := range ff.envs {
			if exprTouchesAny(e, fs) {
				return true
			}
		}
		return false
	})
	censusOf(sc, tree)
	if len(sc.Body) == 0 {
		return nil, fmt.Errorf("scenarioFilter %q leaves no body in %s — every branch was contradicted", filter.Text, tree.Function)
	}
	return sc, nil
}

// nodeTouchesAny reports whether the node's own predicate or any
// descendant's references an assumed axis. The preamble split needs the
// recursive form: a secondary axis's guards nest inside an outer chain
// whose own predicate names a different axis.
func nodeTouchesAny(n *Node, envs []foldAssumptions) bool {
	if n.Predicate != nil {
		for _, fs := range envs {
			if exprTouchesAny(n.Predicate, fs) {
				return true
			}
		}
	}
	for _, c := range n.Children {
		if nodeTouchesAny(c, envs) {
			return true
		}
	}
	return false
}

// filterScenarioIdentity derives the Scenario key (the artifact's merged
// name): a single assignment keeps its `<var>=<value>` form; a multi-value
// set merges per var (`c_flag in {F,I}`; several vars joined by ` && `).
// Var/Value feed the artifact file name (`c_flag_F_or_I.pc`).
func filterScenarioIdentity(assigns []*filterAssignment) (key, varName, value string) {
	var vars []string
	seenVar := map[string]bool{}
	vals := map[string]map[string]bool{}
	for _, a := range assigns {
		for _, v := range a.vars {
			if !seenVar[v] {
				seenVar[v] = true
				vars = append(vars, v)
			}
		}
		for v, val := range a.values {
			if vals[v] == nil {
				vals[v] = map[string]bool{}
			}
			vals[v][val] = true
		}
	}
	sort.Strings(vars)
	if len(assigns) == 1 && len(vars) == 1 {
		return assigns[0].key, vars[0], assigns[0].values[vars[0]]
	}
	if len(vars) == 1 {
		vs := sortedKeys(vals[vars[0]])
		if len(vs) == 1 {
			return vars[0] + "=" + vs[0], vars[0], vs[0]
		}
		return vars[0] + " in {" + strings.Join(vs, ",") + "}", vars[0], strings.Join(vs, "_or_")
	}
	var parts, fileParts []string
	for _, v := range vars {
		vs := sortedKeys(vals[v])
		if len(vs) == 1 {
			parts = append(parts, v+"="+vs[0])
			fileParts = append(fileParts, v+"_"+vs[0])
			continue
		}
		parts = append(parts, v+" in {"+strings.Join(vs, ",")+"}")
		fileParts = append(fileParts, v+"_"+strings.Join(vs, "_or_"))
	}
	return strings.Join(parts, " && "), "filter", strings.Join(fileParts, "_and_")
}
