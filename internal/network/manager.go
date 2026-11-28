package network

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// NetworkManager orchestrates all network infrastructure components. It is the
// default "bridge" implementation of Backend.
type NetworkManager struct {
	bridgeManager *BridgeManager
	dhcpManager   *DHCPManager
	sshManager    *SSHManager
	config        *NetworkConfig
	mu            sync.RWMutex
}

var _ Backend = (*NetworkManager)(nil)

// NetworkConfig represents the overall network configuration
type NetworkConfig struct {
	// Backend selects the network backend. Empty means MICROVM_NETWORK_BACKEND,
	// falling back to "bridge" when the variable is unset.
	Backend       string
	BridgeName    string
	BridgeIP      string
	BridgeCIDR    string
	HostInterface string
	// OverlayIP and OverlayCIDR are the OVN overlay subnet (the VPC's default
	// network). They fall back to BridgeIP/BridgeCIDR when empty so a
	// single-backend install is unchanged, but a host running both backends
	// should set them: the overlay subnet and the NAT bridge subnet must not
	// overlap, or two VMs can end up with the same address on different
	// datapaths. Env: MICROVM_OVN_NETWORK_IP / MICROVM_OVN_NETWORK_CIDR.
	OverlayIP    string
	OverlayCIDR  string
	DHCPRange    DHCPRange
	DNSServers   []string
	SSHPortRange PortRange
	ConfigDir    string
	// When true, do not start an internal DHCP server (use existing one like lxdbr0)
	DisableDHCP bool
	// When false, NAT iptables rules won't be managed (use existing host NAT)
	ManageNAT bool
}

// VMNetworkInfo represents network information for a VM
type VMNetworkInfo struct {
	VMID       string
	MACAddress string
	IPAddress  string
	SSHPort    int
	SSHKeyPair *SSHKeyPair
	Connection *SSHConnection
}

// NewNetworkManager creates a new network manager
func NewNetworkManager(config *NetworkConfig) *NetworkManager {
	// Create bridge manager
	bridgeManager := NewBridgeManager(BridgeConfig{
		Name:          config.BridgeName,
		IPAddress:     config.BridgeIP,
		CIDR:          config.BridgeCIDR,
		HostInterface: config.HostInterface,
		ManageNAT:     config.ManageNAT,
	})

	// Create DHCP manager
	dhcpManager := NewDHCPManager(DHCPConfig{
		ConfigDir:  config.ConfigDir,
		BridgeName: config.BridgeName,
		IPRange:    config.DHCPRange,
		Gateway:    config.BridgeIP,
		DNSServers: config.DNSServers,
		LeaseTime:  "12h",
	})

	// Create SSH manager
	sshManager := NewSSHManager(SSHConfig{
		KeyDir:    fmt.Sprintf("%s/ssh-keys", config.ConfigDir),
		PortRange: config.SSHPortRange,
	})

	return &NetworkManager{
		bridgeManager: bridgeManager,
		dhcpManager:   dhcpManager,
		sshManager:    sshManager,
		config:        config,
	}
}

// Initialize sets up the network infrastructure
func (nm *NetworkManager) Initialize() error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Create and configure bridge
	if err := nm.bridgeManager.CreateBridge(); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Start DHCP server if not disabled
	if !nm.config.DisableDHCP {
		if err := nm.dhcpManager.Start(); err != nil {
			return fmt.Errorf("failed to start DHCP server: %w", err)
		}
	}

	return nil
}

// Shutdown cleans up network infrastructure
func (nm *NetworkManager) Shutdown() error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	var errors []error

	// Stop DHCP server
	if err := nm.dhcpManager.Stop(); err != nil {
		errors = append(errors, fmt.Errorf("failed to stop DHCP server: %w", err))
	}

	// Delete bridge (this will also clean up iptables rules)
	if err := nm.bridgeManager.DeleteBridge(); err != nil {
		errors = append(errors, fmt.Errorf("failed to delete bridge: %w", err))
	}

	if len(errors) > 0 {
		return fmt.Errorf("shutdown errors: %v", errors)
	}

	return nil
}

// AllocateVMNetwork allocates network resources for a VM
func (nm *NetworkManager) AllocateVMNetwork(vmID, macAddress, networkID string) (*VMNetworkInfo, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Generate SSH key pair
	keyPair, err := nm.sshManager.GenerateKeyPair(vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate SSH key pair: %w", err)
	}

	// If DHCP is disabled (using external DHCP like lxdbr0), skip internal IP allocation
	if nm.config.DisableDHCP {
		// In external DHCP mode, we cannot set up port forwarding without knowing the guest IP.
		// Return minimal network info with generated keys and MAC; IP/SSH port will be discovered later.
		return &VMNetworkInfo{
			VMID:       vmID,
			MACAddress: macAddress,
			IPAddress:  "",
			SSHPort:    0,
			SSHKeyPair: keyPair,
			Connection: nil,
		}, nil
	}

	// Allocate IP address via internal DHCP
	ipAddress, err := nm.dhcpManager.AllocateIP(macAddress)
	if err != nil {
		// Clean up key pair on failure
		nm.sshManager.RemoveKeyPair(vmID)
		return nil, fmt.Errorf("failed to allocate IP address: %w", err)
	}

	// Allocate SSH port
	sshPort, err := nm.sshManager.AllocatePort(vmID)
	if err != nil {
		// Clean up on failure
		nm.dhcpManager.ReleaseIP(macAddress)
		nm.sshManager.RemoveKeyPair(vmID)
		return nil, fmt.Errorf("failed to allocate SSH port: %w", err)
	}

	// Set up port forwarding
	if err := nm.sshManager.SetupPortForwarding(vmID, ipAddress, sshPort, 22); err != nil {
		// Clean up on failure
		nm.sshManager.ReleasePort(vmID)
		nm.dhcpManager.ReleaseIP(macAddress)
		nm.sshManager.RemoveKeyPair(vmID)
		return nil, fmt.Errorf("failed to setup port forwarding: %w", err)
	}

	// Get SSH connection info
	connection, err := nm.sshManager.GetSSHConnection(vmID, ipAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH connection info: %w", err)
	}

	return &VMNetworkInfo{
		VMID:       vmID,
		MACAddress: macAddress,
		IPAddress:  ipAddress,
		SSHPort:    sshPort,
		SSHKeyPair: keyPair,
		Connection: connection,
	}, nil
}

