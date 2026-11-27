package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/ovs"
)

// NetworkManager handles network configuration for microVMs
type NetworkManager struct {
	natBridge   string
	dhcpRange   string
	macPrefix   string
	portCounter uint32

	// ovnAllocate is set by NewManager when the OVN backend is selected. It
	// creates the VM's logical port, DHCP reservation and port security, and
	// returns the address the guest should boot with.
	ovnAllocate func(vmID, mac, networkID string) (*models.NetworkInfo, error)
}

// SetOVNAllocator wires the OVN backend into the network manager. It is
// optional: without it OVN modes fall back to a placeholder address.
func (nm *NetworkManager) SetOVNAllocator(fn func(vmID, mac, networkID string) (*models.NetworkInfo, error)) {
	nm.ovnAllocate = fn
}

// NewNetworkManager creates a new network manager
func NewNetworkManager() (*NetworkManager, error) {
	return &NetworkManager{
		natBridge: "fc-br0",
		dhcpRange: "192.168.100.0/24",
		// Locally administered unicast prefix (first octet 0x02). Use 3-byte OUI-style prefix.
		macPrefix:   "02:fc:76",
		portCounter: 2200, // Starting SSH port for NAT mode
	}, nil
}

// truncateVMID safely truncates the VM ID to the specified length
// If the ID is shorter than the requested length, returns the full ID
func truncateVMID(vmID string, maxLen int) string {
	if len(vmID) <= maxLen {
		return vmID
	}
	return vmID[:maxLen]
}

// dhcpTapNameFor computes the deterministic TAP name used in DHCP mode
// Format: fc-XXXX where XXXX is a 4-hex-digit hash from the VM ID
func dhcpTapNameFor(microvmID string) string {
	hash := 0
	for _, c := range microvmID {
		hash = hash*31 + int(c)
	}
	if hash < 0 {
		hash = -hash
	}
	return fmt.Sprintf("fc-%04x", hash%0x10000)
}

// ConfigureNetwork configures network for a microVM based on the specified mode
// customMAC is an optional MAC address to use instead of generating one
// bandwidthIngress and bandwidthEgress are optional bandwidth limits (e.g., "100mbit")
// customIPv6, customIPv6Gateway, customIPv6Prefix are optional custom IPv6 settings
func (nm *NetworkManager) ConfigureNetwork(ctx context.Context, microvmID string, mode models.NetworkMode, customMAC, bandwidthIngress, bandwidthEgress, customIPv6, customIPv6Gateway string, customIPv6Prefix int, networkID string) (*models.NetworkInfo, error) {
	switch mode {
	case models.NetworkModeNAT:
		return nm.configureNATNetwork(ctx, microvmID)
	case models.NetworkModeDHCP:
		return nm.configureDHCPNetwork(ctx, microvmID, customMAC, bandwidthIngress, bandwidthEgress)
	case models.NetworkModeMACVLAN:
		return nm.configureMACVLANNetwork(ctx, microvmID, customMAC, bandwidthIngress, bandwidthEgress, customIPv6, customIPv6Gateway, customIPv6Prefix)
	case models.NetworkModeIPv6Only:
		return nm.configureIPv6OnlyNetwork(ctx, microvmID, customMAC, bandwidthIngress, bandwidthEgress, customIPv6, customIPv6Gateway, customIPv6Prefix)
	case models.NetworkModeOVN:
		return nm.configureOVNNetwork(ctx, microvmID, networkID)
	default:
		return nil, fmt.Errorf("unsupported network mode: %s", mode)
	}
}

// configureNATNetwork configures NAT networking for local development
func (nm *NetworkManager) configureNATNetwork(ctx context.Context, microvmID string) (*models.NetworkInfo, error) {
	// Generate MAC address
	macAddr := nm.generateMACAddress(microvmID)

	// Create TAP interface
	tapName := fmt.Sprintf("fc-tap-%s", truncateVMID(microvmID, 8))
	if err := nm.createTAPInterface(tapName); err != nil {
		return nil, fmt.Errorf("failed to create TAP interface: %w", err)
	}

	// Ensure NAT bridge exists
	if err := nm.ensureNATBridge(); err != nil {
		return nil, fmt.Errorf("failed to ensure NAT bridge: %w", err)
	}

	// Add TAP to bridge
	if err := nm.addTAPToBridge(tapName, nm.natBridge); err != nil {
		return nil, fmt.Errorf("failed to add TAP to bridge: %w", err)
	}

	// Assign IP address from NAT range
	ipAddr, err := nm.assignNATIP(microvmID)
	if err != nil {
		return nil, fmt.Errorf("failed to assign NAT IP: %w", err)
	}

	// Get next available SSH port
	sshPort := nm.getNextSSHPort()

	// Configure port forwarding for SSH
	if err := nm.configurePortForwarding(sshPort, ipAddr, 22); err != nil {
		return nil, fmt.Errorf("failed to configure port forwarding: %w", err)
	}

	return &models.NetworkInfo{
		IPAddress:        ipAddr,
		MACAddress:       macAddr,
		AssignmentMethod: models.NetworkModeNAT,
		SSHPort:          sshPort,
	}, nil
}

// configureDHCPNetwork configures DHCP networking using an existing bridge
// customMAC is an optional MAC address to use instead of generating one
// bandwidthIngress and bandwidthEgress are optional bandwidth limits (e.g., "100mbit")
func (nm *NetworkManager) configureDHCPNetwork(ctx context.Context, microvmID string, customMAC, bandwidthIngress, bandwidthEgress string) (*models.NetworkInfo, error) {
	// Use custom MAC if provided, otherwise generate one
	var macAddr string
	if customMAC != "" {
		macAddr = strings.ToLower(customMAC)
		fmt.Printf("🔍 DHCP: Using custom MAC address %s for VM %s\n", macAddr, microvmID)
	} else {
		macAddr = nm.generateMACAddress(microvmID)
		macAddr = strings.ToLower(macAddr)
		fmt.Printf("🔍 DHCP: Generated MAC address %s for VM %s\n", macAddr, microvmID)
	}

	// Create TAP interface with deterministic short hash-based name
	tapName := dhcpTapNameFor(microvmID)

	// Use provided bandwidth limits or fallback to environment variables
	ingress := bandwidthIngress
	egress := bandwidthEgress
	fmt.Printf("🔍 DEBUG configureDHCPNetwork: VM %s, bandwidthIngress='%s', bandwidthEgress='%s'\n", microvmID, bandwidthIngress, bandwidthEgress)
	if ingress == "" {
		ingress = os.Getenv("MICROVM_BANDWIDTH_INGRESS")
		fmt.Printf("🔍 DEBUG: bandwidthIngress empty, checking env MICROVM_BANDWIDTH_INGRESS='%s'\n", ingress)
	}
	if egress == "" {
		egress = os.Getenv("MICROVM_BANDWIDTH_EGRESS")
		fmt.Printf("🔍 DEBUG: bandwidthEgress empty, checking env MICROVM_BANDWIDTH_EGRESS='%s'\n", egress)
	}

	// Create TAP with bandwidth limits if specified
	if ingress != "" || egress != "" {
		fmt.Printf("🚦 Creating TAP with bandwidth limits: Ingress=%s, Egress=%s\n", ingress, egress)
		if err := nm.createTAPInterfaceWithBandwidth(tapName, ingress, egress); err != nil {
			return nil, fmt.Errorf("failed to create TAP interface with bandwidth limits: %w", err)
		}
	} else {
		if err := nm.createTAPInterface(tapName); err != nil {
			return nil, fmt.Errorf("failed to create TAP interface: %w", err)
		}
	}

	// Get bridge name from environment or use default lxdbr0 (good for local testing)
	// Common options: lxdbr0 (LXD), br0 (manual bridge), virbr0 (libvirt), fc-br0 (firecracker)
	bridgeName := "lxdbr0"
	// Check MICROVM_NETWORK_BRIDGE first (new standard), fallback to MICROVM_DHCP_BRIDGE (legacy)
	if customBridge := os.Getenv("MICROVM_NETWORK_BRIDGE"); customBridge != "" {
		bridgeName = customBridge
	} else if customBridge := os.Getenv("MICROVM_DHCP_BRIDGE"); customBridge != "" {
		bridgeName = customBridge
	}

	// Connect to existing bridge (don't create it)
	if err := nm.addTAPToBridge(tapName, bridgeName); err != nil {
		return nil, fmt.Errorf("failed to add TAP to bridge %s: %w", bridgeName, err)
	}

	// In DHCP mode, IP will be assigned by DHCP server
	// We'll need to wait and discover it or use DHCP reservation
	ipAddr, err := nm.reserveDHCPIP(macAddr)
	if err != nil {
		// If reservation fails, use placeholder - IP will be assigned by DHCP
		ipAddr = "0.0.0.0" // Will be updated once DHCP assigns
	}

	// Apply anti-spoofing protection (enabled by default, can be disabled with MICROVM_DISABLE_ANTISPOOF=true)
	if os.Getenv("MICROVM_DISABLE_ANTISPOOF") != "true" {
		fmt.Printf("🔒 Applying anti-spoofing protection: TAP=%s, MAC=%s, IP=%s\n", tapName, macAddr, ipAddr)
		if err := nm.applyAntiSpoofing(tapName, macAddr, ipAddr, ""); err != nil {
			fmt.Printf("⚠️  Warning: Failed to apply anti-spoofing rules: %v\n", err)
		} else {
			fmt.Printf("✅ Anti-spoofing protection applied successfully\n")
		}
	}

	// Apply DDoS protection (enabled by default, can be disabled with MICROVM_DISABLE_DDOS_PROTECTION=true)
	if os.Getenv("MICROVM_DISABLE_DDOS_PROTECTION") != "true" {
		fmt.Printf("🛡️  Applying DDoS protection: TAP=%s\n", tapName)
		if err := nm.applyDDoSProtection(tapName); err != nil {
			fmt.Printf("⚠️  Warning: Failed to apply DDoS protection: %v\n", err)
		} else {
			fmt.Printf("✅ DDoS protection applied successfully\n")
		}
	}

	return &models.NetworkInfo{
		IPAddress:        ipAddr,
		MACAddress:       macAddr,
		AssignmentMethod: models.NetworkModeDHCP,
		SSHPort:          22, // Standard SSH port in DHCP mode
		TAPDevice:        tapName,
		BridgeName:       bridgeName,
		BandwidthIngress: ingress,
		BandwidthEgress:  egress,
	}, nil
}

