package balloon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/metrics"
	"github.com/haioco/vmlet/internal/models"
)

// VMMemoryProfile tracks memory usage patterns for smart balloon limits
type VMMemoryProfile struct {
	VMID               string    `json:"vm_id"`
	PeakUsageMB        uint64    `json:"peak_usage_mb"`        // Highest usage in tracking window
	PeakUsageTime      time.Time `json:"peak_usage_time"`      // When peak occurred
	AvgUsageMB         uint64    `json:"avg_usage_mb"`         // Average usage over window
	OOMCount           int       `json:"oom_count"`            // OOM events detected (via major faults)
	LastOOMTime        time.Time `json:"last_oom_time"`        // Last OOM event
	LastMajorFaults    uint64    `json:"last_major_faults"`    // Track major faults for OOM detection
	SampleCount        int       `json:"sample_count"`         // Number of samples collected
	LastSampleTime     time.Time `json:"last_sample_time"`     // Last sample timestamp
	GuaranteedMemoryMB uint64    `json:"guaranteed_memory_mb"` // User-set minimum (0 = auto)
}

// AutoBalloonManager handles automatic memory balloon adjustment
type AutoBalloonManager struct {
	enabled           bool
	interval          time.Duration
	fastPollInterval  time.Duration // Fast polling for VMs with active balloons
	mutex             sync.RWMutex
	stopChan          chan struct{}
	vmGetter          VMGetter
	hostMemoryLowMB   uint64                      // Host memory threshold to start reclaiming
	vmMemoryLowMB     uint64                      // VM memory threshold to deflate balloon
	vmSafetyBufferMB  uint64                      // Safety buffer for sudden memory spikes
	maxBalloonPercent float64                     // Maximum balloon as percentage of VM RAM
	alwaysReclaimIdle bool                        // Reclaim from idle VMs even when host isn't low
	lastVMFreeMemory  map[string]uint64           // Track free memory to detect rapid changes
	balloonedVMs      map[string]bool             // VMs whose balloon is currently inflated (actual_mib > 0)
	balloonedMutex    sync.RWMutex                // Protect balloonedVMs
	clientPool        map[string]*http.Client     // Reusable HTTP clients per socket
	clientPoolMutex   sync.Mutex                  // Protect client pool
	vmProfiles        map[string]*VMMemoryProfile // Memory usage profiles per VM
	profilesMutex     sync.RWMutex                // Protect profiles map
	workDir           string                      // Work directory for profile persistence
}

// VMGetter interface to get VMs without circular dependency
type VMGetter interface {
	GetAllVMs() []*models.MicroVMInstance
}

// computeMaxBalloonMB calculates the maximum balloon size for a VM using smart limits
// Priority: 1) User-set guarantee, 2) OOM recovery, 3) Peak usage, 4) Conservative default
func (abm *AutoBalloonManager) computeMaxBalloonMB(vm *models.MicroVMInstance, vmTotalMB uint64) uint64 {
	// Priority 1: If VM has explicit autoscale MinMemoryMB, use that as the floor
	if vm.Autoscale != nil && vm.Autoscale.MinMemoryMB > 0 {
		minMemoryMB := uint64(vm.Autoscale.MinMemoryMB)
		if vmTotalMB > minMemoryMB {
			return vmTotalMB - minMemoryMB
		}
		return 0
	}

	// Get or create VM memory profile
	profile := abm.getOrCreateProfile(vm.ID)

	// Priority 2: User-set guaranteed memory via profile
	if profile.GuaranteedMemoryMB > 0 {
		if vmTotalMB > profile.GuaranteedMemoryMB {
			return vmTotalMB - profile.GuaranteedMemoryMB
		}
		return 0
	}

	// Priority 3: If OOM detected recently (within 24h), be very conservative
	if profile.OOMCount > 0 && time.Since(profile.LastOOMTime) < 24*time.Hour {
		// After OOM: guarantee peak + 25% buffer, very limited balloon
		if profile.PeakUsageMB > 0 {
			guaranteeMB := uint64(float64(profile.PeakUsageMB) * 1.25)
			if guaranteeMB >= vmTotalMB {
				return 0 // No balloon allowed after OOM if peak near total
			}
			maxBalloon := vmTotalMB - guaranteeMB
			log.Printf("🎈 VM %s: OOM recovery mode - max balloon %d MB (peak=%d MB, guarantee=%d MB)",
				vm.ID, maxBalloon, profile.PeakUsageMB, guaranteeMB)
			return maxBalloon
		}
		// OOM but no peak data - disable balloon entirely
		return 0
	}

	// Priority 4: Usage-based limit (if we have enough samples)
	if profile.SampleCount >= 6 && profile.PeakUsageMB > 0 { // At least 1 minute of data
		// Guarantee peak usage + 15% buffer for spikes
		guaranteeMB := uint64(float64(profile.PeakUsageMB) * 1.15)
		if guaranteeMB >= vmTotalMB {
			return 0 // Peak usage near total - no balloon
		}
		return vmTotalMB - guaranteeMB
	}

	// Priority 5: Conservative default for new VMs (no data yet)
	// Use 25% max balloon instead of 75% - much safer
	// For 8GB VM: max balloon = 2GB, user always has 6GB
	conservativeMaxBalloon := uint64(float64(vmTotalMB) * 0.25)
	return conservativeMaxBalloon
}

