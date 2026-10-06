// Package cschk is the structural + SQL-fidelity gate for the convertcs
// output: per-file brace/namespace/type checks, raw-SQL leak detection
// outside the NamedQueries file, and a normalized comparison of every
// NamedQueries const against the plan's source SQL (comment-stripped,
// whitespace-collapsed, bind names wildcarded). Deviations are loud —
// never silently normalized away.
package cschk

import (
	"fmt"
	"regexp"
	"strings"

	"os"

	"tux-migration-tool/internal/csplan"
)

// Issue is one gate finding.
type Issue struct {
	File   string `json:"file"`
	Kind   string `json:"kind"` // braces | namespace | type | sql-leak | sql-deviation | oracle-param
	Detail string `json:"detail"`
}

func (i Issue) Error() string { return fmt.Sprintf("%s: %s: %s", i.File, i.Kind, i.Detail) }

// declaredNameRe captures the declared identifier of a member-style
// declaration (`public string 17DIM_VAL { … }`, `private int n;`). The
// capture is permissive on purpose — the gate's job is to catch the names
// C# cannot declare, not to parse C#.
var declaredNameRe = regexp.MustCompile(`\b(?:public|private|protected|internal)\s+\w+\s+([A-Za-z0-9_]+)\s*[;{=]`)

// validIdentRe is the C# identifier shape: a letter or underscore first.
var validIdentRe = regexp.MustCompile(`^[A-Za-z_]\w*$`)

// Check runs the per-file structural gates. typeName is the one type the
// file must declare ("" skips the check).
func Check(file, content, typeName string) []Issue {
	var issues []Issue
	if !balancedBraces(content) {
		issues = append(issues, Issue{File: file, Kind: "braces", Detail: "unbalanced braces"})
	}
	if !strings.Contains(content, "namespace ") {
		issues = append(issues, Issue{File: file, Kind: "namespace", Detail: "no namespace declaration"})
	}
	if typeName != "" && !strings.Contains(content, typeName) {
		issues = append(issues, Issue{File: file, Kind: "type", Detail: fmt.Sprintf("expected type %s not declared", typeName)})
	}
	// Declared identifiers must be C#-valid: a digit-leading name
	// (`public string 17DIM_VAL`) compiles nowhere — loud here, not at
	// the dotnet build.
	seen := map[string]bool{}
	for _, m := range declaredNameRe.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if validIdentRe.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		issues = append(issues, Issue{File: file, Kind: "type", Detail: fmt.Sprintf("declared identifier %q is not a valid C# identifier", name)})
	}
	// Raw SQL stays inside NamedQueries only — every other generated file
	// must be free of SELECT/INSERT/UPDATE/DELETE/MERGE statement heads.
	if !strings.HasSuffix(strings.ToLower(file), "queries.cs") {
		for _, kw := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "MERGE "} {
			if containsSQLHead(content, kw) {
				issues = append(issues, Issue{File: file, Kind: "sql-leak", Detail: "raw SQL outside NamedQueries (" + strings.TrimSpace(kw) + ")"})
			}
		}
	}
	return issues
}

// SQLFidelity compares every NamedQueries const against the plan's SQL:
// both sides comment-stripped, whitespace-collapsed, bind names
// wildcarded (`:x` → `:?`). Returns one issue per deviating const.
func SQLFidelity(p *csplan.Plan, files map[string]string) []Issue {
	content, ok := files["NamedQueries/"+p.QueriesCls+".cs"]
	if !ok {
		return []Issue{{File: "NamedQueries/" + p.QueriesCls + ".cs", Kind: "sql-deviation", Detail: "NamedQueries file missing"}}
	}
	var issues []Issue
	for _, qp := range p.Queries {
		extracted, err := extractConst(content, qp.Name)
		if err != nil {
			issues = append(issues, Issue{File: p.QueriesCls + ".cs", Kind: "sql-deviation", Detail: err.Error()})
			continue
		}
		want := normalizeSQL(qp.SQL)
		got := normalizeSQL(extracted)
		if got != want {
			detail := fmt.Sprintf("%s deviates from the source SQL (normalized)", qp.Name)
			if os.Getenv("CSCHK_DEBUG") == "1" {
				detail = fmt.Sprintf("%s deviates: got=%q want=%q", qp.Name, got, want)
			}
			issues = append(issues, Issue{
				File:   p.QueriesCls + ".cs",
				Kind:   "sql-deviation",
				Detail: detail,
			})
		}
	}
	return issues
}

