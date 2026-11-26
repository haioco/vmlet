package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/haioco/vmlet/internal/models"
)

func TestNewManager(t *testing.T) {
	// Create temporary directory for testing
	tempDir := filepath.Join(os.TempDir(), "firecracker-test")
	defer os.RemoveAll(tempDir)

	config := &Config{
		WorkDir:        tempDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		KernelPath:     "/opt/firecracker/vmlinux",
		UseJailer:      false,
	}

	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	if manager == nil {
		t.Fatal("Manager should not be nil")
	}

	if manager.workDir != tempDir {
		t.Errorf("Expected work dir %s, got %s", tempDir, manager.workDir)
	}
}

func TestCreateMicroVMValidation(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "firecracker-test")
	defer os.RemoveAll(tempDir)

	config := &Config{
		WorkDir:        tempDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		UseJailer:      false,
	}

	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	ctx := context.Background()

	// Test with nil microVM
	err = manager.CreateMicroVM(ctx, nil)
	if err == nil {
		t.Error("Expected error for nil microVM")
	}

	// Test with valid microVM (this will fail due to missing firecracker binary, but validates the structure)
	microvm := &models.MicroVMInstance{
		ID:     uuid.New().String(),
		NodeID: "test-node",
		Status: models.MicroVMStatusCreating,
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

	// This will fail because firecracker binary doesn't exist, but it validates our structure
	err = manager.CreateMicroVM(ctx, microvm)
	if err == nil {
		t.Error("Expected error due to missing firecracker binary")
	}
	// The error should be about missing firecracker, not about validation
	if microvm.Status != models.MicroVMStatusError {
		t.Error("MicroVM status should be set to error on failure")
	}
}

func TestGetMicroVMStatus(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "firecracker-test")
	defer os.RemoveAll(tempDir)

	config := &Config{
		WorkDir:        tempDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		UseJailer:      false,
	}

	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	ctx := context.Background()

	// Test with empty microVM ID
	_, err = manager.GetMicroVMStatus(ctx, "")
	if err == nil {
		t.Error("Expected error for empty microVM ID")
	}

	// Test with non-existent microVM
	status, err := manager.GetMicroVMStatus(ctx, "non-existent")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if status != models.MicroVMStatusStopped {
		t.Errorf("Expected status %s, got %s", models.MicroVMStatusStopped, status)
	}
}

func TestConfigValidation(t *testing.T) {
	// Test nil config
	err := ValidateConfig(nil)
	if err == nil {
		t.Error("Expected error for nil config")
	}

	// Test empty work dir
	config := &Config{}
	err = ValidateConfig(config)
	if err == nil {
		t.Error("Expected error for empty work dir")
	}

	// Test empty firecracker bin
	config.WorkDir = "/tmp"
	err = ValidateConfig(config)
	if err == nil {
		t.Error("Expected error for empty firecracker bin")
	}

	// Test jailer enabled but no jailer bin
	config.FirecrackerBin = "/usr/local/bin/firecracker"
	config.UseJailer = true
	err = ValidateConfig(config)
	if err == nil {
		t.Error("Expected error for missing jailer bin when jailer is enabled")
	}

	// Test valid config
	config.JailerBin = "/usr/local/bin/jailer"
	err = ValidateConfig(config)
	if err != nil {
		t.Errorf("Unexpected error for valid config: %v", err)
	}
}

func TestDefaultConfigs(t *testing.T) {
	defaultConfig := DefaultConfig()
	if defaultConfig == nil {
		t.Fatal("Default config should not be nil")
	}

	if err := ValidateConfig(defaultConfig); err != nil {
		t.Errorf("Default config should be valid: %v", err)
	}

	localConfig := LocalDevConfig()
	if localConfig == nil {
		t.Fatal("Local dev config should not be nil")
	}

	if err := ValidateConfig(localConfig); err != nil {
		t.Errorf("Local dev config should be valid: %v", err)
	}

	// Local dev config should use relative paths
	if localConfig.WorkDir != "./firecracker-work" {
		t.Errorf("Expected local work dir './firecracker-work', got %s", localConfig.WorkDir)
	}
}
