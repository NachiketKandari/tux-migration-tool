package ir

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// Options tunes extraction. BufferRoles extends the built-in buffer-role
// registry (lowercase name or suffix → role); ForceFragment applies the
// fragment rubric unconditionally.
type Options struct {
	BufferRoles   map[string]string
	ForceFragment bool

	dirMode bool // corpus mode: fragment detection is bypassed
}

// CorpusMode returns a copy of opts in corpus (directory) mode — fragment
// detection bypassed, exactly as ExtractDirOpts folds a whole directory.
// ExtractDirOpts sets the flag itself because it owns the walk; a caller
// that already holds a corpus (the inline pass folding an expanded file)
// needs the same posture for one file, and cannot reach the private field.
func (o Options) CorpusMode() Options {
	o.dirMode = true
	return o
}

// DefaultOptions returns the built-in buffer-role registry.
func DefaultOptions() Options {
	return Options{
		BufferRoles: map[string]string{
			"ibuffer": string(BufferInput),
			"obuffer": string(BufferOutput),
			"sbuffer": string(BufferSend),
			"rbuffer": string(BufferRecv),
		},
	}
}

// ExtractFile scans and extracts one file (file mode: fragment detection
// applies automatically).
func ExtractFile(path string) (*File, error) {
	return ExtractFileOpts(path, DefaultOptions())
}

// ExtractFileOpts is ExtractFile with explicit options.
func ExtractFileOpts(path string, opts Options) (*File, error) {
	// Read once and scan the bytes. tsscan.ScanFile would read the file
	// itself and this function needs the raw text anyway, so calling it
	// meant two full reads of every file in the corpus.
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	facts, err := tsscan.ScanBytes(src, path)
	if err != nil {
		return nil, err
	}
	// The raw lines are needed by one narrow consumer only — recognising
	// `buf = tpalloc(...)` as a buffer reset, which is a statement shape
	// rather than a call shape, so the call facts cannot express it. The
	// bytes are already in hand; thread them down. nil-safe for every
	// other builder.
	lines := sourceLines(src)
	if opts.ForceFragment || isFragmentFacts(facts) {
		fragFacts, err := tsscan.ScanFragment(src, path)
		if err != nil {
			return nil, err
		}
		return build(fragFacts, opts, lines), nil
	}
	return build(facts, opts, lines), nil
}

