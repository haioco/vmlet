package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/haioco/vmlet/internal/cloudinit"
	"github.com/haioco/vmlet/internal/images"
	"github.com/haioco/vmlet/internal/metrics"
	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/network"
	"github.com/haioco/vmlet/internal/storage"
	"github.com/haioco/vmlet/internal/validation"
	"golang.org/x/crypto/ssh"
)

// fetchAndInstallApkStatic downloads an apk-tools-static .apk, extracts apk.static and installs it at targetPath.
func (m *Manager) fetchAndInstallApkStatic(url, targetPath string) error {
	// Create temp dir
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("apk-fetch-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("mkdir temp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download file
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download apk-tools-static: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	// The .apk is a gzipped tar.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var apkStatic []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		// Look for apk.static (path often usr/bin/apk.static or sbin/apk.static)
		if strings.HasSuffix(hdr.Name, "apk.static") {
			apkStatic, err = io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read apk.static: %w", err)
			}
			break
		}
	}
	if len(apkStatic) == 0 {
		return fmt.Errorf("apk.static not found in archive")
	}

	// Write to temp file, then elevate to place into target (mount may require root)
	tmpFile := filepath.Join(tmpDir, "apk.static")
	if err := os.WriteFile(tmpFile, apkStatic, 0755); err != nil {
		return fmt.Errorf("write tmp apk: %w", err)
	}
	// Ensure target directory exists
	_ = os.MkdirAll(filepath.Dir(targetPath), 0755)
	// Copy using sudo to handle permissions on the mounted rootfs
	if err := exec.Command("sudo", "cp", tmpFile, targetPath).Run(); err != nil {
		return fmt.Errorf("sudo cp apk: %w", err)
	}
	_ = exec.Command("sudo", "chmod", "755", targetPath).Run()
	return nil
}

// Manager implements the MicroVMManagerInterface for firecracker
type Manager struct {
	config          *Config
	networkManager  *NetworkManager
	storageManager  *StorageManager
	sshKeyManager   *SSHKeyManager
	configGenerator *ConfigGenerator
	imageManager    *images.ImageManager
	enhancedNetwork network.Backend
	// backendMu guards reads of enhancedNetwork after construction.
	backendMu       sync.RWMutex
	bootValidator   *validation.BootValidator
	errorHandler    *ErrorHandler
	systemValidator *SystemValidator
	reservationMgr  *network.ReservationManager
	storageBackend  storage.StorageBackend
	workDir         string
	firecrackerBin  string
	jailerBin       string

	// consoleFIFOs holds open O_RDWR file descriptors for each VM's console.in
	// named pipe. Keeping a read+write fd open prevents systemd from blocking
	// when it opens the FIFO with O_RDONLY as the VM's stdin.
	consoleFIFOs sync.Map // map[vmID string]*os.File
}

// Config holds configuration for the firecracker manager
type Config struct {
	WorkDir        string `json:"work_dir"`
	FirecrackerBin string `json:"firecracker_bin"`
	JailerBin      string `json:"jailer_bin"`
	KernelPath     string `json:"kernel_path"`
	RootfsPath     string `json:"rootfs_path"`
	UseJailer      bool   `json:"use_jailer"`
	JailerUID      uint32 `json:"jailer_uid"`
	JailerGID      uint32 `json:"jailer_gid"`
	// Networking overrides
	NetworkBridgeName string `json:"network_bridge_name"`
	NetworkBridgeIP   string `json:"network_bridge_ip"`
	NetworkBridgeCIDR string `json:"network_bridge_cidr"`
	HostInterface     string `json:"host_interface"`
	DisableDHCP       bool   `json:"disable_dhcp"`
	ManageNAT         bool   `json:"manage_nat"`
	SSHPortStart      int    `json:"ssh_port_start"`
	SSHPortEnd        int    `json:"ssh_port_end"`
}

// calculateVMIP calculates a consistent IP address for a VM based on its ID
func (m *Manager) calculateVMIP(vmID string) string {
	hash := 0
	for _, c := range vmID {
		hash = hash*31 + int(c)
	}
	// Use range 10-99 for consistency
	ipSuffix := (hash % 90) + 10
	if ipSuffix < 0 {
		ipSuffix = -ipSuffix
	}
	return fmt.Sprintf("192.168.100.%d", ipSuffix)
}

// deriveNATIPv4FromIPv6 creates a private IPv4 address for NAT from an IPv6 address
// This allows IPv6-only VMs to access IPv4-only websites through shared NAT
// Example: 2001:41d0:203:67c1::65 -> 10.100.0.101
func (m *Manager) deriveNATIPv4FromIPv6(ipv6Address string) string {
	if ipv6Address == "" {
		return ""
	}
	// Parse the IPv6 address to extract the last segment
	// For addresses like 2001:41d0:203:67c1::65, extract "65" (hex) -> 101 (decimal)
	ip := net.ParseIP(ipv6Address)
	if ip == nil {
		return ""
	}
	ipv6 := ip.To16()
	if ipv6 == nil {
		return ""
	}
	// Get the last 2 bytes (last 16 bits) of the IPv6 address
	// For ::65, this is 0x0065 = 101 decimal
	lastOctet := int(ipv6[14])<<8 + int(ipv6[15])
	// Map to 10.100.0.X range (X = 2-254, reserve 1 for gateway)
	if lastOctet < 2 {
		lastOctet = 2
	}
	if lastOctet > 254 {
		lastOctet = lastOctet%253 + 2 // Wrap around to 2-254 range
	}
	return fmt.Sprintf("10.100.0.%d", lastOctet)
}

// NewManager creates a new firecracker manager
func NewManager(config *Config, reservationMgr *network.ReservationManager) (*Manager, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	// Validate required paths
	if config.FirecrackerBin == "" {
		config.FirecrackerBin = "/usr/local/bin/firecracker"
	}
	if config.JailerBin == "" {
		config.JailerBin = "/usr/local/bin/jailer"
	}
	if config.WorkDir == "" {
		config.WorkDir = "./firecracker-work"
	}

	// Set working defaults for our fixed configuration
	if config.NetworkBridgeName == "" {
		config.NetworkBridgeName = "fc-br0"
	}
	if config.NetworkBridgeIP == "" {
		config.NetworkBridgeIP = "192.168.100.1"
	}
	if config.NetworkBridgeCIDR == "" {
		config.NetworkBridgeCIDR = "192.168.100.0/24"
	}
	if config.SSHPortStart == 0 {
		config.SSHPortStart = 2200
	}
	if config.SSHPortEnd == 0 {
		config.SSHPortEnd = 2299
	}

	// Normalize work directory to absolute path and ensure it exists
	if !filepath.IsAbs(config.WorkDir) {
		if abs, err := filepath.Abs(config.WorkDir); err == nil {
			config.WorkDir = abs
		}
	}
	if err := os.MkdirAll(config.WorkDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Create legacy managers for backward compatibility
	networkManager, err := NewNetworkManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create network manager: %w", err)
	}

	storageManager, err := NewStorageManager(config.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage manager: %w", err)
	}

	sshKeyManager, err := NewSSHKeyManager(filepath.Join(config.WorkDir, "ssh-keys"))
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH key manager: %w", err)
	}

	configGenerator, err := NewConfigGenerator(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create config generator: %w", err)
	}

	// Create enhanced image manager
	imageManager := images.NewImageManager(
		config.WorkDir,
		filepath.Join(config.WorkDir, "images"),
		filepath.Join(config.WorkDir, "overlays"),
	)
	if err := imageManager.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %w", err)
	}

	// Create enhanced network manager with configurable behavior
	bridgeName := config.NetworkBridgeName
	if bridgeName == "" {
		bridgeName = "fc-nat-br0"
	}
	bridgeIP := config.NetworkBridgeIP
	if bridgeIP == "" {
		bridgeIP = "192.168.100.1"
	}
	bridgeCIDR := config.NetworkBridgeCIDR
	if bridgeCIDR == "" {
		bridgeCIDR = "192.168.100.0/24"
	}
	hostIf := config.HostInterface
	if hostIf == "" {
		hostIf = "eth0"
	}
	sshStart := config.SSHPortStart
	if sshStart == 0 {
		sshStart = 2200
	}
	sshEnd := config.SSHPortEnd
	if sshEnd == 0 {
		sshEnd = 2299
	}

	// Ensure network config dir is absolute
	netConfigDir := filepath.Join(config.WorkDir, "network")
	if !filepath.IsAbs(netConfigDir) {
		if abs, err := filepath.Abs(netConfigDir); err == nil {
			netConfigDir = abs
		}
	}
	networkConfig := &network.NetworkConfig{
		BridgeName:    bridgeName,
		BridgeIP:      bridgeIP,
		BridgeCIDR:    bridgeCIDR,
		HostInterface: hostIf,
		DHCPRange: network.DHCPRange{
			Start: "192.168.100.10",
			End:   "192.168.100.100",
		},
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		SSHPortRange: network.PortRange{
			Start: sshStart,
			End:   sshEnd,
		},
		ConfigDir:   netConfigDir,
		DisableDHCP: config.DisableDHCP,
		ManageNAT:   config.ManageNAT || bridgeName != "lxdbr0", // don't manage NAT on lxdbr0 by default
	}
	enhancedNetwork, err := network.NewNetworkBackend(networkConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize network backend: %w", err)
	}

	// The OVN backend owns the logical topology; hand its allocator to the
	// network manager so an OVN-mode VM gets a logical port, a DHCP
	// reservation and port security, not a placeholder address.
	if ovnBackend, ok := enhancedNetwork.(*network.OVNBackend); ok {
		networkManager.SetOVNAllocator(func(vmID, mac, networkID string) (*models.NetworkInfo, error) {
			info, err := ovnBackend.AllocateVMNetwork(vmID, mac, networkID)
			if err != nil {
				return nil, err
			}
			if info == nil {
				return nil, nil
			}
			return &models.NetworkInfo{
				IPAddress:        info.IPAddress,
				MACAddress:       info.MACAddress,
				AssignmentMethod: models.NetworkModeOVN,
				SSHPort:          22,
			}, nil
		})
	}

	// Create boot validator
	bootValidator := validation.NewBootValidator()

	// Create error handler
	errorHandler := NewErrorHandler(filepath.Join(config.WorkDir, "diagnostics"))

	// Create system validator
	systemValidator := NewSystemValidator()

	// Initialize storage backend (ZFS or File-based)
	storageBackend, err := storage.NewStorageBackend()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage backend: %w", err)
	}

	manager := &Manager{
		config:          config,
		networkManager:  networkManager,
		storageManager:  storageManager,
		sshKeyManager:   sshKeyManager,
		configGenerator: configGenerator,
		imageManager:    imageManager,
		enhancedNetwork: enhancedNetwork,
		bootValidator:   bootValidator,
		errorHandler:    errorHandler,
		systemValidator: systemValidator,
		reservationMgr:  reservationMgr,
		storageBackend:  storageBackend,
		workDir:         config.WorkDir,
		firecrackerBin:  config.FirecrackerBin,
		jailerBin:       config.JailerBin,
	}

	// Bring the selected network backend up. For OVN this connects to the
	// Northbound DB, ensures br-int exists and reconciles the logical topology
	// for this host's network; for bridge it creates the bridge and (unless
	// disabled) starts the DHCP server. Both are idempotent and fast.
	if err := enhancedNetwork.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize network backend: %w", err)
	}

	// Validate the datapath we are about to hand VMs to.
	if err := enhancedNetwork.ValidateNetworkSetup(); err != nil {
		return nil, fmt.Errorf("network backend validation failed: %w", err)
	}

	// Repair the per-VM systemd template now rather than on the first VM:
	// a host that previously ran another microVM platform has a template
	// pointing at a directory that no longer exists, and the first VM would
	// then fail with systemd's opaque "208/STDIN". Only warn here so a
	// non-root or container start still succeeds; the VM start path reports
	// a real error if the template genuinely cannot be written.
	if err := manager.ensureSystemdTemplate(); err != nil {
		fmt.Printf("⚠️  Could not install the %s systemd template: %v\n", systemTemplateName, err)
	}

	// Skip system validation for now to avoid hanging
	fmt.Printf("⚠️  Skipping system validation to avoid blocking\n")

	return manager, nil
}