// configureMACVLANNetwork configures networking for OVH IPv6 nodes using shared bridge
// This uses the production-proven topology: TAP <-> br-microvm (shared bridge)
// - VMs connect to a single shared bridge (br-microvm) that has the host's IPv6
// - ndppd handles NDP proxy for VM IPv6 addresses
// - IPv6 routing directs VM traffic through br-microvm
// customMAC is optional - if provided by API, use it instead of generating one
// bandwidthIngress and bandwidthEgress are optional bandwidth limits (e.g., "100mbit")
// customIPv6, customIPv6Gateway, customIPv6Prefix are optional custom IPv6 settings
func (nm *NetworkManager) configureMACVLANNetwork(ctx context.Context, microvmID, customMAC, bandwidthIngress, bandwidthEgress, customIPv6, customIPv6Gateway string, customIPv6Prefix int) (*models.NetworkInfo, error) {
	// Get parent interface from environment (required for IPv6 routing)
	parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
	if parentIface == "" {
		// Fallback to auto-detection
		var err error
		parentIface, err = nm.getMainInterface()
		if err != nil {
			return nil, fmt.Errorf("failed to get parent interface (set MICROVM_MACVLAN_PARENT): %w", err)
		}
		fmt.Printf("⚠️  MICROVM_MACVLAN_PARENT not set, auto-detected: %s\n", parentIface)
	}

	// Use custom MAC if provided, otherwise generate one
	var macAddr string
	if customMAC != "" {
		macAddr = strings.ToLower(customMAC)
		fmt.Printf("🔍 MACVLAN: Using custom MAC address %s for VM %s\n", macAddr, microvmID)
	} else {
		macAddr = nm.generateMACAddress(microvmID)
		macAddr = strings.ToLower(macAddr)
		fmt.Printf("🔍 MACVLAN: Generated MAC address %s for VM %s\n", macAddr, microvmID)
	}

	// Use shared bridge (production pattern)
	bridgeName := "br-microvm"
	tapName := fmt.Sprintf("tap-%s", truncateVMID(microvmID, 11))

	fmt.Printf("🌐 Shared bridge setup for VM %s:\n", microvmID)
	fmt.Printf("   TAP: %s, Bridge: %s (shared), Physical: %s\n", tapName, bridgeName, parentIface)

	// Ensure shared bridge exists (should be pre-configured by admin/systemd)
	if err := exec.Command("ip", "link", "show", bridgeName).Run(); err != nil {
		// Bridge doesn't exist - create it
		cmd := exec.Command("sudo", "ip", "link", "add", "name", bridgeName, "type", "bridge")
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("failed to create shared bridge %s: %w (output: %s)",
				bridgeName, err, strings.TrimSpace(string(output)))
		}
		fmt.Printf("   ✅ Created shared bridge %s\n", bridgeName)

		// Configure bridge with IPv4 (NAT) address
		cmd = exec.Command("sudo", "ip", "addr", "add", "10.100.0.1/24", "dev", bridgeName)
		if output, err := cmd.CombinedOutput(); err != nil {
			if !strings.Contains(string(output), "RTNETLINK answers: File exists") {
				fmt.Printf("   ⚠️  Warning: Failed to add IPv4 to bridge: %v\n", err)
			}
		}
	} else {
		fmt.Printf("   ♻️  Using existing shared bridge %s\n", bridgeName)
	}

	// Bring bridge up
	cmd := exec.Command("sudo", "ip", "link", "set", "dev", bridgeName, "up")
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to bring bridge %s up: %w (output: %s)",
			bridgeName, err, strings.TrimSpace(string(output)))
	}

	// Create TAP device for Firecracker
	if err := nm.createTAPInterface(tapName); err != nil {
		return nil, fmt.Errorf("failed to create TAP interface: %w", err)
	}
	fmt.Printf("   ✅ Created TAP interface %s\n", tapName)

	// Attach TAP to shared bridge
	cmd = exec.Command("sudo", "ip", "link", "set", tapName, "master", bridgeName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to attach TAP %s to bridge %s: %w (output: %s)",
			tapName, bridgeName, err, strings.TrimSpace(string(output)))
	}

	// Bring TAP interface up
	cmd = exec.Command("sudo", "ip", "link", "set", "dev", tapName, "up")
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to bring TAP %s up: %w (output: %s)",
			tapName, err, strings.TrimSpace(string(output)))
	}

	fmt.Printf("   ✅ Network topology: %s (TAP) <-> %s (Shared Bridge)\n", tapName, bridgeName)

	// Apply bandwidth limits
	ingress := bandwidthIngress
	egress := bandwidthEgress
	if ingress == "" {
		ingress = os.Getenv("MICROVM_BANDWIDTH_INGRESS")
	}
	if egress == "" {
		egress = os.Getenv("MICROVM_BANDWIDTH_EGRESS")
	}

	if egress != "" {
		fmt.Printf("🚦 Applying egress bandwidth limit on TAP: %s\n", egress)
		// Apply on TAP device for per-VM bandwidth limiting
		cmd = exec.Command("sudo", "tc", "qdisc", "add", "dev", tapName, "root", "tbf",
			"rate", egress, "burst", "32kbit", "latency", "400ms")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to set egress limit: %v\n", err)
		}
	}

	if ingress != "" {
		fmt.Printf("🚦 Applying ingress bandwidth limit on TAP: %s\n", ingress)
		cmd = exec.Command("sudo", "tc", "qdisc", "add", "dev", tapName, "handle", "ffff:", "ingress")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to add ingress qdisc: %v\n", err)
		} else {
			cmd = exec.Command("sudo", "tc", "filter", "add", "dev", tapName, "parent", "ffff:",
				"protocol", "ip", "prio", "50", "u32", "match", "u32", "0", "0",
				"police", "rate", ingress, "burst", "10k", "drop", "flowid", ":1")
			if err := cmd.Run(); err != nil {
				fmt.Printf("⚠️  Warning: Failed to set ingress limit: %v\n", err)
			}
		}
	}

	// Default IPv4 for NAT (VMs use 10.100.0.x range)
	ipAddr := "0.0.0.0" // Will be assigned inside VM via DHCP or static config

	// Handle IPv6 configuration - use custom values if provided
	var ipv6Addr, ipv6Gateway string
	var ipv6Prefix int

	if customIPv6 != "" {
		// Use custom IPv6 provided by API
		ipv6Addr = customIPv6
		ipv6Gateway = customIPv6Gateway
		ipv6Prefix = customIPv6Prefix
		if ipv6Prefix == 0 {
			ipv6Prefix = 64 // Default to /64 if not specified
		}
		fmt.Printf("🌐 Using custom IPv6: %s/%d (Gateway: %s)\n", ipv6Addr, ipv6Prefix, ipv6Gateway)
	} else {
		// Generate IPv6 address from MAC using SLAAC (OVH standard)
		ipv6Addr, ipv6Gateway, ipv6Prefix = nm.generateIPv6FromMAC(macAddr, parentIface)
		if ipv6Addr != "" {
			fmt.Printf("🌐 Auto-generated IPv6: %s/%d (Gateway: %s)\n", ipv6Addr, ipv6Prefix, ipv6Gateway)
		}
	}

	fmt.Printf("✅ TAP %s attached to shared bridge %s\n", tapName, bridgeName)

	// Configure IPv6 routing if IPv6 address is assigned
	if ipv6Addr != "" && ipv6Addr != "0.0.0.0" {
		fmt.Printf("🌐 Configuring IPv6 routing for %s...\n", ipv6Addr)

		// Add route to VM through shared bridge (for host to reach VM)
		cmd = exec.Command("sudo", "ip", "-6", "route", "replace", fmt.Sprintf("%s/128", ipv6Addr), "dev", bridgeName)
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to add IPv6 route: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
		} else {
			fmt.Printf("✅ Added IPv6 route: %s/128 → %s\n", ipv6Addr, bridgeName)
		}

		// Enable IPv6 forwarding (should already be enabled system-wide)
		cmd = exec.Command("sudo", "sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to enable IPv6 forwarding: %v\n", err)
		}

		// Enable NDP proxy on parent interface
		cmd = exec.Command("sudo", "sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.proxy_ndp=1", parentIface))
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to enable NDP proxy on %s: %v\n", parentIface, err)
		}

		// Enable NDP proxy on bridge
		cmd = exec.Command("sudo", "sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.proxy_ndp=1", bridgeName))
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to enable NDP proxy on %s: %v\n", bridgeName, err)
		}

		// Add NDP proxy entry for VM's IPv6 address on parent interface (critical for external reachability)
		// This allows the host to respond to NDP solicitations from the OVH gateway for this VM's IPv6
		ndpCmd := exec.Command("sudo", "ip", "-6", "neigh", "add", "proxy", ipv6Addr, "dev", parentIface)
		if output, err := ndpCmd.CombinedOutput(); err != nil {
			if !strings.Contains(string(output), "File exists") {
				fmt.Printf("⚠️  Warning: Failed to add NDP proxy for VM IPv6: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
			}
		} else {
			fmt.Printf("✅ Added NDP proxy for %s on %s\n", ipv6Addr, parentIface)
		}

		// Configure gateway reachability on bridge (critical for OVH)
		// VMs use the OVH gateway directly, so we need to make it reachable from the bridge
		if ipv6Gateway != "" {
			// Get parent interface MAC address
			macOutput, err := exec.Command("ip", "link", "show", parentIface).CombinedOutput()
			if err == nil {
				lines := strings.Split(string(macOutput), "\n")
				for _, line := range lines {
					if strings.Contains(line, "link/ether") {
						fields := strings.Fields(line)
						if len(fields) >= 2 {
							physMAC := fields[1]
							// Add gateway as permanent neighbor on bridge pointing to host's physical MAC
							gwCmd := exec.Command("sudo", "ip", "-6", "neigh", "replace", ipv6Gateway, "lladdr", physMAC, "dev", bridgeName, "nud", "permanent")
							if output, err := gwCmd.CombinedOutput(); err != nil {
								fmt.Printf("⚠️  Warning: Failed to add gateway neighbor on bridge: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
							} else {
								fmt.Printf("✅ Added gateway neighbor %s → %s on %s\n", ipv6Gateway, physMAC, bridgeName)
							}
							break
						}
					}
				}
			}

			// Add NDP proxy entry for gateway on bridge
			gwProxyCmd := exec.Command("sudo", "ip", "-6", "neigh", "add", "proxy", ipv6Gateway, "dev", bridgeName)
			if output, err := gwProxyCmd.CombinedOutput(); err != nil {
				if !strings.Contains(string(output), "File exists") {
					fmt.Printf("⚠️  Warning: Failed to add gateway proxy on bridge: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
				}
			}
		}

		fmt.Printf("✅ IPv6 routing configured for VM (relies on ndppd for NDP proxy)\n")
	}

	return &models.NetworkInfo{
		IPAddress:        ipAddr,
		IPv6Address:      ipv6Addr,
		IPv6Gateway:      ipv6Gateway,
		IPv6PrefixLength: ipv6Prefix,
		MACAddress:       macAddr,
		AssignmentMethod: models.NetworkModeMACVLAN,
		SSHPort:          22,
		TAPDevice:        tapName,    // Firecracker uses TAP device
		BridgeName:       bridgeName, // Store bridge name for reference
		BandwidthIngress: ingress,
		BandwidthEgress:  egress,
	}, nil
}

// configureIPv6OnlyNetwork configures IPv6-only networking with private IPv4 for NAT
// VMs get a unique public IPv6 address and a private IPv4 (10.100.0.x) for accessing IPv4-only sites
func (nm *NetworkManager) configureIPv6OnlyNetwork(ctx context.Context, microvmID, customMAC, bandwidthIngress, bandwidthEgress, customIPv6, customIPv6Gateway string, customIPv6Prefix int) (*models.NetworkInfo, error) {
	// Use custom MAC if provided, otherwise generate one
	var macAddr string
	if customMAC != "" {
		macAddr = strings.ToLower(customMAC)
		fmt.Printf("🌐 IPv6-only: Using custom MAC address %s for VM %s\n", macAddr, microvmID)
	} else {
		macAddr = nm.generateMACAddress(microvmID)
		macAddr = strings.ToLower(macAddr)
		fmt.Printf("🌐 IPv6-only: Generated MAC address %s for VM %s\n", macAddr, microvmID)
	}

	// Get parent interface and bridge from environment
	parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
	if parentIface == "" {
		// Auto-detect default route interface
		cmd := exec.Command("ip", "route", "show", "default")
		output, err := cmd.CombinedOutput()
		if err == nil {
			fields := strings.Fields(string(output))
			for i, f := range fields {
				if f == "dev" && i+1 < len(fields) {
					parentIface = fields[i+1]
					break
				}
			}
		}
		if parentIface == "" {
			parentIface = "eno1" // Fallback to OVH common interface
		}
	}

	bridgeName := os.Getenv("MICROVM_NETWORK_BRIDGE")
	if bridgeName == "" {
		bridgeName = os.Getenv("MICROVM_DHCP_BRIDGE")
	}
	if bridgeName == "" {
		bridgeName = "br-microvm" // Default bridge with NAT support
	}

	// Create TAP interface with deterministic name based on VM ID
	// Format: tap-VMID (truncated to 11 chars to fit Linux 15-char limit)
	// This ensures each VM gets its own unique TAP device that persists across restarts
	tapName := fmt.Sprintf("tap-%s", truncateVMID(microvmID, 11))

	// Use bandwidth limits if specified
	ingress := bandwidthIngress
	egress := bandwidthEgress
	if ingress == "" {
		ingress = os.Getenv("MICROVM_BANDWIDTH_INGRESS")
	}
	if egress == "" {
		egress = os.Getenv("MICROVM_BANDWIDTH_EGRESS")
	}

	if ingress != "" || egress != "" {
		fmt.Printf("🚦 Creating TAP with bandwidth limits: Ingress=%s, Egress=%s\n", ingress, egress)
		if err := nm.createTAPInterfaceWithBandwidth(tapName, ingress, egress); err != nil {
			return nil, fmt.Errorf("failed to create TAP interface with bandwidth limits: %w", err)
		}
	} else {
		if err := nm.createTAPInterface(tapName); err != nil {
			return nil, fmt.Errorf("failed to create TAP interface: %w", err)
		}
	}

	// Add TAP to bridge
	if err := nm.addTAPToBridge(tapName, bridgeName); err != nil {
		return nil, fmt.Errorf("failed to add TAP to bridge %s: %w", bridgeName, err)
	}

	// Handle IPv6 configuration - use custom values if provided, otherwise auto-generate
	var ipv6Addr, ipv6Gateway string
	var ipv6Prefix int

	if customIPv6 != "" {
		ipv6Addr = customIPv6
		ipv6Gateway = customIPv6Gateway
		ipv6Prefix = customIPv6Prefix
		if ipv6Prefix == 0 {
			ipv6Prefix = 64
		}
		fmt.Printf("🌐 IPv6-only: Using custom IPv6: %s/%d (Gateway: %s)\n", ipv6Addr, ipv6Prefix, ipv6Gateway)
	} else {
		// Generate IPv6 address from MAC using SLAAC (OVH standard)
		ipv6Addr, ipv6Gateway, ipv6Prefix = nm.generateIPv6FromMAC(macAddr, parentIface)
		if ipv6Addr != "" {
			fmt.Printf("🌐 IPv6-only: Auto-generated IPv6: %s/%d (Gateway: %s)\n", ipv6Addr, ipv6Prefix, ipv6Gateway)
		} else {
			return nil, fmt.Errorf("failed to generate IPv6 address - check MICROVM_IPV6_SUBNET or host IPv6 configuration")
		}
	}

	// Configure IPv6 routing on host
	if ipv6Addr != "" {
		fmt.Printf("🌐 Configuring IPv6 routing for %s...\n", ipv6Addr)

		// Add route to VM through bridge
		cmd := exec.Command("sudo", "ip", "-6", "route", "replace", fmt.Sprintf("%s/128", ipv6Addr), "dev", bridgeName)
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to add IPv6 route: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
		} else {
			fmt.Printf("✅ Added IPv6 route: %s/128 → %s\n", ipv6Addr, bridgeName)
		}

		// Add NDP proxy entry on parent interface
		cmd = exec.Command("sudo", "ip", "-6", "neigh", "add", "proxy", ipv6Addr, "dev", parentIface)
		if output, err := cmd.CombinedOutput(); err != nil {
			if !strings.Contains(string(output), "File exists") {
				fmt.Printf("⚠️  Warning: Failed to add NDP proxy: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
			}
		} else {
			fmt.Printf("✅ Added NDP proxy for %s on %s\n", ipv6Addr, parentIface)
		}

		// Enable NDP proxy on interfaces
		exec.Command("sudo", "sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.proxy_ndp=1", parentIface)).Run()
		exec.Command("sudo", "sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.proxy_ndp=1", bridgeName)).Run()

		// Configure gateway reachability on bridge (critical for OVH)
		// VMs use the OVH gateway directly, so we need to make it reachable from the bridge
		if ipv6Gateway != "" {
			// Get parent interface MAC address
			macOutput, err := exec.Command("ip", "link", "show", parentIface).CombinedOutput()
			if err == nil {
				lines := strings.Split(string(macOutput), "\n")
				for _, line := range lines {
					if strings.Contains(line, "link/ether") {
						fields := strings.Fields(line)
						if len(fields) >= 2 {
							physMAC := fields[1]
							// Add gateway as permanent neighbor on bridge pointing to host's physical MAC
							cmd = exec.Command("sudo", "ip", "-6", "neigh", "replace", ipv6Gateway, "lladdr", physMAC, "dev", bridgeName, "nud", "permanent")
							if output, err := cmd.CombinedOutput(); err != nil {
								fmt.Printf("⚠️  Warning: Failed to add gateway neighbor on bridge: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
							} else {
								fmt.Printf("✅ Added gateway neighbor %s → %s on %s\n", ipv6Gateway, physMAC, bridgeName)
							}
							break
						}
					}
				}
			}

			// Add NDP proxy entry for gateway on bridge
			cmd = exec.Command("sudo", "ip", "-6", "neigh", "add", "proxy", ipv6Gateway, "dev", bridgeName)
			if output, err := cmd.CombinedOutput(); err != nil {
				if !strings.Contains(string(output), "File exists") {
					fmt.Printf("⚠️  Warning: Failed to add gateway proxy on bridge: %v (output: %s)\n", err, strings.TrimSpace(string(output)))
				}
			}
		}

		// Configure ndppd
		if err := ensureNdppdConfig(bridgeName, ipv6Gateway); err != nil {
			fmt.Printf("⚠️  Warning: Failed to configure ndppd: %v\n", err)
		}
	}

	fmt.Printf("✅ IPv6-only network configured for VM %s: IPv6=%s, Bridge=%s, TAP=%s\n",
		microvmID, ipv6Addr, bridgeName, tapName)

	return &models.NetworkInfo{
		IPAddress:        "0.0.0.0", // No public IPv4, will use NAT
		IPv6Address:      ipv6Addr,
		IPv6Gateway:      ipv6Gateway,
		IPv6PrefixLength: ipv6Prefix,
		MACAddress:       macAddr,
		AssignmentMethod: models.NetworkModeIPv6Only,
		SSHPort:          22,
		TAPDevice:        tapName,
		BridgeName:       bridgeName,
		BandwidthIngress: ingress,
		BandwidthEgress:  egress,
		IPv4NAT:          true, // This VM uses IPv4 NAT for IPv4-only sites
	}, nil
}

