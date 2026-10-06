package csgen

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/cschk"
	"tux-migration-tool/internal/csplan"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/templates"
)

// fillArmBody is the convertcs LLM seam (A2/A3): the residual logic of one
// dispatch arm — the block between the deterministic prologue (repo calls,
// row mapping, structured logging) and the return statement. The prompt
// carries the query-replaced arm view (every EXEC SQL span presented as its
// deterministic repo call), the endpoint contract, and the rendered
// prologue; the model emits only the TODO block, never signatures or SQL.
// Bounded retries feed gate errors back; exhaustion degrades to the TODO
// placeholder (the caller keeps it — never a silent invention). The
// retry/budget/audit skeleton is the shared llm.RunSeam.
func fillArmBody(ctx context.Context, opts Options, p *csplan.Plan, ep EpData, svc fileData, epIdx int) (string, int, []string, error) {
	return llm.RunSeam(ctx, llm.SeamInput{
		Unit: p.Component, Kind: "convertcs", Name: ep.Name,
		Template: string(templates.CsServiceFile), LLM: true,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		AbortOnChatError: true,
		Prompt: func(_ string, notes []string) (string, []llm.Message) {
			prompt := userPrompt(p, ep, svc, opts.Source, notes)
			return prompt, []llm.Message{
				{Role: "system", Content: systemPrompt(p, svc)},
				{Role: "user", Content: prompt},
			}
		},
		Extract: func(content string) string {
			return normalizeBody(llm.ExtractFenced(content, "csharp"), bodyIndent)
		},
		Gate: func(body string) []string {
			return armGates(opts.provider(), p, ep, svc, epIdx, body)
		},
	})
}

// systemPrompt pins the deliverable shape: only the residual block, only
// the listed repo calls, never SQL. Violations are rejected by the gates.
func systemPrompt(p *csplan.Plan, svc fileData) string {
	return `You convert one Tuxedo Pro*C dispatch arm to the residual logic of an ASP.NET Core service method.
Deliverable shape (violations are rejected by automated gates):
- Output ONLY the residual C# statements that fill the tuxconv:TODO block: the statements between the deterministic prologue (repository calls, row mapping, structured logging) and the method's return statement, at one consistent indent level. No method signature, no method braces, no using/namespace declarations, no class boilerplate, no return statement (it is already rendered).
- The service class ` + p.Service + ` already has the repository field ` + repoFieldExpr(svc.RepoField) + ` and the logger _logger.
- Use ONLY the repository methods listed for the endpoint. Do not invent methods, SQL, or constants; do not restate or reformat any SQL (no EXEC SQL anywhere, no string containing SELECT/INSERT/UPDATE/DELETE/MERGE).
- The prologue already fetched the data, mapped the response DTO, and logged. Implement ONLY the arm's residual logic visible in the arm view: derivations, conditionals over fetched values, loops, helper assignments, extra logging — in the SAME order the view shows, under the SAME conditions. A branch you consider dead still gets implemented.
- request.<Prop> reads the request; response.<Prop> reads the mapped DTO. DataReaderHelper is NOT available inside the service method (the prologue already used it).
- Every identifier you reference must be declared: a parameter, the response DTO property, request.<Prop>, or a local declared in your block. An arm-view identifier declared nowhere (a preamble local, a global) is NOT available — declare a local for it with its best-known initial value plus a // NOTE: comment flagging it, never reference it bare.
- Mirror userlog sites as _logger.LogInformation / _logger.LogDebug calls (debug-gated ones -> LogDebug). Never swallow exceptions; the try/catch is already rendered.
- Preserve legacy quirks verbatim (even suspicious logic) with a // NOTE: comment flagging them for review.`
}

func repoFieldExpr(field string) string { return "(_" + field + ")" }

