package ir

import (
	"tux-migration-tool/internal/pred"
	"tux-migration-tool/internal/tsscan"
)

// buildConditions extracts the entry function's top-level dispatch chains.
// Files keep chains of at least two branches; fragments keep single-branch
// chains and synthesize a whole-span default condition when chainless.
// A whole file whose chains all fall under that bar is not left with an
// empty inventory: its single-branch chains are the only arms it has (a
// guard-only service still converts), so they join as the fallback.
func buildConditions(facts *tsscan.SourceFacts, f *File, ops []FmlOp) []Condition {
	if f.Entry == "" && !f.Fragment {
		return nil
	}
	chains := chainsOf(facts, f.Entry)
	minChain := 2
	if f.Fragment {
		minChain = 1
	}
	qualifying, singles := splitChains(chains, minChain)
	if len(qualifying) == 0 && !f.Fragment && len(singles) > 0 {
		// No chain reaches the two-branch bar: include the single-branch
		// chains — an empty inventory would leave the file unmappable.
		qualifying = singles
	}
	decls := map[string]bool{}
	for _, d := range facts.VarDecls {
		decls[d.Name] = true
	}
	for _, d := range facts.Params {
		decls[d.Name] = true
	}
	isFlagVar := func(name string) bool { return decls[name] }

	var out []Condition
	index := 0
	for _, chain := range qualifying {
		for _, b := range chain {
			index++
			cond := Condition{
				Index:     index,
				Kind:      string(b.Kind),
				Expr:      b.Cond,
				StartLine: b.StartLine,
				IsDefault: b.Kind == tsscan.BranchElse,
			}
			cond.EndLine = b.BlockEnd
			if cond.EndLine <= 0 {
				cond.EndLine = b.StartLine
			}
			if b.Kind != tsscan.BranchElse && b.Cond != "" {
				p := pred.Parse(b.Cond)
				cond.Predicate = &p
				seen := map[string]bool{}
				// Flag vars come from the raw condition text (not the
				// predicate tree): subscripted and otherwise unparseable
				// conditions must still yield their variables.
				for _, tok := range identTokens(b.Cond) {
					if isFlagVar(tok) && !seen[tok] {
						seen[tok] = true
						cond.FlagVars = append(cond.FlagVars, tok)
					}
				}
			}
			for _, op := range ops {
				if op.Line >= cond.StartLine && op.Line <= cond.EndLine {
					cond.FmlOps = append(cond.FmlOps, op)
				}
			}
			for _, q := range f.Queries {
				if q.StartLine >= cond.StartLine && q.StartLine <= cond.EndLine {
					cond.QueryIDs = append(cond.QueryIDs, q.ID)
				}
			}
			out = append(out, cond)
		}
	}
	if len(out) == 0 && f.Fragment {
		// chainless fragment: one synthetic default spanning the fragment
		out = append(out, Condition{
			Index: 1, Kind: string(tsscan.BranchElse), IsDefault: true,
			StartLine: 1, EndLine: facts.NumLines,
		})
	}
	return out
}

// splitChains partitions chains by the branch-count bar: qualifying chains
// meet minChain; the rest (all single-branch chains when minChain is 2)
// come back as singles.
func splitChains(chains [][]tsscan.Branch, minChain int) (qualifying, singles [][]tsscan.Branch) {
	for _, chain := range chains {
		if len(chain) >= minChain {
			qualifying = append(qualifying, chain)
		} else {
			singles = append(singles, chain)
		}
	}
	return qualifying, singles
}

// chainsOf groups the entry function's depth-1 branch records into chains:
// a chain starts at `if`, continues through sibling `else if` arms, and may
// end with `else`. Only braced arms participate (chains are brace-delimited;
// unbraced arms neither join nor break a chain).
func chainsOf(facts *tsscan.SourceFacts, entry string) [][]tsscan.Branch {
	var chains [][]tsscan.Branch
	var cur []tsscan.Branch
	for _, b := range facts.Branches {
		if b.Function != entry || b.Depth != 1 || b.BlockStart == 0 {
			continue
		}
		switch b.Kind {
		case tsscan.BranchIf:
			if len(cur) > 0 {
				chains = append(chains, cur)
			}
			cur = []tsscan.Branch{b}
		case tsscan.BranchElseIf:
			if len(cur) > 0 {
				cur = append(cur, b)
			}
		case tsscan.BranchElse:
			if len(cur) > 0 {
				cur = append(cur, b)
				chains = append(chains, cur)
				cur = nil
			}
		}
	}
	if len(cur) > 0 {
		chains = append(chains, cur)
	}
	return chains
}

// identTokens scans raw condition text for identifier-charset tokens
// (subscripts and unparseable shapes still yield their variables). Tokens
// inside call arguments are skipped — a guard's own Fget32 args are not
// flag variables.
func identTokens(text string) []string {
	var out []string
	i := 0
	for i < len(text) {
		c := text[i]
		if isIdentByteFor(c) && (i == 0 || !isIdentByteFor(text[i-1])) && !isDigitByte(c) {
			j := i
			for j < len(text) && isIdentByteFor(text[j]) {
				j++
			}
			name := text[i:j]
			// a call swallows its arguments
			k := j
			for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
				k++
			}
			if k < len(text) && text[k] == '(' {
				depth := 0
				for k < len(text) {
					switch text[k] {
					case '(':
						depth++
					case ')':
						depth--
						if depth == 0 {
							k++
							goto next
						}
					case '\'':
						for k+1 < len(text) && !(text[k] == '\'' && text[k-1] != '\\') {
							k++
						}
					}
					k++
				}
			next:
				i = k
				continue
			}
			out = append(out, name)
			i = j
			continue
		}
		i++
	}
	return out
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }
