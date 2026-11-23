package network

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ReservationManager handles MAC/IP reservations and security policies
type ReservationManager struct {
	hostsFile    string
	bridgeName   string
	subnet       *net.IPNet
	gateway      net.IP
	reservations map[string]string // MAC -> IP
	usedIPs      map[string]bool   // IP -> in-use
	usedMACs     map[string]bool   // MAC -> in-use
	mu           sync.RWMutex
}

// BandwidthLimit represents bandwidth limits for a VM from user perspective
// Note: Values are swapped when applying to TC because TAP direction is opposite of VM direction
type BandwidthLimit struct {
	IngressMbps int // VM download limit (user wget) - applied to TAP egress/root - lower for cost control
	EgressMbps  int // VM upload limit (user upload) - applied to TAP ingress - higher limit
}

// VMReservation represents a complete VM network reservation
type VMReservation struct {
	VMID           string
	MACAddress     string
	IPAddress      string
	BandwidthLimit *BandwidthLimit
}

// NewReservationManager creates a new reservation manager
func NewReservationManager(bridgeName, subnet, hostsFileDir string) (*ReservationManager, error) {
	// Parse subnet
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet: %w", err)
	}

	// Gateway is typically the first IP in the subnet
	gateway := make(net.IP, len(ipnet.IP))
	copy(gateway, ipnet.IP)
	gateway[len(gateway)-1] |= 1

	hostsFile := filepath.Join(hostsFileDir, "dnsmasq.hosts")

	rm := &ReservationManager{
		hostsFile:    hostsFile,
		bridgeName:   bridgeName,
		subnet:       ipnet,
		gateway:      gateway,
		reservations: make(map[string]string),
		usedIPs:      make(map[string]bool),
		usedMACs:     make(map[string]bool),
	}

	// Load existing reservations
	if err := rm.loadReservations(); err != nil {
		return nil, fmt.Errorf("failed to load reservations: %w", err)
	}

	return rm, nil
}

// ReserveForVM reserves a MAC/IP pair for a VM
func (rm *ReservationManager) ReserveForVM(vmID string) (*VMReservation, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Generate deterministic MAC from VM ID
	mac := rm.generateMAC(vmID)

	// Check if MAC already reserved
	if ip, exists := rm.reservations[mac]; exists {
		return &VMReservation{
			VMID:       vmID,
			MACAddress: mac,
			IPAddress:  ip,
		}, nil
	}

	// Allocate next available IP
	ip, err := rm.findNextAvailableIP()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate IP: %w", err)
	}

	// Save reservation
	rm.reservations[mac] = ip
	rm.usedIPs[ip] = true
	rm.usedMACs[mac] = true

	// Write to hosts file
	if err := rm.saveReservations(); err != nil {
		// Rollback
		delete(rm.reservations, mac)
		delete(rm.usedIPs, ip)
		delete(rm.usedMACs, mac)
		return nil, fmt.Errorf("failed to save reservation: %w", err)
	}

	// Reload dnsmasq to pick up new reservation
	if err := rm.reloadDnsmasq(); err != nil {
		return nil, fmt.Errorf("failed to reload dnsmasq: %w", err)
	}

	return &VMReservation{
		VMID:       vmID,
		MACAddress: mac,
		IPAddress:  ip,
	}, nil
}

// ReleaseForVM releases a VM's reservation
func (rm *ReservationManager) ReleaseForVM(vmID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	mac := rm.generateMAC(vmID)

	ip, exists := rm.reservations[mac]
	if !exists {
		return nil // Already released
	}

	delete(rm.reservations, mac)
	delete(rm.usedIPs, ip)
	delete(rm.usedMACs, mac)

	if err := rm.saveReservations(); err != nil {
		return fmt.Errorf("failed to save reservations: %w", err)
	}

	return rm.reloadDnsmasq()
}

