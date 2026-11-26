package ovn

import "strings"

// VMSwitchName is the OVN logical switch name carrying a network's VMs.
// OVN requires names to be DNS-safe, so vmlet derives them from the network
// name deterministically: "customer-3" -> "vm-customer-3".
func VMSwitchName(networkName string) string {
	return sanitize("vm-" + networkName)
}

// GatewaySwitchName is the logical switch that terminates a network's subnet,
// holding the router port and (later) the NAT/floating-IP connections.
func GatewaySwitchName(networkName string) string {
	return sanitize("gw-" + networkName)
}

// GatewayRouterPortName is the logical router port attached to a network's
// gateway switch; it is the network's default gateway.
func GatewayRouterPortName(networkName string) string {
	return sanitize("lrp-" + networkName)
}

// GatewayRouterName is the logical router that interconnects all networks.
func GatewayRouterName(networkName string) string {
	return sanitize("lr-" + networkName)
}

// GatewayRouterMAC is the locally administered MAC OVN expects on a router
// port; every network's gateway port uses the same MAC, which is conventional
// for OVN (the router port is a virtual construct, not a real NIC).
const GatewayRouterMAC = "00:00:00:00:ff:01"

// String literals for port types used by the topology.
const (
	PortTypeRouter   = "router"
	PortTypeLocalnet = "localnet"
)

// sanitize lowercases a name and replaces characters OVN names cannot hold.
func sanitize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "vm-default"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-':
			if !prevDash {
				b.WriteRune(r)
				prevDash = true
			}
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "vm-default"
	}
	return out
}

// NetworkDefinition is the per-network input vmlet derives its OVN topology
// from. Names and CIDR are required; SubnetName/ProviderBridge become the
// localnet uplink once item 1.8 lands.
type NetworkDefinition struct {
	// Name is the vmlet network name (e.g. "customer-3").
	Name string
	// CIDR is the network subnet in CIDR notation (e.g. "10.0.0.0/24").
	CIDR string
	// GatewayIP is the router port address for this subnet (e.g. "10.0.0.1").
	GatewayIP string
	// DNSServers are the addresses handed out by DHCP (item 1.5 wires these).
	DNSServers []string
}

// VMDefinition is the per-VM input vmlet derives logical ports from.
type VMDefinition struct {
	ID string
	// Network is the vmlet network name the VM belongs to.
	Network string
	MAC     string
	IP      string
}

// TopologyFromNetworks derives the desired OVN logical topology from vmlet's
// network definitions: one VM switch per network, a gateway switch holding the
// router port, and the shared logical router all networks hang off.
//
// Every gateway switch carries a "router" port typed port that references the
// network's logical router port; that pair is what routes east-west traffic
// between two networks. Because both ports are created in one transaction,
// the peer relationship is atomic.
func TopologyFromNetworks(networks []NetworkDefinition) Topology {
	switches := make([]LogicalSwitchSpec, 0, len(networks)*2)
	routers := make([]LogicalRouterSpec, 0, len(networks))

	for _, net := range networks {
		if net.Name == "" {
			continue
		}

		gwSwitch := GatewaySwitchName(net.Name)
		routerName := GatewayRouterName(net.Name)
		lrpName := GatewayRouterPortName(net.Name)
		lrpAddress := gatewayAddress(net.GatewayIP)

		switches = append(switches, LogicalSwitchSpec{
			Name: gwSwitch,
			ExternalIDs: map[string]string{
				"vmlet-managed": "true",
				"vmlet-network": net.Name,
				"vmlet-role":    "gateway",
			},
			Ports: []LogicalPortSpec{
				{
					Name:      lspNameForGatewaySwitch(gwSwitch),
					Type:      PortTypeRouter,
					Addresses: []string{GatewayRouterMAC + " " + lrpAddress},
					Options:   map[string]string{"router-port": lrpName},
				},
			},
		})

		// The VM switch starts empty; TopologyFromVMState fills it with VM
		// ports. It never carries router ports, which keeps the two roles
		// (and the two external_id namespaces) separate.
		switches = append(switches, LogicalSwitchSpec{
			Name: VMSwitchName(net.Name),
			ExternalIDs: map[string]string{
				"vmlet-managed": "true",
				"vmlet-network": net.Name,
				"vmlet-role":    "vm",
			},
		})

		routers = append(routers, LogicalRouterSpec{
			Name: routerName,
			ExternalIDs: map[string]string{
				"vmlet-managed": "true",
				"vmlet-network": net.Name,
			},
		})
	}

	return Topology{Switches: switches, Routers: routers}
}

// lspNameForGatewaySwitch is the name of the router-facing port that lives on
// a network's gateway switch.
func lspNameForGatewaySwitch(gwSwitch string) string {
	return gwSwitch + "-lrport"
}

// gatewayAddress normalises a configured router-port address into the form
// OVN expects for Logical_Router_Port.networks ("<ip>/<prefix>").
func gatewayAddress(gatewayIP string) string {
	if gatewayIP == "" {
		return ""
	}
	if strings.Contains(gatewayIP, "/") {
		return gatewayIP
	}
	return gatewayIP + "/24"
}

// TopologyFromVMState derives the desired OVN logical topology from vmlet's
// network definitions plus the running VMs attached to them. Each network gets
// a VM switch, a gateway switch, a router and (once 1.5's DHCP work lands)
// router-port driven addressing; each VM gets one logical port.
func TopologyFromVMState(networks []NetworkDefinition, vms []VMDefinition) Topology {
	byNetwork := make(map[string][]VMDefinition, len(networks))
	for _, vm := range vms {
		byNetwork[vm.Network] = append(byNetwork[vm.Network], vm)
	}

	topology := TopologyFromNetworks(networks)

	for i := range topology.Switches {
		spec := &topology.Switches[i]
		networkName := spec.ExternalIDs["vmlet-network"]
		if networkName == "" {
			continue
		}
		for _, vm := range byNetwork[networkName] {
			ports, ok := vmSwitchPorts(vm, spec.Name)
			if !ok {
				continue
			}
			spec.Ports = append(spec.Ports, ports...)
		}
	}

	return topology
}

// vmSwitchPorts returns the logical ports a VM needs on its network's VM
// switch. It returns ok=false when the switch is not a VM switch.
func vmSwitchPorts(vm VMDefinition, switchName string) ([]LogicalPortSpec, bool) {
	if VMSwitchName(vm.Network) != switchName {
		return nil, false
	}
	addresses := make([]string, 0, 2)
	if vm.MAC != "" && vm.IP != "" {
		addresses = append(addresses, vm.MAC+" "+vm.IP)
	} else if vm.MAC != "" {
		addresses = append(addresses, vm.MAC)
	}

	port := LogicalPortSpec{
		Name:      ovnPortName(vm.ID),
		Addresses: addresses,
		ExternalIDs: map[string]string{
			"vmlet-managed": "true",
			"vmlet-vm":      vm.ID,
			"vmlet-network": vm.Network,
			"vmlet-role":    "vm",
		},
	}
	return []LogicalPortSpec{port}, true
}

// ovnPortName mirrors firecracker.ovnPortNameFor so the NB port name and the
// OVS iface-id stay in lockstep.
func ovnPortName(vmID string) string {
	return "fc-port-" + vmID
}
