package ir

import (
	"sort"
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// This file folds a tpcall's FML op window into the buffer STATE at the call
// line — what TPCall.SendFields/RecvFields carry, and what any future
// service-call inliner has to bind against.
//
// The distinction that makes this a separate concern rather than a filter
// over the op log: an FML buffer is a map, not a stream. Adding a field
// twice leaves one value, deleting a field removes it, and re-freeing a
// buffer empties it. So "the ops before the call" and "the buffer at the
// call" are different questions, and only the second one is safe to bind a
// callee's parameters to — a callee bound to a field the caller deleted, or
// to a field left over from a previous call on a reused buffer, would be
// wired to a value that is not there.
//
// Everything here is a replay in source order over facts the scanner
// already produced, so the result is a pure function of the file: no clock,
// no map iteration, no inference.

// fieldDir selects which op kinds build a buffer's state.
type fieldDir int

const (
	dirSend fieldDir = iota // a send buffer is built by adds, emptied by deletes
	dirRecv                 // a recv buffer is read by gets; there is nothing to add or delete
)

// sendFields is the send buffer's contents when tpcall runs, replayed from
// lo..callLine. The window's lower bound is raised to the last buffer reset
// (see raisePastReset) so a reused buffer does not inherit the previous
// call's fields.
func sendFields(facts *tsscan.SourceFacts, ops []FmlOp, lines []string, buf string, lo, callLine int) []TPField {
	lo = raisePastReset(facts, lines, buf, lo, callLine)
	return replayFields(facts, ops, buf, lo, callLine, dirSend)
}

// recvFields is what the caller takes out of the reply buffer after the
// call, in [callLine..hi]. The reply buffer is produced by the call itself,
// so no reset can precede it inside this window and no bound adjustment
// applies — the Fget32s after the call are the whole story.
func recvFields(facts *tsscan.SourceFacts, ops []FmlOp, buf string, callLine, hi int) []TPField {
	return replayFields(facts, ops, buf, callLine, hi, dirRecv)
}

// replayFields applies every participating op on buf inside [lo,hi] in
// source order and returns the field set that survives. A del removes the
// field outright; a later add of the same field brings it back with the
// later line and target, which is exactly FML's last-write-wins behaviour.
//
// The returned slice is ordered by Field so the JSON is stable regardless of
// the order the writes arrived in.
func replayFields(facts *tsscan.SourceFacts, ops []FmlOp, buf string, lo, hi int, dir fieldDir) []TPField {
	if buf == "" {
		return nil
	}
	// Index of the winning write per field, plus the line it won on. Ops
	// arrive in source order, so a strict > keeps the last write.
	live := map[string]TPField{}
	for _, op := range ops {
		if op.Buffer != buf || op.Line < lo || op.Line > hi || op.Field == "" {
			continue
		}
		switch dir {
		case dirSend:
			switch op.Kind {
			case FmlAdd:
				live[op.Field] = TPField{
					Field:     op.Field,
					Target:    op.Target,
					Line:      op.Line,
					Composite: op.Composite,
				}
			case FmlDel:
				delete(live, op.Field)
			}
		case dirRecv:
			if op.Kind != FmlGet {
				continue
			}
			live[op.Field] = TPField{
				Field:     op.Field,
				Target:    op.Target,
				Line:      op.Line,
				Composite: op.Composite,
				Optional:  op.Optional,
				Unchecked: !returnValueChecked(facts, op.Line),
			}
		}
	}
	if len(live) == 0 {
		return nil
	}
	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]TPField, 0, len(names))
	for _, n := range names {
		out = append(out, live[n])
	}
	return out
}

// raisePastReset moves a window's lower bound past the last time the buffer
// was emptied. Three things do that, and all three are recognised here
// rather than in the scanner:
//
//   - tpfree(buf) and a zeroing memset(buf, 0, n) name the buffer directly,
//     so the call facts are enough.
//   - `buf = tpalloc(...)` does NOT name the buffer — tpalloc's arguments are
//     the type, the sub-type and the length. Binding the reset to a buffer
//     name is a statement shape, not a call shape, which is why this needs
//     the raw line (assignedBufferVar) rather than a new scanner fact.
//
// Without the tpalloc case, a service that reallocates one variable twice
// would carry the first allocation's fields into the second call, and a
// callee would be handed values the caller never sent on that pass. The
// corpus pairs each tpalloc with a tpfree so nothing there regresses, but a
// leaked reallocation is exactly the case that would be wrong.
func raisePastReset(facts *tsscan.SourceFacts, lines []string, buf string, lo, before int) int {
	best := lo - 1
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Line < lo || c.Line > before {
			continue
		}
		switch {
		case c.Name == "tpalloc":
			// `x = tpalloc(...)`: the reset binds to the assignment
			// target, which the call args cannot tell us.
			if assignedBufferVar(lineAt(lines, c.Line), "tpalloc") == buf && c.Line > best {
				best = c.Line
			}
		case isBufferResetCall(c.Name):
			args := splitArgs(c.Args)
			if len(args) == 0 {
				continue
			}
			// tpfree empties unconditionally; memset only in its zeroing
			// form (see isBufferResetCall).
			if c.Name == "memset" && (len(args) < 2 || !isZeroFillArg(args[1])) {
				continue
			}
			if baseIdent(args[0]) == buf && c.Line > best {
				best = c.Line
			}
		}
	}
	return best + 1
}

