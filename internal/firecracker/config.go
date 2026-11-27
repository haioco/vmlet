package firecracker

import (
	"fmt"
	"path/filepath"

	"github.com/haioco/vmlet/internal/models"
)

// ConfigGenerator generates firecracker configuration files
type ConfigGenerator struct {
	config *Config
}

// FirecrackerConfig represents the firecracker configuration structure
type FirecrackerConfig struct {
	BootSource        BootSource         `json:"boot-source"`
	Drives            []Drive            `json:"drives"`
	MachineConfig     MachineConfig      `json:"machine-config"`
	NetworkInterfaces []NetworkInterface `json:"network-interfaces,omitempty"`
	Balloon           *BalloonDevice     `json:"balloon,omitempty"`
	Logger            *Logger            `json:"logger,omitempty"`
	Metrics           *Metrics           `json:"metrics,omitempty"`
}

// BalloonDevice defines the memory balloon device configuration
type BalloonDevice struct {
	AmountMib             uint32 `json:"amount_mib"`
	DeflateOnOom          bool   `json:"deflate_on_oom"`
	StatsPollingIntervalS uint32 `json:"stats_polling_interval_s"`
}

// BootSource defines the kernel and boot parameters
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive defines a block device
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	Partuuid     string `json:"partuuid,omitempty"`
	CacheType    string `json:"cache_type,omitempty"`
}

// MachineConfig defines the virtual machine configuration
type MachineConfig struct {
	VcpuCount   uint32 `json:"vcpu_count"`
	MemSizeMib  uint32 `json:"mem_size_mib"`
	HtEnabled   bool   `json:"ht_enabled"`
	CpuTemplate string `json:"cpu_template,omitempty"`
}

// NetworkInterface defines a network interface
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMac    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// Logger defines logging configuration
type Logger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level"`
	ShowLevel     bool   `json:"show_level"`
	ShowLogOrigin bool   `json:"show_log_origin"`
}

// Metrics defines metrics configuration
type Metrics struct {
	MetricsPath string `json:"metrics_path"`
}

// NewConfigGenerator creates a new configuration generator
func NewConfigGenerator(config *Config) (*ConfigGenerator, error) {
	return &ConfigGenerator{
		config: config,
	}, nil
}

// GenerateConfig generates a firecracker configuration for a microVM
func (cg *ConfigGenerator) GenerateConfig(microvm *models.MicroVMInstance, storagePaths *StoragePaths, privateKey string) (*FirecrackerConfig, error) {
	config := &FirecrackerConfig{
		BootSource: BootSource{
			KernelImagePath: cg.getKernelPath(),
			BootArgs:        cg.generateBootArgs(microvm, privateKey),
		},
		MachineConfig: MachineConfig{
			VcpuCount:  microvm.Resources.VCPUs,
			MemSizeMib: microvm.Resources.MemoryMB,
			HtEnabled:  false, // Disable hyperthreading for security
		},
		// Add balloon device for memory management and monitoring
		Balloon: &BalloonDevice{
			AmountMib:             0,    // Start with no inflation
			DeflateOnOom:          true, // Allow deflation when guest is under memory pressure
			StatsPollingIntervalS: StatsPollingIntervalS(),
		},
	}

	// Add drives
	drives, err := cg.generateDrives(microvm, storagePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to generate drives: %w", err)
	}
	config.Drives = drives

	// Add network interfaces
	networkInterfaces, err := cg.generateNetworkInterfaces(microvm)
	if err != nil {
		return nil, fmt.Errorf("failed to generate network interfaces: %w", err)
	}
	config.NetworkInterfaces = networkInterfaces

	// Add logging
	config.Logger = cg.generateLogger(microvm)

	// Add metrics if enabled
	config.Metrics = cg.generateMetrics(microvm)

	return config, nil
}

// getKernelPath returns the path to the kernel image
func (cg *ConfigGenerator) getKernelPath() string {
	if cg.config.KernelPath != "" {
		return cg.config.KernelPath
	}
	// Default kernel path
	return "/opt/firecracker/vmlinux"
}

// generateBootArgs generates kernel boot arguments
func (cg *ConfigGenerator) generateBootArgs(microvm *models.MicroVMInstance, privateKey string) string {
	baseArgs := "console=ttyS0 reboot=k panic=1 pci=off"

	// Add network configuration based on mode
	switch microvm.Network.AssignmentMethod {
	case models.NetworkModeNAT:
		baseArgs += fmt.Sprintf(" ip=%s::192.168.100.1:255.255.255.0::eth0:off", microvm.Network.IPAddress)
	case models.NetworkModeDHCP:
		// In DHCP mode, let the kernel use DHCP or no IP args (guest handles it)
		baseArgs += " ip=dhcp"
	case models.NetworkModeMACVLAN:
		baseArgs += fmt.Sprintf(" ip=%s:::255.255.255.0::eth0:off", microvm.Network.IPAddress)
	case models.NetworkModeOVN:
		baseArgs += " ip=dhcp" // OVN typically uses DHCP
	}

	// Add cloud-init datasource for NoCloud seeded files
	baseArgs += " ds=nocloud"

	return baseArgs
}

