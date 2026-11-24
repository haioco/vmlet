package ovn

import (
	"context"
	"testing"
)

func TestReconcileTopology_CreatesDesiredState(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	desired := Topology{
		Switches: []LogicalSwitchSpec{
			{
				Name:        "vm-net-a",
				ExternalIDs: map[string]string{"vmlet-managed": "true"},
				Ports: []LogicalPortSpec{
					{Name: "lsp-vm-1", Addresses: []string{"aa:bb:cc:00:00:01 10.0.0.2"}},
					{Name: "lsp-vm-2", Addresses: []string{"aa:bb:cc:00:00:02 10.0.0.3"}},
				},
			},
		},
		Routers: []LogicalRouterSpec{
			{Name: "lr-provider", ExternalIDs: map[string]string{"vmlet-managed": "true"}},
		},
	}

	result, err := c.ReconcileTopology(ctx, desired)
	if err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}
	if result.Created != 4 || result.Deleted != 0 {
		t.Fatalf("result = %+v, want 4 created, 0 deleted (2 ports, 1 switch, 1 router)", result)
	}

	assertTopology(t, c, desired)
}

func TestReconcileTopology_IsIdempotent(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	desired := Topology{
		Switches: []LogicalSwitchSpec{
			{
				Name:        "vm-idem",
				ExternalIDs: map[string]string{"vmlet-managed": "true"},
				Ports: []LogicalPortSpec{
					{Name: "lsp-a", Addresses: []string{"aa:bb:cc:00:00:01 10.0.0.2"}},
					{Name: "lsp-b", Type: "", Addresses: []string{"aa:bb:cc:00:00:02 10.0.0.3"}},
				},
			},
		},
	}

	if _, err := c.ReconcileTopology(ctx, desired); err != nil {
		t.Fatalf("first ReconcileTopology: %v", err)
	}

	second, err := c.ReconcileTopology(ctx, desired)
	if err != nil {
		t.Fatalf("second ReconcileTopology: %v", err)
	}
	if second.Created != 0 || second.Deleted != 0 {
		t.Fatalf("second reconcile changed state: %+v", second)
	}
	assertTopology(t, c, desired)
}

func TestReconcileTopology_UpdatesDriftedPorts(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	base := Topology{
		Switches: []LogicalSwitchSpec{{
			Name:        "vm-drift",
			ExternalIDs: map[string]string{"vmlet-managed": "true"},
			Ports: []LogicalPortSpec{
				{Name: "lsp-a", Addresses: []string{"aa:bb:cc:00:00:01 10.0.0.2"}},
			},
		}},
	}
	if _, err := c.ReconcileTopology(ctx, base); err != nil {
		t.Fatalf("initial ReconcileTopology: %v", err)
	}

	drifted := Topology{
		Switches: []LogicalSwitchSpec{{
			Name:        "vm-drift",
			ExternalIDs: map[string]string{"vmlet-managed": "true"},
			Ports: []LogicalPortSpec{
				{Name: "lsp-a", Addresses: []string{"aa:bb:cc:00:00:09 10.0.0.9"}, Options: map[string]string{"foo": "bar"}},
			},
		}},
	}
	if _, err := c.ReconcileTopology(ctx, drifted); err != nil {
		t.Fatalf("drifted ReconcileTopology: %v", err)
	}
	assertTopology(t, c, drifted)
}

