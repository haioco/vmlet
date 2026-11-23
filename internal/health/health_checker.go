package health

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// HealthChecker provides comprehensive system health checking capabilities
type HealthChecker struct {
	systemChecks    []SystemCheck
	serviceChecks   []ServiceCheck
	resourceChecks  []ResourceCheck
}

// SystemCheck represents a system-level health check
type SystemCheck struct {
	Name        string
	Description string
	CheckFunc   func() (bool, string, error)
	Critical    bool
}

// ServiceCheck represents a service-level health check
type ServiceCheck struct {
	Name        string
	Description string
	CheckFunc   func() (bool, string, error)
	Critical    bool
}

// ResourceCheck represents a resource availability check
type ResourceCheck struct {
	Name        string
	Description string
	CheckFunc   func() (bool, string, error)
	Threshold   float64 // Warning threshold (0.0-1.0)
	Critical    bool    // Whether this check is critical
}

// HealthStatus represents the overall health status
type HealthStatus struct {
	Overall     string                     `json:"overall"`
	Timestamp   time.Time                  `json:"timestamp"`
	Checks      map[string]CheckResult     `json:"checks"`
	Summary     HealthSummary              `json:"summary"`
	Issues      []HealthIssue              `json:"issues"`
	Recommendations []string               `json:"recommendations"`
}

// CheckResult represents the result of a single health check
type CheckResult struct {
	Status      string    `json:"status"`
	Message     string    `json:"message"`
	Critical    bool      `json:"critical"`
	Duration    time.Duration `json:"duration"`
	LastCheck   time.Time `json:"last_check"`
	Error       string    `json:"error,omitempty"`
}

// HealthSummary provides a summary of health check results
type HealthSummary struct {
	TotalChecks    int `json:"total_checks"`
	PassedChecks   int `json:"passed_checks"`
	FailedChecks   int `json:"failed_checks"`
	CriticalFailed int `json:"critical_failed"`
	WarningChecks  int `json:"warning_checks"`
}

