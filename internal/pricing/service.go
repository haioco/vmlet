package pricing

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

// Service implements the pricing engine functionality
type Service struct {
	engine          *models.PricingEngine
	resourceManager ResourceManagerInterface
	config          *config.PricingConfig
	currentPricing  *models.CurrentPricing
	pricingHistory  []PricingHistoryEntry
	mutex           sync.RWMutex
	stopChan        chan struct{}
	updateTicker    *time.Ticker
	lastUpdate      time.Time
}

// ResourceManagerInterface defines the interface for getting resource information
type ResourceManagerInterface interface {
	GetAllResourcePools(ctx context.Context) ([]*models.ResourcePool, error)
	GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error)
}

// PricingHistoryEntry represents a historical pricing record
type PricingHistoryEntry struct {
	Timestamp        time.Time                   `json:"timestamp"`
	Pricing          models.CurrentPricing       `json:"pricing"`
	AvailabilityTier models.AvailabilityTier     `json:"availability_tier"`
	TotalNodes       int                         `json:"total_nodes"`
	AvgUtilization   float64                     `json:"avg_utilization"`
}

// NewService creates a new pricing service
func NewService(resourceManager ResourceManagerInterface, cfg *config.PricingConfig) *Service {
	engine := models.NewPricingEngine()
	
	// Update engine configuration from config
	engine.UpdateConfig(models.PricingConfig{
		ProfitMargin:       cfg.ProfitMargin,
		UpdateIntervalMin:  cfg.UpdateIntervalMin,
		HighTierDiscount:   cfg.HighTierDiscount,
		MediumTierDiscount: cfg.MediumTierDiscount,
		LowTierPremium:     cfg.LowTierPremium,
	})
	
	// Update base rates from config
	engine.UpdateBaseRates(models.BaseRates{
		CPUPerHour:       cfg.BaseCPURate,
		MemoryMBPerHour:  cfg.BaseMemoryRate,
		StorageMBPerHour: cfg.BaseStorageRate,
	})

	return &Service{
		engine:          engine,
		resourceManager: resourceManager,
		config:          cfg,
		pricingHistory:  make([]PricingHistoryEntry, 0, 288), // Store 24 hours of 5-minute intervals
		stopChan:        make(chan struct{}),
		lastUpdate:      time.Now(),
	}
}

// Start begins the pricing update cycle
func (s *Service) Start(ctx context.Context) error {
	log.Printf("Starting pricing service with %d minute update interval", s.config.UpdateIntervalMin)
	
	// Initial pricing calculation
	if err := s.updatePricing(ctx); err != nil {
		return fmt.Errorf("failed initial pricing calculation: %w", err)
	}
	
	// Start the update ticker
	s.updateTicker = time.NewTicker(time.Duration(s.config.UpdateIntervalMin) * time.Minute)
	
	// Start the update loop
	go s.updateLoop(ctx)
	
	return nil
}

// Stop stops the pricing service
func (s *Service) Stop() {
	log.Println("Stopping pricing service")
	if s.updateTicker != nil {
		s.updateTicker.Stop()
	}
	close(s.stopChan)
}

// CalculatePrice calculates the price for given resources on a specific node
func (s *Service) CalculatePrice(ctx context.Context, specs models.ResourceSpecs, nodeID string) (*models.PricingInfo, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	if s.currentPricing == nil {
		return nil, fmt.Errorf("pricing not initialized")
	}
	
	// Calculate the hourly rate using the pricing engine
	hourlyRate := s.engine.CalculateMicroVMPrice(specs, *s.currentPricing)
	
	return &models.PricingInfo{
		HourlyRate: hourlyRate,
		Currency:   s.config.Currency,
	}, nil
}

// GetCurrentPricing returns the current pricing information
func (s *Service) GetCurrentPricing(ctx context.Context, nodeID string) (*models.CurrentPricing, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	if s.currentPricing == nil {
		return nil, fmt.Errorf("pricing not initialized")
	}
	
	// Return a copy to prevent external modifications
	pricing := *s.currentPricing
	return &pricing, nil
}

// UpdatePricing forces an immediate pricing update
func (s *Service) UpdatePricing(ctx context.Context) error {
	return s.updatePricing(ctx)
}

