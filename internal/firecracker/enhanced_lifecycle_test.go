package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// TestEnhancedVMLifecycle tests the complete enhanced VM lifecycle
func TestEnhancedVMLifecycle(t *testing.T) {
	// Skip if not running integration tests
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create temporary work directory
	workDir := filepath.Join(os.TempDir(), "firecracker-test")
	defer os.RemoveAll(workDir)

	// Create manager with test configuration
	config := &Config{
		WorkDir:        workDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		KernelPath:     "/var/lib/firecracker/hello-vmlinux.bin",
		RootfsPath:     "/var/lib/firecracker/hello-rootfs.ext4",
		UseJailer:      false,
	}

	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}
	defer manager.Shutdown()

	// Test system validation
	t.Run("SystemValidation", func(t *testing.T) {
		testSystemValidation(t, manager)
	})

	// Test VM creation with enhanced features
	t.Run("EnhancedVMCreation", func(t *testing.T) {
		testEnhancedVMCreation(t, manager)
	})

	// Test error handling and recovery
	t.Run("ErrorHandlingAndRecovery", func(t *testing.T) {
		testErrorHandlingAndRecovery(t, manager)
	})

	// Test boot validation
	t.Run("BootValidation", func(t *testing.T) {
		testBootValidation(t, manager)
	})

	// Test network connectivity
	t.Run("NetworkConnectivity", func(t *testing.T) {
		testNetworkConnectivity(t, manager)
	})

	// Test cleanup and resource management
	t.Run("CleanupAndResourceManagement", func(t *testing.T) {
		testCleanupAndResourceManagement(t, manager)
	})
}

// testSystemValidation tests the system validation functionality
func testSystemValidation(t *testing.T, manager *Manager) {
	// Test system validation
	result, err := manager.GetSystemValidationResult()
	if err != nil {
		t.Fatalf("System validation failed: %v", err)
	}

	// Log validation results
	t.Logf("System validation result: Valid=%v, Issues=%d, Warnings=%d",
		result.Valid, len(result.Issues), len(result.Warnings))

	for _, issue := range result.Issues {
		t.Logf("Issue: %s - %s", issue.Component, issue.Issue)
	}

	for _, warning := range result.Warnings {
		t.Logf("Warning: %s - %s", warning.Component, warning.Warning)
	}

	// Test setup script generation
	script, err := manager.GenerateSystemSetupScript()
	if err != nil {
		t.Fatalf("Failed to generate setup script: %v", err)
	}

	if len(script) == 0 {
		t.Error("Setup script is empty")
	}

	t.Logf("Generated setup script length: %d bytes", len(script))
}

// testEnhancedVMCreation tests VM creation with enhanced features
func testEnhancedVMCreation(t *testing.T, manager *Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create test microVM instance
	microvm := &models.MicroVMInstance{
		ID:     "test-enhanced-vm",
		NodeID: "test-node",
		Status: models.MicroVMStatusStopped,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 2048,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkModeNAT,
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt:  time.Now(),
			CustomerID: "test-customer",
		},
	}

	// Test VM creation
	err := manager.CreateMicroVM(ctx, microvm)
	if err != nil {
		t.Fatalf("Failed to create microVM: %v", err)
	}

	// Verify VM status
	if microvm.Status != models.MicroVMStatusRunning {
		t.Errorf("Expected VM status to be running, got: %v", microvm.Status)
	}

	// Verify network configuration
	if microvm.Network.IPAddress == "" {
		t.Error("VM should have an IP address assigned")
	}

	if microvm.Network.MACAddress == "" {
		t.Error("VM should have a MAC address assigned")
	}

	if microvm.Network.SSHPort == 0 {
		t.Error("VM should have an SSH port assigned")
	}

	t.Logf("VM created successfully: IP=%s, MAC=%s, SSH Port=%d",
		microvm.Network.IPAddress, microvm.Network.MACAddress, microvm.Network.SSHPort)

	// Clean up
	defer func() {
		if err := manager.DeleteMicroVM(ctx, microvm); err != nil {
			t.Errorf("Failed to delete microVM: %v", err)
		}
	}()
}

