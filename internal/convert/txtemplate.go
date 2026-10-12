package convert

import (
	"regexp"
	"strings"

	"tux-migration-tool/internal/common"
)

// Transaction template replacement (deterministic view shaping): the legacy
// fn_*tran / tp* begin-commit calls are the two ends of the Go
// utils.ExecTransaction template. The model must translate their intent
// (begin → open the wrapper, commit → close it), but every observed attempt
// either copied the helper names (undefined-identifier rejects) or used the
// `tx` handle outside a wrapper (also undefined). The pass replaces the
// view's begin/commit call sites with the exact Go template lines, drops
// the abort sites and the tx-handle guards/init/declarations the wrapper
// subsumes, and leaves everything else byte-identical.

// txOpenLine is built from the shared naming constants rather than spelled as
// a literal, so the injected template line can never disagree with the method
// signature the gates check it against. It is a var because the constants
// arrive through a function call (GoStoreCallPrefix).
var txOpenLine = "err = utils.ExecTransaction(" + common.GoCtx + ", " + common.GoStoreCallPrefix() + "GetDB(), func(" + common.GoTx + " *sqlx.Tx) error {"

var txCloseLines = []string{
	"return nil",
	"})",
	"if err != nil {",
	"\treturn nil, err",
	"}",
}

var (
	txBeginStmtRe   = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(fn_[A-Za-z0-9_]+|tpbegin)\s*\(`)
	txBareBeginRe   = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*(fn_[A-Za-z0-9_]+|tpbegin)\s*\(`)
	txCommitStmtRe  = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*(?:[A-Za-z_][A-Za-z0-9_]*\s*=\s*)?(fn_[A-Za-z0-9_]+|tpcommit)\s*\(`)
	txCommitGuardRe = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*if\s*\(\s*(fn_[A-Za-z0-9_]+|tpcommit)\s*\(`)
	txAbortStmtRe   = regexp.MustCompile(`^\s*(?:/\*.*?\*/\s*)*(fn_[A-Za-z0-9_]+|tpabort)\s*\(`)
)

// txRole classifies a tx call name: begin, commit, abort, or none. The
// helper-trio matching mirrors flow.txCallKind (fn_*begintran/committran/
// aborttran and their _tran spellings).
func txRole(name string) string {
	switch name {
	case "tpbegin":
		return "begin"
	case "tpcommit":
		return "commit"
	case "tpabort":
		return "abort"
	}
	if !strings.HasPrefix(name, "fn_") {
		return ""
	}
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "begintran") || strings.Contains(l, "begin_tran"):
		return "begin"
	case strings.Contains(l, "committran") || strings.Contains(l, "commit_tran"):
		return "commit"
	case strings.Contains(l, "aborttran") || strings.Contains(l, "abort_tran") ||
		strings.Contains(l, "rollbacktran") || strings.Contains(l, "rollback_tran"):
		return "abort"
	}
	return ""
}

// rewriteTxTemplate runs the replacement over a seam-rewritten view.
// Returns the rewritten source and the number of call sites replaced
// (0 → unchanged).
func rewriteTxTemplate(src string) (string, int) {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	handles := map[string]bool{}
	replaced := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := txBeginStmtRe.FindStringSubmatchIndex(line); m != nil {
			name := line[m[4]:m[5]]
			if txRole(name) == "begin" && txCallEndsLine(line, m[5]-1) {
				handles[line[m[2]:m[3]]] = true
				out = append(out, txOpenLine)
				replaced++
				continue
			}
		}
		if m := txBareBeginRe.FindStringSubmatchIndex(line); m != nil {
			name := line[m[2]:m[3]]
			if txRole(name) == "begin" && txCallEndsLine(line, m[3]-1) {
				out = append(out, txOpenLine)
				replaced++
				continue
			}
		}
		if m := txCommitGuardRe.FindStringSubmatchIndex(line); m != nil {
			name := line[m[2]:m[3]]
			if txRole(name) == "commit" && txCommitGuardShape(line, m[3]-1) {
				if _, end, ok := braceBlockLines(lines, i); ok {
					out = append(out, txCloseLines...)
					replaced++
					i = end - 1
					continue
				}
			}
		}
		if m := txCommitStmtRe.FindStringSubmatchIndex(line); m != nil {
			name := line[m[2]:m[3]]
			if txRole(name) == "commit" && txCallEndsLine(line, m[3]-1) {
				out = append(out, txCloseLines...)
				replaced++
				continue
			}
		}
		if m := txAbortStmtRe.FindStringSubmatchIndex(line); m != nil {
			name := line[m[2]:m[3]]
			if txRole(name) == "abort" && txCallEndsLine(line, m[3]-1) {
				replaced++
				continue
			}
		}
		out = append(out, line)
	}
	if len(handles) > 0 {
		out = dropTxHandleLines(out, handles)
	}
	if replaced == 0 {
		return src, 0
	}
	return strings.Join(out, "\n"), replaced
}

// txCallEndsLine reports whether the call whose name ends at nameEnd is the
// whole statement: balanced parens, one closing semicolon, then only
// whitespace/comments.
func txCallEndsLine(line string, nameEnd int) bool {
	open := nameEnd + 1
	if open >= len(line) || line[open] != '(' {
		return false
	}
	_, close, ok := balancedParens(line, open)
	if !ok {
		return false
	}
	rest := strings.TrimSpace(line[close+1:])
	if !strings.HasPrefix(rest, ";") {
		return false
	}
	rest = strings.TrimSpace(rest[1:])
	for strings.HasPrefix(rest, "/*") {
		j := strings.Index(rest[2:], "*/")
		if j < 0 {
			return false
		}
		rest = strings.TrimSpace(rest[2+j+2:])
	}
	return rest == "" || strings.HasPrefix(rest, "//")
}

// txCommitGuardShape requires `if (name(...) == -1)` around the call.
func txCommitGuardShape(line string, nameEnd int) bool {
	open := nameEnd + 1
	if open >= len(line) || line[open] != '(' {
		return false
	}
	_, close, ok := balancedParens(line, open)
	if !ok {
		return false
	}
	rest := strings.TrimSpace(line[close+1:])
	return strings.HasPrefix(rest, "==") && strings.TrimSpace(strings.TrimPrefix(rest, "==")) == "-1)"
}

// dropTxHandleLines removes the tx-handle bookkeeping the wrapper subsumes:
// `if (h == -1) { ... }` guard blocks, `h = 0;` initializations and
// int/long declarations.
func dropTxHandleLines(lines []string, handles map[string]bool) []string {
	var out []string
	guard := make(map[string]*regexp.Regexp, len(handles))
	init := make(map[string]*regexp.Regexp, len(handles))
	decl := make(map[string]*regexp.Regexp, len(handles))
	for h := range handles {
		q := regexp.QuoteMeta(h)
		guard[h] = common.CachedRegexp(`^\s*(?:/\*.*?\*/\s*)*if\s*\(\s*` + q + `\s*==\s*-1\s*\)`)
		init[h] = common.CachedRegexp(`^\s*(?:/\*.*?\*/\s*)*` + q + `\s*=\s*0\s*;`)
		decl[h] = common.CachedRegexp(`^\s*(?:int|long|short)\s+` + q + `\s*;`)
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		dropped := false
		for h := range handles {
			switch {
			case guard[h].MatchString(line):
				if _, end, ok := braceBlockLines(lines, i); ok {
					i = end - 1
					dropped = true
				}
			case init[h].MatchString(line), decl[h].MatchString(line):
				dropped = true
			}
			if dropped {
				break
			}
		}
		if !dropped {
			out = append(out, line)
		}
	}
	return out
}
