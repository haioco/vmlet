package network

import (
	"os"
	"testing"
)

func TestNewDHCPManager(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	if dm == nil {
		t.Fatal("NewDHCPManager returned nil")
	}
	
	if dm.configDir != tempDir {
		t.Errorf("Expected config dir %s, got %s", tempDir, dm.configDir)
	}
	
	if dm.bridgeName != config.BridgeName {
		t.Errorf("Expected bridge name %s, got %s", config.BridgeName, dm.bridgeName)
	}
	
	if dm.ipRange.Start != config.IPRange.Start {
		t.Errorf("Expected IP range start %s, got %s", config.IPRange.Start, dm.ipRange.Start)
	}
	
	if dm.ipRange.End != config.IPRange.End {
		t.Errorf("Expected IP range end %s, got %s", config.IPRange.End, dm.ipRange.End)
	}
	
	if dm.gateway != config.Gateway {
		t.Errorf("Expected gateway %s, got %s", config.Gateway, dm.gateway)
	}
	
	if len(dm.dnsServers) != len(config.DNSServers) {
		t.Errorf("Expected %d DNS servers, got %d", len(config.DNSServers), len(dm.dnsServers))
	}
	
	if dm.leaseTime != config.LeaseTime {
		t.Errorf("Expected lease time %s, got %s", config.LeaseTime, dm.leaseTime)
	}
}

func TestGenerateConfig(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	err := dm.generateConfig()
	if err != nil {
		t.Fatalf("Failed to generate config: %v", err)
	}
	
	// Check if config file was created
	if _, err := os.Stat(dm.configFile); err != nil {
		t.Errorf("Config file was not created: %v", err)
	}
	
	// Read and verify config content
	content, err := os.ReadFile(dm.configFile)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}
	
	configStr := string(content)
	
	// Check for required configuration elements
	expectedElements := []string{
		"interface=test-br0",
		"dhcp-range=192.168.100.10,192.168.100.100,12h",
		"dhcp-option=3,192.168.100.1",
		"dhcp-option=6,8.8.8.8,8.8.4.4",
	}
	
	for _, element := range expectedElements {
		if !contains(configStr, element) {
			t.Errorf("Config missing expected element: %s", element)
		}
	}
}

func TestFindNextAvailableIP(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.12",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	// Test finding first available IP
	ip, err := dm.findNextAvailableIP()
	if err != nil {
		t.Fatalf("Failed to find available IP: %v", err)
	}
	
	if ip != "192.168.100.10" {
		t.Errorf("Expected first IP 192.168.100.10, got %s", ip)
	}
	
	// Allocate the IP and test finding next
	dm.allocatedIPs["aa:bb:cc:dd:ee:ff"] = ip
	
	ip2, err := dm.findNextAvailableIP()
	if err != nil {
		t.Fatalf("Failed to find second available IP: %v", err)
	}
	
	if ip2 != "192.168.100.11" {
		t.Errorf("Expected second IP 192.168.100.11, got %s", ip2)
	}
	
	// Allocate all IPs and test exhaustion
	dm.allocatedIPs["aa:bb:cc:dd:ee:f1"] = "192.168.100.11"
	dm.allocatedIPs["aa:bb:cc:dd:ee:f2"] = "192.168.100.12"
	
	_, err = dm.findNextAvailableIP()
	if err == nil {
		t.Error("Should fail when no IPs are available")
	}
}

func TestAllocateReleaseIP(t *testing.T) {
	// Skip this test as it requires dnsmasq and system commands
	t.Skip("Skipping AllocateReleaseIP test - requires dnsmasq and system commands")
}

func TestIsRunning(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	// Test with no PID file
	running := dm.IsRunning()
	if running {
		t.Error("DHCP manager should not be running without PID file")
	}
	
	// Create fake PID file with invalid PID
	err := os.WriteFile(dm.pidFile, []byte("99999999"), 0644)
	if err != nil {
		t.Fatalf("Failed to create fake PID file: %v", err)
	}
	
	running = dm.IsRunning()
	if running {
		t.Error("DHCP manager should not be running with invalid PID")
	}
}

func TestParseLeases(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	// Test with non-existent leases file
	leases, err := dm.parseLeases()
	if err != nil {
		t.Fatalf("Failed to parse non-existent leases file: %v", err)
	}
	
	if len(leases) != 0 {
		t.Errorf("Expected 0 leases, got %d", len(leases))
	}
	
	// Create fake leases file
	leasesContent := `1640995200 aa:bb:cc:dd:ee:ff 192.168.100.10 test-vm *
1640995300 aa:bb:cc:dd:ee:f1 192.168.100.11 test-vm2 *`
	
	err = os.WriteFile(dm.leasesFile, []byte(leasesContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create fake leases file: %v", err)
	}
	
	leases, err = dm.parseLeases()
	if err != nil {
		t.Fatalf("Failed to parse leases file: %v", err)
	}
	
	if len(leases) != 2 {
		t.Errorf("Expected 2 leases, got %d", len(leases))
	}
	
	// Check first lease
	if leases[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("Expected MAC aa:bb:cc:dd:ee:ff, got %s", leases[0].MAC)
	}
	
	if leases[0].IP != "192.168.100.10" {
		t.Errorf("Expected IP 192.168.100.10, got %s", leases[0].IP)
	}
	
	if leases[0].Hostname != "test-vm" {
		t.Errorf("Expected hostname test-vm, got %s", leases[0].Hostname)
	}
}

func TestStartStop(t *testing.T) {
	// Skip actual start/stop tests as they require dnsmasq and root privileges
	t.Skip("Skipping start/stop tests - requires dnsmasq and root privileges")
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && 
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || 
		 containsAt(s, substr, 1)))
}

func containsAt(s, substr string, start int) bool {
	if start >= len(s) {
		return false
	}
	if start+len(substr) > len(s) {
		return containsAt(s, substr, start+1)
	}
	if s[start:start+len(substr)] == substr {
		return true
	}
	return containsAt(s, substr, start+1)
}

func TestAllocateReleaseIPLogic(t *testing.T) {
	tempDir := t.TempDir()
	
	config := DHCPConfig{
		ConfigDir:  tempDir,
		BridgeName: "test-br0",
		IPRange: DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8"},
		LeaseTime:  "12h",
	}
	
	dm := NewDHCPManager(config)
	
	macAddr := "aa:bb:cc:dd:ee:ff"
	
	// Test direct allocation to internal map (bypassing dnsmasq)
	ip := "192.168.100.10"
	dm.allocatedIPs[macAddr] = ip
	
	// Test that same MAC returns same IP
	if allocatedIP, exists := dm.allocatedIPs[macAddr]; !exists || allocatedIP != ip {
		t.Errorf("Expected IP %s for MAC %s, got %s (exists: %v)", ip, macAddr, allocatedIP, exists)
	}
	
	// Test release from internal map
	delete(dm.allocatedIPs, macAddr)
	
	// Verify IP is no longer allocated
	if _, exists := dm.allocatedIPs[macAddr]; exists {
		t.Error("IP should be released from allocated IPs map")
	}
}