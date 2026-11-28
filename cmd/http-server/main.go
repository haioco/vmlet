package main

import (
	"bufio"
	"bytes"
	"context"

	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/haioco/vmlet/internal/autoscale"
	"github.com/haioco/vmlet/internal/balloon"
	"github.com/haioco/vmlet/internal/cloudinit"
	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/firecracker"
	"github.com/haioco/vmlet/internal/images"
	"github.com/haioco/vmlet/internal/metrics"
	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/monitoring"
	"github.com/haioco/vmlet/internal/network"
	"github.com/haioco/vmlet/internal/ovn"
	"github.com/haioco/vmlet/internal/storage"
)

// ServiceVersion is the release version (SemVer, see docs/VERSIONING.md).
// It is a var so releases can stamp it at build time:
//
//	go build -ldflags "-X main.ServiceVersion=0.2.0" ./cmd/http-server
var ServiceVersion = "0.1.0"

const ServiceName = "Vmlet MicroVM Service"

// calculateVMIP calculates a consistent IP address for a VM based on its ID
func calculateVMIP(vmID string) string {
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

// deriveIPv4NATFromIPv6 derives a private IPv4 NAT address from an IPv6 address
// Takes the last 16 bits of the IPv6 address and maps to 10.100.0.0/24 range
func deriveIPv4NATFromIPv6(ipv6Addr string) string {
	// Parse IPv6 address
	ip := net.ParseIP(ipv6Addr)
	if ip == nil || ip.To4() != nil {
		// Invalid IPv6 or is IPv4, return default
		return "10.100.0.2"
	}

	// Get the last 2 bytes (16 bits) of the IPv6 address
	ipv6Bytes := ip.To16()
	if ipv6Bytes == nil {
		return "10.100.0.2"
	}

	// Use last 2 bytes to create an offset in the 10.100.0.0/24 range
	// Combine last 2 bytes: byte[14] * 256 + byte[15]
	offset := (uint16(ipv6Bytes[14]) << 8) | uint16(ipv6Bytes[15])

	// Map to 10.100.0.2 - 10.100.0.254 range (avoid .0, .1, .255)
	// Use modulo to wrap around the 253 usable addresses
	ipSuffix := 2 + (offset % 253)

	return fmt.Sprintf("10.100.0.%d", ipSuffix)
}

// getPublicIPv4 returns the server's main public IPv4 address
func (s *HTTPServer) getPublicIPv4() (string, error) {
	// Try to get from environment
	if publicIP := os.Getenv("MICROVM_PUBLIC_IPV4"); publicIP != "" {
		return publicIP, nil
	}

	// Try to get from primary network interface
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		// Skip loopback and non-up interfaces
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			// Get IPv4 address
			if ipv4 := ipNet.IP.To4(); ipv4 != nil {
				// Skip private networks
				if !ipv4.IsPrivate() && !ipv4.IsLoopback() {
					return ipv4.String(), nil
				}
			}
		}
	}

	return "", fmt.Errorf("no public IPv4 address found")
}

// setupIPv6Forwarding configures ip6tables rules for IPv6 forwarding
// This allows external IPv6 traffic to reach VMs on the bridge
func setupIPv6Forwarding(ipv6Subnet string) error {
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

type HTTPServer struct {
	manager        *firecracker.Manager
	imageManager   *images.ImageManager
	reservationMgr *network.ReservationManager
	vms            map[string]*models.MicroVMInstance
	vmsMutex       sync.RWMutex // Mutex for VM map access — ALL s.vms accesses MUST go through the helpers below
	persistFile    string
	customerQuotas map[string]*CustomerQuota // Per-customer quotas (nil entry = unlimited)
	quotasFile     string                    // JSON persistence for quotas
	tapCache       map[string]tapCacheEntry  // Cache for TAP device information
	cacheMutex     sync.RWMutex              // Mutex for thread-safe cache access

	// Performance optimization caches
	vmInfoCache     map[string]*VMStatusResponse // Cache for VM information
	vmCacheMutex    sync.RWMutex                 // Mutex for VM info cache
	cacheExpiry     time.Duration                // Cache expiry duration
	lastCacheUpdate time.Time                    // Last cache update timestamp

	// Resource monitoring
	resourceMonitorInit sync.Once
	resourceMonitor     *monitoring.ResourceMonitor

	// Automatic memory balloon management
	balloonManager *balloon.AutoBalloonManager
	vmAutoscaler   *autoscale.VMAutoscaler

	// VM state persistence (SQLite)
	vmStateStorage *storage.SQLiteStorage

	// VM creation queue (to avoid starting all VMs at once)
	creationQueue    chan string // queue of VM IDs to create
	creationQueueMu  sync.Mutex  // protects queue state
	pendingVMs       []string    // pending VM IDs (kept until completion for visibility)
	maxCreateWorkers int         // max concurrent creations
	currentCreating  string      // VM currently being created
	shutdownOnce     sync.Once   // ensures the creation queue is closed exactly once
	shuttingDown     bool        // set on first signal; rejects new creation work

	// Per-VM mutating-operation guard. A reinstall racing a password reset
	// (or a second reinstall) on the same VM fights over the same zvol and
	// TAP: the delete's `zfs destroy` fails with "dataset is busy", the
	// recreation silently reuses the stale disk, and the customer is left
	// with a machine whose password no longer matches the panel.
	vmOpsMu       sync.Mutex      // protects vmOpsInFlight
	vmOpsInFlight map[string]bool // vmID -> a mutating op is running

	// Web console token management
	consoleTokens   map[string]*ConsoleToken
	consoleTokensMu sync.RWMutex
	nodeDomain      string // public domain of this node (e.g. node1.example.com)
}

type tapCacheEntry struct {
	tapDevice string
	timestamp time.Time
}

// ConsoleToken represents a time-limited token granting web console access to a VM.
type ConsoleToken struct {
	Token     string    `json:"token"`
	VMID      string    `json:"vm_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// All s.vms map accesses MUST use these helpers. The map is shared across API
// handlers, background loops (status updater, reboot watcher, balloon,
// autoscaler, billing) and queue workers; any unlocked concurrent access is a
// Go runtime fatal ("concurrent map iteration and map write").

// getVM returns the VM entry under a read lock.
func (s *HTTPServer) getVM(vmID string) (*models.MicroVMInstance, bool) {
	s.vmsMutex.RLock()
	defer s.vmsMutex.RUnlock()
	vm, exists := s.vms[vmID]
	return vm, exists
}

// setVM stores a VM entry under a write lock.
func (s *HTTPServer) setVM(vmID string, vm *models.MicroVMInstance) {
	s.vmsMutex.Lock()
	defer s.vmsMutex.Unlock()
	s.vms[vmID] = vm
}

// deleteVMEntry removes a VM entry under a write lock.
func (s *HTTPServer) deleteVMEntry(vmID string) {
	s.vmsMutex.Lock()
	defer s.vmsMutex.Unlock()
	delete(s.vms, vmID)
}

// snapshotVMMap returns a shallow copy of the VM map for safe iteration.
// Values are shared pointers (same mutation semantics as before); only the
// map structure itself is protected.
func (s *HTTPServer) snapshotVMMap() map[string]*models.MicroVMInstance {
	s.vmsMutex.RLock()
	defer s.vmsMutex.RUnlock()
	cp := make(map[string]*models.MicroVMInstance, len(s.vms))
	for k, v := range s.vms {
		cp[k] = v
	}
	return cp
}

// vmCount returns the number of tracked VMs under a read lock.
func (s *HTTPServer) vmCount() int {
	s.vmsMutex.RLock()
	defer s.vmsMutex.RUnlock()
	return len(s.vms)
}

// getOrInitResourceMonitor lazily initializes the resource monitor
func (s *HTTPServer) getOrInitResourceMonitor() *monitoring.ResourceMonitor {
	s.resourceMonitorInit.Do(func() {
		s.resourceMonitor = monitoring.NewResourceMonitor(5 * time.Second)
	})
	return s.resourceMonitor
}

// startCreationWorkers launches a fixed number of worker goroutines that process VM creations from the queue sequentially per worker.
func (s *HTTPServer) startCreationWorkers() {
	for i := 0; i < s.maxCreateWorkers; i++ {
		go func(workerID int) {
			for vmID := range s.creationQueue {
				// Mark current creating (keep in pending for visibility)
				s.creationQueueMu.Lock()
				s.currentCreating = vmID
				s.creationQueueMu.Unlock()

				// Optional delay between creations
				if d := os.Getenv("MICROVM_CREATE_DELAY_MS"); d != "" {
					if ms, err := strconv.Atoi(d); err == nil && ms > 0 {
						log.Printf("[queue-worker-%d] Delay %dms before creating %s", workerID, ms, vmID)
						time.Sleep(time.Duration(ms) * time.Millisecond)
					}
				}

				vm, exists := s.getVM(vmID)
				if !exists {
					log.Printf("[queue-worker-%d] VM %s disappeared before creation", workerID, vmID)
					s.creationQueueMu.Lock()
					if s.currentCreating == vmID {
						s.currentCreating = ""
					}
					// Remove from pending slice if present
					for idx, id := range s.pendingVMs {
						if id == vmID {
							s.pendingVMs = append(s.pendingVMs[:idx], s.pendingVMs[idx+1:]...)
							break
						}
					}
					s.creationQueueMu.Unlock()
					continue
				}
				log.Printf("[queue-worker-%d] Creating VM %s", workerID, vmID)
				// Debug: Check what labels exist on the VM when worker retrieves it
				if vm.Metadata.Labels != nil {
					log.Printf("[queue-worker-%d] 🔍 VM %s labels: cloudinit_username='%s', cloudinit_password='%s'",
						workerID, vmID, vm.Metadata.Labels["cloudinit_username"], vm.Metadata.Labels["cloudinit_password"])
				} else {
					log.Printf("[queue-worker-%d] ⚠️  VM %s has nil Metadata.Labels!", workerID, vmID)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				// Creation logic
				if validationResult, err := s.manager.ValidateSystemPrerequisites(false); err != nil {
					log.Printf("[queue-worker-%d] System validation failed for VM %s: %v", workerID, vmID, err)
					vm.Status = models.MicroVMStatusError
				} else if !validationResult.Overall {
					log.Printf("[queue-worker-%d] System prerequisites not met for VM %s: %v", workerID, vmID, validationResult.Issues)
					vm.Status = models.MicroVMStatusError
				} else if err := s.manager.CreateMicroVM(ctx, vm); err != nil {
					log.Printf("[queue-worker-%d] Failed to create VM %s: %v", workerID, vmID, err)
					vm.Status = models.MicroVMStatusError
					if cleanupErr := s.cleanupFailedVMInternal(ctx, vmID); cleanupErr != nil {
						log.Printf("[queue-worker-%d] Cleanup failure for VM %s: %v", workerID, vmID, cleanupErr)
					}
				} else {
					log.Printf("[queue-worker-%d] Successfully created VM %s", workerID, vmID)
					// For DHCP VMs, kick off a short fast discovery loop to capture lease quickly
					if vm.Network.AssignmentMethod == models.NetworkModeDHCP {
						go s.startFastDHCPDiscovery(vm.ID, 90*time.Second, 3*time.Second)
					}
				}
				cancel()
				s.persistVMState()
				s.invalidateTAPCache(vmID)
				s.invalidateVMCache(vmID)

				// Clear current and remove from pending
				s.creationQueueMu.Lock()
				if s.currentCreating == vmID {
					s.currentCreating = ""
				}
				for idx, id := range s.pendingVMs {
					if id == vmID {
						s.pendingVMs = append(s.pendingVMs[:idx], s.pendingVMs[idx+1:]...)
						break
					}
				}
				s.creationQueueMu.Unlock()
			}
		}(i)
	}
}

// tryBeginVMOp claims the per-VM mutating-operation slot. Returns false when
// another mutating op (reinstall, password reset) is already running for this
// VM — two of them fight over the same zvol and TAP, and the loser leaves the
// disk "dataset is busy" and the guest out of sync with the panel.
func (s *HTTPServer) tryBeginVMOp(vmID string) bool {
	s.vmOpsMu.Lock()
	defer s.vmOpsMu.Unlock()
	if s.vmOpsInFlight == nil {
		s.vmOpsInFlight = make(map[string]bool)
	}
	if s.vmOpsInFlight[vmID] {
		return false
	}
	s.vmOpsInFlight[vmID] = true
	return true
}

// endVMOp releases the per-VM mutating-operation slot.
func (s *HTTPServer) endVMOp(vmID string) {
	s.vmOpsMu.Lock()
	defer s.vmOpsMu.Unlock()
	delete(s.vmOpsInFlight, vmID)
}

// vmOpInFlight reports (without claiming) whether a mutating op is running.
func (s *HTTPServer) vmOpInFlight(vmID string) bool {
	s.vmOpsMu.Lock()
	defer s.vmOpsMu.Unlock()
	return s.vmOpsInFlight[vmID]
}

// vmCreationPending reports whether a creation for this VM is queued or
// running. A reinstall/password change racing it fights over the same zvol.
func (s *HTTPServer) vmCreationPending(vmID string) bool {
	s.creationQueueMu.Lock()
	defer s.creationQueueMu.Unlock()
	for _, id := range s.pendingVMs {
		if id == vmID {
			return true
		}
	}
	return false
}

// enqueueVMCreation adds a VM ID to the queue for later creation.
func (s *HTTPServer) enqueueVMCreation(vmID string) {
	s.creationQueueMu.Lock()
	if s.shuttingDown {
		s.creationQueueMu.Unlock()
		if vm, ok := s.getVM(vmID); ok {
			vm.Status = models.MicroVMStatusError
		}
		log.Printf("[queue] Rejecting VM %s creation: server is shutting down", vmID)
		return
	}
	// Dedupe: a VM already pending or in-flight must not be enqueued a second
	// time — two workers then create the same VM concurrently (duplicate
	// Firecracker processes on one zvol/TAP), which is how a reinstall storm
	// ends in "dataset is busy" and TAP removal failures.
	for _, id := range s.pendingVMs {
		if id == vmID {
			s.creationQueueMu.Unlock()
			log.Printf("[queue] VM %s is already pending creation; ignoring duplicate enqueue", vmID)
			return
		}
	}
	s.pendingVMs = append(s.pendingVMs, vmID)
	s.creationQueueMu.Unlock()
	select {
	case s.creationQueue <- vmID:
		log.Printf("[queue] Enqueued VM %s (pending_count=%d)", vmID, len(s.pendingVMs))
	default:
		// Queue full - mark VM error immediately
		if vm, ok := s.getVM(vmID); ok {
			vm.Status = models.MicroVMStatusError
		}
		log.Printf("Creation queue full - VM %s marked error", vmID)
	}
}

// drainCreationQueue stops new creation work and waits (bounded) for in-flight
// creations to finish. Workers range over the channel and exit once closed.
func (s *HTTPServer) drainCreationQueue(timeout time.Duration) {
	s.shutdownOnce.Do(func() {
		s.creationQueueMu.Lock()
		s.shuttingDown = true
		s.creationQueueMu.Unlock()
		close(s.creationQueue)
	})

	deadline := time.Now().Add(timeout)
	for {
		s.creationQueueMu.Lock()
		active := s.currentCreating
		pending := len(s.pendingVMs)
		s.creationQueueMu.Unlock()

		if active == "" && pending == 0 {
			fmt.Println("✅ Creation queue drained (no in-flight work)")
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("⚠️  Creation drain timed out (in-flight: %s, pending: %d) — proceeding with shutdown\n", active, pending)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// getCreationQueueStatus returns the current pending queue state.
func (s *HTTPServer) getCreationQueueStatus(w http.ResponseWriter, r *http.Request) {
	s.creationQueueMu.Lock()
	pending := append([]string(nil), s.pendingVMs...)
	current := s.currentCreating
	s.creationQueueMu.Unlock()
	// Ensure empty slice renders as [] instead of null
	if pending == nil {
		pending = []string{}
	}
	resp := map[string]interface{}{
		"pending_count":    len(pending),
		"pending":          pending,
		"workers":          s.maxCreateWorkers,
		"current_creating": current,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// getVMStatsHandler returns resource and balloon stats for a VM
func (s *HTTPServer) getVMStatsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Require process info (must be running or previously started)
	if vm.ProcessInfo == nil || vm.ProcessInfo.PID == 0 {
		http.Error(w, "VM process info unavailable (VM may not be running)", http.StatusConflict)
		return
	}

	monitor := s.getOrInitResourceMonitor()

	// Collect stats
	stats, err := monitor.GetVMResourceStats(vm.ProcessInfo.PID, vm.ProcessInfo.SocketPath, vm.Resources.MemoryMB, vm.Resources.VCPUs)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to collect stats: %v", err), http.StatusInternalServerError)
		return
	}

	// Augment with VM ID & status
	response := map[string]interface{}{
		"vm_id":  vm.ID,
		"status": vm.Status,
		"configured": map[string]interface{}{
			"memory_mb": vm.Resources.MemoryMB,
			"vcpus":     vm.Resources.VCPUs,
		},
		"stats": stats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// Enhanced CreateVMRequest to match CLI format
type CreateVMRequest struct {
	Name          string            `json:"name,omitempty"`
	CustomerID    string            `json:"customer_id,omitempty"`
	ResourceSpecs ResourceSpecs     `json:"resource_specs,omitempty"`
	ImageConfig   ImageConfig       `json:"image_config,omitempty"`
	NetworkConfig NetworkConfig     `json:"network_config,omitempty"`
	Network       NetworkConfig     `json:"network,omitempty"` // Alias for network_config
	StorageConfig StorageConfig     `json:"storage_config,omitempty"`
	CloudInit     *CloudInitConfig  `json:"cloudinit,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"` // Alias for metadata

	// Billing configuration
	PricePerHour float64 `json:"price_per_hour,omitempty"` // Price in specified currency per hour
	Currency     string  `json:"currency,omitempty"`       // Currency code (default: IRR)

	// Legacy fields for backward compatibility
	VCPUs     uint32 `json:"vcpus,omitempty"`
	MemoryMB  uint32 `json:"memory_mb,omitempty"`
	StorageMB uint32 `json:"storage_mb,omitempty"`
	ImageID   string `json:"image_id,omitempty"`
	Image     string `json:"image,omitempty"` // Alternative to image_id
}

// ImageConfig defines VM image configuration
type ImageConfig struct {
	Distribution string   `json:"distribution,omitempty"`
	Version      string   `json:"version,omitempty"`
	Packages     []string `json:"packages,omitempty"`
}

// NetworkConfig defines VM network configuration
type NetworkConfig struct {
	Mode             string                  `json:"mode,omitempty"`              // Alias for AssignmentMethod (preferred)
	AssignmentMethod string                  `json:"assignment_method,omitempty"` // Legacy field name
	NetworkID        string                  `json:"network_id,omitempty"`        // VPC/network to attach to
	EnableSSH        bool                    `json:"enable_ssh,omitempty"`
	MACAddress       string                  `json:"mac_address,omitempty"` // Custom MAC address
	IPAddress        string                  `json:"ip_address,omitempty"`  // Custom IP address
	IPv6Address      string                  `json:"ipv6_address,omitempty"`
	IPv6Gateway      string                  `json:"ipv6_gateway,omitempty"`
	IPv6PrefixLength int                     `json:"ipv6_prefix_length,omitempty"`
	EnableIPv4NAT    bool                    `json:"enable_ipv4_nat,omitempty"`   // Enable IPv4 NAT for IPv6-only VMs
	ParentInterface  string                  `json:"parent_interface,omitempty"`  // For MACVLAN mode
	BandwidthLimit   *network.BandwidthLimit `json:"bandwidth_limit,omitempty"`   // Bandwidth limits (struct format)
	BandwidthIngress string                  `json:"bandwidth_ingress,omitempty"` // VM download limit (wget) - lower to save DC costs
	BandwidthEgress  string                  `json:"bandwidth_egress,omitempty"`  // VM upload limit - higher is acceptable
}

// StorageConfig defines VM storage configuration
type StorageConfig struct {
	Type string `json:"type,omitempty"`
}

// CloudInitConfig defines cloud-init configuration
type CloudInitConfig struct {
	Hostname     string            `json:"hostname,omitempty"`
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
	PasswordHash string            `json:"password_hash,omitempty"`
	SSHKeys      []string          `json:"ssh_keys,omitempty"`
	RunCommands  []string          `json:"run_commands,omitempty"`
	WriteFiles   map[string]string `json:"write_files,omitempty"`
}

// storeCloudInitSSHKeys persists every requested key.
//
// The legacy `cloudinit_sshkey` label only held the first key, so selecting
// two keys in the panel silently dropped the second. Keep the legacy label
// (first key) for older readers and add `cloudinit_sshkeys` as a JSON array
// that the manager reads as the authoritative set.
func storeCloudInitSSHKeys(labels map[string]string, keys []string) {
	if labels == nil {
		return
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
	if len(cleaned) == 0 {
		delete(labels, "cloudinit_sshkey")
		delete(labels, "cloudinit_sshkeys")
		return
	}
	labels["cloudinit_sshkey"] = cleaned[0]
	if b, err := json.Marshal(cleaned); err == nil {
		labels["cloudinit_sshkeys"] = string(b)
	}
}

type CreateVMResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	IPAddress string `json:"ip_address"`
	SSHPort   uint32 `json:"ssh_port"`
	Message   string `json:"message"`
}

type VMStatusResponse struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Status      string                  `json:"status"`
	ImageName   string                  `json:"image_name,omitempty"` // Added for CLI list display
	Resources   ResourceSpecs           `json:"resources"`
	Network     models.NetworkInfo      `json:"network"` // Use models.NetworkInfo to include bandwidth fields
	Storage     StorageInfo             `json:"storage"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
	Uptime      string                  `json:"uptime"`
	Metadata    map[string]string       `json:"metadata"`
	AutoStart   bool                    `json:"auto_start"`
	ProcessInfo *models.ProcessInfo     `json:"process_info,omitempty"`
	Autoscale   *models.AutoscaleConfig `json:"autoscale,omitempty"` // Per-VM autoscaling configuration
	// Troubleshooting helpers
	CloudInitAttached bool   `json:"cloud_init_attached,omitempty"`
	CloudInitPath     string `json:"cloud_init_path,omitempty"`
}

type ResourceSpecs struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMB  uint32 `json:"memory_mb"`
	StorageMB uint32 `json:"storage_mb"`
}

// Remove local NetworkInfo definition - use models.NetworkInfo instead
// This ensures bandwidth fields are included in API responses

type StorageInfo struct {
	RootDevice string `json:"root_device"`
	Size       uint32 `json:"size_mb"`
	Type       string `json:"type"`
}

func resolveWorkDir() string {
	// Prefer explicit env
	env := os.Getenv("MICROVM_WORK_DIR")
	if env != "" {
		return env
	}
	// Backward compatibility: if legacy ./firecracker-work exists use it
	legacy := "./firecracker-work"
	if fi, err := os.Stat(legacy); err == nil && fi.IsDir() {
		return legacy
	}
	// Default modern layout
	return "./work"
}

// resolveKernelPath returns the kernel image path, mirroring startup resolution logic.
func resolveKernelPath() string {
	if k := os.Getenv("MICROVM_KERNEL_PATH"); k != "" {
		return k
	}
	if _, err := os.Stat("/opt/vmlet/kernels/vmlinux"); err == nil {
		return "/opt/vmlet/kernels/vmlinux"
	}
	return "/var/lib/firecracker-containerd/runtime/default-vmlinux.bin"
}

// regenerateVMConfigJSON rebuilds config.json for a VM from its in-memory state
// (loaded from SQLite). Called automatically by startVMHandler when the file is
// missing — for example after a JSON→SQLite migration that did not preserve disk files.
func regenerateVMConfigJSON(vm *models.MicroVMInstance) error {
	workDir := resolveWorkDir()
	microvmDir := filepath.Join(workDir, "microvms", vm.ID)

	// Resolve rootfs path: external label → ZFS zvol → flat file
	rootfsPath := ""
	isZFSVolume := false

	// 1. Explicit external_rootfs_path label (set for migrated/ZFS VMs)
	if vm.Metadata.Labels != nil {
		if ext := strings.TrimSpace(vm.Metadata.Labels["external_rootfs_path"]); ext != "" {
			if _, err := os.Stat(ext); err == nil {
				rootfsPath = ext
				isZFSVolume = true
				log.Printf("🔧 regenerate: using external_rootfs_path label: %s", rootfsPath)
			}
		}
	}

	// 2. Standard ZFS zvol paths
	if rootfsPath == "" {
		for _, candidate := range []string{
			fmt.Sprintf("/dev/zvol/default/vmlet-microvms/%s", vm.ID),
			fmt.Sprintf("/dev/zvol/rpool/vmlet-microvms/%s", vm.ID),
		} {
			if _, err := os.Stat(candidate); err == nil {
				rootfsPath = candidate
				isZFSVolume = true
				log.Printf("🔧 regenerate: found ZFS zvol: %s", rootfsPath)
				break
			}
		}
	}

	// 3. Flat file fallback
	if rootfsPath == "" {
		flat := filepath.Join(microvmDir, "rootfs.ext4")
		if _, err := os.Stat(flat); err == nil {
			rootfsPath = flat
		}
	}

	if rootfsPath == "" {
		return fmt.Errorf("no rootfs found (checked external_rootfs_path label, ZFS zvol paths, and %s/rootfs.ext4) — VM needs reinstall", microvmDir)
	}

	kernelPath := resolveKernelPath()

	imageID := ""
	// Default root device: ZFS volumes are typically partitioned (/dev/vda1),
	// flat ext4 images are whole-disk (/dev/vda).
	rootDevice := "/dev/vda"
	if isZFSVolume {
		rootDevice = "/dev/vda1"
	}
	if vm.Metadata.Labels != nil {
		if id := vm.Metadata.Labels["image_id"]; id != "" {
			imageID = id
		}
		if rd := strings.TrimSpace(vm.Metadata.Labels["boot_root_device"]); rd != "" {
			rootDevice = rd
		}
	}

	// Mirror boot-args logic from generateEnhancedConfig
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/init rw random.trust_cpu=on"
	lower := strings.ToLower(imageID)
	ipArg := ""
	if vm.Network.AssignmentMethod != models.NetworkModeDHCP &&
		vm.Network.IPAddress != "" && vm.Network.IPAddress != "0.0.0.0" {
		ipArg = fmt.Sprintf("ip=%s::%s:255.255.255.0:%s:eth0:off ",
			vm.Network.IPAddress, "192.168.100.1", vm.ID)
	}
	switch {
	case strings.HasPrefix(lower, "ubuntu-") || strings.HasPrefix(lower, "debian-") ||
		strings.HasPrefix(lower, "rocky-") || strings.HasPrefix(lower, "almalinux-") ||
		strings.HasPrefix(lower, "fedora-") || strings.HasPrefix(lower, "archlinux-") ||
		strings.HasPrefix(lower, "sql-server-") || strings.Contains(lower, "debian"):
		bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodules root=" + rootDevice +
			" rw net.ifnames=0 biosdevname=0 random.trust_cpu=on " + ipArg +
			"ds=nocloud;s=/var/lib/cloud/seed/nocloud/ cloud-init.dslist=NoCloud"
	case strings.HasPrefix(lower, "alpine-"):
		bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodules root=" + rootDevice +
			" rw init=/init net.ifnames=0 random.trust_cpu=on rc_parallel=NO " + ipArg +
			"ds=nocloud;s=/var/lib/cloud/seed/nocloud/ cloud-init.dslist=NoCloud"
	}

	tapDevice := vm.Network.TAPDevice
	if tapDevice == "" {
		// Generate a deterministic TAP name using the same hash as the firecracker
		// manager's tapNameFor method, so StartMicroVM creates the right device.
		hash := 0
		for _, c := range vm.ID {
			hash = hash*31 + int(c)
		}
		num := hash % 1000
		if num < 0 {
			num = -num
		}
		tapDevice = fmt.Sprintf("tap%d", num)
		vm.Network.TAPDevice = tapDevice // keep in-memory in sync
		log.Printf("🔧 Generated tap device name %s for VM %s (was empty)", tapDevice, vm.ID)
	}

	drives := []map[string]interface{}{
		{
			"drive_id":       "rootfs",
			"path_on_host":   rootfsPath,
			"is_root_device": true,
			"is_read_only":   false,
		},
	}
	// Re-attach cloud-init ISO if it was recorded in metadata and still exists
	if vm.Metadata.Labels != nil {
		if ciPath := vm.Metadata.Labels["cloudinit_path"]; ciPath != "" {
			if _, err := os.Stat(ciPath); err == nil {
				drives = append(drives, map[string]interface{}{
					"drive_id":       "cloudinit",
					"path_on_host":   ciPath,
					"is_root_device": false,
					"is_read_only":   true,
				})
			}
		}
	}

	cfg := map[string]interface{}{
		"boot-source": map[string]interface{}{
			"kernel_image_path": kernelPath,
			"boot_args":         bootArgs,
		},
		"drives": drives,
		"machine-config": map[string]interface{}{
			"vcpu_count":   vm.Resources.VCPUs,
			"mem_size_mib": vm.Resources.MemoryMB,
		},
		"network-interfaces": []map[string]interface{}{
			{
				"iface_id":      "eth0",
				"guest_mac":     vm.Network.MACAddress,
				"host_dev_name": tapDevice,
			},
		},
		"balloon": map[string]interface{}{
			"amount_mib":               0,
			"deflate_on_oom":           true,
			"stats_polling_interval_s": firecracker.StatsPollingIntervalS(),
		},
		"logger": map[string]interface{}{
			"log_path":        filepath.Join(microvmDir, "firecracker.log"),
			"level":           firecracker.LogLevel(),
			"show_level":      true,
			"show_log_origin": false,
		},
	}

	if err := os.MkdirAll(microvmDir, 0755); err != nil {
		return fmt.Errorf("failed to create microvm dir: %w", err)
	}
	configData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	configPath := filepath.Join(microvmDir, "config.json")
	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		return fmt.Errorf("failed to write config.json: %w", err)
	}
	log.Printf("🔧 Regenerated config.json for VM %s (image=%s tap=%s)", vm.ID, imageID, tapDevice)
	return nil
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool returns environment variable as boolean or default
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
}

