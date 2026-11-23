package main

import (
	"fmt"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
)

// StorageDemo demonstrates the storage integration capabilities
func main() {
	fmt.Println("=== MicroVM Storage Integration Demo ===")
	
	// Demonstrate storage capabilities
	demonstrateStorageCapabilities()
	
	// Demonstrate storage configuration validation
	demonstrateStorageValidation()
	
	// Demonstrate storage management operations
	demonstrateStorageManagement()
}

func demonstrateStorageCapabilities() {
	fmt.Println("\n1. Storage Type Capabilities:")
	fmt.Println("-----------------------------")
	
	// This would typically come from a service instance
	capabilities := getStorageCapabilities()
	
	for storageType, cap := range capabilities {
		fmt.Printf("\n%s:\n", storageType)
		fmt.Printf("  - Supports Resize: %v\n", cap.SupportsResize)
		fmt.Printf("  - Supports Backup: %v\n", cap.SupportsBackup)
		fmt.Printf("  - Supports Restore: %v\n", cap.SupportsRestore)
		fmt.Printf("  - Supports Snapshot: %v\n", cap.SupportsSnapshot)
		fmt.Printf("  - Requires S3 Config: %v\n", cap.RequiresS3Config)
		fmt.Printf("  - Size Range: %d MB - %d MB\n", cap.MinSizeMB, cap.MaxSizeMB)
		fmt.Printf("  - Description: %s\n", cap.Description)
	}
}

func demonstrateStorageValidation() {
	fmt.Println("\n2. Storage Configuration Validation:")
	fmt.Println("------------------------------------")
	
	testConfigs := []struct {
		name   string
		config api.StorageConfig
		valid  bool
	}{
		{
			name: "Local Storage (Valid)",
			config: api.StorageConfig{
				Type: "local",
			},
			valid: true,
		},
		{
			name: "S3 FUSE without config (Invalid)",
			config: api.StorageConfig{
				Type: "s3-fuse",
			},
			valid: false,
		},
		{
			name: "S3 FUSE with config (Valid)",
			config: api.StorageConfig{
				Type: "s3-fuse",
				S3Config: &api.S3Config{
					Bucket:    "my-storage-bucket",
					Region:    "us-east-1",
					AccessKey: "AKIAIOSFODNN7EXAMPLE",
					SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				},
			},
			valid: true,
		},
		{
			name: "Hybrid Storage with config (Valid)",
			config: api.StorageConfig{
				Type: "hybrid",
				S3Config: &api.S3Config{
					Bucket:    "my-backup-bucket",
					Region:    "us-west-2",
					AccessKey: "AKIAIOSFODNN7EXAMPLE",
					SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				},
			},
			valid: true,
		},
	}
	
	for _, tc := range testConfigs {
		fmt.Printf("\n%s:\n", tc.name)
		err := validateStorageConfig(tc.config)
		if tc.valid && err == nil {
			fmt.Printf("  ✓ Configuration is valid\n")
		} else if !tc.valid && err != nil {
			fmt.Printf("  ✓ Configuration correctly rejected: %v\n", err)
		} else if tc.valid && err != nil {
			fmt.Printf("  ✗ Configuration incorrectly rejected: %v\n", err)
		} else {
			fmt.Printf("  ✗ Configuration should have been rejected\n")
		}
	}
}

