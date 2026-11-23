package production

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BackupManager handles backup and disaster recovery operations
type BackupManager struct {
	config      *BackupConfig
	backupDir   string
	mutex       sync.RWMutex
	lastBackup  time.Time
	backupStats BackupStats
}

// BackupConfig defines backup configuration
type BackupConfig struct {
	BackupDir           string        `json:"backup_dir"`
	RetentionDays       int           `json:"retention_days"`
	BackupInterval      time.Duration `json:"backup_interval"`
	CompressBackups     bool          `json:"compress_backups"`
	IncludeConfigs      bool          `json:"include_configs"`
	IncludeData         bool          `json:"include_data"`
	IncludeLogs         bool          `json:"include_logs"`
	MaxBackupSize       int64         `json:"max_backup_size"`
	BackupOnShutdown    bool          `json:"backup_on_shutdown"`
	VerifyBackups       bool          `json:"verify_backups"`
}

// BackupStats tracks backup statistics
type BackupStats struct {
	TotalBackups     int64     `json:"total_backups"`
	SuccessfulBackups int64    `json:"successful_backups"`
	FailedBackups    int64     `json:"failed_backups"`
	LastBackupTime   time.Time `json:"last_backup_time"`
	LastBackupSize   int64     `json:"last_backup_size"`
	TotalBackupSize  int64     `json:"total_backup_size"`
	AverageBackupTime time.Duration `json:"average_backup_time"`
}

