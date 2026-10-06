package gen

import "tux-migration-tool/internal/ir"

// TemplateFor maps a query type (+tx variant flag) to its generation
// template id: the canonical home for the mapping that used to live as
// ir.QueryType.TemplateID (uniform-ir plan §3.2). The IR method stays as a
// deprecated shim delegating here until Phase 5 removes it.
func TemplateFor(qtype ir.QueryType, tx bool) string {
	switch qtype {
	case ir.QuerySelectSingle:
		if tx {
			return "db_method_select_single_tx"
		}
		return ir.TemplateSelectSingle
	case ir.QuerySelectMulti:
		return ir.TemplateSelectMulti
	case ir.QueryInsert:
		if !tx {
			return "db_method_dml_plain"
		}
		return ir.TemplateInsertTx
	case ir.QueryUpdate:
		if !tx {
			return "db_method_dml_plain"
		}
		return ir.TemplateUpdateTx
	case ir.QueryDelete:
		if !tx {
			return "db_method_dml_plain"
		}
		return ir.TemplateDeleteTx
	case ir.QueryMerge:
		return ir.TemplateMerge
	}
	return ""
}
