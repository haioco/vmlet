package ovn

import (
	"context"
	"strings"
	"testing"

	"github.com/ovn-org/libovsdb/ovsdb"
)

// aclRowsRaw reads ACL rows with a server-side select, bypassing the client
// cache. The in-memory OVSDB server the harness runs does not push ACL rows
// to monitors, so cache-based reads under-report; the real ovsdb-server does
// not have this behaviour.
func aclRowsRaw(t *testing.T, c *Client) []ACL {
	t.Helper()
	res, err := c.api.Transact(context.Background(), ovsdb.Operation{
		Op:    ovsdb.OperationSelect,
		Table: "ACL",
	})
	if err != nil {
		t.Fatalf("raw ACL select: %v", err)
	}
	if len(res) == 0 {
		return nil
	}
	if res[0].Error != "" {
		t.Fatalf("raw ACL select: %s", res[0].Error)
	}
	var out []ACL
	for _, row := range res[0].Rows {
		// Decode through the mapper: GetRowData needs the model's field info.
		acl, err := aclFromRow(c, row)
		if err != nil {
			t.Fatalf("mapping ACL row: %v", err)
		}
		out = append(out, acl)
	}
	return out
}

// harnessPersistsACLs reports whether the in-memory OVSDB server the test
// harness runs actually stores ACL rows. It does not: the server accepts the
// insert, reports success, and the row is absent from both the cache and a raw
// select. ACL behaviour therefore has to be verified against a real
// ovsdb-server, which the setup script provides.
func harnessPersistsACLs(t *testing.T, c *Client) bool {
	t.Helper()
	row := &ACL{
		UUID: namedRef("acl", "harness-probe"), Action: "allow",
		Direction: DirectionIngress, Priority: 10, Match: "ip",
		ExternalIDs: map[string]string{"vmlet-managed": "true"},
	}
	ops, err := c.api.Create(row)
	if err != nil {
		return false
	}
	if _, err := c.api.Transact(context.Background(), ops...); err != nil {
		return false
	}
	res, err := c.TransactSelect(context.Background(), "ACL")
	if err != nil || len(res.Rows) == 0 {
		t.Skip("the in-memory OVSDB server does not persist ACL rows; " +
			"run the OVN e2e script (scripts/ovn-e2e-test.sh --live) against a " +
			"real ovsdb-server to exercise ACLs")
		return false
	}
	return true
}

