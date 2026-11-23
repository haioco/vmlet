package autoscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// VMAutoscaler handles per-VM memory autoscaling based on usage metrics
type VMAutoscaler struct {
	vmGetter        VMGetter
	checkInterval   time.Duration
	mutex           sync.RWMutex
	stopChan        chan struct{}
	clientPool      map[string]*http.Client
	clientPoolMutex sync.Mutex
}

// VMGetter interface to get VMs without circular dependency
type VMGetter interface {
	GetAllVMs() []*models.MicroVMInstance
	GetVM(id string) (*models.MicroVMInstance, error)
}

// BalloonStats represents balloon device statistics from Firecracker
type BalloonStats struct {
	TargetMib   uint64 `json:"target_mib"`
	ActualMib   uint64 `json:"actual_mib"`
	TargetPages uint64 `json:"target_pages"`
	ActualPages uint64 `json:"actual_pages"`
	SwapIn      uint64 `json:"swap_in"`
	SwapOut     uint64 `json:"swap_out"`
	MajorFaults uint64 `json:"major_faults"`
	MinorFaults uint64 `json:"minor_faults"`
	FreeMemory  uint64 `json:"free_memory"`
	TotalMemory uint64 `json:"total_memory"`
	Available   uint64 `json:"available_memory"`
	Caches      uint64 `json:"caches"`
}

// NewVMAutoscaler creates a new per-VM autoscaler
func NewVMAutoscaler(vmGetter VMGetter, checkInterval time.Duration) *VMAutoscaler {
	if checkInterval == 0 {
		checkInterval = 10 * time.Second // Default: check every 10 seconds
	}

	return &VMAutoscaler{
		vmGetter:      vmGetter,
		checkInterval: checkInterval,
		stopChan:      make(chan struct{}),
		clientPool:    make(map[string]*http.Client),
	}
}

// Start begins the autoscaling loop
func (va *VMAutoscaler) Start() {
	log.Printf("🔄 Per-VM autoscaler: starting (check interval: %v)", va.checkInterval)
	go va.run()
}

// Stop stops the autoscaling loop
func (va *VMAutoscaler) Stop() {
	close(va.stopChan)

	// Clean up HTTP clients
	va.clientPoolMutex.Lock()
	for k, c := range va.clientPool {
		if c != nil {
			c.CloseIdleConnections()
		}
		delete(va.clientPool, k)
	}
	va.clientPoolMutex.Unlock()

	log.Println("🔄 Per-VM autoscaler: stopped")
}

// run is the main autoscaling loop
func (va *VMAutoscaler) run() {
	ticker := time.NewTicker(va.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-va.stopChan:
			return
		case <-ticker.C:
			va.checkAndScaleVMs()
		}
	}
}

// checkAndScaleVMs checks all VMs with autoscaling enabled and scales them if needed
func (va *VMAutoscaler) checkAndScaleVMs() {
	vms := va.vmGetter.GetAllVMs()
	if len(vms) == 0 {
		return
	}

	for _, vm := range vms {
		// Skip VMs that are not running
		if vm.Status != models.MicroVMStatusRunning {
			continue
		}

		// Skip VMs without autoscaling enabled
		if vm.Autoscale == nil || !vm.Autoscale.Enabled {
			continue
		}

		// Check if we're in cooldown period
		if time.Since(vm.Autoscale.LastScaleAction) < time.Duration(vm.Autoscale.CooldownSeconds)*time.Second {
			continue
		}

		// Get socket path
		socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)

		// Get balloon statistics
		stats, err := va.getBalloonStats(socketPath)
		if err != nil {
			log.Printf("🔄 VM %s: failed to get balloon stats: %v", vm.ID, err)
			continue
		}

		// Calculate memory usage percentage
		if stats.TotalMemory == 0 {
			continue
		}

		usedMemory := stats.TotalMemory - stats.FreeMemory
		usagePercent := float64(usedMemory) / float64(stats.TotalMemory)

		currentMemoryMB := (stats.TotalMemory - uint64(stats.ActualMib)*1024*1024) / (1024 * 1024)

		log.Printf("🔄 VM %s: memory usage %.1f%% (%d/%d MB), balloon: %d MB, available memory: %d MB",
			vm.ID, usagePercent*100, usedMemory/(1024*1024), stats.TotalMemory/(1024*1024),
			stats.ActualMib, currentMemoryMB)

		// Determine scaling action
		if usagePercent > vm.Autoscale.ScaleUpThreshold {
			// Scale UP - deflate balloon to give more memory to guest
			va.scaleUp(vm, stats, usagePercent)
		} else if usagePercent < vm.Autoscale.ScaleDownThreshold {
			// Scale DOWN - inflate balloon to reclaim memory from guest
			va.scaleDown(vm, stats, usagePercent)
		}
	}
}

