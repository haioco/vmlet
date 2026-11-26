package ovn

import (
	"context"
	"testing"
)

func TestNATKeyAndIndex(t *testing.T) {
	key := natKey(NATTypeDNAT, "54.38.119.252", "10.0.0.10", "")
	if key != "dnat|54.38.119.252|10.0.0.10|" {
		t.Errorf("natKey = %q", key)
	}

	rows := []NAT{
		{Type: NATTypeDNAT, ExternalIP: "54.38.119.252", LogicalIP: "10.0.0.10"},
		{Type: NATTypeSNAT, ExternalIP: "54.38.119.252", LogicalIP: "10.0.0.0/24"},
	}
	idx := indexNATs(rows)
	if len(idx) != 2 {
		t.Fatalf("index has %d entries, want 2", len(idx))
	}
	// dnat and snat for the same external IP must not collide.
	if _, ok := idx[natKey(NATTypeDNAT, "54.38.119.252", "10.0.0.10", "")]; !ok {
		t.Error("dnat entry missing")
	}
	if _, ok := idx[natKey(NATTypeSNAT, "54.38.119.252", "10.0.0.0/24", "")]; !ok {
		t.Error("snat entry missing")
	}
}

func TestFloatingIPToNAT(t *testing.T) {
	f := FloatingIP{ExternalIP: "54.38.119.252", InternalIP: "10.0.0.10", MAC: "02:fc:00:00:00:01", Router: "lr-alpha"}
	n := f.toNAT("vm-1")

	if n.Type != NATTypeDNAT {
		t.Errorf("type = %q, want dnat", n.Type)
	}
	if n.ExternalIP != "54.38.119.252" || n.LogicalIP != "10.0.0.10" {
		t.Errorf("addresses = %q -> %q", n.ExternalIP, n.LogicalIP)
	}
	if n.ExternalMAC == nil || *n.ExternalMAC != "02:fc:00:00:00:01" {
		t.Errorf("external_mac = %v, want the VM MAC so OVN answers ARP", n.ExternalMAC)
	}
	if n.ExternalIDs["vmlet-vm"] != "vm-1" || n.ExternalIDs["vmlet-managed"] != "true" {
		t.Errorf("external_ids = %v", n.ExternalIDs)
	}
}

func TestSNATRuleToNAT(t *testing.T) {
	s := SNATRule{Router: "lr-alpha", LogicalIP: "10.0.0.0/24", ExternalIP: "54.38.119.252"}
	n := s.toNAT()

	if n.Type != NATTypeSNAT {
		t.Errorf("type = %q, want snat", n.Type)
	}
	if n.LogicalIP != "10.0.0.0/24" || n.ExternalIP != "54.38.119.252" {
		t.Errorf("addresses = %q -> %q", n.LogicalIP, n.ExternalIP)
	}
	// SNAT is masquerade: no per-VM MAC needed.
	if n.ExternalMAC != nil {
		t.Errorf("external_mac = %v, want nil for snat", n.ExternalMAC)
	}
}

func TestReconcileNAT_AttachDetach(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	router := "lr-alpha"
	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	floating := []FloatingIP{
		{ExternalIP: "54.38.119.252", InternalIP: "10.0.0.10", MAC: "02:fc:00:00:00:01", Router: router},
	}

	// Attach.
	result, err := c.ReconcileNAT(ctx, floating, nil)
	if err != nil {
		t.Fatalf("ReconcileNAT (attach): %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	rows, err := c.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("NAT rows = %d, want 1", len(rows))
	}
	if rows[0].Type != NATTypeDNAT {
		t.Errorf("type = %q, want dnat", rows[0].Type)
	}

	// The router must reference the new row (a rule that is not installed on
	// the router does nothing).
	routers, err := c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	var target *LogicalRouter
	for i := range routers {
		if routers[i].Name == router {
			target = &routers[i]
			break
		}
	}
	if target == nil {
		t.Fatal("router missing")
	}
	if !containsString(target.NAT, rows[0].UUID) {
		t.Errorf("router does not reference the NAT row: %v", target.NAT)
	}

	// Idempotent: applying the same addon again must be a no-op.
	again, err := c.ReconcileNAT(ctx, floating, nil)
	if err != nil {
		t.Fatalf("ReconcileNAT (repeat): %v", err)
	}
	if again.Created != 0 || again.Updated != 0 || again.Deleted != 0 {
		t.Fatalf("second reconcile changed state: %+v", again)
	}

	// Detach: dropping the addon removes the row and the router reference.
	detached, err := c.ReconcileNAT(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ReconcileNAT (detach): %v", err)
	}
	if detached.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", detached.Deleted)
	}

	rows, err = c.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("NAT rows after detach = %d, want 0", len(rows))
	}

	routers, err = c.ListRouters(ctx)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	for i := range routers {
		for _, ref := range routers[i].NAT {
			if ref == "" {
				continue
			}
			t.Errorf("router %q still references a deleted NAT row: %q", routers[i].Name, ref)
		}
	}
}

