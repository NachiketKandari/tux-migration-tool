package ir

import (
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// allFmlOps classifies every Fget32/Fadd32/Fdel32/Foccur32 call into an ordered
// FmlOp list, in source order. Foccur32/Foccur presence checks join as Get-kind
// ops (the counted field belongs to the request contract even when the arm
// never Fget-reads it — the existence test is the read); Fdel32 joins as a
// Del-kind op so a removed field stops being reported as part of a buffer's
// contents.
func allFmlOps(facts *tsscan.SourceFacts, f *File, opts Options) []FmlOp {
	out := make([]FmlOp, 0, 8)
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Name != "Fget32" && c.Name != "Fadd32" && !isFdelCallName(c.Name) &&
			!isFoccurCallName(c.Name) {
			continue
		}
		if op, ok := FmlOpOf(c, facts); ok {
			out = append(out, op)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isFdelCallName reports the field-removal spellings. Fdel32 is the FML32
// form; Fdel is the shorthand, matched case-insensitively because the
// corpus varies — same posture as isFoccurCallName.
func isFdelCallName(name string) bool {
	return name == "Fdel32" || strings.EqualFold(name, "Fdel")
}

// isFoccurCallName reports the FML occurrence-count spellings:
// Foccur32 and the Foccur shorthand (case-insensitive — the corpus
// varies, and the logical part treats every spelling as existence).
func isFoccurCallName(name string) bool {
	return strings.EqualFold(name, "Foccur32") || strings.EqualFold(name, "Foccur")
}

// FmlOpOf is the exported FML classifier (flow consumes it for per-node
// annotation): Fget32 reads the request in, Fadd32 writes the response out,
// Foccur32/Foccur counts a field's occurrences (presence — a Get-kind read
// with no host target); field = 2nd arg, target = 4th (get) / 3rd (add),
// buffer = 1st. Optional marks an FNOTPRES-guarded read; Dropped marks
// session/error plumbing; Code is the legacy error-message code harvested
// from the latest preceding writer of the target variable (or the add's
// own literal).
func FmlOpOf(call *tsscan.FunctionCall, facts *tsscan.SourceFacts) (FmlOp, bool) {
	if isFoccurCallName(call.Name) {
		return foccurOpOf(call)
	}
	var kind FmlOpKind
	switch {
	case call.Name == "Fget32":
		kind = FmlGet
	case call.Name == "Fadd32":
		kind = FmlAdd
	case isFdelCallName(call.Name):
		// Fdel32(buf, field) has no value operand; it is classified by
		// its own shape (2 args, no target).
		return fdelOpOf(call)
	default:
		return FmlOp{}, false
	}
	targetIdx := 3
	if kind == FmlAdd {
		targetIdx = 2
	}
	args := splitArgs(call.Args)
	if len(args) < 4 {
		return FmlOp{}, false
	}
	field := strings.TrimSpace(args[1])
	if n, ok := identifierArg(args[1]); ok {
		field = n
	}
	op := FmlOp{
		Kind:   kind,
		Field:  field,
		Buffer: baseIdent(args[0]),
		Line:   call.Line,
	}
	if targetIdx < len(args) {
		op.Target = baseIdent(args[targetIdx])
		op.Composite = isCompositeArg(args[targetIdx])
	}
	if kind == FmlGet {
		op.Optional = fnotpresGuarded(facts, call)
	}
	op.Dropped = op.Field == "FML_USER_ID" || op.Field == "FML_SESSION_ID" || IsErrField(op.Field)
	if kind == FmlAdd {
		op.Code = harvestCode(facts, call, op.Target)
	}
	return op, true
}

// isCompositeArg reports whether an FML value/destination argument is an
// expression rather than a bare host variable or a string literal. A
// literal is excluded deliberately — its value is known outright, so nothing
// about it is partial. `(char*)&x` and `x` are bare once the cast is
// stripped, so only real expressions (`c_buf[i]`, `*p`, `f(x)`) count, and
// for those Target holds only the leading identifier.
func isCompositeArg(arg string) bool {
	if stringLit(arg) != "" {
		return false
	}
	if _, ok := identifierArg(arg); ok {
		return false
	}
	return true
}

// fdelOpOf classifies Fdel32(buf, field): the field leaves the buffer. Like
// the Foccur shape it needs only 2 args, and like an add it carries no host
// variable — the removal's whole effect is on the buffer's state.
func fdelOpOf(call *tsscan.FunctionCall) (FmlOp, bool) {
	args := splitArgs(call.Args)
	if len(args) < 2 {
		return FmlOp{}, false
	}
	field := strings.TrimSpace(args[1])
	if n, ok := identifierArg(args[1]); ok {
		field = n
	}
	if field == "" {
		return FmlOp{}, false
	}
	op := FmlOp{
		Kind:   FmlDel,
		Field:  field,
		Buffer: baseIdent(args[0]),
		Line:   call.Line,
	}
	op.Dropped = op.Field == "FML_USER_ID" || op.Field == "FML_SESSION_ID" || IsErrField(op.Field)
	return op, true
}

// foccurOpOf classifies an Foccur32/Foccur presence count as a Get-kind op:
// the counted field (2nd arg) belongs to the request contract; the buffer
// (1st arg) is recorded for audit; there is no host target — existence
// itself is the read. Needs only 2 args (`Foccur32(buf, FIELD)`), not the
// 4-arg Fget shape.
func foccurOpOf(call *tsscan.FunctionCall) (FmlOp, bool) {
	args := splitArgs(call.Args)
	if len(args) < 2 {
		return FmlOp{}, false
	}
	field := strings.TrimSpace(args[1])
	if n, ok := identifierArg(args[1]); ok {
		field = n
	}
	if field == "" {
		return FmlOp{}, false
	}
	op := FmlOp{
		Kind:   FmlGet,
		Field:  field,
		Buffer: baseIdent(args[0]),
		Line:   call.Line,
	}
	op.Dropped = op.Field == "FML_USER_ID" || op.Field == "FML_SESSION_ID" || IsErrField(op.Field)
	return op, true
}

// fnotpresGuarded reports whether the get's own guard block (the if on the
// call's line) contains an FNOTPRES check.
func fnotpresGuarded(facts *tsscan.SourceFacts, call *tsscan.FunctionCall) bool {
	for i := range facts.Branches {
		g := &facts.Branches[i]
		if g.BlockStart == 0 || g.StartLine != call.Line {
			continue
		}
		for j := range facts.Branches {
			b := &facts.Branches[j]
			if !strings.Contains(b.Cond, "FNOTPRES") || b.StartLine == 0 {
				continue
			}
			if containsSpan(g.BlockStart, g.BlockStartCol, g.BlockEnd, g.BlockEndCol, b.StartLine, b.StartCol) {
				return true
			}
		}
	}
	return false
}

// harvestCode is the legacy error-code correlation: the latest preceding
// same-function errlog/strcpy/sprintf writer of the target variable owns
// the variable's error code; without one, the add's own string literal; a
// define-named or absent code degrades to "".
func harvestCode(facts *tsscan.SourceFacts, call *tsscan.FunctionCall, target string) string {
	if target != "" {
		best := -1
		bestCode := ""
		for i := range facts.Calls {
			w := &facts.Calls[i]
			if w.Line >= call.Line || w.Func != call.Func {
				continue
			}
			code, writerTarget, ok := writerCode(w)
			if !ok || writerTarget != target {
				continue
			}
			if best < 0 || w.Line > facts.Calls[best].Line {
				best, bestCode = i, code
			}
		}
		if best >= 0 {
			return bestCode
		}
	}
	args := splitArgs(call.Args)
	if len(args) > 2 {
		return stringLit(args[2])
	}
	return ""
}

// writerCode extracts (code, targetVar) from an errlog/strcpy/sprintf call:
// errlog's code is the 2nd arg with the target as the last; strcpy and
// sprintf carry (target, code) as the first two args.
func writerCode(w *tsscan.FunctionCall) (code, target string, ok bool) {
	switch w.Name {
	case "errlog":
		args := splitArgs(w.Args)
		if len(args) < 3 {
			return "", "", false
		}
		code = stringLit(args[1])
		target = baseIdent(args[len(args)-1])
		return code, target, true
	case "strcpy", "sprintf":
		args := splitArgs(w.Args)
		if len(args) < 2 {
			return "", "", false
		}
		return stringLit(args[1]), baseIdent(args[0]), true
	}
	return "", "", false
}

// preambleOps collects the entry function's FML traffic outside every
// condition span; files with no entry carry none.
func preambleOps(facts *tsscan.SourceFacts, f *File, ops []FmlOp) []FmlOp {
	if f.Entry == "" {
		return nil
	}
	out := make([]FmlOp, 0, 8)
	for _, op := range ops {
		if f.entryOwns(op.Line) {
			out = append(out, op)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// entryOwns reports whether the line belongs to the entry function and
// lies outside every condition span.
func (f *File) entryOwns(line int) bool {
	if f.Entry != "" && f.Entry != "__fragment" {
		inEntry := false
		for _, fn := range f.Functions {
			if fn == f.Entry {
				inEntry = true
			}
		}
		if !inEntry {
			return false
		}
	}
	for i := range f.Conditions {
		if f.Conditions[i].ContainsLine(line) {
			return false
		}
	}
	return true
}

// containsSpan reports whether (l,c) lies inside [start..end).
func containsSpan(sL, sC, eL, eC, l, c int) bool {
	beforeStart := sL > l || (sL == l && sC > c)
	atOrAfterEnd := l > eL || (l == eL && c >= eC)
	return !beforeStart && !atOrAfterEnd
}
