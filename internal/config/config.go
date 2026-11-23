package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config represents the main configuration for the MicroVM service
type Config struct {
	Server      ServerConfig      `json:"server"`
	Firecracker FirecrackerConfig `json:"firecracker"`
	Networking  NetworkingConfig  `json:"networking"`
	Storage     StorageConfig     `json:"storage"`
	Resources   ResourceConfig    `json:"resources"`
	Pricing     PricingConfig     `json:"pricing"`
	Billing     BillingConfig     `json:"billing"`
	LXD         LXDConfig         `json:"lxd"`
	Development DevelopmentConfig `json:"development"`
}

// ServerConfig defines server-specific configuration
type ServerConfig struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	GRPCPort    int    `json:"grpc_port"`
	TLSEnabled  bool   `json:"tls_enabled"`
	CertFile    string `json:"cert_file,omitempty"`
	KeyFile     string `json:"key_file,omitempty"`
	LogLevel    string `json:"log_level"`
	MetricsPort int    `json:"metrics_port"`
}

// FirecrackerConfig defines firecracker-specific configuration
type FirecrackerConfig struct {
	BinaryPath        string `json:"binary_path"`
	KernelImagePath   string `json:"kernel_image_path"`
	RootDrivePath     string `json:"root_drive_path"`
	ContainerdSocket  string `json:"containerd_socket"`
	JailerPath        string `json:"jailer_path,omitempty"`
	ChrootBaseDir     string `json:"chroot_base_dir,omitempty"`
	EnableJailer      bool   `json:"enable_jailer"`
	DefaultKernelArgs string `json:"default_kernel_args"`
}

// NetworkingConfig defines networking configuration for different modes
type NetworkingConfig struct {
	DefaultMode   NetworkMode          `json:"default_mode"`
	DHCPConfig    DHCPNetworkConfig    `json:"dhcp_config"`
	MACVLANConfig MACVLANNetworkConfig `json:"macvlan_config"`
	NATConfig     NATNetworkConfig     `json:"nat_config"`
	OVNConfig     OVNNetworkConfig     `json:"ovn_config"`
}

// NetworkMode represents different network configuration modes
type NetworkMode string

const (
	NetworkModeDHCP    NetworkMode = "dhcp"
	NetworkModeMACVLAN NetworkMode = "macvlan"
	NetworkModeNAT     NetworkMode = "nat"
	NetworkModeOVN     NetworkMode = "ovn"
)

// DHCPNetworkConfig defines DHCP networking configuration
type DHCPNetworkConfig struct {
	Enabled       bool     `json:"enabled"`
	BridgeName    string   `json:"bridge_name"`
	DHCPServerIP  string   `json:"dhcp_server_ip"`
	DHCPRange     string   `json:"dhcp_range"`
	DNSServers    []string `json:"dns_servers"`
	MACPrefix     string   `json:"mac_prefix"`
	ReservationDB string   `json:"reservation_db,omitempty"`
}

// MACVLANNetworkConfig defines MACVLAN networking configuration
type MACVLANNetworkConfig struct {
	Enabled         bool     `json:"enabled"`
	ParentInterface string   `json:"parent_interface"`
	IPRange         string   `json:"ip_range"`
	Gateway         string   `json:"gateway"`
	DNSServers      []string `json:"dns_servers"`
	MACPrefix       string   `json:"mac_prefix"`
	VLANTag         int      `json:"vlan_tag,omitempty"`
}

// NATNetworkConfig defines NAT networking configuration (for local development)
type NATNetworkConfig struct {
	Enabled         bool     `json:"enabled"`
	BridgeName      string   `json:"bridge_name"`
	Subnet          string   `json:"subnet"`
	Gateway         string   `json:"gateway"`
	DNSServers      []string `json:"dns_servers"`
	DHCPEnabled     bool     `json:"dhcp_enabled"`
	PortForwardBase int      `json:"port_forward_base"`
	MACPrefix       string   `json:"mac_prefix"`
}

// OVNNetworkConfig defines OVN (Open Virtual Network) configuration
type OVNNetworkConfig struct {
	Enabled          bool     `json:"enabled"`
	OVSBridge        string   `json:"ovs_bridge"`
	LogicalSwitch    string   `json:"logical_switch"`
	LogicalRouter    string   `json:"logical_router"`
	OVNNorthDBSocket string   `json:"ovn_north_db_socket"`
	OVNSouthDBSocket string   `json:"ovn_south_db_socket"`
	SubnetCIDR       string   `json:"subnet_cidr"`
	GatewayIP        string   `json:"gateway_ip"`
	DNSServers       []string `json:"dns_servers"`
}

