package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSSHManager(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2299,
		},
	}
	
	sm := NewSSHManager(config)
	
	if sm == nil {
		t.Fatal("NewSSHManager returned nil")
	}
	
	if sm.keyDir != tempDir {
		t.Errorf("Expected key dir %s, got %s", tempDir, sm.keyDir)
	}
	
	if sm.portRange.Start != config.PortRange.Start {
		t.Errorf("Expected port range start %d, got %d", config.PortRange.Start, sm.portRange.Start)
	}
	
	if sm.portRange.End != config.PortRange.End {
		t.Errorf("Expected port range end %d, got %d", config.PortRange.End, sm.portRange.End)
	}
	
	if sm.allocatedPorts == nil {
		t.Error("Allocated ports map should be initialized")
	}
}

func TestGenerateKeyPair(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2299,
		},
	}
	
	sm := NewSSHManager(config)
	vmID := "test-vm-123"
	
	keyPair, err := sm.GenerateKeyPair(vmID)
	if err != nil {
		t.Fatalf("Failed to generate key pair: %v", err)
	}
	
	if keyPair == nil {
		t.Fatal("Generated key pair is nil")
	}
	
	// Check private key path
	expectedPrivatePath := filepath.Join(tempDir, vmID+"_rsa")
	if keyPair.PrivateKeyPath != expectedPrivatePath {
		t.Errorf("Expected private key path %s, got %s", expectedPrivatePath, keyPair.PrivateKeyPath)
	}
	
	// Check public key path
	expectedPublicPath := filepath.Join(tempDir, vmID+"_rsa.pub")
	if keyPair.PublicKeyPath != expectedPublicPath {
		t.Errorf("Expected public key path %s, got %s", expectedPublicPath, keyPair.PublicKeyPath)
	}
	
	// Check if files were created
	if _, err := os.Stat(keyPair.PrivateKeyPath); err != nil {
		t.Errorf("Private key file was not created: %v", err)
	}
	
	if _, err := os.Stat(keyPair.PublicKeyPath); err != nil {
		t.Errorf("Public key file was not created: %v", err)
	}
	
	// Check file permissions for private key
	info, err := os.Stat(keyPair.PrivateKeyPath)
	if err != nil {
		t.Fatalf("Failed to stat private key file: %v", err)
	}
	
	if info.Mode().Perm() != 0600 {
		t.Errorf("Private key file should have 0600 permissions, got %o", info.Mode().Perm())
	}
	
	// Check that key content is not empty
	if keyPair.PrivateKey == "" {
		t.Error("Private key content should not be empty")
	}
	
	if keyPair.PublicKey == "" {
		t.Error("Public key content should not be empty")
	}
	
	// Verify key content starts with expected headers
	if !contains(keyPair.PrivateKey, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Error("Private key should contain RSA private key header")
	}
	
	if !contains(keyPair.PublicKey, "ssh-rsa") {
		t.Error("Public key should start with ssh-rsa")
	}
}

func TestAllocateReleasePort(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2202, // Small range for testing
		},
	}
	
	sm := NewSSHManager(config)
	
	vmID1 := "test-vm-1"
	vmID2 := "test-vm-2"
	
	// Test port allocation
	port1, err := sm.AllocatePort(vmID1)
	if err != nil {
		t.Fatalf("Failed to allocate port for VM1: %v", err)
	}
	
	if port1 < config.PortRange.Start || port1 > config.PortRange.End {
		t.Errorf("Allocated port %d is outside range %d-%d", port1, config.PortRange.Start, config.PortRange.End)
	}
	
	// Test allocating same VM again (should return same port)
	port1Again, err := sm.AllocatePort(vmID1)
	if err != nil {
		t.Fatalf("Failed to allocate port for VM1 again: %v", err)
	}
	
	if port1 != port1Again {
		t.Errorf("Should return same port for same VM: %d != %d", port1, port1Again)
	}
	
	// Test allocating different VM
	port2, err := sm.AllocatePort(vmID2)
	if err != nil {
		t.Fatalf("Failed to allocate port for VM2: %v", err)
	}
	
	if port1 == port2 {
		t.Errorf("Different VMs should get different ports: %d == %d", port1, port2)
	}
	
	// Test port release
	err = sm.ReleasePort(vmID1)
	if err != nil {
		t.Fatalf("Failed to release port for VM1: %v", err)
	}
	
	// Verify port is no longer allocated
	if _, exists := sm.allocatedPorts[vmID1]; exists {
		t.Error("Port should be released from allocated ports map")
	}
	
	// Test releasing non-existent allocation
	err = sm.ReleasePort("non-existent-vm")
	if err != nil {
		t.Errorf("Releasing non-existent port should not error: %v", err)
	}
}

