package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/token"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/llm"
)

// aiSuggestion is the per-candidate naming proposal (PRD-2026-09-10
// endpoint discovery, user directive: AI-decided draft defaults with a
// deterministic fallback). The draft stays the user's decision — every
// value is advisory and editable. Deterministic marks the no-model
// fallback: names picked from the strongest semantic token source
// available (cursor name → response fields → condition index).
type aiSuggestion struct {
	Name          string
	Route         string
	Methods       map[string]methodPinSuggestion // query ID → pin proposal
	Deterministic bool
}

// methodPinSuggestion proposes a store method name (+ row struct for
// selects). Params are never proposed — the IR derives them from the query
// binds deterministically.
type methodPinSuggestion struct {
	Name string
	Row  string
}

const aiNameSystem = `You name API endpoints and database store methods when converting legacy Tuxedo Pro*C services to idiomatic Go.
Respond ONLY with one JSON object — no prose, no markdown fences:
{"name":"<exported Go method name>","route":"/kebab-or-legacy-route","dbMethods":[{"id":"<query id>","name":"<exported Go method name>","row":"<exported row struct name, empty for non-select queries>"}]}
Rules:
- name/route describe what the endpoint does for its caller; dbMethods names describe what each query reads or writes.
- All names are concise CamelCase Go identifiers (no underscores, no abbreviations you cannot justify from the code).
- One dbMethods entry per query id listed, same id, nothing invented.
- row is filled only for SELECT queries (the row struct the query's columns map to).`

// aiNameEndpoints proposes draft names for every candidate. With a client:
// one LLM call per candidate endpoint carrying the branch source, its FML
// reads/writes, and the involved query SQL — each attempt (prompt + raw
// response + parse outcome) lands in the run's audit trail when rec is set.
// Without a client (or per-candidate on failure): the deterministic picker.
// The census and draft emission never depend on the LLM.
func aiNameEndpoints(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, tree *flow.Tree, candidates []flow.Candidate, src []byte, rec *audit.Recorder) map[string]aiSuggestion {
	out := make(map[string]aiSuggestion, len(candidates))
	queriesByID := make(map[string]*ir.Query, len(f.Queries))
	for _, q := range f.Queries {
		queriesByID[q.ID] = q
	}
	// The distinguishing token is the condition's inventory index: stable
	// across runs and natural as a name suffix.
	keys := make([]string, 0, len(candidates))
	tokens := make(map[string]string, len(candidates))
	for _, c := range candidates {
		keys = append(keys, c.Key)
		tokens[c.Key] = strings.SplitN(strings.TrimPrefix(c.Key, "c"), ".", 2)[0]
	}
	if client == nil {
		for _, c := range candidates {
			out[c.Key] = deterministicSuggestion(c.QueryIDs, c.Adds, c.Gets, "Endpoint"+tokens[c.Key])
		}
	} else {
		lines := strings.Split(string(src), "\n")
		for _, c := range candidates {
			cond, err := flow.ConditionFor(tree, f.Conditions, c.Key)
			if err != nil {
				out[c.Key] = deterministicSuggestion(c.QueryIDs, c.Adds, c.Gets, "Endpoint"+tokens[c.Key])
				continue
			}
			sug, err := aiNameOne(ctx, log, client, b, f, cond, queriesByID, lines, rec)
			if err != nil {
				log.Warn("ai naming failed — deterministic names used", "candidate", c.Key, "error", err)
				out[c.Key] = deterministicSuggestion(c.QueryIDs, c.Adds, c.Gets, "Endpoint"+tokens[c.Key])
				continue
			}
			out[c.Key] = sug
		}
	}
	// Loader-legal defaults: mapping names must be unique (plan.Validate
	// rejects duplicates), so a repeated proposal — identical census shapes
	// often are — gains the condition index, then a numeric suffix.
	// Advisory still, always editable.
	dedupeEndpointNames(out, keys, func(key string) string { return tokens[key] }, log)
	return out
}

