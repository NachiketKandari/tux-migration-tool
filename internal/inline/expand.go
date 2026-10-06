package inline

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/ir"
)

// pending is one helper waiting to be materialized. The chain is the
// helper-of-helper path that led here, which is what makes a cycle
// detectable rather than merely depth-bounded.
type pending struct {
	name  string
	depth int
	via   string
	chain []string
}

// Expand materializes every resolvable cross-file helper the caller calls
// into the caller's own source, and returns the expanded text together with
// the IR re-folded from it.
//
// The walk is a breadth-first traversal of the corpus graph, and every part
// of it is pinned so the result is a function of the inputs alone:
//
//   - The frontier is a FIFO queue seeded from caller.ExternalFns in
//     sorted-name order (ir already sorts them; Expand re-sorts defensively
//     so the output does not depend on how the caller's IR was built).
//   - A pulled-in helper's own externals are enqueued in sorted-name order
//     at depth+1.
//   - Each helper is appended at most once, in first-visit order.
//   - Depth is bounded by Options.MaxDepth, and a helper already on the
//     current chain is refused as a cycle rather than followed.
//
// Nothing consults the clock, a map iteration order, or an LLM, so identical
// inputs produce byte-identical output. A helper that cannot be materialized
// keeps its existing plan-layer handling and appears in Result.Skipped with
// a reason; it is never guessed at.
func Expand(caller *ir.File, corpus *Corpus, opts Options) (*Result, error) {
	if caller == nil {
		return nil, errors.New("inline: caller IR is required")
	}
	if corpus == nil {
		return nil, errors.New("inline: corpus is required")
	}
	src, ok := corpus.Source(caller.Path)
	if !ok {
		return nil, fmt.Errorf("inline: no source text for %s in the corpus", caller.Path)
	}
	callerFacts, _, err := corpus.scanFacts(caller.Path)
	if err != nil {
		return nil, fmt.Errorf("inline: scanning %s: %w", caller.Path, err)
	}

	lines := strings.Split(src, "\n")
	base := contentLines(lines)
	res := &Result{
		Path:          caller.Path,
		OriginalLines: base,
		Sites:         []Site{},
		Skips:         []Skip{},
	}
	// The caller's own file-scope names are never re-declared, and its
	// own function names are never appended over.
	have := fileScopeNames(callerFacts)
	ownFns := make(map[string]bool, len(caller.Functions))
	for _, fn := range caller.Functions {
		ownFns[fn] = true
	}

	appended := make([]string, 0, 64)
	appendedFns := make(map[string]bool, 8)
	maxDepth := opts.maxDepth()

	queue := seed(caller.ExternalFns)

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		// A cycle is checked FIRST, ahead of the already-materialized
		// test. fn_a → fn_b → fn_a would otherwise be absorbed by the
		// dedup and reported as nothing, and that is the one case a user
		// genuinely needs to see. A diamond (two helpers pulling the same
		// third) is not a cycle and still dedups silently below.
		if onChain(item.chain, item.name) {
			res.Skips = append(res.Skips, Skip{
				Fn: item.name, Code: SkipCycle, Depth: item.depth, Via: item.via,
				Detail: "cycle: " + strings.Join(append(item.chain, item.name), " → "),
			})
			continue
		}
		if appendedFns[item.name] {
			// Already materialized through another path. Benign — a
			// helper two of the caller's helpers share — so it is not a
			// skip; the caller can see the single Site that covers it.
			continue
		}
		if ownFns[item.name] {
			res.Skips = append(res.Skips, Skip{
				Fn: item.name, Code: SkipDuplicate, Depth: item.depth, Via: item.via,
				Detail: "the caller defines " + item.name + " itself — nothing to inline",
			})
			continue
		}
		if item.depth > maxDepth {
			res.Skips = append(res.Skips, Skip{
				Fn: item.name, Code: SkipMaxDepth, Depth: item.depth, Via: item.via,
				Detail: fmt.Sprintf("helper-of-helper depth %d exceeds the limit of %d", item.depth, maxDepth),
			})
			continue
		}

		cal, skip := corpus.resolveCallee(item.name, item.depth, item.via)
		if skip != nil {
			res.Skips = append(res.Skips, *skip)
			continue
		}

		// Appended line numbers are read off the split rather than off
		// contentLines: a caller that ends its file with a newline splits
		// into a trailing empty element, and joining that back opens one
		// blank line before the first appended line. len(lines)+1 is the
		// line the first appended element actually lands on, for both the
		// newline-terminated and unterminated cases.
		at := len(lines) + 1 + len(appended)
		block := make([]string, 0, 8)
		if picks, refused := pickGlobals(cal, have); len(picks) > 0 {
			block = append(block, globalBlock(picks)...)
			for _, p := range picks {
				res.AppendedGlobals = append(res.AppendedGlobals, p.name)
			}
			for _, name := range refused {
				res.Skips = append(res.Skips, Skip{
					Fn: cal.name, Code: SkipGlobalSpan, Depth: item.depth, Via: item.via,
					Detail: fmt.Sprintf("%s:%d — file-scope %s is declared across lines; declare it in the caller or the expanded helper will not resolve it", cal.path, cal.def.BodyStartLine, name),
				})
			}
		}
		block = append(block, definitionBlock(cal)...)
		appended = append(appended, block...)

		appendedFns[item.name] = true
		ownFns[item.name] = true
		calIR, _ := corpus.File(cal.path)
		res.Sites = append(res.Sites, Site{
			Fn:             cal.name,
			DefinedIn:      cal.path,
			FromLine:       cal.start,
			ToLine:         cal.end,
			AppendedAtLine: at,
			AppendedToLine: at + len(block) - 1,
			Depth:          item.depth,
			Via:            item.via,
			HasSQL:         calIR != nil && hasQueriesOwnedBy(calIR, cal.name),
		})

		queue = append(queue, frontierOf(corpus, cal, item.depth+1, item.name, item.chain)...)
	}

	lines = append(lines, appended...)
	res.Source = strings.Join(lines, "\n")
	res.ExpandedLines = contentLines(strings.Split(res.Source, "\n"))

	folded, err := ir.ExtractSourceOpts([]byte(res.Source), res.Path, opts.irOpts())
	if err != nil {
		return nil, fmt.Errorf("inline: re-folding %s: %w", res.Path, err)
	}
	res.File = folded
	return res, nil
}