func main() {
	// Parse command-line flags
	var (
		networkBridge = flag.String("network-bridge", getEnvOrDefault("MICROVM_NETWORK_BRIDGE", "lxdbr0"), "Network bridge name")
		networkIP     = flag.String("network-ip", getEnvOrDefault("MICROVM_NETWORK_IP", "10.130.146.1"), "Network bridge IP")
		networkCIDR   = flag.String("network-cidr", getEnvOrDefault("MICROVM_NETWORK_CIDR", "10.130.146.0/24"), "Network CIDR")

		// The OVN overlay subnet. Only needed on a host that runs both backends,
		// where it must not overlap the bridge subnet above.
		ovnNetworkIP   = flag.String("ovn-network-ip", getEnvOrDefault(network.OverlayIPEnvVar, ""), "OVN overlay gateway IP (empty: same as -network-ip)")
		ovnNetworkCIDR = flag.String("ovn-network-cidr", getEnvOrDefault(network.OverlayCIDREnvVar, ""), "OVN overlay CIDR (empty: same as -network-cidr)")
		disableDHCP    = flag.Bool("disable-dhcp", getEnvBool("MICROVM_DISABLE_DHCP", true), "Disable internal DHCP (use external dnsmasq)")
		manageNAT      = flag.Bool("manage-nat", getEnvBool("MICROVM_MANAGE_NAT", false), "Manage NAT rules (false if bridge handles it)")
		hostsFileDir   = flag.String("hosts-file-dir", getEnvOrDefault("MICROVM_HOSTS_DIR", "/var/snap/lxd/common/lxd/networks/lxdbr0"), "Directory for dnsmasq hosts file")
	)
	flag.Parse()

	fmt.Println("🚀 Starting MicroVM HTTP Server")
	fmt.Println("===============================")
	fmt.Printf("🌐 Network Bridge: %s\n", *networkBridge)
	fmt.Printf("🌐 Network IP: %s\n", *networkIP)
	fmt.Printf("🌐 Network CIDR: %s\n", *networkCIDR)
	fmt.Printf("🌐 Disable DHCP: %v\n", *disableDHCP)
	fmt.Printf("🌐 Manage NAT: %v\n", *manageNAT)
	fmt.Printf("🌐 Hosts File Dir: %s\n", *hostsFileDir)

	fmt.Println("📋 Loading configuration...")
	// Load configuration with fallback to default
	cfg, err := config.LoadConfig("")
	if err != nil {
		fmt.Printf("⚠️  Config load failed, using defaults: %v\n", err)
		cfg = config.DefaultConfig()
	}

	// Determine work directory
	workDir := resolveWorkDir()
	fmt.Println("📁 Using work directory:", workDir)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		log.Fatalf("Failed to create work directory %s: %v", workDir, err)
	}

	fmt.Println("🔧 Initializing firecracker manager...")
	// Initialize firecracker manager with working configuration
	// Allow overriding kernel path via environment to test newer/lighter kernels
	// Allow overriding kernel path via env for lightweight Firecracker kernels
	kernelPath := os.Getenv("MICROVM_KERNEL_PATH")
	if kernelPath == "" {
		// Preferred default: installed release kernel if present
		releaseKernel := "/opt/vmlet/kernels/vmlinux"
		if _, err := os.Stat(releaseKernel); err == nil {
			kernelPath = releaseKernel
			fmt.Println("🧰 Using bundled release kernel:", kernelPath)
		} else {
			kernelPath = "/var/lib/firecracker-containerd/runtime/default-vmlinux.bin"
			fmt.Println("🧰 Using fallback kernel:", kernelPath)
		}
	} else {
		fmt.Println("🧪 Using custom kernel:", kernelPath)
	}

	fcConfig := &firecracker.Config{
		WorkDir:           workDir,
		FirecrackerBin:    "/usr/local/bin/firecracker",
		KernelPath:        kernelPath,
		UseJailer:         false,
		NetworkBridgeName: *networkBridge,
		NetworkBridgeIP:   *networkIP,
		NetworkBridgeCIDR: *networkCIDR,
		// Empty means "same as the bridge fields", which keeps a single-backend
		// host configured exactly as before.
		OVNNetworkCIDR: *ovnNetworkCIDR,
		OVNNetworkIP:   *ovnNetworkIP,
		DisableDHCP:    *disableDHCP, // Use external dnsmasq on lxdbr0
		ManageNAT:      *manageNAT,   // LXD manages NAT
		SSHPortStart:   2200,
		SSHPortEnd:     2299,
	}

	// Initialize ReservationManager for MAC/IP reservations and anti-spoofing
	fmt.Println("� Initializing reservation manager...")
	reservationMgr, err := network.NewReservationManager(
		*networkBridge, // bridge name
		*networkCIDR,   // subnet (lxdbr0 subnet)
		*hostsFileDir,  // hosts file directory
	)
	if err != nil {
		log.Printf("⚠️  Warning: Failed to create reservation manager: %v", err)
		log.Printf("⚠️  Anti-spoofing and bandwidth limiting will be disabled")
		reservationMgr = nil // Continue without reservation manager
	} else {
		fmt.Println("✅ Reservation manager initialized successfully")
		// Clean up orphaned ebtables rules from TAPs that no longer exist
		if err := reservationMgr.CleanupOrphanedEbtablesRules(); err != nil {
			log.Printf("⚠️  Warning: Failed to cleanup orphaned ebtables rules: %v", err)
		}
	}

	fmt.Println("�🔥 Creating firecracker manager...")
	manager, err := firecracker.NewManager(fcConfig, reservationMgr)
	if err != nil {
		log.Fatalf("Failed to create firecracker manager: %v", err)
	}
	fmt.Println("✅ Firecracker manager created successfully")

	// Initialize image manager
	fmt.Println("🖼️  Initializing image manager...")
	// Resolve images directory (decoupled) via MICROVM_IMAGES_DIR env, default to workDir/images
	imagesDir := os.Getenv("MICROVM_IMAGES_DIR")
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	if !filepath.IsAbs(imagesDir) {
		if abs, err := filepath.Abs(imagesDir); err == nil {
			imagesDir = abs
		}
	}
	imageManager := images.NewImageManager(workDir, imagesDir, filepath.Join(workDir, "overlays"))
	if err := imageManager.Initialize(); err != nil {
		log.Fatalf("Failed to initialize image manager: %v", err)
	}
	fmt.Println("✅ Image manager initialized")

	// Set up persistence file path
	persistFile := filepath.Join(workDir, "vms-state.json")

	server := &HTTPServer{
		manager:        manager,
		imageManager:   imageManager,
		reservationMgr: reservationMgr,
		vms:            make(map[string]*models.MicroVMInstance),
		persistFile:    persistFile,
		customerQuotas: make(map[string]*CustomerQuota),
		quotasFile:     filepath.Join(workDir, "quotas.json"),
		tapCache:       make(map[string]tapCacheEntry),

		// Initialize performance caches
		vmInfoCache: make(map[string]*VMStatusResponse),
		cacheExpiry: 30 * time.Second, // Cache VM info for 30 seconds
		// Queue defaults (can be tuned via env)
		maxCreateWorkers: func() int {
			if v := os.Getenv("MICROVM_CREATE_WORKERS"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					return n
				}
			}
			return 1 // strict sequential by default
		}(),
		consoleTokens: make(map[string]*ConsoleToken),
		nodeDomain:    os.Getenv("MICROVM_NODE_DOMAIN"),
	}

	// Initialize SQLite storage for VM state persistence
	fmt.Println("💾 Initializing SQLite storage for VM state...")
	dbPath := filepath.Join(workDir, "vms-state.db")
	sqliteStorage, err := storage.NewSQLiteStorage(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize SQLite storage: %v", err)
	}
	server.vmStateStorage = sqliteStorage
	fmt.Printf("✅ SQLite storage initialized: %s\n", dbPath)

	// Wire any persisted VPCs into the OVN backend.
	if nets, err := sqliteStorage.LoadAllNetworks(); err == nil && len(nets) > 0 {
		if b := server.manager.GetOVNBackend(); b != nil {
			b.SetNetworks(toOVNNetworks(nets))
			fmt.Printf("🌐 Loaded %d network(s) into OVN backend\n", len(nets))
		}
	}

	// Migrate from JSON if exists
	if _, err := os.Stat(persistFile); err == nil {
		fmt.Println("📂 Migrating VM state from JSON to SQLite...")
		if err := server.vmStateStorage.MigrateFromJSON(persistFile); err != nil {
			log.Printf("⚠️  Failed to migrate from JSON: %v", err)
		} else {
			// Backup old JSON file
			backupFile := persistFile + ".backup"
			if err := os.Rename(persistFile, backupFile); err != nil {
				log.Printf("⚠️  Failed to backup JSON file: %v", err)
			} else {
				fmt.Printf("✅ JSON file backed up to: %s\n", backupFile)
			}
		}
	}

	// Initialize creation queue and workers
	server.creationQueue = make(chan string, 1024)
	server.startCreationWorkers()

	// Start console token cleanup goroutine
	go server.consoleTokenCleanup()

	// Initialize automatic balloon manager
	enableAutoBaloon := os.Getenv("MICROVM_AUTO_BALLOON") != "false" // enabled by default
	// Optional: configure check interval (ms)
	checkInterval := 60 * time.Second
	if v := os.Getenv("MICROVM_BALLOON_CHECK_INTERVAL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			checkInterval = time.Duration(ms) * time.Millisecond
		}
	}
	// Optional: configure host memory low threshold (MB)
	hostLowMB := uint64(2048)
	if v := os.Getenv("MICROVM_BALLOON_HOST_LOW_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hostLowMB = uint64(n)
		}
	}
	// Optional: configure fast poll interval via env (ms)
	var fastPoll time.Duration
	if v := os.Getenv("MICROVM_BALLOON_FAST_POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			fastPoll = time.Duration(ms) * time.Millisecond
		}
	}
	// Optional: configure safety buffer (MB). 0 means adaptive per-VM.
	var safetyMB uint64
	if v := os.Getenv("MICROVM_BALLOON_SAFETY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			safetyMB = uint64(n)
		}
	}
	// Optional: configure max balloon percentage (0-100). Default 95%.
	maxBalloonPercent := 0.95
	if v := os.Getenv("MICROVM_BALLOON_MAX_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			maxBalloonPercent = float64(n) / 100.0
		}
	}
	// Optional: always reclaim from idle VMs even when host isn't low on memory
	alwaysReclaimIdle := os.Getenv("MICROVM_BALLOON_ALWAYS_RECLAIM") == "true"

	server.balloonManager = balloon.NewAutoBalloonManager(server, balloon.Config{
		Enabled:           enableAutoBaloon,
		CheckInterval:     checkInterval,
		FastPollInterval:  fastPoll, // 0 uses default fast poll
		HostMemoryLowMB:   hostLowMB,
		VMMemoryLowMB:     128,               // Keep at least 128MB free in VM
		VMSafetyBufferMB:  safetyMB,          // 0 => adaptive safety buffer
		ReclaimPercentage: 0.7,               // Reclaim 70% of VM free memory (more aggressive)
		MaxBalloonPercent: maxBalloonPercent, // Default 95% max balloon
		AlwaysReclaimIdle: alwaysReclaimIdle, // Reclaim from idle VMs always
	})

	// Set work directory for profile persistence and load existing profiles
	server.balloonManager.SetWorkDir(workDir)
	server.balloonManager.LoadAllProfiles()

	if enableAutoBaloon {
		server.balloonManager.Start()
		fmt.Println("🎈 Auto-balloon memory management: ENABLED")
	} else {
		fmt.Println("🎈 Auto-balloon memory management: DISABLED")
	}

	// Initialize per-VM autoscaler
	autoscaleInterval := 10 * time.Second
	if v := os.Getenv("MICROVM_AUTOSCALE_INTERVAL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			autoscaleInterval = time.Duration(n) * time.Second
		}
	}
	server.vmAutoscaler = autoscale.NewVMAutoscaler(server, autoscaleInterval)
	server.vmAutoscaler.Start()
	fmt.Printf("🔄 Per-VM autoscaler: ENABLED (check interval: %v)\n", autoscaleInterval)

	// Ensure bridge network is set up
	fmt.Println("🌉 Ensuring bridge network is configured...")
	if err := server.ensureBridgeNetwork(); err != nil {
		log.Printf("⚠️  Failed to set up bridge network: %v", err)
	}

	// Load existing VMs from disk
	fmt.Println("📂 Loading VM state from disk...")
	if err := server.loadVMsFromDisk(); err != nil {
		log.Printf("⚠️  Failed to load VM state: %v", err)
	}

	// Load per-customer quotas (absent file = no quotas configured)
	fmt.Println("📋 Loading customer quotas...")
	server.loadCustomerQuotas()

	// Reconcile VM states — detect stale running VMs from previous session
	fmt.Println("🔍 Reconciling VM states with actual processes...")
	server.reconcileVMStates()

	// Restore IPv4 addons (macvlan interfaces, NAT rules, policy routing)
	fmt.Println("🌐 Restoring IPv4 addon configurations...")
	server.restoreIPv4Addons()

	// Restore IPv4 NAT fallback rules for IPv6-only VMs
	fmt.Println("🌐 Restoring IPv4 NAT fallback rules...")
	server.restoreIPv4FallbackNAT()

	// Restore NDP proxy entries for running IPv6 VMs (critical for MACVLAN mode)
	fmt.Println("🌐 Restoring NDP proxy entries...")
	server.restoreNDPProxyEntries()

	// Auto-start VMs that were running before shutdown
	fmt.Println("🚀 Auto-starting VMs that were previously running...")
	server.autoStartVMs()

	// Clean up orphaned systemd units from previous runs
	fmt.Println("🧹 Cleaning up orphaned systemd units...")
	server.cleanupOrphanedSystemdUnits()

	// Start background task to monitor and fix TAP devices
	go server.startTAPDeviceMonitor()

	// Start background task for periodic VM status updates
	go server.startVMStatusUpdater()
	// Start reboot watcher to auto-restart VMs on guest 'reboot'
	go server.startRebootWatcher()

	// Start periodic VM log sweeper: cap console.log/firecracker.log so long-running
	// guests cannot fill the node's disk (issue #28).
	go server.startVMLogSweeper()

	// Start periodic orphaned unit reaper
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				server.cleanupOrphanedSystemdUnits()
			}
		}
	}()

	// Configure IPv6 forwarding rules for IPv6-only VMs
	// This ensures external IPv6 traffic can reach VMs on the bridge
	ipv6Subnet := os.Getenv("MICROVM_IPV6_SUBNET")
	if ipv6Subnet != "" {
		fmt.Printf("🌐 Configuring IPv6 forwarding for subnet: %s\n", ipv6Subnet)
		if err := setupIPv6Forwarding(ipv6Subnet); err != nil {
			log.Printf("⚠️  Warning: Failed to configure IPv6 forwarding: %v", err)
		} else {
			fmt.Println("✅ IPv6 forwarding rules configured")
		}
	}

	// Setup routes
	router := mux.NewRouter()
	// Enable StrictSlash to redirect /path/ to /path (handles trailing slash issue)
	router.StrictSlash(true)
	router.HandleFunc("/health", server.healthHandler).Methods("GET")
	router.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":    ServiceName,
			"version":    ServiceVersion,
			"build_date": "2025-11-23",
			"features": []string{
				"MACVLAN network mode",
				"DHCP on external bridges",
				"Memory balloon stats",
				"Traffic monitoring",
				"EC2 metadata disabled",
				"Cloud-init NoCloud",
				"Auto-start VMs",
			},
		})
	}).Methods("GET")
	// Resource stats endpoint (VM specific)
	router.HandleFunc("/vms/{id}/stats", server.getVMStatsHandler).Methods("GET")
	// Host resource stats endpoint
	router.HandleFunc("/resources/host", server.getHostResourcesHandler).Methods("GET")
	// Queue visibility endpoint
	router.HandleFunc("/queue/creation", server.getCreationQueueStatus).Methods("GET")
	router.HandleFunc("/vms", server.createVMHandler).Methods("POST")
	router.HandleFunc("/vms", server.listVMsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}", server.getVMHandler).Methods("GET")
	// Health: kernel details endpoint
	router.HandleFunc("/health/kernel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		unameOut, _ := exec.Command("uname", "-a").CombinedOutput()
		// capture the already-resolved kernelPath variable defined above
		kp := kernelPath
		resp := map[string]interface{}{
			"kernel_path":   kp,
			"uname":         strings.TrimSpace(string(unameOut)),
			"custom_kernel": strings.Contains(kp, "/firecracker/resources/x86_64/"),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}).Methods("GET")
	router.HandleFunc("/vms/{id}/start", server.startVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/stop", server.stopVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/sync-config", server.syncVMConfigHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/changeip", server.changeIPHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/reinstall", server.reinstallVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/password", server.resetPasswordHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/ssh-keys", server.updateSSHKeysHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/console/fix", server.repairConsoleHandler).Methods("POST")
	router.HandleFunc("/vms/{id}", server.deleteVMHandler).Methods("DELETE")
	// Console endpoints
	router.HandleFunc("/vms/{id}/console", server.getConsoleHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/console/logs", server.getConsoleLogsHandler).Methods("GET")
	// Web console token API (billing panel calls this to get a temporary URL)
	router.HandleFunc("/vms/{id}/console/token", server.createConsoleTokenHandler).Methods("POST")
	// Web console page and WebSocket (served to end user's browser)
	router.HandleFunc("/console/{token}", server.serveConsolePageHandler).Methods("GET")
	router.HandleFunc("/console/{token}/ws", server.consoleWebSocketHandler).Methods("GET")

	// SSH endpoints
	router.HandleFunc("/vms/{id}/ssh", server.getSSHAccessHandler).Methods("GET")
	// Auto-start management endpoint
	router.HandleFunc("/vms/{id}/auto-start", server.setAutoStartHandler).Methods("PUT")
	// Resource management endpoints
	router.HandleFunc("/vms/{id}/resources", server.updateVMResourcesHandler).Methods("PUT")
	router.HandleFunc("/resources/availability", server.getResourceAvailabilityHandler).Methods("GET")
	// Bandwidth management endpoint (live update without restart)
	router.HandleFunc("/vms/{id}/bandwidth", server.updateBandwidthHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/bandwidth", server.getBandwidthHandler).Methods("GET")
	// Autoscaling endpoints
	router.HandleFunc("/vms/{id}/autoscale", server.setAutoscaleHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/autoscale", server.getAutoscaleHandler).Methods("GET")
	// Memory guarantee endpoints (smart balloon)
	router.HandleFunc("/vms/{id}/memory/guarantee", server.setMemoryGuaranteeHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/memory/profile", server.getMemoryProfileHandler).Methods("GET")
	// Traffic and memory stats endpoints
	router.HandleFunc("/vms/{id}/traffic", server.getTrafficHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/memory", server.getMemoryBillingHandler).Methods("GET")

	// IPv4 Add-on endpoints (add/remove/update public IPv4 addresses)
	router.HandleFunc("/vms/{id}/ipv4", server.addIPv4AddonHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/ipv4", server.listIPv4AddonsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/ipv4/{ip}", server.updateIPv4AddonHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/ipv4/{ip}", server.removeIPv4AddonHandler).Methods("DELETE")
	router.HandleFunc("/vms/{id}/floating-ip", server.attachFloatingIPHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/floating-ip", server.listFloatingIPsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/floating-ip/{address}", server.detachFloatingIPHandler).Methods("DELETE")

	// Network (VPC) management endpoints
	router.HandleFunc("/networks", server.createNetworkHandler).Methods("POST")
	router.HandleFunc("/networks", server.listNetworksHandler).Methods("GET")
	router.HandleFunc("/network-modes", server.listNetworkModesHandler).Methods("GET")
	router.HandleFunc("/networks/{id}", server.getNetworkHandler).Methods("GET")
	router.HandleFunc("/networks/{id}", server.deleteNetworkHandler).Methods("DELETE")

	// Port forwarding endpoints
	router.HandleFunc("/vms/{id}/port-forward", server.attachPortForwardHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/port-forwards", server.listPortForwardsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/port-forward/{external_ip}/{port}", server.detachPortForwardHandler).Methods("DELETE")
	// Snapshot management endpoints (ZFS-backed VMs only)
	router.HandleFunc("/vms/{id}/snapshots", server.createSnapshotHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/snapshots", server.listSnapshotsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/snapshots/{snapshot}", server.rollbackSnapshotHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/snapshots/{snapshot}", server.deleteSnapshotHandler).Methods("DELETE")
	// VM cloning endpoint (create new VM from existing VM snapshot)
	router.HandleFunc("/vms/{id}/clone", server.cloneVMHandler).Methods("POST")
	// Cross-node VM migration (live migration + cold export/import).
	// Fail-soft: if the service cannot start (e.g. firecracker binary
	// missing), the node keeps serving its existing API without the
	// migration surface.
	migHandler, migErr := server.buildMigrationHandler()
	if migErr != nil {
		log.Printf("⚠️  Migration endpoints disabled: %v", migErr)
	} else {
		// peer-facing (called by the other node's vmlet)
		router.HandleFunc("/migration/info", migHandler.InfoHandler).Methods("GET")
		router.HandleFunc("/migration/prepare", migHandler.PrepareHandler).Methods("POST")
		router.HandleFunc("/migration/{id}/disk", migHandler.DiskStreamHandler).Methods("PUT")
		router.HandleFunc("/migration/{id}/memory", migHandler.MemStreamHandler).Methods("PUT")
		router.HandleFunc("/migration/{id}/memextents", migHandler.MemExtentsHandler).Methods("PUT")
		router.HandleFunc("/migration/{id}/vmstate", migHandler.VMStateHandler).Methods("PUT")
		router.HandleFunc("/migration/{id}/file", migHandler.FileHandler).Methods("PUT")
		router.HandleFunc("/migration/{id}/restore", migHandler.RestoreHandler).Methods("POST")
		router.HandleFunc("/migration/{id}/confirm", migHandler.ConfirmHandler).Methods("POST")
		router.HandleFunc("/migration/{id}/abort", migHandler.AbortIncomingHandler).Methods("POST")
		router.HandleFunc("/migration/{id}/status", migHandler.IncomingStatusHandler).Methods("GET")
		// portal-facing (operators / Django panel)
		router.HandleFunc("/vms/{id}/migrate", migHandler.MigrateHandler).Methods("POST")
		router.HandleFunc("/vms/{id}/migration", migHandler.OutgoingStatusHandler).Methods("GET")
		router.HandleFunc("/vms/{id}/migration/abort", migHandler.AbortOutgoingHandler).Methods("POST")
		router.HandleFunc("/vms/{id}/export", migHandler.ExportHandler).Methods("POST")
		router.HandleFunc("/vms/import", migHandler.ImportHandler).Methods("POST")
		router.HandleFunc("/exports", migHandler.ListExportsHandler).Methods("GET")
		router.HandleFunc("/exports/{name}", migHandler.DownloadExportHandler).Methods("GET")
		router.HandleFunc("/exports/{name}", migHandler.DeleteExportHandler).Methods("DELETE")
	}
	// Image management endpoints
	router.HandleFunc("/images", server.listImagesHandler).Methods("GET")
	router.HandleFunc("/images/{id}", server.getImageHandler).Methods("GET")
	router.HandleFunc("/images/{id}/download", server.downloadImageHandler).Methods("POST")

	// Admin endpoints
	router.HandleFunc("/admin/cleanup-orphaned", server.cleanupOrphanedHandler).Methods("POST")
	router.HandleFunc("/admin/orphaned-units", server.listOrphanedUnitsHandler).Methods("GET")
	router.HandleFunc("/admin/quotas/{customer}", server.getQuotaHandler).Methods("GET")
	router.HandleFunc("/admin/quotas/{customer}", server.setQuotaHandler).Methods("PUT")

	// Prometheus metrics (dependency-free exposition)
	metrics.RegisterAll()
	router.HandleFunc("/metrics", metrics.Handler).Methods("GET")

	// Setup HTTP server
	port := cfg.Server.Port
	if envPort := os.Getenv("PORT"); envPort != "" {
		if parsedPort, err := strconv.Atoi(envPort); err == nil {
			port = parsedPort
		}
	}

	// CORS middleware wrapper
	corsHandler := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set CORS headers for all responses
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User-ID, X-Requested-With")
			w.Header().Set("Access-Control-Max-Age", "86400")

			// Handle preflight OPTIONS request
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: corsHandler(router),
	}

	// Start server in goroutine
	go func() {
		fmt.Printf("════════════════════════════════════════════════════════════\n")
		fmt.Printf("  %s v%s\n", ServiceName, ServiceVersion)
		fmt.Printf("════════════════════════════════════════════════════════════\n")
		fmt.Printf("🌐 HTTP server listening on port %d\n", port)
		fmt.Printf("📋 Available endpoints:\n")
		fmt.Printf("   GET  /health                - Health check\n")
		fmt.Printf("   GET  /resources/host        - Host resource stats\n")
		fmt.Printf("   POST /vms                   - Create VM\n")
		fmt.Printf("   GET  /vms                   - List VMs\n")
		fmt.Printf("   GET  /vms/{id}              - Get VM status\n")
		fmt.Printf("   POST /vms/{id}/start        - Start VM\n")
		fmt.Printf("   POST /vms/{id}/stop         - Stop VM\n")
		fmt.Printf("   DELETE /vms/{id}            - Delete VM\n")
		fmt.Printf("   GET  /vms/{id}/console      - Get console access info\n")
		fmt.Printf("   GET  /vms/{id}/console/logs - Stream console logs\n")
		fmt.Printf("   GET  /vms/{id}/ssh          - Get SSH access info\n")
		fmt.Printf("   PUT  /vms/{id}/auto-start   - Set auto-start flag\n")
		fmt.Printf("   PUT  /vms/{id}/resources    - Update VM resources\n")
		fmt.Printf("   PUT  /vms/{id}/bandwidth    - Update bandwidth limits (live)\n")
		fmt.Printf("   GET  /vms/{id}/bandwidth    - Get bandwidth limits\n")
		fmt.Printf("   GET  /resources/availability - Get resource availability\n")
		fmt.Printf("   GET  /images                - List available OS images\n")
		fmt.Printf("   GET  /images/{id}           - Get image details\n")
		fmt.Printf("   POST /images/{id}/download  - Download OS image\n")
		fmt.Printf("\n💡 Example usage:\n")
		fmt.Printf("   curl http://localhost:%d/images\n", port)
		fmt.Printf("   curl http://localhost:%d/resources/host\n", port)
		fmt.Printf("   curl -X POST http://localhost:%d/images/ubuntu-22.04/download\n", port)
		fmt.Printf("   curl -X POST http://localhost:%d/vms -H 'Content-Type: application/json' -d '{\"vcpus\":2,\"memory_mb\":1024,\"image_id\":\"ubuntu-24.04\",\"network\":{\"assignment_method\":\"dhcp\",\"mac_address\":\"02:fc:00:00:00:15\",\"ip_address\":\"10.130.146.150\",\"enable_ssh\":true},\"cloudinit\":{\"username\":\"ubuntu\",\"password\":\"ubuntu123\"}}'\n", port)
		fmt.Printf("   curl http://localhost:%d/vms\n", port)
		fmt.Printf("\n")

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Second signal forces immediate exit (operator escape hatch).
	go func() {
		<-sigChan
		fmt.Printf("\n🛑 Second signal received — forcing immediate exit\n")
		os.Exit(1)
	}()

	fmt.Printf("\n🛑 Shutting down server...\n")

	// Persist VM state FIRST: systemd gives us 30s total (TimeoutStopSec=30),
	// and a slow HTTP drain below must not be allowed to eat the whole budget
	// before the final persist runs.
	server.gracefulShutdown()

	// Stop balloon manager and autoscaler
	if server.balloonManager != nil {
		server.balloonManager.Stop()
	}
	if server.vmAutoscaler != nil {
		server.vmAutoscaler.Stop()
	}

	// Shutdown HTTP server (bounded well inside the systemd timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	fmt.Printf("✅ Server shutdown complete\n")
}

func (s *HTTPServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	// Perform comprehensive health check
	systemHealth, err := s.manager.PerformSystemHealthCheck()
	if err != nil {
		http.Error(w, fmt.Sprintf("Health check failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"status":         systemHealth.Overall,
		"timestamp":      time.Now(),
		"vms":            s.vmCount(),
		"system_health":  systemHealth,
		"service_uptime": time.Since(systemHealth.CheckedAt).String(),
	}

	// Set appropriate HTTP status based on health
	statusCode := http.StatusOK
	switch systemHealth.Overall {
	case "unhealthy":
		statusCode = http.StatusServiceUnavailable
	case "degraded":
		statusCode = http.StatusPartialContent
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(response)
}

// getHostResourcesHandler returns overall host resource statistics
func (s *HTTPServer) getHostResourcesHandler(w http.ResponseWriter, r *http.Request) {
	monitor := s.getOrInitResourceMonitor()

	stats, err := monitor.GetHostResourceStats()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to collect host stats: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// CustomerQuota caps the resources a single customer may hold across live VMs.
// A zero value means unlimited for that dimension (the default: no behavior change
// unless an admin configures quotas).
type CustomerQuota struct {
	MaxVMs       int    `json:"max_vms"`
	MaxVCPUs     uint32 `json:"max_vcpus"`
	MaxMemoryMB  uint32 `json:"max_memory_mb"`
	MaxStorageMB uint32 `json:"max_storage_mb"`
}

// QuotaExceededError names which quota dimension tripped.
type QuotaExceededError struct {
	CustomerID string
	Dimension  string // "vms" | "vcpus" | "memory_mb" | "storage_mb"
	Limit      uint64
	Used       uint64
	Wanted     uint64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("customer %s quota exceeded for %s: limit %d, used %d, requested %d",
		e.CustomerID, e.Dimension, e.Limit, e.Used, e.Wanted)
}

// customerUsage sums live (creating/running) VM resources for a customer.
func (s *HTTPServer) customerUsage(customerID string) (vms int, vcpus uint32, memoryMB uint32, storageMB uint32) {
	s.vmsMutex.RLock()
	defer s.vmsMutex.RUnlock()

	for _, vm := range s.vms {
		if vm.Metadata.CustomerID != customerID {
			continue
		}
		if vm.Status != models.MicroVMStatusCreating && vm.Status != models.MicroVMStatusRunning {
			continue
		}
		vms++
		vcpus += vm.Resources.VCPUs
		memoryMB += vm.Resources.MemoryMB
		storageMB += vm.Resources.StorageMB
	}
	return vms, vcpus, memoryMB, storageMB
}

// checkCustomerQuota verifies a new VM fits within the customer's quota.
// Unknown customers have no quota configured and are always allowed.
func (s *HTTPServer) checkCustomerQuota(customerID string, vcpus, memoryMB, storageMB uint32) error {
	s.vmsMutex.RLock()
	quota, configured := s.customerQuotas[customerID]
	s.vmsMutex.RUnlock()

	if !configured || quota == nil {
		return nil
	}

	usedVMs, usedVCPUs, usedMem, usedStorage := s.customerUsage(customerID)

	if quota.MaxVMs > 0 && usedVMs+1 > quota.MaxVMs {
		return &QuotaExceededError{CustomerID: customerID, Dimension: "vms",
			Limit: uint64(quota.MaxVMs), Used: uint64(usedVMs), Wanted: 1}
	}
	if quota.MaxVCPUs > 0 && usedVCPUs+vcpus > quota.MaxVCPUs {
		return &QuotaExceededError{CustomerID: customerID, Dimension: "vcpus",
			Limit: uint64(quota.MaxVCPUs), Used: uint64(usedVCPUs), Wanted: uint64(vcpus)}
	}
	if quota.MaxMemoryMB > 0 && usedMem+memoryMB > quota.MaxMemoryMB {
		return &QuotaExceededError{CustomerID: customerID, Dimension: "memory_mb",
			Limit: uint64(quota.MaxMemoryMB), Used: uint64(usedMem), Wanted: uint64(memoryMB)}
	}
	if quota.MaxStorageMB > 0 && usedStorage+storageMB > quota.MaxStorageMB {
		return &QuotaExceededError{CustomerID: customerID, Dimension: "storage_mb",
			Limit: uint64(quota.MaxStorageMB), Used: uint64(usedStorage), Wanted: uint64(storageMB)}
	}
	return nil
}

// loadCustomerQuotas reads persisted quotas (non-fatal if absent).
func (s *HTTPServer) loadCustomerQuotas() {
	data, err := os.ReadFile(s.quotasFile)
	if err != nil {
		return
	}
	var quotas map[string]*CustomerQuota
	if err := json.Unmarshal(data, &quotas); err != nil {
		log.Printf("⚠️  Failed to parse quotas file %s: %v", s.quotasFile, err)
		return
	}
	s.vmsMutex.Lock()
	s.customerQuotas = quotas
	s.vmsMutex.Unlock()
	fmt.Printf("📋 Loaded quotas for %d customers from %s\n", len(quotas), s.quotasFile)
}

// saveCustomerQuotas persists quotas (best-effort).
func (s *HTTPServer) saveCustomerQuotas() {
	s.vmsMutex.RLock()
	data, err := json.MarshalIndent(s.customerQuotas, "", "  ")
	s.vmsMutex.RUnlock()
	if err != nil {
		log.Printf("⚠️  Failed to marshal quotas: %v", err)
		return
	}
	if err := os.WriteFile(s.quotasFile, data, 0644); err != nil {
		log.Printf("⚠️  Failed to save quotas to %s: %v", s.quotasFile, err)
	}
}

// getQuotaHandler returns the quota (plus live usage) for a customer.
func (s *HTTPServer) getQuotaHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	customerID := vars["customer"]

	s.vmsMutex.RLock()
	quota, configured := s.customerQuotas[customerID]
	s.vmsMutex.RUnlock()

	usedVMs, usedVCPUs, usedMem, usedStorage := s.customerUsage(customerID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"customer_id": customerID,
		"configured":  configured,
		"quota":       quota,
		"usage": map[string]interface{}{
			"vms": usedVMs, "vcpus": usedVCPUs,
			"memory_mb": usedMem, "storage_mb": usedStorage,
		},
	})
}

// setQuotaHandler creates or replaces a customer's quota.
func (s *HTTPServer) setQuotaHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	customerID := vars["customer"]

	var quota CustomerQuota
	if err := json.NewDecoder(r.Body).Decode(&quota); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	s.vmsMutex.Lock()
	if s.customerQuotas == nil {
		s.customerQuotas = make(map[string]*CustomerQuota)
	}
	s.customerQuotas[customerID] = &quota
	s.vmsMutex.Unlock()
	s.saveCustomerQuotas()

	log.Printf("📋 Quota set for customer %s: %+v", customerID, quota)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"customer_id": customerID,
		"quota":       quota,
	})
}

// pickLegacyNetworkMode is the pre-overlay auto-detect: DHCP mode for an
// external bridge, NAT otherwise.
func pickLegacyNetworkMode() models.NetworkMode {
	// Auto-detect network mode based on bridge configuration
	// If MICROVM_DHCP_BRIDGE or MICROVM_NETWORK_BRIDGE is set to an external bridge (lxdbr0, br0, virbr0),
	// use DHCP mode. Otherwise, use NAT mode with internal bridge.
	dhcpBridge := os.Getenv("MICROVM_DHCP_BRIDGE")
	if dhcpBridge == "" {
		dhcpBridge = os.Getenv("MICROVM_NETWORK_BRIDGE")
	}

	fmt.Printf("   🔍 DEBUG: MICROVM_DHCP_BRIDGE='%s'\n", dhcpBridge)

	// Check if using external bridge (not fc-br0/microvm-br0 managed bridges)
	if dhcpBridge != "" && dhcpBridge != "fc-br0" && dhcpBridge != "microvm-br0" {
		fmt.Printf("   ✅ Auto-detected DHCP mode (external bridge: %s)\n", dhcpBridge)
		return models.NetworkModeDHCP // Use DHCP for external bridges
	}
	fmt.Printf("   ℹ️  Using NAT mode (internal bridge or no bridge configured)\n")
	return models.NetworkModeNAT // default to static mode for internal bridges
}

func (s *HTTPServer) createVMHandler(w http.ResponseWriter, r *http.Request) {
	// Read the raw request body for debugging
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Log the raw request for debugging
	fmt.Printf("🔍 Debug: Raw API request received:\n%s\n", string(bodyBytes))

	var req CreateVMRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		fmt.Printf("❌ Debug: Failed to parse request: %v\n", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Merge "network" into "network_config" if needed (support both field names)
	// Check both "mode" and "assignment_method" fields
	networkFromReq := req.Network.Mode != "" || req.Network.AssignmentMethod != "" || req.Network.ParentInterface != ""
	networkConfigEmpty := req.NetworkConfig.Mode == "" && req.NetworkConfig.AssignmentMethod == ""
	if networkFromReq && networkConfigEmpty {
		req.NetworkConfig = req.Network
		fmt.Printf("🔍 Debug: Merged 'network' into 'network_config'\n")
	}

	// Log what we parsed
	fmt.Printf("🔍 Debug: Parsed backend request:\n")

	// Handle both new and legacy request formats
	var vcpus, memoryMB, storageMB uint32
	var imageID string

	// New format (from CLI)
	if req.ResourceSpecs.VCPUs > 0 || req.ResourceSpecs.MemoryMB > 0 || req.ResourceSpecs.StorageMB > 0 {
		vcpus = req.ResourceSpecs.VCPUs
		memoryMB = req.ResourceSpecs.MemoryMB
		storageMB = req.ResourceSpecs.StorageMB
		fmt.Printf("   Using new format - ResourceSpecs:\n")
		fmt.Printf("     VCPUs: %d\n", vcpus)
		fmt.Printf("     Memory: %d MB\n", memoryMB)
		fmt.Printf("     Storage: %d MB\n", storageMB)
	} else {
		// Legacy format
		vcpus = req.VCPUs
		memoryMB = req.MemoryMB
		storageMB = req.StorageMB
		fmt.Printf("   Using legacy format:\n")
		fmt.Printf("     VCPUs: %d\n", vcpus)
		fmt.Printf("     Memory: %d MB\n", memoryMB)
		fmt.Printf("     Storage: %d MB\n", storageMB)
	}

	// Handle image configuration
	if req.ImageConfig.Distribution != "" && req.ImageConfig.Version != "" {
		// New format: resolve distribution:version to imageID
		imageID = fmt.Sprintf("%s-%s", req.ImageConfig.Distribution, req.ImageConfig.Version)
		fmt.Printf("   Image from ImageConfig:\n")
		fmt.Printf("     Distribution: %s\n", req.ImageConfig.Distribution)
		fmt.Printf("     Version: %s\n", req.ImageConfig.Version)
		fmt.Printf("     Resolved ImageID: %s\n", imageID)
		if len(req.ImageConfig.Packages) > 0 {
			fmt.Printf("     Packages: %s\n", strings.Join(req.ImageConfig.Packages, ", "))
		}
	} else if req.Image != "" {
		// Legacy format: "image" field
		imageID = req.Image
		fmt.Printf("   Image from 'image' field: %s\n", imageID)
	} else if req.ImageID != "" {
		// Legacy format: "image_id" field
		imageID = req.ImageID
		fmt.Printf("   Image from legacy ImageID: %s\n", imageID)
	} else {
		// Default
		imageID = "alpine-3.18"
		fmt.Printf("   Using default image: %s\n", imageID)
	}

	// Handle network mode configuration
	var networkMode models.NetworkMode

	// Debug: Log network config
	fmt.Printf("🔍 DEBUG NetworkConfig: Mode='%s', AssignmentMethod='%s', MAC='%s', IP='%s', ParentInterface='%s'\n",
		req.NetworkConfig.Mode,
		req.NetworkConfig.AssignmentMethod,
		req.NetworkConfig.MACAddress,
		req.NetworkConfig.IPAddress,
		req.NetworkConfig.ParentInterface)

	// Support both "mode" (preferred) and "assignment_method" (legacy) fields
	networkModeStr := req.NetworkConfig.Mode
	if networkModeStr == "" {
		networkModeStr = req.NetworkConfig.AssignmentMethod
	}

	if networkModeStr != "" {
		switch strings.ToLower(networkModeStr) {
		case "static":
			networkMode = models.NetworkModeNAT // static with unique IP on fc-br0
		case "nat":
			networkMode = models.NetworkModeNAT
		case "dhcp":
			networkMode = models.NetworkModeDHCP // DHCP server mode
		case "macvlan":
			networkMode = models.NetworkModeMACVLAN // OVH macvlan mode
			fmt.Printf("   ✅ Using MACVLAN mode with parent interface: %s\n", req.NetworkConfig.ParentInterface)
		case "ipv6-only", "ipv6only", "ipv6":
			networkMode = models.NetworkModeIPv6Only // IPv6 public + IPv4 private NAT
			fmt.Printf("   ✅ Using IPv6-only mode (public IPv6 + private IPv4 NAT)\n")
		case "ovn":
			networkMode = models.NetworkModeOVN
			fmt.Printf("   Using OVN mode (logical overlay via br-int)\n")
		default:
			networkMode = models.NetworkModeNAT
		}
	} else {
		// An OVN node prefers the overlay; a bridge node keeps the existing
		// auto-detect below. Both are per-VM overridable by "mode".
		if s.manager != nil && s.manager.DefaultNetworkMode() == models.NetworkModeOVN {
			networkMode = models.NetworkModeOVN
			fmt.Printf("   ℹ️  Using OVN mode (this host runs the overlay by default)\n")
		} else {
			networkMode = pickLegacyNetworkMode()
		}
	}

	// Refuse a mode this host cannot serve, rather than booting a VM that
	// cannot reach anyone. The API answers 400 here; a VM created another way
	// fails at the manager with the same reason.
	if s.manager != nil {
		if err := s.manager.CheckNetworkMode(networkMode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if vcpus == 0 {
		vcpus = 1
	}
	if storageMB == 0 {
		// Set default based on image type
		if strings.HasPrefix(imageID, "sql-server-") {
			storageMB = 5120 // 5GB for SQL Server images (3.1GB image + headroom)
		} else {
			storageMB = 2048 // 2GB default for OS images
		}
	}

	// Validate image selection
	if _, err := s.imageManager.GetImage(imageID); err != nil {
		fmt.Printf("❌ Debug: Image validation failed for '%s': %v\n", imageID, err)
		http.Error(w, fmt.Sprintf("Invalid image ID: %s", imageID), http.StatusBadRequest)
		return
	}

	fmt.Printf("✅ Debug: Image validation passed for '%s'\n", imageID)

	log.Printf("🔍 DEBUG LINE 467: About to create VM instance\n")

	// Create VM instance with unique UUID-based ID
	vmID := fmt.Sprintf("vm-%s", uuid.New().String()[:8])
	if req.Name != "" {
		vmID = req.Name
	}

	fmt.Printf("🔍 DEBUG LINE 475: VM ID set to %s\n", vmID)

	microvm := &models.MicroVMInstance{
		ID:     vmID,
		NodeID: "local-node",
		Status: models.MicroVMStatusCreating,
		Resources: models.ResourceSpecs{
			VCPUs:     vcpus,
			MemoryMB:  memoryMB,
			StorageMB: storageMB,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: networkMode,
			NetworkID:        req.NetworkConfig.NetworkID,
			MACAddress:       req.NetworkConfig.MACAddress, // Custom MAC from request
			IPAddress:        req.NetworkConfig.IPAddress,  // Custom IP from request (static IP for OVN)
			IPv6Address:      req.NetworkConfig.IPv6Address,
			IPv6Gateway:      req.NetworkConfig.IPv6Gateway,
			IPv6PrefixLength: req.NetworkConfig.IPv6PrefixLength,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
		Metadata: models.MicroVMMetadata{
			CustomerID: func() string {
				if req.CustomerID != "" {
					return req.CustomerID
				}
				return "http-api"
			}(),
			CreatedAt: time.Now(),
			AutoStart: true, // New VMs should auto-start by default
		},
	}

	// For IPv6-only mode, ensure IPv4 NAT is enabled
	if networkMode == models.NetworkModeIPv6Only {
		microvm.Network.IPv4NAT = true
		fmt.Printf("   ✅ IPv4 NAT enabled for IPv6-only mode\n")
	}

	// Log custom network configuration if provided
	if req.NetworkConfig.MACAddress != "" {
		fmt.Printf("   Custom MAC: %s\n", req.NetworkConfig.MACAddress)
	}
	if req.NetworkConfig.IPAddress != "" {
		fmt.Printf("   Custom IP: %s\n", req.NetworkConfig.IPAddress)
	}
	if req.NetworkConfig.IPv6Address != "" {
		fmt.Printf("   Custom IPv6: %s/%d (gw=%s)\n",
			req.NetworkConfig.IPv6Address,
			func() int {
				if req.NetworkConfig.IPv6PrefixLength > 0 {
					return req.NetworkConfig.IPv6PrefixLength
				}
				return 64
			}(),
			req.NetworkConfig.IPv6Gateway)
	}

	// Handle bandwidth limits - support both struct format and string format
	if req.NetworkConfig.BandwidthLimit != nil {
		fmt.Printf("   Bandwidth Limit (struct): Ingress=%d Mbps, Egress=%d Mbps\n",
			req.NetworkConfig.BandwidthLimit.IngressMbps,
			req.NetworkConfig.BandwidthLimit.EgressMbps)

		// Convert bandwidth limits to string format for storage
		if req.NetworkConfig.BandwidthLimit.IngressMbps > 0 {
			microvm.Network.BandwidthIngress = fmt.Sprintf("%dmbit", req.NetworkConfig.BandwidthLimit.IngressMbps)
		}
		if req.NetworkConfig.BandwidthLimit.EgressMbps > 0 {
			microvm.Network.BandwidthEgress = fmt.Sprintf("%dmbit", req.NetworkConfig.BandwidthLimit.EgressMbps)
		}
	}

	// Also support direct string format (e.g., "100mbit", "50Mbit")
	if req.NetworkConfig.BandwidthIngress != "" {
		microvm.Network.BandwidthIngress = req.NetworkConfig.BandwidthIngress
		fmt.Printf("   Bandwidth Ingress (string): %s\n", req.NetworkConfig.BandwidthIngress)
	}
	if req.NetworkConfig.BandwidthEgress != "" {
		microvm.Network.BandwidthEgress = req.NetworkConfig.BandwidthEgress
		fmt.Printf("   Bandwidth Egress (string): %s\n", req.NetworkConfig.BandwidthEgress)
	}

	// Store the selected image ID, image name, and VM name in metadata
	if microvm.Metadata.Labels == nil {
		microvm.Metadata.Labels = make(map[string]string)
	}
	microvm.Metadata.Labels["image_id"] = imageID
	log.Printf("🏷️  Debug: Setting metadata labels...\n")
	if req.ImageConfig.Distribution != "" {
		imageName := fmt.Sprintf("%s:%s", req.ImageConfig.Distribution, req.ImageConfig.Version)
		microvm.Metadata.Labels["image_name"] = imageName
		log.Printf("   image_name: %s (from ImageConfig)\n", imageName)
	} else {
		microvm.Metadata.Labels["image_name"] = imageID
		log.Printf("   image_name: %s (from imageID)\n", imageID)
	}
	if req.Name != "" {
		microvm.Metadata.Labels["name"] = req.Name
		log.Printf("   name: %s\n", req.Name)
	}
	log.Printf("   Total labels: %d\n", len(microvm.Metadata.Labels))

	// Merge user-provided metadata labels (e.g., static_ip overrides)
	if len(req.Metadata) > 0 {
		for k, v := range req.Metadata {
			if v == "" {
				continue
			}
			microvm.Metadata.Labels[k] = v
		}
	}

	// Also copy from Labels field (alias for Metadata)
	if len(req.Labels) > 0 {
		for k, v := range req.Labels {
			if v == "" {
				continue
			}
			microvm.Metadata.Labels[k] = v
		}
	}

	// Auto-enable vsock for SQL Server images (for firewall management via host)
	if strings.HasPrefix(imageID, "sql-server-") {
		microvm.Vsock = &models.VsockConfig{
			Enabled:  true,
			GuestCID: 3, // Guest CID (must be >= 3, 0-2 are reserved by Firecracker)
			UDSPath:  "vsock.sock",
		}
		fmt.Printf("🔌 Auto-enabled vsock for SQL Server image: %s\n", imageID)
	}

	// Store cloud-init configuration in metadata for VM creation
	if req.CloudInit != nil {
		fmt.Printf("☁️  Cloud-Init configuration received:\n")
		if req.CloudInit.Hostname != "" {
			microvm.Metadata.Labels["cloudinit_hostname"] = req.CloudInit.Hostname
			fmt.Printf("   Hostname: %s\n", req.CloudInit.Hostname)
		}
		if req.CloudInit.Username != "" {
			microvm.Metadata.Labels["cloudinit_username"] = req.CloudInit.Username
			fmt.Printf("   Username: %s\n", req.CloudInit.Username)
		}
		if req.CloudInit.PasswordHash != "" {
			microvm.Metadata.Labels["cloudinit_password_hash"] = req.CloudInit.PasswordHash
			fmt.Printf("   PasswordHash: (provided)\n")
		} else if req.CloudInit.Password != "" {
			microvm.Metadata.Labels["cloudinit_password"] = req.CloudInit.Password
			fmt.Printf("   Password: (provided)\n")
		}
		if len(req.CloudInit.SSHKeys) > 0 {
			storeCloudInitSSHKeys(microvm.Metadata.Labels, req.CloudInit.SSHKeys)
			fmt.Printf("   SSH Keys: %d key(s)\n", len(req.CloudInit.SSHKeys))
		}
		// Optional write_files and run_commands
		if len(req.CloudInit.WriteFiles) > 0 {
			if b, err := json.Marshal(req.CloudInit.WriteFiles); err == nil {
				microvm.Metadata.Labels["cloudinit_write_files"] = string(b)
				fmt.Printf("   write_files: %d entries\n", len(req.CloudInit.WriteFiles))
			}
		}
		if len(req.CloudInit.RunCommands) > 0 {
			if b, err := json.Marshal(req.CloudInit.RunCommands); err == nil {
				microvm.Metadata.Labels["cloudinit_run_commands"] = string(b)
				fmt.Printf("   run_commands: %d entries\n", len(req.CloudInit.RunCommands))
			}
		}
		if len(req.ImageConfig.Packages) > 0 {
			microvm.Metadata.Labels["cloudinit_packages"] = strings.Join(req.ImageConfig.Packages, ",")
			fmt.Printf("   Packages: %s\n", strings.Join(req.ImageConfig.Packages, ", "))
		}
	}

	fmt.Printf("🔧 Debug: Created VM instance with:\n")
	fmt.Printf("   ID: %s\n", vmID)
	fmt.Printf("   Resources: %d vCPUs, %d MB RAM, %d MB storage\n", vcpus, memoryMB, storageMB)
	fmt.Printf("   Image ID: %s\n", imageID)
	fmt.Printf("   Network Mode: %s\n", networkMode)

	// For IPv6-only mode, ensure IPv4 NAT is enabled and assign IPv4 NAT address
	if networkMode == models.NetworkModeIPv6Only {
		microvm.Network.IPv4NAT = true

		// If IPv4 NAT address not already assigned, derive it from IPv6 or assign next available
		if microvm.Network.IPv4NATAddress == "" {
			// Derive from IPv6 address if available
			if microvm.Network.IPv6Address != "" {
				microvm.Network.IPv4NATAddress = deriveIPv4NATFromIPv6(microvm.Network.IPv6Address)
				// Store gateway in metadata for cloud-init
				if microvm.Metadata.Labels == nil {
					microvm.Metadata.Labels = make(map[string]string)
				}
				microvm.Metadata.Labels["ipv4_nat_gateway"] = "10.100.0.1"
				fmt.Printf("   ✅ Derived IPv4 NAT address from IPv6: %s (gateway: 10.100.0.1)\n",
					microvm.Network.IPv4NATAddress)
			}
		}
		fmt.Printf("   ✅ IPv4 NAT enabled for IPv6-only mode\n")
	}

	// Store VM
	// Enforce per-customer quotas BEFORE anything is stored, billed, or enqueued.
	if err := s.checkCustomerQuota(
		microvm.Metadata.CustomerID,
		microvm.Resources.VCPUs,
		microvm.Resources.MemoryMB,
		microvm.Resources.StorageMB,
	); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "quota_exceeded",
			"message": err.Error(),
		})
		return
	}
	s.setVM(vmID, microvm)

	// Persist to disk
	s.persistVMState()

	// Enqueue VM for queued creation instead of starting immediately
	s.enqueueVMCreation(vmID)
	microvm.Status = models.MicroVMStatusCreating
	s.persistVMState() // Return response
	response := CreateVMResponse{
		ID:        vmID,
		Status:    string(microvm.Status),
		IPAddress: microvm.Network.IPAddress,
		SSHPort:   microvm.Network.SSHPort,
		Message:   "VM enqueued for creation",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)
}

func (s *HTTPServer) listVMsHandler(w http.ResponseWriter, r *http.Request) {
	var vms []VMStatusResponse

	// Try to use cached data first for better performance
	s.vmCacheMutex.RLock()
	cacheValid := time.Since(s.lastCacheUpdate) < s.cacheExpiry
	if cacheValid && len(s.vmInfoCache) > 0 {
		// Use cached data
		for _, cachedVM := range s.vmInfoCache {
			vms = append(vms, *cachedVM)
		}
		s.vmCacheMutex.RUnlock()

		// Sort by creation time (newest first) for consistent ordering
		sortVMsByCreatedAt(vms, true)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache-Status", "hit")
		json.NewEncoder(w).Encode(vms)
		return
	}
	s.vmCacheMutex.RUnlock()

	// Cache miss or expired - build response from live data
	for _, vm := range s.snapshotVMMap() {
		status := s.buildVMStatusResponse(vm)
		vms = append(vms, *status)

		// Cache the individual VM info for future requests
		s.setCachedVMInfo(vm.ID, status)
	}

	// Sort by creation time (newest first) for consistent ordering
	sortVMsByCreatedAt(vms, true)

	// Update cache timestamp
	s.vmCacheMutex.Lock()
	s.lastCacheUpdate = time.Now()
	s.vmCacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache-Status", "miss")
	json.NewEncoder(w).Encode(vms)
}

// sortVMsByCreatedAt sorts VMs by creation time
func sortVMsByCreatedAt(vms []VMStatusResponse, newestFirst bool) {
	// Use a simple bubble sort for small lists or implement proper sort
	for i := 0; i < len(vms); i++ {
		for j := i + 1; j < len(vms); j++ {
			shouldSwap := false
			if newestFirst {
				// Newest first: sort descending
				shouldSwap = vms[j].CreatedAt.After(vms[i].CreatedAt)
			} else {
				// Oldest first: sort ascending
				shouldSwap = vms[j].CreatedAt.Before(vms[i].CreatedAt)
			}
			if shouldSwap {
				vms[i], vms[j] = vms[j], vms[i]
			}
		}
	}
}

func (s *HTTPServer) getVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Try to use cached data first
	if cachedInfo, found := s.getCachedVMInfo(vmID); found {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache-Status", "hit")
		json.NewEncoder(w).Encode(cachedInfo)
		return
	}

	// Cache miss - get fresh data
	// Get current status from manager
	if status, err := s.manager.GetMicroVMStatus(context.Background(), vmID); err == nil {
		vm.Status = status
	}

	response := s.buildVMStatusResponse(vm)

	// Cache the response for future requests
	s.setCachedVMInfo(vmID, response)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache-Status", "miss")
	json.NewEncoder(w).Encode(response)
}

func (s *HTTPServer) startVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Auto-regenerate config.json from DB state if missing (e.g. after JSON→SQLite migration).
	workDir := resolveWorkDir()
	configPath := filepath.Join(workDir, "microvms", vmID, "config.json")
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		log.Printf("⚠️  config.json missing for VM %s — regenerating from DB state", vmID)
		if regenErr := regenerateVMConfigJSON(vm); regenErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   "config_regen_failed",
				"message": fmt.Sprintf("config.json missing and could not be regenerated: %v", regenErr),
				"status":  string(vm.Status),
			})
			return
		}
		log.Printf("✅ config.json regenerated for VM %s", vmID)
	}

	// Attempt start even if status is 'creating' to recover stuck VMs.
	if err := s.manager.StartMicroVM(context.Background(), vm); err != nil {
		// Provide JSON error for scripts expecting JSON
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "start_failed",
			"message": fmt.Sprintf("Failed to start VM: %v", err),
			"status":  vm.Status,
		})
		return
	}

	// Setup IPv4 NAT rules for IPv6-only VMs with NAT fallback
	if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
		// Get public IP from first network interface
		publicIP, err := s.getPublicIPv4()
		if err == nil && publicIP != "" {
			if err := setupIPv4FallbackNAT(vm.Network.IPv4NATAddress, publicIP); err != nil {
				log.Printf("⚠️  Failed to setup IPv4 NAT for VM %s: %v", vmID, err)
			}
		}
	}

	// Kick off fast DHCP discovery if applicable (manual start scenario)
	if vm.Network.AssignmentMethod == models.NetworkModeDHCP && (vm.Network.IPAddress == "" || vm.Network.IPAddress == "0.0.0.0") {
		go s.startFastDHCPDiscovery(vm.ID, 90*time.Second, 3*time.Second)
	}

	// Enable auto-start when VM is started
	vm.SetAutoStart(true)

	// Invalidate caches after VM start
	s.invalidateTAPCache(vmID)
	s.invalidateVMCache(vmID)

	// Persist status change
	s.persistVMState()

	response := map[string]interface{}{
		"message": "VM start successful",
		"status":  string(vm.Status),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *HTTPServer) stopVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	if err := s.manager.StopMicroVM(context.Background(), vm, false); err != nil {
		http.Error(w, fmt.Sprintf("Failed to stop VM: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("✅ VM %s stopped successfully, status is now: %s\n", vmID, vm.Status)

	// Disable auto-start when VM is stopped
	vm.SetAutoStart(false)

	// Invalidate caches after VM stop
	s.invalidateTAPCache(vmID)
	s.invalidateVMCache(vmID)

	// Persist status change
	s.persistVMState()

	response := map[string]interface{}{
		"message":    "VM stop initiated",
		"status":     string(vm.Status),
		"auto_start": vm.ShouldAutoStart(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// changeIPHandler changes the IP and MAC address of a DHCP-mode VM.
// The VM is stopped, anti-spoofing rules are removed, Firecracker config.json
// is updated with the new MAC, the in-memory model is updated, and the VM is restarted.
// Both mac_address and ip_address are required in the request body.
func (s *HTTPServer) changeIPHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "VM not found"})
		return
	}

	// Parse request body
	var req struct {
		MACAddress string `json:"mac_address"`
		IPAddress  string `json:"ip_address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}

	// Validate: both fields required
	if req.MACAddress == "" || req.IPAddress == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "both mac_address and ip_address are required"})
		return
	}

	// Validate MAC address format
	if _, err := net.ParseMAC(req.MACAddress); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid mac_address format: " + err.Error()})
		return
	}

	// Validate IP address format
	if ip := net.ParseIP(req.IPAddress); ip == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid ip_address format"})
		return
	}

	// Only allow changeip for DHCP mode VMs
	if vm.Network.AssignmentMethod != models.NetworkModeDHCP {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("changeip only supported for DHCP mode VMs (current mode: %s)", vm.Network.AssignmentMethod),
		})
		return
	}

	// Reject if VM is in a transitional state
	if vm.Status == models.MicroVMStatusCreating || vm.Status == "reinstalling" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("cannot change IP while VM is in '%s' state", vm.Status),
		})
		return
	}

	// Save old values for response and rollback
	oldMAC := vm.Network.MACAddress
	oldIP := vm.Network.IPAddress

	// No-op check
	if oldMAC == req.MACAddress && oldIP == req.IPAddress {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "no changes needed, IP and MAC are already set to requested values",
			"old_mac": oldMAC,
			"new_mac": req.MACAddress,
			"old_ip":  oldIP,
			"new_ip":  req.IPAddress,
			"status":  string(vm.Status),
		})
		return
	}

	log.Printf("🔄 Changing IP/MAC for VM %s: MAC %s → %s, IP %s → %s\n", vmID, oldMAC, req.MACAddress, oldIP, req.IPAddress)

	// Serialize against reinstall/password ops on the same VM: an IP change
	// stops and starts the machine and rewrites its config; racing another
	// mutating op leaves the anti-spoofing rules or the disk inconsistent.
	if !s.tryBeginVMOp(vmID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall or password change is already in progress for this VM; wait for it to finish",
		})
		return
	}
	defer s.endVMOp(vmID)

	// Step 1: Stop the VM if it's running
	wasRunning := vm.IsRunning() || s.manager.IsVMProcessRunning(vmID)
	if wasRunning {
		log.Printf("🛑 Stopping VM %s for IP/MAC change...\n", vmID)
		if err := s.manager.StopMicroVM(context.Background(), vm, false); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to stop VM: " + err.Error()})
			return
		}
		// Wait for VM to fully stop
		for i := 0; i < 20; i++ {
			if !s.manager.IsVMProcessRunning(vmID) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Step 2: Remove old anti-spoofing and DDoS protection rules
	if vm.Network.TAPDevice != "" {
		log.Printf("🧹 Removing old anti-spoofing rules for TAP %s (MAC: %s, IP: %s)\n", vm.Network.TAPDevice, oldMAC, oldIP)
		s.manager.RemoveAntiSpoofing(vm.Network.TAPDevice, oldMAC, oldIP, vm.Network.IPv6Address)
		// Also remove old DDoS rules so they can be cleanly reapplied after restart
		s.manager.GetNetworkManager().RemoveDDoSProtection(vm.Network.TAPDevice)
	}

	// Step 3: Update Firecracker config.json with new MAC
	if err := s.manager.UpdateFirecrackerConfigMAC(vmID, req.MACAddress); err != nil {
		// Rollback: re-apply old anti-spoofing
		if vm.Network.TAPDevice != "" {
			s.manager.GetNetworkManager().ApplyAntiSpoofing(vm.Network.TAPDevice, oldMAC, oldIP, vm.Network.IPv6Address)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to update Firecracker config: " + err.Error()})
		return
	}

	// Step 4: Update in-memory model
	vm.Network.MACAddress = req.MACAddress
	vm.Network.IPAddress = req.IPAddress

	// Step 5: Persist state
	s.persistVMState()
	s.invalidateTAPCache(vmID)
	s.invalidateVMCache(vmID)

	// Step 6: Start the VM back up (if it was running before)
	if wasRunning {
		log.Printf("🚀 Starting VM %s with new IP/MAC...\n", vmID)
		if err := s.manager.StartMicroVM(context.Background(), vm); err != nil {
			log.Printf("⚠️  Failed to restart VM %s after IP/MAC change: %v\n", vmID, err)
			// Don't rollback the config - the new values are correct, VM just needs manual start
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   "IP/MAC changed but VM failed to restart: " + err.Error(),
				"old_mac": oldMAC,
				"new_mac": req.MACAddress,
				"old_ip":  oldIP,
				"new_ip":  req.IPAddress,
				"status":  string(vm.Status),
				"note":    "IP/MAC change was applied. Try starting the VM manually.",
			})
			return
		}
		s.persistVMState()
	}

	// Step 7: Apply new anti-spoofing and DDoS protection rules for the new MAC/IP
	// This MUST happen after StartMicroVM so the TAP device is active
	if vm.Network.TAPDevice != "" {
		log.Printf("🔒 Applying new anti-spoofing rules for TAP %s (MAC: %s, IP: %s)\n", vm.Network.TAPDevice, req.MACAddress, req.IPAddress)
		if err := s.manager.GetNetworkManager().ApplyAntiSpoofing(vm.Network.TAPDevice, req.MACAddress, req.IPAddress, vm.Network.IPv6Address); err != nil {
			log.Printf("⚠️  Failed to apply new anti-spoofing rules for VM %s: %v\n", vmID, err)
		}
		// Also reapply DDoS protection (rate limiting) for the TAP
		nm := s.manager.GetNetworkManager()
		if err := nm.ApplyDDoSProtection(vm.Network.TAPDevice); err != nil {
			log.Printf("⚠️  Failed to apply DDoS protection for VM %s: %v\n", vmID, err)
		}
	}

	log.Printf("✅ Successfully changed IP/MAC for VM %s: MAC %s → %s, IP %s → %s\n", vmID, oldMAC, req.MACAddress, oldIP, req.IPAddress)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "IP/MAC changed successfully",
		"old_mac": oldMAC,
		"new_mac": req.MACAddress,
		"old_ip":  oldIP,
		"new_ip":  req.IPAddress,
		"status":  string(vm.Status),
	})
}

func (s *HTTPServer) deleteVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	log.Printf("Deleting VM %s (status: %s)\n", vmID, vm.Status)

	// Delete VM using manager
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := s.manager.DeleteMicroVM(ctx, vm); err != nil {
		log.Printf("Error deleting VM %s: %v\n", vmID, err)
		http.Error(w, fmt.Sprintf("Failed to delete VM: %v", err), http.StatusInternalServerError)
		return
	}

	// Cleanup antispoofing rules for TAP device
	if s.reservationMgr != nil && vm.Network.TAPDevice != "" {
		if err := s.reservationMgr.RemoveSpoofingProtection(
			vm.Network.TAPDevice,
			vm.Network.MACAddress,
			vm.Network.IPAddress,
		); err != nil {
			log.Printf("⚠️  Failed to cleanup antispoofing rules for %s: %v", vm.Network.TAPDevice, err)
		} else {
			log.Printf("✅ Cleaned up antispoofing rules for VM %s (%s)", vmID, vm.Network.TAPDevice)
		}
	}

	// Cleanup IPv4 NAT rules for IPv6-only VMs
	if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
		cleanupIPv4FallbackNAT(vm.Network.IPv4NATAddress)
		log.Printf("✅ Cleaned up IPv4 NAT rules for VM %s (%s)", vmID, vm.Network.IPv4NATAddress)
	}

	// Cleanup IPv4 addon rules and interfaces
	if len(vm.Network.IPv4Addons) > 0 {
		for _, addon := range vm.Network.IPv4Addons {
			if err := removeIPv4Addon(addon.InterfaceID, addon.IPv4Address, vm.Network.IPv4NATAddress, addon.RoutingTable); err != nil {
				log.Printf("⚠️  Failed to fully remove IPv4 addon %s for VM %s: %v", addon.IPv4Address, vmID, err)
			} else {
				log.Printf("✅ Cleaned up IPv4 addon (including interface) for VM %s (%s)", vmID, addon.IPv4Address)
			}
		}
	}

	// Remove from internal map
	s.deleteVMEntry(vmID)

	// Delete memory profile for this VM
	if s.balloonManager != nil {
		if err := s.balloonManager.DeleteProfile(vmID); err != nil {
			log.Printf("⚠️  Failed to delete profile for VM %s: %v", vmID, err)
		}
	}

	// Invalidate caches after VM deletion
	s.invalidateTAPCache(vmID)
	s.invalidateVMCache(vmID)

	// Drop per-VM metric series so cardinality can't grow unboundedly
	metrics.DeleteVM(vmID)

	// Persist to disk
	s.persistVMState()

	log.Printf("Successfully deleted VM %s\n", vmID)

	response := map[string]interface{}{
		"message": "VM deleted successfully",
		"id":      vmID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// reinstallVMHandler reinstalls a VM (delete and recreate with same configuration)
func (s *HTTPServer) reinstallVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	currentImageID := ""
	if vm.Metadata.Labels != nil {
		currentImageID = vm.Metadata.Labels["image_id"]
	}

	// Parse optional request body allowing image override, flags, and cloud-init
	type ReinstallRequest struct {
		ImageID      string           `json:"image_id,omitempty"`
		KeepNetwork  *bool            `json:"keep_network,omitempty"`  // preserve MAC/IP (default true)
		PreserveID   *bool            `json:"preserve_id,omitempty"`   // keep same VM ID (default true)
		ForceRestart bool             `json:"force_restart,omitempty"` // future use
		CloudInit    *CloudInitConfig `json:"cloudinit,omitempty"`     // cloud-init configuration for reinstall
	}
	var req ReinstallRequest
	if r.Body != nil {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&req) // best-effort; empty body OK
	}
	// Apply defaults for pointer fields (nil = use default)
	if req.PreserveID == nil {
		defaultPreserveID := true
		req.PreserveID = &defaultPreserveID
	}
	if req.KeepNetwork == nil {
		defaultKeepNetwork := true
		req.KeepNetwork = &defaultKeepNetwork
	}

	effectiveImageID := strings.TrimSpace(req.ImageID)
	if effectiveImageID == "" {
		effectiveImageID = currentImageID
	}
	if effectiveImageID == "" {
		http.Error(w, "Current VM has no image_id label; please specify image_id explicitly", http.StatusBadRequest)
		return
	}

	// Serialize mutating operations per VM: a reinstall racing another
	// reinstall (or a password reset) on the same VM fights over the same
	// zvol and TAP and leaves a half-deleted machine behind. A pending
	// creation counts too — deleting the VM mid-creation is what produces
	// "dataset is busy" and duplicate Firecracker processes.
	if s.vmCreationPending(vmID) {
		log.Printf("Reinstall for VM %s rejected: a previous reinstall is still creating", vmID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall is already in progress for this VM; wait for it to finish before starting another",
		})
		return
	}
	if !s.tryBeginVMOp(vmID) {
		log.Printf("Reinstall for VM %s rejected: another operation is in progress", vmID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall or password change is already in progress for this VM; wait for it to finish",
		})
		return
	}
	defer s.endVMOp(vmID)

	log.Printf("Reinstalling VM %s (image override: %s, keep_network: %v, preserve_id: %v)\n", vmID, effectiveImageID, *req.KeepNetwork, *req.PreserveID)

	// Validate target image
	if _, err := s.imageManager.GetImage(effectiveImageID); err != nil {
		http.Error(w, fmt.Sprintf("Invalid image_id: %s", effectiveImageID), http.StatusBadRequest)
		return
	}

	// Capture network settings if requested
	preservedNetwork := vm.Network

	// Delete existing VM (ensure it's stopped first)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Stop on the live process, not on the tracked status: a previous
	// reinstall can leave the status at "creating" while a Firecracker
	// process is alive and holding the zvol open — `zfs destroy` then fails
	// with "dataset is busy" and the disk silently survives the reinstall.
	if vm.Status == models.MicroVMStatusRunning || s.manager.IsVMProcessRunning(vmID) {
		_ = s.manager.StopMicroVM(ctx, vm, true)
	}
	if err := s.manager.DeleteMicroVM(ctx, vm); err != nil {
		log.Printf("Error deleting VM %s for reinstall: %v\n", vmID, err)
		http.Error(w, fmt.Sprintf("Failed to delete VM: %v", err), http.StatusInternalServerError)
		return
	}

	// Remove old VM from maps & caches
	s.deleteVMEntry(vmID)
	s.invalidateTAPCache(vmID)
	s.invalidateVMCache(vmID)

	// Decide new VM ID
	newID := vmID
	if !*req.PreserveID {
		newID = fmt.Sprintf("vm-%s", uuid.New().String()[:8])
	}

	// Build new instance config
	newVM := &models.MicroVMInstance{
		ID:     newID,
		NodeID: vm.NodeID,
		Resources: models.ResourceSpecs{
			VCPUs:     vm.Resources.VCPUs,
			MemoryMB:  vm.Resources.MemoryMB,
			StorageMB: vm.Resources.StorageMB,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: preservedNetwork.AssignmentMethod,
			MACAddress:       preservedNetwork.MACAddress,
			IPAddress:        preservedNetwork.IPAddress,
			IPv6Address:      preservedNetwork.IPv6Address,
			IPv6Gateway:      preservedNetwork.IPv6Gateway,
			IPv6PrefixLength: preservedNetwork.IPv6PrefixLength,
			IPv4NAT:          preservedNetwork.IPv4NAT,
			IPv4NATAddress:   preservedNetwork.IPv4NATAddress,
			BandwidthIngress: preservedNetwork.BandwidthIngress,
			BandwidthEgress:  preservedNetwork.BandwidthEgress,
			TAPDevice:        preservedNetwork.TAPDevice,
			BridgeName:       preservedNetwork.BridgeName,
		},
		Storage: models.StorageConfig{Type: vm.Storage.Type},
		Metadata: models.MicroVMMetadata{
			CustomerID: vm.Metadata.CustomerID,
			CreatedAt:  time.Now(),
			AutoStart:  vm.Metadata.AutoStart,
			Labels:     make(map[string]string),
		},
	}

	// Copy existing labels
	if vm.Metadata.Labels != nil {
		for k, v := range vm.Metadata.Labels {
			newVM.Metadata.Labels[k] = v
		}
	}
	// ...but never the reboot-persistence markers. The reboot watcher stamps
	// reuse_existing_rootfs=true on restarts; a reinstall that inherits it
	// reuses the OLD disk instead of installing a fresh one — the exact
	// "reinstall does nothing, password doesn't change" complaint.
	delete(newVM.Metadata.Labels, "reuse_existing_rootfs")
	delete(newVM.Metadata.Labels, "last_restart_error")
	delete(newVM.Metadata.Labels, "wipe_disk")

	// Set effective image metadata
	newVM.Metadata.Labels["image_id"] = effectiveImageID
	if req.ImageID != "" || newVM.Metadata.Labels["image_name"] == "" {
		newVM.Metadata.Labels["image_name"] = effectiveImageID
	}

	// A reinstall means a FRESH OS: the create path must not "persistence
	// fast-path" onto a stale zvol left behind by a failed destroy. Without
	// this, the reinstall silently reboots the old disk, cloud-init never
	// re-runs set-passwords (same instance-id), and the password the panel
	// advertises is not the one the guest accepts.
	newVM.Metadata.Labels["wipe_disk"] = "true"
	// Also rotate the instance-id so cloud-init treats it as a new instance
	// even if a leftover rootfs somehow gets reused.
	newVM.Metadata.Labels["reinstall_nonce"] = time.Now().UTC().Format("20060102T150405Z")

	// Apply cloud-init configuration from request (overrides existing labels)
	if req.CloudInit != nil {
		log.Printf("☁️  Reinstall: Applying new cloud-init configuration for VM %s\n", vmID)
		if req.CloudInit.Hostname != "" {
			newVM.Metadata.Labels["cloudinit_hostname"] = req.CloudInit.Hostname
			log.Printf("   Hostname: %s\n", req.CloudInit.Hostname)
		}
		if req.CloudInit.Username != "" {
			newVM.Metadata.Labels["cloudinit_username"] = req.CloudInit.Username
			log.Printf("   Username: %s\n", req.CloudInit.Username)
		}
		if req.CloudInit.PasswordHash != "" {
			newVM.Metadata.Labels["cloudinit_password_hash"] = req.CloudInit.PasswordHash
			// Clear plain password if hash is provided
			delete(newVM.Metadata.Labels, "cloudinit_password")
			log.Printf("   PasswordHash: (provided)\n")
		} else if req.CloudInit.Password != "" {
			newVM.Metadata.Labels["cloudinit_password"] = req.CloudInit.Password
			// Clear hash if plain password is provided
			delete(newVM.Metadata.Labels, "cloudinit_password_hash")
			log.Printf("   Password: (provided)\n")
		}
		if len(req.CloudInit.SSHKeys) > 0 {
			storeCloudInitSSHKeys(newVM.Metadata.Labels, req.CloudInit.SSHKeys)
			log.Printf("   SSH Keys: %d key(s)\n", len(req.CloudInit.SSHKeys))
		}
		if len(req.CloudInit.WriteFiles) > 0 {
			if b, err := json.Marshal(req.CloudInit.WriteFiles); err == nil {
				newVM.Metadata.Labels["cloudinit_write_files"] = string(b)
				log.Printf("   write_files: %d entries\n", len(req.CloudInit.WriteFiles))
			}
		}
		if len(req.CloudInit.RunCommands) > 0 {
			if b, err := json.Marshal(req.CloudInit.RunCommands); err == nil {
				newVM.Metadata.Labels["cloudinit_run_commands"] = string(b)
				log.Printf("   run_commands: %d entries\n", len(req.CloudInit.RunCommands))
			}
		}
	}

	// If network not preserved, clear MAC/IP (will reassign during creation)
	if !*req.KeepNetwork {
		newVM.Network.MACAddress = ""
		newVM.Network.IPAddress = ""
		newVM.Network.IPv6Address = ""
		newVM.Network.IPv6Gateway = ""
		newVM.Network.IPv6PrefixLength = 0
		newVM.Network.IPv4NATAddress = ""
	}

	// ALWAYS clear TAP device name during reinstall
	// TAP must be recreated fresh even when preserving network config
	// to avoid stale device state from previous VM instance
	newVM.Network.TAPDevice = ""

	// Insert new VM and persist
	newVM.Status = models.MicroVMStatusCreating
	s.setVM(newVM.ID, newVM)
	s.persistVMState()

	// Enqueue for creation
	s.enqueueVMCreation(newVM.ID)

	log.Printf("VM %s reinstalled as %s (image: %s)\n", vmID, newVM.ID, effectiveImageID)

	response := map[string]interface{}{
		"message":      "VM reinstall initiated",
		"vm_id":        newVM.ID,
		"original_id":  vmID,
		"status":       "creating",
		"image_id":     effectiveImageID,
		"preserved_id": *req.PreserveID,
		"network_kept": *req.KeepNetwork,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(response)
}

// resetPasswordHandler resets the root password for a VM
// mountZvolWithRetry mounts a VM's zvol, retrying while the dataset is
// still busy.
//
// Firecracker keeps the zvol open for a moment after it is stopped, and
// `mount` then fails with exit 32 (device busy). That is transient: the
// dataset is released within a second or two. A stale mount from an earlier
// failed reset is cleared first — it would make every later attempt fail
// with the same busy error.
func mountZvolWithRetry(zvolPath, mountPoint string, attempts int, delay time.Duration) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// A previous attempt may have died between mount and umount.
			if out, err := exec.Command("umount", "-l", mountPoint).CombinedOutput(); err == nil {
				log.Printf("Cleared a stale mount at %s: %s\n", mountPoint, strings.TrimSpace(string(out)))
			}
			time.Sleep(delay)
		}
		cmd := exec.Command("mount", zvolPath, mountPoint)
		if output, err := cmd.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
			continue
		}
		return nil
	}
	return lastErr
}

func (s *HTTPServer) resetPasswordHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Parse request body for new password
	type PasswordResetRequest struct {
		Password string `json:"password"`
		Username string `json:"username,omitempty"` // default: root
	}
	var req PasswordResetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if req.Password == "" {
		http.Error(w, "Password is required", http.StatusBadRequest)
		return
	}

	username := req.Username
	if username == "" {
		username = "root"
	}

	// Serialize against reinstall: a password change while the machine is
	// being deleted/recreated mounts the old zvol while the reinstall tries
	// to destroy it ("dataset is busy") and the applied password can end up
	// on a disk the reinstall then replaces. A pending creation counts too.
	if s.vmCreationPending(vmID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall is in progress for this VM; wait for it to finish before changing the password",
		})
		return
	}
	if !s.tryBeginVMOp(vmID) {
		log.Printf("Password reset for VM %s rejected: another operation is in progress", vmID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall or password change is already in progress for this VM; wait for it to finish",
		})
		return
	}
	defer s.endVMOp(vmID)

	// Check if VM is using ZFS storage
	// For now, we support password reset for all VMs as long as ZFS is enabled
	useZFS := os.Getenv("MICROVM_USE_ZFS") == "true"
	if !useZFS {
		http.Error(w, "Password reset only supported when ZFS storage is enabled", http.StatusBadRequest)
		return
	}

	// Stop VM if running
	wasRunning := vm.Status == models.MicroVMStatusRunning
	if wasRunning {
		log.Printf("Stopping VM %s for password reset\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.manager.StopMicroVM(ctx, vm, false); err != nil {
			http.Error(w, fmt.Sprintf("Failed to stop VM: %v", err), http.StatusInternalServerError)
			return
		}
		// Update status
		vm.Status = models.MicroVMStatusStopped
		s.setVM(vmID, vm)
	}

	// From here on the machine is down and MUST come back up, whatever
	// happens next: a customer who asked for a password change and got an
	// error must not be left with a dead machine.
	restartOnFailure := func() {
		if !wasRunning {
			return
		}
		log.Printf("Restarting VM %s after a failed password reset\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.manager.StartMicroVM(ctx, vm); err != nil {
			log.Printf("Failed to restart VM %s after a failed password reset: %v\n", vmID, err)
			return
		}
		vm.Status = models.MicroVMStatusRunning
		s.setVM(vmID, vm)
	}

	// Get ZFS dataset path.
	//
	// MICROVM_ZFS_DATASET already carries the pool ("default/vmlet-microvms"),
	// so the old "%s/%s/%s" build produced "default/default/vmlet-microvms/<id>"
	// — a device that does not exist, and the reason every password reset failed
	// with "mount: special device ... does not exist".
	pool := os.Getenv("MICROVM_ZFS_POOL")
	if pool == "" {
		pool = "default"
	}
	dataset := os.Getenv("MICROVM_ZFS_DATASET")
	if dataset == "" {
		dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
	}
	zfsDataset := fmt.Sprintf("%s/%s", dataset, vmID)

	// Get block device path
	zvolPath := fmt.Sprintf("/dev/zvol/%s", zfsDataset)
	if _, err := os.Stat(zvolPath); err != nil {
		log.Printf("VM disk not found for %s: %s (%v)\n", vmID, zvolPath, err)
		http.Error(w, fmt.Sprintf("VM disk not found: %s", zvolPath), http.StatusNotFound)
		restartOnFailure()
		return
	}

	// Create mount point
	mountPoint := fmt.Sprintf("/tmp/vm-reset-%s", vmID)
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create mount point: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	defer os.RemoveAll(mountPoint)

	// Mount the filesystem.
	//
	// Right after Firecracker stops, the zvol is still held open for a
	// moment and `mount` fails with exit 32 (device busy). Retry for a few
	// seconds before giving up — a busy zvol is a transient state, not a
	// broken disk, and the customer gets a password change that works.
	if err := mountZvolWithRetry(zvolPath, mountPoint, 10, time.Second); err != nil {
		log.Printf("Failed to mount %s for VM %s after %d attempts: %v\n", zvolPath, vmID, 10, err)
		http.Error(w, fmt.Sprintf("Failed to mount VM filesystem: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	defer exec.Command("umount", mountPoint).Run()

	escapedPassword := strings.ReplaceAll(req.Password, "'", "'\\''")
	chpasswdCmd := fmt.Sprintf("echo '%s:%s' | chpasswd", username, escapedPassword)
	resetCmd := exec.Command("chroot", mountPoint, "/bin/bash", "-c", chpasswdCmd)
	if output, err := resetCmd.CombinedOutput(); err != nil {
		log.Printf("Password reset failed for VM %s: %v, output: %s\n", vmID, err, string(output))
		http.Error(w, fmt.Sprintf("Failed to reset password: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}

	// Unmount
	if err := exec.Command("umount", mountPoint).Run(); err != nil {
		log.Printf("Warning: failed to unmount %s: %v\n", mountPoint, err)
	}

	// Restart VM if it was running
	if wasRunning {
		log.Printf("Restarting VM %s after password reset\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.manager.StartMicroVM(ctx, vm); err != nil {
			log.Printf("Failed to restart VM %s: %v\n", vmID, err)
			response := map[string]interface{}{
				"message":  "Password reset successful but VM failed to restart",
				"username": username,
				"vm_id":    vmID,
				"status":   "stopped",
				"error":    err.Error(),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(response)
			return
		}
		vm.Status = models.MicroVMStatusRunning
		s.setVM(vmID, vm)
	}

	// Update metadata with new password
	if vm.Metadata.Labels == nil {
		vm.Metadata.Labels = make(map[string]string)
	}
	vm.Metadata.Labels["cloudinit_password"] = req.Password
	s.setVM(vmID, vm)
	s.saveVMsToDisk()

	response := map[string]interface{}{
		"message":  "Password reset successful",
		"username": username,
		"vm_id":    vmID,
		"status":   vm.Status,
	}

	log.Printf("Password reset successful for VM %s (user: %s)\n", vmID, username)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// guestUserHome resolves a user's home directory and numeric ids from the
// mounted guest's /etc/passwd. Writing authorized_keys directly with the host
// (instead of shelling out to chroot + echo) avoids both quoting problems and
// leaving a key with the wrong owner.
func guestUserHome(mountPoint, username string) (string, int, int, error) {
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc", "passwd"))
	if err != nil {
		return "", 0, 0, fmt.Errorf("read guest /etc/passwd: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 6 || fields[0] != username {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			return "", 0, 0, fmt.Errorf("invalid uid for %s", username)
		}
		gid, err := strconv.Atoi(fields[3])
		if err != nil {
			return "", 0, 0, fmt.Errorf("invalid gid for %s", username)
		}
		home := strings.TrimSpace(fields[5])
		if home == "" {
			return "", 0, 0, fmt.Errorf("user %s has no home directory", username)
		}
		return filepath.Join(mountPoint, home), uid, gid, nil
	}
	return "", 0, 0, fmt.Errorf("user %s not found in guest", username)
}

// updateSSHKeysHandler replaces a guest user's authorized_keys.
//
// The disk is mounted, so the VM must be stopped first — mounting a live
// filesystem can corrupt it. A running VM is stopped and restarted around the
// write, matching the password-reset handler; every failure path restarts it
// so a failed key update never leaves a customer's machine down.
func (s *HTTPServer) updateSSHKeysHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	type SSHKeysUpdateRequest struct {
		SSHKeys  []string `json:"ssh_keys"`
		Username string   `json:"username,omitempty"`
	}
	var req SSHKeysUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(req.SSHKeys) > 50 {
		http.Error(w, "Too many SSH keys (max 50)", http.StatusBadRequest)
		return
	}
	keys := make([]string, 0, len(req.SSHKeys))
	seen := make(map[string]struct{}, len(req.SSHKeys))
	for _, key := range req.SSHKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		parts := strings.Fields(key)
		if len(parts) < 2 ||
			!(strings.HasPrefix(parts[0], "ssh-") ||
				strings.HasPrefix(parts[0], "ecdsa-sha2-") ||
				strings.HasPrefix(parts[0], "sk-ssh-") ||
				strings.HasPrefix(parts[0], "sk-ecdsa-")) {
			http.Error(w, "Invalid SSH public key", http.StatusBadRequest)
			return
		}
		if _, err := base64.StdEncoding.DecodeString(parts[1]); err != nil {
			if _, rawErr := base64.RawStdEncoding.DecodeString(parts[1]); rawErr != nil {
				http.Error(w, "Invalid SSH public key encoding", http.StatusBadRequest)
				return
			}
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = "root"
	}

	// Serialize against reinstall/password reset: mounting the old zvol while
	// a reinstall is replacing it fails with "dataset is busy" and can write
	// keys to a disk that is about to be destroyed.
	if s.vmCreationPending(vmID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall is in progress for this VM; wait for it to finish before changing SSH keys",
		})
		return
	}
	if !s.tryBeginVMOp(vmID) {
		log.Printf("SSH key update for VM %s rejected: another operation is in progress", vmID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "a reinstall, password change or key update is already in progress for this VM",
		})
		return
	}
	defer s.endVMOp(vmID)

	if os.Getenv("MICROVM_USE_ZFS") != "true" {
		http.Error(w, "SSH key update only supported when ZFS storage is enabled", http.StatusBadRequest)
		return
	}

	wasRunning := vm.Status == models.MicroVMStatusRunning
	if wasRunning {
		log.Printf("Stopping VM %s for SSH key update\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.manager.StopMicroVM(ctx, vm, false); err != nil {
			http.Error(w, fmt.Sprintf("Failed to stop VM: %v", err), http.StatusInternalServerError)
			return
		}
		vm.Status = models.MicroVMStatusStopped
		s.setVM(vmID, vm)
	}

	mountPoint := fmt.Sprintf("/tmp/vm-ssh-keys-%s", vmID)
	mounted := false
	unmount := func() {
		if !mounted {
			return
		}
		mounted = false
		if err := exec.Command("umount", mountPoint).Run(); err != nil {
			log.Printf("Warning: failed to unmount %s: %v\n", mountPoint, err)
		}
	}

	restartOnFailure := func() {
		// Never restart the VM with its disk still mounted on the host.
		unmount()
		if !wasRunning {
			return
		}
		log.Printf("Restarting VM %s after a failed SSH key update\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.manager.StartMicroVM(ctx, vm); err != nil {
			log.Printf("Failed to restart VM %s after a failed SSH key update: %v\n", vmID, err)
			return
		}
		vm.Status = models.MicroVMStatusRunning
		s.setVM(vmID, vm)
	}

	// MICROVM_ZFS_DATASET already carries the pool (see resetPasswordHandler).
	pool := os.Getenv("MICROVM_ZFS_POOL")
	if pool == "" {
		pool = "default"
	}
	dataset := os.Getenv("MICROVM_ZFS_DATASET")
	if dataset == "" {
		dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
	}
	zfsDataset := fmt.Sprintf("%s/%s", dataset, vmID)
	zvolPath := fmt.Sprintf("/dev/zvol/%s", zfsDataset)
	if _, err := os.Stat(zvolPath); err != nil {
		log.Printf("VM disk not found for %s: %s (%v)\n", vmID, zvolPath, err)
		http.Error(w, fmt.Sprintf("VM disk not found: %s", zvolPath), http.StatusNotFound)
		restartOnFailure()
		return
	}

	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create mount point: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	defer os.RemoveAll(mountPoint)

	if err := mountZvolWithRetry(zvolPath, mountPoint, 10, time.Second); err != nil {
		log.Printf("Failed to mount %s for VM %s after %d attempts: %v\n", zvolPath, vmID, 10, err)
		http.Error(w, fmt.Sprintf("Failed to mount VM filesystem: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	mounted = true
	defer unmount()

	homeDir, uid, gid, err := guestUserHome(mountPoint, username)
	if err != nil {
		log.Printf("SSH key update failed for VM %s: %v\n", vmID, err)
		http.Error(w, fmt.Sprintf("Failed to resolve user home: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create .ssh directory: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	if err := os.Chmod(sshDir, 0700); err != nil {
		http.Error(w, fmt.Sprintf("Failed to secure .ssh directory: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}
	if err := os.Chown(sshDir, uid, gid); err != nil {
		http.Error(w, fmt.Sprintf("Failed to set .ssh ownership: %v", err), http.StatusInternalServerError)
		restartOnFailure()
		return
	}

	authPath := filepath.Join(sshDir, "authorized_keys")
	if len(keys) == 0 {
		if err := os.Remove(authPath); err != nil && !os.IsNotExist(err) {
			http.Error(w, fmt.Sprintf("Failed to clear authorized_keys: %v", err), http.StatusInternalServerError)
			restartOnFailure()
			return
		}
	} else {
		tmpPath := authPath + ".tmp"
		content := strings.Join(keys, "\n") + "\n"
		if err := os.WriteFile(tmpPath, []byte(content), 0600); err != nil {
			http.Error(w, fmt.Sprintf("Failed to write authorized_keys: %v", err), http.StatusInternalServerError)
			restartOnFailure()
			return
		}
		if err := os.Chown(tmpPath, uid, gid); err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("Failed to set authorized_keys ownership: %v", err), http.StatusInternalServerError)
			restartOnFailure()
			return
		}
		if err := os.Chmod(tmpPath, 0600); err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("Failed to secure authorized_keys: %v", err), http.StatusInternalServerError)
			restartOnFailure()
			return
		}
		if err := os.Rename(tmpPath, authPath); err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("Failed to install authorized_keys: %v", err), http.StatusInternalServerError)
			restartOnFailure()
			return
		}
	}

	unmount()

	// Persist the new key set before restarting: StartMicroVM re-seeds the
	// guest's NoCloud user-data from these labels, so restarting first would
	// write the OLD authorized_keys back into the fresh seed.
	if vm.Metadata.Labels == nil {
		vm.Metadata.Labels = make(map[string]string)
	}
	storeCloudInitSSHKeys(vm.Metadata.Labels, keys)
	s.setVM(vmID, vm)
	s.saveVMsToDisk()

	if wasRunning {
		log.Printf("Restarting VM %s after SSH key update\n", vmID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.manager.StartMicroVM(ctx, vm); err != nil {
			log.Printf("Failed to restart VM %s: %v\n", vmID, err)
			response := map[string]interface{}{
				"message":  "SSH keys updated but VM failed to restart",
				"username": username,
				"vm_id":    vmID,
				"status":   "stopped",
				"error":    err.Error(),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(response)
			return
		}
		vm.Status = models.MicroVMStatusRunning
		s.setVM(vmID, vm)
	}

	response := map[string]interface{}{
		"message":  "SSH keys updated",
		"username": username,
		"vm_id":    vmID,
		"status":   vm.Status,
		"ssh_keys": keys,
	}
	log.Printf("SSH keys updated for VM %s (user: %s, %d key(s))\n", vmID, username, len(keys))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// repairConsoleHandler fixes serial console login on an existing VM disk.
// VMs provisioned before the getty fix carry a masked serial-getty; this
// unmasks it, enables the service and installs root autologin, so the web
// console accepts keyboard input on next boot. VM must be stopped (mounting
// a live guest filesystem would corrupt it). ZFS backend only.
// Works for VMs missing from service state too (verified by zvol + no live process).
func (s *HTTPServer) repairConsoleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, tracked := s.getVM(vmID)
	if tracked {
		if vm.Status == models.MicroVMStatusRunning {
			http.Error(w, "VM must be stopped before console repair (refusing to mount a live disk)", http.StatusConflict)
			return
		}
	} else {
		// Untracked VM: refuse only if a process is actually alive.
		if s.manager != nil && s.manager.IsVMProcessRunning(vmID) {
			http.Error(w, "VM must be stopped before console repair (refusing to mount a live disk)", http.StatusConflict)
			return
		}
	}
	if os.Getenv("MICROVM_USE_ZFS") != "true" {
		http.Error(w, "Console repair only supported when ZFS storage is enabled", http.StatusBadRequest)
		return
	}

	pool := os.Getenv("MICROVM_ZFS_POOL")
	if pool == "" {
		pool = "default"
	}
	dataset := os.Getenv("MICROVM_ZFS_DATASET")
	if dataset == "" {
		dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
	}
	zvolPath := fmt.Sprintf("/dev/zvol/%s/%s", dataset, vmID)
	if _, err := os.Stat(zvolPath); err != nil {
		http.Error(w, fmt.Sprintf("VM disk not found: %s", zvolPath), http.StatusNotFound)
		return
	}

	mountPoint := fmt.Sprintf("/tmp/vm-console-fix-%s", vmID)
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create mount point: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(mountPoint)

	if err := exec.Command("mount", zvolPath, mountPoint).Run(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to mount VM filesystem: %v", err), http.StatusInternalServerError)
		return
	}
	defer exec.Command("umount", mountPoint).Run()

	// Distro detection: guest os-release first (works without labels),
	// then VM labels, then empty (systemd path is the safe default).
	distro := detectGuestDistro(mountPoint)
	if distro == "" && tracked && vm.Metadata.Labels != nil {
		if imageID, ok := vm.Metadata.Labels["image_id"]; ok {
			lower := strings.ToLower(imageID)
			for _, prefix := range []string{"ubuntu", "debian", "alpine", "almalinux", "rocky", "fedora", "archlinux"} {
				if strings.HasPrefix(lower, prefix) {
					distro = prefix
					break
				}
			}
		}
	}
	cloudinit.ConfigureSerialConsole(mountPoint, distro)
	_ = exec.Command("sync").Run()

	if err := exec.Command("umount", mountPoint).Run(); err != nil {
		log.Printf("Warning: failed to unmount %s: %v", mountPoint, err)
	}

	if tracked {
		if vm.Metadata.Labels == nil {
			vm.Metadata.Labels = make(map[string]string)
		}
		vm.Metadata.Labels["console_repaired"] = "true"
		s.setVM(vmID, vm)
		s.saveVMsToDisk()
	}

	log.Printf("Console repair successful for VM %s (distro=%s, tracked=%v)\n", vmID, distro, tracked)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Console repair successful — serial getty enabled with root autologin",
		"vm_id":   vmID,
		"distro":  distro,
		"tracked": tracked,
	})
}

// detectGuestDistro reads ID= from the guest's /etc/os-release.
func detectGuestDistro(mountPoint string) string {
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc", "os-release"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "ID="); ok {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return ""
}

// getConsoleHandler provides console access information for a VM
func (s *HTTPServer) getConsoleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check if VM is running
	if vm.Status != models.MicroVMStatusRunning {
		http.Error(w, "VM is not running", http.StatusBadRequest)
		return
	}

	// Get console log path
	consoleLogPath := s.manager.GetConsoleLogPath(vmID)

	// Check if console log exists
	if _, err := os.Stat(consoleLogPath); os.IsNotExist(err) {
		http.Error(w, "Console not available", http.StatusNotFound)
		return
	}

	// Build console access URLs
	baseURL := fmt.Sprintf("http://%s", r.Host)
	consoleInfo := map[string]interface{}{
		"websocket_url": "", // WebSocket not implemented yet
		"http_log_url":  fmt.Sprintf("%s/vms/%s/console/logs", baseURL, vmID),
		"access_token":  "", // No auth yet
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(consoleInfo)
}

// getSSHAccessHandler provides SSH access information for a VM
func (s *HTTPServer) getSSHAccessHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check if VM is running
	if vm.Status != models.MicroVMStatusRunning {
		http.Error(w, "VM is not running", http.StatusConflict)
		return
	}

	workDir := resolveWorkDir()
	sshKeyPath := filepath.Join(workDir, "ssh-keys", fmt.Sprintf("%s_rsa", vmID))

	if _, err := os.Stat(sshKeyPath); os.IsNotExist(err) {
		http.Error(w, "SSH key not available for this VM", http.StatusNotFound)
		return
	}

	sshInfo := map[string]interface{}{
		"username":           "root",
		"port":               int(vm.Network.SSHPort),
		"key_path":           sshKeyPath,
		"host_key_path":      "",
		"connection_command": fmt.Sprintf("ssh -i %s -p %d root@%s", sshKeyPath, vm.Network.SSHPort, vm.Network.IPAddress),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sshInfo)
}

// getTrafficHandler returns traffic statistics and resets counters (for billing)
func (s *HTTPServer) getTrafficHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	// Get VM from in-memory map
	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check if VM is running
	if vm.Status != "running" {
		http.Error(w, "VM is not running", http.StatusConflict)
		return
	}

	// Get TAP device name; fall back to reading it from config.json when the
	// field is empty (e.g. VMs recovered from billing without a saved FC config).
	tapDevice := vm.Network.TAPDevice
	if tapDevice == "" {
		tapDevice = s.getTAPDeviceName(vmID)
		if tapDevice == "" {
			http.Error(w, "TAP device not available", http.StatusNotFound)
			return
		}
		// Persist the discovered name so future calls don't need to re-read.
		vm.Network.TAPDevice = tapDevice
		s.persistVMState()
	}

	// Get current traffic stats from network interface via sysfs.
	// If sysfs reports the device missing (tap deleted from netns while Firecracker
	// still holds the fd), discover the real interface name from /proc/{pid}/fdinfo.
	stats, err := getTrafficStatsFromSysfs(tapDevice)
	if err != nil {
		pid := 0
		if vm.ProcessInfo != nil {
			pid = vm.ProcessInfo.PID
		}
		if pid > 0 {
			realTAP, fdErr := getActualTAPFromPID(pid)
			if fdErr == nil && realTAP != "" && realTAP != tapDevice {
				log.Printf("🔍 VM %s: tap in record=%s not in sysfs, actual tap=%s (from fdinfo); updating", vmID, tapDevice, realTAP)
				tapDevice = realTAP
				vm.Network.TAPDevice = realTAP
				// Retry sysfs with the correct name
				stats, err = getTrafficStatsFromSysfs(tapDevice)
			}
		}
		if err != nil {
			// Interface truly unavailable (may have been removed); return zeros to
			// avoid billing-panel 500 errors. Log for operator visibility.
			log.Printf("⚠️  VM %s: cannot read traffic stats for tap=%s (pid=%d): %v — returning zeros", vmID, tapDevice, pid, err)
			stats = &TrafficStats{}
		}
	}

	// Calculate delta since last reset
	ingressDelta := stats.IngressBytes - vm.TrafficIngressBytes
	egressDelta := stats.EgressBytes - vm.TrafficEgressBytes

	// Prepare response
	response := map[string]interface{}{
		"vm_id":         vmID,
		"ingress_bytes": ingressDelta,
		"egress_bytes":  egressDelta,
		"total_bytes":   ingressDelta + egressDelta,
		"ingress_mb":    float64(ingressDelta) / 1024 / 1024,
		"egress_mb":     float64(egressDelta) / 1024 / 1024,
		"total_mb":      float64(ingressDelta+egressDelta) / 1024 / 1024,
		"reset":         true,
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}

	// Reset counters by storing current values (delta will be 0 on next call)
	vm.TrafficIngressBytes = stats.IngressBytes
	vm.TrafficEgressBytes = stats.EgressBytes
	s.setVM(vmID, vm)

	// Persist the new baseline so it survives service restarts.
	// Without this, a restart causes the next call to return all traffic
	// since VM boot instead of since the last reset (double-billing).
	s.persistVMState()

	// Return response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// TrafficStats represents network traffic statistics
type TrafficStats struct {
	IngressBytes uint64
	EgressBytes  uint64
}

// getActualTAPFromPID discovers the real TAP interface name for a Firecracker
// process by scanning /proc/{pid}/fd for the /dev/net/tun symlink and reading
// the "iff:" field from the corresponding fdinfo entry.
// This works even when the interface is no longer visible via "ip link" because
// Firecracker still holds the TUN/TAP fd open.
func getActualTAPFromPID(pid int) (string, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", fdDir, err)
	}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil || link != "/dev/net/tun" {
			continue
		}
		fdinfoData, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(fdinfoData), "\n") {
			if strings.HasPrefix(line, "iff:") {
				ifName := strings.TrimSpace(strings.TrimPrefix(line, "iff:"))
				if ifName != "" {
					return ifName, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no TUN/TAP fd found in /proc/%d/fd", pid)
}

// getTrafficStatsFromSysfs reads traffic stats from /sys/class/net
func getTrafficStatsFromSysfs(tapDevice string) (*TrafficStats, error) {
	var rxBytes, txBytes uint64

	// Read RX bytes (ingress)
	rxPath := fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", tapDevice)
	data, err := os.ReadFile(rxPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read rx_bytes: %w", err)
	}
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &rxBytes)

	// Read TX bytes (egress)
	txPath := fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", tapDevice)
	data, err = os.ReadFile(txPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read tx_bytes: %w", err)
	}
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &txBytes)

	// TAP device stats are from host's perspective:
	// - TAP rx_bytes = data received by TAP from VM = VM's egress (upload)
	// - TAP tx_bytes = data transmitted by TAP to VM = VM's ingress (download)
	return &TrafficStats{
		IngressBytes: txBytes, // VM receives what TAP transmits
		EgressBytes:  rxBytes, // VM sends what TAP receives
	}, nil
}

// IPv4AddonRequest represents a request to add a public IPv4 address
type IPv4AddonRequest struct {
	IPv4Address string `json:"ipv4_address"` // Public IPv4 address (required)
	MACAddress  string `json:"mac_address"`  // OVH-assigned MAC (required for OVH)
	Gateway     string `json:"gateway"`      // Gateway IP (optional, defaults to x.x.x.254)
}

// addIPv4AddonHandler adds a public IPv4 address to a VM via 1:1 NAT
func (s *HTTPServer) addIPv4AddonHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// VM must have an internal NAT address
	if vm.Network.IPv4NATAddress == "" {
		http.Error(w, "VM does not have an internal IPv4 NAT address", http.StatusBadRequest)
		return
	}

	var req IPv4AddonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.IPv4Address == "" {
		http.Error(w, "ipv4_address is required", http.StatusBadRequest)
		return
	}

	if req.MACAddress == "" {
		http.Error(w, "mac_address is required (OVH-assigned MAC)", http.StatusBadRequest)
		return
	}

	// Check if this IP is already assigned
	for _, addon := range vm.Network.IPv4Addons {
		if addon.IPv4Address == req.IPv4Address {
			http.Error(w, "IPv4 address already assigned to this VM", http.StatusConflict)
			return
		}
	}

	// Determine gateway (default to .254 of the IP's subnet)
	gateway := req.Gateway
	if gateway == "" {
		// Parse IP and construct gateway
		parts := strings.Split(req.IPv4Address, ".")
		if len(parts) == 4 {
			gateway = fmt.Sprintf("%s.%s.%s.254", parts[0], parts[1], parts[2])
		}
	}

	// Generate unique interface name and routing table
	addonIndex := len(vm.Network.IPv4Addons)
	interfaceName := fmt.Sprintf("ipv4-%s-%d", truncateVMID(vmID, 8), addonIndex)
	routingTable := 100 + addonIndex // Use tables 100, 101, 102, etc.

	internalIP := vm.Network.IPv4NATAddress

	// Execute network configuration
	if err := configureIPv4Addon(interfaceName, req.IPv4Address, req.MACAddress, gateway, internalIP, routingTable); err != nil {
		http.Error(w, fmt.Sprintf("Failed to configure IPv4 addon: %v", err), http.StatusInternalServerError)
		return
	}

	// Create addon record
	addon := models.IPv4Addon{
		IPv4Address:  req.IPv4Address,
		MACAddress:   req.MACAddress,
		InterfaceID:  interfaceName,
		RoutingTable: routingTable,
		Enabled:      true,
		CreatedAt:    time.Now(),
	}

	// Add to VM
	vm.Network.IPv4Addons = append(vm.Network.IPv4Addons, addon)
	s.setVM(vmID, vm)
	s.persistVMState()

	log.Printf("✅ Added IPv4 addon %s to VM %s (interface: %s, internal: %s)", req.IPv4Address, vmID, interfaceName, internalIP)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":      "IPv4 addon added successfully",
		"vm_id":        vmID,
		"ipv4_address": req.IPv4Address,
		"internal_ip":  internalIP,
		"interface":    interfaceName,
		"enabled":      true,
	})
}

// listIPv4AddonsHandler lists all public IPv4 addresses assigned to a VM
func (s *HTTPServer) listIPv4AddonsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":       vmID,
		"internal_ip": vm.Network.IPv4NATAddress,
		"ipv4_addons": vm.Network.IPv4Addons,
		"count":       len(vm.Network.IPv4Addons),
	})
}

// updateIPv4AddonHandler updates an existing IPv4 addon (change IP, MAC, or gateway)
func (s *HTTPServer) updateIPv4AddonHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]
	oldIP := vars["ip"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// VM must have an internal NAT address
	if vm.Network.IPv4NATAddress == "" {
		http.Error(w, "VM does not have an internal IPv4 NAT address", http.StatusBadRequest)
		return
	}

	// Parse request body
	var req IPv4AddonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.IPv4Address == "" {
		http.Error(w, "ipv4_address is required", http.StatusBadRequest)
		return
	}

	if req.MACAddress == "" {
		http.Error(w, "mac_address is required", http.StatusBadRequest)
		return
	}

	// Find the existing addon
	var foundAddon *models.IPv4Addon
	var foundIndex int
	for i, addon := range vm.Network.IPv4Addons {
		if addon.IPv4Address == oldIP {
			foundAddon = &vm.Network.IPv4Addons[i]
			foundIndex = i
			break
		}
	}

	if foundAddon == nil {
		http.Error(w, "IPv4 address not found on this VM", http.StatusNotFound)
		return
	}

	// Check if new IP is already assigned to this VM (and it's not the same addon)
	if req.IPv4Address != oldIP {
		for _, addon := range vm.Network.IPv4Addons {
			if addon.IPv4Address == req.IPv4Address {
				http.Error(w, "New IPv4 address already assigned to this VM", http.StatusConflict)
				return
			}
		}
	}

	// Determine gateway
	gateway := req.Gateway
	if gateway == "" {
		parts := strings.Split(req.IPv4Address, ".")
		if len(parts) == 4 {
			gateway = fmt.Sprintf("%s.%s.%s.254", parts[0], parts[1], parts[2])
		}
	}

	internalIP := vm.Network.IPv4NATAddress
	interfaceName := foundAddon.InterfaceID
	routingTable := foundAddon.RoutingTable

	// Store old config for rollback
	oldConfig := *foundAddon

	// Step 1: Remove old configuration
	log.Printf("🔄 Updating IPv4 addon for VM %s: %s → %s", vmID, oldIP, req.IPv4Address)
	if err := removeIPv4Addon(interfaceName, oldIP, internalIP, routingTable); err != nil {
		log.Printf("⚠️  Warning: Failed to remove old IPv4 addon config: %v", err)
	}

	// Step 2: Apply new configuration (reusing same interface name and routing table)
	if err := updateIPv4AddonConfig(interfaceName, req.IPv4Address, req.MACAddress, gateway, internalIP, routingTable); err != nil {
		log.Printf("❌ Failed to apply new IPv4 addon config: %v", err)
		// Attempt rollback
		log.Printf("🔄 Attempting rollback to old configuration...")
		if rollbackErr := updateIPv4AddonConfig(interfaceName, oldConfig.IPv4Address, oldConfig.MACAddress, gateway, internalIP, routingTable); rollbackErr != nil {
			log.Printf("❌ CRITICAL: Rollback failed: %v", rollbackErr)
			http.Error(w, fmt.Sprintf("Failed to update and rollback failed: %v", err), http.StatusInternalServerError)
			return
		}
		log.Printf("✅ Rollback successful")
		http.Error(w, fmt.Sprintf("Failed to update IPv4 addon: %v", err), http.StatusInternalServerError)
		return
	}

	// Step 3: Update VM record
	vm.Network.IPv4Addons[foundIndex] = models.IPv4Addon{
		IPv4Address:  req.IPv4Address,
		MACAddress:   req.MACAddress,
		InterfaceID:  interfaceName,
		RoutingTable: routingTable,
		Enabled:      true,
		CreatedAt:    foundAddon.CreatedAt,
	}
	s.setVM(vmID, vm)
	s.persistVMState()

	log.Printf("✅ Updated IPv4 addon for VM %s: %s → %s (interface: %s, internal: %s)", vmID, oldIP, req.IPv4Address, interfaceName, internalIP)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":     "IPv4 addon updated successfully",
		"vm_id":       vmID,
		"old_address": oldIP,
		"new_address": req.IPv4Address,
		"internal_ip": internalIP,
		"interface":   interfaceName,
		"enabled":     true,
	})
}

// removeIPv4AddonHandler removes a public IPv4 address from a VM
func (s *HTTPServer) removeIPv4AddonHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]
	targetIP := vars["ip"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Find the addon
	var foundAddon *models.IPv4Addon
	var foundIndex int
	for i, addon := range vm.Network.IPv4Addons {
		if addon.IPv4Address == targetIP {
			foundAddon = &addon
			foundIndex = i
			break
		}
	}

	if foundAddon == nil {
		http.Error(w, "IPv4 address not found on this VM", http.StatusNotFound)
		return
	}

	// Remove network configuration
	if err := removeIPv4Addon(foundAddon.InterfaceID, foundAddon.IPv4Address, vm.Network.IPv4NATAddress, foundAddon.RoutingTable); err != nil {
		log.Printf("⚠️  Warning: Failed to fully remove IPv4 addon config: %v", err)
	}

	// Remove from VM
	vm.Network.IPv4Addons = append(vm.Network.IPv4Addons[:foundIndex], vm.Network.IPv4Addons[foundIndex+1:]...)
	s.setVM(vmID, vm)
	s.persistVMState()

	log.Printf("✅ Removed IPv4 addon %s from VM %s", targetIP, vmID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":      "IPv4 addon removed successfully",
		"vm_id":        vmID,
		"ipv4_address": targetIP,
	})
}

// configureIPv4Addon sets up macvlan interface, NAT rules, and policy routing for a public IPv4
func configureIPv4Addon(interfaceName, publicIP, mac, gateway, internalIP string, routingTable int) error {
	// Get parent interface (eno1 or from env)
	parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
	if parentIface == "" {
		parentIface = "eno1"
	}

	// STEP 1: Clean up any existing rules for this IP (prevents duplicates on restart)
	log.Printf("🧹 Cleaning up existing rules for %s before adding new ones...", publicIP)
	cleanupIPv4AddonRules(publicIP, internalIP, routingTable)

	// STEP 2: Create macvlan interface
	interfaceCommands := [][]string{
		{"ip", "link", "add", interfaceName, "link", parentIface, "type", "macvlan", "mode", "bridge"},
		{"ip", "link", "set", interfaceName, "address", mac},
		{"ip", "addr", "add", publicIP + "/32", "dev", interfaceName},
		{"ip", "link", "set", interfaceName, "up"},
	}

	for _, cmd := range interfaceCommands {
		c := exec.Command("sudo", cmd...)
		if output, err := c.CombinedOutput(); err != nil {
			outStr := string(output)
			if !strings.Contains(outStr, "File exists") && !strings.Contains(outStr, "RTNETLINK answers: File exists") {
				return fmt.Errorf("command %v failed: %v (output: %s)", cmd, err, outStr)
			}
		}
	}

	// STEP 3: Add policy routing (deduplicate: keep only one rule)
	// First check if rule already exists
	checkCmd := exec.Command("sudo", "ip", "rule", "list")
	checkOutput, _ := checkCmd.CombinedOutput()
	ruleExists := strings.Contains(string(checkOutput), fmt.Sprintf("from %s lookup %d", publicIP, routingTable))

	if !ruleExists {
		cmd := exec.Command("sudo", "ip", "rule", "add", "from", publicIP, "table", fmt.Sprintf("%d", routingTable))
		if output, err := cmd.CombinedOutput(); err != nil {
			outStr := string(output)
			if !strings.Contains(outStr, "File exists") {
				return fmt.Errorf("failed to add policy routing rule: %v (output: %s)", err, outStr)
			}
		}
	}

	// Add route to routing table
	cmd := exec.Command("sudo", "ip", "route", "add", "default", "via", gateway, "dev", interfaceName, "table", fmt.Sprintf("%d", routingTable), "onlink")
	if output, err := cmd.CombinedOutput(); err != nil {
		outStr := string(output)
		if !strings.Contains(outStr, "File exists") && !strings.Contains(outStr, "RTNETLINK answers: File exists") {
			return fmt.Errorf("failed to add default route: %v (output: %s)", err, outStr)
		}
	}

	// STEP 4: Add iptables rules (with -C check to prevent duplicates)
	iptablesRules := [][]string{
		// DNAT: incoming traffic to public IP → VM internal IP
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},

		// SNAT: outgoing traffic from VM internal IP → public IP (insert at top to take priority)
		{"iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-s", internalIP, "!", "-d", "10.100.0.0/24", "-j", "SNAT", "--to-source", publicIP},

		// NAT Hairpinning: allow VM to reach its own public IP from inside
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-s", internalIP, "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", internalIP, "-d", internalIP, "-j", "MASQUERADE"},

		// Ensure forwarding is allowed
		{"iptables", "-A", "FORWARD", "-d", internalIP, "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-s", internalIP, "-j", "ACCEPT"},
	}

	for _, rule := range iptablesRules {
		// Check if rule exists (replace -A/-I with -C for check)
		checkRule := make([]string, len(rule))
		copy(checkRule, rule)
		for i, arg := range checkRule {
			if arg == "-A" || arg == "-I" {
				checkRule[i] = "-C"
				// Remove position argument for -I rules
				if arg == "-I" && i+1 < len(checkRule) {
					checkRule = append(checkRule[:i+1], checkRule[i+2:]...)
				}
				break
			}
		}

		// Test if rule exists
		checkCmd := exec.Command("sudo", checkRule...)
		if err := checkCmd.Run(); err != nil {
			// Rule doesn't exist, add it
			addCmd := exec.Command("sudo", rule...)
			if output, err := addCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to add iptables rule %v: %v (output: %s)", rule, err, string(output))
			}
		}
	}

	log.Printf("✅ IPv4 addon configured: %s → %s (no duplicates)", publicIP, internalIP)
	return nil
}

// cleanupIPv4AddonRules removes all existing rules for a specific IP to prevent duplicates
// Uses efficient iptables-save/restore approach to handle bulk deletions
func cleanupIPv4AddonRules(publicIP, internalIP string, routingTable int) {
	// Remove duplicate policy routing rules (with safety limit to prevent infinite loops)
	const maxPolicyRuleCleanups = 100
	cleanedRules := 0

	for i := 0; i < maxPolicyRuleCleanups; i++ {
		cmd := exec.Command("sudo", "ip", "rule", "del", "from", publicIP, "table", fmt.Sprintf("%d", routingTable))
		if err := cmd.Run(); err != nil {
			break // No more rules to delete
		}
		cleanedRules++
	}

	if cleanedRules > 0 {
		log.Printf("🧹 Cleaned up %d duplicate policy routing rules for %s", cleanedRules, publicIP)
	}

	if cleanedRules >= maxPolicyRuleCleanups {
		log.Printf("⚠️  Hit cleanup limit (%d rules) for %s - there may be more duplicates", maxPolicyRuleCleanups, publicIP)
	}

	// Use iptables-save/restore for efficient bulk cleanup
	cleanupIptablesForIP(publicIP, internalIP)
}

// cleanupIptablesForIP efficiently removes all iptables rules for a given IP pair
// This uses iptables-save/restore which is 100x faster than individual iptables -D commands
func cleanupIptablesForIP(publicIP, internalIP string) {
	// Save current nat table rules
	saveCmd := exec.Command("iptables-save", "-t", "nat")
	output, err := saveCmd.Output()
	if err != nil {
		log.Printf("⚠️  Failed to save iptables: %v", err)
		return
	}

	lines := strings.Split(string(output), "\n")
	var filteredLines []string
	removedCount := 0

	for _, line := range lines {
		// Skip rules containing our public or internal IP
		if strings.Contains(line, publicIP) || (internalIP != "" && strings.Contains(line, internalIP)) {
			removedCount++
			continue
		}
		filteredLines = append(filteredLines, line)
	}

	if removedCount == 0 {
		return // No rules to clean up
	}

	log.Printf("🧹 Removing %d duplicate iptables rules for %s/%s", removedCount, publicIP, internalIP)

	// Restore filtered rules
	restoreCmd := exec.Command("iptables-restore", "-T", "nat")
	restoreCmd.Stdin = strings.NewReader(strings.Join(filteredLines, "\n"))
	if err := restoreCmd.Run(); err != nil {
		log.Printf("⚠️  Failed to restore iptables: %v (rules may be partially cleaned)", err)
		// Fall back to slow individual deletions
		cleanupIPv4AddonRulesSlow(publicIP, internalIP)
	} else {
		log.Printf("✅ Cleaned up %d duplicate rules efficiently", removedCount)
	}
}

// cleanupIPv4AddonRulesSlow is the fallback slow cleanup method (kept for compatibility)
func cleanupIPv4AddonRulesSlow(publicIP, internalIP string) {
	log.Printf("⚠️  Using slow cleanup method for %s", publicIP)
	const maxIterations = 100

	// Remove all DNAT rules for this public IP
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-t", "nat", "-D", "PREROUTING", "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP)
		if err := cmd.Run(); err != nil {
			break
		}
	}

	// Remove hairpin DNAT rules
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-t", "nat", "-D", "PREROUTING", "-s", internalIP, "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP)
		if err := cmd.Run(); err != nil {
			break
		}
	}

	// Remove SNAT rules for this internal IP
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", internalIP, "!", "-d", "10.100.0.0/24", "-j", "SNAT", "--to-source", publicIP)
		if err := cmd.Run(); err != nil {
			break
		}
	}

	// Remove hairpin MASQUERADE rules
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", internalIP, "-d", internalIP, "-j", "MASQUERADE")
		if err := cmd.Run(); err != nil {
			break
		}
	}

	// Remove FORWARD rules
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-D", "FORWARD", "-d", internalIP, "-j", "ACCEPT")
		if err := cmd.Run(); err != nil {
			break
		}
	}

	// Remove FORWARD rules for source
	for i := 0; i < maxIterations; i++ {
		cmd := exec.Command("sudo", "iptables", "-D", "FORWARD", "-s", internalIP, "-j", "ACCEPT")
		if err := cmd.Run(); err != nil {
			break
		}
	}
}

// updateIPv4AddonConfig updates macvlan interface with new IP/MAC and updates NAT rules
// Reuses the same interface name and routing table for atomic updates
func updateIPv4AddonConfig(interfaceName, publicIP, mac, gateway, internalIP string, routingTable int) error {
	// Get parent interface
	parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
	if parentIface == "" {
		parentIface = "eno1"
	}

	commands := [][]string{
		// Delete existing interface
		{"ip", "link", "del", interfaceName},
		// Recreate macvlan interface with new MAC
		{"ip", "link", "add", interfaceName, "link", parentIface, "type", "macvlan", "mode", "bridge"},
		{"ip", "link", "set", interfaceName, "address", mac},
		{"ip", "addr", "add", publicIP + "/32", "dev", interfaceName},
		{"ip", "link", "set", interfaceName, "up"},

		// Update policy routing (delete old rule first, then add new)
		{"ip", "route", "replace", "default", "via", gateway, "dev", interfaceName, "table", fmt.Sprintf("%d", routingTable), "onlink"},
		{"ip", "rule", "add", "from", publicIP, "table", fmt.Sprintf("%d", routingTable)},

		// DNAT: incoming traffic to public IP → VM internal IP
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},

		// SNAT: outgoing traffic from VM internal IP → public IP
		{"iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-s", internalIP, "!", "-d", "10.100.0.0/24", "-j", "SNAT", "--to-source", publicIP},

		// NAT Hairpinning: allow VM to reach its own public IP from inside
		// This enables certbot, self-pings, and services that connect to their own public IP
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-s", internalIP, "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", internalIP, "-d", internalIP, "-j", "MASQUERADE"},

		// Ensure forwarding is allowed
		{"iptables", "-A", "FORWARD", "-d", internalIP, "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-s", internalIP, "-j", "ACCEPT"},
	}

	for _, cmd := range commands {
		// For iptables rules, check if rule already exists before adding (prevent duplicates)
		if cmd[0] == "iptables" || cmd[0] == "ip6tables" {
			checkRule := make([]string, len(cmd))
			copy(checkRule, cmd)
			skipAdd := false
			for i, arg := range checkRule {
				if arg == "-A" || arg == "-I" {
					checkRule[i] = "-C"
					// Remove position argument for -I rules (e.g., -I POSTROUTING 1)
					if arg == "-I" && i+2 < len(checkRule) {
						if _, err := fmt.Sscanf(checkRule[i+2], "%d", new(int)); err == nil {
							checkRule = append(checkRule[:i+2], checkRule[i+3:]...)
						}
					}
					break
				}
			}
			if exec.Command("sudo", checkRule...).Run() == nil {
				skipAdd = true // Rule already exists
			}
			if skipAdd {
				continue
			}
		}

		c := exec.Command("sudo", cmd...)
		if output, err := c.CombinedOutput(); err != nil {
			outStr := string(output)
			// Ignore expected errors during update
			if !strings.Contains(outStr, "File exists") &&
				!strings.Contains(outStr, "RTNETLINK answers: File exists") &&
				!strings.Contains(outStr, "Cannot find device") &&
				!strings.Contains(outStr, "No such file or directory") {
				return fmt.Errorf("command %v failed: %v (output: %s)", cmd, err, outStr)
			}
		}
	}

	log.Printf("✅ IPv4 addon updated: %s → %s (dedup checked)", publicIP, internalIP)
	return nil
}

// removeIPv4Addon removes macvlan interface, NAT rules, and policy routing
func removeIPv4Addon(interfaceName, publicIP, internalIP string, routingTable int) error {
	commands := [][]string{
		// Remove iptables rules
		{"iptables", "-t", "nat", "-D", "PREROUTING", "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},
		{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", internalIP, "!", "-d", "10.100.0.0/24", "-j", "SNAT", "--to-source", publicIP},

		// Remove NAT hairpinning rules
		{"iptables", "-t", "nat", "-D", "PREROUTING", "-s", internalIP, "-d", publicIP, "-j", "DNAT", "--to-destination", internalIP},
		{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", internalIP, "-d", internalIP, "-j", "MASQUERADE"},

		// Remove policy routing
		{"ip", "rule", "del", "from", publicIP, "table", fmt.Sprintf("%d", routingTable)},
		{"ip", "route", "del", "default", "table", fmt.Sprintf("%d", routingTable)},

		// Remove interface
		{"ip", "link", "del", interfaceName},
	}

	var lastErr error
	for _, cmd := range commands {
		c := exec.Command("sudo", cmd...)
		if _, err := c.CombinedOutput(); err != nil {
			lastErr = err
			// Continue trying to remove other components
		}
	}

	return lastErr
}

// truncateVMID helper to shorten VM ID for interface names
func truncateVMID(vmID string, maxLen int) string {
	if len(vmID) <= maxLen {
		return vmID
	}
	return vmID[:maxLen]
}

// setupIPv4FallbackNAT creates iptables rules for IPv6-only VMs with IPv4 NAT fallback
// These rules provide outbound IPv4 connectivity via NAT for VMs without a public IPv4 address
func setupIPv4FallbackNAT(internalIP, publicIP string) error {
	if internalIP == "" || publicIP == "" {
		return fmt.Errorf("internal IP and public IP are required")
	}

	log.Printf("🔧 Setting up IPv4 NAT fallback: %s → %s", internalIP, publicIP)

	// Rules to create (with deduplication check)
	rulesToCheck := [][]string{
		// SNAT: outbound traffic from internal IP → public IP
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", internalIP + "/32", "!", "-d", "10.100.0.0/24", "-j", "SNAT", "--to-source", publicIP},

		// MASQUERADE: hairpin NAT for VM to reach itself via internal IP
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", internalIP + "/32", "-d", internalIP + "/32", "-j", "MASQUERADE"},
	}

	for _, rule := range rulesToCheck {
		// Check if rule exists (replace -A with -C)
		checkRule := make([]string, len(rule))
		copy(checkRule, rule)
		for i, arg := range checkRule {
			if arg == "-A" {
				checkRule[i] = "-C"
				break
			}
		}

		// Test if rule exists
		checkCmd := exec.Command("sudo", checkRule...)
		if err := checkCmd.Run(); err != nil {
			// Rule doesn't exist, add it
			addCmd := exec.Command("sudo", rule...)
			if output, err := addCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to add IPv4 NAT rule %v: %v (output: %s)", rule, err, string(output))
			}
			log.Printf("✅ Added IPv4 NAT rule: %s", strings.Join(rule[4:], " "))
		} else {
			log.Printf("ℹ️  IPv4 NAT rule already exists: %s", strings.Join(rule[4:], " "))
		}
	}

	log.Printf("✅ IPv4 NAT fallback configured for %s", internalIP)
	return nil
}

// cleanupIPv4FallbackNAT removes all NAT rules for a given internal IP
// Uses efficient iptables-save/restore for bulk deletion
func cleanupIPv4FallbackNAT(internalIP string) {
	if internalIP == "" {
		return
	}

	// Use the efficient cleanup method
	cleanupIptablesForIP("", internalIP)
}

// restoreIPv4Addons restores all IPv4 addon configurations on startup
// This recreates macvlan interfaces, NAT rules, and policy routing for persisted addons
func (s *HTTPServer) restoreIPv4Addons() {
	restoredCount := 0
	for vmID, vm := range s.snapshotVMMap() {
		if len(vm.Network.IPv4Addons) == 0 {
			continue
		}

		// VM must have an internal NAT address
		if vm.Network.IPv4NATAddress == "" {
			log.Printf("⚠️  VM %s has IPv4 addons but no internal NAT address, skipping", vmID)
			continue
		}

		for i, addon := range vm.Network.IPv4Addons {
			if !addon.Enabled {
				continue
			}

			// Determine gateway from IP
			gateway := ""
			parts := strings.Split(addon.IPv4Address, ".")
			if len(parts) == 4 {
				gateway = fmt.Sprintf("%s.%s.%s.254", parts[0], parts[1], parts[2])
			}

			// Restore the network configuration
			err := configureIPv4Addon(
				addon.InterfaceID,
				addon.IPv4Address,
				addon.MACAddress,
				gateway,
				vm.Network.IPv4NATAddress,
				addon.RoutingTable,
			)
			if err != nil {
				log.Printf("⚠️  Failed to restore IPv4 addon %s for VM %s: %v", addon.IPv4Address, vmID, err)
				// Mark as disabled if restoration failed
				vm.Network.IPv4Addons[i].Enabled = false
			} else {
				restoredCount++
				log.Printf("✅ Restored IPv4 addon %s → %s for VM %s", addon.IPv4Address, vm.Network.IPv4NATAddress, vmID)
			}
		}
		s.setVM(vmID, vm)
	}

	if restoredCount > 0 {
		log.Printf("🌐 Restored %d IPv4 addon(s) on startup", restoredCount)
		s.persistVMState()
	}
}

// restoreIPv4FallbackNAT restores IPv4 NAT fallback rules for IPv6-only VMs on startup
// This recreates SNAT and MASQUERADE rules for VMs with internal NAT addresses
func (s *HTTPServer) restoreIPv4FallbackNAT() {
	publicIP, err := s.getPublicIPv4()
	if err != nil {
		log.Printf("⚠️  Failed to get public IPv4, skipping NAT fallback restoration: %v", err)
		return
	}

	restoredCount := 0
	for vmID, vm := range s.snapshotVMMap() {
		// Only restore for VMs with IPv4 NAT fallback enabled
		if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
			if err := setupIPv4FallbackNAT(vm.Network.IPv4NATAddress, publicIP); err != nil {
				log.Printf("⚠️  Failed to restore IPv4 NAT fallback for VM %s (%s): %v", vmID, vm.Network.IPv4NATAddress, err)
			} else {
				restoredCount++
				log.Printf("✅ Restored IPv4 NAT fallback for VM %s: %s → %s", vmID, vm.Network.IPv4NATAddress, publicIP)
			}
		}
	}

	if restoredCount > 0 {
		log.Printf("🌐 Restored IPv4 NAT fallback for %d VM(s) on startup", restoredCount)
	}
}

// restoreNDPProxyEntries restores NDP proxy entries for all running IPv6-only VMs on startup
// This is critical for MACVLAN mode where VMs survive service restarts due to KillMode=process
// Without this, VMs would be locally accessible but not globally reachable
func (s *HTTPServer) restoreNDPProxyEntries() {
	// Get parent interface from environment (same as network manager does)
	parentIface := os.Getenv("MICROVM_MACVLAN_PARENT")
	if parentIface == "" {
		// Not in MACVLAN mode or not configured, skip silently
		return
	}

	restoredCount := 0
	for vmID, vm := range s.snapshotVMMap() {
		// Only restore for running VMs with IPv6 addresses
		if vm.Status != models.MicroVMStatusRunning {
			continue
		}

		if vm.Network.IPv6Address == "" || vm.Network.IPv6Address == "0.0.0.0" {
			continue
		}

		// Check if NDP proxy already exists
		checkCmd := exec.Command("ip", "-6", "neigh", "show", "proxy")
		output, err := checkCmd.Output()
		if err == nil && strings.Contains(string(output), vm.Network.IPv6Address) {
			// Proxy already exists, skip
			continue
		}

		// Add NDP proxy entry
		addCmd := exec.Command("sudo", "ip", "-6", "neigh", "add", "proxy", vm.Network.IPv6Address, "dev", parentIface)
		if output, err := addCmd.CombinedOutput(); err != nil {
			// Ignore "File exists" errors
			if !strings.Contains(string(output), "File exists") {
				log.Printf("⚠️  Failed to restore NDP proxy for VM %s (%s): %v (output: %s)", vmID, vm.Network.IPv6Address, err, strings.TrimSpace(string(output)))
			}
		} else {
			restoredCount++
			log.Printf("✅ Restored NDP proxy for VM %s: %s on %s", vmID, vm.Network.IPv6Address, parentIface)
		}
	}

	if restoredCount > 0 {
		log.Printf("🌐 Restored NDP proxy entries for %d VM(s) on startup", restoredCount)
	}
}

// createSnapshotHandler creates a ZFS snapshot of a VM's disk
func (s *HTTPServer) createSnapshotHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	_, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Parse request body for optional snapshot name
	var req struct {
		Name string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Generate snapshot name if not provided
	snapshotName := req.Name
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snap-%d", time.Now().Unix())
	}

	// Check if using ZFS backend
	backend, ok := s.manager.GetStorageBackend().(storage.SnapshotableBackend)
	if !ok {
		http.Error(w, "Snapshots only supported with ZFS backend", http.StatusNotImplemented)
		return
	}

	// Single snapshot policy: delete all existing snapshots before creating new one
	existingSnaps, err := backend.ListSnapshots(vmID)
	if err == nil && len(existingSnaps) > 0 {
		fmt.Printf("🗑️  Deleting %d existing snapshot(s) for VM %s (single snapshot policy)\n", len(existingSnaps), vmID)
		for _, oldSnap := range existingSnaps {
			if err := backend.DeleteSnapshot(vmID, oldSnap); err != nil {
				fmt.Printf("⚠️  Warning: failed to delete old snapshot %s: %v\n", oldSnap, err)
			}
		}
	}

	// Create snapshot
	if err := backend.SnapshotDisk(vmID, snapshotName); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create snapshot: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":         vmID,
		"snapshot_name": snapshotName,
		"created_at":    time.Now().UTC().Format(time.RFC3339),
		"message":       "Snapshot created successfully",
	})
}

// listSnapshotsHandler lists all snapshots for a VM with size info for billing
func (s *HTTPServer) listSnapshotsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	_, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check if using ZFS backend
	backend, ok := s.manager.GetStorageBackend().(storage.SnapshotableBackend)
	if !ok {
		http.Error(w, "Snapshots only supported with ZFS backend", http.StatusNotImplemented)
		return
	}

	// Get snapshot info with billing data
	snapshots, err := backend.GetSnapshotInfo(vmID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list snapshots: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":     vmID,
		"snapshots": snapshots,
		"count":     len(snapshots),
	})
}

// rollbackSnapshotHandler rolls back a VM to a specific snapshot
func (s *HTTPServer) rollbackSnapshotHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]
	snapshotName := vars["snapshot"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// VM must be stopped for rollback
	if vm.Status == models.MicroVMStatusRunning {
		http.Error(w, "VM must be stopped before rollback", http.StatusConflict)
		return
	}

	// Check if using ZFS backend
	backend, ok := s.manager.GetStorageBackend().(storage.SnapshotableBackend)
	if !ok {
		http.Error(w, "Snapshots only supported with ZFS backend", http.StatusNotImplemented)
		return
	}

	// Rollback to snapshot
	if err := backend.RollbackToSnapshot(vmID, snapshotName); err != nil {
		http.Error(w, fmt.Sprintf("Failed to rollback: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":         vmID,
		"snapshot_name": snapshotName,
		"message":       "Rollback successful - VM disk restored to snapshot state",
	})
}

// deleteSnapshotHandler deletes a specific snapshot
func (s *HTTPServer) deleteSnapshotHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]
	snapshotName := vars["snapshot"]

	_, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check if using ZFS backend
	backend, ok := s.manager.GetStorageBackend().(storage.SnapshotableBackend)
	if !ok {
		http.Error(w, "Snapshots only supported with ZFS backend", http.StatusNotImplemented)
		return
	}

	// Delete snapshot
	if err := backend.DeleteSnapshot(vmID, snapshotName); err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete snapshot: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":         vmID,
		"snapshot_name": snapshotName,
		"message":       "Snapshot deleted successfully",
	})
}

// cloneVMHandler creates a new VM from an existing VM's snapshot
func (s *HTTPServer) cloneVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sourceVMID := vars["id"]

	sourceVM, exists := s.getVM(sourceVMID)
	if !exists {
		http.Error(w, "Source VM not found", http.StatusNotFound)
		return
	}

	// Parse request body for clone configuration
	var req struct {
		Name          string                `json:"name"`
		ResourceSpecs *models.ResourceSpecs `json:"resource_specs,omitempty"`
		NetworkConfig *struct {
			AssignmentMethod string `json:"assignment_method,omitempty"`
			MACAddress       string `json:"mac_address,omitempty"`
			IPAddress        string `json:"ip_address,omitempty"`
		} `json:"network_config,omitempty"`
		CloudInit *struct {
			Hostname string `json:"hostname,omitempty"`
			Username string `json:"username,omitempty"`
			Password string `json:"password,omitempty"`
		} `json:"cloudinit,omitempty"`
		AutoStart bool `json:"auto_start,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Generate new VM ID if name not provided
	newVMID := req.Name
	if newVMID == "" {
		newVMID = fmt.Sprintf("%s-clone-%s", sourceVMID, uuid.New().String()[:8])
	}

	// Check if VM already exists
	if _, exists := s.getVM(newVMID); exists {
		http.Error(w, "VM with this name already exists", http.StatusConflict)
		return
	}

	// Check if using ZFS backend
	backend, ok := s.manager.GetStorageBackend().(storage.SnapshotableBackend)
	if !ok {
		http.Error(w, "Cloning only supported with ZFS backend", http.StatusNotImplemented)
		return
	}

	// Ensure source VM is stopped for consistent snapshot
	wasRunning := sourceVM.Status == models.MicroVMStatusRunning
	if wasRunning {
		fmt.Printf("🛑 Stopping source VM %s for consistent clone snapshot\n", sourceVMID)
		if err := s.manager.StopMicroVM(context.Background(), sourceVM, false); err != nil {
			http.Error(w, fmt.Sprintf("Failed to stop source VM: %v", err), http.StatusInternalServerError)
			return
		}
		sourceVM.Status = models.MicroVMStatusStopped
	}

	// Create clone snapshot with timestamp
	cloneSnapshotName := fmt.Sprintf("clone-%d", time.Now().Unix())
	if err := backend.SnapshotDisk(sourceVMID, cloneSnapshotName); err != nil {
		http.Error(w, fmt.Sprintf("Failed to create clone snapshot: %v", err), http.StatusInternalServerError)
		// Restart source VM if it was running
		if wasRunning {
			s.manager.StartMicroVM(context.Background(), sourceVM)
			sourceVM.Status = models.MicroVMStatusRunning
		}
		return
	}

	// Create full independent copy of the disk (not COW clone)
	fmt.Printf("📋 Creating full copy of disk from snapshot...\n")
	if err := backend.CopyDisk(sourceVMID, cloneSnapshotName, newVMID); err != nil {
		backend.DeleteSnapshot(sourceVMID, cloneSnapshotName)
		http.Error(w, fmt.Sprintf("Failed to copy disk: %v", err), http.StatusInternalServerError)
		if wasRunning {
			s.manager.StartMicroVM(context.Background(), sourceVM)
			sourceVM.Status = models.MicroVMStatusRunning
		}
		return
	}

	// Clean up clone snapshot after successful copy
	backend.DeleteSnapshot(sourceVMID, cloneSnapshotName)

	// Create new VM instance with cloned disk
	newVM := &models.MicroVMInstance{
		ID:        newVMID,
		NodeID:    sourceVM.NodeID,
		Status:    models.MicroVMStatusStopped,
		Resources: sourceVM.Resources,
		Storage:   sourceVM.Storage,
		Network: models.NetworkInfo{
			AssignmentMethod: sourceVM.Network.AssignmentMethod,
			BridgeName:       sourceVM.Network.BridgeName,
		},
		Pricing: sourceVM.Pricing,
		Metadata: models.MicroVMMetadata{
			CreatedAt:    time.Now(),
			LastActivity: time.Now(),
			CustomerID:   sourceVM.Metadata.CustomerID,
			AutoStart:    req.AutoStart,
			Labels:       make(map[string]string),
		},
	}

	// Copy labels from source
	if sourceVM.Metadata.Labels != nil {
		for k, v := range sourceVM.Metadata.Labels {
			newVM.Metadata.Labels[k] = v
		}
	}
	newVM.Metadata.Labels["cloned_from"] = sourceVMID
	newVM.Metadata.Labels["clone_timestamp"] = time.Now().Format(time.RFC3339)
	newVM.Metadata.Labels["reuse_existing_rootfs"] = "true" // Use cloned zvol

	// Override with custom resource specs if provided
	if req.ResourceSpecs != nil {
		newVM.Resources = *req.ResourceSpecs
	}

	// Apply custom network configuration if provided
	if req.NetworkConfig != nil {
		if req.NetworkConfig.AssignmentMethod != "" {
			newVM.Network.AssignmentMethod = models.NetworkMode(req.NetworkConfig.AssignmentMethod)
		}
		if req.NetworkConfig.MACAddress != "" {
			newVM.Network.MACAddress = req.NetworkConfig.MACAddress
		}
		if req.NetworkConfig.IPAddress != "" {
			newVM.Network.IPAddress = req.NetworkConfig.IPAddress
		}
	}

	// Apply cloud-init configuration if provided
	// Store in labels for later use during VM initialization
	if req.CloudInit != nil {
		// The clone copies the source labels (including its hostname); a
		// hostname supplied for the clone must win, or it boots as the source.
		if req.CloudInit.Hostname != "" {
			newVM.Metadata.Labels["cloudinit_hostname"] = req.CloudInit.Hostname
		}
		if req.CloudInit.Username != "" {
			newVM.Metadata.Labels["cloudinit_username"] = req.CloudInit.Username
		}
		if req.CloudInit.Password != "" {
			newVM.Metadata.Labels["cloudinit_password"] = req.CloudInit.Password
		}
		// Mark that cloud-init should be regenerated for the clone
		newVM.Metadata.Labels["regenerate_cloudinit"] = "true"
	}

	// Register new VM first
	s.setVM(newVMID, newVM)

	// Call CreateMicroVM to set up work directory and config
	if err := s.manager.CreateMicroVM(context.Background(), newVM); err != nil {
		s.deleteVMEntry(newVMID)
		backend.DeleteDisk(newVMID) // Clean up cloned disk
		http.Error(w, fmt.Sprintf("Failed to initialize cloned VM: %v", err), http.StatusInternalServerError)
		if wasRunning {
			s.manager.StartMicroVM(context.Background(), sourceVM)
			sourceVM.Status = models.MicroVMStatusRunning
		}
		return
	}

	s.persistVMState()

	// Restart source VM if it was running
	if wasRunning {
		if err := s.manager.StartMicroVM(context.Background(), sourceVM); err != nil {
			fmt.Printf("⚠️  Warning: failed to restart source VM %s: %v\n", sourceVMID, err)
		} else {
			sourceVM.Status = models.MicroVMStatusRunning
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"vm_id":        newVMID,
		"source_vm_id": sourceVMID,
		"status":       newVM.Status,
		"message":      "VM cloned successfully - use POST /vms/{id}/start to boot",
		"resources":    newVM.Resources,
		"network":      newVM.Network,
	})
}

// getConsoleLogsHandler streams console logs for a VM
func (s *HTTPServer) getConsoleLogsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	_, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Check query for raw firecracker logs
	raw := r.URL.Query().Get("raw") == "true"

	var consoleLogPath string
	if raw {
		// Stream raw firecracker.log without filtering
		consoleLogPath = filepath.Join(filepath.Dir(s.manager.GetConsoleLogPath(vmID)), "firecracker.log")
	} else {
		// Get filtered console view
		consoleLogPath = s.manager.GetConsoleLogPath(vmID)
		// Extract console output from firecracker logs
		if err := s.extractConsoleOutput(vmID); err != nil {
			fmt.Printf("Warning: failed to extract console output: %v\n", err)
		}
	}

	// Check if console log exists
	if _, err := os.Stat(consoleLogPath); os.IsNotExist(err) {
		http.Error(w, "Console logs not available", http.StatusNotFound)
		return
	}

	// Parse query parameters
	follow := r.URL.Query().Get("follow") == "true"
	linesParam := r.URL.Query().Get("lines")
	since := r.URL.Query().Get("since")

	// Parse lines parameter
	lines := 100 // default
	if linesParam != "" {
		if parsedLines, err := strconv.Atoi(linesParam); err == nil && parsedLines > 0 {
			lines = parsedLines
		}
	}

	// Set appropriate headers
	w.Header().Set("Content-Type", "text/plain")
	if follow {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}

	// Open console log file
	file, err := os.Open(consoleLogPath)
	if err != nil {
		http.Error(w, "Failed to open console log", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	if follow {
		// Stream logs with follow (tail -f behavior)
		s.streamConsoleLogsFollow(w, r, file, lines, since)
	} else {
		// Return last N lines
		s.streamConsoleLogsStatic(w, file, lines, since)
	}
}

// streamConsoleLogsStatic returns the last N lines from console log
func (s *HTTPServer) streamConsoleLogsStatic(w http.ResponseWriter, file *os.File, lines int, since string) {
	// Read all lines from file
	scanner := bufio.NewScanner(file)
	var allLines []string

	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	// Get last N lines
	startIndex := 0
	if len(allLines) > lines {
		startIndex = len(allLines) - lines
	}

	// Write lines to response
	for i := startIndex; i < len(allLines); i++ {
		fmt.Fprintln(w, allLines[i])
	}
}

// streamConsoleLogsFollow implements tail -f behavior for console logs
func (s *HTTPServer) streamConsoleLogsFollow(w http.ResponseWriter, r *http.Request, file *os.File, lines int, since string) {
	// First, send existing lines
	s.streamConsoleLogsStatic(w, file, lines, since)

	// Flush initial content
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Get current file position
	currentPos, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return
	}

	// Poll for new content
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check if file has grown
			stat, err := file.Stat()
			if err != nil {
				return
			}

			if stat.Size() < currentPos {
				// The log was truncated or rotated by the sweeper (issue #28).
				// Start over from the beginning instead of stalling forever.
				currentPos = 0
				file.Seek(0, io.SeekStart)
				continue
			}

			if stat.Size() > currentPos {
				// Read new content
				file.Seek(currentPos, io.SeekStart)
				scanner := bufio.NewScanner(file)

				for scanner.Scan() {
					fmt.Fprintln(w, scanner.Text())
				}

				// Update position
				currentPos, _ = file.Seek(0, io.SeekEnd)

				// Flush new content
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

// syncVMRealState checks the actual process state and syncs with stored state
func (s *HTTPServer) syncVMRealState(vm *models.MicroVMInstance) {
	// If no manager is attached (tests or isolated server), skip synchronization
	if s.manager == nil {
		return
	}

	// Check if the VM process is actually running
	isRunning := s.manager.IsVMProcessRunning(vm.ID)

	// Sync status if it doesn't match reality
	if isRunning && vm.Status != models.MicroVMStatusRunning {
		log.Printf("Syncing VM %s status: stored=%s, actual=running\n", vm.ID, vm.Status)
		vm.Status = models.MicroVMStatusRunning
		metrics.IncCounter(metrics.StatusSyncs, map[string]string{"result": "to_running"})
		// Update process info
		if err := s.manager.UpdateVMProcessInfo(vm); err != nil {
			log.Printf("Warning: Failed to update process info for VM %s: %v\n", vm.ID, err)
		}
		s.persistVMState()
	} else if !isRunning && vm.Status == models.MicroVMStatusRunning {
		log.Printf("Syncing VM %s status: stored=running, actual=stopped\n", vm.ID)
		vm.Status = models.MicroVMStatusStopped
		vm.ClearProcessInfo()
		metrics.IncCounter(metrics.StatusSyncs, map[string]string{"result": "to_stopped"})
		if err := s.saveVMsToDisk(); err != nil {
			log.Printf("❌ ERROR: Failed to persist VM %s status change to disk: %v\n", vm.ID, err)
		} else {
			log.Printf("✅ Successfully persisted VM %s status to stopped\n", vm.ID)
		}
	}
	// No metric on the no-op path: with ~50 VMs per refresh cycle it would be pure noise.
	// Liveness of the sync loop is visible via microvm_vms_total refreshes.
}

// discoverVMIP attempts to discover the actual IP address a VM is using
func (s *HTTPServer) discoverVMIP(vmID string) string {
	vm, exists := s.getVM(vmID)
	if !exists {
		return ""
	}

	// For DHCP mode VMs, query the DHCP lease
	if vm.Network.AssignmentMethod == models.NetworkModeDHCP {
		if vm.Network.MACAddress != "" && vm.Network.BridgeName != "" {
			// Try to get DHCP lease IP using the network manager
			if dhcpIP, err := s.manager.GetNetworkManager().GetDHCPLeaseIP(vm.Network.MACAddress, vm.Network.BridgeName); err == nil && dhcpIP != "" {
				// Update the VM's stored IP address
				vm.Network.IPAddress = dhcpIP
				return dhcpIP
			}
		}
		// If DHCP lease not found yet, return stored IP (might be 0.0.0.0)
		return vm.Network.IPAddress
	}

	// For NAT mode, calculate the expected IP for this VM using the same hash algorithm
	hash := 0
	for _, c := range vmID {
		hash = hash*31 + int(c)
	}
	ipSuffix := (hash % 90) + 10 // Range 10-99
	if ipSuffix < 0 {
		ipSuffix = -ipSuffix
	}
	expectedIP := fmt.Sprintf("192.168.100.%d", ipSuffix)

	// Only try the calculated IP for this specific VM
	// This ensures each VM gets its unique IP
	if s.pingIP(expectedIP) {
		return expectedIP
	}

	// If the expected IP is not reachable, return empty
	// This prevents VMs from "stealing" each other's IPs
	return ""
}

// pingIP checks if an IP address is reachable
func (s *HTTPServer) pingIP(ip string) bool {
	cmd := exec.Command("ping", "-c", "1", "-W", "1", ip)
	err := cmd.Run()
	return err == nil
}

// formatUptime formats a duration into a human-readable uptime string
func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	} else if d < 24*time.Hour {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	} else {
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
}

// loadVMsFromDisk loads VM state from SQLite database
func (s *HTTPServer) loadVMsFromDisk() error {
	if s.vmStateStorage == nil {
		fmt.Printf("📂 SQLite storage not initialized, starting fresh\n")
		return nil
	}

	vms, err := s.vmStateStorage.LoadAllVMs()
	if err != nil {
		return fmt.Errorf("failed to load VMs from SQLite: %w", err)
	}

	s.vms = vms
	fmt.Printf("📂 Loaded %d VMs from SQLite database\n", len(vms))

	// Verify which VMs are actually still running using proper process detection
	runningCount := 0
	tapUpdated := 0
	for vmID, vm := range s.snapshotVMMap() {
		if s.manager.IsVMProcessRunning(vmID) {
			vm.Status = models.MicroVMStatusRunning
			// Update process info for running VMs
			if err := s.manager.UpdateVMProcessInfo(vm); err != nil {
				fmt.Printf("⚠️  Failed to update process info for VM %s: %v\n", vmID, err)
				// Fallback: recover PID from systemd (covers VMs with no pid file on disk)
				if reattachErr := s.reattachVMProcess(vm); reattachErr != nil {
					fmt.Printf("⚠️  Could not reattach process info for VM %s: %v\n", vmID, reattachErr)
				}
			}
			// Populate / correct tap_device for running VMs.
			// Priority: (1) fdinfo from live process (most authoritative),
			//           (2) config.json (may be stale after migration).
			if vm.ProcessInfo != nil && vm.ProcessInfo.PID > 0 {
				if realTAP, err := getActualTAPFromPID(vm.ProcessInfo.PID); err == nil && realTAP != "" {
					if vm.Network.TAPDevice != realTAP {
						fmt.Printf("🔧 VM %s: correcting tap_device %q → %q (from fdinfo)\n", vmID, vm.Network.TAPDevice, realTAP)
						vm.Network.TAPDevice = realTAP
						tapUpdated++
					}
				} else if vm.Network.TAPDevice == "" {
					if tap := s.getTAPDeviceName(vmID); tap != "" {
						vm.Network.TAPDevice = tap
						tapUpdated++
						fmt.Printf("🔧 Populated tap_device=%s for VM %s from config.json\n", tap, vmID)
					}
				}
			} else if vm.Network.TAPDevice == "" {
				if tap := s.getTAPDeviceName(vmID); tap != "" {
					vm.Network.TAPDevice = tap
					tapUpdated++
					fmt.Printf("🔧 Populated tap_device=%s for VM %s from config.json\n", tap, vmID)
				}
			}
			// Re-open console FIFO keeper fd so WebSocket console input works after restart
			s.manager.ReopenConsoleFIFO(vmID)
			runningCount++
		} else {
			vm.Status = models.MicroVMStatusStopped
			// Clear process info for stopped VMs
			vm.ClearProcessInfo()
		}
	}

	fmt.Printf("📊 Found %d VMs still running after restart\n", runningCount)
	if tapUpdated > 0 {
		fmt.Printf("💾 Persisting tap_device for %d VMs back to SQLite\n", tapUpdated)
		s.persistVMState()
	}
	return nil
}

// saveVMsToDisk saves VM state to SQLite database
func (s *HTTPServer) saveVMsToDisk() error {
	if s.vmStateStorage == nil {
		return fmt.Errorf("SQLite storage not initialized")
	}

	// Snapshot under lock, then write to SQLite unlocked: holding vmsMutex
	// across slow disk I/O would stall every API handler and background loop.
	vms := s.snapshotVMMap()

	// Save each active VM to SQLite
	for _, vm := range vms {
		if err := s.vmStateStorage.SaveVM(vm); err != nil {
			return fmt.Errorf("failed to save VM %s: %w", vm.ID, err)
		}
	}

	// Remove from SQLite any VMs that were deleted from the in-memory map
	activeIDs := make(map[string]struct{}, len(vms))
	for id := range vms {
		activeIDs[id] = struct{}{}
	}
	if err := s.vmStateStorage.SyncVMs(activeIDs); err != nil {
		log.Printf("⚠️  Failed to sync VM deletions to SQLite: %v\n", err)
	}

	return nil
}

// isVMActuallyRunning checks if a VM is actually running by checking processes
func (s *HTTPServer) isVMActuallyRunning(vmID string) bool {
	return s.manager.IsVMProcessRunning(vmID)
}

// reattachVMProcess recovers ProcessInfo for a VM whose firecracker.pid file is
// missing but whose systemd service is active. This covers VMs that predate the
// current service work-dir (e.g. recovered from billing panel without full disk state).
func (s *HTTPServer) reattachVMProcess(vm *models.MicroVMInstance) error {
	serviceName := fmt.Sprintf("firecracker-vm@%s", vm.ID)
	out, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", serviceName).Output()
	if err != nil {
		return fmt.Errorf("systemctl show failed: %w", err)
	}
	pidStr := strings.TrimSpace(string(out))
	if pidStr == "0" || pidStr == "" {
		return fmt.Errorf("systemd MainPID is 0 (service not active)")
	}
	var pid int
	if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == 0 {
		return fmt.Errorf("invalid PID from systemd: %q", pidStr)
	}

	workDir := resolveWorkDir()
	microvmDir := filepath.Join(workDir, "microvms", vm.ID)
	if err := os.MkdirAll(microvmDir, 0755); err != nil {
		return fmt.Errorf("failed to create microvm dir: %w", err)
	}

	// Persist pid file so subsequent calls to GetVMProcessInfo succeed.
	pidFile := filepath.Join(microvmDir, "firecracker.pid")
	if writeErr := os.WriteFile(pidFile, []byte(pidStr), 0644); writeErr != nil {
		log.Printf("⚠️  Could not write pid file for VM %s: %v", vm.ID, writeErr)
	}

	socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)
	configPath := filepath.Join(microvmDir, "config.json")
	vm.ProcessInfo = &models.ProcessInfo{
		PID:        pid,
		StartTime:  time.Now(),
		ConfigPath: configPath,
		SocketPath: socketPath,
	}
	log.Printf("🔗 Reattached process info for VM %s (PID=%d via systemd)", vm.ID, pid)
	return nil
}

// persistVMState saves VM state after any change
func (s *HTTPServer) persistVMState() {
	if err := s.saveVMsToDisk(); err != nil {
		log.Printf("⚠️  Failed to persist VM state: %v\n", err)
	}
}

// autoStartVMs automatically starts VMs that were running before server shutdown
func (s *HTTPServer) autoStartVMs() {
	if s.vmCount() == 0 {
		fmt.Printf("📋 No VMs to auto-start\n")
		return
	}

	// First, detect which VMs are actually still running
	runningVMs, err := s.manager.DetectRunningVMs()
	if err != nil {
		fmt.Printf("⚠️  Failed to detect running VMs: %v\n", err)
		runningVMs = []string{}
	}

	fmt.Printf("🔍 Detected %d VMs still running after restart\n", len(runningVMs))

	// Update status for VMs that are still running
	for _, vmID := range runningVMs {
		if vm, exists := s.getVM(vmID); exists {
			vm.Status = models.MicroVMStatusRunning
			vm.UpdateLastActivity()
			// Update process info
			if err := s.manager.UpdateVMProcessInfo(vm); err != nil {
				fmt.Printf("⚠️  Failed to update process info for running VM %s: %v\n", vmID, err)
				// Fallback: recover PID from systemd (covers VMs with no pid file on disk)
				if reattachErr := s.reattachVMProcess(vm); reattachErr != nil {
					fmt.Printf("⚠️  Could not reattach process info for VM %s: %v\n", vmID, reattachErr)
				}
			}
			fmt.Printf("✅ VM %s was already running\n", vmID)
		}
	}

	// Check for VMs that should be auto-started but aren't running
	autoStartCount := 0
	for vmID, vm := range s.snapshotVMMap() {
		// Skip VMs that are already running
		if contains(runningVMs, vmID) {
			continue
		}

		// IMPORTANT: If auto_start is explicitly disabled, respect that setting
		// and skip all legacy/activity-based auto-start logic
		if !vm.ShouldAutoStart() {
			fmt.Printf("⏭️  Skipping VM %s - auto_start is disabled\n", vmID)
			// Ensure status is marked as stopped since it's not running
			vm.Status = models.MicroVMStatusStopped
			continue
		}

		// Auto-start is enabled, check if we should start this VM
		shouldAutoStart := true // Default to true since auto_start flag is set

		// For extra safety, also check legacy conditions
		// VM was marked as running (legacy support) - should still auto-start
		if vm.Status == models.MicroVMStatusRunning {
			shouldAutoStart = true
		}

		// VM had recent activity (were likely running) - should still auto-start
		if !vm.Metadata.LastActivity.IsZero() {
			timeSinceActivity := time.Since(vm.Metadata.LastActivity)
			if timeSinceActivity < 24*time.Hour { // If active in last 24 hours
				shouldAutoStart = true
			}
		}

		if shouldAutoStart {
			fmt.Printf("🔄 Auto-starting VM %s (IP: %s)...\n", vmID, vm.Network.IPAddress)

			// Set status to creating
			vm.Status = models.MicroVMStatusCreating

			// Start VM synchronously to ensure proper startup
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

			if err := s.manager.CreateMicroVM(ctx, vm); err != nil {
				fmt.Printf("❌ Failed to auto-start VM %s: %v\n", vmID, err)
				vm.Status = models.MicroVMStatusError
				cancel()
				continue
			}

			// Invalidate TAP cache after auto-start
			s.invalidateTAPCache(vmID)
			cancel()

			// Verify VM is actually running
			if s.manager.IsVMProcessRunning(vmID) {
				vm.Status = models.MicroVMStatusRunning
				vm.UpdateLastActivity()
				fmt.Printf("✅ Successfully auto-started VM %s\n", vmID)
				// For DHCP VMs, run a brief fast discovery to latch the lease promptly
				if vm.Network.AssignmentMethod == models.NetworkModeDHCP {
					go s.startFastDHCPDiscovery(vm.ID, 90*time.Second, 3*time.Second)
				}
				autoStartCount++
			} else {
				vm.Status = models.MicroVMStatusError
				fmt.Printf("❌ VM %s failed to start properly\n", vmID)
			}
		}
	}

	if autoStartCount > 0 {
		fmt.Printf("🚀 Successfully auto-started %d VMs\n", autoStartCount)
		// Persist the updated states
		s.persistVMState()
	} else {
		fmt.Printf("📋 No VMs were eligible for auto-start\n")
	}
}

// parseVMUnitName extracts the firecracker-vm unit name from a systemctl list-units row.
// systemctl prefixes failed units with a red "●" bullet (and other state markers), so the
// unit name is NOT necessarily the first whitespace-separated field.
func parseVMUnitName(line string) (unitName, vmID string, ok bool) {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "firecracker-vm@") {
			unitName = f
			vmID = strings.TrimPrefix(f, "firecracker-vm@")
			vmID = strings.TrimSuffix(vmID, ".service")
			return unitName, vmID, true
		}
	}
	return "", "", false
}