// StorageConfig defines storage configuration options
type StorageConfig struct {
	DefaultType  string        `json:"default_type"`
	LocalConfig  LocalStorage  `json:"local_config"`
	S3Config     S3Storage     `json:"s3_config"`
	HybridConfig HybridStorage `json:"hybrid_config"`
}

// LocalStorage defines local storage configuration
type LocalStorage struct {
	BasePath   string `json:"base_path"`
	MaxSizeGB  int    `json:"max_size_gb"`
	FileSystem string `json:"file_system"`
}

// S3Storage defines S3 storage configuration
type S3Storage struct {
	DefaultBucket  string `json:"default_bucket"`
	DefaultRegion  string `json:"default_region"`
	AccessKey      string `json:"access_key,omitempty"`
	SecretKey      string `json:"secret_key,omitempty"`
	UseIAMRole     bool   `json:"use_iam_role"`
	S3FSMountPoint string `json:"s3fs_mount_point"`
	NBDServerPort  int    `json:"nbd_server_port"`
}

// HybridStorage defines hybrid storage configuration
type HybridStorage struct {
	LocalCachePath  string `json:"local_cache_path"`
	CacheSizeGB     int    `json:"cache_size_gb"`
	SyncIntervalMin int    `json:"sync_interval_min"`
	S3Bucket        string `json:"s3_bucket"`
	S3Region        string `json:"s3_region"`
}

// ResourceConfig defines resource management configuration
type ResourceConfig struct {
	BufferPercentage       float64 `json:"buffer_percentage"`
	MaxVCPUsPerMicroVM     int     `json:"max_vcpus_per_microvm"`
	MaxMemoryMBPerMicroVM  int     `json:"max_memory_mb_per_microvm"`
	MaxStorageMBPerMicroVM int     `json:"max_storage_mb_per_microvm"`
	MonitoringIntervalSec  int     `json:"monitoring_interval_sec"`
	OverloadThreshold      float64 `json:"overload_threshold"`
}

// PricingConfig defines pricing engine configuration
type PricingConfig struct {
	BaseCPURate        float64 `json:"base_cpu_rate"`
	BaseMemoryRate     float64 `json:"base_memory_rate"`
	BaseStorageRate    float64 `json:"base_storage_rate"`
	ProfitMargin       float64 `json:"profit_margin"`
	UpdateIntervalMin  int     `json:"update_interval_min"`
	HighTierDiscount   float64 `json:"high_tier_discount"`
	MediumTierDiscount float64 `json:"medium_tier_discount"`
	LowTierPremium     float64 `json:"low_tier_premium"`
	Currency           string  `json:"currency"`
}

// BillingConfig defines billing service configuration
type BillingConfig struct {
	// Enabled determines if billing is enabled globally (default: true)
	Enabled bool `json:"enabled"`
	// CheckpointIntervalMinutes is the interval for automatic checkpoints (default: 5)
	CheckpointIntervalMinutes int `json:"checkpoint_interval_minutes"`
	// DefaultCurrency is the default currency for billing (default: "IRR" for Tomans)
	DefaultCurrency string `json:"default_currency"`
	// BillWhilePaused determines if VMs are billed while paused (default: true)
	BillWhilePaused bool `json:"bill_while_paused"`
	// BillWhileStopped determines if VMs are billed while stopped (default: true)
	BillWhileStopped bool `json:"bill_while_stopped"`
	// SaveHistoricalCheckpoints determines if checkpoint history is saved (default: false)
	SaveHistoricalCheckpoints bool `json:"save_historical_checkpoints"`
}

// LXDConfig defines LXD integration configuration
type LXDConfig struct {
	Enabled      bool   `json:"enabled"`
	Socket       string `json:"socket"`
	ServerURL    string `json:"server_url,omitempty"`
	ClientCert   string `json:"client_cert,omitempty"`
	ClientKey    string `json:"client_key,omitempty"`
	ServerCert   string `json:"server_cert,omitempty"`
	ProjectName  string `json:"project_name"`
	PollInterval int    `json:"poll_interval_sec"`
}

// DevelopmentConfig defines development-specific configuration
type DevelopmentConfig struct {
	SimulateLXD        bool   `json:"simulate_lxd"`
	MockResourceUsage  bool   `json:"mock_resource_usage"`
	EnableDebugLogging bool   `json:"enable_debug_logging"`
	DisableAuth        bool   `json:"disable_auth"`
	TestDataPath       string `json:"test_data_path,omitempty"`
}

