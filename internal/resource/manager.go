package resource

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

// Manager handles resource monitoring and allocation for microVMs
type Manager struct {
	lxdClient       LXDClient
	config          *config.Config
	resourcePool    *models.ResourcePool
	allocatedVMs    map[string]*models.ResourceSpecs // Track allocated resources by VM ID
	mutex           sync.RWMutex
	stopChan        chan struct{}
	conflictChan    chan ConflictEvent
	updateInterval  time.Duration
	lastHealthCheck time.Time
}

// ConflictEvent represents a resource conflict that needs attention
type ConflictEvent struct {
	Type        ConflictType
	Message     string
	Severity    ConflictSeverity
	Timestamp   time.Time
	NodeID      string
	Utilization float64
}

// ConflictType represents different types of resource conflicts
type ConflictType string

const (
	ConflictTypeOverload     ConflictType = "overload"
	ConflictTypeInsufficientResources ConflictType = "insufficient_resources"
	ConflictTypeLXDUnhealthy ConflictType = "lxd_unhealthy"
	ConflictTypeAllocationFailure ConflictType = "allocation_failure"
)

// ConflictSeverity represents the severity of a conflict
type ConflictSeverity string

const (
	ConflictSeverityLow      ConflictSeverity = "low"
	ConflictSeverityMedium   ConflictSeverity = "medium"
	ConflictSeverityHigh     ConflictSeverity = "high"
	ConflictSeverityCritical ConflictSeverity = "critical"
)

// NewManager creates a new resource manager
func NewManager(lxdClient LXDClient, cfg *config.Config, nodeID string) *Manager {
	return &Manager{
		lxdClient:      lxdClient,
		config:         cfg,
		allocatedVMs:   make(map[string]*models.ResourceSpecs),
		stopChan:       make(chan struct{}),
		conflictChan:   make(chan ConflictEvent, 100), // Buffer for conflict events
		updateInterval: time.Duration(cfg.Resources.MonitoringIntervalSec) * time.Second,
		resourcePool: &models.ResourcePool{
			NodeID:      nodeID,
			LastUpdated: time.Now(),
		},
	}
}

// Start begins the resource monitoring process
func (m *Manager) Start(ctx context.Context) error {
	log.Printf("Starting resource manager with %v update interval", m.updateInterval)
	
	// Initial resource discovery
	if err := m.updateResourcePool(ctx); err != nil {
		return fmt.Errorf("failed initial resource discovery: %w", err)
	}
	
	// Start monitoring goroutine
	go m.monitoringLoop(ctx)
	
	// Start conflict detection goroutine
	go m.conflictDetectionLoop(ctx)
	
	return nil
}

// Stop stops the resource monitoring process
func (m *Manager) Stop() {
	log.Println("Stopping resource manager")
	close(m.stopChan)
}

// GetResourcePool returns the current resource pool state
func (m *Manager) GetResourcePool() *models.ResourcePool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	// Return a copy to prevent external modifications
	pool := *m.resourcePool
	return &pool
}

// AllocateResources attempts to allocate resources for a microVM
func (m *Manager) AllocateResources(vmID string, specs models.ResourceSpecs) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	// Check if VM is already allocated
	if _, exists := m.allocatedVMs[vmID]; exists {
		return fmt.Errorf("resources already allocated for VM %s", vmID)
	}
	
	// Check if resources are available
	if !m.resourcePool.CanAllocate(specs) {
		m.sendConflictEvent(ConflictEvent{
			Type:      ConflictTypeInsufficientResources,
			Message:   fmt.Sprintf("Cannot allocate resources for VM %s: insufficient resources", vmID),
			Severity:  ConflictSeverityHigh,
			Timestamp: time.Now(),
			NodeID:    m.resourcePool.NodeID,
		})
		return models.ErrInsufficientResources
	}
	
	// Check if allocation would cause overload
	tempPool := *m.resourcePool
	if err := tempPool.AllocateResources(specs); err != nil {
		return err
	}
	
	if tempPool.IsOverloaded() {
		m.sendConflictEvent(ConflictEvent{
			Type:        ConflictTypeOverload,
			Message:     fmt.Sprintf("Allocation of VM %s would cause node overload", vmID),
			Severity:    ConflictSeverityMedium,
			Timestamp:   time.Now(),
			NodeID:      m.resourcePool.NodeID,
			Utilization: tempPool.GetUtilizationPercentage(),
		})
		return models.ErrNodeOverloaded
	}
	
	// Perform the allocation
	if err := m.resourcePool.AllocateResources(specs); err != nil {
		m.sendConflictEvent(ConflictEvent{
			Type:      ConflictTypeAllocationFailure,
			Message:   fmt.Sprintf("Failed to allocate resources for VM %s: %v", vmID, err),
			Severity:  ConflictSeverityHigh,
			Timestamp: time.Now(),
			NodeID:    m.resourcePool.NodeID,
		})
		return err
	}
	
	// Track the allocation
	specsCopy := specs
	m.allocatedVMs[vmID] = &specsCopy
	
	log.Printf("Allocated resources for VM %s: CPU=%d, Memory=%dMB, Storage=%dMB", 
		vmID, specs.VCPUs, specs.MemoryMB, specs.StorageMB)
	
	return nil
}

