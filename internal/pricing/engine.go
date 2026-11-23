package pricing

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

// Engine implements the PricingEngineInterface for the API service
type Engine struct {
	service *Service
	mutex   sync.RWMutex
}

// NewEngine creates a new pricing engine that implements the API interface
func NewEngine(service *Service) *Engine {
	return &Engine{
		service: service,
	}
}

// CalculatePrice calculates the price for given resources on a specific node
func (e *Engine) CalculatePrice(ctx context.Context, specs api.ResourceSpecs, nodeID string) (*models.PricingInfo, error) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	
	// Convert API ResourceSpecs to models ResourceSpecs
	modelSpecs := models.ResourceSpecs{
		VCPUs:     specs.VCPUs,
		MemoryMB:  specs.MemoryMB,
		StorageMB: specs.StorageMB,
	}
	
	return e.service.CalculatePrice(ctx, modelSpecs, nodeID)
}

// GetCurrentPricing returns current pricing information
func (e *Engine) GetCurrentPricing(ctx context.Context, nodeID string) (*models.CurrentPricing, error) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	
	return e.service.GetCurrentPricing(ctx, nodeID)
}

// UpdatePricing forces an immediate pricing update
func (e *Engine) UpdatePricing(ctx context.Context) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	
	return e.service.UpdatePricing(ctx)
}

// ResourceManagerAdapter adapts the resource manager to work with the pricing service
type ResourceManagerAdapter struct {
	resourceManager api.ResourceManagerInterface
}

// NewResourceManagerAdapter creates a new adapter
func NewResourceManagerAdapter(rm api.ResourceManagerInterface) *ResourceManagerAdapter {
	return &ResourceManagerAdapter{
		resourceManager: rm,
	}
}

// GetAllResourcePools gets all resource pools
func (a *ResourceManagerAdapter) GetAllResourcePools(ctx context.Context) ([]*models.ResourcePool, error) {
	// Get resource availability for all nodes (empty nodeID means all nodes)
	return a.resourceManager.GetResourceAvailability(ctx, "")
}

// GetResourcePool gets a specific resource pool by node ID
func (a *ResourceManagerAdapter) GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error) {
	pools, err := a.resourceManager.GetResourceAvailability(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	
	if len(pools) == 0 {
		return nil, fmt.Errorf("no resource pool found for node %s", nodeID)
	}
	
	// Return the first pool (should be the only one for a specific node)
	return pools[0], nil
}

// PricingManager manages the pricing engine lifecycle and provides additional functionality
type PricingManager struct {
	engine          *Engine
	service         *Service
	resourceManager api.ResourceManagerInterface
	mutex           sync.RWMutex
	started         bool
}

// NewPricingManager creates a new pricing manager
func NewPricingManager(resourceManager api.ResourceManagerInterface, baseRates models.BaseRates) *PricingManager {
	// Create adapter for resource manager
	adapter := NewResourceManagerAdapter(resourceManager)
	
	// Convert base rates to config.PricingConfig
	cfg := &config.PricingConfig{
		BaseCPURate:        baseRates.CPUPerHour,
		BaseMemoryRate:     baseRates.MemoryMBPerHour,
		BaseStorageRate:    baseRates.StorageMBPerHour,
		ProfitMargin:       0.30, // Default from models
		UpdateIntervalMin:  5,    // Default from models
		HighTierDiscount:   0.40, // Default from models
		MediumTierDiscount: 0.20, // Default from models
		LowTierPremium:     0.25, // Default from models
		Currency:           "USD",
	}
	
	// Create pricing service
	service := NewService(adapter, cfg)
	
	// Create engine
	engine := NewEngine(service)
	
	return &PricingManager{
		engine:          engine,
		service:         service,
		resourceManager: resourceManager,
	}
}

// Start starts the pricing manager
func (pm *PricingManager) Start(ctx context.Context) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	if pm.started {
		return fmt.Errorf("pricing manager already started")
	}
	
	if err := pm.service.Start(ctx); err != nil {
		return fmt.Errorf("failed to start pricing service: %w", err)
	}
	
	pm.started = true
	log.Println("Pricing manager started successfully")
	return nil
}

// Stop stops the pricing manager
func (pm *PricingManager) Stop() {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	if !pm.started {
		return
	}
	
	pm.service.Stop()
	pm.started = false
	log.Println("Pricing manager stopped")
}

// GetEngine returns the pricing engine for API integration
func (pm *PricingManager) GetEngine() api.PricingEngineInterface {
	return pm.engine
}

// GetService returns the pricing service for direct access
func (pm *PricingManager) GetService() *Service {
	return pm.service
}

// IsStarted returns whether the pricing manager is started
func (pm *PricingManager) IsStarted() bool {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()
	return pm.started
}

