package storage

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ZFSBackend implements storage using ZFS zvols
type ZFSBackend struct {
	pool    string
	dataset string
	baseDir string
}

// NewZFSBackend creates a new ZFS storage backend
func NewZFSBackend(pool, dataset, baseDir string) (*ZFSBackend, error) {
	if pool == "" {
		return nil, fmt.Errorf("ZFS pool name is required")
	}
	if dataset == "" {
		dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
	}
	if baseDir == "" {
		baseDir = "/opt/vmlet/zvols"
	}

	// Verify ZFS is available
	if err := exec.Command("zfs", "list", pool).Run(); err != nil {
		return nil, fmt.Errorf("ZFS pool '%s' not found or ZFS not available: %w", pool, err)
	}

	// Create dataset if it doesn't exist
	if err := exec.Command("zfs", "list", dataset).Run(); err != nil {
		fmt.Printf("Creating ZFS dataset: %s\n", dataset)
		cmd := exec.Command("zfs", "create",
			"-o", fmt.Sprintf("mountpoint=%s", baseDir),
			"-o", "compression=lz4",
			"-o", "recordsize=64k",
			dataset)
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("failed to create ZFS dataset: %s: %w", string(output), err)
		}
	}

	return &ZFSBackend{
		pool:    pool,
		dataset: dataset,
		baseDir: baseDir,
	}, nil
}

// CreateDisk creates a new ZFS zvol for a VM
func (z *ZFSBackend) CreateDisk(vmID string, sizeGB int) (string, error) {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	// Check if zvol already exists
	if err := exec.Command("zfs", "list", zvolName).Run(); err == nil {
		return z.GetDiskPath(vmID), nil
	}

	// Create zvol with sparse provisioning (like LXD) - no refreservation
	volsize := fmt.Sprintf("%dG", sizeGB)
	cmd := exec.Command("zfs", "create",
		"-s", // sparse zvol - thin provisioning, no space reservation
		"-V", volsize,
		"-o", "compression=lz4",
		"-o", "volblocksize=64k",
		zvolName)

	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to create zvol: %s: %w", string(output), err)
	}

	devicePath := z.GetDiskPath(vmID)

	// Wait for device to appear
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(devicePath); err == nil {
			fmt.Printf("✅ Created ZFS zvol: %s (%s)\n", zvolName, devicePath)
			return devicePath, nil
		}
		exec.Command("sleep", "0.1").Run()
	}

	return devicePath, nil
}

// GetDiskPath returns the device path for a VM's zvol
func (z *ZFSBackend) GetDiskPath(vmID string) string {
	// ZFS zvols appear as /dev/zvol/pool/dataset/name
	return fmt.Sprintf("/dev/zvol/%s/%s", z.dataset, vmID)
}

// DeleteDisk removes a VM's zvol
func (z *ZFSBackend) DeleteDisk(vmID string) error {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	// Check if zvol exists
	if err := exec.Command("zfs", "list", zvolName).Run(); err != nil {
		return nil // Already deleted
	}

	// A leftover loop-mount of the zvol (e.g. the cloud-init seeding step's
	// `mount -o loop`) keeps the dataset open and every `zfs destroy` fails
	// with "dataset is busy" — the reinstall then silently reuses the stale
	// disk. Unmount anything still mounted from this zvol first, then rebuild
	// the kernel's block-device references; without the udev trigger a zvol
	// that was mounted briefly stays "busy" for minutes even with no holder.
	z.unmountZvolMounts(z.GetDiskPath(vmID))
	_ = exec.Command("udevadm", "trigger", "--subsystem-match=block").Run()
	_ = exec.Command("udevadm", "settle").Run()

	var lastOutput string
	for attempt := 0; attempt < 6; attempt++ {
		output, err := exec.Command("zfs", "destroy", "-r", zvolName).CombinedOutput()
		if err == nil {
			fmt.Printf("✅ Deleted ZFS zvol: %s\n", zvolName)
			return nil
		}
		lastOutput = strings.TrimSpace(string(output))
		if strings.Contains(lastOutput, "dependent clones") {
			// Operator inspections leave `inspect-*` clones behind; their
			// existence makes the origin undeletable and leaks the zvol.
			z.destroyInspectClones(zvolName)
		} else if !strings.Contains(lastOutput, "busy") {
			return fmt.Errorf("failed to destroy zvol: %s: %w", lastOutput, err)
		}
		// "dataset is busy" is transient: give the kernel and udev time to
		// release the device references, then retry with a growing delay.
		time.Sleep(time.Duration(2+attempt*2) * time.Second)
		z.unmountZvolMounts(z.GetDiskPath(vmID))
	}
	return fmt.Errorf("failed to destroy zvol: %s", lastOutput)
}

