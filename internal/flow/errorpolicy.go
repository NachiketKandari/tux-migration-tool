package flow

import (
	"strings"

	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/pred"
	"tux-migration-tool/internal/tsscan"
)

// ErrorPolicyOf classifies one branch predicate as a post-statement error
// guard, answering the only question an emitter needs: does this predicate
// treat an empty result as a failure?
//
// It deliberately does NOT answer "is this a status guard" — the two are not
// the same predicate. Three distinctions carry the whole contract:
//
//   - `if (SQLCODE != 0)` is strict. An empty result is an error.
//   - `if (SQLCODE != 0 && SQLCODE != NO_DATA_FOUND)` tolerates an empty
//     result. It says out loud that status 100 is not a failure.
//   - `else if (SQLCODE == NO_DATA_FOUND)` states no error policy at all. It
//     is the tolerant case's success arm, and reading it as strict inverts
//     the meaning of the arm above it.
//
// The fourth case is why this returns an unclassified verdict instead of a
// best guess: a predicate that ORs the status register with an unrelated
// business test (`SQLCODE == NO_DATA_FOUND || sql_x == 'N'`) has two different
// failure halves, and collapsing it to one polarity would assert something the
// source does not say.
//
// An unset return means "this is not a status guard". That is distinct from
// contract.PolicyUnclassified and must stay distinct: unset leaves the
// emitter's existing behaviour alone, whereas unclassified is a guard we found
// and could not read, which the census counts.
func ErrorPolicyOf(e *pred.Expr) contract.ErrorPolicy {
	if e == nil {
		return ""
	}
	scan, ok := scanStatusPredicate(e)
	if !ok {
		return "" // no status register anywhere: not this function's business
	}
	switch {
	case scan.mixedWithBusiness:
		// Rule 5. Checked before tolerance so an OR-ed business test can
		// never be silently absorbed into "this tolerates no rows".
		return contract.PolicyUnclassified
	case scan.excludesBenign:
		return contract.PolicyTolerantNoRows
	case scan.benignSuccessArm:
		// `== NO_DATA_FOUND`: the success arm of a tolerant guard. It
		// declares no error policy; its sibling above it does.
		return ""
	default:
		return contract.PolicyStrict
	}
}

// statusScan is the accumulated reading of one predicate over the status
// register.
type statusScan struct {
	// references is how many status-register occurrences the predicate
	// contains. Zero means it is not a status guard.
	references int
	// excludesBenign records `SQLCODE != NO_DATA_FOUND` — the guard
	// explicitly discounting a benign code. This is the tolerant spelling.
	excludesBenign bool
	// benignSuccessArm records `SQLCODE == NO_DATA_FOUND` — the predicate
	// testing for empty-result directly. It states no failure contract.
	benignSuccessArm bool
	// hasBusinessOperand records a non-status term somewhere in the
	// predicate. It separates the pure `== NO_DATA_FOUND` success arm from a
	// no-rows test fused with business logic — both declare no failure
	// contract, but only one is a guard's success arm.
	hasBusinessOperand bool
	// mixedWithBusiness records the status register appearing inside a
	// boolean connective alongside a non-status term, which makes the
	// predicate a business test that happens to mention a status code.
	mixedWithBusiness bool
}

// scanStatusPredicate walks e collecting everything the verdict needs in one
// pass. It reports whether the predicate references the status register at
// all.
//
// The mixedWithBusiness rule is structural rather than textual: a status
// comparison is a leaf of the boolean tree, and if it shares an Or node with
// anything that is not a status comparison, the predicate is not a pure error
// guard. That distinguishes `SQLCODE != 0 && SQLCODE != NO_DATA_FOUND`
// (both leaves are status comparisons under And) from
// `SQLCODE == NO_DATA_FOUND || sql_x == 'N'` (an Or whose other operand is a
// business comparison) without listing either spelling.
func scanStatusPredicate(e *pred.Expr) (statusScan, bool) {
	var s statusScan
	var walk func(x *pred.Expr, mixedByOr bool)
	walk = func(x *pred.Expr, mixedByOr bool) {
		if x == nil {
			return
		}
		switch x.Kind {
		case pred.KindOr:
			// An Or chooses between alternatives. When one of them is the
			// status register, the predicate is a business choice that
			// happens to test a status code — including the degenerate case
			// of a disjunction over two status comparisons, which is a
			// tautology rather than a guard. Either way it declares no
			// single failure contract, so it is counted, not guessed.
			before := s.references
			for i := range x.Items {
				walk(&x.Items[i], true)
			}
			if s.references > before {
				s.mixedWithBusiness = true
			}
		case pred.KindAnd:
			for i := range x.Items {
				walk(&x.Items[i], mixedByOr)
			}
		case pred.KindNot:
			walk(x.Inner, true)
		case pred.KindCmp:
			statusSide := statusOperand(x.L) != nil || statusOperand(x.R) != nil
			if !statusSide {
				s.hasBusinessOperand = true
				if mixedByOr {
					s.mixedWithBusiness = true
				}
				return
			}
			s.references += countStatusOperands(x)
			if isBenign(x.R) || isBenign(x.L) {
				if x.Op == "!=" {
					s.excludesBenign = true
				} else if x.Op == "==" {
					s.benignSuccessArm = true
				}
			}
			// `SQLCODE != 0` is the guard's own "any failure" test. It says
			// nothing about empty results either way, so it only marks the
			// predicate as a status guard.
		case pred.KindIdent, pred.KindCall:
			if statusOperand(x) != nil {
				s.references++
				if mixedByOr {
					// A bare status register OR-ed with business logic.
					s.mixedWithBusiness = true
				}
				return
			}
			s.hasBusinessOperand = true
			if mixedByOr {
				s.mixedWithBusiness = true
			}
		case pred.KindRaw:
			// pred degrades unparseable text to Raw. A Raw node mentioning
			// the status register is a guard we cannot read structurally, and
			// must not pass for "not a guard": treating it as absent would
			// drop it out of the census entirely, which is the silent
			// failure this whole type exists to prevent. Count it instead.
			if mentionsStatusRegister(x.Text) {
				s.references++
				s.mixedWithBusiness = true
				s.hasBusinessOperand = true
			}
		}
	}
	walk(e, false)
	return s, s.references > 0
}