// OracleParams checks each repo method's OracleParameter list against the
// plan's params (count must match — the render is deterministic, so a
// mismatch means template drift).
func OracleParams(p *csplan.Plan, files map[string]string) []Issue {
	content, ok := files["Repository/"+p.Repo+".cs"]
	if !ok {
		return []Issue{{File: "Repository/" + p.Repo + ".cs", Kind: "oracle-param", Detail: "repository file missing"}}
	}
	var issues []Issue
	qpByID := make(map[string]csplan.QueryPlan, len(p.Queries))
	for _, q := range p.Queries {
		qpByID[q.ID] = q
	}
	for _, ep := range p.Endpoints {
		for _, id := range ep.QueryIDs {
			qp, ok := qpByID[id]
			if !ok {
				continue
			}
			want := len(qp.Params)
			// count parameters inside this const's method: the method name
			// is the anchor (unique per endpoint+const pair)
			n := countOracleParams(content, qp)
			if n != want {
				issues = append(issues, Issue{
					File:   p.Repo + ".cs",
					Kind:   "oracle-param",
					Detail: fmt.Sprintf("%s carries %d OracleParameter(s), plan has %d", qp.Name, n, want),
				})
			}
		}
	}
	return issues
}

// sqlHeadRe matches an SQL statement head outside string literals —
// approximate but adequate for generated files (the SQL lives in the
// NamedQueries verbatim strings only).
var sqlHeadRe = regexp.MustCompile(`(?im)^\s*(SELECT|INSERT|UPDATE|DELETE|MERGE)\s`)

// sqlHeadByKw attributes each leak to its own keyword: one SQL head must
// yield one issue naming the statement actually seen, not five issues (the
// generic regex cannot tell which keyword matched — and the mislabeled
// strings feed the seam's retry notes verbatim).
var sqlHeadByKw = map[string]*regexp.Regexp{
	"SELECT ": regexp.MustCompile(`(?im)^\s*SELECT\s`),
	"INSERT ": regexp.MustCompile(`(?im)^\s*INSERT\s`),
	"UPDATE ": regexp.MustCompile(`(?im)^\s*UPDATE\s`),
	"DELETE ": regexp.MustCompile(`(?im)^\s*DELETE\s`),
	"MERGE ":  regexp.MustCompile(`(?im)^\s*MERGE\s`),
}

func containsSQLHead(content, kw string) bool {
	re, ok := sqlHeadByKw[kw]
	if !ok {
		return sqlHeadRe.MatchString(stripVerbatim(content))
	}
	return re.MatchString(stripVerbatim(content))
}

// stripVerbatim removes @"..."; blocks (non-greedy to the terminating
// `";`) so the NamedQueries-style literals never trip the SQL-head check.
func stripVerbatim(content string) string {
	var sb strings.Builder
	for {
		i := strings.Index(content, "@\"")
		if i < 0 {
			sb.WriteString(content)
			break
		}
		sb.WriteString(content[:i])
		rest := content[i+2:]
		j := strings.Index(rest, "\";")
		if j < 0 {
			break
		}
		content = rest[j+2:]
	}
	return sb.String()
}

// balancedBraces counts braces outside string literals (rough: verbatim
// blocks removed first, then quote-pairs dropped).
func balancedBraces(content string) bool {
	s := stripVerbatim(content)
	depth := 0
	inStr := false
	inLine := false
	inBlock := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			inBlock = true
			i++
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

var (
	constRe   = regexp.MustCompile(`(?s)public const string\s+(\w+)\s*=\s*@"(.*?)";`)
	bindRe    = regexp.MustCompile(`:[A-Za-z_]\w*`)
	commentRe = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
)

// extractConst pulls one const's verbatim SQL body.
func extractConst(content, name string) (string, error) {
	for _, m := range constRe.FindAllStringSubmatch(content, -1) {
		if m[1] == name {
			return m[2], nil
		}
	}
	return "", fmt.Errorf("const %s not found", name)
}

// normalizeSQL canonicalizes both sides for comparison.
func normalizeSQL(sql string) string {
	s := commentRe.ReplaceAllString(sql, " ")
	s = bindRe.ReplaceAllString(s, ":?")
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// countOracleParams counts the OracleParameter ctor calls carrying the
// query's bind names (binds are unique per query in practice and the
// render is deterministic).
// countOracleParams counts the OracleParameter constructions inside the
// repo method that calls the query's const. The count is method-scoped: a
// merged endpoint's queries can share a bind name (two PX_COMPANIES shapes
// both bind c_enable_d2u_flg), and a file-wide count would double.
func countOracleParams(content string, qp csplan.QueryPlan) int {
	idx := strings.Index(content, "."+qp.Name+",")
	if idx < 0 {
		idx = strings.Index(content, qp.Name)
	}
	if idx < 0 {
		return 0
	}
	start := strings.LastIndex(content[:idx], "public async Task")
	if start < 0 {
		start = 0
	}
	end := len(content)
	if next := strings.Index(content[idx:], "public async Task"); next >= 0 {
		end = idx + next
	}
	return strings.Count(content[start:end], "new OracleParameter(")
}
