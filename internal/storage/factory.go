package storage

import (
	"fmt"
	"os"
	"strings"
)

// NewStorageBackend creates a storage backend based on environment configuration
func NewStorageBackend() (StorageBackend, error) {
	useZFS := strings.ToLower(os.Getenv("MICROVM_USE_ZFS")) == "true"

	if useZFS {
		fmt.Println("🗄️  Initializing ZFS storage backend...")

		pool := os.Getenv("MICROVM_ZFS_POOL")
		if pool == "" {
			pool = "default"
		}

		dataset := os.Getenv("MICROVM_ZFS_DATASET")
		if dataset == "" {
			dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
		}

		baseDir := os.Getenv("MICROVM_ZFS_MOUNTPOINT")
		if baseDir == "" {
			baseDir = "/opt/vmlet/zvols"
		}

		backend, err := NewZFSBackend(pool, dataset, baseDir)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize ZFS backend: %w", err)
		}

		fmt.Printf("✅ ZFS backend initialized: pool=%s, dataset=%s\n", pool, dataset)
		return backend, nil
	}

	// Default to file-based storage
	fmt.Println("📁 Initializing file-based storage backend...")

	baseDir := os.Getenv("MICROVM_WORK_DIR")
	if baseDir == "" {
		baseDir = "/opt/vmlet/work"
	}

	backend, err := NewFileBackend(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize file backend: %w", err)
	}

	fmt.Printf("✅ File backend initialized: base_dir=%s\n", baseDir)
	return backend, nil
}

// IsSnapshotable checks if the storage backend supports snapshots
func IsSnapshotable(backend StorageBackend) bool {
	_, ok := backend.(SnapshotableBackend)
	return ok
}

// GetSnapshotableBackend returns the backend as a SnapshotableBackend if supported
func GetSnapshotableBackend(backend StorageBackend) (SnapshotableBackend, error) {
	if snapBackend, ok := backend.(SnapshotableBackend); ok {
		return snapBackend, nil
	}
	return nil, fmt.Errorf("storage backend does not support snapshots")
}
