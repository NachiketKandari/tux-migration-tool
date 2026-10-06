// Same-file helper signatures (user directive 2026-09-17): a fn_* function
// defined in the main file itself is a helper, not session plumbing — it
// converts as a KindFnHelper unit (controller/fns.go), and its caller calls
// the generated method. Two independent LLM seams must agree on the method
// signature, so the signature is derived deterministically here and
// prescribed verbatim to both seams: the legacy parameter list in order,
// middleware-owned session params dropped, C types mapped to Go.
package plan

import (
	"strings"

	scanner "tux-migration-tool/internal/tsscan"
)

// FnParam is one deterministic Go parameter of a converted helper. Name
// stays the legacy spelling so the helper body (whose view carries the
// legacy locals verbatim) reads naturally.
type FnParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// localHelper is one same-file helper discovered by Build: its span, the
// deterministic signature, and the canonical query IDs its body owns.
type localHelper struct {
	name   string
	goName string
	start  int
	end    int
	params []FnParam
	ret    string
	qids   []string
}

// helperDropParams are the middleware-owned identifiers a helper signature
// never carries: the Go middleware owns the service name, the session id,
// and the shared error buffers. Unlike the stub-arg scrub, the user id
// stays — it is a real input the helper may bind into SQL.
var helperDropParams = map[string]bool{
	"c_ServiceName": true, "c_errmsg": true, "c_err_msg": true,
	"errmsg": true, "err_msg": true,
	"li_session_id": true, "l_sssn_id": true, "DEF_USR": true, "DEF_SSSN": true,
}

// helperSignature derives a legacy helper's Go parameter list and return
// type from its declaration text: params in legacy order, session params
// dropped, C scalar/pointer types mapped (char*→string, long→int64,
// int→int, double→float64, pointers preserved). ok=false when the
// declaration cannot be read confidently — the caller keeps the fn's
// legacy drop/stub handling instead of guessing a signature.
func helperSignature(def scanner.FunctionDef, src string) ([]FnParam, string, bool) {
	lines := strings.Split(src, "\n")
	if def.StartLine < 1 || def.BodyStartLine <= def.StartLine || def.BodyStartLine > len(lines) {
		return nil, "", false
	}
	decl := strings.Join(lines[def.StartLine-1:def.BodyStartLine-1], "\n")
	nameIdx := strings.Index(decl, def.Name)
	if nameIdx < 0 {
		return nil, "", false
	}
	open := strings.Index(decl[nameIdx+len(def.Name):], "(")
	if open < 0 {
		return nil, "", false
	}
	open += nameIdx + len(def.Name)
	content, _, ok := fnBalancedParens(decl, open)
	if !ok {
		return nil, "", false
	}
	ret, ok := helperGoType(def.ReturnType)
	if !ok {
		return nil, "", false
	}
	var params []FnParam
	for _, raw := range fnSplitTopCommas(content) {
		p := strings.TrimSpace(raw)
		if p == "" || strings.EqualFold(p, "void") {
			continue
		}
		p = strings.TrimSpace(strings.TrimPrefix(p, "const "))
		name := fnLastIdent(p)
		if name == "" || name == p {
			return nil, "", false // unnamed or malformed parameter
		}
		typ := strings.TrimSpace(p[:strings.LastIndex(p, name)])
		if typ == "" || helperDropParams[name] {
			if typ == "" {
				return nil, "", false
			}
			continue
		}
		gt, ok := helperGoType(typ)
		if !ok {
			return nil, "", false
		}
		if gt == "" {
			return nil, "", false // a void parameter is never a value
		}
		params = append(params, FnParam{Name: name, Type: gt})
	}
	return params, ret, true
}

// helperGoType maps a C declaration type to its Go spelling. Pointer depth
// is preserved for scalars; char pointers/arrays and varchar collapse to
// string (the value model the helper seam consumes); void maps to "".
func helperGoType(ctype string) (string, bool) {
	t := strings.TrimSpace(ctype)
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(t, "const ")), "const"))
	if i := strings.Index(t, "["); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	ptr := 0
	for strings.HasSuffix(t, "*") {
		ptr++
		t = strings.TrimSpace(strings.TrimSuffix(t, "*"))
	}
	var base string
	switch t {
	case "void":
		if ptr > 0 {
			return "", false
		}
		return "", true
	case "char", "varchar", "unsigned char":
		base = "string"
	case "int", "short":
		base = "int"
	case "long":
		base = "int64"
	case "double":
		base = "float64"
	case "float":
		base = "float32"
	default:
		return "", false
	}
	if base == "string" {
		return "string", true
	}
	return strings.Repeat("*", ptr) + base, true
}

// fnLastIdent returns the trailing Go-safe identifier of a declaration
// fragment ("" when it does not end in one).
func fnLastIdent(s string) string {
	s = strings.TrimSpace(s)
	end := len(s)
	for end > 0 && !isFnIdentByte(s[end-1]) {
		end--
	}
	start := end
	for start > 0 && isFnIdentByte(s[start-1]) {
		start--
	}
	return s[start:end]
}

func isFnIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// fnBalancedParens returns the content between the paren at start and its
// matching close, honoring string/char literals.
func fnBalancedParens(s string, start int) (string, int, bool) {
	depth := 0
	inStr, inChar, esc := false, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
		case inChar:
			if ch == '\\' {
				esc = true
			} else if ch == '\'' {
				inChar = false
			}
		case ch == '"':
			inStr = true
		case ch == '\'':
			inChar = true
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
			if depth == 0 {
				return s[start+1 : i], i, true
			}
		}
	}
	return "", 0, false
}

// fnSplitTopCommas splits on commas at bracket depth zero, honoring
// literals.
func fnSplitTopCommas(s string) []string {
	var parts []string
	depth := 0
	inStr, inChar, esc := false, false, false
	last := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
		case inChar:
			if ch == '\\' {
				esc = true
			} else if ch == '\'' {
				inChar = false
			}
		case ch == '"':
			inStr = true
		case ch == '\'':
			inChar = true
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
		case ch == ',' && depth == 0:
			parts = append(parts, s[last:i])
			last = i + 1
		}
	}
	return append(parts, s[last:])
}