// configureOVNNetwork configures OVN networking: the backend creates the
// logical port, the DHCP reservation and port security, and the TAP is
// attached to br-int with the iface-id that binds it to that port.
func (nm *NetworkManager) configureOVNNetwork(ctx context.Context, microvmID, networkID string) (*models.NetworkInfo, error) {
	// Generate MAC address
	macAddr := nm.generateMACAddress(microvmID)

	// Create TAP interface
	tapName := fmt.Sprintf("fc-tap-%s", truncateVMID(microvmID, 8))
	if err := nm.createTAPInterface(tapName); err != nil {
		return nil, fmt.Errorf("failed to create TAP interface: %w", err)
	}

	// Connect to the OVS integration bridge with the logical port binding.
	if err := nm.addTAPToOVSBridge(ctx, tapName, ovsIntegrationBridge, ovnPortNameFor(microvmID)); err != nil {
		return nil, fmt.Errorf("failed to add TAP to OVS bridge: %w", err)
	}

	ipAddr := ""
	if nm.ovnAllocate != nil {
		// The backend owns the logical topology: it allocates and pins the
		// address, binds the subnet's DHCP_Options and sets port security.
		info, err := nm.ovnAllocate(microvmID, macAddr, networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate OVN network: %w", err)
		}
		if info != nil {
			if info.IPAddress != "" {
				ipAddr = info.IPAddress
			}
			if info.MACAddress != "" {
				macAddr = info.MACAddress
			}
		}
	} else {
		// No backend wired (e.g. tests): keep a placeholder address so the VM
		// still boots, and say so loudly.
		fmt.Printf("⚠️  OVN backend not wired; VM %s gets a placeholder OVN address\n", microvmID)
		addr, err := nm.assignOVNIP(microvmID)
		if err != nil {
			return nil, fmt.Errorf("failed to assign OVN IP: %w", err)
		}
		ipAddr = addr
	}

	return &models.NetworkInfo{
		IPAddress:        ipAddr,
		MACAddress:       macAddr,
		AssignmentMethod: models.NetworkModeOVN,
		SSHPort:          22, // Standard SSH port in OVN mode
	}, nil
}

