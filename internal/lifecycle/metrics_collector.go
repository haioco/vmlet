package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// MicroVMMetrics represents performance metrics for a microVM
type MicroVMMetrics struct {
	MicroVMID     string                 `json:"microvm_id"`
	Timestamp     time.Time              `json:"timestamp"`
	CPUMetrics    CPUMetrics             `json:"cpu_metrics"`
	MemoryMetrics MemoryMetrics          `json:"memory_metrics"`
	IOMetrics     IOMetrics              `json:"io_metrics"`
	NetworkMetrics NetworkMetrics        `json:"network_metrics"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
}

// CPUMetrics represents CPU usage metrics
type CPUMetrics struct {
	UsagePercent    float64 `json:"usage_percent"`
	UserPercent     float64 `json:"user_percent"`
	SystemPercent   float64 `json:"system_percent"`
	IdlePercent     float64 `json:"idle_percent"`
	LoadAverage1m   float64 `json:"load_average_1m"`
	LoadAverage5m   float64 `json:"load_average_5m"`
	LoadAverage15m  float64 `json:"load_average_15m"`
}

// MemoryMetrics represents memory usage metrics
type MemoryMetrics struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsagePercent   float64 `json:"usage_percent"`
	CachedBytes    uint64  `json:"cached_bytes"`
	BuffersBytes   uint64  `json:"buffers_bytes"`
	SwapTotalBytes uint64  `json:"swap_total_bytes"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes"`
}

// IOMetrics represents I/O usage metrics
type IOMetrics struct {
	ReadBytesPerSec  uint64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec uint64 `json:"write_bytes_per_sec"`
	ReadOpsPerSec    uint64 `json:"read_ops_per_sec"`
	WriteOpsPerSec   uint64 `json:"write_ops_per_sec"`
	IOWaitPercent    float64 `json:"io_wait_percent"`
}

// NetworkMetrics represents network usage metrics
type NetworkMetrics struct {
	RxBytesPerSec   uint64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec   uint64 `json:"tx_bytes_per_sec"`
	RxPacketsPerSec uint64 `json:"rx_packets_per_sec"`
	TxPacketsPerSec uint64 `json:"tx_packets_per_sec"`
	RxErrors        uint64 `json:"rx_errors"`
	TxErrors        uint64 `json:"tx_errors"`
}

// MetricsCollector collects performance metrics for microVMs
type MetricsCollector struct {
	config *Config
	
	mu      sync.RWMutex
	metrics map[string][]*MicroVMMetrics
	
	// Previous metrics for calculating rates
	previousMetrics map[string]*MicroVMMetrics
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(config *Config) *MetricsCollector {
	return &MetricsCollector{
		config:          config,
		metrics:         make(map[string][]*MicroVMMetrics),
		previousMetrics: make(map[string]*MicroVMMetrics),
	}
}

// CollectMetrics collects current metrics for a microVM
func (mc *MetricsCollector) CollectMetrics(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	metrics := &MicroVMMetrics{
		MicroVMID: microvm.ID,
		Timestamp: time.Now(),
	}

	// Collect CPU metrics
	cpuMetrics, err := mc.collectCPUMetrics(microvm)
	if err != nil {
		return fmt.Errorf("failed to collect CPU metrics: %w", err)
	}
	metrics.CPUMetrics = cpuMetrics

	// Collect memory metrics
	memoryMetrics, err := mc.collectMemoryMetrics(microvm)
	if err != nil {
		return fmt.Errorf("failed to collect memory metrics: %w", err)
	}
	metrics.MemoryMetrics = memoryMetrics

	// Collect I/O metrics
	ioMetrics, err := mc.collectIOMetrics(microvm)
	if err != nil {
		return fmt.Errorf("failed to collect I/O metrics: %w", err)
	}
	metrics.IOMetrics = ioMetrics

	// Collect network metrics
	networkMetrics, err := mc.collectNetworkMetrics(microvm)
	if err != nil {
		return fmt.Errorf("failed to collect network metrics: %w", err)
	}
	metrics.NetworkMetrics = networkMetrics

	// Calculate uptime
	metrics.UptimeSeconds = int64(time.Since(microvm.Metadata.CreatedAt).Seconds())

	// Store metrics
	mc.storeMetrics(microvm.ID, metrics)

	return nil
}

// GetMetrics returns the latest metrics for a microVM
func (mc *MetricsCollector) GetMetrics(microvmID string) (*MicroVMMetrics, error) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	metricsList, exists := mc.metrics[microvmID]
	if !exists || len(metricsList) == 0 {
		return nil, fmt.Errorf("no metrics found for microVM %s", microvmID)
	}

	// Return the latest metrics
	return metricsList[len(metricsList)-1], nil
}

