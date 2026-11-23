package network

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSHManager handles SSH key generation and port forwarding
type SSHManager struct {
	keyDir        string
	portRange     PortRange
	allocatedPorts map[string]int // vmID -> port mapping
	mu            sync.RWMutex
}

// PortRange represents the range of ports available for SSH forwarding
type PortRange struct {
	Start int
	End   int
}

// SSHConfig represents SSH configuration for a VM
type SSHConfig struct {
	KeyDir       string
	PortRange    PortRange
}

// SSHKeyPair represents an SSH key pair
type SSHKeyPair struct {
	PrivateKeyPath string
	PublicKeyPath  string
	PrivateKey     string
	PublicKey      string
}

// SSHConnection represents SSH connection information
type SSHConnection struct {
	Host       string
	Port       int
	User       string
	KeyPath    string
	Command    string
}

// NewSSHManager creates a new SSH manager
func NewSSHManager(config SSHConfig) *SSHManager {
	keyDir := config.KeyDir
	if keyDir == "" {
		keyDir = "/var/lib/microvm-service/ssh-keys"
	}

	return &SSHManager{
		keyDir:         keyDir,
		portRange:      config.PortRange,
		allocatedPorts: make(map[string]int),
	}
}

// GenerateKeyPair generates an SSH key pair for a VM
func (sm *SSHManager) GenerateKeyPair(vmID string) (*SSHKeyPair, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Create key directory
	if err := os.MkdirAll(sm.keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	// Generate RSA key pair
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}

	// Generate public key
	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate public key: %w", err)
	}

	// Encode private key
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privateKeyBytes := pem.EncodeToMemory(privateKeyPEM)

	// Format public key
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)

	// Define file paths
	privateKeyPath := filepath.Join(sm.keyDir, fmt.Sprintf("%s_rsa", vmID))
	publicKeyPath := filepath.Join(sm.keyDir, fmt.Sprintf("%s_rsa.pub", vmID))

	// Write private key
	if err := ioutil.WriteFile(privateKeyPath, privateKeyBytes, 0600); err != nil {
		return nil, fmt.Errorf("failed to write private key: %w", err)
	}

	// Write public key
	if err := ioutil.WriteFile(publicKeyPath, publicKeyBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write public key: %w", err)
	}

	return &SSHKeyPair{
		PrivateKeyPath: privateKeyPath,
		PublicKeyPath:  publicKeyPath,
		PrivateKey:     string(privateKeyBytes),
		PublicKey:      string(publicKeyBytes),
	}, nil
}

// AllocatePort allocates a port for SSH forwarding
func (sm *SSHManager) AllocatePort(vmID string) (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check if VM already has a port
	if port, exists := sm.allocatedPorts[vmID]; exists {
		return port, nil
	}

	// Find next available port
	port, err := sm.findNextAvailablePort()
	if err != nil {
		return 0, fmt.Errorf("failed to find available port: %w", err)
	}

	sm.allocatedPorts[vmID] = port
	return port, nil
}

// ReleasePort releases a port allocation
func (sm *SSHManager) ReleasePort(vmID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	port, exists := sm.allocatedPorts[vmID]
	if !exists {
		return nil // Already released
	}

	// Remove port forwarding rule
	if err := sm.removePortForwarding(port, "22"); err != nil {
		return fmt.Errorf("failed to remove port forwarding: %w", err)
	}

	delete(sm.allocatedPorts, vmID)
	return nil
}

