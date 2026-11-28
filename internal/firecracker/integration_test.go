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

// TestFirecrackerIntegration tests the complete firecracker integration
func TestFirecrackerIntegration(t *testing.T) {
	// Skip this test if firecracker binary is not available
	if _, err := os.Stat("/usr/local/bin/firecracker"); os.IsNotExist(err) {
		t.Skip("Firecracker binary not found, skipping integration test")
	}

	// Create temporary directory for testing
	tempDir := filepath.Join(os.TempDir(), "firecracker-integration-test")
	defer os.RemoveAll(tempDir)

	config := &Config{
		WorkDir:        tempDir,
		FirecrackerBin: "/usr/local/bin/firecracker",
		KernelPath:     "./hello-vmlinux.bin", // Use local kernel if available
		UseJailer:      false,
	}

	// Validate config
	if err := ValidateConfig(config); err != nil {
		t.Fatalf("Config validation failed: %v", err)
	}

	// Create manager
	manager, err := NewManager(config, nil)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	ctx := context.Background()

	// Create test microVM
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

	// Test microVM creation (this will likely fail due to missing kernel/rootfs, but tests the flow)
	t.Run("CreateMicroVM", func(t *testing.T) {
		err := manager.CreateMicroVM(ctx, microvm)
		// We expect this to fail due to missing kernel/rootfs files, but it should not panic
		if err == nil {
			t.Log("MicroVM creation succeeded (unexpected but good)")

			// If creation succeeded, test other operations
			t.Run("GetStatus", func(t *testing.T) {
				status, err := manager.GetMicroVMStatus(ctx, microvm.ID)
				if err != nil {
					t.Errorf("Failed to get microVM status: %v", err)
				}
				t.Logf("MicroVM status: %s", status)
			})

			t.Run("StopMicroVM", func(t *testing.T) {
				err := manager.StopMicroVM(ctx, microvm, false)
				if err != nil {
					t.Errorf("Failed to stop microVM: %v", err)
				}
			})

			t.Run("DeleteMicroVM", func(t *testing.T) {
				err := manager.DeleteMicroVM(ctx, microvm)
				if err != nil {
					t.Errorf("Failed to delete microVM: %v", err)
				}
			})
		} else {
			t.Logf("MicroVM creation failed as expected: %v", err)
			// Verify that the error handling worked correctly
			if microvm.Status != models.MicroVMStatusError {
				t.Errorf("Expected microVM status to be error, got: %s", microvm.Status)
			}
		}
	})
}

// TestNetworkManagerIntegration tests network manager functionality
func TestNetworkManagerIntegration(t *testing.T) {
	// Skip if not running as root (required for network operations)
	if os.Geteuid() != 0 {
		t.Skip("Network integration tests require root privileges")
	}

	networkManager, err := NewNetworkManager()
	if err != nil {
		t.Fatalf("Failed to create network manager: %v", err)
	}

	ctx := context.Background()
	testMicrovmID := uuid.New().String()

	t.Run("ConfigureNATNetwork", func(t *testing.T) {
		networkInfo, err := networkManager.ConfigureNetwork(ctx, testMicrovmID, models.NetworkModeNAT, "", "", "", "", "", 0, "")
		if err != nil {
			t.Errorf("Failed to configure NAT network: %v", err)
			return
		}

		if networkInfo.AssignmentMethod != models.NetworkModeNAT {
			t.Errorf("Expected NAT mode, got: %s", networkInfo.AssignmentMethod)
		}

		if networkInfo.MACAddress == "" {
			t.Error("MAC address should not be empty")
		}

		if networkInfo.IPAddress == "" {
			t.Error("IP address should not be empty")
		}

		// Cleanup
		err = networkManager.CleanupNetwork(ctx, testMicrovmID, models.NetworkModeNAT)
		if err != nil {
			t.Errorf("Failed to cleanup network: %v", err)
		}
	})
}

// TestStorageManagerIntegration tests storage manager functionality
func TestStorageManagerIntegration(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "firecracker-storage-test")
	defer os.RemoveAll(tempDir)

	storageManager, err := NewStorageManager(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage manager: %v", err)
	}

	ctx := context.Background()
	testMicrovmID := uuid.New().String()

	t.Run("ProvisionLocalStorage", func(t *testing.T) {
		config := &models.StorageConfig{
			Type: models.StorageTypeLocal,
		}

		storagePaths, err := storageManager.ProvisionStorage(ctx, testMicrovmID, config, 2048)
		if err != nil {
			t.Errorf("Failed to provision local storage: %v", err)
			return
		}

		if storagePaths.RootfsPath == "" {
			t.Error("Rootfs path should not be empty")
		}

		// Check if files were created
		if _, err := os.Stat(storagePaths.RootfsPath); os.IsNotExist(err) {
			t.Error("Rootfs file was not created")
		}

		// Cleanup
		err = storageManager.CleanupStorage(ctx, testMicrovmID, config)
		if err != nil {
			t.Errorf("Failed to cleanup storage: %v", err)
		}
	})
}

