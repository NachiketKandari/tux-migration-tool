// Nice names (GT-7 CLI --nice-names): the one remaining LLM use in the
// default pipeline, and an optional one. It may rewrite string literals
// (test-case descriptions) and comments only — the gate re-scans the
// polished block and requires identical tokens for every other kind, so
// names can improve while assertions and structure cannot move. Any miss
// returns the original block untouched (best-effort by design).
package testgen

import (
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"strings"

	"tux-migration-tool/internal/llm"
)

// polish applies the optional nice-names pass (a no-op when disabled or
// when no client is wired).
func (o Options) polish(ctx context.Context, block string) string {
	if !o.NiceNames || o.Client == nil || strings.TrimSpace(block) == "" {
		return block
	}
	out, err := polishNames(ctx, block, o)
	if err != nil {
		return block
	}
	return out
}

// polishNames asks the seam for the same suite method with nicer test-case
// descriptions and comments, then enforces the names-only contract.
func polishNames(ctx context.Context, block string, opts Options) (string, error) {
	maxTokens := opts.Budget.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 2000
	}
	resp, err := opts.Client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: "You polish Go test names. You may change ONLY string literals and comments. Never change identifiers, calls, assertions, literals of other kinds, or control flow."},
			{Role: "user", Content: "Rewrite the test-case descriptions (the quoted `desc:` values) and comments below to be clear, specific human names. Output exactly one ```go fenced block containing the identical code otherwise.\n\n```go\n" + block + "\n```"},
		},
		Temperature: 0.2,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", err
	}
	polished := strings.TrimSpace(llm.ExtractFenced(resp.Content, "go"))
	if polished == "" {
		return "", fmt.Errorf("nice-names: empty response")
	}
	if err := sameStructureExceptStrings(block, polished); err != nil {
		return "", fmt.Errorf("nice-names gate: %w", err)
	}
	return polished, nil
}

// sameStructureExceptStrings compares two Go snippets token by token.
// Comments are ignored (they may be polished freely); token kinds must match
// exactly and token text must match for every kind except STRING (the names
// being polished). Any other drift — an identifier, an operator, a number —
// fails the gate.
func sameStructureExceptStrings(a, b string) error {
	ta, err := scanTokens(a)
	if err != nil {
		return err
	}
	tb, err := scanTokens(b)
	if err != nil {
		return err
	}
	if len(ta) != len(tb) {
		return fmt.Errorf("token count changed (%d → %d)", len(ta), len(tb))
	}
	for i := range ta {
		if ta[i].tok != tb[i].tok {
			return fmt.Errorf("token %d kind changed (%s → %s)", i, ta[i].tok, tb[i].tok)
		}
		if ta[i].tok == token.STRING {
			continue
		}
		if ta[i].lit != tb[i].lit {
			return fmt.Errorf("token %d text changed (%q → %q)", i, ta[i].lit, tb[i].lit)
		}
	}
	return nil
}

type scanTok struct {
	tok token.Token
	lit string
}

// scanTokens tokenizes a snippet with comments skipped: comments are outside
// the names-only contract's structural comparison.
func scanTokens(src string) ([]scanTok, error) {
	fset := token.NewFileSet()
	file := fset.AddFile("block.go", -1, len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), nil, scanner.ScanComments)
	var out []scanTok
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT || tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		out = append(out, scanTok{tok: tok, lit: lit})
	}
	return out, nil
}
