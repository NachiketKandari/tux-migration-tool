package sqlchk

import (
	"strconv"
	"strings"
)

type tokKind int

const (
	tokWord   tokKind = iota // keyword / unquoted identifier, lowercased
	tokStr                   // '...' string literal, verbatim
	tokQIdent                // "..." quoted identifier, verbatim
	tokBind                  // :name → canonical :<first-appearance ordinal>
	tokNum                   // numeric literal
	tokPunct                 // operator / punctuation
)

// sqlToken is one normalized SQL token; Raw keeps a bind's original spelling
// (Text carries the canonical ordinal) so named-vs-positional comparison can
// pick the right projection.
type sqlToken struct {
	Kind tokKind
	Text string
	Raw  string
}

var (
	clauseWords = map[string]bool{
		"where": true, "group": true, "order": true, "having": true,
		"set": true, "values": true, "for": true, "connect": true,
		"start": true, "minus": true, "union": true, "intersect": true,
	}
	joinWords = map[string]bool{
		"inner": true, "left": true, "right": true, "full": true, "cross": true,
		"natural": true, "join": true, "on": true, "using": true, "as": true,
	}
)

func isStopWord(t sqlToken) bool {
	return t.Kind == tokWord && (clauseWords[t.Text] || joinWords[t.Text])
}

// tokenize splits SQL into normalized tokens: keywords and unquoted
// identifiers case-folded, binds rewritten to first-appearance ordinals (so
// positional and named styles become the same canonical sequence), string
// literals and quoted identifiers verbatim, comments dropped. A bind may
// carry Pro*C's spaced spelling (`: name`) — the name is what compares.
func tokenize(sql string) []sqlToken {
	var toks []sqlToken
	bindOrd := map[string]int{}
	next := 1
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 4
			}
		case c == '\'':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			toks = append(toks, sqlToken{Kind: tokStr, Text: sql[i:j]})
			i = j
		case c == '"':
			j := strings.IndexByte(sql[i+1:], '"')
			if j < 0 {
				toks = append(toks, sqlToken{Kind: tokQIdent, Text: sql[i:]})
				i = len(sql)
			} else {
				toks = append(toks, sqlToken{Kind: tokQIdent, Text: sql[i : i+j+2]})
				i += j + 2
			}
		case c == ':':
			// Pro*C tolerates whitespace between the colon and the host-var
			// name (`: c_from_date`); the canonical spelling is contiguous.
			j := i + 1
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
				j++
			}
			nameStart := j
			for j < len(sql) && isWordChar(sql[j]) {
				j++
			}
			if nameStart == j {
				// A colon with no name (`:=`, `::`, a stray `:`) is punctuation.
				toks = append(toks, sqlToken{Kind: tokPunct, Text: ":"})
				i++
				continue
			}
			name := sql[nameStart:j]
			ord, seen := bindOrd[name]
			if !seen {
				ord = next
				next++
				bindOrd[name] = ord
			}
			toks = append(toks, sqlToken{Kind: tokBind, Text: ":" + strconv.Itoa(ord), Raw: ":" + name})
			i = j
		default:
			switch {
			case isWordChar(c) && (c < '0' || c > '9'):
				j := i
				for j < len(sql) && isWordChar(sql[j]) {
					j++
				}
				toks = append(toks, sqlToken{Kind: tokWord, Text: strings.ToLower(sql[i:j])})
				i = j
			case c >= '0' && c <= '9':
				j := i
				for j < len(sql) && (isWordChar(sql[j]) || sql[j] == '.') {
					j++
				}
				toks = append(toks, sqlToken{Kind: tokNum, Text: sql[i:j]})
				i = j
			default:
				two := ""
				if i+1 < len(sql) {
					two = sql[i : i+2]
				}
				if two == "||" || two == "<=" || two == ">=" || two == "<>" || two == "!=" {
					toks = append(toks, sqlToken{Kind: tokPunct, Text: two})
					i += 2
				} else {
					toks = append(toks, sqlToken{Kind: tokPunct, Text: string(c)})
					i++
				}
			}
		}
	}
	return toks
}

func isWordChar(c byte) bool {
	return c == '_' || c == '$' || c == '#' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// normalizeSQL tokenizes and canonicalizes table aliases: `FROM DEMO_PRICE p`
// rewrites every later bare `p` to the table, so a renamed alias disappears
// from the comparison while a renamed table still flags (PF-6.2). Pro*C
// host-variable INTO lists (`SELECT ... INTO :a, :b FROM`) are dropped from
// both sides — the INTO clause is a Pro*C construct, not SQL (BP-8: the
// python target strips it for oracledb executability, so it is tolerance,
// never a deviation).
func normalizeSQL(sql string) []sqlToken {
	toks := dropIntoLists(tokenize(sql))
	aliases := aliasMap(toks)
	if len(aliases) == 0 {
		return toks
	}
	out := make([]sqlToken, len(toks))
	for i, t := range toks {
		if t.Kind == tokWord {
			if table, ok := aliases[t.Text]; ok {
				t.Text = table
			}
		}
		out[i] = t
	}
	return out
}

// dropIntoLists removes `INTO :a, :b, …` spans (an INTO followed by bind
// tokens and commas, ending at the first non-bind token). `INSERT INTO t`
// (INTO followed by a word) is untouched.
func dropIntoLists(toks []sqlToken) []sqlToken {
	var out []sqlToken
	for i := 0; i < len(toks); i++ {
		if toks[i].Kind == tokWord && toks[i].Text == "into" && i+1 < len(toks) && toks[i+1].Kind == tokBind {
			j := i + 1
			for j < len(toks) && (toks[j].Kind == tokBind || (toks[j].Kind == tokPunct && toks[j].Text == ",")) {
				j++
			}
			i = j - 1
			continue
		}
		out = append(out, toks[i])
	}
	return out
}

// aliasMap finds table aliases after from/join/update — `T alias`,
// `T AS alias` — and across comma-separated FROM items (PF-6.2). Aliases are
// captured anywhere the trigger word appears; identical inputs produce
// identical maps, so the rewrite stays comparison-symmetric.
func aliasMap(toks []sqlToken) map[string]string {
	aliases := map[string]string{}
	for i := 0; i < len(toks); i++ {
		if toks[i].Kind == tokWord && (toks[i].Text == "from" || toks[i].Text == "join" || toks[i].Text == "update") {
			captureAlias(toks, i+1, aliases)
		}
	}
	return aliases
}

// captureAlias reads `table [AS] alias` pairs from pos, continuing across
// comma separators; a missing alias (next token a clause keyword or
// punctuation) or a clause keyword itself ends the capture.
func captureAlias(toks []sqlToken, pos int, aliases map[string]string) {
	i := pos
	for i < len(toks) {
		if toks[i].Kind != tokWord || isStopWord(toks[i]) {
			return
		}
		table := toks[i].Text
		j := i + 1
		if j+1 < len(toks) && toks[j].Kind == tokWord && toks[j].Text == "as" && toks[j+1].Kind == tokWord {
			j++
		}
		if j < len(toks) && toks[j].Kind == tokWord && !isStopWord(toks[j]) {
			if toks[j].Text != table {
				aliases[toks[j].Text] = table
			}
			i = j + 1
		} else {
			i = j
		}
		if i < len(toks) && toks[i].Kind == tokPunct && toks[i].Text == "," {
			i++
			continue
		}
		return
	}
}
