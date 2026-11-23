package storage

// StorageBackend defines the interface for VM disk storage
type StorageBackend interface {
	// CreateDisk creates a new disk for a VM
	CreateDisk(vmID string, sizeGB int) (string, error)

	// GetDiskPath returns the path to a VM's disk
	GetDiskPath(vmID string) string

	// DeleteDisk removes a VM's disk
	DeleteDisk(vmID string) error

	// GetDiskSize returns the size of a VM's disk in GB
	GetDiskSize(vmID string) (int, error)

	// GetDiskUsage returns the actual used space of a VM's disk in bytes
	GetDiskUsage(vmID string) (int64, error)

	// ResizeDisk resizes a VM's disk (grow-only, newSizeMB is the exact target size in MiB)
	ResizeDisk(vmID string, newSizeMB int) error

	// GetStats returns statistics about the storage backend
	GetStats() (map[string]interface{}, error)
}

// SnapshotInfo contains detailed information about a snapshot
type SnapshotInfo struct {
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	UsedBytes  int64  `json:"used_bytes"`  // Unique data size (delta from parent)
	ReferBytes int64  `json:"refer_bytes"` // Referenced data size
	UsedHuman  string `json:"used_human"`  // Human-readable size (e.g., "125M")
	ReferHuman string `json:"refer_human"` // Human-readable referenced size
}

// SnapshotableBackend extends StorageBackend with snapshot capabilities
type SnapshotableBackend interface {
	StorageBackend

	// SnapshotDisk creates a snapshot of a VM's disk
	SnapshotDisk(vmID, snapshotName string) error

	// CloneDisk creates a clone of a VM's disk from a snapshot
	CloneDisk(sourceVMID, snapshotName, targetVMID string) error

	// CopyDisk creates a full independent copy of a VM's disk from a snapshot
	CopyDisk(sourceVMID, snapshotName, targetVMID string) error

	// ListSnapshots lists all snapshots for a VM (names only)
	ListSnapshots(vmID string) ([]string, error)

	// GetSnapshotInfo returns detailed info about snapshots including size for billing
	GetSnapshotInfo(vmID string) ([]SnapshotInfo, error)

	// RollbackToSnapshot rolls back a VM disk to a specific snapshot
	RollbackToSnapshot(vmID, snapshotName string) error

	// DeleteSnapshot removes a specific snapshot
	DeleteSnapshot(vmID, snapshotName string) error
}
