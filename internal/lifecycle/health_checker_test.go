package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

func TestNewHealthChecker(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()

	hc := NewHealthChecker(microvmManager, config)
	if hc == nil {
		t.Fatal("Health checker should not be nil")
	}

	if hc.microvmManager != microvmManager {
		t.Error("MicroVM manager not set correctly")
	}
	if hc.config != config {
		t.Error("Config not set correctly")
	}
}

func TestCheckHealth(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Network: models.NetworkInfo{
			IPAddress: "192.168.1.100",
			SSHPort:   22,
		},
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Set up mock to return running status
	microvmManager.status[microvm.ID] = models.MicroVMStatusRunning

	ctx := context.Background()
	err := hc.CheckHealth(ctx, microvm)

	// Note: This test will fail network connectivity checks since we're not
	// actually running a microVM, but we can verify the process status check works
	if err == nil {
		t.Error("Expected health check to fail due to network connectivity")
	}

	// Verify health status was created
	status, err := hc.GetHealthStatus(microvm.ID)
	if err != nil {
		t.Fatalf("Failed to get health status: %v", err)
	}

	if status.MicroVMID != microvm.ID {
		t.Error("Health status microVM ID mismatch")
	}

	if len(status.HealthChecks) == 0 {
		t.Error("No health checks were performed")
	}

	// Verify process status check passed
	if processCheck, exists := status.HealthChecks["process_status"]; exists {
		if !processCheck.Success {
			t.Error("Process status check should have passed")
		}
	} else {
		t.Error("Process status check not found")
	}
}

func TestCheckHealthWithNilMicroVM(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	ctx := context.Background()
	err := hc.CheckHealth(ctx, nil)
	if err == nil {
		t.Error("Expected error when checking health of nil microVM")
	}
}

func TestGetHealthStatusNotFound(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	_, err := hc.GetHealthStatus("nonexistent-microvm")
	if err == nil {
		t.Error("Expected error when getting health status for nonexistent microVM")
	}
}

func TestGetUnhealthyMicroVMs(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	microvm1 := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Network: models.NetworkInfo{
			IPAddress: "192.168.1.100",
			SSHPort:   22,
		},
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	microvm2 := &models.MicroVMInstance{
		ID:     "test-microvm-2",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Network: models.NetworkInfo{
			IPAddress: "192.168.1.101",
			SSHPort:   22,
		},
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Set up mocks - one healthy, one unhealthy
	microvmManager.status[microvm1.ID] = models.MicroVMStatusRunning
	microvmManager.status[microvm2.ID] = models.MicroVMStatusError

	ctx := context.Background()

	// Check health for both microVMs
	hc.CheckHealth(ctx, microvm1)
	hc.CheckHealth(ctx, microvm2)

	// Get unhealthy microVMs
	unhealthy := hc.GetUnhealthyMicroVMs()

	// Both should be unhealthy due to network connectivity failures
	// but we can verify the function works
	if len(unhealthy) == 0 {
		t.Error("Expected some unhealthy microVMs")
	}
}

func TestIsHealthy(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	// Test with nonexistent microVM
	if hc.IsHealthy("nonexistent") {
		t.Error("Nonexistent microVM should not be healthy")
	}

	// Create a health status manually
	hc.mu.Lock()
	hc.healthStatus["test-microvm"] = &HealthStatus{
		MicroVMID: "test-microvm",
		IsHealthy: true,
	}
	hc.mu.Unlock()

	if !hc.IsHealthy("test-microvm") {
		t.Error("MicroVM should be healthy")
	}

	// Update to unhealthy
	hc.mu.Lock()
	hc.healthStatus["test-microvm"].IsHealthy = false
	hc.mu.Unlock()

	if hc.IsHealthy("test-microvm") {
		t.Error("MicroVM should not be healthy")
	}
}

func TestGetFailureCount(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	// Test with nonexistent microVM
	count := hc.GetFailureCount("nonexistent")
	if count != 0 {
		t.Error("Failure count for nonexistent microVM should be 0")
	}

	// Create a health status manually
	hc.mu.Lock()
	hc.healthStatus["test-microvm"] = &HealthStatus{
		MicroVMID:    "test-microvm",
		FailureCount: 5,
	}
	hc.mu.Unlock()

	count = hc.GetFailureCount("test-microvm")
	if count != 5 {
		t.Errorf("Expected failure count 5, got %d", count)
	}
}

func TestRemoveHealthStatus(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	// Create a health status manually
	hc.mu.Lock()
	hc.healthStatus["test-microvm"] = &HealthStatus{
		MicroVMID: "test-microvm",
		IsHealthy: true,
	}
	hc.unhealthyVMs["test-microvm"] = time.Now()
	hc.mu.Unlock()

	// Remove health status
	hc.RemoveHealthStatus("test-microvm")

	// Verify removal
	hc.mu.RLock()
	_, exists1 := hc.healthStatus["test-microvm"]
	_, exists2 := hc.unhealthyVMs["test-microvm"]
	hc.mu.RUnlock()

	if exists1 {
		t.Error("Health status should be removed")
	}
	if exists2 {
		t.Error("Unhealthy VM entry should be removed")
	}
}

func TestCheckProcessStatus(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
	}

	ctx := context.Background()

	// Test with running status
	microvmManager.status[microvm.ID] = models.MicroVMStatusRunning
	result := hc.checkProcessStatus(ctx, microvm)
	if !result.Success {
		t.Error("Process status check should succeed for running microVM")
	}
	if result.Name != "process_status" {
		t.Error("Check name should be 'process_status'")
	}

	// Test with stopped status
	microvmManager.status[microvm.ID] = models.MicroVMStatusStopped
	result = hc.checkProcessStatus(ctx, microvm)
	if result.Success {
		t.Error("Process status check should fail for stopped microVM")
	}
	if result.Error == "" {
		t.Error("Error message should be set for failed check")
	}
}

