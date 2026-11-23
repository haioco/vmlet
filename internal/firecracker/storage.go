package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// StorageManager handles storage provisioning for microVMs
type StorageManager struct {
	workDir     string
	imagesDir   string
	s3fsDir     string
	nbdDir      string
}

// StoragePaths contains paths to storage devices for a microVM
type StoragePaths struct {
	RootfsPath string
	DataPath   string
	S3Config   *models.S3Config
}

// StorageInfo contains information about microVM storage
type StorageInfo struct {
	MicroVMID    string                `json:"microvm_id"`
	StorageType  models.StorageType    `json:"storage_type"`
	TotalSizeMB  uint32               `json:"total_size_mb"`
	UsedSizeMB   uint32               `json:"used_size_mb"`
	Paths        *StoragePaths        `json:"paths"`
	S3Config     *models.S3Config     `json:"s3_config,omitempty"`
	LastBackup   *time.Time           `json:"last_backup,omitempty"`
	BackupCount  int                  `json:"backup_count"`
}

// StorageCapabilities describes the capabilities of a storage type
type StorageCapabilities struct {
	SupportsResize     bool   `json:"supports_resize"`
	SupportsBackup     bool   `json:"supports_backup"`
	SupportsRestore    bool   `json:"supports_restore"`
	SupportsSnapshot   bool   `json:"supports_snapshot"`
	RequiresS3Config   bool   `json:"requires_s3_config"`
	MaxSizeMB          uint32 `json:"max_size_mb"`
	MinSizeMB          uint32 `json:"min_size_mb"`
	Description        string `json:"description"`
}

// NewStorageManager creates a new storage manager
func NewStorageManager(workDir string) (*StorageManager, error) {
	imagesDir := filepath.Join(workDir, "images")
	s3fsDir := filepath.Join(workDir, "s3fs")
	nbdDir := filepath.Join(workDir, "nbd")

	// Create directories
	for _, dir := range []string{imagesDir, s3fsDir, nbdDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return &StorageManager{
		workDir:   workDir,
		imagesDir: imagesDir,
		s3fsDir:   s3fsDir,
		nbdDir:    nbdDir,
	}, nil
}

// ProvisionStorage provisions storage for a microVM based on configuration
func (sm *StorageManager) ProvisionStorage(ctx context.Context, microvmID string, config *models.StorageConfig, sizeMB uint32) (*StoragePaths, error) {
	switch config.Type {
	case models.StorageTypeLocal:
		return sm.provisionLocalStorage(ctx, microvmID, sizeMB)
	case models.StorageTypeS3Fuse:
		return sm.provisionS3FuseStorage(ctx, microvmID, config.S3Config, sizeMB)
	case models.StorageTypeS3NBD:
		return sm.provisionS3NBDStorage(ctx, microvmID, config.S3Config, sizeMB)
	case models.StorageTypeHybrid:
		return sm.provisionHybridStorage(ctx, microvmID, config.S3Config, sizeMB)
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", config.Type)
	}
}

// provisionLocalStorage provisions local block device storage
func (sm *StorageManager) provisionLocalStorage(ctx context.Context, microvmID string, sizeMB uint32) (*StoragePaths, error) {
	// Create rootfs image
	rootfsPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-rootfs.ext4", microvmID))
	if err := sm.createRootfsImage(rootfsPath, 1024); err != nil { // 1GB for rootfs
		return nil, fmt.Errorf("failed to create rootfs image: %w", err)
	}

	// Create data disk if size > rootfs size
	var dataPath string
	if sizeMB > 1024 {
		dataPath = filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
		dataSizeMB := sizeMB - 1024
		if err := sm.createDataImage(dataPath, dataSizeMB); err != nil {
			return nil, fmt.Errorf("failed to create data image: %w", err)
		}
	}

	return &StoragePaths{
		RootfsPath: rootfsPath,
		DataPath:   dataPath,
	}, nil
}

// provisionS3FuseStorage provisions S3-backed storage using s3fs-fuse
func (sm *StorageManager) provisionS3FuseStorage(ctx context.Context, microvmID string, s3Config *models.S3Config, sizeMB uint32) (*StoragePaths, error) {
	if s3Config == nil {
		return nil, fmt.Errorf("S3 config is required for S3 FUSE storage")
	}

	// Create mount point
	mountPoint := filepath.Join(sm.s3fsDir, microvmID)
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mount point: %w", err)
	}

	// Mount S3 bucket using s3fs
	if err := sm.mountS3Bucket(s3Config, mountPoint); err != nil {
		return nil, fmt.Errorf("failed to mount S3 bucket: %w", err)
	}

	// Create rootfs image on S3
	rootfsPath := filepath.Join(mountPoint, "rootfs.ext4")
	if err := sm.createRootfsImage(rootfsPath, 1024); err != nil {
		return nil, fmt.Errorf("failed to create rootfs image on S3: %w", err)
	}

	// Create data image if needed
	var dataPath string
	if sizeMB > 1024 {
		dataPath = filepath.Join(mountPoint, "data.ext4")
		dataSizeMB := sizeMB - 1024
		if err := sm.createDataImage(dataPath, dataSizeMB); err != nil {
			return nil, fmt.Errorf("failed to create data image on S3: %w", err)
		}
	}

	return &StoragePaths{
		RootfsPath: rootfsPath,
		DataPath:   dataPath,
		S3Config:   s3Config,
	}, nil
}