// CreateMicroVM creates a new microVM instance with enhanced image and network management
func (m *Manager) CreateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	// Update status to creating
	microvm.Status = models.MicroVMStatusCreating

	// Set timeout for startup optimization (60 seconds for boot validation)
	_, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Step 1: Configure network based on mode
	var staticIP string
	var macAddress string

	// DEBUG: Log the assignment method
	fmt.Printf("🔍 DEBUG Manager.CreateMicroVM: VM %s has AssignmentMethod='%s' (comparing to NetworkModeDHCP='%s')\n",
		microvm.ID, microvm.Network.AssignmentMethod, models.NetworkModeDHCP)

	// For DHCP, MACVLAN, IPv6-only and OVN modes, use the NetworkManager to
	// configure networking properly
	if microvm.Network.AssignmentMethod == models.NetworkModeDHCP || microvm.Network.AssignmentMethod == models.NetworkModeMACVLAN || microvm.Network.AssignmentMethod == models.NetworkModeIPv6Only || microvm.Network.AssignmentMethod == models.NetworkModeOVN {
		fmt.Printf("🌐 Configuring %s networking for VM %s...\n", microvm.Network.AssignmentMethod, microvm.ID)

		// Check if custom MAC/IP are provided in the VM config
		if microvm.Network.MACAddress != "" {
			fmt.Printf("   Using custom MAC address: %s\n", microvm.Network.MACAddress)
			macAddress = microvm.Network.MACAddress
		}
		if microvm.Network.IPAddress != "" {
			fmt.Printf("   Using custom IP address: %s\n", microvm.Network.IPAddress)
			staticIP = microvm.Network.IPAddress
		}

		// For OVN, record the static IP on the backend before allocation so
		// the allocator uses it instead of auto-assigning.
		if microvm.Network.AssignmentMethod == models.NetworkModeOVN {
			if ovnBackend, ok := m.enhancedNetwork.(*network.OVNBackend); ok && ovnBackend != nil {
				if staticIP != "" {
					ovnBackend.SetStaticIP(microvm.ID, staticIP)
				}
				// Staged custom rules must be re-applied on every allocation, or
				// a VM booted before its rules were set would come back up
				// without them.
				if len(microvm.Network.SecurityRules) > 0 {
					ovnBackend.SetSecurityRules(microvm.ID, microvm.Network.SecurityRules)
				}
			}
		}

		// Pass bandwidth limits and custom IPv6 from API request to network configuration
		// Use the actual assignment method (DHCP or MACVLAN) not hardcoded DHCP
		networkInfo, err := m.networkManager.ConfigureNetwork(ctx, microvm.ID, microvm.Network.AssignmentMethod, macAddress, microvm.Network.BandwidthIngress, microvm.Network.BandwidthEgress, microvm.Network.IPv6Address, microvm.Network.IPv6Gateway, microvm.Network.IPv6PrefixLength, microvm.Network.NetworkID)
		if err != nil {
			microvm.Status = models.MicroVMStatusError
			return fmt.Errorf("failed to configure %s network: %w", microvm.Network.AssignmentMethod, err)
		}

		// Update microVM network info, but preserve custom MAC/IP if provided
		if macAddress == "" {
			macAddress = networkInfo.MACAddress
		} else {
			networkInfo.MACAddress = macAddress
		}
		if staticIP == "" {
			staticIP = networkInfo.IPAddress // Will be 0.0.0.0 placeholder for DHCP/MACVLAN
		} else {
			networkInfo.IPAddress = staticIP
		}

		// Update network fields without overwriting custom MAC/IP/IPv6
		microvm.Network.MACAddress = macAddress
		microvm.Network.IPAddress = staticIP
		microvm.Network.TAPDevice = networkInfo.TAPDevice
		microvm.Network.BridgeName = networkInfo.BridgeName
		microvm.Network.AssignmentMethod = networkInfo.AssignmentMethod
		microvm.Network.BandwidthIngress = networkInfo.BandwidthIngress
		microvm.Network.BandwidthEgress = networkInfo.BandwidthEgress

		// Update IPv6 info - use custom if provided, otherwise use auto-generated
		if microvm.Network.IPv6Address == "" {
			microvm.Network.IPv6Address = networkInfo.IPv6Address
		}
		// Always copy gateway and prefix from network config if not explicitly provided by user
		// This ensures VMs get the correct gateway even when only IPv6 address is auto-generated
		if microvm.Network.IPv6Gateway == "" && networkInfo.IPv6Gateway != "" {
			microvm.Network.IPv6Gateway = networkInfo.IPv6Gateway
		}
		if microvm.Network.IPv6PrefixLength == 0 && networkInfo.IPv6PrefixLength > 0 {
			microvm.Network.IPv6PrefixLength = networkInfo.IPv6PrefixLength
		}

		// Update IPv4 NAT flag from network configuration
		if networkInfo.IPv4NAT {
			microvm.Network.IPv4NAT = true
		}

		fmt.Printf("✅ %s network configured: MAC=%s, Interface=%s\n", microvm.Network.AssignmentMethod, macAddress, networkInfo.TAPDevice)
		if microvm.Network.IPv6Address != "" {
			fmt.Printf("🌐 IPv6: %s/%d (Gateway: %s)\n", microvm.Network.IPv6Address, microvm.Network.IPv6PrefixLength, microvm.Network.IPv6Gateway)
		}
		if microvm.Network.IPv4NAT {
			fmt.Printf("🔄 IPv4 NAT enabled for IPv4-only website access\n")
		}
		if microvm.Network.BandwidthIngress != "" || microvm.Network.BandwidthEgress != "" {
			fmt.Printf("📊 Bandwidth limits: Ingress=%s, Egress=%s\n",
				microvm.Network.BandwidthIngress, microvm.Network.BandwidthEgress)
		}
	} else {
		// For non-DHCP modes, use the legacy static IP assignment
		macAddress = m.networkManager.generateMACAddress(microvm.ID)
		macAddress = strings.ToLower(macAddress)

		// Assign static IP in our working range (192.168.100.10-100.99)
		hash := 0
		for _, c := range microvm.ID {
			hash = hash*31 + int(c)
		}
		ipSuffix := (hash % 90) + 10 // Range 10-99
		if ipSuffix < 0 {
			ipSuffix = -ipSuffix
		}
		staticIP = fmt.Sprintf("192.168.100.%d", ipSuffix)

		// Allow override via metadata label if provided
		if microvm.Metadata.Labels != nil {
			if desired, ok := microvm.Metadata.Labels["static_ip"]; ok && desired != "" {
				staticIP = desired
			} else if desired, ok := microvm.Metadata.Labels["cloudinit_static_ip"]; ok && desired != "" {
				staticIP = desired
			}
		}

		// Update microVM with network info
		// For DHCP mode, don't set a static IP (it will be obtained via DHCP)
		if microvm.Network.AssignmentMethod == models.NetworkModeDHCP {
			microvm.Network.IPAddress = "" // Will be discovered after DHCP lease
		} else {
			microvm.Network.IPAddress = staticIP
		}
		microvm.Network.MACAddress = macAddress
		microvm.Network.SSHPort = 22 // Standard SSH port
	} // Step 2: Prepare VM image using image manager
	imageID := "alpine-3.18" // Default
	if microvm.Metadata.Labels != nil {
		if id, exists := microvm.Metadata.Labels["image_id"]; exists && id != "" {
			imageID = id
		} else if name, exists := microvm.Metadata.Labels["image_name"]; exists && name != "" {
			imageID = name
		}
	}

	// Get SSH public key
	sshKeyPath := filepath.Join(m.workDir, "ssh-keys", "vm-key.pub")
	sshPublicKey := ""
	if keyData, err := os.ReadFile(sshKeyPath); err == nil {
		sshPublicKey = string(keyData)
	}

	// Extract cloud-init username and password from metadata if provided
	username := ""
	password := ""
	if microvm.Metadata.Labels != nil {
		if un := microvm.Metadata.Labels["cloudinit_username"]; un != "" {
			username = un
		}
		if pw := microvm.Metadata.Labels["cloudinit_password"]; pw != "" {
			password = pw
		}
	}
	fmt.Printf("🔍 Debug: Extracted username='%s' password='%s' from metadata\n", username, password)

	var vmRootfs string
	// Use externally supplied rootfs path when provided (e.g., LXD->MicroVM migration)
	if microvm.Metadata.Labels != nil {
		if externalRootfs := strings.TrimSpace(microvm.Metadata.Labels["external_rootfs_path"]); externalRootfs != "" {
			// Retry stat check up to 5 times with 1 second delay
			// This handles cases where partition devices take time to appear after ZFS clone
			var st os.FileInfo
			var err error
			maxRetries := 5
			for i := 0; i < maxRetries; i++ {
				st, err = os.Stat(externalRootfs)
				if err == nil {
					break
				}
				if i < maxRetries-1 {
					fmt.Printf("⏳ Waiting for external rootfs device %s to appear (attempt %d/%d)...\n", externalRootfs, i+1, maxRetries)
					time.Sleep(1 * time.Second)
				}
			}

			if err == nil && !st.IsDir() {
				fmt.Printf("📦 Using external rootfs for VM %s: %s\n", microvm.ID, externalRootfs)
				vmRootfs = externalRootfs
			} else {
				microvm.Status = models.MicroVMStatusError
				if err != nil {
					return fmt.Errorf("external_rootfs_path not accessible after %d retries: %s: %w", maxRetries, externalRootfs, err)
				}
				return fmt.Errorf("external_rootfs_path is a directory, expected file or block device: %s", externalRootfs)
			}
		}
	}
	// Reuse existing rootfs if this is a reboot path with persistence label set
	if vmRootfs == "" && microvm.Metadata.Labels != nil && microvm.Metadata.Labels["reuse_existing_rootfs"] == "true" {
		// ZFS production disks live as zvols, not as rootfs.ext4 files —
		// without this check every guest reboot on a ZFS node would fall
		// through to the prepare path and WIPE the customer's disk.
		if os.Getenv("MICROVM_USE_ZFS") == "true" && m.storageBackend != nil {
			zfsPool := os.Getenv("MICROVM_ZFS_POOL")
			if zfsPool == "" {
				zfsPool = "default"
			}
			zfsDataset := os.Getenv("MICROVM_ZFS_DATASET")
			if zfsDataset == "" {
				zfsDataset = fmt.Sprintf("%s/vmlet-microvms", zfsPool)
			}
			zvolPath := fmt.Sprintf("/dev/zvol/%s/%s", zfsDataset, microvm.ID)
			if st, err := os.Stat(zvolPath); err == nil && st.Mode()&os.ModeDevice != 0 {
				fmt.Printf("🔁 Reboot persistence: reusing existing ZFS zvol for VM %s at %s\n", microvm.ID, zvolPath)
				vmRootfs = zvolPath
			}
		}
		if vmRootfs == "" {
			existingRootfs := filepath.Join(m.workDir, "microvms", microvm.ID, "rootfs.ext4")
			if st, err := os.Stat(existingRootfs); err == nil && !st.IsDir() {
				fmt.Printf("🔁 Reboot persistence: reusing existing rootfs for VM %s at %s\n", microvm.ID, existingRootfs)
				vmRootfs = existingRootfs
			} else {
				fmt.Printf("⚠️  Persistence requested but existing rootfs not found; preparing fresh image for VM %s\n", microvm.ID)
			}
		}
	}
	if vmRootfs == "" { // normal creation path
		if microvm.Metadata.Labels != nil && strings.EqualFold(strings.TrimSpace(microvm.Metadata.Labels["skip_image_prepare"]), "true") {
			microvm.Status = models.MicroVMStatusError
			return fmt.Errorf("skip_image_prepare=true but no valid rootfs source was provided")
		}
		// A reinstall sets wipe_disk=true: destroy any stale zvol left behind
		// by a failed delete BEFORE preparing the image, so the "persistence
		// fast-path" inside prepareVMImageWithSize cannot silently reuse the
		// customer's old disk.
		wipeRequested := microvm.Metadata.Labels != nil &&
			strings.EqualFold(strings.TrimSpace(microvm.Metadata.Labels["wipe_disk"]), "true")
		if wipeRequested && os.Getenv("MICROVM_USE_ZFS") == "true" && m.storageBackend != nil {
			fmt.Printf("🧽 Reinstall wipe: destroying stale disk for VM %s before fresh install\n", microvm.ID)
			if err := m.storageBackend.DeleteDisk(microvm.ID); err != nil {
				fmt.Printf("⚠️  Reinstall wipe: pre-delete of disk for %s failed (continuing): %v\n", microvm.ID, err)
			}
		}
		fmt.Printf("Preparing %s image for VM %s with IP %s (size: %dMB)...\n", imageID, microvm.ID, staticIP, microvm.Resources.StorageMB)
		preparedRootfs, prepErr := m.prepareVMImageWithSize(imageID, microvm.ID, staticIP, sshPublicKey, microvm.Resources.StorageMB, username, password, wipeRequested)
		if prepErr != nil {
			microvm.Status = models.MicroVMStatusError
			return fmt.Errorf("failed to prepare VM image: %w", prepErr)
		}
		vmRootfs = preparedRootfs
		// Consume the one-shot wipe marker: a later recreate (guest reboot,
		// node restart) must REUSE the freshly installed disk, not wipe it.
		if wipeRequested && microvm.Metadata.Labels != nil {
			delete(microvm.Metadata.Labels, "wipe_disk")
		}
	}

	vmImage := &images.VMImage{RootfsPath: vmRootfs}

	// Configure cloud-init based on network mode
	cfg := &cloudinit.Config{
		Hostname:   microvm.ID, // default hostname
		Username:   username,   // from prepareVMImageWithSize
		Password:   password,   // from prepareVMImageWithSize
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		MACAddress: microvm.Network.MACAddress, // Pass MAC for interface matching
	}

	// Set static IP and IPv6 for non-DHCP modes
	if microvm.Network.AssignmentMethod != models.NetworkModeDHCP {
		cfg.StaticIP = staticIP
		cfg.Gateway = "192.168.100.1"

		// Add IPv6 configuration if available
		if microvm.Network.IPv6Address != "" {
			cfg.StaticIPv6 = microvm.Network.IPv6Address
			cfg.IPv6Gateway = microvm.Network.IPv6Gateway
			if microvm.Network.IPv6PrefixLength > 0 {
				cfg.IPv6Prefix = microvm.Network.IPv6PrefixLength
			} else {
				cfg.IPv6Prefix = 64 // Default prefix
			}
			// Add IPv6 DNS if not present
			if len(cfg.DNSServers) == 2 && cfg.DNSServers[0] == "8.8.8.8" {
				cfg.DNSServers = append(cfg.DNSServers, "2001:4860:4860::8888")
			}
		}

		// For IPv6-only mode or when IPv4 NAT is explicitly enabled, configure IPv4 NAT address
		// This enables access to IPv4-only websites (like GitHub) via shared NAT
		if microvm.Network.IPv4NAT || microvm.Network.AssignmentMethod == models.NetworkModeIPv6Only {
			// Use pre-assigned IPv4 NAT address from HTTP server, or derive from IPv6
			ipv4NatAddr := microvm.Network.IPv4NATAddress
			if ipv4NatAddr == "" && microvm.Network.IPv6Address != "" {
				// Derive private IPv4 address from IPv6 address last octet
				// Example: 2001:41d0:203:67c1::65 -> 10.100.0.101 (::65 = 101 decimal)
				ipv4NatAddr = m.deriveNATIPv4FromIPv6(microvm.Network.IPv6Address)
			}

			if ipv4NatAddr != "" {
				cfg.IPv4NATAddress = ipv4NatAddr
				cfg.IPv4NATGateway = "10.100.0.1"
				fmt.Printf("🔍 DEBUG: Just set cfg.IPv4NATAddress='%s' for %s\n", cfg.IPv4NATAddress, microvm.ID)
				// Ensure it's stored in VM network info for API visibility
				microvm.Network.IPv4NAT = true
				microvm.Network.IPv4NATAddress = ipv4NatAddr
				fmt.Printf("🌐 IPv4 NAT enabled for %s: IPv6=%s, IPv4 NAT=%s\n",
					microvm.ID, microvm.Network.IPv6Address, ipv4NatAddr)
			}
		}
	}
	// else: DHCP mode - don't configure static networking, let DHCP handle it
	// If labels specify hostname/username/sshkey, honor them (override defaults)
	if microvm.Metadata.Labels != nil {
		if hn := microvm.Metadata.Labels["cloudinit_hostname"]; hn != "" {
			cfg.Hostname = hn
		}
		if un := microvm.Metadata.Labels["cloudinit_username"]; un != "" {
			cfg.Username = un
		}
		// A reinstall stamps a nonce so cloud-init re-runs its per-instance
		// modules (set-passwords) instead of skipping them as "already done".
		if nonce := microvm.Metadata.Labels["reinstall_nonce"]; nonce != "" {
			cfg.InstanceIDNonce = nonce
		}
		// Accept plaintext or hashed password via labels
		if ph := microvm.Metadata.Labels["cloudinit_password_hash"]; ph != "" {
			cfg.PasswordHash = ph
		} else if pw := microvm.Metadata.Labels["cloudinit_password"]; pw != "" {
			cfg.Password = pw
		}
		// Every imported key must reach the guest, not just the first one.
		// cloudInitSSHKeys reads the JSON array and falls back to the legacy
		// single-key label for VMs created before multiple keys were kept.
		if keys := cloudInitSSHKeys(microvm.Metadata.Labels); len(keys) > 0 {
			cfg.SSHKeys = keys
		}
		// Distro hint
		if imgID, ok := microvm.Metadata.Labels["image_id"]; ok {
			lower := strings.ToLower(imgID)
			switch {
			case strings.HasPrefix(lower, "ubuntu-"):
				cfg.Distro = "ubuntu"
			case strings.HasPrefix(lower, "debian-"):
				cfg.Distro = "debian"
			case strings.HasPrefix(lower, "alpine-"):
				cfg.Distro = "alpine"
			case strings.HasPrefix(lower, "almalinux-"):
				cfg.Distro = "almalinux"
			case strings.HasPrefix(lower, "rocky-"):
				cfg.Distro = "rocky"
			case strings.HasPrefix(lower, "fedora-"):
				cfg.Distro = "fedora"
			case strings.HasPrefix(lower, "archlinux-"):
				cfg.Distro = "archlinux"
			case strings.HasPrefix(lower, "sql-server-"):
				cfg.Distro = "ubuntu" // SQL Server runs on Ubuntu
				// Pass SA password via vendor-data for SQL Server configuration
				// The SQL Server image uses /opt/vmlet/set-sql-password.sh which parses:
				//   grep "sa_password:" "$VENDOR_DATA" | cut -d: -f2-
				// So we need a simple format that can be parsed with grep
				if saPassword := microvm.Metadata.Labels["sa_password"]; saPassword != "" {
					cfg.VendorData = fmt.Sprintf(`#cloud-config
sa_password: %s

runcmd:
  - echo "SQL Server instance %s initialized with custom SA password"
`, saPassword, microvm.ID)
					fmt.Printf("🔐 SQL Server: SA password configured via vendor-data for %s\n", microvm.ID)
				}
			}
		}
	}

	// A customer-facing name is not a hostname: keep only [a-z0-9-] so the
	// guest's `hostname(1)` accepts it (Persian names / spaces otherwise make
	// systemd fall back to "localhost"). microvm.ID is the fallback because it
	// is always a usable label.
	cfg.Hostname = cloudinit.SanitizeHostname(cfg.Hostname, microvm.ID)

	// Seed NoCloud files into rootfs so kernel ds=nocloud can always find it
	// This happens AFTER vendor-data is configured (for SQL Server SA password)
	if err := cloudinit.SeedRootfsWithNoCloud(cfg, vmImage.RootfsPath); err != nil {
		fmt.Printf("⚠️  Warning: Failed to seed rootfs with NoCloud files: %v\n", err)
	} else {
		fmt.Printf("☁️  Seeded NoCloud files into rootfs for VM %s\n", microvm.ID)
		if microvm.Metadata.Labels == nil {
			microvm.Metadata.Labels = map[string]string{}
		}
		microvm.Metadata.Labels["cloudinit_seeded"] = "true"
	}

	// Debug: Log cloud-init config before seeding
	fmt.Printf("🔧 Cloud-init config for %s: Username=%s, Password=%s, Hostname=%s\n", microvm.ID, cfg.Username, cfg.Password, cfg.Hostname)
	if cfg.StaticIPv6 != "" {
		fmt.Printf("🌐 IPv6 config for %s: Address=%s/%d, Gateway=%s\n", microvm.ID, cfg.StaticIPv6, cfg.IPv6Prefix, cfg.IPv6Gateway)
	}
	if cfg.IPv4NATAddress != "" {
		fmt.Printf("🌐 IPv4 NAT config for %s: Address=%s, Gateway=%s\n", microvm.ID, cfg.IPv4NATAddress, cfg.IPv4NATGateway)
	} else {
		fmt.Printf("⚠️  IPv4 NAT NOT configured for %s (IPv4NATAddress is empty)\n", microvm.ID)
	}

	// NOTE: Cloud-init seeding moved to after vendor-data configuration

	// Optionally also generate and attach a cloud-init seed drive (vfat) for broader compatibility
	// For Ubuntu/Debian with NoCloud already seeded into the filesystem and proper kernel ds=nocloud,
	// skip attaching the extra ISO to avoid /dev/vdb probing delays unless explicitly forced.
	shouldAttachISO := true
	distroHint := ""
	if microvm.Metadata.Labels != nil {
		if imgID, ok := microvm.Metadata.Labels["image_id"]; ok {
			lower := strings.ToLower(imgID)
			switch {
			case strings.HasPrefix(lower, "ubuntu-"):
				distroHint = "ubuntu"
			case strings.HasPrefix(lower, "debian-"):
				distroHint = "debian"
			case strings.HasPrefix(lower, "alpine-"):
				distroHint = "alpine"
			}
		}
	}
	// Allow override via label
	forceISO := false
	if microvm.Metadata.Labels != nil {
		if v, ok := microvm.Metadata.Labels["cloudinit_force_iso"]; ok && strings.ToLower(v) == "true" {
			forceISO = true
		}
	}
	if (distroHint == "ubuntu" || distroHint == "debian") && !forceISO {
		shouldAttachISO = false
	}

	if shouldAttachISO {
		cloudInitPath, err := m.generateCloudInitISO(microvm, staticIP)
		if err != nil {
			fmt.Printf("⚠️  Warning: Failed to generate cloud-init ISO: %v\n", err)
			// Don't fail the VM creation if cloud-init fails, just log it
		} else if cloudInitPath != "" {
			vmImage.CloudInitPath = cloudInitPath
			fmt.Printf("☁️  Cloud-init seed image created at %s\n", cloudInitPath)
			if microvm.Metadata.Labels == nil {
				microvm.Metadata.Labels = map[string]string{}
			}
			microvm.Metadata.Labels["cloudinit_path"] = cloudInitPath
			microvm.Metadata.Labels["cloudinit_attached"] = "true"
			fmt.Printf("☁️  Will attach cloud-init seed drive as /dev/vdb for VM %s\n", microvm.ID)
		}
	} else {
		if microvm.Metadata.Labels == nil {
			microvm.Metadata.Labels = map[string]string{}
		}
		microvm.Metadata.Labels["cloudinit_attached"] = "false"
		microvm.Metadata.Labels["cloudinit_mode"] = "seeded_rootfs_only"
		fmt.Printf("☁️  Skipping cloud-init ISO attachment (using seeded NoCloud files) for VM %s\n", microvm.ID)
	}

	// Step 3: Skip storage provisioning for now (using rootfs directly)
	storagePaths := &StoragePaths{
		RootfsPath: vmImage.RootfsPath,
	}

	// Step 4: Set up network interface (skip for DHCP/MACVLAN modes - already configured)
	// For DHCP/MACVLAN modes, interface was already created by NetworkManager.ConfigureNetwork()
	var tapName string
	if microvm.Network.AssignmentMethod == models.NetworkModeDHCP || microvm.Network.AssignmentMethod == models.NetworkModeMACVLAN {
		// Use the interface that was already created (TAP for DHCP, macvlan for MACVLAN)
		tapName = microvm.Network.TAPDevice
		fmt.Printf("✅ Using existing %s interface: %s\n", microvm.Network.AssignmentMethod, tapName)
		if microvm.Network.BridgeName != "" {
			fmt.Printf("   (Bridge/Parent: %s)\n", microvm.Network.BridgeName)
		}

		// Apply anti-spoofing protection for DHCP mode
		if m.reservationMgr != nil && macAddress != "" {
			// Wait a moment for DHCP to potentially assign IP
			time.Sleep(2 * time.Second)

			// Try to get IP from reservation or use placeholder
			ipAddr := microvm.Network.IPAddress
			if ipAddr == "" || ipAddr == "0.0.0.0" {
				// Check if we have a reservation
				if reservation, err := m.reservationMgr.GetReservation(microvm.ID); err == nil && reservation != nil {
					ipAddr = reservation.IPAddress
				}
			}

			if ipAddr != "" && ipAddr != "0.0.0.0" {
				fmt.Printf("🔒 Applying anti-spoofing protection: TAP=%s, MAC=%s, IP=%s\n",
					tapName, macAddress, ipAddr)

				if err := m.reservationMgr.ApplySpoofingProtection(tapName, macAddress, ipAddr); err != nil {
					fmt.Printf("⚠️  Warning: Failed to apply anti-spoofing rules: %v\n", err)
					// Don't fail VM creation, just log warning
				} else {
					fmt.Printf("✅ Anti-spoofing protection applied successfully\n")
				}
			}

			// Apply bandwidth limits if specified
			if microvm.Network.BandwidthIngress != "" || microvm.Network.BandwidthEgress != "" {
				bandwidthLimit := parseBandwidthLimits(microvm.Network.BandwidthIngress, microvm.Network.BandwidthEgress)
				if bandwidthLimit != nil {
					fmt.Printf("🚦 Applying bandwidth limits: TAP=%s, Ingress=%dMbps, Egress=%dMbps\n",
						tapName, bandwidthLimit.IngressMbps, bandwidthLimit.EgressMbps)

					if err := m.reservationMgr.ApplyBandwidthLimit(tapName, bandwidthLimit); err != nil {
						fmt.Printf("⚠️  Warning: Failed to apply bandwidth limits: %v\n", err)
					} else {
						fmt.Printf("✅ Bandwidth limits applied successfully\n")
					}
				}
			}
		}
	} else {
		// For NAT/other modes: create TAP and attach to bridge
		tapName = m.tapNameFor(microvm.ID)
		if err := m.createTAPInterfaceWithRetry(ctx, tapName); err != nil {
			// Clean up on failure
			microvm.Status = models.MicroVMStatusError
			m.cleanupWithRecovery(ctx, microvm, vmImage, storagePaths)
			return m.errorHandler.RetryWithBackoff(ctx, func() error {
				return fmt.Errorf("failed to create TAP interface after retries: %w", err)
			}, "network_interface_creation")
		}

		bridgeName := m.config.NetworkBridgeName
		if bridgeName == "" {
			bridgeName = "fc-br0"
		}

		if err := m.attachTAPToBridgeWithRetry(ctx, tapName, bridgeName); err != nil {
			// Clean up on failure
			_ = m.networkManager.removeTAPInterface(tapName)
			microvm.Status = models.MicroVMStatusError
			m.cleanupWithRecovery(ctx, microvm, vmImage, storagePaths)
			return m.errorHandler.RetryWithBackoff(ctx, func() error {
				return fmt.Errorf("failed to attach TAP to bridge after retries: %w", err)
			}, "network_interface_creation")
		}

		// Apply anti-spoofing protection for static IP mode
		if m.reservationMgr != nil && staticIP != "" && macAddress != "" {
			fmt.Printf("🔒 Applying anti-spoofing protection: TAP=%s, MAC=%s, IP=%s\n",
				tapName, macAddress, staticIP)

			if err := m.reservationMgr.ApplySpoofingProtection(tapName, macAddress, staticIP); err != nil {
				fmt.Printf("⚠️  Warning: Failed to apply anti-spoofing rules: %v\n", err)
				// Don't fail VM creation, just log warning
			} else {
				fmt.Printf("✅ Anti-spoofing protection applied successfully\n")
			}

			// Apply bandwidth limits if specified
			if microvm.Network.BandwidthIngress != "" || microvm.Network.BandwidthEgress != "" {
				bandwidthLimit := parseBandwidthLimits(microvm.Network.BandwidthIngress, microvm.Network.BandwidthEgress)
				if bandwidthLimit != nil {
					fmt.Printf("🚦 Applying bandwidth limits: TAP=%s, Ingress=%dMbps, Egress=%dMbps\n",
						tapName, bandwidthLimit.IngressMbps, bandwidthLimit.EgressMbps)

					if err := m.reservationMgr.ApplyBandwidthLimit(tapName, bandwidthLimit); err != nil {
						fmt.Printf("⚠️  Warning: Failed to apply bandwidth limits: %v\n", err)
					} else {
						fmt.Printf("✅ Bandwidth limits applied successfully\n")
					}
				}
			}
		}
	}

	// Step 5: Generate enhanced firecracker configuration
	// Convert StoragePaths to map for compatibility
	storagePathsMap := make(map[string]string)
	if storagePaths.RootfsPath != "" {
		storagePathsMap["rootfs"] = storagePaths.RootfsPath
	}
	if storagePaths.DataPath != "" {
		storagePathsMap["data"] = storagePaths.DataPath
	}

	fcConfig, err := m.generateEnhancedConfig(microvm, vmImage, storagePathsMap)
	if err != nil {
		// Clean up on failure
		_ = m.networkManager.removeTAPInterface(tapName)
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to generate enhanced firecracker config: %w", err)
	}

	// Step 6: Create microVM directory and write configuration
	microvmDir := filepath.Join(m.workDir, "microvms", microvm.ID)
	if err := os.MkdirAll(microvmDir, 0755); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to create microvm directory: %w", err)
	}

	configPath := filepath.Join(microvmDir, "config.json")

	// Step 6: Set up console logging & the Firecracker logger *before* the config is
	// written. setupConsoleLogging mutates the config map (adds the "logger" device);
	// doing it after the write meant the logger block never reached disk and every
	// API request was logged into console.log (issue #28).
	consoleLogPath := filepath.Join(microvmDir, "console.log")
	if err := m.setupConsoleLogging(fcConfig, consoleLogPath); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to setup console logging: %w", err)
	}

	configData, err := json.MarshalIndent(fcConfig, "", "  ")
	if err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to write config file: %w", err)
	}

	// Step 6.1: Console log file already prepared above

	// Step 7: Start firecracker process (use short socket path to avoid SUN_LEN issues)
	socketPath := m.socketPathFor(microvm.ID)
	if err := m.startFirecracker(ctx, microvm.ID, configPath, socketPath); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	// Step 8: Wait for SSH port to become ready (max 30s)
	sshPort := microvm.Network.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	defer bootCancel()

	bootStart := time.Now()
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", microvm.Network.IPAddress, sshPort), 500*time.Millisecond)
		if err == nil {
			conn.Close()
			metrics.SetGauge(metrics.LastBootDurationSeconds, time.Since(bootStart).Seconds(), map[string]string{"vm_id": microvm.ID})
			break
		}
		select {
		case <-bootCtx.Done():
			fmt.Printf("Warning: SSH not ready after 30s for VM %s\n", microvm.ID)
			goto bootCheck
		case <-time.After(500 * time.Millisecond):
		}
	}