// listFailedVMUnits returns the raw output of systemctl list-units for failed firecracker VMs.
func listFailedVMUnits() (string, error) {
	cmd := exec.Command("systemctl", "list-units", "firecracker-vm@*", "--state=failed",
		"--no-legend", "--no-pager", "--plain", "--full")
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=0", "TERM=dumb")
	out, err := cmd.Output()
	return string(out), err
}

// cleanupOrphanedSystemdUnits removes failed systemd units for VMs that no longer exist
func (s *HTTPServer) cleanupOrphanedSystemdUnits() {
	output, err := listFailedVMUnits()
	if err != nil {
		return
	}

	cleaned := 0
	failed := 0
	ownerVMIDs := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		unitName, vmID, ok := parseVMUnitName(line)
		if !ok || vmID == "" {
			continue
		}
		if ownerVMIDs[vmID] {
			continue
		}
		ownerVMIDs[vmID] = true

		s.vmsMutex.RLock()
		_, exists := s.vms[vmID]
		s.vmsMutex.RUnlock()

		if exists {
			continue
		}

		if out, err := exec.Command("systemctl", "reset-failed", unitName).CombinedOutput(); err != nil {
			failed++
			log.Printf("🧹 Warning: reset-failed %s failed: %v (output: %s)", unitName, err, strings.TrimSpace(string(out)))
			continue
		}
		cleaned++
	}

	if cleaned > 0 || failed > 0 {
		log.Printf("🧹 Orphaned systemd units: cleaned=%d failed=%d", cleaned, failed)
	}

	// Verify: re-read the failed list so a silently-failing cleanup is visible
	if remaining, err := listFailedVMUnits(); err == nil {
		remainingCount := 0
		for _, l := range strings.Split(strings.TrimSpace(remaining), "\n") {
			if strings.TrimSpace(l) != "" {
				remainingCount++
			}
		}
		if remainingCount > 0 {
			log.Printf("🧹 Warning: %d failed units remain after cleanup", remainingCount)
		} else {
			log.Printf("🧹 Verified: no failed firecracker-vm units remain")
		}
	}
}