// mentionsStatusRegister reports whether raw predicate text names the status
// register. Used only for pred.Raw nodes, where structure is unavailable and a
// text test is the best available signal.
func mentionsStatusRegister(text string) bool {
	u := upperASCIIText(text)
	for _, r := range contract.StatusRegisters {
		if containsToken(u, r) {
			return true
		}
	}
	return false
}

// upperASCIIText uppercases ASCII letters, matching contract.upperASCII's fold
// so a register matches here exactly when it matches there.
func upperASCIIText(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// containsToken reports whether needle appears in s delimited by non-identifier
// bytes on both sides, so "SQLCODE" matches "SQLCODE != 0" but not "MYCODE".
func containsToken(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] != needle {
			continue
		}
		if i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		if i+len(needle) < len(s) && isIdentByte(s[i+len(needle)]) {
			continue
		}
		return true
	}
	return false
}

// ParseGuardPolicy classifies raw condition text, degrading a nil or blank
// condition to "not a guard". It is the convenience entry point for callers
// holding a scanner Branch fact rather than an already-parsed tree.
func ParseGuardPolicy(cond string) contract.ErrorPolicy {
	if strings.TrimSpace(cond) == "" {
		return ""
	}
	e := pred.Parse(cond)
	return ErrorPolicyOf(&e)
}

// IsBenignStatusCond reports whether a condition is the pure
// `SQLCODE == NO_DATA_FOUND` shape — the success arm of a tolerant guard, or a
// fetch-loop break.
//
// It requires the predicate to contain nothing but the status test. A no-rows
// test fused with business logic (`(SQLCODE == NO_DATA_FOUND) && c_flag == 'A'`)
// declares no failure contract either, but it is not a guard's success arm, and
// reporting it as one would put it in the same census bucket as the 74 real
// arms. Use StatusReferencesWithoutPolicy to count that shape instead.
func IsBenignStatusCond(cond string) bool {
	e := pred.Parse(cond)
	s, ok := scanStatusPredicate(&e)
	return ok && s.benignSuccessArm && !s.excludesBenign &&
		!s.hasBusinessOperand && !s.mixedWithBusiness
}

// StatusReferencesWithoutPolicy reports whether a condition mentions the status
// register yet declares no error policy of its own. These are the no-rows
// branches: success arms, fetch-loop breaks, and no-rows tests fused with
// business logic. They are counted separately from real error guards so that
// "every guard was read" can be asserted without conflating a guard with the
// business branch next to it.
func StatusReferencesWithoutPolicy(cond string) bool {
	if ParseGuardPolicy(cond).IsSet() {
		return false
	}
	e := pred.Parse(cond)
	_, ok := scanStatusPredicate(&e)
	return ok
}

// Guard is one status guard paired with the query it follows. It carries the
// classification plus the two facts the guard's body contributed: the errlog
// code an operator would have seen, and whether the body does compensating
// work (rollback / cursor close / abort) that the emitters otherwise drop
// without saying so.
type Guard struct {
	Line        int                  `json:"line"`
	EndLine     int                  `json:"end_line,omitempty"`
	Cond        string               `json:"cond"`
	Policy      contract.ErrorPolicy `json:"policy"`
	ErrCode     string               `json:"err_code,omitempty"`
	Compensates bool                 `json:"compensates,omitempty"`
	Benign      bool                 `json:"benign,omitempty"` // a `== NO_DATA_FOUND` success arm
}

