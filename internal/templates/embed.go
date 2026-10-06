package templates

import (
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Version stamps the embedded template set (PRD: templates are versioned data
// files so eval can pin template versions).
const Version = "v1"

// ID identifies one template in the set.
type ID string

const (
	ModelFile               ID = "model_file"
	DBInterfaceFile         ID = "db_interface_file"
	DBMethodSelectMulti     ID = "db_method_select_multi"
	DBMethodSelectSingle    ID = "db_method_select_single"
	DBMethodSelectSingleTx  ID = "db_method_select_single_tx"
	DBMethodInsertTx        ID = "db_method_insert_tx"
	DBMethodUpdateTx        ID = "db_method_update_tx"
	DBMethodDeleteTx        ID = "db_method_delete_tx"
	DBMethodMerge           ID = "db_method_merge"
	DBMethodDMLPlain        ID = "db_method_dml_plain"
	ControllerInterfaceFile ID = "controller_interface_file"
	ControllerMethod        ID = "controller_method"
	ControllerMethodTx      ID = "controller_method_tx"
	FnStubFile              ID = "fn_stub_file"
	HandlerInterfaceFile    ID = "handler_interface_file"
	HandlerMethod           ID = "handler_method"
	RouterSnippet           ID = "router_snippet"

	// Python batch target (PRD-2026-09-08 BP-3): the batchpy emitter's
	// section templates, distilled from the local-only reference
	// conversions (the two reference shapes).
	PyBatchHeader       ID = "py_batch_header"
	PyBatchConst        ID = "py_batch_const"
	PyBatchDALFetch     ID = "py_batch_dal_fetch"
	PyBatchDALDML       ID = "py_batch_dal_dml"
	PyBatchPhase        ID = "py_batch_phase"
	PyBatchEntrypoint   ID = "py_batch_entrypoint"
	PyBatchServiceHead  ID = "py_batch_service_head"
	PyBatchRepoFetchIt  ID = "py_batch_repo_fetch_iterator"
	PyBatchRepoFetchOne ID = "py_batch_repo_fetch_single"
	PyBatchRepoDML      ID = "py_batch_repo_dml"
	PyBatchRepoRebuild  ID = "py_batch_repo_rebuild"
	PyBatchServiceShell ID = "py_batch_service_shell"
	// PyDBRouterFile emits the `core/db_router.py` wrapper the generated
	// modules import (pygentest plan F4: the import existed and the package
	// never did, so every generated module was un-importable). It is rendered
	// as a *support file* next to the modules, from the same plan — the fake
	// twin pygentest drives must carry the surface this template defines.
	PyDBRouterFile ID = "py_db_router_file"
	// PyDBHarnessFile emits the stdlib-only exec harness (pygentest plan §4
	// Phase 2b): fake router/cursor, import-by-path with fakes injected, and
	// the (txn_id, sql, binds) recorder. It renders from the *same*
	// PyDBRouterFileData contract as PyDBRouterFile — one contract, two
	// halves — and a compile-check pins the surfaces together.
	PyDBHarnessFile ID = "py_db_harness_file"
	// PySQLRulesFile emits the Tier-1 SQL well-formedness rules (pygentest
	// plan §1a) as the suite's third stdlib-only module: the F1 killer. They
	// judge the SQL text of the EMITTED module as SQL — `sqlchk` strips the
	// INTO host list from both sides, so a dead statement and a fixed one
	// score identically there. No plan data: the rules are a closed set, and
	// only the statements they judge come from the module.
	PySQLRulesFile ID = "py_sqlrules_file"
	// PyTestSuiteFile emits the generated assertion suite (tiers 1-4) for one
	// batch module. Driven by pyplan.Plan — the same plan the emitter read —
	// so "the generated code is right" becomes N passing assertions instead
	// of a text diff. The suite's exit code is the gate.
	PyTestSuiteFile ID = "py_test_suite_file"

	// Post-conversion Go test target (PRD-2026-09-09 GT-2): the gentest
	// layer templates, distilled from the examples/item reference
	// conversions.
	TestDBFile           ID = "test_db_file"
	TestDBMethod         ID = "test_db_method"
	TestControllerFile   ID = "test_controller_file"
	TestControllerMethod ID = "test_controller_method"
	TestHandlerFile      ID = "test_handler_file"
	TestHandlerMethod    ID = "test_handler_method"

	// C# target (convertcs): the .NET Core emitter's file templates —
	// the seven-file Controller/DTO/NamedQueries/Repository/Service shape,
	// distilled from the local-only reference conversion.
	CsControllerFile       ID = "cs_controller_file"
	CsDTOFile              ID = "cs_dto_file"
	CsNamedQueriesFile     ID = "cs_namedqueries_file"
	CsRepoInterfaceFile    ID = "cs_repo_interface_file"
	CsRepoFile             ID = "cs_repo_file"
	CsServiceInterfaceFile ID = "cs_service_interface_file"
	CsServiceFile          ID = "cs_service_file"
)

// AllIDs lists every template in the embedded set.
var AllIDs = []ID{
	ModelFile,
	DBInterfaceFile,
	DBMethodSelectMulti,
	DBMethodSelectSingle,
	DBMethodSelectSingleTx,
	DBMethodInsertTx,
	DBMethodUpdateTx,
	DBMethodDeleteTx,
	DBMethodMerge,
	DBMethodDMLPlain,
	ControllerInterfaceFile,
	ControllerMethod,
	ControllerMethodTx,
	FnStubFile,
	HandlerInterfaceFile,
	HandlerMethod,
	RouterSnippet,
	PyBatchHeader,
	PyBatchConst,
	PyBatchDALFetch,
	PyBatchDALDML,
	PyBatchPhase,
	PyBatchEntrypoint,
	PyBatchServiceHead,
	PyBatchRepoFetchIt,
	PyBatchRepoFetchOne,
	PyBatchRepoDML,
	PyBatchRepoRebuild,
	PyDBRouterFile,
	PyDBHarnessFile,
	PySQLRulesFile,
	PyTestSuiteFile,
	PyBatchServiceShell,
	TestDBFile,
	TestDBMethod,
	TestControllerFile,
	TestControllerMethod,
	TestHandlerFile,
	TestHandlerMethod,
	CsControllerFile,
	CsDTOFile,
	CsNamedQueriesFile,
	CsRepoInterfaceFile,
	CsRepoFile,
	CsServiceInterfaceFile,
	CsServiceFile,
}

func (id ID) path() string { return "templates/" + string(id) + ".tmpl" }

// load parses and returns the template for id.
func load(id ID) (*template.Template, error) {
	if !registered(id) {
		return nil, fmt.Errorf("templates: unknown template id %q", id)
	}
	t, err := template.ParseFS(templateFS, id.path())
	if err != nil {
		return nil, fmt.Errorf("templates: parse %s: %w", id, err)
	}
	return t.Templates()[0], nil
}

func registered(id ID) bool {
	for _, known := range AllIDs {
		if known == id {
			return true
		}
	}
	return false
}
