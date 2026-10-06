package contract

// MappingView is the minimal endpoint-mapping surface the uniform builder
// needs (uniform-ir plan §3.1): the endpoint list in mapping order plus the
// per-query DB pins. plan.Mapping and csplan.Mapping adapt to it
// (plan/contract_adapter.go, csplan/contract_adapter.go) so mapping YAMLs
// stay per-language while endpoint-resolution logic unifies behind this
// interface. No YAML changes; old files work unchanged (Phase 6 scaffold).
type MappingView interface {
	// Endpoints returns the mapped endpoints in mapping order.
	Endpoints() []EndpointView
	// DBPin returns the pinned DB/const name for a query id ("" unpinned).
	DBPin(queryID string) string
}

// EndpointView is one mapped endpoint's reference: exactly one of the four
// reference forms is set (condition index, discover candidate key,
// dispatch-axis slice, scenario filter).
type EndpointView struct {
	Name           string
	Route          string
	Condition      int
	ConditionRef   string
	ScenarioRef    string
	ScenarioFilter string
}

// RefKind reports which reference form the endpoint uses.
func (e EndpointView) RefKind() string {
	switch {
	case e.ScenarioFilter != "":
		return "scenarioFilter"
	case e.ScenarioRef != "":
		return "scenarioRef"
	case e.ConditionRef != "":
		return "conditionRef"
	default:
		return "condition"
	}
}

// StaticMapping is an in-memory MappingView for tests and adapters.
type StaticMapping struct {
	Eps []EndpointView
	Pin map[string]string
}

// Endpoints implements MappingView.
func (m StaticMapping) Endpoints() []EndpointView { return m.Eps }

// DBPin implements MappingView.
func (m StaticMapping) DBPin(queryID string) string { return m.Pin[queryID] }
