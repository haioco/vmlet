package api

import (
	"context"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// ResourceManagerInterface defines the interface for resource management
type ResourceManagerInterface interface {
	// Find a suitable node for the given resource requirements
	FindSuitableNode(ctx context.Context, specs ResourceSpecs) (*NodeInfo, error)
	
	// Allocate resources on a specific node
	AllocateResources(ctx context.Context, nodeID string, specs ResourceSpecs) error
	
	// Deallocate resources on a specific node
	DeallocateResources(ctx context.Context, nodeID string, specs ResourceSpecs) error
	
	// Get resource availability for all nodes or a specific node
	GetResourceAvailability(ctx context.Context, nodeID string) ([]*models.ResourcePool, error)
	
	// Update resource usage information
	UpdateResourceUsage(ctx context.Context, nodeID string) error
}

// PricingEngineInterface defines the interface for pricing calculations
type PricingEngineInterface interface {
	// Calculate price for given resources on a specific node
	CalculatePrice(ctx context.Context, specs ResourceSpecs, nodeID string) (*models.PricingInfo, error)
	
	// Get current pricing information
	GetCurrentPricing(ctx context.Context, nodeID string) (*models.CurrentPricing, error)
	
	// Update pricing based on current resource availability
	UpdatePricing(ctx context.Context) error
}

// MicroVMManagerInterface defines the interface for microVM lifecycle management
type MicroVMManagerInterface interface {
	// Create a new microVM
	CreateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error
	
	// Start a microVM
	StartMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error
	
	// Stop a microVM
	StopMicroVM(ctx context.Context, microvm *models.MicroVMInstance, force bool) error
	
	// Delete a microVM
	DeleteMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error
	
	// Get microVM status from the underlying system
	GetMicroVMStatus(ctx context.Context, microvmID string) (models.MicroVMStatus, error)
}

// StorageManagerInterface defines the interface for storage management operations
type StorageManagerInterface interface {
	// Provision storage for a microVM
	ProvisionStorage(ctx context.Context, microvmID string, config *models.StorageConfig, sizeMB uint32) (*StoragePaths, error)
	
	// Cleanup storage resources for a microVM
	CleanupStorage(ctx context.Context, microvmID string, config *models.StorageConfig) error
	
	// Get storage information for a microVM
	GetStorageInfo(ctx context.Context, microvmID string) (*StorageInfo, error)
	
	// Resize storage for a microVM (if supported by storage type)
	ResizeStorage(ctx context.Context, microvmID string, config *models.StorageConfig, newSizeMB uint32) error
	
	// Backup storage to S3 (for local and hybrid storage types)
	BackupStorage(ctx context.Context, microvmID string, config *models.StorageConfig, s3Config *models.S3Config) error
	
	// Restore storage from S3 backup
	RestoreStorage(ctx context.Context, microvmID string, config *models.StorageConfig, s3Config *models.S3Config, backupID string) error
	
	// List available storage types and their capabilities
	GetStorageCapabilities() map[models.StorageType]StorageCapabilities
}

// LifecycleManagerInterface defines the interface for microVM lifecycle management
type LifecycleManagerInterface interface {
	// Start lifecycle management operations
	Start(ctx context.Context) error
	
	// Stop lifecycle management operations
	Stop() error
	
	// Register a microVM for lifecycle management
	RegisterMicroVM(microvm *models.MicroVMInstance) error
	
	// Unregister a microVM from lifecycle management
	UnregisterMicroVM(microvmID string) error
	
	// Get current metrics for a microVM
	GetMicroVMMetrics(microvmID string) (*MicroVMMetrics, error)
	
	// Get current health status for a microVM
	GetMicroVMHealth(microvmID string) (*HealthStatus, error)
	
	// Trigger manual recovery for a microVM
	TriggerRecovery(ctx context.Context, microvmID string) error
	
	// Perform graceful shutdown for maintenance
	GracefulShutdown(ctx context.Context, microvmID string) error
	
	// Restart a microVM
	RestartMicroVM(ctx context.Context, microvmID string) error
}

// StorageInterface defines the interface for persistent storage operations
type StorageInterface interface {
	// Store a new microVM
	StoreMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error
	
	// Get a microVM by ID
	GetMicroVM(ctx context.Context, microvmID string) (*models.MicroVMInstance, error)
	
	// Update an existing microVM
	UpdateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error
	
	// Delete a microVM
	DeleteMicroVM(ctx context.Context, microvmID string) error
	
	// List microVMs with optional filters
	ListMicroVMs(ctx context.Context, filters StorageFilters) ([]*models.MicroVMInstance, error)
	
	// Store resource pool information
	StoreResourcePool(ctx context.Context, pool *models.ResourcePool) error
	
	// Get resource pool by node ID
	GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error)
	
	// List all resource pools
	ListResourcePools(ctx context.Context) ([]*models.ResourcePool, error)
}

