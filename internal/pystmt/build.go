package pystmt

import (
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/pred"
	scanner "tux-migration-tool/internal/tsscan"
)

// builder assembles the IR for one file.
//
// The shape of the assembly is: build a *region tree* first (every function
// body, branch, loop and switch, with the line range each one owns), then fill
// each region with the statements that belong directly to it. Doing it in that
// order is what makes the result provably well-nested rather than plausible —
// a statement belongs to the innermost region containing it, and there is no
// path by which two regions can both claim it.
//
// The alternative (walk the source once, emitting as tokens are seen) is the
// shape that loses statements: an unbraced arm's body, a `case` label's
// following brace, and a `}while(…)` tail all have to be anticipated.
type builder struct {
	src   *source
	facts *scanner.SourceFacts
	limit int

	// sqlByLine maps a region's start line to its query reference, so a
	// SQLRef statement can name the IR unit it became.
	sqlByLine map[int]QueryRef

	// reason records, per line, why it was accounted for. The keys are the
	// Coverage reasons; a line that gets none is uncovered.
	reason map[int]string

	// claims records every (line, col) position a statement claims, so the
	// residue pass can ask "is this scanner fact inside a statement I built?"
	// without re-deriving the statement set.
	claims []claim

	// built is the function set under construction, parallel to the regions.
	built []Func
}

// countKinds fills a histogram from a statement stream. Pre-seeding every kind
// would be wrong here: an absent kind must read as absent, not as zero, or
// "this corpus has no continue statement" and "this corpus has a continue
// statement I failed to find" become the same report.
func countKinds(stmts []Stmt, out map[Kind]int) {
	Walk(stmts, func(s *Stmt) { out[s.Kind]++ })
}

type claim struct {
	startLine, startCol int
	endLine, endCol     int
}

// newBuilder indexes the source and pre-resolves the SQL regions.
func newBuilder(in Input) *builder {
	limit := in.Limit
	if limit <= 0 {
		limit = strings.Count(in.Src, "\n") + 1
	}
	b := &builder{
		src:       newSource(in.Src, in.Facts),
		facts:     in.Facts,
		limit:     limit,
		sqlByLine: map[int]QueryRef{},
		reason:    map[int]string{},
	}
	for _, q := range in.SQL {
		if _, seen := b.sqlByLine[q.StartLine]; !seen {
			b.sqlByLine[q.StartLine] = q
		}
	}
	return b
}

func (b *builder) factsPath() string {
	if b.facts == nil {
		return ""
	}
	return b.facts.Path
}

// span builds a Span from two 1-based (line, col) positions, resolving the
// byte offsets the scanner's convention does not carry.
//
// The byte fields are what let a consumer slice the exact original text
// without re-deriving it, so they are filled here rather than left zero — a
// span with StartByte == 0 is indistinguishable from "line 1 column 1", and
// that ambiguity is exactly the kind that makes a byte-exact claim
// untestable.
func (b *builder) span(startLine, startCol, endLine, endCol int) Span {
	return Span{
		StartLine: startLine,
		StartCol:  startCol,
		EndLine:   endLine,
		EndCol:    endCol,
		StartByte: b.src.byteAt(startLine, startCol),
		EndByte:   b.src.byteAt(endLine, endCol),
	}
}

// ---------------------------------------------------------------------------
// Regions
// ---------------------------------------------------------------------------

// regionKind discriminates a block owner.
type regionKind int

const (
	regionFunc regionKind = iota
	regionIf
	regionLoop
	regionSwitch
	regionArm
)

// region is one block-owning statement and the line range it owns.
type region struct {
	kind regionKind
	// head is the line the keyword is on (the `if` line, the `case` line, the
	// function's signature line). It is accounted as a header, not as code.
	head int
	// body is the inclusive line range statements directly in this region may
	// occupy. For a braced region it excludes the opening and closing brace
	// lines; for an unbraced arm it is the single statement's extent.
	bodyStart, bodyEnd int
	// braced records whether the body had braces. It changes nothing about
	// nesting but decides whether the delimiter lines are brace or code.
	braced bool
	// bodyCol is the 1-based column the first body statement starts at, and 0
	// when it starts at the line's first code column. It is non-zero only for
	// an unbraced arm whose body shares the header's line
	// (`if(strcmp(…)=="M00") i_row=0;`) — 24 of those on dtcmpt.pc. Without
	// it the region's first line is classified from the line's first code
	// column, which is the `if` itself, so the arm's body reads as the whole
	// header and the assignment it was supposed to carry is never seen.
	bodyCol int
	// tail is the do-while's `while(…)` line, 0 when not a do-while.
	tail int
	// label and isDefault carry a case arm's parsed label.
	label     string
	isDefault bool
	// stmt is the statement this region belongs to, nil for a function body.
	stmt *Stmt
	// kids are the directly nested regions.
	kids []*region
	// parent is the enclosing region, nil for a function body.
	parent *region
}

// depth reports a region's nesting level, 1 for a function body.
func (r *region) depth() int {
	d := 1
	for p := r.parent; p != nil; p = p.parent {
		d++
	}
	return d - 1
}

// functions builds every function the scanner found, in source order.
func (b *builder) functions() []Func {
	if b.facts == nil {
		return nil
	}
	for i := range b.facts.Functions {
		fn := &b.facts.Functions[i]
		b.built = append(b.built, b.function(fn))
	}
	return b.built
}

// function builds one function's region tree and fills it.
func (b *builder) function(fn *scanner.FunctionDef) Func {
	end := fn.BodyEndLine
	if end <= 0 || end > b.limit {
		end = fn.BodyEndLine
		if end <= 0 {
			end = b.limit
		}
	}
	f := Func{
		Name:       fn.Name,
		ReturnType: fn.ReturnType,
		Header:     b.span(fn.StartLine, fn.Col, end, fn.Col),
		Body:       b.span(fn.BodyStartLine, 1, end, len(b.src.line(end))+1),
		Counts:     map[Kind]int{},
	}
	f.Params = b.params(fn)
	root := &region{
		kind:      regionFunc,
		head:      fn.StartLine,
		bodyStart: fn.BodyStartLine + 1,
		bodyEnd:   end - 1,
		braced:    true,
	}
	for l := root.bodyStart; l <= root.bodyEnd; l++ {
		b.note(l)
	}
	kids := b.regionsIn(fn, root.bodyStart, root.bodyEnd)
	root.kids = kids
	for _, k := range kids {
		k.parent = root
	}
	f.Stmts = b.fill(root)
	countKinds(f.Stmts, f.Counts)
	if len(f.Counts) == 0 {
		f.Counts = nil
	}
	return f
}

