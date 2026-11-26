package firecracker

import (
	"context"
	"testing"

	"github.com/haioco/vmlet/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageCapabilities(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	capabilities := sm.GetStorageCapabilities()

	// Test that all storage types have capabilities defined
	assert.Contains(t, capabilities, models.StorageTypeLocal)
	assert.Contains(t, capabilities, models.StorageTypeS3Fuse)
	assert.Contains(t, capabilities, models.StorageTypeS3NBD)
	assert.Contains(t, capabilities, models.StorageTypeHybrid)

	// Test local storage capabilities
	localCap := capabilities[models.StorageTypeLocal]
	assert.True(t, localCap.SupportsResize)
	assert.True(t, localCap.SupportsBackup)
	assert.True(t, localCap.SupportsRestore)
	assert.False(t, localCap.RequiresS3Config)
	assert.Greater(t, localCap.MaxSizeMB, uint32(0))
	assert.Greater(t, localCap.MinSizeMB, uint32(0))
	assert.NotEmpty(t, localCap.Description)

	// Test S3 FUSE capabilities
	s3fuseCap := capabilities[models.StorageTypeS3Fuse]
	assert.False(t, s3fuseCap.SupportsResize)
	assert.False(t, s3fuseCap.SupportsBackup)
	assert.False(t, s3fuseCap.SupportsRestore)
	assert.True(t, s3fuseCap.RequiresS3Config)
	assert.Greater(t, s3fuseCap.MaxSizeMB, localCap.MaxSizeMB) // S3 should support larger sizes

	// Test S3 NBD capabilities
	s3nbdCap := capabilities[models.StorageTypeS3NBD]
	assert.False(t, s3nbdCap.SupportsResize)
	assert.False(t, s3nbdCap.SupportsBackup)
	assert.False(t, s3nbdCap.SupportsRestore)
	assert.True(t, s3nbdCap.SupportsSnapshot) // NBD supports snapshots
	assert.True(t, s3nbdCap.RequiresS3Config)

	// Test hybrid storage capabilities
	hybridCap := capabilities[models.StorageTypeHybrid]
	assert.True(t, hybridCap.SupportsResize)
	assert.True(t, hybridCap.SupportsBackup)
	assert.True(t, hybridCap.SupportsRestore)
	assert.True(t, hybridCap.RequiresS3Config)
}

func TestStorageTypeValidation(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-validation"

	testCases := []struct {
		name        string
		storageType models.StorageType
		s3Config    *models.S3Config
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Local storage without S3 config",
			storageType: models.StorageTypeLocal,
			s3Config:    nil,
			expectError: false,
		},
		{
			name:        "S3 FUSE without S3 config",
			storageType: models.StorageTypeS3Fuse,
			s3Config:    nil,
			expectError: true,
			errorMsg:    "S3 config is required",
		},
		{
			name:        "S3 NBD without S3 config",
			storageType: models.StorageTypeS3NBD,
			s3Config:    nil,
			expectError: true,
			errorMsg:    "S3 config is required",
		},
		{
			name:        "Hybrid without S3 config",
			storageType: models.StorageTypeHybrid,
			s3Config:    nil,
			expectError: true,
			errorMsg:    "S3 config is required",
		},
		{
			name:        "Invalid storage type",
			storageType: models.StorageType("invalid"),
			s3Config:    nil,
			expectError: true,
			errorMsg:    "unsupported storage type",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := &models.StorageConfig{
				Type:     tc.storageType,
				S3Config: tc.s3Config,
			}

			_, err := sm.ProvisionStorage(ctx, microvmID+"-"+tc.name, config, 2048)

			if tc.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				// For local storage, we might get an error due to missing tools, but not validation error
				if err != nil {
					assert.NotContains(t, err.Error(), "S3 config is required")
					assert.NotContains(t, err.Error(), "unsupported storage type")
				}
			}
		})
	}
}

