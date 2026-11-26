package firecracker

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ErrorHandler provides enhanced error handling and recovery capabilities
type ErrorHandler struct {
	maxRetries     int
	baseDelay      time.Duration
	maxDelay       time.Duration
	diagnosticsDir string
}

// RetryConfig defines retry behavior for operations
type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// DiagnosticData contains diagnostic information for failed operations
type DiagnosticData struct {
	OperationType   string                 `json:"operation_type"`
	MicroVMID       string                 `json:"microvm_id"`
	ErrorMessage    string                 `json:"error_message"`
	Timestamp       time.Time              `json:"timestamp"`
	SystemInfo      map[string]interface{} `json:"system_info"`
	NetworkInfo     map[string]interface{} `json:"network_info"`
	StorageInfo     map[string]interface{} `json:"storage_info"`
	ConsoleLog      string                 `json:"console_log,omitempty"`
	StackTrace      string                 `json:"stack_trace,omitempty"`
	RetryAttempts   int                    `json:"retry_attempts"`
	RecoveryActions []string               `json:"recovery_actions"`
}

// NewErrorHandler creates a new error handler with default configuration
func NewErrorHandler(diagnosticsDir string) *ErrorHandler {
	return &ErrorHandler{
		maxRetries:     3,
		baseDelay:      2 * time.Second,
		maxDelay:       30 * time.Second,
		diagnosticsDir: diagnosticsDir,
	}
}

// WithRetryConfig configures retry behavior
func (eh *ErrorHandler) WithRetryConfig(config RetryConfig) *ErrorHandler {
	eh.maxRetries = config.MaxRetries
	eh.baseDelay = config.BaseDelay
	eh.maxDelay = config.MaxDelay
	return eh
}

