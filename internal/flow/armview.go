package flow

// ArmView is the language-neutral endpoint slice view (uniform-ir plan
// §3.4): one line per kept source line with EXEC SQL / FML / ATMI constructs
// replaced by bracket placeholders that carry query and field identities
// but zero host-language syntax. Backends render their LLM-seam prompts
// from this view via their namer; the Go-specific RenderSpan stays as the
// Go formatter over the same tree until backends cut over.
type ViewLineKind string

const (
	ViewCode        ViewLineKind = "code"
	ViewQuery       ViewLineKind = "query"
	ViewFMLOp       ViewLineKind = "fml_op"
	ViewTPCall      ViewLineKind = "tpcall"
	ViewReturn      ViewLineKind = "return"
	ViewDropped     ViewLineKind = "dropped"
	ViewPlaceholder ViewLineKind = "placeholder"
)

// ViewLine is one ArmView line with its source line number.
type ViewLine struct {
	Line int          `json:"line"`
	Kind ViewLineKind `json:"kind"`
	Text string       `json:"text"`
	// QueryID carries the IR query id for ViewQuery lines; Fields carries
	// FML field names for ViewFMLOp lines; Call carries the callee for
	// ViewTPCall/ViewDropped lines.
	QueryID string   `json:"query_id,omitempty"`
	Fields  []string `json:"fields,omitempty"`
	Call    string   `json:"call,omitempty"`
}

// ArmView renders the nodes whose span sits inside [from, to] as
// language-neutral placeholder lines. It never emits Go/Python/C# syntax:
// SQL becomes "[query:<id> <sub>]", FML becomes "[fml:get|add FIELD]",
// tpreturn becomes "[return ...]", dropped Tuxedo plumbing becomes
// "[dropped:<call>]".
func ArmView(tree *Tree, from, to int) []ViewLine {
	v := &armViewer{from: from, to: to}
	if tree != nil {
		v.nodes(tree.Root)
	}
	return v.out
}

type armViewer struct {
	from, to int
	out      []ViewLine
}

func (v *armViewer) nodes(ns []*Node) {
	for _, n := range ns {
		if n.EndLine < v.from || n.Line > v.to {
			continue
		}
		v.node(n)
	}
}

func (v *armViewer) node(n *Node) {
	switch n.Kind {
	case KindSQL:
		switch n.Sub {
		case "OPEN", "CLOSE", "DECLARE_CURSOR", "DECLARE_SECTION", "INCLUDE":
			v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewDropped, Text: "[dropped:" + n.Sub + "]", Call: n.Sub})
			return
		}
		if len(n.QueryIDs) > 0 {
			for _, id := range n.QueryIDs {
				v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewQuery, Text: "[query:" + id + " " + n.Sub + "]", QueryID: id, Call: n.Sub})
			}
			return
		}
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewPlaceholder, Text: "[unlinked-sql:" + n.Sub + "]", Call: n.Sub})
	case KindReturn:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewReturn, Text: "[return]"})
	case KindDecl:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewDropped, Text: "[dropped:decl]"})
	case KindBranch:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewCode, Text: "[branch:" + n.Sub + " " + n.Cond + "]"})
		v.nodes(n.Children)
		v.out = append(v.out, ViewLine{Line: n.EndLine, Kind: ViewCode, Text: "[end-branch]"})
	case KindLoop:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewCode, Text: "[loop:" + n.Sub + " " + n.Cond + "]"})
		v.nodes(n.Children)
		v.out = append(v.out, ViewLine{Line: n.EndLine, Kind: ViewCode, Text: "[end-loop]"})
	case KindStmt:
		if len(n.FmlOps) > 0 {
			for _, op := range n.FmlOps {
				v.out = append(v.out, ViewLine{
					Line: n.Line, Kind: ViewFMLOp,
					Text:   "[fml:" + string(op.Kind) + " " + op.Field + "]",
					Fields: []string{op.Field},
				})
			}
			return
		}
		if len(n.Calls) > 0 {
			v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewCode, Text: "[call:" + n.Calls[0] + "]", Call: n.Calls[0]})
			return
		}
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewCode, Text: n.Text})
	case KindUnknown:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewPlaceholder, Text: n.Text})
	default:
		v.out = append(v.out, ViewLine{Line: n.Line, Kind: ViewCode, Text: n.Text})
	}
}

// CallFor / RowFor generalize the Go-specific Resolver (FLW-D5) to
// language-neutral naming: backends implement these over their namer +
// plan, and formatters (Go RenderSpan today, others tomorrow) consume them.
// They live here so flow stays the one home for the arm-view contract.
type CallResolver interface {
	// CallFor returns the backend's call expression for a query unit.
	CallFor(queryID string) (expr string, ok bool)
	// RowFor returns the backend's row type for a multi-row query unit.
	RowFor(queryID string) (typ string, ok bool)
}