// getOrCreateProfile returns existing profile or creates new one
// It first tries to load from disk if not in memory
func (abm *AutoBalloonManager) getOrCreateProfile(vmID string) *VMMemoryProfile {
	abm.profilesMutex.RLock()
	profile, exists := abm.vmProfiles[vmID]
	abm.profilesMutex.RUnlock()

	if exists {
		return profile
	}

	// Try to load from disk first
	if abm.workDir != "" {
		_ = abm.LoadProfile(vmID) // Ignore error, will create new if not found
		// Check if loaded
		abm.profilesMutex.RLock()
		profile, exists = abm.vmProfiles[vmID]
		abm.profilesMutex.RUnlock()
		if exists {
			return profile
		}
	}

	// Create new profile
	abm.profilesMutex.Lock()
	defer abm.profilesMutex.Unlock()

	// Double-check after acquiring write lock
	if profile, exists = abm.vmProfiles[vmID]; exists {
		return profile
	}

	profile = &VMMemoryProfile{
		VMID:           vmID,
		LastSampleTime: time.Now(),
	}
	abm.vmProfiles[vmID] = profile
	return profile
}

// updateVMProfile updates the memory usage profile for a VM
func (abm *AutoBalloonManager) updateVMProfile(vmID string, usedMB uint64, stats *BalloonStats) {
	profile := abm.getOrCreateProfile(vmID)

	abm.profilesMutex.Lock()
	defer abm.profilesMutex.Unlock()

	// Update peak usage if current is higher
	if usedMB > profile.PeakUsageMB {
		profile.PeakUsageMB = usedMB
		profile.PeakUsageTime = time.Now()
	}

	// Update running average
	if profile.SampleCount == 0 {
		profile.AvgUsageMB = usedMB
	} else {
		// Exponential moving average (weight recent samples more)
		profile.AvgUsageMB = (profile.AvgUsageMB*9 + usedMB) / 10
	}

	// Detect OOM via major faults spike (>10000 new faults indicates memory thrashing)
	if profile.LastMajorFaults > 0 && stats.MajorFaults > profile.LastMajorFaults+10000 {
		profile.OOMCount++
		profile.LastOOMTime = time.Now()
		log.Printf("🎈 OOM DETECTED: VM %s - major faults spike %d -> %d (OOM count: %d)",
			vmID, profile.LastMajorFaults, stats.MajorFaults, profile.OOMCount)
	}
	profile.LastMajorFaults = stats.MajorFaults

	profile.SampleCount++
	profile.LastSampleTime = time.Now()

	// Reset peak after 24 hours to adapt to changing workloads
	if time.Since(profile.PeakUsageTime) > 24*time.Hour {
		profile.PeakUsageMB = usedMB
		profile.PeakUsageTime = time.Now()
	}
}

// SetVMGuaranteedMemory sets explicit memory guarantee for a VM (API use)
func (abm *AutoBalloonManager) SetVMGuaranteedMemory(vmID string, guaranteeMB uint64) {
	profile := abm.getOrCreateProfile(vmID)

	abm.profilesMutex.Lock()
	profile.GuaranteedMemoryMB = guaranteeMB
	abm.profilesMutex.Unlock()

	log.Printf("🎈 VM %s: memory guarantee set to %d MB", vmID, guaranteeMB)

	// Auto-save to persist the guarantee
	abm.SaveProfile(vmID)
}

// GetVMProfile returns the memory profile for a VM (API use)
func (abm *AutoBalloonManager) GetVMProfile(vmID string) *VMMemoryProfile {
	abm.profilesMutex.RLock()
	defer abm.profilesMutex.RUnlock()

	if profile, exists := abm.vmProfiles[vmID]; exists {
		// Return a copy to avoid race conditions
		profileCopy := *profile
		return &profileCopy
	}
	return nil
}

// SetWorkDir sets the work directory for profile persistence
func (abm *AutoBalloonManager) SetWorkDir(workDir string) {
	abm.workDir = workDir
	log.Printf("🎈 Auto-balloon manager: work directory set to %s", workDir)
}

// getProfilePath returns the file path for a VM's profile
func (abm *AutoBalloonManager) getProfilePath(vmID string) string {
	if abm.workDir == "" {
		return ""
	}
	return filepath.Join(abm.workDir, "microvms", vmID, "profile.json")
}