func demonstrateStorageManagement() {
	fmt.Println("\n3. Storage Management Operations:")
	fmt.Println("---------------------------------")
	
	// Example microVM provisioning requests with different storage types
	requests := []struct {
		name    string
		request api.ProvisionMicroVMRequest
	}{
		{
			name: "High-Performance Local Storage",
			request: api.ProvisionMicroVMRequest{
				Resources: api.ResourceSpecs{
					VCPUs:     2,
					MemoryMB:  2048,
					StorageMB: 10240, // 10GB
				},
				Storage: api.StorageConfig{
					Type: "local",
				},
				Network: api.NetworkConfig{
					Mode: "nat",
				},
				CustomerID: "customer-123",
			},
		},
		{
			name: "Cost-Effective S3 Storage",
			request: api.ProvisionMicroVMRequest{
				Resources: api.ResourceSpecs{
					VCPUs:     1,
					MemoryMB:  1024,
					StorageMB: 51200, // 50GB
				},
				Storage: api.StorageConfig{
					Type: "s3-fuse",
					S3Config: &api.S3Config{
						Bucket:    "microvm-storage",
						Region:    "us-east-1",
						AccessKey: "AKIAIOSFODNN7EXAMPLE",
						SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
					},
				},
				Network: api.NetworkConfig{
					Mode: "dhcp",
				},
				CustomerID: "customer-456",
			},
		},
		{
			name: "Hybrid Storage with Backup",
			request: api.ProvisionMicroVMRequest{
				Resources: api.ResourceSpecs{
					VCPUs:     4,
					MemoryMB:  4096,
					StorageMB: 20480, // 20GB
				},
				Storage: api.StorageConfig{
					Type: "hybrid",
					S3Config: &api.S3Config{
						Bucket:    "microvm-backups",
						Region:    "us-west-2",
						AccessKey: "AKIAIOSFODNN7EXAMPLE",
						SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
					},
				},
				Network: api.NetworkConfig{
					Mode: "macvlan",
				},
				CustomerID: "customer-789",
			},
		},
	}
	
	for _, req := range requests {
		fmt.Printf("\n%s:\n", req.name)
		fmt.Printf("  - Storage Type: %s\n", req.request.Storage.Type)
		fmt.Printf("  - Storage Size: %d MB\n", req.request.Resources.StorageMB)
		fmt.Printf("  - S3 Integration: %v\n", req.request.Storage.S3Config != nil)
		
		// Show what operations would be available
		capabilities := getStorageCapabilities()
		if cap, exists := capabilities[models.StorageType(req.request.Storage.Type)]; exists {
			fmt.Printf("  - Available Operations:\n")
			if cap.SupportsResize {
				fmt.Printf("    • Resize storage\n")
			}
			if cap.SupportsBackup {
				fmt.Printf("    • Backup to S3\n")
			}
			if cap.SupportsRestore {
				fmt.Printf("    • Restore from S3\n")
			}
			if cap.SupportsSnapshot {
				fmt.Printf("    • Create snapshots\n")
			}
		}
		
		// Simulate storage management operations
		fmt.Printf("  - Estimated Setup Time: %s\n", estimateSetupTime(req.request.Storage.Type))
		fmt.Printf("  - Performance Profile: %s\n", getPerformanceProfile(req.request.Storage.Type))
	}
	
	// Demonstrate storage resize operation
	fmt.Printf("\nStorage Resize Example:\n")
	fmt.Printf("  Original Size: 10 GB\n")
	fmt.Printf("  New Size: 20 GB\n")
	fmt.Printf("  Operation: Extend local storage and resize filesystem\n")
	fmt.Printf("  Downtime: None (online resize supported)\n")
	
	// Demonstrate backup operation
	fmt.Printf("\nStorage Backup Example:\n")
	fmt.Printf("  Source: Local storage (microvm-123)\n")
	fmt.Printf("  Destination: S3 bucket (microvm-backups)\n")
	fmt.Printf("  Backup ID: microvm-123-20241102-143022\n")
	fmt.Printf("  Operation: Create compressed backup of rootfs and data disks\n")
}

// Helper functions

func getStorageCapabilities() map[models.StorageType]api.StorageCapabilities {
	return map[models.StorageType]api.StorageCapabilities{
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

func validateStorageConfig(config api.StorageConfig) error {
	capabilities := getStorageCapabilities()
	
	storageType := models.StorageType(config.Type)
	cap, exists := capabilities[storageType]
	if !exists {
		return fmt.Errorf("unsupported storage type: %s", config.Type)
	}
	
	if cap.RequiresS3Config && config.S3Config == nil {
		return fmt.Errorf("S3 configuration is required for storage type: %s", config.Type)
	}
	
	return nil
}

func estimateSetupTime(storageType string) string {
	switch models.StorageType(storageType) {
	case models.StorageTypeLocal:
		return "< 30 seconds"
	case models.StorageTypeS3Fuse:
		return "1-2 minutes"
	case models.StorageTypeS3NBD:
		return "30-60 seconds"
	case models.StorageTypeHybrid:
		return "1-3 minutes"
	default:
		return "Unknown"
	}
}

func getPerformanceProfile(storageType string) string {
	switch models.StorageType(storageType) {
	case models.StorageTypeLocal:
		return "High IOPS, Low Latency"
	case models.StorageTypeS3Fuse:
		return "Moderate IOPS, Higher Latency"
	case models.StorageTypeS3NBD:
		return "Good IOPS, Moderate Latency"
	case models.StorageTypeHybrid:
		return "High IOPS (cached), Durable (S3)"
	default:
		return "Unknown"
	}
}