func (s *HTTPServer) cleanupOrphanedHandler(w http.ResponseWriter, r *http.Request) {
	s.cleanupOrphanedSystemdUnits()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Orphaned units cleaned up",
	})
}

func (s *HTTPServer) listOrphanedUnitsHandler(w http.ResponseWriter, r *http.Request) {
	output, err := listFailedVMUnits()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list units: %v", err), http.StatusInternalServerError)
		return
	}

	var orphaned []string
	for _, line := range strings.Split(output, "\n") {
		unitName, vmID, ok := parseVMUnitName(line)
		if !ok {
			continue
		}

		s.vmsMutex.RLock()
		_, exists := s.vms[vmID]
		s.vmsMutex.RUnlock()

		if !exists {
			orphaned = append(orphaned, unitName)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"orphaned_units": orphaned,
		"count":          len(orphaned),
	})
}

// reconcileVMStates checks all VMs marked as running and verifies they are actually alive
func (s *HTTPServer) reconcileVMStates() {
	s.vmsMutex.Lock()
	defer s.vmsMutex.Unlock()

	runningCount := 0
	staleCount := 0

	for vmID, vm := range s.vms {
		if vm.Status != models.MicroVMStatusRunning {
			continue
		}

		isRunning := s.manager.IsVMProcessRunning(vmID)
		if !isRunning {
			fmt.Printf("⚠️  VM %s marked running but process is dead — marking as stopped\n", vmID)
			vm.Status = models.MicroVMStatusStopped
			vm.ClearProcessInfo()
			staleCount++
		} else {
			runningCount++
		}
	}

	if staleCount > 0 {
		s.persistVMState()
	}

	fmt.Printf("🔍 Reconciliation complete: %d running, %d stale-stopped\n", runningCount, staleCount)
}

