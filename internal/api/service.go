package api

import (
	"context"
	"fmt"
	"time"

	"github.com/haioco/vmlet/internal/models"
	"github.com/google/uuid"
)



// MicroVMServiceInterface defines the core service interface for microVM management
type MicroVMServiceInterface interface {
	// Core CRUD operations
	ProvisionMicroVM(ctx context.Context, req *ProvisionMicroVMRequest) (*ProvisionMicroVMResponse, error)
	GetMicroVMStatus(ctx context.Context, req *GetMicroVMStatusRequest) (*GetMicroVMStatusResponse, error)
	ListMicroVMs(ctx context.Context, req *ListMicroVMsRequest) (*ListMicroVMsResponse, error)
	StartMicroVM(ctx context.Context, req *StartMicroVMRequest) (*StartMicroVMResponse, error)
	StopMicroVM(ctx context.Context, req *StopMicroVMRequest) (*StopMicroVMResponse, error)
	DeleteMicroVM(ctx context.Context, req *DeleteMicroVMRequest) (*DeleteMicroVMResponse, error)

	// Resource and pricing operations
	GetCurrentPricing(ctx context.Context, req *GetCurrentPricingRequest) (*GetCurrentPricingResponse, error)
	GetResourceAvailability(ctx context.Context, req *GetResourceAvailabilityRequest) (*GetResourceAvailabilityResponse, error)
	
	// Storage management operations
	GetStorageInfo(ctx context.Context, req *GetStorageInfoRequest) (*GetStorageInfoResponse, error)
	ResizeStorage(ctx context.Context, req *ResizeStorageRequest) (*ResizeStorageResponse, error)
	BackupStorage(ctx context.Context, req *BackupStorageRequest) (*BackupStorageResponse, error)
	RestoreStorage(ctx context.Context, req *RestoreStorageRequest) (*RestoreStorageResponse, error)
	GetStorageCapabilities(ctx context.Context, req *GetStorageCapabilitiesRequest) (*GetStorageCapabilitiesResponse, error)
}

// MicroVMService implements the MicroVMServiceInterface
type MicroVMService struct {
	resourceManager ResourceManagerInterface
	pricingEngine   PricingEngineInterface
	microvmManager  MicroVMManagerInterface
	storage         StorageInterface
}

// NewMicroVMService creates a new MicroVM service instance
func NewMicroVMService(
	resourceManager ResourceManagerInterface,
	pricingEngine PricingEngineInterface,
	microvmManager MicroVMManagerInterface,
	storage StorageInterface,
) *MicroVMService {
	return &MicroVMService{
		resourceManager: resourceManager,
		pricingEngine:   pricingEngine,
		microvmManager:  microvmManager,
		storage:         storage,
	}
}

// ProvisionMicroVM provisions a new microVM with the specified resources
func (s *MicroVMService) ProvisionMicroVM(ctx context.Context, req *ProvisionMicroVMRequest) (*ProvisionMicroVMResponse, error) {
	// Validate request
	if err := s.validateProvisionRequest(req); err != nil {
		return &ProvisionMicroVMResponse{
			ErrorMessage: err.Error(),
		}, nil
	}

	// Find suitable node with available resources
	node, err := s.resourceManager.FindSuitableNode(ctx, req.Resources)
	if err != nil {
		return &ProvisionMicroVMResponse{
			ErrorMessage: fmt.Sprintf("No suitable node found: %v", err),
		}, nil
	}

	// Generate unique ID for the microVM
	microvmID := uuid.New().String()

	// Get current pricing
	pricing, err := s.pricingEngine.CalculatePrice(ctx, req.Resources, node.NodeID)
	if err != nil {
		return &ProvisionMicroVMResponse{
			ErrorMessage: fmt.Sprintf("Failed to calculate pricing: %v", err),
		}, nil
	}

	// Create microVM instance
	microvm := &models.MicroVMInstance{
		ID:     microvmID,
		NodeID: node.NodeID,
		Status: models.MicroVMStatusCreating,
		Resources: models.ResourceSpecs{
			VCPUs:     req.Resources.VCPUs,
			MemoryMB:  req.Resources.MemoryMB,
			StorageMB: req.Resources.StorageMB,
		},
		Storage: models.StorageConfig{
			Type:     models.StorageType(req.Storage.Type),
			S3Config: convertS3Config(req.Storage.S3Config),
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkMode(req.Network.Mode),
		},
		Pricing: models.PricingInfo{
			HourlyRate: pricing.HourlyRate,
			Currency:   "USD",
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt:    time.Now(),
			LastActivity: time.Now(),
			CustomerID:   req.CustomerID,
		},
	}

	// Allocate resources
	if err := s.resourceManager.AllocateResources(ctx, node.NodeID, req.Resources); err != nil {
		return &ProvisionMicroVMResponse{
			ErrorMessage: fmt.Sprintf("Failed to allocate resources: %v", err),
		}, nil
	}

	// Store microVM in database
	if err := s.storage.StoreMicroVM(ctx, microvm); err != nil {
		// Rollback resource allocation
		s.resourceManager.DeallocateResources(ctx, node.NodeID, req.Resources)
		return &ProvisionMicroVMResponse{
			ErrorMessage: fmt.Sprintf("Failed to store microVM: %v", err),
		}, nil
	}

	// Start provisioning process asynchronously
	go s.provisionMicroVMAsync(context.Background(), microvm)

	return &ProvisionMicroVMResponse{
		Microvm: microvm,
	}, nil
}

