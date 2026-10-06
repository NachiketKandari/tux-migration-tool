package analyzer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	scanner "tux-migration-tool/internal/tsscan"
)

// tpDynamicService names a tpcall/tpacall site whose target service is not a
// string literal (a variable or a wider expression) and is therefore
// unresolvable to a file by construction.
const tpDynamicService = "(dynamic)"

// TpSvcDep is one outbound Tuxedo service dependency of a file: every
// tpcall/tpacall site grouped by target service name. Kind is "tpcall" when
// every site is sync, "tpacall" when every site is async, else "both".
// Depth is 1 for a direct call target, 2 for a target's target, and so on —
// the closure is a breadth-first search, so Depth is the shortest call
// distance. Resolution (File/Score/Complexity) needs the scanned tree, so it
// is filled by resolveTpDeps in directory mode; single-file reports keep the
// skeleton (Resolved=false) — the dependency is counted, never guessed.
type TpSvcDep struct {
	Service    string
	Kind       string
	Calls      int
	Depth      int
	File       string
	Resolved   bool
	Score      int
	Complexity string
}

// FnFileDep is one external-function defining file used by a file: every
// resolved external fn grouped by the file that defines it (one entry per
// file — a shared helper file counts once no matter how many of its fns are
// called). Like TpSvcDep it prices the real body behind the call instead of
// the tier weight alone.
type FnFileDep struct {
	File       string
	Fns        []string
	Score      int
	Complexity string
}

// tpDepSkeletons groups one file's tpcall/tpacall sites by target service.
// The first top-level argument is the service (mirroring ir.buildTPCalls);
// a non-literal first argument aggregates under tpDynamicService.
func tpDepSkeletons(facts *scanner.SourceFacts) []TpSvcDep {
	byService := map[string]*TpSvcDep{}
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if !c.IsTpCall {
			continue
		}
		svc := tpCallService(c)
		if svc == "" {
			svc = tpDynamicService
		}
		d, ok := byService[svc]
		if !ok {
			d = &TpSvcDep{Service: svc}
			byService[svc] = d
		}
		d.Calls++
		kind := "tpcall"
		if c.Name == "tpacall" {
			kind = "tpacall"
		}
		switch {
		case d.Calls == 1:
			d.Kind = kind
		case d.Kind != kind:
			d.Kind = "both"
		}
	}
	deps := make([]TpSvcDep, 0, len(byService))
	for _, d := range byService {
		deps = append(deps, *d)
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Service != deps[j].Service {
			return deps[i].Service < deps[j].Service
		}
		return deps[i].Kind < deps[j].Kind
	})
	return deps
}

// tpCallService returns the unquoted target service of a tpcall/tpacall
// site, or "" when the first argument is not a string literal.
func tpCallService(c *scanner.FunctionCall) string {
	args := splitTopArgs(c.Args)
	if len(args) == 0 {
		return ""
	}
	return unquoteLit(args[0])
}

// splitTopArgs splits raw call-argument text on top-level commas: commas
// inside quotes or nested parens/brackets never split.
func splitTopArgs(args string) []string {
	var out []string
	depth := 0
	quote := byte(0)
	start := 0
	for i := 0; i < len(args); i++ {
		ch := args[i]
		if quote != 0 {
			if ch == '\\' && quote == '"' {
				i++
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, args[start:i])
				start = i + 1
			}
		}
	}
	if start <= len(args) {
		out = append(out, args[start:])
	}
	return out
}

// unquoteLit returns the contents of a double-quoted string literal, else "".
func unquoteLit(arg string) string {
	s := strings.TrimSpace(arg)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return ""
}

// svcStem returns the match key of a scanned file: its lowercased,
// extension-free base name (SVC_FOO.pc → svc_foo).
func svcStem(path string) string {
	return strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
}

