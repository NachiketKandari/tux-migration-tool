// Package pygentest is the execution harness for generated batchpy modules
// (docs/pygentest-plan.md §4 Phase 2b). It provides the stdlib-only machinery
// every generated suite runs on:
//
//   - a fake oracledb and a recording core.db_router, injected into sys.modules
//     before the module under test is imported;
//   - import-by-path, so a suite loads the artifact the emitter wrote rather
//     than a copy of it;
//   - the recorder every assertion compares: (txn_id, normalised_sql, binds),
//     plus the transaction boundaries get_connection() blocks produce.
//
// The suites themselves (the four assertion tiers) are Phase 2c/2d; this
// package owns the mechanism, and the compile-check that keeps the fake in
// step with the production router template.
package pygentest

import (
	"tux-migration-tool/internal/templates"
)

// The harness's written shape. The pygentest command writes it beside the
// module under test under File, and emitted suites import it by Module.
const (
	File   = "_pygentest_harness.py"
	Module = "_pygentest_harness"
	// SQLRulesFile is the Tier-1 rule module a generated suite imports
	// (docs/pygentest-plan.md §1a). Same convention as File: written beside
	// the module under test, imported by module name.
	SQLRulesFile = "_pygentest_sqlrules.py"
	// SQLRulesModule is SQLRulesFile's import name.
	SQLRulesModule = "_pygentest_sqlrules"
)

// Harness renders the harness through the provider (nil = embedded).
//
// It renders from the same PyDBRouterFileData contract the production
// core/db_router.py renders from — one contract, two halves — so the fake's
// class and enum names cannot quietly diverge from the code it stands in for.
// The method/cursor surface is pinned separately by the compile-check in
// exec_test.go, which loads both halves for real under python3.
func Harness(prov templates.Provider, data templates.PyDBRouterFileData) (string, error) {
	if prov == nil {
		prov = templates.NewEmbeddedProvider()
	}
	return prov.Render(templates.PyDBHarnessFile, data)
}