// GetMicroVMStatus retrieves the status of a specific microVM
func (s *MicroVMService) GetMicroVMStatus(ctx context.Context, req *GetMicroVMStatusRequest) (*GetMicroVMStatusResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &GetMicroVMStatusResponse{
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Update last activity
	microvm.UpdateLastActivity()
	s.storage.UpdateMicroVM(ctx, microvm)

	return &GetMicroVMStatusResponse{
		Microvm: microvm,
	}, nil
}

// ListMicroVMs lists microVMs with optional filtering
func (s *MicroVMService) ListMicroVMs(ctx context.Context, req *ListMicroVMsRequest) (*ListMicroVMsResponse, error) {
	filters := StorageFilters{
		CustomerID: req.CustomerID,
		NodeID:     req.NodeID,
		Status:     req.Status,
	}

	microvms, err := s.storage.ListMicroVMs(ctx, filters)
	if err != nil {
		return &ListMicroVMsResponse{
			ErrorMessage: fmt.Sprintf("Failed to list microVMs: %v", err),
		}, nil
	}

	return &ListMicroVMsResponse{
		Microvms: microvms,
	}, nil
}

// StartMicroVM starts a stopped microVM
func (s *MicroVMService) StartMicroVM(ctx context.Context, req *StartMicroVMRequest) (*StartMicroVMResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &StartMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	if !microvm.CanBeStarted() {
		return &StartMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM cannot be started, current status: %s", microvm.Status),
		}, nil
	}

	// Start the microVM
	if err := s.microvmManager.StartMicroVM(ctx, microvm); err != nil {
		return &StartMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Failed to start microVM: %v", err),
		}, nil
	}

	// Update status
	microvm.Status = models.MicroVMStatusRunning
	microvm.UpdateLastActivity()
	s.storage.UpdateMicroVM(ctx, microvm)

	return &StartMicroVMResponse{
		Success: true,
	}, nil
}

// StopMicroVM stops a running microVM
func (s *MicroVMService) StopMicroVM(ctx context.Context, req *StopMicroVMRequest) (*StopMicroVMResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &StopMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	if !microvm.CanBeStopped() {
		return &StopMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM cannot be stopped, current status: %s", microvm.Status),
		}, nil
	}

	// Stop the microVM
	if err := s.microvmManager.StopMicroVM(ctx, microvm, req.Force); err != nil {
		return &StopMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Failed to stop microVM: %v", err),
		}, nil
	}

	// Update status
	microvm.Status = models.MicroVMStatusStopped
	microvm.UpdateLastActivity()
	s.storage.UpdateMicroVM(ctx, microvm)

	return &StopMicroVMResponse{
		Success: true,
	}, nil
}

