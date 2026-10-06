package ir

import (
	"sort"
	"strings"
)

// The cross-call join: what the caller puts in an S buffer against what the
// callee reads out of it, and the same for the reply.
//
// A tpcall's contract is RUNTIME DATA, not a C signature. The callee is
// `void SVC_X(TPSVCINFO* rqst)` and its real parameter is the FML buffer at
// rqst->data, so there is no argument list to bind the way there is for an
// fn_* call. Two files agree on FML FIELD NAMES and nothing else — the host
// variables holding a field are unrelated on the two sides. That is the
// whole reason this is a join keyed on field name.
//
// Both halves already exist before this file runs:
//
//   - the caller's side is TPCall.SendFields / RecvFields, the buffer states
//     at the call line (see srwindow.go);
//   - the callee's side is its own fml_ops plus its resolved buffer roles —
//     ibuffer reads are what it expects, obuffer writes are what it produces.
//
// So this is a join, not new extraction. Its entire job is to line the two
// up and to say, out loud, where they do not line up.
//
// THE FALLBACK IS THE POINT. joinTPCalls runs in directory mode only, after
// service resolution, and it populates TPCall.Callee only when exactly one
// corpus file defined the service. Every tpcall in the tracked corpus names a
// callee that is referenced but never defined, so Callee stays nil for all
// of them and the KindTPCall placeholder is produced exactly as before. A
// nil Callee is not a failure state — it is the signal that says "the
// placeholder is correct here", and it is the majority case by design.

