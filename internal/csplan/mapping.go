// Package csplan builds the deterministic decomposition plan for the
// convertcs (Pro*C → .NET Core / C#) target: IR + the user's endpoint
// mapping → one endpoint per dispatch-arm slice, one query plan per SQL
// unit (kind, NamedQueries const name, named Oracle parameters, DTO row
// properties). The tool never invents endpoints, routes, or type names —
// the mapping is user data; unpinned query names fall back to
// deterministic derivation. The LLM is not involved in planning.
package csplan

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"tux-migration-tool/internal/flow"
)

// MethodPin is the user's optional pin for one NamedQueries const name.
type MethodPin struct {
	Name string `yaml:"name"`
}

// Endpoint is one user-mapped arm: the same four reference forms as the
// Go mapping (condition index, discover candidate key, dispatch-axis
// slice, scenario filter) promoted to an ASP.NET Core action with a
// user-chosen method name and the literal [Route(...)] value.
type Endpoint struct {
	Condition      int    `yaml:"condition"`
	ConditionRef   string `yaml:"conditionRef"`
	ScenarioRef    string `yaml:"scenarioRef"`
	ScenarioFilter string `yaml:"scenarioFilter"`
	Name           string `yaml:"name"`
	Route          string `yaml:"route"`
}

// Mapping is the convertcs mapping YAML: the generated tree's namespace
// identity plus which arms become actions. Everything naming-shaped is
// user data; the plan only derives what the mapping leaves unset.
type Mapping struct {
	Source string `yaml:"source"` // dir-mode: the entry .pc file this mapping converts

	Namespace  string `yaml:"namespace"`  // root namespace (e.g. AcmeBackendApi)
	Area       string `yaml:"area"`       // dotted area under the root (e.g. AcmeApplication.CustomerAuthenticate)
	Component  string `yaml:"component"`  // class stem: <Component>Controller/Service/Repository/Queries/DTO
	ApiVersion string `yaml:"apiVersion"` // [ApiVersion("1.0")] — default 1.0
	RequestDTO string `yaml:"requestDTO"` // shared request type the actions take (default object)
	Validator  string `yaml:"validator"`  // optional validator tag ("" skips the validation block)

	Endpoints []Endpoint `yaml:"endpoints"`

	// RequestFields maps a bind host var to the request property that
	// supplies it (sql_cst_pan_no: MobileNo → request.MobileNo).
	RequestFields map[string]string `yaml:"requestFields"`
	// ParamNames maps a bind host var to the named Oracle parameter
	// (defaults to the request property, else a deterministic name).
	ParamNames map[string]string `yaml:"paramNames"`
	// DBMethods pins a query unit's NamedQueries const name (q1: name: …).
	DBMethods map[string]MethodPin `yaml:"dbMethods"`
}

var csIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MappingSourceOf reads only a mapping's source field — the lenient matcher
// for convention directories, where unrelated drafts (other targets, older
// formats) must never poison a run. Parse errors still surface; a draft
// without a source returns "". Validation stays with LoadMapping, which
// only the matched winner goes through.
func MappingSourceOf(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("csplan: read mapping %s: %w", path, err)
	}
	var m struct {
		Source string `yaml:"source"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("csplan: parse mapping %s: %w", path, err)
	}
	return m.Source, nil
}

// LoadMapping reads and validates a convertcs mapping YAML file. Strict
// fields: a typo is an error, never a silent default.
func LoadMapping(path string) (*Mapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("csplan: read mapping %s: %w", path, err)
	}
	var m Mapping
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("csplan: parse mapping %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("csplan: %s: %w", path, err)
	}
	return &m, nil
}

// Validate enforces the mapping invariants: namespace identity present,
// at least one endpoint, unique action names, valid C# identifiers, and
// the one-reference-per-endpoint rule shared with the Go mapping.
func (m *Mapping) Validate() error {
	if m.Namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	if m.Component == "" {
		return fmt.Errorf("component must not be empty")
	}
	if !csIdentRe.MatchString(m.Component) {
		return fmt.Errorf("component %q is not a valid C# identifier", m.Component)
	}
	if len(m.Endpoints) == 0 {
		return fmt.Errorf("at least one endpoint must be mapped — the tool never invents endpoints")
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
			return fmt.Errorf("endpoints[%d] (%s): exactly one of condition, conditionRef, scenarioRef or scenarioFilter must be set", i, e.Name)
		}
		switch {
		case e.ConditionRef != "":
			if refs[e.ConditionRef] {
				return fmt.Errorf("endpoints[%d]: candidate %s mapped twice", i, e.ConditionRef)
			}
			refs[e.ConditionRef] = true
		case e.ScenarioRef != "":
			// Format validation deliberately lives in csplan.Build (D4):
			// a malformed scenarioRef error must name the axis the file
			// actually dispatches on — a fact only Build has. The load
			// pass keeps the twice-mapped check.
			if scens[e.ScenarioRef] {
				return fmt.Errorf("endpoints[%d]: scenario %s mapped twice", i, e.ScenarioRef)
			}
			scens[e.ScenarioRef] = true
		case e.ScenarioFilter != "":
			// Same split as scenarioRef: syntax at load, axis/value
			// existence against the file in Build.
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
		if !csIdentRe.MatchString(e.Name) {
			return fmt.Errorf("endpoints[%d].name %q is not a valid C# identifier", i, e.Name)
		}
		if names[e.Name] {
			return fmt.Errorf("endpoints[%d]: endpoint name %q used twice", i, e.Name)
		}
		names[e.Name] = true
		if strings.TrimSpace(e.Route) == "" {
			return fmt.Errorf("endpoints[%d].route must not be empty", i)
		}
	}
	for qid, pin := range m.DBMethods {
		if !csIdentRe.MatchString(pin.Name) {
			return fmt.Errorf("dbMethods[%q].name %q is not a valid C# identifier", qid, pin.Name)
		}
	}
	return nil
}