// aiNameScenarios proposes draft names for the entry's scenario slices
// (SCEN-5): with a client, one call per scenario carrying its census, the
// involved query SQL, and the unique-block excerpt (the per-transaction
// logic — the distinguishing naming signal); deterministic otherwise. The
// census and draft emission never depend on the LLM.
func aiNameScenarios(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, scens []*flow.Scenario, diff *flow.ScenarioDiff, src []byte, rec *audit.Recorder) map[string]aiSuggestion {
	out := make(map[string]aiSuggestion, len(scens))
	queriesByID := make(map[string]*ir.Query, len(f.Queries))
	for _, q := range f.Queries {
		queriesByID[q.ID] = q
	}
	lines := strings.Split(string(src), "\n")
	for _, sc := range scens {
		out[sc.Key] = deterministicSuggestion(scenarioQueryIDs(sc), sc.Adds, sc.Gets, "Endpoint"+common.CamelGo(sc.Value))
	}
	if client != nil {
		for _, sc := range scens {
			sug, err := aiNameScenarioOne(ctx, log, client, b, f, sc, queriesByID, lines, diff, rec)
			if err != nil {
				log.Warn("ai scenario naming failed — deterministic names used", "scenario", sc.Key, "error", err)
				continue
			}
			out[sc.Key] = sug
		}
	}
	// Loader-legal defaults: mapping names must be unique, so a repeated
	// proposal (identical census shapes often are) gains the axis value,
	// then a numeric suffix — advisory still, always editable.
	keys := make([]string, 0, len(scens))
	tokens := make(map[string]string, len(scens))
	for _, sc := range scens {
		keys = append(keys, sc.Key)
		tokens[sc.Key] = common.CamelGo(sc.Value)
	}
	dedupeEndpointNames(out, keys, func(key string) string { return tokens[key] }, log)
	dedupeRowNames(out, queriesByID, log)
	return out
}

// dedupeRowNames clears a suggested row name that another query already
// claimed with a different shape — two generated structs under one name do
// not compile. The cleared pin falls back to the deterministic profile row
// name (derived from the unique method name); identical shapes may share the
// name (gen emits one struct). Sorted iteration keeps the outcome
// deterministic.
func dedupeRowNames(sugs map[string]aiSuggestion, queriesByID map[string]*ir.Query, log *slog.Logger) {
	shape := map[string]string{} // row name → shape fingerprint
	owner := map[string]string{} // row name → first query id
	keys := make([]string, 0, len(sugs))
	for k := range sugs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sug := sugs[k]
		ids := make([]string, 0, len(sug.Methods))
		for id := range sug.Methods {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			pin := sug.Methods[id]
			if pin.Row == "" {
				continue
			}
			q := queriesByID[id]
			if q == nil {
				continue
			}
			fp := strings.Join(q.RowShape, "|")
			if prev, ok := shape[pin.Row]; ok {
				if prev != fp {
					log.Warn("row name collision — deterministic row name used",
						"row", pin.Row, "query", id, "claimed_by", owner[pin.Row])
					pin.Row = ""
					sug.Methods[id] = pin
				}
				continue
			}
			shape[pin.Row] = fp
			owner[pin.Row] = id
		}
		sugs[k] = sug
	}
}

// uniqueEndpointName returns a name unique within taken: base when free,
// else base+token (the distinguishing scenario value / condition index),
// else the first free numeric suffix. Every returned name is registered in
// taken, so a collision rename can never collide with a later entry — the
// cascade the first cut missed. The empty name is the draft's TODO
// placeholder, not a proposal: it stays empty and unregistered.
func uniqueEndpointName(base, token string, taken map[string]bool) string {
	if base == "" {
		return ""
	}
	if !taken[base] {
		taken[base] = true
		return base
	}
	if token != "" && !taken[base+token] {
		taken[base+token] = true
		return base + token
	}
	for i := 2; ; i++ {
		cand := base + strconv.Itoa(i)
		if !taken[cand] {
			taken[cand] = true
			return cand
		}
	}
}

// dedupeEndpointNames makes every suggestion's name unique over order —
// plan.Validate rejects duplicates ("endpoint name %q used twice"), and the
// name is the generated Go method. token returns the entry's distinguishing
// suffix; the map is walked in order so the first proposal keeps its name
// and later collisions gain the suffix, then a number. Empty names (the
// TODO placeholder) pass through untouched, untouched suggestions are not
// rewritten.
func dedupeEndpointNames(out map[string]aiSuggestion, order []string, token func(string) string, log *slog.Logger) {
	taken := map[string]bool{}
	for _, key := range order {
		sug := out[key]
		name := uniqueEndpointName(sug.Name, token(key), taken)
		if name == sug.Name {
			continue
		}
		log.Debug("endpoint name collision — distinguishing suffix added",
			"key", key, "name", sug.Name, "kept", name)
		sug.Name = name
		out[key] = sug
	}
}

// scenarioQueryIDs lists the scenario's census query ids.
func scenarioQueryIDs(sc *flow.Scenario) []string {
	out := make([]string, 0, len(sc.Queries))
	for _, q := range sc.Queries {
		out = append(out, q.ID)
	}
	return out
}

