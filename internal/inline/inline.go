// Package inline resolves cross-file fn_* helpers and materializes their
// definitions into the calling file, so the existing single-file machinery
// handles them as helpers the caller always owned.
//
// Why append rather than expand at the call site: an fn_* call is an
// EXPRESSION — `if(fn_check(a, &flg) == -1)`. Replacing an expression with
// a statement sequence means hoisting statements out of whatever encloses
// it (an if header, a for condition, a + operand), which is a real C-to-C
// transformation with real evaluation-order and control-flow hazards.
// C function scope is per-function, so appending a whole definition cannot
// collide with the caller's locals at all: the hazards never arise. And
// because the appended text lands below the caller's last line, every line
// number the caller's IR already carries — StartLine, EndLine, Condition
// spans, ExternalFn.Callsites, tpcall windows — stays exactly where it was.
//
// Appending also lands the feature on machinery that already ships. The
// re-folded IR sees each helper as locally defined and locally called,
// which is precisely the shape plan.Build turns into a fixed-signature
// KindFnHelper unit and convert.rewriteHelperCalls rewrites to a
// `s.<GoName>(...)` call from the generated controller. The helper's body
// lands in controller/fns.go, called by the main controller only.
//
// Determinism is the design constraint throughout: identical inputs produce
// byte-identical output. The frontier is a FIFO queue seeded from the
// caller's externals in sorted-name order, callee resolution reuses ir's
// first-wins corpus table, and nothing consults the clock, a map iteration
// order, or an LLM. A callee that cannot be resolved is never guessed — it
// stays an ExternalFn and keeps the plan layer's existing stub policy, and
// every refusal is recorded in Result.Skipped rather than dropped.
package inline

import (
	"sort"
	"strings"
	"sync"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/tsscan"
)

// DefaultMaxDepth bounds how far a chain of helpers may be followed. A
// caller calls a helper, which calls a helper, which calls a helper: the
// corpus convention stops at one or two hops, and the bound is a backstop
// against a pathological chain rather than a tuning knob.
const DefaultMaxDepth = 4

// Options tunes the expansion. The zero value is usable: MaxDepth falls back
// to DefaultMaxDepth and IROptions to the default corpus-mode extraction.
type Options struct {
	// MaxDepth caps helper-of-helper resolution. 0 → DefaultMaxDepth.
	MaxDepth int

	// IROptions is the extraction posture for the re-fold. It is put in
	// corpus mode automatically, because the caller came out of a corpus
	// scan and must not be re-fragmented.
	IROptions ir.Options
}

func (o Options) maxDepth() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return DefaultMaxDepth
}

func (o Options) irOpts() ir.Options {
	base := o.IROptions
	if base.BufferRoles == nil {
		base = ir.DefaultOptions()
	}
	return base.CorpusMode()
}

// Site records one helper materialized into the caller: which function, where
// it came from, the source span it was lifted from, and where it landed in
// the expanded text. Sites are in append order, which is the corpus's
// deterministic order — the same input always yields the same list.
type Site struct {
	// Fn is the helper's legacy name (fn_min_check).
	Fn string `json:"fn"`
	// DefinedIn is the corpus file the definition was lifted from.
	DefinedIn string `json:"defined_in"`
	// FromLine/ToLine are the 1-based inclusive span in DefinedIn.
	FromLine int `json:"from_line"`
	ToLine   int `json:"to_line"`
	// AppendedAtLine is the 1-based line the definition starts on in the
	// expanded source, and AppendedToLine where it ends. Both are past the
	// caller's original last line.
	AppendedAtLine int `json:"appended_at_line"`
	AppendedToLine int `json:"appended_to_line"`
	// Depth is the helper-of-helper distance: 1 for a helper the caller
	// calls directly, 2 for one a pulled-in helper calls, and so on.
	Depth int `json:"depth"`
	// Via names the helper that pulled this one in ("" at depth 1).
	Via string `json:"via,omitempty"`
	// HasSQL mirrors the resolved external-fn fact: whether the body runs
	// database units. False means pure logic, still materialized.
	HasSQL bool `json:"has_sql,omitempty"`
}

