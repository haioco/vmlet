package network

import (
	"testing"
)

func TestNewNetworkManager(t *testing.T) {
	tempDir := t.TempDir()
	
	config := &NetworkConfig{
		BridgeName:    "test-br0",
		BridgeIP:      "192.168.100.1",
		BridgeCIDR:    "192.168.100.0/24",
		HostInterface: "eth0",
		DHCPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: PortRange{Start: 2200, End: 2299},
		ConfigDir:    tempDir,
	}
	
	nm := NewNetworkManager(config)
	
	if nm == nil {
		t.Fatal("NewNetworkManager returned nil")
	}
	
	if nm.config != config {
		t.Error("Network manager should store config reference")
	}
	
	if nm.bridgeManager == nil {
		t.Error("Bridge manager should be initialized")
	}
	
	if nm.dhcpManager == nil {
		t.Error("DHCP manager should be initialized")
	}
	
	if nm.sshManager == nil {
		t.Error("SSH manager should be initialized")
	}
}

func TestAllocateVMNetwork(t *testing.T) {
	// Skip this test as it requires system commands (dnsmasq, iptables)
	t.Skip("Skipping AllocateVMNetwork test - requires system commands")
}

func TestReleaseVMNetwork(t *testing.T) {
	// Skip this test as it requires system commands (dnsmasq, iptables)
	t.Skip("Skipping ReleaseVMNetwork test - requires system commands")
}

func TestValidateNetworkSetup(t *testing.T) {
	tempDir := t.TempDir()
	
	config := &NetworkConfig{
		BridgeName:    "test-br0",
		BridgeIP:      "192.168.100.1",
		BridgeCIDR:    "192.168.100.0/24",
		HostInterface: "eth0",
		DHCPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: PortRange{Start: 2200, End: 2299},
		ConfigDir:    tempDir,
	}
	
	nm := NewNetworkManager(config)
	
	// Test validation without initialization (should fail)
	err := nm.ValidateNetworkSetup()
	if err == nil {
		t.Error("Network validation should fail without initialization")
	}
}

func TestGetNetworkStatus(t *testing.T) {
	tempDir := t.TempDir()
	
	config := &NetworkConfig{
		BridgeName:    "test-br0",
		BridgeIP:      "192.168.100.1",
		BridgeCIDR:    "192.168.100.0/24",
		HostInterface: "eth0",
		DHCPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: PortRange{Start: 2200, End: 2299},
		ConfigDir:    tempDir,
	}
	
	nm := NewNetworkManager(config)
	
	// Test getting status without initialization (should fail)
	_, err := nm.GetNetworkStatus()
	if err == nil {
		t.Error("Getting network status should fail without initialization")
	}
}

func TestGetVMNetworkInfo(t *testing.T) {
	tempDir := t.TempDir()
	
	config := &NetworkConfig{
		BridgeName:    "test-br0",
		BridgeIP:      "192.168.100.1",
		BridgeCIDR:    "192.168.100.0/24",
		HostInterface: "eth0",
		DHCPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: PortRange{Start: 2200, End: 2299},
		ConfigDir:    tempDir,
	}
	
	nm := NewNetworkManager(config)
	
	// Test getting info for non-existent VM
	_, err := nm.GetVMNetworkInfo("non-existent-vm")
	if err == nil {
		t.Error("Getting VM network info should fail for non-existent VM")
	}
}

func TestTestVMConnectivity(t *testing.T) {
	tempDir := t.TempDir()
	
	config := &NetworkConfig{
		BridgeName:    "test-br0",
		BridgeIP:      "192.168.100.1",
		BridgeCIDR:    "192.168.100.0/24",
		HostInterface: "eth0",
		DHCPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: PortRange{Start: 2200, End: 2299},
		ConfigDir:    tempDir,
	}
	
	nm := NewNetworkManager(config)
	
	// Test connectivity for non-existent VM
	err := nm.TestVMConnectivity("non-existent-vm")
	if err == nil {
		t.Error("Testing VM connectivity should fail for non-existent VM")
	}
}

func TestInitializeShutdown(t *testing.T) {
	// Skip initialization and shutdown tests as they require system privileges
	t.Skip("Skipping initialization/shutdown tests - requires system privileges")
}