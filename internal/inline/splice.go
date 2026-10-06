package inline

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/tsscan"
)

// banner is the provenance line written above every materialized definition.
// It names the helper and the exact span it was lifted from, so a reader of
// the expanded text — human or model — can jump back to the real file. It is
// a C block comment on a single line, which keeps the appended line
// accounting exact (AppendedAtLine/AppendedToLine) and costs the re-fold
// nothing: the scanner classifies it as a plain block comment, not a banner.
func banner(cal *callee) string {
	return fmt.Sprintf("/* tuxconv:inlined %s from %s:%d-%d */",
		cal.name, cal.path, cal.start, cal.end)
}

// definitionBlock is the verbatim text appended for one helper: the banner,
// then the definition's own lines, byte for byte.
//
// No rewriting at all, and that is the point. Because a C definition carries
// its own scope, nothing inside it can collide with the caller's locals, so
// the hygiene/renaming machinery a textual inline would need — and where the
// real risk of a silent miscompile lives — never runs. The EXEC SQL regions,
// the branches, the returns, and the tpreturns arrive already attached to
// the right function, which is what lets the ordinary extractor treat this
// helper exactly like one the file defined itself.
func definitionBlock(cal *callee) []string {
	lines := strings.Split(cal.sourceText, "\n")
	out := make([]string, 0, cal.end-cal.start+2)
	out = append(out, banner(cal))
	out = append(out, lines[cal.start-1:cal.end]...)
	return out
}

// globalPick is one file-scope declaration the callee owns and the caller
// does not.
type globalPick struct {
	name string
	line string
}

// pickGlobals returns the callee's file-scope declarations the caller does
// not already have, in the callee's own source order.
//
// A helper that reads a file-scope global of its own would break without
// this: the declaration is not in the caller's file, and the expanded text is
// an IR input whose only job is to make the helper legible. The caller's own
// declaration always wins on a name collision — it comes first in the
// expanded text, and ir's declaration lookup is first-wins — so a collision
// is not a hazard, only a thing worth reporting.
//
// A declaration the scanner reported on a line that does not itself contain
// the name (a wrapped multi-line declaration) is refused rather than
// half-spliced: a fragment of a declaration is worse than a recorded skip.
func pickGlobals(cal *callee, have map[string]bool) (picked []globalPick, refused []string) {
	lines := strings.Split(cal.sourceText, "\n")
	for _, d := range cal.facts.VarDecls {
		if d.Func != "" || d.Name == "" || have[d.Name] {
			continue
		}
		if d.Line < 1 || d.Line > len(lines) {
			refused = append(refused, d.Name)
			continue
		}
		text := lines[d.Line-1]
		if !strings.Contains(text, d.Name) {
			refused = append(refused, d.Name)
			continue
		}
		picked = append(picked, globalPick{name: d.Name, line: text})
		have[d.Name] = true
	}
	return picked, refused
}

// globalBlock renders the picked declarations as appendable lines.
func globalBlock(picks []globalPick) []string {
	if len(picks) == 0 {
		return nil
	}
	out := make([]string, 0, len(picks)+2)
	out = append(out, "/* tuxconv:inlined file-scope declarations */")
	for _, p := range picks {
		out = append(out, p.line)
	}
	return out
}

// fileScopeNames collects the file-scope declaration names a translation
// unit already carries. Function-scope locals are deliberately excluded:
// they cannot collide across function bodies.
func fileScopeNames(facts *tsscan.SourceFacts) map[string]bool {
	out := make(map[string]bool)
	if facts == nil {
		return out
	}
	for _, d := range facts.VarDecls {
		if d.Func == "" && d.Name != "" {
			out[d.Name] = true
		}
	}
	return out
}