// SkipCode classifies a refusal. Codes are stable identifiers — the CLI
// groups on them — while Detail carries the specific names and lines.
type SkipCode string

const (
	// SkipNotInCorpus: no corpus file defines this helper. It stays an
	// ExternalFn and the plan layer stubs it as before.
	SkipNotInCorpus SkipCode = "not-in-corpus"
	// SkipNoDefinition: a corpus file claims the name but the scanner
	// found no function definition for it (a prototype, a variable, a
	// parse the grammar could not recover).
	SkipNoDefinition SkipCode = "no-definition"
	// SkipSessionPlumbing: chk_* — the Tuxedo middleware owns session and
	// error plumbing (§4.8.4.1). The plan layer drops these too; inlining
	// them would inject a function the middleware already provides.
	SkipSessionPlumbing SkipCode = "session-plumbing"
	// SkipTransactionPlumbing: the fn_*Begin/Commit/Abort helper trio,
	// which utils.ExecTransaction owns. Same drop as the plan layer.
	SkipTransactionPlumbing SkipCode = "transaction-plumbing"
	// SkipMaxDepth: the chain is deeper than Options.MaxDepth.
	SkipMaxDepth SkipCode = "max-depth"
	// SkipDuplicate: the name is already in the expanded text (a helper
	// pulled in twice through different paths, or a name the caller
	// defines itself). Recorded, never appended twice.
	SkipDuplicate SkipCode = "duplicate"
	// SkipFmlTraffic: the helper body touches FML (Fget32/Fadd32/Foccur)
	// or issues its own tpcall. The buffers belong to the caller's
	// service scope, not the helper's, so its traffic would be
	// mis-attributed onto the caller's contract. Refused loudly rather
	// than silently rewritten.
	SkipFmlTraffic SkipCode = "fml-traffic"
	// SkipUnbalanced: the definition's braces never close, so there is no
	// trustworthy span to lift.
	SkipUnbalanced SkipCode = "unbalanced"
	// SkipCycle: the helper is already on the chain that pulled it in
	// (fn_a → fn_b → fn_a). Lifting it would recurse forever; the chain
	// is reported so the cycle is visible rather than merely bounded away.
	SkipCycle SkipCode = "cycle"
	// SkipGlobalSpan: a file-scope declaration the callee needs is written
	// across several lines, so it cannot be lifted whole. Recorded rather
	// than half-spliced.
	SkipGlobalSpan SkipCode = "global-declaration-span"
)

// Skip records one refusal, with the reason a reader needs and no more.
// Nothing is ever dropped silently: an external fn the pass declines to
// inline keeps its existing plan-layer behavior, and this says so.
type Skip struct {
	Fn     string   `json:"fn"`
	Code   SkipCode `json:"code"`
	Detail string   `json:"detail,omitempty"`
	// Depth is the helper-of-helper distance the skip happened at.
	Depth int `json:"depth,omitempty"`
	// Via names the helper that pulled this one in ("" at depth 1).
	Via string `json:"via,omitempty"`
}

// Result is one caller's expansion: the expanded source, the IR re-folded
// from it, and the full account of what moved and what did not.
type Result struct {
	// Path is the caller's path, carried through to the re-folded IR so
	// the expanded file is indistinguishable from the original.
	Path string `json:"path"`
	// Source is the expanded text. It is an IR input, never written to
	// disk and never emitted as C — appended text can include a DECLARE
	// SECTION that a standalone translation unit would not accept.
	Source string `json:"-"`

	// File is the IR re-folded from Source. When nothing was inlined it is
	// byte-identical to the caller's own extraction.
	File *ir.File `json:"-"`

	// OriginalLines is the caller's line count before expansion. Every
	// line 1..OriginalLines of Source is the caller's own text, byte for
	// byte — the invariant that keeps its IR's line references valid.
	OriginalLines int `json:"original_lines"`
	// ExpandedLines is Source's full line count.
	ExpandedLines int `json:"expanded_lines"`

	// Sites and Skips are in deterministic order and are never nil-vs-nil
	// ambiguous: an empty expansion carries both empty.
	Sites           []Site   `json:"sites"`
	Skips           []Skip   `json:"skips"`
	AppendedGlobals []string `json:"appended_globals,omitempty"`
}