// cleanupEbtablesRulesForTAP removes ALL ebtables rules for a specific TAP device
// This prevents duplicate rules from accumulating
func (rm *ReservationManager) cleanupEbtablesRulesForTAP(tapDevice string) error {
	if tapDevice == "" {
		return nil
	}

	// ebtables output format differs between legacy/nft backends and may not include
	// stable line numbers. Delete by full rule spec instead.
	listRules := func() (string, error) {
		variants := [][]string{
			{"ebtables", "-L", "FORWARD", "--Lx", "--Ln"},
			{"ebtables", "-L", "FORWARD", "--Ln"},
			{"ebtables", "-L", "FORWARD"},
		}
		for _, args := range variants {
			out, err := exec.Command("sudo", args...).CombinedOutput()
			if err == nil {
				return string(out), nil
			}
		}
		return "", fmt.Errorf("failed to list ebtables rules")
	}

	const maxPasses = 20
	totalRemoved := 0

	for pass := 0; pass < maxPasses; pass++ {
		rulesOutput, err := listRules()
		if err != nil {
			if pass == 0 {
				return err
			}
			break
		}

		removedThisPass := 0
		for _, line := range strings.Split(rulesOutput, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasPrefix(line, "-") || !strings.Contains(line, tapDevice) {
				continue
			}

			args := strings.Fields(line)
			if len(args) == 0 {
				continue
			}

			deleteArgs := append([]string{"ebtables", "-D", "FORWARD"}, args...)
			if err := exec.Command("sudo", deleteArgs...).Run(); err == nil {
				removedThisPass++
				totalRemoved++
			}
		}

		if removedThisPass == 0 {
			break
		}
	}

	if totalRemoved > 0 {
		fmt.Printf("🧹 Cleaned up %d existing ebtables rules for %s\n", totalRemoved, tapDevice)
	}

	return nil
}

// CleanupOrphanedEbtablesRules removes ebtables rules for TAP devices that no longer exist
// Call this at service startup to clean up stale rules
func (rm *ReservationManager) CleanupOrphanedEbtablesRules() error {
	// Get all ebtables FORWARD rules
	output, err := exec.Command("sudo", "ebtables", "-L", "FORWARD", "--Ln").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to list ebtables rules: %w", err)
	}

	// Find all fc-* TAP devices mentioned in rules
	lines := strings.Split(string(output), "\n")
	tapDevices := make(map[string]bool)
	for _, line := range lines {
		// Look for "-i fc-XXXX" pattern
		if idx := strings.Index(line, "-i fc-"); idx >= 0 {
			// Extract TAP device name
			rest := line[idx+3:] // Skip "-i "
			fields := strings.Fields(rest)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "fc-") {
				tapDevices[fields[0]] = true
			}
		}
	}

	// Check which TAP devices still exist
	orphanedTAPs := []string{}
	for tap := range tapDevices {
		if err := exec.Command("ip", "link", "show", "dev", tap).Run(); err != nil {
			// TAP device doesn't exist - it's orphaned
			orphanedTAPs = append(orphanedTAPs, tap)
		}
	}

	// Clean up rules for orphaned TAPs (both ebtables AND iptables)
	cleanedCount := 0
	for _, tap := range orphanedTAPs {
		rm.cleanupEbtablesRulesForTAP(tap)
		rm.cleanupIptablesRulesForTAP(tap)
		cleanedCount++
	}

	if cleanedCount > 0 {
		fmt.Printf("🧹 Cleaned up ebtables+iptables rules for %d orphaned TAP devices\n", cleanedCount)
	}

	return nil
}