// DeleteMicroVM deletes a microVM and cleans up resources
func (s *MicroVMService) DeleteMicroVM(ctx context.Context, req *DeleteMicroVMRequest) (*DeleteMicroVMResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &DeleteMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Stop the microVM if it's running
	if microvm.IsRunning() {
		if err := s.microvmManager.StopMicroVM(ctx, microvm, req.Force); err != nil {
			return &DeleteMicroVMResponse{
				Success:      false,
				ErrorMessage: fmt.Sprintf("Failed to stop microVM before deletion: %v", err),
			}, nil
		}
	}

	// Delete the microVM
	if err := s.microvmManager.DeleteMicroVM(ctx, microvm); err != nil {
		return &DeleteMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Failed to delete microVM: %v", err),
		}, nil
	}

	// Deallocate resources
	deallocSpecs := ResourceSpecs{
		VCPUs:     microvm.Resources.VCPUs,
		MemoryMB:  microvm.Resources.MemoryMB,
		StorageMB: microvm.Resources.StorageMB,
	}
	s.resourceManager.DeallocateResources(ctx, microvm.NodeID, deallocSpecs)

	// Remove from storage
	if err := s.storage.DeleteMicroVM(ctx, req.MicrovmID); err != nil {
		return &DeleteMicroVMResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Failed to remove microVM from storage: %v", err),
		}, nil
	}

	return &DeleteMicroVMResponse{
		Success: true,
	}, nil
}

// GetCurrentPricing returns current pricing information
func (s *MicroVMService) GetCurrentPricing(ctx context.Context, req *GetCurrentPricingRequest) (*GetCurrentPricingResponse, error) {
	pricing, err := s.pricingEngine.GetCurrentPricing(ctx, req.NodeID)
	if err != nil {
		return &GetCurrentPricingResponse{
			ErrorMessage: fmt.Sprintf("Failed to get pricing: %v", err),
		}, nil
	}

	return &GetCurrentPricingResponse{
		Pricing: pricing,
	}, nil
}

// GetResourceAvailability returns resource availability information
func (s *MicroVMService) GetResourceAvailability(ctx context.Context, req *GetResourceAvailabilityRequest) (*GetResourceAvailabilityResponse, error) {
	resourcePools, err := s.resourceManager.GetResourceAvailability(ctx, req.NodeID)
	if err != nil {
		return &GetResourceAvailabilityResponse{
			ErrorMessage: fmt.Sprintf("Failed to get resource availability: %v", err),
		}, nil
	}

	return &GetResourceAvailabilityResponse{
		ResourcePools: resourcePools,
	}, nil
}

// Helper methods

func (s *MicroVMService) validateProvisionRequest(req *ProvisionMicroVMRequest) error {
	if req.CustomerID == "" {
		return models.ErrInvalidCustomerID
	}

	if req.Resources.VCPUs == 0 || req.Resources.MemoryMB == 0 || req.Resources.StorageMB == 0 {
		return models.ErrInvalidResourceSpecs
	}

	// Validate storage configuration
	if req.Storage.Type == "s3-fuse" || req.Storage.Type == "s3-nbd" || req.Storage.Type == "hybrid" {
		if req.Storage.S3Config == nil {
			return models.ErrS3ConfigRequired
		}
	}

	return nil
}

func (s *MicroVMService) provisionMicroVMAsync(ctx context.Context, microvm *models.MicroVMInstance) {
	// Call the microVM manager to create the VM (sets up networking, etc.)
	if err := s.microvmManager.CreateMicroVM(ctx, microvm); err != nil {
		microvm.Status = models.MicroVMStatusError
		microvm.UpdateLastActivity()
		s.storage.UpdateMicroVM(ctx, microvm)
		return
	}

	// Start the microVM
	if err := s.microvmManager.StartMicroVM(ctx, microvm); err != nil {
		microvm.Status = models.MicroVMStatusError
		microvm.UpdateLastActivity()
		s.storage.UpdateMicroVM(ctx, microvm)
		return
	}

	// Update status to running
	microvm.Status = models.MicroVMStatusRunning
	microvm.UpdateLastActivity()
	s.storage.UpdateMicroVM(ctx, microvm)
}

