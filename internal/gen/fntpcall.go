package gen

import (
	"fmt"
	"sort"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
)

// TPCallNotRendered is the reason code for a tpcall site the deterministic
// path does not render. One is emitted per site.
//
// It exists because the alternative was worse than silence. The tpcall
// placeholder path (internal/convert renderTPCallPlaceholders) is
// entry-scoped: it reads plan.KindTPCall units, and plan builds those from
// the entry service's own tpcalls. The corpus entry SVC_DUMMY_PRFL has zero
// tpcalls — both of the file's tpcalls live inside fn helpers. So the
// placeholder never fired, and a tpcall reached the emitted tree as nothing
// at all: no call, no TODO, no reason code. A reader of the generated Go
// could not tell that a call to another service had been dropped.
//
// Option B of the tpcall decision would render these; it is deferred
// (docs/deterministic-walk-plan.md, "Pinned: TPCall is Option A"). Until
// then they are counted.
const TPCallNotRendered = "R-TPCALL"

// tpcallsFor returns the tpcall sites the IR attributes to one fn helper.
//
// The IR already records the owning function on each TPCall (ir.TPCall.
// Function), so this is a filter over ground truth rather than a scan for
// call-shaped text — the same reason the control-flow accounting walks the
// flow tree instead of grepping the source.
func (s *Service) tpcallsFor(helper string) []ir.TPCall {
	var out []ir.TPCall
	for _, tp := range s.Main.TPCalls {
		if tp.Function == helper {
			out = append(out, tp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartLine < out[j].StartLine })
	return out
}

// detFnTPCallTODOs renders one reason-coded gap per tpcall site in a helper.
//
// The message names the callee and the legacy line, because the gap is
// otherwise unattributable: the emitted statement list has no marker at the
// point the call would have been, and the reader has nothing to match the
// legacy source against.
func detFnTPCallTODOs(goName string, tps []ir.TPCall) []string {
	var out []string
	for _, tp := range tps {
		kind := "tpcall"
		if tp.Async {
			kind = "tpacall (async: the reply arrives via a later tpgetrply)"
		}
		out = append(out, fmt.Sprintf(
			"// tuxconv:TODO %s: %s calls %q at legacy line %d — a call into another "+
				"service is not rendered; %s returns the failure status so its caller's "+
				"check fires rather than continuing without the reply",
			TPCallNotRendered, kind, tp.Service, tp.StartLine, goName))
	}
	return out
}

// detFnTPCallTail is the return a helper carrying an unrendered tpcall must
// end on.
//
// This is the part of Option A that carries the weight. A tpcall is how the
// callee's data arrives; a helper that skips it has not done its work. But
// the generated callers test the legacy status convention — `== -1` for
// failure — so returning the legacy SUCCESS value lets execution walk straight
// past the check. In the corpus that path ends at
// UpdateRpdDummyProfileDevationq59, which writes the dummy profile to the
// database: a stub that silently succeeds and persists data it never fetched
// is worse than no stub at all.
//
// So any helper with an unrendered tpcall returns the failure status. That is
// deliberately uniform across helpers whether or not their SQL also rendered:
// the caller cannot tell which part ran, and partial work reported as success
// is the failure mode being closed.
func detFnTPCallTail(h *plan.FnHelper) string {
	if h.Return == "" {
		return "return"
	}
	return "return -1"
}
