// Package pygen is the deterministic Python batch generator (PRD
// 2026-09-08 BP-3/BP-6): plan + templates → one Python module per batch
// program. The simple cursor-batch shape renders fully deterministically;
// the repository shape renders the SQL constants and repository methods
// deterministically and fills the service body through the LLM seam (the
// single LLM gap, -no-llm yields tuxconv:TODO placeholders). Every result
// passes the pychk gates and carries the logic-retention report.
package pygen

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/pycensus"
	"tux-migration-tool/internal/pychk"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/sqlchk"
	"tux-migration-tool/internal/templates"
	scanner "tux-migration-tool/internal/tsscan"
)

// Options carries one generation run's inputs.
type Options struct {
	Plan       *pyplan.Plan
	Source     string // full .pc source (the CodeView input)
	SourcePath string
	NoLLM      bool
	Client     llm.Client
	Budget     budget.Budget
	MaxRetries int
	// Audit archives each LLM attempt (prompt + raw response + gate errors)
	// when set; nil skips the archive — never the generation itself.
	Audit *audit.Recorder
	// Check is the structural gate (injectable for tests); nil = pychk.Check.
	Check func(src string) []pychk.Issue
	// Templates is the template provider (user overlay over the embedded
	// set); nil = the embedded defaults.
	Templates templates.Provider
	// Facts is the scanner's structural record for the source. The §6.1
	// accounting census needs it (calls, branches, loops, declarations);
	// nil skips the census, which is how a caller that has no facts (or a
	// test that only cares about rendered bytes) opts out. The census is
	// an observation either way — it never gates in Phase 0.
	Facts *scanner.SourceFacts
}

// provider resolves the run's template set (nil-safe embedded default).
func (o Options) provider() templates.Provider {
	if o.Templates == nil {
		return templates.NewEmbeddedProvider()
	}
	return o.Templates
}

// Result is one generated module with its gate outcomes and report.
type Result struct {
	Content   string
	LLMCalls  int
	LLMFilled bool
	Structure []pychk.Issue
	PyOK      bool
	PyMode    string // ast | structural | unavailable
	PyDetail  string
	Fidelity  []sqlchk.Result
	Retention Retention
	// Census is the §6.1 accounting report. Phase 0 of
	// docs/pybatchpy-logic-plan.md lands it report-only: it changes no
	// emitted output and gates nothing yet, but every construct the drop
	// table silently ignores now has a number.
	Census pycensus.Census
	Notes  []string
	// SupportFiles are the extra files the generated module imports and the
	// caller must write next to it (relative paths under the output dir).
	// The db-router wrapper is the first one — F4: the modules imported
	// `core.db_router` and nothing ever produced it, so every generated
	// module failed at import before any check could run.
	SupportFiles []SupportFile
}

// SupportFile is one auxiliary file to write under the output directory.
// Path is the import path the generated module uses with dots as slashes
// (`core.db_router` → `core/db_router.py`); it is derived from the plan, so
// a caller never has to know the layout.
type SupportFile struct {
	Path    string
	Content string
}