bootCheck:

	if err := m.testVMConnectivity(microvm.Network.IPAddress); err != nil {
		fmt.Printf("Warning: VM connectivity test failed: %v\n", err)
	} else {
		fmt.Printf("VM connectivity test successful: %s\n", microvm.Network.IPAddress)
	}

	if microvm.Metadata.Labels != nil {
		if pw := microvm.Metadata.Labels["cloudinit_password"]; pw != "" {
			if err := m.verifySSHPassword(microvm, pw); err != nil {
				fmt.Printf("Warning: SSH password verification failed for VM %s: %v\n", microvm.ID, err)
			} else {
				fmt.Printf("SSH password verified for VM %s\n", microvm.ID)
			}
		}
	}

	microvm.Status = models.MicroVMStatusRunning
	microvm.UpdateLastActivity()

	// Set auto-start flag for newly created VMs
	microvm.SetAutoStart(true)

	// Update process information
	if err := m.UpdateVMProcessInfo(microvm); err != nil {
		fmt.Printf("Warning: failed to update process info for VM %s: %v\n", microvm.ID, err)
	}

	return nil
}

// StartMicroVM starts an existing microVM
func (m *Manager) StartMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	// If already running, just refresh status and return
	if m.IsVMProcessRunning(microvm.ID) {
		microvm.Status = models.MicroVMStatusRunning
		microvm.UpdateLastActivity()
		if err := m.UpdateVMProcessInfo(microvm); err != nil {
			fmt.Printf("Warning: failed to update process info for VM %s: %v\n", microvm.ID, err)
		}
		return nil
	}

	// Proceed to start if we have a config; allow recovery from 'creating' or 'error' states
	microvmDir := filepath.Join(m.workDir, "microvms", microvm.ID)
	configPath := filepath.Join(microvmDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("cannot start VM %s: missing config at %s", microvm.ID, configPath)
	}

	// Ensure TAP device is ready before starting Firecracker
	// This handles the case where TAP was deleted or not attached to bridge after reboot
	if microvm.Network.TAPDevice != "" {
		fmt.Printf("🔧 Ensuring TAP device %s is ready for VM %s...\n", microvm.Network.TAPDevice, microvm.ID)
		if err := m.networkManager.EnsureTAPReady(
			microvm.Network.TAPDevice,
			microvm.Network.BridgeName,
			microvm.Network.BandwidthIngress,
			microvm.Network.BandwidthEgress,
		); err != nil {
			return fmt.Errorf("failed to ensure TAP device ready: %w", err)
		}
	}

	socketPath := m.socketPathFor(microvm.ID)
	microvm.Status = models.MicroVMStatusCreating

	// Start firecracker process
	if err := m.startFirecracker(ctx, microvm.ID, configPath, socketPath); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	// Wait for microVM to be ready
	if err := m.waitForMicroVMReady(ctx, socketPath, 30*time.Second); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("microvm failed to become ready: %w", err)
	}

	microvm.Status = models.MicroVMStatusRunning
	microvm.UpdateLastActivity()

	// Set auto-start flag when manually starting VMs
	microvm.SetAutoStart(true)

	// Update process information
	if err := m.UpdateVMProcessInfo(microvm); err != nil {
		fmt.Printf("Warning: failed to update process info for VM %s: %v\n", microvm.ID, err)
	}

	return nil
}

// StopMicroVM stops a running microVM
func (m *Manager) StopMicroVM(ctx context.Context, microvm *models.MicroVMInstance, force bool) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	if !microvm.CanBeStopped() && !force {
		return fmt.Errorf("microvm %s cannot be stopped, current status: %s", microvm.ID, microvm.Status)
	}

	microvm.Status = models.MicroVMStatusStopping

	// Find and stop the firecracker process. stopFirecracker now verifies the
	// unit actually left active/activating state, so an error here means the
	// guest may still be running (e.g. stuck in uninterruptible I/O). Returning
	// the error lets DELETE fail loudly instead of reporting a clean delete
	// while the zvol/TAP/address are still held by a live guest.
	if err := m.stopFirecracker(ctx, microvm.ID, force); err != nil {
		microvm.Status = models.MicroVMStatusError
		return fmt.Errorf("failed to stop firecracker: %w", err)
	}

	microvm.Status = models.MicroVMStatusStopped
	microvm.UpdateLastActivity()

	// Clear process information when VM stops
	microvm.ClearProcessInfo()

	return nil
}

// DeleteMicroVM deletes a microVM and cleans up resources using enhanced managers
func (m *Manager) DeleteMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	fmt.Printf("🗑️  Deleting VM %s (status: %s)...\n", microvm.ID, microvm.Status)

	// Step 1: Stop the microVM if it's running (force stop to ensure it stops).
	// Check the live process too: a half-finished reinstall leaves the status
	// at "creating" while a Firecracker process is alive holding the zvol
	// open — without this stop, the delete below fails with "dataset is busy"
	// and the disk silently survives the reinstall.
	if microvm.IsRunning() || m.IsVMProcessRunning(microvm.ID) {
		fmt.Printf("🛑 Stopping VM %s before deletion...\n", microvm.ID)
		if err := m.StopMicroVM(ctx, microvm, true); err != nil {
			return fmt.Errorf("failed to stop microvm before deletion: %w", err)
		}
	}

	// Step 2: Wait for VM to fully stop and verify it's not running
	fmt.Printf("⏳ Verifying VM %s is fully stopped...\n", microvm.ID)
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		if !m.IsVMProcessRunning(microvm.ID) {
			fmt.Printf("✅ VM %s confirmed stopped\n", microvm.ID)
			break
		}
		if i == maxRetries-1 {
			fmt.Printf("⚠️  Warning: VM %s still running after stop attempt, forcing cleanup\n", microvm.ID)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Step 3: Clean up network resources (TAP, anti-spoofing, bandwidth limits)
	var cleanupErrors []error

	// Clean up TAP interface(s)
	// Prefer removing the exact TAP device recorded in VM network info (DHCP mode)
	if microvm.Network.TAPDevice != "" {
		fmt.Printf("🧹 Cleaning up network for TAP: %s\n", microvm.Network.TAPDevice)

		// Remove anti-spoofing protection first (ebtables/iptables rules)
		if err := m.networkManager.removeAntiSpoofing(
			microvm.Network.TAPDevice,
			microvm.Network.MACAddress,
			microvm.Network.IPAddress,
			microvm.Network.IPv6Address,
		); err != nil {
			fmt.Printf("⚠️  Warning: Failed to remove anti-spoofing rules for %s: %v\n", microvm.Network.TAPDevice, err)
		} else {
			fmt.Printf("✅ Removed anti-spoofing rules for %s\n", microvm.Network.TAPDevice)
		}

		// Remove bandwidth limits via reservation manager if available
		if m.reservationMgr != nil {
			if err := m.reservationMgr.RemoveBandwidthLimit(microvm.Network.TAPDevice); err != nil {
				fmt.Printf("⚠️  Warning: Failed to remove bandwidth limits: %v\n", err)
			}
		}

		// Now remove the TAP interface
		if err := m.networkManager.removeTAPInterface(microvm.Network.TAPDevice); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove TAP interface %s: %w", microvm.Network.TAPDevice, err))
		} else {
			fmt.Printf("✅ Removed TAP interface: %s\n", microvm.Network.TAPDevice)
		}
	}

	// Also attempt legacy/alternate naming to cover older VMs
	legacyTap := m.tapNameFor(microvm.ID)
	if legacyTap != microvm.Network.TAPDevice {
		// Clean up legacy TAP anti-spoofing rules too
		m.networkManager.removeAntiSpoofing(legacyTap, microvm.Network.MACAddress, microvm.Network.IPAddress, microvm.Network.IPv6Address)
		if m.reservationMgr != nil {
			m.reservationMgr.RemoveBandwidthLimit(legacyTap)
		}
		m.networkManager.removeTAPInterface(legacyTap)
	}

	// Clean up legacy network (for backward compatibility)
	if err := m.networkManager.CleanupNetwork(ctx, microvm.ID, microvm.Network.AssignmentMethod); err != nil {
		// Non-fatal; log quietly for now
		fmt.Printf("ℹ️  Legacy network cleanup skipped: %v\n", err)
	}

	// Clean up VM image and overlays
	if err := m.imageManager.RemoveVMImage(microvm.ID); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove VM image: %w", err))
	}

	// Clean up VM-specific rootfs image (alpine VMs)
	vmRootfsPath := filepath.Join(m.workDir, fmt.Sprintf("rootfs-%s.img", microvm.ID))
	if _, err := os.Stat(vmRootfsPath); err == nil {
		if err := os.Remove(vmRootfsPath); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove VM rootfs: %w", err))
		} else {
			fmt.Printf("✓ Removed VM-specific rootfs: %s\n", vmRootfsPath)
		}
	}

	// Clean up cloud-init ISO
	cloudInitPath := filepath.Join(m.workDir, fmt.Sprintf("cloudinit-%s.iso", microvm.ID))
	if _, err := os.Stat(cloudInitPath); err == nil {
		if err := os.Remove(cloudInitPath); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove cloud-init ISO: %w", err))
		} else {
			fmt.Printf("✓ Removed cloud-init ISO: %s\n", cloudInitPath)
		}
	}

	// Step 4: Clean up ZFS zvol if using ZFS storage backend
	if m.storageBackend != nil && os.Getenv("MICROVM_USE_ZFS") == "true" {
		fmt.Printf("🗑️  Deleting ZFS zvol for VM %s...\n", microvm.ID)

		// Retry ZFS deletion until success or 60-second timeout
		zfsDeleted := false
		timeout := 60 * time.Second
		startTime := time.Now()
		retryDelay := 2 * time.Second
		attempt := 0

		for time.Since(startTime) < timeout {
			attempt++

			if attempt > 1 {
				elapsed := time.Since(startTime)
				remaining := timeout - elapsed
				fmt.Printf("⏳ ZFS zvol busy, waiting %v before retry (attempt %d, elapsed: %v, remaining: %v)...\n",
					retryDelay, attempt, elapsed.Round(time.Second), remaining.Round(time.Second))
				time.Sleep(retryDelay)
			}

			if err := m.storageBackend.DeleteDisk(microvm.ID); err != nil {
				if strings.Contains(err.Error(), "dataset is busy") || strings.Contains(err.Error(), "device busy") {
					// Continue retrying if within timeout
					if time.Since(startTime) < timeout {
						continue
					}
					cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to delete ZFS zvol after %v timeout: %w", timeout, err))
				} else {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to delete ZFS zvol: %w", err))
					break
				}
			} else {
				fmt.Printf("✅ Deleted ZFS zvol: default/vmlet-microvms/%s (attempt %d, elapsed: %v)\n",
					microvm.ID, attempt, time.Since(startTime).Round(time.Second))
				zfsDeleted = true
				break
			}
		}

		if !zfsDeleted {
			fmt.Printf("⚠️  Warning: ZFS zvol may still exist, manual cleanup may be required: zfs destroy default/vmlet-microvms/%s\n", microvm.ID)
		}
	}

	// Clean up storage
	if err := m.storageManager.CleanupStorage(ctx, microvm.ID, &microvm.Storage); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to cleanup storage: %w", err))
	}

	// Clean up SSH keys (legacy)
	if err := m.sshKeyManager.DeleteKeyPair(microvm.ID); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to cleanup SSH keys: %w", err))
	}

	// Remove microVM directory
	microvmDir := filepath.Join(m.workDir, "microvms", microvm.ID)
	if err := os.RemoveAll(microvmDir); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove microvm directory: %w", err))
	}

	// Log cleanup errors but don't fail the operation
	for _, err := range cleanupErrors {
		fmt.Printf("Warning: cleanup error for microvm %s: %v\n", microvm.ID, err)
	}

	return nil
}

// GetMicroVMStatus gets the current status of a microVM
func (m *Manager) GetMicroVMStatus(ctx context.Context, microvmID string) (models.MicroVMStatus, error) {
	if microvmID == "" {
		return models.MicroVMStatusError, fmt.Errorf("microvmID cannot be empty")
	}

	// Check if firecracker process is running using PID file and process check
	if m.IsVMProcessRunning(microvmID) {
		return models.MicroVMStatusRunning, nil
	}

	// Check if socket exists and is accessible (fallback)
	socketPath := m.socketPathFor(microvmID)
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return models.MicroVMStatusStopped, nil
	}

	// Try to connect to the socket to verify it's responsive
	if err := m.checkFirecrackerHealth(ctx, socketPath); err != nil {
		return models.MicroVMStatusStopped, nil
	}

	return models.MicroVMStatusRunning, nil
}