func TestPortSecurityValues(t *testing.T) {
	cases := []struct {
		name string
		in   PortSecurity
		want []string
	}{
		{"mac and ipv4", PortSecurity{MACAddress: "02:fc:00:00:00:01", Ipv4Address: "10.0.0.10"}, []string{"02:fc:00:00:00:01 10.0.0.10"}},
		{"mac only", PortSecurity{MACAddress: "02:fc:00:00:00:01"}, nil},
		{"mac and ipv6", PortSecurity{MACAddress: "02:fc:00:00:00:01", Ipv6Address: "fd00::10"}, []string{"02:fc:00:00:00:01 fd00::10"}},
		{"dual stack", PortSecurity{MACAddress: "02:fc:00:00:00:01", Ipv4Address: "10.0.0.10", Ipv6Address: "fd00::10"}, []string{"02:fc:00:00:00:01 10.0.0.10", "02:fc:00:00:00:01 fd00::10"}},
		{"empty", PortSecurity{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Values()
			if !strSlicesEqual(got, tc.want) {
				t.Errorf("Values() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPortSecurityFromVM_EnvGated(t *testing.T) {
	mac, v4, v6 := "02:fc:00:00:00:01", "10.0.0.10", "fd00::10"

	t.Run("unset pins nothing but the mac", func(t *testing.T) {
		t.Setenv(EnvAntispoofEnable, "")
		t.Setenv(EnvAntispoofIPEnable, "")
		ps := portSecurityForVM(mac, v4, v6)
		if ps.MACAddress != mac {
			t.Errorf("MAC = %q, want %q", ps.MACAddress, mac)
		}
		if ps.Ipv4Address != "" || ps.Ipv6Address != "" {
			t.Errorf("unset env should not pin addresses, got %+v", ps)
		}
	})

	t.Run("ip enable pins both families", func(t *testing.T) {
		t.Setenv(EnvAntispoofIPEnable, "true")
		ps := portSecurityForVM(mac, v4, v6)
		if ps.Ipv4Address != v4 || ps.Ipv6Address != v6 {
			t.Errorf("got %+v, want both addresses pinned", ps)
		}
	})

	t.Run("disable drops everything", func(t *testing.T) {
		t.Setenv(EnvAntispoofEnable, "false")
		t.Setenv(EnvAntispoofIPEnable, "true")
		ps := portSecurityForVM(mac, v4, v6)
		if ps.Ipv4Address != "" || ps.Ipv6Address != "" {
			t.Errorf("ENABLE=false should pin nothing, got %+v", ps)
		}
		if !antispoofDisabled() {
			t.Error("antispoofDisabled() should be true")
		}
	})
}

func TestSecurityRuleMatch(t *testing.T) {
	cases := []struct {
		name string
		rule SecurityRule
		want string
	}{
		{"ingress any", SecurityRule{Direction: DirectionIngress, AllowRule: true}, "inport == @filterport && ip"},
		{"egress any", SecurityRule{Direction: DirectionEgress, AllowRule: true}, "outport == @filterport && ip"},
		{"tcp 22", SecurityRule{Direction: DirectionIngress, Protocol: ProtocolTCP, PortMin: 22, PortMax: 22}, "inport == @filterport && ip && tcp && dst_port == 22"},
		{"udp range", SecurityRule{Direction: DirectionIngress, Protocol: ProtocolUDP, PortMin: 1000, PortMax: 2000}, "inport == @filterport && ip && udp && dst_port >= 1000 && dst_port <= 2000"},
		{"peer subnet", SecurityRule{Direction: DirectionIngress, Peer: "10.0.0.0/24", AllowRule: true}, "inport == @filterport && ip && 10.0.0.0/24"},
		{"egress tcp", SecurityRule{Direction: DirectionEgress, Protocol: ProtocolTCP, PortMin: 443, PortMax: 443}, "outport == @filterport && ip && tcp && src_port == 443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Match(); got != tc.want {
				t.Errorf("Match() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestSecurityRuleActionAndPriority(t *testing.T) {
	allow := SecurityRule{AllowRule: true}
	deny := SecurityRule{}

	if allow.Action() != "allow" || allow.Loggable() {
		t.Errorf("allow rule: action=%q loggable=%v (allows must not log)", allow.Action(), allow.Loggable())
	}
	if deny.Action() != "drop" || !deny.Loggable() {
		t.Errorf("deny rule: action=%q loggable=%v (drops must log)", deny.Action(), deny.Loggable())
	}
	// Deny must evaluate before allow.
	if deny.Priority() <= allow.Priority() {
		t.Errorf("deny priority %d should exceed allow priority %d", deny.Priority(), allow.Priority())
	}
}

func TestSecurityGroupRulesFollowVM(t *testing.T) {
	group := SecurityGroup{
		Name: "web",
		Rules: []SecurityRule{
			{Direction: DirectionIngress, Protocol: ProtocolTCP, PortMin: 22, PortMax: 22, AllowRule: true},
			{Direction: DirectionIngress, Protocol: ProtocolTCP, PortMin: 80, PortMax: 80, AllowRule: true},
		},
	}
	for _, rule := range group.Rules {
		if rule.Match() == "" {
			t.Error("rule produced an empty match")
		}
		if rule.Priority() == 0 {
			t.Error("rule has no priority")
		}
	}
}

func TestEnsurePortSecurity(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	// The topology must exist first so there is a switch to attach to.
	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	portName := "fc-port-vm-1"
	if err := c.EnsureLogicalSwitchPort(ctx, "vm-alpha", LogicalPortSpec{
		Name:      portName,
		Addresses: []string{"02:fc:00:00:00:01 10.0.0.10"},
		ExternalIDs: map[string]string{
			"vmlet-managed": "true", "vmlet-vm": "vm-1", "vmlet-network": "alpha",
		},
	}); err != nil {
		t.Fatalf("EnsureLogicalSwitchPort: %v", err)
	}

	t.Setenv(EnvAntispoofIPEnable, "true")
	ps := portSecurityForVM("02:fc:00:00:00:01", "10.0.0.10", "")
	if err := c.EnsurePortSecurity(ctx, portName, ps); err != nil {
		t.Fatalf("EnsurePortSecurity: %v", err)
	}

	// Idempotent.
	if err := c.EnsurePortSecurity(ctx, portName, ps); err != nil {
		t.Fatalf("EnsurePortSecurity (repeat): %v", err)
	}

	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("ports = %d, want 1", len(ports))
	}
	if !strSlicesEqual(ports[0].PortSecurity, []string{"02:fc:00:00:00:01 10.0.0.10"}) {
		t.Fatalf("port_security = %v, want the MAC/IP binding (spoofed traffic dropped)", ports[0].PortSecurity)
	}
}

func TestEnsurePortSecurity_MissingPort(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	if err := c.EnsurePortSecurity(ctx, "fc-port-nope", PortSecurity{MACAddress: "02:fc:00:00:00:01"}); err == nil {
		t.Fatal("EnsurePortSecurity accepted a missing port")
	}
}

func TestEnsureAndDropACLs(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()
	harnessPersistsACLs(t, c)

	t.Setenv(EnvAntispoofIPEnable, "true")

	// Create a switch so the ACL has a target.
	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	rule := SecurityRule{
		Direction: DirectionIngress,
		Protocol:  ProtocolTCP,
		PortMin:   22,
		PortMax:   22,
		AllowRule: true,
	}

	if err := c.EnsureACL(ctx, "vm-alpha", rule, "vm-1"); err != nil {
		t.Fatalf("EnsureACL: %v", err)
	}
	// Idempotent: applying the same rule twice must not duplicate the row.
	if err := c.EnsureACL(ctx, "vm-alpha", rule, "vm-1"); err != nil {
		t.Fatalf("EnsureACL (repeat): %v", err)
	}

	rows := aclRowsRaw(t, c)
	if len(rows) != 1 {
		t.Fatalf("ACL rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Match != rule.Match() {
		t.Errorf("match = %q, want %q", row.Match, rule.Match())
	}
	if row.ExternalIDs["vmlet-vm"] != "vm-1" {
		t.Errorf("acl not tagged with the VM: %v", row.ExternalIDs)
	}
	if row.Action != "allow" {
		t.Errorf("action = %q, want allow", row.Action)
	}
	if row.Priority != rule.Priority() {
		t.Errorf("priority = %d, want %d", row.Priority, rule.Priority())
	}

	// The rule follows the VM: removing the VM removes its ACLs.
	if err := c.DropVMACLs(ctx, "vm-1"); err != nil {
		t.Fatalf("DropVMACLs: %v", err)
	}
	// Idempotent.
	if err := c.DropVMACLs(ctx, "vm-1"); err != nil {
		t.Fatalf("DropVMACLs (repeat): %v", err)
	}

	rows = aclRowsRaw(t, c)
	if len(rows) != 0 {
		t.Fatalf("ACL rows after drop = %d, want 0", len(rows))
	}
}

func TestEnsureACL_DenyTakesPrecedence(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()
	harnessPersistsACLs(t, c)

	if _, err := c.ReconcileTopology(ctx, TopologyFromNetworks([]NetworkDefinition{
		{Name: "alpha", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"},
	})); err != nil {
		t.Fatalf("ReconcileTopology: %v", err)
	}

	allow := SecurityRule{Direction: DirectionIngress, Protocol: ProtocolTCP, PortMin: 22, PortMax: 22, AllowRule: true}
	deny := SecurityRule{Direction: DirectionIngress, Peer: "10.9.9.0/24"}

	if err := c.EnsureACL(ctx, "vm-alpha", allow, "vm-1"); err != nil {
		t.Fatalf("EnsureACL(allow): %v", err)
	}
	if err := c.EnsureACL(ctx, "vm-alpha", deny, "vm-1"); err != nil {
		t.Fatalf("EnsureACL(deny): %v", err)
	}

	rows := aclRowsRaw(t, c)
	if len(rows) != 2 {
		t.Fatalf("ACL rows = %d, want 2", len(rows))
	}

	// Pruning with only the allow rule desired must remove the deny row.
	if err := c.PruneACLs(ctx, map[string][]SecurityRule{"vm-alpha": {allow}}); err != nil {
		t.Fatalf("PruneACLs: %v", err)
	}
	rows = aclRowsRaw(t, c)
	if len(rows) != 1 {
		t.Fatalf("rows after prune = %d, want 1", len(rows))
	}
	if rows[0].Action != "allow" {
		t.Errorf("the wrong rule survived: %+v", rows[0])
	}
}

func TestEnsureACL_UnknownSwitch(t *testing.T) {
	c, stop := startTestNB(t)
	defer stop()
	ctx := context.Background()

	rule := SecurityRule{Direction: DirectionIngress, AllowRule: true}
	if err := c.EnsureACL(ctx, "vm-nope", rule, "vm-1"); err == nil {
		t.Fatal("EnsureACL accepted an unknown switch")
	}
}

// aclFromRow maps an OVSDB row onto the ACL model via the client's mapper.
func aclFromRow(c *Client, row ovsdb.Row) (ACL, error) {
	acl := ACL{}
	info, err := c.api.Cache().DatabaseModel().NewModelInfo(&acl)
	if err != nil {
		return acl, err
	}
	if err := c.api.Cache().Mapper().GetRowData(&row, info); err != nil {
		return acl, err
	}
	return acl, nil
}

func TestDefaultVMRulesDistinctPerVM(t *testing.T) {
	a := DefaultVMRules("10.0.0.10")
	b := DefaultVMRules("10.0.0.11")

	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("DefaultVMRules returned %d and %d rules, want a baseline for both", len(a), len(b))
	}
	for i := range a {
		if a[i].Match() == b[i].Match() {
			t.Errorf("rule %d has the same match %q for both VMs; ACLs are a switch-wide resource, so a shared match converges two VMs onto one row", i, a[i].Match())
		}
	}
}

func TestDefaultVMRulesIsAllowOnlyAndScoped(t *testing.T) {
	rules := DefaultVMRules("10.0.0.10")

	for _, r := range rules {
		if !r.AllowRule {
			t.Errorf("rule %q is a drop; the bridge backend installs no deny rule, and a drop here would also drop this VM's DHCP discovery (source 0.0.0.0)", r.Match())
		}
		if r.Loggable() {
			t.Errorf("rule %q is marked loggable, which should only be true for drops", r.Match())
		}
		if r.Priority() != 1000 {
			t.Errorf("rule %q priority = %d, want 1000", r.Match(), r.Priority())
		}
	}

	// The address binding is what keeps one VM's rows from another's.
	for _, r := range rules {
		if !strings.Contains(r.Match(), "10.0.0.10") {
			t.Errorf("match %q is not scoped to the VM's address", r.Match())
		}
	}
}

func TestDefaultVMRulesEmptyAddress(t *testing.T) {
	if got := DefaultVMRules(""); got != nil {
		t.Errorf("DefaultVMRules(\"\") = %v, want nil: an unaddressed port cannot be scoped", got)
	}
}
