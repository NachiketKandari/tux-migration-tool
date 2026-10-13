package pygen

import (
	"strconv"
	"strings"

	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/templates"
)

// Template data payloads — the field names are the template surface (BP-3).

type headerData struct {
	Module, SourcePath, ServiceName, Entry string
	Shape, DMLLoop                         string
	RouterClass, WrapperImport, LoggerName string
	ChunkSize                              int
}

type constData struct{ Name, SQL string }

type dalFetchData struct{ Name, Const, Source string }

type dalDMLData struct {
	Name, Const, Mode         string
	HasBinds                  bool
	Projection, RowProjection string
}

type phaseData struct {
	Index                 int
	Method, Cursor, Table string
	FetchFn, DMLFn, Mode  string
	ReadMode, WriteMode   string
}

type entryCall struct{ Key, Method string }

type entrypointData struct {
	Entrypoint, ServiceName string
	Calls                   []entryCall
}

type serviceHeadData struct {
	ClassName, ServiceName, SourcePath, RouterClass string
}

type repoData struct {
	Name, Const, Const2 string
	BindKwargs, Doc     string
	ReadMode, WriteMode string
	Binds               []string
	HasBinds            bool
	// NoRowsIsError mirrors the legacy SQLCODE guard's polarity: does an
	// empty result count as a failure? The templates use it to decide whether
	// a single-row fetch that came back empty raises or returns None.
	//
	// It is a bool rather than the policy itself because the templates need a
	// decision, and "unset" must map to the pre-existing behaviour rather than
	// to a default polarity. Strict reads it directly; tolerant and unset both
	// read false.
	NoRowsIsError bool
	// ErrPolicyName is the source's own word for the policy, rendered into the
	// docstring so a reader of the generated Python can see that the polarity
	// came from the legacy guard rather than from a guess. Empty when the
	// source stated no contract.
	ErrPolicyName string
}

type serviceShellData struct {
	RepoName, ClassName, ServiceName string
	SourcePath, RouterClass          string
	RepoBlocks, Body                 string
}

func renderHeader(prov templates.Provider, p *pyplan.Plan, sourcePath string) string {
	return render(prov, templates.PyBatchHeader, headerData{
		Module: p.Module, SourcePath: sourcePath, ServiceName: p.ServiceName, Entry: p.Flow.Entry,
		Shape: p.Shape, DMLLoop: p.DMLLoop,
		RouterClass: p.Wrapper.RouterClass, WrapperImport: p.Wrapper.Import, LoggerName: p.LoggerName,
		ChunkSize: p.ChunkSize,
	})
}

// renderRepoMethod renders one repository method by its kind.
func renderRepoMethod(prov templates.Provider, p *pyplan.Plan, m pyplan.RepoMethod) string {
	d := repoData{
		Name: m.Name, Doc: repoDoc(m),
		ReadMode: p.Wrapper.ReadMode, WriteMode: p.Wrapper.WriteMode,
		Binds: m.Binds, HasBinds: len(m.Binds) > 0, BindKwargs: kwargs(m.Binds),
		// The legacy guard decides whether an empty result is a failure. Only
		// an explicit strict verdict raises; tolerant and unset keep the
		// pre-existing return-None behaviour, because "the source said
		// nothing" is not the same claim as "the source said tolerant".
		NoRowsIsError: m.ErrPolicy == contract.PolicyStrict,
	}
	if m.ErrPolicy.IsSet() {
		d.ErrPolicyName = string(m.ErrPolicy)
	}
	if len(m.Consts) > 0 {
		d.Const = m.Consts[0]
	}
	if len(m.Consts) > 1 {
		d.Const2 = m.Consts[1]
	}
	switch {
	case m.Kind == "rebuild":
		return render(prov, templates.PyBatchRepoRebuild, d)
	case m.QueryKind == "SELECT_MULTI":
		return render(prov, templates.PyBatchRepoFetchIt, d)
	case m.QueryKind == "SELECT_SINGLE":
		return render(prov, templates.PyBatchRepoFetchOne, d)
	default:
		return render(prov, templates.PyBatchRepoDML, d)
	}
}

func repoDoc(m pyplan.RepoMethod) string {
	switch m.Kind {
	case "rebuild":
		return "Rebuilds the staging table (truncate + insert)."
	case "fetch":
		return "Fetches rows (READ mode)."
	default:
		return "Executes the DML (WRITE mode)."
	}
}

// kwargs renders the oracledb named-bind kwargs: {"a": a, "b": b}.
func kwargs(binds []string) string {
	if len(binds) == 0 {
		return ""
	}
	parts := make([]string, len(binds))
	for i, b := range binds {
		parts[i] = strconv.Quote(b) + ": " + b
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
