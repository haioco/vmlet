package network

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DHCPManager handles DHCP server configuration and lifecycle
type DHCPManager struct {
	configDir     string
	configFile    string
	leasesFile    string
	pidFile       string
	bridgeName    string
	ipRange       DHCPRange
	gateway       string
	dnsServers    []string
	leaseTime     string
	mu            sync.RWMutex
	allocatedIPs  map[string]string // MAC -> IP mapping
}

// DHCPRange represents the IP range for DHCP allocation
type DHCPRange struct {
	Start string
	End   string
}

// DHCPConfig represents DHCP server configuration
type DHCPConfig struct {
	ConfigDir    string
	BridgeName   string
	IPRange      DHCPRange
	Gateway      string
	DNSServers   []string
	LeaseTime    string
}

// DHCPLease represents a DHCP lease
type DHCPLease struct {
	MAC       string
	IP        string
	Hostname  string
	ExpiresAt time.Time
}

// NewDHCPManager creates a new DHCP manager
func NewDHCPManager(config DHCPConfig) *DHCPManager {
	configDir := config.ConfigDir
	if configDir == "" {
		configDir = "/var/lib/microvm-service/dhcp"
	}

	return &DHCPManager{
		configDir:    configDir,
		configFile:   filepath.Join(configDir, "dnsmasq.conf"),
		leasesFile:   filepath.Join(configDir, "dhcp.leases"),
		pidFile:      filepath.Join(configDir, "dnsmasq.pid"),
		bridgeName:   config.BridgeName,
		ipRange:      config.IPRange,
		gateway:      config.Gateway,
		dnsServers:   config.DNSServers,
		leaseTime:    config.LeaseTime,
		allocatedIPs: make(map[string]string),
	}
}

// Start starts the DHCP server
func (dm *DHCPManager) Start() error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	// Create config directory
	if err := os.MkdirAll(dm.configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Generate configuration
	if err := dm.generateConfig(); err != nil {
		return fmt.Errorf("failed to generate DHCP config: %w", err)
	}

	// Start dnsmasq
	if err := dm.startDnsmasq(); err != nil {
		return fmt.Errorf("failed to start dnsmasq: %w", err)
	}

	// Load existing leases
	if err := dm.loadLeases(); err != nil {
		return fmt.Errorf("failed to load existing leases: %w", err)
	}

	return nil
}

// Stop stops the DHCP server
func (dm *DHCPManager) Stop() error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	return dm.stopDnsmasq()
}

// AllocateIP allocates an IP address for a MAC address
func (dm *DHCPManager) AllocateIP(macAddr string) (string, error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	// Check if MAC already has an IP
	if ip, exists := dm.allocatedIPs[macAddr]; exists {
		return ip, nil
	}

	// Find next available IP
	ip, err := dm.findNextAvailableIP()
	if err != nil {
		return "", fmt.Errorf("failed to find available IP: %w", err)
	}

	// Reserve the IP
	dm.allocatedIPs[macAddr] = ip

	// Add static DHCP entry
	if err := dm.addStaticEntry(macAddr, ip); err != nil {
		delete(dm.allocatedIPs, macAddr)
		return "", fmt.Errorf("failed to add static DHCP entry: %w", err)
	}

	return ip, nil
}

// ReleaseIP releases an IP address
func (dm *DHCPManager) ReleaseIP(macAddr string) error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	ip, exists := dm.allocatedIPs[macAddr]
	if !exists {
		return nil // Already released
	}

	// Remove static entry
	if err := dm.removeStaticEntry(macAddr, ip); err != nil {
		return fmt.Errorf("failed to remove static DHCP entry: %w", err)
	}

	delete(dm.allocatedIPs, macAddr)
	return nil
}

// GetLeases returns all current DHCP leases
func (dm *DHCPManager) GetLeases() ([]DHCPLease, error) {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	return dm.parseLeases()
}

// IsRunning checks if the DHCP server is running
func (dm *DHCPManager) IsRunning() bool {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	// Check if PID file exists and process is running
	pidBytes, err := ioutil.ReadFile(dm.pidFile)
	if err != nil {
		return false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return false
	}

	// Check if process exists
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process is alive
	err = process.Signal(os.Signal(nil))
	return err == nil
}

// generateConfig generates dnsmasq configuration
func (dm *DHCPManager) generateConfig() error {
	config := fmt.Sprintf(`# dnsmasq configuration for microVM DHCP
interface=%s
bind-interfaces
dhcp-range=%s,%s,%s
dhcp-option=3,%s
dhcp-option=6,%s
dhcp-leasefile=%s
pid-file=%s
log-facility=/var/log/dnsmasq.log
log-dhcp
`,
		dm.bridgeName,
		dm.ipRange.Start,
		dm.ipRange.End,
		dm.leaseTime,
		dm.gateway,
		strings.Join(dm.dnsServers, ","),
		dm.leasesFile,
		dm.pidFile,
	)

	return ioutil.WriteFile(dm.configFile, []byte(config), 0644)
}