// generateDrives generates drive configurations
func (cg *ConfigGenerator) generateDrives(microvm *models.MicroVMInstance, storagePaths *StoragePaths) ([]Drive, error) {
	var drives []Drive

	// Root filesystem drive
	rootDrive := Drive{
		DriveID:      "rootfs",
		PathOnHost:   storagePaths.RootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
		CacheType:    "Unsafe", // Use unsafe cache for better performance in development
	}
	drives = append(drives, rootDrive)

	// Data drive if present
	if storagePaths.DataPath != "" {
		dataDrive := Drive{
			DriveID:      "data",
			PathOnHost:   storagePaths.DataPath,
			IsRootDevice: false,
			IsReadOnly:   false,
			CacheType:    "Unsafe",
		}
		drives = append(drives, dataDrive)
	}

	return drives, nil
}

// generateNetworkInterfaces generates network interface configurations
func (cg *ConfigGenerator) generateNetworkInterfaces(microvm *models.MicroVMInstance) ([]NetworkInterface, error) {
	var interfaces []NetworkInterface

	// Generate TAP interface name based on microVM ID and network mode
	var hostDevName string
	switch microvm.Network.AssignmentMethod {
	case models.NetworkModeDHCP:
		// Use the TAP device recorded in network info (deterministic via dhcpTapNameFor)
		if microvm.Network.TAPDevice != "" {
			hostDevName = microvm.Network.TAPDevice
		} else {
			hostDevName = ovnTAPName(microvm.ID) // fallback
		}
	case models.NetworkModeNAT, models.NetworkModeOVN:
		hostDevName = ovnTAPName(microvm.ID)
	case models.NetworkModeMACVLAN:
		// Use the TAP device recorded in network info (includes proper macvlan name)
		if microvm.Network.TAPDevice != "" {
			hostDevName = microvm.Network.TAPDevice
		} else {
			hostDevName = fmt.Sprintf("mv-%s", microvm.ID[:12]) // fallback with correct prefix and length
		}
	default:
		return nil, fmt.Errorf("unsupported network mode: %s", microvm.Network.AssignmentMethod)
	}

	iface := NetworkInterface{
		IfaceID:     "eth0",
		GuestMac:    microvm.Network.MACAddress,
		HostDevName: hostDevName,
	}
	interfaces = append(interfaces, iface)

	return interfaces, nil
}

// generateLogger generates logging configuration
func (cg *ConfigGenerator) generateLogger(microvm *models.MicroVMInstance) *Logger {
	logPath := filepath.Join(cg.config.WorkDir, "microvms", microvm.ID, "firecracker.log")

	return &Logger{
		LogPath:       logPath,
		Level:         "Info",
		ShowLevel:     true,
		ShowLogOrigin: false,
	}
}

// generateMetrics generates metrics configuration
func (cg *ConfigGenerator) generateMetrics(microvm *models.MicroVMInstance) *Metrics {
	metricsPath := filepath.Join(cg.config.WorkDir, "microvms", microvm.ID, "metrics.fifo")

	return &Metrics{
		MetricsPath: metricsPath,
	}
}

// GenerateCloudInitConfig generates cloud-init configuration for SSH key injection
func (cg *ConfigGenerator) GenerateCloudInitConfig(microvm *models.MicroVMInstance, publicKey string) (string, error) {
	cloudInitConfig := fmt.Sprintf(`#cloud-config
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

# Set hostname
hostname: %s

# Configure network (if not using DHCP)
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: %t
      addresses:
        - %s/24
      gateway4: 192.168.100.1
      nameservers:
        addresses: [8.8.8.8, 8.8.4.4]

# Install basic packages
packages:
  - curl
  - wget
  - vim
  - htop

# Run commands on first boot
runcmd:
  - systemctl enable ssh
  - systemctl start ssh
  - echo "MicroVM %s initialized" > /var/log/microvm-init.log
`, publicKey, microvm.ID,
		microvm.Network.AssignmentMethod == models.NetworkModeDHCP,
		microvm.Network.IPAddress, microvm.ID)

	return cloudInitConfig, nil
}

// GenerateUserData generates user data for the microVM
func (cg *ConfigGenerator) GenerateUserData(microvm *models.MicroVMInstance, publicKey string) (string, error) {
	// Generate cloud-init user data
	userData := fmt.Sprintf(`#!/bin/bash
# MicroVM initialization script for %s

# Set up SSH key
mkdir -p /home/ubuntu/.ssh
echo "%s" > /home/ubuntu/.ssh/authorized_keys
chmod 600 /home/ubuntu/.ssh/authorized_keys
chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys

# Configure network if not DHCP
if [ "%s" != "dhcp" ]; then
    cat > /etc/netplan/01-microvm.yaml << EOF
network:
  version: 2
  ethernets:
    eth0:
      addresses: [%s/24]
      gateway4: 192.168.100.1
      nameservers:
        addresses: [8.8.8.8, 8.8.4.4]
EOF
    netplan apply
fi

# Start SSH service
systemctl enable ssh
systemctl start ssh

# Log initialization
echo "MicroVM %s initialized at $(date)" >> /var/log/microvm-init.log
`, microvm.ID, publicKey, microvm.Network.AssignmentMethod,
		microvm.Network.IPAddress, microvm.ID)

	return userData, nil
}