// startFirecracker starts a firecracker process as an independent daemon
func (m *Manager) startFirecracker(ctx context.Context, microvmID, configPath, socketPath string) error {
	// Ensure socket directory exists (but don't remove it - other VMs may be using it!)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("failed to create socket dir: %w", err)
	}
	// Remove only this VM's stale socket file if it exists
	os.Remove(socketPath)

	var cmd *exec.Cmd
	if m.config.UseJailer {
		// Use jailer for additional security
		cmd = exec.Command(m.jailerBin,
			"--id", microvmID,
			"--exec-file", m.firecrackerBin,
			"--uid", fmt.Sprintf("%d", m.config.JailerUID),
			"--gid", fmt.Sprintf("%d", m.config.JailerGID),
			"--chroot-base-dir", m.workDir,
			"--",
			"--api-sock", socketPath,
			"--config-file", configPath,
		)
	} else {
		// Run firecracker directly - NOTE: Using exec.Command instead of CommandContext
		// to avoid tying the process to the parent context
		cmd = exec.Command(m.firecrackerBin,
			"--api-sock", socketPath,
			"--config-file", configPath,
		)
	}

	// Set up logging
	logFile := filepath.Join(filepath.Dir(configPath), "firecracker.log")
	logWriter, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	defer logWriter.Close()

	// Set up console logging — rotate instead of truncating so boot history
	// survives restarts (the web console tails this file). Keep one generation.
	// The systemd unit appends via StandardOutput=append:.
	consoleLogFile := filepath.Join(filepath.Dir(configPath), "console.log")
	rotateConsoleLog(consoleLogFile)
	// Keep the writer for the non-systemd path (cmd.Stdout assigned below)
	consoleWriter, err := os.OpenFile(consoleLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create console log file: %w", err)
	}
	defer consoleWriter.Close()

	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	// Create console.in FIFO for bidirectional serial console access.
	// The systemd unit has StandardInput=file:<fifo>, which opens it O_RDONLY.
	// We must open it O_RDWR here FIRST so that systemd's open doesn't block
	// (a FIFO open for reading blocks until there is at least one writer).
	consoleFifoPath := filepath.Join(filepath.Dir(configPath), "console.in")

	// Check if FIFO already exists and is a named pipe
	// Don't remove it if it exists and is valid (could be in use by active console)
	if fi, err := os.Stat(consoleFifoPath); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
		// FIFO exists and is a named pipe - reuse it
		fmt.Printf("⚠️  Console FIFO already exists, reusing: %s\n", consoleFifoPath)
	} else {
		// Remove stale file (if any) and create new FIFO
		os.Remove(consoleFifoPath)
		if fifoErr := syscall.Mkfifo(consoleFifoPath, 0600); fifoErr != nil {
			fmt.Printf("⚠️  Could not create console FIFO: %v\n", fifoErr)
		} else {
			fmt.Printf("✅ Console FIFO created: %s\n", consoleFifoPath)
		}
	}

	// Open the FIFO O_RDWR (keeper fd) to prevent systemd from blocking
	if fifoFd, openErr := os.OpenFile(consoleFifoPath, os.O_RDWR, 0); openErr == nil {
		// Store it so we can close it when the VM stops
		m.consoleFIFOs.Store(microvmID, fifoFd)
		fmt.Printf("✅ Opened console FIFO O_RDWR (keeper fd)\n")
	} else {
		fmt.Printf("⚠️  Could not open console FIFO O_RDWR: %v\n", openErr)
	}

	// Use systemd template service for VM independence
	serviceName := fmt.Sprintf("firecracker-vm@%s", microvmID)

	// The template has to exist with the right paths, and it has to be
	// regenerated whenever this host's work dir or Firecracker binary changes.
	// A stale one (e.g. left by a previous install pointing at a removed
	// directory) fails with an opaque "208/STDIN" from systemd.
	if err := m.ensureSystemdTemplate(); err != nil {
		return fmt.Errorf("failed to install %s systemd template: %w", systemTemplateName, err)
	}

	// Remove existing socket
	os.Remove(socketPath)

	// Start the VM using systemd service
	systemdCmd := exec.Command("systemctl", "start", serviceName)
	output, err := systemdCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("⚠️  systemctl start failed: %v, output: %s\n", err, string(output))
		return fmt.Errorf("failed to start firecracker service %s: %w", serviceName, err)
	}

	fmt.Printf("✅ Firecracker VM started as systemd service: %s\n", serviceName)

	// Wait a moment for the service to start
	time.Sleep(2 * time.Second)

	// Verify the service is running
	statusCmd := exec.Command("systemctl", "is-active", serviceName)
	if statusOutput, err := statusCmd.CombinedOutput(); err != nil {
		fmt.Printf("⚠️  Service status check failed: %v, output: %s\n", err, string(statusOutput))
	} else {
		fmt.Printf("Service status: %s\n", strings.TrimSpace(string(statusOutput)))
	}

	// Get the PID from systemd and save it to PID file for compatibility
	pidCmd := exec.Command("systemctl", "show", "--property=MainPID", "--value", serviceName)
	if pidOutput, err := pidCmd.CombinedOutput(); err == nil {
		pid := strings.TrimSpace(string(pidOutput))
		if pid != "0" && pid != "" {
			pidFile := filepath.Join(filepath.Dir(configPath), "firecracker.pid")
			if err := os.WriteFile(pidFile, []byte(pid), 0644); err != nil {
				fmt.Printf("⚠️  Failed to write PID file: %v\n", err)
			} else {
				fmt.Printf("✅ Saved PID %s to file\n", pid)
			}
		}
	}

	return nil
}

// stopFirecracker stops a firecracker VM service and verifies it actually left
// the active/activating state.
//
// `systemctl kill -s SIGKILL` alone does not stop the unit: systemd keeps the
// unit "active" while the process is still alive, and Firecracker can sit in
// uninterruptible (D) state on a thrashing ZFS host. The old force path ignored
// that, marked the VM stopped, and a subsequent delete reported success while
// the guest kept the zvol, TAP and IP. Always escalate and verify here.
func (m *Manager) stopFirecracker(ctx context.Context, microvmID string, force bool) error {
	serviceName := fmt.Sprintf("firecracker-vm@%s", microvmID)

	if force {
		// Kill the process, then ask systemd to bring the unit down as well so
		// it cannot be auto-restarted by Restart= policies while we clean up.
		if out, err := exec.Command("systemctl", "kill", "--signal=SIGKILL", serviceName).CombinedOutput(); err != nil {
			fmt.Printf("Warning: systemctl kill %s: %v (%s)\n", serviceName, err, strings.TrimSpace(string(out)))
		}
		exec.Command("systemctl", "stop", "--no-block", serviceName).Run()
	} else {
		output, err := exec.Command("systemctl", "stop", serviceName).CombinedOutput()
		if err != nil {
			if state := serviceActiveState(serviceName); state == "inactive" || state == "failed" {
				fmt.Printf("Service %s was already stopped\n", serviceName)
			} else {
				// Graceful stop failed; fall back to SIGKILL rather than leaving
				// the guest running.
				fmt.Printf("Graceful stop of %s failed (%v, %s); forcing\n", serviceName, err, strings.TrimSpace(string(output)))
				exec.Command("systemctl", "kill", "--signal=SIGKILL", serviceName).Run()
				exec.Command("systemctl", "stop", "--no-block", serviceName).Run()
			}
		}
	}

	// Wait up to 20s for the unit to leave active/activating/reloading.
	deadline := time.Now().Add(20 * time.Second)
	for {
		state := serviceActiveState(serviceName)
		if state == "inactive" || state == "failed" || state == "unknown" || state == "" {
			fmt.Printf("✅ Stopped firecracker service: %s (state=%s)\n", serviceName, state)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker service %s still %s after stop; guest may be stuck in I/O", serviceName, state)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Close the console.in FIFO keeper fd so the FIFO has no remaining writers.
	// This lets any tail goroutines detect EOF and Firecracker's stdin close cleanly.
	if fd, loaded := m.consoleFIFOs.LoadAndDelete(microvmID); loaded {
		if f, ok := fd.(*os.File); ok {
			f.Close()
		}
	}

	// Clean up PID file
	microvmDir := filepath.Join(m.workDir, "microvms", microvmID)
	pidFile := filepath.Join(microvmDir, "firecracker.pid")
	os.Remove(pidFile)

	return nil
}

// serviceActiveState returns `systemctl is-active` output for a unit
// (active / activating / reloading / inactive / failed / unknown).
func serviceActiveState(serviceName string) string {
	out, _ := exec.Command("systemctl", "is-active", serviceName).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// IsVMProcessRunning checks if a VM's systemd service is running.
//
// `systemctl is-active` exits non-zero for both "inactive" and "activating",
// and the caller must not treat the transient start-up state as "down": doing
// so re-issued StartMicroVM while the guest was already booting and produced
// spurious restarts of running VMs (observed in the image E2E run).
func (m *Manager) IsVMProcessRunning(vmID string) bool {
	serviceName := fmt.Sprintf("firecracker-vm@%s", vmID)

	output, _ := exec.Command("systemctl", "is-active", serviceName).CombinedOutput()
	switch strings.TrimSpace(string(output)) {
	case "active", "activating", "reloading":
		return true
	}
	return false
}

// waitForMicroVMReady waits for a microVM to become ready
func (m *Manager) waitForMicroVMReady(ctx context.Context, socketPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for microvm to become ready")
		case <-ticker.C:
			if err := m.checkFirecrackerHealth(ctx, socketPath); err == nil {
				return nil
			}
		}
	}
}

// checkFirecrackerHealth checks if firecracker is healthy
func (m *Manager) checkFirecrackerHealth(ctx context.Context, socketPath string) error {
	// For now, just check if socket exists and is accessible
	if _, err := os.Stat(socketPath); err != nil {
		return err
	}
	return nil
}

// socketPathFor returns a short, system-wide path for the Firecracker API socket
// SocketPathFor exposes the per-VM Firecracker API socket path (used by the
// migration package to drive snapshot APIs on the same socket).
func (m *Manager) SocketPathFor(vmID string) string {
	return m.socketPathFor(vmID)
}

func (m *Manager) socketPathFor(vmID string) string {
	// Prefer /run (tmpfs) if available, else fallback to /tmp
	base := "/run/microvmsvc"
	if _, err := os.Stat("/run"); os.IsNotExist(err) {
		base = "/tmp/microvmsvc"
	}
	// Ensure base dir exists (ignore errors here; startFirecracker will ensure again)
	_ = os.MkdirAll(base, 0755)
	return filepath.Join(base, fmt.Sprintf("%s.sock", vmID))
}

// tapNameFor returns a short interface name for the VM's TAP device
func (m *Manager) tapNameFor(vmID string) string {
	// Use simple naming to avoid Firecracker interface name issues
	// Hash the VM ID to get a consistent short name
	hash := 0
	for _, c := range vmID {
		hash = hash*31 + int(c)
	}
	// Use modulo to get a number between 0-999
	num := hash % 1000
	if num < 0 {
		num = -num
	}
	return fmt.Sprintf("tap%d", num)
}

// generateEnhancedConfig generates firecracker configuration with working settings
func (m *Manager) generateEnhancedConfig(microvm *models.MicroVMInstance, vmImage *images.VMImage, storagePaths map[string]string) (map[string]interface{}, error) {
	// Use the working rootfs path
	rootfsPath := vmImage.RootfsPath

	// Fallback to configured rootfs if needed
	if rootfsPath == "" || func() bool { _, err := os.Stat(rootfsPath); return err != nil }() {
		if m.config.RootfsPath != "" {
			rootfsPath = m.config.RootfsPath
		} else {
			return nil, fmt.Errorf("no valid rootfs path available")
		}
	}

	// Decide boot args based on image type: Debian/Ubuntu need systemd (no init=/init)
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/init rw random.trust_cpu=on"
	rootDevice := "/dev/vda"
	if microvm != nil && microvm.Metadata.Labels != nil {
		fmt.Printf("🔍 Debug: microvm.Metadata.Labels contains %d entries for %s\n", len(microvm.Metadata.Labels), microvm.ID)
		if rd := strings.TrimSpace(microvm.Metadata.Labels["boot_root_device"]); rd != "" {
			rootDevice = rd
			fmt.Printf("📝 Boot root device override for VM %s: %s\n", microvm.ID, rootDevice)
		}
		if imgID, ok := microvm.Metadata.Labels["image_id"]; ok {
			fmt.Printf("🔍 Debug: Found image_id=%s for VM %s\n", imgID, microvm.ID)
			lower := strings.ToLower(imgID)
			// Match systemd-based distros: ubuntu, debian, rocky, sql-server, or any image containing "debian" (e.g., vyos-debian-router)
			if strings.HasPrefix(lower, "ubuntu-") || strings.HasPrefix(lower, "debian-") || strings.HasPrefix(lower, "rocky-") || strings.HasPrefix(lower, "almalinux-") || strings.HasPrefix(lower, "fedora-") || strings.HasPrefix(lower, "archlinux-") || strings.HasPrefix(lower, "sql-server-") || strings.Contains(lower, "debian") {
				// Let the distro init manage boot; point root to /dev/vda. Enable classic interface naming.
				// Direct NoCloud to seeded filesystem path for faster metadata discovery, avoid network delays.
				// Provide early static IP via kernel ip= ONLY for non-DHCP modes to avoid forcing 0.0.0.0 when using DHCP.
				// Format ip=IP::GATEWAY:NETMASK:HOSTNAME:IFACE:off
				ipArg := ""
				fmt.Printf("🔍 Boot args debug: AssignmentMethod='%s', IPAddress='%s', checking DHCP mode\n", microvm.Network.AssignmentMethod, microvm.Network.IPAddress)
				// A static ip= is only correct for modes that actually have a
				// bridge gateway to point at. OVN is excluded for two reasons:
				// its guest must use the DHCP_Options we bound to its logical
				// port, and the hardcoded 192.168.100.1 below is the NAT-mode
				// gateway, which does not exist on an OVN network -- passing it
				// left an OVN VM with a default route straight off the overlay.
				usesDHCP := microvm.Network.AssignmentMethod == models.NetworkModeDHCP ||
					microvm.Network.AssignmentMethod == models.NetworkModeOVN
				if !usesDHCP && microvm.Network.IPAddress != "" && microvm.Network.IPAddress != "0.0.0.0" {
					ipArg = fmt.Sprintf("ip=%s::%s:255.255.255.0:%s:eth0:off ", microvm.Network.IPAddress, "192.168.100.1", microvm.ID)
					fmt.Printf("🔍 Boot args: Adding ip= parameter: %s\n", ipArg)
				} else {
					fmt.Printf("🔍 Boot args: Skipping ip= parameter (DHCP/OVN mode or empty/0.0.0.0 IP)\n")
				}
				// Include 'nomodules' to prevent attempts to load kernel modules in a microVM (saves time and noise)
				// Add cloud-init.dslist=NoCloud to disable EC2 metadata lookups (169.254.169.254)
				bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodules root=" + rootDevice + " rw net.ifnames=0 biosdevname=0 random.trust_cpu=on " + ipArg + "ds=nocloud;s=/var/lib/cloud/seed/nocloud/ cloud-init.dslist=NoCloud"
				fmt.Printf("📝 Boot args for VM %s: %s\n", microvm.ID, bootArgs)
			} else if strings.HasPrefix(lower, "alpine-") {
				// Alpine: force early init shim (/init) to bring up networking before OpenRC.
				// Keep interface names classic (eth0) and avoid module loading for faster boot.
				// DO NOT inject ip= for DHCP mode.
				// Add rc_parallel=NO to disable parallel service startup (prevents OpenRC deadlocks)
				ipArg := ""
				if microvm.Network.AssignmentMethod != models.NetworkModeDHCP && microvm.Network.IPAddress != "" && microvm.Network.IPAddress != "0.0.0.0" {
					ipArg = fmt.Sprintf("ip=%s::%s:255.255.255.0:%s:eth0:off ", microvm.Network.IPAddress, "192.168.100.1", microvm.ID)
				}
				// Add cloud-init.dslist=NoCloud to disable EC2 metadata lookups (169.254.169.254)
				bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodules root=" + rootDevice + " rw init=/init net.ifnames=0 random.trust_cpu=on rc_parallel=NO " + ipArg + "ds=nocloud;s=/var/lib/cloud/seed/nocloud/ cloud-init.dslist=NoCloud"
				fmt.Printf("📝 Boot args (Alpine) for VM %s: %s\n", microvm.ID, bootArgs)
			} else {
				fmt.Printf("🔍 Debug: image_id %s does not match ubuntu- or debian- prefix\n", imgID)
			}
		} else {
			fmt.Printf("🔍 Debug: image_id label not found in Metadata.Labels\n")
		}
	} else {
		fmt.Printf("🔍 Debug: microvm or Metadata.Labels is nil\n")
	}

	// Determine kernel path based on image
	kernelPath := m.config.KernelPath
	// Use standard working kernel for all VMs
	// (SQL-specific kernel has I/O port issues)

	config := map[string]interface{}{
		"boot-source": map[string]interface{}{
			"kernel_image_path": kernelPath,
			"boot_args":         bootArgs,
		},
		"drives": []map[string]interface{}{
			{
				"drive_id":       "rootfs",
				"path_on_host":   rootfsPath,
				"is_root_device": true,
				"is_read_only":   false,
			},
		},
		"machine-config": map[string]interface{}{
			"vcpu_count":   microvm.Resources.VCPUs,
			"mem_size_mib": microvm.Resources.MemoryMB,
		},
		"network-interfaces": []map[string]interface{}{
			{
				"iface_id":  "eth0",
				"guest_mac": microvm.Network.MACAddress,
				"host_dev_name": func() string {
					// Use TAP device from network configuration if available (DHCP mode)
					if microvm.Network.TAPDevice != "" {
						return microvm.Network.TAPDevice
					}
					// Fall back to legacy TAP naming for NAT mode
					return m.tapNameFor(microvm.ID)
				}(),
			},
		},
		// Ensure balloon device is present for memory management and monitoring
		"balloon": map[string]interface{}{
			"amount_mib":               0,
			"deflate_on_oom":           true,
			"stats_polling_interval_s": StatsPollingIntervalS(),
		},
	}

	// Add vsock device for host-guest communication (used by managed SQL Server)
	if microvm.Vsock != nil && microvm.Vsock.Enabled {
		vsockUDSPath := microvm.Vsock.UDSPath
		if vsockUDSPath == "" {
			vsockUDSPath = "vsock.sock" // Default socket name
		}
		// Firecracker expects absolute path for uds_path
		microvmDir := filepath.Join(m.workDir, "microvms", microvm.ID)
		vsockAbsPath := filepath.Join(microvmDir, vsockUDSPath)

		config["vsock"] = map[string]interface{}{
			"guest_cid": microvm.Vsock.GuestCID,
			"uds_path":  vsockAbsPath,
		}
		fmt.Printf("🔌 Vsock enabled for VM %s: CID=%d, socket=%s\n",
			microvm.ID, microvm.Vsock.GuestCID, vsockAbsPath)
	}

	// Add cloud-init VFAT seed drive as /dev/vdb if available
	if vmImage.CloudInitPath != "" {
		if _, err := os.Stat(vmImage.CloudInitPath); err == nil {
			cloudInitDrive := map[string]interface{}{
				"drive_id":       "cloudinit",
				"path_on_host":   vmImage.CloudInitPath,
				"is_root_device": false,
				"is_read_only":   true,
			}
			drives := config["drives"].([]map[string]interface{})
			config["drives"] = append(drives, cloudInitDrive)
		}
	}

	// Add additional storage drives
	driveIndex := 2
	for driveType, path := range storagePaths {
		if driveType != "rootfs" {
			drive := map[string]interface{}{
				"drive_id":       fmt.Sprintf("drive%d", driveIndex),
				"path_on_host":   path,
				"is_root_device": false,
				"is_read_only":   false,
			}
			drives := config["drives"].([]map[string]interface{})
			config["drives"] = append(drives, drive)
			driveIndex++
		}
	}

	return config, nil
}

// validateVMBoot performs simple boot validation
func (m *Manager) validateVMBoot(ctx context.Context, vmID, consoleLogPath, ipAddress string) error {
	// Simple validation - check if console log shows successful boot
	if _, err := os.Stat(consoleLogPath); err != nil {
		return fmt.Errorf("console log not found: %w", err)
	}

	// Test connectivity
	return m.testVMConnectivity(ipAddress)
}

// cleanupWithRecovery performs cleanup with error recovery
func (m *Manager) cleanupWithRecovery(ctx context.Context, microvm *models.MicroVMInstance, vmImage *images.VMImage, storagePaths *StoragePaths) {
	var cleanupErrors []error

	// Clean up TAP interface
	tapName := m.tapNameFor(microvm.ID)
	if err := m.networkManager.removeTAPInterface(tapName); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("network cleanup failed: %w", err))
	}

	if vmImage != nil {
		if err := m.imageManager.RemoveVMImage(microvm.ID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("image cleanup failed: %w", err))
		}
	}

	if storagePaths != nil {
		if err := m.storageManager.CleanupStorage(ctx, microvm.ID, &microvm.Storage); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("storage cleanup failed: %w", err))
		}
	}

	// Log cleanup errors but don't fail the operation
	for _, err := range cleanupErrors {
		fmt.Printf("Warning: cleanup error for microvm %s: %v\n", microvm.ID, err)
	}
}