func TestReconcileTopology_PrunesUndesiredRows(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	first := Topology{
		Switches: []LogicalSwitchSpec{
			{
				Name:        "vm-keep",
				ExternalIDs: map[string]string{"vmlet-managed": "true"},
				Ports:       []LogicalPortSpec{{Name: "lsp-keep", Addresses: []string{"aa:bb:cc:00:00:01 10.0.0.2"}}},
			},
			{
				Name:        "vm-drop",
				ExternalIDs: map[string]string{"vmlet-managed": "true"},
				Ports:       []LogicalPortSpec{{Name: "lsp-drop", Addresses: []string{"aa:bb:cc:00:00:02 10.0.0.3"}}},
			},
		},
		Routers: []LogicalRouterSpec{{Name: "lr-drop", ExternalIDs: map[string]string{"vmlet-managed": "true"}}},
	}
	if _, err := c.ReconcileTopology(ctx, first); err != nil {
		t.Fatalf("initial ReconcileTopology: %v", err)
	}

	pruned := Topology{
		Switches: []LogicalSwitchSpec{{
			Name:        "vm-keep",
			ExternalIDs: map[string]string{"vmlet-managed": "true"},
			Ports:       []LogicalPortSpec{{Name: "lsp-keep", Addresses: []string{"aa:bb:cc:00:00:01 10.0.0.2"}}},
		}},
	}
	result, err := c.ReconcileTopology(ctx, pruned)
	if err != nil {
		t.Fatalf("prune ReconcileTopology: %v", err)
	}
	if result.Deleted < 3 {
		t.Fatalf("result = %+v, want at least 3 deletions (port, switch, router)", result)
	}
	assertTopology(t, c, pruned)
}

func TestClient_initialState(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	if len(switches) != 0 {
		t.Fatalf("expected empty database, got %d switches", len(switches))
	}
	if got := c.Schema().Name; got != NorthboundDatabase {
		t.Fatalf("schema name = %q, want %q", got, NorthboundDatabase)
	}
}

func TestNewClient_defaultsEndpoint(t *testing.T) {
	cfg := Config{}
	if cfg.Endpoint != "" {
		t.Skip("explicit endpoint configured")
	}
	if got := SelectNBEndpoint(""); got != DefaultNBEndpoint {
		t.Fatalf("SelectNBEndpoint() = %q, want %q", got, DefaultNBEndpoint)
	}
}

// assertTopology verifies the live database matches the desired topology.
func assertTopology(t *testing.T, c *Client, want Topology) {
	t.Helper()
	ctx := context.Background()

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	routers, err := c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	ports, err := c.ListSwitchPorts(ctx)
	if err != nil {
		t.Fatalf("ListSwitchPorts: %v", err)
	}

	if len(switches) != len(want.Switches) {
		t.Fatalf("switches = %d, want %d", len(switches), len(want.Switches))
	}
	if len(routers) != len(want.Routers) {
		t.Fatalf("routers = %d, want %d", len(routers), len(want.Routers))
	}

	wantPorts := 0
	for _, sw := range want.Switches {
		wantPorts += len(sw.Ports)
	}
	if len(ports) != wantPorts {
		t.Fatalf("ports = %d, want %d", len(ports), wantPorts)
	}

	byName := map[string]LogicalSwitch{}
	for _, sw := range switches {
		byName[sw.Name] = sw
	}
	portsByName := map[string]LogicalSwitchPort{}
	for _, p := range ports {
		portsByName[p.Name] = p
	}

	for _, sw := range want.Switches {
		got, ok := byName[sw.Name]
		if !ok {
			t.Fatalf("logical switch %q missing", sw.Name)
		}
		if !mapsEqual(got.ExternalIDs, sw.ExternalIDs) {
			t.Fatalf("switch %q external_ids = %v, want %v", sw.Name, got.ExternalIDs, sw.ExternalIDs)
		}
		if len(got.Ports) != len(sw.Ports) {
			t.Fatalf("switch %q references %d ports, want %d", sw.Name, len(got.Ports), len(sw.Ports))
		}
		for _, p := range sw.Ports {
			gotPort, ok := portsByName[p.Name]
			if !ok {
				t.Fatalf("port %q missing", p.Name)
			}
			if !p.equalModel(&gotPort) {
				t.Fatalf("port %q = %+v, want %s", p.Name, gotPort, p.Addresses)
			}
			if !containsString(got.Ports, gotPort.UUID) {
				t.Fatalf("switch %q does not reference port %q", sw.Name, p.Name)
			}
		}
	}
}