// gracefulShutdown handles graceful shutdown of the server and VMs.
// Running VMs are intentionally LEFT RUNNING (they are independent systemd
// units and survive service restarts — verified in production). This only
// quiesces service-side work and persists final state.
func (s *HTTPServer) gracefulShutdown() {
	fmt.Println("🛑 Initiating graceful shutdown...")

	// Stop accepting new creation work and wait (bounded) for in-flight creations.
	// A half-created VM leaves partial disk/network state, so drain first.
	fmt.Println("⏳ Draining VM creation queue...")
	s.drainCreationQueue(10 * time.Second)

	// Update VM statuses before shutdown
	// IMPORTANT: Preserve user's auto_start preference - don't override it
	fmt.Println("📊 Updating VM statuses before shutdown...")
	s.vmsMutex.Lock()
	for vmID, vm := range s.vms {
		if s.manager.IsVMProcessRunning(vmID) {
			// Mark as running but PRESERVE the auto_start setting
			// If user explicitly disabled auto_start, don't re-enable it
			vm.Status = models.MicroVMStatusRunning
			vm.UpdateLastActivity()
			if vm.ShouldAutoStart() {
				fmt.Printf("📝 VM %s marked as running (will auto-start)\n", vmID)
			} else {
				fmt.Printf("📝 VM %s marked as running (auto-start DISABLED by user)\n", vmID)
			}
		} else {
			vm.Status = models.MicroVMStatusStopped
			// Don't change auto-start flag for stopped VMs - preserve user preference
			fmt.Printf("📝 VM %s marked as stopped\n", vmID)
		}
	}
	s.vmsMutex.Unlock()

	// Persist final state
	if err := s.saveVMsToDisk(); err != nil {
		fmt.Printf("⚠️  Failed to save final VM state: %v\n", err)
	} else {
		fmt.Printf("💾 Final VM state saved successfully\n")
	}

	fmt.Println("✅ Graceful shutdown complete")
}