// params collects the function's parameters from the scanner's Param records,
// keeping only the ones whose signature line is this function's.
func (b *builder) params(fn *scanner.FunctionDef) []Param {
	var out []Param
	for _, p := range b.facts.Params {
		if p.Func != "" && p.Func != fn.Name {
			continue
		}
		if p.Line != fn.StartLine {
			continue
		}
		out = append(out, Param{
			Type: p.Type, Name: p.Name, Array: p.Array,
			Span: b.span(p.Line, p.Col, p.Line, p.Col+len(p.Name)),
		})
	}
	return out
}

// regionsIn builds the directly-nested regions of one block: branches, loops,
// switches (and their arms). Nesting is decided by containment, not by the
// scanner's Depth fields — Depth counts braces and does not distinguish a
// chain sibling from a child, whereas "does this region's line range sit
// inside that one's" does.
func (b *builder) regionsIn(fn *scanner.FunctionDef, start, end int) []*region {
	var all []*region
	all = append(all, b.branchRegions(fn, start, end)...)
	all = append(all, b.loopRegions(fn, start, end)...)
	all = append(all, b.switchRegions(start, end)...)
	roots := b.nest(all)
	adoptArmsIn(roots)
	return roots
}

// adoptArmsIn applies adoptArms to every switch in a finished tree, depth
// first so a switch nested inside an arm is corrected before its own parent
// looks at the (now stable) child list.
func adoptArmsIn(rs []*region) {
	for _, r := range rs {
		if r.kind == regionSwitch {
			adoptArms(r)
		}
		adoptArmsIn(r.kids)
	}
}

// adoptArms re-files a switch's arms and every region nested inside them.
//
// It has to run *after* nest, because containment at the switch level cannot
// distinguish "inside the switch" from "inside arm 'D'". An `if` in an arm's
// body nests directly under the switch by line containment alone, which would
// let it claim the arm's own statements. Adopting by the arm's own extent is
// the narrower, correct answer — and it is what keeps an arm's statements from
// leaking into its switch body.
//
// Regions that fall in an arm's extent but are already a descendant of it (a
// loop inside the arm's inner `if`) are left alone; only the outermost one is
// re-filed, so nesting depth is preserved rather than flattened.
func adoptArms(sw *region) {
	var move []*region
	var kept []*region
	for _, k := range sw.kids {
		if k.kind == regionArm {
			// Arms stay in sw.kids: emit reads them from there to build the
			// Switch's Arms field. Dropping them here would leave the switch
			// with no arms and the arm bodies re-attributed to the switch body.
			kept = append(kept, k)
			continue
		}
		move = append(move, k)
	}
	for _, arm := range kept {
		var stay []*region
		for _, k := range move {
			if !contains(arm.bodyStart, arm.bodyEnd, k.bodyStart, k.bodyEnd) {
				stay = append(stay, k)
				continue
			}
			// Already inside a deeper region of this arm: leave it be, so
			// nesting depth is preserved rather than flattened.
			if innermostHost(k, arm.kids) != nil {
				stay = append(stay, k)
				continue
			}
			arm.kids = append(arm.kids, k)
			k.parent = arm
		}
		move = stay
	}
	sw.kids = append(kept, move...)
}

// innermostHost reports a region among hosts that already contains r, or nil.
func innermostHost(r *region, hosts []*region) *region {
	var found *region
	for _, h := range hosts {
		if h == r {
			continue
		}
		if !contains(h.bodyStart, h.bodyEnd, r.bodyStart, r.bodyEnd) {
			continue
		}
		if found == nil || h.depth() > found.depth() {
			found = h
		}
	}
	return found
}

// nest re-parents a flat list of regions so each sits under the smallest
// region that contains it. Regions that overlap without containment cannot
// happen for well-formed C; when they do (an unbalanced file), the earlier one
// wins and the later one stays at the parent level rather than being dropped.
func (b *builder) nest(all []*region) []*region {
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].bodyStart != all[j].bodyStart {
			return all[i].bodyStart < all[j].bodyStart
		}
		return all[i].bodyEnd > all[j].bodyEnd
	})
	var roots []*region
	for _, r := range all {
		var host *region
		for _, c := range roots {
			if contains(c.bodyStart, c.bodyEnd, r.bodyStart, r.bodyEnd) {
				host = c
			}
		}
		if host == nil {
			roots = append(roots, r)
			continue
		}
		host.kids = append(host.kids, r)
		r.parent = host
	}
	return roots
}

func contains(outerStart, outerEnd, start, end int) bool {
	if outerStart >= outerEnd {
		return false
	}
	return start >= outerStart && end <= outerEnd
}

// branchRegions builds one region per if/else-if/else arm.
//
// An unbraced arm (`if(strcmp(…)=="M00") i_row=0;`, 24 of them on dtcmpt.pc)
// has no BlockStart/BlockEnd from the scanner — the field is 0. Its body is
// whatever single statement follows the header's closing paren, on the same
// line or the next. Deriving that extent here is what keeps the 24
// frequency-ladder assignments inside their arms instead of leaking them into
// the enclosing loop body.
func (b *builder) branchRegions(fn *scanner.FunctionDef, start, end int) []*region {
	var out []*region
	for i := range b.facts.Branches {
		br := &b.facts.Branches[i]
		if br.Function != "" && br.Function != fn.Name {
			continue
		}
		if br.StartLine < start || br.StartLine > end {
			continue
		}
		r := &region{kind: regionIf, head: br.StartLine}
		if br.BlockStart > 0 && br.BlockEnd > 0 {
			r.braced = true
			r.bodyStart, r.bodyEnd = br.BlockStart+1, br.BlockEnd-1
			if r.bodyStart > r.bodyEnd {
				r.bodyStart, r.bodyEnd = r.bodyEnd, r.bodyStart
			}
		} else {
			s, e, c := b.unbracedBody(br.StartLine, br.StartCol, end)
			if s == 0 {
				continue
			}
			r.bodyStart, r.bodyEnd, r.bodyCol = s, e, c
		}
		r.stmt = &Stmt{Kind: If, Chain: chainOf(br.Kind), Span: b.span(br.StartLine, br.StartCol, br.StartLine, br.StartCol)}
		r.stmt.CondText = br.Cond
		b.claimCondition(br.StartLine, br.StartCol)
		out = append(out, r)
	}
	return out
}

