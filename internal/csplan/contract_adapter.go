package csplan

import "tux-migration-tool/internal/contract"

// ContractView adapts the convertcs mapping to the uniform MappingView
// (uniform-ir plan §3.1/Phase 6 scaffold): endpoints in mapping order plus
// the per-query NamedQueries const pins. No YAML changes; resolution bodies
// still live per-plan until Phase 4 unifies them behind this interface.
func (m *Mapping) ContractView() contract.MappingView {
	v := contract.StaticMapping{Pin: map[string]string{}}
	if m == nil {
		return v
	}
	for _, e := range m.Endpoints {
		v.Eps = append(v.Eps, contract.EndpointView{
			Name: e.Name, Route: e.Route,
			Condition: e.Condition, ConditionRef: e.ConditionRef,
			ScenarioRef: e.ScenarioRef, ScenarioFilter: e.ScenarioFilter,
		})
	}
	for id, pin := range m.DBMethods {
		if pin.Name != "" {
			v.Pin[id] = pin.Name
		}
	}
	return v
}