// Generate renders the module for one plan. The LLM seam runs only for the
// repository shape when a client is supplied and NoLLM is false; failures
// degrade to the placeholder body with a note (never a silent SQL invention).
func Generate(ctx context.Context, opts Options) (Result, error) {
	res := Result{}
	p := opts.Plan
	prov := opts.provider()
	check := opts.Check
	if check == nil {
		check = pychk.Check
	}

	var secs []string
	var serviceBody string
	secs = append(secs, renderHeader(prov, p, opts.SourcePath))
	secs = append(secs, banner("SQL Query Constants (source fidelity-gated via sqlchk)"))
	for _, c := range p.Consts {
		secs = append(secs, render(prov, templates.PyBatchConst, constData{Name: c.Name, SQL: c.SQL}))
	}

	if p.Shape == "simple" {
		secs = append(secs, banner("Data Access Layer (pure cursor functions — mockable with a cursor double)"))
		for _, fn := range p.DAL {
			if fn.Kind == "fetch" {
				secs = append(secs, render(prov, templates.PyBatchDALFetch, dalFetchData{Name: fn.Name, Const: fn.Const, Source: strings.TrimPrefix(fn.Name, "fetch_")}))
				continue
			}
			secs = append(secs, render(prov, templates.PyBatchDALDML, dalDMLData{
				Name: fn.Name, Const: fn.Const, Mode: p.DMLLoop,
				HasBinds:      len(fn.BindIdx) > 0,
				Projection:    projection("r", fn.BindIdx),
				RowProjection: projection("row", fn.BindIdx),
			}))
		}
		calls := make([]entryCall, 0, len(p.Phases))
		for _, ph := range p.Phases {
			calls = append(calls, entryCall{Key: resultKey(ph.Cursor), Method: ph.Method})
		}
		entry := render(prov, templates.PyBatchEntrypoint, entrypointData{
			Entrypoint: p.Entrypoint, ServiceName: p.ServiceName, Calls: calls,
		})
		var svc []string
		svc = append(svc, render(prov, templates.PyBatchServiceHead, serviceHeadData{
			ClassName: p.ClassName, ServiceName: p.ServiceName, SourcePath: opts.SourcePath, RouterClass: p.Wrapper.RouterClass,
		}))
		for _, ph := range p.Phases {
			svc = append(svc, render(prov, templates.PyBatchPhase, phaseData{
				Index: ph.Index, Method: ph.Method, Cursor: ph.Cursor, Table: ph.Table,
				FetchFn: ph.FetchFn, DMLFn: ph.DMLFn, Mode: p.DMLLoop,
				ReadMode: p.Wrapper.ReadMode, WriteMode: p.Wrapper.WriteMode,
			}))
		}
		svc = append(svc, entry)
		serviceBody = strings.Join(svc, "\n\n")
		secs = append(secs, banner("Service Layer"))
		secs = append(secs, svc...)
	} else {
		body := placeholderBody(p.Entrypoint)
		if !opts.NoLLM && opts.Client != nil {
			filled, calls, notes, err := fillServiceBody(ctx, opts)
			res.LLMCalls = calls
			if err == nil {
				body = filled
				res.LLMFilled = true
			} else {
				// Rejected-attempt notes are failure context, not findings —
				// on success the audit trail already archives every attempt.
				res.Notes = append(res.Notes, notes...)
				res.Notes = append(res.Notes, "llm fill failed: "+err.Error()+" — placeholder body emitted")
			}
		} else if !opts.NoLLM {
			res.Notes = append(res.Notes, "no llm client available — placeholder body emitted")
		}
		serviceBody = body
		secs = append(secs, repoSection(prov, p, opts.SourcePath, body))
	}
	if reg := consolidationRegister(p); reg != "" {
		secs = append(secs, reg)
	}

	res.Content = strings.Join(secs, "\n\n") + "\n"
	res.Structure = check(res.Content)
	res.Structure = append(res.Structure, txnIssues(res.Content)...)
	if p.Shape == "repo" {
		// The seam rule is repository-shape-only: simple-shape services hold
		// the cursor-function DAL calls with direct router connections (the
		// local-only reference design).
		res.Structure = append(res.Structure, seamIssues(serviceBody)...)
	}
	res.PyOK, res.PyMode, res.PyDetail = interpreterGate(res.Content)
	res.Fidelity = fidelityOf(p, res.Content)
	res.Retention = retentionOf(p, res.Content, serviceBody, res.Fidelity, res.LLMCalls, res.Structure, res.PyOK, res.PyMode, res.LLMFilled)
	res.Census = censusOf(opts)
	res.SupportFiles = renderSupportFiles(prov, p)
	return res, nil
}

// censusOf computes the §6.1 accounting census when the run supplied scanner
// facts. It reads the plan and the source, never the rendered module, so the
// report stays an independent measurement rather than a restatement of what
// the emitter chose to emit.
func censusOf(opts Options) pycensus.Census {
	if opts.Facts == nil || opts.Plan == nil {
		return pycensus.Census{}
	}
	return pycensus.Compute(pycensus.Input{
		Flow:  opts.Plan.Flow,
		Plan:  opts.Plan,
		Facts: opts.Facts,
		Lines: strings.Split(opts.Source, "\n"),
	})
}

// repoSection renders the repository banner + deterministic repo methods +
// the service body (class passthrough or shell) — the tail section of every
// repo-shape module. Generate and assembleModule (the seam gate's view)
// share it, so the structural gate always sees the exact bytes that would
// be written (A2.4: the one assembly).
func repoSection(prov templates.Provider, p *pyplan.Plan, sourcePath, body string) string {
	var secs []string
	secs = append(secs, banner("Repository (deterministic scaffold — BP-3)"))
	var blocks []string
	for _, m := range p.Repo {
		blocks = append(blocks, renderRepoMethod(prov, p, m))
	}
	if strings.HasPrefix(strings.TrimSpace(body), "class ") {
		secs = append(secs, strings.TrimRight(strings.TrimSpace(body), "\n"))
	} else {
		secs = append(secs, render(prov, templates.PyBatchServiceShell, serviceShellData{
			RepoName: p.RepoName, ClassName: p.ClassName, ServiceName: p.ServiceName,
			SourcePath: sourcePath, RouterClass: p.Wrapper.RouterClass,
			RepoBlocks: strings.Join(blocks, "\n\n"), Body: body,
		}))
	}
	return strings.Join(secs, "\n\n")
}

// consolidationRegister states, in the module itself, every statement Phase 1
// removed from the repository and why. §8's guard for this change is "log
// every elision" and §6.4 asks for a declared deviation with a rationale: a
// reader of the generated module must be able to see that 16 source
// statements became 6 repository methods without guessing which 10 went where.
//
// It carries source lines and reasons only — never SQL text — so the register
// cannot reintroduce SQL outside a constant, which both the seam prompt and
// the fidelity gate forbid.
func consolidationRegister(p *pyplan.Plan) string {
	var lines []string
	for _, m := range p.Repo {
		if len(m.MergedSrcLines) == 0 {
			continue
		}
		lines = append(lines,
			fmt.Sprintf("#   %-30s <- source lines %s — %d statements, identical token sequences",
				m.Name, lineList(append([]int{m.SrcLine}, m.MergedSrcLines...)), len(m.MergedSrcLines)+1),
			"# "+strings.Repeat(" ", 33)+"(differ only in inter-token whitespace)")
	}
	for _, d := range p.DateMath {
		lines = append(lines, fmt.Sprintf("#   %-30s <- source line %d — %s: %s",
			"[date math]", d.SrcLine, d.Op, d.Reason))
	}
	if len(lines) == 0 {
		return ""
	}
	out := append([]string{
		banner("Consolidation register (pybatchpy Phase 1 — statements the repository does not own)"),
	}, lines...)
	return strings.Join(out, "\n")
}

