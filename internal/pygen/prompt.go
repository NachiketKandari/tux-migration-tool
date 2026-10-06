package pygen

import (
	"context"
	"fmt"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/pychk"
	"tux-migration-tool/internal/pyplan"
)

// fillServiceBody is the BP-6 LLM seam: the service orchestration body for
// repository-shape batches. The prompt carries the CodeView (SQL regions
// replaced by deterministic repo-call placeholders), the repo signatures,
// the logging-parity list, and the dropped-construct inventory — never raw
// SQL, so the fidelity gate stays meaningful. Bounded retries feed
// structural errors back; the caller degrades to the placeholder on failure.
// The retry/budget/audit skeleton is the shared llm.RunSeam.
func fillServiceBody(ctx context.Context, opts Options) (body string, calls int, notes []string, err error) {
	p := opts.Plan
	check := opts.Check
	if check == nil {
		check = pychk.Check
	}
	return llm.RunSeam(ctx, llm.SeamInput{
		Unit: p.Module, Kind: "batchpy", Name: p.ClassName,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		AbortOnChatError: true,
		Prompt: func(_ string, notes []string) (string, []llm.Message) {
			prompt := userPrompt(p, opts, notes)
			return prompt, []llm.Message{
				{Role: "system", Content: systemPrompt(p)},
				{Role: "user", Content: prompt},
			}
		},
		Extract: func(content string) string {
			return reindent(llm.ExtractFenced(content, "python"))
		},
		Gate: func(serviceBody string) []string {
			content := assembleModule(opts.provider(), p, opts.SourcePath, serviceBody)
			issues := check(content)
			issues = append(issues, txnIssues(content)...)
			issues = append(issues, seamIssues(serviceBody)...)
			issues = append(issues, contractIssues(opts.Plan, serviceBody)...)
			if len(issues) == 0 {
				return nil
			}
			return []string{"structural check failed: " + issueSummary(issues)}
		},
	})
}

func systemPrompt(p *pyplan.Plan) string {
	return `You convert Tuxedo Pro*C batch programs to Python service modules.
Deliverable shape (violations are rejected by automated gates):
- Output ONLY the service class BODY: method definitions at one 4-space indent level. No imports, no class line, no module-level statements, no if __name__ block, no logging configuration, no print().
- The service class is ` + p.ClassName + `. It already has self.db_router and self.repo set in __init__.
- Transactions are owned by the ` + p.Wrapper.RouterClass + ` context manager: NEVER call conn.commit() or conn.rollback() anywhere. Never touch self.db_router in the service body — every connection lives inside the repository methods.
- Use ONLY the repository methods listed for each block. Do not invent methods, SQL, or constants; do not restate or reformat any SQL (no string containing SELECT/INSERT/UPDATE/DELETE may appear in the body).
- Cursor rows are positional TUPLES — unpack by index/slice, never by dict key.
- Implement EVERY block of the orchestration contract, in order, with the SAME control flow the code view shows: same loops, same branches, same state initializations and resets. Do not drop, merge, reorder, or summarize blocks or branches. A branch you consider dead still gets implemented.
- Every repository method listed under "must call" MUST be called in the body at least once, under the same condition the code view shows for it.
- Mirror the userlog sites as logger.info/logger.debug calls (debug-gated ones -> logger.debug); wrap the orchestration in try/except Exception -> logger.error + raise. Never swallow exceptions.
- The entrypoint must be def ` + p.Entrypoint + `(self, workers: int = 1) -> Dict[str, Any]: returning {"status": "SUCCESS", ...} with per-block counts.
- Preserve legacy quirks verbatim (even suspicious arithmetic) with a NOTE comment flagging them for review.
- If you need a small state holder, use plain local variables or a nested plain class (dataclasses/typing imports are NOT available). No time.sleep, no retry logic of your own.`
}