// SaveProfile persists a VM's memory profile to disk
func (abm *AutoBalloonManager) SaveProfile(vmID string) error {
	if abm.workDir == "" {
		return nil // Persistence not configured
	}

	abm.profilesMutex.RLock()
	profile, exists := abm.vmProfiles[vmID]
	if !exists {
		abm.profilesMutex.RUnlock()
		return nil // No profile to save
	}
	// Make a copy while holding lock
	profileCopy := *profile
	abm.profilesMutex.RUnlock()

	profilePath := abm.getProfilePath(vmID)
	if profilePath == "" {
		return nil
	}

	// Ensure directory exists
	dir := filepath.Dir(profilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create profile directory: %w", err)
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(&profileCopy, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	// Write atomically (write to temp, then rename)
	tempPath := profilePath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}
	if err := os.Rename(tempPath, profilePath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename profile: %w", err)
	}

	log.Printf("🎈 VM %s: profile saved to %s", vmID, profilePath)
	return nil
}

// LoadProfile loads a VM's memory profile from disk
func (abm *AutoBalloonManager) LoadProfile(vmID string) error {
	if abm.workDir == "" {
		return nil // Persistence not configured
	}

	profilePath := abm.getProfilePath(vmID)
	if profilePath == "" {
		return nil
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No saved profile, not an error
		}
		return fmt.Errorf("failed to read profile: %w", err)
	}

	var profile VMMemoryProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return fmt.Errorf("failed to unmarshal profile: %w", err)
	}

	abm.profilesMutex.Lock()
	abm.vmProfiles[vmID] = &profile
	abm.profilesMutex.Unlock()

	log.Printf("🎈 VM %s: profile loaded from %s (peak=%d MB, guarantee=%d MB, oom_count=%d)",
		vmID, profilePath, profile.PeakUsageMB, profile.GuaranteedMemoryMB, profile.OOMCount)
	return nil
}

// SaveAllProfiles persists all VM profiles to disk
func (abm *AutoBalloonManager) SaveAllProfiles() {
	if abm.workDir == "" {
		return
	}

	abm.profilesMutex.RLock()
	vmIDs := make([]string, 0, len(abm.vmProfiles))
	for vmID := range abm.vmProfiles {
		vmIDs = append(vmIDs, vmID)
	}
	abm.profilesMutex.RUnlock()

	for _, vmID := range vmIDs {
		if err := abm.SaveProfile(vmID); err != nil {
			log.Printf("🎈 Warning: failed to save profile for VM %s: %v", vmID, err)
		}
	}
}

// LoadAllProfiles loads profiles for all VMs with profile.json files
func (abm *AutoBalloonManager) LoadAllProfiles() {
	if abm.workDir == "" {
		return
	}

	microvmsDir := filepath.Join(abm.workDir, "microvms")
	entries, err := os.ReadDir(microvmsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("🎈 Warning: failed to read microvms directory: %v", err)
		}
		return
	}

	loadedCount := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()
		if err := abm.LoadProfile(vmID); err != nil {
			log.Printf("🎈 Warning: failed to load profile for VM %s: %v", vmID, err)
		} else if abm.vmProfiles[vmID] != nil {
			loadedCount++
		}
	}

	if loadedCount > 0 {
		log.Printf("🎈 Auto-balloon manager: loaded %d VM profiles from disk", loadedCount)
	}
}

// DeleteProfile removes a VM's profile from memory and disk
func (abm *AutoBalloonManager) DeleteProfile(vmID string) error {
	// Remove from memory
	abm.profilesMutex.Lock()
	delete(abm.vmProfiles, vmID)
	abm.profilesMutex.Unlock()

	if abm.workDir == "" {
		return nil
	}

	profilePath := abm.getProfilePath(vmID)
	if profilePath == "" {
		return nil
	}

	// Remove directory and all files
	profileDir := filepath.Dir(profilePath)
	if err := os.RemoveAll(profileDir); err != nil {
		return fmt.Errorf("failed to delete profile directory: %w", err)
	}

	log.Printf("🎈 VM %s: profile deleted from %s", vmID, profileDir)
	return nil
}

// CleanupStaleProfiles removes profiles for VMs that are not in the system
// Only called from external code with current VM list
func (abm *AutoBalloonManager) CleanupStaleProfiles(activeVMIDs map[string]bool) {
	if abm.workDir == "" {
		return
	}

	microvmsDir := filepath.Join(abm.workDir, "microvms")
	entries, err := os.ReadDir(microvmsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("🎈 Warning: failed to read microvms directory for cleanup: %v", err)
		}
		return
	}

	cleanedCount := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()
		if !activeVMIDs[vmID] {
			// This profile is for a deleted/non-existent VM
			profileDir := filepath.Join(microvmsDir, vmID)
			if err := os.RemoveAll(profileDir); err != nil {
				log.Printf("🎈 Warning: failed to clean profile for VM %s: %v", vmID, err)
			} else {
				log.Printf("🎈 Cleaned up stale profile for VM %s", vmID)
				cleanedCount++
			}
		}
	}

	if cleanedCount > 0 {
		log.Printf("🎈 Profile cleanup: removed %d stale VM profiles", cleanedCount)
	}
}