// DefaultConfig returns a default configuration suitable for local development
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:        "localhost",
			Port:        8080,
			GRPCPort:    9090,
			TLSEnabled:  false,
			LogLevel:    "info",
			MetricsPort: 8081,
		},
		Firecracker: FirecrackerConfig{
			BinaryPath:        "/usr/local/bin/firecracker",
			KernelImagePath:   "/var/lib/firecracker-containerd/runtime/default-vmlinux.bin",
			RootDrivePath:     "/var/lib/firecracker-containerd/runtime/default-rootfs.img",
			ContainerdSocket:  "/run/firecracker-containerd/containerd.sock",
			EnableJailer:      false,
			DefaultKernelArgs: "console=ttyS0 noapic reboot=k panic=1 pci=off nomodules rw",
		},
		Networking: NetworkingConfig{
			DefaultMode: NetworkModeNAT,
			DHCPConfig: DHCPNetworkConfig{
				Enabled:      false,
				BridgeName:   "fc-dhcp-br0",
				DHCPServerIP: "192.168.200.1",
				DHCPRange:    "192.168.200.10-192.168.200.100",
				DNSServers:   []string{"8.8.8.8", "8.8.4.4"},
				MACPrefix:    "AA:FC:00",
			},
			MACVLANConfig: MACVLANNetworkConfig{
				Enabled:         false,
				ParentInterface: "eth0",
				IPRange:         "192.168.1.0/24",
				Gateway:         "192.168.1.1",
				DNSServers:      []string{"8.8.8.8", "8.8.4.4"},
				MACPrefix:       "AA:FC:01",
			},
			NATConfig: NATNetworkConfig{
				Enabled:         true,
				BridgeName:      "fc-nat-br0",
				Subnet:          "192.168.100.0/24",
				Gateway:         "192.168.100.1",
				DNSServers:      []string{"8.8.8.8", "8.8.4.4"},
				DHCPEnabled:     true,
				PortForwardBase: 2200,
				MACPrefix:       "AA:FC:02",
			},
			OVNConfig: OVNNetworkConfig{
				Enabled:          false,
				OVSBridge:        "br-int",
				LogicalSwitch:    "fc-ls",
				LogicalRouter:    "fc-lr",
				OVNNorthDBSocket: "unix:/var/run/ovn/ovnnb_db.sock",
				OVNSouthDBSocket: "unix:/var/run/ovn/ovnsb_db.sock",
				SubnetCIDR:       "192.168.150.0/24",
				GatewayIP:        "192.168.150.1",
				DNSServers:       []string{"8.8.8.8", "8.8.4.4"},
			},
		},
		Storage: StorageConfig{
			DefaultType: "local",
			LocalConfig: LocalStorage{
				BasePath:   "/var/lib/firecracker-containerd/storage",
				MaxSizeGB:  100,
				FileSystem: "ext4",
			},
			S3Config: S3Storage{
				DefaultBucket:  "microvm-storage",
				DefaultRegion:  "us-east-1",
				UseIAMRole:     true,
				S3FSMountPoint: "/mnt/s3fs",
				NBDServerPort:  10809,
			},
			HybridConfig: HybridStorage{
				LocalCachePath:  "/var/cache/microvm-storage",
				CacheSizeGB:     20,
				SyncIntervalMin: 15,
			},
		},
		Resources: ResourceConfig{
			BufferPercentage:       0.20,
			MaxVCPUsPerMicroVM:     16,
			MaxMemoryMBPerMicroVM:  65536,
			MaxStorageMBPerMicroVM: maxStorageMBPerMicroVMFromEnv(102400),
			MonitoringIntervalSec:  30,
			OverloadThreshold:      0.80,
		},
		Pricing: PricingConfig{
			BaseCPURate:        0.05,
			BaseMemoryRate:     0.0001,
			BaseStorageRate:    0.00005,
			ProfitMargin:       0.30,
			UpdateIntervalMin:  5,
			HighTierDiscount:   0.40,
			MediumTierDiscount: 0.20,
			LowTierPremium:     0.25,
			Currency:           "USD",
		},
		Billing: BillingConfig{
			Enabled:                   true,
			CheckpointIntervalMinutes: 5,
			DefaultCurrency:           "IRR",
			BillWhilePaused:           true,
			BillWhileStopped:          true,
			SaveHistoricalCheckpoints: false,
		},
		LXD: LXDConfig{
			Enabled:      false, // Disabled for local development
			Socket:       "/var/lib/lxd/unix.socket",
			ProjectName:  "default",
			PollInterval: 30,
		},
		Development: DevelopmentConfig{
			SimulateLXD:        true,
			MockResourceUsage:  true,
			EnableDebugLogging: true,
			DisableAuth:        true,
		},
	}
}

