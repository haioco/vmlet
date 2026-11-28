package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// BridgeManager handles network bridge creation and configuration
type BridgeManager struct {
	bridgeName    string
	bridgeIP      string
	bridgeCIDR    string
	hostInterface string
	manageNAT     bool
	// adoptBridge marks the bridge as host-owned: CreateBridge verifies it
	// exists instead of creating it, and NAT rules are never written.
	adoptBridge bool
}

// BridgeConfig represents bridge configuration
type BridgeConfig struct {
	Name          string
	IPAddress     string
	CIDR          string
	HostInterface string
	ManageNAT     bool
	AdoptBridge   bool
}

// NewBridgeManager creates a new bridge manager
func NewBridgeManager(config BridgeConfig) *BridgeManager {
	return &BridgeManager{
		bridgeName:    config.Name,
		bridgeIP:      config.IPAddress,
		bridgeCIDR:    config.CIDR,
		hostInterface: config.HostInterface,
		manageNAT:     config.ManageNAT,
		adoptBridge:   config.AdoptBridge,
	}
}

// CreateBridge creates and configures the NAT bridge
func (bm *BridgeManager) CreateBridge() error {
	// Check if bridge already exists
	if bm.bridgeExists() {
		return nil
	}

	// An adopted bridge is the host's, not ours: creating it would race the
	// host's own configuration (and could shadow a bridge that carries public
	// ranges). Refuse instead, so a typo in the bridge name is a startup error
	// rather than a silently unreachable network.
	if bm.adoptBridge {
		return fmt.Errorf("bridge %s does not exist and -bridge-adopt forbids creating it", bm.bridgeName)
	}

	// Create bridge interface
	if err := bm.createBridgeInterface(); err != nil {
		return fmt.Errorf("failed to create bridge interface: %w", err)
	}

	// Configure bridge IP
	if err := bm.configureBridgeIP(); err != nil {
		return fmt.Errorf("failed to configure bridge IP: %w", err)
	}

	// Bring bridge up
	if err := bm.bringBridgeUp(); err != nil {
		return fmt.Errorf("failed to bring bridge up: %w", err)
	}

	// Optionally enable IP forwarding and NAT management
	if bm.manageNAT {
		// Enable IP forwarding
		if err := bm.enableIPForwarding(); err != nil {
			return fmt.Errorf("failed to enable IP forwarding: %w", err)
		}

		// Configure NAT rules
		if err := bm.configureNATRules(); err != nil {
			return fmt.Errorf("failed to configure NAT rules: %w", err)
		}
	}

	return nil
}

// DeleteBridge removes the bridge and cleans up configuration
func (bm *BridgeManager) DeleteBridge() error {
	// An adopted bridge is the host's: deleting it would cut every VM on it off
	// the network, and the rules on it are the host's to remove.
	if bm.adoptBridge {
		return nil
	}

	// Remove NAT rules if managed
	if bm.manageNAT {
		if err := bm.removeNATRules(); err != nil {
			return fmt.Errorf("failed to remove NAT rules: %w", err)
		}
	}

	// Delete bridge interface
	if err := bm.deleteBridgeInterface(); err != nil {
		return fmt.Errorf("failed to delete bridge interface: %w", err)
	}

	return nil
}

// bridgeExists checks if the bridge interface already exists
func (bm *BridgeManager) bridgeExists() bool {
	cmd := exec.Command("ip", "link", "show", bm.bridgeName)
	return cmd.Run() == nil
}

// createBridgeInterface creates the bridge interface
func (bm *BridgeManager) createBridgeInterface() error {
	cmd := exec.Command("ip", "link", "add", bm.bridgeName, "type", "bridge")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create bridge %s: %w", bm.bridgeName, err)
	}
	return nil
}

// configureBridgeIP assigns IP address to the bridge
func (bm *BridgeManager) configureBridgeIP() error {
	cidr := fmt.Sprintf("%s/%s", bm.bridgeIP, strings.Split(bm.bridgeCIDR, "/")[1])
	cmd := exec.Command("ip", "addr", "add", cidr, "dev", bm.bridgeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to assign IP %s to bridge %s: %w", cidr, bm.bridgeName, err)
	}
	return nil
}

// bringBridgeUp activates the bridge interface
func (bm *BridgeManager) bringBridgeUp() error {
	cmd := exec.Command("ip", "link", "set", bm.bridgeName, "up")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to bring up bridge %s: %w", bm.bridgeName, err)
	}
	return nil
}

// enableIPForwarding enables IP forwarding on the host
func (bm *BridgeManager) enableIPForwarding() error {
	// Enable IPv4 forwarding
	cmd := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Make it persistent
	forwardingFile := "/proc/sys/net/ipv4/ip_forward"
	if err := os.WriteFile(forwardingFile, []byte("1"), 0644); err != nil {
		// This might fail in containers, but sysctl should work
		return nil
	}

	return nil
}