// lineAt returns 1-indexed source line n, or "" when the source is absent or
// shorter than the scanner's line numbering claims. Returning "" makes every
// line-shape check below decline, which is the safe direction: the field
// list can then only be staler, never confidently wrong.
func lineAt(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// assignedBufferVar reports the variable a call's result is assigned to on
// the same source line, or "" when the call is not the right-hand side of a
// simple assignment.
//
// This is deliberately narrow — it recognises exactly one shape, the Tuxedo
// allocation idiom `sbuffer = (char *)tpalloc("FML32",NULL,1024);`, and
// declines everything else. That is the right trade for a rule whose failure
// modes are asymmetric: a false positive resets a buffer that was not reset
// and silently DROPS a field the caller really does send, while a false
// negative keeps a stale field and is caught downstream as a loud mismatch.
// Erring toward the second is the only safe direction, so anything not
// clearly a plain assignment returns "".
//
// Two near-misses are excluded explicitly, and both would have been live bugs
// in the first draft of this function:
//
//   - `x == tpalloc(...)` is a test, not an assignment. The `=` that starts
//     the operator is preceded by whitespace, so a "look at the previous
//     char" test alone accepts it; the following char has to be checked too.
//   - The name is read as the identifier ENDING at the `=`, not the one
//     starting at the beginning of the line. `if((sbuffer = tpalloc(...)))`
//     is a real idiom, and reading left-to-right from the line start yields
//     the prefix "if".
func assignedBufferVar(line, callee string) string {
	at := strings.Index(line, callee+"(")
	if at < 0 {
		return ""
	}
	lhs := line[:at]
	// The rightmost `=` that is a real assignment is the one that assigns
	// to this call. Skip any `=` belonging to ==, !=, <= or >=, which is
	// why BOTH neighbours are examined: in `a == b` the operator's first
	// `=` is preceded by a space and its second by `=`.
	eq := -1
	for i := len(lhs) - 1; i >= 0; i-- {
		if lhs[i] != '=' {
			continue
		}
		if i > 0 {
			switch lhs[i-1] {
			case '=', '!', '<', '>':
				continue
			}
		}
		if i+1 < len(lhs) && lhs[i+1] == '=' {
			continue
		}
		eq = i
		break
	}
	if eq < 0 {
		return ""
	}
	// Walk left from the `=` over the name. Whitespace first: C spaces
	// around `=`, so `sbuffer = tpalloc(...)` puts a blank immediately
	// before the operator, and reading the identifier without skipping it
	// finds nothing adjacent and declines.
	j := eq
	for j > 0 && (lhs[j-1] == ' ' || lhs[j-1] == '\t') {
		j--
	}
	start := j
	for j > 0 && isIdentByteFor(lhs[j-1]) {
		j--
	}
	if j == start {
		return "" // nothing adjacent to the `=` — not an `x = f()` shape
	}
	// A member assignment (`s.arena = tpalloc(...)`) names a struct field,
	// not a buffer variable the FML ops could ever refer to.
	if j > 0 && (lhs[j-1] == '.' || lhs[j-1] == '>') {
		return ""
	}
	return lhs[j:start]
}

// isBufferResetCall reports the calls that can empty a buffer. memset is
// admitted only in its zeroing form: memset(b, 0, n) leaves no field, but
// memset(b, c, n) with c != 0 scribbles a byte pattern whose field
// semantics we cannot describe, and calling that a reset would be a guess.
// The fill byte is checked in raisePastReset, so a non-zero memset is
// treated as no reset — which leaves the field list possibly stale rather
// than confidently wrong.
func isBufferResetCall(name string) bool {
	return name == "tpfree" || name == "memset"
}

// isZeroFillArg reports whether a memset fill argument is zero: the integer
// 0 or a NUL character literal ('\0', '\000', and the other zero escapes).
func isZeroFillArg(arg string) bool {
	s := strings.TrimSpace(arg)
	if s == "0" {
		return true
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		body := s[1 : len(s)-1]
		return strings.Trim(body, "0") == ""
	}
	return false
}

// returnValueChecked reports whether the FML read on this line had its
// return value tested. The Tuxedo idiom is `if(Fget32(...) == -1)`, which
// the scanner records as a branch whose condition mentions the call and
// which starts on the call's own line. A bare `Fget32(...)` statement — the
// shape most recv reads actually take — starts no branch, so it is
// unchecked, and the destination keeps whatever it held if the field is
// absent.
func returnValueChecked(facts *tsscan.SourceFacts, line int) bool {
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.StartLine != line || b.StartLine == 0 {
			continue
		}
		if strings.Contains(b.Cond, "Fget32") || strings.Contains(b.Cond, "Foccur32") {
			return true
		}
	}
	return false
}