// provisionS3NBDStorage provisions S3-backed storage using NBD
func (sm *StorageManager) provisionS3NBDStorage(ctx context.Context, microvmID string, s3Config *models.S3Config, sizeMB uint32) (*StoragePaths, error) {
	if s3Config == nil {
		return nil, fmt.Errorf("S3 config is required for S3 NBD storage")
	}

	// Start NBD server for rootfs
	rootfsNBDPath, err := sm.startNBDServer(microvmID, "rootfs", s3Config, 1024)
	if err != nil {
		return nil, fmt.Errorf("failed to start NBD server for rootfs: %w", err)
	}

	// Start NBD server for data disk if needed
	var dataNBDPath string
	if sizeMB > 1024 {
		dataSizeMB := sizeMB - 1024
		dataNBDPath, err = sm.startNBDServer(microvmID, "data", s3Config, dataSizeMB)
		if err != nil {
			return nil, fmt.Errorf("failed to start NBD server for data: %w", err)
		}
	}

	return &StoragePaths{
		RootfsPath: rootfsNBDPath,
		DataPath:   dataNBDPath,
		S3Config:   s3Config,
	}, nil
}

// provisionHybridStorage provisions hybrid storage (local cache + S3 sync)
func (sm *StorageManager) provisionHybridStorage(ctx context.Context, microvmID string, s3Config *models.S3Config, sizeMB uint32) (*StoragePaths, error) {
	if s3Config == nil {
		return nil, fmt.Errorf("S3 config is required for hybrid storage")
	}

	// Create local storage first
	localPaths, err := sm.provisionLocalStorage(ctx, microvmID, sizeMB)
	if err != nil {
		return nil, fmt.Errorf("failed to provision local storage: %w", err)
	}

	// Set up S3 sync
	if err := sm.setupS3Sync(microvmID, s3Config, localPaths); err != nil {
		return nil, fmt.Errorf("failed to setup S3 sync: %w", err)
	}

	return &StoragePaths{
		RootfsPath: localPaths.RootfsPath,
		DataPath:   localPaths.DataPath,
		S3Config:   s3Config,
	}, nil
}

// CleanupStorage cleans up storage resources for a microVM
func (sm *StorageManager) CleanupStorage(ctx context.Context, microvmID string, config *models.StorageConfig) error {
	switch config.Type {
	case models.StorageTypeLocal:
		return sm.cleanupLocalStorage(microvmID)
	case models.StorageTypeS3Fuse:
		return sm.cleanupS3FuseStorage(microvmID)
	case models.StorageTypeS3NBD:
		return sm.cleanupS3NBDStorage(microvmID)
	case models.StorageTypeHybrid:
		return sm.cleanupHybridStorage(microvmID, config.S3Config)
	default:
		return fmt.Errorf("unsupported storage type for cleanup: %s", config.Type)
	}
}