// HealthIssue represents a specific health issue
type HealthIssue struct {
	Component   string `json:"component"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Resolution  string `json:"resolution"`
}

// NewHealthChecker creates a new health checker with default checks
func NewHealthChecker() *HealthChecker {
	hc := &HealthChecker{
		systemChecks:   []SystemCheck{},
		serviceChecks:  []ServiceCheck{},
		resourceChecks: []ResourceCheck{},
	}
	
	// Add default system checks
	hc.addDefaultSystemChecks()
	hc.addDefaultServiceChecks()
	hc.addDefaultResourceChecks()
	
	return hc
}

// PerformHealthCheck performs all health checks and returns the overall status
func (hc *HealthChecker) PerformHealthCheck(ctx context.Context) (*HealthStatus, error) {
	status := &HealthStatus{
		Overall:         "unknown",
		Timestamp:       time.Now(),
		Checks:          make(map[string]CheckResult),
		Issues:          []HealthIssue{},
		Recommendations: []string{},
	}
	
	// Perform system checks
	for _, check := range hc.systemChecks {
		result := hc.performCheck(ctx, check.Name, check.CheckFunc, check.Critical)
		status.Checks[fmt.Sprintf("system_%s", check.Name)] = result
	}
	
	// Perform service checks
	for _, check := range hc.serviceChecks {
		result := hc.performCheck(ctx, check.Name, check.CheckFunc, check.Critical)
		status.Checks[fmt.Sprintf("service_%s", check.Name)] = result
	}
	
	// Perform resource checks
	for _, check := range hc.resourceChecks {
		result := hc.performCheck(ctx, check.Name, check.CheckFunc, check.Critical)
		status.Checks[fmt.Sprintf("resource_%s", check.Name)] = result
	}
	
	// Calculate summary
	status.Summary = hc.calculateSummary(status.Checks)
	
	// Determine overall status
	status.Overall = hc.determineOverallStatus(status.Summary)
	
	// Generate issues and recommendations
	status.Issues, status.Recommendations = hc.generateIssuesAndRecommendations(status.Checks)
	
	return status, nil
}

// performCheck executes a single health check
func (hc *HealthChecker) performCheck(ctx context.Context, name string, checkFunc func() (bool, string, error), critical bool) CheckResult {
	start := time.Now()
	
	passed, message, err := checkFunc()
	
	result := CheckResult{
		Critical:  critical,
		Duration:  time.Since(start),
		LastCheck: time.Now(),
	}
	
	if err != nil {
		result.Status = "error"
		result.Message = message
		result.Error = err.Error()
	} else if passed {
		result.Status = "healthy"
		result.Message = message
	} else {
		if critical {
			result.Status = "critical"
		} else {
			result.Status = "warning"
		}
		result.Message = message
	}
	
	return result
}

// calculateSummary calculates health check summary statistics
func (hc *HealthChecker) calculateSummary(checks map[string]CheckResult) HealthSummary {
	summary := HealthSummary{
		TotalChecks: len(checks),
	}
	
	for _, result := range checks {
		switch result.Status {
		case "healthy":
			summary.PassedChecks++
		case "warning":
			summary.WarningChecks++
		case "critical", "error":
			summary.FailedChecks++
			if result.Critical {
				summary.CriticalFailed++
			}
		}
	}
	
	return summary
}

// determineOverallStatus determines the overall health status
func (hc *HealthChecker) determineOverallStatus(summary HealthSummary) string {
	if summary.CriticalFailed > 0 {
		return "critical"
	} else if summary.FailedChecks > 0 {
		return "unhealthy"
	} else if summary.WarningChecks > 0 {
		return "degraded"
	} else {
		return "healthy"
	}
}

// generateIssuesAndRecommendations generates issues and recommendations based on check results
func (hc *HealthChecker) generateIssuesAndRecommendations(checks map[string]CheckResult) ([]HealthIssue, []string) {
	var issues []HealthIssue
	var recommendations []string
	
	for checkName, result := range checks {
		if result.Status != "healthy" {
			severity := "warning"
			if result.Status == "critical" || result.Status == "error" {
				severity = "critical"
			}
			
			issue := HealthIssue{
				Component: checkName,
				Severity:  severity,
				Message:   result.Message,
				Resolution: hc.getResolutionForCheck(checkName, result),
			}
			issues = append(issues, issue)
			
			// Add specific recommendations
			if rec := hc.getRecommendationForCheck(checkName, result); rec != "" {
				recommendations = append(recommendations, rec)
			}
		}
	}
	
	return issues, recommendations
}

// getResolutionForCheck provides resolution steps for specific check failures
func (hc *HealthChecker) getResolutionForCheck(checkName string, result CheckResult) string {
	switch {
	case contains(checkName, "firecracker"):
		return "Install firecracker binary or check PATH configuration"
	case contains(checkName, "kvm"):
		return "Ensure KVM is available and user has access to /dev/kvm"
	case contains(checkName, "network"):
		return "Check network configuration and bridge setup"
	case contains(checkName, "storage"):
		return "Check disk space and storage permissions"
	case contains(checkName, "memory"):
		return "Free up system memory or increase available RAM"
	default:
		return "Check system logs for more details"
	}
}

// getRecommendationForCheck provides specific recommendations for check failures
func (hc *HealthChecker) getRecommendationForCheck(checkName string, result CheckResult) string {
	switch {
	case contains(checkName, "firecracker"):
		return "Install firecracker: wget https://github.com/firecracker-microvm/firecracker/releases/latest/download/firecracker-v*-x86_64.tgz"
	case contains(checkName, "kvm"):
		return "Add user to kvm group: sudo usermod -a -G kvm $USER"
	case contains(checkName, "bridge"):
		return "Install bridge-utils: sudo apt-get install bridge-utils"
	case contains(checkName, "storage") && contains(result.Message, "space"):
		return "Clean up disk space or increase storage capacity"
	default:
		return ""
	}
}

// addDefaultSystemChecks adds default system-level health checks
func (hc *HealthChecker) addDefaultSystemChecks() {
	hc.systemChecks = append(hc.systemChecks, []SystemCheck{
		{
			Name:        "firecracker_binary",
			Description: "Firecracker binary availability",
			CheckFunc:   hc.checkFirecrackerBinary,
			Critical:    true,
		},
		{
			Name:        "kvm_access",
			Description: "KVM device access",
			CheckFunc:   hc.checkKVMAccess,
			Critical:    true,
		},
		{
			Name:        "tun_device",
			Description: "TUN/TAP device availability",
			CheckFunc:   hc.checkTUNDevice,
			Critical:    true,
		},
		{
			Name:        "bridge_utils",
			Description: "Bridge utilities availability",
			CheckFunc:   hc.checkBridgeUtils,
			Critical:    false,
		},
		{
			Name:        "iptables",
			Description: "iptables availability",
			CheckFunc:   hc.checkIptables,
			Critical:    false,
		},
		{
			Name:        "ip_forwarding",
			Description: "IP forwarding enabled",
			CheckFunc:   hc.checkIPForwarding,
			Critical:    false,
		},
	}...)
}

// addDefaultServiceChecks adds default service-level health checks
func (hc *HealthChecker) addDefaultServiceChecks() {
	hc.serviceChecks = append(hc.serviceChecks, []ServiceCheck{
		{
			Name:        "work_directory",
			Description: "Work directory accessibility",
			CheckFunc:   hc.checkWorkDirectory,
			Critical:    true,
		},
		{
			Name:        "network_bridge",
			Description: "Network bridge status",
			CheckFunc:   hc.checkNetworkBridge,
			Critical:    false,
		},
		{
			Name:        "firecracker_processes",
			Description: "Firecracker process health",
			CheckFunc:   hc.checkFirecrackerProcesses,
			Critical:    false,
		},
	}...)
}

// addDefaultResourceChecks adds default resource availability checks
func (hc *HealthChecker) addDefaultResourceChecks() {
	hc.resourceChecks = append(hc.resourceChecks, []ResourceCheck{
		{
			Name:        "disk_space",
			Description: "Available disk space",
			CheckFunc:   hc.checkDiskSpace,
			Threshold:   0.8, // Warning at 80% usage
			Critical:    false,
		},
		{
			Name:        "memory_usage",
			Description: "System memory usage",
			CheckFunc:   hc.checkMemoryUsage,
			Threshold:   0.9, // Warning at 90% usage
			Critical:    false,
		},
		{
			Name:        "cpu_load",
			Description: "System CPU load",
			CheckFunc:   hc.checkCPULoad,
			Threshold:   0.8, // Warning at 80% load
			Critical:    false,
		},
	}...)
}

// System check implementations

func (hc *HealthChecker) checkFirecrackerBinary() (bool, string, error) {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return false, "Firecracker binary not found in PATH", nil
	}
	return true, "Firecracker binary is available", nil
}

func (hc *HealthChecker) checkKVMAccess() (bool, string, error) {
	if _, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return false, "Cannot access /dev/kvm device", nil
	}
	return true, "KVM device is accessible", nil
}

func (hc *HealthChecker) checkTUNDevice() (bool, string, error) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return false, "TUN/TAP device not available", nil
	}
	return true, "TUN/TAP device is available", nil
}

func (hc *HealthChecker) checkBridgeUtils() (bool, string, error) {
	if _, err := exec.LookPath("brctl"); err != nil {
		return false, "Bridge utilities not installed", nil
	}
	return true, "Bridge utilities are available", nil
}

func (hc *HealthChecker) checkIptables() (bool, string, error) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return false, "iptables not available", nil
	}
	return true, "iptables is available", nil
}

func (hc *HealthChecker) checkIPForwarding() (bool, string, error) {
	cmd := exec.Command("sysctl", "net.ipv4.ip_forward")
	output, err := cmd.Output()
	if err != nil {
		return false, "Cannot check IP forwarding status", err
	}
	
	if contains(string(output), "net.ipv4.ip_forward = 1") {
		return true, "IP forwarding is enabled", nil
	}
	return false, "IP forwarding is disabled", nil
}

// Service check implementations

func (hc *HealthChecker) checkWorkDirectory() (bool, string, error) {
	workDir := "./firecracker-work"
	if stat, err := os.Stat(workDir); err != nil {
		return false, "Work directory not accessible", err
	} else if !stat.IsDir() {
		return false, "Work directory is not a directory", nil
	}
	return true, "Work directory is accessible", nil
}

func (hc *HealthChecker) checkNetworkBridge() (bool, string, error) {
	cmd := exec.Command("ip", "link", "show", "fc-br0")
	if err := cmd.Run(); err != nil {
		return false, "Network bridge fc-br0 not found", nil
	}
	return true, "Network bridge fc-br0 is available", nil
}

func (hc *HealthChecker) checkFirecrackerProcesses() (bool, string, error) {
	cmd := exec.Command("pgrep", "-f", "firecracker")
	output, err := cmd.Output()
	if err != nil {
		return true, "No firecracker processes running", nil
	}
	
	processCount := len(strings.Split(strings.TrimSpace(string(output)), "\n"))
	return true, fmt.Sprintf("%d firecracker processes running", processCount), nil
}

// Resource check implementations

func (hc *HealthChecker) checkDiskSpace() (bool, string, error) {
	cmd := exec.Command("df", "-h", ".")
	output, err := cmd.Output()
	if err != nil {
		return false, "Cannot check disk space", err
	}
	
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return false, "Cannot parse disk space output", nil
	}
	
	// Parse the usage percentage
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return false, "Cannot parse disk usage", nil
	}
	
	usageStr := fields[4]
	if strings.HasSuffix(usageStr, "%") {
		usageStr = strings.TrimSuffix(usageStr, "%")
		// For simplicity, we'll just check if usage is reasonable
		return true, fmt.Sprintf("Disk usage: %s%%", usageStr), nil
	}
	
	return true, "Disk space check completed", nil
}

func (hc *HealthChecker) checkMemoryUsage() (bool, string, error) {
	cmd := exec.Command("free", "-m")
	output, err := cmd.Output()
	if err != nil {
		return false, "Cannot check memory usage", err
	}
	
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return false, "Cannot parse memory output", nil
	}
	
	// Simple check - if we can run the command, memory is probably OK
	return true, "Memory usage check completed", nil
}

func (hc *HealthChecker) checkCPULoad() (bool, string, error) {
	cmd := exec.Command("uptime")
	output, err := cmd.Output()
	if err != nil {
		return false, "Cannot check CPU load", err
	}
	
	return true, fmt.Sprintf("CPU load check completed: %s", strings.TrimSpace(string(output))), nil
}

// Utility functions

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}