// sourceLines splits source into 1-indexed-addressable lines: index i is
// source line i+1, matching every Line field the scanner reports. A trailing
// newline yields no extra empty line, so len(lines) is the file's line count.
func sourceLines(src []byte) []string {
	s := string(src)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// ExtractSourceOpts extracts IR from source text already in memory, using
// path for identity only — the same deterministic fold ExtractFileOpts
// applies to a file on disk, so identical bytes in yield byte-identical IR
// out. The inline pass (internal/inline) needs this to re-fold its expanded
// source: the expanded file is an IR input, never written to disk, and
// going through the ordinary extractor is what makes the inlined helpers
// indistinguishable from helpers the file always defined itself.
func ExtractSourceOpts(src []byte, path string, opts Options) (*File, error) {
	facts, err := tsscan.ScanBytes(src, path)
	if err != nil {
		return nil, err
	}
	lines := sourceLines(src)
	if !opts.dirMode && (opts.ForceFragment || isFragmentFacts(facts)) {
		fragFacts, err := tsscan.ScanFragment(src, path)
		if err != nil {
			return nil, err
		}
		return build(fragFacts, opts, lines), nil
	}
	return build(facts, opts, lines), nil
}

// ExtractDir walks a directory for .pc/.pcf files and extracts each in
// corpus mode: fragment detection is bypassed, external fns resolve against
// the whole scanned corpus, and tpcall services resolve to corpus files.
func ExtractDir(dir string) ([]*File, error) {
	return ExtractDirOpts(dir, DefaultOptions())
}

// ExtractDirOpts is ExtractDir with explicit options.
func ExtractDirOpts(dir string, opts Options) ([]*File, error) {
	opts.dirMode = true
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".pc", ".pcf":
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	files := make([]*File, 0, len(paths))
	factsByFile := make(map[string]*tsscan.SourceFacts, len(paths))
	for _, p := range paths {
		// One read per file, then scan the bytes — ScanFile + ReadFile
		// read every corpus file twice.
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		facts, err := tsscan.ScanBytes(src, p)
		if err != nil {
			return nil, err
		}
		factsByFile[p] = facts
		files = append(files, build(facts, opts, sourceLines(src)))
	}

	// External fn resolution: a fn_*/chk_* symbol resolves against any
	// corpus file defining a function with that name.
	type def struct {
		file   *File
		hasSQL bool
		qids   []string
	}
	defs := make(map[string]def, len(files))
	for _, f := range files {
		for _, fn := range f.Functions {
			if _, ok := defs[fn]; !ok {
				defs[fn] = def{file: f, hasSQL: f.hasQueriesOwnedBy(fn), qids: f.queryIDsOwnedBy(fn)}
			}
		}
	}
	for _, f := range files {
		for i := range f.ExternalFns {
			if d, ok := defs[f.ExternalFns[i].Name]; ok {
				f.ExternalFns[i].Resolved = true
				f.ExternalFns[i].DefinedIn = d.file.Path
				f.ExternalFns[i].HasSQL = d.hasSQL
				f.ExternalFns[i].QueryIDs = d.qids
			}
		}
		// tpcall service → corpus file whose entry or base name matches
		for i := range f.TPCalls {
			svc := f.TPCalls[i].Service
			if svc == "" {
				continue
			}
			var matches []string
			seen := map[string]bool{}
			for _, g := range files {
				base := strings.TrimSuffix(filepath.Base(g.Path), filepath.Ext(g.Path))
				if g.Entry == svc || equalFold(base, svc) {
					if !seen[g.Path] {
						seen[g.Path] = true
						matches = append(matches, g.Path)
					}
				}
			}
			f.TPCalls[i].ServiceFile = strings.Join(matches, ",")
		}
	}
	// The cross-call join needs every ServiceFile resolved first, since it
	// looks the callee up by path — hence its own pass rather than being
	// folded into the loop above. Directory mode only: single-file mode has
	// no corpus to join against, which is exactly why every tpcall there
	// keeps a nil Callee and the placeholder path.
	joinTPCalls(files)
	return files, nil
}

// hasQueriesOwnedBy reports whether any unit's owning function is fn.
func (f *File) hasQueriesOwnedBy(fn string) bool {
	for _, q := range f.Queries {
		if q.OwningFunction == fn {
			return true
		}
	}
	return false
}

// queryIDsOwnedBy lists the unit ids owned by fn, in IR order.
func (f *File) queryIDsOwnedBy(fn string) []string {
	var out []string
	for _, q := range f.Queries {
		if q.OwningFunction == fn {
			out = append(out, q.ID)
		}
	}
	return out
}

// isFragmentFacts decides the fragment rubric: no SVC_* entry and no
// function definitions at all (helper files with function definitions keep
// full-file semantics).
func isFragmentFacts(facts *tsscan.SourceFacts) bool {
	for _, fn := range facts.Functions {
		if strings.HasPrefix(fn.Name, "SVC_") {
			return false
		}
	}
	return len(facts.Functions) == 0
}

// LiveFacts is the one home of the comment-live-facts rule: any call, SQL
// statement, or query whose start position sits inside a recorded comment
// span is dropped. On healthy scanner output this is a no-op (the scanner
// never records comment-dead facts); it guards foreign fact sources and is
// the queryable rule every other consumer reuses.
func LiveFacts(facts *tsscan.SourceFacts) *tsscan.SourceFacts {
	if !anyComment(facts) {
		return facts
	}
	out := *facts
	out.Calls = nil
	for _, c := range facts.Calls {
		if !facts.InComment(c.Line, c.Col) {
			out.Calls = append(out.Calls, c)
		}
	}
	out.AllSQL = nil
	for _, s := range facts.AllSQL {
		if !facts.InComment(s.StartLine, s.StartCol) {
			out.AllSQL = append(out.AllSQL, s)
		}
	}
	out.Queries = nil
	for _, s := range facts.Queries {
		if !facts.InComment(s.StartLine, s.StartCol) {
			out.Queries = append(out.Queries, s)
		}
	}
	return &out
}

func anyComment(facts *tsscan.SourceFacts) bool {
	return len(facts.Comments) > 0
}

// build folds facts into the File IR.
func build(rawFacts *tsscan.SourceFacts, opts Options, lines []string) *File {
	facts := LiveFacts(rawFacts)

	f := &File{Path: facts.Path, Fragment: facts.Fragment, HostVars: []HostVar{}}
	for _, fn := range facts.Functions {
		f.Functions = append(f.Functions, fn.Name)
		f.FunctionSpans = append(f.FunctionSpans, FuncSpan{
			Name:      fn.Name,
			BodyStart: fn.BodyStartLine,
			BodyEnd:   fn.BodyEndLine,
		})
		if f.Entry == "" && strings.HasPrefix(fn.Name, "SVC_") {
			f.Entry = fn.Name
		}
	}
	if facts.Fragment && f.Entry == "" {
		f.Entry = "__fragment"
	}
	f.BranchCount, f.BranchingFactor = branchingOf(facts)
	f.Unbalanced = unbalancedOf(facts)
	if len(facts.ParseErrors) > 0 {
		f.ParseErrors = make([]ParseError, 0, len(facts.ParseErrors))
		for _, pe := range facts.ParseErrors {
			f.ParseErrors = append(f.ParseErrors, ParseError{Kind: pe.Kind, Node: pe.Node, Line: pe.Line, Col: pe.Col})
		}
	}

	ops := allFmlOps(facts, f, opts)
	f.Queries = buildQueries(facts, ops)
	f.Conditions = buildConditions(facts, f, ops)
	f.Defines = buildDefines(facts)
	f.FmlOps = preambleOps(facts, f, ops)
	f.Buffers = buildBuffers(facts, f, ops, opts)
	f.TPCalls = buildTPCalls(facts, f, ops, opts, lines)
	f.HostVars = buildHostVars(facts, f)
	f.ExternalFns = buildExternalFns(facts)
	return f
}

// branchingOf counts if/else-if headers and sums 2^NestDepth over them
// (else arms and loops never contribute).
func branchingOf(facts *tsscan.SourceFacts) (int, int) {
	count, factor := 0, 0
	for _, b := range facts.Branches {
		if b.Kind == tsscan.BranchElse {
			continue
		}
		count++
		factor += 1 << b.NestDepth
	}
	return count, factor
}

// unbalancedOf converts scanner regions to IR records.
func unbalancedOf(facts *tsscan.SourceFacts) []Unbalanced {
	if len(facts.Unbalanced) == 0 {
		return nil
	}
	out := make([]Unbalanced, 0, len(facts.Unbalanced))
	for _, u := range facts.Unbalanced {
		out = append(out, Unbalanced{Kind: u.Kind, Line: u.StartLine, Col: u.StartCol})
	}
	return out
}

// buildDefines builds the define map from recorded directives.
func buildDefines(facts *tsscan.SourceFacts) []Define {
	var f File
	for _, d := range facts.Directives {
		switch d.Kind {
		case "define":
			name, value, macro := parseDefineArg(d.Arg)
			if name == "" {
				continue
			}
			f.Defines = append(f.Defines, Define{
				Name: name, Value: value, Line: d.Line,
				Function: containingFunctionName(facts.Functions, d.Line),
				Macro:    macro,
			})
		case "undef":
			name := strings.TrimSpace(d.Arg)
			if name == "" {
				continue
			}
			f.Defines = append(f.Defines, Define{
				Name: name, Line: d.Line, Undef: true,
				Function: containingFunctionName(facts.Functions, d.Line),
			})
		}
	}
	if len(f.Defines) == 0 {
		return nil
	}
	return f.Defines
}

// parseDefineArg splits "NAME VALUE" / "NAME(params) VALUE" rest-of-line
// text into its parts; a macro's value keeps its parameter list (audit-only
// anyway — macros are never substituted).
func parseDefineArg(arg string) (name, value string, macro bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", false
	}
	i := 0
	for i < len(arg) && isIdentByteFor(arg[i]) {
		i++
	}
	name = arg[:i]
	rest := strings.TrimSpace(arg[i:])
	if strings.HasPrefix(rest, "(") {
		return name, rest, true
	}
	return name, rest, false
}

