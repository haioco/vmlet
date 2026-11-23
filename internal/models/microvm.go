package models

import (
	"time"
)

// MicroVMStatus represents the current state of a microVM
type MicroVMStatus string

const (
	MicroVMStatusCreating MicroVMStatus = "creating"
	MicroVMStatusRunning  MicroVMStatus = "running"
	MicroVMStatusStopping MicroVMStatus = "stopping"
	MicroVMStatusStopped  MicroVMStatus = "stopped"
	MicroVMStatusError    MicroVMStatus = "error"
)

// StorageType represents different storage backend options
type StorageType string

const (
	StorageTypeLocal  StorageType = "local"
	StorageTypeS3Fuse StorageType = "s3-fuse"
	StorageTypeS3NBD  StorageType = "s3-nbd"
	StorageTypeHybrid StorageType = "hybrid"
)

// NetworkMode represents different network configuration options
type NetworkMode string

const (
	NetworkModeDHCP     NetworkMode = "dhcp"
	NetworkModeMACVLAN  NetworkMode = "macvlan"
	NetworkModeNAT      NetworkMode = "nat"
	NetworkModeOVN      NetworkMode = "ovn"
	NetworkModeIPv6Only NetworkMode = "ipv6-only" // IPv6 public + IPv4 private (NAT for IPv4-only sites)
)

// ResourceSpecs defines the resource requirements for a microVM
type ResourceSpecs struct {
	VCPUs     uint32 `json:"vcpus" validate:"required,min=1,max=16"`
	MemoryMB  uint32 `json:"memory_mb" validate:"required,min=128,max=65536"`
	StorageMB uint32 `json:"storage_mb" validate:"required,min=1024,max=512000"`
}

// StorageConfig defines storage configuration for a microVM
type StorageConfig struct {
	Type     StorageType `json:"type" validate:"required"`
	S3Config *S3Config   `json:"s3_config,omitempty"`
}

// S3Config defines S3 storage configuration
type S3Config struct {
	Bucket    string `json:"bucket" validate:"required"`
	Region    string `json:"region" validate:"required"`
	AccessKey string `json:"access_key" validate:"required"`
	SecretKey string `json:"secret_key" validate:"required"`
}

// NetworkConfig defines network configuration for a microVM
type NetworkConfig struct {
	Mode     NetworkMode `json:"mode" validate:"required"`
	CustomIP string      `json:"custom_ip,omitempty" validate:"omitempty,ip"`
}

// NetworkInfo contains network details for a running microVM
type NetworkInfo struct {
	IPAddress        string      `json:"ip_address"`
	IPv6Address      string      `json:"ipv6_address,omitempty"`
	IPv6Gateway      string      `json:"ipv6_gateway,omitempty"`
	IPv6PrefixLength int         `json:"ipv6_prefix_length,omitempty"`
	IPv4NAT          bool        `json:"ipv4_nat,omitempty"`         // Enable IPv4 NAT for IPv6-only VMs
	IPv4NATAddress   string      `json:"ipv4_nat_address,omitempty"` // Private IPv4 address for NAT
	MACAddress       string      `json:"mac_address"`
	AssignmentMethod NetworkMode `json:"assignment_method"`
	SSHPort          uint32      `json:"ssh_port"`
	PublicKey        string      `json:"public_key"`
	TAPDevice        string      `json:"tap_device,omitempty"`        // TAP interface name
	BridgeName       string      `json:"bridge_name,omitempty"`       // Bridge to attach to (for DHCP mode)
	BandwidthIngress string      `json:"bandwidth_ingress,omitempty"` // e.g., "80Mibit"
	BandwidthEgress  string      `json:"bandwidth_egress,omitempty"`  // e.g., "100Mibit"
	IPv4Addons       []IPv4Addon `json:"ipv4_addons,omitempty"`       // Additional public IPv4 addresses
}

// IPv4Addon represents an additional public IPv4 address assigned to a VM
type IPv4Addon struct {
	IPv4Address  string    `json:"ipv4_address"`            // Public IPv4 address (e.g., "54.38.119.252")
	MACAddress   string    `json:"mac_address"`             // OVH-assigned MAC for this IP
	InterfaceID  string    `json:"interface_id,omitempty"`  // Host interface name (e.g., "ipv4-vm-xxx-0")
	RoutingTable int       `json:"routing_table,omitempty"` // Policy routing table number
	Enabled      bool      `json:"enabled"`                 // Whether this addon is active
	CreatedAt    time.Time `json:"created_at"`
}

// PricingInfo contains pricing details for a microVM
type PricingInfo struct {
	HourlyRate float64 `json:"hourly_rate"`
	Currency   string  `json:"currency"`
}

