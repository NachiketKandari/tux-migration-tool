// Package plan builds the deterministic decomposition plan (PRD F2,
// plan-conversion §3): IR + the user's endpoint mapping → ordered generation
// units with template marking, target paths, dependency order, and token
// estimates. The tool never invents endpoints or their names — the mapping
// is user data (§4.2.8); DB method names get deterministic fallbacks the
// mapping may pin. The LLM is not involved in planning.
package plan

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"tux-migration-tool/internal/flow"
)

// MethodPin is the user's optional pin for one DB method: the method name,
// per-bind "name" or "name:Type" parameters, and the result row struct name
// — reference-quality signatures without an LLM. Unpinned aspects fall back
// to deterministic naming.
type MethodPin struct {
	Name   string   `yaml:"name"`
	Params []string `yaml:"params"`
	Row    string   `yaml:"row"`
}

// Endpoint is one user-mapped condition: the IR condition inventory index
// (1-based) promoted to an API endpoint with a user-chosen Go method name
// and route path (§4.2.8 — the tool never decides endpoint-ness).
// ConditionRef is the discovery alternative (PRD-2026-09-10): a stable
// candidate key from `tuxconv discover` — "c<n>" for a top-level condition,
// "c<n>.<k>" for a qualifying nested branch. ScenarioRef is the
// dispatch-axis alternative (PRD-2026-09-12 SCEN-5): "var=value" — the
// scenario slice the flow fold derives (e.g. "trn_cd=A"). ScenarioFilter
// is the scenario-filter alternative (scenario-filter plan §5): a boolean
// over the detected dispatch axes (`c_flag == 'F' || c_flag == 'I'`,
// `c_flag == 'H' && new_flag == 'K'`) re-folded as one endpoint. Exactly
// one of the four must be set; condition/conditionRef/scenarioRef stay
// loadable (additive schema, zero golden churn).
type Endpoint struct {
	Condition      int    `yaml:"condition"`
	ConditionRef   string `yaml:"conditionRef"`
	ScenarioRef    string `yaml:"scenarioRef"`
	ScenarioFilter string `yaml:"scenarioFilter"`
	Name           string `yaml:"name"`
	Route          string `yaml:"route"`
}

// RefOrIndex reports the endpoint's condition reference for error messages.
func (e Endpoint) RefOrIndex() string {
	switch {
	case e.ConditionRef != "":
		return e.ConditionRef
	case e.ScenarioRef != "":
		return "scenario " + e.ScenarioRef
	case e.ScenarioFilter != "":
		return "scenarioFilter " + e.ScenarioFilter
	}
	return fmt.Sprintf("condition %d", e.Condition)
}

// ParseScenarioRef splits a scenarioRef ("trn_cd=A") into its axis-key
// identifier and value (SCEN-D7). The schema home for the draft key format:
// a non-empty identifier, '=', and a non-empty verbatim value.
func ParseScenarioRef(ref string) (key, value string, err error) {
	key, value, ok := strings.Cut(ref, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return "", "", fmt.Errorf("scenarioRef %q must be <key>=<value> (e.g. trn_cd=A)", ref)
	}
	if i := strings.IndexAny(key, " \t"); i >= 0 {
		return "", "", fmt.Errorf("scenarioRef %q — the key must be a bare identifier", ref)
	}
	return key, value, nil
}

// Mapping is the user-specified conversion mapping (F3 run input): the
// target service identity plus which conditions become endpoints. Loaded
// from a small YAML file the user edits. Source is optional and only used
// by dir-mode convert fan-out: it names the entry .pc/.pcf file (bare file
// name) this mapping converts, letting one mapping directory cover a
// multi-service corpus.
type Mapping struct {
	Source     string               `yaml:"source"`
	Service    string               `yaml:"service"`
	Module     string               `yaml:"module"`
	ReadDBs    []string             `yaml:"readDBs"`
	RouteGroup string               `yaml:"routeGroup"`
	Endpoints  []Endpoint           `yaml:"endpoints"`
	DBMethods  map[string]MethodPin `yaml:"dbMethods"`
}

// MappingSourceOf reads only a mapping's source field — the lenient matcher
// for convention directories, where unrelated drafts (other targets, older
// formats) must never poison a run. Parse errors still surface; a draft
// without a source returns "". Validation stays with LoadMapping, which
// only the matched winner goes through.
func MappingSourceOf(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("plan: read mapping %s: %w", path, err)
	}
	var m struct {
		Source string `yaml:"source"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("plan: parse mapping %s: %w", path, err)
	}
	return m.Source, nil
}

// MappingServiceOf reads only the service field — the lenient read
// discover uses to seed its service-name uniqueness space from drafts
// already on disk. A parse error surfaces; callers decide whether that is
// fatal (discover ignores it: it must not fail on a file it only wants to
// avoid repeating).
func MappingServiceOf(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("plan: read mapping %s: %w", path, err)
	}
	var m struct {
		Service string `yaml:"service"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("plan: parse mapping %s: %w", path, err)
	}
	return m.Service, nil
}