// Shutdown gracefully shuts down the manager and cleans up resources
func (m *Manager) Shutdown() error {
	var errors []error

	// Shutdown enhanced network manager
	if err := m.enhancedNetwork.Shutdown(); err != nil {
		errors = append(errors, fmt.Errorf("failed to shutdown enhanced network: %w", err))
	}

	// Clean up image cache
	if err := m.imageManager.CleanupImages(); err != nil {
		errors = append(errors, fmt.Errorf("failed to cleanup images: %w", err))
	}

	if len(errors) > 0 {
		return fmt.Errorf("shutdown errors: %v", errors)
	}

	return nil
}

// ValidateSystemPrerequisites performs comprehensive system validation
func (m *Manager) ValidateSystemPrerequisites(autoFix bool) (*ValidationResult, error) {
	return m.systemValidator.ValidateAndSetup(autoFix)
}

// GetSystemValidationResult returns the current system validation status
func (m *Manager) GetSystemValidationResult() (*ValidationResult, error) {
	return m.systemValidator.ValidateSystem()
}

// GenerateSystemSetupScript generates a script to fix system issues
func (m *Manager) GenerateSystemSetupScript() (string, error) {
	result, err := m.systemValidator.ValidateSystem()
	if err != nil {
		return "", fmt.Errorf("failed to validate system: %w", err)
	}

	return m.systemValidator.GenerateSetupScript(result), nil
}

// testVMConnectivity tests if the VM is reachable via ping
func (m *Manager) testVMConnectivity(ipAddress string) error {
	cmd := exec.Command("ping", "-c", "3", "-W", "2", ipAddress)
	return cmd.Run()
}

// verifySSHPassword attempts to authenticate via SSH with the given password
func (m *Manager) verifySSHPassword(microvm *models.MicroVMInstance, password string) error {
	if microvm.Network.IPAddress == "" {
		return fmt.Errorf("VM has no IP address")
	}
	sshPort := microvm.Network.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	username := "root"
	if microvm.Metadata.Labels != nil {
		if un := microvm.Metadata.Labels["cloudinit_username"]; un != "" {
			username = un
		}
	}
	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", microvm.Network.IPAddress, sshPort)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("SSH password auth failed: %w", err)
	}
	defer client.Close()
	return nil
}

func (m *Manager) resetPassword(ctx context.Context, microvm *models.MicroVMInstance, newPassword string) error {
	if microvm.Network.IPAddress == "" {
		return fmt.Errorf("VM has no IP address")
	}
	sshPort := microvm.Network.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	username := microvm.Metadata.Labels["cloudinit_username"]
	if username == "" {
		username = "root"
	}
	escapedPassword := strings.ReplaceAll(newPassword, "'", "'\\''")
	cmd := fmt.Sprintf("echo '%s:%s' | chpasswd && sed -i 's/^#*PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && (systemctl restart ssh || systemctl restart sshd || true)", username, escapedPassword)
	sshConfig := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(newPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", microvm.Network.IPAddress, sshPort)
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session creation failed: %w", err)
	}
	defer session.Close()
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("password reset command failed: %w", err)
	}
	fmt.Printf("Password reset completed for VM %s\n", microvm.ID)
	return nil
}

// createWorkingRootfs creates a working busybox-based rootfs
func (m *Manager) createWorkingRootfs(rootfsPath string) error {
	fmt.Printf("Creating working rootfs at %s\n", rootfsPath)

	// Create 128MB image
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", rootfsPath), "bs=1M", "count=128")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create rootfs image: %w", err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", rootfsPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to format rootfs: %w", err)
	}

	// Mount and setup
	mountDir := filepath.Join(m.workDir, "rootfs-setup")
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount dir: %w", err)
	}

	// Mount the image
	cmd = exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}

	defer func() {
		exec.Command("sudo", "umount", mountDir).Run()
	}()

	// Create directory structure
	dirs := []string{"bin", "sbin", "etc", "proc", "sys", "dev", "tmp", "var", "usr", "home", "root"}
	for _, dir := range dirs {
		cmd = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, dir))
		cmd.Run()
	}

	// Install static busybox if available
	if _, err := os.Stat("/usr/bin/busybox"); err == nil {
		cmd = exec.Command("sudo", "cp", "/usr/bin/busybox", filepath.Join(mountDir, "bin", "busybox"))
		if err := cmd.Run(); err == nil {
			// Create symlinks for common commands
			commands := []string{"sh", "ash", "bash", "ls", "cat", "echo", "ps", "mount", "umount", "ip", "ping"}
			for _, cmdName := range commands {
				cmd = exec.Command("sudo", "ln", "-sf", "/bin/busybox", filepath.Join(mountDir, "bin", cmdName))
				cmd.Run()
			}
		}
	}

	// Create working init script with DHCP support
	initScript := `#!/bin/busybox sh
echo "MicroVM starting with busybox..."
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
echo "microvm" > /proc/sys/kernel/hostname
/bin/busybox ip link set lo up
if /bin/busybox ip link show eth0 >/dev/null 2>&1; then
    echo "Configuring eth0 network interface..."
    /bin/busybox ip link set eth0 up
    # Wait briefly for link to be ready
    /bin/busybox sleep 1
    # Disable reverse path filtering for better DHCP compatibility
    echo 0 > /proc/sys/net/ipv4/conf/eth0/rp_filter 2>/dev/null || true
    echo 0 > /proc/sys/net/ipv4/conf/all/rp_filter 2>/dev/null || true
    echo "Attempting DHCP configuration..."
    # Kill any existing udhcpc to avoid conflicts
    /bin/busybox pkill -x udhcpc 2>/dev/null || true
    # Try DHCP with foreground until lease, then background daemon
    # -f: foreground until lease obtained, -b: background after lease
    # -t: max tries, -T: timeout per try, -A: wait time after failure
    /bin/busybox udhcpc -i eth0 -f -b -t 10 -T 3 -A 2 2>&1 || {
        echo "First DHCP attempt failed, retrying..."
        /bin/busybox sleep 2
        /bin/busybox udhcpc -i eth0 -f -b -t 5 -T 3 2>&1 || {
            echo "DHCP failed after retries, using static IP fallback..."
            # Fallback to static IP if DHCP fails
            /bin/busybox ip addr add 192.168.100.10/24 dev eth0 2>/dev/null || true
            /bin/busybox ip route add default via 192.168.100.1 2>/dev/null || true
            echo "nameserver 8.8.8.8" > /etc/resolv.conf
            echo "nameserver 8.8.4.4" >> /etc/resolv.conf
            echo "Network configured: 192.168.100.10 (static fallback)"
        }
    }
    # Wait and verify IP assignment
    for i in 1 2 3 4 5; do
        if /bin/busybox ip addr show eth0 | /bin/busybox grep -q 'inet '; then
            IP_ADDR=$(/bin/busybox ip addr show eth0 | /bin/busybox grep 'inet ' | /bin/busybox awk '{print $2}' | /bin/busybox cut -d/ -f1)
            echo "✅ Network configured: $IP_ADDR"
            # Send gratuitous ARP to announce our IP
            /bin/busybox arping -c 2 -A -I eth0 "$IP_ADDR" 2>/dev/null || true
            break
        fi
        echo "Waiting for IP assignment (attempt $i/5)..."
        /bin/busybox sleep 1
    done
    # Ensure resolv.conf has nameservers
    if [ ! -s /etc/resolv.conf ] || ! /bin/busybox grep -q nameserver /etc/resolv.conf; then
        echo "nameserver 8.8.8.8" > /etc/resolv.conf
        echo "nameserver 8.8.4.4" >> /etc/resolv.conf
    fi
fi
echo "System ready!"
exec /bin/busybox sh
`

	initPath := filepath.Join(mountDir, "init")
	if err := os.WriteFile(initPath+".tmp", []byte(initScript), 0755); err != nil {
		return fmt.Errorf("failed to write init script: %w", err)
	}

	cmd = exec.Command("sudo", "mv", initPath+".tmp", initPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to move init script: %w", err)
	}

	cmd = exec.Command("sudo", "chmod", "+x", initPath)
	cmd.Run()

	// Create basic passwd file
	passwdContent := "root:x:0:0:root:/root:/bin/sh\n"
	passwdPath := filepath.Join(mountDir, "etc", "passwd")
	if err := os.WriteFile(passwdPath+".tmp", []byte(passwdContent), 0644); err != nil {
		return fmt.Errorf("failed to write passwd: %w", err)
	}

	cmd = exec.Command("sudo", "mv", passwdPath+".tmp", passwdPath)
	cmd.Run()

	fmt.Printf("Working rootfs created successfully\n")
	return nil
}

// createVMSpecificRootfs creates a VM-specific rootfs with unique IP configuration
func (m *Manager) createVMSpecificRootfs(rootfsPath, staticIP string) error {
	fmt.Printf("Creating VM-specific rootfs at %s with IP %s\n", rootfsPath, staticIP)

	// Create a 50MB sparse file
	cmd := exec.Command("dd", "if=/dev/zero", "of="+rootfsPath, "bs=1M", "count=0", "seek=50")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create rootfs file: %w", err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", rootfsPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to format rootfs: %w", err)
	}

	// Create temporary mount directory
	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("rootfs-mount-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Mount the rootfs
	cmd = exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}
	defer func() {
		exec.Command("sudo", "umount", mountDir).Run()
	}()

	// Create directory structure
	dirs := []string{"bin", "sbin", "etc", "proc", "sys", "dev", "tmp", "var", "root"}
	for _, dir := range dirs {
		dirPath := filepath.Join(mountDir, dir)
		cmd = exec.Command("sudo", "mkdir", "-p", dirPath)
		cmd.Run()
	}

	// Copy static busybox
	cmd = exec.Command("sudo", "cp", "/usr/bin/busybox", filepath.Join(mountDir, "bin", "busybox"))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy busybox: %w", err)
	}

	// Create busybox symlinks
	busyboxCommands := []string{"sh", "ls", "cat", "echo", "mount", "umount", "ps", "kill", "ip", "ping", "udhcpc", "awk", "cut", "grep", "dropbear", "dropbearkey"}
	for _, cmdName := range busyboxCommands {
		linkPath := filepath.Join(mountDir, "bin", cmdName)
		cmd = exec.Command("sudo", "ln", "-sf", "busybox", linkPath)
		cmd.Run()
	}

	// Create additional symlinks for compatibility
	cmd = exec.Command("sudo", "ln", "-sf", "/bin/busybox", filepath.Join(mountDir, "bin", "init"))
	cmd.Run()

	// Copy dropbear SSH server if available
	if _, err := os.Stat("/usr/sbin/dropbear"); err == nil {
		cmd = exec.Command("sudo", "cp", "/usr/sbin/dropbear", filepath.Join(mountDir, "sbin", "dropbear"))
		cmd.Run()
		cmd = exec.Command("sudo", "cp", "/usr/bin/dropbearkey", filepath.Join(mountDir, "bin", "dropbearkey"))
		cmd.Run()
	}

	// Create init as a shell script that calls busybox
	initPath := filepath.Join(mountDir, "init")
	initScript := "#!/bin/busybox sh\nexec /etc/init.d/rcS\n"
	cmd = exec.Command("sudo", "tee", initPath)
	cmd.Stdin = strings.NewReader(initScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create init script: %w", err)
	}
	cmd = exec.Command("sudo", "chmod", "+x", initPath)
	cmd.Run()

	// Create startup script that will be called by busybox init
	startupScript := fmt.Sprintf(`#!/bin/sh
	echo "MicroVM starting..."
	/bin/busybox mount -t proc proc /proc
	/bin/busybox mount -t sysfs sysfs /sys
	/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
	# Mount devpts so PTYs work for SSH sessions
	if ! /bin/busybox grep -q devpts /proc/filesystems 2>/dev/null; then
		echo "devpts not listed in /proc/filesystems (kernel option?), attempting anyway"
	fi
	/bin/busybox mkdir -p /dev/pts
	mount -t devpts devpts /dev/pts 2>/dev/null || echo "Warning: devpts mount failed"
	ln -sf /dev/pts/ptmx /dev/ptmx
	echo "microvm" > /proc/sys/kernel/hostname

	# Create necessary directories
	/bin/busybox mkdir -p /var/log /var/run /root/.ssh /etc/dropbear

	# Set up loopback
	/bin/busybox ip link set lo up

	# Configure network interface
	if /bin/busybox ip link show eth0 >/dev/null 2>&1; then
		/bin/busybox ip link set eth0 up
		echo "Attempting DHCP configuration..."
		/bin/busybox udhcpc -i eth0 -n -q -t 10 2>/dev/null || {
			echo "DHCP failed, using static IP..."
			/bin/busybox ip addr add %s/24 dev eth0
			/bin/busybox ip route add default via 192.168.100.1
			echo "Network configured: %s (static)"
		}
		IP_ADDR=$(/bin/busybox ip addr show eth0 | /bin/busybox grep 'inet ' | /bin/busybox awk '{print $2}' | /bin/busybox cut -d/ -f1)
		echo "Network configured: $IP_ADDR"
	fi

	# Ensure /sbin/getty path exists (some init/inittab references) for silence
	/bin/busybox mkdir -p /sbin
	ln -sf /bin/busybox /sbin/getty

	# Start SSH server if dropbear is available
	if [ -f /sbin/dropbear ]; then
		echo "Starting SSH server..."
		if [ ! -f /etc/dropbear/dropbear_rsa_host_key ]; then
			/bin/dropbearkey -t rsa -f /etc/dropbear/dropbear_rsa_host_key
		fi
		# Do not disable PTY (-s removes password logins), keep foreground (-F)
		/sbin/dropbear -r /etc/dropbear/dropbear_rsa_host_key -p 22 -F -E 2>/var/log/dropbear.log &
		echo "SSH server started on port 22"
	fi

	echo "System ready!"
	echo "SSH access available"
	`, staticIP, staticIP)

	startupPath := filepath.Join(mountDir, "etc", "init.d", "rcS")
	cmd = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "init.d"))
	cmd.Run()

	// Create the startup script using sudo tee to ensure proper permissions
	cmd = exec.Command("sudo", "tee", startupPath)
	cmd.Stdin = strings.NewReader(startupScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write startup script: %w", err)
	}

	cmd = exec.Command("sudo", "chmod", "+x", startupPath)
	cmd.Run()

	// Create inittab for busybox init (use getty on ttyS0 for interactive console and ensure PTY)
	inittabContent := `::sysinit:/etc/init.d/rcS
	ttyS0::respawn:/bin/busybox getty -L ttyS0 115200 vt100
	::respawn:/bin/busybox sh
	::ctrlaltdel:/bin/busybox reboot
	::shutdown:/bin/busybox umount -a -r
	`
	inittabPath := filepath.Join(mountDir, "etc", "inittab")
	cmd = exec.Command("sudo", "tee", inittabPath)
	cmd.Stdin = strings.NewReader(inittabContent)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write inittab: %w", err)
	}

	// Create basic passwd file
	passwdContent := "root:x:0:0:root:/root:/bin/sh\n"
	passwdPath := filepath.Join(mountDir, "etc", "passwd")
	cmd = exec.Command("sudo", "tee", passwdPath)
	cmd.Stdin = strings.NewReader(passwdContent)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write passwd: %w", err)
	}

	// Set up SSH authorized keys
	sshDir := filepath.Join(mountDir, "root", ".ssh")
	cmd = exec.Command("sudo", "mkdir", "-p", sshDir)
	cmd.Run()

	// Copy SSH public key if it exists
	sshKeyPath := filepath.Join(m.workDir, "ssh-keys", "vm-key.pub")
	if _, err := os.Stat(sshKeyPath); err == nil {
		authorizedKeysPath := filepath.Join(sshDir, "authorized_keys")
		cmd = exec.Command("sudo", "cp", sshKeyPath, authorizedKeysPath)
		if err := cmd.Run(); err == nil {
			cmd = exec.Command("sudo", "chmod", "600", authorizedKeysPath)
			cmd.Run()
			cmd = exec.Command("sudo", "chown", "root:root", authorizedKeysPath)
			cmd.Run()
			fmt.Printf("SSH public key installed for root access\n")
		}
	}

	fmt.Printf("VM-specific rootfs created successfully with IP %s\n", staticIP)
	return nil
}

// setupConsoleLogging configures console output redirection to log files
// rotateConsoleLog moves an oversized console.log to console.log.1 (single
// generation) instead of truncating, so the web console keeps boot history.
// Small logs are left alone; missing files are created empty.
func rotateConsoleLog(consoleLogPath string) {
	const maxConsoleLogBytes = 50 * 1024 * 1024 // 50 MB

	st, err := os.Stat(consoleLogPath)
	if err != nil {
		_ = os.WriteFile(consoleLogPath, nil, 0644)
		return
	}
	if st.Size() < maxConsoleLogBytes {
		return
	}
	_ = os.Remove(consoleLogPath + ".1")
	if err := os.Rename(consoleLogPath, consoleLogPath+".1"); err != nil {
		// Fall back to truncate rather than failing the VM start.
		if f, terr := os.OpenFile(consoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); terr == nil {
			f.Close()
		}
		return
	}
	_ = os.WriteFile(consoleLogPath, nil, 0644)
}