// destroyInspectClones removes the platform's own `inspect-*` clones of a VM
// disk. A manual inspection clones the zvol and mounts it under /mnt; if the
// operator forgets to clean up, the clone makes the origin zvol undeletable
// ("volume has dependent clones") and the address/storage stay allocated
// forever. Only clones literally named inspect-* are touched.
func (z *ZFSBackend) destroyInspectClones(zvolName string) {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", "clones", zvolName).Output()
	if err != nil {
		return
	}
	for _, clone := range strings.Fields(string(out)) {
		if clone == "-" || clone == "" {
			continue
		}
		base := clone
		if idx := strings.LastIndex(clone, "/"); idx >= 0 {
			base = clone[idx+1:]
		}
		if !strings.HasPrefix(base, "inspect-") {
			continue
		}
		if _, statErr := os.Stat("/mnt/" + base); statErr == nil {
			_, _ = exec.Command("umount", "-l", "/mnt/"+base).CombinedOutput()
		}
		if out, err := exec.Command("zfs", "destroy", "-r", clone).CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Could not destroy inspect clone %s: %v: %s\n", clone, err, strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("🧽 Removed inspect clone %s\n", clone)
		}
	}
}

// unmountZvolMounts lazy-unmounts every mount whose source is the given zvol
// device path. Lazy (-l) is deliberate: the mount may be held by a process
// that is on its way out, and blocking here would stall the API request.
func (z *ZFSBackend) unmountZvolMounts(zvolPath string) {
	if zvolPath == "" {
		return
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != zvolPath {
			continue
		}
		mountPoint := strings.ReplaceAll(fields[1], "\\040", " ")
		if out, err := exec.Command("umount", "-l", mountPoint).CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Could not unmount %s (source %s): %v: %s\n", mountPoint, zvolPath, err, string(out))
		} else {
			fmt.Printf("🧽 Unmounted leftover %s mount at %s\n", zvolPath, mountPoint)
		}
	}
}

// SnapshotDisk creates a snapshot of a VM's disk
func (z *ZFSBackend) SnapshotDisk(vmID, snapshotName string) error {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)
	snapName := fmt.Sprintf("%s@%s", zvolName, snapshotName)

	cmd := exec.Command("zfs", "snapshot", snapName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create snapshot: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Created snapshot: %s\n", snapName)
	return nil
}

// CloneDisk creates a clone of a VM's disk from a snapshot
func (z *ZFSBackend) CloneDisk(sourceVMID, snapshotName, targetVMID string) error {
	sourceSnap := fmt.Sprintf("%s/%s@%s", z.dataset, sourceVMID, snapshotName)
	targetZvol := fmt.Sprintf("%s/%s", z.dataset, targetVMID)

	cmd := exec.Command("zfs", "clone", sourceSnap, targetZvol)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to clone zvol: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Cloned %s to %s\n", sourceSnap, targetZvol)
	return nil
}

// CopyDisk creates a full independent copy of a VM's disk from a snapshot
func (z *ZFSBackend) CopyDisk(sourceVMID, snapshotName, targetVMID string) error {
	sourceSnap := fmt.Sprintf("%s/%s@%s", z.dataset, sourceVMID, snapshotName)
	targetZvol := fmt.Sprintf("%s/%s", z.dataset, targetVMID)

	// First, clone the snapshot (fast COW operation)
	cmd := exec.Command("zfs", "clone", sourceSnap, targetZvol)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to clone zvol: %s: %w", string(output), err)
	}

	fmt.Printf("📋 Created temporary clone %s from %s\n", targetZvol, sourceSnap)

	// Promote the clone to make it independent from the source
	cmd = exec.Command("zfs", "promote", targetZvol)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Clean up clone on failure
		exec.Command("zfs", "destroy", targetZvol).Run()
		return fmt.Errorf("failed to promote clone: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Promoted %s to independent zvol (full copy)\n", targetZvol)
	return nil
}

