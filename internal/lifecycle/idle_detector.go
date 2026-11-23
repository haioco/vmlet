package lifecycle

import (
	"fmt"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// IdleStatus represents the idle status of a microVM
type IdleStatus struct {
	MicroVMID        string    `json:"microvm_id"`
	IsIdle           bool      `json:"is_idle"`
	IdleSince        time.Time `json:"idle_since,omitempty"`
	LastActivity     time.Time `json:"last_activity"`
	IdleDuration     time.Duration `json:"idle_duration"`
	ActivityScore    float64   `json:"activity_score"`
	NotificationSent bool      `json:"notification_sent"`
}

// ActivityThresholds defines thresholds for determining activity
type ActivityThresholds struct {
	CPUUsageThreshold     float64 `json:"cpu_usage_threshold"`
	MemoryUsageThreshold  float64 `json:"memory_usage_threshold"`
	NetworkRxThreshold    uint64  `json:"network_rx_threshold"`
	NetworkTxThreshold    uint64  `json:"network_tx_threshold"`
	IOReadThreshold       uint64  `json:"io_read_threshold"`
	IOWriteThreshold      uint64  `json:"io_write_threshold"`
}

// DefaultActivityThresholds returns default thresholds for activity detection
func DefaultActivityThresholds() ActivityThresholds {
	return ActivityThresholds{
		CPUUsageThreshold:    5.0,     // 5% CPU usage
		MemoryUsageThreshold: 10.0,    // 10% memory usage change
		NetworkRxThreshold:   1024,    // 1KB/s network receive
		NetworkTxThreshold:   1024,    // 1KB/s network transmit
		IOReadThreshold:      1024,    // 1KB/s disk read
		IOWriteThreshold:     1024,    // 1KB/s disk write
	}
}

// IdleDetector detects idle microVMs for cost optimization
type IdleDetector struct {
	config     *Config
	thresholds ActivityThresholds
	
	mu          sync.RWMutex
	idleStatus  map[string]*IdleStatus
	lastMetrics map[string]*MicroVMMetrics
}

// NewIdleDetector creates a new idle detector
func NewIdleDetector(config *Config) *IdleDetector {
	return &IdleDetector{
		config:      config,
		thresholds:  DefaultActivityThresholds(),
		idleStatus:  make(map[string]*IdleStatus),
		lastMetrics: make(map[string]*MicroVMMetrics),
	}
}

// IsIdle checks if a microVM is currently idle
func (id *IdleDetector) IsIdle(microvm *models.MicroVMInstance) bool {
	if microvm == nil {
		return false
	}

	id.mu.RLock()
	defer id.mu.RUnlock()

	status, exists := id.idleStatus[microvm.ID]
	if !exists {
		return false
	}

	return status.IsIdle && time.Since(status.IdleSince) >= id.config.IdleThreshold
}

// UpdateActivity updates activity information for a microVM
func (id *IdleDetector) UpdateActivity(microvm *models.MicroVMInstance, metrics *MicroVMMetrics) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}
	if metrics == nil {
		return fmt.Errorf("metrics cannot be nil")
	}

	id.mu.Lock()
	defer id.mu.Unlock()

	// Get or create idle status
	status, exists := id.idleStatus[microvm.ID]
	if !exists {
		status = &IdleStatus{
			MicroVMID:    microvm.ID,
			LastActivity: microvm.Metadata.LastActivity,
		}
		id.idleStatus[microvm.ID] = status
	}

	// Calculate activity score based on current metrics
	activityScore := id.calculateActivityScore(metrics)
	status.ActivityScore = activityScore

	// Determine if microVM is currently active
	isActive := id.isCurrentlyActive(metrics)

	if isActive {
		// MicroVM is active
		status.IsIdle = false
		status.LastActivity = time.Now()
		status.IdleSince = time.Time{}
		status.IdleDuration = 0
		status.NotificationSent = false
	} else {
		// MicroVM appears inactive
		if !status.IsIdle {
			// Just became idle
			status.IsIdle = true
			status.IdleSince = time.Now()
		}
		
		// Update idle duration
		if !status.IdleSince.IsZero() {
			status.IdleDuration = time.Since(status.IdleSince)
		}
	}

	// Store current metrics for next comparison
	id.lastMetrics[microvm.ID] = metrics

	return nil
}

// GetIdleStatus returns the idle status for a microVM
func (id *IdleDetector) GetIdleStatus(microvmID string) (*IdleStatus, error) {
	id.mu.RLock()
	defer id.mu.RUnlock()

	status, exists := id.idleStatus[microvmID]
	if !exists {
		return nil, fmt.Errorf("no idle status found for microVM %s", microvmID)
	}

	// Return a copy to avoid race conditions
	statusCopy := *status
	return &statusCopy, nil
}

// GetIdleMicroVMs returns a list of microVMs that have been idle longer than the threshold
func (id *IdleDetector) GetIdleMicroVMs() []string {
	id.mu.RLock()
	defer id.mu.RUnlock()

	var idleMicroVMs []string
	threshold := id.config.IdleThreshold

	for microvmID, status := range id.idleStatus {
		if status.IsIdle && !status.IdleSince.IsZero() && time.Since(status.IdleSince) >= threshold {
			idleMicroVMs = append(idleMicroVMs, microvmID)
		}
	}

	return idleMicroVMs
}