// BackupEntry represents a backup entry
type BackupEntry struct {
	Filename    string    `json:"filename"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	Type        string    `json:"type"`
	Compressed  bool      `json:"compressed"`
	Verified    bool      `json:"verified"`
}

// NewBackupManager creates a new backup manager
func NewBackupManager(config *BackupConfig) (*BackupManager, error) {
	if config == nil {
		config = DefaultBackupConfig()
	}

	// Create backup directory
	if err := os.MkdirAll(config.BackupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	return &BackupManager{
		config:    config,
		backupDir: config.BackupDir,
	}, nil
}

// DefaultBackupConfig returns default backup configuration
func DefaultBackupConfig() *BackupConfig {
	return &BackupConfig{
		BackupDir:         "/var/backups/microvm-service",
		RetentionDays:     30,
		BackupInterval:    24 * time.Hour,
		CompressBackups:   true,
		IncludeConfigs:    true,
		IncludeData:       true,
		IncludeLogs:       false,
		MaxBackupSize:     10 * 1024 * 1024 * 1024, // 10GB
		BackupOnShutdown:  true,
		VerifyBackups:     true,
	}
}

// CreateBackup creates a full backup of the service
func (bm *BackupManager) CreateBackup(ctx context.Context, backupType string) (*BackupEntry, error) {
	startTime := time.Now()
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	bm.backupStats.TotalBackups++

	// Generate backup filename
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("microvm-service-%s-%s", backupType, timestamp)
	if bm.config.CompressBackups {
		filename += ".tar.gz"
	} else {
		filename += ".tar"
	}

	backupPath := filepath.Join(bm.backupDir, filename)

	// Create backup
	size, err := bm.createBackupArchive(ctx, backupPath, backupType)
	if err != nil {
		bm.backupStats.FailedBackups++
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	// Verify backup if enabled
	verified := false
	if bm.config.VerifyBackups {
		if err := bm.verifyBackup(ctx, backupPath); err != nil {
			log.Printf("Backup verification failed: %v", err)
		} else {
			verified = true
		}
	}

	// Update statistics
	bm.backupStats.SuccessfulBackups++
	bm.backupStats.LastBackupTime = time.Now()
	bm.backupStats.LastBackupSize = size
	bm.backupStats.TotalBackupSize += size
	bm.lastBackup = time.Now()

	// Calculate average backup time
	backupDuration := time.Since(startTime)
	if bm.backupStats.SuccessfulBackups > 0 {
		totalTime := bm.backupStats.AverageBackupTime * time.Duration(bm.backupStats.SuccessfulBackups-1)
		bm.backupStats.AverageBackupTime = (totalTime + backupDuration) / time.Duration(bm.backupStats.SuccessfulBackups)
	} else {
		bm.backupStats.AverageBackupTime = backupDuration
	}

	entry := &BackupEntry{
		Filename:   filename,
		Path:       backupPath,
		Size:       size,
		CreatedAt:  time.Now(),
		Type:       backupType,
		Compressed: bm.config.CompressBackups,
		Verified:   verified,
	}

	log.Printf("Backup created successfully: %s (%d bytes, %v)", filename, size, backupDuration)
	return entry, nil
}

// createBackupArchive creates the actual backup archive
func (bm *BackupManager) createBackupArchive(ctx context.Context, backupPath, backupType string) (int64, error) {
	file, err := os.Create(backupPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create backup file: %w", err)
	}
	defer file.Close()

	var writer io.Writer = file
	var gzWriter *gzip.Writer

	// Add compression if enabled
	if bm.config.CompressBackups {
		gzWriter = gzip.NewWriter(file)
		writer = gzWriter
		defer gzWriter.Close()
	}

	tarWriter := tar.NewWriter(writer)
	defer tarWriter.Close()

	// Add files to backup based on type and configuration
	if err := bm.addFilesToBackup(ctx, tarWriter, backupType); err != nil {
		return 0, fmt.Errorf("failed to add files to backup: %w", err)
	}

	// Close writers to ensure all data is written
	if err := tarWriter.Close(); err != nil {
		return 0, fmt.Errorf("failed to close tar writer: %w", err)
	}

	if gzWriter != nil {
		if err := gzWriter.Close(); err != nil {
			return 0, fmt.Errorf("failed to close gzip writer: %w", err)
		}
	}

	// Get final file size
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to get backup file info: %w", err)
	}

	return info.Size(), nil
}

// addFilesToBackup adds files to the backup archive
func (bm *BackupManager) addFilesToBackup(ctx context.Context, tarWriter *tar.Writer, backupType string) error {
	// Define what to backup based on type
	var pathsToBackup []string

	switch backupType {
	case "full":
		if bm.config.IncludeConfigs {
			pathsToBackup = append(pathsToBackup, "configs/")
		}
		if bm.config.IncludeData {
			pathsToBackup = append(pathsToBackup, "firecracker-work/")
		}
		if bm.config.IncludeLogs {
			pathsToBackup = append(pathsToBackup, "logs/")
		}
	case "config":
		pathsToBackup = append(pathsToBackup, "configs/")
	case "data":
		pathsToBackup = append(pathsToBackup, "firecracker-work/")
	default:
		return fmt.Errorf("unknown backup type: %s", backupType)
	}

	// Add each path to the backup
	for _, path := range pathsToBackup {
		if err := bm.addPathToBackup(ctx, tarWriter, path); err != nil {
			log.Printf("Warning: Failed to backup path %s: %v", path, err)
			// Continue with other paths
		}
	}

	return nil
}

// addPathToBackup adds a path (file or directory) to the backup
func (bm *BackupManager) addPathToBackup(ctx context.Context, tarWriter *tar.Writer, path string) error {
	return filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip files that can't be accessed
			return nil
		}

		// Skip if context is cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header: %w", err)
		}

		// Use relative path in archive
		header.Name = filePath

		// Write header
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		// Write file content if it's a regular file
		if info.Mode().IsRegular() {
			file, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open file: %w", err)
			}
			defer file.Close()

			if _, err := io.Copy(tarWriter, file); err != nil {
				return fmt.Errorf("failed to copy file content: %w", err)
			}
		}

		return nil
	})
}

// verifyBackup verifies the integrity of a backup
func (bm *BackupManager) verifyBackup(ctx context.Context, backupPath string) error {
	file, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}
	defer file.Close()

	var reader io.Reader = file

	// Handle compressed backups
	if strings.HasSuffix(backupPath, ".gz") {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	// Verify tar archive
	tarReader := tar.NewReader(reader)
	fileCount := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar verification failed: %w", err)
		}

		fileCount++

		// Skip file content for verification (just check headers)
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return fmt.Errorf("failed to read file content: %w", err)
			}
		}
	}

	if fileCount == 0 {
		return fmt.Errorf("backup appears to be empty")
	}

	log.Printf("Backup verification successful: %d files", fileCount)
	return nil
}

// ListBackups returns a list of available backups
func (bm *BackupManager) ListBackups() ([]BackupEntry, error) {
	files, err := os.ReadDir(bm.backupDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	var backups []BackupEntry
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "microvm-service-") {
			info, err := file.Info()
			if err != nil {
				continue
			}

			// Parse backup type from filename
			parts := strings.Split(file.Name(), "-")
			backupType := "unknown"
			if len(parts) >= 3 {
				backupType = parts[2]
			}

			backups = append(backups, BackupEntry{
				Filename:   file.Name(),
				Path:       filepath.Join(bm.backupDir, file.Name()),
				Size:       info.Size(),
				CreatedAt:  info.ModTime(),
				Type:       backupType,
				Compressed: strings.HasSuffix(file.Name(), ".gz"),
			})
		}
	}

	return backups, nil
}

// CleanupOldBackups removes backups older than the retention period
func (bm *BackupManager) CleanupOldBackups(ctx context.Context) error {
	backups, err := bm.ListBackups()
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	cutoff := time.Now().AddDate(0, 0, -bm.config.RetentionDays)
	cleaned := 0

	for _, backup := range backups {
		if backup.CreatedAt.Before(cutoff) {
			if err := os.Remove(backup.Path); err != nil {
				log.Printf("Failed to remove old backup %s: %v", backup.Filename, err)
			} else {
				cleaned++
				bm.backupStats.TotalBackupSize -= backup.Size
			}
		}
	}

	if cleaned > 0 {
		log.Printf("Cleaned up %d old backups", cleaned)
	}

	return nil
}

// RestoreFromBackup restores from a backup (placeholder implementation)
func (bm *BackupManager) RestoreFromBackup(ctx context.Context, backupPath, restoreDir string) error {
	log.Printf("Restoring from backup: %s to %s", backupPath, restoreDir)
	
	// Create restore directory
	if err := os.MkdirAll(restoreDir, 0755); err != nil {
		return fmt.Errorf("failed to create restore directory: %w", err)
	}

	// Open backup file
	file, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}
	defer file.Close()

	var reader io.Reader = file

	// Handle compressed backups
	if strings.HasSuffix(backupPath, ".gz") {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	// Extract tar archive
	tarReader := tar.NewReader(reader)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar extraction failed: %w", err)
		}

		// Create the file path
		targetPath := filepath.Join(restoreDir, header.Name)

		// Create directory if needed
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
			continue
		}

		// Create parent directory
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}

		// Extract regular file
		if header.Typeflag == tar.TypeReg {
			outFile, err := os.Create(targetPath)
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to extract file: %w", err)
			}

			outFile.Close()

			// Set file permissions
			if err := os.Chmod(targetPath, os.FileMode(header.Mode)); err != nil {
				log.Printf("Warning: Failed to set permissions for %s: %v", targetPath, err)
			}
		}
	}

	log.Printf("Restore completed successfully to: %s", restoreDir)
	return nil
}

// GetBackupStats returns backup statistics
func (bm *BackupManager) GetBackupStats() BackupStats {
	bm.mutex.RLock()
	defer bm.mutex.RUnlock()
	
	return bm.backupStats
}

// ScheduleBackups starts automatic backup scheduling
func (bm *BackupManager) ScheduleBackups(ctx context.Context) error {
	ticker := time.NewTicker(bm.config.BackupInterval)
	defer ticker.Stop()

	log.Printf("Backup scheduler started with %v interval", bm.config.BackupInterval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := bm.CreateBackup(ctx, "full"); err != nil {
				log.Printf("Scheduled backup failed: %v", err)
			}
			
			// Cleanup old backups
			if err := bm.CleanupOldBackups(ctx); err != nil {
				log.Printf("Backup cleanup failed: %v", err)
			}
		}
	}
}