// DeallocateResources deallocates resources for a microVM
func (m *Manager) DeallocateResources(vmID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	// Check if VM is allocated
	specs, exists := m.allocatedVMs[vmID]
	if !exists {
		return fmt.Errorf("no resources allocated for VM %s", vmID)
	}
	
	// Deallocate resources
	m.resourcePool.DeallocateResources(*specs)
	
	// Remove from tracking
	delete(m.allocatedVMs, vmID)
	
	log.Printf("Deallocated resources for VM %s: CPU=%d, Memory=%dMB, Storage=%dMB", 
		vmID, specs.VCPUs, specs.MemoryMB, specs.StorageMB)
	
	return nil
}

// GetAllocatedResources returns the resources allocated to a specific VM
func (m *Manager) GetAllocatedResources(vmID string) (*models.ResourceSpecs, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	specs, exists := m.allocatedVMs[vmID]
	if !exists {
		return nil, fmt.Errorf("no resources allocated for VM %s", vmID)
	}
	
	// Return a copy
	specsCopy := *specs
	return &specsCopy, nil
}

// GetConflictEvents returns a channel for receiving conflict events
func (m *Manager) GetConflictEvents() <-chan ConflictEvent {
	return m.conflictChan
}

// monitoringLoop runs the main resource monitoring loop
func (m *Manager) monitoringLoop(ctx context.Context) {
	ticker := time.NewTicker(m.updateInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			if err := m.updateResourcePool(ctx); err != nil {
				log.Printf("Error updating resource pool: %v", err)
				m.sendConflictEvent(ConflictEvent{
					Type:      ConflictTypeLXDUnhealthy,
					Message:   fmt.Sprintf("Failed to update resource pool: %v", err),
					Severity:  ConflictSeverityHigh,
					Timestamp: time.Now(),
					NodeID:    m.resourcePool.NodeID,
				})
			}
		}
	}
}

// conflictDetectionLoop runs conflict detection checks
func (m *Manager) conflictDetectionLoop(ctx context.Context) {
	// Run conflict detection every 15 seconds (more frequent than resource updates)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.detectConflicts()
		}
	}
}

// updateResourcePool updates the resource pool with current data from LXD
func (m *Manager) updateResourcePool(ctx context.Context) error {
	// Check LXD health
	if !m.lxdClient.IsHealthy(ctx) {
		return fmt.Errorf("LXD client is unhealthy")
	}
	m.lastHealthCheck = time.Now()
	
	// Get system capacity
	capacity, err := m.lxdClient.GetSystemResources(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system resources: %w", err)
	}
	
	// Get current usage
	usage, err := m.lxdClient.GetResourceUsage(ctx)
	if err != nil {
		return fmt.Errorf("failed to get resource usage: %w", err)
	}
	
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	// Update resource pool
	m.resourcePool.Capacity = *capacity
	
	// Update LXD usage from API, but preserve firecracker usage from our tracking
	usage.FirecrackerCPU = m.resourcePool.Usage.FirecrackerCPU
	usage.FirecrackerMemoryMB = m.resourcePool.Usage.FirecrackerMemoryMB
	usage.FirecrackerStorageMB = m.resourcePool.Usage.FirecrackerStorageMB
	
	m.resourcePool.Usage = *usage
	
	// Recalculate availability
	m.resourcePool.CalculateAvailability()
	
	log.Printf("Resource pool updated - CPU: %d/%d, Memory: %dMB/%dMB, Storage: %dMB/%dMB", 
		m.resourcePool.Availability.AvailableCPU, m.resourcePool.Capacity.TotalCPU,
		m.resourcePool.Availability.AvailableMemoryMB, m.resourcePool.Capacity.TotalMemoryMB,
		m.resourcePool.Availability.AvailableStorageMB, m.resourcePool.Capacity.TotalStorageMB)
	
	return nil
}