// GetPricingHistory returns historical pricing data
func (s *Service) GetPricingHistory(ctx context.Context, hours int) ([]PricingHistoryEntry, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	if hours <= 0 {
		hours = 24 // Default to 24 hours
	}
	
	// Calculate how many entries to return (5-minute intervals)
	maxEntries := hours * 12
	
	// Return the most recent entries up to maxEntries (newest first)
	var historyToReturn []PricingHistoryEntry
	if len(s.pricingHistory) <= maxEntries {
		// Return a copy of all history
		historyToReturn = make([]PricingHistoryEntry, len(s.pricingHistory))
		copy(historyToReturn, s.pricingHistory)
	} else {
		// Return the most recent maxEntries
		startIndex := len(s.pricingHistory) - maxEntries
		historyToReturn = make([]PricingHistoryEntry, maxEntries)
		copy(historyToReturn, s.pricingHistory[startIndex:])
	}
	
	// Reverse the slice to return newest first
	for i, j := 0, len(historyToReturn)-1; i < j; i, j = i+1, j-1 {
		historyToReturn[i], historyToReturn[j] = historyToReturn[j], historyToReturn[i]
	}
	
	return historyToReturn, nil
}

// GetPricingTierInfo returns information about pricing tiers
func (s *Service) GetPricingTierInfo() PricingTierInfo {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	return PricingTierInfo{
		HighTier: TierInfo{
			Name:               "High Availability",
			DiscountPercentage: s.engine.GetDiscountPercentage(models.AvailabilityTierHigh),
			Description:        "More than 60% resources available",
		},
		MediumTier: TierInfo{
			Name:               "Medium Availability", 
			DiscountPercentage: s.engine.GetDiscountPercentage(models.AvailabilityTierMedium),
			Description:        "30-60% resources available",
		},
		LowTier: TierInfo{
			Name:               "Low Availability",
			DiscountPercentage: s.engine.GetDiscountPercentage(models.AvailabilityTierLow),
			Description:        "Less than 30% resources available",
		},
		CurrentTier: s.getCurrentTier(),
	}
}

// GetPricingStats returns pricing statistics
func (s *Service) GetPricingStats(ctx context.Context) (*PricingStats, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	if s.currentPricing == nil {
		return nil, fmt.Errorf("pricing not initialized")
	}
	
	// Calculate statistics from history
	stats := &PricingStats{
		CurrentTier:    s.currentPricing.AvailabilityTier,
		LastUpdate:     s.lastUpdate,
		NextUpdate:     s.currentPricing.NextUpdateAt,
		UpdateInterval: time.Duration(s.config.UpdateIntervalMin) * time.Minute,
		HistoryEntries: len(s.pricingHistory),
	}
	
	// Calculate average pricing over the last 24 hours
	if len(s.pricingHistory) > 0 {
		var totalCPUMultiplier, totalMemoryMultiplier, totalStorageMultiplier float64
		var tierCounts = make(map[models.AvailabilityTier]int)
		
		for _, entry := range s.pricingHistory {
			totalCPUMultiplier += entry.Pricing.CurrentMultipliers.CPUMultiplier
			totalMemoryMultiplier += entry.Pricing.CurrentMultipliers.MemoryMultiplier
			totalStorageMultiplier += entry.Pricing.CurrentMultipliers.StorageMultiplier
			tierCounts[entry.AvailabilityTier]++
		}
		
		count := float64(len(s.pricingHistory))
		stats.AvgCPUMultiplier = totalCPUMultiplier / count
		stats.AvgMemoryMultiplier = totalMemoryMultiplier / count
		stats.AvgStorageMultiplier = totalStorageMultiplier / count
		stats.TierDistribution = tierCounts
	}
	
	return stats, nil
}

// ShouldUpdatePricing checks if pricing should be updated based on time or significant resource changes
func (s *Service) ShouldUpdatePricing(ctx context.Context) (bool, string) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	// Check time-based update
	if s.engine.ShouldUpdatePricing(s.lastUpdate) {
		return true, "scheduled update interval reached"
	}
	
	// Check for significant resource changes
	resourcePools, err := s.resourceManager.GetAllResourcePools(ctx)
	if err != nil {
		log.Printf("Error checking resource pools for pricing update: %v", err)
		return false, "error checking resources"
	}
	
	// Convert slice of pointers to slice of values for the pricing engine
	poolValues := make([]models.ResourcePool, len(resourcePools))
	for i, pool := range resourcePools {
		poolValues[i] = *pool
	}
	
	// Calculate current tier
	currentTier := s.engine.CalculatePricing(poolValues).AvailabilityTier
	
	// If tier changed, update immediately
	if s.currentPricing != nil && currentTier != s.currentPricing.AvailabilityTier {
		return true, fmt.Sprintf("availability tier changed from %s to %s", s.currentPricing.AvailabilityTier, currentTier)
	}
	
	return false, "no update needed"
}