// VsockConfig defines vsock (virtio-socket) configuration for VM-host communication
type VsockConfig struct {
	Enabled  bool   `json:"enabled"`   // Whether vsock is enabled for this VM
	GuestCID uint32 `json:"guest_cid"` // Guest CID (must be >= 3, 0-2 are reserved)
	UDSPath  string `json:"uds_path"`  // Unix domain socket path (relative to VM dir)
}

// MicroVMMetadata contains metadata for a microVM
type MicroVMMetadata struct {
	CreatedAt    time.Time         `json:"created_at"`
	LastActivity time.Time         `json:"last_activity"`
	CustomerID   string            `json:"customer_id"`
	AutoStart    bool              `json:"auto_start"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// ProcessInfo contains process-related information for a microVM
type ProcessInfo struct {
	PID        int       `json:"pid"`
	StartTime  time.Time `json:"start_time"`
	ConfigPath string    `json:"config_path"`
	SocketPath string    `json:"socket_path"`
}

// AutoscaleConfig defines autoscaling configuration for a microVM
type AutoscaleConfig struct {
	Enabled            bool      `json:"enabled"`
	MinMemoryMB        uint32    `json:"min_memory_mb"`        // Minimum memory to scale down to
	MaxMemoryMB        uint32    `json:"max_memory_mb"`        // Maximum memory to scale up to
	ScaleUpThreshold   float64   `json:"scale_up_threshold"`   // Scale up when usage > this % (e.g., 0.80 = 80%)
	ScaleDownThreshold float64   `json:"scale_down_threshold"` // Scale down when usage < this % (e.g., 0.30 = 30%)
	CooldownSeconds    int       `json:"cooldown_seconds"`     // Cooldown period between scaling actions
	LastScaleAction    time.Time `json:"last_scale_action"`    // Last time scaling was performed
}

// MicroVMInstance represents a complete microVM instance
type MicroVMInstance struct {
	ID                  string           `json:"id"`
	NodeID              string           `json:"node_id"`
	Status              MicroVMStatus    `json:"status"`
	Resources           ResourceSpecs    `json:"resources"`
	Storage             StorageConfig    `json:"storage"`
	Network             NetworkInfo      `json:"network"`
	Pricing             PricingInfo      `json:"pricing"`
	Billing             *BillingInfo     `json:"billing,omitempty"` // Per-VM billing configuration
	Metadata            MicroVMMetadata  `json:"metadata"`
	ProcessInfo         *ProcessInfo     `json:"process_info,omitempty"`
	TrafficIngressBytes uint64           `json:"traffic_ingress_bytes"` // Total bytes received since last reset
	TrafficEgressBytes  uint64           `json:"traffic_egress_bytes"`  // Total bytes transmitted since last reset
	Autoscale           *AutoscaleConfig `json:"autoscale,omitempty"`   // Per-VM autoscaling configuration
	Vsock               *VsockConfig     `json:"vsock,omitempty"`       // Vsock configuration for host-guest communication
}

// Validate validates the MicroVMInstance fields
func (m *MicroVMInstance) Validate() error {
	// Basic validation logic would go here
	// For now, just check required fields
	if m.ID == "" {
		return ErrInvalidMicroVMID
	}
	if m.NodeID == "" {
		return ErrInvalidNodeID
	}
	if m.Metadata.CustomerID == "" {
		return ErrInvalidCustomerID
	}
	return nil
}

// IsRunning returns true if the microVM is in a running state
func (m *MicroVMInstance) IsRunning() bool {
	return m.Status == MicroVMStatusRunning
}

// CanBeStarted returns true if the microVM can be started
func (m *MicroVMInstance) CanBeStarted() bool {
	// Allow starting when explicitly stopped or when stuck in creating state
	// (e.g., creation completed partially and requires manual start).
	return m.Status == MicroVMStatusStopped || m.Status == MicroVMStatusCreating
}

// CanBeStopped returns true if the microVM can be stopped
func (m *MicroVMInstance) CanBeStopped() bool {
	return m.Status == MicroVMStatusRunning
}

// UpdateLastActivity updates the last activity timestamp
func (m *MicroVMInstance) UpdateLastActivity() {
	m.Metadata.LastActivity = time.Now()
}

// ShouldAutoStart returns true if the VM should be auto-started
func (m *MicroVMInstance) ShouldAutoStart() bool {
	return m.Metadata.AutoStart
}

// SetAutoStart sets the auto-start flag for the VM
func (m *MicroVMInstance) SetAutoStart(autoStart bool) {
	m.Metadata.AutoStart = autoStart
}

// UpdateProcessInfo updates the process information for the VM
func (m *MicroVMInstance) UpdateProcessInfo(pid int, configPath, socketPath string) {
	m.ProcessInfo = &ProcessInfo{
		PID:        pid,
		StartTime:  time.Now(),
		ConfigPath: configPath,
		SocketPath: socketPath,
	}
}

// ClearProcessInfo clears the process information when VM stops
func (m *MicroVMInstance) ClearProcessInfo() {
	m.ProcessInfo = nil
}