// SweepVMLogs caps per-VM serial log files without restarting the guest.
//
// Firecracker and the kernel console append to console.log through a file
// descriptor held by systemd, so renaming the file would keep feeding the old
// inode and leave the fresh console.log empty (blank web console). Truncating in
// place is compatible with O_APPEND: the next write starts at offset 0 again.
//
// Returns the number of files rotated and the number of bytes reclaimed.
func (m *Manager) SweepVMLogs() (int, int64) {
	const (
		maxConsoleLogBytes     = 64 * 1024 * 1024
		maxFirecrackerLogBytes = 16 * 1024 * 1024
		keepTailBytes          = 1024 * 1024
	)

	microvmsDir := filepath.Join(m.workDir, "microvms")
	entries, err := os.ReadDir(microvmsDir)
	if err != nil {
		return 0, 0
	}

	rotated := 0
	var reclaimed int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, lf := range []struct {
			name string
			max  int64
		}{
			{"console.log", maxConsoleLogBytes},
			{"firecracker.log", maxFirecrackerLogBytes},
		} {
			path := filepath.Join(microvmsDir, entry.Name(), lf.name)
			n, err := capLogFile(path, lf.max, keepTailBytes)
			if err != nil {
				continue
			}
			if n > 0 {
				rotated++
				reclaimed += n
			}
		}
	}

	if rotated > 0 {
		fmt.Printf("🧹 Rotated %d oversized VM log file(s), reclaimed %.1f MB\n",
			rotated, float64(reclaimed)/(1024*1024))
	}
	return rotated, reclaimed
}

// capLogFile truncates path in place when it exceeds maxBytes, keeping the last
// keepTailBytes of content in path+".1". Returns bytes reclaimed.
func capLogFile(path string, maxBytes, keepTailBytes int64) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, nil
	}
	size := st.Size()
	if size <= maxBytes || size == 0 {
		return 0, nil
	}

	tailLen := keepTailBytes
	if tailLen > size {
		tailLen = size
	}
	tail := make([]byte, tailLen)
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	_, err = f.ReadAt(tail, size-tailLen)
	f.Close()
	if err != nil && err != io.EOF {
		return 0, err
	}
	if err := os.WriteFile(path+".1", tail, 0644); err != nil {
		return 0, err
	}
	if err := os.Truncate(path, 0); err != nil {
		return 0, err
	}
	return size - tailLen, nil
}

func (m *Manager) setupConsoleLogging(fcConfig map[string]interface{}, consoleLogPath string) error {
	// Rotate (never truncate): boot history must survive for the web console.
	rotateConsoleLog(consoleLogPath)

	// Update firecracker config to redirect console output
	bootSource, ok := fcConfig["boot-source"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid boot-source configuration")
	}

	// Update boot args to redirect console to file
	bootArgs, ok := bootSource["boot_args"].(string)
	if !ok {
		bootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/init rw"
	}

	// Ensure console output goes to ttyS0 (which we'll redirect)
	if !strings.Contains(bootArgs, "console=ttyS0") {
		bootArgs = "console=ttyS0 " + bootArgs
	}
	bootSource["boot_args"] = bootArgs

	// Add logger configuration to firecracker config
	fcConfig["logger"] = map[string]interface{}{
		"log_path":        filepath.Join(filepath.Dir(consoleLogPath), "firecracker.log"),
		"level":           LogLevel(),
		"show_level":      true,
		"show_log_origin": false,
	}

	return nil
}

// GetConsoleFIFOPath returns the path to the console.in FIFO for a VM.
func (m *Manager) GetConsoleFIFOPath(vmID string) string {
	return filepath.Join(m.workDir, "microvms", vmID, "console.in")
}

// ReopenConsoleFIFO re-opens an existing console.in FIFO with O_RDWR after a
// service restart (VMs keep running; we just need to reclaim the keeper fd).
func (m *Manager) ReopenConsoleFIFO(vmID string) {
	path := m.GetConsoleFIFOPath(vmID)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return // FIFO does not exist
	}
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("⚠️  Could not reopen console FIFO for VM %s: %v\n", vmID, err)
		return
	}
	m.consoleFIFOs.Store(vmID, fd)
	fmt.Printf("✅ Reopened console FIFO for running VM %s\n", vmID)
}

// GetConsoleLogPath returns the path to the console log file for a VM
func (m *Manager) GetConsoleLogPath(vmID string) string {
	return filepath.Join(m.workDir, "microvms", vmID, "console.log")
}

// extractConsoleOutput extracts VM console output from firecracker logs
func (m *Manager) extractConsoleOutput(vmID string) error {
	firecrackerLogPath := filepath.Join(m.workDir, "microvms", vmID, "firecracker.log")
	consoleLogPath := m.GetConsoleLogPath(vmID)

	// Read firecracker log
	firecrackerLog, err := os.ReadFile(firecrackerLogPath)
	if err != nil {
		return fmt.Errorf("failed to read firecracker log: %w", err)
	}

	// Extract console lines (lines that don't start with timestamp or firecracker prefixes)
	lines := strings.Split(string(firecrackerLog), "\n")
	var consoleLines []string

	for _, line := range lines {
		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Skip firecracker internal logs (lines starting with timestamp in brackets)
		if strings.HasPrefix(strings.TrimSpace(line), "[") && strings.Contains(line, "]") {
			// Check if it's a kernel/system message vs firecracker log
			if strings.Contains(line, "MicroVM starting") ||
				strings.Contains(line, "Network configured") ||
				strings.Contains(line, "System ready") ||
				strings.Contains(line, "Available commands") ||
				strings.Contains(line, "BusyBox") ||
				strings.Contains(line, "Enter 'help'") ||
				strings.Contains(line, "can't access tty") ||
				strings.Contains(line, "Attempting DHCP") ||
				strings.Contains(line, "DHCP failed") {
				consoleLines = append(consoleLines, line)
			}
			continue
		}

		// Include lines that look like console output
		if strings.Contains(line, "MicroVM starting") ||
			strings.Contains(line, "Network configured") ||
			strings.Contains(line, "System ready") ||
			strings.Contains(line, "Available commands") ||
			strings.Contains(line, "BusyBox") ||
			strings.Contains(line, "Enter 'help'") ||
			strings.Contains(line, "can't access tty") ||
			strings.Contains(line, "Attempting DHCP") ||
			strings.Contains(line, "DHCP failed") ||
			strings.HasPrefix(line, "/ #") || // Shell prompt
			strings.HasPrefix(line, "~ #") { // Shell prompt
			consoleLines = append(consoleLines, line)
		}
	}

	// Write console output to console log file
	consoleOutput := strings.Join(consoleLines, "\n")
	if len(consoleLines) > 0 {
		consoleOutput += "\n"
	}

	return os.WriteFile(consoleLogPath, []byte(consoleOutput), 0644)
}

// GetVMProcessInfo returns process information for a VM
func (m *Manager) GetVMProcessInfo(vmID string) (*models.ProcessInfo, error) {
	microvmDir := filepath.Join(m.workDir, "microvms", vmID)
	pidFile := filepath.Join(microvmDir, "firecracker.pid")
	configPath := filepath.Join(microvmDir, "config.json")
	socketPath := m.socketPathFor(vmID)

	// Read PID from file
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read PID file: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return nil, fmt.Errorf("failed to parse PID: %w", err)
	}

	// Get process start time (approximate)
	var startTime time.Time
	if stat, err := os.Stat(pidFile); err == nil {
		startTime = stat.ModTime()
	} else {
		startTime = time.Now()
	}

	return &models.ProcessInfo{
		PID:        pid,
		StartTime:  startTime,
		ConfigPath: configPath,
		SocketPath: socketPath,
	}, nil
}

// UpdateVMProcessInfo updates the process information for a VM after starting
func (m *Manager) UpdateVMProcessInfo(microvm *models.MicroVMInstance) error {
	if processInfo, err := m.GetVMProcessInfo(microvm.ID); err == nil {
		microvm.ProcessInfo = processInfo
		return nil
	} else {
		return err
	}
}

// DetectRunningVMs detects VMs that are currently running by checking processes
func (m *Manager) DetectRunningVMs() ([]string, error) {
	var runningVMs []string

	// Check microvms directory for VM directories
	microvmsDir := filepath.Join(m.workDir, "microvms")
	entries, err := os.ReadDir(microvmsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read microvms directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			vmID := entry.Name()
			if m.IsVMProcessRunning(vmID) {
				runningVMs = append(runningVMs, vmID)
			}
		}
	}

	return runningVMs, nil
}

// UpdateVMResources updates the resource allocation for a running VM
func (m *Manager) UpdateVMResources(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	// Check if VM is running
	if microvm.Status != models.MicroVMStatusRunning {
		// For stopped VMs, just update the configuration - no restart required
		return m.updateVMConfiguration(microvm)
	}

	// For running VMs, try to apply changes dynamically
	socketPath := m.socketPathFor(microvm.ID)

	// Check if socket exists and is accessible
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return fmt.Errorf("VM is not accessible via API socket")
	}

	// Try to update resources via Firecracker API
	if err := m.updateVMResourcesViaAPI(ctx, microvm, socketPath); err != nil {
		// If dynamic update fails, check if it's a restart-required change
		if m.requiresRestart(microvm) {
			return fmt.Errorf("resource update requires VM restart: %w", err)
		}
		return fmt.Errorf("failed to update VM resources: %w", err)
	}

	// Update the VM configuration file for persistence
	if err := m.updateVMConfiguration(microvm); err != nil {
		fmt.Printf("Warning: failed to update VM configuration file: %v\n", err)
	}

	return nil
}

// updateVMResourcesViaAPI updates VM resources using the Firecracker API
func (m *Manager) updateVMResourcesViaAPI(ctx context.Context, microvm *models.MicroVMInstance, socketPath string) error {
	// Check what resources changed and determine if they can be updated dynamically
	configPath := filepath.Join(m.workDir, "microvms", microvm.ID, "config.json")

	// Read current configuration
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read VM configuration: %w", err)
	}

	var currentConfig map[string]interface{}
	if err := json.Unmarshal(configData, &currentConfig); err != nil {
		return fmt.Errorf("failed to parse VM configuration: %w", err)
	}

	// Check current machine config
	machineConfig, ok := currentConfig["machine-config"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid machine configuration")
	}

	currentVCPUs := uint32(machineConfig["vcpu_count"].(float64))
	currentMemoryMB := uint32(machineConfig["mem_size_mib"].(float64))

	// Get current storage size from ZFS zvol or config
	var currentStorageMB uint32
	if m.storageBackend != nil {
		// Get actual size from ZFS zvol
		diskPath := m.storageBackend.GetDiskPath(microvm.ID)
		if strings.HasPrefix(diskPath, "/dev/zvol/") {
			// zfs needs the dataset name, not the /dev/zvol device path
			zvolName := strings.TrimPrefix(diskPath, "/dev/zvol/")
			// Query ZFS for current volsize in exact bytes (-p prints raw bytes, no humanized units)
			cmd := exec.Command("zfs", "get", "-Hp", "-o", "value", "volsize", zvolName)
			if output, err := cmd.Output(); err == nil {
				if bytes, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64); err == nil {
					currentStorageMB = uint32(bytes / (1024 * 1024))
				}
			}
		}
	}

	// Fallback: read from config if ZFS query failed
	if currentStorageMB == 0 {
		// Cannot determine the real disk size; resizing blindly is destructive, so skip it
		fmt.Printf("⚠️  Could not determine current disk size for VM %s; skipping disk resize\n", microvm.ID)
	}

	var updates []string
	var requiresRestart bool

	// 1. Check vCPU changes (REQUIRES RESTART - Firecracker limitation)
	if microvm.Resources.VCPUs != currentVCPUs {
		requiresRestart = true
		updates = append(updates, fmt.Sprintf("vCPU: %d -> %d (requires restart)", currentVCPUs, microvm.Resources.VCPUs))
	}

	// 2. Handle memory changes via balloon device (CAN BE DONE ONLINE)
	if microvm.Resources.MemoryMB != currentMemoryMB {
		// Firecracker balloon sets the amount to *inflate* (memory to reclaim from guest)
		// If we want to increase memory, we deflate (reduce balloon size)
		// If we want to decrease memory, we inflate (increase balloon size)
		// For simplicity, we'll treat memory changes as requiring restart for now
		// TODO: Implement proper balloon inflation/deflation logic
		fmt.Printf("⚠️  Memory changes (%d MB -> %d MB) require VM restart\n", currentMemoryMB, microvm.Resources.MemoryMB)
		requiresRestart = true
		updates = append(updates, fmt.Sprintf("memory: %d MB -> %d MB (requires restart)", currentMemoryMB, microvm.Resources.MemoryMB))
	}

	// 3. Handle disk resize via storage backend (grow-only; ZFS can resize online)
	if currentStorageMB > 0 && microvm.Resources.StorageMB != currentStorageMB {
		if m.storageBackend != nil {
			if microvm.Resources.StorageMB < currentStorageMB {
				fmt.Printf("⚠️  Refusing to shrink disk for VM %s: %d MB -> %d MB (shrink is destructive, keeping current size)\n", microvm.ID, currentStorageMB, microvm.Resources.StorageMB)
				updates = append(updates, fmt.Sprintf("storage: %d MB -> %d MB (shrink refused, kept current size)", currentStorageMB, microvm.Resources.StorageMB))
			} else if err := m.storageBackend.ResizeDisk(microvm.ID, int(microvm.Resources.StorageMB)); err != nil {
				fmt.Printf("⚠️  Disk resize failed: %v\n", err)
				requiresRestart = true
				updates = append(updates, fmt.Sprintf("storage: %d MB -> %d MB (resize failed, requires restart)", currentStorageMB, microvm.Resources.StorageMB))
			} else {
				updates = append(updates, fmt.Sprintf("storage: %d MB -> %d MB (resized online)", currentStorageMB, microvm.Resources.StorageMB))
			}
		} else {
			// No storage backend - requires restart
			requiresRestart = true
			updates = append(updates, fmt.Sprintf("storage: %d MB -> %d MB (requires restart)", currentStorageMB, microvm.Resources.StorageMB))
		}
	}

	if len(updates) > 0 {
		fmt.Printf("📊 Resource updates for VM %s:\n", microvm.ID)
		for _, update := range updates {
			fmt.Printf("   - %s\n", update)
		}
	}

	if requiresRestart {
		return fmt.Errorf("some resource changes require VM restart")
	}

	return nil
}

// updateMemoryBalloon updates VM memory via Firecracker balloon device
func (m *Manager) updateMemoryBalloon(socketPath string, targetMemoryMB uint32) error {
	// Create HTTP client for Unix socket
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	// Prepare balloon update request
	payload := map[string]interface{}{
		"amount_mib": targetMemoryMB,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal balloon payload: %w", err)
	}

	// Send PATCH request to Firecracker balloon API
	req, err := http.NewRequest("PATCH", "http://localhost/balloon", bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create balloon request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send balloon request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("balloon API returned status %d: %s", resp.StatusCode, string(body))
	}

	fmt.Printf("✅ Updated memory balloon to %d MB\n", targetMemoryMB)
	return nil
}

// updateVMConfiguration updates the VM configuration file with new resource specifications
func (m *Manager) updateVMConfiguration(microvm *models.MicroVMInstance) error {
	configPath := filepath.Join(m.workDir, "microvms", microvm.ID, "config.json")

	// Read current configuration
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read VM configuration: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return fmt.Errorf("failed to parse VM configuration: %w", err)
	}

	// Update machine configuration
	machineConfig, ok := config["machine-config"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid machine configuration")
	}

	machineConfig["vcpu_count"] = microvm.Resources.VCPUs
	machineConfig["mem_size_mib"] = microvm.Resources.MemoryMB

	// Update storage configuration if needed
	drives, ok := config["drives"].([]interface{})
	if ok && len(drives) > 0 {
		// Update root drive size (this is informational as the actual resize would need filesystem operations)
		for _, drive := range drives {
			if driveMap, ok := drive.(map[string]interface{}); ok {
				if isRoot, exists := driveMap["is_root_device"]; exists && isRoot.(bool) {
					// Note: Actual storage resize would require more complex operations
					// including filesystem resize, which is beyond the scope of this basic implementation
					break
				}
			}
		}
	}

	// Write updated configuration
	updatedConfigData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated configuration: %w", err)
	}

	if err := os.WriteFile(configPath, updatedConfigData, 0644); err != nil {
		return fmt.Errorf("failed to write updated configuration: %w", err)
	}

	return nil
}

// requiresRestart determines if resource changes require a VM restart
func (m *Manager) requiresRestart(microvm *models.MicroVMInstance) bool {
	// For this basic implementation, most resource changes require restart
	// In a production system, you would have more sophisticated logic here
	return true
}

// createTAPInterfaceWithRetry creates a TAP interface with retry logic and enhanced error handling
func (m *Manager) createTAPInterfaceWithRetry(ctx context.Context, tapName string) error {
	return m.errorHandler.RetryWithBackoff(ctx, func() error {
		// First, validate system prerequisites
		if err := m.validateNetworkPrerequisites(); err != nil {
			return fmt.Errorf("network prerequisites not met: %w", err)
		}

		// Check if interface already exists and clean it up if necessary
		if err := exec.Command("ip", "link", "show", tapName).Run(); err == nil {
			// Interface exists, try to remove it first
			if cleanupErr := m.networkManager.removeTAPInterface(tapName); cleanupErr != nil {
				return fmt.Errorf("failed to cleanup existing TAP interface: %w", cleanupErr)
			}
		}

		// Create the TAP interface
		if err := m.networkManager.createTAPInterface(tapName); err != nil {
			// Collect diagnostics on failure
			if diagnostics, diagErr := m.errorHandler.CollectDiagnostics(ctx, "network_interface_creation", tapName, err, 0); diagErr == nil {
				m.errorHandler.StoreDiagnostics(diagnostics)
			}
			return fmt.Errorf("TAP interface creation failed: %w", err)
		}

		return nil
	}, "network_interface_creation")
}

// attachTAPToBridgeWithRetry attaches TAP interface to bridge with retry logic
func (m *Manager) attachTAPToBridgeWithRetry(ctx context.Context, tapName, bridgeName string) error {
	return m.errorHandler.RetryWithBackoff(ctx, func() error {
		// Ensure bridge exists
		if err := m.ensureBridgeExists(bridgeName); err != nil {
			return fmt.Errorf("failed to ensure bridge exists: %w", err)
		}

		// Attach TAP to bridge
		if err := m.networkManager.addTAPToBridge(tapName, bridgeName); err != nil {
			// Collect diagnostics on failure
			if diagnostics, diagErr := m.errorHandler.CollectDiagnostics(ctx, "network_interface_creation", tapName, err, 0); diagErr == nil {
				m.errorHandler.StoreDiagnostics(diagnostics)
			}
			return fmt.Errorf("TAP bridge attachment failed: %w", err)
		}

		return nil
	}, "network_interface_creation")
}