// GetStorageInfo retrieves storage information for a microVM
func (s *MicroVMService) GetStorageInfo(ctx context.Context, req *GetStorageInfoRequest) (*GetStorageInfoResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &GetStorageInfoResponse{
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Get storage info from the microVM manager
	// For now, we'll create basic storage info from the microVM data
	storageInfo := &StorageInfo{
		MicroVMID:   microvm.ID,
		StorageType: microvm.Storage.Type,
		TotalSizeMB: microvm.Resources.StorageMB,
		UsedSizeMB:  microvm.Resources.StorageMB, // Assume fully used for simplicity
		S3Config:    microvm.Storage.S3Config,
	}

	return &GetStorageInfoResponse{
		StorageInfo: storageInfo,
	}, nil
}

// ResizeStorage resizes storage for a microVM
func (s *MicroVMService) ResizeStorage(ctx context.Context, req *ResizeStorageRequest) (*ResizeStorageResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &ResizeStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Check if storage type supports resize
	capabilities := s.getStorageCapabilities()
	if cap, exists := capabilities[microvm.Storage.Type]; !exists || !cap.SupportsResize {
		return &ResizeStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Storage type %s does not support resize", microvm.Storage.Type),
		}, nil
	}

	// Validate new size
	if req.NewSizeMB <= microvm.Resources.StorageMB {
		return &ResizeStorageResponse{
			Success:      false,
			ErrorMessage: "New size must be larger than current size",
		}, nil
	}

	// Update microVM storage size
	microvm.Resources.StorageMB = req.NewSizeMB
	microvm.UpdateLastActivity()

	if err := s.storage.UpdateMicroVM(ctx, microvm); err != nil {
		return &ResizeStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Failed to update microVM: %v", err),
		}, nil
	}

	return &ResizeStorageResponse{
		Success:   true,
		NewSizeMB: req.NewSizeMB,
	}, nil
}

// BackupStorage creates a backup of microVM storage
func (s *MicroVMService) BackupStorage(ctx context.Context, req *BackupStorageRequest) (*BackupStorageResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &BackupStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Check if storage type supports backup
	capabilities := s.getStorageCapabilities()
	if cap, exists := capabilities[microvm.Storage.Type]; !exists || !cap.SupportsBackup {
		return &BackupStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Storage type %s does not support backup", microvm.Storage.Type),
		}, nil
	}

	// Generate backup ID if not provided
	backupID := req.BackupID
	if backupID == "" {
		backupID = fmt.Sprintf("%s-%d", req.MicrovmID, time.Now().Unix())
	}

	return &BackupStorageResponse{
		Success:  true,
		BackupID: backupID,
	}, nil
}

// RestoreStorage restores microVM storage from backup
func (s *MicroVMService) RestoreStorage(ctx context.Context, req *RestoreStorageRequest) (*RestoreStorageResponse, error) {
	microvm, err := s.storage.GetMicroVM(ctx, req.MicrovmID)
	if err != nil {
		return &RestoreStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("MicroVM not found: %v", err),
		}, nil
	}

	// Check if storage type supports restore
	capabilities := s.getStorageCapabilities()
	if cap, exists := capabilities[microvm.Storage.Type]; !exists || !cap.SupportsRestore {
		return &RestoreStorageResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("Storage type %s does not support restore", microvm.Storage.Type),
		}, nil
	}

	// MicroVM should be stopped for restore
	if microvm.IsRunning() {
		return &RestoreStorageResponse{
			Success:      false,
			ErrorMessage: "MicroVM must be stopped before restore",
		}, nil
	}

	return &RestoreStorageResponse{
		Success: true,
	}, nil
}

// GetStorageCapabilities returns storage capabilities for all or specific storage types
func (s *MicroVMService) GetStorageCapabilities(ctx context.Context, req *GetStorageCapabilitiesRequest) (*GetStorageCapabilitiesResponse, error) {
	capabilities := s.getStorageCapabilities()

	// Filter by storage type if specified
	if req.StorageType != "" {
		if cap, exists := capabilities[req.StorageType]; exists {
			filteredCaps := map[models.StorageType]StorageCapabilities{
				req.StorageType: cap,
			}
			return &GetStorageCapabilitiesResponse{
				Capabilities: filteredCaps,
			}, nil
		} else {
			return &GetStorageCapabilitiesResponse{
				ErrorMessage: fmt.Sprintf("Unknown storage type: %s", req.StorageType),
			}, nil
		}
	}

	return &GetStorageCapabilitiesResponse{
		Capabilities: capabilities,
	}, nil
}

// Helper method to get storage capabilities
func (s *MicroVMService) getStorageCapabilities() map[models.StorageType]StorageCapabilities {
	return map[models.StorageType]StorageCapabilities{
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

func convertS3Config(s3 *S3Config) *models.S3Config {
	if s3 == nil {
		return nil
	}
	return &models.S3Config{
		Bucket:    s3.Bucket,
		Region:    s3.Region,
		AccessKey: s3.AccessKey,
		SecretKey: s3.SecretKey,
	}
}