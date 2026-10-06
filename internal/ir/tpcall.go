package ir

import (
	"tux-migration-tool/internal/tsscan"
)

// buildTPCalls correlates every tpcall with its FML contracts: send =
// Fadd32-into-send-buffer ops before the call, recv = Fget32-from-recv-
// buffer ops after it, both inside the innermost enclosing branch block
// (else the function body). Identified buffers with empty contracts on both
// sides are recorded as ambiguous — never guessed away.
//
// Async tpacall has no inline reply: the reply arrives via a later
// tpgetrply(cd, data, len, flags) in the same function, so the recv side
// correlates Fget32-from-tpgetrply-buffer ops after the tpgetrply line.
// No tpgetrply after the call leaves the recv side empty by construction.
//
// lines is the raw source, needed only to recognise `buf = tpalloc(...)` as
// a buffer reset (see assignedBufferVar); nil disables that and leaves the
// other two resets — tpfree and a zeroing memset — fully in force.
func buildTPCalls(facts *tsscan.SourceFacts, f *File, ops []FmlOp, opts Options, lines []string) []TPCall {
	var out []TPCall
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if !c.IsTpCall {
			continue
		}
		args := splitArgs(c.Args)
		tc := TPCall{
			StartLine: c.Line,
			EndLine:   c.Line,
			Function:  c.Func,
			Async:     c.Name == "tpacall",
		}
		if len(args) > 0 {
			tc.Service = stringLit(args[0])
		}
		if len(args) > 1 {
			tc.SendBuffer = baseIdent(args[1])
		}
		// tpcall(svc, send, sendlen, recv, recvlen, flags): args[3] is
		// the recv buffer. tpacall(svc, send, sendlen, flags): args[3]
		// is flags — never a buffer. Leave RecvBuffer empty for the
		// async variant; the reply arrives via tpgetrply.
		if len(args) > 3 && c.Name != "tpacall" {
			tc.RecvBuffer = baseIdent(args[3])
		}
		lo, hi := enclosingSpan(facts, c)
		if tc.Async {
			correlateAsyncRecv(facts, c, &tc, ops)
		}
		for _, op := range ops {
			switch {
			case op.Line < c.Line && op.Line >= lo && op.Buffer != "" && op.Buffer == tc.SendBuffer:
				// Adds AND deletes are send-side facts. Recording only
				// the adds would make the log disagree with the source
				// exactly where it matters — a field the caller removed
				// before calling.
				if op.Kind == FmlAdd || op.Kind == FmlDel {
					tc.SendFML = append(tc.SendFML, op)
				}
			// Sync only: async recv correlates from the tpgetrply line
			// (correlateAsyncRecv above) — the generic post-call window
			// would double-count the same reads.
			case c.Name != "tpacall" && op.Line > c.Line && op.Line <= hi && op.Buffer != "" && op.Buffer == tc.RecvBuffer:
				if op.Kind == FmlGet {
					tc.RecvFML = append(tc.RecvFML, op)
					if op.Line > tc.EndLine {
						tc.EndLine = op.Line
					}
				}
			}
		}
		tc.Ambiguous = tc.SendBuffer != "" && tc.RecvBuffer != "" &&
			len(tc.SendFML) == 0 && len(tc.RecvFML) == 0
		// The folded buffer state at the call line, over the same window
		// the op logs above cover. This is the view a service-call
		// inliner binds against: SendFields says what actually crosses
		// the call, RecvFields what the caller takes back and whether
		// each read was checked. See srwindow.go.
		tc.SendFields = sendFields(facts, ops, lines, tc.SendBuffer, lo, c.Line)
		tc.RecvFields = recvFields(facts, ops, tc.RecvBuffer, c.Line, hi)
		out = append(out, tc)
	}
	return out
}

// correlateAsyncRecv completes an async tpacall's recv side from the first
// tpgetrply after the call in the same function: tpgetrply's data arg
// (args[1]) is the reply buffer, and RecvFML gathers the Fget32 reads of
// that buffer after the tpgetrply line inside its enclosing span.
func correlateAsyncRecv(facts *tsscan.SourceFacts, c *tsscan.FunctionCall, tc *TPCall, ops []FmlOp) {
	var reply *tsscan.FunctionCall
	for i := range facts.Calls {
		g := &facts.Calls[i]
		if g.Name != "tpgetrply" || g.Func != c.Func || g.Line <= c.Line {
			continue
		}
		if reply == nil || g.Line < reply.Line {
			reply = g
		}
	}
	if reply == nil {
		return
	}
	rargs := splitArgs(reply.Args)
	if len(rargs) < 2 {
		return
	}
	tc.RecvBuffer = baseIdent(rargs[1])
	if tc.RecvBuffer == "" {
		return
	}
	rlo, rhi := enclosingSpan(facts, reply)
	for _, op := range ops {
		if op.Kind != FmlGet || op.Buffer == "" || op.Buffer != tc.RecvBuffer {
			continue
		}
		if op.Line > reply.Line && op.Line <= rhi && op.Line >= rlo {
			tc.RecvFML = append(tc.RecvFML, op)
			if op.Line > tc.EndLine {
				tc.EndLine = op.Line
			}
		}
	}
}

// enclosingSpan is the innermost branch block containing the call, else the
// enclosing function body, else the whole file.
func enclosingSpan(facts *tsscan.SourceFacts, c *tsscan.FunctionCall) (int, int) {
	lo, hi := 1, facts.NumLines
	if c.Func != "" {
		for i := range facts.Functions {
			fn := &facts.Functions[i]
			if fn.Name == c.Func && fn.BodyStartLine > 0 {
				lo = fn.BodyStartLine
				if fn.BodyEndLine > 0 {
					hi = fn.BodyEndLine
				}
			}
		}
	}
	// innermost braced branch block containing the call line
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.BlockStart == 0 || b.BlockEnd == 0 {
			continue
		}
		if b.BlockStart <= c.Line && c.Line <= b.BlockEnd {
			if b.BlockStart >= lo && b.BlockEnd <= hi {
				lo, hi = b.BlockStart, b.BlockEnd
			}
		}
	}
	return lo, hi
}
