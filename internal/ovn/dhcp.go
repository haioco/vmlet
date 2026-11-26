package ovn

import "fmt"

// DHCPOptions is a row of the OVN_Northbound DHCP_Options table.
// OVN's DHCPv4 server answers from these rows; guests reference them through
// Logical_Switch_Port.dhcpv4_options.
type DHCPOptions struct {
	UUID        string            `ovsdb:"_uuid"`
	CIDR        string            `ovsdb:"cidr"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// DNS is a row of the DNS table. The real schema models records as a
// string-to-string map ("hostname" -> "ip" or the reverse depending on OVN
// version); vmlet stores "ip" -> "hostname" so lookups by IP are direct.
type DNS struct {
	UUID        string            `ovsdb:"_uuid"`
	Records     map[string]string `ovsdb:"records"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// ACL is a row of the Logical_Switch.acls target table. It is modelled so the
// DB schema validates and so item 1.6 (security groups) has somewhere to land.
type ACL struct {
	UUID        string            `ovsdb:"_uuid"`
	Action      string            `ovsdb:"action"`
	Direction   string            `ovsdb:"direction"`
	Match       string            `ovsdb:"match"`
	Priority    int               `ovsdb:"priority"`
	Log         bool              `ovsdb:"log"`
	Severity    *string           `ovsdb:"severity"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// DHCPReservation is a per-VM static DHCP binding keyed by MAC.
type DHCPReservation struct {
	// PortName is the logical switch port the reservation belongs to.
	PortName string
	// MAC is the guest's MAC address (the reservation key).
	MAC string
	// IP is the statically assigned address, in CIDR-less form.
	IP string
	// Hostname is the VM's hostname, used for the DNS record.
	Hostname string
}

// DHCPSpec describes the desired subnet-level DHCP configuration. One row is
// created per logical switch (per subnet) and referenced by its VM ports.
type DHCPSpec struct {
	// Switch is the logical switch the subnet lives on.
	Switch string
	// CIDR is the subnet in CIDR notation (e.g. "10.0.0.0/24").
	CIDR string
	// Gateway is the subnet's default gateway.
	Gateway string
	// DNSServers are handed to guests through DHCP option 6.
	DNSServers []string
	// LeaseTime in seconds (OVN default is 3600 when empty).
	LeaseTime string
	// Reservations are the MAC-keyed static bindings on this subnet.
	Reservations []DHCPReservation
}

// dnsKeyExternalID is the external_ids key matching a DNS row to its
// reservation, so the reconcile can prune stale records.
const dnsKeyExternalID = "vmlet-reservation"

// dhcpExternalIDs builds the external_ids marking a DHCP row as vmlet-managed.
func dhcpExternalIDs(network string) map[string]string {
	return map[string]string{
		"vmlet-managed": "true",
		"vmlet-network": network,
	}
}

// dhcpOptions builds the OVSDB map OVN expects on a DHCP_Options row:
// subnet mask and gateway are derived from the CIDR, DNS and lease time come
// from the spec.
func dhcpOptions(spec DHCPSpec) (map[string]string, error) {
	_, ipNet, err := parseCIDR(spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("ovn: invalid DHCP subnet %q: %w", spec.CIDR, err)
	}
	gateway := spec.Gateway
	serverIP := firstUsable(ipNet)
	if gateway == "" {
		gateway = serverIP.String()
	}
	lease := spec.LeaseTime
	if lease == "" {
		lease = "3600"
	}

	options := map[string]string{
		"server_id":  serverIP.String(),
		"server_mac": GatewayRouterMAC,
		"lease_time": lease,
		"router":     gateway,
	}
	if mask := netmaskToDotted(ipNet); mask != "0.0.0.0" {
		options["netmask"] = mask
	}
	if len(spec.DNSServers) > 0 {
		options["dns_server"] = spec.DNSServers[0]
		for _, extra := range spec.DNSServers[1:] {
			options["dns_server"] = options["dns_server"] + " " + extra
		}
	}
	return options, nil
}

// DHCPSpecsFromState derives one DHCPSpec per network subnet, with a
// reservation per VM. VMs whose network is not described are dropped, so a
// stale VM record cannot introduce a bogus subnet.
func DHCPSpecsFromState(networks []NetworkDefinition, vms []VMDefinition) []DHCPSpec {
	byNetwork := make(map[string][]VMDefinition, len(networks))
	for _, vm := range vms {
		byNetwork[vm.Network] = append(byNetwork[vm.Network], vm)
	}

	specs := make([]DHCPSpec, 0, len(networks))
	for _, net := range networks {
		if net.Name == "" {
			continue
		}

		spec := DHCPSpec{
			Switch:     GatewaySwitchName(net.Name),
			CIDR:       net.CIDR,
			Gateway:    net.GatewayIP,
			DNSServers: net.DNSServers,
		}

		for _, vm := range byNetwork[net.Name] {
			spec.Reservations = append(spec.Reservations, DHCPReservation{
				PortName: ovnPortName(vm.ID),
				MAC:      vm.MAC,
				IP:       vm.IP,
				Hostname: hostnameFor(vm),
			})
		}
		specs = append(specs, spec)
	}
	return specs
}

// hostnameFor builds the DNS hostname for a VM reservation.
func hostnameFor(vm VMDefinition) string {
	if vm.ID == "" {
		return ""
	}
	return vmIDPrefix(vm.ID) + "." + sanitize(vm.Network)
}

// vmIDPrefix keeps hostnames short: OVN DNS records use "ip hostname", so a
// long VM id would produce an unwieldy name.
func vmIDPrefix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