func userPrompt(p *csplan.Plan, ep EpData, svc fileData, source string, notes []string) string {
	var sb strings.Builder
	scenario := ""
	if ep.Scenario != "" {
		scenario = " · scenario " + ep.Scenario
	}
	fmt.Fprintf(&sb, "Component: %s · service: %s · endpoint: %s%s · arm source lines %s\n",
		p.Component, p.Service, ep.Name, scenario, ep.Span)
	if ep.Filter != "" {
		fmt.Fprintf(&sb, "Scenario filter: %s — the arm is the re-fold under %s", ep.Filter, strings.Join(ep.FilterMatched, ", "))
		if len(ep.FilterPruned) > 0 {
			sb.WriteString("; pruned at plan time: " + strings.Join(ep.FilterPruned, ", "))
		}
		sb.WriteString("; the surviving arm guards stay live for runtime dispatch.\n")
	}
	if len(ep.Guards) > 0 {
		var lines []string
		for _, g := range ep.Guards {
			lines = append(lines, fmt.Sprintf("L%d: %s", g.Line, g.Cond))
		}
		sb.WriteString("Runtime dispatch guards — keep every one live as an if condition in the residual block: " + strings.Join(lines, "; ") + "\n")
	}
	fmt.Fprintf(&sb, "Return type: Task<%s> — the method already ends with `return %s;`\n\n", ep.RetType, ep.ReturnExpr)

	sb.WriteString("Repository methods available (deterministic calls, already rendered in the prologue):\n")
	for _, q := range ep.Queries {
		kind, ret := "select", "DataTable"
		if q.DML {
			kind, ret = "dml", "int"
		}
		fmt.Fprintf(&sb, "  - _%s.%s(%sCancellationToken ct) → Task<%s>  [%s]\n",
			svc.RepoField, q.MethodName, q.SigArgs, ret, kind)
	}

	if props := ep.Props; len(props) > 0 {
		sb.WriteString("\nResponse DTO " + svc.Service + "DTO." + ep.DTOName + " properties (the prologue mapped them):\n")
		for _, pr := range props {
			sb.WriteString("  - response." + pr + "\n")
		}
	}
	var reqProps []string
	seen := map[string]bool{}
	for _, q := range ep.Queries {
		for _, prm := range q.Params {
			if prm.RequestProp != "" && !seen[prm.RequestProp] {
				seen[prm.RequestProp] = true
				reqProps = append(reqProps, prm.RequestProp)
			}
		}
	}
	if len(reqProps) > 0 {
		sb.WriteString("\nRequest properties read (request.<Prop>):\n")
		for _, r := range reqProps {
			sb.WriteString("  - request." + r + "\n")
		}
	}

	sb.WriteString("\nDeterministic prologue (already rendered above the TODO block — do not repeat it):\n")
	for _, l := range strings.Split(prologueOf(ep, svc), "\n") {
		if l == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("    " + l + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("Arm view (source lines " + ep.Span + ", every EXEC SQL span replaced by its deterministic repo call — the view is the whole truth of the arm):\n")
	sb.WriteString("```c\n")
	sb.WriteString(armView(source, p, ep, svc))
	sb.WriteString("\n```\n")

	if len(ep.Residue) > 0 {
		// SCEN-D4 slice evidence, now reaching the seam (engine-wiring
		// audit Tier-1 #7): the planner kept these source regions verbatim
		// because they touch the dispatch axis — the LLM must implement
		// them, not summarize them away.
		sb.WriteString("\nSlice residue (regions the planner kept verbatim — implement them faithfully inside the residual logic):\n")
		for _, r := range ep.Residue {
			sb.WriteString("  - " + r + "\n")
		}
	}

	if len(notes) > 0 {
		sb.WriteString("\nEarlier attempts failed these gate checks — fix every listed problem:\n")
		for _, n := range notes {
			sb.WriteString("  - " + n + "\n")
		}
	}
	sb.WriteString("\nEmit the residual block only, inside one ```csharp fence.\n")
	return sb.String()
}

// armView renders the endpoint's source span with every EXEC SQL span
// replaced by its deterministic repo call — the query-replaced scenario
// slice the body prompt carries (A2). Queries the endpoint does not own
// (other arms' SQL inside the span) render as an explicit dropped marker:
// the view never shows raw SQL, and never pretends a dropped arm is live.
func armView(source string, p *csplan.Plan, ep EpData, svc fileData) string {
	lines := strings.Split(source, "\n")
	spanByLine := map[int]*csplan.QueryPlan{}
	for i := range p.Queries {
		q := &p.Queries[i]
		for l := q.Line[0]; l <= q.Line[1]; l++ {
			spanByLine[l] = q
		}
	}
	callByQuery := map[string]string{}
	for _, q := range ep.Queries {
		callByQuery[q.QueryID] = callLine(q, svc.RepoField)
	}
	from, to := epLineSpan(ep)
	if from < 1 || to < from {
		// An empty arm view is never a prompt: the slice kept no body
		// lines (or the span never resolved) — the caller degrades loudly
		// instead of prompting the model with nothing.
		return ""
	}
	var out []string
	for l := from; l <= to && l-1 < len(lines); l++ {
		if q := spanByLine[l]; q != nil {
			if l == q.Line[0] {
				if call, ok := callByQuery[q.ID]; ok {
					out = append(out, call)
				} else {
					out = append(out, "// (SQL dropped: not reachable in this scenario — the repository method for it is not in the contract)")
				}
			}
			continue
		}
		out = append(out, strings.TrimRight(lines[l-1], " \t\r"))
	}
	return strings.Join(out, "\n")
}

func epLineSpan(ep EpData) (int, int) {
	if ep.LineSpan[0] > 0 && ep.LineSpan[1] >= ep.LineSpan[0] {
		return ep.LineSpan[0], ep.LineSpan[1]
	}
	var from, to int
	_, _ = fmt.Sscanf(ep.Span, "%d-%d", &from, &to)
	if from < 1 || to < from {
		return 0, 0
	}
	return from, to
}

// callLine renders the one deterministic C# call that replaces a query's
// EXEC SQL span — the same line the prologue renders (the template's
// CallLHS line, byte for byte, minus the leading indent).
func callLine(q QData, repoField string) string {
	return fmt.Sprintf("%s = await _%s.%s(%sct);", q.CallLHS, repoField, q.MethodName, q.Args)
}

// prologueOf re-renders the deterministic statements that precede the TODO
// block (the template's prologue: repo calls + row mapping + logging) —
// the prompt's faithful view of what the model must not repeat.
func prologueOf(ep EpData, svc fileData) string {
	var out []string
	pad := strings.Repeat(" ", bodyIndent)
	for _, q := range ep.Queries {
		out = append(out, pad+callLine(q, svc.RepoField))
		if q.MapSingle {
			out = append(out,
				pad+"var row = "+q.VarName+".Rows.Count > 0 ? "+q.VarName+".Rows[0] : null;",
				pad+"var response = new "+ep.DTOName,
				pad+"{")
			for _, pr := range q.Props {
				out = append(out, pad+"    "+pr+" = row != null ? DataReaderHelper.GetStr(row, \""+pr+"\") : string.Empty,")
			}
			out = append(out,
				pad+"};",
				pad+"_logger.LogInformation("+q.LogLine+");")
		}
		if q.MapMulti {
			out = append(out,
				pad+"var response = new List<"+ep.DTOName+">();",
				pad+"foreach (DataRow row in "+q.VarName+".Rows)",
				pad+"{",
				pad+"    response.Add(new "+ep.DTOName,
				pad+"    {")
			for _, pr := range q.Props {
				out = append(out, pad+"        "+pr+" = DataReaderHelper.GetStr(row, \""+pr+"\"),")
			}
			out = append(out,
				pad+"    });",
				pad+"}",
				pad+"_logger.LogInformation("+q.LogLine+");")
		}
	}
	return strings.Join(out, "\n")
}

// armGates are the body gates (A3), evaluated over the assembled service
// file so the model's block is judged in the exact bytes the tree would
// write: fixed signature and return statement, every required repo call
// present, no raw SQL / EXEC SQL, brace balance.
func armGates(prov templates.Provider, p *csplan.Plan, ep EpData, svc fileData, epIdx int, body string) []string {
	if strings.TrimSpace(body) == "" {
		return []string{"the residual block is empty"}
	}
	trial := svc
	trial.Endpoints = append([]EpData(nil), svc.Endpoints...)
	trial.Endpoints[epIdx] = filledEp(ep, body)
	content, err := renderServiceFile(prov, trial)
	if err != nil {
		return []string{"service render failed: " + err.Error()}
	}
	var errs []string
	for _, is := range cschk.Check("Service/"+p.Service+".cs", content, p.Service) {
		errs = append(errs, is.Kind+": "+is.Detail)
	}
	sig := fmt.Sprintf("public async Task<%s> %s(", ep.RetType, ep.Name)
	if !strings.Contains(content, sig) {
		errs = append(errs, "the fixed method signature is missing — never emit signatures, the scaffold owns them")
	}
	if !strings.Contains(content, "return "+ep.ReturnExpr+";") {
		errs = append(errs, "the return statement (`return "+ep.ReturnExpr+";`) is missing — never emit it, the scaffold owns it")
	}
	for _, q := range ep.Queries {
		if !strings.Contains(content, "."+q.MethodName+"(") {
			errs = append(errs, "required repository call "+q.MethodName+" is missing from the assembled method")
		}
	}
	if containsExecSQL(body) {
		errs = append(errs, "the residual block carries EXEC SQL — data access is the repository's, via the listed methods only")
	}
	if strings.Contains(body, "tuxconv:TODO") {
		errs = append(errs, "the residual block echoes the tuxconv:TODO marker — emit implemented logic, not the placeholder")
	}
	errs = append(errs, guardErrs(ep.Guards, body)...)
	sort.Strings(errs)
	return errs
}

// filledEp returns ep with the candidate body as its TodoSlot.
func filledEp(ep EpData, body string) EpData {
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	ep.TodoSlot = body
	return ep
}

// containsExecSQL reports an EXEC SQL span (any spelling/case) in the block.
func containsExecSQL(body string) bool {
	return strings.Contains(strings.ToUpper(body), "EXEC SQL")
}

// normalizeBody repairs the model's arbitrary indentation: the block is
// dedented by its smallest common indent and re-indented to the residual
// block's slot (16 spaces), blank lines dropped at both ends. Relative
// nesting survives.
func normalizeBody(body string, indent int) string {
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return ""
	}
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := leadingSpaces(l)
		if min < 0 || n < min {
			min = n
		}
	}
	pad := strings.Repeat(" ", indent)
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(l) >= min {
			l = l[min:]
		}
		out[i] = pad + l
	}
	return strings.Join(out, "\n") + "\n"
}

func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}