// Config for auto balloon manager
type Config struct {
	Enabled           bool
	CheckInterval     time.Duration
	FastPollInterval  time.Duration
	HostMemoryLowMB   uint64  // Start reclaiming when host free memory < this
	VMMemoryLowMB     uint64  // Deflate balloon when VM free memory < this
	VMSafetyBufferMB  uint64  // Always keep this much free in VM (safety for sudden spikes)
	ReclaimPercentage float64 // Percentage of VM free memory to reclaim (0.0-1.0)
	MaxBalloonPercent float64 // Maximum percentage of VM memory to balloon (0.0-1.0), 0 = use default 95%
	AlwaysReclaimIdle bool    // Reclaim from idle VMs even when host isn't low on memory
}

// NewAutoBalloonManager creates a new automatic balloon manager
func NewAutoBalloonManager(vmGetter VMGetter, config Config) *AutoBalloonManager {
	if config.CheckInterval == 0 {
		config.CheckInterval = 30 * time.Second
	}
	if config.FastPollInterval < 0 {
		config.FastPollInterval = 0
	}
	if config.HostMemoryLowMB == 0 {
		config.HostMemoryLowMB = 2048 // 2GB default
	}
	if config.VMMemoryLowMB == 0 {
		config.VMMemoryLowMB = 128 // 128MB default minimum free in VM
	}
	// Note: VMSafetyBufferMB == 0 now means "adaptive" (computed per-VM).
	if config.ReclaimPercentage == 0 {
		config.ReclaimPercentage = 0.7 // Reclaim 70% of free memory by default (more aggressive)
	}
	if config.MaxBalloonPercent == 0 {
		config.MaxBalloonPercent = 0.95 // Allow ballooning up to 95% of VM memory (was 70%)
	}

	return &AutoBalloonManager{
		enabled:  config.Enabled,
		interval: config.CheckInterval,
		fastPollInterval: func() time.Duration {
			if config.FastPollInterval > 0 {
				return config.FastPollInterval
			}
			return 3 * time.Second // default fast poll
		}(),
		stopChan:          make(chan struct{}),
		vmGetter:          vmGetter,
		hostMemoryLowMB:   config.HostMemoryLowMB,
		vmMemoryLowMB:     config.VMMemoryLowMB,
		vmSafetyBufferMB:  config.VMSafetyBufferMB,
		maxBalloonPercent: config.MaxBalloonPercent,
		alwaysReclaimIdle: config.AlwaysReclaimIdle,
		lastVMFreeMemory:  make(map[string]uint64),
		balloonedVMs:      make(map[string]bool),
		clientPool:        make(map[string]*http.Client),
		vmProfiles:        make(map[string]*VMMemoryProfile),
	}
}

// Start begins the automatic balloon management loop
func (abm *AutoBalloonManager) Start() {
	if !abm.enabled {
		log.Println("🎈 Auto-balloon manager: disabled")
		return
	}

	safetyMode := "adaptive"
	if abm.vmSafetyBufferMB > 0 {
		safetyMode = fmt.Sprintf("fixed=%dMB", abm.vmSafetyBufferMB)
	}
	log.Printf("🎈 Auto-balloon manager: starting (interval: %v, fast poll: %v, hostLow: %d MB, vmLow: %d MB, safety: %s, maxBalloon: %.0f%%, alwaysReclaim: %v)",
		abm.interval, abm.fastPollInterval, abm.hostMemoryLowMB, abm.vmMemoryLowMB, safetyMode, abm.maxBalloonPercent*100, abm.alwaysReclaimIdle)

	go abm.run()
	go abm.fastPollRun() // Start fast polling loop for VMs with active balloons
}

// Stop stops the automatic balloon management
func (abm *AutoBalloonManager) Stop() {
	if !abm.enabled {
		return
	}
	close(abm.stopChan)
	// Close pooled clients' idle connections
	abm.clientPoolMutex.Lock()
	for k, c := range abm.clientPool {
		if c != nil {
			c.CloseIdleConnections()
		}
		delete(abm.clientPool, k)
	}
	abm.clientPoolMutex.Unlock()

	// Save all profiles before stopping
	abm.SaveAllProfiles()

	log.Println("🎈 Auto-balloon manager: stopped")
}

// run is the main loop for automatic balloon adjustment
func (abm *AutoBalloonManager) run() {
	ticker := time.NewTicker(abm.interval)
	defer ticker.Stop()

	// Periodic save ticker (every 5 minutes)
	saveTicker := time.NewTicker(5 * time.Minute)
	defer saveTicker.Stop()

	// Periodic cleanup ticker (every 10 minutes)
	cleanupTicker := time.NewTicker(10 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-abm.stopChan:
			return
		case <-ticker.C:
			abm.adjustBalloons()
		case <-saveTicker.C:
			// Periodically save profiles to disk
			abm.SaveAllProfiles()
		case <-cleanupTicker.C:
			// Periodically clean up stale profiles for deleted VMs
			abm.cleanupStaleProfilesInternal()
		}
	}
}