// Supporting types for the interfaces

// NodeInfo represents information about a node
type NodeInfo struct {
	NodeID           string                    `json:"node_id"`
	ResourcePool     *models.ResourcePool      `json:"resource_pool"`
	AvailabilityTier models.AvailabilityTier   `json:"availability_tier"`
}

// StoragePaths contains paths to storage devices for a microVM
type StoragePaths struct {
	RootfsPath string             `json:"rootfs_path"`
	DataPath   string             `json:"data_path"`
	S3Config   *models.S3Config   `json:"s3_config,omitempty"`
}

// StorageInfo contains information about microVM storage
type StorageInfo struct {
	MicroVMID    string                `json:"microvm_id"`
	StorageType  models.StorageType    `json:"storage_type"`
	TotalSizeMB  uint32               `json:"total_size_mb"`
	UsedSizeMB   uint32               `json:"used_size_mb"`
	Paths        *StoragePaths        `json:"paths"`
	S3Config     *models.S3Config     `json:"s3_config,omitempty"`
	LastBackup   *time.Time           `json:"last_backup,omitempty"`
	BackupCount  int                  `json:"backup_count"`
}

// StorageCapabilities describes the capabilities of a storage type
type StorageCapabilities struct {
	SupportsResize     bool   `json:"supports_resize"`
	SupportsBackup     bool   `json:"supports_backup"`
	SupportsRestore    bool   `json:"supports_restore"`
	SupportsSnapshot   bool   `json:"supports_snapshot"`
	RequiresS3Config   bool   `json:"requires_s3_config"`
	MaxSizeMB          uint32 `json:"max_size_mb"`
	MinSizeMB          uint32 `json:"min_size_mb"`
	Description        string `json:"description"`
}

// MicroVMMetrics represents performance metrics for a microVM (from lifecycle package)
type MicroVMMetrics struct {
	MicroVMID     string         `json:"microvm_id"`
	Timestamp     time.Time      `json:"timestamp"`
	CPUMetrics    CPUMetrics     `json:"cpu_metrics"`
	MemoryMetrics MemoryMetrics  `json:"memory_metrics"`
	IOMetrics     IOMetrics      `json:"io_metrics"`
	NetworkMetrics NetworkMetrics `json:"network_metrics"`
	UptimeSeconds int64          `json:"uptime_seconds"`
}

// CPUMetrics represents CPU usage metrics
type CPUMetrics struct {
	UsagePercent    float64 `json:"usage_percent"`
	UserPercent     float64 `json:"user_percent"`
	SystemPercent   float64 `json:"system_percent"`
	IdlePercent     float64 `json:"idle_percent"`
	LoadAverage1m   float64 `json:"load_average_1m"`
	LoadAverage5m   float64 `json:"load_average_5m"`
	LoadAverage15m  float64 `json:"load_average_15m"`
}

// MemoryMetrics represents memory usage metrics
type MemoryMetrics struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsagePercent   float64 `json:"usage_percent"`
	CachedBytes    uint64  `json:"cached_bytes"`
	BuffersBytes   uint64  `json:"buffers_bytes"`
	SwapTotalBytes uint64  `json:"swap_total_bytes"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes"`
}

// IOMetrics represents I/O usage metrics
type IOMetrics struct {
	ReadBytesPerSec  uint64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec uint64 `json:"write_bytes_per_sec"`
	ReadOpsPerSec    uint64 `json:"read_ops_per_sec"`
	WriteOpsPerSec   uint64 `json:"write_ops_per_sec"`
	IOWaitPercent    float64 `json:"io_wait_percent"`
}

// NetworkMetrics represents network usage metrics
type NetworkMetrics struct {
	RxBytesPerSec   uint64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec   uint64 `json:"tx_bytes_per_sec"`
	RxPacketsPerSec uint64 `json:"rx_packets_per_sec"`
	TxPacketsPerSec uint64 `json:"tx_packets_per_sec"`
	RxErrors        uint64 `json:"rx_errors"`
	TxErrors        uint64 `json:"tx_errors"`
}

// HealthStatus represents the health status of a microVM
type HealthStatus struct {
	MicroVMID        string                 `json:"microvm_id"`
	IsHealthy        bool                   `json:"is_healthy"`
	LastHealthCheck  time.Time              `json:"last_health_check"`
	LastHealthyTime  time.Time              `json:"last_healthy_time"`
	FailureCount     int                    `json:"failure_count"`
	HealthChecks     map[string]CheckResult `json:"health_checks"`
	ErrorMessage     string                 `json:"error_message,omitempty"`
}

