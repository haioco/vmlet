package storage

import (
	"path/filepath"
	"testing"

	"github.com/haioco/vmlet/internal/models"
)

// TestSchemaInitWouldHaveCaughtTheMatchColumn is here because it did not: the
// vm_security_rules table shipped with a PRIMARY KEY naming a column that did
// not exist, and schema init is only exercised at process start, so no unit
// test covered it and the service crash-looped on a fresh database.
func TestSchemaInitOnFreshDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vms-state.db")
	st, err := NewSQLiteStorage(db)
	if err != nil {
		t.Fatalf("NewSQLiteStorage on a fresh database: %v", err)
	}
	defer st.Close()

	if err := st.SaveNetwork(&models.Network{ID: "n1", Name: "default", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1"}); err != nil {
		t.Fatalf("SaveNetwork: %v", err)
	}
	got, err := st.LoadNetwork("n1")
	if err != nil {
		t.Fatalf("LoadNetwork: %v", err)
	}
	if got.CIDR != "10.0.0.0/24" {
		t.Errorf("CIDR = %q", got.CIDR)
	}

	if err := st.SaveSecurityRules("vm-1", []models.SecurityRule{
		{Direction: "ingress", Protocol: "tcp", PortMin: 22, PortMax: 22, Allow: false},
	}); err != nil {
		t.Fatalf("SaveSecurityRules: %v", err)
	}
	rules, err := st.LoadSecurityRules("vm-1")
	if err != nil {
		t.Fatalf("LoadSecurityRules: %v", err)
	}
	if len(rules) != 1 || rules[0].PortMin != 22 {
		t.Errorf("rules = %+v, want one rule for tcp/22", rules)
	}

	// Network firewall rules round-trip through the networks row.
	n := got
	n.FirewallRules = rules
	if err := st.SaveNetwork(n); err != nil {
		t.Fatalf("SaveNetwork(FirewallRules): %v", err)
	}
	back, err := st.LoadNetwork("n1")
	if err != nil {
		t.Fatalf("LoadNetwork after firewall save: %v", err)
	}
	if len(back.FirewallRules) != 1 {
		t.Errorf("FirewallRules = %+v, want one rule", back.FirewallRules)
	}

	// VMs carry a network id.
	if err := st.SaveVM(&models.MicroVMInstance{
		ID: "vm-1", Status: "stopped",
		Resources: models.ResourceSpecs{VCPUs: 1, MemoryMB: 256, StorageMB: 1024},
		Network:   models.NetworkInfo{NetworkID: "n1", MACAddress: "02:fc:00:00:00:01"},
		Metadata:  models.MicroVMMetadata{Labels: map[string]string{}},
	}); err != nil {
		t.Fatalf("SaveVM: %v", err)
	}
	vms, err := st.LoadAllVMs()
	if err != nil {
		t.Fatalf("LoadAllVMs: %v", err)
	}
	if vms["vm-1"].Network.NetworkID != "n1" {
		t.Errorf("VM network_id = %q, want n1", vms["vm-1"].Network.NetworkID)
	}
}
