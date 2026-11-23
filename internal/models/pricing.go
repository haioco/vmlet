package models

import (
	"math"
	"time"
)

// AvailabilityTier represents different pricing tiers based on resource availability
type AvailabilityTier string

const (
	AvailabilityTierHigh   AvailabilityTier = "high"
	AvailabilityTierMedium AvailabilityTier = "medium"
	AvailabilityTierLow    AvailabilityTier = "low"
)

// BaseRates defines the base pricing rates for resources
type BaseRates struct {
	CPUPerHour      float64 `json:"cpu_per_hour"`
	MemoryMBPerHour float64 `json:"memory_mb_per_hour"`
	StorageMBPerHour float64 `json:"storage_mb_per_hour"`
}

// PricingMultipliers defines dynamic pricing multipliers
type PricingMultipliers struct {
	CPUMultiplier     float64 `json:"cpu_multiplier"`
	MemoryMultiplier  float64 `json:"memory_multiplier"`
	StorageMultiplier float64 `json:"storage_multiplier"`
}

// CurrentPricing represents the current pricing state
type CurrentPricing struct {
	BaseRates          BaseRates          `json:"base_rates"`
	CurrentMultipliers PricingMultipliers `json:"current_multipliers"`
	AvailabilityTier   AvailabilityTier   `json:"availability_tier"`
	NextUpdateAt       time.Time          `json:"next_update_at"`
}

// PricingEngine handles dynamic pricing calculations
type PricingEngine struct {
	baseRates BaseRates
	config    PricingConfig
}

// PricingConfig defines configuration for the pricing engine
type PricingConfig struct {
	ProfitMargin       float64 `json:"profit_margin"`        // e.g., 0.30 for 30% profit margin
	UpdateIntervalMin  int     `json:"update_interval_min"`  // Update interval in minutes
	HighTierDiscount   float64 `json:"high_tier_discount"`   // e.g., 0.40 for 40% discount
	MediumTierDiscount float64 `json:"medium_tier_discount"` // e.g., 0.20 for 20% discount
	LowTierPremium     float64 `json:"low_tier_premium"`     // e.g., 0.25 for 25% premium
}

// NewPricingEngine creates a new pricing engine with default configuration
func NewPricingEngine() *PricingEngine {
	return &PricingEngine{
		baseRates: BaseRates{
			CPUPerHour:       0.05,   // $0.05 per CPU per hour
			MemoryMBPerHour:  0.0001, // $0.0001 per MB per hour
			StorageMBPerHour: 0.00005, // $0.00005 per MB per hour
		},
		config: PricingConfig{
			ProfitMargin:       0.30,
			UpdateIntervalMin:  5,
			HighTierDiscount:   0.40,
			MediumTierDiscount: 0.20,
			LowTierPremium:     0.25,
		},
	}
}

// CalculatePricing calculates current pricing based on resource pools
func (pe *PricingEngine) CalculatePricing(resourcePools []ResourcePool) CurrentPricing {
	// Calculate overall availability across all nodes
	tier := pe.calculateOverallAvailabilityTier(resourcePools)
	
	// Calculate multipliers based on availability tier
	multipliers := pe.calculateMultipliers(tier, resourcePools)
	
	return CurrentPricing{
		BaseRates:          pe.baseRates,
		CurrentMultipliers: multipliers,
		AvailabilityTier:   tier,
		NextUpdateAt:       time.Now().Add(time.Duration(pe.config.UpdateIntervalMin) * time.Minute),
	}
}

// calculateOverallAvailabilityTier determines the overall availability tier
func (pe *PricingEngine) calculateOverallAvailabilityTier(resourcePools []ResourcePool) AvailabilityTier {
	if len(resourcePools) == 0 {
		return AvailabilityTierLow
	}

	totalCapacity := ResourceCapacity{}
	totalAvailability := ResourceAvailability{}

	// Aggregate capacity and availability across all nodes
	for _, pool := range resourcePools {
		totalCapacity.TotalCPU += pool.Capacity.TotalCPU
		totalCapacity.TotalMemoryMB += pool.Capacity.TotalMemoryMB
		totalCapacity.TotalStorageMB += pool.Capacity.TotalStorageMB

		totalAvailability.AvailableCPU += pool.Availability.AvailableCPU
		totalAvailability.AvailableMemoryMB += pool.Availability.AvailableMemoryMB
		totalAvailability.AvailableStorageMB += pool.Availability.AvailableStorageMB
	}

	// Calculate average availability percentage
	cpuAvail := float64(totalAvailability.AvailableCPU) / float64(totalCapacity.TotalCPU)
	memAvail := float64(totalAvailability.AvailableMemoryMB) / float64(totalCapacity.TotalMemoryMB)
	storageAvail := float64(totalAvailability.AvailableStorageMB) / float64(totalCapacity.TotalStorageMB)

	avgAvailability := (cpuAvail + memAvail + storageAvail) / 3.0

	if avgAvailability > 0.60 {
		return AvailabilityTierHigh
	} else if avgAvailability > 0.30 {
		return AvailabilityTierMedium
	}
	return AvailabilityTierLow
}