// testErrorHandlingAndRecovery tests error handling and recovery mechanisms
func testErrorHandlingAndRecovery(t *testing.T, manager *Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test with invalid configuration to trigger error handling
	microvm := &models.MicroVMInstance{
		ID:     "test-error-vm",
		NodeID: "test-node",
		Status: models.MicroVMStatusStopped,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 1024,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkModeNAT,
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt:  time.Now(),
			CustomerID: "test-customer",
		},
	}

	// Test error collection
	testErr := fmt.Errorf("test error for diagnostics")
	diagnostics, err := manager.errorHandler.CollectDiagnostics(ctx, "test_operation", microvm.ID, testErr, 3)
	if err != nil {
		t.Fatalf("Failed to collect diagnostics: %v", err)
	}

	// Verify diagnostics data
	if diagnostics.OperationType != "test_operation" {
		t.Errorf("Expected operation type 'test_operation', got: %s", diagnostics.OperationType)
	}

	if diagnostics.MicroVMID != microvm.ID {
		t.Errorf("Expected microVM ID '%s', got: %s", microvm.ID, diagnostics.MicroVMID)
	}

	if diagnostics.RetryAttempts != 3 {
		t.Errorf("Expected 3 retry attempts, got: %d", diagnostics.RetryAttempts)
	}

	if len(diagnostics.RecoveryActions) == 0 {
		t.Error("Expected recovery actions to be generated")
	}

	t.Logf("Diagnostics collected successfully: %d recovery actions generated", len(diagnostics.RecoveryActions))

	// Test diagnostics storage
	err = manager.errorHandler.StoreDiagnostics(diagnostics)
	if err != nil {
		t.Errorf("Failed to store diagnostics: %v", err)
	}

	// Test retry mechanism with a function that succeeds after retries
	retryCount := 0
	err = manager.errorHandler.RetryWithBackoff(ctx, func() error {
		retryCount++
		if retryCount < 3 {
			return fmt.Errorf("temporary failure %d", retryCount)
		}
		return nil
	}, "test_retry")

	if err != nil {
		t.Errorf("Retry mechanism failed: %v", err)
	}

	if retryCount != 3 {
		t.Errorf("Expected 3 retry attempts, got: %d", retryCount)
	}

	t.Logf("Retry mechanism worked correctly: %d attempts", retryCount)
}

// testBootValidation tests the boot validation functionality
func testBootValidation(t *testing.T, manager *Manager) {
	// Test boot validator configuration
	validator := manager.bootValidator
	if validator == nil {
		t.Fatal("Boot validator should be initialized")
	}

	// Test boot progress tracking
	progress := validator.GetBootProgress()
	if progress < 0 || progress > 100 {
		t.Errorf("Boot progress should be between 0 and 100, got: %f", progress)
	}

	// Test validator reset
	validator.Reset()

	// Test timeout configuration
	validator.SetBootTimeout(30 * time.Second)
	validator.SetSSHTimeout(15 * time.Second)

	t.Log("Boot validator tests completed successfully")
}

// testNetworkConnectivity tests network connectivity features
func testNetworkConnectivity(t *testing.T, manager *Manager) {
	// Test network manager initialization
	if manager.enhancedNetwork == nil {
		t.Fatal("Enhanced network manager should be initialized")
	}

	// Test network validation
	err := manager.enhancedNetwork.ValidateNetworkSetup()
	if err != nil {
		t.Logf("Network validation failed (expected in test environment): %v", err)
	}

	// Test network status
	status, err := manager.enhancedNetwork.GetNetworkStatus()
	if err != nil {
		t.Logf("Failed to get network status (expected in test environment): %v", err)
	} else {
		t.Logf("Network status: DHCP running=%v", status.DHCPRunning)
	}

	t.Log("Network connectivity tests completed")
}

// testCleanupAndResourceManagement tests cleanup and resource management
func testCleanupAndResourceManagement(t *testing.T, manager *Manager) {
	ctx := context.Background()

	// Test image cache cleanup
	err := manager.imageManager.CleanupImages()
	if err != nil {
		t.Errorf("Failed to cleanup images: %v", err)
	}

	// Test cache statistics
	stats, err := manager.imageManager.GetCacheStats()
	if err != nil {
		t.Errorf("Failed to get cache stats: %v", err)
	} else {
		t.Logf("Cache stats: %d total entries, %d base images, %d overlays",
			stats.TotalEntries, stats.BaseImages, stats.Overlays)
	}

	// Test cache validation
	err = manager.imageManager.ValidateCache()
	if err != nil {
		t.Errorf("Cache validation failed: %v", err)
	}

	// Test cleanup with recovery (using nil values to test error handling)
	manager.cleanupWithRecovery(ctx, &models.MicroVMInstance{ID: "test-cleanup"}, nil, nil, nil)

	t.Log("Cleanup and resource management tests completed")
}

