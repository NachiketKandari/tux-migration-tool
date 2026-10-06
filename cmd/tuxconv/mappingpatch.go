package main

import (
	"regexp"
	"strconv"
	"strings"

	"tux-migration-tool/internal/plan"
)

// Mapping-yaml patching for the ainames pass. The drafts are hand-rendered
// text whose comments carry the census evidence and the provenance markers,
// and the loader runs KnownFields(true) — so a naming pass must rewrite
// values line-by-line, never marshal the struct back (yaml.v3 would drop
// every comment). Everything here is line-oriented and value-scoped: only
// name:/route:/dbMethods pin values and their marker comments change.

// epBlock is one `  - <ref-kind>: <value>` endpoint block's patch coordinates.
type epBlock struct {
	refKind   string // scenarioRef | scenarioFilter | conditionRef | condition
	refValue  string // unquoted scalar before the census comment
	nameLine  int    // 0-based index, -1 when absent
	routeLine int
	aiNamed   bool // the name line already carries an ai-suggested marker
}

// refKey is the join key against plan.Endpoint (endpointRefKey).
func (b epBlock) refKey() string {
	return b.refKind + ":" + b.refValue
}

var (
	epBlockStartRe = regexp.MustCompile(`^ {2}- (scenarioRef|scenarioFilter|conditionRef|condition):[ \t]*("[^"]*"|[^ \t#]*)`)
	epNameLineRe   = regexp.MustCompile(`^([ \t]+)name:[ \t]*("[^"]*"|[^ \t#]*)[ \t]*(#.*)?$`)
	epRouteLineRe  = regexp.MustCompile(`^([ \t]+)route:[ \t]*("[^"]*"|[^ \t#]*)[ \t]*(#.*)?$`)
	dbMethodLineRe = regexp.MustCompile(`^([ \t]+)((?:[A-Za-z0-9_]+)(?::[A-Za-z0-9_]+)?):[ \t]*\{([^}]*)\}([ \t]*#.*)?$`)
	pinNameRe      = regexp.MustCompile(`name:[ \t]*[A-Za-z0-9_]+`)
	pinRowRe       = regexp.MustCompile(`row:[ \t]*[A-Za-z0-9_]+`)
	topLevelKeyRe  = regexp.MustCompile(`^[A-Za-z_]`)
)

const aiSuggestedMarker = "# ai-suggested — edit freely"

// endpointRefKey is the join key a mapping endpoint answers to.
func endpointRefKey(e plan.Endpoint) string {
	switch {
	case e.ScenarioRef != "":
		return "scenarioRef:" + e.ScenarioRef
	case e.ScenarioFilter != "":
		return "scenarioFilter:" + e.ScenarioFilter
	case e.ConditionRef != "":
		return "conditionRef:" + e.ConditionRef
	default:
		return "condition:" + strconv.Itoa(e.Condition)
	}
}

// endpointBlocks scans the yaml's endpoint list. A block runs from its
// `  - <ref>:` line to the next block start or the first top-level key
// (dbMethods:). Blocks outside those shapes (user additions, comments)
// are never touched.
func endpointBlocks(lines []string) []epBlock {
	var out []epBlock
	cur := -1
	for i, line := range lines {
		if m := epBlockStartRe.FindStringSubmatch(line); m != nil {
			out = append(out, epBlock{refKind: m[1], refValue: strings.Trim(m[2], `"`), nameLine: -1, routeLine: -1})
			cur = len(out) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		if topLevelKeyRe.MatchString(line) {
			cur = -1
			continue
		}
		if epNameLineRe.MatchString(line) && out[cur].nameLine < 0 {
			out[cur].nameLine = i
			out[cur].aiNamed = strings.Contains(line, "ai-suggested")
			continue
		}
		if epRouteLineRe.MatchString(line) && out[cur].routeLine < 0 {
			out[cur].routeLine = i
		}
	}
	return out
}

// endpointForRef finds the mapping endpoint a block belongs to.
func endpointForRef(m *plan.Mapping, b epBlock) (plan.Endpoint, bool) {
	want := b.refKey()
	for _, e := range m.Endpoints {
		if endpointRefKey(e) == want {
			return e, true
		}
	}
	return plan.Endpoint{}, false
}

// patchMappingYAML rewrites the yaml text: every endpoint block with a
// suggestion gets its name/route values and marker replaced; dbMethods
// pins named by the suggestions' Methods update in place. All other
// lines pass through byte-for-byte.
func patchMappingYAML(raw string, blocks []epBlock, suggestions map[string]aiSuggestion) string {
	lines := strings.Split(raw, "\n")
	for _, b := range blocks {
		sug, ok := suggestions[b.refKey()]
		if !ok {
			continue
		}
		if b.nameLine >= 0 && sug.Name != "" {
			lines[b.nameLine] = nameIndent(lines[b.nameLine]) + "name: " + strconv.Quote(sug.Name) + " " + aiSuggestedMarker
		}
		if b.routeLine >= 0 && sug.Route != "" {
			lines[b.routeLine] = nameIndent(lines[b.routeLine]) + "route: " + strconv.Quote(sug.Route) + " " + aiSuggestedMarker
		}
	}
	return patchDBMethodLines(lines, suggestions)
}

// patchDBMethodLines applies the suggestions' method pins to the
// dbMethods block: `{name: X, row: Y}` values update in place, the marker
// flips to ai-suggested, and untouched entries (queries of pruned
// endpoints, fn-library pins) pass through untouched.
func patchDBMethodLines(lines []string, suggestions map[string]aiSuggestion) string {
	pins := map[string]methodPinSuggestion{}
	for _, sug := range suggestions {
		for id, m := range sug.Methods {
			if _, dup := pins[id]; !dup {
				pins[id] = m
			}
		}
	}
	if len(pins) == 0 {
		return strings.Join(lines, "\n")
	}
	inDB := false
	for i, line := range lines {
		if topLevelKeyRe.MatchString(line) {
			inDB = strings.HasPrefix(line, "dbMethods:")
			continue
		}
		if !inDB {
			continue
		}
		m := dbMethodLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pin, ok := pins[m[2]]
		if !ok {
			continue
		}
		content := m[3]
		changed := false
		if pin.Name != "" {
			content, changed = replacePinField(content, pinNameRe, "name", pin.Name, changed)
		}
		if pin.Row != "" {
			if pinRowRe.MatchString(content) {
				content, changed = replacePinField(content, pinRowRe, "row", pin.Row, changed)
			} else {
				content = strings.TrimSpace(content + ", row: " + pin.Row)
				changed = true
			}
		}
		if !changed {
			continue
		}
		lines[i] = m[1] + m[2] + ": {" + content + "}   " + aiSuggestedMarker
	}
	return strings.Join(lines, "\n")
}

// replacePinField rewrites one `field: value` inside a dbMethods brace
// body, reporting whether anything changed.
func replacePinField(content string, re *regexp.Regexp, field, value string, changed bool) (string, bool) {
	replacement := field + ": " + value
	if re.MatchString(content) {
		if re.FindString(content) == replacement {
			return content, changed
		}
		return re.ReplaceAllString(content, replacement), true
	}
	return content, changed
}

// nameIndent preserves the block's indentation from the original line.
func nameIndent(line string) string {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return line[:i]
		}
	}
	return ""
}