// cleanupIptablesRulesForTAP removes ALL iptables FORWARD rules for a specific TAP device
// This prevents stale rules from accumulating for TAPs that no longer exist
func (rm *ReservationManager) cleanupIptablesRulesForTAP(tapDevice string) {
	// Use iptables-save to get all rules, filter out the ones for this TAP, then restore
	// This is much more efficient than deleting rules one by one
	const maxIterations = 500 // Safety limit

	// Strategy: repeatedly try to delete rules containing this TAP until none remain
	// We delete common rule patterns for this TAP device
	rulePatterns := [][]string{
		{"iptables", "-D", "FORWARD", "-i", tapDevice, "-m", "state", "--state", "INVALID", "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-i", tapDevice, "-p", "tcp", "--syn", "-m", "limit", "--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", tapDevice, "-p", "tcp", "--syn", "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-i", tapDevice, "-p", "icmp", "--icmp-type", "echo-request", "-m", "limit", "--limit", "1/sec", "--limit-burst", "2", "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", tapDevice, "-p", "icmp", "--icmp-type", "echo-request", "-j", "DROP"},
	}

	totalCleaned := 0
	for _, pattern := range rulePatterns {
		for i := 0; i < maxIterations; i++ {
			if err := exec.Command("sudo", pattern...).Run(); err != nil {
				break
			}
			totalCleaned++
		}
	}

	// Also clean any anti-spoof iptables rules for this TAP (pattern: -i tapDevice ! -s <ip> -j DROP)
	// These require checking iptables -S FORWARD output since the IP varies
	output, err := exec.Command("sudo", "iptables", "-S", "FORWARD").CombinedOutput()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, tapDevice) && strings.Contains(line, "! -s") && strings.Contains(line, "-j DROP") {
				// Parse the rule and convert -A to -D
				delRule := strings.Replace(line, "-A FORWARD", "-D FORWARD", 1)
				args := strings.Fields(delRule)
				if len(args) > 1 {
					exec.Command("sudo", append([]string{"iptables"}, args...)...).Run()
					totalCleaned++
				}
			}
		}
	}

	if totalCleaned > 0 {
		fmt.Printf("🧹 Cleaned up %d iptables FORWARD rules for orphaned TAP %s\n", totalCleaned, tapDevice)
	}
}

// ApplySpoofingProtection applies ebtables and iptables rules to prevent MAC/IP spoofing
func (rm *ReservationManager) ApplySpoofingProtection(tapDevice, macAddr, ipAddr string) error {
	// Allow disabling anti-spoofing via env for environments where firewall backend is incompatible
	if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_ENABLE"), "false") {
		fmt.Printf("🔕 Anti-spoofing disabled via MICROVM_ANTISPOOF_ENABLE=false\n")
		return nil
	}

	// Wait briefly until TAP device exists to avoid timing races
	for i := 0; i < 30; i++ { // ~3s max
		if err := exec.Command("ip", "link", "show", "dev", tapDevice).Run(); err == nil {
			break
		}
		// sleep 100ms
		_ = exec.Command("bash", "-lc", "sleep 0.1").Run()
	}

	// CRITICAL: Clean up any existing rules for this TAP device BEFORE adding new ones
	// This prevents duplicate rules from accumulating on VM restart
	rm.cleanupEbtablesRulesForTAP(tapDevice)

	// ebtables: Anti-spoofing with ARP passthrough
	// Strategy:
	// 1. Allow ARP from the VM (needed for inter-VM communication and gateway discovery)
	// 2. Block all other packets with wrong source MAC (prevents MAC spoofing for IP traffic)

	// Rule 1: Allow ARP packets from this VM (for inter-VM ping to work)
	allowARPRule := []string{
		"ebtables", "-I", "FORWARD", "1",
		"-i", tapDevice,
		"-p", "ARP",
		"-j", "ACCEPT",
	}

	// Rule 2: Drop non-ARP packets with wrong source MAC
	blockSpoofRule := []string{
		"ebtables", "-A", "FORWARD",
		"-i", tapDevice,
		"!", "-s", macAddr,
		"-j", "DROP",
	}

	// Apply ARP rule first (so it's evaluated before the DROP rule)
	if err := exec.Command("sudo", allowARPRule...).Run(); err != nil {
		if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_STRICT"), "false") {
			fmt.Printf("⚠️  Anti-spoof (ebtables ARP) failed but continuing (STRICT=false): %v\n", err)
		} else {
			return fmt.Errorf("failed to apply ebtables ARP rule: %w", err)
		}
	}

	// Apply MAC spoofing protection
	if err := exec.Command("sudo", blockSpoofRule...).Run(); err != nil {
		if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_STRICT"), "false") {
			fmt.Printf("⚠️  Anti-spoof (ebtables) failed but continuing (STRICT=false): %v\n", err)
		} else {
			return fmt.Errorf("failed to apply ebtables MAC filter: %w", err)
		}
	}

	// iptables: Only allow packets from the assigned IP address (anti-spoofing)
	// Skip if no valid IPv4 yet (e.g., DHCP not assigned). We'll rely on MAC-level ebtables until IP is known.
	// For DHCP scenarios, IP-based anti-spoofing can block legitimate traffic (DHCP replies, return packets).
	// Use MICROVM_ANTISPOOF_IP_ENABLE=true to enable IP-based filtering (default: disabled for DHCP compatibility)
	if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_IP_ENABLE"), "true") && ipAddr != "" && ipAddr != "0.0.0.0" {
		iptablesRule := []string{
			"iptables", "-I", "FORWARD",
			"-i", tapDevice,
			"!", "-s", ipAddr,
			"-j", "DROP",
		}

		if err := exec.Command("sudo", iptablesRule...).Run(); err != nil {
			if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_STRICT"), "false") {
				fmt.Printf("⚠️  Anti-spoof (iptables) failed but continuing (STRICT=false): %v\n", err)
			} else {
				return fmt.Errorf("failed to apply iptables IP filter: %w", err)
			}
		}
	}

	return nil
}

