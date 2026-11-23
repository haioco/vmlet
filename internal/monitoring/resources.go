package monitoring

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ResourceStats represents resource usage statistics for a VM or host
type ResourceStats struct {
	// Process-level statistics
	PID             int     `json:"pid"`
	ProcessName     string  `json:"process_name"`
	CPUPercent      float64 `json:"cpu_percent"`
	MemoryRSSBytes  uint64  `json:"memory_rss_bytes"`
	MemoryRSSMB     float64 `json:"memory_rss_mb"`
	MemoryVirtBytes uint64  `json:"memory_virt_bytes"`
	MemoryVirtMB    float64 `json:"memory_virt_mb"`
	MemoryPercent   float64 `json:"memory_percent"`
	ThreadCount     int     `json:"thread_count"`
	FileDescriptors int     `json:"file_descriptors"`

	// VM configuration
	ConfiguredMemoryMB uint32 `json:"configured_memory_mb"`
	ConfiguredVCPUs    uint32 `json:"configured_vcpus"`

	// Ballooning statistics
	BalloonEnabled bool          `json:"balloon_enabled"`
	BalloonStats   *BalloonStats `json:"balloon_stats,omitempty"`

	// Host-level statistics
	HostMemoryTotal     uint64 `json:"host_memory_total_bytes,omitempty"`
	HostMemoryFree      uint64 `json:"host_memory_free_bytes,omitempty"`
	HostMemoryAvailable uint64 `json:"host_memory_available_bytes,omitempty"`

	// Timestamp
	Timestamp time.Time `json:"timestamp"`
}

// BalloonStats represents memory balloon device statistics
type BalloonStats struct {
	// Target and actual balloon sizes
	TargetMiB   uint32 `json:"target_mib"`
	ActualMiB   uint32 `json:"actual_mib"`
	TargetPages uint32 `json:"target_pages"`
	ActualPages uint32 `json:"actual_pages"`

	// Guest memory statistics
	SwapIn             *uint64 `json:"swap_in,omitempty"`
	SwapOut            *uint64 `json:"swap_out,omitempty"`
	MajorFaults        *uint64 `json:"major_faults,omitempty"`
	MinorFaults        *uint64 `json:"minor_faults,omitempty"`
	FreeMemory         *uint64 `json:"free_memory,omitempty"`
	TotalMemory        *uint64 `json:"total_memory,omitempty"`
	AvailableMemory    *uint64 `json:"available_memory,omitempty"`
	DiskCaches         *uint64 `json:"disk_caches,omitempty"`
	HugetlbAllocations *uint64 `json:"hugetlb_allocations,omitempty"`
	HugetlbFailures    *uint64 `json:"hugetlb_failures,omitempty"`
}

// HostResourceStats represents overall host resource statistics
type HostResourceStats struct {
	TotalMemoryMB     float64   `json:"total_memory_mb"`
	AvailableMemoryMB float64   `json:"available_memory_mb"`
	UsedMemoryMB      float64   `json:"used_memory_mb"`
	MemoryPercent     float64   `json:"memory_percent"`
	CPUCount          int       `json:"cpu_count"`
	LoadAverage1      float64   `json:"load_average_1"`
	LoadAverage5      float64   `json:"load_average_5"`
	LoadAverage15     float64   `json:"load_average_15"`
	Timestamp         time.Time `json:"timestamp"`
}

// ResourceMonitor provides resource monitoring capabilities
type ResourceMonitor struct {
	// Configuration
	updateInterval time.Duration
}

// NewResourceMonitor creates a new resource monitor
func NewResourceMonitor(updateInterval time.Duration) *ResourceMonitor {
	return &ResourceMonitor{
		updateInterval: updateInterval,
	}
}