func TestReconcileNAT_MultipleAddonsAndSNAT(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	router := "lr-alpha"
	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	floating := []FloatingIP{
		{ExternalIP: "54.38.119.252", InternalIP: "10.0.0.10", MAC: "02:fc:00:00:00:01", Router: router},
		{ExternalIP: "54.38.119.253", InternalIP: "10.0.0.11", MAC: "02:fc:00:00:00:02", Router: router},
	}
	snats := []SNATRule{{Router: router, LogicalIP: "10.0.0.0/24", ExternalIP: "54.38.119.254"}}

	result, err := c.ReconcileNAT(ctx, floating, snats)
	if err != nil {
		t.Fatalf("ReconcileNAT: %v", err)
	}
	if result.Created != 3 {
		t.Fatalf("created = %d, want 3 (2 dnat + 1 snat)", result.Created)
	}

	rows, err := c.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	dnat, snat := 0, 0
	for _, row := range rows {
		switch row.Type {
		case NATTypeDNAT:
			dnat++
		case NATTypeSNAT:
			snat++
		}
	}
	if dnat != 2 || snat != 1 {
		t.Fatalf("dnat = %d, snat = %d, want 2 and 1", dnat, snat)
	}

	// Removing one floating IP leaves the other rows and the SNAT intact.
	pruned, err := c.ReconcileNAT(ctx, floating[:1], snats)
	if err != nil {
		t.Fatalf("ReconcileNAT: %v", err)
	}
	if pruned.Deleted != 1 || pruned.Created != 0 {
		t.Fatalf("prune result = %+v, want 1 deletion", pruned)
	}

	rows, err = c.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("NAT rows = %d, want 2", len(rows))
	}
}

func TestReconcileNAT_SkipsIncompleteAndUnknownRouter(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	// Incomplete entries are ignored rather than creating half a rule.
	result, err := c.ReconcileNAT(ctx, []FloatingIP{
		{ExternalIP: "", InternalIP: "10.0.0.10", Router: "lr-alpha"},
		{ExternalIP: "54.38.119.252", InternalIP: "", Router: "lr-alpha"},
	}, nil)
	if err != nil {
		t.Fatalf("ReconcileNAT: %v", err)
	}
	if result.Created != 0 {
		t.Errorf("created = %d, want 0 for incomplete entries", result.Created)
	}

	// A rule on a router that does not exist is a loud error.
	if _, err := c.ReconcileNAT(ctx, []FloatingIP{
		{ExternalIP: "54.38.119.252", InternalIP: "10.0.0.10", Router: "lr-nope"},
	}, nil); err == nil {
		t.Fatal("ReconcileNAT accepted an unknown router")
	}
}

func TestReconcileNAT_SameExternalIPForDNATAndSNAT(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	router := "lr-alpha"
	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	// A common OVN setup: the same public IP is DNAT'd to one VM and SNAT's
	// the rest of the subnet. Both rows must coexist.
	if _, err := c.ReconcileNAT(ctx,
		[]FloatingIP{{ExternalIP: "54.38.119.252", InternalIP: "10.0.0.10", MAC: "02:fc:00:00:00:01", Router: router}},
		[]SNATRule{{Router: router, LogicalIP: "10.0.0.0/24", ExternalIP: "54.38.119.252"}},
	); err != nil {
		t.Fatalf("ReconcileNAT: %v", err)
	}

	rows, err := c.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("NAT rows = %d, want 2 (dnat + snat on the same IP)", len(rows))
	}
}
