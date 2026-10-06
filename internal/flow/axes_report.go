package flow

import (
	"fmt"
	"strconv"
	"strings"
)

// AxesReport is the axes artifact's JSON shape (scenario-filter plan §3):
// the ranked registry for one entry, written next to the scenario set as
// <entry>.axes.json. Deterministic: the slice is AxesFor's output, order
// included.
type AxesReport struct {
	Entry string          `json:"entry"`
	Axes  []*DispatchAxis `json:"axes"`
}

// RenderAxesMD renders the human twin of the axes registry: one row per
// axis with its rank, scenario key, ref/alias, kind, domain, site count,
// guard lines, and default-arm mark. Rank 0 is the slicer's own axis.
func RenderAxesMD(entry string, axes []*DispatchAxis) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Dispatch axes — %s\n\n", entry)
	sb.WriteString("Ranked registry (rank 0 is the slicer's own dispatch axis; a\n")
	sb.WriteString("`scenarioFilter` expression may intersect any listed axis).\n\n")
	if len(axes) == 0 {
		sb.WriteString("No dispatch axis detected — the entry folds as one whole-function scenario.\n")
		return sb.String()
	}
	sb.WriteString("| rank | var | ref | alias | kind | domain | sites | guard lines | default |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for i, a := range axes {
		alias := a.Alias
		if alias == "" {
			alias = "—"
		}
		def := ""
		if a.HasDefault {
			def = "yes (" + a.DefaultKey() + ")"
		}
		fmt.Fprintf(&sb, "| %d | %s | %s | %s | %s | %s | %d | %s | %s |\n",
			i, mdCell(a.Key()), mdCell(a.Ref), mdCell(alias), mdCell(string(a.Kind)),
			mdCell(strings.Join(a.Domain, ", ")), a.Sites, mdCell(joinInts(a.GuardLines)), mdCell(def))
	}
	return sb.String()
}

// mdCell escapes the table-breaking byte (guard against exotic ref text).
func mdCell(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
}

// joinInts renders guard lines compactly ("12, 15, 18").
func joinInts(vals []int) string {
	if len(vals) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}