// RetryWithBackoff executes an operation with exponential backoff retry logic
func (eh *ErrorHandler) RetryWithBackoff(ctx context.Context, operation func() error, operationType string) error {
	var lastErr error

	for attempt := 0; attempt <= eh.maxRetries; attempt++ {
		if attempt > 0 {
			// Calculate exponential backoff delay
			delay := eh.calculateBackoffDelay(attempt)

			select {
			case <-ctx.Done():
				return fmt.Errorf("operation cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
				// Continue with retry
			}
		}

		err := operation()
		if err == nil {
			return nil // Success
		}

		lastErr = err

		// Check if error is retryable
		if !eh.isRetryableError(err) {
			return fmt.Errorf("non-retryable error in %s: %w", operationType, err)
		}
	}

	return fmt.Errorf("operation %s failed after %d attempts: %w", operationType, eh.maxRetries+1, lastErr)
}

// CollectDiagnostics collects comprehensive diagnostic information for a failed operation
func (eh *ErrorHandler) CollectDiagnostics(ctx context.Context, operationType, microvmID string, err error, retryAttempts int) (*DiagnosticData, error) {
	diagnostics := &DiagnosticData{
		OperationType: operationType,
		MicroVMID:     microvmID,
		ErrorMessage:  err.Error(),
		Timestamp:     time.Now(),
		RetryAttempts: retryAttempts,
		SystemInfo:    make(map[string]interface{}),
		NetworkInfo:   make(map[string]interface{}),
		StorageInfo:   make(map[string]interface{}),
	}

	// Collect system information
	if systemInfo, sysErr := eh.collectSystemInfo(); sysErr == nil {
		diagnostics.SystemInfo = systemInfo
	}

	// Collect network information
	if networkInfo, netErr := eh.collectNetworkInfo(); netErr == nil {
		diagnostics.NetworkInfo = networkInfo
	}

	// Collect storage information
	if storageInfo, storErr := eh.collectStorageInfo(microvmID); storErr == nil {
		diagnostics.StorageInfo = storageInfo
	}

	// Collect console log if available
	if consoleLog, logErr := eh.collectConsoleLog(microvmID); logErr == nil {
		diagnostics.ConsoleLog = consoleLog
	}

	// Generate recovery actions
	diagnostics.RecoveryActions = eh.generateRecoveryActions(operationType, err)

	return diagnostics, nil
}

// StoreDiagnostics stores diagnostic data for later analysis
func (eh *ErrorHandler) StoreDiagnostics(diagnostics *DiagnosticData) error {
	// Create diagnostics directory if it doesn't exist
	if err := createDirectory(eh.diagnosticsDir); err != nil {
		return fmt.Errorf("failed to create diagnostics directory: %w", err)
	}

	// Generate filename with timestamp
	filename := fmt.Sprintf("%s_%s_%s.json",
		diagnostics.OperationType,
		diagnostics.MicroVMID,
		diagnostics.Timestamp.Format("20060102_150405"))

	filepath := fmt.Sprintf("%s/%s", eh.diagnosticsDir, filename)

	// Marshal and write diagnostics
	data, err := marshalJSON(diagnostics)
	if err != nil {
		return fmt.Errorf("failed to marshal diagnostics: %w", err)
	}

	if err := writeFile(filepath, string(data)); err != nil {
		return fmt.Errorf("failed to write diagnostics file: %w", err)
	}

	return nil
}

// RecoverFromError attempts to recover from specific error conditions
func (eh *ErrorHandler) RecoverFromError(ctx context.Context, operationType, microvmID string, err error) error {
	switch operationType {
	case "vm_creation":
		return eh.recoverFromVMCreationError(ctx, microvmID, err)
	case "network_setup":
		return eh.recoverFromNetworkError(ctx, microvmID, err)
	case "storage_provision":
		return eh.recoverFromStorageError(ctx, microvmID, err)
	case "boot_validation":
		return eh.recoverFromBootError(ctx, microvmID, err)
	case "network_interface_creation":
		return eh.recoverFromNetworkInterfaceError(ctx, microvmID, err)
	case "cleanup":
		return eh.recoverFromCleanupError(ctx, microvmID, err)
	default:
		return fmt.Errorf("no recovery strategy for operation type: %s", operationType)
	}
}

// calculateBackoffDelay calculates exponential backoff delay
func (eh *ErrorHandler) calculateBackoffDelay(attempt int) time.Duration {
	// Exponential backoff: baseDelay * 2^(attempt-1)
	multiplier := 1 << uint(attempt-1) // 2^(attempt-1)
	delay := time.Duration(int64(eh.baseDelay) * int64(multiplier))

	// Cap at maximum delay
	if delay > eh.maxDelay {
		delay = eh.maxDelay
	}

	return delay
}

// isRetryableError determines if an error is retryable
func (eh *ErrorHandler) isRetryableError(err error) bool {
	errorMsg := err.Error()

	// Network-related errors are usually retryable
	retryablePatterns := []string{
		"connection refused",
		"timeout",
		"temporary failure",
		"resource temporarily unavailable",
		"network unreachable",
		"no route to host",
		"connection reset",
	}

	for _, pattern := range retryablePatterns {
		if containsSubstring(errorMsg, pattern) {
			return true
		}
	}

	// Non-retryable errors
	nonRetryablePatterns := []string{
		"permission denied",
		"file not found",
		"invalid configuration",
		"authentication failed",
		"quota exceeded",
	}

	for _, pattern := range nonRetryablePatterns {
		if containsSubstring(errorMsg, pattern) {
			return false
		}
	}

	// Default to retryable for unknown errors
	return true
}

// collectSystemInfo collects system-level diagnostic information
func (eh *ErrorHandler) collectSystemInfo() (map[string]interface{}, error) {
	info := make(map[string]interface{})

	// Memory information
	if memInfo, err := executeCommandWithOutput("free", "-m"); err == nil {
		info["memory"] = memInfo
	}

	// Disk space information
	if diskInfo, err := executeCommandWithOutput("df", "-h"); err == nil {
		info["disk_space"] = diskInfo
	}

	// CPU information
	if cpuInfo, err := executeCommandWithOutput("nproc"); err == nil {
		info["cpu_count"] = cpuInfo
	}

	// Load average
	if loadInfo, err := executeCommandWithOutput("uptime"); err == nil {
		info["load_average"] = loadInfo
	}

	// Firecracker process information
	if fcInfo, err := executeCommandWithOutput("pgrep", "-f", "firecracker"); err == nil {
		info["firecracker_processes"] = fcInfo
	}

	return info, nil
}

// collectNetworkInfo collects network-related diagnostic information
func (eh *ErrorHandler) collectNetworkInfo() (map[string]interface{}, error) {
	info := make(map[string]interface{})

	// Network interfaces
	if ifInfo, err := executeCommandWithOutput("ip", "addr", "show"); err == nil {
		info["interfaces"] = ifInfo
	}

	// Routing table
	if routeInfo, err := executeCommandWithOutput("ip", "route", "show"); err == nil {
		info["routes"] = routeInfo
	}

	// Bridge information
	if bridgeInfo, err := executeCommandWithOutput("brctl", "show"); err == nil {
		info["bridges"] = bridgeInfo
	}

	// iptables rules
	if iptablesInfo, err := executeCommandWithOutput("iptables", "-L", "-n"); err == nil {
		info["iptables"] = iptablesInfo
	}

	// NAT rules
	if natInfo, err := executeCommandWithOutput("iptables", "-t", "nat", "-L", "-n"); err == nil {
		info["nat_rules"] = natInfo
	}

	// DHCP leases
	if dhcpInfo, err := executeCommandWithOutput("cat", "/var/lib/dhcp/dhcpd.leases"); err == nil {
		info["dhcp_leases"] = dhcpInfo
	}

	return info, nil
}

// collectStorageInfo collects storage-related diagnostic information
func (eh *ErrorHandler) collectStorageInfo(microvmID string) (map[string]interface{}, error) {
	info := make(map[string]interface{})

	// Loop devices
	if loopInfo, err := executeCommandWithOutput("losetup", "-a"); err == nil {
		info["loop_devices"] = loopInfo
	}

	// NBD devices
	if nbdInfo, err := executeCommandWithOutput("lsblk", "-f"); err == nil {
		info["block_devices"] = nbdInfo
	}

	// Mount points
	if mountInfo, err := executeCommandWithOutput("mount"); err == nil {
		info["mounts"] = mountInfo
	}

	// Filesystem usage
	if fsInfo, err := executeCommandWithOutput("df", "-i"); err == nil {
		info["filesystem_usage"] = fsInfo
	}

	return info, nil
}

// collectConsoleLog collects console log for a specific microVM
func (eh *ErrorHandler) collectConsoleLog(microvmID string) (string, error) {
	logPath := fmt.Sprintf("/var/lib/firecracker/microvms/%s/firecracker.log", microvmID)
	return readFile(logPath)
}

// generateRecoveryActions generates suggested recovery actions based on error type
func (eh *ErrorHandler) generateRecoveryActions(operationType string, err error) []string {
	var actions []string
	errorMsg := err.Error()

	switch operationType {
	case "vm_creation":
		if containsSubstring(errorMsg, "permission denied") {
			actions = append(actions, "Check file permissions for Firecracker binary and work directory")
			actions = append(actions, "Verify user has necessary privileges to create VMs")
		}
		if containsSubstring(errorMsg, "resource") {
			actions = append(actions, "Check available system resources (CPU, memory)")
			actions = append(actions, "Verify no resource limits are being exceeded")
		}

	case "network_setup":
		if containsSubstring(errorMsg, "bridge") {
			actions = append(actions, "Verify bridge-utils package is installed")
			actions = append(actions, "Check if bridge interface already exists")
			actions = append(actions, "Ensure proper network permissions")
		}
		if containsSubstring(errorMsg, "iptables") {
			actions = append(actions, "Verify iptables is installed and accessible")
			actions = append(actions, "Check for conflicting firewall rules")
		}

	case "storage_provision":
		if containsSubstring(errorMsg, "space") {
			actions = append(actions, "Check available disk space")
			actions = append(actions, "Clean up old VM images if necessary")
		}
		if containsSubstring(errorMsg, "s3") {
			actions = append(actions, "Verify S3 credentials and permissions")
			actions = append(actions, "Check S3 bucket accessibility")
		}

	case "boot_validation":
		if containsSubstring(errorMsg, "timeout") {
			actions = append(actions, "Increase boot timeout if VM is slow to start")
			actions = append(actions, "Check VM console logs for boot errors")
		}
		if containsSubstring(errorMsg, "ssh") {
			actions = append(actions, "Verify SSH service is enabled in VM image")
			actions = append(actions, "Check SSH key configuration")
			actions = append(actions, "Ensure network connectivity to VM")
		}
	}

	// Generic recovery actions
	actions = append(actions, "Review system logs for additional error details")
	actions = append(actions, "Restart the microVM service if issues persist")

	return actions
}

// Recovery methods for specific error types

func (eh *ErrorHandler) recoverFromVMCreationError(ctx context.Context, microvmID string, err error) error {
	// Attempt to clean up any partial VM state
	// This would typically involve calling cleanup methods on various managers
	return fmt.Errorf("VM creation recovery not yet implemented: %w", err)
}

func (eh *ErrorHandler) recoverFromNetworkError(ctx context.Context, microvmID string, err error) error {
	// Attempt to reset network configuration
	// This might involve recreating bridges, resetting iptables rules, etc.
	return fmt.Errorf("network recovery not yet implemented: %w", err)
}

func (eh *ErrorHandler) recoverFromStorageError(ctx context.Context, microvmID string, err error) error {
	// Attempt to recover storage state
	// This might involve unmounting filesystems, cleaning up loop devices, etc.
	return fmt.Errorf("storage recovery not yet implemented: %w", err)
}

func (eh *ErrorHandler) recoverFromBootError(ctx context.Context, microvmID string, err error) error {
	// Attempt to recover from boot failures
	// This might involve restarting the VM, checking console output, etc.
	return fmt.Errorf("boot recovery not yet implemented: %w", err)
}

// recoverFromNetworkInterfaceError attempts to recover from network interface creation failures
func (eh *ErrorHandler) recoverFromNetworkInterfaceError(ctx context.Context, microvmID string, err error) error {
	errorMsg := err.Error()

	// Handle TAP interface creation failures
	if containsSubstring(errorMsg, "tap") || containsSubstring(errorMsg, "interface") {
		// Try to clean up any existing TAP interface
		tapName := fmt.Sprintf("tap%d", hashString(microvmID)%1000)
		if cleanupErr := executeCommand("sudo", "ip", "link", "delete", tapName); cleanupErr == nil {
			// Wait a moment for cleanup to complete
			time.Sleep(1 * time.Second)

			// Try to recreate the interface
			if createErr := executeCommand("sudo", "ip", "tuntap", "add", "dev", tapName, "mode", "tap"); createErr == nil {
				if upErr := executeCommand("sudo", "ip", "link", "set", "dev", tapName, "up"); upErr == nil {
					return nil // Successfully recovered
				}
			}
		}
	}

	// Handle bridge attachment failures
	if containsSubstring(errorMsg, "bridge") {
		// Try to ensure bridge exists and retry attachment
		bridgeName := "fc-br0"
		if bridgeErr := eh.ensureBridgeExists(bridgeName); bridgeErr == nil {
			// Retry the original operation after ensuring bridge exists
			return nil
		}
	}

	return fmt.Errorf("network interface recovery failed: %w", err)
}

// recoverFromCleanupError attempts to recover from cleanup failures
func (eh *ErrorHandler) recoverFromCleanupError(ctx context.Context, microvmID string, err error) error {
	errorMsg := err.Error()

	// Handle process cleanup failures
	if containsSubstring(errorMsg, "process") || containsSubstring(errorMsg, "pid") {
		// Force kill any remaining processes
		if killErr := eh.forceKillVMProcesses(microvmID); killErr == nil {
			return nil
		}
	}

	// Handle file cleanup failures
	if containsSubstring(errorMsg, "file") || containsSubstring(errorMsg, "directory") {
		// Force remove files with elevated privileges
		vmDir := fmt.Sprintf("/tmp/microvmsvc/%s", microvmID)
		if removeErr := executeCommand("sudo", "rm", "-rf", vmDir); removeErr == nil {
			return nil
		}
	}

	// Handle network cleanup failures
	if containsSubstring(errorMsg, "network") || containsSubstring(errorMsg, "tap") {
		// Force cleanup network interfaces
		if netErr := eh.forceCleanupNetworkInterfaces(microvmID); netErr == nil {
			return nil
		}
	}

	return fmt.Errorf("cleanup recovery failed: %w", err)
}

// Helper methods for recovery operations

func (eh *ErrorHandler) ensureBridgeExists(bridgeName string) error {
	// Check if bridge exists
	if err := executeCommand("ip", "link", "show", bridgeName); err == nil {
		return nil // Bridge already exists
	}

	// Create bridge
	if err := executeCommand("sudo", "ip", "link", "add", "name", bridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Assign IP to bridge
	if err := executeCommand("sudo", "ip", "addr", "add", "192.168.100.1/24", "dev", bridgeName); err != nil {
		return fmt.Errorf("failed to assign IP to bridge: %w", err)
	}

	// Bring bridge up
	if err := executeCommand("sudo", "ip", "link", "set", "dev", bridgeName, "up"); err != nil {
		return fmt.Errorf("failed to bring bridge up: %w", err)
	}

	return nil
}

func (eh *ErrorHandler) forceKillVMProcesses(microvmID string) error {
	// Find and kill firecracker processes for this VM
	cmd := fmt.Sprintf("pgrep -f 'firecracker.*%s' | xargs -r sudo kill -9", microvmID)
	return executeCommand("sh", "-c", cmd)
}

func (eh *ErrorHandler) forceCleanupNetworkInterfaces(microvmID string) error {
	tapName := fmt.Sprintf("tap%d", hashString(microvmID)%1000)

	// Remove TAP interface
	executeCommand("sudo", "ip", "link", "delete", tapName)

	// Remove any iptables rules related to this VM
	cmd := fmt.Sprintf("sudo iptables -t nat -D PREROUTING -p tcp --dport 22%s -j DNAT --to-destination 192.168.100.%d:22 2>/dev/null || true",
		microvmID[len(microvmID)-2:], hashString(microvmID)%100+10)
	executeCommand("sh", "-c", cmd)

	return nil
}

func hashString(s string) int {
	hash := 0
	for _, c := range s {
		hash = hash*31 + int(c)
	}
	if hash < 0 {
		hash = -hash
	}
	return hash
}

// Helper functions (these would typically be in a separate utils package)

func containsSubstring(s, substr string) bool {
	return strings.Contains(s, substr)
}

func createDirectory(path string) error {
	return executeCommand("mkdir", "-p", path)
}

func writeFile(path, content string) error {
	return executeCommand("sh", "-c", fmt.Sprintf("echo '%s' > %s", content, path))
}

func readFile(path string) (string, error) {
	return executeCommandWithOutput("cat", path)
}

func marshalJSON(v interface{}) ([]byte, error) {
	// This would use json.Marshal in a real implementation
	return []byte("{}"), nil
}

func executeCommand(name string, args ...string) error {
	_, err := executeCommandWithOutput(name, args...)
	return err
}

func executeCommandWithOutput(name string, args ...string) (string, error) {
	// This would use exec.Command in a real implementation
	// For now, return empty string and nil error as placeholder
	return "", nil
}
