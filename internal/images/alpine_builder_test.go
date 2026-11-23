package images

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewAlpineBuilder(t *testing.T) {
	workDir := "/tmp/test-work"
	cacheDir := "/tmp/test-cache"
	
	builder := NewAlpineBuilder(workDir, cacheDir)
	
	if builder == nil {
		t.Fatal("NewAlpineBuilder returned nil")
	}
	
	if builder.workDir != workDir {
		t.Errorf("Expected workDir %s, got %s", workDir, builder.workDir)
	}
	
	if builder.cacheDir != cacheDir {
		t.Errorf("Expected cacheDir %s, got %s", cacheDir, builder.cacheDir)
	}
	
	if builder.alpineRepo == "" {
		t.Error("Alpine repository URL should not be empty")
	}
	
	if builder.alpineArch != "x86_64" {
		t.Errorf("Expected architecture x86_64, got %s", builder.alpineArch)
	}
	
	if builder.imageSize <= 0 {
		t.Errorf("Image size should be positive, got %d", builder.imageSize)
	}
}

func TestCalculateChecksum(t *testing.T) {
	// Create temporary test file
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.img")
	
	testContent := "test content for checksum"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	builder := NewAlpineBuilder(tempDir, tempDir)
	
	checksum1, err := builder.calculateChecksum(testFile)
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}
	
	if checksum1 == "" {
		t.Error("Checksum should not be empty")
	}
	
	// Calculate again to ensure consistency
	checksum2, err := builder.calculateChecksum(testFile)
	if err != nil {
		t.Fatalf("Failed to calculate checksum second time: %v", err)
	}
	
	if checksum1 != checksum2 {
		t.Errorf("Checksums should be identical: %s != %s", checksum1, checksum2)
	}
}

func TestLoadExistingImage(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.img")
	
	testContent := "test image content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	builder := NewAlpineBuilder(tempDir, tempDir)
	
	image, err := builder.loadExistingImage("test-image", testFile)
	if err != nil {
		t.Fatalf("Failed to load existing image: %v", err)
	}
	
	if image.ID != "test-image" {
		t.Errorf("Expected image ID 'test-image', got %s", image.ID)
	}
	
	if image.Path != testFile {
		t.Errorf("Expected image path %s, got %s", testFile, image.Path)
	}
	
	if image.Size != int64(len(testContent)) {
		t.Errorf("Expected image size %d, got %d", len(testContent), image.Size)
	}
	
	if image.Checksum == "" {
		t.Error("Image checksum should not be empty")
	}
}

func TestValidateImage(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.img")
	
	// Create a test file
	testContent := "test image content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	builder := NewAlpineBuilder(tempDir, tempDir)
	
	// Calculate checksum for the test file
	checksum, err := builder.calculateChecksum(testFile)
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}
	
	image := &BaseImage{
		ID:        "test-image",
		Path:      testFile,
		Size:      int64(len(testContent)),
		CreatedAt: time.Now(),
		Checksum:  checksum,
	}
	
	// Test validation with correct checksum (skip filesystem check for test file)
	// Note: This test file is not actually ext4, so validation will fail on filesystem check
	// In a real scenario, this would be a proper ext4 image
	err = builder.ValidateImage(image)
	if err == nil {
		t.Error("Image validation should fail for non-ext4 test file")
	}
	
	// Test validation with incorrect checksum
	image.Checksum = "invalid-checksum"
	err = builder.ValidateImage(image)
	if err == nil {
		t.Error("Image validation should fail with incorrect checksum")
	}
	
	// Test validation with non-existent file
	image.Path = "/non/existent/file"
	err = builder.ValidateImage(image)
	if err == nil {
		t.Error("Image validation should fail with non-existent file")
	}
}

func TestCreateImageFile(t *testing.T) {
	tempDir := t.TempDir()
	_ = filepath.Join(tempDir, "test.img")
	
	builder := NewAlpineBuilder(tempDir, tempDir)
	builder.imageSize = 1 // 1MB for testing
	
	// Skip actual dd command execution in tests
	// This would require mocking the exec.Command calls
	t.Skip("Skipping createImageFile test - requires system commands")
}

func TestFormatExt4(t *testing.T) {
	tempDir := t.TempDir()
	_ = filepath.Join(tempDir, "test.img")
	
	_ = NewAlpineBuilder(tempDir, tempDir)
	
	// Skip actual mkfs.ext4 command execution in tests
	// This would require mocking the exec.Command calls
	t.Skip("Skipping formatExt4 test - requires system commands")
}

func TestMountUnmountImage(t *testing.T) {
	tempDir := t.TempDir()
	_ = filepath.Join(tempDir, "test.img")
	mountDir := filepath.Join(tempDir, "mount")
	
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		t.Fatalf("Failed to create mount directory: %v", err)
	}
	
	_ = NewAlpineBuilder(tempDir, tempDir)
	
	// Skip actual mount/umount command execution in tests
	// This would require root privileges and system commands
	t.Skip("Skipping mount/unmount tests - requires root privileges")
}

func TestBuildBaseImageValidation(t *testing.T) {
	tempDir := t.TempDir()
	builder := NewAlpineBuilder(tempDir, tempDir)
	
	// Test with invalid version
	_, err := builder.BuildBaseImage("")
	if err == nil {
		t.Error("BuildBaseImage should fail with empty version")
	}
	
	// Skip full build test as it requires system commands and network access
	t.Skip("Skipping full BuildBaseImage test - requires system commands and network")
}

func TestConfigureSystem(t *testing.T) {
	tempDir := t.TempDir()
	mountDir := filepath.Join(tempDir, "mount")
	
	// Create mock filesystem structure
	dirs := []string{
		filepath.Join(mountDir, "etc", "ssh"),
		filepath.Join(mountDir, "etc", "runlevels", "default"),
		filepath.Join(mountDir, "etc", "network"),
		filepath.Join(mountDir, "root", ".ssh"),
	}
	
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}
	
	_ = NewAlpineBuilder(tempDir, tempDir)
	
	// Skip actual system configuration as it requires chroot
	t.Skip("Skipping configureSystem test - requires chroot and system commands")
}

func TestSetupCleanupChroot(t *testing.T) {
	tempDir := t.TempDir()
	mountDir := filepath.Join(tempDir, "mount")
	
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		t.Fatalf("Failed to create mount directory: %v", err)
	}
	
	_ = NewAlpineBuilder(tempDir, tempDir)
	
	// Skip chroot setup/cleanup tests as they require root privileges
	t.Skip("Skipping chroot tests - requires root privileges")
}