func chainOf(k scanner.BranchKind) Chain {
	switch k {
	case scanner.BranchElse:
		return ChainElse
	case scanner.BranchElseIf:
		return ChainElseIf
	}
	return ChainHead
}

// unbracedBody finds the single statement that is an unbraced arm's body, and
// the column it starts at on that line (0 when the body is on a later line).
//
// The column matters: `if(strcmp(…)=="M00") i_row=0;` puts the header and the
// body on one line, and classifying from the line's first code column reads
// the header as the statement. It is 1-based and points just past the closing
// paren, so the caller records it as the region's bodyCol.
func (b *builder) unbracedBody(head, col, limit int) (int, int, int) {
	if after := b.src.headerEnd(head, col); after > 0 {
		text := b.src.line(head)
		if rest := strings.TrimSpace(stripFrom(text, after-1)); rest != "" && !isBraceOnly(rest) {
			return head, b.lastCodeLine(head, after, limit), after
		}
		next := b.nextCodeLine(head+1, limit)
		if next == 0 {
			return 0, 0, 0
		}
		return next, b.lastCodeLine(next, 1, limit), 0
	}
	next := b.nextCodeLine(head+1, limit)
	if next == 0 {
		return 0, 0, 0
	}
	return next, b.lastCodeLine(next, 1, limit), 0
}

// nextCodeLine returns the first line at or after `from` that carries code.
func (b *builder) nextCodeLine(from, limit int) int {
	for l := from; l <= limit && l <= len(b.src.lines); l++ {
		if b.src.hasCode(l) {
			return l
		}
	}
	return 0
}

// lastCodeLine returns the last line of a statement beginning at (line, col),
// bounded by `limit`.
func (b *builder) lastCodeLine(line, col, limit int) int {
	end, _ := b.src.stmtExtent(line, col, limit)
	if end < line {
		return line
	}
	return end
}

// loopRegions builds one region per for/while/do.
func (b *builder) loopRegions(fn *scanner.FunctionDef, start, end int) []*region {
	var out []*region
	for i := range b.facts.Loops {
		l := &b.facts.Loops[i]
		if l.Function != "" && l.Function != fn.Name {
			continue
		}
		if l.StartLine < start || l.StartLine > end {
			continue
		}
		r := &region{kind: regionLoop, head: l.StartLine}
		if l.BlockStart > 0 && l.BlockEnd > 0 {
			r.braced = true
			r.bodyStart, r.bodyEnd = l.BlockStart+1, l.BlockEnd-1
			if l.Kind == scanner.LoopDo {
				// A do-while's closing line carries the tail `while (…)`.
				r.bodyEnd = l.BlockEnd
				r.tail = l.WhileLine
			}
		} else {
			next := b.nextCodeLine(l.StartLine+1, end)
			if next == 0 {
				continue
			}
			r.bodyStart = next
			r.bodyEnd = b.lastCodeLine(next, 1, end)
		}
		if r.bodyStart > r.bodyEnd {
			continue
		}
		r.stmt = b.loopStmt(l)
		b.claimCondition(l.StartLine, l.StartCol)
		out = append(out, r)
	}
	return out
}

// loopStmt builds the statement for one loop header, splitting a for's three
// clauses and a do-while's tail condition out of the recorded Cond.
func (b *builder) loopStmt(l *scanner.Loop) *Stmt {
	col := l.StartCol
	s := &Stmt{Span: b.span(l.StartLine, col, l.StartLine, col+headerWidth(l.Cond))}
	switch l.Kind {
	case scanner.LoopFor:
		s.Kind = For
		s.CondText = l.Cond
		// A for header's Cond is the whole `init; cond; step` run with the
		// parens already stripped by the scanner, so the three clauses come
		// apart on top-level semicolons. A `for(;;)` has none and yields one
		// empty clause list, which is correct.
		parts := splitTop(l.Cond, ";")
		switch len(parts) {
		case 3:
			s.Init = exprOf(parts[0])
			s.CondText = parts[1]
			s.Cond = condOf(parts[1])
			s.Step = exprOf(parts[2])
		case 2:
			s.Init = exprOf(parts[0])
			s.CondText = parts[1]
			s.Cond = condOf(parts[1])
		case 1:
			// A `for(a; b)` with no step: `parts[0]` is `a; b`, so the last
			// clause is the condition, not the whole run. Reading it as one
			// condition makes the emitted loop test `a` and ignore `b`.
			run := splitTop(parts[0], ";")
			last := run[len(run)-1]
			if s.Init = exprOf(run[0]); s.Init != nil {
				s.CondText = last
				s.Cond = condOf(last)
			}
		}
	case scanner.LoopDo:
		s.Kind = DoWhile
		s.TailLine = l.WhileLine
		s.CondText = l.Cond
		s.Cond = condOf(l.Cond)
	default:
		s.Kind = While
		s.CondText = l.Cond
		s.Cond = condOf(l.Cond)
	}
	return s
}

// headerWidth is a presentational width for a header span; the byte-exact
// extent is not recoverable from the scanner's Cond alone.
func headerWidth(cond string) int {
	w := len(cond) + 3 // `for (…)`
	if w < 8 {
		w = 8
	}
	return w
}

func exprOf(text string) *Expr {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	e := ParseExpr(text)
	return &e
}

// condOf parses a condition into the shared pred.Expr, which degrades to Raw
// rather than failing. Every condition in the IR goes through here so there is
// one condition grammar in the codebase, not two.
func condOf(text string) *pred.Expr {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	e := pred.Parse(text)
	return &e
}

