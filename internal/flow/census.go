package flow

import (
	"sort"
	"strings"

	"tux-migration-tool/internal/pred"
)

// CensusCond is one legacy condition the deterministic draft renders — the
// transparency census Workstream B gates the LLM seam against. Line/Cond
// locate the source branch; Skeleton is the rename-robust canonical form
// (operators and literal values kept, identifiers → `#`); Idents are the
// predicate's identifiers in first-appearance order; Effects are the LHS
// identifiers of direct assignments in the branch body; HasElse reports
// whether the if/elseif chain continues into an else/elseif sibling.
type CensusCond struct {
	Line     int      `json:"line"`
	Cond     string   `json:"cond"`
	Skeleton string   `json:"skeleton"`
	Idents   []string `json:"idents"`
	Effects  []string `json:"effects"`
	HasElse  bool     `json:"has_else"`
}

// Skeleton renders a rename-robust canonical string for a predicate:
// operators and literal values are kept, every identifier becomes `#`.
// `cnt_d2u > 0 || i_cnt_d2us > 0` → `# > 0 || # > 0`, while a single-arm
// `cntD2u > 0` → `# > 0` — the lost-OR-arm shape matches neither arm alone.
//
// Foccur32 presence checks normalize to the Go presence form they
// transliterate to: `Foccur32(buf, FML_X) > 0` → `# != ""`,
// `Foccur32(buf, FML_X) == 0` → `# == ""`. The buffer spelling never
// participates, and a presence check never collides with a generic
// numeric `x > 0` (`# > 0`).
func Skeleton(e *pred.Expr) string {
	if e == nil {
		return ""
	}
	if _, present, ok := pred.PresenceOf(e); ok {
		if present {
			return `# != ""`
		}
		return `# == ""`
	}
	if _, present, ok := pred.GoPresenceOf(e); ok {
		if present {
			return `# != ""`
		}
		return `# == ""`
	}
	switch e.Kind {
	case "or", "and":
		sep := " && "
		if e.Kind == "or" {
			sep = " || "
		}
		parts := make([]string, 0, len(e.Items))
		for i := range e.Items {
			parts = append(parts, Skeleton(&e.Items[i]))
		}
		return strings.Join(parts, sep)
	case "not":
		s := Skeleton(e.Inner)
		if e.Inner != nil && e.Inner.Kind == "cmp" {
			return "!(" + s + ")"
		}
		return "!" + s
	case "cmp":
		return Skeleton(e.L) + " " + e.Op + " " + Skeleton(e.R)
	case "ident":
		if e.Name == "NULL" {
			return "nil"
		}
		return "#"
	case "lit":
		return normLit(e.Text)
	case "call":
		return "#"
	default:
		return strings.Join(strings.Fields(e.Text), " ")
	}
}

// ExprIdents collects a predicate's identifiers in first-appearance order:
// ident leaves, call callees, and identifiers inside call argument text.
// Raw degrade nodes contribute their word-like tokens (best effort).
func ExprIdents(e *pred.Expr) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	var walk func(x *pred.Expr)
	walk = func(x *pred.Expr) {
		if x == nil {
			return
		}
		switch x.Kind {
		case "or", "and":
			for i := range x.Items {
				walk(&x.Items[i])
			}
		case "not":
			walk(x.Inner)
		case "cmp":
			walk(x.L)
			walk(x.R)
		case "ident":
			add(x.Name)
		case "call":
			add(x.Name)
			for _, a := range x.Args {
				for _, id := range scanIdents(a) {
					add(id)
				}
			}
		case "raw":
			for _, id := range scanIdents(x.Text) {
				add(id)
			}
		}
	}
	walk(e)
	return out
}

// normLit normalizes a literal for skeleton matching: C char literals
// (`'Y'`) become Go strings (`"Y"`) so the transliteration the seam
// requires doesn't break the match; everything else is trimmed verbatim.
func normLit(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 3 && t[0] == '\'' && t[len(t)-1] == '\'' {
		return `"` + t[1:len(t)-1] + `"`
	}
	return t
}

// scanIdents extracts word-like identifiers from free text, skipping pure
// numbers and C type keywords that never name a variable.
func scanIdents(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if !isIdentStartByte(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isIdentByte(s[j]) {
			j++
		}
		word := s[i:j]
		if !isNumWord(word) && !isTypeWord(word) {
			out = append(out, word)
		}
		i = j
	}
	return out
}

func isNumWord(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isTypeWord(s string) bool {
	switch strings.ToLower(s) {
	case "int", "long", "short", "char", "float", "double", "void",
		"unsigned", "signed", "static", "const", "struct", "varchar":
		return true
	}
	return false
}

// censusEffects collects the LHS identifiers of direct assignments in a
// branch body's immediate statement children — the same top-level assign
// rule the renderer uses for its `lhs := rhs` draft.
func censusEffects(children []*Node) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range children {
		if c.Kind != KindStmt || c.Text == "" {
			continue
		}
		for _, line := range strings.Split(c.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || topLevelAssignIndex(line) < 0 {
				continue
			}
			lhs, _ := splitAssign(line)
			for _, id := range lhsIdents(lhs) {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// lhsIdents extracts the assigned identifiers from an assignment LHS:
// the leading identifier of each comma-separated target (`a, b = ...`
// assigns both; `i_err_op[0]` assigns `i_err_op`; `*p` assigns `p`).
func lhsIdents(lhs string) []string {
	var out []string
	for _, part := range strings.Split(lhs, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimLeft(p, "*(")
		p = strings.TrimSpace(p)
		if p == "" || !isIdentStartByte(p[0]) {
			continue
		}
		j := 1
		for j < len(p) && isIdentByte(p[j]) {
			j++
		}
		out = append(out, p[:j])
	}
	return out
}

// ConditionCensus returns the transparency census for a line span: the same
// walk RenderSpan performs (nil resolver — store placeholders don't shift
// branches), so the census and the draft can never diverge. Callers must
// honor the additive/never-fatal contract: census runs only when the flow
// draft is non-empty.
func ConditionCensus(tree *Tree, from, to int) []CensusCond {
	if tree == nil || len(tree.Root) == 0 {
		return nil
	}
	return RenderSpan(tree, nil, from, to, 1).Conditions
}

// censusSkipped reports whether a branch stays out of the census, mirroring
// the renderer's own elisions plus the plumbing-predicate filter. Loops
// never reach here (the census covers if/elseif headers only).
//
// Foccur32/Foccur presence checks (`Foccur32(buf, FML_X) > 0`) are business
// logic — which optional request flag arrived — and never skip: they stay
// in the census with a presence-normalized skeleton (`# != ""` / `# == ""`).
func censusSkipped(n *Node, fetchDepth int) bool {
	if n.Kind != KindBranch {
		return true
	}
	if n.Sub == "else" {
		return true
	}
	if isRequestGuard(n) || isDebugIf(n) {
		return true
	}
	if fetchDepth > 0 && strings.Contains(n.Cond, "SQLCODE") {
		return true
	}
	c := n.Cond
	if strings.Contains(c, "SQLCODE") || strings.Contains(c, "Ferror32") ||
		strings.Contains(c, "FNOTPRES") || strings.Contains(c, "DEBUG_") {
		return true
	}
	if n.Predicate == nil {
		return true
	}
	return false
}