// RemoveSpoofingProtection removes anti-spoofing rules for a TAP device
func (rm *ReservationManager) RemoveSpoofingProtection(tapDevice, macAddr, ipAddr string) error {
	// Use the comprehensive cleanup that removes ALL rules for this TAP device
	rm.cleanupEbtablesRulesForTAP(tapDevice)

	// Remove iptables rule (only if IP-based anti-spoofing was enabled)
	if strings.EqualFold(os.Getenv("MICROVM_ANTISPOOF_IP_ENABLE"), "true") && ipAddr != "" && ipAddr != "0.0.0.0" {
		// Remove all matching iptables rules (may have duplicates)
		for i := 0; i < 10; i++ { // Try up to 10 times to catch duplicates
			iptablesRule := []string{
				"iptables", "-D", "FORWARD",
				"-i", tapDevice,
				"!", "-s", ipAddr,
				"-j", "DROP",
			}
			if err := exec.Command("sudo", iptablesRule...).Run(); err != nil {
				break // No more rules to remove
			}
		}
	}

	return nil
}

// ApplyBandwidthLimit applies traffic control (tc) bandwidth limits to a TAP device
// Optimized for smooth traffic with larger burst sizes and lower latency
func (rm *ReservationManager) ApplyBandwidthLimit(tapDevice string, limit *BandwidthLimit) error {
	if limit == nil {
		return nil
	}

	// Apply VM download limit using TBF on TAP root/egress - lower limit to save DC bandwidth costs
	// When VM user runs 'wget', traffic flows: Internet -> Host -> TAP root -> VM
	// We apply IngressMbps (VM download) to TAP egress because traffic exits TAP into VM
	if limit.IngressMbps > 0 {
		rate := fmt.Sprintf("%dmbit", limit.IngressMbps)
		// Optimized: burst 128kb, latency 50ms for smoother transfers
		if err := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "root", "tbf",
			"rate", rate, "burst", "128kb", "latency", "50ms").Run(); err != nil {
			return fmt.Errorf("failed to add egress qdisc: %w", err)
		}
	}

	// Apply VM upload limit using police filter on TAP ingress - higher limit
	// When VM user uploads, traffic flows: VM -> TAP ingress -> Host -> Internet
	// We apply EgressMbps (VM upload) to TAP ingress because traffic enters TAP from VM
	if limit.EgressMbps > 0 {
		rate := fmt.Sprintf("%dmbit", limit.EgressMbps)

		if err := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "handle", "ffff:", "ingress").Run(); err != nil {
			return fmt.Errorf("failed to add ingress qdisc: %w", err)
		}

		// Optimized: burst 128kb, mtu 64kb for smoother uploads
		if err := exec.Command("sudo", "tc", "filter", "add", "dev", tapDevice, "parent", "ffff:",
			"protocol", "all", "u32", "match", "u32", "0", "0",
			"police", "rate", rate, "burst", "128kb", "mtu", "64kb", "drop").Run(); err != nil {
			return fmt.Errorf("failed to add ingress policing: %w", err)
		}
	}

	return nil
}