// cleanupStaleProfilesInternal removes profiles for non-existent VMs
func (abm *AutoBalloonManager) cleanupStaleProfilesInternal() {
	// Get current active VMs from vmGetter
	if abm.vmGetter == nil {
		return
	}

	activeVMs := abm.vmGetter.GetAllVMs()
	activeVMIDs := make(map[string]bool)
	for _, vm := range activeVMs {
		activeVMIDs[vm.ID] = true
	}

	abm.CleanupStaleProfiles(activeVMIDs)
}

// fastPollRun is a fast polling loop specifically for VMs with active balloons
// This ensures rapid response to memory pressure
func (abm *AutoBalloonManager) fastPollRun() {
	ticker := time.NewTicker(abm.fastPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-abm.stopChan:
			return
		case <-ticker.C:
			abm.fastCheckBallooned()
		}
	}
}

// fastCheckBallooned quickly checks only VMs with active balloons for urgent deflation
func (abm *AutoBalloonManager) fastCheckBallooned() {
	vms := abm.vmGetter.GetAllVMs()
	if len(vms) == 0 {
		return
	}

	for _, vm := range vms {
		if vm.Status != models.MicroVMStatusRunning {
			continue
		}

		// Only VMs with an inflated balloon need fast polling; everything else is
		// covered by the slower adjustBalloons pass. Polling balloon-less VMs here
		// used to hit GET /balloon/statistics for every running guest every cycle,
		// and Firecracker logs two fc_api lines per request into console.log
		// (19 GB of serial logs on node62 — issue #28).
		if !abm.isBallooned(vm.ID) {
			continue
		}

		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			continue
		}

		// Get current balloon stats
		stats, err := abm.getBalloonStats(socketPath)
		if err != nil {
			continue
		}

		currentBalloonMB := stats.ActualMib
		if currentBalloonMB == 0 {
			continue // Skip VMs without active balloons
		}

		vmFreeMB := stats.FreeMemory / (1024 * 1024)
		vmTotalMB := stats.TotalMemory / (1024 * 1024)
		safetyMB := abm.computeSafetyBufferMB(vmTotalMB)

		// Store previous free memory
		abm.mutex.Lock()
		lastFreeMB, exists := abm.lastVMFreeMemory[vm.ID]
		abm.lastVMFreeMemory[vm.ID] = vmFreeMB
		abm.mutex.Unlock()

		// Detect rapid memory drop (user is allocating memory)
		if exists && lastFreeMB > vmFreeMB+100 {
			// Memory dropped by more than 100 MB - user is allocating!
			log.Printf("🎈 FAST: VM %s memory drop %d -> %d MB (balloon: %d MB, safety=%dMB mode=%s)",
				vm.ID, lastFreeMB, vmFreeMB, currentBalloonMB, safetyMB, safetyLabel(abm.vmSafetyBufferMB))
		}

		// URGENT: Deflate immediately if below safety buffer
		if vmFreeMB < safetyMB {
			deflateMB := safetyMB - vmFreeMB
			newBalloonMB := int64(currentBalloonMB) - int64(deflateMB)
			if newBalloonMB < 0 {
				newBalloonMB = 0
			}

			log.Printf("🎈 FAST URGENT: VM %s deflation %d -> %d MB (free: %d MB < safety %d MB, mode=%s)",
				vm.ID, currentBalloonMB, newBalloonMB, vmFreeMB, safetyMB, safetyLabel(abm.vmSafetyBufferMB))

			if err := abm.setBalloonSize(socketPath, uint64(newBalloonMB)); err != nil {
				log.Printf("🎈 Error in fast deflation for VM %s: %v", vm.ID, err)
				metrics.IncCounter(metrics.BalloonErrors, map[string]string{"vm_id": vm.ID})
			} else {
				abm.setBallooned(vm.ID, newBalloonMB > 0)
			}
		}
	}
}

// setBallooned records whether a VM currently has an inflated balloon. Only these
// VMs are sampled by the fast loop.
func (abm *AutoBalloonManager) setBallooned(vmID string, active bool) {
	abm.balloonedMutex.Lock()
	defer abm.balloonedMutex.Unlock()
	if abm.balloonedVMs == nil {
		abm.balloonedVMs = make(map[string]bool)
	}
	if active {
		abm.balloonedVMs[vmID] = true
	} else {
		delete(abm.balloonedVMs, vmID)
	}
}

// isBallooned reports whether the fast poller should sample this VM.
func (abm *AutoBalloonManager) isBallooned(vmID string) bool {
	abm.balloonedMutex.RLock()
	defer abm.balloonedMutex.RUnlock()
	return abm.balloonedVMs[vmID]
}

