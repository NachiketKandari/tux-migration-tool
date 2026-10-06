package convert

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/gen"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pred"
)

// guardBindings maps host-language spellings (pred.IdentKey: lowercased
// camelCase) to the canonical legacy guard identifier they stand for. The
// gate accepts a translated predicate only when its identifiers bind to the
// guard's identifiers through this map — an unrelated variable that happens
// to compare against the same literal never satisfies a guard.
type guardBindings struct {
	canon map[string]string
}

func newGuardBindings() *guardBindings {
	return &guardBindings{canon: map[string]string{}}
}

// claim records that spelling names ident. A key claimed by two different
// canonical identifiers goes ambiguous ("") and stops binding.
func (b *guardBindings) claim(spelling, ident string) {
	k := pred.IdentKey(spelling)
	if k == "" {
		return
	}
	if prev, ok := b.canon[k]; ok {
		if prev != ident {
			b.canon[k] = ""
		}
		return
	}
	b.canon[k] = ident
}

func (b *guardBindings) claimIdent(ident string) { b.claim(ident, ident) }

// same reports whether two host-language identifiers name the same guard
// variable: through the binding map when claimed, else by identical
// spelling keys (snake/camel/Pascal renderings of one name).
func (b *guardBindings) same(a, c string) bool {
	ka, kc := pred.IdentKey(a), pred.IdentKey(c)
	if v, ok := b.canon[ka]; ok && v != "" {
		ka = v
	}
	if v, ok := b.canon[kc]; ok && v != "" {
		kc = v
	}
	return ka == kc
}

func (b *guardBindings) clone() *guardBindings {
	out := newGuardBindings()
	for k, v := range b.canon {
		out.canon[k] = v
	}
	return out
}

// extendFromAssigns adds body-declared locals that are derived from an
// already-bound spelling (`axis := request.PxGrowthFlg`,
// `flag := cFlag`), fixpoint over chains. This keeps the local-variable
// idiom (the real run declared `c_flag := request.PxGrowthFlg`) bound
// without accepting unrelated names.
func (b *guardBindings) extendFromAssigns(assigns [][2]string) {
	for pass := 0; pass < 4; pass++ {
		changed := false
		for _, pair := range assigns {
			ident := ""
			for _, tok := range identTokens(pair[1]) {
				if v, ok := b.canon[pred.IdentKey(tok)]; ok && v != "" {
					ident = v
					break
				}
			}
			if ident == "" {
				continue
			}
			for _, tok := range identTokens(pair[0]) {
				k := pred.IdentKey(tok)
				if prev, ok := b.canon[k]; !ok {
					b.canon[k] = ident
					changed = true
				} else if prev != ident {
					b.canon[k] = ""
				}
			}
		}
		if !changed {
			break
		}
	}
}

// guardBindingsFor builds the base bindings for a slice's guards: every
// guard identifier, plus the Go request-field name that reads its FML field
// (the axis flag is typically inlined as request.<Field> or copied from it
// into a local).
//
// Foccur32 presence guards (`Foccur32(buf, FML_X) > 0`) bind the same way:
// the Go presence form (`request.X != ""`, `X != ""`, or a derived local)
// names the request field FieldFromFML(FML_X) — claimed here so the
// equivalence gate meets the legacy FML spelling.
func guardBindingsFor(guards []flow.MixedGuard, endpoint string, c *ir.Condition, svc *gen.Service) *guardBindings {
	b := newGuardBindings()
	idents := map[string]bool{}
	var foccurFields []string
	foccurSeen := map[string]bool{}
	for _, g := range guards {
		for _, text := range []string{g.Cond, g.Alt} {
			if text == "" {
				continue
			}
			pe := pred.Parse(text)
			for _, id := range flow.ExprIdents(&pe) {
				idents[id] = true
			}
			for _, f := range pred.FoccurFields(&pe) {
				if !foccurSeen[f] {
					foccurSeen[f] = true
					foccurFields = append(foccurFields, f)
				}
			}
		}
	}
	names := make([]string, 0, len(idents))
	for id := range idents {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		b.claimIdent(id)
	}
	for _, f := range foccurFields {
		if goField := common.FieldFromFML(f); goField != "" {
			b.claim(goField, f)
		}
	}
	if c == nil || svc == nil {
		return b
	}
	fml := svc.FMLRequestMap(endpoint, c)
	for _, op := range c.FmlOps {
		if op.Kind != ir.FmlGet || op.Dropped || op.Error {
			continue
		}
		field := fml[op.Field]
		if field == "" {
			continue
		}
		base := strings.TrimSuffix(op.Target, ".arr")
		base = strings.TrimSuffix(base, ".len")
		for _, id := range names {
			if base == id || strings.HasSuffix(base, id) {
				b.claim(field, id)
			}
		}
	}
	return b
}