// detectConflicts checks for various resource conflicts
func (m *Manager) detectConflicts() {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	// Check for overload conditions
	utilization := m.resourcePool.GetUtilizationPercentage()
	if utilization > m.config.Resources.OverloadThreshold {
		severity := ConflictSeverityMedium
		if utilization > 0.90 {
			severity = ConflictSeverityCritical
		} else if utilization > 0.85 {
			severity = ConflictSeverityHigh
		}
		
		m.sendConflictEvent(ConflictEvent{
			Type:        ConflictTypeOverload,
			Message:     fmt.Sprintf("Node utilization is %.1f%% (threshold: %.1f%%)", utilization*100, m.config.Resources.OverloadThreshold*100),
			Severity:    severity,
			Timestamp:   time.Now(),
			NodeID:      m.resourcePool.NodeID,
			Utilization: utilization,
		})
	}
	
	// Check LXD health
	if time.Since(m.lastHealthCheck) > 2*m.updateInterval {
		m.sendConflictEvent(ConflictEvent{
			Type:      ConflictTypeLXDUnhealthy,
			Message:   "LXD health check is overdue",
			Severity:  ConflictSeverityHigh,
			Timestamp: time.Now(),
			NodeID:    m.resourcePool.NodeID,
		})
	}
	
	// Check for resource availability issues
	if m.resourcePool.Availability.AvailableCPU == 0 ||
		m.resourcePool.Availability.AvailableMemoryMB < 512 ||
		m.resourcePool.Availability.AvailableStorageMB < 1024 {
		
		m.sendConflictEvent(ConflictEvent{
			Type:      ConflictTypeInsufficientResources,
			Message:   "Very low resource availability detected",
			Severity:  ConflictSeverityMedium,
			Timestamp: time.Now(),
			NodeID:    m.resourcePool.NodeID,
		})
	}
}

// sendConflictEvent sends a conflict event to the conflict channel
func (m *Manager) sendConflictEvent(event ConflictEvent) {
	select {
	case m.conflictChan <- event:
		// Event sent successfully
	default:
		// Channel is full, log the event
		log.Printf("Conflict event channel full, dropping event: %s", event.Message)
	}
}

// GetResourceUtilization returns current resource utilization statistics
func (m *Manager) GetResourceUtilization() ResourceUtilizationStats {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	totalUsedCPU := m.resourcePool.Usage.LXDCPU + m.resourcePool.Usage.FirecrackerCPU
	totalUsedMemory := m.resourcePool.Usage.LXDMemoryMB + m.resourcePool.Usage.FirecrackerMemoryMB
	totalUsedStorage := m.resourcePool.Usage.LXDStorageMB + m.resourcePool.Usage.FirecrackerStorageMB
	
	return ResourceUtilizationStats{
		NodeID:                m.resourcePool.NodeID,
		CPUUtilization:        float64(totalUsedCPU) / float64(m.resourcePool.Capacity.TotalCPU),
		MemoryUtilization:     float64(totalUsedMemory) / float64(m.resourcePool.Capacity.TotalMemoryMB),
		StorageUtilization:    float64(totalUsedStorage) / float64(m.resourcePool.Capacity.TotalStorageMB),
		OverallUtilization:    m.resourcePool.GetUtilizationPercentage(),
		AvailabilityTier:      m.resourcePool.GetAvailabilityTier(),
		AllocatedMicroVMs:     len(m.allocatedVMs),
		LastUpdated:           m.resourcePool.LastUpdated,
		LXDHealthy:            time.Since(m.lastHealthCheck) < 2*m.updateInterval,
	}
}

// ResourceUtilizationStats provides detailed utilization statistics
type ResourceUtilizationStats struct {
	NodeID                string                    `json:"node_id"`
	CPUUtilization        float64                   `json:"cpu_utilization"`
	MemoryUtilization     float64                   `json:"memory_utilization"`
	StorageUtilization    float64                   `json:"storage_utilization"`
	OverallUtilization    float64                   `json:"overall_utilization"`
	AvailabilityTier      models.AvailabilityTier   `json:"availability_tier"`
	AllocatedMicroVMs     int                       `json:"allocated_microvms"`
	LastUpdated           time.Time                 `json:"last_updated"`
	LXDHealthy            bool                      `json:"lxd_healthy"`
}

// CanAcceptNewMicroVM checks if the node can accept a new microVM with given specs
func (m *Manager) CanAcceptNewMicroVM(specs models.ResourceSpecs) (bool, string) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	// Check basic resource availability
	if !m.resourcePool.CanAllocate(specs) {
		return false, "insufficient resources available"
	}
	
	// Check if allocation would cause overload
	tempPool := *m.resourcePool
	if err := tempPool.AllocateResources(specs); err != nil {
		return false, fmt.Sprintf("allocation would fail: %v", err)
	}
	
	if tempPool.IsOverloaded() {
		return false, "allocation would cause node overload"
	}
	
	// Check LXD health
	if time.Since(m.lastHealthCheck) > 2*m.updateInterval {
		return false, "LXD health check is overdue"
	}
	
	return true, "resources available"
}