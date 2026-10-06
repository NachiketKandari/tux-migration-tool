package gen

import (
	"fmt"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/plan"
)

// fmlFields returns the deduplicated FML field list of an op set, in
// appearance order, skipping dropped plumbing (§4.8.4).
func fmlFields(ops []ir.FmlOp) []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Dropped || op.Field == "" || seen[op.Field] {
			continue
		}
		seen[op.Field] = true
		out = append(out, op.Field)
	}
	return out
}

// tpcallUnits returns the plan's tpcall units in ID order.
func tpcallUnits(p *plan.Plan) []plan.Unit {
	var out []plan.Unit
	for _, u := range sortedUnits(p) {
		if u.Kind == plan.KindTPCall && u.TP != nil {
			out = append(out, u)
		}
	}
	return out
}

// PlaceholderFile renders controller/tpcall_placeholders.go (PF-4.5): one
// compilable stub per tpcall unit, each carrying its full send/recv FML
// contract in a standardized `tuxconv:TODO` marker. The stub returns zero
// values plus errPlaceholder — Tier A/B stay green by construction, the
// build never breaks, and `tuxconv:TODO` greps the complete inventory of
// external gaps.
func (s *Service) PlaceholderFile(p *plan.Plan) (string, error) {
	units := tpcallUnits(p)
	if len(units) == 0 {
		return "", fmt.Errorf("gen: plan has no tpcall units")
	}
	var sb strings.Builder
	sb.WriteString("package controller\n\n")
	sb.WriteString("import \"errors\"\n\n")
	sb.WriteString("// errPlaceholder marks every unwired external-interaction stub in this\n")
	sb.WriteString("// file. The build never breaks; `tuxconv:TODO` greps the gap inventory (PF-4.5).\n")
	sb.WriteString("var errPlaceholder = errors.New(\"tuxconv placeholder: external service call not wired\")\n")
	for _, u := range units {
		sb.WriteString("\n" + s.tpcallStub(u) + "\n")
	}
	return gofmt(sb.String())
}

// tpcallStub renders one placeholder function: marker comment block with the
// complete contract, then a stub returning zero values + errPlaceholder.
func (s *Service) tpcallStub(u plan.Unit) string {
	tp := u.TP
	send := fmlFields(tp.SendFML)
	recv := fmlFields(tp.RecvFML)
	if len(send) == 0 {
		send = []string{"(none correlated)"}
	}
	if len(recv) == 0 {
		recv = []string{"(none correlated)"}
	}
	reason := "the target Go service has no outbound-call convention yet (R8); wire by hand or upgrade with a tpcalls mapping pin (PF-4.7)"
	if tp.Ambiguous {
		if tp.SendBuffer == "" || tp.RecvBuffer == "" {
			reason = "send/recv buffer variables could not be correlated at the call site (PF-4.3) — the contract is recorded as ambiguous"
		} else {
			reason = "buffers were identified but the call's block contains no FML ops — the send/recv contract is empty; wire by hand"
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "// tuxconv:TODO tp:%s — %s:%s\n", tp.Service, filepath.Base(u.SourceFile), u.SourceLines)
	if tp.ServiceFile != "" {
		fmt.Fprintf(&sb, "// target: %s (the corpus file(s) behind service %s)\n", tp.ServiceFile, tp.Service)
	}
	fmt.Fprintf(&sb, "// send: %s\n", strings.Join(send, ", "))
	fmt.Fprintf(&sb, "// recv: %s\n", strings.Join(recv, ", "))
	fmt.Fprintf(&sb, "// reason: %s\n", reason)
	fmt.Fprintf(&sb, "func %s(send map[string]string) (recv map[string]string, err error) {\n", u.Name)
	sb.WriteString("\treturn nil, errPlaceholder\n")
	sb.WriteString("}")
	return sb.String()
}

// PlaceholderSignatures lists the tpcall placeholder functions an endpoint's
// controller body may call — the prompt-side contract of PF-4.5. Names come
// from the plan units themselves, so prompt and stub can never drift.
// Endpoints without tpcalls get an empty list (prompts unchanged).
func (s *Service) PlaceholderSignatures(c *ir.Condition, p *plan.Plan) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, u := range sortedUnits(p) {
		if u.Kind != plan.KindTPCall || u.TP == nil {
			continue
		}
		if c.ContainsLine(u.TP.StartLine) {
			sig := u.Name + "(send map[string]string) (recv map[string]string, error)"
			if u.TP.ServiceFile != "" {
				sig += " — target corpus file: " + u.TP.ServiceFile
			}
			out = append(out, sig)
		}
	}
	return out
}
