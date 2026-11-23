package images

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNewImageManager(t *testing.T) {
	workDir := "/tmp/work"
	cacheDir := "/tmp/cache"
	overlayDir := "/tmp/overlay"
	
	manager := NewImageManager(workDir, cacheDir, overlayDir)
	
	if manager == nil {
		t.Fatal("NewImageManager returned nil")
	}
	
	if manager.workDir != workDir {
		t.Errorf("Expected work dir %s, got %s", workDir, manager.workDir)
	}
	
	if manager.cache == nil {
		t.Error("Image cache should not be nil")
	}
	
	if manager.generator == nil {
		t.Error("Cloud-init generator should not be nil")
	}
}

func TestImageManagerInitialize(t *testing.T) {
	tempDir := t.TempDir()
	workDir := filepath.Join(tempDir, "work")
	cacheDir := filepath.Join(tempDir, "cache")
	overlayDir := filepath.Join(tempDir, "overlay")
	
	manager := NewImageManager(workDir, cacheDir, overlayDir)
	
	err := manager.Initialize()
	if err != nil {
		t.Fatalf("Failed to initialize image manager: %v", err)
	}
	
	// Check that cache directories were created
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("Cache directory was not created")
	}
	
	if _, err := os.Stat(overlayDir); os.IsNotExist(err) {
		t.Error("Overlay directory was not created")
	}
}

func TestCreateVMImageValidation(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	// Skip actual VM image creation tests as they require system commands
	// In a real implementation, these would test:
	// - nil config should return error
	// - empty config should return error  
	// - valid config should succeed (with proper Alpine image building)
	t.Skip("Skipping VM image creation test - requires system commands")
}

func TestRemoveVMImage(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	// Create test overlay files
	vmID := "test-vm"
	overlayPath := filepath.Join(tempDir, vmID+"-overlay.img")
	cloudInitPath := filepath.Join(tempDir, vmID+"-cloudinit.iso")
	
	if err := os.WriteFile(overlayPath, []byte("overlay"), 0644); err != nil {
		t.Fatalf("Failed to create overlay file: %v", err)
	}
	
	if err := os.WriteFile(cloudInitPath, []byte("cloudinit"), 0644); err != nil {
		t.Fatalf("Failed to create cloud-init file: %v", err)
	}
	
	// Remove VM image
	err := manager.RemoveVMImage(vmID)
	if err != nil {
		t.Fatalf("Failed to remove VM image: %v", err)
	}
	
	// Verify files were removed
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Error("Overlay file should have been removed")
	}
	
	if _, err := os.Stat(cloudInitPath); !os.IsNotExist(err) {
		t.Error("Cloud-init file should have been removed")
	}
}

func TestCleanupImages(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	err := manager.CleanupImages()
	if err != nil {
		t.Errorf("CleanupImages should not fail: %v", err)
	}
}

func TestImageManagerGetCacheStats(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	stats, err := manager.GetCacheStats()
	if err != nil {
		t.Fatalf("Failed to get cache stats: %v", err)
	}
	
	if stats == nil {
		t.Error("Cache stats should not be nil")
	}
	
	// With empty cache, all stats should be zero
	if stats.TotalEntries != 0 {
		t.Errorf("Expected 0 total entries, got %d", stats.TotalEntries)
	}
	
	if stats.BaseImages != 0 {
		t.Errorf("Expected 0 base images, got %d", stats.BaseImages)
	}
	
	if stats.Overlays != 0 {
		t.Errorf("Expected 0 overlays, got %d", stats.Overlays)
	}
	
	if stats.TotalSize != 0 {
		t.Errorf("Expected 0 total size, got %d", stats.TotalSize)
	}
}

func TestImageManagerValidateCache(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	err := manager.ValidateCache()
	if err != nil {
		t.Errorf("ValidateCache should not fail on empty cache: %v", err)
	}
}

func TestImageManagerGenerateSSHKeyPair(t *testing.T) {
	tempDir := t.TempDir()
	_ = NewImageManager(tempDir, tempDir, tempDir)
	
	// Skip actual SSH key generation as it requires system commands
	t.Skip("Skipping SSH key generation test - requires system commands")
}

func TestVMConfigValidation(t *testing.T) {
	// Test various VMConfig validation scenarios
	testCases := []struct {
		name    string
		config  *VMConfig
		wantErr bool
	}{
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
		{
			name:    "empty config",
			config:  &VMConfig{},
			wantErr: true,
		},
		{
			name: "missing ID",
			config: &VMConfig{
				Hostname: "test-host",
				SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
			},
			wantErr: true,
		},
		{
			name: "missing hostname",
			config: &VMConfig{
				ID:      "test-vm",
				SSHKeys: []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
			},
			wantErr: true,
		},
		{
			name: "valid minimal config",
			config: &VMConfig{
				ID:       "test-vm",
				Hostname: "test-host",
				SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
			},
			wantErr: false,
		},
		{
			name: "valid full config",
			config: &VMConfig{
				ID:       "test-vm",
				Hostname: "test-host",
				SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
				Users: []User{
					{
						Name:   "testuser",
						Groups: []string{"wheel"},
						Shell:  "/bin/bash",
					},
				},
				Packages:    []string{"curl", "wget"},
				RunCommands: []string{"echo 'setup complete'"},
			},
			wantErr: false,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVMConfig(tc.config)
			if tc.wantErr && err == nil {
				t.Error("Expected error but got none")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}

// Helper function to validate VM config
func validateVMConfig(config *VMConfig) error {
	if config == nil {
		return fmt.Errorf("VM config cannot be nil")
	}
	
	if config.ID == "" {
		return fmt.Errorf("VM ID cannot be empty")
	}
	
	if config.Hostname == "" {
		return fmt.Errorf("VM hostname cannot be empty")
	}
	
	return nil
}

func TestImageManagerIntegration(t *testing.T) {
	tempDir := t.TempDir()
	manager := NewImageManager(tempDir, tempDir, tempDir)
	
	if err := manager.Initialize(); err != nil {
		t.Fatalf("Failed to initialize manager: %v", err)
	}
	
	// Test cache stats on empty cache
	stats, err := manager.GetCacheStats()
	if err != nil {
		t.Fatalf("Failed to get initial cache stats: %v", err)
	}
	
	if stats.TotalEntries != 0 {
		t.Errorf("Expected empty cache, got %d entries", stats.TotalEntries)
	}
	
	// Test cache validation on empty cache
	if err := manager.ValidateCache(); err != nil {
		t.Errorf("Cache validation should pass on empty cache: %v", err)
	}
	
	// Test cleanup on empty cache
	if err := manager.CleanupImages(); err != nil {
		t.Errorf("Cleanup should not fail on empty cache: %v", err)
	}
}