// Helper methods for storage operations

func (sm *StorageManager) createRootfsImage(imagePath string, sizeMB uint32) error {
	// Create empty file
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", imagePath), 
		"bs=1M", fmt.Sprintf("count=%d", sizeMB))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create image file: %w", err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", imagePath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to format image: %w", err)
	}

	// Mount temporarily to set up basic filesystem
	tempMount := filepath.Join(os.TempDir(), fmt.Sprintf("fc-mount-%d", os.Getpid()))
	if err := os.MkdirAll(tempMount, 0755); err != nil {
		return fmt.Errorf("failed to create temp mount: %w", err)
	}
	defer os.RemoveAll(tempMount)

	cmd = exec.Command("mount", "-o", "loop", imagePath, tempMount)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount image: %w", err)
	}
	defer exec.Command("umount", tempMount).Run()

	// Create basic directory structure
	dirs := []string{"bin", "sbin", "etc", "var", "usr", "home", "root", "tmp", "dev", "proc", "sys"}
	for _, dir := range dirs {
		os.MkdirAll(filepath.Join(tempMount, dir), 0755)
	}

	return nil
}

func (sm *StorageManager) createDataImage(imagePath string, sizeMB uint32) error {
	// Create empty file
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", imagePath), 
		"bs=1M", fmt.Sprintf("count=%d", sizeMB))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create data image file: %w", err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", imagePath)
	return cmd.Run()
}

func (sm *StorageManager) mountS3Bucket(s3Config *models.S3Config, mountPoint string) error {
	// Create credentials file
	credFile := filepath.Join(sm.s3fsDir, fmt.Sprintf(".s3fs-creds-%s", s3Config.Bucket))
	credContent := fmt.Sprintf("%s:%s", s3Config.AccessKey, s3Config.SecretKey)
	if err := os.WriteFile(credFile, []byte(credContent), 0600); err != nil {
		return fmt.Errorf("failed to create credentials file: %w", err)
	}

	// Mount S3 bucket
	cmd := exec.Command("s3fs", s3Config.Bucket, mountPoint,
		"-o", fmt.Sprintf("passwd_file=%s", credFile),
		"-o", fmt.Sprintf("url=https://s3.%s.amazonaws.com", s3Config.Region),
		"-o", "use_cache=/tmp",
		"-o", "allow_other")
	
	return cmd.Run()
}

func (sm *StorageManager) unmountS3Bucket(mountPoint string) error {
	cmd := exec.Command("fusermount", "-u", mountPoint)
	return cmd.Run()
}

func (sm *StorageManager) startNBDServer(microvmID, diskType string, s3Config *models.S3Config, sizeMB uint32) (string, error) {
	// Create NBD device path
	nbdDevice := fmt.Sprintf("/dev/nbd%d", sm.getNextNBDDevice())
	
	// Create S3 object key
	s3Key := fmt.Sprintf("microvms/%s/%s.img", microvmID, diskType)
	
	// Start NBD server (this is a simplified example - you'd use a proper S3-NBD server)
	cmd := exec.Command("qemu-nbd", "--connect", nbdDevice, 
		fmt.Sprintf("s3://%s/%s", s3Config.Bucket, s3Key))
	
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start NBD server: %w", err)
	}

	// Store process info for cleanup
	pidFile := filepath.Join(sm.nbdDir, fmt.Sprintf("%s-%s.pid", microvmID, diskType))
	os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	return nbdDevice, nil
}

func (sm *StorageManager) stopNBDServer(microvmID, diskType string) error {
	pidFile := filepath.Join(sm.nbdDir, fmt.Sprintf("%s-%s.pid", microvmID, diskType))
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return nil // Already stopped
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return fmt.Errorf("failed to parse PID: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("failed to kill NBD server: %w", err)
	}

	os.Remove(pidFile)
	return nil
}

