package lifecycle

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
	"golang.org/x/crypto/ssh"
)

// HealthStatus represents the health status of a microVM
type HealthStatus struct {
	MicroVMID        string                 `json:"microvm_id"`
	IsHealthy        bool                   `json:"is_healthy"`
	LastHealthCheck  time.Time              `json:"last_health_check"`
	LastHealthyTime  time.Time              `json:"last_healthy_time"`
	FailureCount     int                    `json:"failure_count"`
	HealthChecks     map[string]CheckResult `json:"health_checks"`
	ErrorMessage     string                 `json:"error_message,omitempty"`
}

// CheckResult represents the result of a specific health check
type CheckResult struct {
	Name        string        `json:"name"`
	Success     bool          `json:"success"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
	Timestamp   time.Time     `json:"timestamp"`
}

// HealthChecker manages health checking for microVMs
type HealthChecker struct {
	microvmManager api.MicroVMManagerInterface
	config         *Config
	
	mu            sync.RWMutex
	healthStatus  map[string]*HealthStatus
	unhealthyVMs  map[string]time.Time
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(microvmManager api.MicroVMManagerInterface, config *Config) *HealthChecker {
	return &HealthChecker{
		microvmManager: microvmManager,
		config:         config,
		healthStatus:   make(map[string]*HealthStatus),
		unhealthyVMs:   make(map[string]time.Time),
	}
}

// CheckHealth performs comprehensive health checks on a microVM
func (hc *HealthChecker) CheckHealth(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	// Create timeout context for health checks
	checkCtx, cancel := context.WithTimeout(ctx, hc.config.HealthCheckTimeout)
	defer cancel()

	// Initialize or get existing health status
	hc.mu.Lock()
	status, exists := hc.healthStatus[microvm.ID]
	if !exists {
		status = &HealthStatus{
			MicroVMID:       microvm.ID,
			HealthChecks:    make(map[string]CheckResult),
			LastHealthyTime: time.Now(),
		}
		hc.healthStatus[microvm.ID] = status
	}
	hc.mu.Unlock()

	// Perform individual health checks
	checks := []func(context.Context, *models.MicroVMInstance) CheckResult{
		hc.checkProcessStatus,
		hc.checkNetworkConnectivity,
		hc.checkSSHConnectivity,
		hc.checkResourceUsage,
	}

	allHealthy := true
	for _, check := range checks {
		result := check(checkCtx, microvm)
		status.HealthChecks[result.Name] = result
		if !result.Success {
			allHealthy = false
		}
	}

	// Update health status
	hc.mu.Lock()
	status.LastHealthCheck = time.Now()
	status.IsHealthy = allHealthy

	if allHealthy {
		status.LastHealthyTime = time.Now()
		status.FailureCount = 0
		status.ErrorMessage = ""
		delete(hc.unhealthyVMs, microvm.ID)
	} else {
		status.FailureCount++
		hc.unhealthyVMs[microvm.ID] = time.Now()
		
		// Collect error messages from failed checks
		var errors []string
		for _, check := range status.HealthChecks {
			if !check.Success && check.Error != "" {
				errors = append(errors, fmt.Sprintf("%s: %s", check.Name, check.Error))
			}
		}
		if len(errors) > 0 {
			status.ErrorMessage = fmt.Sprintf("Health check failures: %v", errors)
		}
	}
	hc.mu.Unlock()

	if !allHealthy {
		return fmt.Errorf("health check failed for microVM %s", microvm.ID)
	}

	return nil
}

// GetHealthStatus returns the current health status for a microVM
func (hc *HealthChecker) GetHealthStatus(microvmID string) (*HealthStatus, error) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	status, exists := hc.healthStatus[microvmID]
	if !exists {
		return nil, fmt.Errorf("no health status found for microVM %s", microvmID)
	}

	// Return a copy to avoid race conditions
	statusCopy := *status
	statusCopy.HealthChecks = make(map[string]CheckResult)
	for k, v := range status.HealthChecks {
		statusCopy.HealthChecks[k] = v
	}

	return &statusCopy, nil
}

// GetUnhealthyMicroVMs returns a list of microVM IDs that are currently unhealthy
func (hc *HealthChecker) GetUnhealthyMicroVMs() []string {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	var unhealthy []string
	for microvmID := range hc.unhealthyVMs {
		unhealthy = append(unhealthy, microvmID)
	}

	return unhealthy
}

// RemoveHealthStatus removes health status tracking for a microVM
func (hc *HealthChecker) RemoveHealthStatus(microvmID string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	delete(hc.healthStatus, microvmID)
	delete(hc.unhealthyVMs, microvmID)
}

// checkProcessStatus checks if the firecracker process is running
func (hc *HealthChecker) checkProcessStatus(ctx context.Context, microvm *models.MicroVMInstance) CheckResult {
	start := time.Now()
	result := CheckResult{
		Name:      "process_status",
		Timestamp: start,
	}

	status, err := hc.microvmManager.GetMicroVMStatus(ctx, microvm.ID)
	result.Duration = time.Since(start)

	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("failed to get microVM status: %v", err)
		return result
	}

	if status != models.MicroVMStatusRunning {
		result.Success = false
		result.Error = fmt.Sprintf("microVM status is %s, expected running", status)
		return result
	}

	result.Success = true
	return result
}

// checkNetworkConnectivity checks if the microVM's network interface is accessible
func (hc *HealthChecker) checkNetworkConnectivity(ctx context.Context, microvm *models.MicroVMInstance) CheckResult {
	start := time.Now()
	result := CheckResult{
		Name:      "network_connectivity",
		Timestamp: start,
	}

	if microvm.Network.IPAddress == "" {
		result.Duration = time.Since(start)
		result.Success = false
		result.Error = "no IP address assigned to microVM"
		return result
	}

	// Try to ping the microVM (using a simple TCP connection test)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:22", microvm.Network.IPAddress), 5*time.Second)
	result.Duration = time.Since(start)

	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("failed to connect to microVM network: %v", err)
		return result
	}

	conn.Close()
	result.Success = true
	return result
}

// checkSSHConnectivity checks if SSH is accessible on the microVM
func (hc *HealthChecker) checkSSHConnectivity(ctx context.Context, microvm *models.MicroVMInstance) CheckResult {
	start := time.Now()
	result := CheckResult{
		Name:      "ssh_connectivity",
		Timestamp: start,
	}

	if microvm.Network.IPAddress == "" {
		result.Duration = time.Since(start)
		result.Success = false
		result.Error = "no IP address assigned to microVM"
		return result
	}

	sshPort := microvm.Network.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", microvm.Network.IPAddress, sshPort), 5*time.Second)
	result.Duration = time.Since(start)

	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("failed to connect to SSH port: %v", err)
		return result
	}
	conn.Close()

	if microvm.Metadata.Labels != nil {
		if password := microvm.Metadata.Labels["cloudinit_password"]; password != "" {
			username := microvm.Metadata.Labels["cloudinit_username"]
			if username == "" {
				username = "root"
			}
			sshConfig := &ssh.ClientConfig{
				User:            username,
				Auth:            []ssh.AuthMethod{ssh.Password(password)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         5 * time.Second,
			}
			sshClient, sshErr := ssh.Dial("tcp", fmt.Sprintf("%s:%d", microvm.Network.IPAddress, sshPort), sshConfig)
			if sshErr != nil {
				result.Success = false
				result.Error = fmt.Sprintf("SSH password auth failed: %v", sshErr)
				return result
			}
			sshClient.Close()
		}
	}

	result.Success = true
	return result
}

// checkResourceUsage checks if the microVM is using resources within expected limits
func (hc *HealthChecker) checkResourceUsage(ctx context.Context, microvm *models.MicroVMInstance) CheckResult {
	start := time.Now()
	result := CheckResult{
		Name:      "resource_usage",
		Timestamp: start,
	}

	// For now, this is a placeholder check
	// In a real implementation, this would query actual resource usage
	// from the hypervisor or guest agent
	
	result.Duration = time.Since(start)
	result.Success = true
	
	// TODO: Implement actual resource usage checking
	// This could involve:
	// - Querying firecracker API for resource metrics
	// - Using cgroups to check resource consumption
	// - Connecting to guest agent for internal metrics
	
	return result
}

// IsHealthy returns true if a microVM is currently healthy
func (hc *HealthChecker) IsHealthy(microvmID string) bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	status, exists := hc.healthStatus[microvmID]
	if !exists {
		return false
	}

	return status.IsHealthy
}

// GetFailureCount returns the current failure count for a microVM
func (hc *HealthChecker) GetFailureCount(microvmID string) int {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	status, exists := hc.healthStatus[microvmID]
	if !exists {
		return 0
	}

	return status.FailureCount
}