// getTAPDeviceName returns the actual TAP device name for a VM by reading the firecracker config
func (s *HTTPServer) getTAPDeviceName(vmID string) string {
	// Get working directory from environment or use default
	workDir := os.Getenv("MICROVM_WORK_DIR")
	if workDir == "" {
		workDir = resolveWorkDir()
	}
	configPath := filepath.Join(workDir, "microvms", vmID, "config.json")

	// Try to read the firecracker config to get the actual TAP device name
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Fallback to firecracker manager's naming pattern
		return s.generateFallbackTAPName(vmID)
	}

	// Parse JSON to extract TAP device name
	var config struct {
		NetworkInterfaces []struct {
			HostDevName string `json:"host_dev_name"`
		} `json:"network-interfaces"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return s.generateFallbackTAPName(vmID)
	}

	if len(config.NetworkInterfaces) > 0 && config.NetworkInterfaces[0].HostDevName != "" {
		tapDevice := config.NetworkInterfaces[0].HostDevName
		// Verify the TAP device actually exists on the system
		if s.verifyTAPDeviceExists(tapDevice) {
			return tapDevice
		}
	}

	// Fallback to firecracker manager's naming pattern
	return s.generateFallbackTAPName(vmID)
}

// generateFallbackTAPName generates a fallback TAP device name using the same logic as firecracker manager
func (s *HTTPServer) generateFallbackTAPName(vmID string) string {
	// Use the same hashing logic as the firecracker manager's tapNameFor method
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

// verifyTAPDeviceExists checks if a TAP device actually exists on the system
func (s *HTTPServer) verifyTAPDeviceExists(tapName string) bool {
	cmd := exec.Command("ip", "link", "show", tapName)
	err := cmd.Run()
	return err == nil
}

// refreshTAPDeviceInfo refreshes TAP device information for a VM with caching
func (s *HTTPServer) refreshTAPDeviceInfo(vmID string) string {
	// Check cache first (cache for 30 seconds)
	s.cacheMutex.RLock()
	if entry, exists := s.tapCache[vmID]; exists {
		if time.Since(entry.timestamp) < 30*time.Second {
			s.cacheMutex.RUnlock()
			return entry.tapDevice
		}
	}
	s.cacheMutex.RUnlock()

	// Cache miss or expired, refresh the information
	tapName := s.getTAPDeviceName(vmID)

	// If we got a valid TAP device name and it exists, cache and return it
	if tapName != "" && s.verifyTAPDeviceExists(tapName) {
		s.cacheTAPDevice(vmID, tapName)
		return tapName
	}

	// If configuration method failed, try to discover by scanning network interfaces
	// Look for TAP devices that might belong to this VM
	cmd := exec.Command("ip", "link", "show", "type", "tuntap")
	output, err := cmd.Output()
	if err != nil {
		s.cacheTAPDevice(vmID, tapName) // Cache even empty result to avoid repeated failures
		return tapName                  // Return whatever we had, even if empty
	}

	// Parse output to find potential TAP devices
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "tap") && strings.Contains(line, "state UP") {
			// Extract interface name
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ifaceName := strings.TrimSuffix(parts[1], ":")
				// Check if this could be our TAP device by checking if it's attached to the right bridge
				if s.isTAPDeviceForVM(ifaceName, vmID) {
					s.cacheTAPDevice(vmID, ifaceName)
					return ifaceName
				}
			}
		}
	}

	// Cache the result even if discovery failed
	s.cacheTAPDevice(vmID, tapName)
	return tapName // Return original result if discovery failed
}

// cacheTAPDevice caches TAP device information for a VM
func (s *HTTPServer) cacheTAPDevice(vmID, tapDevice string) {
	s.cacheMutex.Lock()
	defer s.cacheMutex.Unlock()
	s.tapCache[vmID] = tapCacheEntry{
		tapDevice: tapDevice,
		timestamp: time.Now(),
	}
}

// invalidateTAPCache invalidates the TAP device cache for a VM
func (s *HTTPServer) invalidateTAPCache(vmID string) {
	s.cacheMutex.Lock()
	defer s.cacheMutex.Unlock()
	delete(s.tapCache, vmID)
}

// isTAPDeviceForVM checks if a TAP device belongs to a specific VM
func (s *HTTPServer) isTAPDeviceForVM(tapName, vmID string) bool {
	// Check if the TAP device is attached to the expected bridge
	cmd := exec.Command("ip", "link", "show", tapName)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Look for master bridge information
	outputStr := string(output)
	if strings.Contains(outputStr, "master fc-br0") || strings.Contains(outputStr, "master lxdbr0") {
		// This is a potential match - we could do more sophisticated matching here
		// For now, we'll use a simple heuristic
		return true
	}

	return false
}

// isTAPDeviceUp checks if a TAP device is up and running
func (s *HTTPServer) isTAPDeviceUp(tapName string) bool {
	cmd := exec.Command("ip", "link", "show", tapName)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "state UP")
}

// ensureTAPDeviceUp ensures a TAP device is created and up
func (s *HTTPServer) ensureTAPDeviceUp(vmID string) error {
	// For now, let the firecracker manager handle TAP device creation
	// This function will be called after VM creation to verify the device
	return nil
}

// verifyAndFixTAPDevice verifies TAP device is up and fixes it if needed
func (s *HTTPServer) verifyAndFixTAPDevice(vmID string) error {
	// Minimal verification: ensure the TAP interface for this VM exists and is up.
	tapName := s.refreshTAPDeviceInfo(vmID)
	if tapName == "" {
		return nil
	}
	if !s.isTAPDeviceUp(tapName) {
		cmd := exec.Command("sudo", "ip", "link", "set", "dev", tapName, "up")
		_ = cmd.Run() // best effort; ignore error to avoid spamming logs
	}
	return nil
}

// ensureBridgeNetwork validates that the configured bridge exists
// Only manages bridges explicitly configured as managed (MICROVM_MANAGE_BRIDGE=true)
func (s *HTTPServer) ensureBridgeNetwork() error {
	// Get configured bridge name from environment (REQUIRED)
	bridgeName := os.Getenv("MICROVM_DHCP_BRIDGE")
	if bridgeName == "" {
		return fmt.Errorf("❌ MICROVM_DHCP_BRIDGE environment variable not set.\n" +
			"Please configure the bridge name in /etc/systemd/system/vmlet.service:\n" +
			"Environment=MICROVM_DHCP_BRIDGE=lxdbr0  (or virbr0, br0, etc.)")
	}

	// Check if this is a managed bridge (we created it) or external
	manageBridge := getEnvBool("MICROVM_MANAGE_BRIDGE", false)

	// Check if bridge exists
	cmd := exec.Command("ip", "link", "show", bridgeName)
	if err := cmd.Run(); err != nil {
		if manageBridge {
			// This is our managed bridge - we should create it
			return fmt.Errorf("❌ Managed bridge %s not found. Cannot create bridges automatically.\n"+
				"Please create manually: sudo ip link add %s type bridge && sudo ip link set %s up",
				bridgeName, bridgeName, bridgeName)
		}
		// External bridge doesn't exist - error
		return fmt.Errorf("❌ Bridge %s not found.\n"+
			"For LXD: lxdbr0 should exist (check: lxc network list)\n"+
			"For libvirt: virbr0 should exist (check: virsh net-list)\n"+
			"For custom: sudo ip link add %s type bridge && sudo ip link set %s up",
			bridgeName, bridgeName, bridgeName)
	}

	// Bridge exists - only manage it if MICROVM_MANAGE_BRIDGE=true
	if manageBridge {
		// Bring our managed bridge up if needed
		if err := exec.Command("sudo", "ip", "link", "set", "dev", bridgeName, "up").Run(); err != nil {
			return fmt.Errorf("failed to bring managed bridge up: %w", err)
		}
		fmt.Printf("✅ Managed bridge %s is ready\n", bridgeName)
	} else {
		// External bridge - just verify it exists, don't touch it
		fmt.Printf("✅ Using external bridge %s (managed by LXD/libvirt/system)\n", bridgeName)
	}

	return nil
}

// startTAPDeviceMonitor starts a background task to monitor and fix TAP devices
func (s *HTTPServer) startTAPDeviceMonitor() {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	fmt.Printf("🔍 Starting TAP device monitor\n")
	for range ticker.C {
		s.checkAndFixTAPDevices()
	}
}

// startFastDHCPDiscovery polls DHCP lease files quickly for a short window to capture the VM's IP soon after boot
func (s *HTTPServer) startFastDHCPDiscovery(vmID string, timeout time.Duration, interval time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		vm, exists := s.getVM(vmID)
		if !exists {
			return
		}
		// Only for DHCP-configured VMs
		if vm.Network.AssignmentMethod != models.NetworkModeDHCP {
			return
		}
		// If we already have a non-empty IP, stop early
		if vm.Network.IPAddress != "" && vm.Network.IPAddress != "0.0.0.0" {
			return
		}
		if vm.Network.MACAddress != "" && vm.Network.BridgeName != "" {
			if ip, err := s.manager.GetNetworkManager().GetDHCPLeaseIP(vm.Network.MACAddress, vm.Network.BridgeName); err == nil && ip != "" {
				if ip != vm.Network.IPAddress {
					vm.Network.IPAddress = ip
					s.persistVMState()
					s.invalidateVMCache(vmID)
					log.Printf("[dhcp] Fast discovery: VM %s acquired IP %s", vmID, ip)
				}
				return
			}
		}
		time.Sleep(interval)
	}
}

// checkAndFixTAPDevices checks all running VMs and fixes their TAP devices if needed
func (s *HTTPServer) checkAndFixTAPDevices() {
	for vmID, vm := range s.snapshotVMMap() {
		if vm.Status == models.MicroVMStatusRunning && s.isVMActuallyRunning(vmID) {
			// Verify and fix TAP device
			if err := s.verifyAndFixTAPDevice(vmID); err != nil {
				fmt.Printf("⚠️  Failed to fix TAP device for VM %s: %v\n", vmID, err)
			}
		}
	}
}

// contains checks if a string slice contains a specific string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// extractConsoleOutput extracts VM console output from firecracker logs
func (s *HTTPServer) extractConsoleOutput(vmID string) error {
	// If systemd redirects Firecracker stdout to console.log and it's non-empty, skip extraction
	consoleLogPath := s.manager.GetConsoleLogPath(vmID)
	if info, err := os.Stat(consoleLogPath); err == nil && info.Size() > 0 {
		return nil
	}

	// Fallback: extract from firecracker.log in the same VM dir
	firecrackerLogPath := filepath.Join(filepath.Dir(consoleLogPath), "firecracker.log")
	firecrackerLog, err := os.ReadFile(firecrackerLogPath)
	if err != nil {
		return fmt.Errorf("failed to read firecracker log: %w", err)
	}
	lines := strings.Split(string(firecrackerLog), "\n")
	var consoleLines []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Include kernel bracketed lines and known markers
		if strings.HasPrefix(strings.TrimSpace(line), "[") ||
			strings.Contains(line, "MicroVM starting") ||
			strings.Contains(line, "System ready") ||
			strings.Contains(line, "Attempting DHCP") ||
			strings.Contains(line, "DHCP failed") ||
			strings.Contains(line, "BusyBox") ||
			strings.Contains(line, "can't access tty") {
			consoleLines = append(consoleLines, line)
		}
	}
	return os.WriteFile(consoleLogPath, []byte(strings.Join(consoleLines, "\n")), 0644)
}

// setAutoStartHandler manages auto-start settings for a VM
func (s *HTTPServer) setAutoStartHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	var req struct {
		AutoStart bool `json:"auto_start"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Update auto-start setting
	vm.SetAutoStart(req.AutoStart)

	// Invalidate cache for this VM
	s.invalidateVMCache(vmID)

	// Persist the change
	s.persistVMState()

	response := map[string]interface{}{
		"message":    "Auto-start setting updated",
		"vm_id":      vmID,
		"auto_start": vm.ShouldAutoStart(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// syncVMConfigHandler accepts a billing-provided VM config.json payload and updates
// the persisted VM state accordingly. It does not create or start VMs; it only
// syncs fields such as network, resources, and storage paths into the in-memory
// `s.vms` map and persists the state.
func (s *HTTPServer) syncVMConfigHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	if vmID == "" {
		http.Error(w, "missing vm id", http.StatusBadRequest)
		return
	}

	// Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Parse incoming config.json (generic map)
	var cfg map[string]interface{}
	if err := json.Unmarshal(body, &cfg); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	s.cacheMutex.Lock()
	defer s.cacheMutex.Unlock()

	vm, exists := s.getVM(vmID)
	if !exists {
		// Create a minimal VM entry when missing
		vm = &models.MicroVMInstance{
			ID:     vmID,
			NodeID: s.nodeDomain,
			Status: models.MicroVMStatusStopped,
			Resources: models.ResourceSpecs{
				VCPUs:    1,
				MemoryMB: 512,
			},
			Storage: models.StorageConfig{Type: models.StorageTypeLocal},
			Network: models.NetworkInfo{AssignmentMethod: models.NetworkModeDHCP},
			Metadata: models.MicroVMMetadata{
				CreatedAt:    time.Now(),
				LastActivity: time.Now(),
				CustomerID:   "billing-sync",
				AutoStart:    false,
				Labels:       map[string]string{"reconstructed": "true"},
			},
		}
		s.setVM(vmID, vm)
	}

	// Support two incoming payload shapes:
	// 1) Firecracker-style config.json (machine-config, network-interfaces, drives)
	// 2) Billing-created VM payload (top-level vcpus, memory_mb, storage_mb, image_id, network, cloudinit)

	// If billing-style top-level fields exist, prefer them
	if vcpuTop, ok := cfg["vcpus"].(float64); ok && int(vcpuTop) > 0 {
		vm.Resources.VCPUs = uint32(vcpuTop)
	}
	if memTop, ok := cfg["memory_mb"].(float64); ok && int(memTop) > 0 {
		vm.Resources.MemoryMB = uint32(memTop)
	}
	if storTop, ok := cfg["storage_mb"].(float64); ok && int(storTop) > 0 {
		vm.Resources.StorageMB = uint32(storTop)
	}
	if imgTop, ok := cfg["image_id"].(string); ok && imgTop != "" {
		if vm.Metadata.Labels == nil {
			vm.Metadata.Labels = map[string]string{}
		}
		vm.Metadata.Labels["image_id"] = imgTop
		vm.Metadata.Labels["image_name"] = imgTop
	}

	// Billing-style network object (flat) handling
	if netTop, ok := cfg["network"].(map[string]interface{}); ok {
		if am, ok := netTop["assignment_method"].(string); ok && am != "" {
			switch strings.ToLower(am) {
			case "dhcp":
				vm.Network.AssignmentMethod = models.NetworkModeDHCP
			case "macvlan":
				vm.Network.AssignmentMethod = models.NetworkModeMACVLAN
			case "ipv6-only":
				vm.Network.AssignmentMethod = models.NetworkModeIPv6Only
			default:
				// keep existing
			}
		}
		if mac, ok := netTop["mac_address"].(string); ok && mac != "" {
			vm.Network.MACAddress = mac
			if vm.Network.AssignmentMethod == "" {
				vm.Network.AssignmentMethod = models.NetworkModeMACVLAN
			}
		}
		if ip, ok := netTop["ip_address"].(string); ok && ip != "" {
			vm.Network.IPAddress = ip
		}
		if gw, ok := netTop["gateway"].(string); ok && gw != "" {
			if vm.Metadata.Labels == nil {
				vm.Metadata.Labels = map[string]string{}
			}
			vm.Metadata.Labels["gateway"] = gw
		}
		if nm, ok := netTop["netmask"].(string); ok && nm != "" {
			if vm.Metadata.Labels == nil {
				vm.Metadata.Labels = map[string]string{}
			}
			vm.Metadata.Labels["netmask"] = nm
		}
		if bi, ok := netTop["bandwidth_ingress"].(string); ok && bi != "" {
			vm.Network.BandwidthIngress = bi
		}
		if be, ok := netTop["bandwidth_egress"].(string); ok && be != "" {
			vm.Network.BandwidthEgress = be
		}
		if sshEnable, ok := netTop["enable_ssh"].(bool); ok && sshEnable {
			vm.Network.SSHPort = 22
		}
	}

	// cloudinit top-level object (billing payload)
	if ci, ok := cfg["cloudinit"].(map[string]interface{}); ok {
		if vm.Metadata.Labels == nil {
			vm.Metadata.Labels = map[string]string{}
		}
		if u, ok := ci["username"].(string); ok && u != "" {
			vm.Metadata.Labels["cloudinit_username"] = u
		}
		if p, ok := ci["password"].(string); ok && p != "" {
			vm.Metadata.Labels["cloudinit_password"] = p
		}
		if hn, ok := ci["hostname"].(string); ok && hn != "" {
			vm.Metadata.Labels["hostname"] = hn
		}
	}

	// Fall back to machine-config if billing-style fields were not present
	if mc, ok := cfg["machine-config"].(map[string]interface{}); ok {
		if vcpu, ok := mc["vcpu_count"].(float64); ok && int(vcpu) > 0 {
			vm.Resources.VCPUs = uint32(vcpu)
		}
		if mem, ok := mc["mem_size_mib"].(float64); ok && int(mem) > 0 {
			vm.Resources.MemoryMB = uint32(mem)
		}
	}

	// network-interfaces (support both hyphenated and underscored keys)
	var netIfs []interface{}
	if v, ok := cfg["network-interfaces"]; ok {
		if arr, ok := v.([]interface{}); ok {
			netIfs = arr
		}
	} else if v, ok := cfg["network_interfaces"]; ok {
		if arr, ok := v.([]interface{}); ok {
			netIfs = arr
		}
	}

	if len(netIfs) > 0 {
		if ni, ok := netIfs[0].(map[string]interface{}); ok {
			if mac, ok := ni["guest_mac"].(string); ok && mac != "" {
				vm.Network.MACAddress = mac
			}
			if tap, ok := ni["host_dev_name"].(string); ok && tap != "" {
				vm.Network.TAPDevice = tap
			}
		}
	}

	// drives -> extract path_on_host
	if drives, ok := cfg["drives"].([]interface{}); ok && len(drives) > 0 {
		if d0, ok := drives[0].(map[string]interface{}); ok {
			if path, ok := d0["path_on_host"].(string); ok && path != "" {
				if strings.Contains(path, "/dev/zvol") {
					vm.Storage.Type = models.StorageConfig{Type: models.StorageType("zfs")}.Type
					if vm.Metadata.Labels == nil {
						vm.Metadata.Labels = map[string]string{}
					}
					vm.Metadata.Labels["zvol_path"] = path
				} else {
					if vm.Metadata.Labels == nil {
						vm.Metadata.Labels = map[string]string{}
					}
					vm.Metadata.Labels["storage_path"] = path
				}
			}
		}
	}

	// boot-source boot args may contain root device; preserve in labels
	if bs, ok := cfg["boot-source"].(map[string]interface{}); ok {
		if args, ok := bs["boot_args"].(string); ok && args != "" {
			if vm.Metadata.Labels == nil {
				vm.Metadata.Labels = map[string]string{}
			}
			vm.Metadata.Labels["boot_args"] = args
		}
	}

	// machine-config -> maybe also set storage size from drives or labels (best-effort)
	// network assignment method inference: if guest_mac present -> macvlan, else dhcp
	if vm.Network.MACAddress != "" {
		vm.Network.AssignmentMethod = models.NetworkModeMACVLAN
	} else {
		vm.Network.AssignmentMethod = models.NetworkModeDHCP
	}

	// persist and respond
	s.persistVMState()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "vm_id": vmID})
}