func TestFindNextAvailablePort(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2202,
		},
	}
	
	sm := NewSSHManager(config)
	
	// Test finding first available port
	port, err := sm.findNextAvailablePort()
	if err != nil {
		t.Fatalf("Failed to find available port: %v", err)
	}
	
	if port != 2200 {
		t.Errorf("Expected first port 2200, got %d", port)
	}
	
	// Allocate the port and test finding next
	sm.allocatedPorts["vm1"] = port
	
	port2, err := sm.findNextAvailablePort()
	if err != nil {
		t.Fatalf("Failed to find second available port: %v", err)
	}
	
	if port2 != 2201 {
		t.Errorf("Expected second port 2201, got %d", port2)
	}
	
	// Allocate all ports and test exhaustion
	sm.allocatedPorts["vm2"] = 2201
	sm.allocatedPorts["vm3"] = 2202
	
	_, err = sm.findNextAvailablePort()
	if err == nil {
		t.Error("Should fail when no ports are available")
	}
}

func TestRemoveKeyPair(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2299,
		},
	}
	
	sm := NewSSHManager(config)
	vmID := "test-vm-123"
	
	// Generate key pair first
	_, err := sm.GenerateKeyPair(vmID)
	if err != nil {
		t.Fatalf("Failed to generate key pair: %v", err)
	}
	
	privateKeyPath := filepath.Join(tempDir, vmID+"_rsa")
	publicKeyPath := filepath.Join(tempDir, vmID+"_rsa.pub")
	
	// Verify files exist
	if _, err := os.Stat(privateKeyPath); err != nil {
		t.Fatalf("Private key file should exist: %v", err)
	}
	
	if _, err := os.Stat(publicKeyPath); err != nil {
		t.Fatalf("Public key file should exist: %v", err)
	}
	
	// Remove key pair
	err = sm.RemoveKeyPair(vmID)
	if err != nil {
		t.Fatalf("Failed to remove key pair: %v", err)
	}
	
	// Verify files are removed
	if _, err := os.Stat(privateKeyPath); !os.IsNotExist(err) {
		t.Error("Private key file should be removed")
	}
	
	if _, err := os.Stat(publicKeyPath); !os.IsNotExist(err) {
		t.Error("Public key file should be removed")
	}
	
	// Test removing non-existent key pair (should not error)
	err = sm.RemoveKeyPair("non-existent-vm")
	if err != nil {
		t.Errorf("Removing non-existent key pair should not error: %v", err)
	}
}

func TestGetSSHConnection(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2299,
		},
	}
	
	sm := NewSSHManager(config)
	vmID := "test-vm-123"
	vmIP := "192.168.100.10"
	
	// Test without allocated port
	_, err := sm.GetSSHConnection(vmID, vmIP)
	if err == nil {
		t.Error("Should fail when no port is allocated")
	}
	
	// Generate key pair and allocate port
	_, err = sm.GenerateKeyPair(vmID)
	if err != nil {
		t.Fatalf("Failed to generate key pair: %v", err)
	}
	
	port, err := sm.AllocatePort(vmID)
	if err != nil {
		t.Fatalf("Failed to allocate port: %v", err)
	}
	
	// Get SSH connection info
	connection, err := sm.GetSSHConnection(vmID, vmIP)
	if err != nil {
		t.Fatalf("Failed to get SSH connection: %v", err)
	}
	
	if connection == nil {
		t.Fatal("SSH connection is nil")
	}
	
	if connection.Host != "localhost" {
		t.Errorf("Expected host localhost, got %s", connection.Host)
	}
	
	if connection.Port != port {
		t.Errorf("Expected port %d, got %d", port, connection.Port)
	}
	
	if connection.User != "root" {
		t.Errorf("Expected user root, got %s", connection.User)
	}
	
	expectedKeyPath := filepath.Join(tempDir, vmID+"_rsa")
	if connection.KeyPath != expectedKeyPath {
		t.Errorf("Expected key path %s, got %s", expectedKeyPath, connection.KeyPath)
	}
	
	// Note: This is a simplified check - the actual command formatting might differ
	if !contains(connection.Command, "ssh") || !contains(connection.Command, expectedKeyPath) {
		t.Errorf("SSH command should contain ssh and key path: %s", connection.Command)
	}
}

func TestSetupPortForwarding(t *testing.T) {
	// Skip port forwarding tests as they require iptables and root privileges
	t.Skip("Skipping port forwarding tests - requires iptables and root privileges")
}

func TestTestSSHConnection(t *testing.T) {
	// Skip SSH connection tests as they require actual SSH server
	t.Skip("Skipping SSH connection tests - requires actual SSH server")
}

func TestIsPortAvailable(t *testing.T) {
	tempDir := t.TempDir()
	
	config := SSHConfig{
		KeyDir: tempDir,
		PortRange: PortRange{
			Start: 2200,
			End:   2299,
		},
	}
	
	sm := NewSSHManager(config)
	
	// Test with a high port number that's likely available
	available := sm.isPortAvailable(65432)
	// This test mainly ensures the function doesn't panic
	// The actual result depends on system state
	_ = available
}