func (sm *StorageManager) getNextNBDDevice() int {
	// Simple NBD device allocation - in production, you'd track this properly
	for i := 0; i < 16; i++ {
		device := fmt.Sprintf("/dev/nbd%d", i)
		if _, err := os.Stat(device); err == nil {
			// Check if device is in use
			cmd := exec.Command("lsblk", device)
			if cmd.Run() != nil {
				return i // Device is available
			}
		}
	}
	return 0 // Fallback to nbd0
}

func (sm *StorageManager) setupS3Sync(microvmID string, s3Config *models.S3Config, localPaths *StoragePaths) error {
	// Create sync script
	syncScript := filepath.Join(sm.workDir, "sync", fmt.Sprintf("%s-sync.sh", microvmID))
	if err := os.MkdirAll(filepath.Dir(syncScript), 0755); err != nil {
		return fmt.Errorf("failed to create sync directory: %w", err)
	}

	scriptContent := fmt.Sprintf(`#!/bin/bash
# S3 sync script for microVM %s
export AWS_ACCESS_KEY_ID=%s
export AWS_SECRET_ACCESS_KEY=%s
export AWS_DEFAULT_REGION=%s

# Sync rootfs
aws s3 sync %s s3://%s/microvms/%s/rootfs.ext4

# Sync data disk if exists
if [ -f "%s" ]; then
    aws s3 sync %s s3://%s/microvms/%s/data.ext4
fi
`, microvmID, s3Config.AccessKey, s3Config.SecretKey, s3Config.Region,
		localPaths.RootfsPath, s3Config.Bucket, microvmID,
		localPaths.DataPath, localPaths.DataPath, s3Config.Bucket, microvmID)

	if err := os.WriteFile(syncScript, []byte(scriptContent), 0755); err != nil {
		return fmt.Errorf("failed to write sync script: %w", err)
	}

	// Set up periodic sync (this would typically be done with cron or systemd timer)
	return nil
}

func (sm *StorageManager) cleanupLocalStorage(microvmID string) error {
	// Remove image files
	rootfsPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-rootfs.ext4", microvmID))
	dataPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
	
	os.Remove(rootfsPath)
	os.Remove(dataPath)
	
	return nil
}

func (sm *StorageManager) cleanupS3FuseStorage(microvmID string) error {
	mountPoint := filepath.Join(sm.s3fsDir, microvmID)
	
	// Unmount S3 bucket
	sm.unmountS3Bucket(mountPoint)
	
	// Remove mount point
	os.RemoveAll(mountPoint)
	
	return nil
}

func (sm *StorageManager) cleanupS3NBDStorage(microvmID string) error {
	// Stop NBD servers
	sm.stopNBDServer(microvmID, "rootfs")
	sm.stopNBDServer(microvmID, "data")
	
	return nil
}

func (sm *StorageManager) cleanupHybridStorage(microvmID string, s3Config *models.S3Config) error {
	// Clean up local storage
	if err := sm.cleanupLocalStorage(microvmID); err != nil {
		return err
	}
	
	// Remove sync script
	syncScript := filepath.Join(sm.workDir, "sync", fmt.Sprintf("%s-sync.sh", microvmID))
	os.Remove(syncScript)
	
	return nil
}

// GetStorageInfo retrieves storage information for a microVM
func (sm *StorageManager) GetStorageInfo(ctx context.Context, microvmID string) (*StorageInfo, error) {
	// This would typically query the storage system for actual usage
	// For now, we'll return basic information based on file system
	
	rootfsPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-rootfs.ext4", microvmID))
	dataPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
	
	var totalSize, usedSize uint32
	
	// Check rootfs
	if stat, err := os.Stat(rootfsPath); err == nil {
		totalSize += uint32(stat.Size() / (1024 * 1024)) // Convert to MB
		usedSize += uint32(stat.Size() / (1024 * 1024))  // Assume fully used for simplicity
	}
	
	// Check data disk
	if stat, err := os.Stat(dataPath); err == nil {
		totalSize += uint32(stat.Size() / (1024 * 1024))
		usedSize += uint32(stat.Size() / (1024 * 1024))
	}
	
	paths := &StoragePaths{
		RootfsPath: rootfsPath,
		DataPath:   dataPath,
	}
	
	info := &StorageInfo{
		MicroVMID:   microvmID,
		StorageType: models.StorageTypeLocal, // Default assumption
		TotalSizeMB: totalSize,
		UsedSizeMB:  usedSize,
		Paths:       paths,
	}
	
	return info, nil
}

