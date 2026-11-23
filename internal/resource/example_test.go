package resource

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

// ExampleResourceService demonstrates how to use the resource service
func ExampleResourceService() {
	// Create configuration
	cfg := config.DefaultConfig()
	cfg.Development.SimulateLXD = true // Use mock LXD for this example
	
	// Create resource service
	service, err := NewService(cfg, "example-node-1")
	if err != nil {
		log.Fatalf("Failed to create resource service: %v", err)
	}
	
	// Start the service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	if err := service.Start(ctx); err != nil {
		log.Fatalf("Failed to start resource service: %v", err)
	}
	defer service.Stop()
	
	// Wait for initial resource discovery
	time.Sleep(2 * time.Second)
	
	// Check resource availability
	pool, err := service.GetResourceAvailability()
	if err != nil {
		log.Fatalf("Failed to get resource availability: %v", err)
	}
	
	fmt.Printf("Node: %s\n", pool.NodeID)
	fmt.Printf("Available CPU: %d/%d\n", pool.Availability.AvailableCPU, pool.Capacity.TotalCPU)
	fmt.Printf("Available Memory: %d/%d MB\n", pool.Availability.AvailableMemoryMB, pool.Capacity.TotalMemoryMB)
	fmt.Printf("Available Storage: %d/%d MB\n", pool.Availability.AvailableStorageMB, pool.Capacity.TotalStorageMB)
	
	// Define microVM specifications
	specs := models.ResourceSpecs{
		VCPUs:     2,
		MemoryMB:  2048,
		StorageMB: 20480, // 20GB
	}
	
	// Check if we can accept this microVM
	canAccept, reason, err := service.CanAcceptMicroVM(specs)
	if err != nil {
		log.Fatalf("Failed to check microVM acceptance: %v", err)
	}
	
	fmt.Printf("Can accept microVM: %t (%s)\n", canAccept, reason)
	
	if canAccept {
		// Allocate resources for the microVM
		vmID := "example-vm-1"
		if err := service.AllocateResourcesForMicroVM(vmID, specs); err != nil {
			log.Fatalf("Failed to allocate resources: %v", err)
		}
		
		fmt.Printf("Successfully allocated resources for %s\n", vmID)
		
		// Get utilization statistics
		stats, err := service.GetResourceUtilization()
		if err != nil {
			log.Fatalf("Failed to get utilization stats: %v", err)
		}
		
		fmt.Printf("Resource Utilization:\n")
		fmt.Printf("  CPU: %.1f%%\n", stats.CPUUtilization*100)
		fmt.Printf("  Memory: %.1f%%\n", stats.MemoryUtilization*100)
		fmt.Printf("  Storage: %.1f%%\n", stats.StorageUtilization*100)
		fmt.Printf("  Overall: %.1f%%\n", stats.OverallUtilization*100)
		fmt.Printf("  Allocated MicroVMs: %d\n", stats.AllocatedMicroVMs)
		fmt.Printf("  Availability Tier: %s\n", stats.AvailabilityTier)
		
		// Simulate some time passing
		time.Sleep(3 * time.Second)
		
		// Deallocate resources
		if err := service.DeallocateResourcesForMicroVM(vmID); err != nil {
			log.Fatalf("Failed to deallocate resources: %v", err)
		}
		
		fmt.Printf("Successfully deallocated resources for %s\n", vmID)
	}
	
	// Get health status
	health := service.GetHealthStatus()
	fmt.Printf("Service Health: %s\n", health.Status)
	fmt.Printf("Recent Conflicts: %d\n", health.RecentConflicts)
	
	// Get recent conflicts
	conflicts := service.GetRecentConflicts(5)
	if len(conflicts) > 0 {
		fmt.Printf("Recent Conflicts:\n")
		for _, conflict := range conflicts {
			fmt.Printf("  [%s] %s: %s\n", conflict.Severity, conflict.Type, conflict.Message)
		}
	} else {
		fmt.Println("No recent conflicts")
	}
	
	// Output:
	// Node: example-node-1
	// Available CPU: 10/16
	// Available Memory: 18000/32768 MB
	// Available Storage: 700000/1048576 MB
	// Can accept microVM: true (resources available)
	// Successfully allocated resources for example-vm-1
	// Resource Utilization:
	//   CPU: 37.5%
	//   Memory: 61.2%
	//   Storage: 72.1%
	//   Overall: 56.9%
	//   Allocated MicroVMs: 1
	//   Availability Tier: medium
	// Successfully deallocated resources for example-vm-1
	// Service Health: healthy
	// Recent Conflicts: 0
	// No recent conflicts
}