package models

import (
	"time"
)

// ResourceCapacity represents the total capacity of a node
type ResourceCapacity struct {
	TotalCPU      uint32 `json:"total_cpu"`
	TotalMemoryMB uint32 `json:"total_memory_mb"`
	TotalStorageMB uint32 `json:"total_storage_mb"`
}

// ResourceUsage represents current resource usage on a node
type ResourceUsage struct {
	LXDCPU              uint32 `json:"lxd_cpu"`
	LXDMemoryMB         uint32 `json:"lxd_memory_mb"`
	LXDStorageMB        uint32 `json:"lxd_storage_mb"`
	FirecrackerCPU      uint32 `json:"firecracker_cpu"`
	FirecrackerMemoryMB uint32 `json:"firecracker_memory_mb"`
	FirecrackerStorageMB uint32 `json:"firecracker_storage_mb"`
}

// ResourceAvailability represents available resources for microVM allocation
type ResourceAvailability struct {
	AvailableCPU      uint32 `json:"available_cpu"`
	AvailableMemoryMB uint32 `json:"available_memory_mb"`
	AvailableStorageMB uint32 `json:"available_storage_mb"`
}

// ResourcePool represents the complete resource state of a node
type ResourcePool struct {
	NodeID       string                `json:"node_id"`
	Capacity     ResourceCapacity      `json:"capacity"`
	Usage        ResourceUsage         `json:"usage"`
	Availability ResourceAvailability  `json:"availability"`
	LastUpdated  time.Time             `json:"last_updated"`
}

// CalculateAvailability calculates available resources based on capacity and usage
// Maintains 20% buffer for LXD workloads as per requirements
func (rp *ResourcePool) CalculateAvailability() {
	const bufferPercentage = 0.20

	// Calculate total used resources
	totalUsedCPU := rp.Usage.LXDCPU + rp.Usage.FirecrackerCPU
	totalUsedMemory := rp.Usage.LXDMemoryMB + rp.Usage.FirecrackerMemoryMB
	totalUsedStorage := rp.Usage.LXDStorageMB + rp.Usage.FirecrackerStorageMB

	// Calculate buffer (20% of total capacity)
	bufferCPU := uint32(float64(rp.Capacity.TotalCPU) * bufferPercentage)
	bufferMemory := uint32(float64(rp.Capacity.TotalMemoryMB) * bufferPercentage)
	bufferStorage := uint32(float64(rp.Capacity.TotalStorageMB) * bufferPercentage)

	// Calculate available resources (capacity - used - buffer)
	rp.Availability.AvailableCPU = rp.calculateAvailable(rp.Capacity.TotalCPU, totalUsedCPU, bufferCPU)
	rp.Availability.AvailableMemoryMB = rp.calculateAvailable(rp.Capacity.TotalMemoryMB, totalUsedMemory, bufferMemory)
	rp.Availability.AvailableStorageMB = rp.calculateAvailable(rp.Capacity.TotalStorageMB, totalUsedStorage, bufferStorage)

	rp.LastUpdated = time.Now()
}

// calculateAvailable is a helper function to calculate available resources
func (rp *ResourcePool) calculateAvailable(total, used, buffer uint32) uint32 {
	if used+buffer >= total {
		return 0
	}
	return total - used - buffer
}

// GetUtilizationPercentage returns the current utilization percentage
func (rp *ResourcePool) GetUtilizationPercentage() float64 {
	totalUsedCPU := rp.Usage.LXDCPU + rp.Usage.FirecrackerCPU
	totalUsedMemory := rp.Usage.LXDMemoryMB + rp.Usage.FirecrackerMemoryMB
	totalUsedStorage := rp.Usage.LXDStorageMB + rp.Usage.FirecrackerStorageMB

	// Calculate average utilization across all resource types
	cpuUtil := float64(totalUsedCPU) / float64(rp.Capacity.TotalCPU)
	memUtil := float64(totalUsedMemory) / float64(rp.Capacity.TotalMemoryMB)
	storageUtil := float64(totalUsedStorage) / float64(rp.Capacity.TotalStorageMB)

	return (cpuUtil + memUtil + storageUtil) / 3.0
}

// CanAllocate checks if the requested resources can be allocated
func (rp *ResourcePool) CanAllocate(specs ResourceSpecs) bool {
	return rp.Availability.AvailableCPU >= specs.VCPUs &&
		rp.Availability.AvailableMemoryMB >= specs.MemoryMB &&
		rp.Availability.AvailableStorageMB >= specs.StorageMB
}

// AllocateResources allocates the specified resources (updates usage)
func (rp *ResourcePool) AllocateResources(specs ResourceSpecs) error {
	if !rp.CanAllocate(specs) {
		return ErrInsufficientResources
	}

	// Update firecracker usage
	rp.Usage.FirecrackerCPU += specs.VCPUs
	rp.Usage.FirecrackerMemoryMB += specs.MemoryMB
	rp.Usage.FirecrackerStorageMB += specs.StorageMB

	// Recalculate availability
	rp.CalculateAvailability()

	return nil
}

// DeallocateResources deallocates the specified resources (updates usage)
func (rp *ResourcePool) DeallocateResources(specs ResourceSpecs) {
	// Update firecracker usage (ensure we don't go below zero)
	if rp.Usage.FirecrackerCPU >= specs.VCPUs {
		rp.Usage.FirecrackerCPU -= specs.VCPUs
	} else {
		rp.Usage.FirecrackerCPU = 0
	}

	if rp.Usage.FirecrackerMemoryMB >= specs.MemoryMB {
		rp.Usage.FirecrackerMemoryMB -= specs.MemoryMB
	} else {
		rp.Usage.FirecrackerMemoryMB = 0
	}

	if rp.Usage.FirecrackerStorageMB >= specs.StorageMB {
		rp.Usage.FirecrackerStorageMB -= specs.StorageMB
	} else {
		rp.Usage.FirecrackerStorageMB = 0
	}

	// Recalculate availability
	rp.CalculateAvailability()
}

// IsOverloaded checks if the node is overloaded (>80% utilization)
func (rp *ResourcePool) IsOverloaded() bool {
	return rp.GetUtilizationPercentage() > 0.80
}

// GetAvailabilityTier returns the availability tier based on free resources
func (rp *ResourcePool) GetAvailabilityTier() AvailabilityTier {
	// Calculate average availability percentage
	cpuAvail := float64(rp.Availability.AvailableCPU) / float64(rp.Capacity.TotalCPU)
	memAvail := float64(rp.Availability.AvailableMemoryMB) / float64(rp.Capacity.TotalMemoryMB)
	storageAvail := float64(rp.Availability.AvailableStorageMB) / float64(rp.Capacity.TotalStorageMB)

	avgAvailability := (cpuAvail + memAvail + storageAvail) / 3.0

	if avgAvailability > 0.60 {
		return AvailabilityTierHigh
	} else if avgAvailability > 0.30 {
		return AvailabilityTierMedium
	}
	return AvailabilityTierLow
}