// LoadConfig loads configuration from a file
// maxStorageMBPerMicroVMFromEnv returns the per-VM storage ceiling: the given
// default, or MICROVM_MAX_STORAGE_MB when set to a sane value (>= 1GB). This
// lets a node serve plans larger than the historical 100GB default without a
// config file — the production nodes run on defaults + env only.
func maxStorageMBPerMicroVMFromEnv(def int) int {
	if v := strings.TrimSpace(os.Getenv("MICROVM_MAX_STORAGE_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1024 {
			return n
		}
	}
	return def
}

func LoadConfig(configPath string) (*Config, error) {
	// If no config path provided, use default config
	if configPath == "" {
		return DefaultConfig(), nil
	}

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s", configPath)
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	// Parse JSON
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %v", err)
	}

	return &config, nil
}

// SaveConfig saves configuration to a file
func (c *Config) SaveConfig(configPath string) error {
	// Create directory if it doesn't exist
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	// Write to file
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate server configuration
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server port: %d", c.Server.Port)
	}

	if c.Server.GRPCPort <= 0 || c.Server.GRPCPort > 65535 {
		return fmt.Errorf("invalid gRPC port: %d", c.Server.GRPCPort)
	}

	// Validate firecracker configuration
	if c.Firecracker.BinaryPath == "" {
		return fmt.Errorf("firecracker binary path is required")
	}

	if c.Firecracker.KernelImagePath == "" {
		return fmt.Errorf("kernel image path is required")
	}

	if c.Firecracker.RootDrivePath == "" {
		return fmt.Errorf("root drive path is required")
	}

	// Validate networking configuration
	if err := c.validateNetworkingConfig(); err != nil {
		return fmt.Errorf("networking configuration error: %v", err)
	}

	// Validate resource configuration
	if c.Resources.BufferPercentage < 0 || c.Resources.BufferPercentage > 1 {
		return fmt.Errorf("buffer percentage must be between 0 and 1")
	}

	if c.Resources.OverloadThreshold < 0 || c.Resources.OverloadThreshold > 1 {
		return fmt.Errorf("overload threshold must be between 0 and 1")
	}

	return nil
}

// validateNetworkingConfig validates networking configuration
func (c *Config) validateNetworkingConfig() error {
	// Validate default mode
	switch c.Networking.DefaultMode {
	case NetworkModeDHCP, NetworkModeMACVLAN, NetworkModeNAT, NetworkModeOVN:
		// Valid modes
	default:
		return fmt.Errorf("invalid default network mode: %s", c.Networking.DefaultMode)
	}

	// Validate NAT configuration if enabled
	if c.Networking.NATConfig.Enabled {
		if c.Networking.NATConfig.BridgeName == "" {
			return fmt.Errorf("NAT bridge name is required when NAT is enabled")
		}
		if c.Networking.NATConfig.Subnet == "" {
			return fmt.Errorf("NAT subnet is required when NAT is enabled")
		}
	}

	// Validate MACVLAN configuration if enabled
	if c.Networking.MACVLANConfig.Enabled {
		if c.Networking.MACVLANConfig.ParentInterface == "" {
			return fmt.Errorf("MACVLAN parent interface is required when MACVLAN is enabled")
		}
	}

	return nil
}

// GetNetworkConfig returns the network configuration for the specified mode
func (c *Config) GetNetworkConfig(mode NetworkMode) interface{} {
	switch mode {
	case NetworkModeDHCP:
		return c.Networking.DHCPConfig
	case NetworkModeMACVLAN:
		return c.Networking.MACVLANConfig
	case NetworkModeNAT:
		return c.Networking.NATConfig
	case NetworkModeOVN:
		return c.Networking.OVNConfig
	default:
		return nil
	}
}

// IsNetworkModeEnabled checks if a specific network mode is enabled
func (c *Config) IsNetworkModeEnabled(mode NetworkMode) bool {
	switch mode {
	case NetworkModeDHCP:
		return c.Networking.DHCPConfig.Enabled
	case NetworkModeMACVLAN:
		return c.Networking.MACVLANConfig.Enabled
	case NetworkModeNAT:
		return c.Networking.NATConfig.Enabled
	case NetworkModeOVN:
		return c.Networking.OVNConfig.Enabled
	default:
		return false
	}
}