// ResizeStorage resizes storage for a microVM (if supported)
func (sm *StorageManager) ResizeStorage(ctx context.Context, microvmID string, config *models.StorageConfig, newSizeMB uint32) error {
	switch config.Type {
	case models.StorageTypeLocal:
		return sm.resizeLocalStorage(microvmID, newSizeMB)
	case models.StorageTypeHybrid:
		return sm.resizeLocalStorage(microvmID, newSizeMB)
	case models.StorageTypeS3Fuse, models.StorageTypeS3NBD:
		return fmt.Errorf("resize not supported for storage type: %s", config.Type)
	default:
		return fmt.Errorf("unsupported storage type: %s", config.Type)
	}
}

// BackupStorage creates a backup of storage to S3
func (sm *StorageManager) BackupStorage(ctx context.Context, microvmID string, config *models.StorageConfig, s3Config *models.S3Config) error {
	if s3Config == nil {
		return fmt.Errorf("S3 config is required for backup")
	}
	
	switch config.Type {
	case models.StorageTypeLocal, models.StorageTypeHybrid:
		return sm.backupLocalStorage(microvmID, s3Config)
	case models.StorageTypeS3Fuse, models.StorageTypeS3NBD:
		return fmt.Errorf("backup not needed for S3-based storage types")
	default:
		return fmt.Errorf("unsupported storage type: %s", config.Type)
	}
}

// RestoreStorage restores storage from S3 backup
func (sm *StorageManager) RestoreStorage(ctx context.Context, microvmID string, config *models.StorageConfig, s3Config *models.S3Config, backupID string) error {
	if s3Config == nil {
		return fmt.Errorf("S3 config is required for restore")
	}
	
	switch config.Type {
	case models.StorageTypeLocal, models.StorageTypeHybrid:
		return sm.restoreLocalStorage(microvmID, s3Config, backupID)
	case models.StorageTypeS3Fuse, models.StorageTypeS3NBD:
		return fmt.Errorf("restore not needed for S3-based storage types")
	default:
		return fmt.Errorf("unsupported storage type: %s", config.Type)
	}
}

// GetStorageCapabilities returns capabilities for all storage types
func (sm *StorageManager) GetStorageCapabilities() map[models.StorageType]StorageCapabilities {
	return map[models.StorageType]StorageCapabilities{
		models.StorageTypeLocal: {
			SupportsResize:   true,
			SupportsBackup:   true,
			SupportsRestore:  true,
			SupportsSnapshot: false,
			RequiresS3Config: false,
			MaxSizeMB:        102400, // 100GB
			MinSizeMB:        1024,   // 1GB
			Description:      "Local block device storage with high performance",
		},
		models.StorageTypeS3Fuse: {
			SupportsResize:   false,
			SupportsBackup:   false,
			SupportsRestore:  false,
			SupportsSnapshot: false,
			RequiresS3Config: true,
			MaxSizeMB:        1048576, // 1TB
			MinSizeMB:        1024,    // 1GB
			Description:      "S3-backed storage using FUSE filesystem",
		},
		models.StorageTypeS3NBD: {
			SupportsResize:   false,
			SupportsBackup:   false,
			SupportsRestore:  false,
			SupportsSnapshot: true,
			RequiresS3Config: true,
			MaxSizeMB:        1048576, // 1TB
			MinSizeMB:        1024,    // 1GB
			Description:      "S3-backed storage using Network Block Device",
		},
		models.StorageTypeHybrid: {
			SupportsResize:   true,
			SupportsBackup:   true,
			SupportsRestore:  true,
			SupportsSnapshot: false,
			RequiresS3Config: true,
			MaxSizeMB:        102400, // 100GB
			MinSizeMB:        1024,   // 1GB
			Description:      "Hybrid storage with local cache and S3 sync",
		},
	}
}

