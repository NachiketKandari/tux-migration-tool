package inline

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// callee is one resolved helper definition: the corpus file it lives in, the
// scanner's function span, and the trustworthy source span to lift.
type callee struct {
	name  string
	path  string
	facts *tsscan.SourceFacts
	def   tsscan.FunctionDef

	// start/end are the resolved 1-based inclusive lines of the WHOLE
	// definition — return type through the closing brace. They differ from
	// def.StartLine/BodyEndLine in two ways the scanner does not resolve
	// for us: the return type may sit on its own line above the name, and
	// an unbalanced body has no trustworthy end.
	start int
	end   int

	// sourceText is the defining file's text, attached at resolve time so
	// span resolution and splicing never re-read the corpus.
	sourceText string
}

// resolveCallee locates name in the corpus and pins its definition span.
//
// Every refusal is a Skip with a code, never a silent nil: the caller has
// asked what happened to a helper it can see, and "nothing" is an answer it
// must be able to print.
func (c *Corpus) resolveCallee(name string, depth int, via string) (*callee, *Skip) {
	if isChkPref(name) {
		return nil, &Skip{Fn: name, Code: SkipSessionPlumbing, Depth: depth, Via: via,
			Detail: "session/error plumbing — the Tuxedo middleware owns chk_* (§4.8.4.1)"}
	}
	if role := txRole(name); role != "" {
		return nil, &Skip{Fn: name, Code: SkipTransactionPlumbing, Depth: depth, Via: via,
			Detail: "transaction " + role + " plumbing — utils.ExecTransaction owns begin/commit/rollback"}
	}
	defFile, ok := c.defOf[name]
	if !ok {
		return nil, &Skip{Fn: name, Code: SkipNotInCorpus, Depth: depth, Via: via,
			Detail: "no corpus file defines " + name}
	}
	facts, haveSrc, err := c.scanFacts(defFile.Path)
	if err != nil {
		return nil, &Skip{Fn: name, Code: SkipNoDefinition, Depth: depth, Via: via,
			Detail: fmt.Sprintf("scanning %s: %v", defFile.Path, err)}
	}
	if !haveSrc {
		return nil, &Skip{Fn: name, Code: SkipNoDefinition, Depth: depth, Via: via,
			Detail: defFile.Path + " has no source text in the corpus"}
	}
	def, found := definitionOf(facts, name)
	if !found {
		return nil, &Skip{Fn: name, Code: SkipNoDefinition, Depth: depth, Via: via,
			Detail: fmt.Sprintf("%s defines no function named %s (prototype, variable, or unrecovered parse)", defFile.Path, name)}
	}
	if def.BodyEndLine <= 0 {
		return nil, &Skip{Fn: name, Code: SkipUnbalanced, Depth: depth, Via: via,
			Detail: fmt.Sprintf("%s:%d — the definition's braces never close", defFile.Path, def.StartLine)}
	}
	src, _ := c.source[defFile.Path]
	cal := &callee{
		name:       name,
		path:       defFile.Path,
		facts:      facts,
		def:        def,
		start:      definitionStart(src, def),
		end:        def.BodyEndLine,
		sourceText: src,
	}
	if ifml := fmlInside(cal); ifml != "" {
		return nil, &Skip{Fn: name, Code: SkipFmlTraffic, Depth: depth, Via: via,
			Detail: fmt.Sprintf("%s:%d — the body uses %s; the FML buffers belong to the caller's service scope, so its traffic would be mis-attributed onto the caller's contract", defFile.Path, def.BodyStartLine, ifml)}
	}
	return cal, nil
}

// definitionOf finds a function definition by name, first-wins in scanner
// order. The scanner records definitions, so a name with a body resolves
// even when a prototype for it also appears.
func definitionOf(facts *tsscan.SourceFacts, name string) (tsscan.FunctionDef, bool) {
	for _, fn := range facts.Functions {
		if fn.Name == name && fn.BodyStartLine > 0 {
			return fn, true
		}
	}
	return tsscan.FunctionDef{}, false
}