// scaleUp increases VM memory by deflating the balloon
func (va *VMAutoscaler) scaleUp(vm *models.MicroVMInstance, stats *BalloonStats, usagePercent float64) {
	currentBalloonMB := stats.ActualMib
	if currentBalloonMB == 0 {
		log.Printf("🔄 VM %s: cannot scale up - balloon already at 0 MB (at max memory)", vm.ID)
		return
	}

	// Calculate available memory (total - balloon)
	availableMemoryMB := (stats.TotalMemory / (1024 * 1024)) - uint64(currentBalloonMB)

	// Check if we can scale up
	if availableMemoryMB >= uint64(vm.Autoscale.MaxMemoryMB) {
		log.Printf("🔄 VM %s: already at max memory %d MB", vm.ID, vm.Autoscale.MaxMemoryMB)
		return
	}

	// Calculate how much to deflate (reduce balloon)
	// Deflate 25% of current balloon size or enough to reach max, whichever is smaller
	deflateMB := currentBalloonMB / 4
	newAvailableMB := availableMemoryMB + uint64(deflateMB)

	if newAvailableMB > uint64(vm.Autoscale.MaxMemoryMB) {
		deflateMB = uint64(vm.Autoscale.MaxMemoryMB) - availableMemoryMB
		newAvailableMB = uint64(vm.Autoscale.MaxMemoryMB)
	}

	newBalloonMB := currentBalloonMB - deflateMB

	log.Printf("🔄 ⬆️  VM %s: SCALE UP - usage %.1f%% > threshold %.1f%%, deflating balloon %d -> %d MB (available memory: %d -> %d MB)",
		vm.ID, usagePercent*100, vm.Autoscale.ScaleUpThreshold*100,
		currentBalloonMB, newBalloonMB, availableMemoryMB, newAvailableMB)

	socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)
	if err := va.setBalloonSize(socketPath, newBalloonMB); err != nil {
		log.Printf("🔄 Error scaling up VM %s: %v", vm.ID, err)
		return
	}

	// Update last scale action time
	vm.Autoscale.LastScaleAction = time.Now()
	log.Printf("🔄 ✅ VM %s: scaled up successfully, cooldown for %d seconds", vm.ID, vm.Autoscale.CooldownSeconds)
}

// scaleDown decreases VM memory by inflating the balloon
func (va *VMAutoscaler) scaleDown(vm *models.MicroVMInstance, stats *BalloonStats, usagePercent float64) {
	currentBalloonMB := stats.ActualMib
	availableMemoryMB := (stats.TotalMemory / (1024 * 1024)) - uint64(currentBalloonMB)

	// Check if we're already at minimum memory
	if availableMemoryMB <= uint64(vm.Autoscale.MinMemoryMB) {
		log.Printf("🔄 VM %s: already at min memory %d MB", vm.ID, vm.Autoscale.MinMemoryMB)
		return
	}

	// Calculate how much to inflate (increase balloon)
	// Reclaim 25% of unused memory or enough to reach min, whichever is smaller
	freeMemoryMB := stats.FreeMemory / (1024 * 1024)
	inflateMB := freeMemoryMB / 4

	newAvailableMB := availableMemoryMB - inflateMB
	if newAvailableMB < uint64(vm.Autoscale.MinMemoryMB) {
		inflateMB = availableMemoryMB - uint64(vm.Autoscale.MinMemoryMB)
		newAvailableMB = uint64(vm.Autoscale.MinMemoryMB)
	}

	// Don't reclaim if inflation is too small (< 64 MB)
	if inflateMB < 64 {
		return
	}

	newBalloonMB := currentBalloonMB + inflateMB

	log.Printf("🔄 ⬇️  VM %s: SCALE DOWN - usage %.1f%% < threshold %.1f%%, inflating balloon %d -> %d MB (available memory: %d -> %d MB)",
		vm.ID, usagePercent*100, vm.Autoscale.ScaleDownThreshold*100,
		currentBalloonMB, newBalloonMB, availableMemoryMB, newAvailableMB)

	socketPath := fmt.Sprintf("/run/microvmsvc/%s.sock", vm.ID)
	if err := va.setBalloonSize(socketPath, newBalloonMB); err != nil {
		log.Printf("🔄 Error scaling down VM %s: %v", vm.ID, err)
		return
	}

	// Update last scale action time
	vm.Autoscale.LastScaleAction = time.Now()
	log.Printf("🔄 ✅ VM %s: scaled down successfully, cooldown for %d seconds", vm.ID, vm.Autoscale.CooldownSeconds)
}

// getOrCreateClient gets or creates a reusable HTTP client for a socket
func (va *VMAutoscaler) getOrCreateClient(socketPath string) *http.Client {
	va.clientPoolMutex.Lock()
	defer va.clientPoolMutex.Unlock()

	if client, exists := va.clientPool[socketPath]; exists {
		return client
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 2,
			MaxConnsPerHost:     5,
			IdleConnTimeout:     60 * time.Second,
			DisableKeepAlives:   false,
		},
		Timeout: 3 * time.Second,
	}

	va.clientPool[socketPath] = client
	return client
}

// getBalloonStats gets balloon statistics from Firecracker
func (va *VMAutoscaler) getBalloonStats(socketPath string) (*BalloonStats, error) {
	client := va.getOrCreateClient(socketPath)

	resp, err := client.Get("http://localhost/balloon/statistics")
	if err != nil {
		return nil, fmt.Errorf("failed to get balloon stats: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var stats BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("failed to decode stats: %w", err)
	}

	return &stats, nil
}

// setBalloonSize sets the balloon size via Firecracker API
func (va *VMAutoscaler) setBalloonSize(socketPath string, sizeMB uint64) error {
	client := va.getOrCreateClient(socketPath)

	payload := map[string]interface{}{
		"amount_mib": sizeMB,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("PATCH", "http://localhost/balloon", bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