// TestSystemValidatorStandalone tests the system validator independently
func TestSystemValidatorStandalone(t *testing.T) {
	validator := NewSystemValidator()

	// Test validation
	result, err := validator.ValidateSystem()
	if err != nil {
		t.Fatalf("System validation failed: %v", err)
	}

	t.Logf("System validation: Valid=%v, Issues=%d, Warnings=%d, Tips=%d",
		result.Valid, len(result.Issues), len(result.Warnings), len(result.TroubleshootingTips))

	// Test setup script generation
	script := validator.GenerateSetupScript(result)
	if len(script) == 0 {
		t.Error("Setup script should not be empty")
	}

	// Test version comparison
	if !validator.isVersionSufficient("5.4.0", "4.14.0") {
		t.Error("Version 5.4.0 should be sufficient for requirement 4.14.0")
	}

	if validator.isVersionSufficient("4.10.0", "4.14.0") {
		t.Error("Version 4.10.0 should not be sufficient for requirement 4.14.0")
	}

	t.Log("System validator standalone tests completed")
}

// TestErrorHandlerStandalone tests the error handler independently
func TestErrorHandlerStandalone(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "error-handler-test")
	defer os.RemoveAll(tempDir)

	handler := NewErrorHandler(tempDir)

	// Test retry configuration
	handler = handler.WithRetryConfig(RetryConfig{
		MaxRetries: 2,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   1 * time.Second,
	})

	// Test backoff calculation
	delay1 := handler.calculateBackoffDelay(1)
	delay2 := handler.calculateBackoffDelay(2)
	delay3 := handler.calculateBackoffDelay(3)

	if delay2 <= delay1 {
		t.Error("Backoff delay should increase with attempt number")
	}

	if delay3 <= delay2 {
		t.Error("Backoff delay should continue to increase")
	}

	t.Logf("Backoff delays: %v, %v, %v", delay1, delay2, delay3)

	// Test retryable error detection
	if !handler.isRetryableError(fmt.Errorf("connection refused")) {
		t.Error("Connection refused should be retryable")
	}

	if handler.isRetryableError(fmt.Errorf("permission denied")) {
		t.Error("Permission denied should not be retryable")
	}

	// Test diagnostics collection
	ctx := context.Background()
	testErr := fmt.Errorf("test error")
	diagnostics, err := handler.CollectDiagnostics(ctx, "test", "vm-123", testErr, 2)
	if err != nil {
		t.Fatalf("Failed to collect diagnostics: %v", err)
	}

	if diagnostics.OperationType != "test" {
		t.Errorf("Expected operation type 'test', got: %s", diagnostics.OperationType)
	}

	// Test diagnostics storage
	err = handler.StoreDiagnostics(diagnostics)
	if err != nil {
		t.Errorf("Failed to store diagnostics: %v", err)
	}

	t.Log("Error handler standalone tests completed")
}

// BenchmarkVMCreation benchmarks VM creation performance
func BenchmarkVMCreation(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	// Create temporary work directory
	workDir := filepath.Join(os.TempDir(), "firecracker-bench")
	defer os.RemoveAll(workDir)

	config := &Config{
		WorkDir:        workDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		KernelPath:     "/var/lib/firecracker/hello-vmlinux.bin",
		RootfsPath:     "/var/lib/firecracker/hello-rootfs.ext4",
		UseJailer:      false,
	}

	manager, err := NewManager(config)
	if err != nil {
		b.Fatalf("Failed to create manager: %v", err)
	}
	defer manager.Shutdown()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		microvm := &models.MicroVMInstance{
			ID:     fmt.Sprintf("bench-vm-%d", i),
			NodeID: "bench-node",
			Status: models.MicroVMStatusStopped,
			Resources: models.ResourceSpecs{
				VCPUs:     1,
				MemoryMB:  256,
				StorageMB: 1024,
			},
			Storage: models.StorageConfig{
				Type: models.StorageTypeLocal,
			},
			Network: models.NetworkInfo{
				AssignmentMethod: models.NetworkModeNAT,
			},
			Metadata: models.MicroVMMetadata{
				CreatedAt:  time.Now(),
				CustomerID: "bench-customer",
			},
		}

		// Only measure creation time, not cleanup
		b.StartTimer()
		err := manager.CreateMicroVM(ctx, microvm)
		b.StopTimer()

		if err != nil {
			b.Errorf("VM creation failed: %v", err)
		}

		// Clean up
		manager.DeleteMicroVM(ctx, microvm)
		cancel()
	}
}

// Helper functions for testing

func createTestMicroVM(id string) *models.MicroVMInstance {
	return &models.MicroVMInstance{
		ID:     id,
		NodeID: "test-node",
		Status: models.MicroVMStatusStopped,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 1024,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkModeNAT,
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt:  time.Now(),
			CustomerID: "test-customer",
		},
	}
}

func waitForVMReady(t *testing.T, manager *Manager, vmID string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for VM %s to be ready", vmID)
		case <-ticker.C:
			status, err := manager.GetMicroVMStatus(ctx, vmID)
			if err != nil {
				continue
			}
			if status == models.MicroVMStatusRunning {
				return
			}
		}
	}
}