// setAutoscaleHandler configures autoscaling for a VM
func (s *HTTPServer) setAutoscaleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	var req struct {
		Enabled            bool    `json:"enabled"`
		MinMemoryMB        uint32  `json:"min_memory_mb"`
		MaxMemoryMB        uint32  `json:"max_memory_mb"`
		ScaleUpThreshold   float64 `json:"scale_up_threshold"`
		ScaleDownThreshold float64 `json:"scale_down_threshold"`
		CooldownSeconds    int     `json:"cooldown_seconds"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate thresholds
	if req.ScaleUpThreshold <= 0 || req.ScaleUpThreshold > 1 {
		http.Error(w, "scale_up_threshold must be between 0 and 1", http.StatusBadRequest)
		return
	}
	if req.ScaleDownThreshold <= 0 || req.ScaleDownThreshold >= req.ScaleUpThreshold {
		http.Error(w, "scale_down_threshold must be between 0 and scale_up_threshold", http.StatusBadRequest)
		return
	}
	if req.MinMemoryMB <= 0 || req.MaxMemoryMB <= req.MinMemoryMB {
		http.Error(w, "invalid memory range: max_memory_mb must be greater than min_memory_mb", http.StatusBadRequest)
		return
	}
	if req.MaxMemoryMB > vm.Resources.MemoryMB {
		http.Error(w, fmt.Sprintf("max_memory_mb (%d) cannot exceed VM allocated memory (%d MB)", req.MaxMemoryMB, vm.Resources.MemoryMB), http.StatusBadRequest)
		return
	}
	if req.CooldownSeconds < 10 {
		req.CooldownSeconds = 30 // Minimum 30 seconds cooldown
	}

	// Create or update autoscale configuration
	vm.Autoscale = &models.AutoscaleConfig{
		Enabled:            req.Enabled,
		MinMemoryMB:        req.MinMemoryMB,
		MaxMemoryMB:        req.MaxMemoryMB,
		ScaleUpThreshold:   req.ScaleUpThreshold,
		ScaleDownThreshold: req.ScaleDownThreshold,
		CooldownSeconds:    req.CooldownSeconds,
		LastScaleAction:    time.Time{}, // Reset last action
	}

	// If enabling autoscaling, configure balloon to start at min memory
	if req.Enabled && vm.Status == models.MicroVMStatusRunning {
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vmID)
		initialBalloonMB := vm.Resources.MemoryMB - req.MaxMemoryMB
		if initialBalloonMB > 0 {
			// Set balloon to reserve (total - max) memory
			log.Printf("🔄 VM %s: initializing autoscale, setting balloon to %d MB (total: %d MB, max: %d MB)",
				vmID, initialBalloonMB, vm.Resources.MemoryMB, req.MaxMemoryMB)
			if err := s.setBalloonSize(socketPath, uint64(initialBalloonMB)); err != nil {
				log.Printf("⚠️  VM %s: failed to initialize balloon: %v", vmID, err)
			}
		}
	}

	// Invalidate cache and persist
	s.invalidateVMCache(vmID)
	s.persistVMState()

	response := map[string]interface{}{
		"message":   "Autoscale configuration updated",
		"vm_id":     vmID,
		"autoscale": vm.Autoscale,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getMemoryBillingHandler returns memory usage for billing panel
// This endpoint shows the REAL memory available to the customer (plan memory)
// and how much of it is actually being used by the VM
func (s *HTTPServer) getMemoryBillingHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Get plan memory (what customer paid for)
	var planMemoryMB uint64
	if vm.Autoscale != nil && vm.Autoscale.MinMemoryMB > 0 {
		planMemoryMB = uint64(vm.Autoscale.MinMemoryMB)
	} else {
		planMemoryMB = uint64(vm.Resources.MemoryMB)
	}

	response := map[string]interface{}{
		"vm_id":          vmID,
		"plan_memory_mb": planMemoryMB,
		"status":         vm.Status,
	}

	// If VM is running, calculate real memory usage
	if vm.Status == models.MicroVMStatusRunning {
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vmID)
		if stats, err := s.getBalloonStats(socketPath); err == nil {
			// Real memory calculation from balloon stats:
			//
			// IMPORTANT: The guest kernel's total_memory INCLUDES memory claimed by balloon.
			// The balloon device takes memory from the guest, but the guest still sees
			// it as part of total memory (just not usable).
			//
			// Firecracker balloon stats:
			// - total_memory: Total RAM seen by guest kernel (includes balloon-claimed)
			// - actual_mib: Memory claimed by balloon device (taken from guest)
			// - available_memory: Free + reclaimable memory (what guest can actually use)
			//
			// Real usable memory = total_memory - balloon_actual
			// Real in_use by processes = usable_memory - available_memory

			totalMB := stats.TotalMemory / (1024 * 1024)
			freeMB := stats.FreeMemory / (1024 * 1024)
			availableMB := stats.Available / (1024 * 1024)
			balloonMB := uint64(stats.ActualMib) // Memory claimed by balloon

			// Calculate memory actually usable by the guest (excluding balloon)
			var usableMB uint64
			if totalMB > balloonMB {
				usableMB = totalMB - balloonMB
			} else {
				usableMB = planMemoryMB // fallback to plan memory
			}

			// Real memory in use by processes = usable - available
			// This excludes the balloon-claimed memory
			var inUseMB uint64
			if usableMB > availableMB {
				inUseMB = usableMB - availableMB
			} else {
				inUseMB = 0
			}

			// usage percent based on plan memory (what customer paid for)
			usagePercent := float64(inUseMB) / float64(planMemoryMB) * 100
			if usagePercent > 100 {
				usagePercent = 100
			}

			response["memory"] = map[string]interface{}{
				"in_use_mb":     inUseMB,
				"free_mb":       freeMB,
				"available_mb":  availableMB,
				"usable_mb":     usableMB, // actual usable memory (total - balloon)
				"plan_mb":       planMemoryMB,
				"usage_percent": fmt.Sprintf("%.1f", usagePercent),
			}
		} else {
			// Balloon stats unavailable, return plan memory as estimate
			response["memory"] = map[string]interface{}{
				"in_use_mb":     0,
				"available_mb":  planMemoryMB,
				"plan_mb":       planMemoryMB,
				"usage_percent": "0.0",
				"note":          "balloon stats unavailable",
			}
		}
	} else {
		// VM not running
		response["memory"] = map[string]interface{}{
			"in_use_mb":     0,
			"available_mb":  0,
			"plan_mb":       planMemoryMB,
			"usage_percent": "0.0",
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getAutoscaleHandler retrieves autoscaling configuration and current stats for a VM
func (s *HTTPServer) getAutoscaleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	response := map[string]interface{}{
		"vm_id":     vmID,
		"autoscale": vm.Autoscale,
	}

	// If VM is running, get current balloon stats
	if vm.Status == models.MicroVMStatusRunning && vm.Autoscale != nil {
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vmID)
		if stats, err := s.getBalloonStats(socketPath); err == nil {
			usedMemory := stats.TotalMemory - stats.FreeMemory
			usagePercent := float64(usedMemory) / float64(stats.TotalMemory)
			availableMemoryMB := (stats.TotalMemory / (1024 * 1024)) - uint64(stats.ActualMib)

			response["current_stats"] = map[string]interface{}{
				"usage_percent":       fmt.Sprintf("%.1f%%", usagePercent*100),
				"used_memory_mb":      usedMemory / (1024 * 1024),
				"free_memory_mb":      stats.FreeMemory / (1024 * 1024),
				"total_memory_mb":     stats.TotalMemory / (1024 * 1024),
				"balloon_mb":          stats.ActualMib,
				"available_memory_mb": availableMemoryMB,
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// setMemoryGuaranteeHandler sets a minimum memory guarantee for smart balloon
// This prevents the balloon from taking too much memory from the VM
func (s *HTTPServer) setMemoryGuaranteeHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	var req struct {
		GuaranteedMemoryMB uint64 `json:"guaranteed_memory_mb"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate: guarantee must be <= total VM memory
	if req.GuaranteedMemoryMB > uint64(vm.Resources.MemoryMB) {
		http.Error(w, fmt.Sprintf("guaranteed_memory_mb (%d) cannot exceed VM total memory (%d MB)",
			req.GuaranteedMemoryMB, vm.Resources.MemoryMB), http.StatusBadRequest)
		return
	}

	// Set the memory guarantee in balloon manager
	if s.balloonManager != nil {
		s.balloonManager.SetVMGuaranteedMemory(vmID, req.GuaranteedMemoryMB)
	}

	// If VM is running and has balloon, adjust it immediately
	if vm.Status == models.MicroVMStatusRunning && req.GuaranteedMemoryMB > 0 {
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vmID)
		if stats, err := s.getBalloonStats(socketPath); err == nil {
			maxBalloonMB := uint64(vm.Resources.MemoryMB) - req.GuaranteedMemoryMB
			if stats.ActualMib > maxBalloonMB {
				// Current balloon exceeds new limit - deflate immediately
				log.Printf("🎈 VM %s: deflating balloon %d -> %d MB (new guarantee: %d MB)",
					vmID, stats.ActualMib, maxBalloonMB, req.GuaranteedMemoryMB)
				if err := s.setBalloonSize(socketPath, maxBalloonMB); err != nil {
					log.Printf("⚠️  VM %s: failed to adjust balloon: %v", vmID, err)
				}
			}
		}
	}

	response := map[string]interface{}{
		"message":              "Memory guarantee set",
		"vm_id":                vmID,
		"guaranteed_memory_mb": req.GuaranteedMemoryMB,
		"max_balloon_mb":       uint64(vm.Resources.MemoryMB) - req.GuaranteedMemoryMB,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getMemoryProfileHandler returns the VM memory profile for smart balloon
func (s *HTTPServer) getMemoryProfileHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	response := map[string]interface{}{
		"vm_id":           vmID,
		"total_memory_mb": vm.Resources.MemoryMB,
	}

	// Get profile from balloon manager
	if s.balloonManager != nil {
		if profile := s.balloonManager.GetVMProfile(vmID); profile != nil {
			response["profile"] = map[string]interface{}{
				"peak_usage_mb":        profile.PeakUsageMB,
				"peak_usage_time":      profile.PeakUsageTime,
				"avg_usage_mb":         profile.AvgUsageMB,
				"oom_count":            profile.OOMCount,
				"last_oom_time":        profile.LastOOMTime,
				"sample_count":         profile.SampleCount,
				"guaranteed_memory_mb": profile.GuaranteedMemoryMB,
			}

			// Calculate effective max balloon
			if profile.GuaranteedMemoryMB > 0 {
				response["max_balloon_mb"] = uint64(vm.Resources.MemoryMB) - profile.GuaranteedMemoryMB
				response["balloon_policy"] = "explicit_guarantee"
			} else if profile.OOMCount > 0 {
				guaranteeMB := uint64(float64(profile.PeakUsageMB) * 1.25)
				if guaranteeMB > uint64(vm.Resources.MemoryMB) {
					response["max_balloon_mb"] = 0
				} else {
					response["max_balloon_mb"] = uint64(vm.Resources.MemoryMB) - guaranteeMB
				}
				response["balloon_policy"] = "oom_recovery"
			} else if profile.SampleCount >= 6 && profile.PeakUsageMB > 0 {
				guaranteeMB := uint64(float64(profile.PeakUsageMB) * 1.15)
				if guaranteeMB > uint64(vm.Resources.MemoryMB) {
					response["max_balloon_mb"] = 0
				} else {
					response["max_balloon_mb"] = uint64(vm.Resources.MemoryMB) - guaranteeMB
				}
				response["balloon_policy"] = "usage_based"
			} else {
				response["max_balloon_mb"] = uint64(float64(vm.Resources.MemoryMB) * 0.25)
				response["balloon_policy"] = "conservative_default"
			}
		} else {
			// No profile yet - show default
			response["max_balloon_mb"] = uint64(float64(vm.Resources.MemoryMB) * 0.25)
			response["balloon_policy"] = "conservative_default"
			response["profile"] = nil
		}
	}

	// Get current balloon stats if VM is running
	if vm.Status == models.MicroVMStatusRunning {
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vmID)
		if stats, err := s.getBalloonStats(socketPath); err == nil {
			response["current_stats"] = map[string]interface{}{
				"balloon_mb":          stats.ActualMib,
				"free_memory_mb":      stats.FreeMemory / (1024 * 1024),
				"used_memory_mb":      (stats.TotalMemory - stats.FreeMemory) / (1024 * 1024),
				"available_memory_mb": (stats.TotalMemory / (1024 * 1024)) - uint64(stats.ActualMib),
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// setBalloonSize is a helper method to set balloon size via Firecracker API
func (s *HTTPServer) setBalloonSize(socketPath string, sizeMB uint64) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true, // Close connection after request to prevent connection leaks
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
	}
	defer transport.CloseIdleConnections()

	payload := map[string]interface{}{"amount_mib": sizeMB}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest("PATCH", "http://localhost/balloon", bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// getBalloonStats is a helper method to get balloon statistics
func (s *HTTPServer) getBalloonStats(socketPath string) (*autoscale.BalloonStats, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true, // Close connection after request to prevent connection leaks
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
	}
	defer transport.CloseIdleConnections()

	resp, err := client.Get("http://localhost/balloon/statistics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var stats autoscale.BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

// Performance optimization methods

// getCachedVMInfo retrieves VM information from cache if available and not expired
func (s *HTTPServer) getCachedVMInfo(vmID string) (*VMStatusResponse, bool) {
	s.vmCacheMutex.RLock()
	defer s.vmCacheMutex.RUnlock()

	if cachedInfo, exists := s.vmInfoCache[vmID]; exists {
		// Check if cache is still valid (within expiry time)
		if time.Since(s.lastCacheUpdate) < s.cacheExpiry {
			return cachedInfo, true
		}
	}

	return nil, false
}

// setCachedVMInfo stores VM information in cache
func (s *HTTPServer) setCachedVMInfo(vmID string, info *VMStatusResponse) {
	s.vmCacheMutex.Lock()
	defer s.vmCacheMutex.Unlock()

	s.vmInfoCache[vmID] = info
}

// invalidateVMCache removes a specific VM from cache
func (s *HTTPServer) invalidateVMCache(vmID string) {
	s.vmCacheMutex.Lock()
	defer s.vmCacheMutex.Unlock()

	delete(s.vmInfoCache, vmID)
}

// invalidateAllVMCache clears the entire VM cache
func (s *HTTPServer) invalidateAllVMCache() {
	s.vmCacheMutex.Lock()
	defer s.vmCacheMutex.Unlock()

	s.vmInfoCache = make(map[string]*VMStatusResponse)
}

// buildVMStatusResponse builds a VMStatusResponse from a MicroVMInstance
func (s *HTTPServer) buildVMStatusResponse(vm *models.MicroVMInstance) *VMStatusResponse {
	// Sync real VM state with stored state
	s.syncVMRealState(vm)

	// Update the original VM in s.vms to prevent re-syncing the same VM
	s.vmsMutex.Lock()
	if originalVM, exists := s.vms[vm.ID]; exists {
		originalVM.Status = vm.Status
		originalVM.ProcessInfo = vm.ProcessInfo
	}
	s.vmsMutex.Unlock()

	// Discover the actual IP address the VM is using
	actualIP := s.discoverVMIP(vm.ID)
	if actualIP == "" {
		actualIP = vm.Network.IPAddress // fallback to stored IP
	}

	// Get refreshed TAP device information
	tapDevice := s.refreshTAPDeviceInfo(vm.ID)
	if tapDevice == "" {
		tapDevice = "-" // Show dash when TAP device is not available
	}

	// Calculate uptime
	uptime := time.Since(vm.Metadata.CreatedAt)

	// Extract image name and VM name from labels if available
	imageName := ""
	name := vm.ID
	if vm.Metadata.Labels != nil {
		if n, exists := vm.Metadata.Labels["name"]; exists && n != "" {
			name = n
		}
		if iname, exists := vm.Metadata.Labels["image_name"]; exists {
			imageName = iname
		} else if imageID, exists := vm.Metadata.Labels["image_id"]; exists {
			imageName = imageID
		}
	}

	// Cloud-init troubleshooting fields
	cloudInitPath := ""
	cloudInitAttached := false
	if vm.Metadata.Labels != nil {
		if p, ok := vm.Metadata.Labels["cloudinit_path"]; ok && p != "" {
			cloudInitPath = p
			cloudInitAttached = true
		}
	}

	// Remove unused gateway and dnsServers variables
	// (models.NetworkInfo doesn't have these fields)

	return &VMStatusResponse{
		ID:        vm.ID,
		Name:      name,
		Status:    string(vm.Status),
		ImageName: imageName, // Include image name for CLI display
		Resources: ResourceSpecs{
			VCPUs:     vm.Resources.VCPUs,
			MemoryMB:  vm.Resources.MemoryMB,
			StorageMB: vm.Resources.StorageMB,
		},
		Network: models.NetworkInfo{
			IPAddress:        actualIP, // Use discovered IP
			MACAddress:       vm.Network.MACAddress,
			TAPDevice:        tapDevice, // Use refreshed TAP device info
			AssignmentMethod: vm.Network.AssignmentMethod,
			SSHPort:          vm.Network.SSHPort,
			PublicKey:        vm.Network.PublicKey,
			BridgeName:       vm.Network.BridgeName,
			BandwidthIngress: vm.Network.BandwidthIngress, // Include bandwidth limits
			BandwidthEgress:  vm.Network.BandwidthEgress,  // Include bandwidth limits
			IPv6Address:      vm.Network.IPv6Address,      // Include IPv6 configuration
			IPv6Gateway:      vm.Network.IPv6Gateway,      // Include IPv6 gateway
			IPv6PrefixLength: vm.Network.IPv6PrefixLength, // Include IPv6 prefix length
			IPv4NAT:          vm.Network.IPv4NAT,          // Include IPv4 NAT flag
			IPv4NATAddress:   vm.Network.IPv4NATAddress,   // Include private IPv4 NAT address
		},
		Storage: StorageInfo{
			RootDevice: "/dev/vda",
			Size:       vm.Resources.StorageMB,
			Type:       "local",
		},
		CreatedAt:         vm.Metadata.CreatedAt,
		UpdatedAt:         vm.Metadata.CreatedAt,
		Uptime:            formatUptime(uptime),
		AutoStart:         vm.ShouldAutoStart(),
		ProcessInfo:       vm.ProcessInfo,
		Autoscale:         vm.Autoscale, // Include autoscale configuration
		CloudInitAttached: cloudInitAttached,
		CloudInitPath:     cloudInitPath,
		Metadata: func() map[string]string {
			meta := map[string]string{
				"customer_id": vm.Metadata.CustomerID,
			}
			for k, v := range vm.Metadata.Labels {
				meta[k] = v
			}
			return meta
		}(),
	}
}

// startVMStatusUpdater starts a background goroutine for periodic VM status updates
func (s *HTTPServer) startVMStatusUpdater() {
	ticker := time.NewTicker(15 * time.Second) // Update every 15 seconds
	defer ticker.Stop()

	fmt.Printf("🔄 Starting VM status updater\n")
	for range ticker.C {
		s.updateVMStatusCache()
	}
}

// startVMLogSweeper periodically caps per-VM serial log files (console.log,
// firecracker.log) so a long-running guest cannot fill the node's disk. Files are
// truncated in place, so running guests are unaffected and the web console keeps
// streaming (issue #28).
func (s *HTTPServer) startVMLogSweeper() {
	if s.manager == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	fmt.Printf("🧹 Starting VM log sweeper (5m interval)\n")
	for range ticker.C {
		s.manager.SweepVMLogs()
	}
}

// startRebootWatcher monitors for guest-initiated reboots (process exit) and restarts VMs
// automatically when they were previously marked running and AutoStart is enabled.
// This converts an in-guest 'reboot' into an actual restart instead of a shutdown.
func (s *HTTPServer) startRebootWatcher() {
	ticker := time.NewTicker(8 * time.Second)
	go func() {
		for range ticker.C {
			for vmID, vm := range s.snapshotVMMap() {
				// Only act on VMs that we believe are running
				if vm.Status != models.MicroVMStatusRunning {
					continue
				}
				// Skip if still active
				if s.manager.IsVMProcessRunning(vmID) {
					continue
				}
				// Skip if user disabled auto-start
				if !vm.ShouldAutoStart() {
					continue
				}
				// Never resurrect a VM that is being reinstalled or deleted
				// right now: restarting the Firecracker process mid-delete
				// re-opens the zvol and makes the reinstall's `zfs destroy`
				// fail with "dataset is busy" — the caller then proceeds and
				// the disk silently survives the reinstall.
				if s.vmOpInFlight(vmID) || s.vmCreationPending(vmID) {
					continue
				}
				log.Printf("🔁 Detected exited process for running VM %s; treating as guest reboot and restarting", vmID)
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				vm.Status = models.MicroVMStatusCreating
				// Mark intent to reuse existing rootfs during this reboot if possible
				if vm.Metadata.Labels == nil {
					vm.Metadata.Labels = map[string]string{}
				}
				vm.Metadata.Labels["reuse_existing_rootfs"] = "true"

				if err := s.manager.CreateMicroVM(ctx, vm); err != nil {
					log.Printf("❌ Failed to restart VM %s after guest reboot: %v", vmID, err)
					vm.Status = models.MicroVMStatusStopped
					if vm.Metadata.Labels == nil {
						vm.Metadata.Labels = map[string]string{}
					}
					vm.Metadata.Labels["last_restart_error"] = err.Error()
					vm.UpdateLastActivity()
				} else {
					vm.Status = models.MicroVMStatusRunning
					vm.UpdateLastActivity()
					log.Printf("✅ VM %s successfully restarted after guest reboot", vmID)
					if vm.Network.AssignmentMethod == models.NetworkModeDHCP {
						go s.startFastDHCPDiscovery(vm.ID, 60*time.Second, 5*time.Second)
					}
					// Cleanup persistence label to avoid unintended reuse on manual restarts
					delete(vm.Metadata.Labels, "reuse_existing_rootfs")
				}
				cancel()
				s.persistVMState()
				s.invalidateVMCache(vmID)
			}
		}
	}()
}

// updateVMStatusCache updates the VM status cache in the background
func (s *HTTPServer) updateVMStatusCache() {
	// Create a new cache map to avoid holding locks for too long
	newCache := make(map[string]*VMStatusResponse)

	// Process VMs in parallel for better performance
	var wg sync.WaitGroup
	cacheMutex := sync.Mutex{}

	for vmID, vm := range s.snapshotVMMap() {
		wg.Add(1)
		go func(id string, vmInstance *models.MicroVMInstance) {
			defer wg.Done()

			// Create a copy of the VM to avoid race conditions
			vmCopy := *vmInstance

			// Update VM status from manager
			if status, err := s.manager.GetMicroVMStatus(context.Background(), id); err == nil {
				vmCopy.Status = status
			}

			// Build status response
			statusResponse := s.buildVMStatusResponse(&vmCopy)

			// Add to new cache (thread-safe)
			cacheMutex.Lock()
			newCache[id] = statusResponse
			cacheMutex.Unlock()
		}(vmID, vm)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Replace the cache atomically
	s.vmCacheMutex.Lock()
	s.vmInfoCache = newCache
	s.lastCacheUpdate = time.Now()
	s.vmCacheMutex.Unlock()

	fmt.Printf("🔄 Updated VM status cache for %d VMs\n", len(newCache))

	// Refresh Prometheus gauges from the fresh cache snapshot.
	s.refreshVMMetrics(newCache)
}

// refreshVMMetrics updates process-wide gauges from a cache snapshot.
func (s *HTTPServer) refreshVMMetrics(cache map[string]*VMStatusResponse) {
	byStatus := make(map[string]float64)
	for _, resp := range cache {
		byStatus[resp.Status]++
	}
	for _, st := range []string{"creating", "running", "stopping", "stopped", "error"} {
		metrics.SetGauge(metrics.VMsTotal, byStatus[st], map[string]string{"status": st})
	}
	if freeMB, err := hostFreeMemoryMB(); err == nil {
		metrics.SetGauge(metrics.HostMemoryFreeMB, float64(freeMB), nil)
	}
}

// hostFreeMemoryMB reads MemAvailable from /proc/meminfo.
func hostFreeMemoryMB() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		var val uint64
		if _, err := fmt.Sscanf(line, "MemAvailable: %d kB", &val); err == nil {
			return val / 1024, nil
		}
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

// updateVMResourcesHandler handles VM resource update requests
func (s *HTTPServer) updateVMResourcesHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	fmt.Printf("⚙️  HANDLER CALLED: updateVMResourcesHandler for VM %s\n", vmID)

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	fmt.Printf("⚙️  VM found, current status: %s\n", vm.Status)

	var req struct {
		models.ResourceSpecs
		BandwidthIngressMbps *uint32  `json:"bandwidth_ingress_mbps,omitempty"`
		BandwidthEgressMbps  *uint32  `json:"bandwidth_egress_mbps,omitempty"`
		PricePerHour         *float64 `json:"price_per_hour,omitempty"`
		Currency             string   `json:"currency,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Check if any resource fields were actually provided (not just price/currency update)
	resourcesProvided := req.VCPUs > 0 || req.MemoryMB > 0 || req.StorageMB > 0

	// Validate resource specifications only if resources were provided
	if resourcesProvided {
		if err := s.validateResourceSpecs(&req.ResourceSpecs); err != nil {
			http.Error(w, fmt.Sprintf("Invalid resource specifications: %v", err), http.StatusBadRequest)
			return
		}

		// Check resource availability
		if err := s.checkResourceAvailability(vmID, &req.ResourceSpecs); err != nil {
			http.Error(w, fmt.Sprintf("Insufficient resources: %v", err), http.StatusConflict)
			return
		}
	}

	// Store old resources for rollback
	oldResources := vm.Resources
	oldBandwidthIngress := vm.Network.BandwidthIngress
	oldBandwidthEgress := vm.Network.BandwidthEgress

	// Update VM resources only if resources were provided
	if resourcesProvided {
		vm.Resources = req.ResourceSpecs
	}

	// Update bandwidth if provided
	bandwidthUpdated := false
	if req.BandwidthIngressMbps != nil {
		vm.Network.BandwidthIngress = fmt.Sprintf("%dmbit", *req.BandwidthIngressMbps)
		bandwidthUpdated = true
	}
	if req.BandwidthEgressMbps != nil {
		vm.Network.BandwidthEgress = fmt.Sprintf("%dmbit", *req.BandwidthEgressMbps)
		bandwidthUpdated = true
	}

	fmt.Printf("🔧 Attempting to update VM %s resources: vCPUs=%d->%d, Mem=%d->%dMB, Storage=%d->%dMB\n",
		vmID, oldResources.VCPUs, req.VCPUs, oldResources.MemoryMB, req.MemoryMB, oldResources.StorageMB, req.StorageMB)

	if bandwidthUpdated {
		fmt.Printf("🌐 Updating bandwidth: ingress=%s, egress=%s\n", vm.Network.BandwidthIngress, vm.Network.BandwidthEgress)
	}

	// Try to apply resource changes to running VM (only if resources were changed)
	var requiresRestart bool
	var restartedVM bool
	if resourcesProvided && vm.Status == models.MicroVMStatusRunning {
		fmt.Printf("🏃 VM %s is running, attempting live update...\n", vmID)
		if err := s.manager.UpdateVMResources(context.Background(), vm); err != nil {
			errMsg := err.Error()
			fmt.Printf("📊 UpdateVMResources returned error: %v\n", errMsg)
			// Check if this is a restart-required change
			if strings.Contains(errMsg, "restart") {
				requiresRestart = true
				fmt.Printf("🔄 VM %s requires restart for resource changes, performing automatic restart...\n", vmID)

				// Perform automatic restart
				if err := s.restartVMForResourceUpdate(vmID, vm); err != nil {
					// Rollback on restart failure
					vm.Resources = oldResources
					http.Error(w, fmt.Sprintf("Failed to restart VM for resource update: %v", err), http.StatusInternalServerError)
					return
				}
				restartedVM = true
				fmt.Printf("✅ VM %s restarted successfully with new resources\n", vmID)
			} else {
				// Other errors - rollback
				vm.Resources = oldResources
				vm.Network.BandwidthIngress = oldBandwidthIngress
				vm.Network.BandwidthEgress = oldBandwidthEgress
				http.Error(w, fmt.Sprintf("Failed to update VM resources: %v", err), http.StatusInternalServerError)
				return
			}
		}
	} else if resourcesProvided {
		// VM is stopped - just update configuration
		if err := s.manager.UpdateVMResources(context.Background(), vm); err != nil {
			vm.Resources = oldResources
			vm.Network.BandwidthIngress = oldBandwidthIngress
			vm.Network.BandwidthEgress = oldBandwidthEgress
			http.Error(w, fmt.Sprintf("Failed to update VM configuration: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Apply bandwidth limits if VM is running and bandwidth was updated
	if bandwidthUpdated && vm.Status == models.MicroVMStatusRunning {
		tapDevice := vm.Network.TAPDevice
		if tapDevice == "" {
			tapDevice = fmt.Sprintf("tap-%s", vmID)
		}

		// Apply VM download limit to TAP root/egress - lower limit to save DC bandwidth costs
		// bandwidth_ingress = VM download (wget) -> applied to TAP root
		if req.BandwidthIngressMbps != nil {
			exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "root").Run()
			cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "root", "tbf",
				"rate", vm.Network.BandwidthIngress, "burst", "128kb", "latency", "50ms")
			if err := cmd.Run(); err != nil {
				fmt.Printf("⚠️  Failed to apply VM download bandwidth: %v\n", err)
			} else {
				fmt.Printf("✅ VM download bandwidth limit applied: %s\n", vm.Network.BandwidthIngress)
			}
		}

		// Apply VM upload limit to TAP ingress - higher limit acceptable
		// bandwidth_egress = VM upload -> applied to TAP ingress
		if req.BandwidthEgressMbps != nil {
			exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "ingress").Run()
			cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "handle", "ffff:", "ingress")
			if err := cmd.Run(); err == nil {
				cmd = exec.Command("sudo", "tc", "filter", "add", "dev", tapDevice, "parent", "ffff:",
					"protocol", "all", "u32", "match", "u32", "0", "0",
					"police", "rate", vm.Network.BandwidthEgress, "burst", "128kb", "mtu", "64kb", "drop")
				if err := cmd.Run(); err != nil {
					fmt.Printf("⚠️  Failed to apply VM upload bandwidth filter: %v\n", err)
				} else {
					fmt.Printf("✅ VM upload bandwidth limit applied: %s\n", vm.Network.BandwidthEgress)
				}
			} else {
				fmt.Printf("⚠️  Failed to apply VM upload bandwidth qdisc: %v\n", err)
			}
		}
	}

	// Invalidate caches after resource update
	s.invalidateVMCache(vmID)

	// Persist the change
	s.persistVMState()

	response := map[string]interface{}{
		"message":          "VM resources updated successfully",
		"vm_id":            vmID,
		"resources":        vm.Resources,
		"requires_restart": requiresRestart,
		"restarted":        restartedVM,
	}

	if bandwidthUpdated {
		response["bandwidth"] = map[string]string{
			"ingress": vm.Network.BandwidthIngress,
			"egress":  vm.Network.BandwidthEgress,
		}
	}

	if restartedVM {
		response["message"] = "VM resources updated and VM restarted automatically"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getResourceAvailabilityHandler returns cluster resource availability information
func (s *HTTPServer) getResourceAvailabilityHandler(w http.ResponseWriter, r *http.Request) {
	availability := s.calculateResourceAvailability()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(availability)
}

// validateResourceSpecs validates resource specifications
func (s *HTTPServer) validateResourceSpecs(resources *models.ResourceSpecs) error {
	if resources.VCPUs == 0 {
		return fmt.Errorf("vCPUs must be greater than 0")
	}
	if resources.VCPUs > 16 {
		return fmt.Errorf("vCPUs cannot exceed 16")
	}
	if resources.MemoryMB < 128 {
		return fmt.Errorf("memory must be at least 128MB")
	}
	if resources.MemoryMB > 65536 {
		return fmt.Errorf("memory cannot exceed 64GB (65536MB)")
	}
	if resources.StorageMB < 1024 {
		return fmt.Errorf("storage must be at least 1GB (1024MB)")
	}
	if max := maxStorageMBPerVM(); resources.StorageMB > max {
		return fmt.Errorf("storage cannot exceed %dMB (%d allowed, set MICROVM_MAX_STORAGE_MB to raise)", max, max)
	}
	return nil
}

// maxStorageMBPerVM returns the per-VM storage ceiling. Default 100GB, matching
// the historical hardcoded limit; MICROVM_MAX_STORAGE_MB overrides it so the
// node can serve plans larger than 100GB (e.g. flash-storage-enterprise 250GB).
func maxStorageMBPerVM() uint32 {
	if v := strings.TrimSpace(os.Getenv("MICROVM_MAX_STORAGE_MB")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n >= 1024 {
			return uint32(n)
		}
	}
	return 102400
}

// restartVMForResourceUpdate performs a controlled restart of a VM to apply resource changes
func (s *HTTPServer) restartVMForResourceUpdate(vmID string, vm *models.MicroVMInstance) error {
	ctx := context.Background()

	fmt.Printf("🛑 Stopping VM %s for resource update...\n", vmID)

	// Stop the VM (force=true to ensure clean stop)
	if err := s.manager.StopMicroVM(ctx, vm, true); err != nil {
		return fmt.Errorf("failed to stop VM: %w", err)
	}

	// Wait a moment for cleanup
	time.Sleep(2 * time.Second)

	// Update VM configuration with new resources
	if err := s.manager.UpdateVMResources(ctx, vm); err != nil {
		return fmt.Errorf("failed to update VM configuration: %w", err)
	}

	fmt.Printf("🚀 Starting VM %s with new resources...\n", vmID)

	// Start the VM with new configuration
	if err := s.manager.StartMicroVM(ctx, vm); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}

	// Update VM status
	vm.Status = models.MicroVMStatusRunning

	fmt.Printf("✅ VM %s restarted: vCPUs=%d, Memory=%dMB, Storage=%dMB\n",
		vmID, vm.Resources.VCPUs, vm.Resources.MemoryMB, vm.Resources.StorageMB)

	return nil
} // checkResourceAvailability checks if requested resources are available
func (s *HTTPServer) checkResourceAvailability(vmID string, resources *models.ResourceSpecs) error {
	availability := s.calculateResourceAvailability()

	// Get current VM resources to calculate the difference
	currentVM, exists := s.getVM(vmID)
	if !exists {
		return fmt.Errorf("VM not found")
	}

	// Calculate resource difference (what we need to add/remove)
	vcpuDiff := int32(resources.VCPUs) - int32(currentVM.Resources.VCPUs)
	memoryDiff := int32(resources.MemoryMB) - int32(currentVM.Resources.MemoryMB)
	storageDiff := int32(resources.StorageMB) - int32(currentVM.Resources.StorageMB)

	// Check if we have enough available resources for increases
	if vcpuDiff > 0 && uint32(vcpuDiff) > availability.AvailableVCPUs {
		return fmt.Errorf("insufficient vCPUs: requested %d additional, available %d", vcpuDiff, availability.AvailableVCPUs)
	}
	if memoryDiff > 0 && uint32(memoryDiff) > availability.AvailableMemoryMB {
		return fmt.Errorf("insufficient memory: requested %d MB additional, available %d MB", memoryDiff, availability.AvailableMemoryMB)
	}
	if storageDiff > 0 && uint32(storageDiff) > availability.AvailableStorageMB {
		return fmt.Errorf("insufficient storage: requested %d MB additional, available %d MB", storageDiff, availability.AvailableStorageMB)
	}

	return nil
}

// calculateResourceAvailability calculates current resource availability
func (s *HTTPServer) calculateResourceAvailability() *ResourceAvailability {
	// Define system limits (these would typically come from configuration)
	const (
		maxVCPUs     = 64
		maxMemoryMB  = 32768   // 32GB
		maxStorageMB = 1048576 // 1TB
	)

	// Calculate used resources
	var usedVCPUs, usedMemoryMB, usedStorageMB uint32
	for _, vm := range s.snapshotVMMap() {
		usedVCPUs += vm.Resources.VCPUs
		usedMemoryMB += vm.Resources.MemoryMB
		usedStorageMB += vm.Resources.StorageMB
	}

	// Calculate available resources
	availableVCPUs := maxVCPUs - usedVCPUs
	availableMemoryMB := maxMemoryMB - usedMemoryMB
	availableStorageMB := maxStorageMB - usedStorageMB

	return &ResourceAvailability{
		TotalVCPUs:         maxVCPUs,
		AvailableVCPUs:     availableVCPUs,
		TotalMemoryMB:      maxMemoryMB,
		AvailableMemoryMB:  availableMemoryMB,
		TotalStorageMB:     maxStorageMB,
		AvailableStorageMB: availableStorageMB,
	}
}

// ResourceAvailability represents cluster resource availability
type ResourceAvailability struct {
	TotalVCPUs         uint32 `json:"total_vcpus"`
	AvailableVCPUs     uint32 `json:"available_vcpus"`
	TotalMemoryMB      uint32 `json:"total_memory_mb"`
	AvailableMemoryMB  uint32 `json:"available_memory_mb"`
	TotalStorageMB     uint32 `json:"total_storage_mb"`
	AvailableStorageMB uint32 `json:"available_storage_mb"`
}

// cleanupFailedVMInternal performs cleanup operations when VM creation fails
func (s *HTTPServer) cleanupFailedVMInternal(ctx context.Context, vmID string) error {
	fmt.Printf("🧹 Cleaning up failed VM %s...\n", vmID)

	var cleanupErrors []error

	// Clean up TAP interface (multiple naming patterns)
	tapName := s.generateFallbackTAPName(vmID)
	_ = exec.Command("sudo", "ip", "link", "delete", tapName).Run()

	// Also try the MACVLAN-style tap name
	tapName2 := fmt.Sprintf("tap-%s", vmID)
	if len(tapName2) > 15 {
		tapName2 = tapName2[:15]
	}
	_ = exec.Command("sudo", "ip", "link", "delete", tapName2).Run()

	// Clean up MACVLAN interface and per-VM bridge (for MACVLAN mode)
	vmHash := fmt.Sprintf("%x", hashVMID(vmID))[:8]
	macvlanName := fmt.Sprintf("mv-%s", vmHash)
	bridgeName := fmt.Sprintf("br-%s", vmHash)

	// Remove MACVLAN interface
	if err := exec.Command("sudo", "ip", "link", "delete", macvlanName).Run(); err == nil {
		fmt.Printf("   ✅ Removed MACVLAN interface %s\n", macvlanName)
	}

	// Remove per-VM bridge
	if err := exec.Command("sudo", "ip", "link", "delete", bridgeName).Run(); err == nil {
		fmt.Printf("   ✅ Removed bridge %s\n", bridgeName)
	}

	// Clean up VM directory
	workDir := resolveWorkDir()
	vmDir := filepath.Join(workDir, "microvms", vmID)
	if err := os.RemoveAll(vmDir); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove VM directory: %w", err))
	}

	// Clean up rootfs file
	rootfsPath := filepath.Join(workDir, fmt.Sprintf("rootfs-%s.img", vmID))
	if err := os.Remove(rootfsPath); err != nil && !os.IsNotExist(err) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove rootfs: %w", err))
	}

	// Clean up any firecracker processes
	if err := exec.Command("pkill", "-f", fmt.Sprintf("firecracker.*%s", vmID)).Run(); err != nil {
		// This is expected if no process is running, so don't add to errors
	}

	// Log cleanup errors but don't fail the operation
	for _, err := range cleanupErrors {
		fmt.Printf("⚠️  Cleanup warning for VM %s: %v\n", vmID, err)
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("cleanup completed with %d warnings", len(cleanupErrors))
	}

	fmt.Printf("✅ Cleanup completed for VM %s\n", vmID)
	return nil
}

// hashVMID returns a consistent hash for a VM ID
func hashVMID(vmID string) uint32 {
	var h uint32
	for _, c := range vmID {
		h = h*31 + uint32(c)
	}
	return h
}

// Image management handlers

// listImagesHandler returns all available OS images.
//
// Optional query parameters:
//   - available=true|1  only include images whose file exists on this node
//   - include_eol=false only include images that are not end-of-life
func (s *HTTPServer) listImagesHandler(w http.ResponseWriter, r *http.Request) {
	allImages := s.imageManager.GetAvailableImages()

	query := r.URL.Query()
	includeEOL := true
	if v := query.Get("include_eol"); v == "false" || v == "0" {
		includeEOL = false
	}
	availableOnly := query.Get("available") == "true" || query.Get("available") == "1"

	result := make([]*images.ImageSpec, 0, len(allImages))
	for _, image := range allImages {
		if !includeEOL && image.EOL {
			continue
		}
		if availableOnly && !image.Available {
			continue
		}
		result = append(result, image)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"images":         result,
		"total":          len(result),
		"include_eol":    includeEOL,
		"available_only": availableOnly,
	})
}

// getImageHandler returns details for a specific image
func (s *HTTPServer) getImageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	imageID := vars["id"]

	image, err := s.imageManager.GetImage(imageID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Image not found: %s", imageID), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(image)
}

// downloadImageHandler downloads an OS image
func (s *HTTPServer) downloadImageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	imageID := vars["id"]

	// Check if image exists
	image, err := s.imageManager.GetImage(imageID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Image not found: %s", imageID), http.StatusNotFound)
		return
	}

	if image.Available {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Image already available",
			"image":   image,
		})
		return
	}

	if !s.imageManager.CanDownload(imageID) {
		http.Error(w, fmt.Sprintf("Image %s is not present on this node and has no download source; build it on the node with the image build pipeline", imageID), http.StatusNotImplemented)
		return
	}

	// Start download in background
	go func() {
		fmt.Printf("📥 Starting download of %s...\n", image.Name)
		if err := s.imageManager.DownloadImage(imageID); err != nil {
			fmt.Printf("❌ Failed to download %s: %v\n", image.Name, err)
		} else {
			fmt.Printf("✅ Successfully downloaded %s\n", image.Name)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Download started",
		"status":  "downloading",
		"image":   image,
	})
}

// GetAllVMs implements the VMGetter interface for balloon manager
func (s *HTTPServer) GetAllVMs() []*models.MicroVMInstance {
	s.vmCacheMutex.RLock()
	defer s.vmCacheMutex.RUnlock()

	vms := make([]*models.MicroVMInstance, 0, s.vmCount())
	for _, vmInstance := range s.snapshotVMMap() {
		vms = append(vms, vmInstance)
	}
	return vms
}

// GetVM implements the VMGetter interface for per-VM autoscaler
func (s *HTTPServer) GetVM(id string) (*models.MicroVMInstance, error) {
	s.vmCacheMutex.RLock()
	defer s.vmCacheMutex.RUnlock()

	vm, exists := s.getVM(id)
	if !exists {
		return nil, fmt.Errorf("VM not found: %s", id)
	}
	return vm, nil
}

// BandwidthUpdateRequest represents a request to update bandwidth limits
type BandwidthUpdateRequest struct {
	BandwidthIngress string `json:"bandwidth_ingress,omitempty"` // e.g., "100mbit", "1gbit"
	BandwidthEgress  string `json:"bandwidth_egress,omitempty"`  // e.g., "50mbit", "500kbit"
}

// updateBandwidthHandler handles PUT /vms/{id}/bandwidth - update bandwidth limits live
func (s *HTTPServer) updateBandwidthHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	var req BandwidthUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate bandwidth format (e.g., "100mbit", "1gbit", "500kbit")
	if req.BandwidthIngress != "" {
		if err := validateBandwidthFormat(req.BandwidthIngress); err != nil {
			http.Error(w, fmt.Sprintf("Invalid bandwidth_ingress format: %v", err), http.StatusBadRequest)
			return
		}
	}
	if req.BandwidthEgress != "" {
		if err := validateBandwidthFormat(req.BandwidthEgress); err != nil {
			http.Error(w, fmt.Sprintf("Invalid bandwidth_egress format: %v", err), http.StatusBadRequest)
			return
		}
	}

	// Get TAP device name
	tapDevice := vm.Network.TAPDevice
	if tapDevice == "" {
		// Try to compute it from VM ID
		tapDevice = fmt.Sprintf("tap-vm-%s", vmID[:8])
	}

	// Store old values for response
	oldIngress := vm.Network.BandwidthIngress
	oldEgress := vm.Network.BandwidthEgress

	// Apply bandwidth changes using tc commands (works on running VMs)
	if req.BandwidthEgress != "" && req.BandwidthEgress != oldEgress {
		// Update egress (from VM) limit - need to replace existing qdisc
		// First try to delete existing root qdisc
		exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "root").Run()

		// Add new tbf qdisc with the new rate (optimized: burst 128kb, latency 50ms)
		cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "root", "tbf",
			"rate", req.BandwidthEgress, "burst", "128kb", "latency", "50ms")
		if err := cmd.Run(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to update egress bandwidth: %v", err), http.StatusInternalServerError)
			return
		}
		vm.Network.BandwidthEgress = req.BandwidthEgress
	}

	if req.BandwidthIngress != "" && req.BandwidthIngress != oldIngress {
		// Update ingress (to VM) limit - need to replace existing ingress qdisc
		// First try to delete existing ingress qdisc
		exec.Command("sudo", "tc", "qdisc", "del", "dev", tapDevice, "ingress").Run()

		// Add new ingress qdisc
		cmd := exec.Command("sudo", "tc", "qdisc", "add", "dev", tapDevice, "handle", "ffff:", "ingress")
		if err := cmd.Run(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to add ingress qdisc: %v", err), http.StatusInternalServerError)
			return
		}

		// Add police filter for ingress (optimized: burst 128kb, mtu 64kb)
		cmd = exec.Command("sudo", "tc", "filter", "add", "dev", tapDevice, "parent", "ffff:",
			"protocol", "all", "u32", "match", "u32", "0", "0",
			"police", "rate", req.BandwidthIngress, "burst", "128kb", "mtu", "64kb", "drop")
		if err := cmd.Run(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to update ingress bandwidth: %v", err), http.StatusInternalServerError)
			return
		}
		vm.Network.BandwidthIngress = req.BandwidthIngress
	}

	// Persist the change
	s.persistVMState()

	response := map[string]interface{}{
		"message": "Bandwidth limits updated successfully",
		"vm_id":   vmID,
		"bandwidth": map[string]interface{}{
			"ingress": map[string]string{
				"old": oldIngress,
				"new": vm.Network.BandwidthIngress,
			},
			"egress": map[string]string{
				"old": oldEgress,
				"new": vm.Network.BandwidthEgress,
			},
		},
		"tap_device":       tapDevice,
		"applied_live":     true,
		"requires_restart": false,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getBandwidthHandler handles GET /vms/{id}/bandwidth - get current bandwidth limits
func (s *HTTPServer) getBandwidthHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.getVM(vmID)
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Get TAP device name
	tapDevice := vm.Network.TAPDevice
	if tapDevice == "" {
		tapDevice = fmt.Sprintf("tap-vm-%s", vmID[:8])
	}

	// Get actual bandwidth limits from tc
	var actualIngress, actualEgress string

	// Get egress (root qdisc)
	cmd := exec.Command("tc", "qdisc", "show", "dev", tapDevice)
	output, _ := cmd.Output()
	if strings.Contains(string(output), "tbf") {
		// Parse rate from output like "qdisc tbf 8001: root refcnt 2 rate 100Mbit burst 4Kb lat 400ms"
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "tbf") && strings.Contains(line, "rate") {
				parts := strings.Fields(line)
				for i, p := range parts {
					if p == "rate" && i+1 < len(parts) {
						actualEgress = parts[i+1]
						break
					}
				}
			}
		}
	}

	// Get ingress (police filter)
	cmd = exec.Command("tc", "filter", "show", "dev", tapDevice, "parent", "ffff:")
	output, _ = cmd.Output()
	if strings.Contains(string(output), "police") && strings.Contains(string(output), "rate") {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "police") && strings.Contains(line, "rate") {
				parts := strings.Fields(line)
				for i, p := range parts {
					if p == "rate" && i+1 < len(parts) {
						actualIngress = parts[i+1]
						break
					}
				}
			}
		}
	}

	response := map[string]interface{}{
		"vm_id":      vmID,
		"tap_device": tapDevice,
		"bandwidth": map[string]interface{}{
			"ingress": map[string]string{
				"configured": vm.Network.BandwidthIngress,
				"actual":     actualIngress,
			},
			"egress": map[string]string{
				"configured": vm.Network.BandwidthEgress,
				"actual":     actualEgress,
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// validateBandwidthFormat validates bandwidth string format (e.g., "100mbit", "1gbit", "500kbit")
func validateBandwidthFormat(bandwidth string) error {
	bandwidth = strings.ToLower(bandwidth)
	// Order by longest suffix first to match correctly (e.g., "mbit" before "bit")
	validSuffixes := []string{"gbit", "mbit", "kbit", "bit", "gbps", "mbps", "kbps", "bps"}
	hasValidSuffix := false
	for _, suffix := range validSuffixes {
		if strings.HasSuffix(bandwidth, suffix) {
			hasValidSuffix = true
			// Extract numeric part
			numPart := strings.TrimSuffix(bandwidth, suffix)
			if _, err := strconv.ParseFloat(numPart, 64); err != nil {
				return fmt.Errorf("invalid numeric value: %s", numPart)
			}
			break
		}
	}
	if !hasValidSuffix {
		return fmt.Errorf("bandwidth must end with valid unit (bit, kbit, mbit, gbit, bps, kbps, mbps, gbps)")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Web Console (token-based, xterm.js + WebSocket SSH proxy)
// ─────────────────────────────────────────────────────────────────────────────

// consolePageHTML is the embedded xterm.js console page.
// Placeholders: {{VM_ID}} and {{WS_URL}}.
const consolePageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Console — {{VM_ID}}</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.min.css"/>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#111;display:flex;flex-direction:column;height:100vh;overflow:hidden;font-family:monospace}
#hdr{background:#1e1e1e;color:#ccc;padding:7px 14px;display:flex;align-items:center;gap:10px;
     border-bottom:1px solid #333;flex-shrink:0}
#hdr h1{font-size:13px;color:#e0e0e0;white-space:nowrap}
#badge{font-size:11px;padding:2px 8px;border-radius:3px;background:#444;white-space:nowrap}
#badge.ok{background:#1b5e20;color:#a5d6a7}
#badge.err{background:#b71c1c;color:#ef9a9a}
#hint{margin-left:auto;font-size:11px;color:#666;white-space:nowrap}
#wrap{flex:1;overflow:hidden;padding:4px 4px 0 4px}
</style>
</head>
<body>
<div id="hdr">
  <h1>VM Console &nbsp;/&nbsp; {{VM_ID}}</h1>
  <span id="badge">Connecting…</span>
  <span id="hint">Copy: Ctrl+Shift+C &nbsp;·&nbsp; Paste: Ctrl+Shift+V</span>
</div>
<div id="wrap"></div>
<script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js"></script>
<script>
const WS_URL = "{{WS_URL}}";
const term = new Terminal({
  cursorBlink: true,
  fontSize: 14,
  fontFamily: 'Consolas,"Courier New",monospace',
  theme:{background:"#111",foreground:"#f0f0f0",cursor:"#ffffff"},
  scrollback: 5000,
});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("wrap"));
fit.fit();
term.focus();

const badge = document.getElementById("badge");
let ws;
function connect(){
  ws = new WebSocket(WS_URL);
  ws.binaryType = "arraybuffer";
  ws.onopen = ()=>{
    badge.textContent="Connected"; badge.className="ok";
    sendResize();
  };
  ws.onmessage = e=>{
    term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data);
  };
  ws.onclose = ()=>{
    badge.textContent="Disconnected"; badge.className="err";
    term.write("\r\n\x1b[31mSession closed.\x1b[0m\r\n");
  };
  ws.onerror = ()=>{ badge.textContent="Error"; badge.className="err"; };
}
function sendResize(){
  if(ws && ws.readyState===WebSocket.OPEN)
    ws.send(JSON.stringify({type:"resize",rows:term.rows,cols:term.cols}));
}
term.onData(d=>{
  if(ws && ws.readyState===WebSocket.OPEN)
    ws.send(new TextEncoder().encode(d));
});
term.onResize(()=>sendResize());
window.addEventListener("resize",()=>{ fit.fit(); sendResize(); });
connect();
</script>
</body>
</html>`

// createConsoleTokenHandler issues a short-lived token the billing panel can hand to the browser.
//
//	POST /vms/{id}/console/token
//	Body (optional): {"ttl_minutes": 30}
//
// Response: {"token":"…","url":"https://…/console/…","expires_at":"…","ttl_minutes":30,"vm_id":"…"}
func (s *HTTPServer) createConsoleTokenHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	if _, exists := s.getVM(vmID); !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Parse optional TTL from body
	type tokenReq struct {
		TTLMinutes int `json:"ttl_minutes"`
	}
	var req tokenReq
	req.TTLMinutes = 30
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = r.Body.Close()
	}
	if req.TTLMinutes <= 0 || req.TTLMinutes > 1440 {
		req.TTLMinutes = 30
	}

	token := uuid.New().String()
	ct := &ConsoleToken{
		Token:     token,
		VMID:      vmID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Duration(req.TTLMinutes) * time.Minute),
	}

	s.consoleTokensMu.Lock()
	s.consoleTokens[token] = ct
	s.consoleTokensMu.Unlock()

	// Build the console URL.  Prefer MICROVM_NODE_DOMAIN env var, then r.Host.
	host := r.Host
	if s.nodeDomain != "" {
		host = s.nodeDomain
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	consoleURL := fmt.Sprintf("%s://%s/console/%s", scheme, host, token)

	log.Printf("Console token issued for VM %s (expires %s)", vmID, ct.ExpiresAt.UTC().Format(time.RFC3339))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":       token,
		"url":         consoleURL,
		"expires_at":  ct.ExpiresAt.UTC().Format(time.RFC3339),
		"ttl_minutes": req.TTLMinutes,
		"vm_id":       vmID,
	})
}

// serveConsolePageHandler serves the xterm.js HTML page for a given token.
//
//	GET /console/{token}
func (s *HTTPServer) serveConsolePageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	token := vars["token"]

	s.consoleTokensMu.RLock()
	ct, exists := s.consoleTokens[token]
	s.consoleTokensMu.RUnlock()

	if !exists || time.Now().After(ct.ExpiresAt) {
		http.Error(w, "Console token invalid or expired", http.StatusForbidden)
		return
	}

	// Build WebSocket URL — same host, ws:// or wss://
	// Resolve the host exactly like the token-mint handler above. The page is
	// normally reached through a reverse proxy, and nginx's `$host` strips the
	// port, so r.Host alone hands the browser a wss:// URL on :443 — a port
	// where nothing is listening. The page then loads, the socket dies, and
	// xterm reports "disconnected" on a perfectly healthy VM.
	host := r.Host
	if s.nodeDomain != "" {
		host = s.nodeDomain
	}
	wsScheme := "wss"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		wsScheme = "ws"
	}
	wsURL := fmt.Sprintf("%s://%s/console/%s/ws", wsScheme, host, token)

	page := strings.NewReplacer("{{VM_ID}}", ct.VMID, "{{WS_URL}}", wsURL).Replace(consolePageHTML)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, page)
}