// GetProcessResourceStats gets resource statistics for a specific process
func (rm *ResourceMonitor) GetProcessResourceStats(pid int) (*ResourceStats, error) {
	stats := &ResourceStats{
		PID:       pid,
		Timestamp: time.Now(),
	}

	// Read process name
	processName, err := readProcessName(pid)
	if err != nil {
		return nil, fmt.Errorf("failed to read process name: %w", err)
	}
	stats.ProcessName = processName

	// Read memory statistics from /proc/[pid]/status
	memStats, err := readProcessMemoryStats(pid)
	if err != nil {
		return nil, fmt.Errorf("failed to read memory stats: %w", err)
	}
	stats.MemoryRSSBytes = memStats.RSSBytes
	stats.MemoryRSSMB = float64(memStats.RSSBytes) / (1024 * 1024)
	stats.MemoryVirtBytes = memStats.VirtBytes
	stats.MemoryVirtMB = float64(memStats.VirtBytes) / (1024 * 1024)
	stats.ThreadCount = memStats.Threads

	// Read CPU statistics
	cpuPercent, err := readProcessCPUPercent(pid)
	if err != nil {
		// CPU percent is best effort, don't fail if unavailable
		cpuPercent = 0
	}
	stats.CPUPercent = cpuPercent

	// Read file descriptor count
	fdCount, err := countFileDescriptors(pid)
	if err != nil {
		// FD count is best effort
		fdCount = 0
	}
	stats.FileDescriptors = fdCount

	// Calculate memory percent based on host memory
	hostMem, err := readHostMemoryInfo()
	if err == nil && hostMem.TotalBytes > 0 {
		stats.MemoryPercent = (float64(stats.MemoryRSSBytes) / float64(hostMem.TotalBytes)) * 100
		stats.HostMemoryTotal = hostMem.TotalBytes
		stats.HostMemoryFree = hostMem.FreeBytes
		stats.HostMemoryAvailable = hostMem.AvailableBytes
	}

	return stats, nil
}

// GetVMResourceStats gets resource statistics for a VM including balloon info
func (rm *ResourceMonitor) GetVMResourceStats(pid int, socketPath string, configuredMemMB, configuredVCPUs uint32) (*ResourceStats, error) {
	// Get base process stats
	stats, err := rm.GetProcessResourceStats(pid)
	if err != nil {
		return nil, err
	}

	// Add VM configuration
	stats.ConfiguredMemoryMB = configuredMemMB
	stats.ConfiguredVCPUs = configuredVCPUs

	// Try to get balloon statistics
	if socketPath != "" {
		balloonStats, err := rm.GetBalloonStats(socketPath)
		if err == nil && balloonStats != nil {
			stats.BalloonEnabled = true
			stats.BalloonStats = balloonStats
		}
		// If error, BalloonEnabled stays false (default value)
	}

	return stats, nil
}

// GetBalloonStats retrieves balloon device statistics from Firecracker API
func (rm *ResourceMonitor) GetBalloonStats(socketPath string) (*BalloonStats, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("socket path is empty")
	}

	// Check if socket exists
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("socket does not exist: %s", socketPath)
	}

	// Create HTTP client with Unix socket transport
	// IMPORTANT: Use DisableKeepAlives to prevent connection leaks that cause
	// "Too many open connections" errors in Firecracker
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true, // Close connection after each request
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
	// Ensure connections are closed when done
	defer transport.CloseIdleConnections()

	// Try to get balloon statistics first (more reliable indicator of balloon presence)
	balloonStatsData, statsErr := getBalloonStatsData(client)
	if statsErr == nil && balloonStatsData != nil {
		// We got stats, balloon is definitely present
		return balloonStatsData, nil
	}

	// If stats failed, try config endpoint as fallback
	balloonConfig, configErr := getBalloonConfig(client)
	if configErr != nil {
		// Both endpoints failed, balloon not available
		return nil, fmt.Errorf("balloon not available: stats error: %v, config error: %v", statsErr, configErr)
	}

	// We have config but no stats (stats might not be enabled yet)
	return balloonConfig, nil
}