// aiNameScenarioOne is the per-scenario naming call: census + query SQL +
// the scenario's unique blocks (clipped — the per-transaction logic is the
// naming signal; shared init is identical across scenarios and carries
// none). Advisory seam: bounded retries, audit-traced, deterministic fallback.
func aiNameScenarioOne(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, sc *flow.Scenario, queriesByID map[string]*ir.Query, lines []string, diff *flow.ScenarioDiff, rec *audit.Recorder) (aiSuggestion, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Scenario %s of the %s dispatch axis (the legacy entry folds into one slice per dispatch value; contradicted branches are already removed).\n", sc.Key, sc.Var)
	if sc.Default {
		sb.WriteString("This slice is the dispatch chain's DEFAULT arm — the legacy else: it runs when the dispatch value matches none of the other slices' values.\n")
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "Request fields read: %s\n", strings.Join(sc.Gets, ", "))
	fmt.Fprintf(&sb, "Response fields written: %s\n", strings.Join(sc.Adds, ", "))
	if len(sc.TxSpans) > 0 {
		var txq []string
		for _, q := range sc.Queries {
			if q.DML && q.Tx {
				txq = append(txq, q.ID)
			}
		}
		if len(txq) > 0 {
			fmt.Fprintf(&sb, "Transactional queries (live begin→commit spans): %s\n", strings.Join(txq, ", "))
		}
	}
	sb.WriteString("\nQueries (id — kind — SQL):\n")
	const maxQuerySQL = 25
	listed := 0
	for _, qy := range sc.Queries {
		q := queriesByID[qy.ID]
		if q == nil {
			continue
		}
		if listed == maxQuerySQL {
			fmt.Fprintf(&sb, "... and %d more (census carries the full list)\n", len(sc.Queries)-listed)
			break
		}
		listed++
		fmt.Fprintf(&sb, "%s (%s): %s\n", q.ID, q.Type, oneLine(q.SQL))
	}
	if diff != nil {
		if excerpt, n := uniqueExcerpt(diff.Unique[sc.Key], lines); n > 0 {
			fmt.Fprintf(&sb, "\nThis scenario's unique logic (%d lines of %d total):\n\n%s\n", n, excerptLines(diff.Unique[sc.Key]), excerpt)
		}
	}
	prompt := sb.String()

	content, _, _, err := llm.RunSeam(ctx, llm.SeamInput{
		Unit: f.Entry, Kind: "discover", Name: strings.ReplaceAll(sc.Key, "=", "_"),
		Audit: rec, Client: client, Budget: b,
		MaxRetries:  1,
		Temperature: 0.2,
		Prompt: func(_ string, _ []string) (string, []llm.Message) {
			return prompt, []llm.Message{{Role: "system", Content: aiNameSystem}, {Role: "user", Content: prompt}}
		},
		Gate: func(content string) []string {
			if _, perr := parseNameJSON(content, scenarioQueryIDs(sc)); perr != nil {
				return []string{perr.Error()}
			}
			return nil
		},
	})
	if err != nil {
		return aiSuggestion{}, err
	}
	sug, perr := parseNameJSON(content, scenarioQueryIDs(sc))
	if perr != nil {
		return aiSuggestion{}, perr // unreachable: the gate validated this content
	}
	log.Debug("ai scenario naming proposal", "scenario", sc.Key, "name", sug.Name)
	return sug, nil
}

// uniqueExcerpt renders the source lines of a scenario's unique blocks,
// clipped to the naming prompt's budget (150 lines — advisory naming needs
// the distinguishing logic, not the whole slice). Returns the excerpt and
// the line count it carries.
func uniqueExcerpt(blocks []flow.LineBlock, lines []string) (string, int) {
	const maxLines = 150
	var sb strings.Builder
	n := 0
	for _, blk := range blocks {
		if n >= maxLines {
			break
		}
		for l := blk.Start; l <= blk.End && n < maxLines; l++ {
			if l < 1 || l > len(lines) {
				continue
			}
			sb.WriteString(lines[l-1])
			sb.WriteString("\n")
			n++
		}
	}
	return sb.String(), n
}

// excerptLines totals the line count of a block list.
func excerptLines(blocks []flow.LineBlock) int {
	n := 0
	for _, b := range blocks {
		n += b.End - b.Start + 1
	}
	return n
}

// deterministicSuggestion picks draft defaults without any model: the best
// semantic token source wins — a cursor query's name (cur_px_item_hist →
// GetPxItemHist), then the first response field (FML_PX_ITEM_DATE →
// GetPxItemDate), then the first read, then the fallback key. The one home
// for census-shaped naming: candidates and scenario slices feed it alike.
func deterministicSuggestion(queryIDs, adds, gets []string, fallback string) aiSuggestion {
	name := ""
	for _, id := range queryIDs {
		if len(id) > 4 && strings.EqualFold(id[:4], "cur_") {
			name = "Get" + common.CamelGo(id[4:])
			break
		}
	}
	if name == "" {
		for _, src := range [][]string{adds, gets} {
			for _, fld := range src {
				fld = strings.ToUpper(fld)
				fld = strings.TrimPrefix(fld, "FML_")
				if camel := common.CamelGo(fld); camel != "" {
					name = "Get" + camel
					break
				}
			}
			if name != "" {
				break
			}
		}
	}
	if name == "" {
		name = fallback
	}
	sug := aiSuggestion{Name: name, Route: "/" + kebabName(name), Deterministic: true}
	sug.Methods = map[string]methodPinSuggestion{}
	return sug
}