// configureNATRules sets up iptables rules for NAT and forwarding
func (bm *BridgeManager) configureNATRules() error {
	rules := [][]string{
		// Enable NAT for outgoing traffic
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", bm.bridgeCIDR, "-o", bm.hostInterface, "-j", "MASQUERADE"},
		// Allow forwarding from bridge to host interface
		{"iptables", "-A", "FORWARD", "-i", bm.bridgeName, "-o", bm.hostInterface, "-j", "ACCEPT"},
		// Allow established connections back
		{"iptables", "-A", "FORWARD", "-i", bm.hostInterface, "-o", bm.bridgeName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}

	for _, rule := range rules {
		// Check if rule already exists
		checkRule := append([]string{}, rule...)
		checkRule[3] = "-C" // Change -A to -C for check
		checkCmd := exec.Command(checkRule[0], checkRule[1:]...)
		if checkCmd.Run() == nil {
			continue // Rule already exists
		}

		// Add the rule
		cmd := exec.Command(rule[0], rule[1:]...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to add iptables rule %v: %w", rule, err)
		}
	}

	return nil
}

// configureIPv6ForwardingRules sets up ip6tables rules for IPv6 forwarding
func (bm *BridgeManager) configureIPv6ForwardingRules(ipv6Subnet string) error {
	if ipv6Subnet == "" {
		return nil // No IPv6 subnet configured, skip
	}

	rules := [][]string{
		// Allow forwarding to IPv6 subnet
		{"ip6tables", "-I", "FORWARD", "1", "-d", ipv6Subnet, "-j", "ACCEPT"},
		// Allow forwarding from IPv6 subnet
		{"ip6tables", "-I", "FORWARD", "2", "-s", ipv6Subnet, "-j", "ACCEPT"},
	}

	for _, rule := range rules {
		// Check if rule already exists
		checkRule := append([]string{}, rule...)
		checkRule[2] = "-C"                                 // Change -I to -C for check
		checkRule = append(checkRule[:3], checkRule[4:]...) // Remove position argument for check
		checkCmd := exec.Command(checkRule[0], checkRule[1:]...)
		if checkCmd.Run() == nil {
			continue // Rule already exists
		}

		// Add the rule
		cmd := exec.Command(rule[0], rule[1:]...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to add ip6tables rule %v: %w", rule, err)
		}
	}

	return nil
}

// removeNATRules removes iptables rules
func (bm *BridgeManager) removeNATRules() error {
	rules := [][]string{
		{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", bm.bridgeCIDR, "-o", bm.hostInterface, "-j", "MASQUERADE"},
		{"iptables", "-D", "FORWARD", "-i", bm.bridgeName, "-o", bm.hostInterface, "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", bm.hostInterface, "-o", bm.bridgeName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}

	for _, rule := range rules {
		cmd := exec.Command(rule[0], rule[1:]...)
		// Ignore errors when removing rules (they might not exist)
		cmd.Run()
	}

	return nil
}

// deleteBridgeInterface removes the bridge interface
func (bm *BridgeManager) deleteBridgeInterface() error {
	if !bm.bridgeExists() {
		return nil
	}

	// Bring bridge down first
	cmd := exec.Command("ip", "link", "set", bm.bridgeName, "down")
	cmd.Run() // Ignore errors

	// Delete bridge
	cmd = exec.Command("ip", "link", "delete", bm.bridgeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete bridge %s: %w", bm.bridgeName, err)
	}

	return nil
}

// GetBridgeInfo returns information about the bridge
func (bm *BridgeManager) GetBridgeInfo() (*BridgeInfo, error) {
	if !bm.bridgeExists() {
		return nil, fmt.Errorf("bridge %s does not exist", bm.bridgeName)
	}

	return &BridgeInfo{
		Name:      bm.bridgeName,
		IPAddress: bm.bridgeIP,
		CIDR:      bm.bridgeCIDR,
		Status:    "up",
	}, nil
}

// ValidateBridge checks if the bridge is properly configured
func (bm *BridgeManager) ValidateBridge() error {
	if !bm.bridgeExists() {
		return fmt.Errorf("bridge %s does not exist", bm.bridgeName)
	}

	// Check if bridge has correct IP
	cmd := exec.Command("ip", "addr", "show", bm.bridgeName)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get bridge info: %w", err)
	}

	if !strings.Contains(string(output), bm.bridgeIP) {
		return fmt.Errorf("bridge %s does not have expected IP %s", bm.bridgeName, bm.bridgeIP)
	}

	// Test connectivity by pinging the bridge IP
	if err := bm.testBridgeConnectivity(); err != nil {
		return fmt.Errorf("bridge connectivity test failed: %w", err)
	}

	return nil
}

// testBridgeConnectivity tests if the bridge is reachable
func (bm *BridgeManager) testBridgeConnectivity() error {
	// Parse bridge IP
	ip := net.ParseIP(bm.bridgeIP)
	if ip == nil {
		return fmt.Errorf("invalid bridge IP: %s", bm.bridgeIP)
	}

	// Try to connect to the bridge IP (this should work locally)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:22", bm.bridgeIP), 1*time.Second)
	if err != nil {
		// This is expected to fail, but the attempt should not result in "no route to host"
		if strings.Contains(err.Error(), "no route to host") {
			return fmt.Errorf("no route to bridge IP %s", bm.bridgeIP)
		}
	} else {
		conn.Close()
	}

	return nil
}

// ConfigureIPv6Forwarding configures IPv6 forwarding rules for the bridge
// This should be called during service initialization with the IPv6 subnet
func (bm *BridgeManager) ConfigureIPv6Forwarding(ipv6Subnet string) error {
	return bm.configureIPv6ForwardingRules(ipv6Subnet)
}

// BridgeInfo contains information about a bridge
type BridgeInfo struct {
	Name      string
	IPAddress string
	CIDR      string
	Status    string
}
