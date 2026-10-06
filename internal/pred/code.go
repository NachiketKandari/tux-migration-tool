package pred

import (
	"strings"

	"tux-migration-tool/internal/common"
)

// ParseCode parses a Go/C# boolean condition into the same tree as Parse:
// dotted selector chains collapse to their last segment
// (`request.PxGrowthFlg` → `PxGrowthFlg`) and char/rune literals become
// double-quoted strings, so translated runtime guards land in the legacy
// predicate grammar without losing their identifier names.
func ParseCode(text string) Expr {
	return Parse(rewriteCode(text))
}

// rewriteCode performs the language-syntax rewrite backing ParseCode.
func rewriteCode(text string) string {
	var sb strings.Builder
	i := 0
	for i < len(text) {
		c := text[i]
		switch {
		case c == '"' || c == '\'':
			q := c
			j := i + 1
			for j < len(text) {
				if text[j] == '\\' {
					j += 2
					continue
				}
				if text[j] == q {
					break
				}
				j++
			}
			if j >= len(text) {
				j = len(text) - 1
			}
			lit := text[i : j+1]
			if q == '\'' {
				lit = `"` + strings.TrimSuffix(strings.TrimPrefix(lit, "'"), "'") + `"`
			}
			sb.WriteString(lit)
			i = j + 1
		case isIdentChar(c) && (c < '0' || c > '9'):
			j := i
			for j < len(text) && (isIdentChar(text[j]) || text[j] == '.') {
				j++
			}
			for j > i && text[j-1] == '.' {
				j--
			}
			name := text[i:j]
			if k := strings.LastIndexByte(name, '.'); k >= 0 {
				last := name[k+1:]
				// Pro*C varchar host vars read as `<base>.arr` (and `.len`)
				// name the base variable, not the member: keep the base.
				if last == "arr" || last == "len" {
					name = name[:k]
					if k2 := strings.LastIndexByte(name, '.'); k2 >= 0 {
						name = name[k2+1:]
					}
				} else {
					name = last
				}
			}
			sb.WriteString(name)
			i = j
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return sb.String()
}

// IdentKey is the spelling-insensitive key two identifiers share when they
// name the same legacy variable in a host language: lowercased CamelLowerGo
// so snake/camel/Pascal renderings of the SAME name match while distinct
// legacy names (c_flag vs px_growth_flg) keep their own identity. Callers
// build their accepted-spelling maps on this key.
func IdentKey(s string) string {
	return strings.ToLower(common.CamelLowerGo(s))
}