// updateLoop runs the pricing update loop
func (s *Service) updateLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopChan:
			return
		case <-s.updateTicker.C:
			if err := s.updatePricing(ctx); err != nil {
				log.Printf("Error updating pricing: %v", err)
			}
		}
	}
}

// updatePricing performs the actual pricing update
func (s *Service) updatePricing(ctx context.Context) error {
	// Get current resource pools
	resourcePools, err := s.resourceManager.GetAllResourcePools(ctx)
	if err != nil {
		return fmt.Errorf("failed to get resource pools: %w", err)
	}
	
	// Convert slice of pointers to slice of values for the pricing engine
	poolValues := make([]models.ResourcePool, len(resourcePools))
	for i, pool := range resourcePools {
		poolValues[i] = *pool
	}
	
	// Calculate new pricing
	newPricing := s.engine.CalculatePricing(poolValues)
	
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	// Update current pricing
	s.currentPricing = &newPricing
	s.lastUpdate = time.Now()
	
	// Add to history
	historyEntry := PricingHistoryEntry{
		Timestamp:        s.lastUpdate,
		Pricing:          newPricing,
		AvailabilityTier: newPricing.AvailabilityTier,
		TotalNodes:       len(resourcePools),
		AvgUtilization:   s.calculateAverageUtilization(resourcePools),
	}
	
	s.addToHistory(historyEntry)
	
	log.Printf("Pricing updated - Tier: %s, CPU Multiplier: %.2f, Memory Multiplier: %.2f, Storage Multiplier: %.2f",
		newPricing.AvailabilityTier,
		newPricing.CurrentMultipliers.CPUMultiplier,
		newPricing.CurrentMultipliers.MemoryMultiplier,
		newPricing.CurrentMultipliers.StorageMultiplier)
	
	return nil
}

// addToHistory adds an entry to the pricing history
func (s *Service) addToHistory(entry PricingHistoryEntry) {
	s.pricingHistory = append(s.pricingHistory, entry)
	
	// Keep only the last 288 entries (24 hours of 5-minute intervals)
	if len(s.pricingHistory) > 288 {
		s.pricingHistory = s.pricingHistory[1:]
	}
}

// calculateAverageUtilization calculates the average utilization across all resource pools
func (s *Service) calculateAverageUtilization(resourcePools []*models.ResourcePool) float64 {
	if len(resourcePools) == 0 {
		return 0.0
	}
	
	totalUtilization := 0.0
	for _, pool := range resourcePools {
		totalUtilization += pool.GetUtilizationPercentage()
	}
	
	return totalUtilization / float64(len(resourcePools))
}

// getCurrentTier returns the current availability tier
func (s *Service) getCurrentTier() models.AvailabilityTier {
	if s.currentPricing == nil {
		return models.AvailabilityTierLow
	}
	return s.currentPricing.AvailabilityTier
}

// Supporting types

// PricingTierInfo provides information about pricing tiers
type PricingTierInfo struct {
	HighTier    TierInfo                    `json:"high_tier"`
	MediumTier  TierInfo                    `json:"medium_tier"`
	LowTier     TierInfo                    `json:"low_tier"`
	CurrentTier models.AvailabilityTier     `json:"current_tier"`
}

// TierInfo provides details about a specific pricing tier
type TierInfo struct {
	Name               string  `json:"name"`
	DiscountPercentage float64 `json:"discount_percentage"`
	Description        string  `json:"description"`
}

// PricingStats provides pricing statistics
type PricingStats struct {
	CurrentTier          models.AvailabilityTier                    `json:"current_tier"`
	LastUpdate           time.Time                                  `json:"last_update"`
	NextUpdate           time.Time                                  `json:"next_update"`
	UpdateInterval       time.Duration                              `json:"update_interval"`
	HistoryEntries       int                                        `json:"history_entries"`
	AvgCPUMultiplier     float64                                    `json:"avg_cpu_multiplier"`
	AvgMemoryMultiplier  float64                                    `json:"avg_memory_multiplier"`
	AvgStorageMultiplier float64                                    `json:"avg_storage_multiplier"`
	TierDistribution     map[models.AvailabilityTier]int            `json:"tier_distribution"`
}