// pruneBallooned drops bookkeeping for VMs that no longer exist.
func (abm *AutoBalloonManager) pruneBallooned(present map[string]bool) {
	abm.balloonedMutex.Lock()
	defer abm.balloonedMutex.Unlock()
	for id := range abm.balloonedVMs {
		if !present[id] {
			delete(abm.balloonedVMs, id)
		}
	}
}

// adjustBalloons checks all VMs and adjusts balloons based on memory pressure
func (abm *AutoBalloonManager) adjustBalloons() {
	vms := abm.vmGetter.GetAllVMs()
	if len(vms) == 0 {
		return
	}

	// Get host memory stats
	hostFreeMB, err := getHostFreeMemoryMB()
	if err != nil {
		log.Printf("🎈 Error getting host memory: %v", err)
		return
	}

	runningVMs := 0
	for _, vm := range vms {
		if vm.Status == models.MicroVMStatusRunning {
			runningVMs++
		}
	}

	log.Printf("🎈 Auto-balloon check: %d running VMs, host free: %d MB (threshold: %d MB)", runningVMs, hostFreeMB, abm.hostMemoryLowMB)

	// Check if we need to reclaim memory from VMs
	hostNeedsMemory := hostFreeMB < abm.hostMemoryLowMB

	// Collect VM stats for priority-based reclamation
	type vmStats struct {
		vm             *models.MicroVMInstance
		socketPath     string
		stats          *BalloonStats
		vmFreeMB       uint64
		vmTotalMB      uint64
		safetyMB       uint64
		currentBalloon uint64
		maxBalloonMB   uint64 // Per-VM max balloon (respects autoscale min_memory)
		reclaimable    uint64 // How much we CAN reclaim
	}

	var vmStatsList []vmStats
	present := make(map[string]bool, len(vms))

	for _, vm := range vms {
		present[vm.ID] = true
		if vm.Status != models.MicroVMStatusRunning {
			continue
		}

		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			continue
		}

		stats, err := abm.getBalloonStats(socketPath)
		if err != nil {
			continue
		}

		vmFreeMB := stats.FreeMemory / (1024 * 1024)
		vmTotalMB := stats.TotalMemory / (1024 * 1024)
		vmUsedMB := vmTotalMB - vmFreeMB
		safetyMB := abm.computeSafetyBufferMB(vmTotalMB)
		currentBalloon := stats.ActualMib
		abm.setBallooned(vm.ID, currentBalloon > 0)

		// Update VM memory profile for smart balloon limits
		abm.updateVMProfile(vm.ID, vmUsedMB, stats)

		// Check if OOM was just detected and handle emergency
		profile := abm.getOrCreateProfile(vm.ID)
		if profile.OOMCount > 0 && time.Since(profile.LastOOMTime) < 5*time.Second {
			// OOM just detected - emergency deflation!
			log.Printf("🎈 EMERGENCY: VM %s OOM detected - deflating balloon to 0", vm.ID)
			if err := abm.setBalloonSize(socketPath, 0); err != nil {
				log.Printf("🎈 Error in emergency deflation for VM %s: %v", vm.ID, err)
			} else {
				abm.setBallooned(vm.ID, false)
			}
			continue
		}

		// Calculate max balloon - respects autoscale min_memory if configured
		maxBalloonMB := abm.computeMaxBalloonMB(vm, vmTotalMB)

		// Calculate how much we can potentially reclaim
		var reclaimable uint64
		if vmFreeMB > safetyMB {
			reclaimable = vmFreeMB - safetyMB
			// Cap by max balloon limit
			if currentBalloon+reclaimable > maxBalloonMB {
				if maxBalloonMB > currentBalloon {
					reclaimable = maxBalloonMB - currentBalloon
				} else {
					reclaimable = 0
				}
			}
		}

		vmStatsList = append(vmStatsList, vmStats{
			vm:             vm,
			maxBalloonMB:   maxBalloonMB,
			socketPath:     socketPath,
			stats:          stats,
			vmFreeMB:       vmFreeMB,
			vmTotalMB:      vmTotalMB,
			safetyMB:       safetyMB,
			currentBalloon: currentBalloon,
			reclaimable:    reclaimable,
		})
	}

	// Process each VM
	abm.pruneBallooned(present)
	for _, vs := range vmStatsList {
		// Determine if we should reclaim from this VM
		shouldReclaim := hostNeedsMemory || abm.alwaysReclaimIdle

		// Check if VM is "idle" (free memory > 2x safety buffer)
		vmIsIdle := vs.vmFreeMB > vs.safetyMB*2

		if shouldReclaim && vs.reclaimable > 0 && vmIsIdle {
			// RECLAIM MODE: Inflate balloon to take memory from guest

			// Calculate how much to reclaim this cycle
			// More aggressive: take 70% of reclaimable (configurable)
			reclaimMB := uint64(float64(vs.reclaimable) * 0.7)

			// Minimum reclaim: at least 64 MB to avoid tiny adjustments
			if reclaimMB < 64 {
				if vs.reclaimable >= 64 {
					reclaimMB = 64
				} else {
					continue // Skip if too small
				}
			}

			newBalloonMB := vs.currentBalloon + reclaimMB

			// Apply per-VM max balloon cap (respects autoscale min_memory)
			if newBalloonMB > vs.maxBalloonMB {
				newBalloonMB = vs.maxBalloonMB
			}

			// Extra safety: ensure we don't leave less than safety buffer
			minGuestMB := vs.safetyMB
			if vs.vmTotalMB-newBalloonMB < minGuestMB {
				newBalloonMB = vs.vmTotalMB - minGuestMB
			}

			if newBalloonMB > vs.currentBalloon+32 { // Only if meaningful change
				reason := "hostLow"
				if !hostNeedsMemory {
					reason = "idleReclaim"
				}
				// Show max balloon source in log
				maxSource := fmt.Sprintf("%.0f%%", abm.maxBalloonPercent*100)
				if vs.vm.Autoscale != nil && vs.vm.Autoscale.MinMemoryMB > 0 {
					maxSource = fmt.Sprintf("autoscale:min=%dMB", vs.vm.Autoscale.MinMemoryMB)
				}
				log.Printf("🎈 VM %s: inflate %d -> %d MB (free=%d MB, safety=%d MB, reason=%s, maxBalloon=%d MB [%s])",
					vs.vm.ID, vs.currentBalloon, newBalloonMB, vs.vmFreeMB, vs.safetyMB, reason, vs.maxBalloonMB, maxSource)
				if err := abm.setBalloonSize(vs.socketPath, newBalloonMB); err != nil {
					log.Printf("🎈 Error inflating balloon for VM %s: %v", vs.vm.ID, err)
					metrics.IncCounter(metrics.BalloonErrors, map[string]string{"vm_id": vs.vm.ID})
				} else {
					abm.setBallooned(vs.vm.ID, newBalloonMB > 0)
				}
			}
		} else if vs.currentBalloon > vs.maxBalloonMB {
			// AUTOSCALE CAP: Balloon exceeds max allowed (e.g., autoscale min_memory set after inflation)
			// Deflate to maxBalloonMB to respect customer's paid RAM
			maxSource := fmt.Sprintf("%.0f%%", abm.maxBalloonPercent*100)
			if vs.vm.Autoscale != nil && vs.vm.Autoscale.MinMemoryMB > 0 {
				maxSource = fmt.Sprintf("autoscale:min=%dMB", vs.vm.Autoscale.MinMemoryMB)
			}
			log.Printf("🎈 VM %s: autoscale-cap deflate %d -> %d MB (exceeds maxBalloon=%d MB [%s])",
				vs.vm.ID, vs.currentBalloon, vs.maxBalloonMB, vs.maxBalloonMB, maxSource)
			if err := abm.setBalloonSize(vs.socketPath, vs.maxBalloonMB); err != nil {
				log.Printf("🎈 Error deflating balloon for VM %s: %v", vs.vm.ID, err)
				metrics.IncCounter(metrics.BalloonErrors, map[string]string{"vm_id": vs.vm.ID})
			} else {
				abm.setBallooned(vs.vm.ID, vs.maxBalloonMB > 0)
			}
		} else if vs.currentBalloon > 0 && !hostNeedsMemory {
			// RELEASE MODE: Deflate balloon if guest needs memory AND host is OK

			if vs.vmFreeMB < vs.safetyMB {
				// URGENT: Guest below safety buffer - deflate immediately
				deflateMB := vs.safetyMB - vs.vmFreeMB + 64 // Give back safety + 64 MB buffer
				newBalloonMB := int64(vs.currentBalloon) - int64(deflateMB)
				if newBalloonMB < 0 {
					newBalloonMB = 0
				}

				log.Printf("🎈 VM %s: URGENT deflate %d -> %d MB (free=%d MB < safety %d MB)",
					vs.vm.ID, vs.currentBalloon, newBalloonMB, vs.vmFreeMB, vs.safetyMB)
				if err := abm.setBalloonSize(vs.socketPath, uint64(newBalloonMB)); err != nil {
					log.Printf("🎈 Error deflating balloon for VM %s: %v", vs.vm.ID, err)
					metrics.IncCounter(metrics.BalloonErrors, map[string]string{"vm_id": vs.vm.ID})
				} else {
					abm.setBallooned(vs.vm.ID, newBalloonMB > 0)
				}
			} else if vs.vmFreeMB < vs.safetyMB*2 && vs.currentBalloon > 0 {
				deflateMB := vs.currentBalloon / 4
				if deflateMB < 64 {
					deflateMB = 64
				}
				if deflateMB > vs.currentBalloon {
					deflateMB = vs.currentBalloon
				}
				if deflateMB == 0 {
					continue
				}
				newBalloonMB := vs.currentBalloon - deflateMB

				log.Printf("🎈 VM %s: gradual deflate %d -> %d MB (free=%d MB, safety=%d MB)",
					vs.vm.ID, vs.currentBalloon, newBalloonMB, vs.vmFreeMB, vs.safetyMB)
				if err := abm.setBalloonSize(vs.socketPath, newBalloonMB); err != nil {
					log.Printf("🎈 Error deflating balloon for VM %s: %v", vs.vm.ID, err)
					metrics.IncCounter(metrics.BalloonErrors, map[string]string{"vm_id": vs.vm.ID})
				} else {
					abm.setBallooned(vs.vm.ID, newBalloonMB > 0)
				}
			}
			// If guest has plenty of free memory (>2x safety), keep balloon as-is
		}
	}
}