// consoleWebSocketHandler upgrades the connection to WebSocket and streams the
// VM serial console (ttyS0) bidirectionally via the on-disk FIFO + log file.
// This works regardless of VM network state — even for stopped or booting VMs.
//
//	GET /console/{token}/ws
func (s *HTTPServer) consoleWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	token := vars["token"]

	s.consoleTokensMu.RLock()
	ct, exists := s.consoleTokens[token]
	s.consoleTokensMu.RUnlock()

	if !exists || time.Now().After(ct.ExpiresAt) {
		http.Error(w, "Console token invalid or expired", http.StatusForbidden)
		return
	}

	vmID := ct.VMID
	workDir := resolveWorkDir()
	vmDir := filepath.Join(workDir, "microvms", vmID)
	consoleLogPath := filepath.Join(vmDir, "console.log")
	consoleFifoPath := s.manager.GetConsoleFIFOPath(vmID)

	// Upgrade to WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return true },
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error (console %s): %v", vmID, err)
		return
	}
	defer wsConn.Close()

	log.Printf("Serial console WebSocket connected — VM %s", vmID)

	// Determine if bidirectional input is available.
	// console.in is a named pipe (FIFO) created by startFirecracker; it only
	// exists while/after the VM has been started at least once.
	hasFifo := false
	if fi, statErr := os.Stat(consoleFifoPath); statErr == nil {
		hasFifo = fi.Mode()&os.ModeNamedPipe != 0
	}

	// Wait briefly for console.log to appear (VM may be in the first seconds of boot)
	var logFile *os.File
	for i := 0; i < 20; i++ {
		logFile, err = os.Open(consoleLogPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Goroutine: tail console.log → WebSocket (works even when VM is stopped)
	stopTail := make(chan struct{})
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		if logFile == nil {
			_ = wsConn.WriteMessage(websocket.BinaryMessage,
				[]byte("\r\n[Console log not available — VM has not been started yet]\r\n"))
			return
		}
		defer logFile.Close()

		// Send the last 256 KB of history so actual guest output is visible even when
		// Firecracker internal logs (fc_api balloon spam) fill the recent portion of
		// the log.  Filtered lines are stripped before sending to the browser.
		if size, seekErr := logFile.Seek(0, io.SeekEnd); seekErr == nil {
			const historyWindow = 256 * 1024
			if size > historyWindow {
				// Seeking backwards lands mid-line. Skip the partial first line,
				// otherwise its tail (e.g. "s-instance:fc_api] ...") is emitted as a
				// bogus line -- it no longer contains the "[anonymous-instance:"
				// marker the noise filter matches on, so it slips through.
				_, _ = logFile.Seek(-historyWindow, io.SeekEnd)
				var skip [1]byte
				for {
					n, rErr := logFile.Read(skip[:])
					if n == 0 || rErr != nil {
						break
					}
					if skip[0] == '\n' {
						break
					}
				}
			} else {
				_, _ = logFile.Seek(0, io.SeekStart)
			}
		}

		buf := make([]byte, 4096)
		pendingLine := ""

		// The serial console is a character stream, not a line-oriented file. A
		// shell prompt ("root@host:~# ") has no trailing newline, so pure line
		// buffering would withhold it until the user pressed Enter, making the
		// console look frozen. Flush a held partial line once the guest goes quiet
		// for a moment. writeMu keeps the timer and the read loop from interleaving
		// WebSocket frames.
		var writeMu sync.Mutex
		const partialFlushDelay = 120 * time.Millisecond
		var flushTimer *time.Timer

		flushPartial := func() {
			writeMu.Lock()
			defer writeMu.Unlock()
			if pendingLine == "" {
				return
			}
			partial := pendingLine
			pendingLine = ""
			// A partial line can be half of a Firecracker log line too; drop it
			// rather than leaking a truncated log fragment to the browser.
			if shouldFilterConsoleNoiseLine(partial) {
				return
			}
			_ = wsConn.WriteMessage(websocket.BinaryMessage, []byte(partial))
		}

		for {
			select {
			case <-stopTail:
				return
			default:
			}
			n, readErr := logFile.Read(buf)
			if n > 0 {
				if flushTimer != nil {
					flushTimer.Stop()
				}

				writeMu.Lock()
				chunk := pendingLine + string(buf[:n])
				lines := strings.Split(chunk, "\n")

				if strings.HasSuffix(chunk, "\n") {
					pendingLine = ""
					// Split yields a trailing empty element for the chunk's final
					// newline. That element is a chunk-boundary artifact, not a real
					// blank line: if every line in the chunk was filtered out, it
					// still leaves a non-empty buffer and we would write a bare "\n".
					// Firecracker emits ~2 fc_api lines every 2s, so this produced a
					// phantom newline each cycle and scrolled the browser terminal
					// down forever even with an idle guest.
					if len(lines) > 1 {
						lines = lines[:len(lines)-1]
					}
				} else {
					pendingLine = lines[len(lines)-1]
					lines = lines[:len(lines)-1]
				}

				var filtered strings.Builder
				for _, line := range lines {
					if shouldFilterConsoleNoiseLine(line) {
						continue
					}
					filtered.WriteString(line)
					filtered.WriteByte('\n')
				}

				if filtered.Len() > 0 {
					if writeErr := wsConn.WriteMessage(websocket.BinaryMessage, []byte(filtered.String())); writeErr != nil {
						writeMu.Unlock()
						return
					}
				}
				hasPartial := pendingLine != ""
				writeMu.Unlock()

				if hasPartial {
					flushTimer = time.AfterFunc(partialFlushDelay, flushPartial)
				}
			}
			if readErr == io.EOF {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if readErr != nil {
				return
			}
		}
	}()

	defer func() {
		close(stopTail)
		<-tailDone
	}()

	var fifoWriter *os.File
	if hasFifo {
		// Open FIFO with blocking I/O (no O_NONBLOCK) so writes don't fail with EAGAIN
		// We need to keep this fd open for the duration of the WebSocket connection
		if writer, openErr := os.OpenFile(consoleFifoPath, os.O_WRONLY, 0); openErr == nil {
			fifoWriter = writer
			log.Printf("✅ Opened console FIFO (blocking) for VM %s", vmID)
		} else {
			log.Printf("⚠️  Failed to open console FIFO for VM %s: %v", vmID, openErr)
		}
	}
	defer func() {
		if fifoWriter != nil {
			_ = fifoWriter.Close()
		}
	}()

	// Read from WebSocket; forward keystrokes to the VM via console.in FIFO.
	for {
		_, data, wsErr := wsConn.ReadMessage()
		if wsErr != nil {
			break
		}
		// Debug: Log incoming WebSocket data (only for non-control messages)
		// Reduce noise by not logging every keystroke
		var ctrl struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "resize" {
			// resize control message - skip debug log
		} else if len(data) > 0 && (len(data) > 1 || data[0] >= 32) {
			// Only log if it's not a single control character
			// This filters out arrow keys, function keys, etc.
			log.Printf("console ws got %d bytes for VM %s", len(data), vmID)
		}

		// Ignore resize control messages (no PTY on raw serial console)
		if ctrl.Type == "resize" {
			continue
		}
		if !hasFifo {
			continue // read-only mode; VM started before FIFO support
		}

		if fifoWriter == nil {
			// Attempt to (re)open once if writer is not currently available.
			if writer, openErr := os.OpenFile(consoleFifoPath, os.O_WRONLY, 0); openErr == nil {
				fifoWriter = writer
				log.Printf("✅ Reopened console FIFO for VM %s", vmID)
			} else {
				log.Printf("⚠️  Failed to open console FIFO for VM %s: %v", vmID, openErr)
				continue
			}
		}

		if _, writeErr := fifoWriter.Write(data); writeErr != nil {
			// Check if it's a "broken pipe" error (EPIPE) which means the reader closed
			// This can happen if the VM stops or systemd closes stdin
			if isEPIPE(writeErr) {
				log.Printf("⚠️  FIFO broken pipe for VM %s (VM may have stopped): %v", vmID, writeErr)
				_ = fifoWriter.Close()
				fifoWriter = nil
			} else {
				// Other write errors (disk full, etc.)
				log.Printf("⚠️  FIFO write error for VM %s: %v", vmID, writeErr)
			}
		} else {
			// Successfully wrote data - don't log every keystroke to reduce noise
			// log.Printf("✅ FIFO wrote %d bytes for VM %s", len(data), vmID)
		}
	}

	log.Printf("Serial console WebSocket disconnected — VM %s", vmID)
}

func shouldFilterConsoleNoiseLine(line string) bool {
	// Filter Firecracker internal log lines that appear on stdout when no logger
	// is configured (older VMs without a "logger" section in config.json).
	// These lines all start with a timestamp and bracket-enclosed thread name.
	// Examples:
	//   2026-02-28T06:24:48.416056718 [anonymous-instance:fc_api] ...
	//   2026-02-28T06:24:48.438157597 [anonymous-instance:main] ...
	//   2026-02-28T06:24:48.446741483 [anonymous-instance:fc_vcpu 0] ...
	if strings.Contains(line, "[anonymous-instance:") {
		return true
	}
	// Guest filesystem corruption noise: a damaged FS can emit thousands of
	// identical EXT4 error lines (one per lookup), which floods the terminal,
	// makes the page churn and forces users to reload. Volume is the signal —
	// these repeat verbatim with only the PID/inode varying.
	if strings.Contains(line, "EXT4-fs error") {
		return true
	}
	if strings.Contains(line, "deleted inode referenced") {
		return true
	}
	// Kernel OOM / crash spam from guests
	if strings.Contains(line, "Killed process") ||
		strings.Contains(line, "Out of memory: Killed process") ||
		strings.Contains(line, "general protection fault") {
		return true
	}
	return false
}

// isEPIPE checks if an error is EPIPE (broken pipe)
func isEPIPE(err error) bool {
	if err == nil {
		return false
	}
	// Check for syscall.EPIPE
	if err == syscall.EPIPE {
		return true
	}
	// Check for *os.PathError with EPIPE
	if opErr, ok := err.(*os.PathError); ok {
		if opErr.Err == syscall.EPIPE {
			return true
		}
	}
	// Check error string as fallback
	return strings.Contains(strings.ToLower(err.Error()), "broken pipe") ||
		strings.Contains(strings.ToLower(err.Error()), "epipe")
}

// consoleTokenCleanup periodically removes expired console tokens from memory.
func (s *HTTPServer) consoleTokenCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.consoleTokensMu.Lock()
		for token, ct := range s.consoleTokens {
			if now.After(ct.ExpiresAt) {
				delete(s.consoleTokens, token)
			}
		}
		s.consoleTokensMu.Unlock()
	}
}

// ovnFloatingFor returns the OVN backend if this host runs it, else nil. The
// caller answers 400/404 so the reason is specific rather than a nil panic.
func (s *HTTPServer) ovnFloatingFor() *network.OVNBackend {
	if s.manager == nil {
		return nil
	}
	return s.manager.GetOVNBackend()
}

// attachFloatingIPHandler maps a public address onto a VM on the OVN overlay:
// POST /vms/{id}/floating-ip
func (s *HTTPServer) attachFloatingIPHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "floating IPs require MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}

	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Address) == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}
	if net.ParseIP(req.Address) == nil {
		http.Error(w, fmt.Sprintf("invalid address %q", req.Address), http.StatusBadRequest)
		return
	}

	if err := b.AttachFloatingIP(vmID, req.Address); err != nil {
		http.Error(w, fmt.Sprintf("failed to attach floating IP: %v", err), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "floating IP attached",
		"vm_id":   vmID,
		"address": req.Address,
	})
}

// listFloatingIPsHandler lists a VM's public addresses:
// GET /vms/{id}/floating-ip
func (s *HTTPServer) listFloatingIPsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "floating IPs require MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b.ListFloatingIPs(vmID))
}

// detachFloatingIPHandler withdraws a public address:
// DELETE /vms/{id}/floating-ip/{address}
func (s *HTTPServer) detachFloatingIPHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "floating IPs require MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}

	if err := b.DetachFloatingIP(vmID, vars["address"]); err != nil {
		http.Error(w, fmt.Sprintf("failed to detach floating IP: %v", err), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "floating IP detached",
		"vm_id":   vmID,
		"address": vars["address"],
	})
}

// --- VPC / Network endpoints ---

// createNetworkHandler: POST /networks
func (s *HTTPServer) createNetworkHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string   `json:"name"`
		CIDR       string   `json:"cidr"`
		Gateway    string   `json:"gateway"`
		DNSServers []string `json:"dns_servers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.CIDR == "" {
		http.Error(w, "cidr is required", http.StatusBadRequest)
		return
	}

	nw := &models.Network{
		ID:         uuid.New().String(),
		Name:       req.Name,
		CIDR:       req.CIDR,
		Gateway:    req.Gateway,
		DNSServers: req.DNSServers,
		CreatedAt:  time.Now(),
	}
	if nw.Gateway == "" {
		_, ipNet, err := net.ParseCIDR(req.CIDR)
		if err == nil {
			ip := make(net.IP, len(ipNet.IP))
			copy(ip, ipNet.IP)
			ip[len(ip)-1] |= 1
			nw.Gateway = ip.String()
		}
	}

	if s.vmStateStorage != nil {
		if err := s.vmStateStorage.SaveNetwork(nw); err != nil {
			http.Error(w, fmt.Sprintf("failed to save network: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Wire into OVN backend if running
	if b := s.manager.GetOVNBackend(); b != nil {
		b.SetNetworks(toOVNNetworks(appendNetworks(s, nw)))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(nw)
}

// listNetworksHandler: GET /networks
func (s *HTTPServer) listNetworksHandler(w http.ResponseWriter, r *http.Request) {
	var nets []*models.Network
	if s.vmStateStorage != nil {
		loaded, err := s.vmStateStorage.LoadAllNetworks()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to load networks: %v", err), http.StatusInternalServerError)
			return
		}
		nets = loaded
	}
	if nets == nil {
		nets = []*models.Network{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nets)
}

// getNetworkHandler: GET /networks/{id}
func (s *HTTPServer) getNetworkHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if s.vmStateStorage == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	net, err := s.vmStateStorage.LoadNetwork(id)
	if err != nil {
		http.Error(w, "network not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(net)
}

// deleteNetworkHandler: DELETE /networks/{id}
func (s *HTTPServer) deleteNetworkHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if s.vmStateStorage != nil {
		if err := s.vmStateStorage.DeleteNetwork(id); err != nil {
			http.Error(w, fmt.Sprintf("failed to delete network: %v", err), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "network deleted", "id": id})
}

// --- Port forwarding endpoints ---

// attachPortForwardHandler: POST /vms/{id}/port-forward
func (s *HTTPServer) attachPortForwardHandler(w http.ResponseWriter, r *http.Request) {
	vmID := mux.Vars(r)["id"]
	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "port forwarding requires MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}

	var req struct {
		ExternalIP   string `json:"external_ip"`
		ExternalPort int    `json:"external_port"`
		InternalPort int    `json:"internal_port"`
		Protocol     string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.ExternalIP == "" || req.ExternalPort == 0 || req.InternalPort == 0 {
		http.Error(w, "external_ip, external_port, and internal_port are required", http.StatusBadRequest)
		return
	}
	if req.Protocol == "" {
		req.Protocol = "tcp"
	}

	pf := network.PortForward{
		VMID:         vmID,
		ExternalIP:   req.ExternalIP,
		ExternalPort: req.ExternalPort,
		InternalPort: req.InternalPort,
		Protocol:     req.Protocol,
	}
	if err := b.AttachPortForward(pf); err != nil {
		http.Error(w, fmt.Sprintf("failed to attach port forward: %v", err), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":       "port forward attached",
		"vm_id":         vmID,
		"external_ip":   req.ExternalIP,
		"external_port": req.ExternalPort,
		"internal_port": req.InternalPort,
		"protocol":      req.Protocol,
	})
}

// listPortForwardsHandler: GET /vms/{id}/port-forwards
func (s *HTTPServer) listPortForwardsHandler(w http.ResponseWriter, r *http.Request) {
	vmID := mux.Vars(r)["id"]
	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "port forwarding requires MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b.ListPortForwards(vmID))
}

// detachPortForwardHandler: DELETE /vms/{id}/port-forward/{external_ip}/{port}
func (s *HTTPServer) detachPortForwardHandler(w http.ResponseWriter, r *http.Request) {
	vmID := mux.Vars(r)["id"]
	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "port forwarding requires MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}
	vars := mux.Vars(r)
	externalIP := vars["external_ip"]
	portStr := vars["port"]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	if err := b.DetachPortForward(vmID, externalIP, port); err != nil {
		http.Error(w, fmt.Sprintf("failed to detach port forward: %v", err), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "port forward detached"})
}

// toOVNNetworks converts models.Network to network.OVNNetwork.
func toOVNNetworks(nets []*models.Network) []*network.OVNNetwork {
	out := make([]*network.OVNNetwork, 0, len(nets))
	for _, n := range nets {
		out = append(out, &network.OVNNetwork{
			ID:         n.ID,
			Name:       n.Name,
			CIDR:       n.CIDR,
			Gateway:    n.Gateway,
			DNSServers: n.DNSServers,
		})
	}
	return out
}

// appendNetworks is a helper to build the network list for SetNetworks.
func appendNetworks(s *HTTPServer, net *models.Network) []*models.Network {
	var nets []*models.Network
	if s.vmStateStorage != nil {
		loaded, err := s.vmStateStorage.LoadAllNetworks()
		if err == nil {
			nets = loaded
		}
	}
	// Ensure the new network is included
	found := false
	for _, n := range nets {
		if n.ID == net.ID {
			found = true
			break
		}
	}
	if !found {
		nets = append(nets, net)
	}
	return nets
}

// setSecurityRulesHandler: PUT /vms/{id}/security-rules
// Replaces a VM's security group. The allow-only baseline stays; custom rules
// sit above it by priority, and an Allow=false entry is a deny rule.
func (s *HTTPServer) setSecurityRulesHandler(w http.ResponseWriter, r *http.Request) {
	vmID := mux.Vars(r)["id"]
	vm, ok := s.getVM(vmID)
	if !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	var req struct {
		Rules []models.SecurityRule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	// Validate before storing, so a bad rule cannot half-apply.
	if _, err := network.ToOVNRules(req.Rules); err != nil {
		http.Error(w, fmt.Sprintf("invalid rule: %v", err), http.StatusBadRequest)
		return
	}

	if s.vmStateStorage != nil {
		if err := s.vmStateStorage.SaveSecurityRules(vmID, req.Rules); err != nil {
			http.Error(w, fmt.Sprintf("failed to save rules: %v", err), http.StatusInternalServerError)
			return
		}
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "security groups require MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}
	if err := b.ApplySecurityRules(vmID, req.Rules); err != nil {
		http.Error(w, fmt.Sprintf("failed to apply rules: %v", err), http.StatusUnprocessableEntity)
		return
	}
	vm.Network.HasSecurityRules = true

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "security rules applied",
		"vm_id":   vmID,
		"rules":   req.Rules,
	})
}

// getSecurityRulesHandler: GET /vms/{id}/security-rules
func (s *HTTPServer) getSecurityRulesHandler(w http.ResponseWriter, r *http.Request) {
	vmID := mux.Vars(r)["id"]
	if _, ok := s.getVM(vmID); !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	var rules []models.SecurityRule
	if s.vmStateStorage != nil {
		loaded, err := s.vmStateStorage.LoadSecurityRules(vmID)
		if err == nil {
			rules = loaded
		}
	}
	if rules == nil {
		rules = []models.SecurityRule{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rules)
}

// listNetworkModesHandler reports the network modes this host can serve
// right now, so a client can offer only the choices that will work. "ovn" is
// present when the overlay is up, even when the default backend is the
// bridge: the two coexist since the overlay probe.
func (s *HTTPServer) listNetworkModesHandler(w http.ResponseWriter, r *http.Request) {
	defaultMode := models.NetworkModeNAT
	var modes []string
	if s.manager != nil {
		modes = s.manager.SupportedNetworkModes()
		defaultMode = s.manager.DefaultNetworkMode()
	} else {
		modes = []string{string(models.NetworkModeNAT)}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"default": string(defaultMode),
		"modes":   modes,
	})
}

// setNetworkFirewallHandler: PUT /networks/{id}/firewall
// Network ACLs: stateless rules evaluated for traffic on the VPC's logical
// switch, before any per-VM security-group rule.
func (s *HTTPServer) setNetworkFirewallHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if s.vmStateStorage == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	netRow, err := s.vmStateStorage.LoadNetwork(id)
	if err != nil {
		http.Error(w, "network not found", http.StatusNotFound)
		return
	}
	var req struct {
		Rules []models.SecurityRule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if _, err := network.ToOVNRules(req.Rules); err != nil {
		http.Error(w, fmt.Sprintf("invalid rule: %v", err), http.StatusBadRequest)
		return
	}
	netRow.FirewallRules = req.Rules
	if err := s.vmStateStorage.SaveNetwork(netRow); err != nil {
		http.Error(w, fmt.Sprintf("failed to save firewall: %v", err), http.StatusInternalServerError)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "network firewall requires MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}
	if err := b.ApplyNetworkFirewall(ovn.VMSwitchName(netRow.Name), req.Rules); err != nil {
		http.Error(w, fmt.Sprintf("failed to apply firewall: %v", err), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "network firewall applied",
		"id":      id,
		"rules":   req.Rules,
	})
}

// getNetworkFirewallHandler: GET /networks/{id}/firewall
func (s *HTTPServer) getNetworkFirewallHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if s.vmStateStorage == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	netRow, err := s.vmStateStorage.LoadNetwork(id)
	if err != nil {
		http.Error(w, "network not found", http.StatusNotFound)
		return
	}
	if netRow.FirewallRules == nil {
		netRow.FirewallRules = []models.SecurityRule{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(netRow.FirewallRules)
}

// peerNetworksHandler: POST /networks/{id}/peer
// Inter-VPC routing: two VPCs' routers exchange routes while keeping separate
// broadcast domains.
func (s *HTTPServer) peerNetworksHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if s.vmStateStorage == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	self, err := s.vmStateStorage.LoadNetwork(id)
	if err != nil {
		http.Error(w, "network not found", http.StatusNotFound)
		return
	}
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.PeerID == "" || req.PeerID == id {
		http.Error(w, "peer_id is required and must differ from this network", http.StatusBadRequest)
		return
	}
	peer, err := s.vmStateStorage.LoadNetwork(req.PeerID)
	if err != nil {
		http.Error(w, "peer network not found", http.StatusBadRequest)
		return
	}
	// Refuse overlapping subnets: two identical prefixes make the routes
	// ambiguous and OVN would silently prefer one.
	if cidrsOverlap(self.CIDR, peer.CIDR) {
		http.Error(w, fmt.Sprintf("cannot peer %s and %s: subnets overlap", self.CIDR, peer.CIDR), http.StatusBadRequest)
		return
	}

	b := s.ovnFloatingFor()
	if b == nil {
		http.Error(w, "peering requires MICROVM_NETWORK_BACKEND=ovn", http.StatusBadRequest)
		return
	}
	if err := b.PeerNetworks(context.Background(), id, req.PeerID); err != nil {
		http.Error(w, fmt.Sprintf("failed to peer: %v", err), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "networks peered",
		"id":      id,
		"peer_id": req.PeerID,
	})
}

// cidrsOverlap reports whether two CIDRs share any address.
func cidrsOverlap(a, b string) bool {
	_, na, err := net.ParseCIDR(a)
	if err != nil {
		return false
	}
	_, nb, err := net.ParseCIDR(b)
	if err != nil {
		return false
	}
	return na.Contains(nb.IP) || nb.Contains(na.IP)
}