// Helper methods for additional storage operations

func (sm *StorageManager) resizeLocalStorage(microvmID string, newSizeMB uint32) error {
	dataPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
	
	// Check current size
	stat, err := os.Stat(dataPath)
	if err != nil {
		return fmt.Errorf("failed to get current size: %w", err)
	}
	
	currentSizeMB := uint32(stat.Size() / (1024 * 1024))
	if newSizeMB <= currentSizeMB {
		return fmt.Errorf("new size must be larger than current size (%d MB)", currentSizeMB)
	}
	
	// Extend the file
	additionalSizeMB := newSizeMB - currentSizeMB
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", dataPath),
		"bs=1M", fmt.Sprintf("count=%d", additionalSizeMB), "conv=notrunc", "oflag=append")
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to extend storage: %w", err)
	}
	
	// Resize the filesystem
	cmd = exec.Command("resize2fs", dataPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to resize filesystem: %w", err)
	}
	
	return nil
}

func (sm *StorageManager) backupLocalStorage(microvmID string, s3Config *models.S3Config) error {
	rootfsPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-rootfs.ext4", microvmID))
	dataPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
	
	timestamp := time.Now().Format("20060102-150405")
	backupID := fmt.Sprintf("%s-%s", microvmID, timestamp)
	
	// Backup rootfs
	rootfsKey := fmt.Sprintf("backups/%s/rootfs.ext4", backupID)
	if err := sm.uploadToS3(rootfsPath, s3Config, rootfsKey); err != nil {
		return fmt.Errorf("failed to backup rootfs: %w", err)
	}
	
	// Backup data disk if it exists
	if _, err := os.Stat(dataPath); err == nil {
		dataKey := fmt.Sprintf("backups/%s/data.ext4", backupID)
		if err := sm.uploadToS3(dataPath, s3Config, dataKey); err != nil {
			return fmt.Errorf("failed to backup data disk: %w", err)
		}
	}
	
	return nil
}

func (sm *StorageManager) restoreLocalStorage(microvmID string, s3Config *models.S3Config, backupID string) error {
	rootfsPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-rootfs.ext4", microvmID))
	dataPath := filepath.Join(sm.imagesDir, fmt.Sprintf("%s-data.ext4", microvmID))
	
	// Restore rootfs
	rootfsKey := fmt.Sprintf("backups/%s/rootfs.ext4", backupID)
	if err := sm.downloadFromS3(s3Config, rootfsKey, rootfsPath); err != nil {
		return fmt.Errorf("failed to restore rootfs: %w", err)
	}
	
	// Restore data disk if backup exists
	dataKey := fmt.Sprintf("backups/%s/data.ext4", backupID)
	if err := sm.downloadFromS3(s3Config, dataKey, dataPath); err != nil {
		// Data disk backup might not exist, which is okay
		fmt.Printf("Warning: failed to restore data disk (might not exist): %v\n", err)
	}
	
	return nil
}

func (sm *StorageManager) uploadToS3(localPath string, s3Config *models.S3Config, s3Key string) error {
	// Use AWS CLI for upload (in production, you'd use AWS SDK)
	cmd := exec.Command("aws", "s3", "cp", localPath, fmt.Sprintf("s3://%s/%s", s3Config.Bucket, s3Key))
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", s3Config.AccessKey),
		fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", s3Config.SecretKey),
		fmt.Sprintf("AWS_DEFAULT_REGION=%s", s3Config.Region),
	)
	
	return cmd.Run()
}

func (sm *StorageManager) downloadFromS3(s3Config *models.S3Config, s3Key, localPath string) error {
	// Use AWS CLI for download (in production, you'd use AWS SDK)
	cmd := exec.Command("aws", "s3", "cp", fmt.Sprintf("s3://%s/%s", s3Config.Bucket, s3Key), localPath)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", s3Config.AccessKey),
		fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", s3Config.SecretKey),
		fmt.Sprintf("AWS_DEFAULT_REGION=%s", s3Config.Region),
	)
	
	return cmd.Run()
}