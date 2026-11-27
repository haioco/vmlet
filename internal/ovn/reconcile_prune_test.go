package ovn

import (
	"context"
	"testing"
)

// TestReconcileTopologyDoesNotDeleteUnknownSwitches is the regression test for a
// destructive bug: the reconciler deleted every logical switch and router that
// was not in its desired set. Because the desired set is only as wide as the
// networks this process knows about, a host serving one VPC deleted another
// VPC's switch and router - taking that subnet and every VM on it down.
func TestReconcileTopologyDoesNotDeleteUnknownSwitches(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	seedSwitch := &LogicalSwitch{
		UUID: "sw-other", Name: "vm-other",
		ExternalIDs: map[string]string{"vmlet-managed": "true", "vmlet-network": "other"},
	}
	seedSwitchOps, err := c.api.Create(seedSwitch)
	if err != nil {
		t.Fatalf("building seed switch: %v", err)
	}
	if _, err := c.api.Transact(ctx, seedSwitchOps...); err != nil {
		t.Fatalf("seeding switch: %v", err)
	}
	seedRouter := &LogicalRouter{
		Name:        "lr-other",
		ExternalIDs: map[string]string{"vmlet-managed": "true", "vmlet-network": "other"},
	}
	seedRouterOps, err := c.api.Create(seedRouter)
	if err != nil {
		t.Fatalf("building seed router: %v", err)
	}
	if _, err := c.api.Transact(ctx, seedRouterOps...); err != nil {
		t.Fatalf("seeding router: %v", err)
	}

	// Prove the seeds really landed, so "not found later" means deleted rather
	// than never created. The in-memory server does not persist everything.
	seeded, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	if !hasName(seeded, "vm-other") {
		t.Fatalf("seeded switch vm-other did not persist; cannot assert pruning behaviour")
	}
	seededRouters, err := c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	if !hasRouterName(seededRouters, "lr-other") {
		t.Fatalf("seeded router lr-other did not persist; cannot assert pruning behaviour")
	}

	// Desired state mentions only "default": this host does not serve "other".
	desired := TopologyFromNetworks([]NetworkDefinition{
		{Name: "default", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})
	if desired.Prune {
		t.Fatal("TopologyFromNetworks defaulted to Prune=true; pruning must be opt-in")
	}
	if _, err := c.ReconcileTopology(ctx, desired); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	if !hasName(switches, "vm-other") {
		t.Error("ReconcileTopology deleted a switch outside its desired set")
	}

	routers, err := c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	if !hasRouterName(routers, "lr-other") {
		t.Error("ReconcileTopology deleted a router outside its desired set")
	}
}

// TestReconcileTopologyPrunesWhenOptedIn proves the opt-in still works, so
// operators who want the old clean-up behaviour can have it deliberately.
func TestReconcileTopologyPrunesWhenOptedIn(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	seed := &LogicalSwitch{
		UUID: "sw-other", Name: "vm-other",
		ExternalIDs: map[string]string{"vmlet-managed": "true", "vmlet-network": "other"},
	}
	seedOps, err := c.api.Create(seed)
	if err != nil {
		t.Fatalf("building seed: %v", err)
	}
	if _, err := c.api.Transact(ctx, seedOps...); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	desired := TopologyFromNetworks([]NetworkDefinition{
		{Name: "default", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})
	desired.Prune = true
	if _, err := c.ReconcileTopology(ctx, desired); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		t.Fatalf("ListSwitches: %v", err)
	}
	if hasName(switches, "vm-other") {
		t.Error("Prune=true did not delete the unwanted switch")
	}
}

func hasName(switches []LogicalSwitch, name string) bool {
	for _, s := range switches {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasRouterName(routers []LogicalRouter, name string) bool {
	for _, r := range routers {
		if r.Name == name {
			return true
		}
	}
	return false
}
