// The LLM gap-fill seam (PRD-2026-09-09 GT-D6): the one place gentest calls
// the AI. It sits inside the per-function worker — after the deterministic
// template proves unable to shape that function (field-mapping controllers)
// — and is gated by budget ceilings, a parse+shape gate, and bounded
// retries that feed gate failures back. The db layer never reaches it (SQL
// is known); handlers never reach it (the controller mock owns mapping).
package testgen

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"

	"tux-migration-tool/internal/llm"
)

// fillCtrlMethod fills one field-mapping controller's suite method through
// the LLM seam. Returns the block, the number of chat calls made, and the
// last error when every attempt was rejected. The retry/budget/audit
// skeleton is the shared llm.RunSeam.
func fillCtrlMethod(ctx context.Context, u *unit, opts Options) (string, int, error) {
	block, calls, _, err := llm.RunSeam(ctx, llm.SeamInput{
		Unit: u.sc.name, Kind: "gentest", Name: u.fn.Name,
		Audit: opts.Audit, Client: opts.Client, Budget: opts.Budget, MaxRetries: opts.MaxRetries,
		AbortOnChatError: true,
		// 2× the output ceiling: thinking-mode models spend part of the
		// request cap on reasoning that never reaches content (live run
		// 2026-09-10: two of four attempts burned the full cap in reasoning
		// and returned empty content); the extracted block itself is well
		// under the Budget.CheckOutput ceiling.
		MaxTokens: 2 * opts.Budget.MaxOutputTokens,
		Prompt: func(_ string, notes []string) (string, []llm.Message) {
			prompt := ctrlUserPrompt(u, notes)
			return prompt, []llm.Message{
				{Role: "system", Content: ctrlSystemPrompt(u)},
				{Role: "user", Content: prompt},
			}
		},
		Extract: func(content string) string { return llm.ExtractFenced(content, "go") },
		Gate: func(block string) []string {
			if strings.TrimSpace(block) == "" {
				return []string{"model returned no Go code (empty response content — often reasoning-only output); emit ONLY one ```go fenced block containing the single suite method"}
			}
			if err := gateCtrlBlock(block, u); err != nil {
				return []string{err.Error()}
			}
			return nil
		},
	})
	return block, calls, err
}

func ctrlSystemPrompt(u *unit) string {
	storeVar := strings.ToLower(u.sc.name) + "Store"
	ctrlVar := strings.ToLower(u.sc.name) + "Controller"
	return `You author Go unit tests for a converted service's controller layer.
Deliverable shape (violations are rejected by automated gates):
- Output ONLY ONE complete suite method: func (suite *` + u.suite + `) Test` + u.fn.Name + `() { ... }
- No package clause, no imports, no other functions, no comments outside the method.
- Table-driven: declare testCases := []struct{ desc string; <one string field per request field> ; mockInput []any; expectedError string; expectedOutput <response type> }{...} with a "StoreError" case (mockInput []any{nil, errors.New("store error")}) and a "Success" case.
- Iterate with for _, testCase := range testCases { suite.T().Run(testCase.desc, func(t *testing.T) { ... }) }.
- Mock ONLY with suite.` + storeVar + ` (a gomock mock): wrap every EXPECT in ` + "`" + `if testCase.mockInput != nil { ... }` + "`" + `, the ctx arg is gomock.Any(), and the remaining args are the concrete literals the function passes — never request references. Multi-call flows get one case field per call (mockInput, mockInput2, …), each guarded, EXPECTed in call order with matching Return payloads.
- Build the request inside the subtest after the EXPECT block: request := &models.<RequestType>{<Field>: testCase.<Field>} and call suite.` + ctrlVar + `.` + u.fn.Name + `(suite.ctx, request).
- Reproduce the function's field mapping EXACTLY: the Success expectedOutput must be what the function returns given the mocked rows — copy the mapping from the source, do not guess.
- Validations: assert.ErrorContains(t, err, testCase.expectedError) on the error path; assert.NoError(t, err) and assert.Equal(t, <the value the controller call returned>, testCase.expectedOutput) on success. Call the package-level assert.* functions with t — never suite.Assert().
- Use only the suite fields, models types, and imports listed in the prompt (errors, sql, time are available). Never reference packages outside them.
- No commit/rollback, no t.Parallel, no time.Sleep.`
}