// MarkNotificationSent marks that an idle notification has been sent for a microVM
func (id *IdleDetector) MarkNotificationSent(microvmID string) error {
	id.mu.Lock()
	defer id.mu.Unlock()

	status, exists := id.idleStatus[microvmID]
	if !exists {
		return fmt.Errorf("no idle status found for microVM %s", microvmID)
	}

	status.NotificationSent = true
	return nil
}

// RemoveIdleStatus removes idle status tracking for a microVM
func (id *IdleDetector) RemoveIdleStatus(microvmID string) {
	id.mu.Lock()
	defer id.mu.Unlock()

	delete(id.idleStatus, microvmID)
	delete(id.lastMetrics, microvmID)
}

// calculateActivityScore calculates an activity score based on current metrics
func (id *IdleDetector) calculateActivityScore(metrics *MicroVMMetrics) float64 {
	score := 0.0

	// CPU activity (weight: 30%)
	if metrics.CPUMetrics.UsagePercent > id.thresholds.CPUUsageThreshold {
		score += 0.3 * (metrics.CPUMetrics.UsagePercent / 100.0)
	}

	// Memory activity (weight: 20%)
	if metrics.MemoryMetrics.UsagePercent > id.thresholds.MemoryUsageThreshold {
		score += 0.2 * (metrics.MemoryMetrics.UsagePercent / 100.0)
	}

	// Network activity (weight: 25%)
	networkActivity := float64(metrics.NetworkMetrics.RxBytesPerSec + metrics.NetworkMetrics.TxBytesPerSec)
	networkThreshold := float64(id.thresholds.NetworkRxThreshold + id.thresholds.NetworkTxThreshold)
	if networkActivity > networkThreshold {
		score += 0.25 * min(networkActivity/networkThreshold, 1.0)
	}

	// I/O activity (weight: 25%)
	ioActivity := float64(metrics.IOMetrics.ReadBytesPerSec + metrics.IOMetrics.WriteBytesPerSec)
	ioThreshold := float64(id.thresholds.IOReadThreshold + id.thresholds.IOWriteThreshold)
	if ioActivity > ioThreshold {
		score += 0.25 * min(ioActivity/ioThreshold, 1.0)
	}

	return min(score, 1.0) // Cap at 1.0
}

// isCurrentlyActive determines if a microVM is currently active based on metrics
func (id *IdleDetector) isCurrentlyActive(metrics *MicroVMMetrics) bool {
	// Check CPU activity
	if metrics.CPUMetrics.UsagePercent > id.thresholds.CPUUsageThreshold {
		return true
	}

	// Check network activity
	if metrics.NetworkMetrics.RxBytesPerSec > id.thresholds.NetworkRxThreshold ||
		metrics.NetworkMetrics.TxBytesPerSec > id.thresholds.NetworkTxThreshold {
		return true
	}

	// Check I/O activity
	if metrics.IOMetrics.ReadBytesPerSec > id.thresholds.IOReadThreshold ||
		metrics.IOMetrics.WriteBytesPerSec > id.thresholds.IOWriteThreshold {
		return true
	}

	// Check for significant memory usage changes (compared to previous metrics)
	if lastMetrics, exists := id.lastMetrics[metrics.MicroVMID]; exists {
		memoryChange := abs(metrics.MemoryMetrics.UsagePercent - lastMetrics.MemoryMetrics.UsagePercent)
		if memoryChange > id.thresholds.MemoryUsageThreshold {
			return true
		}
	}

	return false
}

// SetActivityThresholds updates the activity thresholds
func (id *IdleDetector) SetActivityThresholds(thresholds ActivityThresholds) {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.thresholds = thresholds
}

// GetActivityThresholds returns the current activity thresholds
func (id *IdleDetector) GetActivityThresholds() ActivityThresholds {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return id.thresholds
}

// GetIdleStatistics returns statistics about idle microVMs
func (id *IdleDetector) GetIdleStatistics() map[string]interface{} {
	id.mu.RLock()
	defer id.mu.RUnlock()

	totalMicroVMs := len(id.idleStatus)
	idleCount := 0
	var totalIdleDuration time.Duration
	var longestIdleDuration time.Duration

	for _, status := range id.idleStatus {
		if status.IsIdle {
			idleCount++
			if !status.IdleSince.IsZero() {
				idleDuration := time.Since(status.IdleSince)
				totalIdleDuration += idleDuration
				if idleDuration > longestIdleDuration {
					longestIdleDuration = idleDuration
				}
			}
		}
	}

	var averageIdleDuration time.Duration
	if idleCount > 0 {
		averageIdleDuration = totalIdleDuration / time.Duration(idleCount)
	}

	return map[string]interface{}{
		"total_microvms":         totalMicroVMs,
		"idle_count":            idleCount,
		"active_count":          totalMicroVMs - idleCount,
		"idle_percentage":       float64(idleCount) / float64(totalMicroVMs) * 100,
		"average_idle_duration": averageIdleDuration.String(),
		"longest_idle_duration": longestIdleDuration.String(),
	}
}

// Helper functions

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}