// LoadMapping reads and validates a mapping YAML file. An absent module
// defaults to the service name — discover drafts load without a second edit
// (the generated import prefix then equals the service name; set module: in
// the yaml when generating into an existing repo).
func LoadMapping(path string) (*Mapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plan: read mapping %s: %w", path, err)
	}
	var m Mapping
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plan: parse mapping %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plan: %s: %w", path, err)
	}
	if m.Module == "" {
		m.Module = m.Service
	}
	return &m, nil
}

// Validate enforces the mapping invariants: service identity present, at
// least one endpoint, unique Go method names and condition indices, valid
// identifiers, routes under the group. Module is optional (defaults to the
// service at load); when set it must be a plausible import path.
func (m *Mapping) Validate() error {
	if m.Source != "" {
		if err := validEntryName(m.Source); err != nil {
			return err
		}
	}
	if m.Service == "" {
		return fmt.Errorf("service must not be empty")
	}
	if !token.IsIdentifier(m.Service) {
		return fmt.Errorf("service %q is not a valid Go identifier", m.Service)
	}
	if m.Module != "" && !strings.HasPrefix(m.Module, strings.SplitN(m.Module, "/", 2)[0]) {
		return fmt.Errorf("module %q is malformed", m.Module)
	}
	if len(m.Endpoints) == 0 {
		return fmt.Errorf("at least one endpoint must be mapped — the tool never invents endpoints (§4.2.8)")
	}
	conds := map[int]bool{}
	refs := map[string]bool{}
	scens := map[string]bool{}
	filters := map[string]bool{}
	names := map[string]bool{}
	for i, e := range m.Endpoints {
		set := 0
		if e.Condition >= 1 {
			set++
		}
		if e.ConditionRef != "" {
			set++
		}
		if e.ScenarioRef != "" {
			set++
		}
		if e.ScenarioFilter != "" {
			set++
		}
		if set != 1 {
			return fmt.Errorf("endpoints[%d] (%s): exactly one of condition (1-based inventory index), conditionRef (discover candidate key), scenarioRef (dispatch-axis slice) or scenarioFilter (boolean over dispatch axes) must be set", i, e.Name)
		}
		switch {
		case e.ConditionRef != "":
			if refs[e.ConditionRef] {
				return fmt.Errorf("endpoints[%d]: candidate %s mapped twice", i, e.ConditionRef)
			}
			refs[e.ConditionRef] = true
		case e.ScenarioRef != "":
			if _, _, err := ParseScenarioRef(e.ScenarioRef); err != nil {
				return fmt.Errorf("endpoints[%d]: %w", i, err)
			}
			if scens[e.ScenarioRef] {
				return fmt.Errorf("endpoints[%d]: scenario %s mapped twice", i, e.ScenarioRef)
			}
			scens[e.ScenarioRef] = true
		case e.ScenarioFilter != "":
			// Syntax is validated at load (like scenarioRef); axis/value
			// existence defers to plan build, against the file that
			// actually dispatches.
			if _, err := flow.ParseScenarioFilter(e.ScenarioFilter); err != nil {
				return fmt.Errorf("endpoints[%d]: %w", i, err)
			}
			if filters[e.ScenarioFilter] {
				return fmt.Errorf("endpoints[%d]: scenarioFilter %s mapped twice", i, e.ScenarioFilter)
			}
			filters[e.ScenarioFilter] = true
		default:
			if e.Condition < 1 {
				return fmt.Errorf("endpoints[%d].condition must be a 1-based inventory index", i)
			}
			if conds[e.Condition] {
				return fmt.Errorf("endpoints[%d]: condition %d mapped twice", i, e.Condition)
			}
			conds[e.Condition] = true
		}
		if !token.IsIdentifier(e.Name) {
			return fmt.Errorf("endpoints[%d].name %q is not a valid Go identifier", i, e.Name)
		}
		if names[e.Name] {
			return fmt.Errorf("endpoints[%d]: endpoint name %q used twice", i, e.Name)
		}
		names[e.Name] = true
		if !strings.HasPrefix(e.Route, "/") {
			return fmt.Errorf("endpoints[%d].route %q must start with /", i, e.Route)
		}
	}
	for qid, pin := range m.DBMethods {
		if !token.IsIdentifier(pin.Name) {
			return fmt.Errorf("dbMethods[%q].name %q is not a valid Go identifier", qid, pin.Name)
		}
	}
	return nil
}

// ImportPath returns the module path with the service folder appended when
// the last segment is not already the service name.
func (m *Mapping) ImportPath(folder string) string {
	if folder == "" || strings.HasSuffix(m.Module, "/"+folder) {
		return m.Module
	}
	return m.Module + "/" + folder
}

// validEntryName enforces the dir-mode source reference: a bare .pc/.pcf
// file name — no directories, no other extensions — so the entry-file match
// stays a plain basename comparison.
func validEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("source %q must be a bare .pc/.pcf file name", name)
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".pc" && ext != ".pcf" {
		return fmt.Errorf("source %q must end in .pc or .pcf", name)
	}
	return nil
}