func TestCheckNetworkConnectivity(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	ctx := context.Background()

	// Test with no IP address
	microvm := &models.MicroVMInstance{
		ID:      "test-microvm-1",
		NodeID:  "test-node-1",
		Network: models.NetworkInfo{},
	}

	result := hc.checkNetworkConnectivity(ctx, microvm)
	if result.Success {
		t.Error("Network connectivity check should fail with no IP address")
	}
	if result.Name != "network_connectivity" {
		t.Error("Check name should be 'network_connectivity'")
	}

	// Test with invalid IP address (will fail to connect)
	microvm.Network.IPAddress = "192.168.1.100"
	result = hc.checkNetworkConnectivity(ctx, microvm)
	if result.Success {
		t.Error("Network connectivity check should fail for unreachable IP")
	}
}

func TestCheckSSHConnectivity(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	ctx := context.Background()

	// Test with no IP address
	microvm := &models.MicroVMInstance{
		ID:      "test-microvm-1",
		NodeID:  "test-node-1",
		Network: models.NetworkInfo{},
	}

	result := hc.checkSSHConnectivity(ctx, microvm)
	if result.Success {
		t.Error("SSH connectivity check should fail with no IP address")
	}
	if result.Name != "ssh_connectivity" {
		t.Error("Check name should be 'ssh_connectivity'")
	}

	// Test with invalid IP address (will fail to connect)
	microvm.Network.IPAddress = "192.168.1.100"
	microvm.Network.SSHPort = 22
	result = hc.checkSSHConnectivity(ctx, microvm)
	if result.Success {
		t.Error("SSH connectivity check should fail for unreachable IP")
	}

	// Test with default SSH port (0 should use 22)
	microvm.Network.SSHPort = 0
	result = hc.checkSSHConnectivity(ctx, microvm)
	if result.Success {
		t.Error("SSH connectivity check should fail for unreachable IP")
	}
}

func TestCheckResourceUsage(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	config := DefaultConfig()
	hc := NewHealthChecker(microvmManager, config)

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
	}

	ctx := context.Background()
	result := hc.checkResourceUsage(ctx, microvm)

	// This is currently a placeholder check that always succeeds
	if !result.Success {
		t.Error("Resource usage check should succeed (placeholder implementation)")
	}
	if result.Name != "resource_usage" {
		t.Error("Check name should be 'resource_usage'")
	}
}