// switchRegions locates switches by a contained source scan and builds one
// region per switch and per case label inside it.
//
// It does NOT use SourceFacts.Switches. That field is declared additive but
// arrives empty on dtcmpt.pc even though the file has
// `switch(sql_msi_intvl_sch_typ)` at :386 with four arms — a skeleton built
// from it would omit the exact dispatch whose 'D' arm is the §6.2 defect. The
// scan below is the same decision pycensus makes, for the same reason, and it
// is why the IR and the census agree on the arm count.
func (b *builder) switchRegions(start, end int) []*region {
	var out []*region
	for l := start; l <= end; l++ {
		cond, ok := b.switchHeader(l)
		if !ok {
			continue
		}
		open, close, ok := b.src.braceBlock(l, 1, end)
		if !ok {
			continue // unbalanced: the switch still shows up as a header
		}
		sw := &region{kind: regionSwitch, head: l, braced: true, bodyStart: open + 1, bodyEnd: close - 1}
		if sw.bodyStart > sw.bodyEnd {
			sw.bodyStart = sw.bodyEnd
		}
		sw.stmt = &Stmt{
			Kind:     Switch,
			Span:     b.span(l, 1, close, 1),
			CondText: cond,
			Cond:     condOf(cond),
		}
		b.claimCondition(l, 1)
		for _, a := range b.armRegions(l, close) {
			sw.kids = append(sw.kids, a)
			a.parent = sw
		}
		out = append(out, sw)
	}
	return out
}

// switchHeader reports whether a line opens a switch and returns its
// condition text.
func (b *builder) switchHeader(line int) (string, bool) {
	t := b.src.codeOnly(line)
	rest, ok := cutKeyword(t, "switch")
	if !ok {
		return "", false
	}
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(rest[open+1 : i]), true
			}
		}
	}
	return "", false
}

// armRegions builds one region per case/default label inside a switch.
//
// An arm's extent is its own brace block when it has one, and otherwise the
// run of lines up to the next label or the switch's closing brace. The
// unbraced case is not hypothetical: dtcmpt.pc:1259's `default :` is followed
// by two statements and no braces at all, and a parser that assumed braces
// would attribute them to nothing.
func (b *builder) armRegions(head, close int) []*region {
	var labels []armLabel
	for l := head + 1; l < close; l++ {
		t := b.src.codeOnly(l)
		if rest, ok := cutKeyword(t, "case"); ok {
			rest = strings.TrimSpace(rest)
			if i := strings.IndexByte(rest, ':'); i >= 0 {
				rest = strings.TrimSpace(rest[:i])
			}
			if rest != "" {
				labels = append(labels, armLabel{line: l, text: rest})
			}
			continue
		}
		if rest, ok := cutKeyword(t, "default"); ok && strings.HasPrefix(strings.TrimSpace(rest), ":") {
			labels = append(labels, armLabel{line: l, isDefault: true})
		}
	}
	out := make([]*region, 0, len(labels))
	for i, lb := range labels {
		limit := close - 1
		if i+1 < len(labels) {
			limit = labels[i+1].line - 1
		}
		r := &region{kind: regionArm, head: lb.line, label: lb.text, isDefault: lb.isDefault}
		ob, cb, braced := b.armBlock(lb.line, limit)
		switch {
		case braced:
			r.braced = true
			r.bodyStart, r.bodyEnd = ob+1, cb-1
			if r.bodyStart > r.bodyEnd {
				r.bodyStart, r.bodyEnd = r.bodyEnd, r.bodyStart
			}
		case b.nextCodeLine(lb.line+1, limit) != 0:
			next := b.nextCodeLine(lb.line+1, limit)
			r.bodyStart = next
			// The whole run to the next label, not one statement's extent. An
			// unbraced arm owns every statement up to the next label, so
			// `v = 0;` followed by `break;` is one body of two — and taking
			// only the first statement's last line leaves the rest belonging to
			// the switch, where fill re-walks it and it appears twice.
			r.bodyEnd = limit
		default:
			r.bodyStart, r.bodyEnd = limit, limit
		}
		out = append(out, r)
	}
	return out
}

// armBlock finds an arm's *own* brace block, if it has one.
//
// It must not use the general braceBlock from the label line: that matches the
// first `{` anywhere below, which for an unbraced arm whose first statement is
// an `if` is the *inner* block. The arm is then given the `if`'s body as its
// whole extent — so the arm claims nothing before the `if`, and the statements
// that genuinely precede it belong to the switch instead. That is exactly the
// leak adoptArms was added to prevent, arriving through the other door.
//
// The arm's own brace has to be on the label line or the line right after it;
// anything further down belongs to a statement inside the arm.
func (b *builder) armBlock(label, limit int) (open, close int, ok bool) {
	// On the label line, after the colon.
	text := b.src.codeOnly(label)
	col := 0
	if i := strings.IndexByte(text, ':'); i >= 0 {
		col = i + 1
	}
	if c := b.src.blockBraceCol(label, col+1); c > 0 {
		return b.src.braceBlock(label, c, limit)
	}
	next := label + 1
	if next > limit || next > b.limit {
		return 0, 0, false
	}
	if !isBraceOnly(b.src.codeOnly(next)) {
		return 0, 0, false
	}
	return b.src.braceBlock(next, 1, limit)
}

// armLabel is one parsed `case X:` / `default:` label.
type armLabel struct {
	line      int
	text      string
	isDefault bool
}

func (b *builder) note(line int) {
	if line <= 0 || line > b.limit {
		return
	}
	if _, seen := b.reason[line]; !seen {
		b.reason[line] = "pending"
	}
}

// stripFrom returns text from a 0-based column on, comment-blanked.
func stripFrom(text string, col int) string {
	if col >= len(text) {
		return ""
	}
	return strings.TrimSpace(text[col:])
}

// ---------------------------------------------------------------------------
// Filling regions
// ---------------------------------------------------------------------------