// CleanupNetwork cleans up network resources for a microVM
func (nm *NetworkManager) CleanupNetwork(ctx context.Context, microvmID string, mode models.NetworkMode) error {
	switch mode {
	case models.NetworkModeNAT:
		tapName := fmt.Sprintf("fc-tap-%s", truncateVMID(microvmID, 8))
		macAddr := nm.generateMACAddress(microvmID)

		// Remove anti-spoofing rules
		_ = nm.removeAntiSpoofing(tapName, macAddr, "", "")

		// Remove DDoS protection rules
		_ = nm.removeDDoSProtection(tapName)

		// Remove port forwarding
		if err := nm.removePortForwarding(microvmID); err != nil {
			return fmt.Errorf("failed to remove port forwarding: %w", err)
		}
		// Remove TAP interface
		return nm.removeTAPInterface(tapName)

	case models.NetworkModeDHCP:
		tapName := dhcpTapNameFor(microvmID)
		macAddr := nm.generateMACAddress(microvmID)

		// Remove anti-spoofing rules (best effort)
		_ = nm.removeAntiSpoofing(tapName, macAddr, "", "")

		// Remove DDoS protection rules (best effort)
		_ = nm.removeDDoSProtection(tapName)

		// Release DHCP reservation if any
		nm.releaseDHCPIP(microvmID)

		// Remove TAP interface
		return nm.removeTAPInterface(tapName)

	case models.NetworkModeMACVLAN:
		// Direct bridge mode: tap, bridge, physical interface (no macvlan)
		tapName := fmt.Sprintf("tap-%s", truncateVMID(microvmID, 11))
		bridgeName := fmt.Sprintf("br-%s", truncateVMID(microvmID, 12))

		// Generate MAC address deterministically (for logging purposes)
		macAddr := nm.generateMACAddress(microvmID)
		fmt.Printf("🗑️  Cleaning up bridge network for %s (MAC: %s)\n", microvmID, macAddr)

		// Get parent interface
		parentIface := os.Getenv("MICROVM_PARENT_INTERFACE")
		if parentIface == "" {
			parentIface, _ = nm.getMainInterface()
			if parentIface == "" {
				parentIface = "eno1"
			}
		}

		// Clean up IPv6 routing (best effort - ignore errors)
		// List all IPv6 routes on the bridge and remove them
		if cmd := exec.Command("ip", "-6", "route", "show", "dev", bridgeName); cmd.Run() == nil {
			if output, err := cmd.Output(); err == nil && len(output) > 0 {
				// Parse routes and remove NDP proxy entries
				routes := strings.Split(strings.TrimSpace(string(output)), "\n")
				for _, route := range routes {
					if strings.Contains(route, "/128") {
						fields := strings.Fields(route)
						if len(fields) > 0 {
							ipv6Addr := strings.Split(fields[0], "/")[0]
							// Remove NDP proxy entry
							_ = exec.Command("sudo", "ip", "-6", "neigh", "del", "proxy", ipv6Addr, "dev", parentIface).Run()
							fmt.Printf("🗑️  Removed NDP proxy for %s\n", ipv6Addr)
						}
					}
				}
			}
		}

		// Clean up bandwidth limits (tc qdiscs) on bridge
		_ = exec.Command("sudo", "tc", "qdisc", "del", "dev", bridgeName, "root").Run()
		_ = exec.Command("sudo", "tc", "qdisc", "del", "dev", bridgeName, "ingress").Run()

		// Remove bridge (this will also remove attached TAP)
		_ = exec.Command("sudo", "ip", "link", "set", "dev", bridgeName, "down").Run()
		_ = exec.Command("sudo", "ip", "link", "delete", bridgeName).Run()

		// Remove TAP interface if still present
		_ = exec.Command("sudo", "ip", "link", "delete", tapName).Run()

		// Release public IP (placeholder)
		nm.releasePublicIP(microvmID)

		fmt.Printf("✅ Cleaned up bridge network for %s\n", microvmID)
		return nil

	case models.NetworkModeOVN:
		// TAP used for OVN follows fc-tap-<idprefix> pattern
		tapName := fmt.Sprintf("fc-tap-%s", truncateVMID(microvmID, 8))
		macAddr := nm.generateMACAddress(microvmID)

		// Remove anti-spoofing rules
		_ = nm.removeAntiSpoofing(tapName, macAddr, "", "")

		// Remove DDoS protection rules
		_ = nm.removeDDoSProtection(tapName)

		// Remove OVN logical port
		if err := nm.removeOVNLogicalPort(microvmID); err != nil {
			return fmt.Errorf("failed to remove OVN logical port: %w", err)
		}
		// Remove TAP from OVS bridge
		if err := nm.removeTAPFromOVSBridge(ctx, tapName, "br-int"); err != nil {
			return fmt.Errorf("failed to remove TAP from OVS bridge: %w", err)
		}
		// Remove TAP interface
		return nm.removeTAPInterface(tapName)

	default:
		return fmt.Errorf("unsupported network mode for cleanup: %s", mode)
	}
}

// Helper methods for network operations

func (nm *NetworkManager) generateMACAddress(microvmID string) string {
	// Generate a deterministic MAC address based on microVM ID
	hash := 0
	for _, c := range microvmID {
		hash = hash*31 + int(c)
	}

	// Use hash to generate last 3 octets (for a total of 6 octets)
	octet1 := (hash >> 16) & 0xFF
	octet2 := (hash >> 8) & 0xFF
	octet3 := hash & 0xFF

	return fmt.Sprintf("%s:%02X:%02X:%02X", nm.macPrefix, octet1, octet2, octet3)
}

func (nm *NetworkManager) createTAPInterface(tapName string) error {
	// Clean up any stale TAP interface that might still exist (e.g., after a reboot cycle)
	if err := nm.cleanupStaleTAPInterface(tapName); err != nil {
		return err
	}

	// Create TAP interface with sudo
	cmd := exec.Command("sudo", "ip", "tuntap", "add", "dev", tapName, "mode", "tap")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create TAP interface %s: %w (output: %s)", tapName, err, strings.TrimSpace(string(output)))
	}

	// Bring interface up with sudo
	cmd = exec.Command("sudo", "ip", "link", "set", "dev", tapName, "up")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to bring TAP interface %s up: %w (output: %s)", tapName, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// cleanupStaleTAPInterface removes an existing TAP device so creation can succeed deterministically
func (nm *NetworkManager) cleanupStaleTAPInterface(tapName string) error {
	// Fast path: interface is gone
	if err := exec.Command("ip", "link", "show", tapName).Run(); err != nil {
		return nil
	}

	fmt.Printf("♻️  TAP interface %s already exists; cleaning up stale interface before recreation\n", tapName)

	if output, err := exec.Command("sudo", "ip", "link", "set", "dev", tapName, "down").CombinedOutput(); err != nil {
		fmt.Printf("⚠️  Failed to bring TAP %s down before deletion: %v (output: %s)\n", tapName, err, strings.TrimSpace(string(output)))
	}

	if output, err := exec.Command("sudo", "ip", "link", "delete", tapName).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete existing TAP interface %s: %w (output: %s)", tapName, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// createTAPInterfaceWithBandwidth creates a TAP interface with bandwidth limits
func (nm *NetworkManager) createTAPInterfaceWithBandwidth(tapName, ingress, egress string) error {
	// Create TAP interface
	if err := nm.createTAPInterface(tapName); err != nil {
		return err
	}

	// Apply egress (outgoing from VM) bandwidth limit using tc
	if egress != "" {
		// Calculate appropriate burst based on rate for smooth traffic
		// burst should be at least rate/HZ and large enough for good throughput
		burst := "128kb" // default
		var rateNum int
		if _, err := fmt.Sscanf(egress, "%dmbit", &rateNum); err == nil {
			if rateNum <= 20 {
				burst = "32kb"
			} else if rateNum <= 50 {
				burst = "64kb"
			} else if rateNum <= 100 {
				burst = "128kb"
			} else if rateNum <= 200 {
				burst = "256kb"
			} else {
				burst = "384kb"
			}
		}
		cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapName, "root", "tbf",
			"rate", egress, "burst", burst, "latency", "50ms")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to set egress limit on %s: %w", tapName, err)
		}
	}

	// Apply ingress (incoming to VM) bandwidth limit using tc ingress with police action
	if ingress != "" {
		cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapName, "handle", "ffff:", "ingress")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to add ingress qdisc on %s: %w", tapName, err)
		}

		// Add police filter for ingress with adequate burst
		cmd = exec.Command("sudo", "tc", "filter", "add", "dev", tapName, "parent", "ffff:",
			"protocol", "all", "u32", "match", "u32", "0", "0",
			"police", "rate", ingress, "burst", "128kb", "mtu", "64kb", "drop", "flowid", ":1")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to set ingress limit on %s: %w", tapName, err)
		}
	}

	return nil
}

