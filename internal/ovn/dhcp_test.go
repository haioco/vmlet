package ovn

import (
	"context"
	"testing"
)

// seedNetwork reconciles the switches/routers a DHCP test needs, so ports
// exist to attach reservations to.
func seedNetwork(t *testing.T, c *Client, ctx context.Context, network, cidr, gatewayIP string) {
	t.Helper()
	desired := TopologyFromNetworks([]NetworkDefinition{
		{Name: network, CIDR: cidr, GatewayIP: gatewayIP},
	})
	if _, err := c.ReconcileTopology(ctx, desired); err != nil {
		t.Fatalf("seeding topology for %q: %v", network, err)
	}
}

func TestDHCPOptions_DerivedFromSubnet(t *testing.T) {
	spec := DHCPSpec{
		Switch:     GatewaySwitchName("alpha"),
		CIDR:       "10.0.0.0/24",
		Gateway:    "",
		DNSServers: []string{"8.8.8.8", "1.1.1.1"},
		LeaseTime:  "",
	}

	got, err := dhcpOptions(spec)
	if err != nil {
		t.Fatalf("dhcpOptions: %v", err)
	}

	want := map[string]string{
		"server_id":  "10.0.0.1",
		"server_mac": GatewayRouterMAC,
		"lease_time": "3600",
		"netmask":    "255.255.255.0",
		"router":     "10.0.0.1",
		"dns_server": "8.8.8.8 1.1.1.1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("options[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestDHCPOptions_ExplicitGatewayAndLease(t *testing.T) {
	spec := DHCPSpec{
		CIDR:      "192.168.5.0/24",
		Gateway:   "192.168.5.254",
		LeaseTime: "7200",
	}
	got, err := dhcpOptions(spec)
	if err != nil {
		t.Fatalf("dhcpOptions: %v", err)
	}
	if got["router"] != "192.168.5.254" {
		t.Errorf("router = %q, want 192.168.5.254", got["router"])
	}
	if got["lease_time"] != "7200" {
		t.Errorf("lease_time = %q, want 7200", got["lease_time"])
	}
	if got["server_id"] != "192.168.5.1" {
		t.Errorf("server_id = %q, want 192.168.5.1", got["server_id"])
	}
}

func TestDHCPOptions_RejectsBadCIDR(t *testing.T) {
	if _, err := dhcpOptions(DHCPSpec{CIDR: "not-a-cidr"}); err == nil {
		t.Fatal("dhcpOptions accepted an invalid CIDR")
	}
}

func TestFirstUsable(t *testing.T) {
	_, ipNet, err := parseCIDR("10.0.0.0/24")
	if err != nil {
		t.Fatalf("parseCIDR: %v", err)
	}
	if got := firstUsable(ipNet).String(); got != "10.0.0.1" {
		t.Errorf("firstUsable = %q, want 10.0.0.1", got)
	}
	if got := netmaskToDotted(ipNet); got != "255.255.255.0" {
		t.Errorf("netmaskToDotted = %q, want 255.255.255.0", got)
	}
}

func TestEnsureDHCPOptions_Idempotent(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()
	seedNetwork(t, c, ctx, "alpha", "10.0.0.0/24", "10.0.0.1")

	spec := DHCPSpec{
		Switch:     GatewaySwitchName("alpha"),
		CIDR:       "10.0.0.0/24",
		DNSServers: []string{"8.8.8.8"},
	}

	if _, err := c.EnsureDHCPOptions(ctx, spec, "alpha"); err != nil {
		t.Fatalf("EnsureDHCPOptions: %v", err)
	}

	// The first call returns the named-UUID referenced by the row it created;
	// the second reads the resolved UUID from the cache. Idempotency therefore
	// has to be asserted on the row content, not on the identifier.
	if _, err := c.EnsureDHCPOptions(ctx, spec, "alpha"); err != nil {
		t.Fatalf("EnsureDHCPOptions (repeat): %v", err)
	}

	rows, err := c.ListDHCPOptions(ctx)
	if err != nil {
		t.Fatalf("ListDHCPOptions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("DHCP_Options rows = %d, want 1", len(rows))
	}
	if rows[0].Options["dns_server"] != "8.8.8.8" {
		t.Errorf("dns_server = %q, want 8.8.8.8", rows[0].Options["dns_server"])
	}
}

func TestEnsureDHCPReservation_SurvivesRestart(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()
	seedNetwork(t, c, ctx, "alpha", "10.0.0.0/24", "10.0.0.1")

	// The VM port is derived exactly as production derives it.
	portName := "fc-port-vm-1"
	if err := c.EnsureLogicalSwitchPort(ctx, VMSwitchName("alpha"), LogicalPortSpec{
		Name:      portName,
		Addresses: []string{"02:fc:00:00:00:01 10.0.0.10"},
		ExternalIDs: map[string]string{
			"vmlet-managed": "true",
			"vmlet-vm":      "vm-1",
			"vmlet-network": "alpha",
		},
	}); err != nil {
		t.Fatalf("EnsureLogicalSwitchPort: %v", err)
	}

	if _, err := c.EnsureDHCPOptions(ctx, DHCPSpec{
		Switch: GatewaySwitchName("alpha"), CIDR: "10.0.0.0/24",
	}, "alpha"); err != nil {
		t.Fatalf("EnsureDHCPOptions: %v", err)
	}
	rows, err := c.ListDHCPOptions(ctx)
	if err != nil {
		t.Fatalf("ListDHCPOptions: %v", err)
	}
	dhcpUUID := rows[0].UUID

	// First binding.
	if err := c.EnsureDHCPReservation(ctx, portName, dhcpUUID); err != nil {
		t.Fatalf("EnsureDHCPReservation: %v", err)
	}

	// Rebind (simulating a controller restart applying the same desired state):
	// must not duplicate or change anything.
	if err := c.EnsureDHCPReservation(ctx, portName, dhcpUUID); err != nil {
		t.Fatalf("EnsureDHCPReservation (repeat): %v", err)
	}

	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("ports = %d, want 1", len(ports))
	}
	if ports[0].DHCPv4Options == nil || *ports[0].DHCPv4Options != dhcpUUID {
		t.Fatalf("dhcpv4_options = %v, want %q", ports[0].DHCPv4Options, dhcpUUID)
	}

	// The static address is preserved on the port itself.
	if len(ports[0].Addresses) != 1 || ports[0].Addresses[0] != "02:fc:00:00:00:01 10.0.0.10" {
		t.Errorf("addresses = %v, want the static binding", ports[0].Addresses)
	}
}

func TestEnsureDNSRecord_IdempotentAndPruned(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	if err := c.EnsureDNSRecord(ctx, "10.0.0.10", "vm-1.alpha"); err != nil {
		t.Fatalf("EnsureDNSRecord: %v", err)
	}
	if err := c.EnsureDNSRecord(ctx, "10.0.0.10", "vm-1.alpha"); err != nil {
		t.Fatalf("EnsureDNSRecord (repeat): %v", err)
	}

	rows, err := c.ListDNSRecords(ctx)
	if err != nil {
		t.Fatalf("ListDNSRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("DNS rows = %d, want 1", len(rows))
	}
	if len(rows[0].Records) != 2 || rows[0].Records[0] != "10.0.0.10" || rows[0].Records[1] != "vm-1.alpha" {
		t.Fatalf("records = %v, want [10.0.0.10 vm-1.alpha]", rows[0].Records)
	}

	// Renaming the host updates in place instead of adding a row.
	if err := c.EnsureDNSRecord(ctx, "10.0.0.10", "renamed.alpha"); err != nil {
		t.Fatalf("EnsureDNSRecord (rename): %v", err)
	}
	rows, err = c.ListDNSRecords(ctx)
	if err != nil {
		t.Fatalf("ListDNSRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("DNS rows after rename = %d, want 1", len(rows))
	}
	if rows[0].Records[1] != "renamed.alpha" {
		t.Errorf("records = %v, want the renamed host", rows[0].Records)
	}

	// A VM that no longer exists is pruned.
	if err := c.PruneDNSRecords(ctx, map[string]string{}); err != nil {
		t.Fatalf("PruneDNSRecords: %v", err)
	}
	rows, err = c.ListDNSRecords(ctx)
	if err != nil {
		t.Fatalf("ListDNSRecords: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("DNS rows after prune = %d, want 0", len(rows))
	}
}

func TestEnsureDNSRecord_SkipsEmptyInputs(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	if err := c.EnsureDNSRecord(ctx, "", "host"); err != nil {
		t.Fatalf("EnsureDNSRecord(empty ip): %v", err)
	}
	if err := c.EnsureDNSRecord(ctx, "10.0.0.10", ""); err != nil {
		t.Fatalf("EnsureDNSRecord(empty host): %v", err)
	}
	rows, err := c.ListDNSRecords(ctx)
	if err != nil {
		t.Fatalf("ListDNSRecords: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no DNS rows, got %d", len(rows))
	}
}

func TestDropDHCPReservation(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()
	seedNetwork(t, c, ctx, "alpha", "10.0.0.0/24", "10.0.0.1")

	portName := "fc-port-vm-1"
	if err := c.EnsureLogicalSwitchPort(ctx, VMSwitchName("alpha"), LogicalPortSpec{
		Name:      portName,
		Addresses: []string{"02:fc:00:00:00:01 10.0.0.10"},
	}); err != nil {
		t.Fatalf("EnsureLogicalSwitchPort: %v", err)
	}
	if _, err := c.EnsureDHCPOptions(ctx, DHCPSpec{
		Switch: GatewaySwitchName("alpha"), CIDR: "10.0.0.0/24",
	}, "alpha"); err != nil {
		t.Fatalf("EnsureDHCPOptions: %v", err)
	}
	rows, err := c.ListDHCPOptions(ctx)
	if err != nil {
		t.Fatalf("ListDHCPOptions: %v", err)
	}
	if err := c.EnsureDHCPReservation(ctx, portName, rows[0].UUID); err != nil {
		t.Fatalf("EnsureDHCPReservation: %v", err)
	}

	if err := c.DropDHCPReservation(ctx, portName); err != nil {
		t.Fatalf("DropDHCPReservation: %v", err)
	}
	// Idempotent.
	if err := c.DropDHCPReservation(ctx, portName); err != nil {
		t.Fatalf("DropDHCPReservation (repeat): %v", err)
	}

	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ports[0].DHCPv4Options != nil {
		t.Errorf("dhcpv4_options = %q, want cleared (nil)", *ports[0].DHCPv4Options)
	}

	// Dropping an unknown port is a no-op.
	if err := c.DropDHCPReservation(ctx, "fc-port-nope"); err != nil {
		t.Fatalf("DropDHCPReservation(unknown): %v", err)
	}
}

func TestDHCPResolverBuildsPerSubnetRows(t *testing.T) {
	// The desired-state helper the reconciler will use: one DHCP_Options row per
	// network, one reservation per VM, one DNS record per reservation.
	networks := []NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1", DNSServers: []string{"8.8.8.8"}},
		{Name: "beta", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"},
	}
	vms := []VMDefinition{
		{ID: "vm-1", Network: "alpha", MAC: "02:fc:00:00:00:01", IP: "10.0.0.10"},
		{ID: "vm-2", Network: "alpha", MAC: "02:fc:00:00:00:02", IP: "10.0.0.11"},
		{ID: "vm-3", Network: "beta", MAC: "02:fc:00:00:00:03", IP: "10.0.1.10"},
	}

	specs := DHCPSpecsFromState(networks, vms)
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2 (one per subnet)", len(specs))
	}

	alpha := specs[0]
	if alpha.Switch != GatewaySwitchName("alpha") || alpha.CIDR != "10.0.0.0/24" {
		t.Errorf("alpha spec = %+v, want switch %q cidr 10.0.0.0/24", alpha, GatewaySwitchName("alpha"))
	}
	if len(alpha.Reservations) != 2 {
		t.Errorf("alpha reservations = %d, want 2", len(alpha.Reservations))
	}
	for _, r := range alpha.Reservations {
		if r.MAC == "" || r.IP == "" || r.PortName == "" {
			t.Errorf("incomplete reservation %+v", r)
		}
	}
	if len(specs[1].Reservations) != 1 || specs[1].CIDR != "10.0.1.0/24" {
		t.Errorf("beta spec = %+v, want 1 reservation on 10.0.1.0/24", specs[1])
	}
}

func TestDHCPSpecsFromState_IgnoresVMsOnUnknownNetworks(t *testing.T) {
	specs := DHCPSpecsFromState(
		[]NetworkDefinition{{Name: "alpha", CIDR: "10.0.0.0/24"}},
		[]VMDefinition{{ID: "vm-x", Network: "nope", MAC: "02:fc:00:00:00:09", IP: "10.9.9.9"}},
	)
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	if len(specs[0].Reservations) != 0 {
		t.Errorf("reservations = %+v, want none", specs[0].Reservations)
	}
}