// fill builds the statement list a region owns directly, recursing into its
// nested regions and stitching each if chain's arms onto their head.
func (b *builder) fill(r *region) []Stmt {
	var out []Stmt
	childByHead := map[int]*region{}
	for _, k := range r.kids {
		if k.kind == regionArm {
			// An arm is emitted through its switch's Arms, not here — but its
			// lines must still be skipped below (see `inside`), or the switch
			// body walks them a second time and every arm statement appears
			// twice: once in the switch's Body and once in the arm's.
			continue
		}
		childByHead[k.head] = k
	}
	// inside is the extent a child region owns, so the enclosing fill does not
	// walk its lines a second time. Matching on the head line alone is not
	// enough: a switch's arms and an `if`'s block both start below their
	// header, so without this the parent's loop re-classifies them as its own
	// statements and every one of them appears twice — once nested, once flat.
	inside := func(line int) bool {
		for _, k := range r.kids {
			if k.kind == regionArm {
				if line >= k.head && line <= k.bodyEnd {
					return true
				}
				continue
			}
			if line > k.head && line <= k.bodyEnd {
				return true
			}
		}
		return false
	}
	for l := r.bodyStart; l <= r.bodyEnd; l++ {
		if c, isChild := childByHead[l]; isChild {
			out = append(out, b.emit(c))
			continue
		}
		if inside(l) {
			continue
		}
		st, ok := b.statementAt(l, r)
		if ok {
			out = append(out, st)
		}
	}
	b.stitchChain(&out)
	return out
}

// emit builds a nested statement and fills its own body. An arm child is
// consumed here rather than in fill's line loop, because a case label is not a
// statement — it is the header of a block that hangs off the switch's Arms.
func (b *builder) emit(r *region) Stmt {
	s := *r.stmt
	s.Depth = r.depth()
	switch r.kind {
	case regionIf, regionLoop, regionSwitch:
		s.Body = b.fill(r)
	}
	if r.kind == regionSwitch {
		for _, a := range r.kids {
			if a.kind != regionArm {
				continue
			}
			s.Arms = append(s.Arms, Arm{
				Label:     a.label,
				IsDefault: a.isDefault,
				Span:      b.span(a.head, 1, a.head, len(b.src.line(a.head))+1),
				Body:      b.fill(a),
			})
		}
	}
	return s
}

// stitchChain turns a flat sibling list of `if`, `else if`, `else` into nested
// chains: each arm's statements move to the preceding if's Else field.
//
// Siblings are what the scanner reports (it flattens chains deliberately), so
// this is the step that recovers the shape an emitter needs. An `else if`
// arrives as a nested If inside Else, so a reader sees the chain rather than
// three unrelated branches.
func (b *builder) stitchChain(out *[]Stmt) {
	var kept []Stmt
	// chainHead is the innermost `if` a following `else` should attach to. It
	// is not simply kept[last]: after an `else if` is nested, the next `else`
	// belongs to *that* if, so the chain has to be followed rather than
	// re-derived. Without this an `if / else if / else` loses its final else
	// into a sibling slot, and an emitter writes two branches for three arms.
	var chainHead *Stmt
	for _, st := range *out {
		switch st.Chain {
		case ChainHead:
			kept = append(kept, st)
			chainHead = &kept[len(kept)-1]
			continue
		case ChainElseIf, ChainElse:
		default:
			// Not part of a chain. It ends the previous one.
			chainHead = nil
			kept = append(kept, st)
			continue
		}
		if chainHead == nil || chainHead.Kind != If || len(chainHead.Else) > 0 {
			// A dangling else/else-if with no head to attach to (an
			// unbalanced file) stays where it is rather than vanishing.
			chainHead = nil
			kept = append(kept, st)
			continue
		}
		chainHead.Else = []Stmt{st}
		// The next arm in the chain hangs off the one just nested.
		if st.Kind == If && len(st.Else) == 0 {
			chainHead = &chainHead.Else[0]
		}
	}
	*out = kept
}