// GuardAfter finds the status guard that immediately follows a query and
// classifies it.
//
// Pairing is by adjacency: the nearest branch starting after the query's last
// line, within guardWindow lines, that declares a policy (or is a benign arm).
// A query's guard is written directly under the statement, so anything further
// away belongs to a different statement — and pairing across that gap would
// attribute one query's contract to another, which is worse than having none.
//
// A guard is reported once per call. A query with several call sites may have
// several guards; each site is resolved separately by the caller, and a site
// with no guard yields no verdict rather than an inherited one.
func GuardAfter(facts *tsscan.SourceFacts, q *ir.Query) (Guard, bool) {
	if facts == nil || q == nil {
		return Guard{}, false
	}
	best := -1
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.StartLine <= q.EndLine || b.StartLine > q.EndLine+guardWindow {
			continue
		}
		if q.OwningFunction != "" && b.Function != q.OwningFunction {
			continue // never attribute a guard from another function
		}
		if !mentionsStatusRegister(upperASCIIText(b.Cond)) {
			continue
		}
		if best < 0 || b.StartLine < facts.Branches[best].StartLine {
			best = i
		}
	}
	if best < 0 {
		return Guard{}, false
	}
	b := &facts.Branches[best]
	g := Guard{
		Line:    b.StartLine,
		EndLine: b.BlockEnd,
		Cond:    b.Cond,
		Policy:  ParseGuardPolicy(b.Cond),
		Benign:  IsBenignStatusCond(b.Cond),
	}
	g.ErrCode, g.Compensates = guardBodyFacts(facts, b)
	return g, true
}

// guardWindow bounds how far below a query a guard may sit and still be its
// guard. The corpus puts the branch on the very next line; the slack absorbs
// a blank line or a comment block and nothing more.
const guardWindow = 3

// guardBodyFacts reads the guard's block for the errlog code and the
// compensating work it performs. Both are facts about the block's span, which
// is why they are gathered here rather than by the emitters: the emitters
// never see the legacy body.
func guardBodyFacts(facts *tsscan.SourceFacts, b *tsscan.Branch) (code string, compensates bool) {
	for i := range facts.Calls {
		c := &facts.Calls[i]
		if c.Line <= b.StartLine {
			continue
		}
		if b.BlockEnd > 0 && c.Line > b.BlockEnd {
			break
		}
		switch c.Name {
		case "errlog":
			if code == "" {
				code = errlogCode(c.Args)
			}
		case "tpabort", "tpterm", "exit", "tpclose":
			compensates = true
		}
	}
	return code, compensates
}

// errlogCode extracts the message id from an errlog argument list. errlog's
// second argument is the code — a literal "S3nnnn" message id in the legacy
// source. The code is returned as read; nothing about a particular customer's
// error catalogue is written into this package.
func errlogCode(args string) string {
	fields := splitTopLevelArgs(args)
	if len(fields) < 2 {
		return ""
	}
	s := strings.TrimSpace(fields[1])
	s = strings.Trim(s, `"'`)
	if s == "" {
		return ""
	}
	return s
}

// splitTopLevelArgs splits a C argument list on commas that are not nested in
// parens, brackets or quotes.
func splitTopLevelArgs(args string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(args); i++ {
		c := args[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, args[start:i])
			start = i + 1
		}
	}
	out = append(out, args[start:])
	return out
}

// statusOperand returns e when it names the status register, nil otherwise.
func statusOperand(e *pred.Expr) *pred.Expr {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case pred.KindIdent:
		if contract.IsStatusRegister(e.Name) {
			return e
		}
	case pred.KindCall:
		// `sqlca.sqlerr(...)` is not a thing; a Call naming the register
		// (`SQLCODE(...)`) is. Kept narrow so a call that merely mentions
		// the word does not count.
		if contract.IsStatusRegister(e.Name) {
			return e
		}
	}
	return nil
}

// countStatusOperands counts how many sides of a comparison name the status
// register, so `SQLCODE != SQLERR` counts 2 and `SQLCODE != 0` counts 1.
func countStatusOperands(x *pred.Expr) int {
	n := 0
	if statusOperand(x.L) != nil {
		n++
	}
	if statusOperand(x.R) != nil {
		n++
	}
	return n
}

// isBenign reports whether e names a status code that means empty-result
// rather than failure (contract.BenignStatusCodes).
func isBenign(e *pred.Expr) bool {
	if e == nil {
		return false
	}
	if e.Kind == pred.KindIdent {
		return contract.BenignStatus(e.Name)
	}
	if e.Kind == pred.KindLit {
		return contract.BenignStatus(unquoteLit(e.Text))
	}
	return false
}

// unquoteLit strips the quotes and a trailing L/suffix a C literal may carry,
// so `"NO_DATA_FOUND"`, `'NO_DATA_FOUND'` and `NO_DATA_FOUND` compare alike.
func unquoteLit(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\'' {
			continue
		}
		if c == 'L' || c == 'l' {
			// Only strip a length prefix when it precedes a literal, never
			// from the middle of a name like "SELL".
			if i > 0 && (s[i-1] == '"' || s[i-1] == '\'') {
				continue
			}
		}
		out = append(out, c)
	}
	return string(out)
}