// definitionStart resolves the first line of the WHOLE definition, which may
// sit above the line the scanner pins the name to. The scanner anchors
// FunctionDef.StartLine at the function NAME, so this walks back over a
// return type that wrapped onto its own line:
//
//	int
//	fn_min_check(char *a, char *b)
//	{ ... }
//
// It stops conservatively — at a blank line, a preprocessor directive, a
// comment edge, or a line ending in ; { } — because a wrong start line
// would splice a fragment of unrelated code into the caller, which is worse
// than lifting the definition one line short (the grammar recovers, and the
// cost is a missing return type the plan layer reports as an unreadable
// declaration rather than as corrupt output).
func definitionStart(src string, def tsscan.FunctionDef) int {
	lines := strings.Split(src, "\n")
	start := def.StartLine
	for start > 1 {
		prev := strings.TrimSpace(lines[start-2])
		if prev == "" || isDeclBoundary(prev) || isCommentEdge(prev) {
			break
		}
		// A wrapped return type is a bare type word: "int", "unsigned
		// long", "static char *". Anything with a paren, comma, or
		// semicolon is a declaration of something else.
		if !isTypeishLine(prev) {
			break
		}
		start--
	}
	return start
}

// isDeclBoundary reports a line that cannot be part of a return type: it
// ends a statement, opens a body, or closes one.
func isDeclBoundary(line string) bool {
	switch {
	case strings.HasSuffix(line, ";"), strings.HasSuffix(line, "{"), strings.HasSuffix(line, "}"):
		return true
	}
	return false
}

// isCommentEdge reports a comment's first or last line.
func isCommentEdge(line string) bool {
	return strings.HasPrefix(line, "/*") || strings.HasPrefix(line, "*") ||
		strings.HasSuffix(line, "*/")
}

// isTypeishLine reports a line that could be a bare C type specifier — the
// shape a wrapped return type takes.
func isTypeishLine(line string) bool {
	if strings.ContainsAny(line, "(),;{}=") {
		return false
	}
	if line == "" {
		return false
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '_', c == '*':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ' ':
		default:
			return false
		}
	}
	return true
}

// spanCalls returns the distinct function names called INSIDE a callee's
// definition span, in first-appearance order. A name called on a line
// outside the span belongs to some other function in the same file and is
// not this helper's dependency.
//
// This is the dependency set the expansion follows, and it has to be read
// from the span rather than from the callee file's ExternalFns: that record
// holds the symbols the callee's own file leaves UNDEFINED, so a sibling
// helper defined right beside it is local there and absent from it — and
// lifting the first without the second would leave the expanded caller with
// an undeclared call.
func spanCalls(cal *callee) []string {
	seen := map[string]bool{}
	var out []string
	for i := range cal.facts.Calls {
		c := &cal.facts.Calls[i]
		if c.Line < cal.start || c.Line > cal.end || c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c.Name)
	}
	return out
}

// fmlInside reports the FML or tpcall call a callee's body makes, or "" when
// the body is clean. A helper touching the service's buffers is out of
// contract — the buffers are the caller's scope — and lifting it would let
// its traffic be attributed to the caller's request/response contract, so
// the pass refuses rather than quietly producing a wrong contract.
func fmlInside(cal *callee) string {
	seen := map[string]bool{}
	var names []string
	for i := range cal.facts.Calls {
		c := &cal.facts.Calls[i]
		if c.Func != cal.name {
			continue
		}
		var offending string
		switch {
		case c.Name == "Fget32", c.Name == "Fadd32",
			strings.EqualFold(c.Name, "Foccur32"), strings.EqualFold(c.Name, "Foccur"):
			offending = c.Name
		case c.IsTpCall:
			offending = c.Name
		}
		if offending == "" || seen[offending] {
			continue
		}
		seen[offending] = true
		names = append(names, offending)
	}
	return strings.Join(names, ", ")
}