// statementAt classifies the statement starting on a line, or reports false
// when the line carries no statement (blank, comment, a brace delimiter, a
// preprocessor line, or the tail of a construct already claimed).
//
// The classifier is a fixed cascade, ordered so the unambiguous shapes are
// taken before the heuristic ones:
//
//	break / continue / return   keyword, unambiguous
//	declaration                 a VarDecl fact on the line, else a type-word
//	                            head — the only shape that starts with a type
//	assignment                 a top-level `=` that is not `==`, or ++/--
//	call                       a leading identifier followed by `(`
//	expression                 everything else
func (b *builder) statementAt(line int, r *region) (Stmt, bool) {
	if line < 1 || line > b.limit {
		return Stmt{}, false
	}
	text := b.src.codeOnly(line)
	if text == "" {
		if isPreproc(b.src.line(line)) {
			b.reason[line] = "preproc"
		} else {
			b.reason[line] = "blank"
		}
		return Stmt{}, false
	}
	if isPreproc(b.src.line(line)) {
		b.reason[line] = "preproc"
		return Stmt{}, false
	}
	if isBraceOnly(text) {
		b.reason[line] = "brace"
		return Stmt{}, false
	}
	if _, seen := b.reason[line]; seen && b.reason[line] == "stmt" {
		return Stmt{}, false
	}
	// A line already claimed as a nested region's header belongs to that
	// region, not here.
	if b.isHeadOf(line, r) {
		b.reason[line] = "header"
		return Stmt{}, false
	}

	col := b.src.firstCodeCol(line)
	if col == 0 {
		b.reason[line] = "blank"
		return Stmt{}, false
	}
	// On an unbraced arm's own line the body starts past the header, not at
	// the line's first code column — `if (x) y = 1;` has to classify `y = 1;`,
	// not the header that precedes it.
	if r.bodyCol > 0 && line == r.bodyStart {
		col = r.bodyCol
	}
	limit := b.regionLimit(r, line)
	endLine, endCol := b.src.stmtExtent(line, col, limit)
	full := b.statementText(line, col, endLine, endCol)
	head := strings.TrimSpace(full)

	s := Stmt{Span: b.span(line, col, endLine, endCol), Text: collapse(full)}
	b.noteRange(line, endLine)
	b.reason[line] = "stmt"
	b.claims = append(b.claims, claim{line, col, endLine, endCol})

	switch {
	case isWord(head, "break"):
		s.Kind = Break
		s.Name = "break"
	case isWord(head, "continue"):
		s.Kind = Continue
		s.Name = "continue"
	case isWord(head, "return"):
		s.Kind = Return
		s.CondText = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(head, "return"), ";"))
		s.RHS = exprOf(s.CondText)
	case gotoLabel(head):
		// Not in the vocabulary, but not dropped: a KindExpr keeps the text so
		// a later phase can see the transfer rather than infer it from silence.
		s.Kind = KindExpr
		s.Name = "goto"
		s.CondText = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(head, "goto"), ";"))
	case b.isDeclLine(line, head, full):
		s.Kind = Decl
		s.Name, s.Names, s.Initializer = b.declOf(line, full)
	default:
		if op, lhs, rhs, ok := splitAssign(head); ok {
			s.Kind = Assign
			s.Op = op
			s.LHS = exprOf(lhs)
			s.RHS = exprOf(rhs)
		} else if name, args, ok := splitCall(head); ok {
			s.Kind = Call
			s.Name = name
			s.Args = ParseArgs(args)
		} else {
			s.Kind = KindExpr
			s.RHS = exprOf(head)
		}
	}
	// A call fact on the statement's own first token makes this a Call
	// whatever the cascade decided — the scanner's identification is
	// authoritative for *what* the statement is, the cascade only supplies
	// the shape.
	if s.Kind == Assign || s.Kind == Call {
		if call := b.callAt(line); call != nil {
			// Only an assignment the cascade already found stays an Assign;
			// promoting a bare `tpterm();` to one would invent a left-hand
			// side that does not exist. What the fact contributes either way is
			// the callee and its parsed arguments.
			if s.Kind == Assign {
				if s.LHS == nil {
					s.LHS = exprOf(strings.TrimSuffix(head, ";"))
				}
				if s.RHS == nil {
					s.RHS = &Expr{Kind: ExprCall, Name: call.Name, Args: ParseArgs(call.Args)}
				}
			} else {
				s.Args = ParseArgs(call.Args)
			}
			s.Name = call.Name
		}
	}
	// A return fact on this line means the statement is a return even if the
	// cascade read it as an expression.
	if s.Kind == KindExpr {
		for i := range b.facts.Returns {
			if b.facts.Returns[i].Line == line {
				s.Kind = Return
				s.RHS = exprOf(strings.TrimSpace(strings.TrimSuffix(head, ";")))
				break
			}
		}
	}
	if q, ok := b.sqlByLine[line]; ok {
		s.Kind = SQLRef
		s.QueryID = q.ID
		s.SQLKind = q.Kind
	}
	return s, true
}

// regionLimit bounds a statement's extent walk so it cannot run past the
// region that owns it. Without it, an unbalanced construct would swallow the
// remainder of the function.
func (b *builder) regionLimit(r *region, line int) int {
	limit := b.limit
	if r.bodyEnd > 0 && r.bodyEnd < limit {
		limit = r.bodyEnd
	}
	for _, k := range r.kids {
		if k.bodyStart > line && k.bodyStart-1 < limit {
			limit = k.bodyStart - 1
		}
	}
	return limit
}

// isHeadOf reports whether a line is the header of one of the region's direct
// children — the `if` line, the `for` line, the `case` line, the `while` tail
// of a do.
func (b *builder) isHeadOf(line int, r *region) bool {
	for _, k := range r.kids {
		if k.head == line {
			return true
		}
		if k.kind == regionLoop && k.tail == line {
			return true
		}
	}
	return false
}

