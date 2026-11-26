package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/haioco/vmlet/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStorageManager(t *testing.T) {
	tempDir := t.TempDir()

	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)
	require.NotNil(t, sm)

	// Check that directories were created
	assert.DirExists(t, filepath.Join(tempDir, "images"))
	assert.DirExists(t, filepath.Join(tempDir, "s3fs"))
	assert.DirExists(t, filepath.Join(tempDir, "nbd"))
}

func TestProvisionLocalStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-1"

	config := &models.StorageConfig{
		Type: models.StorageTypeLocal,
	}

	paths, err := sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.NoError(t, err)
	require.NotNil(t, paths)

	// Check that rootfs image was created
	assert.NotEmpty(t, paths.RootfsPath)
	assert.FileExists(t, paths.RootfsPath)

	// Check that data image was created (since total size > 1024MB)
	assert.NotEmpty(t, paths.DataPath)
	assert.FileExists(t, paths.DataPath)

	// Check that S3Config is nil for local storage
	assert.Nil(t, paths.S3Config)
}

func TestProvisionLocalStorageSmallSize(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-small"

	config := &models.StorageConfig{
		Type: models.StorageTypeLocal,
	}

	paths, err := sm.ProvisionStorage(ctx, microvmID, config, 1024)
	require.NoError(t, err)
	require.NotNil(t, paths)

	// Check that rootfs image was created
	assert.NotEmpty(t, paths.RootfsPath)
	assert.FileExists(t, paths.RootfsPath)

	// Check that no data image was created (since total size == rootfs size)
	assert.Empty(t, paths.DataPath)
}

func TestProvisionS3FuseStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-s3fuse"

	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	}

	config := &models.StorageConfig{
		Type:     models.StorageTypeS3Fuse,
		S3Config: s3Config,
	}

	// Note: This test will fail in CI/CD without s3fs installed
	// In a real environment, you'd mock the s3fs mount operation
	_, err = sm.ProvisionStorage(ctx, microvmID, config, 2048)

	// We expect this to fail in test environment without s3fs
	// but we can test the validation logic
	if err != nil {
		assert.Contains(t, err.Error(), "failed to mount S3 bucket")
	}
}

func TestProvisionS3FuseStorageWithoutConfig(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-s3fuse-no-config"

	config := &models.StorageConfig{
		Type:     models.StorageTypeS3Fuse,
		S3Config: nil,
	}

	_, err = sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S3 config is required")
}

func TestProvisionS3NBDStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-s3nbd"

	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	}

	config := &models.StorageConfig{
		Type:     models.StorageTypeS3NBD,
		S3Config: s3Config,
	}

	// Note: This test will fail without NBD support
	_, err = sm.ProvisionStorage(ctx, microvmID, config, 2048)

	// We expect this to fail in test environment without NBD
	if err != nil {
		assert.Contains(t, err.Error(), "failed to start NBD server")
	}
}

func TestProvisionHybridStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-hybrid"

	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	}

	config := &models.StorageConfig{
		Type:     models.StorageTypeHybrid,
		S3Config: s3Config,
	}

	paths, err := sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.NoError(t, err)
	require.NotNil(t, paths)

	// Check that local storage was created
	assert.NotEmpty(t, paths.RootfsPath)
	assert.FileExists(t, paths.RootfsPath)
	assert.NotEmpty(t, paths.DataPath)
	assert.FileExists(t, paths.DataPath)

	// Check that S3Config is preserved
	assert.NotNil(t, paths.S3Config)
	assert.Equal(t, s3Config.Bucket, paths.S3Config.Bucket)

	// Check that sync script was created
	syncScript := filepath.Join(tempDir, "sync", microvmID+"-sync.sh")
	assert.FileExists(t, syncScript)
}

func TestProvisionUnsupportedStorageType(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-unsupported"

	config := &models.StorageConfig{
		Type: models.StorageType("unsupported"),
	}

	_, err = sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported storage type")
}

func TestCleanupLocalStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-cleanup"

	config := &models.StorageConfig{
		Type: models.StorageTypeLocal,
	}

	// First provision storage
	paths, err := sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.NoError(t, err)

	// Verify files exist
	assert.FileExists(t, paths.RootfsPath)
	assert.FileExists(t, paths.DataPath)

	// Clean up storage
	err = sm.CleanupStorage(ctx, microvmID, config)
	require.NoError(t, err)

	// Verify files are removed
	assert.NoFileExists(t, paths.RootfsPath)
	assert.NoFileExists(t, paths.DataPath)
}

func TestCleanupHybridStorage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-microvm-cleanup-hybrid"

	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	}

	config := &models.StorageConfig{
		Type:     models.StorageTypeHybrid,
		S3Config: s3Config,
	}

	// First provision storage
	paths, err := sm.ProvisionStorage(ctx, microvmID, config, 2048)
	require.NoError(t, err)

	// Verify files exist
	assert.FileExists(t, paths.RootfsPath)
	assert.FileExists(t, paths.DataPath)

	syncScript := filepath.Join(tempDir, "sync", microvmID+"-sync.sh")
	assert.FileExists(t, syncScript)

	// Clean up storage
	err = sm.CleanupStorage(ctx, microvmID, config)
	require.NoError(t, err)

	// Verify files are removed
	assert.NoFileExists(t, paths.RootfsPath)
	assert.NoFileExists(t, paths.DataPath)
	assert.NoFileExists(t, syncScript)
}

func TestCreateRootfsImage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	imagePath := filepath.Join(tempDir, "test-rootfs.ext4")

	err = sm.createRootfsImage(imagePath, 100) // 100MB
	require.NoError(t, err)

	// Check that image file was created
	assert.FileExists(t, imagePath)

	// Check file size (should be approximately 100MB)
	stat, err := os.Stat(imagePath)
	require.NoError(t, err)

	expectedSize := int64(100 * 1024 * 1024)                                // 100MB in bytes
	assert.InDelta(t, expectedSize, stat.Size(), float64(expectedSize)*0.1) // Allow 10% variance
}

func TestCreateDataImage(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	imagePath := filepath.Join(tempDir, "test-data.ext4")

	err = sm.createDataImage(imagePath, 200) // 200MB
	require.NoError(t, err)

	// Check that image file was created
	assert.FileExists(t, imagePath)

	// Check file size (should be approximately 200MB)
	stat, err := os.Stat(imagePath)
	require.NoError(t, err)

	expectedSize := int64(200 * 1024 * 1024)                                // 200MB in bytes
	assert.InDelta(t, expectedSize, stat.Size(), float64(expectedSize)*0.1) // Allow 10% variance
}

func TestGetNextNBDDevice(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	// This test just verifies the method doesn't panic
	// In a real environment, it would check actual NBD devices
	deviceNum := sm.getNextNBDDevice()
	assert.GreaterOrEqual(t, deviceNum, 0)
	assert.LessOrEqual(t, deviceNum, 15)
}

func TestSetupS3Sync(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	microvmID := "test-microvm-sync"
	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	}

	localPaths := &StoragePaths{
		RootfsPath: "/path/to/rootfs.ext4",
		DataPath:   "/path/to/data.ext4",
	}

	err = sm.setupS3Sync(microvmID, s3Config, localPaths)
	require.NoError(t, err)

	// Check that sync script was created
	syncScript := filepath.Join(tempDir, "sync", microvmID+"-sync.sh")
	assert.FileExists(t, syncScript)

	// Check script content
	content, err := os.ReadFile(syncScript)
	require.NoError(t, err)

	scriptContent := string(content)
	assert.Contains(t, scriptContent, s3Config.AccessKey)
	assert.Contains(t, scriptContent, s3Config.Region)
	assert.Contains(t, scriptContent, s3Config.Bucket)
	assert.Contains(t, scriptContent, localPaths.RootfsPath)
	assert.Contains(t, scriptContent, localPaths.DataPath)
}