// GetPricingHistory returns historical pricing data
func (pm *PricingManager) GetPricingHistory(ctx context.Context, hours int) ([]PricingHistoryEntry, error) {
	return pm.service.GetPricingHistory(ctx, hours)
}

// GetPricingTierInfo returns information about pricing tiers
func (pm *PricingManager) GetPricingTierInfo() PricingTierInfo {
	return pm.service.GetPricingTierInfo()
}

// GetPricingStats returns pricing statistics
func (pm *PricingManager) GetPricingStats(ctx context.Context) (*PricingStats, error) {
	return pm.service.GetPricingStats(ctx)
}

// ShouldUpdatePricing checks if pricing should be updated
func (pm *PricingManager) ShouldUpdatePricing(ctx context.Context) (bool, string) {
	return pm.service.ShouldUpdatePricing(ctx)
}

// ForceUpdate forces an immediate pricing update
func (pm *PricingManager) ForceUpdate(ctx context.Context) error {
	return pm.service.UpdatePricing(ctx)
}

// CalculateMicroVMPrice calculates the total price for a microVM with given specs
func (pm *PricingManager) CalculateMicroVMPrice(ctx context.Context, specs models.ResourceSpecs) (float64, error) {
	pricing, err := pm.service.GetCurrentPricing(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("failed to get current pricing: %w", err)
	}
	
	// Use the pricing engine to calculate the price
	engine := models.NewPricingEngine()
	return engine.CalculateMicroVMPrice(specs, *pricing), nil
}

// GetCurrentDiscountPercentage returns the current discount percentage
func (pm *PricingManager) GetCurrentDiscountPercentage(ctx context.Context) (float64, error) {
	pricing, err := pm.service.GetCurrentPricing(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("failed to get current pricing: %w", err)
	}
	
	engine := models.NewPricingEngine()
	return engine.GetDiscountPercentage(pricing.AvailabilityTier), nil
}

// GetPricingForSpecs returns detailed pricing breakdown for specific resource specs
func (pm *PricingManager) GetPricingForSpecs(ctx context.Context, specs models.ResourceSpecs) (*DetailedPricing, error) {
	pricing, err := pm.service.GetCurrentPricing(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get current pricing: %w", err)
	}
	
	// Calculate individual component costs
	cpuCost := float64(specs.VCPUs) * pricing.BaseRates.CPUPerHour * pricing.CurrentMultipliers.CPUMultiplier
	memoryCost := float64(specs.MemoryMB) * pricing.BaseRates.MemoryMBPerHour * pricing.CurrentMultipliers.MemoryMultiplier
	storageCost := float64(specs.StorageMB) * pricing.BaseRates.StorageMBPerHour * pricing.CurrentMultipliers.StorageMultiplier
	
	totalCost := cpuCost + memoryCost + storageCost
	
	return &DetailedPricing{
		Specs:           specs,
		CPUCost:         cpuCost,
		MemoryCost:      memoryCost,
		StorageCost:     storageCost,
		TotalHourlyCost: totalCost,
		Currency:        "USD",
		AvailabilityTier: pricing.AvailabilityTier,
		Multipliers:     pricing.CurrentMultipliers,
		BaseRates:       pricing.BaseRates,
		CalculatedAt:    time.Now(),
	}, nil
}

// DetailedPricing provides a detailed breakdown of pricing
type DetailedPricing struct {
	Specs            models.ResourceSpecs       `json:"specs"`
	CPUCost          float64                    `json:"cpu_cost"`
	MemoryCost       float64                    `json:"memory_cost"`
	StorageCost      float64                    `json:"storage_cost"`
	TotalHourlyCost  float64                    `json:"total_hourly_cost"`
	Currency         string                     `json:"currency"`
	AvailabilityTier models.AvailabilityTier    `json:"availability_tier"`
	Multipliers      models.PricingMultipliers  `json:"multipliers"`
	BaseRates        models.BaseRates           `json:"base_rates"`
	CalculatedAt     time.Time                  `json:"calculated_at"`
}

// EstimateMonthlyCost estimates the monthly cost for a microVM
func (dp *DetailedPricing) EstimateMonthlyCost() float64 {
	hoursPerMonth := 24.0 * 30.0 // Approximate hours in a month
	return dp.TotalHourlyCost * hoursPerMonth
}

// EstimateDailyCost estimates the daily cost for a microVM
func (dp *DetailedPricing) EstimateDailyCost() float64 {
	return dp.TotalHourlyCost * 24.0
}

// GetSavingsFromTier returns the savings percentage compared to base pricing
func (dp *DetailedPricing) GetSavingsFromTier() float64 {
	engine := models.NewPricingEngine()
	return engine.GetDiscountPercentage(dp.AvailabilityTier)
}