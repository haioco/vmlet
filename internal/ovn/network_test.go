package ovn

import (
	"context"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"customer-3":   "customer-3",
		"Customer_3":   "customer-3",
		"net one":      "net-one",
		"net--one":     "net-one",
		"tenant/infra": "tenant-infra",
		"":             "vm-default",
		"---":          "vm-default",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTopologyNamesAreStableAndUnique(t *testing.T) {
	net := "Customer_3"
	vm, gw, router, lrp := VMSwitchName(net), GatewaySwitchName(net),
		GatewayRouterName(net), GatewayRouterPortName(net)

	want := map[string]string{
		vm:     "vm-customer-3",
		gw:     "gw-customer-3",
		router: "lr-customer-3",
		lrp:    "lrp-customer-3",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("name = %q, want %q", got, expected)
		}
	}

	// All four names must differ so no switch collides with a router.
	seen := map[string]bool{}
	for _, name := range []string{vm, gw, router, lrp} {
		if seen[name] {
			t.Errorf("duplicate derived name %q", name)
		}
		seen[name] = true
	}
}

func TestTopologyFromNetworks_IsolatesNetworks(t *testing.T) {
	topology := TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"},
		{Name: "beta", CIDR: "10.0.2.0/24", GatewayIP: "10.0.2.1"},
	})

	if len(topology.Routers) != 2 {
		t.Fatalf("routers = %d, want 2", len(topology.Routers))
	}

	// A network's switch only ever carries that network's external_id.
	byName := map[string]LogicalSwitchSpec{}
	for _, sw := range topology.Switches {
		byName[sw.Name] = sw
		if sw.ExternalIDs["vmlet-network"] == "" {
			t.Errorf("switch %q has no vmlet-network external id", sw.Name)
		}
		if sw.ExternalIDs["vmlet-managed"] != "true" {
			t.Errorf("switch %q is not marked vmlet-managed", sw.Name)
		}
	}

	alpha := byName[VMSwitchName("alpha")]
	if alpha.Name == "" {
		t.Fatalf("alpha VM switch missing")
	}
	for _, port := range alpha.Ports {
		if port.ExternalIDs["vmlet-network"] != "" && port.ExternalIDs["vmlet-network"] != "alpha" {
			t.Errorf("alpha switch carries port for network %q", port.ExternalIDs["vmlet-network"])
		}
	}
}

func TestTopologyFromNetworks_RouterPortsPerNetwork(t *testing.T) {
	topology := TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"},
	})

	found := false
	for _, sw := range topology.Switches {
		if sw.Name != GatewaySwitchName("alpha") {
			continue
		}
		for _, port := range sw.Ports {
			if port.Type != PortTypeRouter {
				t.Errorf("gateway switch port %q has type %q, want %q", port.Name, port.Type, PortTypeRouter)
			}
			if port.Options["router-port"] != GatewayRouterPortName("alpha") {
				t.Errorf("port %q targets router port %q, want %q",
					port.Name, port.Options["router-port"], GatewayRouterPortName("alpha"))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no router-typed port on the gateway switch")
	}
}

func TestTopologyFromNetworks_SkipsUnnamed(t *testing.T) {
	topology := TopologyFromNetworks([]NetworkDefinition{
		{Name: "", CIDR: "10.0.0.0/24"},
	})
	if len(topology.Switches) != 0 || len(topology.Routers) != 0 {
		t.Fatalf("expected an empty topology, got %+v", topology)
	}
}

func TestGatewayAddress(t *testing.T) {
	cases := map[string]string{
		"10.0.0.1":    "10.0.0.1/24",
		"10.0.0.1/24": "10.0.0.1/24",
		"":            "",
	}
	for in, want := range cases {
		if got := gatewayAddress(in); got != want {
			t.Errorf("gatewayAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTopologyFromVMState_PlacesVMsOnTheirNetwork(t *testing.T) {
	topology := TopologyFromVMState(
		[]NetworkDefinition{
			{Name: "alpha", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"},
			{Name: "beta", CIDR: "10.0.2.0/24", GatewayIP: "10.0.2.1"},
		},
		[]VMDefinition{
			{ID: "vm-a", Network: "alpha", MAC: "02:fc:00:00:00:01", IP: "10.0.1.10"},
			{ID: "vm-b", Network: "beta", MAC: "02:fc:00:00:00:02", IP: "10.0.2.10"},
		},
	)

	byName := map[string]LogicalSwitchSpec{}
	for _, sw := range topology.Switches {
		byName[sw.Name] = sw
	}

	alphaPorts := byName[VMSwitchName("alpha")].Ports
	if len(alphaPorts) != 1 {
		t.Fatalf("alpha switch ports = %d, want 1", len(alphaPorts))
	}
	if alphaPorts[0].Name != "fc-port-vm-a" {
		t.Errorf("port name = %q, want fc-port-vm-a", alphaPorts[0].Name)
	}
	if len(alphaPorts[0].Addresses) != 1 || alphaPorts[0].Addresses[0] != "02:fc:00:00:00:01 10.0.1.10" {
		t.Errorf("addresses = %v, want [02:fc:00:00:00:01 10.0.1.10]", alphaPorts[0].Addresses)
	}
	if alphaPorts[0].ExternalIDs["vmlet-vm"] != "vm-a" {
		t.Errorf("vm external id = %q, want vm-a", alphaPorts[0].ExternalIDs["vmlet-vm"])
	}

	betaPorts := byName[VMSwitchName("beta")].Ports
	if len(betaPorts) != 1 || betaPorts[0].ExternalIDs["vmlet-vm"] != "vm-b" {
		t.Fatalf("beta ports = %+v, want the vm-b port", betaPorts)
	}

	// The beta VM must not appear on alpha's switch (isolation).
	for _, port := range alphaPorts {
		if port.ExternalIDs["vmlet-vm"] == "vm-b" {
			t.Error("vm-b leaked onto the alpha switch")
		}
	}
}

func TestTopologyFromVMState_VMOnUnknownNetworkIgnored(t *testing.T) {
	topology := TopologyFromVMState(
		[]NetworkDefinition{{Name: "alpha", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"}},
		[]VMDefinition{{ID: "vm-x", Network: "does-not-exist", MAC: "02:fc:00:00:00:09"}},
	)

	for _, sw := range topology.Switches {
		if sw.Name == VMSwitchName("does-not-exist") {
			t.Error("a switch was created for an unknown network")
		}
		for _, port := range sw.Ports {
			if port.ExternalIDs["vmlet-vm"] == "vm-x" {
				t.Errorf("vm-x was placed on switch %q", sw.Name)
			}
		}
	}
}

func TestReconcileTopology_TwoNetworksAreRouted(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	desired := TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"},
		{Name: "beta", CIDR: "10.0.2.0/24", GatewayIP: "10.0.2.1"},
	})

	if _, err := c.ReconcileTopology(ctx, desired); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	if len(switches) != 4 {
		t.Fatalf("switches = %d, want 4 (vm+gw per network)", len(switches))
	}

	routers, err := c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	if len(routers) != 2 {
		t.Fatalf("routers = %d, want 2", len(routers))
	}

	// Idempotent: applying the same topology again changes nothing.
	second, err := c.ReconcileTopology(ctx, desired)
	if err != nil {
		t.Fatalf("second ReconcileTopology: %v", err)
	}
	if second.Created != 0 || second.Deleted != 0 {
		t.Fatalf("second reconcile changed state: %+v", second)
	}
}