// GetHostResourceStats gets overall host resource statistics
func (rm *ResourceMonitor) GetHostResourceStats() (*HostResourceStats, error) {
	stats := &HostResourceStats{
		Timestamp: time.Now(),
	}

	// Get memory info
	memInfo, err := readHostMemoryInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to read memory info: %w", err)
	}
	stats.TotalMemoryMB = float64(memInfo.TotalBytes) / (1024 * 1024)
	stats.AvailableMemoryMB = float64(memInfo.AvailableBytes) / (1024 * 1024)
	stats.UsedMemoryMB = float64(memInfo.TotalBytes-memInfo.AvailableBytes) / (1024 * 1024)
	stats.MemoryPercent = (float64(memInfo.TotalBytes-memInfo.AvailableBytes) / float64(memInfo.TotalBytes)) * 100

	// Get CPU count
	cpuCount, err := readCPUCount()
	if err == nil {
		stats.CPUCount = cpuCount
	}

	// Get load average
	loadAvg, err := readLoadAverage()
	if err == nil {
		stats.LoadAverage1 = loadAvg[0]
		stats.LoadAverage5 = loadAvg[1]
		stats.LoadAverage15 = loadAvg[2]
	}

	return stats, nil
}

// Helper functions for reading /proc filesystem

type processMemoryStats struct {
	RSSBytes  uint64
	VirtBytes uint64
	Threads   int
}

func readProcessMemoryStats(pid int) (*processMemoryStats, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil, err
	}

	stats := &processMemoryStats{}
	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "VmRSS:":
			if len(fields) >= 3 {
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				stats.RSSBytes = val * 1024 // Convert KB to bytes
			}
		case "VmSize:":
			if len(fields) >= 3 {
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				stats.VirtBytes = val * 1024 // Convert KB to bytes
			}
		case "Threads:":
			val, _ := strconv.Atoi(fields[1])
			stats.Threads = val
		}
	}

	return stats, nil
}

func readProcessName(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readProcessCPUPercent(pid int) (float64, error) {
	// Read process CPU time
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}

	fields := strings.Fields(string(statData))
	if len(fields) < 22 {
		return 0, fmt.Errorf("invalid stat format")
	}

	// This is a simplified calculation - for accurate CPU%, need to track over time
	// Fields: utime(13), stime(14), starttime(21)
	utime, _ := strconv.ParseUint(fields[13], 10, 64)
	stime, _ := strconv.ParseUint(fields[14], 10, 64)

	totalTime := utime + stime

	// Read system uptime
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}

	uptimeFields := strings.Fields(string(uptimeData))
	if len(uptimeFields) < 1 {
		return 0, fmt.Errorf("invalid uptime format")
	}

	uptime, _ := strconv.ParseFloat(uptimeFields[0], 64)

	// Get clock ticks per second
	clockTicks := float64(100) // Usually 100 on Linux

	// Calculate CPU time in seconds
	seconds := float64(totalTime) / clockTicks

	// Calculate percent (simple approximation)
	cpuPercent := (seconds / uptime) * 100

	return cpuPercent, nil
}

func countFileDescriptors(pid int) (int, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

type hostMemoryInfo struct {
	TotalBytes     uint64
	FreeBytes      uint64
	AvailableBytes uint64
}

func readHostMemoryInfo() (*hostMemoryInfo, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}

	info := &hostMemoryInfo{}
	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			info.TotalBytes = val * 1024 // Convert KB to bytes
		case "MemFree:":
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			info.FreeBytes = val * 1024
		case "MemAvailable:":
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			info.AvailableBytes = val * 1024
		}
	}

	return info, nil
}

func readCPUCount() (int, error) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0, err
	}

	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "processor") {
			count++
		}
	}

	return count, nil
}

func readLoadAverage() ([3]float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return [3]float64{}, fmt.Errorf("invalid loadavg format")
	}

	var loadAvg [3]float64
	loadAvg[0], _ = strconv.ParseFloat(fields[0], 64)
	loadAvg[1], _ = strconv.ParseFloat(fields[1], 64)
	loadAvg[2], _ = strconv.ParseFloat(fields[2], 64)

	return loadAvg, nil
}

