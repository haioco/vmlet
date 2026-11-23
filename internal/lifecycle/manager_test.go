package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// Mock implementations are in test_utils.go

// Test functions

func TestNewManager(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	storage := newMockStorage()
	config := DefaultConfig()

	manager, err := NewManager(microvmManager, storage, config)
	if err != nil {
		t.Fatalf("Failed to create lifecycle manager: %v", err)
	}

	if manager == nil {
		t.Fatal("Manager should not be nil")
	}

	if manager.config != config {
		t.Error("Config not set correctly")
	}
}

func TestNewManagerWithNilParams(t *testing.T) {
	config := DefaultConfig()

	// Test with nil microvmManager
	_, err := NewManager(nil, newMockStorage(), config)
	if err == nil {
		t.Error("Expected error with nil microvmManager")
	}

	// Test with nil storage
	_, err = NewManager(newMockMicroVMManager(), nil, config)
	if err == nil {
		t.Error("Expected error with nil storage")
	}

	// Test with nil config (should use default)
	manager, err := NewManager(newMockMicroVMManager(), newMockStorage(), nil)
	if err != nil {
		t.Fatalf("Failed to create manager with nil config: %v", err)
	}
	if manager.config == nil {
		t.Error("Config should be set to default when nil is passed")
	}
}

func TestRegisterAndUnregisterMicroVM(t *testing.T) {
	manager, err := NewManager(newMockMicroVMManager(), newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Test registration
	err = manager.RegisterMicroVM(microvm)
	if err != nil {
		t.Fatalf("Failed to register microVM: %v", err)
	}

	// Verify registration
	manager.mu.RLock()
	registered, exists := manager.runningMicroVMs[microvm.ID]
	manager.mu.RUnlock()

	if !exists {
		t.Error("MicroVM not found in running microVMs")
	}
	if registered.ID != microvm.ID {
		t.Error("Registered microVM ID mismatch")
	}

	// Test unregistration
	err = manager.UnregisterMicroVM(microvm.ID)
	if err != nil {
		t.Fatalf("Failed to unregister microVM: %v", err)
	}

	// Verify unregistration
	manager.mu.RLock()
	_, exists = manager.runningMicroVMs[microvm.ID]
	manager.mu.RUnlock()

	if exists {
		t.Error("MicroVM should not exist after unregistration")
	}
}

func TestRegisterMicroVMWithNil(t *testing.T) {
	manager, err := NewManager(newMockMicroVMManager(), newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	err = manager.RegisterMicroVM(nil)
	if err == nil {
		t.Error("Expected error when registering nil microVM")
	}
}

func TestUnregisterMicroVMWithEmptyID(t *testing.T) {
	manager, err := NewManager(newMockMicroVMManager(), newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	err = manager.UnregisterMicroVM("")
	if err == nil {
		t.Error("Expected error when unregistering with empty ID")
	}
}

func TestGracefulShutdown(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	manager, err := NewManager(microvmManager, newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Register the microVM
	err = manager.RegisterMicroVM(microvm)
	if err != nil {
		t.Fatalf("Failed to register microVM: %v", err)
	}

	// Test graceful shutdown
	ctx := context.Background()
	err = manager.GracefulShutdown(ctx, microvm.ID)
	if err != nil {
		t.Fatalf("Failed to gracefully shutdown microVM: %v", err)
	}

	// Verify the microVM was stopped
	status, err := microvmManager.GetMicroVMStatus(ctx, microvm.ID)
	if err != nil {
		t.Fatalf("Failed to get microVM status: %v", err)
	}
	if status != models.MicroVMStatusStopped {
		t.Errorf("Expected microVM status to be stopped, got %s", status)
	}
}

func TestRestartMicroVM(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	manager, err := NewManager(microvmManager, newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Register the microVM
	err = manager.RegisterMicroVM(microvm)
	if err != nil {
		t.Fatalf("Failed to register microVM: %v", err)
	}

	// Test restart
	ctx := context.Background()
	err = manager.RestartMicroVM(ctx, microvm.ID)
	if err != nil {
		t.Fatalf("Failed to restart microVM: %v", err)
	}

	// Verify the microVM is running again
	status, err := microvmManager.GetMicroVMStatus(ctx, microvm.ID)
	if err != nil {
		t.Fatalf("Failed to get microVM status: %v", err)
	}
	if status != models.MicroVMStatusRunning {
		t.Errorf("Expected microVM status to be running after restart, got %s", status)
	}
}

func TestTriggerRecovery(t *testing.T) {
	microvmManager := newMockMicroVMManager()
	manager, err := NewManager(microvmManager, newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	microvm := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	// Register the microVM
	err = manager.RegisterMicroVM(microvm)
	if err != nil {
		t.Fatalf("Failed to register microVM: %v", err)
	}

	// Test trigger recovery
	ctx := context.Background()
	err = manager.TriggerRecovery(ctx, microvm.ID)
	if err != nil {
		t.Fatalf("Failed to trigger recovery: %v", err)
	}

	// Give some time for recovery to start
	time.Sleep(100 * time.Millisecond)

	// Verify recovery status exists
	status, err := manager.recoveryManager.GetRecoveryStatus(microvm.ID)
	if err != nil {
		t.Fatalf("Failed to get recovery status: %v", err)
	}
	if status.MicroVMID != microvm.ID {
		t.Error("Recovery status microVM ID mismatch")
	}
}

func TestLoadRunningMicroVMs(t *testing.T) {
	storage := newMockStorage()
	manager, err := NewManager(newMockMicroVMManager(), storage, DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	// Add some running microVMs to storage
	microvm1 := &models.MicroVMInstance{
		ID:     "test-microvm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusRunning,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}
	microvm2 := &models.MicroVMInstance{
		ID:     "test-microvm-2",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusStopped,
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
	}

	storage.StoreMicroVM(context.Background(), microvm1)
	storage.StoreMicroVM(context.Background(), microvm2)

	// Load running microVMs
	ctx := context.Background()
	err = manager.loadRunningMicroVMs(ctx)
	if err != nil {
		t.Fatalf("Failed to load running microVMs: %v", err)
	}

	// Verify only running microVMs were loaded
	manager.mu.RLock()
	count := len(manager.runningMicroVMs)
	_, exists1 := manager.runningMicroVMs[microvm1.ID]
	_, exists2 := manager.runningMicroVMs[microvm2.ID]
	manager.mu.RUnlock()

	if count != 1 {
		t.Errorf("Expected 1 running microVM, got %d", count)
	}
	if !exists1 {
		t.Error("Running microVM should be loaded")
	}
	if exists2 {
		t.Error("Stopped microVM should not be loaded")
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config == nil {
		t.Fatal("Default config should not be nil")
	}

	// Test some default values
	if config.HealthCheckInterval != 30*time.Second {
		t.Error("Default health check interval incorrect")
	}
	if config.MetricsInterval != 60*time.Second {
		t.Error("Default metrics interval incorrect")
	}
	if config.IdleThreshold != 24*time.Hour {
		t.Error("Default idle threshold incorrect")
	}
	if config.MaxRecoveryAttempts != 3 {
		t.Error("Default max recovery attempts incorrect")
	}
}

func TestManagerStartStop(t *testing.T) {
	manager, err := NewManager(newMockMicroVMManager(), newMockStorage(), DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Start the manager
	err = manager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}

	// Let it run briefly
	time.Sleep(100 * time.Millisecond)

	// Stop the manager
	err = manager.Stop()
	if err != nil {
		t.Fatalf("Failed to stop manager: %v", err)
	}
}