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

// Service provides high-level resource management functionality
type Service struct {
	manager      *Manager
	config       *config.Config
	nodeID       string
	mutex        sync.RWMutex
	isRunning    bool
	conflictLog  []ConflictEvent
	maxLogSize   int
}

// NewService creates a new resource service
func NewService(cfg *config.Config, nodeID string) (*Service, error) {
	// Create LXD client based on configuration
	var lxdClient LXDClient
	
	if cfg.Development.SimulateLXD || !cfg.LXD.Enabled {
		log.Println("Using mock LXD client for development/testing")
		lxdClient = NewMockLXDClient()
	} else {
		log.Printf("Using HTTP LXD client with server URL: %s", cfg.LXD.ServerURL)
		lxdClient = NewHTTPLXDClient(cfg.LXD.ServerURL, cfg.LXD.ProjectName)
	}
	
	// Create resource manager
	manager := NewManager(lxdClient, cfg, nodeID)
	
	return &Service{
		manager:     manager,
		config:      cfg,
		nodeID:      nodeID,
		conflictLog: make([]ConflictEvent, 0),
		maxLogSize:  1000, // Keep last 1000 conflict events
	}, nil
}

// Start starts the resource service
func (s *Service) Start(ctx context.Context) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if s.isRunning {
		return fmt.Errorf("resource service is already running")
	}
	
	log.Printf("Starting resource service for node %s", s.nodeID)
	
	// Start the resource manager
	if err := s.manager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start resource manager: %w", err)
	}
	
	// Start conflict event processing
	go s.processConflictEvents(ctx)
	
	s.isRunning = true
	log.Println("Resource service started successfully")
	
	return nil
}

// Stop stops the resource service
func (s *Service) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if !s.isRunning {
		return
	}
	
	log.Println("Stopping resource service")
	s.manager.Stop()
	s.isRunning = false
	log.Println("Resource service stopped")
}

// IsRunning returns whether the service is currently running
func (s *Service) IsRunning() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.isRunning
}

// GetResourceAvailability returns current resource availability
func (s *Service) GetResourceAvailability() (*models.ResourcePool, error) {
	if !s.IsRunning() {
		return nil, fmt.Errorf("resource service is not running")
	}
	
	return s.manager.GetResourcePool(), nil
}

// AllocateResourcesForMicroVM allocates resources for a new microVM
func (s *Service) AllocateResourcesForMicroVM(vmID string, specs models.ResourceSpecs) error {
	if !s.IsRunning() {
		return fmt.Errorf("resource service is not running")
	}
	
	// Validate resource specifications
	if err := s.validateResourceSpecs(specs); err != nil {
		return fmt.Errorf("invalid resource specifications: %w", err)
	}
	
	log.Printf("Attempting to allocate resources for microVM %s: CPU=%d, Memory=%dMB, Storage=%dMB", 
		vmID, specs.VCPUs, specs.MemoryMB, specs.StorageMB)
	
	return s.manager.AllocateResources(vmID, specs)
}

// DeallocateResourcesForMicroVM deallocates resources for a microVM
func (s *Service) DeallocateResourcesForMicroVM(vmID string) error {
	if !s.IsRunning() {
		return fmt.Errorf("resource service is not running")
	}
	
	log.Printf("Deallocating resources for microVM %s", vmID)
	return s.manager.DeallocateResources(vmID)
}

// GetMicroVMResources returns the resources allocated to a specific microVM
func (s *Service) GetMicroVMResources(vmID string) (*models.ResourceSpecs, error) {
	if !s.IsRunning() {
		return nil, fmt.Errorf("resource service is not running")
	}
	
	return s.manager.GetAllocatedResources(vmID)
}

// CanAcceptMicroVM checks if a microVM with given specs can be accepted
func (s *Service) CanAcceptMicroVM(specs models.ResourceSpecs) (bool, string, error) {
	if !s.IsRunning() {
		return false, "resource service is not running", fmt.Errorf("resource service is not running")
	}
	
	// Validate resource specifications
	if err := s.validateResourceSpecs(specs); err != nil {
		return false, fmt.Sprintf("invalid resource specifications: %v", err), err
	}
	
	canAccept, reason := s.manager.CanAcceptNewMicroVM(specs)
	return canAccept, reason, nil
}

// GetResourceUtilization returns detailed resource utilization statistics
func (s *Service) GetResourceUtilization() (ResourceUtilizationStats, error) {
	if !s.IsRunning() {
		return ResourceUtilizationStats{}, fmt.Errorf("resource service is not running")
	}
	
	return s.manager.GetResourceUtilization(), nil
}

// GetRecentConflicts returns recent conflict events
func (s *Service) GetRecentConflicts(limit int) []ConflictEvent {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	if limit <= 0 || limit > len(s.conflictLog) {
		limit = len(s.conflictLog)
	}
	
	// Return the most recent conflicts
	start := len(s.conflictLog) - limit
	if start < 0 {
		start = 0
	}
	
	conflicts := make([]ConflictEvent, limit)
	copy(conflicts, s.conflictLog[start:])
	
	return conflicts
}

