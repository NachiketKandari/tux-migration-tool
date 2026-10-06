package convert

import (
	"fmt"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/plan"
)

// The controller-body prompts (whole view, fragment, composer stitch) carry
// the same deterministic fact block; only the stance words differ — the
// model is told to honor "the view", "the fragment", or "the merged body".
// factsWording names those stances so writePromptFacts stays byte-identical
// across builders and a wording tune lands in one place.
type factsWording struct {
	scenarioDecl string // scenario-slice aside; empty = no scenario section
	requiredCall string // REQUIRED CALLS clause after "every one must appear "
	helperScope  string // "Legacy helpers <scope> — never substitute..."
}

var (
	// fragmentWording scopes the facts to one statement fragment.
	fragmentWording = factsWording{
		scenarioDecl: "Legacy declarations",
		requiredCall: "in your statements, under the same condition the fragment shows: ",
		helperScope:  "in the fragment",
	}
	// viewWording scopes the facts to the whole branch view.
	viewWording = factsWording{
		scenarioDecl: "Legacy declarations in the leading region",
		requiredCall: "in the body, under the same condition the view shows: ",
		helperScope:  "in the view",
	}
	// composerWording scopes the facts to the merged body the stitch call
	// receives; the composer carries no scenario-slice section.
	composerWording = factsWording{
		requiredCall: "in the merged body, under the same condition its fragment shows: ",
	}
)

// writePromptFacts writes the sections every controller-body prompt shares,
// in one canonical order: scenario slice, DB contract, REQUIRED CALLS,
// shared blocks, transaction facts, legacy constants, error codes, helper
// mappings, stubbed helpers, signature + structs. Callers own the framing
// (endpoint header, fragment banner, locals, source text, flow draft).
// includeShared gates the scenario shared-block extents (the chunk path
// carries them only on the first fragment).
func writePromptFacts(sb *strings.Builder, w factsWording, scen *scenPrompt, includeShared bool, source, receiver, dbContract, contract string, helpers, constants, errCodes []string, stubs []plan.Stub) {
	if scen != nil && w.scenarioDecl != "" {
		fmt.Fprintf(sb, "Scenario slice: %s — contradicted branches already folded away, implement exactly what remains. %s (int counters, EXEC SQL INCLUDE headers) are context only.\n", scen.Key, w.scenarioDecl)
		if scen.Filter != "" {
			fmt.Fprintf(sb, "Scenario filter: %s — the slice is the re-fold under %s", scen.Filter, strings.Join(scen.Matched, ", "))
			if len(scen.Pruned) > 0 {
				sb.WriteString("; pruned at plan time: " + strings.Join(scen.Pruned, ", "))
			}
			sb.WriteString("; the surviving arm guards stay live for runtime dispatch.\n")
		}
		for _, r := range scen.Residue {
			sb.WriteString("Unfolded predicate — kept verbatim, the fold could not decide it: " + r + "\n")
		}
		sb.WriteString("\n")
	}
	// The FoldMixed guards are the runtime dispatch contract — the
	// deterministic guard gate rejects a body that drops one, so every seam
	// (view, fragment, composer, repair) carries them explicitly.
	if scen != nil && len(scen.Guards) > 0 {
		sb.WriteString("Runtime dispatch guards — keep every one live as an if condition in the body: " +
			strings.Join(scen.Guards, "; ") + "\n\n")
	}
	writeDBContract(sb, dbContract)
	if calls := requiredCalls(source, receiver); len(calls) > 0 {
		sb.WriteString("REQUIRED CALLS — every one must appear " + w.requiredCall +
			strings.Join(calls, ", ") + "\n\n")
	}
	if scen != nil && includeShared {
		for _, s := range scen.Shared {
			sb.WriteString("Shared blocks — " + s + "\n")
		}
		if len(scen.Shared) > 0 {
			sb.WriteString("\n")
		}
	}
	if scen != nil && len(scen.TxNotes) > 0 {
		sb.WriteString("Transaction facts (preserve the transaction shape):\n")
		for _, n := range scen.TxNotes {
			sb.WriteString("  - " + n + "\n")
		}
		sb.WriteString("\n")
	}
	if len(constants) > 0 {
		sb.WriteString("Legacy constants (use literal values directly):\n")
		for _, c := range constants {
			sb.WriteString("  - " + c + "\n")
		}
		sb.WriteString("\n")
	}
	if len(errCodes) > 0 {
		sb.WriteString("Legacy error codes (retain in returned error text): " +
			strings.Join(errCodes, ", ") + "\n\n")
	}
	if len(helpers) > 0 {
		sb.WriteString("Legacy helpers " + w.helperScope + " — never substitute one fn's symbol for another:\n")
		for _, h := range helpers {
			sb.WriteString("  - " + h + "\n")
		}
		sb.WriteString("\n")
	}
	if len(stubs) > 0 {
		sb.WriteString("Stubbed helpers (generated package-level stubs, variadic args, int return): call the RIGHT stub per legacy fn, passing only declared identifiers (declare zero-value locals for C-only names; out-pointers become &local):\n")
		writeStubEntries(sb, stubs)
		sb.WriteString("\n")
	}
	sb.WriteString("Signature + structs (exact names; types noted once):\n" + contract + "\n\n")
}

// writeDBContract emits the one DB-contract block every body prompt carries.
func writeDBContract(sb *strings.Builder, dbContract string) {
	sb.WriteString("DB layer contract (call these; never write SQL):\n" + dbContract + "\n\n")
}

// writeStubEntries renders the stubbed-helper entries: the section prose
// differs per builder, the entry shape never does.
func writeStubEntries(sb *strings.Builder, stubs []plan.Stub) {
	for _, st := range stubs {
		fmt.Fprintf(sb, "  - %s(...) → %s(args ...any) int\n", st.Fn, common.CamelLowerGo(st.Fn))
	}
}