// kebabName renders a Go name as a route segment ("GetPxItemHist" →
// "px-item-hist").
func kebabName(name string) string {
	var sb strings.Builder
	for i, r := range name {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				sb.WriteByte('-')
			}
			sb.WriteRune(r - 'A' + 'a')
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func aiNameOne(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, cond *ir.Condition, queriesByID map[string]*ir.Query, lines []string, rec *audit.Recorder) (aiSuggestion, error) {
	var sb strings.Builder
	sb.WriteString("Endpoint branch (legacy C):\n\n")
	sb.WriteString(branchSlice(lines, cond.StartLine, cond.EndLine) + "\n\n")
	var gets, adds []string
	for _, op := range cond.FmlOps {
		switch op.Kind {
		case ir.FmlGet:
			gets = append(gets, op.Field)
		case ir.FmlAdd:
			if !op.Error {
				adds = append(adds, op.Field)
			}
		}
	}
	fmt.Fprintf(&sb, "Request fields read: %s\n", strings.Join(gets, ", "))
	fmt.Fprintf(&sb, "Response fields written: %s\n", strings.Join(adds, ", "))
	sb.WriteString("\nQueries (id — kind — SQL):\n")
	for _, id := range cond.QueryIDs {
		q := queriesByID[id]
		if q == nil {
			continue
		}
		fmt.Fprintf(&sb, "%s (%s): %s\n", id, q.Type, oneLine(q.SQL))
	}
	prompt := sb.String()

	// Two attempts (MaxRetries 1), the advisory-seam posture: chat and gate
	// failures retry, and the deterministic picker takes over on exhaustion.
	content, _, _, err := llm.RunSeam(ctx, llm.SeamInput{
		Unit: f.Entry, Kind: "discover", Name: fmt.Sprintf("c%d", cond.Index),
		Audit: rec, Client: client, Budget: b,
		MaxRetries:  1,
		Temperature: 0.2,
		Prompt: func(_ string, _ []string) (string, []llm.Message) {
			return prompt, []llm.Message{{Role: "system", Content: aiNameSystem}, {Role: "user", Content: prompt}}
		},
		Gate: func(content string) []string {
			if _, perr := parseNameJSON(content, cond.QueryIDs); perr != nil {
				return []string{perr.Error()}
			}
			return nil
		},
	})
	if err != nil {
		return aiSuggestion{}, err
	}
	sug, perr := parseNameJSON(content, cond.QueryIDs)
	if perr != nil {
		return aiSuggestion{}, perr // unreachable: the gate validated this content
	}
	log.Debug("ai naming proposal", "condition", cond.Index, "name", sug.Name)
	return sug, nil
}

// parseNameJSON extracts the JSON object from the model output (fence- or
// prose-tolerant via llm.JSONObject) and validates every field —
// identifiers must be valid Go identifiers, routes must start with /, and
// only query ids the branch actually references are accepted.
func parseNameJSON(content string, allowedQueryIDs []string) (aiSuggestion, error) {
	obj := llm.JSONObject(content)
	if obj == "" {
		return aiSuggestion{}, fmt.Errorf("no JSON object in the response")
	}
	var raw struct {
		Name      string `json:"name"`
		Route     string `json:"route"`
		DBMethods []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Row  string `json:"row"`
		} `json:"dbMethods"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return aiSuggestion{}, fmt.Errorf("parsing naming JSON: %w", err)
	}
	sug := aiSuggestion{Methods: map[string]methodPinSuggestion{}}
	if token.IsIdentifier(raw.Name) && token.IsExported(raw.Name) {
		sug.Name = raw.Name
	}
	if strings.HasPrefix(raw.Route, "/") && len(raw.Route) > 1 {
		sug.Route = raw.Route
	}
	allowed := map[string]bool{}
	for _, id := range allowedQueryIDs {
		allowed[id] = true
	}
	for _, m := range raw.DBMethods {
		if !allowed[m.ID] || !token.IsIdentifier(m.Name) || !token.IsExported(m.Name) {
			continue
		}
		pin := methodPinSuggestion{Name: m.Name}
		if m.Row != "" && token.IsIdentifier(m.Row) && token.IsExported(m.Row) {
			pin.Row = m.Row
		}
		sug.Methods[m.ID] = pin
	}
	return sug, nil
}

// branchSlice slices the 1-based inclusive line range out of the source.
func branchSlice(lines []string, from, to int) string {
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