func lineList(lines []int) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = strconv.Itoa(l)
	}
	return strings.Join(parts, ", ")
}

// assembleModule builds the full repo-shape module with one service body —
// the seam gate's view of what Generate would write (the same repoSection
// the write path uses).
func assembleModule(prov templates.Provider, p *pyplan.Plan, sourcePath, body string) string {
	var secs []string
	secs = append(secs, renderHeader(prov, p, sourcePath))
	secs = append(secs, banner("SQL Query Constants (source fidelity-gated via sqlchk)"))
	for _, c := range p.Consts {
		secs = append(secs, render(prov, templates.PyBatchConst, constData{Name: c.Name, SQL: c.SQL}))
	}
	secs = append(secs, repoSection(prov, p, sourcePath, body))
	if reg := consolidationRegister(p); reg != "" {
		secs = append(secs, reg)
	}
	return strings.Join(secs, "\n\n") + "\n"
}

// interpreterGate upgrades the structural gate with python3's ast.parse via
// the shared pychk.CheckSource; the outcome is always reported, never faked.
func interpreterGate(content string) (bool, string, string) {
	return pychk.CheckSource(content)
}

// renderSupportFiles renders the auxiliary files the generated module
// imports. Today that is exactly one — the db-router wrapper (F4) — keyed on
// the plan's wrapper config so the path always matches what the header
// template imports.
//
// The file is produced on every run, not only when the module references it:
// the import is unconditional in the header template, so an importable
// module is a precondition of every check below this layer. A caller that
// already wrote the file overwrites it with identical bytes.
func renderSupportFiles(prov templates.Provider, p *pyplan.Plan) []SupportFile {
	if p == nil || strings.TrimSpace(p.Wrapper.Import) == "" {
		return nil
	}
	return []SupportFile{{
		Path:    importToPath(p.Wrapper.Import),
		Content: render(prov, templates.PyDBRouterFile, templates.NewPyDBRouterData(p.Wrapper.RouterClass, p.Wrapper.ReadMode, p.Wrapper.WriteMode)),
	}}
}

// importToPath converts a Python import path to its file location under the
// output directory: `core.db_router` → `core/db_router.py`. A bare name
// (no dot) lands at the output root.
func importToPath(imp string) string {
	return strings.ReplaceAll(strings.TrimSpace(imp), ".", "/") + ".py"
}

// placeholderBody is the -no-llm service body (BP-6): a loud, resumable gap.
func placeholderBody(entrypoint string) string {
	return `    def ` + entrypoint + `(self, workers: int = 1) -> Dict[str, Any]:
        """Unified runner entry point orchestrating all phases."""
        # tuxconv:TODO service body — pending LLM fill (BP-6; -no-llm run)
        raise NotImplementedError("batchpy: service body pending LLM fill")`
}

func fidelityOf(p *pyplan.Plan, content string) []sqlchk.Result {
	targets := make([]pychk.FidelityTarget, 0, len(p.Consts))
	for _, c := range p.Consts {
		targets = append(targets, pychk.FidelityTarget{Const: c.Name, QueryID: c.ID, Source: c.SQL})
	}
	return pychk.Fidelity(targets, content)
}

// render executes one template id against data through the run's provider
// (nil provider = embedded).
func render(prov templates.Provider, id templates.ID, data any) string {
	if prov == nil {
		prov = templates.NewEmbeddedProvider()
	}
	out, err := prov.Render(id, data)
	if err != nil {
		return "# tuxconv: template error (" + string(id) + "): " + err.Error()
	}
	return strings.TrimRight(out, "\n")
}

func banner(title string) string {
	bar := strings.Repeat("=", 69)
	return "# " + bar + "\n# " + title + "\n# " + bar
}

// projection renders the fetch-row bind projection for executemany/execute:
// a bind tuple per row — "r[0], r[1]", or "r[0]," when a single bind needs
// the 1-tuple comma.
func projection(prefix string, idx []int) string {
	if len(idx) == 0 {
		return ""
	}
	parts := make([]string, len(idx))
	for i, ix := range idx {
		parts[i] = prefix + "[" + strconv.Itoa(ix) + "]"
	}
	out := strings.Join(parts, ", ")
	if len(idx) == 1 {
		out += ","
	}
	return out
}

// resultKey converts a cursor name to its result-dict key (px_reject →
// px_reject_records).
func resultKey(cursor string) string { return cursor + "_records" }