// GetMetricsHistory returns historical metrics for a microVM
func (mc *MetricsCollector) GetMetricsHistory(microvmID string, since time.Time) ([]*MicroVMMetrics, error) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	metricsList, exists := mc.metrics[microvmID]
	if !exists {
		return nil, fmt.Errorf("no metrics found for microVM %s", microvmID)
	}

	var filteredMetrics []*MicroVMMetrics
	for _, metrics := range metricsList {
		if metrics.Timestamp.After(since) {
			filteredMetrics = append(filteredMetrics, metrics)
		}
	}

	return filteredMetrics, nil
}

// RemoveMetrics removes metrics tracking for a microVM
func (mc *MetricsCollector) RemoveMetrics(microvmID string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	delete(mc.metrics, microvmID)
	delete(mc.previousMetrics, microvmID)
}

// storeMetrics stores metrics and manages retention
func (mc *MetricsCollector) storeMetrics(microvmID string, metrics *MicroVMMetrics) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Add new metrics
	mc.metrics[microvmID] = append(mc.metrics[microvmID], metrics)
	mc.previousMetrics[microvmID] = metrics

	// Clean up old metrics based on retention period
	cutoff := time.Now().Add(-mc.config.MetricsRetentionPeriod)
	var filteredMetrics []*MicroVMMetrics
	
	for _, m := range mc.metrics[microvmID] {
		if m.Timestamp.After(cutoff) {
			filteredMetrics = append(filteredMetrics, m)
		}
	}
	
	mc.metrics[microvmID] = filteredMetrics
}

// collectCPUMetrics collects CPU usage metrics from cgroups
func (mc *MetricsCollector) collectCPUMetrics(microvm *models.MicroVMInstance) (CPUMetrics, error) {
	var metrics CPUMetrics

	// Try to read from cgroups (this is a simplified implementation)
	// In a real implementation, this would read from the actual cgroup paths
	// for the firecracker process
	
	// For now, simulate some metrics
	// TODO: Implement actual cgroup reading
	metrics.UsagePercent = 15.5  // Simulated CPU usage
	metrics.UserPercent = 10.2
	metrics.SystemPercent = 5.3
	metrics.IdlePercent = 84.5
	metrics.LoadAverage1m = 0.5
	metrics.LoadAverage5m = 0.3
	metrics.LoadAverage15m = 0.2

	return metrics, nil
}

// collectMemoryMetrics collects memory usage metrics from cgroups
func (mc *MetricsCollector) collectMemoryMetrics(microvm *models.MicroVMInstance) (MemoryMetrics, error) {
	var metrics MemoryMetrics

	// Try to read from cgroups memory statistics
	// This is a simplified implementation
	// TODO: Implement actual cgroup memory reading
	
	totalMB := microvm.Resources.MemoryMB
	metrics.TotalBytes = uint64(totalMB) * 1024 * 1024
	metrics.UsedBytes = uint64(float64(metrics.TotalBytes) * 0.6) // Simulate 60% usage
	metrics.FreeBytes = metrics.TotalBytes - metrics.UsedBytes
	metrics.AvailableBytes = metrics.FreeBytes
	metrics.UsagePercent = float64(metrics.UsedBytes) / float64(metrics.TotalBytes) * 100
	metrics.CachedBytes = uint64(float64(metrics.TotalBytes) * 0.1)
	metrics.BuffersBytes = uint64(float64(metrics.TotalBytes) * 0.05)

	return metrics, nil
}