// ReleaseVMNetwork releases network resources for a VM
func (nm *NetworkManager) ReleaseVMNetwork(vmID, macAddress string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	var errors []error

	// Release SSH port and remove port forwarding (only if we allocated one)
	if !nm.config.DisableDHCP {
		if err := nm.sshManager.ReleasePort(vmID); err != nil {
			errors = append(errors, fmt.Errorf("failed to release SSH port: %w", err))
		}
		// Release IP address (internal DHCP only)
		if err := nm.dhcpManager.ReleaseIP(macAddress); err != nil {
			errors = append(errors, fmt.Errorf("failed to release IP address: %w", err))
		}
	}

	// Remove SSH key pair
	if err := nm.sshManager.RemoveKeyPair(vmID); err != nil {
		errors = append(errors, fmt.Errorf("failed to remove SSH key pair: %w", err))
	}

	if len(errors) > 0 {
		return fmt.Errorf("release errors: %v", errors)
	}

	return nil
}

// ValidateNetworkSetup validates that the network infrastructure is properly configured
func (nm *NetworkManager) ValidateNetworkSetup() error {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	// Validate bridge
	if err := nm.bridgeManager.ValidateBridge(); err != nil {
		return fmt.Errorf("bridge validation failed: %w", err)
	}

	// Check DHCP server only when internal DHCP is enabled
	if !nm.config.DisableDHCP {
		if !nm.dhcpManager.IsRunning() {
			return fmt.Errorf("DHCP server is not running")
		}
	}

	return nil
}

// GetNetworkStatus returns the current status of network components
func (nm *NetworkManager) GetNetworkStatus() (*NetworkStatus, error) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	// Get bridge info
	bridgeInfo, err := nm.bridgeManager.GetBridgeInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge info: %w", err)
	}

	// Get DHCP leases (only if internal DHCP is enabled)
	var leases []DHCPLease
	if !nm.config.DisableDHCP {
		l, err := nm.dhcpManager.GetLeases()
		if err != nil {
			return nil, fmt.Errorf("failed to get DHCP leases: %w", err)
		}
		leases = l
	}

	return &NetworkStatus{
		Bridge: bridgeInfo,
		DHCPRunning: func() bool {
			if nm.config.DisableDHCP {
				return false
			}
			return nm.dhcpManager.IsRunning()
		}(),
		DHCPLeases: leases,
	}, nil
}

// TestVMConnectivity tests SSH connectivity to a VM
func (nm *NetworkManager) TestVMConnectivity(vmID string) error {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	return nm.sshManager.TestSSHConnection(vmID, 30*time.Second)
}

// GetVMNetworkInfo returns network information for a VM
func (nm *NetworkManager) GetVMNetworkInfo(vmID string) (*VMNetworkInfo, error) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	// If internal DHCP is disabled, detailed info may not be available yet
	if nm.config.DisableDHCP {
		return &VMNetworkInfo{
			VMID:       vmID,
			MACAddress: "",
			IPAddress:  "",
			SSHPort:    0,
			SSHKeyPair: nil,
			Connection: nil,
		}, nil
	}

	// Get SSH connection info
	connection, err := nm.sshManager.GetSSHConnection(vmID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH connection info: %w", err)
	}

	// Find VM's DHCP lease to get IP and MAC
	leases, err := nm.dhcpManager.GetLeases()
	if err != nil {
		return nil, fmt.Errorf("failed to get DHCP leases: %w", err)
	}

	// Look for lease that might belong to this VM
	// This is a simplified approach - in practice you'd want better VM->lease mapping
	var vmLease *DHCPLease
	for _, lease := range leases {
		if strings.Contains(lease.Hostname, vmID) {
			vmLease = &lease
			break
		}
	}

	if vmLease == nil {
		return nil, fmt.Errorf("no network lease found for VM %s", vmID)
	}

	return &VMNetworkInfo{
		VMID:       vmID,
		MACAddress: vmLease.MAC,
		IPAddress:  vmLease.IP,
		SSHPort:    connection.Port,
		Connection: connection,
	}, nil
}

// NetworkStatus represents the current status of network infrastructure
type NetworkStatus struct {
	Bridge      *BridgeInfo
	DHCPRunning bool
	DHCPLeases  []DHCPLease
}