// calculateMultipliers calculates pricing multipliers based on availability and demand
func (pe *PricingEngine) calculateMultipliers(tier AvailabilityTier, resourcePools []ResourcePool) PricingMultipliers {
	baseMultiplier := 1.0 + pe.config.ProfitMargin

	// Apply tier-based adjustments
	var tierMultiplier float64
	switch tier {
	case AvailabilityTierHigh:
		tierMultiplier = 1.0 - pe.config.HighTierDiscount
	case AvailabilityTierMedium:
		tierMultiplier = 1.0 - pe.config.MediumTierDiscount
	case AvailabilityTierLow:
		tierMultiplier = 1.0 + pe.config.LowTierPremium
	default:
		tierMultiplier = 1.0
	}

	// Calculate demand factor based on current utilization
	demandFactor := pe.calculateDemandFactor(resourcePools)

	finalMultiplier := baseMultiplier * tierMultiplier * demandFactor

	return PricingMultipliers{
		CPUMultiplier:     finalMultiplier,
		MemoryMultiplier:  finalMultiplier,
		StorageMultiplier: finalMultiplier,
	}
}

// calculateDemandFactor calculates a demand-based pricing factor
func (pe *PricingEngine) calculateDemandFactor(resourcePools []ResourcePool) float64 {
	if len(resourcePools) == 0 {
		return 1.0
	}

	totalUtilization := 0.0
	for _, pool := range resourcePools {
		totalUtilization += pool.GetUtilizationPercentage()
	}

	avgUtilization := totalUtilization / float64(len(resourcePools))

	// Demand factor increases with utilization
	// At 0% utilization: factor = 0.8
	// At 50% utilization: factor = 1.0
	// At 80% utilization: factor = 1.4
	demandFactor := 0.8 + (avgUtilization * 0.75)

	// Cap the demand factor to prevent extreme pricing
	return math.Min(demandFactor, 2.0)
}

// CalculateMicroVMPrice calculates the hourly price for a microVM
func (pe *PricingEngine) CalculateMicroVMPrice(specs ResourceSpecs, pricing CurrentPricing) float64 {
	cpuCost := float64(specs.VCPUs) * pricing.BaseRates.CPUPerHour * pricing.CurrentMultipliers.CPUMultiplier
	memoryCost := float64(specs.MemoryMB) * pricing.BaseRates.MemoryMBPerHour * pricing.CurrentMultipliers.MemoryMultiplier
	storageCost := float64(specs.StorageMB) * pricing.BaseRates.StorageMBPerHour * pricing.CurrentMultipliers.StorageMultiplier

	return cpuCost + memoryCost + storageCost
}

// GetDiscountPercentage returns the discount percentage for the current tier
func (pe *PricingEngine) GetDiscountPercentage(tier AvailabilityTier) float64 {
	switch tier {
	case AvailabilityTierHigh:
		return pe.config.HighTierDiscount * 100
	case AvailabilityTierMedium:
		return pe.config.MediumTierDiscount * 100
	case AvailabilityTierLow:
		return -pe.config.LowTierPremium * 100 // Negative discount = premium
	default:
		return 0
	}
}

// ShouldUpdatePricing checks if pricing should be updated based on time
func (pe *PricingEngine) ShouldUpdatePricing(lastUpdate time.Time) bool {
	return time.Since(lastUpdate) >= time.Duration(pe.config.UpdateIntervalMin)*time.Minute
}

// UpdateBaseRates updates the base pricing rates
func (pe *PricingEngine) UpdateBaseRates(rates BaseRates) {
	pe.baseRates = rates
}

// UpdateConfig updates the pricing engine configuration
func (pe *PricingEngine) UpdateConfig(config PricingConfig) {
	pe.config = config
}