// RemoveBandwidthLimit removes traffic control rules from a TAP device
func (rm *ReservationManager) RemoveBandwidthLimit(tapDevice string) error {
	// Remove root qdisc (this removes all egress rules)
	exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "root").Run()

	// Remove ingress qdisc
	exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "ingress").Run()

	return nil
}

// generateMAC generates a deterministic MAC address from VM ID
func (rm *ReservationManager) generateMAC(vmID string) string {
	// Use MD5 hash of VM ID to generate MAC
	hash := md5.Sum([]byte(vmID))

	// Use Firecracker OUI prefix: 02:fc:76
	// Local admin bit set (0x02), unicast
	mac := fmt.Sprintf("02:fc:76:%02x:%02x:%02x",
		hash[0], hash[1], hash[2])

	return mac
}

// findNextAvailableIP finds the next available IP in the subnet
func (rm *ReservationManager) findNextAvailableIP() (string, error) {
	// Start from gateway + 10 to leave some space for static assignments
	ip := make(net.IP, len(rm.gateway))
	copy(ip, rm.gateway)
	ip[len(ip)-1] += 10

	// Iterate through subnet
	for rm.subnet.Contains(ip) {
		ipStr := ip.String()

		// Skip gateway, broadcast, and already used IPs
		if !ip.Equal(rm.gateway) &&
			!ip.Equal(rm.subnet.IP) &&
			!rm.usedIPs[ipStr] {
			return ipStr, nil
		}

		// Increment IP
		ip = incrementIP(ip)
	}

	return "", fmt.Errorf("no available IPs in subnet %s", rm.subnet.String())
}

// loadReservations loads existing reservations from the hosts file
func (rm *ReservationManager) loadReservations() error {
	content, err := os.ReadFile(rm.hostsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist yet
		}
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Format: MAC,IP
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}

		mac := strings.ToLower(strings.TrimSpace(parts[0]))
		ip := strings.TrimSpace(parts[1])

		rm.reservations[mac] = ip
		rm.usedIPs[ip] = true
		rm.usedMACs[mac] = true
	}

	return nil
}

