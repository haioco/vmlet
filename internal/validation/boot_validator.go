package validation

import (
	"context"
	"fmt"
	"io"
	"time"
)

// BootValidator orchestrates VM boot monitoring and validation
type BootValidator struct {
	consoleMonitor *ConsoleMonitor
	sshChecker     *SSHChecker
	networkTester  *NetworkTester
	bootTimeout    time.Duration
	sshTimeout     time.Duration
}

// ValidationConfig contains configuration for boot validation
type ValidationConfig struct {
	BootTimeout time.Duration
	SSHTimeout  time.Duration
	SSHConfig   *SSHConfig
	VMIP        string
}

// ValidationResult contains the complete validation result
type ValidationResult struct {
	Success        bool
	BootResult     *BootResult
	SSHResult      *SSHResult
	NetworkResult  *NetworkTestResult
	HealthStatus   *HealthStatus
	ErrorMessage   string
	TotalDuration  time.Duration
	ValidatedAt    time.Time
}

// NewBootValidator creates a new boot validator with default settings
func NewBootValidator() *BootValidator {
	return &BootValidator{
		consoleMonitor: NewConsoleMonitor(60 * time.Second), // 60 second boot timeout
		sshChecker:     NewSSHChecker(10, 3*time.Second),    // 10 retries, 3 second intervals
		networkTester:  NewNetworkTester(10 * time.Second),  // 10 second network timeout
		bootTimeout:    60 * time.Second,
		sshTimeout:     30 * time.Second, // 30 second SSH timeout as per requirements
	}
}

// NewBootValidatorWithConfig creates a boot validator with custom configuration
func NewBootValidatorWithConfig(bootTimeout, sshTimeout time.Duration) *BootValidator {
	return &BootValidator{
		consoleMonitor: NewConsoleMonitor(bootTimeout),
		sshChecker:     NewSSHChecker(int(sshTimeout.Seconds()/3), 3*time.Second),
		networkTester:  NewNetworkTester(10 * time.Second),
		bootTimeout:    bootTimeout,
		sshTimeout:     sshTimeout,
	}
}

// ValidateVMBoot performs comprehensive VM boot validation
func (bv *BootValidator) ValidateVMBoot(ctx context.Context, consoleReader io.Reader, config *ValidationConfig) (*ValidationResult, error) {
	startTime := time.Now()
	
	result := &ValidationResult{
		Success:     false,
		ValidatedAt: time.Now(),
	}

	// Step 1: Monitor boot process through console output
	bootResult, err := bv.consoleMonitor.MonitorBoot(ctx, consoleReader)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("Boot monitoring failed: %v", err)
		result.TotalDuration = time.Since(startTime)
		return result, nil
	}
	result.BootResult = bootResult

	// If boot failed, don't proceed with other tests
	if !bootResult.Success {
		result.ErrorMessage = fmt.Sprintf("VM boot failed: %s", bootResult.ErrorMessage)
		result.TotalDuration = time.Since(startTime)
		return result, nil
	}

	// Step 2: Wait for SSH service to become ready
	if config.SSHConfig != nil {
		sshResult, err := bv.sshChecker.WaitForSSHReady(ctx, config.SSHConfig)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("SSH validation failed: %v", err)
			result.TotalDuration = time.Since(startTime)
			return result, nil
		}
		result.SSHResult = sshResult

		// If SSH failed, still continue with network tests but mark as failed
		if !sshResult.Success {
			result.ErrorMessage = fmt.Sprintf("SSH connectivity failed: %s", sshResult.ErrorMessage)
		}
	}

	// Step 3: Test network connectivity
	if config.VMIP != "" {
		networkResult, err := bv.networkTester.TestNetworkConnectivity(ctx, config.VMIP)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("Network testing failed: %v", err)
			result.TotalDuration = time.Since(startTime)
			return result, nil
		}
		result.NetworkResult = networkResult
	}

	// Step 4: Perform comprehensive health check
	if config.SSHConfig != nil && config.VMIP != "" {
		healthStatus, err := bv.networkTester.CheckVMHealth(ctx, config.VMIP, config.SSHConfig)
		if err != nil {
			// Health check failure is not critical, just log it
			result.ErrorMessage = fmt.Sprintf("Health check failed: %v", err)
		} else {
			result.HealthStatus = healthStatus
		}
	}

	// Determine overall success
	bootSuccess := result.BootResult != nil && result.BootResult.Success
	sshSuccess := result.SSHResult == nil || result.SSHResult.Success
	networkSuccess := result.NetworkResult == nil || result.NetworkResult.Success

	result.Success = bootSuccess && sshSuccess && networkSuccess
	
	if result.Success && result.ErrorMessage != "" {
		// Clear error message if overall validation succeeded
		result.ErrorMessage = ""
	}

	result.TotalDuration = time.Since(startTime)
	return result, nil
}

// MonitorBoot monitors only the boot process without SSH/network validation
func (bv *BootValidator) MonitorBoot(ctx context.Context, consoleReader io.Reader) (*BootResult, error) {
	return bv.consoleMonitor.MonitorBoot(ctx, consoleReader)
}

// ValidateSSHAccess validates only SSH connectivity
func (bv *BootValidator) ValidateSSHAccess(ctx context.Context, config *SSHConfig) (*SSHResult, error) {
	return bv.sshChecker.ValidateSSHAccess(ctx, config)
}

// CheckVMHealth performs only health checking
func (bv *BootValidator) CheckVMHealth(ctx context.Context, vmIP string, sshConfig *SSHConfig) (*HealthStatus, error) {
	return bv.networkTester.CheckVMHealth(ctx, vmIP, sshConfig)
}

// GetBootProgress returns the current boot progress percentage
func (bv *BootValidator) GetBootProgress() float64 {
	return bv.consoleMonitor.GetBootProgress()
}

// Reset resets all validators for a new validation session
func (bv *BootValidator) Reset() {
	bv.consoleMonitor.Reset()
}

// SetBootTimeout updates the boot timeout
func (bv *BootValidator) SetBootTimeout(timeout time.Duration) {
	bv.bootTimeout = timeout
	bv.consoleMonitor = NewConsoleMonitor(timeout)
}

// SetSSHTimeout updates the SSH timeout
func (bv *BootValidator) SetSSHTimeout(timeout time.Duration) {
	bv.sshTimeout = timeout
	bv.sshChecker = NewSSHChecker(int(timeout.Seconds()/3), 3*time.Second)
}