// Expanded reports whether the pass materialized anything.
func (r *Result) Expanded() bool { return len(r.Sites) > 0 }

// Corpus is the resolution table: every scanned file's IR and source text,
// plus the fn-name → defining-file index the pass resolves against.
//
// The index is built first-wins over sorted file paths, which is the same
// rule ir.ExtractDirOpts applies when it fills ExternalFn.DefinedIn — so
// the pass and the IR can never disagree about who defines a helper.
// A Corpus is safe for concurrent use (dir mode fans out per service).
type Corpus struct {
	ir     map[string]*ir.File
	source map[string]string

	// defOf maps a defined function name to its defining file, first-wins
	// in sorted path order.
	defOf map[string]*ir.File

	mu    sync.Mutex
	facts map[string]*tsscan.SourceFacts
}

// NewCorpus indexes a set of scanned files and their source text. Files
// without source text are still indexed (they can define helpers whose
// definitions are therefore unavailable, which surfaces as a loud skip
// rather than a silent one); a caller whose own source is missing is an
// error from Expand, because it cannot be expanded at all.
func NewCorpus(files []*ir.File, sources map[string]string) *Corpus {
	c := &Corpus{
		ir:     make(map[string]*ir.File, len(files)),
		source: make(map[string]string, len(sources)),
		defOf:  make(map[string]*ir.File, len(files)),
		facts:  make(map[string]*tsscan.SourceFacts, len(files)),
	}
	for _, f := range files {
		if f == nil {
			continue
		}
		c.ir[f.Path] = f
	}
	for p, s := range sources {
		c.source[p] = s
	}
	// First-wins over sorted paths — the same resolution order
	// ir.ExtractDirOpts uses, so DefinedIn and this index agree.
	paths := make([]string, 0, len(c.ir))
	for p := range c.ir {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, fn := range c.ir[p].Functions {
			if _, ok := c.defOf[fn]; !ok {
				c.defOf[fn] = c.ir[p]
			}
		}
	}
	return c
}

// File returns a corpus file's IR by path.
func (c *Corpus) File(path string) (*ir.File, bool) {
	f, ok := c.ir[path]
	return f, ok
}

// Source returns a corpus file's text by path.
func (c *Corpus) Source(path string) (string, bool) {
	s, ok := c.source[path]
	return s, ok
}

// Paths lists the corpus files in sorted order.
func (c *Corpus) Paths() []string {
	out := make([]string, 0, len(c.ir))
	for p := range c.ir {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// scanFacts scans a corpus file's source, memoizing the result. The scan is
// the pass's only filesystem-independent work on the callee and is shared
// across every caller that pulls the same helper in.
func (c *Corpus) scanFacts(path string) (*tsscan.SourceFacts, bool, error) {
	c.mu.Lock()
	if f, ok := c.facts[path]; ok {
		c.mu.Unlock()
		return f, true, nil
	}
	c.mu.Unlock()

	src, ok := c.source[path]
	if !ok {
		return nil, false, nil
	}
	f, err := tsscan.ScanBytes([]byte(src), path)
	if err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	c.facts[path] = f
	c.mu.Unlock()
	return f, true, nil
}

// isChkPref reports the session/error plumbing convention. Kept local rather
// than reaching into plan: the pass must not depend on a layer that sits
// above it, and the rule is one prefix.
func isChkPref(name string) bool { return strings.HasPrefix(name, "chk_") }

// txRole classifies the transaction-helper trio (begin/commit/abort) the
// utils.ExecTransaction template owns. Mirrors plan.txHelperRole's rule
// without importing plan.
func txRole(name string) string {
	if !strings.HasPrefix(name, "fn_") {
		return ""
	}
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "begintran") || strings.Contains(l, "begin_tran"):
		return "begin"
	case strings.Contains(l, "committran") || strings.Contains(l, "commit_tran"):
		return "commit"
	case strings.Contains(l, "aborttran") || strings.Contains(l, "abort_tran") ||
		strings.Contains(l, "rollbacktran") || strings.Contains(l, "rollback_tran"):
		return "abort"
	}
	return ""
}
