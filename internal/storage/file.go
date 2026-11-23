package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// FileBackend implements storage using regular files (ext4 images)
type FileBackend struct {
	baseDir string
}

// NewFileBackend creates a new file-based storage backend
func NewFileBackend(baseDir string) (*FileBackend, error) {
	if baseDir == "" {
		baseDir = "/opt/vmlet/work"
	}

	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &FileBackend{
		baseDir: baseDir,
	}, nil
}

// CreateDisk creates a new ext4 image file for a VM
func (f *FileBackend) CreateDisk(vmID string, sizeGB int) (string, error) {
	diskPath := f.GetDiskPath(vmID)

	// Check if disk already exists
	if _, err := os.Stat(diskPath); err == nil {
		return diskPath, nil
	}

	// Create sparse file
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", diskPath),
		"bs=1M", "count=0", fmt.Sprintf("seek=%d", sizeGB*1024))
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to create disk file: %s: %w", string(output), err)
	}

	// Format as ext4
	cmd = exec.Command("mkfs.ext4", "-F", diskPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		os.Remove(diskPath)
		return "", fmt.Errorf("failed to format disk: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Created disk file: %s (%dGB)\n", diskPath, sizeGB)
	return diskPath, nil
}

// GetDiskPath returns the file path for a VM's disk
func (f *FileBackend) GetDiskPath(vmID string) string {
	return filepath.Join(f.baseDir, fmt.Sprintf("rootfs-%s.img", vmID))
}

// DeleteDisk removes a VM's disk file
func (f *FileBackend) DeleteDisk(vmID string) error {
	diskPath := f.GetDiskPath(vmID)

	if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete disk: %w", err)
	}

	fmt.Printf("✅ Deleted disk file: %s\n", diskPath)
	return nil
}

// GetDiskSize returns the size of a VM's disk in GB
func (f *FileBackend) GetDiskSize(vmID string) (int, error) {
	diskPath := f.GetDiskPath(vmID)

	stat, err := os.Stat(diskPath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat disk: %w", err)
	}

	sizeGB := int(stat.Size() / (1024 * 1024 * 1024))
	return sizeGB, nil
}

// GetDiskUsage returns the actual used space of a VM's disk in bytes
func (f *FileBackend) GetDiskUsage(vmID string) (int64, error) {
	diskPath := f.GetDiskPath(vmID)

	stat, err := os.Stat(diskPath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat disk: %w", err)
	}

	return stat.Size(), nil
}

// ResizeDisk resizes a VM's disk (grow-only, newSizeMB is the exact target size in MiB; requires VM to be stopped)
func (f *FileBackend) ResizeDisk(vmID string, newSizeMB int) error {
	diskPath := f.GetDiskPath(vmID)

	// Resize the file
	cmd := exec.Command("truncate", "-s", fmt.Sprintf("%dM", newSizeMB), diskPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to resize disk file: %s: %w", string(output), err)
	}

	// Resize the filesystem
	cmd = exec.Command("resize2fs", diskPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to resize filesystem: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Resized disk %s to %dMB\n", diskPath, newSizeMB)
	return nil
}

// GetStats returns statistics about the file storage backend
func (f *FileBackend) GetStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Count disk files
	files, err := filepath.Glob(filepath.Join(f.baseDir, "rootfs-*.img"))
	if err != nil {
		return nil, fmt.Errorf("failed to list disks: %w", err)
	}

	stats["disk_count"] = len(files)
	stats["base_dir"] = f.baseDir
	stats["backend_type"] = "file"

	// Calculate total size
	var totalSize int64
	for _, file := range files {
		if stat, err := os.Stat(file); err == nil {
			totalSize += stat.Size()
		}
	}
	stats["total_size_bytes"] = totalSize
	stats["total_size_gb"] = totalSize / (1024 * 1024 * 1024)

	return stats, nil
}