// GetHealthStatus returns the health status of the resource service
func (s *Service) GetHealthStatus() ResourceServiceHealth {
	utilization := ResourceUtilizationStats{}
	var err error
	
	if s.IsRunning() {
		utilization, err = s.GetResourceUtilization()
	}
	
	recentConflicts := s.GetRecentConflicts(10)
	criticalConflicts := 0
	
	for _, conflict := range recentConflicts {
		if conflict.Severity == ConflictSeverityCritical {
			criticalConflicts++
		}
	}
	
	status := "healthy"
	if err != nil {
		status = "unhealthy"
	} else if !s.IsRunning() {
		status = "stopped"
	} else if criticalConflicts > 0 {
		status = "degraded"
	} else if utilization.OverallUtilization > s.config.Resources.OverloadThreshold {
		status = "overloaded"
	}
	
	return ResourceServiceHealth{
		Status:            status,
		IsRunning:         s.IsRunning(),
		NodeID:            s.nodeID,
		Utilization:       utilization,
		RecentConflicts:   len(recentConflicts),
		CriticalConflicts: criticalConflicts,
		LastCheck:         time.Now(),
	}
}

// ResourceServiceHealth represents the health status of the resource service
type ResourceServiceHealth struct {
	Status            string                    `json:"status"`
	IsRunning         bool                      `json:"is_running"`
	NodeID            string                    `json:"node_id"`
	Utilization       ResourceUtilizationStats `json:"utilization"`
	RecentConflicts   int                       `json:"recent_conflicts"`
	CriticalConflicts int                       `json:"critical_conflicts"`
	LastCheck         time.Time                 `json:"last_check"`
}

// processConflictEvents processes conflict events from the manager
func (s *Service) processConflictEvents(ctx context.Context) {
	conflictChan := s.manager.GetConflictEvents()
	
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-conflictChan:
			s.handleConflictEvent(event)
		}
	}
}

// handleConflictEvent handles a conflict event
func (s *Service) handleConflictEvent(event ConflictEvent) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	// Add to conflict log
	s.conflictLog = append(s.conflictLog, event)
	
	// Trim log if it exceeds maximum size
	if len(s.conflictLog) > s.maxLogSize {
		s.conflictLog = s.conflictLog[len(s.conflictLog)-s.maxLogSize:]
	}
	
	// Log the conflict event
	logLevel := "INFO"
	switch event.Severity {
	case ConflictSeverityHigh:
		logLevel = "WARN"
	case ConflictSeverityCritical:
		logLevel = "ERROR"
	}
	
	log.Printf("[%s] Resource conflict (%s): %s", logLevel, event.Type, event.Message)
	
	// Handle specific conflict types
	switch event.Type {
	case ConflictTypeOverload:
		s.handleOverloadConflict(event)
	case ConflictTypeLXDUnhealthy:
		s.handleLXDHealthConflict(event)
	case ConflictTypeInsufficientResources:
		s.handleInsufficientResourcesConflict(event)
	case ConflictTypeAllocationFailure:
		s.handleAllocationFailureConflict(event)
	}
}

// handleOverloadConflict handles node overload conflicts
func (s *Service) handleOverloadConflict(event ConflictEvent) {
	if event.Severity == ConflictSeverityCritical {
		log.Printf("CRITICAL: Node %s is critically overloaded (%.1f%% utilization)", 
			event.NodeID, event.Utilization*100)
		// In a production system, this might trigger alerts or automatic scaling
	}
}

// handleLXDHealthConflict handles LXD health issues
func (s *Service) handleLXDHealthConflict(event ConflictEvent) {
	log.Printf("LXD health issue detected on node %s: %s", event.NodeID, event.Message)
	// In a production system, this might trigger LXD service restart or failover
}

// handleInsufficientResourcesConflict handles insufficient resource conflicts
func (s *Service) handleInsufficientResourcesConflict(event ConflictEvent) {
	log.Printf("Insufficient resources on node %s: %s", event.NodeID, event.Message)
	// In a production system, this might trigger capacity planning alerts
}

// handleAllocationFailureConflict handles allocation failure conflicts
func (s *Service) handleAllocationFailureConflict(event ConflictEvent) {
	log.Printf("Resource allocation failure on node %s: %s", event.NodeID, event.Message)
	// In a production system, this might trigger retry logic or alternative node selection
}

// validateResourceSpecs validates resource specifications against configured limits
func (s *Service) validateResourceSpecs(specs models.ResourceSpecs) error {
	if specs.VCPUs == 0 {
		return models.ErrInvalidResourceSpecs
	}
	
	if specs.VCPUs > uint32(s.config.Resources.MaxVCPUsPerMicroVM) {
		return fmt.Errorf("CPU count exceeds maximum allowed (%d)", s.config.Resources.MaxVCPUsPerMicroVM)
	}
	
	if specs.MemoryMB == 0 {
		return models.ErrInvalidResourceSpecs
	}
	
	if specs.MemoryMB > uint32(s.config.Resources.MaxMemoryMBPerMicroVM) {
		return fmt.Errorf("memory exceeds maximum allowed (%d MB)", s.config.Resources.MaxMemoryMBPerMicroVM)
	}
	
	if specs.StorageMB == 0 {
		return models.ErrInvalidResourceSpecs
	}
	
	if specs.StorageMB > uint32(s.config.Resources.MaxStorageMBPerMicroVM) {
		return fmt.Errorf("storage exceeds maximum allowed (%d MB)", s.config.Resources.MaxStorageMBPerMicroVM)
	}
	
	return nil
}

// GetNodeID returns the node ID for this resource service
func (s *Service) GetNodeID() string {
	return s.nodeID
}

// UpdateConfiguration updates the service configuration
func (s *Service) UpdateConfiguration(cfg *config.Config) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	s.config = cfg
	log.Println("Resource service configuration updated")
	
	return nil
}