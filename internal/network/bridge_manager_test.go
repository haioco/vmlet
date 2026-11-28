package network

import (
	"os"
	"strings"
	"testing"
)

func TestNewBridgeManager(t *testing.T) {
	config := BridgeConfig{
		Name:          "test-br0",
		IPAddress:     "192.168.100.1",
		CIDR:          "192.168.100.0/24",
		HostInterface: "eth0",
	}

	bm := NewBridgeManager(config)

	if bm == nil {
		t.Fatal("NewBridgeManager returned nil")
	}

	if bm.bridgeName != config.Name {
		t.Errorf("Expected bridge name %s, got %s", config.Name, bm.bridgeName)
	}

	if bm.bridgeIP != config.IPAddress {
		t.Errorf("Expected bridge IP %s, got %s", config.IPAddress, bm.bridgeIP)
	}

	if bm.bridgeCIDR != config.CIDR {
		t.Errorf("Expected bridge CIDR %s, got %s", config.CIDR, bm.bridgeCIDR)
	}

	if bm.hostInterface != config.HostInterface {
		t.Errorf("Expected host interface %s, got %s", config.HostInterface, bm.hostInterface)
	}
}

func TestBridgeExists(t *testing.T) {
	config := BridgeConfig{
		Name:          "nonexistent-br0",
		IPAddress:     "192.168.100.1",
		CIDR:          "192.168.100.0/24",
		HostInterface: "eth0",
	}

	bm := NewBridgeManager(config)

	// Test with non-existent bridge
	exists := bm.bridgeExists()
	if exists {
		t.Error("Bridge should not exist")
	}
}

func TestGetBridgeInfo(t *testing.T) {
	config := BridgeConfig{
		Name:          "test-br0",
		IPAddress:     "192.168.100.1",
		CIDR:          "192.168.100.0/24",
		HostInterface: "eth0",
	}

	bm := NewBridgeManager(config)

	// Test with non-existent bridge
	_, err := bm.GetBridgeInfo()
	if err == nil {
		t.Error("GetBridgeInfo should fail for non-existent bridge")
	}
}

func TestCreateDeleteBridge(t *testing.T) {
	// Skip actual bridge creation/deletion tests as they require root privileges
	t.Skip("Skipping bridge creation/deletion tests - requires root privileges and system commands")
}

func TestValidateBridge(t *testing.T) {
	config := BridgeConfig{
		Name:          "nonexistent-br0",
		IPAddress:     "192.168.100.1",
		CIDR:          "192.168.100.0/24",
		HostInterface: "eth0",
	}

	bm := NewBridgeManager(config)

	// Test validation with non-existent bridge
	err := bm.ValidateBridge()
	if err == nil {
		t.Error("Bridge validation should fail for non-existent bridge")
	}
}

func TestConfigureNATRules(t *testing.T) {
	// Skip NAT rules tests as they require root privileges and iptables
	t.Skip("Skipping NAT rules tests - requires root privileges and iptables")
}

func TestTestBridgeConnectivity(t *testing.T) {
	config := BridgeConfig{
		Name:          "test-br0",
		IPAddress:     "192.168.100.1",
		CIDR:          "192.168.100.0/24",
		HostInterface: "eth0",
	}

	bm := NewBridgeManager(config)

	// Test connectivity check (this will fail as bridge doesn't exist)
	err := bm.testBridgeConnectivity()
	// This test is mainly to ensure the function doesn't panic
	// The actual connectivity test would require a real bridge
	_ = err // Ignore error for this test
}

// An adopted bridge is the host's: vmlet verifies it instead of creating it, so
// a host that bridges public ranges (haioflash's br0) is not shadowed by a
// substitute with nothing routed behind it.
func TestAdoptBridgeIsVerifiedNotCreated(t *testing.T) {
	const missing = "vmlet-no-such-bridge-xyz"

	// Without adopt, a missing bridge is created (this host cannot actually
	// create one, so only the "must exist" branch is asserted here).
	owning := NewBridgeManager(BridgeConfig{
		Name:        missing,
		IPAddress:   "192.168.100.1",
		CIDR:        "192.168.100.0/24",
		ManageNAT:   true,
		AdoptBridge: false,
	})
	_ = owning

	adopted := NewBridgeManager(BridgeConfig{
		Name:        missing,
		AdoptBridge: true,
	})

	if err := adopted.CreateBridge(); err == nil {
		t.Fatal("adopted CreateBridge() succeeded on a missing bridge; it must refuse, not create")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the missing bridge %q", err, missing)
	}

	// Shutdown on an adopted bridge must not delete the host's interface.
	if err := adopted.DeleteBridge(); err != nil {
		t.Errorf("DeleteBridge() on an adopted bridge = %v, want a no-op", err)
	}
	if _, err := os.Stat("/sys/class/net/" + missing); !os.IsNotExist(err) {
		t.Errorf("adopted DeleteBridge() appears to have removed %s", missing)
	}
}