// saveReservations saves reservations to the hosts file
func (rm *ReservationManager) saveReservations() error {
	var lines []string
	lines = append(lines, "# Static DHCP reservations for microVMs")
	lines = append(lines, "# Format: MAC,IP")

	for mac, ip := range rm.reservations {
		lines = append(lines, fmt.Sprintf("%s,%s", mac, ip))
	}

	content := strings.Join(lines, "\n") + "\n"

	// Create directory if it doesn't exist
	dir := filepath.Dir(rm.hostsFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	return os.WriteFile(rm.hostsFile, []byte(content), 0644)
}

// reloadDnsmasq sends SIGHUP to dnsmasq to reload configuration
func (rm *ReservationManager) reloadDnsmasq() error {
	// For lxdbr0, use snap's lxd dnsmasq
	// Try to find the PID
	out, err := exec.Command("pgrep", "-f", "dnsmasq.*lxdbr0").Output()
	if err != nil {
		// No dnsmasq running for lxdbr0, that's okay
		return nil
	}

	pids := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, pidStr := range pids {
		if pidStr == "" {
			continue
		}

		// Send SIGHUP to reload
		if err := exec.Command("sudo", "kill", "-HUP", pidStr).Run(); err != nil {
			return fmt.Errorf("failed to reload dnsmasq (PID %s): %w", pidStr, err)
		}
	}

	return nil
}

// Helper function to increment IP address
func incrementIP(ip net.IP) net.IP {
	ip = ip.To4()
	if ip == nil {
		return nil
	}

	// Convert to uint32
	val := binary.BigEndian.Uint32(ip)
	val++

	// Convert back to IP
	newIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(newIP, val)
	return newIP
}

// GetReservation returns the reservation for a VM
func (rm *ReservationManager) GetReservation(vmID string) (*VMReservation, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	mac := rm.generateMAC(vmID)
	ip, exists := rm.reservations[mac]
	if !exists {
		return nil, fmt.Errorf("no reservation found for VM %s", vmID)
	}

	return &VMReservation{
		VMID:       vmID,
		MACAddress: mac,
		IPAddress:  ip,
	}, nil
}

// ListReservations returns all current reservations
func (rm *ReservationManager) ListReservations() []*VMReservation {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var reservations []*VMReservation
	for mac, ip := range rm.reservations {
		reservations = append(reservations, &VMReservation{
			MACAddress: mac,
			IPAddress:  ip,
		})
	}

	return reservations
}

// TrafficStats represents network traffic statistics
type TrafficStats struct {
	IngressBytes uint64 // Bytes received (download)
	EgressBytes  uint64 // Bytes transmitted (upload)
}

// GetTrafficStats retrieves traffic statistics for a TAP device using iptables counters
func (rm *ReservationManager) GetTrafficStats(tapDevice string) (*TrafficStats, error) {
	if tapDevice == "" {
		return nil, fmt.Errorf("TAP device name is required")
	}

	stats := &TrafficStats{}

	// Get ingress (download) bytes: packets FROM bridge TO tap device
	cmd := exec.Command("sudo", "iptables", "-L", "FORWARD", "-v", "-x", "-n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get iptables stats: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Look for forward rules involving our TAP device
		if strings.Contains(line, tapDevice) {
			// Parse: pkts bytes target prot opt in out source destination
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				var bytes uint64
				fmt.Sscanf(fields[1], "%d", &bytes)

				// Ingress: traffic going TO the tap device (out column)
				if len(fields) >= 7 && fields[6] == tapDevice {
					stats.IngressBytes += bytes
				}
				// Egress: traffic coming FROM the tap device (in column)
				if len(fields) >= 6 && fields[5] == tapDevice {
					stats.EgressBytes += bytes
				}
			}
		}
	}

	// Fallback: use /sys/class/net if iptables stats are not available
	if stats.IngressBytes == 0 && stats.EgressBytes == 0 {
		stats.IngressBytes, stats.EgressBytes = rm.getTrafficFromSysfs(tapDevice)
	}

	return stats, nil
}

// getTrafficFromSysfs reads traffic stats from /sys/class/net
func (rm *ReservationManager) getTrafficFromSysfs(tapDevice string) (uint64, uint64) {
	var rxBytes, txBytes uint64

	// Read RX bytes (ingress)
	rxPath := fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", tapDevice)
	if data, err := os.ReadFile(rxPath); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &rxBytes)
	}

	// Read TX bytes (egress)
	txPath := fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", tapDevice)
	if data, err := os.ReadFile(txPath); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &txBytes)
	}

	return rxBytes, txBytes
}

// ResetTrafficCounters resets traffic counters for a TAP device
// Note: /sys/class/net counters cannot be reset, so this is a logical reset
// The calling code should store the current value and calculate deltas
func (rm *ReservationManager) ResetTrafficCounters(tapDevice string) error {
	// Traffic counters in /sys/class/net are kernel-maintained and cannot be reset
	// The reset is handled by the caller storing current values and calculating deltas
	// This function is kept for API consistency
	return nil
}