// scenarioGuardErrs requires every runtime-dispatch guard to stay live with
// the same predicate: identical structure (|| / && / ! / comparison),
// identical literals, and identifiers that bind to the guard's variables.
// An empty guard list or an unparseable body (the parse gate owns it)
// passes silently; a nil binder falls back to spelling-key equality.
func scenarioGuardErrs(guards []flow.MixedGuard, bind *guardBindings, body string) []string {
	if len(guards) == 0 {
		return nil
	}
	if bind == nil {
		bind = newGuardBindings()
	}
	wrapped := bodyParseWrap(body)
	fset, block, _, ok := parseBodyWrapped(body)
	if !ok {
		return nil
	}
	var conds []*pred.Expr
	ast.Inspect(block, func(n ast.Node) bool {
		ist, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if txt := condSource(wrapped, fset, ist); txt != "" {
			pe := pred.ParseCode(txt)
			conds = append(conds, &pe)
		}
		return true
	})
	local := bind.clone()
	local.extendFromAssigns(bodyAssignments(wrapped, fset, block))
	var errs []string
	seen := map[[2]string]bool{}
	for _, g := range guards {
		key := [2]string{g.Cond, g.Alt}
		if seen[key] {
			continue
		}
		seen[key] = true
		wants := []*pred.Expr{}
		for _, text := range []string{g.Cond, g.Alt} {
			if text == "" {
				continue
			}
			pe := pred.ParseCode(text)
			wants = append(wants, &pe)
		}
		matched := false
		for _, want := range wants {
			for _, got := range conds {
				if pred.Equivalent(want, got, local.same) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Sprintf("condition at line %d (`%s`) lost — runtime dispatch guard, keep it live as an if condition with the same predicate", g.Line, g.Cond))
		}
	}
	return errs
}

// bodyAssignments collects (lhs, rhs)-text pairs for simple-identifier
// assignments and value specs in a controller body (the local-binding
// evidence the guard gate extends bindings from).
func bodyAssignments(wrapped string, fset *token.FileSet, block *ast.BlockStmt) [][2]string {
	var out [][2]string
	ast.Inspect(block, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE && x.Tok != token.ASSIGN {
				return true
			}
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" || len(x.Rhs) == 0 {
					continue
				}
				rhs := x.Rhs[min(i, len(x.Rhs)-1)]
				out = append(out, [2]string{id.Name, nodeSource(wrapped, fset, rhs)})
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if name.Name == "_" || i >= len(x.Values) {
					continue
				}
				out = append(out, [2]string{name.Name, nodeSource(wrapped, fset, x.Values[i])})
			}
		}
		return true
	})
	return out
}

// nodeSource slices an AST node's source text from the wrapped body, "" when
// the offsets are outside it.
func nodeSource(wrapped string, fset *token.FileSet, n ast.Node) string {
	if n == nil {
		return ""
	}
	start := fset.Position(n.Pos()).Offset
	end := fset.Position(n.End()).Offset
	if start < 0 || end > len(wrapped) || start >= end {
		return ""
	}
	return wrapped[start:end]
}

// identTokens returns the identifier-like tokens in a source snippet (string
// and char literal contents skipped) — the local-binding scan for RHS
// expressions.
func identTokens(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			q := c
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == q {
					break
				}
				j++
			}
			i = j + 1
		case isGoIdentStart(c):
			j := i
			for j < len(s) && (isGoIdentPart(s[j]) || s[j] == '.') {
				j++
			}
			for j > i && s[j-1] == '.' {
				j--
			}
			name := s[i:j]
			if k := strings.LastIndexByte(name, '.'); k >= 0 {
				name = name[k+1:]
			}
			out = append(out, name)
			i = j
		default:
			i++
		}
	}
	return out
}

func isGoIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isGoIdentPart(c byte) bool {
	return isGoIdentStart(c) || (c >= '0' && c <= '9')
}
