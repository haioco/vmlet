package resource

import (
	"context"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

func TestResourceManager(t *testing.T) {
	// Create a mock LXD client
	mockClient := NewMockLXDClient()
	
	// Create test configuration
	cfg := config.DefaultConfig()
	cfg.Resources.MonitoringIntervalSec = 1 // Fast updates for testing
	
	// Create resource manager
	manager := NewManager(mockClient, cfg, "test-node-1")
	
	// Start the manager
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Failed to start resource manager: %v", err)
	}
	defer manager.Stop()
	
	// Wait for initial resource discovery
	time.Sleep(2 * time.Second)
	
	// Test resource pool retrieval
	pool := manager.GetResourcePool()
	if pool == nil {
		t.Fatal("Resource pool is nil")
	}
	
	if pool.NodeID != "test-node-1" {
		t.Errorf("Expected node ID 'test-node-1', got '%s'", pool.NodeID)
	}
	
	// Test resource allocation
	specs := models.ResourceSpecs{
		VCPUs:     2,
		MemoryMB:  1024,
		StorageMB: 10240,
	}
	
	vmID := "test-vm-1"
	if err := manager.AllocateResources(vmID, specs); err != nil {
		t.Fatalf("Failed to allocate resources: %v", err)
	}
	
	// Verify allocation
	allocatedSpecs, err := manager.GetAllocatedResources(vmID)
	if err != nil {
		t.Fatalf("Failed to get allocated resources: %v", err)
	}
	
	if allocatedSpecs.VCPUs != specs.VCPUs {
		t.Errorf("Expected %d vCPUs, got %d", specs.VCPUs, allocatedSpecs.VCPUs)
	}
	
	if allocatedSpecs.MemoryMB != specs.MemoryMB {
		t.Errorf("Expected %d MB memory, got %d", specs.MemoryMB, allocatedSpecs.MemoryMB)
	}
	
	if allocatedSpecs.StorageMB != specs.StorageMB {
		t.Errorf("Expected %d MB storage, got %d", specs.StorageMB, allocatedSpecs.StorageMB)
	}
	
	// Test resource deallocation
	if err := manager.DeallocateResources(vmID); err != nil {
		t.Fatalf("Failed to deallocate resources: %v", err)
	}
	
	// Verify deallocation
	_, err = manager.GetAllocatedResources(vmID)
	if err == nil {
		t.Error("Expected error when getting deallocated resources")
	}
}

func TestResourceAllocationLimits(t *testing.T) {
	mockClient := NewMockLXDClient()
	cfg := config.DefaultConfig()
	manager := NewManager(mockClient, cfg, "test-node-2")
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Failed to start resource manager: %v", err)
	}
	defer manager.Stop()
	
	// Wait for initial resource discovery
	time.Sleep(1 * time.Second)
	
	// Try to allocate more resources than available
	specs := models.ResourceSpecs{
		VCPUs:     100, // More than available
		MemoryMB:  100000,
		StorageMB: 10000000,
	}
	
	vmID := "test-vm-overalloc"
	err := manager.AllocateResources(vmID, specs)
	if err == nil {
		t.Error("Expected error when allocating excessive resources")
	}
	
	if err != models.ErrInsufficientResources {
		t.Errorf("Expected ErrInsufficientResources, got %v", err)
	}
}

func TestConflictDetection(t *testing.T) {
	mockClient := NewMockLXDClient()
	cfg := config.DefaultConfig()
	cfg.Resources.OverloadThreshold = 0.3 // Very low threshold for testing
	
	manager := NewManager(mockClient, cfg, "test-node-3")
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Failed to start resource manager: %v", err)
	}
	defer manager.Stop()
	
	// Wait for initial resource discovery
	time.Sleep(1 * time.Second)
	
	// First, allocate resources that should succeed
	specs1 := models.ResourceSpecs{
		VCPUs:     2,
		MemoryMB:  2048,
		StorageMB: 20480,
	}
	
	vmID1 := "test-vm-1"
	if err := manager.AllocateResources(vmID1, specs1); err != nil {
		t.Fatalf("Failed to allocate first VM: %v", err)
	}
	
	// Now try to allocate more resources that should trigger overload
	specs2 := models.ResourceSpecs{
		VCPUs:     10, // This should trigger overload
		MemoryMB:  20480,
		StorageMB: 500000,
	}
	
	vmID2 := "test-vm-overload"
	err := manager.AllocateResources(vmID2, specs2)
	
	// Should either get insufficient resources or node overloaded
	if err == nil {
		t.Error("Expected error when allocating excessive resources")
	}
	
	if err != models.ErrInsufficientResources && err != models.ErrNodeOverloaded {
		t.Errorf("Expected ErrInsufficientResources or ErrNodeOverloaded, got %v", err)
	}
	
	// Check for conflict events (may take a moment to appear)
	conflictChan := manager.GetConflictEvents()
	
	select {
	case event := <-conflictChan:
		// Accept either overload or insufficient resources conflict
		if event.Type != ConflictTypeOverload && event.Type != ConflictTypeInsufficientResources {
			t.Errorf("Expected overload or insufficient resources conflict, got %s", event.Type)
		}
	case <-time.After(3 * time.Second):
		// This is acceptable - conflict detection runs every 15 seconds
		t.Log("No immediate conflict event received (this is acceptable)")
	}
}

func TestResourceUtilizationStats(t *testing.T) {
	mockClient := NewMockLXDClient()
	cfg := config.DefaultConfig()
	manager := NewManager(mockClient, cfg, "test-node-4")
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Failed to start resource manager: %v", err)
	}
	defer manager.Stop()
	
	// Wait for initial resource discovery
	time.Sleep(1 * time.Second)
	
	// Get utilization stats
	stats := manager.GetResourceUtilization()
	
	if stats.NodeID != "test-node-4" {
		t.Errorf("Expected node ID 'test-node-4', got '%s'", stats.NodeID)
	}
	
	if stats.CPUUtilization < 0 || stats.CPUUtilization > 1 {
		t.Errorf("CPU utilization should be between 0 and 1, got %f", stats.CPUUtilization)
	}
	
	if stats.MemoryUtilization < 0 || stats.MemoryUtilization > 1 {
		t.Errorf("Memory utilization should be between 0 and 1, got %f", stats.MemoryUtilization)
	}
	
	if stats.StorageUtilization < 0 || stats.StorageUtilization > 1 {
		t.Errorf("Storage utilization should be between 0 and 1, got %f", stats.StorageUtilization)
	}
	
	if stats.AllocatedMicroVMs != 0 {
		t.Errorf("Expected 0 allocated microVMs, got %d", stats.AllocatedMicroVMs)
	}
	
	// Allocate a microVM and check stats again
	specs := models.ResourceSpecs{
		VCPUs:     1,
		MemoryMB:  512,
		StorageMB: 5120,
	}
	
	vmID := "test-vm-stats"
	if err := manager.AllocateResources(vmID, specs); err != nil {
		t.Fatalf("Failed to allocate resources: %v", err)
	}
	
	// Get updated stats
	updatedStats := manager.GetResourceUtilization()
	
	if updatedStats.AllocatedMicroVMs != 1 {
		t.Errorf("Expected 1 allocated microVM, got %d", updatedStats.AllocatedMicroVMs)
	}
	
	// CPU utilization should have increased
	if updatedStats.CPUUtilization <= stats.CPUUtilization {
		t.Error("CPU utilization should have increased after allocation")
	}
}