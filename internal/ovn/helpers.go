package ovn

import (
	"fmt"
	"sort"
)

// namedRef builds the named-UUID used to reference a row that is created
// earlier in the same transaction. OVSDB resolves it against the real UUID
// when the transaction commits.
func namedRef(kind, name string) string {
	return fmt.Sprintf("@vmlet-%s-%s", kind, name)
}

func strPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		got, ok := b[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func containsSwitch(specs []LogicalSwitchSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func containsRouter(specs []LogicalRouterSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

// indexSwitchPorts indexes the current switch ports by name.
func indexSwitchPorts(ports []LogicalSwitchPort) map[string]*LogicalSwitchPort {
	out := make(map[string]*LogicalSwitchPort, len(ports))
	for i := range ports {
		out[ports[i].Name] = &ports[i]
	}
	return out
}

// indexPortsBySwitch maps a logical switch name to the set of port names the
// switch currently references.
func indexPortsBySwitch(switches []LogicalSwitch, ports []LogicalSwitchPort) map[string]map[string]struct{} {
	byUUID := make(map[string]string, len(ports))
	for i := range ports {
		byUUID[ports[i].UUID] = ports[i].Name
	}

	out := make(map[string]map[string]struct{}, len(switches))
	for i := range switches {
		names := make(map[string]struct{}, len(switches[i].Ports))
		for _, uuid := range switches[i].Ports {
			if name, ok := byUUID[uuid]; ok {
				names[name] = struct{}{}
			}
		}
		out[switches[i].Name] = names
	}
	return out
}

// sortedNames returns the keys of a set in stable order.
func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedPortNames returns the desired port names in stable order.
func sortedPortNames(specs map[string]LogicalPortSpec) []string {
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