// validateNetworkPrerequisites validates that network prerequisites are met
func (m *Manager) validateNetworkPrerequisites() error {
	// Check if required network tools are available
	requiredTools := []string{"ip", "brctl", "iptables"}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required network tool '%s' not found: %w", tool, err)
		}
	}

	// Check if TUN/TAP module is loaded
	if err := exec.Command("lsmod").Run(); err == nil {
		output, _ := exec.Command("lsmod").Output()
		if !strings.Contains(string(output), "tun") {
			// Try to load the module
			if err := exec.Command("sudo", "modprobe", "tun").Run(); err != nil {
				return fmt.Errorf("TUN/TAP kernel module not available: %w", err)
			}
		}
	}

	// Check if /dev/net/tun exists
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		return fmt.Errorf("/dev/net/tun device not found")
	}

	return nil
}

// ensureBridgeExists ensures that the specified bridge exists and is properly configured
func (m *Manager) ensureBridgeExists(bridgeName string) error {
	// Check if bridge exists
	if err := exec.Command("ip", "link", "show", bridgeName).Run(); err == nil {
		return nil // Bridge already exists
	}

	// Create bridge
	if err := exec.Command("sudo", "ip", "link", "add", "name", bridgeName, "type", "bridge").Run(); err != nil {
		return fmt.Errorf("failed to create bridge %s: %w", bridgeName, err)
	}

	// Assign IP to bridge
	bridgeIP := m.config.NetworkBridgeIP
	if bridgeIP == "" {
		bridgeIP = "192.168.100.1"
	}
	bridgeCIDR := m.config.NetworkBridgeCIDR
	if bridgeCIDR == "" {
		bridgeCIDR = "192.168.100.0/24"
	}

	// Extract CIDR from bridge IP if not provided separately
	if !strings.Contains(bridgeIP, "/") {
		bridgeIP = bridgeIP + "/24"
	}

	if err := exec.Command("sudo", "ip", "addr", "add", bridgeIP, "dev", bridgeName).Run(); err != nil {
		// Ignore error if address already exists
		if !strings.Contains(err.Error(), "File exists") {
			return fmt.Errorf("failed to assign IP to bridge %s: %w", bridgeName, err)
		}
	}

	// Bring bridge up
	if err := exec.Command("sudo", "ip", "link", "set", "dev", bridgeName, "up").Run(); err != nil {
		return fmt.Errorf("failed to bring bridge %s up: %w", bridgeName, err)
	}

	// Configure NAT if required
	if m.config.ManageNAT {
		if err := m.configureNATForBridge(bridgeName, bridgeCIDR); err != nil {
			return fmt.Errorf("failed to configure NAT for bridge %s: %w", bridgeName, err)
		}
	}

	return nil
}

// configureNATForBridge configures NAT rules for the bridge
func (m *Manager) configureNATForBridge(bridgeName, bridgeCIDR string) error {
	// Enable IP forwarding
	if err := exec.Command("sudo", "sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Add masquerading rule
	if err := exec.Command("sudo", "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", bridgeCIDR, "-j", "MASQUERADE").Run(); err != nil {
		// Ignore error if rule already exists
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to add masquerading rule: %w", err)
		}
	}

	return nil
}

// PerformSystemHealthCheck performs comprehensive system health checks
func (m *Manager) PerformSystemHealthCheck() (*SystemHealthStatus, error) {
	health := &SystemHealthStatus{
		Overall:         "unknown",
		Components:      make(map[string]ComponentHealth),
		CheckedAt:       time.Now(),
		Issues:          []string{},
		Recommendations: []string{},
	}

	// Check system validator
	if validationResult, err := m.systemValidator.ValidateSystem(); err == nil {
		health.Components["system_validation"] = ComponentHealth{
			Status:  boolToHealthStatus(validationResult.Overall),
			Details: validationResult,
		}
		if !validationResult.Overall {
			for _, issue := range validationResult.Issues {
				health.Issues = append(health.Issues, issue.Issue)
			}
			health.Recommendations = append(health.Recommendations, validationResult.Recommendations...)
		}
	} else {
		health.Components["system_validation"] = ComponentHealth{
			Status:  "unhealthy",
			Details: map[string]interface{}{"error": err.Error()},
		}
		health.Issues = append(health.Issues, fmt.Sprintf("System validation failed: %v", err))
	}

	// Check firecracker binary
	if _, err := exec.LookPath(m.firecrackerBin); err == nil {
		health.Components["firecracker_binary"] = ComponentHealth{
			Status:  "healthy",
			Details: map[string]interface{}{"path": m.firecrackerBin},
		}
	} else {
		health.Components["firecracker_binary"] = ComponentHealth{
			Status:  "unhealthy",
			Details: map[string]interface{}{"error": err.Error()},
		}
		health.Issues = append(health.Issues, fmt.Sprintf("Firecracker binary not found: %v", err))
	}

	// Check network bridge
	bridgeName := m.config.NetworkBridgeName
	if bridgeName == "" {
		bridgeName = "fc-br0"
	}
	if err := exec.Command("ip", "link", "show", bridgeName).Run(); err == nil {
		health.Components["network_bridge"] = ComponentHealth{
			Status:  "healthy",
			Details: map[string]interface{}{"bridge": bridgeName},
		}
	} else {
		health.Components["network_bridge"] = ComponentHealth{
			Status:  "unhealthy",
			Details: map[string]interface{}{"error": err.Error()},
		}
		health.Issues = append(health.Issues, fmt.Sprintf("Network bridge %s not found", bridgeName))
		health.Recommendations = append(health.Recommendations, fmt.Sprintf("Create network bridge: sudo ip link add name %s type bridge", bridgeName))
	}

	// Check work directory
	if stat, err := os.Stat(m.workDir); err == nil && stat.IsDir() {
		health.Components["work_directory"] = ComponentHealth{
			Status:  "healthy",
			Details: map[string]interface{}{"path": m.workDir},
		}
	} else {
		health.Components["work_directory"] = ComponentHealth{
			Status:  "unhealthy",
			Details: map[string]interface{}{"error": err.Error()},
		}
		health.Issues = append(health.Issues, fmt.Sprintf("Work directory %s not accessible", m.workDir))
	}

	// Determine overall health
	healthyComponents := 0
	totalComponents := len(health.Components)

	for _, component := range health.Components {
		if component.Status == "healthy" {
			healthyComponents++
		}
	}

	if healthyComponents == totalComponents {
		health.Overall = "healthy"
	} else if healthyComponents > totalComponents/2 {
		health.Overall = "degraded"
	} else {
		health.Overall = "unhealthy"
	}

	return health, nil
}

// SystemHealthStatus represents the overall system health
type SystemHealthStatus struct {
	Overall         string                     `json:"overall"`
	Components      map[string]ComponentHealth `json:"components"`
	CheckedAt       time.Time                  `json:"checked_at"`
	Issues          []string                   `json:"issues"`
	Recommendations []string                   `json:"recommendations"`
}

// ComponentHealth represents the health of a specific component
type ComponentHealth struct {
	Status  string      `json:"status"`
	Details interface{} `json:"details"`
}

func boolToHealthStatus(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// prepareVMImageWithSize prepares a VM image with the specified disk size.
// forceFresh=true (reinstall) skips the persistence fast-path and always
// provisions a brand-new disk from the base image — a reinstall that reuses
// a leftover zvol keeps the old OS and the old cloud-init instance state, so
// the freshly seeded password never reaches the guest.
func (m *Manager) prepareVMImageWithSize(imageID, vmID, staticIP, sshPublicKey string, storageMB uint32, username, password string, forceFresh bool) (string, error) {
	// Fast path: persistence reuse (only for reboot scenario). We look for an existing rootfs.
	// This complements caller logic that sets reuse_existing_rootfs label.
	if !forceFresh && os.Getenv("MICROVM_FORCE_RECREATE") != "true" { // allow override via env
		// Check ZFS zvol first (most common in production)
		if os.Getenv("MICROVM_USE_ZFS") == "true" && m.storageBackend != nil {
			zfsPool := os.Getenv("MICROVM_ZFS_POOL")
			if zfsPool == "" {
				zfsPool = "default"
			}
			zfsDataset := os.Getenv("MICROVM_ZFS_DATASET")
			if zfsDataset == "" {
				zfsDataset = fmt.Sprintf("%s/vmlet-microvms", zfsPool)
			}
			zvolPath := fmt.Sprintf("/dev/zvol/%s/%s", zfsDataset, vmID)
			if st, err := os.Stat(zvolPath); err == nil && st.Mode()&os.ModeDevice != 0 {
				fmt.Printf("🔁 Persistence fast-path: existing ZFS zvol detected for %s at %s\n", vmID, zvolPath)
				return zvolPath, nil
			}
		}
		// Fallback: check for file-based rootfs.ext4
		persistLabelPath := filepath.Join(m.workDir, "microvms", vmID, "rootfs.ext4")
		if st, err := os.Stat(persistLabelPath); err == nil && !st.IsDir() {
			fmt.Printf("🔁 Persistence fast-path: existing rootfs detected for %s at %s (size=%d bytes)\n", vmID, persistLabelPath, st.Size())
			return persistLabelPath, nil
		}
	}
	// Convert MB to GB for storage backend
	sizeGB := int(storageMB / 1024)
	if sizeGB < 1 {
		sizeGB = 1
	}

	// Check if we should use storage backend (ZFS or enhanced file storage)
	useStorageBackend := os.Getenv("MICROVM_USE_ZFS") == "true"

	if useStorageBackend && m.storageBackend != nil {
		fmt.Printf("📦 Using ZFS storage backend for VM %s\n", vmID)

		// Create disk via storage backend (ZFS zvol)
		diskPath, err := m.storageBackend.CreateDisk(vmID, sizeGB)
		if err != nil {
			return "", fmt.Errorf("failed to create ZFS disk: %w", err)
		}

		fmt.Printf("✅ ZFS zvol created: %s (%dGB)\n", diskPath, sizeGB)

		// Get base image path from environment or default location
		imagesDir := os.Getenv("MICROVM_IMAGES_DIR")
		if imagesDir == "" {
			imagesDir = filepath.Join(m.workDir, "images")
		}

		// Resolve the base image through the shared catalog. This keeps the
		// ZFS path in sync with the image manager and handles non-standard
		// filenames such as routers and VDI images.
		baseImagePath, err := images.ResolveImagePath(imagesDir, imageID)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(baseImagePath); err != nil {
			return "", fmt.Errorf("base image not found: %s", baseImagePath)
		}

		// Copy base image to ZFS zvol
		fmt.Printf("📋 Copying base image to ZFS zvol...\n")
		cmd := exec.Command("sudo", "dd", fmt.Sprintf("if=%s", baseImagePath), fmt.Sprintf("of=%s", diskPath), "bs=4M", "conv=sparse", "status=progress")
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to copy image to zvol: %w\nOutput: %s", err, string(output))
		}

		// Check and repair filesystem before resize
		fmt.Printf("🔍 Checking filesystem integrity...\n")
		if err := exec.Command("sudo", "e2fsck", "-fy", diskPath).Run(); err != nil {
			fmt.Printf("⚠️  Warning: e2fsck reported issues: %v\n", err)
		}

		// Resize filesystem to fill the zvol
		fmt.Printf("📏 Resizing filesystem...\n")
		if err := exec.Command("sudo", "resize2fs", diskPath).Run(); err != nil {
			fmt.Printf("⚠️  Warning: failed to resize filesystem: %v\n", err)
		}

		fmt.Printf("✅ ZFS disk ready: %s\n", diskPath)
		return diskPath, nil
	}

	// Legacy path: use local file storage
	// For Alpine, create custom rootfs with specified size
	if imageID == "alpine-3.18" {
		vmRootfs := filepath.Join(m.workDir, fmt.Sprintf("rootfs-%s.img", vmID))
		return vmRootfs, m.createVMSpecificRootfsWithSize(vmRootfs, staticIP, storageMB, username, password)
	}

	// For other images, use the image manager
	return m.imageManager.PrepareImageForVM(imageID, vmID, staticIP, sshPublicKey, storageMB, username, password)
}

// cloudInitSSHKeys returns every SSH key stored for a VM.
//
// `cloudinit_sshkeys` is a JSON array written by the HTTP API. Older VMs only
// have `cloudinit_sshkey` (the first key of the create/reinstall request), so
// that label remains the fallback.
func cloudInitSSHKeys(labels map[string]string) []string {
	if labels == nil {
		return nil
	}
	var keys []string
	if raw := labels["cloudinit_sshkeys"]; raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			keys = parsed
		}
	}
	if len(keys) == 0 {
		if key := strings.TrimSpace(labels["cloudinit_sshkey"]); key != "" {
			keys = []string{key}
		}
	}
	cleaned := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, key)
	}
	return cleaned
}

// generateCloudInitISO generates a cloud-init ISO for the VM
func (m *Manager) generateCloudInitISO(microvm *models.MicroVMInstance, staticIP string) (string, error) {
	// Check if cloud-init metadata exists
	if microvm.Metadata.Labels == nil {
		return "", nil // No cloud-init config
	}

	hostname, hasHostname := microvm.Metadata.Labels["cloudinit_hostname"]
	username, hasUsername := microvm.Metadata.Labels["cloudinit_username"]
	sshKeys := cloudInitSSHKeys(microvm.Metadata.Labels)

	// Skip if no cloud-init config is provided
	if !hasHostname && !hasUsername && len(sshKeys) == 0 {
		return "", nil
	}

	// Set defaults if not provided
	if hostname == "" {
		hostname = microvm.ID
	}
	// The label is customer input; seed a valid hostname, not the raw name.
	hostname = cloudinit.SanitizeHostname(hostname, microvm.ID)
	if username == "" {
		username = "ubuntu"
	}

	// Prepare cloud-init configuration
	config := &cloudinit.Config{
		Hostname:   hostname,
		Username:   username,
		StaticIP:   staticIP,
		Gateway:    "192.168.100.1",
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		MACAddress: microvm.Network.MACAddress, // Pass MAC for interface matching
	}

	// Add IPv6 configuration if available
	if microvm.Network.IPv6Address != "" {
		config.StaticIPv6 = microvm.Network.IPv6Address
		config.IPv6Gateway = microvm.Network.IPv6Gateway
		if microvm.Network.IPv6PrefixLength > 0 {
			config.IPv6Prefix = microvm.Network.IPv6PrefixLength
		} else {
			config.IPv6Prefix = 64
		}
		// Add IPv6 DNS if not present
		if len(config.DNSServers) == 2 {
			config.DNSServers = append(config.DNSServers, "2001:4860:4860::8888")
		}
	}

	// Hint distro for correct network-config rendering (Ubuntu uses netplan v2; Debian/Alpine use ENI v1)
	if microvm.Metadata.Labels != nil {
		if imgID, ok := microvm.Metadata.Labels["image_id"]; ok {
			lower := strings.ToLower(imgID)
			switch {
			case strings.HasPrefix(lower, "ubuntu-"):
				config.Distro = "ubuntu"
			case strings.HasPrefix(lower, "debian-"):
				config.Distro = "debian"
			case strings.HasPrefix(lower, "alpine-"):
				config.Distro = "alpine"
			}
		}
	}

	// Incorporate password (plaintext or hash) from labels if present
	if microvm.Metadata.Labels != nil {
		if ph, ok := microvm.Metadata.Labels["cloudinit_password_hash"]; ok && ph != "" {
			config.PasswordHash = ph
		} else if pw, ok := microvm.Metadata.Labels["cloudinit_password"]; ok && pw != "" {
			config.Password = pw
		}
	}
	// Add every SSH key, not just the legacy first one.
	if len(sshKeys) > 0 {
		config.SSHKeys = sshKeys
	}

	// Optional write_files and run_commands
	// Parse JSON-encoded labels if present
	if jsonStr, ok := microvm.Metadata.Labels["cloudinit_write_files"]; ok && jsonStr != "" {
		var wf map[string]string
		if err := json.Unmarshal([]byte(jsonStr), &wf); err == nil {
			config.WriteFiles = wf
		}
	}
	if jsonStr, ok := microvm.Metadata.Labels["cloudinit_run_commands"]; ok && jsonStr != "" {
		var rc []string
		if err := json.Unmarshal([]byte(jsonStr), &rc); err == nil {
			config.RunCommands = rc
		}
	}

	// Create cloud-init ISO path
	cloudInitPath := filepath.Join(m.workDir, fmt.Sprintf("cloudinit-%s.iso", microvm.ID))

	// Generate the ISO
	if err := cloudinit.GenerateCloudInitISO(config, cloudInitPath); err != nil {
		return "", fmt.Errorf("failed to generate cloud-init ISO: %w", err)
	}

	return cloudInitPath, nil
}

