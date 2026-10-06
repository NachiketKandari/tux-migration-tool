package plan

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MarshalJSON renders the plan deterministically (sorted units, stable field
// order via the struct).
func (p *Plan) Marshal() ([]byte, error) {
	SortUnits(p.Units)
	return json.MarshalIndent(p, "", "  ")
}

// WriteJSON emits plan.json — the machine twin the convert stage consumes.
func WriteJSON(w io.Writer, p *Plan) error {
	data, err := p.Marshal()
	if err != nil {
		return fmt.Errorf("plan: marshal: %w", err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// WriteMD emits plan.md — the human-readable twin. The mapping file is the
// edit surface; plan.json ids/refs are immutable (plan-conversion §3).
func WriteMD(w io.Writer, p *Plan) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Conversion plan — %s\n\n", p.Service)
	fmt.Fprintf(&sb, "- source: `%s`\n- module: `%s`\n- endpoints mapped: %d (user-specified, §4.2.8)\n\n",
		p.Source, p.Module, len(p.Mapping.Endpoints))

	sb.WriteString("## Endpoints\n\n| condition | method | route |\n|---|---|---|\n")
	for _, e := range p.Mapping.Endpoints {
		fmt.Fprintf(&sb, "| %d | `%s` | `%s` |\n", e.Condition, e.Name, e.Route)
	}

	fmt.Fprintf(&sb, "\n## Units (%d)\n\n| id | kind | name | target | template | llm | tokens | deps |\n|---|---|---|---|---|---|---|---|\n", len(p.Units))
	total := 0
	for _, u := range p.Units {
		llm := "—"
		if u.LLM {
			llm = "llm"
		}
		deps := strings.Join(u.Deps, ",")
		fmt.Fprintf(&sb, "| %s | %s | %s | `%s` | %s | %s | %d | %s |\n",
			u.ID, u.Kind, u.Name, u.TargetPath, u.TemplateID, llm, u.TokenEstimate, deps)
		total += u.TokenEstimate
	}
	fmt.Fprintf(&sb, "\nEstimated generation input: ~%d tokens (chars/4 heuristic).\n", total)

	if len(p.Skipped) > 0 {
		sb.WriteString("\n## Skipped IR units\n\n| query | reason |\n|---|---|\n")
		for _, s := range p.Skipped {
			fmt.Fprintf(&sb, "| `%s` | %s |\n", s.QueryID, s.Reason)
		}
	}
	if len(p.Stubs) > 0 {
		sb.WriteString("\n## Stubs — unresolved external fns converted as panicking placeholders (stub and carry on, 2026-09-10)\n\n")
		for _, b := range p.Stubs {
			fmt.Fprintf(&sb, "- `%s`: %s (endpoints: %s)\n", b.Fn, b.Reason, strings.Join(b.Endpoints, ", "))
		}
	}
	if len(p.Dropped) > 0 {
		sb.WriteString("\n## Dropped constructs (§4.8.4)\n\n")
		for _, d := range p.Dropped {
			fmt.Fprintf(&sb, "- %s\n", d)
		}
	}
	if len(p.Warnings) > 0 {
		// The unmapped-arm advisories (G-SCEN1 spirit): the tool never
		// invents endpoints, but it never stays silent about a reachable
		// dispatch arm either (engine-wiring audit Tier-1 #5: these were
		// computed and dropped from every plan artifact).
		sb.WriteString("\n## Coverage warnings — reachable arms no endpoint maps (map them or they stay logic-only)\n\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&sb, "- %s\n", w)
		}
	}
	_, err := io.WriteString(w, sb.String())
	return err
}
