// Same-file helper call rewriting (user directive 2026-09-17): a fn_*
// function the main file defines converts as a KindFnHelper unit whose Go
// method lives in controller/fns.go. The view's legacy call site carries
// the mapped Go call so the model copies it: the callee becomes
// s.<GoName>, and the args the helper's Go signature dropped (service
// name, session id, error buffers) are removed — the remaining args are the
// helper's parameters in order. The same rewrite runs inside helper bodies
// (nested helper calls). Calls may span lines (the corpus wraps long
// argument lists), so the scan runs over the whole view, not per line.
package convert

import (
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/plan"
)

// rewriteHelperCalls maps every legacy same-file helper call in the view to
// its generated Go method call, dropping the middleware-owned args the
// helper signature does not carry. Returns the rewritten source and the
// number of call sites rewritten.
func rewriteHelperCalls(src string, helpers map[string]plan.FnHelper) (string, int) {
	if len(helpers) == 0 {
		return src, 0
	}
	names := make(map[string]bool, len(helpers))
	for n := range helpers {
		names[n] = true
	}
	callRe := stubCallRe(names)
	var b strings.Builder
	pos := 0
	changed := 0
	for {
		loc := callRe.FindStringIndex(src[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		open := start + strings.Index(src[start:end], "(")
		name := strings.TrimSpace(src[start:open])
		h, known := helpers[name]
		if !known {
			pos = end
			continue
		}
		content, close, ok := balancedParens(src, open)
		if !ok {
			pos = end
			continue
		}
		b.WriteString(src[pos:start])
		b.WriteString(helperCallSpelling(h, content))
		pos = close + 1
		changed++
	}
	if changed == 0 {
		return src, 0
	}
	b.WriteString(src[pos:])
	return b.String(), changed
}

// helperCallSpelling renders the Go call for one helper site: the callee is
// s.<GoName>, and args the helper's signature dropped (middleware-owned,
// session-scoped) are removed; the rest keep their verbatim spelling for
// the model to map to locals.
func helperCallSpelling(h plan.FnHelper, content string) string {
	kept := make(map[string]bool, len(h.Params))
	for _, p := range h.Params {
		kept[p.Name] = true
	}
	args := splitTopArgs(content)
	out := make([]string, 0, len(args))
	for _, a := range args {
		if n := normalizeSeamTarget(a); sessionArgNames[n] && !kept[n] {
			continue
		}
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return common.GoCtrlRecv + "." + h.GoName + "(" + strings.Join(out, ", ") + ")"
}