// computeSafetyBufferMB returns the safety buffer in MB for a VM total memory.
// If a fixed safety buffer is configured (abm.vmSafetyBufferMB > 0), it is used.
// Otherwise, an adaptive value is computed as:
//
//	max(512MB, min(40% of VM RAM, 2048MB))
//
// This is more conservative than the old 25%/1024MB to prevent OOM issues.
func (abm *AutoBalloonManager) computeSafetyBufferMB(vmTotalMB uint64) uint64 {
	if abm.vmSafetyBufferMB > 0 {
		return abm.vmSafetyBufferMB
	}
	// Adaptive: 40% of VM total, clamped between 512 and 2048 MB
	// More conservative than old 25%/1024 to prevent OOM
	buf := vmTotalMB * 40 / 100
	if buf < 512 {
		buf = 512
	}
	if buf > 2048 {
		buf = 2048
	}
	return buf
}

// safetyLabel returns a human-readable description of safety buffer mode.
func safetyLabel(cfgValue uint64) string {
	if cfgValue > 0 {
		return fmt.Sprintf("fixed=%dMB", cfgValue)
	}
	return "adaptive"
}

// BalloonStats represents balloon device statistics
type BalloonStats struct {
	TargetMib   uint64 `json:"target_mib"`
	ActualMib   uint64 `json:"actual_mib"`
	SwapIn      uint64 `json:"swap_in"`
	SwapOut     uint64 `json:"swap_out"`
	MajorFaults uint64 `json:"major_faults"`
	MinorFaults uint64 `json:"minor_faults"`
	FreeMemory  uint64 `json:"free_memory"`
	TotalMemory uint64 `json:"total_memory"`
}