// startDnsmasq starts the dnsmasq process
func (dm *DHCPManager) startDnsmasq() error {
	// Check if already running
	if dm.IsRunning() {
		return nil
	}

	cmd := exec.Command("dnsmasq", "-C", dm.configFile)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start dnsmasq: %w", err)
	}

	// Wait a moment for dnsmasq to start
	time.Sleep(500 * time.Millisecond)

	// Verify it's running
	if !dm.IsRunning() {
		return fmt.Errorf("dnsmasq failed to start properly")
	}

	return nil
}

// stopDnsmasq stops the dnsmasq process
func (dm *DHCPManager) stopDnsmasq() error {
	pidBytes, err := ioutil.ReadFile(dm.pidFile)
	if err != nil {
		return nil // Already stopped
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return fmt.Errorf("invalid PID in file: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return nil // Process doesn't exist
	}

	// Send SIGTERM
	if err := process.Signal(os.Interrupt); err != nil {
		// Try SIGKILL if SIGTERM fails
		process.Kill()
	}

	// Remove PID file
	os.Remove(dm.pidFile)

	return nil
}

// findNextAvailableIP finds the next available IP in the range
func (dm *DHCPManager) findNextAvailableIP() (string, error) {
	startIP := net.ParseIP(dm.ipRange.Start)
	endIP := net.ParseIP(dm.ipRange.End)

	if startIP == nil || endIP == nil {
		return "", fmt.Errorf("invalid IP range: %s - %s", dm.ipRange.Start, dm.ipRange.End)
	}

	// Convert IPs to integers for easier iteration
	start := ipToInt(startIP)
	end := ipToInt(endIP)

	// Check each IP in range
	for i := start; i <= end; i++ {
		ip := intToIP(i)
		ipStr := ip.String()

		// Check if IP is already allocated
		allocated := false
		for _, allocatedIP := range dm.allocatedIPs {
			if allocatedIP == ipStr {
				allocated = true
				break
			}
		}

		if !allocated {
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no available IPs in range %s - %s", dm.ipRange.Start, dm.ipRange.End)
}

// addStaticEntry adds a static DHCP entry
func (dm *DHCPManager) addStaticEntry(macAddr, ip string) error {
	// For now, we'll restart dnsmasq with updated config
	// In production, you might want to use dnsmasq's dynamic configuration
	return dm.regenerateConfigAndRestart()
}

// removeStaticEntry removes a static DHCP entry
func (dm *DHCPManager) removeStaticEntry(macAddr, ip string) error {
	// For now, we'll restart dnsmasq with updated config
	return dm.regenerateConfigAndRestart()
}

// regenerateConfigAndRestart regenerates config and restarts dnsmasq
func (dm *DHCPManager) regenerateConfigAndRestart() error {
	// Stop dnsmasq
	if err := dm.stopDnsmasq(); err != nil {
		return fmt.Errorf("failed to stop dnsmasq: %w", err)
	}

	// Generate new config with static entries
	if err := dm.generateConfigWithStaticEntries(); err != nil {
		return fmt.Errorf("failed to generate config: %w", err)
	}

	// Start dnsmasq
	if err := dm.startDnsmasq(); err != nil {
		return fmt.Errorf("failed to start dnsmasq: %w", err)
	}

	return nil
}

// generateConfigWithStaticEntries generates config including static entries
func (dm *DHCPManager) generateConfigWithStaticEntries() error {
	config := fmt.Sprintf(`# dnsmasq configuration for microVM DHCP
interface=%s
bind-interfaces
dhcp-range=%s,%s,%s
dhcp-option=3,%s
dhcp-option=6,%s
dhcp-leasefile=%s
pid-file=%s
log-facility=/var/log/dnsmasq.log
log-dhcp
`,
		dm.bridgeName,
		dm.ipRange.Start,
		dm.ipRange.End,
		dm.leaseTime,
		dm.gateway,
		strings.Join(dm.dnsServers, ","),
		dm.leasesFile,
		dm.pidFile,
	)

	// Add static entries
	for mac, ip := range dm.allocatedIPs {
		config += fmt.Sprintf("dhcp-host=%s,%s\n", mac, ip)
	}

	return ioutil.WriteFile(dm.configFile, []byte(config), 0644)
}

// loadLeases loads existing leases from file
func (dm *DHCPManager) loadLeases() error {
	leases, err := dm.parseLeases()
	if err != nil {
		return err
	}

	// Update allocated IPs map
	for _, lease := range leases {
		dm.allocatedIPs[lease.MAC] = lease.IP
	}

	return nil
}

// parseLeases parses the DHCP leases file
func (dm *DHCPManager) parseLeases() ([]DHCPLease, error) {
	content, err := ioutil.ReadFile(dm.leasesFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []DHCPLease{}, nil
		}
		return nil, fmt.Errorf("failed to read leases file: %w", err)
	}

	var leases []DHCPLease
	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}

		// dnsmasq lease format: timestamp mac ip hostname client-id
		timestamp, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}

		lease := DHCPLease{
			MAC:       parts[1],
			IP:        parts[2],
			Hostname:  parts[3],
			ExpiresAt: time.Unix(timestamp, 0),
		}

		leases = append(leases, lease)
	}

	return leases, nil
}

// Helper functions for IP conversion
func ipToInt(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 + uint32(ip[1])<<16 + uint32(ip[2])<<8 + uint32(ip[3])
}

func intToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}