// TestSSHKeyManagerIntegration tests SSH key manager functionality
func TestSSHKeyManagerIntegration(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "firecracker-ssh-test")
	defer os.RemoveAll(tempDir)

	sshKeyManager, err := NewSSHKeyManager(tempDir)
	if err != nil {
		t.Fatalf("Failed to create SSH key manager: %v", err)
	}

	testMicrovmID := uuid.New().String()

	t.Run("GenerateKeyPair", func(t *testing.T) {
		publicKey, privateKey, err := sshKeyManager.GenerateKeyPair(testMicrovmID)
		if err != nil {
			t.Errorf("Failed to generate key pair: %v", err)
			return
		}

		if publicKey == "" {
			t.Error("Public key should not be empty")
		}

		if privateKey == "" {
			t.Error("Private key should not be empty")
		}

		// Test key retrieval
		retrievedPublic, retrievedPrivate, err := sshKeyManager.GetKeyPair(testMicrovmID)
		if err != nil {
			t.Errorf("Failed to retrieve key pair: %v", err)
			return
		}

		if retrievedPublic != publicKey {
			t.Error("Retrieved public key doesn't match generated key")
		}

		if retrievedPrivate != privateKey {
			t.Error("Retrieved private key doesn't match generated key")
		}

		// Test key existence check
		if !sshKeyManager.KeyExists(testMicrovmID) {
			t.Error("Key should exist")
		}

		// Test key deletion
		err = sshKeyManager.DeleteKeyPair(testMicrovmID)
		if err != nil {
			t.Errorf("Failed to delete key pair: %v", err)
		}

		if sshKeyManager.KeyExists(testMicrovmID) {
			t.Error("Key should not exist after deletion")
		}
	})
}

// TestConfigGeneratorIntegration tests configuration generator functionality
func TestConfigGeneratorIntegration(t *testing.T) {
	config := &Config{
		WorkDir:        "/tmp/firecracker-test",
		FirecrackerBin: "/usr/local/bin/firecracker",
		KernelPath:     "/opt/firecracker/vmlinux",
		UseJailer:      false,
	}

	configGenerator, err := NewConfigGenerator(config)
	if err != nil {
		t.Fatalf("Failed to create config generator: %v", err)
	}

	microvm := &models.MicroVMInstance{
		ID:     uuid.New().String(),
		NodeID: "test-node",
		Resources: models.ResourceSpecs{
			VCPUs:     2,
			MemoryMB:  1024,
			StorageMB: 2048,
		},
		Network: models.NetworkInfo{
			IPAddress:        "192.168.100.10",
			MACAddress:       "AA:FC:00:01:02:03",
			AssignmentMethod: models.NetworkModeNAT,
		},
	}

	storagePaths := &StoragePaths{
		RootfsPath: "/tmp/rootfs.ext4",
		DataPath:   "/tmp/data.ext4",
	}

	t.Run("GenerateConfig", func(t *testing.T) {
		fcConfig, err := configGenerator.GenerateConfig(microvm, storagePaths, "test-private-key")
		if err != nil {
			t.Errorf("Failed to generate config: %v", err)
			return
		}

		if fcConfig.MachineConfig.VcpuCount != microvm.Resources.VCPUs {
			t.Errorf("Expected %d vCPUs, got %d", microvm.Resources.VCPUs, fcConfig.MachineConfig.VcpuCount)
		}

		if fcConfig.MachineConfig.MemSizeMib != microvm.Resources.MemoryMB {
			t.Errorf("Expected %d MB memory, got %d", microvm.Resources.MemoryMB, fcConfig.MachineConfig.MemSizeMib)
		}

		if len(fcConfig.Drives) == 0 {
			t.Error("Expected at least one drive")
		}

		if len(fcConfig.NetworkInterfaces) == 0 {
			t.Error("Expected at least one network interface")
		}

		if fcConfig.NetworkInterfaces[0].GuestMac != microvm.Network.MACAddress {
			t.Errorf("Expected MAC %s, got %s", microvm.Network.MACAddress, fcConfig.NetworkInterfaces[0].GuestMac)
		}
	})

	t.Run("GenerateCloudInitConfig", func(t *testing.T) {
		cloudInit, err := configGenerator.GenerateCloudInitConfig(microvm, "ssh-rsa AAAAB3NzaC1yc2E...")
		if err != nil {
			t.Errorf("Failed to generate cloud-init config: %v", err)
			return
		}

		if cloudInit == "" {
			t.Error("Cloud-init config should not be empty")
		}

		// Check if it contains expected elements
		if !contains(cloudInit, "#cloud-config") {
			t.Error("Cloud-init config should contain #cloud-config header")
		}

		if !contains(cloudInit, microvm.ID) {
			t.Error("Cloud-init config should contain microVM ID")
		}
	})
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsAt(s, substr, 1)))
}

func containsAt(s, substr string, start int) bool {
	if start >= len(s) {
		return false
	}
	if start+len(substr) > len(s) {
		return containsAt(s, substr, start+1)
	}
	if s[start:start+len(substr)] == substr {
		return true
	}
	return containsAt(s, substr, start+1)
}