// joinTPCalls populates TPCall.Callee (or TPCall.JoinRefusal) for every tpcall
// whose callee is present in the corpus. Called once, from directory mode,
// after service files have been resolved — the join needs the whole corpus,
// which is precisely why it cannot run in single-file mode.
func joinTPCalls(files []*File) {
	byPath := make(map[string]*File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	for _, f := range files {
		for i := range f.TPCalls {
			tc := &f.TPCalls[i]
			if tc.Service == "" {
				continue
			}
			matches := serviceFileList(tc.ServiceFile)
			if len(matches) == 0 {
				// No callee file in the corpus. NOT a refusal: this is the
				// long-standing placeholder path and its output has to stay
				// byte-identical, so nothing is recorded here at all.
				continue
			}
			if len(matches) > 1 {
				// Picking one of several files that define the service
				// would be a guess, and a wrong guess produces a wrong
				// binding rather than a missing one.
				tc.JoinRefusal = RefuseAmbiguousCallee
				continue
			}
			calleeFile, ok := byPath[matches[0]]
			if !ok {
				tc.JoinRefusal = RefuseCalleeNoEntry
				continue
			}
			callee, refusal := projectCallee(tc, calleeFile)
			if refusal != "" {
				tc.JoinRefusal = refusal
				continue
			}
			tc.Callee = callee
		}
	}
}

// serviceFileList splits TPCall.ServiceFile, which resolution stores as a
// comma-joined list precisely so that an ambiguous match is representable.
// An unresolvable service is the empty string, not a one-element list
// containing it.
func serviceFileList(joined string) []string {
	if joined == "" {
		return nil
	}
	parts := strings.Split(joined, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// projectCallee builds the callee's contract from its own IR and joins it
// against the caller's buffers. A non-empty refusal means Callee must stay
// nil.
func projectCallee(tc *TPCall, g *File) (*TPCallee, TPJoinRefusal) {
	if g.Entry == "" {
		return nil, RefuseCalleeNoEntry
	}
	span, ok := g.functionSpan(g.Entry)
	if !ok {
		// The file defines the service by base name but has no SVC_ entry
		// to attribute ops to. Its Fget32s may belong to a helper, and
		// folding a helper's read into "the service expects" would report
		// a dependency the service does not have and could bind the wrong
		// variable. Refuse instead of approximating.
		return nil, RefuseCalleeNoEntry
	}
	inBufs, outBufs := g.buffersByRole()
	if len(inBufs) == 0 && len(outBufs) == 0 {
		return nil, RefuseCalleeNoBuffers
	}
	c := &TPCallee{Service: tc.Service, Path: g.Path, Entry: g.Entry}
	c.Expects, c.Produces = calleeContract(g, span, inBufs, outBufs)
	joinSides(tc, c)
	return c, ""
}

// fieldAcc accumulates one field's accesses across the callee's entry body,
// keeping first-seen order so the projected contract follows source order
// rather than map iteration order.
type fieldAcc struct {
	order  int
	fields []TPField
}

// calleeContract is the callee's own side: every field its entry function
// reads out of an input buffer, and every field it writes into an output
// one.
//
// This is a UNION over the entry function's body, not a replay like
// sendFields/recvFields. Those answer "what is in the buffer at THIS line",
// where last-write-wins is the only correct answer. This answers "what can
// this service touch at all", where the correct answer is every field any
// path touches — a guarded read on a cold branch is still a dependency, and
// dropping it would report the caller as complete when it is not. Union is
// also the safe direction: it can only add a field, which surfaces as a
// reported finding rather than a silent wrong binding.
//
// f.FmlOps holds preamble ops and f.Conditions[i].FmlOps hold conditional
// ones, so both are walked; an op outside the entry body's span belongs to
// another function and is skipped.
func calleeContract(g *File, span FuncSpan, inBufs, outBufs map[string]bool) (expects, produces []TPField) {
	in, out := map[string]*fieldAcc{}, map[string]*fieldAcc{}
	record := func(m map[string]*fieldAcc, field string, op FmlOp) {
		a, ok := m[field]
		if !ok {
			a = &fieldAcc{order: len(m)}
			m[field] = a
		}
		a.fields = append(a.fields, TPField{
			Field:     field,
			Target:    op.Target,
			Line:      op.Line,
			Composite: op.Composite,
			Optional:  op.Optional,
		})
	}
	visit := func(op FmlOp) {
		if !span.contains(op.Line) {
			return
		}
		switch op.Kind {
		case FmlGet:
			if inBufs[op.Buffer] {
				record(in, op.Field, op)
			}
		case FmlAdd:
			if outBufs[op.Buffer] {
				record(out, op.Field, op)
			}
		}
	}
	for _, op := range g.FmlOps {
		visit(op)
	}
	for i := range g.Conditions {
		for _, op := range g.Conditions[i].FmlOps {
			visit(op)
		}
	}
	return collapse(in), collapse(out)
}

// collapse folds a field's accesses into one TPField, in source order, and
// reports a field that one side touches through more than one variable. A
// field read into two variables has no single binding, and silently taking
// the first would name a variable that is only one of the destinations.
func collapse(m map[string]*fieldAcc) []TPField {
	fields := make([]*fieldAcc, 0, len(m))
	for _, a := range m {
		fields = append(fields, a)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].order < fields[j].order })
	out := make([]TPField, 0, len(fields))
	for _, a := range fields {
		out = append(out, a.fields[0])
	}
	return out
}

// joinSides lines the two sides up on field name and records every field that
// only one side touches.
func joinSides(tc *TPCall, c *TPCallee) {
	sent := indexFields(tc.SendFields)
	read := indexFields(tc.RecvFields)
	expects := indexFields(c.Expects)
	produces := indexFields(c.Produces)

	// Every field any of the four sides touches, sorted, so the output is
	// a function of the corpus and not of map iteration order.
	all := map[string]bool{}
	for _, m := range []map[string]TPField{sent, read, expects, produces} {
		for k := range m {
			all[k] = true
		}
	}
	names := make([]string, 0, len(all))
	for k := range all {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		sf, sends := sent[name]
		rf, reads := read[name]
		xf, wants := expects[name]
		pf, makes := produces[name]
		in := sends && wants
		out := makes && reads
		if in || out {
			b := TPBinding{Field: name}
			if sends {
				b.CallerVar = sf.Target
			}
			if wants {
				b.CalleeVar = xf.Target
			}
			if makes {
				b.CalleeVar = pf.Target
			}
			if reads {
				b.CallerVar = rf.Target
				b.CallerUnchecked = rf.Unchecked
			}
			switch {
			case in && out:
				b.Direction = "inout"
			case in:
				b.Direction = "in"
			default:
				b.Direction = "out"
			}
			b.Composite = sf.Composite || rf.Composite || xf.Composite || pf.Composite
			c.Bindings = append(c.Bindings, b)
		}
		// One side touches the field and the other does not.
		if wants && !sends {
			c.Issues = append(c.Issues, fnotpresIssue(IssueCalleeExpectsUnsent, name,
				xf.Target, "callee", "sends"))
		}
		if reads && !makes {
			c.Issues = append(c.Issues, fnotpresIssue(IssueCallerReadsUnwritten, name,
				rf.Target, "caller", "writes"))
		}
		if sends && !wants {
			c.Issues = append(c.Issues, deadTrafficIssue(IssueSentUnread, name,
				sf.Target, "caller", "sends this field", "never reads it"))
		}
		if makes && !reads {
			c.Issues = append(c.Issues, deadTrafficIssue(IssueWrittenUnread, name,
				pf.Target, "callee", "writes this field", "never reads it back"))
		}
		if wants && splitTarget(c.Expects, name) {
			c.Issues = append(c.Issues, TPIssue{
				Code:      IssueFieldSplitTarget,
				Field:     name,
				CalleeVar: xf.Target,
				Note: "The callee touches this field through more than one " +
					"variable, so there is no single binding to report. The " +
					"first in source order is shown; an inline rewrite has to " +
					"write all of them.",
			})
		}
	}
}

// indexFields keys a field list by field name, first occurrence winning.
func indexFields(fields []TPField) map[string]TPField {
	m := make(map[string]TPField, len(fields))
	for _, f := range fields {
		if _, ok := m[f.Field]; !ok {
			m[f.Field] = f
		}
	}
	return m
}

// splitTarget reports whether a field is accessed through more than one
// target variable within one field list.
func splitTarget(fields []TPField, name string) bool {
	first, seen := "", false
	for _, f := range fields {
		if f.Field != name {
			continue
		}
		if !seen {
			first, seen = f.Target, true
			continue
		}
		if f.Target != first {
			return true
		}
	}
	return false
}

// fnotpresNote is the shared explanation for BOTH FNOTPRES join findings,
// because the mechanism is identical in each and stating it once keeps the
// two from drifting apart.
//
// It is deliberately explicit that the destination is not filled with
// garbage, because that is the natural wrong assumption and it is what makes
// these findings look like errors. Fget32 on an absent field returns -1 and
// sets Ferror32 to FNOTPRES; it leaves the destination variable UNTOUCHED.
// The read fails; the variable keeps whatever it held. For a fresh local
// that is indeterminate in C, and a converted Go build gets the zero value —
// defined, but not necessarily what the C original produced. So the honest
// severity is "a default-value dependency", not "a fault", and the way to
// find out whether it matters is to check whether anything downstream needs
// the field to have been present.
const fnotpresNote = "Fget32 of a field the buffer does not carry returns -1 and " +
	"sets Ferror32 = FNOTPRES. It does not fill the destination with garbage and " +
	"it does not read absent storage: the read fails and leaves the destination " +
	"UNTOUCHED, so the variable keeps whatever value it held beforehand. For a " +
	"freshly declared local that is indeterminate in C, and a converted Go build " +
	"gets the zero value, which is defined but may differ from the C original. " +
	"So this is a default-value dependency rather than a fault, and it is only a " +
	"real problem if something downstream requires the field to have been " +
	"present. Code that cares tests the Fget32 return value; an unguarded read " +
	"does not, which is why the dependency is invisible at the call site."

// fnotpresIssue builds either FNOTPRES finding, naming the variable on the
// side that performs the read and what the other side failed to do with it.
func fnotpresIssue(code TPIssueCode, field, varName, side, missing string) TPIssue {
	iss := TPIssue{Code: code, Field: field, Note: fnotpresNote}
	if varName != "" {
		iss.CalleeVar = varName
		iss.CallerVar = varName
		iss.Note = "The " + side + " reads this field into " + varName +
			", and the other side never " + missing + " it. " + fnotpresNote
	}
	return iss
}

// deadTrafficIssue builds a sent_unread / written_unread finding. These are
// NOT FNOTPRES cases and carry no default-value semantics: nothing fails,
// there is simply traffic the other side never consumes.
func deadTrafficIssue(code TPIssueCode, field, varName, side, verb, tail string) TPIssue {
	iss := TPIssue{Code: code, Field: field, CallerVar: varName, CalleeVar: varName}
	iss.Note = "The " + side + " " + verb + " (" + varName +
		") and the other side " + tail + ". Dead traffic, not a fault: it costs " +
		"a buffer slot and nothing else."
	return iss
}

// functionSpan returns the body span for a named function.
func (f *File) functionSpan(name string) (FuncSpan, bool) {
	for _, s := range f.FunctionSpans {
		if s.Name == name {
			return s, true
		}
	}
	return FuncSpan{}, false
}

// contains reports whether a 1-based source line falls inside the span. A
// zero BodyEnd means the scanner never closed the body, so the span is open
// upward: a zero-width span would drop every real op in it, which is the
// wrong way to fail.
func (s FuncSpan) contains(line int) bool {
	if line < s.BodyStart {
		return false
	}
	if s.BodyEnd <= 0 {
		return true
	}
	return line <= s.BodyEnd
}

// buffersByRole partitions the file's FML buffer variables by resolved role.
func (f *File) buffersByRole() (in, out map[string]bool) {
	in, out = map[string]bool{}, map[string]bool{}
	for _, b := range f.Buffers {
		switch b.Role {
		case BufferInput:
			in[b.Name] = true
		case BufferOutput:
			out[b.Name] = true
		}
	}
	return in, out
}