// getOrCreateClient gets or creates a reusable HTTP client for a socket
func (abm *AutoBalloonManager) getOrCreateClient(socketPath string) *http.Client {
	abm.clientPoolMutex.Lock()
	defer abm.clientPoolMutex.Unlock()

	if client, exists := abm.clientPool[socketPath]; exists {
		return client
	}

	// Create a new client with connection pooling
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   2,
			MaxConnsPerHost:       5,
			IdleConnTimeout:       60 * time.Second,
			DisableKeepAlives:     false,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 2 * time.Second,
	}

	abm.clientPool[socketPath] = client
	return client
}

// getBalloonStats gets balloon statistics from Firecracker socket
func (abm *AutoBalloonManager) getBalloonStats(socketPath string) (*BalloonStats, error) {
	client := abm.getOrCreateClient(socketPath)

	resp, err := client.Get("http://localhost/balloon/statistics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var stats BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}

	return &stats, nil
}

// setBalloonSize adjusts the balloon size via Firecracker API
func (abm *AutoBalloonManager) setBalloonSize(socketPath string, sizeMB uint64) error {
	client := abm.getOrCreateClient(socketPath)

	payload := fmt.Sprintf(`{"amount_mib": %d}`, sizeMB)
	req, err := http.NewRequest("PATCH", "http://localhost/balloon", bytes.NewBufferString(payload))
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

// Legacy functions for backward compatibility (will be removed)
func getBalloonStats(socketPath string) (*BalloonStats, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true, // Close connection after request to prevent connection leaks
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
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

	var stats BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}

	return &stats, nil
}

// setBalloonSize adjusts the balloon size via Firecracker API
func setBalloonSize(socketPath string, sizeMB uint64) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true, // Close connection after request to prevent connection leaks
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}
	defer transport.CloseIdleConnections()

	payload := fmt.Sprintf(`{"amount_mib": %d}`, sizeMB)
	req, err := http.NewRequest("PATCH", "http://localhost/balloon", bytes.NewBufferString(payload))
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

// getHostFreeMemoryMB returns free memory on host in MB
func getHostFreeMemoryMB() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			var val uint64
			fmt.Sscanf(line, "MemAvailable: %d kB", &val)
			return val / 1024, nil
		}
	}

	return 0, fmt.Errorf("MemAvailable not found")
}
