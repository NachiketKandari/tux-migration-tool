package pygentest

import "tux-migration-tool/internal/templates"

// SQLRules renders the Tier-1 SQL well-formedness rules (docs/pygentest-plan.md
// §1a) — the module a generated suite imports to judge the SQL text of the
// emitted module. It takes no plan data: the rule set is closed, and only the
// statements it judges come from the artifact.
//
// It is a third rendered module beside the router's two halves because the
// rules are the oracle's own logic: they must be testable on their own (each
// rule has its own teeth probe) rather than buried in a
// generated file nobody reads before running.
func SQLRules(prov templates.Provider) (string, error) {
	if prov == nil {
		prov = templates.NewEmbeddedProvider()
	}
	return prov.Render(templates.PySQLRulesFile, nil)
}