// collectIOMetrics collects I/O usage metrics
func (mc *MetricsCollector) collectIOMetrics(microvm *models.MicroVMInstance) (IOMetrics, error) {
	var metrics IOMetrics

	// Try to read from /proc/diskstats or cgroup blkio statistics
	// This is a simplified implementation
	// TODO: Implement actual I/O metrics collection
	
	// Simulate some I/O metrics
	metrics.ReadBytesPerSec = 1024 * 100   // 100 KB/s read
	metrics.WriteBytesPerSec = 1024 * 50   // 50 KB/s write
	metrics.ReadOpsPerSec = 10
	metrics.WriteOpsPerSec = 5
	metrics.IOWaitPercent = 2.5

	return metrics, nil
}

// collectNetworkMetrics collects network usage metrics
func (mc *MetricsCollector) collectNetworkMetrics(microvm *models.MicroVMInstance) (NetworkMetrics, error) {
	var metrics NetworkMetrics

	// Try to read from /proc/net/dev or network namespace statistics
	// This is a simplified implementation
	// TODO: Implement actual network metrics collection
	
	// Simulate some network metrics
	metrics.RxBytesPerSec = 1024 * 20    // 20 KB/s received
	metrics.TxBytesPerSec = 1024 * 15    // 15 KB/s transmitted
	metrics.RxPacketsPerSec = 25
	metrics.TxPacketsPerSec = 20
	metrics.RxErrors = 0
	metrics.TxErrors = 0

	return metrics, nil
}

// Helper functions for reading system metrics (simplified implementations)

// readCgroupFile reads a value from a cgroup file
func readCgroupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// parseCgroupMemoryStats parses memory statistics from cgroup memory.stat
func parseCgroupMemoryStats(content string) map[string]uint64 {
	stats := make(map[string]uint64)
	lines := strings.Split(content, "\n")
	
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				stats[fields[0]] = value
			}
		}
	}
	
	return stats
}

// getCgroupPath returns the cgroup path for a microVM
func getCgroupPath(microvmID string, subsystem string) string {
	// This would need to be implemented based on how firecracker
	// organizes its cgroups
	return filepath.Join("/sys/fs/cgroup", subsystem, "firecracker", microvmID)
}

// GetAverageMetrics calculates average metrics over a time period
func (mc *MetricsCollector) GetAverageMetrics(microvmID string, duration time.Duration) (*MicroVMMetrics, error) {
	since := time.Now().Add(-duration)
	history, err := mc.GetMetricsHistory(microvmID, since)
	if err != nil {
		return nil, err
	}

	if len(history) == 0 {
		return nil, fmt.Errorf("no metrics available for averaging")
	}

	// Calculate averages
	avg := &MicroVMMetrics{
		MicroVMID: microvmID,
		Timestamp: time.Now(),
	}

	var totalCPU, totalMemUsage, totalIORead, totalIOWrite, totalNetRx, totalNetTx float64
	count := float64(len(history))

	for _, m := range history {
		totalCPU += m.CPUMetrics.UsagePercent
		totalMemUsage += m.MemoryMetrics.UsagePercent
		totalIORead += float64(m.IOMetrics.ReadBytesPerSec)
		totalIOWrite += float64(m.IOMetrics.WriteBytesPerSec)
		totalNetRx += float64(m.NetworkMetrics.RxBytesPerSec)
		totalNetTx += float64(m.NetworkMetrics.TxBytesPerSec)
	}

	avg.CPUMetrics.UsagePercent = totalCPU / count
	avg.MemoryMetrics.UsagePercent = totalMemUsage / count
	avg.IOMetrics.ReadBytesPerSec = uint64(totalIORead / count)
	avg.IOMetrics.WriteBytesPerSec = uint64(totalIOWrite / count)
	avg.NetworkMetrics.RxBytesPerSec = uint64(totalNetRx / count)
	avg.NetworkMetrics.TxBytesPerSec = uint64(totalNetTx / count)

	// Use the latest metrics for other fields
	latest := history[len(history)-1]
	avg.MemoryMetrics.TotalBytes = latest.MemoryMetrics.TotalBytes
	avg.MemoryMetrics.UsedBytes = uint64(float64(latest.MemoryMetrics.TotalBytes) * avg.MemoryMetrics.UsagePercent / 100)
	avg.MemoryMetrics.FreeBytes = avg.MemoryMetrics.TotalBytes - avg.MemoryMetrics.UsedBytes
	avg.UptimeSeconds = latest.UptimeSeconds

	return avg, nil
}