// seed turns the caller's external-fn records into the initial frontier, in
// sorted-name order. The sort is redundant against ir.buildExternalFns but
// makes Expand's determinism independent of how the caller's IR was built.
func seed(externals []ir.ExternalFn) []pending {
	names := make([]string, 0, len(externals))
	for _, e := range externals {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	out := make([]pending, 0, len(names))
	for _, n := range names {
		out = append(out, pending{name: n, depth: 1})
	}
	return out
}

// frontierOf enqueues the helpers a materialized definition depends on, so a
// helper-of-a-helper becomes legible in the same expansion.
//
// The dependency set is the calls INSIDE the lifted span that the caller
// would not otherwise have — not the callee file's ExternalFns. Those record
// the symbols the callee's own file leaves UNDEFINED, so a sibling helper
// defined right beside it in that same file is not among them, yet lifting
// fn_outer into a caller that lacks fn_inner would leave the expanded text
// with an undeclared call. So the set is the span's calls filtered to those
// the callee file either defines (lift it here) or records as external
// (resolve it in the corpus). Names matching neither are C library and ATMI
// calls and are ignored.
//
// Sorted by name, at depth+1, carrying the chain so a cycle is detectable
// rather than merely depth-bounded.
func frontierOf(corpus *Corpus, cal *callee, depth int, via string, chain []string) []pending {
	calIR, ok := corpus.File(cal.path)
	if !ok {
		return nil
	}
	local := make(map[string]bool, len(calIR.Functions))
	for _, fn := range calIR.Functions {
		local[fn] = true
	}
	external := make(map[string]bool, len(calIR.ExternalFns))
	for _, e := range calIR.ExternalFns {
		external[e.Name] = true
	}

	var names []string
	for _, name := range spanCalls(cal) {
		if name == cal.name || (!local[name] && !external[name]) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	next := append(append([]string{}, chain...), via)
	out := make([]pending, 0, len(names))
	for _, n := range names {
		out = append(out, pending{name: n, depth: depth, via: via, chain: next})
	}
	return out
}

func onChain(chain []string, name string) bool {
	for _, n := range chain {
		if n == name {
			return true
		}
	}
	return false
}

func hasQueriesOwnedBy(f *ir.File, fn string) bool {
	for _, q := range f.Queries {
		if q.OwningFunction == fn {
			return true
		}
	}
	return false
}

// contentLines counts the content lines of a split source: a trailing
// newline does not open a line. Appended line numbers are computed against
// this so AppendedAtLine is the line a reader actually sees, whether or not
// the caller ended its file with a newline.
func contentLines(lines []string) int {
	n := len(lines)
	if n > 0 && lines[n-1] == "" {
		n--
	}
	return n
}