func (nm *NetworkManager) removeTAPInterface(tapName string) error {
	cmd := exec.Command("sudo", "ip", "link", "delete", tapName)
	return cmd.Run()
}

func (nm *NetworkManager) ensureNATBridge() error {
	// Check if bridge exists
	cmd := exec.Command("ip", "link", "show", nm.natBridge)
	if cmd.Run() == nil {
		return nil // Bridge already exists
	}

	// Create bridge with sudo
	cmd = exec.Command("sudo", "ip", "link", "add", "name", nm.natBridge, "type", "bridge")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Assign IP to bridge with sudo
	cmd = exec.Command("sudo", "ip", "addr", "add", "192.168.100.1/24", "dev", nm.natBridge)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to assign IP to bridge: %w", err)
	}

	// Bring bridge up with sudo
	cmd = exec.Command("sudo", "ip", "link", "set", "dev", nm.natBridge, "up")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to bring bridge up: %w", err)
	}

	// Configure NAT
	return nm.configureNAT()
}

func (nm *NetworkManager) addTAPToBridge(tapName, bridgeName string) error {
	cmd := exec.Command("sudo", "ip", "link", "set", "dev", tapName, "master", bridgeName)
	return cmd.Run()
}

// EnsureTAPReady ensures the TAP device exists and is attached to the bridge.
// This is called before starting a VM to handle the case where the TAP was deleted
// or not properly attached (e.g., after a host reboot or manual cleanup).
// For DHCP mode, it uses the TAP device name from the VM's network info.
func (nm *NetworkManager) EnsureTAPReady(tapName, bridgeName, bandwidthIngress, bandwidthEgress string) error {
	if tapName == "" {
		return fmt.Errorf("TAP device name is empty")
	}
	if bridgeName == "" {
		// Default to DHCP bridge from environment
		bridgeName = os.Getenv("MICROVM_NETWORK_BRIDGE")
		if bridgeName == "" {
			bridgeName = os.Getenv("MICROVM_DHCP_BRIDGE")
		}
		if bridgeName == "" {
			bridgeName = "lxdbr0"
		}
	}

	// Check if TAP exists
	tapExists := exec.Command("ip", "link", "show", tapName).Run() == nil

	if !tapExists {
		fmt.Printf("🔧 TAP device %s doesn't exist, creating it...\n", tapName)
		// Create TAP with bandwidth limits if specified
		if bandwidthIngress != "" || bandwidthEgress != "" {
			if err := nm.createTAPInterfaceWithBandwidth(tapName, bandwidthIngress, bandwidthEgress); err != nil {
				return fmt.Errorf("failed to create TAP interface with bandwidth: %w", err)
			}
		} else {
			if err := nm.createTAPInterface(tapName); err != nil {
				return fmt.Errorf("failed to create TAP interface: %w", err)
			}
		}
		fmt.Printf("✅ Created TAP device %s\n", tapName)
	} else {
		// TAP exists, ensure it's up
		cmd := exec.Command("sudo", "ip", "link", "set", "dev", tapName, "up")
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Warning: Failed to bring TAP %s up: %v (output: %s)\n", tapName, err, string(output))
		}
	}

	// Check if TAP is attached to the bridge
	checkCmd := exec.Command("ip", "link", "show", tapName)
	output, err := checkCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to check TAP status: %w", err)
	}

	// Check if "master <bridgeName>" is in the output
	if !strings.Contains(string(output), "master "+bridgeName) {
		fmt.Printf("🔧 TAP device %s not attached to bridge %s, attaching...\n", tapName, bridgeName)
		if err := nm.addTAPToBridge(tapName, bridgeName); err != nil {
			return fmt.Errorf("failed to add TAP to bridge %s: %w", bridgeName, err)
		}
		fmt.Printf("✅ Attached TAP %s to bridge %s\n", tapName, bridgeName)
	} else {
		fmt.Printf("✅ TAP device %s already attached to bridge %s\n", tapName, bridgeName)
	}

	return nil
}

// applyAntiSpoofing applies anti-spoofing rules to a TAP interface using ebtables
// This locks down the source MAC address and optionally source IP addresses (IPv4/IPv6)
// to prevent VMs from spoofing other addresses on the network
// ApplyAntiSpoofing is a public wrapper around applyAntiSpoofing for external access
func (nm *NetworkManager) ApplyAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr string) error {
	return nm.applyAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr)
}

func (nm *NetworkManager) applyAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr string) error {
	// Check if ebtables is available
	if err := exec.Command("which", "ebtables").Run(); err != nil {
		fmt.Printf("⚠️  ebtables not available, skipping anti-spoofing rules for %s\n", tapName)
		return nil // Non-fatal - continue without anti-spoofing
	}

	// Ensure idempotency: remove any existing anti-spoof rules for this TAP first.
	// This prevents duplicate rule accumulation across restarts/reinstalls/reconfigurations.
	if err := nm.removeAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr); err != nil {
		fmt.Printf("⚠️  Failed to pre-clean anti-spoofing rules for %s: %v\n", tapName, err)
	}

	fmt.Printf("🔒 Applying anti-spoofing rules to %s (MAC: %s, IPv4: %s, IPv6: %s)\n",
		tapName, macAddr, ipv4Addr, ipv6Addr)

	// Rule 1: Drop all packets with source MAC that doesn't match the assigned MAC
	// This prevents MAC spoofing
	cmd := exec.Command("sudo", "ebtables", "-A", "FORWARD", "-i", tapName,
		"-s", "!", macAddr, "-j", "DROP")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply MAC anti-spoof rule: %v\n", err)
	}

	// Rule 2: Drop IPv4 ARP packets with incorrect source MAC
	cmd = exec.Command("sudo", "ebtables", "-A", "FORWARD", "-i", tapName,
		"-p", "ARP", "--arp-mac-src", "!", macAddr, "-j", "DROP")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply ARP anti-spoof rule: %v\n", err)
	}

	// Rule 3: If IPv4 address is assigned (not 0.0.0.0), drop ARP with wrong IP
	if ipv4Addr != "" && ipv4Addr != "0.0.0.0" {
		cmd = exec.Command("sudo", "ebtables", "-A", "FORWARD", "-i", tapName,
			"-p", "ARP", "--arp-ip-src", "!", ipv4Addr, "-j", "DROP")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Failed to apply ARP IP anti-spoof rule: %v\n", err)
		}

		// Also use iptables to filter IPv4 packets with wrong source IP
		cmd = exec.Command("sudo", "iptables", "-I", "FORWARD", "-i", tapName,
			"!", "-s", ipv4Addr, "-j", "DROP")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Failed to apply IPv4 source filter: %v\n", err)
		}
	}

	// Rule 4: If IPv6 address is assigned, filter packets with wrong source IPv6
	if ipv6Addr != "" && ipv6Addr != "::" {
		cmd = exec.Command("sudo", "ip6tables", "-I", "FORWARD", "-i", tapName,
			"!", "-s", ipv6Addr, "-j", "DROP")
		if err := cmd.Run(); err != nil {
			fmt.Printf("⚠️  Failed to apply IPv6 source filter: %v\n", err)
		}
	}

	fmt.Printf("✅ Anti-spoofing rules applied to %s\n", tapName)
	return nil
}

// ApplyDDoSProtection is a public wrapper around applyDDoSProtection for external access
func (nm *NetworkManager) ApplyDDoSProtection(tapName string) error {
	return nm.applyDDoSProtection(tapName)
}

// applyDDoSProtection applies DDoS protection rules to prevent attack traffic from reaching the VM
// This blocks: SYN floods, INVALID packets, excessive ARP requests, and rate limits traffic
func (nm *NetworkManager) applyDDoSProtection(tapName string) error {
	fmt.Printf("🛡️  Applying DDoS protection to %s\n", tapName)

	// Check if iptables is available
	if err := exec.Command("which", "iptables").Run(); err != nil {
		fmt.Printf("⚠️  iptables not available, skipping DDoS protection for %s\n", tapName)
		return nil
	}

	// CRITICAL: Remove any existing DDoS rules for this TAP first to prevent duplicates
	// This handles the case where rules already exist from a previous run/restart
	nm.removeDDoSProtection(tapName)

	// Protection 1: Drop INVALID packets (malformed, out of sequence, etc.)
	cmd := exec.Command("sudo", "iptables", "-I", "FORWARD", "1", "-i", tapName,
		"-m", "state", "--state", "INVALID", "-j", "DROP")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply INVALID packet drop rule: %v\n", err)
	}

	// Protection 2: Limit SYN flood attacks (max 10 new connections per second)
	cmd = exec.Command("sudo", "iptables", "-I", "FORWARD", "2", "-i", tapName,
		"-p", "tcp", "--syn", "-m", "limit", "--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply SYN rate limit rule: %v\n", err)
	}

	cmd = exec.Command("sudo", "iptables", "-I", "FORWARD", "3", "-i", tapName,
		"-p", "tcp", "--syn", "-j", "DROP")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply SYN drop rule: %v\n", err)
	}

	// Protection 3: Rate limit ICMP (ping) to 1 per second
	cmd = exec.Command("sudo", "iptables", "-I", "FORWARD", "4", "-i", tapName,
		"-p", "icmp", "--icmp-type", "echo-request", "-m", "limit",
		"--limit", "1/sec", "--limit-burst", "2", "-j", "ACCEPT")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply ICMP rate limit rule: %v\n", err)
	}

	cmd = exec.Command("sudo", "iptables", "-I", "FORWARD", "5", "-i", tapName,
		"-p", "icmp", "--icmp-type", "echo-request", "-j", "DROP")
	if err := cmd.Run(); err != nil {
		fmt.Printf("⚠️  Failed to apply ICMP drop rule: %v\n", err)
	}

	// Protection 4: Rate limit ARP requests (prevents ARP scanning).
	// Strategy:
	//   1. Try ebtables with -m limit (works on ebtables-legacy backend).
	//   2. If -m limit is not supported (nf_tables backend, Ubuntu 22.04+),
	//      fall back to nft bridge-filter rules which work on all modern kernels.
	//   NEVER add a blanket ARP DROP without a paired rate-limit ACCEPT.
	arpProtected := false
	if exec.Command("which", "ebtables").Run() == nil {
		cmd = exec.Command("sudo", "ebtables", "-I", "FORWARD", "1", "-i", tapName,
			"-p", "ARP", "--arp-op", "Request", "-m", "limit",
			"--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT")
		if err := cmd.Run(); err == nil {
			cmd = exec.Command("sudo", "ebtables", "-I", "FORWARD", "2", "-i", tapName,
				"-p", "ARP", "--arp-op", "Request", "-j", "DROP")
			if err := cmd.Run(); err != nil {
				fmt.Printf("⚠️  Failed to apply ebtables ARP drop rule: %v\n", err)
			} else {
				arpProtected = true
				fmt.Printf("✅ ARP rate limiting applied via ebtables\n")
			}
		} else {
			fmt.Printf("⚠️  ebtables -m limit not supported (nf_tables backend), trying nft fallback\n")
		}
	}
	if !arpProtected {
		if err := nm.applyARPRateLimitNFT(tapName); err != nil {
			fmt.Printf("⚠️  ARP rate limiting skipped (nft fallback also failed): %v\n", err)
		} else {
			fmt.Printf("✅ ARP rate limiting applied via nft bridge\n")
		}
	}

	fmt.Printf("✅ DDoS protection applied to %s\n", tapName)
	return nil
}