// Firecracker API client functions

func getBalloonConfig(client *http.Client) (*BalloonStats, error) {
	req, err := http.NewRequest("GET", "http://localhost/balloon", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var config struct {
		AmountMiB             uint32 `json:"amount_mib"`
		DeflateOnOom          bool   `json:"deflate_on_oom"`
		StatsPollingIntervalS uint16 `json:"stats_polling_interval_s"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, err
	}

	// Convert MiB to pages (4KB pages)
	pages := config.AmountMiB * 256 // 1 MiB = 256 pages of 4KB

	return &BalloonStats{
		TargetMiB:   config.AmountMiB,
		ActualMiB:   config.AmountMiB, // Actual will be updated from stats
		TargetPages: pages,
		ActualPages: pages,
	}, nil
}

func getBalloonStatsData(client *http.Client) (*BalloonStats, error) {
	req, err := http.NewRequest("GET", "http://localhost/balloon/statistics", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var stats struct {
		TargetPages        uint32  `json:"target_pages"`
		ActualPages        uint32  `json:"actual_pages"`
		TargetMiB          uint32  `json:"target_mib"`
		ActualMiB          uint32  `json:"actual_mib"`
		SwapIn             *uint64 `json:"swap_in"`
		SwapOut            *uint64 `json:"swap_out"`
		MajorFaults        *uint64 `json:"major_faults"`
		MinorFaults        *uint64 `json:"minor_faults"`
		FreeMemory         *uint64 `json:"free_memory"`
		TotalMemory        *uint64 `json:"total_memory"`
		AvailableMemory    *uint64 `json:"available_memory"`
		DiskCaches         *uint64 `json:"disk_caches"`
		HugetlbAllocations *uint64 `json:"hugetlb_allocations"`
		HugetlbFailures    *uint64 `json:"hugetlb_failures"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}

	return &BalloonStats{
		TargetPages:        stats.TargetPages,
		ActualPages:        stats.ActualPages,
		TargetMiB:          stats.TargetMiB,
		ActualMiB:          stats.ActualMiB,
		SwapIn:             stats.SwapIn,
		SwapOut:            stats.SwapOut,
		MajorFaults:        stats.MajorFaults,
		MinorFaults:        stats.MinorFaults,
		FreeMemory:         stats.FreeMemory,
		TotalMemory:        stats.TotalMemory,
		AvailableMemory:    stats.AvailableMemory,
		DiskCaches:         stats.DiskCaches,
		HugetlbAllocations: stats.HugetlbAllocations,
		HugetlbFailures:    stats.HugetlbFailures,
	}, nil
}

// FindVMProcesses finds all Firecracker processes and their details
func FindVMProcesses() ([]VMProcess, error) {
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}

	var processes []VMProcess

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Check if directory name is a number (PID)
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Read command line
		cmdlinePath := filepath.Join(procDir, entry.Name(), "cmdline")
		cmdlineData, err := os.ReadFile(cmdlinePath)
		if err != nil {
			continue
		}

		cmdline := string(cmdlineData)

		// Check if it's a Firecracker process
		if !strings.Contains(cmdline, "firecracker") {
			continue
		}

		// Extract socket path and config path
		socketPath := extractSocketPath(cmdline)
		configPath := extractConfigPath(cmdline)

		processes = append(processes, VMProcess{
			PID:        pid,
			SocketPath: socketPath,
			ConfigPath: configPath,
		})
	}

	return processes, nil
}

// VMProcess represents a Firecracker VM process
type VMProcess struct {
	PID        int
	SocketPath string
	ConfigPath string
}

func extractSocketPath(cmdline string) string {
	// Look for --api-sock argument
	parts := strings.Split(cmdline, "\x00")
	for i, part := range parts {
		if part == "--api-sock" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractConfigPath(cmdline string) string {
	// Look for --config-file argument
	parts := strings.Split(cmdline, "\x00")
	for i, part := range parts {
		if part == "--config-file" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