// statementText returns the statement's source text, comments blanked.
func (b *builder) statementText(line, col, endLine, endCol int) string {
	var sb strings.Builder
	for l := line; l <= endLine; l++ {
		text := b.src.line(l)
		start := 0
		if l == line {
			start = col - 1
		}
		stop := len(text)
		if l == endCol && endCol > 0 {
			stop = endCol - 1
		}
		for i := start; i < stop; i++ {
			if b.src.inComment(l, i+1) {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteByte(text[i])
		}
		if l < endLine {
			sb.WriteByte(' ')
		}
	}
	return strings.TrimSpace(sb.String())
}

// noteRange marks every line a multi-line statement occupies.
func (b *builder) noteRange(from, to int) {
	for l := from; l <= to; l++ {
		if _, seen := b.reason[l]; !seen {
			b.reason[l] = "stmt"
		}
	}
}

// callAt returns the scanner's call fact starting at a line, or nil. Only the
// leftmost fact is considered: `strcpy(x, tuxgetenv("ORA_GRP"))` records two
// calls on one line, and the statement is the outer one.
func (b *builder) callAt(line int) *scanner.FunctionCall {
	var best *scanner.FunctionCall
	for i := range b.facts.Calls {
		c := &b.facts.Calls[i]
		if c.Line != line {
			continue
		}
		if best == nil || c.Col < best.Col {
			best = c
		}
	}
	return best
}

// claimCondition records a call fact that sits inside a control header's
// condition rather than in a statement of its own.
//
// `if(tpopen() == -1)` is a call the scanner records, and it is genuinely
// accounted for — the branch carries it as CondText. But it is not a statement
// on a line the line-scope classifier walks, so without this the residue pass
// reports it as unclaimed. That is precisely the "unaccounted" report the
// accounting exists to raise, and it would be raised here for a construct the
// IR handles correctly.
//
// Only the header line is claimed, and only from the branch keyword's column,
// so a call in the header's own trailing statement (`if (x) tpbegin();`) is
// still claimed by that statement rather than double-counted.
func (b *builder) claimCondition(line, col int) {
	if b.facts == nil {
		return
	}
	for i := range b.facts.Calls {
		c := &b.facts.Calls[i]
		if c.Line == line && c.Col >= col {
			b.claims = append(b.claims, claim{line, c.Col, line, c.Col + len(c.Name)})
		}
	}
}

// isDeclLine reports a declaration. Two tests, in order of authority: a
// VarDecl fact on the line, else a leading type word.
//
// The fallback is not redundant. The scanner's vocabulary deliberately excludes
// pointer declarations (`walk.go`: "pointer declarations are not VarDecls"), so
// `TPINIT* ptr_tpinfo;` at dtcmpt.pc:101 has no fact at all; a skeleton that
// trusted the fact alone would report that line as unaccounted — which is
// exactly the kind of residue the census exists to make visible rather than
// absorb.
func (b *builder) isDeclLine(line int, head, full string) bool {
	for i := range b.facts.VarDecls {
		d := &b.facts.VarDecls[i]
		if d.Line == line && d.Name != "" {
			return true
		}
	}
	rest, ok := cutKeyword(head, "static")
	if ok {
		head = rest
	}
	word := leadingWord(head)
	if word == "" {
		return false
	}
	if !declHeads[strings.ToLower(word)] {
		return false
	}
	// A type word must be followed by something that can be a declarator:
	// an identifier, a `*`, or `[]`. `long + 1;` is not a declaration.
	tail := strings.TrimLeft(head[len(word):], " \t")
	if tail == "" {
		return false
	}
	c := tail[0]
	return isIdentByte(c) || c == '*' || c == '['
}

// declHeads are the type words this corpus declares with. It is an
// enumeration rather than a heuristic for the same reason pyplan's Oracle
// builtin list is: an unrecognised head falls through to the next branch of
// the cascade, so a new type is a visible miss rather than a silent
// misclassification.
// Every key is lowercase: the lookup lowercases its probe, so a mixed-case key
// here can never match. That is not a cosmetic slip — `TPINIT` used to be
// spelled in caps and was therefore unreachable, which turned
// `TPINIT* ptr_tpinfo;` into an unclassified expression and hid a real
// declaration from the accounting entirely.
var declHeads = map[string]bool{
	"char": true, "short": true, "int": true, "long": true,
	"float": true, "double": true, "void": true, "signed": true,
	"unsigned": true, "struct": true, "union": true, "enum": true,
	"const": true, "volatile": true, "size_t": true, "time_t": true,
	"tpinit": true, "bool": true, "pid_t": true, "short_t": true,
}

// declOf extracts a declaration's base type, declarator list and initializer.
func (b *builder) declOf(line int, full string) (string, []string, *Expr) {
	text := strings.TrimSuffix(strings.TrimSpace(full), ";")
	lhs, rhs, hasInit := splitTopAssign(text)
	base := leadingWord(strings.TrimSpace(lhs))
	names := declarators(strings.TrimSpace(lhs), base)
	var init *Expr
	if hasInit {
		init = exprOf(rhs)
	}
	return base, names, init
}

// splitTopAssign splits a declaration or assignment at its top-level `=`.
func splitTopAssign(text string) (string, string, bool) {
	parts := splitTop(text, "=")
	if len(parts) < 2 {
		return text, "", false
	}
	// Rejoin anything after the first `=` (a `==` cannot appear here, but a
	// compound `+=`-style header could).
	rest := strings.Join(parts[1:], "=")
	return parts[0], rest, true
}

// declarators pulls the declared names out of a declaration's left-hand side,
// dropping the type word, any pointer stars and any array subscripts.
//
// The type word is dropped by position, not by name: the first identifier run
// on the left-hand side is the base type, so `int i, j, k;` yields i, j, k and
// not `int, i, j, k`. `declOf` already computed that same run as `base`, and
// passing it in keeps the two from disagreeing about where the type ends.
func declarators(lhs, base string) []string {
	lhs = strings.TrimSpace(lhs)
	var out []string
	i := 0
	for i < len(lhs) {
		c := lhs[i]
		switch {
		case isIdentByte(c):
			j := i
			for j < len(lhs) && isIdentByte(lhs[j]) {
				j++
			}
			if w := lhs[i:j]; w != base {
				out = append(out, w)
			}
			i = j
		case c == '[':
			j := strings.IndexByte(lhs[i:], ']')
			if j < 0 {
				return out
			}
			i += j + 1
		case c == '*' || c == ' ' || c == '\t':
			i++
		default:
			i++
		}
	}
	return out
}

// splitAssign splits `lhs = rhs`, `lhs += rhs`, `lhs++` and `lhs--` at the
// top level, excluding every comparison operator. A comparison appearing here
// means the line is an expression, not an assignment — which is the whole
// reason `if (a == b)` cannot be misread as a write to `a`.
func splitAssign(text string) (op, lhs, rhs string, ok bool) {
	t := strings.TrimSuffix(strings.TrimSpace(text), ";")
	if t == "" {
		return "", "", "", false
	}
	for _, o := range []string{"++", "--"} {
		if t == o {
			return o, "", "", false
		}
		if strings.HasPrefix(t, o) {
			return o, strings.TrimSpace(t[2:]), "", true
		}
		if strings.HasSuffix(t, o) {
			return o, strings.TrimSpace(t[:len(t)-2]), "", true
		}
	}
	depth := 0
	for i := 0; i < len(t); i++ {
		// `c` is bound outside the switch: the trailing `=` / `!=` tests below
		// are not cases, so a switch-scoped `c` would be out of scope at the
		// point they run and the function would not compile.
		c := t[i]
		switch c {
		case '"', '\'':
			i = scanLiteral(t, i, c) - 1
			continue
		case '(', '[':
			depth++
			continue
		case ')', ']':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if c == '=' {
			if i+1 < len(t) && t[i+1] == '=' {
				return "", "", "", false
			}
			if i > 0 && strings.IndexByte("+-*/%&|^<>!", t[i-1]) >= 0 && t[i-1] != '!' && t[i-1] != '<' && t[i-1] != '>' {
				return string(t[i-1]), strings.TrimSpace(t[:i-1]), strings.TrimSpace(t[i+1:]), true
			}
			return "=", strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:]), true
		}
		if c == '!' || c == '<' || c == '>' {
			if i+1 < len(t) && t[i+1] == '=' {
				return "", "", "", false
			}
		}
	}
	return "", "", "", false
}

// splitCall splits a bare call statement into callee and argument text. Only
// a leading identifier counts, so `(char *)x` and `a + b()` are not calls.
func splitCall(text string) (name, args string, ok bool) {
	t := strings.TrimSuffix(strings.TrimSpace(text), ";")
	i := 0
	for i < len(t) && isIdentByte(t[i]) {
		i++
	}
	if i == 0 || !strings.HasPrefix(t[i:], "(") {
		return "", "", false
	}
	name = t[:i]
	depth := 0
	for j := i; j < len(t); j++ {
		switch t[j] {
		case '"', '\'':
			j = scanLiteral(t, j, t[j]) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return name, t[i+1 : j], true
			}
		}
	}
	return name, t[i+1:], true
}