func TestStorageInfoGeneration(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-info"

	// Test getting storage info for non-existent microVM
	info, err := sm.GetStorageInfo(ctx, microvmID)
	require.NoError(t, err)
	require.NotNil(t, info)

	assert.Equal(t, microvmID, info.MicroVMID)
	assert.Equal(t, models.StorageTypeLocal, info.StorageType)
	assert.Equal(t, uint32(0), info.TotalSizeMB) // No files exist yet
	assert.Equal(t, uint32(0), info.UsedSizeMB)
	assert.NotNil(t, info.Paths)
}

func TestResizeStorageValidation(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-resize"

	testCases := []struct {
		name        string
		storageType models.StorageType
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Local storage resize",
			storageType: models.StorageTypeLocal,
			expectError: false,
		},
		{
			name:        "Hybrid storage resize",
			storageType: models.StorageTypeHybrid,
			expectError: false,
		},
		{
			name:        "S3 FUSE resize not supported",
			storageType: models.StorageTypeS3Fuse,
			expectError: true,
			errorMsg:    "resize not supported",
		},
		{
			name:        "S3 NBD resize not supported",
			storageType: models.StorageTypeS3NBD,
			expectError: true,
			errorMsg:    "resize not supported",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := &models.StorageConfig{
				Type: tc.storageType,
			}

			err := sm.ResizeStorage(ctx, microvmID+"-"+tc.name, config, 4096)

			if tc.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				// For supported types, we might get an error due to missing files, but not unsupported error
				if err != nil {
					assert.NotContains(t, err.Error(), "resize not supported")
				}
			}
		})
	}
}

func TestBackupRestoreValidation(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-backup"

	s3Config := &models.S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-key",
		SecretKey: "test-secret",
	}

	testCases := []struct {
		name               string
		storageType        models.StorageType
		expectBackupError  bool
		expectRestoreError bool
		errorMsg           string
	}{
		{
			name:               "Local storage backup/restore",
			storageType:        models.StorageTypeLocal,
			expectBackupError:  false,
			expectRestoreError: false,
		},
		{
			name:               "Hybrid storage backup/restore",
			storageType:        models.StorageTypeHybrid,
			expectBackupError:  false,
			expectRestoreError: false,
		},
		{
			name:               "S3 FUSE backup not needed",
			storageType:        models.StorageTypeS3Fuse,
			expectBackupError:  true,
			expectRestoreError: true,
			errorMsg:           "not needed for S3-based storage types",
		},
		{
			name:               "S3 NBD backup not needed",
			storageType:        models.StorageTypeS3NBD,
			expectBackupError:  true,
			expectRestoreError: true,
			errorMsg:           "not needed for S3-based storage types",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := &models.StorageConfig{
				Type:     tc.storageType,
				S3Config: s3Config,
			}

			// Test backup
			err := sm.BackupStorage(ctx, microvmID+"-"+tc.name, config, s3Config)
			if tc.expectBackupError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				// For supported types, we might get an error due to missing AWS CLI, but not unsupported error
				if err != nil {
					assert.NotContains(t, err.Error(), "not needed")
					assert.NotContains(t, err.Error(), "not applicable")
				}
			}

			// Test restore
			err = sm.RestoreStorage(ctx, microvmID+"-"+tc.name, config, s3Config, "backup-123")
			if tc.expectRestoreError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				// For supported types, we might get an error due to missing AWS CLI, but not unsupported error
				if err != nil {
					assert.NotContains(t, err.Error(), "not needed")
					assert.NotContains(t, err.Error(), "not applicable")
				}
			}
		})
	}
}

func TestBackupWithoutS3Config(t *testing.T) {
	tempDir := t.TempDir()
	sm, err := NewStorageManager(tempDir)
	require.NoError(t, err)

	ctx := context.Background()
	microvmID := "test-backup-no-s3"

	config := &models.StorageConfig{
		Type: models.StorageTypeLocal,
	}

	err = sm.BackupStorage(ctx, microvmID, config, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "S3 config is required for backup")

	err = sm.RestoreStorage(ctx, microvmID, config, nil, "backup-123")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "S3 config is required for restore")
}