// DefineAt resolves a define at (fn, line) with scoped shadowing: function
// scope wins within its function from its line onward; file scope is
// visible from its line onward; a latest-effective #undef tombstones the
// name; macros never resolve.
func (f *File) DefineAt(fn string, line int, name string) (Define, bool) {
	var fnDef, fileDef *Define
	for i := range f.Defines {
		d := &f.Defines[i]
		if d.Name != name || d.Line > line {
			continue
		}
		if d.Function != "" && d.Function == fn {
			fnDef = d
		} else if d.Function == "" {
			fileDef = d
		}
	}
	for _, cand := range []*Define{fnDef, fileDef} {
		if cand == nil {
			continue
		}
		if cand.Undef || cand.Macro {
			return Define{}, false
		}
		return *cand, true
	}
	return Define{}, false
}

// containingFunctionName names the function whose body spans the line
// ("" for file scope).
func containingFunctionName(fns []tsscan.FunctionDef, line int) string {
	best := -1
	for i := range fns {
		fn := &fns[i]
		if fn.BodyStartLine > 0 && fn.BodyStartLine <= line && (best < 0 || fn.BodyStartLine >= fns[best].BodyStartLine) {
			if fn.BodyEndLine > 0 && line > fn.BodyEndLine {
				continue
			}
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return fns[best].Name
}

// buildExternalFns collects called-but-not-locally-defined fn_*/chk_*
// symbols, sorted by name, with callsites in source order.
func buildExternalFns(facts *tsscan.SourceFacts) []ExternalFn {
	local := make(map[string]bool, len(facts.Functions))
	for _, fn := range facts.Functions {
		local[fn.Name] = true
	}
	var names []string
	sites := map[string][]int{}
	for _, c := range facts.Calls {
		if !c.IsFnPref && !c.IsChkPref {
			continue
		}
		if local[c.Name] {
			continue
		}
		if _, ok := sites[c.Name]; !ok {
			names = append(names, c.Name)
		}
		sites[c.Name] = append(sites[c.Name], c.Line)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]ExternalFn, 0, len(names))
	for _, n := range names {
		out = append(out, ExternalFn{Name: n, Callsites: sites[n]})
	}
	return out
}

// buildBuffers records every FML buffer variable and tpcall buffer argument
// with its resolved role, sorted by name.
func buildBuffers(facts *tsscan.SourceFacts, f *File, ops []FmlOp, opts Options) []BufferRole {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, op := range ops {
		add(op.Buffer)
	}
	for _, c := range facts.Calls {
		if !c.IsTpCall {
			continue
		}
		args := splitArgs(c.Args)
		if len(args) >= 2 {
			add(baseIdent(args[1]))
		}
		// tpacall has no recv-buffer arg (args[3] is flags) — only
		// tpcall contributes a recv buffer here.
		if len(args) >= 4 && c.Name != "tpacall" {
			add(baseIdent(args[3]))
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]BufferRole, 0, len(names))
	for _, n := range names {
		out = append(out, BufferRole{Name: n, Role: resolveBufferRole(n, opts)})
	}
	return out
}

// resolveBufferRole matches the buffer's full name or last '_' segment
// against the registry, case-insensitively on BOTH sides — the config yaml
// may carry CamelCase keys ("Ibuffer"), the engine's contract is lowercase
// ("ibuffer") — so a capitalized registry key can never silently degrade
// every buffer to unknown-role. Unknown roles are recorded, not guessed.
func resolveBufferRole(name string, opts Options) FmlBufferRole {
	for _, cand := range []string{strings.ToLower(name), lastSegmentLower(name)} {
		if role, ok := opts.BufferRoles[cand]; ok {
			return FmlBufferRole(role)
		}
		for k, v := range opts.BufferRoles {
			if strings.ToLower(k) == cand {
				return FmlBufferRole(v)
			}
		}
	}
	return BufferUnknown
}

func lastSegmentLower(name string) string {
	if idx := strings.LastIndexByte(name, '_'); idx >= 0 {
		return strings.ToLower(name[idx+1:])
	}
	return strings.ToLower(name)
}

// buildHostVars unions query binds, row shapes, FML targets, and condition
// flag vars, typed from the file's own declarations when possible.
func buildHostVars(facts *tsscan.SourceFacts, f *File) []HostVar {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, q := range f.Queries {
		for _, b := range q.Binds {
			add(baseName(b))
		}
		for _, r := range q.RowShape {
			add(baseName(r))
		}
	}
	for _, op := range f.FmlOps {
		add(op.Target)
	}
	for _, c := range f.Conditions {
		for _, op := range c.FmlOps {
			add(op.Target)
		}
		for _, v := range c.FlagVars {
			add(v)
		}
	}
	for _, tc := range f.TPCalls {
		for _, op := range tc.SendFML {
			add(op.Target)
		}
		for _, op := range tc.RecvFML {
			add(op.Target)
		}
	}
	sort.Strings(names)

	decls := map[string]tsscan.VarDecl{}
	for _, d := range facts.VarDecls {
		if _, ok := decls[d.Name]; !ok {
			decls[d.Name] = d
		}
	}
	for _, d := range facts.Params {
		if _, ok := decls[d.Name]; !ok {
			decls[d.Name] = d
		}
	}
	sections := sectionSpans(facts)
	out := make([]HostVar, 0, len(names))
	for _, n := range names {
		hv := HostVar{Name: n}
		if d, ok := decls[n]; ok {
			hv.CType = d.Type
			hv.GoHint = goHint(d.Type)
			hv.Array = d.Array
			hv.Nullable = d.Type == "varchar"
			hv.InDeclareSection = inSection(sections, d.Line)
		} else {
			hv.FromHeader = true
		}
		out = append(out, hv)
	}
	if out == nil {
		out = []HostVar{}
	}
	return out
}

// baseName strips an array-occurrence subscript from a host-variable
// reference ("px_jthldr[0]" -> "px_jthldr").
func baseName(name string) string {
	if idx := strings.IndexByte(name, '['); idx >= 0 {
		return name[:idx]
	}
	return name
}

// sectionSpans pairs BEGIN/END DECLARE SECTION statements into spans.
func sectionSpans(facts *tsscan.SourceFacts) [][2]int {
	var out [][2]int
	open := -1
	for _, s := range facts.AllSQL {
		if s.Kind != tsscan.SQLDeclareSection {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(s.Normalized), "BEGIN") {
			if open < 0 {
				open = s.StartLine
			}
		} else {
			end := s.EndLine
			if open < 0 {
				open = s.StartLine
			}
			out = append(out, [2]int{open, end})
			open = -1
		}
	}
	if open >= 0 {
		out = append(out, [2]int{open, facts.NumLines})
	}
	return out
}

func inSection(sections [][2]int, line int) bool {
	for _, s := range sections {
		if line >= s[0] && line <= s[1] {
			return true
		}
	}
	return false
}

// goHint maps C scalar types to Go-side hints used by generation.
func goHint(cType string) string {
	switch cType {
	case "char", "varchar":
		return "string"
	case "int":
		return "int"
	case "long":
		return "int64"
	case "short":
		return "int16"
	case "double":
		return "float64"
	case "float":
		return "float32"
	}
	return ""
}