// createVMSpecificRootfsWithSize creates a VM-specific rootfs with the specified size
func (m *Manager) createVMSpecificRootfsWithSize(rootfsPath, staticIP string, sizeMB uint32, username, password string) error {
	// Use defaults if not provided
	if username == "" {
		username = "demo"
	}
	if password == "" {
		password = "vmlet-demo-pw"
	}

	fmt.Printf("Creating VM-specific rootfs at %s with IP %s (size: %dMB)\n", rootfsPath, staticIP, sizeMB)

	// Create sparse file with the specified size
	cmd := exec.Command("dd", "if=/dev/zero", "of="+rootfsPath, "bs=1M", "count=0", fmt.Sprintf("seek=%d", sizeMB))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create rootfs file: %w", err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", rootfsPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to format rootfs: %w", err)
	}

	// Create temporary mount directory
	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("rootfs-mount-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Mount the rootfs
	cmd = exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}
	defer func() {
		exec.Command("sudo", "umount", mountDir).Run()
	}()

	// Try to populate the rootfs from official Alpine minirootfs to ensure apk is fully functional
	func() {
		candidates := []string{
			"https://dl-cdn.alpinelinux.org/alpine/v3.18/releases/x86_64/alpine-minirootfs-3.18.6-x86_64.tar.gz",
			"https://dl-cdn.alpinelinux.org/alpine/v3.18/releases/x86_64/alpine-minirootfs-3.18.5-x86_64.tar.gz",
			"https://dl-cdn.alpinelinux.org/alpine/v3.18/releases/x86_64/alpine-minirootfs-3.18.4-x86_64.tar.gz",
			"https://dl-cdn.alpinelinux.org/alpine/v3.18/releases/x86_64/alpine-minirootfs-3.18.0-x86_64.tar.gz",
		}
		tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("alpine-mini-%d", time.Now().UnixNano()))
		_ = os.MkdirAll(tmpDir, 0755)
		defer os.RemoveAll(tmpDir)
		for _, url := range candidates {
			archive := filepath.Join(tmpDir, filepath.Base(url))
			// Download
			resp, err := http.Get(url)
			if err != nil {
				continue
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				continue
			}
			f, err := os.Create(archive)
			if err != nil {
				resp.Body.Close()
				continue
			}
			if _, err := io.Copy(f, resp.Body); err != nil {
				f.Close()
				resp.Body.Close()
				continue
			}
			f.Close()
			resp.Body.Close()
			// Extract into mountDir (requires sudo)
			if err := exec.Command("sudo", "tar", "-xzf", archive, "-C", mountDir).Run(); err != nil {
				continue
			}
			fmt.Printf("✓ Extracted Alpine minirootfs from %s\n", url)
			break
		}
	}()

	// Create directory structure
	dirs := []string{"bin", "sbin", "etc", "proc", "sys", "dev", "tmp", "var", "root", "usr/bin", "usr/sbin", "var/lib/apk", "var/cache/apk"}
	for _, dir := range dirs {
		dirPath := filepath.Join(mountDir, dir)
		cmd = exec.Command("sudo", "mkdir", "-p", dirPath)
		cmd.Run()
	}

	// Copy static busybox
	cmd = exec.Command("sudo", "cp", "/usr/bin/busybox", filepath.Join(mountDir, "bin", "busybox"))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy busybox: %w", err)
	}

	// Create busybox symlinks with proper error checking (add wget/tar for apk bootstrap). Some host busybox builds may omit these applets; we'll still create symlinks for consistency.
	busyboxCommands := []string{"sh", "ls", "cat", "echo", "mount", "umount", "ps", "kill", "ip", "ping", "udhcpc", "awk", "cut", "grep", "vi", "top", "free", "df", "wget", "tar"}
	for _, cmdName := range busyboxCommands {
		linkPath := filepath.Join(mountDir, "bin", cmdName)
		cmd = exec.Command("sudo", "ln", "-sf", "busybox", linkPath)
		if err := cmd.Run(); err != nil {
			fmt.Printf("Warning: failed to create symlink for %s: %v\n", cmdName, err)
		}
	}
	// Force-create wget/tar symlinks even if earlier attempt failed (avoid missing due to host busybox config)
	_ = exec.Command("sudo", "ln", "-sf", "busybox", filepath.Join(mountDir, "bin", "wget")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "busybox", filepath.Join(mountDir, "bin", "tar")).Run()

	// Ensure /bin/sh exists and is executable
	shPath := filepath.Join(mountDir, "bin", "sh")
	cmd = exec.Command("sudo", "test", "-x", shPath)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Creating /bin/sh symlink manually...\n")
		cmd = exec.Command("sudo", "ln", "-sf", "busybox", shPath)
		cmd.Run()
	}

	// Copy dropbear SSH server and its dependencies if available
	if _, err := os.Stat("/usr/sbin/dropbear"); err == nil {
		cmd = exec.Command("sudo", "cp", "/usr/sbin/dropbear", filepath.Join(mountDir, "sbin", "dropbear"))
		cmd.Run()
		cmd = exec.Command("sudo", "cp", "/usr/bin/dropbearkey", filepath.Join(mountDir, "bin", "dropbearkey"))
		cmd.Run()

		// Copy required shared libraries for dropbear
		libs := []string{
			"/lib/x86_64-linux-gnu/libtomcrypt.so.1",
			"/lib/x86_64-linux-gnu/libtommath.so.1",
			"/lib/x86_64-linux-gnu/libz.so.1",
			"/lib/x86_64-linux-gnu/libcrypt.so.1",
			"/lib/x86_64-linux-gnu/libc.so.6",
			"/lib/x86_64-linux-gnu/libgmp.so.10",
			"/lib64/ld-linux-x86-64.so.2",
		}

		// Create library directories
		cmd = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "lib64"), filepath.Join(mountDir, "lib", "x86_64-linux-gnu"))
		cmd.Run()

		// Copy each library
		for _, lib := range libs {
			if _, err := os.Stat(lib); err == nil {
				if strings.Contains(lib, "/lib64/") {
					cmd = exec.Command("sudo", "cp", lib, filepath.Join(mountDir, "lib64", filepath.Base(lib)))
				} else {
					cmd = exec.Command("sudo", "cp", lib, filepath.Join(mountDir, "lib", "x86_64-linux-gnu", filepath.Base(lib)))
				}
				cmd.Run()
			}
		}
	}

	// Bootstrap apk (Alpine package manager) using internal downloader/extractor (no dependency on host wget/tar).
	apkTarget := filepath.Join(mountDir, "bin", "apk")
	apkURL := "https://dl-cdn.alpinelinux.org/alpine/v3.18/main/x86_64/apk-tools-static-2.14.4-r0.apk"
	start := time.Now()
	if err := m.fetchAndInstallApkStatic(apkURL, apkTarget); err != nil {
		fmt.Printf("Warning: apk bootstrap skipped: %v\n", err)
	} else {
		// Verify installation and report size
		if fi, statErr := os.Stat(apkTarget); statErr == nil {
			fmt.Printf("✓ Embedded apk-tools-static (%s) at %s (size: %d bytes, took %s)\n", filepath.Base(apkURL), apkTarget, fi.Size(), time.Since(start))
		} else {
			fmt.Printf("⚠️  apk binary missing after bootstrap attempt: %v\n", statErr)
		}
	}
	// Provide /etc/apk/repositories file for convenience if apk is present
	reposPath := filepath.Join(mountDir, "etc", "apk")
	cmd = exec.Command("sudo", "mkdir", "-p", reposPath)
	_ = cmd.Run()
	repositories := "http://dl-cdn.alpinelinux.org/alpine/v3.18/main\nhttp://dl-cdn.alpinelinux.org/alpine/v3.18/community\n"
	cmd = exec.Command("sudo", "tee", filepath.Join(reposPath, "repositories"))
	cmd.Stdin = strings.NewReader(repositories)
	_ = cmd.Run()
	if _, statErr := os.Stat(filepath.Join(reposPath, "repositories")); statErr == nil {
		fmt.Printf("✓ Alpine repositories file created\n")
	}

	// Initialize apk database using a real chroot (bind /proc,/sys,/dev) so package index and installed DB populate correctly.
	apkDbPath := filepath.Join(mountDir, "lib", "apk", "db")
	_ = exec.Command("sudo", "mkdir", "-p", apkDbPath).Run()
	if _, err := os.Stat(apkTarget); err == nil {
		installedFile := filepath.Join(apkDbPath, "installed")
		if fi, statErr := os.Stat(installedFile); statErr == nil && fi.Size() > 0 {
			fmt.Printf("✓ Existing apk database detected (installed size=%d), skipping chroot init\n", fi.Size())
		} else {
			fmt.Printf("Initializing apk database inside chroot...\n")
			// Bind mount required pseudo filesystems
			binds := [][2]string{{"/proc", filepath.Join(mountDir, "proc")}, {"/sys", filepath.Join(mountDir, "sys")}, {"/dev", filepath.Join(mountDir, "dev")}}
			for _, b := range binds {
				_ = exec.Command("sudo", "mount", "--bind", b[0], b[1]).Run()
			}
			// Ensure DNS resolution inside chroot
			_ = exec.Command("sudo", "cp", "/etc/resolv.conf", filepath.Join(mountDir, "etc", "resolv.conf")).Run()
			// Ensure keys directory exists (minirootfs should provide it; create if missing)
			keysDir := filepath.Join(mountDir, "etc", "apk", "keys")
			_ = exec.Command("sudo", "mkdir", "-p", keysDir).Run()
			// Perform initdb then add essential packages; update indexes afterwards
			initCmd := exec.Command("sudo", "chroot", mountDir, "/bin/apk", "--no-cache", "--initdb")
			if err := initCmd.Run(); err != nil {
				fmt.Printf("⚠️  apk initdb failed: %v\n", err)
			}
			addCmd := exec.Command("sudo", "chroot", mountDir, "/bin/apk", "--no-cache", "add", "alpine-baselayout", "busybox", "alpine-keys")
			if err := addCmd.Run(); err != nil {
				fmt.Printf("⚠️  apk add baseline packages failed: %v (attempting busybox only)\n", err)
				_ = exec.Command("sudo", "chroot", mountDir, "/bin/apk", "--no-cache", "add", "busybox").Run()
			} else {
				fmt.Printf("✓ Installed baseline packages into chroot (baselayout, busybox, keys)\n")
			}
			_ = exec.Command("sudo", "chroot", mountDir, "/bin/apk", "update").Run()
			// Show resulting installed DB size
			if fi, statErr := os.Stat(installedFile); statErr == nil {
				fmt.Printf("apk 'installed' size after chroot init: %d bytes\n", fi.Size())
			} else {
				fmt.Printf("⚠️  apk installed DB file missing after chroot init attempts\n")
			}
			// Unmount binds in reverse order
			for i := len(binds) - 1; i >= 0; i-- {
				_ = exec.Command("sudo", "umount", binds[i][1]).Run()
			}
		}
	} else {
		fmt.Printf("⚠️  apk binary missing - creating placeholder DB (will be non-functional)\n")
		for _, dbFile := range []string{"installed", "lock", "triggers", "scripts.tar"} {
			_ = exec.Command("sudo", "touch", filepath.Join(apkDbPath, dbFile)).Run()
		}
		_ = exec.Command("sudo", "chmod", "600", filepath.Join(apkDbPath, "lock")).Run()
	}

	// Pre-generate dropbear host key to avoid cost during first boot (minor boot time improvement)
	dropbearDir := filepath.Join(mountDir, "etc", "dropbear")
	_ = exec.Command("sudo", "mkdir", "-p", dropbearDir).Run()
	keyPath := filepath.Join(dropbearDir, "dropbear_rsa_host_key")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		genStart := time.Now()
		if err := exec.Command("sudo", "dropbearkey", "-t", "rsa", "-f", keyPath).Run(); err != nil {
			fmt.Printf("Warning: pre-generating dropbear host key failed: %v\n", err)
		} else {
			fmt.Printf("✓ Pre-generated dropbear host key (%s) in %s\n", keyPath, time.Since(genStart))
		}
	}

	// Create init as a direct copy of static busybox (after apk bootstrap so init exists regardless of apk success)
	initPath := filepath.Join(mountDir, "init")
	cmd = exec.Command("sudo", "cp", "/usr/bin/busybox", initPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy busybox as init: %w", err)
	}

	// Create startup script that will be called by busybox init
	startupScript := fmt.Sprintf(`#!/bin/sh
echo "Alpine MicroVM starting..."
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts 2>/dev/null || echo "Warning: devpts mount failed"
ln -sf /dev/pts/ptmx /dev/ptmx
hostname alpine-vm

# Minimal directories
mkdir -p /var/log /var/run /root/.ssh /etc/dropbear /etc/apk

# Ensure DNS is usable inside the VM
echo "nameserver 8.8.8.8" > /etc/resolv.conf

# Initialize apk database on first boot if empty
if [ ! -s /lib/apk/db/installed ]; then
    echo "Initializing apk database..."
    apk fix --no-cache --no-network >/dev/null 2>&1 || true
fi

# Loopback
ip link set lo up

# Fast network config (short DHCP timeout, fallback to static immediately)
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 up
    udhcpc -i eth0 -n -q -t 3 -T 1 2>/dev/null || {
        ip addr add %s/24 dev eth0
        ip route add default via 192.168.100.1
    }
    IP_ADDR=$(ip addr show eth0 | grep 'inet ' | awk '{print $2}' | cut -d/ -f1)
    echo "Network: $IP_ADDR"
fi

# apk already embedded at build time; just verify
if [ -x /bin/apk ]; then
    /bin/apk --version 2>/dev/null || true
else
    echo "apk missing (unexpected)"
fi

export TERM=xterm
stty sane 2>/dev/null || true

# Start SSH immediately (host key pre-generated in image build)
if [ -f /sbin/dropbear ]; then
    /sbin/dropbear -r /etc/dropbear/dropbear_rsa_host_key -p 22 -F -E 2>&1 &
    echo "SSH server started on port 22"
fi

echo "System ready! Disk size: %dMB"
`, staticIP, sizeMB)

	startupPath := filepath.Join(mountDir, "etc", "init.d", "rcS")
	cmd = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "init.d"))
	cmd.Run()

	cmd = exec.Command("sudo", "tee", startupPath)
	cmd.Stdin = strings.NewReader(startupScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write startup script: %w", err)
	}

	cmd = exec.Command("sudo", "chmod", "+x", startupPath)
	cmd.Run()

	// Create inittab for busybox init
	inittabContent := `::sysinit:/etc/init.d/rcS
	ttyS0::respawn:/bin/busybox getty -L ttyS0 115200 vt100
	::respawn:/bin/sh
	::ctrlaltdel:/bin/reboot
	::shutdown:/bin/umount -a -r
	`
	inittabPath := filepath.Join(mountDir, "etc", "inittab")
	cmd = exec.Command("sudo", "tee", inittabPath)
	cmd.Stdin = strings.NewReader(inittabContent)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write inittab: %w", err)
	}

	// Create basic passwd file with root and the configured user
	passwdContent := fmt.Sprintf(`root:x:0:0:root:/root:/bin/sh
%s:x:1000:1000:User:/home/%s:/bin/sh
`, username, username)
	passwdPath := filepath.Join(mountDir, "etc", "passwd")
	cmd = exec.Command("sudo", "tee", passwdPath)
	cmd.Stdin = strings.NewReader(passwdContent)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write passwd: %w", err)
	}

	// Generate password hash using openssl
	hashCmd := exec.Command("openssl", "passwd", "-1", password)
	hashOut, err := hashCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to generate password hash: %w", err)
	}
	passwordHash := strings.TrimSpace(string(hashOut))

	// Create shadow file with the generated password hash (enable root login with same password)
	shadowContent := fmt.Sprintf(`root:%s:0:0:99999:7:::
%s:%s:19000:0:99999:7:::
`, passwordHash, username, passwordHash)
	shadowPath := filepath.Join(mountDir, "etc", "shadow")
	cmd = exec.Command("sudo", "tee", shadowPath)
	cmd.Stdin = strings.NewReader(shadowContent)
	cmd.Run()
	exec.Command("sudo", "chmod", "640", shadowPath).Run()

	// Create group file
	groupContent := fmt.Sprintf(`root:x:0:
%s:x:1000:
`, username)
	groupPath := filepath.Join(mountDir, "etc", "group")
	cmd = exec.Command("sudo", "tee", groupPath)
	cmd.Stdin = strings.NewReader(groupContent)
	cmd.Run()

	// Create home directory for user
	userHome := filepath.Join(mountDir, "home", username)
	exec.Command("sudo", "mkdir", "-p", userHome).Run()
	exec.Command("sudo", "chown", "1000:1000", userHome).Run()

	// Set up SSH authorized keys
	sshKeyPath := filepath.Join(m.workDir, "ssh-keys", "vm-key.pub")
	if _, err := os.Stat(sshKeyPath); err == nil {
		sshDir := filepath.Join(mountDir, "root", ".ssh")
		cmd = exec.Command("sudo", "mkdir", "-p", sshDir)
		cmd.Run()

		authorizedKeysPath := filepath.Join(sshDir, "authorized_keys")
		cmd = exec.Command("sudo", "cp", sshKeyPath, authorizedKeysPath)
		if err := cmd.Run(); err == nil {
			cmd = exec.Command("sudo", "chmod", "600", authorizedKeysPath)
			cmd.Run()
			cmd = exec.Command("sudo", "chown", "root:root", authorizedKeysPath)
			cmd.Run()
			fmt.Printf("SSH public key installed for root access\n")
		}
	}

	fmt.Printf("VM-specific rootfs created successfully with IP %s (size: %dMB)\n", staticIP, sizeMB)
	return nil
}

// UpdateFirecrackerConfigMAC updates the guest_mac in Firecracker's config.json
// This must be called when the VM is stopped, before restarting with a new MAC address.
func (m *Manager) UpdateFirecrackerConfigMAC(vmID, newMAC string) error {
	configPath := filepath.Join(m.workDir, "microvms", vmID, "config.json")

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read VM configuration: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return fmt.Errorf("failed to parse VM configuration: %w", err)
	}

	// Update guest_mac in network-interfaces
	netIfaces, ok := config["network-interfaces"].([]interface{})
	if !ok || len(netIfaces) == 0 {
		return fmt.Errorf("no network-interfaces found in config.json")
	}

	iface, ok := netIfaces[0].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid network-interfaces format in config.json")
	}

	oldMAC, _ := iface["guest_mac"].(string)
	iface["guest_mac"] = newMAC

	updatedConfigData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated configuration: %w", err)
	}

	if err := os.WriteFile(configPath, updatedConfigData, 0644); err != nil {
		return fmt.Errorf("failed to write updated configuration: %w", err)
	}

	fmt.Printf("✅ Updated Firecracker config.json for VM %s: MAC %s → %s\n", vmID, oldMAC, newMAC)
	return nil
}

// RemoveAntiSpoofing is a public wrapper around removeAntiSpoofing for external access
func (m *Manager) RemoveAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr string) error {
	return m.networkManager.removeAntiSpoofing(tapName, macAddr, ipv4Addr, ipv6Addr)
}

// GetNetworkManager returns the network manager for external access
func (m *Manager) GetNetworkManager() *NetworkManager {
	return m.networkManager
}

// GetStorageBackend returns the storage backend for external access (snapshot management)
func (m *Manager) GetStorageBackend() storage.StorageBackend {
	return m.storageBackend
}

// GetNetworkBackend returns the network backend this manager constructed, so
// the HTTP layer can call backend-specific operations (floating IPs on the OVN
// backend) without re-creating or re-configuring anything.
func (m *Manager) GetNetworkBackend() network.Backend {
	m.backendMu.RLock()
	defer m.backendMu.RUnlock()
	return m.enhancedNetwork
}

// parseBandwidthLimits parses bandwidth limit strings (e.g., "100mbit", "50Mbit") to Mbps integers
func parseBandwidthLimits(ingressStr, egressStr string) *network.BandwidthLimit {
	limit := &network.BandwidthLimit{}
	hasLimit := false

	if ingressStr != "" {
		if mbps := parseBandwidthString(ingressStr); mbps > 0 {
			limit.IngressMbps = mbps
			hasLimit = true
		}
	}

	if egressStr != "" {
		if mbps := parseBandwidthString(egressStr); mbps > 0 {
			limit.EgressMbps = mbps
			hasLimit = true
		}
	}

	if !hasLimit {
		return nil
	}
	return limit
}

// parseBandwidthString converts strings like "100mbit", "50Mbit", "1gbit" to Mbps
func parseBandwidthString(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}

	var value int
	var unit string

	// Try parsing with units
	if _, err := fmt.Sscanf(s, "%d%s", &value, &unit); err == nil {
		switch unit {
		case "mbit", "mbps", "m":
			return value
		case "gbit", "gbps", "g":
			return value * 1000
		case "kbit", "kbps", "k":
			return value / 1000
		}
	}

	// Try parsing as just number (assume Mbps)
	if _, err := fmt.Sscanf(s, "%d", &value); err == nil {
		return value
	}

	return 0
}