// SetupPortForwarding sets up port forwarding for SSH access
func (sm *SSHManager) SetupPortForwarding(vmID, vmIP string, hostPort, guestPort int) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Add iptables rule for port forwarding
	rule := []string{
		"iptables", "-t", "nat", "-A", "PREROUTING",
		"-p", "tcp", "--dport", strconv.Itoa(hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", vmIP, guestPort),
	}

	// Check if rule already exists
	checkRule := append([]string{}, rule...)
	checkRule[3] = "-C" // Change -A to -C for check
	checkCmd := exec.Command(checkRule[0], checkRule[1:]...)
	if checkCmd.Run() == nil {
		return nil // Rule already exists
	}

	// Add the rule
	cmd := exec.Command(rule[0], rule[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to add port forwarding rule: %w", err)
	}

	return nil
}

// RemoveKeyPair removes SSH key pair for a VM
func (sm *SSHManager) RemoveKeyPair(vmID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	privateKeyPath := filepath.Join(sm.keyDir, fmt.Sprintf("%s_rsa", vmID))
	publicKeyPath := filepath.Join(sm.keyDir, fmt.Sprintf("%s_rsa.pub", vmID))

	// Remove files (ignore errors if files don't exist)
	os.Remove(privateKeyPath)
	os.Remove(publicKeyPath)

	return nil
}

// GetSSHConnection returns SSH connection information for a VM
func (sm *SSHManager) GetSSHConnection(vmID, vmIP string) (*SSHConnection, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	port, exists := sm.allocatedPorts[vmID]
	if !exists {
		return nil, fmt.Errorf("no port allocated for VM %s", vmID)
	}

	privateKeyPath := filepath.Join(sm.keyDir, fmt.Sprintf("%s_rsa", vmID))

	// Check if key file exists
	if _, err := os.Stat(privateKeyPath); err != nil {
		return nil, fmt.Errorf("SSH key not found for VM %s: %w", vmID, err)
	}

	command := fmt.Sprintf("ssh -i %s -p %d root@localhost", privateKeyPath, port)

	return &SSHConnection{
		Host:    "localhost",
		Port:    port,
		User:    "root",
		KeyPath: privateKeyPath,
		Command: command,
	}, nil
}

// TestSSHConnection tests SSH connectivity to a VM
func (sm *SSHManager) TestSSHConnection(vmID string, timeout time.Duration) error {
	connection, err := sm.GetSSHConnection(vmID, "")
	if err != nil {
		return fmt.Errorf("failed to get SSH connection info: %w", err)
	}

	// Test SSH connection with timeout
	cmd := exec.Command("ssh", 
		"-i", connection.KeyPath,
		"-p", strconv.Itoa(connection.Port),
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("%s@%s", connection.User, connection.Host),
		"echo 'SSH connection successful'",
	)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("SSH connection test failed: %w", err)
		}
		return nil
	case <-time.After(timeout):
		cmd.Process.Kill()
		return fmt.Errorf("SSH connection test timed out after %v", timeout)
	}
}

// findNextAvailablePort finds the next available port in the range
func (sm *SSHManager) findNextAvailablePort() (int, error) {
	for port := sm.portRange.Start; port <= sm.portRange.End; port++ {
		// Check if port is already allocated
		allocated := false
		for _, allocatedPort := range sm.allocatedPorts {
			if allocatedPort == port {
				allocated = true
				break
			}
		}

		if !allocated && sm.isPortAvailable(port) {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no available ports in range %d-%d", sm.portRange.Start, sm.portRange.End)
}

// isPortAvailable checks if a port is available on the host
func (sm *SSHManager) isPortAvailable(port int) bool {
	cmd := exec.Command("netstat", "-ln")
	output, err := cmd.Output()
	if err != nil {
		return true // Assume available if we can't check
	}

	portStr := fmt.Sprintf(":%d ", port)
	return !strings.Contains(string(output), portStr)
}

// removePortForwarding removes port forwarding rule
func (sm *SSHManager) removePortForwarding(hostPort int, guestPort string) error {
	// Remove iptables rule
	rule := []string{
		"iptables", "-t", "nat", "-D", "PREROUTING",
		"-p", "tcp", "--dport", strconv.Itoa(hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("*:%s", guestPort),
	}

	cmd := exec.Command(rule[0], rule[1:]...)
	// Ignore errors when removing rules (they might not exist)
	cmd.Run()

	return nil
}