func ctrlUserPrompt(u *unit, notes []string) string {
	sc := u.sc
	f := u.ctrl
	storeVar := strings.ToLower(sc.name) + "Store"
	ctrlVar := strings.ToLower(sc.name) + "Controller"
	var sb strings.Builder
	fmt.Fprintf(&sb, "Service: %s · suite: %s · controller field: suite.%s (interface %s) · store mock field: suite.%s (%s)\n\n",
		sc.name, u.suite, ctrlVar, ctrlIfaceName(sc), storeVar, "db.Mock"+dbIfaceName(sc))
	sb.WriteString("Suite fields available: suite.ctx (context.Context), suite.mockController (*gomock.Controller), suite." + storeVar + " (store mock), suite." + ctrlVar + "\n\n")
	sb.WriteString("Function under test (verbatim source):\n\n```go\n" + f.Src + "\n```\n\n")
	sb.WriteString("Dependencies to mock (in call order):\n")
	for i, c := range f.StoreCalls {
		input := "mockInput"
		if i > 0 {
			input = fmt.Sprintf("mockInput%d", i+1)
		}
		fmt.Fprintf(&sb, "  - suite.%s.%s(%s) — case field %s; EXPECT ctx as gomock.Any(), remaining args as the concrete literals the source passes\n", storeVar, c.Method, strings.Join(c.Args, ", "), input)
		if lit := mockReturnLiteral(sc, c.Method); lit != "nil" {
			fmt.Fprintf(&sb, "    Return payload shape for the Success case: %s\n", lit)
		}
	}
	if len(f.StoreCalls) > 0 {
		if df := sc.dbFacts.DB[f.StoreCalls[0].Method]; df != nil && df.RowType != "" {
			fmt.Fprintf(&sb, "\nRow struct shape (db tags → fields):\n")
			for _, fl := range sc.models.Structs[structBase(df.RowType)] {
				if fl.DB != "" {
					fmt.Fprintf(&sb, "  - %s %s (db tag %s)\n", fl.Name, fl.Type, fl.DB)
				}
			}
		}
	}
	fmt.Fprintf(&sb, "\nRequest type: models.%s — case fields (assumed values):\n", structBase(f.RequestType))
	for _, fv := range sc.fixtures.FieldValues(structBase(f.RequestType)) {
		fmt.Fprintf(&sb, "  - %s: %q\n", fv[0], fv[1])
	}
	fmt.Fprintf(&sb, "Response type: %s — assumed Success expectedOutput value: %s\n", f.ResponseType, responseLiteral(sc, sc.fixtures, f.ResponseType))
	sb.WriteString("\nRules: the error case must exercise the store error path; the Success case must assert the exact mapped output (assert.Equal(t, <the value the call returned>, testCase.expectedOutput)); every EXPECT sits behind an `if testCase.<input> != nil` guard with gomock.Any() as the ctx matcher.")
	if len(notes) > 0 {
		sb.WriteString("\n\nEarlier attempts failed these gate checks — fix every listed problem:\n")
		for _, n := range notes {
			sb.WriteString("  - " + n + "\n")
		}
	}
	return sb.String()
}

// gateCtrlBlock is the contract gate for LLM blocks: the block must parse
// as one method on the right receiver with the right name, and must carry
// the reference contract — table-driven cases, every EXPECT inside an
// `if testCase.mockInput… != nil` guard with a gomock.Any ctx matcher, and
// the reference validations.
func gateCtrlBlock(block string, u *unit) error {
	if !strings.Contains(block, "testCases") {
		return fmt.Errorf("block is not table-driven (no testCases declaration)")
	}
	if !strings.Contains(block, "gomock.Any()") {
		return fmt.Errorf("store mock EXPECT does not use a gomock.Any() ctx matcher")
	}
	if !strings.Contains(block, "if testCase.mockInput") || !strings.Contains(block, "!= nil") {
		return fmt.Errorf("EXPECT is not guarded by an `if testCase.mockInput… != nil` check")
	}
	if !strings.Contains(block, "assert.ErrorContains") || !strings.Contains(block, "assert.NoError") {
		return fmt.Errorf("validations must be assert.ErrorContains on error + assert.NoError/assert.Equal on success")
	}
	// Order-insensitive Equal check (live-run fix, 2026-09-10): testify
	// treats the want/actual pair symmetrically, and the model names the
	// returned value freely (the reference shape's `actualOutput` was one
	// choice among many — live runs wrote `data`). The line must reference
	// testCase.expectedOutput and one plain identifier for the actual
	// value; nil or a literal means the mapped output was never asserted.
	eqRe := regexp.MustCompile(`assert\.Equal\(t,\s*(?:testCase\.expectedOutput\s*,\s*([A-Za-z_][A-Za-z0-9_]*)|([A-Za-z_][A-Za-z0-9_]*)\s*,\s*testCase\.expectedOutput)\s*\)`)
	eqFound := false
	for _, line := range strings.Split(block, "\n") {
		m := eqRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		actual := m[1]
		if actual == "" {
			actual = m[2]
		}
		if actual == "nil" || actual == "true" || actual == "false" {
			continue // the mapped output was never really asserted
		}
		eqFound = true
		break
	}
	if !eqFound {
		return fmt.Errorf("the success path must assert.Equal the returned value against testCase.expectedOutput (e.g. assert.Equal(t, data, testCase.expectedOutput))")
	}
	src := "package gentestgate\n\n" + block
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "block.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	// The request must be declared as a pointer inside the subtest
	// (`request := &models.X{...}`, the reference shape) and the block must
	// carry exactly one suite method.
	count := 0
	pointerReq := false
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recv := ""
		switch t := fd.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				recv = id.Name
			}
		case *ast.Ident:
			recv = t.Name
		}
		if recv != u.suite || fd.Name.Name != "Test"+u.fn.Name {
			return fmt.Errorf("want one method func (suite *%s) Test%s(), got func on %s named %s", u.suite, u.fn.Name, recv, fd.Name.Name)
		}
		count++
		if fd.Body != nil {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) == 0 {
					return true
				}
				if amp, ok := as.Rhs[0].(*ast.UnaryExpr); ok && amp.Op == token.AND {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == "request" {
							pointerReq = true
						}
					}
				}
				return true
			})
		}
	}
	if count != 1 {
		return fmt.Errorf("want exactly one suite method, got %d", count)
	}
	if !pointerReq {
		return fmt.Errorf("the request must be declared inside the subtest as `request := &models.<RequestType>{...}`")
	}
	return nil
}