// isWord reports a statement that is *only* the keyword — `break;`, `continue;`.
// A keyword with a payload (`return x;`, `goto done;`) is not.
func isWord(text, kw string) bool {
	rest, ok := cutKeyword(text, kw)
	if !ok {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	return rest == "" || rest == ";"
}

// gotoLabel reports a `goto <label>;`. It is deliberately not isWord: `goto`
// always carries a label, so the bare-keyword test can never match it and the
// transfer used to fall through to the expression branch with no Name and no
// indication it was a jump at all.
func gotoLabel(text string) bool {
	rest, ok := cutKeyword(text, "goto")
	if !ok {
		return false
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), ";")
	return rest != ""
}

func leadingWord(text string) string {
	i := 0
	for i < len(text) && isIdentByte(text[i]) {
		i++
	}
	return text[:i]
}

// ---------------------------------------------------------------------------
// Accounting
// ---------------------------------------------------------------------------

// coverage partitions every line of every scanned body into exactly one
// reason and lists what is left.
func (b *builder) coverage() Coverage {
	c := Coverage{
		ByReason: map[string]int{},
		ByKind:   map[Kind]int{},
	}
	if b.facts == nil {
		// Build is reachable before the scanner has run — the batch flow
		// constructs the IR early to decide whether it needs one at all. A nil
		// Facts must read as "nothing scanned", not dereference.
		return c
	}
	c.Scanned = len(b.facts.Functions)
	for i := range b.facts.Functions {
		fn := &b.facts.Functions[i]
		start, end := fn.BodyStartLine, fn.BodyEndLine
		if end <= 0 || end > b.limit {
			end = b.limit
		}
		for l := start; l <= end; l++ {
			if l < 1 || l > len(b.src.lines) {
				continue
			}
			c.Lines++
			reason := b.reasonOf(l)
			if reason == "" {
				c.Uncovered = append(c.Uncovered, l)
				continue
			}
			c.ByReason[reason]++
		}
	}
	for _, r := range b.built {
		Walk(r.Stmts, func(s *Stmt) {
			c.ByKind[s.Kind]++
			if s.Depth > c.Depth {
				c.Depth = s.Depth
			}
		})
	}
	sort.Ints(c.Uncovered)
	if len(c.ByKind) == 0 {
		c.ByKind = nil
	}
	return c
}

// reasonOf decides why a line was accounted for, or "" when nothing claimed
// it. The pending marker set during assembly is resolved here: a line noted
// but never turned into a statement is a header, a brace, or a genuine hole,
// and only the source can tell which.
func (b *builder) reasonOf(line int) string {
	if r, ok := b.reason[line]; ok && r != "pending" {
		return r
	}
	text := b.src.codeOnly(line)
	switch {
	case text == "":
		if isPreproc(b.src.line(line)) {
			return "preproc"
		}
		return "blank"
	case isBraceOnly(text):
		return "brace"
	case b.isControlHeader(line):
		return "header"
	}
	if _, ok := b.reason[line]; ok {
		return "header"
	}
	return ""
}

// isControlHeader reports a line that opens a control construct: `if`, `else`,
// `while`, `for`, `do`, `switch`, `case`, `default`, or a `}while (…)` tail.
func (b *builder) isControlHeader(line int) bool {
	t := strings.TrimSpace(b.src.codeOnly(line))
	if t == "" {
		return false
	}
	// A do-while tail: the line starts with the closing brace and names
	// `while` after it.
	if strings.HasPrefix(t, "}") {
		if _, ok := cutKeyword(strings.TrimSpace(t[1:]), "while"); ok {
			return true
		}
	}
	for _, kw := range []string{"if", "else", "while", "for", "do", "switch", "case", "default"} {
		if _, ok := cutKeyword(t, kw); ok {
			return true
		}
	}
	return false
}

// residue closes the loop from the other side: a scanner fact inside a
// scanned function that no statement claimed.
func (b *builder) residue() Residue {
	r := Residue{}
	if b.facts == nil {
		return r
	}
	inBody := func(line int) bool {
		for i := range b.facts.Functions {
			fn := &b.facts.Functions[i]
			end := fn.BodyEndLine
			if end <= 0 || end > b.limit {
				end = b.limit
			}
			if line >= fn.BodyStartLine && line <= end {
				return true
			}
		}
		return false
	}
	claimedAt := func(line, col int) bool {
		for _, cl := range b.claims {
			if line < cl.startLine || line > cl.endLine {
				continue
			}
			if line == cl.startLine && col < cl.startCol {
				continue
			}
			if line == cl.endLine && col >= cl.endCol {
				continue
			}
			return true
		}
		return false
	}
	for i := range b.facts.Calls {
		c := &b.facts.Calls[i]
		if !inBody(c.Line) || claimedAt(c.Line, c.Col) {
			continue
		}
		r.Calls = append(r.Calls, c.Name+"@"+strconv.Itoa(c.Line))
	}
	for i := range b.facts.VarDecls {
		d := &b.facts.VarDecls[i]
		if !inBody(d.Line) || claimedAt(d.Line, d.Col) {
			continue
		}
		r.Decls = append(r.Decls, d.Name+"@"+strconv.Itoa(d.Line))
	}
	for i := range b.facts.Returns {
		ret := &b.facts.Returns[i]
		if inBody(ret.Line) && !claimedAt(ret.Line, ret.Col) {
			r.Returns = append(r.Returns, ret.Line)
		}
	}
	builtNames := map[string]bool{}
	for _, f := range b.built {
		builtNames[f.Name] = true
	}
	for i := range b.facts.Functions {
		fn := &b.facts.Functions[i]
		if !builtNames[fn.Name] {
			r.MissingFuncs = append(r.MissingFuncs, fn.Name)
		}
	}
	sort.Strings(r.Calls)
	sort.Strings(r.Decls)
	sort.Ints(r.Returns)
	sort.Strings(r.MissingFuncs)
	return r
}