// CheckResult represents the result of a specific health check
type CheckResult struct {
	Name        string        `json:"name"`
	Success     bool          `json:"success"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
	Timestamp   time.Time     `json:"timestamp"`
}

// StorageFilters defines filters for listing microVMs
type StorageFilters struct {
	CustomerID string                `json:"customer_id,omitempty"`
	NodeID     string                `json:"node_id,omitempty"`
	Status     models.MicroVMStatus  `json:"status,omitempty"`
}

// Request/Response types for the service interface

type ResourceSpecs struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMB  uint32 `json:"memory_mb"`
	StorageMB uint32 `json:"storage_mb"`
}

type S3Config struct {
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type StorageConfig struct {
	Type     string    `json:"type"`
	S3Config *S3Config `json:"s3_config,omitempty"`
}

type NetworkConfig struct {
	Mode     string `json:"mode"`
	CustomIP string `json:"custom_ip,omitempty"`
}

// Request types
type ProvisionMicroVMRequest struct {
	Resources        ResourceSpecs  `json:"resources"`
	Storage          StorageConfig  `json:"storage"`
	Network          NetworkConfig  `json:"network"`
	CustomerID       string         `json:"customer_id"`
	PreferredNodeID  string         `json:"preferred_node_id,omitempty"`
}

type GetMicroVMStatusRequest struct {
	MicrovmID string `json:"microvm_id"`
}

type ListMicroVMsRequest struct {
	CustomerID string                `json:"customer_id,omitempty"`
	NodeID     string                `json:"node_id,omitempty"`
	Status     models.MicroVMStatus  `json:"status,omitempty"`
}

type StartMicroVMRequest struct {
	MicrovmID string `json:"microvm_id"`
}

type StopMicroVMRequest struct {
	MicrovmID string `json:"microvm_id"`
	Force     bool   `json:"force,omitempty"`
}

type DeleteMicroVMRequest struct {
	MicrovmID string `json:"microvm_id"`
	Force     bool   `json:"force,omitempty"`
}

type GetCurrentPricingRequest struct {
	NodeID string `json:"node_id,omitempty"`
}

type GetResourceAvailabilityRequest struct {
	NodeID string `json:"node_id,omitempty"`
}

type GetStorageInfoRequest struct {
	MicrovmID string `json:"microvm_id"`
}

type ResizeStorageRequest struct {
	MicrovmID   string `json:"microvm_id"`
	NewSizeMB   uint32 `json:"new_size_mb"`
}

type BackupStorageRequest struct {
	MicrovmID string           `json:"microvm_id"`
	S3Config  *models.S3Config `json:"s3_config"`
	BackupID  string           `json:"backup_id,omitempty"`
}

type RestoreStorageRequest struct {
	MicrovmID string           `json:"microvm_id"`
	S3Config  *models.S3Config `json:"s3_config"`
	BackupID  string           `json:"backup_id"`
}

type GetStorageCapabilitiesRequest struct {
	StorageType models.StorageType `json:"storage_type,omitempty"`
}

// Response types
type ProvisionMicroVMResponse struct {
	Microvm      *models.MicroVMInstance `json:"microvm,omitempty"`
	ErrorMessage string                  `json:"error_message,omitempty"`
}

type GetMicroVMStatusResponse struct {
	Microvm      *models.MicroVMInstance `json:"microvm,omitempty"`
	ErrorMessage string                  `json:"error_message,omitempty"`
}

type ListMicroVMsResponse struct {
	Microvms     []*models.MicroVMInstance `json:"microvms"`
	ErrorMessage string                    `json:"error_message,omitempty"`
}

type StartMicroVMResponse struct {
	Success      bool   `json:"success"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type StopMicroVMResponse struct {
	Success      bool   `json:"success"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type DeleteMicroVMResponse struct {
	Success      bool   `json:"success"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type GetCurrentPricingResponse struct {
	Pricing      *models.CurrentPricing `json:"pricing,omitempty"`
	ErrorMessage string                 `json:"error_message,omitempty"`
}

type GetResourceAvailabilityResponse struct {
	ResourcePools []*models.ResourcePool `json:"resource_pools"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
}

type GetStorageInfoResponse struct {
	StorageInfo  *StorageInfo `json:"storage_info,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
}

type ResizeStorageResponse struct {
	Success      bool   `json:"success"`
	NewSizeMB    uint32 `json:"new_size_mb,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type BackupStorageResponse struct {
	Success      bool   `json:"success"`
	BackupID     string `json:"backup_id,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type RestoreStorageResponse struct {
	Success      bool   `json:"success"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type GetStorageCapabilitiesResponse struct {
	Capabilities map[models.StorageType]StorageCapabilities `json:"capabilities"`
	ErrorMessage string                                     `json:"error_message,omitempty"`
}