// RemoveDDoSProtection is a public wrapper around removeDDoSProtection for external access
func (nm *NetworkManager) RemoveDDoSProtection(tapName string) error {
	return nm.removeDDoSProtection(tapName)
}

// removeDDoSProtection removes ALL DDoS protection rules for a TAP interface
// Loops to handle accumulated duplicate rules from previous bugs
func (nm *NetworkManager) removeDDoSProtection(tapName string) error {
	// Best effort cleanup - ignore errors as rules may not exist
	const maxCleanupIterations = 500 // Safety limit to prevent infinite loops

	// Define all iptables DDoS rules to clean up
	iptablesRules := [][]string{
		{"iptables", "-D", "FORWARD", "-i", tapName, "-m", "state", "--state", "INVALID", "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-i", tapName, "-p", "tcp", "--syn", "-m", "limit", "--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", tapName, "-p", "tcp", "--syn", "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-i", tapName, "-p", "icmp", "--icmp-type", "echo-request", "-m", "limit", "--limit", "1/sec", "--limit-burst", "2", "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", tapName, "-p", "icmp", "--icmp-type", "echo-request", "-j", "DROP"},
	}

	// Remove ALL instances of each iptables rule (handles duplicates)
	for _, rule := range iptablesRules {
		for i := 0; i < maxCleanupIterations; i++ {
			if err := exec.Command("sudo", rule...).Run(); err != nil {
				break // No more instances of this rule
			}
		}
	}

	// Remove ALL instances of ebtables DDoS rules.
	// Handle both `--arp-op` and `--arp-opcode` variants since different ebtables backends
	// normalize/print and sometimes only accept one spelling.
	ebtablesRules := [][]string{
		{"ebtables", "-D", "FORWARD", "-i", tapName, "-p", "ARP", "--arp-op", "Request", "-m", "limit", "--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT"},
		{"ebtables", "-D", "FORWARD", "-i", tapName, "-p", "ARP", "--arp-op", "Request", "-j", "DROP"},
		{"ebtables", "-D", "FORWARD", "-i", tapName, "-p", "ARP", "--arp-opcode", "Request", "-m", "limit", "--limit", "10/sec", "--limit-burst", "20", "-j", "ACCEPT"},
		{"ebtables", "-D", "FORWARD", "-i", tapName, "-p", "ARP", "--arp-opcode", "Request", "-j", "DROP"},
	}

	for _, rule := range ebtablesRules {
		for i := 0; i < maxCleanupIterations; i++ {
			if err := exec.Command("sudo", rule...).Run(); err != nil {
				break // No more instances of this rule
			}
		}
	}

	// Remove nft ARP rate-limit rules for this TAP (used as fallback on nf_tables backend)
	nm.removeARPRateLimitNFT(tapName)

	return nil
}

// applyARPRateLimitNFT applies ARP rate-limit rules via nft bridge filtering.
// Used as a fallback when ebtables '-m limit' is not supported (nf_tables backend).
func (nm *NetworkManager) applyARPRateLimitNFT(tapName string) error {
	if exec.Command("which", "nft").Run() != nil {
		return fmt.Errorf("nft not available")
	}
	// Build the ruleset as a single nft -f - script to avoid shell quoting issues.
	// The table/chain are created idempotently; existing rules with the same TAP are
	// removed first to prevent duplicates.
	nm.removeARPRateLimitNFT(tapName)
	ruleset := fmt.Sprintf(
		"add table bridge vmlet_arp_limit\n"+
			"add chain bridge vmlet_arp_limit forward { type filter hook forward priority 0; policy accept; }\n"+
			"add rule bridge vmlet_arp_limit forward iifname \"%s\" ether type arp limit rate 10/second burst 20 packets accept\n"+
			"add rule bridge vmlet_arp_limit forward iifname \"%s\" ether type arp drop\n",
		tapName, tapName,
	)
	cmd := exec.Command("sudo", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft ruleset failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeARPRateLimitNFT removes nft ARP rate-limit rules for a specific TAP.
// Finds rules by handle in the vmlet_arp_limit forward chain.
func (nm *NetworkManager) removeARPRateLimitNFT(tapName string) {
	out, err := exec.Command("sudo", "nft", "-a", "list", "chain", "bridge", "vmlet_arp_limit", "forward").Output()
	if err != nil {
		return // Table/chain doesn't exist yet
	}
	quoted := `"` + tapName + `"`
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, quoted) {
			continue
		}
		// Extract handle number from trailing "# handle N"
		if idx := strings.LastIndex(line, "# handle "); idx >= 0 {
			handle := strings.TrimSpace(line[idx+9:])
			exec.Command("sudo", "nft", "delete", "rule", "bridge", "vmlet_arp_limit", "forward", "handle", handle).Run()
		}
	}
}

// removeAntiSpoofing removes anti-spoofing rules for a TAP interface
// Loops to remove ALL duplicate rules that may have accumulated from restarts or changeIP calls
func (nm *NetworkManager) removeAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr string) error {
	// Best effort cleanup - ignore errors as rules may not exist
	const maxIter = 100 // Safety limit to prevent infinite loops

	// 1) Remove anti-spoof ebtables rules for this TAP by rule pattern, not exact MAC/IP.
	// This handles stale rules when IP/MAC changed between runs.
	if err := exec.Command("which", "ebtables").Run(); err == nil {
		listOutput, listErr := exec.Command("sudo", "ebtables", "-L", "FORWARD", "--Lx", "--Ln").CombinedOutput()
		if listErr != nil {
			listOutput, _ = exec.Command("sudo", "ebtables", "-L", "FORWARD", "--Ln").CombinedOutput()
		}

		if len(listOutput) > 0 {
			for _, line := range strings.Split(string(listOutput), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || !strings.HasPrefix(line, "-") || !strings.Contains(line, tapName) {
					continue
				}

				isAntiSpoof := (strings.Contains(line, "-s !") && strings.Contains(line, "-j DROP")) ||
					(strings.Contains(line, "--arp-mac-src !") && strings.Contains(line, "-j DROP")) ||
					(strings.Contains(line, "--arp-ip-src !") && strings.Contains(line, "-j DROP"))

				if !isAntiSpoof {
					continue
				}

				args := strings.Fields(line)
				if len(args) == 0 {
					continue
				}
				_ = exec.Command("sudo", append([]string{"ebtables", "-D", "FORWARD"}, args...)...).Run()
			}
		}
	}

	// 2) Remove legacy exact-match ebtables rules (best effort)
	for i := 0; i < maxIter; i++ {
		if exec.Command("sudo", "ebtables", "-D", "FORWARD", "-i", tapName,
			"-s", "!", macAddr, "-j", "DROP").Run() != nil {
			break
		}
	}
	for i := 0; i < maxIter; i++ {
		if exec.Command("sudo", "ebtables", "-D", "FORWARD", "-i", tapName,
			"-p", "ARP", "--arp-mac-src", "!", macAddr, "-j", "DROP").Run() != nil {
			break
		}
	}

	if ipv4Addr != "" && ipv4Addr != "0.0.0.0" {
		for i := 0; i < maxIter; i++ {
			if exec.Command("sudo", "ebtables", "-D", "FORWARD", "-i", tapName,
				"-p", "ARP", "--arp-ip-src", "!", ipv4Addr, "-j", "DROP").Run() != nil {
				break
			}
		}
	}

	// 3) Remove all iptables anti-spoof FORWARD rules for this TAP (dynamic IP-safe)
	for pass := 0; pass < maxIter; pass++ {
		output, err := exec.Command("sudo", "iptables", "-S", "FORWARD").CombinedOutput()
		if err != nil {
			break
		}

		removed := false
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "-A FORWARD") {
				continue
			}
			if !strings.Contains(line, "-i "+tapName) || !strings.Contains(line, "! -s") || !strings.Contains(line, "-j DROP") {
				continue
			}

			del := strings.Replace(line, "-A FORWARD", "-D FORWARD", 1)
			args := strings.Fields(del)
			if len(args) == 0 {
				continue
			}
			if err := exec.Command("sudo", append([]string{"iptables"}, args...)...).Run(); err == nil {
				removed = true
			}
		}

		if !removed {
			break
		}
	}

	// 4) Remove all ip6tables anti-spoof FORWARD rules for this TAP
	for pass := 0; pass < maxIter; pass++ {
		output, err := exec.Command("sudo", "ip6tables", "-S", "FORWARD").CombinedOutput()
		if err != nil {
			break
		}

		removed := false
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "-A FORWARD") {
				continue
			}
			if !strings.Contains(line, "-i "+tapName) || !strings.Contains(line, "! -s") || !strings.Contains(line, "-j DROP") {
				continue
			}

			del := strings.Replace(line, "-A FORWARD", "-D FORWARD", 1)
			args := strings.Fields(del)
			if len(args) == 0 {
				continue
			}
			if err := exec.Command("sudo", append([]string{"ip6tables"}, args...)...).Run(); err == nil {
				removed = true
			}
		}

		if !removed {
			break
		}
	}

	fmt.Printf("🧹 Removed all anti-spoofing rules for TAP %s (MAC: %s, IPv4: %s, IPv6: %s)\n",
		tapName, macAddr, ipv4Addr, ipv6Addr)
	return nil
}