func userPrompt(p *pyplan.Plan, opts Options, notes []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Module: %s · batch: %s · logger: %s · entrypoint: %s\n\n", p.Module, p.ServiceName, p.LoggerName, p.Entrypoint)
	sb.WriteString("Repository methods available (self.repo.*):\n")
	for _, m := range p.Repo {
		sig := m.Name + "(" + strings.Join(m.Binds, ", ") + ")"
		if len(m.RowShape) > 0 {
			// Row provenance for the positional-tuple rule (engine-wiring
			// audit Tier-2: the plan computed RowShape and nothing read it).
			sig += " -> row (" + strings.Join(m.RowShape, ", ") + ")"
		}
		fmt.Fprintf(&sb, "  - %s  [%s]\n", sig, m.Kind)
	}
	sb.WriteString("\nSQL constants available (never restate SQL):\n")
	for _, c := range p.Consts {
		fmt.Fprintf(&sb, "  - %s\n", c.Name)
	}
	if len(p.Flow.Logs) > 0 {
		sb.WriteString("\nSource log sites to mirror (logger level hinted):\n")
		for _, l := range p.Flow.Logs {
			lvl := "info"
			if l.DebugGated {
				lvl = "debug"
			}
			fmt.Fprintf(&sb, "  - %s(%s) -> logger.%s\n", l.Call, l.Text, lvl)
		}
	}
	if len(p.Flow.Dropped) > 0 {
		sb.WriteString("\nDropped constructs (do not reintroduce):\n")
		for _, d := range p.Flow.Dropped {
			fmt.Fprintf(&sb, "  - [%s] %s (line %d)\n", d.Kind, d.Call, d.Line)
		}
	}
	if len(notes) > 0 {
		sb.WriteString("\nEarlier attempts failed these gate checks — fix every listed problem:\n")
		for _, n := range notes {
			sb.WriteString("  - " + n + "\n")
		}
	}
	sb.WriteString("\nORCHESTRATION CONTRACT — implement exactly these blocks, in order, and nothing else:\n")
	for _, b := range p.Orchestration(opts.Source) {
		fmt.Fprintf(&sb, "\n--- Block %d: %s ---\n", b.Index, b.Name)
		if len(b.Methods) > 0 {
			sb.WriteString("must call (at least once, under the same condition as the view): " + strings.Join(b.Methods, ", ") + "\n")
		}
		sb.WriteString("\n```c\n" + b.View + "\n```\n")
	}
	return sb.String()
}

func issueSummary(issues []pychk.Issue) string {
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, fmt.Sprintf("line %d: %s", i.Line, i.Msg))
	}
	return strings.Join(parts, "; ")
}

// reindent repairs the common model formatting slip: a service body whose
// first def starts flush while the rest is method-indented. When the first
// non-blank line is a flush def/async def, the whole block shifts one level
// right so the methods land inside the class.
func reindent(body string) string {
	lines := strings.Split(body, "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") {
			if common.Leading(l) == "" {
				for i := range lines {
					if lines[i] != "" {
						lines[i] = "    " + lines[i]
					}
				}
			}
			break
		}
		break // body starts with something else (e.g. a comment) — leave as-is
	}
	return strings.Join(lines, "\n")
}

// txnIssues is the BP-5 gate the structural check cannot see: transactions
// are wrapper-owned — no commit/rollback anywhere in the module.
func txnIssues(content string) []pychk.Issue {
	var issues []pychk.Issue
	for i, raw := range strings.Split(content, "\n") {
		code := raw
		if idx := strings.Index(code, "#"); idx >= 0 {
			code = code[:idx]
		}
		if strings.Contains(code, ".commit()") || strings.Contains(code, ".rollback()") {
			issues = append(issues, pychk.Issue{Line: i + 1, Msg: "BP-5: commit/rollback is wrapper-owned — never call it in generated code"})
		}
	}
	return issues
}

// seamIssues is the BP-6 seam gate over the service body only: data access
// goes through self.repo methods — self.db_router is the repository's
// dependency, not a context manager, and calling it from the service bypasses
// the scaffold (the repository class's own self.db_router.get_connection is
// legitimate and lives outside this body).
func seamIssues(serviceBody string) []pychk.Issue {
	var issues []pychk.Issue
	for i, raw := range strings.Split(serviceBody, "\n") {
		code := raw
		if idx := strings.Index(code, "#"); idx >= 0 {
			code = code[:idx]
		}
		if strings.Contains(code, "self.db_router(") || strings.Contains(code, "self.db_router.") {
			issues = append(issues, pychk.Issue{Line: i + 1, Msg: "BP-6: use self.repo methods for data access; the router is not a context manager"})
		}
	}
	return issues
}

// contractIssues is the orchestration-contract gate (BP-6 hardening): the
// body must define the entrypoint and call every repository method the
// blocks list — a dropped sub-flow is a rejection, not a silent gap.
func contractIssues(p *pyplan.Plan, body string) []pychk.Issue {
	var issues []pychk.Issue
	if !strings.Contains(body, "def "+p.Entrypoint+"(") {
		issues = append(issues, pychk.Issue{
			Msg: "orchestration contract: the body must define " + p.Entrypoint + "(self, workers: int = 1) -> Dict[str, Any]"})
	}
	for _, m := range p.Repo {
		if !strings.Contains(body, "."+m.Name+"(") {
			issues = append(issues, pychk.Issue{
				Msg: "orchestration contract: repository method " + m.Name + " is missing — every must-call in the blocks is required, under its block's condition"})
		}
	}
	return issues
}