// ListSnapshots lists all snapshots for a VM
func (z *ZFSBackend) ListSnapshots(vmID string) ([]string, error) {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	cmd := exec.Command("zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", zvolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var snapshots []string
	for _, line := range lines {
		if line != "" && strings.Contains(line, "@") {
			// Extract snapshot name (after @)
			parts := strings.Split(line, "@")
			if len(parts) == 2 {
				snapshots = append(snapshots, parts[1])
			}
		}
	}

	return snapshots, nil
}

// GetSnapshotInfo returns detailed information about snapshots including size for billing
func (z *ZFSBackend) GetSnapshotInfo(vmID string) ([]SnapshotInfo, error) {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	// Get snapshot properties: name, creation time, used space (delta), referenced space
	cmd := exec.Command("zfs", "list", "-H", "-t", "snapshot", "-o", "name,creation,used,refer", "-p", "-r", zvolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot info: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var snapshots []SnapshotInfo
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		fullName := fields[0]
		if !strings.Contains(fullName, "@") {
			continue
		}

		// Extract snapshot name (after @)
		parts := strings.Split(fullName, "@")
		if len(parts) != 2 {
			continue
		}
		snapName := parts[1]

		// Parse creation time (Unix timestamp)
		createdAt := fields[1]

		// Parse used bytes (unique data, delta from parent)
		usedBytes, _ := strconv.ParseInt(fields[2], 10, 64)

		// Parse referenced bytes (total data)
		referBytes, _ := strconv.ParseInt(fields[3], 10, 64)

		snapshots = append(snapshots, SnapshotInfo{
			Name:       snapName,
			CreatedAt:  createdAt,
			UsedBytes:  usedBytes,
			ReferBytes: referBytes,
			UsedHuman:  humanizeBytes(usedBytes),
			ReferHuman: humanizeBytes(referBytes),
		})
	}

	return snapshots, nil
}

// RollbackToSnapshot rolls back a VM disk to a specific snapshot
func (z *ZFSBackend) RollbackToSnapshot(vmID, snapshotName string) error {
	snapFullName := fmt.Sprintf("%s/%s@%s", z.dataset, vmID, snapshotName)

	// ZFS rollback requires -r flag to destroy snapshots created after the target
	cmd := exec.Command("zfs", "rollback", "-r", snapFullName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to rollback to snapshot: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Rolled back %s/%s to snapshot %s\n", z.dataset, vmID, snapshotName)
	return nil
}

// DeleteSnapshot removes a specific snapshot
func (z *ZFSBackend) DeleteSnapshot(vmID, snapshotName string) error {
	snapFullName := fmt.Sprintf("%s/%s@%s", z.dataset, vmID, snapshotName)

	cmd := exec.Command("zfs", "destroy", snapFullName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete snapshot: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Deleted snapshot: %s\n", snapFullName)
	return nil
}

// humanizeBytes converts bytes to human-readable format
func humanizeBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"K", "M", "G", "T", "P"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.1f%s", float64(bytes)/float64(div), units[exp])
}

// GetDiskSize returns the size of a VM's disk in GB
func (z *ZFSBackend) GetDiskSize(vmID string) (int, error) {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	cmd := exec.Command("zfs", "get", "-H", "-o", "value", "volsize", zvolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to get disk size: %w", err)
	}

	sizeStr := strings.TrimSpace(string(output))
	// Parse size (e.g., "10G" -> 10)
	if strings.HasSuffix(sizeStr, "G") {
		size, err := strconv.Atoi(strings.TrimSuffix(sizeStr, "G"))
		if err != nil {
			return 0, fmt.Errorf("failed to parse size: %w", err)
		}
		return size, nil
	}

	return 0, fmt.Errorf("unexpected size format: %s", sizeStr)
}

// GetDiskUsage returns the actual used space of a VM's disk in bytes
func (z *ZFSBackend) GetDiskUsage(vmID string) (int64, error) {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)

	cmd := exec.Command("zfs", "get", "-H", "-o", "value", "-p", "used", zvolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to get disk usage: %w", err)
	}

	used, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse usage: %w", err)
	}

	return used, nil
}

// ResizeDisk resizes a VM's disk (grow-only, newSizeMB is the exact target size in MiB)
func (z *ZFSBackend) ResizeDisk(vmID string, newSizeMB int) error {
	zvolName := fmt.Sprintf("%s/%s", z.dataset, vmID)
	volsize := fmt.Sprintf("%dM", newSizeMB)

	cmd := exec.Command("zfs", "set", fmt.Sprintf("volsize=%s", volsize), zvolName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to resize zvol: %s: %w", string(output), err)
	}

	fmt.Printf("✅ Resized zvol %s to %s\n", zvolName, volsize)
	return nil
}

// GetStats returns statistics about the ZFS storage backend
func (z *ZFSBackend) GetStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Get dataset info
	cmd := exec.Command("zfs", "list", "-H", "-o", "used,avail,refer,compressratio", z.dataset)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get dataset stats: %w", err)
	}

	fields := strings.Fields(string(output))
	if len(fields) >= 4 {
		stats["used"] = fields[0]
		stats["available"] = fields[1]
		stats["referenced"] = fields[2]
		stats["compression_ratio"] = fields[3]
	}

	// Count zvols
	cmd = exec.Command("zfs", "list", "-H", "-t", "volume", "-r", z.dataset)
	output, err = cmd.CombinedOutput()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		count := 0
		for _, line := range lines {
			if line != "" {
				count++
			}
		}
		stats["zvol_count"] = count
	}

	stats["pool"] = z.pool
	stats["dataset"] = z.dataset
	stats["base_dir"] = z.baseDir

	return stats, nil
}