func (nm *NetworkManager) assignNATIP(microvmID string) (string, error) {
	// Simple IP assignment based on microVM ID hash
	hash := 0
	for _, c := range microvmID {
		hash = hash*31 + int(c)
	}

	// Assign IP in range 192.168.100.10-192.168.100.254
	ipSuffix := (hash % 245) + 10
	return fmt.Sprintf("192.168.100.%d", ipSuffix), nil
}

func (nm *NetworkManager) getNextSSHPort() uint32 {
	nm.portCounter++
	return nm.portCounter
}

func (nm *NetworkManager) configurePortForwarding(hostPort uint32, guestIP string, guestPort uint32) error {
	// Configure iptables rule for port forwarding
	rule := fmt.Sprintf("PREROUTING -t nat -p tcp --dport %d -j DNAT --to-destination %s:%d",
		hostPort, guestIP, guestPort)

	args := append([]string{"-A"}, strings.Fields(rule)...)
	cmd := exec.Command("iptables", args...)
	return cmd.Run()
}

func (nm *NetworkManager) removePortForwarding(microvmID string) error {
	// This is a simplified cleanup - in production, you'd track the specific rules
	return nil
}

func (nm *NetworkManager) configureNAT() error {
	// Enable IP forwarding with sudo
	cmd := exec.Command("sudo", "sysctl", "-w", "net.ipv4.ip_forward=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Configure masquerading with sudo
	cmd = exec.Command("sudo", "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "192.168.100.0/24", "-j", "MASQUERADE")
	return cmd.Run()
}

func (nm *NetworkManager) reserveDHCPIP(macAddr string) (string, error) {
	// Check if MAC has a fixed-address reservation in dhcpd.conf
	if ip := nm.getFixedAddressFromDHCPDConf(macAddr); ip != "" {
		return ip, nil
	}
	// Otherwise return placeholder; IP will be discovered from leases file after VM boots
	return "0.0.0.0", nil
}

// getFixedAddressFromDHCPDConf parses dhcpd.conf to find fixed-address for a MAC
func (nm *NetworkManager) getFixedAddressFromDHCPDConf(macAddr string) string {
	confPaths := []string{
		"/etc/dhcp/dhcpd.conf",
		"/etc/dhcpd.conf",
		os.Getenv("MICROVM_DHCPD_CONF"),
	}
	macLower := strings.ToLower(strings.ReplaceAll(macAddr, ":", ""))

	for _, confPath := range confPaths {
		if confPath == "" {
			continue
		}
		data, err := os.ReadFile(confPath)
		if err != nil {
			continue
		}
		text := string(data)
		// Parse host { ... } blocks
		// Look for: host <name> { hardware ethernet <mac>; fixed-address <ip>; }
		lines := strings.Split(text, "\n")
		var inBlock, foundMAC bool
		var currentIP string
		for _, line := range lines {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "host ") && strings.Contains(l, "{") {
				inBlock = true
				foundMAC = false
				currentIP = ""
				continue
			}
			if inBlock {
				// Check for MAC line: hardware ethernet 02:fc:77:00:00:0a;
				if strings.Contains(l, "hardware ethernet") {
					parts := strings.Fields(l)
					for _, p := range parts {
						cleanMAC := strings.ToLower(strings.ReplaceAll(strings.Trim(p, ";"), ":", ""))
						if cleanMAC == macLower {
							foundMAC = true
							break
						}
					}
				}
				// Check for fixed-address line: fixed-address 192.168.100.19;
				if strings.Contains(l, "fixed-address") {
					parts := strings.Fields(l)
					if len(parts) >= 2 {
						currentIP = strings.Trim(parts[1], ";")
					}
				}
				if strings.HasPrefix(l, "}") {
					if foundMAC && currentIP != "" {
						return currentIP
					}
					inBlock = false
					foundMAC = false
					currentIP = ""
				}
			}
		}
	}
	return ""
}

// GetDHCPLeaseIP queries the DHCP server's lease file to get the assigned IP for a MAC address
func (nm *NetworkManager) GetDHCPLeaseIP(macAddr, bridgeName string) (string, error) {
	// Try common DHCP lease file locations
	leaseFiles := []string{
		fmt.Sprintf("/var/snap/lxd/common/lxd/networks/%s/dnsmasq.leases", bridgeName),
		fmt.Sprintf("/var/lib/misc/dnsmasq.%s.leases", bridgeName),
		"/var/lib/misc/dnsmasq.leases",
		"/var/lib/dhcp/dhcpd.leases",  // ISC DHCP (global)
		"/var/lib/dhcp/dhcpd.leases~", // ISC DHCP (backup)
	}

	macLower := strings.ToLower(macAddr)

	for _, leaseFile := range leaseFiles {
		data, err := os.ReadFile(leaseFile)
		if err != nil {
			continue // Try next file
		}

		// First, try dnsmasq format: timestamp mac ip hostname client-id
		// Example: 1762846517 02:fc:76:a8:4f:ca 10.130.146.124 * *
		text := string(data)
		if strings.Contains(text, "dnsmasq") || strings.Count(text, "\n{") == 0 {
			lines := strings.Split(text, "\n")
			for _, line := range lines {
				if strings.Contains(strings.ToLower(line), macLower) {
					fields := strings.Fields(line)
					if len(fields) >= 3 {
						return fields[2], nil // Return IP address
					}
				}
			}
		}

		// Next, try ISC dhcpd format: lease <ip> { ... hardware ethernet <mac>; binding state active; }
		// We'll scan blocks heuristically
		if strings.Contains(text, "lease ") {
			lines := strings.Split(text, "\n")
			var inBlock bool
			var currentIP string
			for _, line := range lines {
				l := strings.TrimSpace(line)
				if strings.HasPrefix(l, "lease ") && strings.HasSuffix(l, "{") {
					// start of block
					parts := strings.Fields(l)
					if len(parts) >= 2 {
						currentIP = parts[1]
						inBlock = true
					}
					continue
				}
				if inBlock {
					// check for MAC line
					// patterns: hardware ethernet 02:fc:...;
					if strings.Contains(strings.ToLower(l), "hardware ethernet") && strings.Contains(strings.ToLower(l), macLower) {
						// optional: prefer active bindings only
						// we could look ahead for 'binding state active;' but many files keep last state at end
						return currentIP, nil
					}
					if strings.HasPrefix(l, "}") {
						// end of block
						inBlock = false
						currentIP = ""
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no DHCP lease found for MAC %s", macAddr)
}

func (nm *NetworkManager) releaseDHCPIP(microvmID string) error {
	// Release DHCP reservation
	return nil
}

func (nm *NetworkManager) getMainInterface() (string, error) {
	// Get the main network interface
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			// Check if interface has an IP address
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			if len(addrs) > 0 {
				return iface.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no suitable main interface found")
}

func (nm *NetworkManager) createMACVLANInterface(macvlanName, parentIface, macAddr string) error {
	cmd := exec.Command("ip", "link", "add", "link", parentIface, "name", macvlanName,
		"type", "macvlan", "mode", "bridge")
	if err := cmd.Run(); err != nil {
		return err
	}

	// Set MAC address
	cmd = exec.Command("ip", "link", "set", "dev", macvlanName, "address", macAddr)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Bring interface up
	cmd = exec.Command("ip", "link", "set", "dev", macvlanName, "up")
	return cmd.Run()
}

func (nm *NetworkManager) removeMACVLANInterface(macvlanName string) error {
	// Bring down interface first
	_ = exec.Command("sudo", "ip", "link", "set", "dev", macvlanName, "down").Run()

	// Delete macvlan interface
	cmd := exec.Command("sudo", "ip", "link", "delete", macvlanName)
	return cmd.Run()
}

// ensureBridgeWithParent ensures a Linux bridge exists and contains the parent interface.
// Idempotent: skips operations if already configured.
func ensureBridgeWithParent(bridgeName, parentIface string) error {
	// Check if bridge exists
	if exec.Command("ip", "link", "show", bridgeName).Run() != nil {
		if output, err := exec.Command("sudo", "ip", "link", "add", "name", bridgeName, "type", "bridge").CombinedOutput(); err != nil {
			return fmt.Errorf("create bridge %s failed: %w (output: %s)", bridgeName, err, strings.TrimSpace(string(output)))
		}
		if output, err := exec.Command("sudo", "ip", "link", "set", bridgeName, "up").CombinedOutput(); err != nil {
			return fmt.Errorf("bring bridge %s up failed: %w (output: %s)", bridgeName, err, strings.TrimSpace(string(output)))
		}
	}
	// Check if parent already enslaved
	// 'bridge link' lists ports; simpler: parse 'ip link show master <bridge>'
	out, _ := exec.Command("bash", "-c", fmt.Sprintf("ip -o link show master %s | awk -F': ' '{print $2}'", bridgeName)).CombinedOutput()
	if !strings.Contains(string(out), parentIface) {
		if output, err := exec.Command("sudo", "ip", "link", "set", parentIface, "master", bridgeName).CombinedOutput(); err != nil {
			return fmt.Errorf("failed to attach parent iface %s to bridge %s: %w (output: %s)", parentIface, bridgeName, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (nm *NetworkManager) assignPublicIP(microvmID string) (string, error) {
	// This would integrate with OVH API or similar to assign a public IP
	// For now, return placeholder
	return "203.0.113.100", nil
}

func (nm *NetworkManager) releasePublicIP(microvmID string) error {
	// Release public IP allocation
	return nil
}

// ovsIntegrationBridge is the bridge OVN's ovn-controller manages.
const ovsIntegrationBridge = "br-int"

// ovnPortNameFor derives the OVN logical switch port name for a VM. OVN's
// ovn-controller binds it to the TAP through the interface's
// external_ids:iface-id, so the two names must match.
//
// The name must be identical to the one internal/ovn writes into the
// Northbound DB, which is why it is NOT truncated: the TAP name is what has to
// fit in the 15-character IFNAMSIZ limit, and that truncation happens
// separately. Truncating the port name too made the iface-id disagree with the
// logical port, and ovn-controller silently never bound them.
func ovnPortNameFor(microvmID string) string {
	sanitised := microvmID
	var b strings.Builder
	for _, r := range microvmID {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if valid {
			b.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		sanitised = s
	}
	return fmt.Sprintf("%s-%s", ovnPortPrefix, sanitised)
}

// ovnPortPrefix is the prefix shared with internal/ovn's LogicalPortName.
const ovnPortPrefix = "fc-port"

// ovsClient is built lazily so the OVS bridge is only required in OVN mode.
func (nm *NetworkManager) ovsClient() (*ovs.Client, error) {
	return ovs.NewClient()
}

// addTAPToOVSBridge attaches a TAP to the OVS integration bridge and tags it
// with the iface-ID OVN's ovn-controller uses to find its logical port.
// Idempotent.
func (nm *NetworkManager) addTAPToOVSBridge(ctx context.Context, tapName, bridgeName, ifaceID string) error {
	client, err := nm.ovsClient()
	if err != nil {
		return err
	}
	if err := client.EnsureBridge(ctx, bridgeName); err != nil {
		return err
	}
	return client.AttachPort(ctx, bridgeName, tapName, ifaceID)
}

func (nm *NetworkManager) removeTAPFromOVSBridge(ctx context.Context, tapName, bridgeName string) error {
	client, err := nm.ovsClient()
	if err != nil {
		return err
	}
	return client.DetachPort(ctx, bridgeName, tapName)
}

func (nm *NetworkManager) configureOVNLogicalPort(microvmID, macAddr string) error {
	// The logical switch port is created by internal/ovn's reconcile loop from
	// vmlet's VM state; this call only guarantees the OVS side exists.
	// The iface-id is set by addTAPToOVSBridge.
	return nil
}

func (nm *NetworkManager) removeOVNLogicalPort(microvmID string) error {
	// Logical topology removal is handled by the internal/ovn reconcile loop.
	return nil
}

func (nm *NetworkManager) assignOVNIP(microvmID string) (string, error) {
	// Assign IP from OVN subnet
	// This would typically be handled by OVN DHCP
	return "10.0.0.100", nil
}

// generateIPv6FromMAC generates an IPv6 address from MAC address using SLAAC (EUI-64)
// This follows OVH's standard IPv6 assignment method
// Returns: (ipv6Address, gateway, prefixLength) or ("", "", 0) if IPv6 is not configured
func (nm *NetworkManager) generateIPv6FromMAC(macAddr, parentIface string) (string, string, int) {
	var ipv6Prefix string
	var prefixLen int

	// First, check if IPv6 subnet is explicitly configured (for multi-node deployments)
	configuredSubnet := os.Getenv("MICROVM_IPV6_SUBNET")

	if configuredSubnet != "" {
		// Parse configured subnet (e.g., "2001:41d0:203:67c1::/56")
		parts := strings.Split(configuredSubnet, "/")
		if len(parts) == 2 {
			ipv6Prefix = strings.TrimSuffix(parts[0], "::")
			fmt.Sscanf(parts[1], "%d", &prefixLen)
			fmt.Printf("🌐 Using configured IPv6 subnet: %s/%d\n", ipv6Prefix, prefixLen)
		}
	}

	// If not configured, auto-detect from parent interface
	if ipv6Prefix == "" {
		cmd := exec.Command("ip", "-6", "addr", "show", "dev", parentIface, "scope", "global")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", "", 0 // IPv6 not configured
		}

		// Parse IPv6 address and prefix
		// Example output: "inet6 2001:41d0:203:67c1::/56 scope global"
		lines := strings.Split(string(output), "\n")

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "inet6") && strings.Contains(line, "scope global") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					// Parse address/prefix (e.g., "2001:41d0:203:67c1::/56")
					addrPrefix := fields[1]
					parts := strings.Split(addrPrefix, "/")
					if len(parts) == 2 {
						ipv6Prefix = strings.TrimSuffix(parts[0], "::")
						fmt.Sscanf(parts[1], "%d", &prefixLen)
						fmt.Printf("🌐 Auto-detected IPv6 subnet from %s: %s/%d\n", parentIface, ipv6Prefix, prefixLen)
						break
					}
				}
			}
		}

		if ipv6Prefix == "" {
			return "", "", 0 // No global IPv6 found
		}
	}

	// Parse the MAC address to generate EUI-64 interface identifier
	// Format: XX:XX:XX:XX:XX:XX -> remove colons
	macHex := strings.ReplaceAll(strings.ToLower(macAddr), ":", "")
	if len(macHex) != 12 {
		fmt.Printf("⚠️  Invalid MAC address format: %s\n", macAddr)
		return "", "", 0
	}

	// Convert MAC to EUI-64 format:
	// 1. Take first 3 bytes (6 hex chars)
	// 2. Insert FF:FE in the middle
	// 3. Take last 3 bytes (6 hex chars)
	// 4. Flip the 7th bit of the first byte (universal/local bit)

	// Parse first byte
	firstByteVal := uint8(0)
	fmt.Sscanf(macHex[0:2], "%02x", &firstByteVal)
	firstByteVal ^= 0x02 // Flip the universal/local bit

	// Build EUI-64 interface identifier (64 bits = 16 hex chars)
	// For IPv6, we need 4 groups of 4 hex digits separated by colons
	// MAC: aa:bb:cc:dd:ee:ff -> EUI-64: aabb:ccff:fedd:eeff (with bit flip on first byte)
	// EUI-64 is: byte0^02 + byte1 : byte2 + FF : FE + byte3 : byte4 + byte5
	interfaceID := fmt.Sprintf("%02x%s:%sff:fe%s:%s%s",
		firstByteVal,  // first byte with bit flipped (becomes XX in group XXXX)
		macHex[2:4],   // second byte (completes first group)
		macHex[4:6],   // third byte (start of second group)
		macHex[6:8],   // fourth byte (end of third group)
		macHex[8:10],  // fifth byte (start of fourth group)
		macHex[10:12]) // sixth byte (end of fourth group)

	// Remove trailing :: from prefix if present
	ipv6Prefix = strings.TrimSuffix(ipv6Prefix, "::")

	// Combine prefix with interface ID
	// For /56, the EUI-64 (64 bits) directly follows the prefix (56 bits rounded to 64 as group boundary)
	// So we need 8 groups total: 4 from prefix + 4 from EUI-64
	// For subnet 0: prefix:eui64 (e.g., 2001:41d0:203:67c1:00fc:76ff:fe6e:dc39)
	var vmIPv6 string

	if prefixLen <= 64 {
		// For /56 or /64, directly join prefix with EUI-64
		// This gives us 8 groups total
		vmIPv6 = fmt.Sprintf("%s:%s", ipv6Prefix, interfaceID)
	} else {
		// Unsupported prefix length
		fmt.Printf("⚠️  Unsupported IPv6 prefix length: /%d\n", prefixLen)
		return "", "", 0
	}

	// Gateway is typically ::1 on the same subnet
	// Check if gateway is explicitly configured first
	gateway := os.Getenv("MICROVM_IPV6_GATEWAY")
	if gateway == "" {
		// Auto-generate gateway based on prefix
		gateway = fmt.Sprintf("%s::1", ipv6Prefix)
		if prefixLen <= 56 {
			gateway = fmt.Sprintf("%s:0:0::1", ipv6Prefix)
		}
	}

	return vmIPv6, gateway, 64 // Always use /64 for VM assignments
}

// ensureNdppdConfig configures ndppd to enable bidirectional IPv6 connectivity
// This is critical for OVH-style IPv6 setups where the gateway is not in the same /64 subnet
// We need to proxy:
// 1. VM IPs on the physical interface (so external traffic can reach VMs)
// 2. Gateway IP on the bridge (so VMs can discover gateway's MAC via NDP)
func ensureNdppdConfig(bridgeName, ipv6Gateway string) error {
	const ndppdConfigPath = "/etc/ndppd.conf"

	if ipv6Gateway == "" {
		return fmt.Errorf("ipv6Gateway is required")
	}

	// Read existing config if it exists
	existingConfig := ""
	if data, err := os.ReadFile(ndppdConfigPath); err == nil {
		existingConfig = string(data)
	}

	// Check if bridge proxy already exists
	bridgeProxyExists := strings.Contains(existingConfig, fmt.Sprintf("proxy %s", bridgeName))

	if !bridgeProxyExists {
		// Append bridge proxy configuration
		bridgeProxy := fmt.Sprintf(`
# Proxy gateway on bridge so VMs can discover gateway MAC via NDP
proxy %s {
    rule %s/128 {
        static
    }
}
`, bridgeName, ipv6Gateway)

		// If config doesn't exist, create it with physical interface proxy too
		if existingConfig == "" {
			// Detect physical interface
			parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
			if parentIface == "" {
				parentIface = "eno1" // Default for OVH
			}

			// Get IPv6 block from environment
			ipv6Block := os.Getenv("MICROVM_IPV6_BLOCK")
			if ipv6Block == "" {
				fmt.Printf("⚠️  MICROVM_IPV6_BLOCK not set, skipping ndppd physical interface config\n")
			} else {
				existingConfig = fmt.Sprintf(`# Auto-generated by Vmlet
# Proxy VM IPs on physical interface (outbound - internet→VM)
proxy %s {
    rule %s {
        auto
    }
}
`, parentIface, ipv6Block)
			}
		}

		newConfig := existingConfig + bridgeProxy

		// Write config
		if err := os.WriteFile(ndppdConfigPath, []byte(newConfig), 0644); err != nil {
			return fmt.Errorf("failed to write ndppd config: %w", err)
		}

		fmt.Printf("✅ Added ndppd proxy for gateway %s on %s\n", ipv6Gateway, bridgeName)

		// Reload ndppd
		if err := exec.Command("sudo", "systemctl", "restart", "ndppd").Run(); err != nil {
			return fmt.Errorf("failed to restart ndppd: %w", err)
		}

		fmt.Printf("✅ Restarted ndppd service\n")
	}

	return nil
}