// resolveTpDeps fills the resolution half of every report's TpSvcDeps
// against the scanned tree, then walks the full transitive closure, then
// rolls every resolved external fn's defining file into FnFileDeps.
//
// Service→file matching uses the one-match rule (zero or several matches
// stay unresolved — never a silent pick). The closure is a breadth-first
// search per root over service names: visited roots break cycles (A→B→A
// terminates), a target resolving to the root's own file is skipped (a file
// never inflates its own score), and Depth records the shortest call
// distance. TpDepScore sums every uniquely reachable target's own
// ComplexityScore; TpTotalScore adds the fn-file roll-up on top. Overall
// cost is O(R·(V+E)) for R reports — linear scans with map indexes, no
// nested walks.
func resolveTpDeps(reports []*Report) {
	byFile := make(map[string]*Report, len(reports))
	byStem := make(map[string][]*Report)
	for _, r := range reports {
		byFile[r.File] = r
		byStem[svcStem(r.File)] = append(byStem[svcStem(r.File)], r)
	}
	resolveDirect := func(d *TpSvcDep) {
		if d.Service == tpDynamicService {
			return
		}
		matches := byStem[strings.ToLower(d.Service)]
		if len(matches) != 1 {
			return
		}
		d.File = matches[0].File
		d.Resolved = true
		d.Score = matches[0].ComplexityScore
		d.Complexity = matches[0].Complexity
	}
	for _, r := range reports {
		for i := range r.TpSvcDeps {
			d := &r.TpSvcDeps[i]
			d.Depth = 1
			resolveDirect(d)
		}
	}
	for _, r := range reports {
		expandClosure(r, byFile, byStem)
		rollFnFiles(r, byFile)
		r.TpTotalScore = r.ComplexityScore + r.TpDepScore + r.FnDepScore
		r.Reasons += fmt.Sprintf("; tp svc dep score +%d", r.TpDepScore)
		if n := countDepth(r.TpSvcDeps, 2); n > 0 {
			r.Reasons += fmt.Sprintf(" (%d transitive)", n)
		}
		if r.TpUnresolved > 0 {
			var missing []string
			for _, d := range r.TpSvcDeps {
				if !d.Resolved {
					missing = append(missing, d.Service)
				}
			}
			sort.Strings(missing)
			r.Reasons += fmt.Sprintf("; tp svc unresolved: %s", strings.Join(missing, ", "))
		}
		if len(r.FnFileDeps) > 0 {
			r.Reasons += fmt.Sprintf("; fn file dep score +%d", r.FnDepScore)
		}
	}
}

// expandClosure breadth-first-searches the service graph from one report's
// direct deps, appending reachable targets (resolved or not) with their
// shortest Depth and folding the scores into TpDepScore/TpUnresolved.
func expandClosure(r *Report, byFile map[string]*Report, byStem map[string][]*Report) {
	seen := map[string]bool{svcStem(r.File): true}
	type queueItem struct {
		svc   string
		depth int
	}
	var queue []queueItem
	for _, d := range r.TpSvcDeps {
		key := strings.ToLower(d.Service)
		if !seen[key] {
			seen[key] = true
			queue = append(queue, queueItem{svc: d.Service, depth: 2})
		}
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		matches := byStem[strings.ToLower(item.svc)]
		if len(matches) != 1 {
			continue
		}
		next := matches[0]
		for _, d := range next.TpSvcDeps {
			key := strings.ToLower(d.Service)
			if seen[key] {
				continue
			}
			seen[key] = true
			nd := d
			nd.Depth = item.depth
			if nd.File == r.File {
				continue // never roll the root into itself
			}
			r.TpSvcDeps = append(r.TpSvcDeps, nd)
			queue = append(queue, queueItem{svc: d.Service, depth: item.depth + 1})
		}
	}
	// A service calling itself by name resolves to its own file — drop the
	// entry so a file never inflates its own dep score.
	kept := r.TpSvcDeps[:0]
	for _, d := range r.TpSvcDeps {
		if d.Resolved && d.File == r.File {
			continue
		}
		kept = append(kept, d)
	}
	r.TpSvcDeps = kept
	sort.Slice(r.TpSvcDeps, func(i, j int) bool {
		if r.TpSvcDeps[i].Depth != r.TpSvcDeps[j].Depth {
			return r.TpSvcDeps[i].Depth < r.TpSvcDeps[j].Depth
		}
		return r.TpSvcDeps[i].Service < r.TpSvcDeps[j].Service
	})
	depScore := 0
	unresolved := 0
	for _, d := range r.TpSvcDeps {
		if d.Resolved {
			depScore += d.Score
		} else {
			unresolved++
		}
	}
	r.TpDepScore = depScore
	r.TpUnresolved = unresolved
}

// rollFnFiles groups a report's resolved external fns by defining file and
// sums one score per file into FnDepScore. The tier weight already in the
// own score prices the call; this prices the body behind it.
func rollFnFiles(r *Report, byFile map[string]*Report) {
	byFileDep := map[string]*FnFileDep{}
	for _, fn := range r.ExternalFns {
		if !fn.Resolved || fn.DefinedIn == "" || fn.DefinedIn == r.File {
			continue
		}
		t, ok := byFile[fn.DefinedIn]
		if !ok {
			continue
		}
		d, ok := byFileDep[fn.DefinedIn]
		if !ok {
			d = &FnFileDep{File: fn.DefinedIn, Score: t.ComplexityScore, Complexity: t.Complexity}
			byFileDep[fn.DefinedIn] = d
		}
		d.Fns = append(d.Fns, fn.Name)
	}
	deps := make([]FnFileDep, 0, len(byFileDep))
	for _, d := range byFileDep {
		sort.Strings(d.Fns)
		deps = append(deps, *d)
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].File < deps[j].File })
	r.FnFileDeps = deps
	score := 0
	for _, d := range deps {
		score += d.Score
	}
	r.FnDepScore = score
}

func countDepth(deps []TpSvcDep, min int) int {
	n := 0
	for _, d := range deps {
		if d.Depth >= min {
			n++
		}
	}
	return n
}
