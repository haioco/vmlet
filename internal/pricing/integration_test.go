package pricing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
)

// MockAPIResourceManager implements api.ResourceManagerInterface for testing
type MockAPIResourceManager struct {
	resourcePools []*models.ResourcePool
}

func (m *MockAPIResourceManager) FindSuitableNode(ctx context.Context, specs api.ResourceSpecs) (*api.NodeInfo, error) {
	for _, pool := range m.resourcePools {
		modelSpecs := models.ResourceSpecs{
			VCPUs:     specs.VCPUs,
			MemoryMB:  specs.MemoryMB,
			StorageMB: specs.StorageMB,
		}
		if pool.CanAllocate(modelSpecs) {
			return &api.NodeInfo{
				NodeID:           pool.NodeID,
				ResourcePool:     pool,
				AvailabilityTier: pool.GetAvailabilityTier(),
			}, nil
		}
	}
	return nil, models.ErrInsufficientResources
}

func (m *MockAPIResourceManager) AllocateResources(ctx context.Context, nodeID string, specs api.ResourceSpecs) error {
	for _, pool := range m.resourcePools {
		if pool.NodeID == nodeID {
			modelSpecs := models.ResourceSpecs{
				VCPUs:     specs.VCPUs,
				MemoryMB:  specs.MemoryMB,
				StorageMB: specs.StorageMB,
			}
			return pool.AllocateResources(modelSpecs)
		}
	}
	return models.ErrNodeNotFound
}

func (m *MockAPIResourceManager) DeallocateResources(ctx context.Context, nodeID string, specs api.ResourceSpecs) error {
	for _, pool := range m.resourcePools {
		if pool.NodeID == nodeID {
			modelSpecs := models.ResourceSpecs{
				VCPUs:     specs.VCPUs,
				MemoryMB:  specs.MemoryMB,
				StorageMB: specs.StorageMB,
			}
			pool.DeallocateResources(modelSpecs)
			return nil
		}
	}
	return models.ErrNodeNotFound
}

func (m *MockAPIResourceManager) GetResourceAvailability(ctx context.Context, nodeID string) ([]*models.ResourcePool, error) {
	if nodeID == "" {
		// Return all pools
		return m.resourcePools, nil
	}
	
	// Return specific pool
	for _, pool := range m.resourcePools {
		if pool.NodeID == nodeID {
			return []*models.ResourcePool{pool}, nil
		}
	}
	
	return nil, models.ErrNodeNotFound
}

func (m *MockAPIResourceManager) UpdateResourceUsage(ctx context.Context, nodeID string) error {
	// Mock implementation - just update timestamps
	for _, pool := range m.resourcePools {
		if nodeID == "" || pool.NodeID == nodeID {
			pool.LastUpdated = time.Now()
		}
	}
	return nil
}

func TestPricingEngineIntegration(t *testing.T) {
	// Create test resource pools with different availability levels
	resourcePools := []*models.ResourcePool{
		{
			NodeID: "high-availability-node",
			Capacity: models.ResourceCapacity{
				TotalCPU:       16,
				TotalMemoryMB:  32768,
				TotalStorageMB: 204800,
			},
			Usage: models.ResourceUsage{
				LXDCPU:              2,
				LXDMemoryMB:         4096,
				LXDStorageMB:        20480,
				FirecrackerCPU:      1,
				FirecrackerMemoryMB: 1024,
				FirecrackerStorageMB: 5120,
			},
			LastUpdated: time.Now(),
		},
		{
			NodeID: "medium-availability-node",
			Capacity: models.ResourceCapacity{
				TotalCPU:       8,
				TotalMemoryMB:  16384,
				TotalStorageMB: 102400,
			},
			Usage: models.ResourceUsage{
				LXDCPU:              4,
				LXDMemoryMB:         8192,
				LXDStorageMB:        51200,
				FirecrackerCPU:      2,
				FirecrackerMemoryMB: 2048,
				FirecrackerStorageMB: 10240,
			},
			LastUpdated: time.Now(),
		},
		{
			NodeID: "low-availability-node",
			Capacity: models.ResourceCapacity{
				TotalCPU:       4,
				TotalMemoryMB:  8192,
				TotalStorageMB: 51200,
			},
			Usage: models.ResourceUsage{
				LXDCPU:              3,
				LXDMemoryMB:         6144,
				LXDStorageMB:        40960,
				FirecrackerCPU:      1,
				FirecrackerMemoryMB: 1024,
				FirecrackerStorageMB: 5120,
			},
			LastUpdated: time.Now(),
		},
	}
	
	// Calculate availability for all pools
	for _, pool := range resourcePools {
		pool.CalculateAvailability()
	}
	
	mockRM := &MockAPIResourceManager{
		resourcePools: resourcePools,
	}
	
	// Create pricing manager
	baseRates := models.BaseRates{
		CPUPerHour:       0.05,
		MemoryMBPerHour:  0.0001,
		StorageMBPerHour: 0.00005,
	}
	
	pricingManager := NewPricingManager(mockRM, baseRates)
	
	ctx := context.Background()
	err := pricingManager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start pricing manager: %v", err)
	}
	defer pricingManager.Stop()
	
	// Test pricing calculation for different resource specs
	testSpecs := []models.ResourceSpecs{
		{VCPUs: 1, MemoryMB: 1024, StorageMB: 10240},   // Small VM
		{VCPUs: 2, MemoryMB: 4096, StorageMB: 20480},   // Medium VM
		{VCPUs: 4, MemoryMB: 8192, StorageMB: 40960},   // Large VM
	}
	
	for _, specs := range testSpecs {
		t.Run(fmt.Sprintf("Pricing for %dCPU_%dMB_%dMB", specs.VCPUs, specs.MemoryMB, specs.StorageMB), func(t *testing.T) {
			// Test pricing calculation
			price, err := pricingManager.CalculateMicroVMPrice(ctx, specs)
			if err != nil {
				t.Fatalf("Failed to calculate price: %v", err)
			}
			
			if price <= 0 {
				t.Error("Expected positive price")
			}
			
			// Test detailed pricing
			detailedPricing, err := pricingManager.GetPricingForSpecs(ctx, specs)
			if err != nil {
				t.Fatalf("Failed to get detailed pricing: %v", err)
			}
			
			if detailedPricing.TotalHourlyCost != price {
				t.Errorf("Expected detailed pricing total to match calculated price: %f != %f", 
					detailedPricing.TotalHourlyCost, price)
			}
			
			// Verify cost breakdown
			expectedTotal := detailedPricing.CPUCost + detailedPricing.MemoryCost + detailedPricing.StorageCost
			if abs(detailedPricing.TotalHourlyCost-expectedTotal) > 0.0001 {
				t.Errorf("Cost breakdown doesn't add up: %f != %f + %f + %f", 
					detailedPricing.TotalHourlyCost, detailedPricing.CPUCost, 
					detailedPricing.MemoryCost, detailedPricing.StorageCost)
			}
			
			// Test monthly and daily estimates
			dailyCost := detailedPricing.EstimateDailyCost()
			monthlyCost := detailedPricing.EstimateMonthlyCost()
			
			if dailyCost != price*24 {
				t.Errorf("Daily cost calculation incorrect: %f != %f", dailyCost, price*24)
			}
			
			if monthlyCost != price*24*30 {
				t.Errorf("Monthly cost calculation incorrect: %f != %f", monthlyCost, price*24*30)
			}
		})
	}
}

func TestPricingTierBehavior(t *testing.T) {
	ctx := context.Background()
	
	// Test different availability scenarios
	scenarios := []struct {
		name          string
		resourcePools []*models.ResourcePool
		expectedTier  models.AvailabilityTier
	}{
		{
			name: "High Availability",
			resourcePools: []*models.ResourcePool{
				createHighAvailabilityPool("node1"),
				createHighAvailabilityPool("node2"),
			},
			expectedTier: models.AvailabilityTierHigh,
		},
		{
			name: "Medium Availability",
			resourcePools: []*models.ResourcePool{
				createMediumAvailabilityPool("node1"),
				createMediumAvailabilityPool("node2"),
			},
			expectedTier: models.AvailabilityTierMedium,
		},
		{
			name: "Low Availability",
			resourcePools: []*models.ResourcePool{
				createLowAvailabilityPool("node1"),
				createLowAvailabilityPool("node2"),
			},
			expectedTier: models.AvailabilityTierLow,
		},
	}
	
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Calculate availability for pools
			for _, pool := range scenario.resourcePools {
				pool.CalculateAvailability()
			}
			
			mockRM := &MockAPIResourceManager{
				resourcePools: scenario.resourcePools,
			}
			
			baseRates := models.BaseRates{
				CPUPerHour:       0.05,
				MemoryMBPerHour:  0.0001,
				StorageMBPerHour: 0.00005,
			}
			
			pricingManager := NewPricingManager(mockRM, baseRates)
			
			err := pricingManager.Start(ctx)
			if err != nil {
				t.Fatalf("Failed to start pricing manager: %v", err)
			}
			defer pricingManager.Stop()
			
			// Get current pricing
			pricing, err := pricingManager.GetService().GetCurrentPricing(ctx, "")
			if err != nil {
				t.Fatalf("Failed to get current pricing: %v", err)
			}
			
			if pricing.AvailabilityTier != scenario.expectedTier {
				t.Errorf("Expected tier %s, got %s", scenario.expectedTier, pricing.AvailabilityTier)
			}
			
			// Verify pricing multipliers are appropriate for the tier
			switch scenario.expectedTier {
			case models.AvailabilityTierHigh:
				// Should have lower multipliers (discounts)
				if pricing.CurrentMultipliers.CPUMultiplier >= 1.0 {
					t.Error("Expected discounted pricing for high availability tier")
				}
			case models.AvailabilityTierLow:
				// Should have higher multipliers (premium)
				if pricing.CurrentMultipliers.CPUMultiplier <= 1.0 {
					t.Error("Expected premium pricing for low availability tier")
				}
			}
		})
	}
}

func TestPricingHistoryTracking(t *testing.T) {
	resourcePools := []*models.ResourcePool{
		createMediumAvailabilityPool("node1"),
	}
	
	// Calculate availability
	for _, pool := range resourcePools {
		pool.CalculateAvailability()
	}
	
	mockRM := &MockAPIResourceManager{
		resourcePools: resourcePools,
	}
	
	baseRates := models.BaseRates{
		CPUPerHour:       0.05,
		MemoryMBPerHour:  0.0001,
		StorageMBPerHour: 0.00005,
	}
	
	pricingManager := NewPricingManager(mockRM, baseRates)
	
	ctx := context.Background()
	err := pricingManager.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start pricing manager: %v", err)
	}
	defer pricingManager.Stop()
	
	// Force multiple pricing updates to create history
	for i := 0; i < 5; i++ {
		err := pricingManager.ForceUpdate(ctx)
		if err != nil {
			t.Fatalf("Failed to force pricing update: %v", err)
		}
		time.Sleep(10 * time.Millisecond) // Small delay for different timestamps
	}
	
	// Get pricing history
	history, err := pricingManager.GetPricingHistory(ctx, 1)
	if err != nil {
		t.Fatalf("Failed to get pricing history: %v", err)
	}
	
	if len(history) == 0 {
		t.Error("Expected pricing history to contain entries")
	}
	
	// Verify history entries are properly ordered (most recent first)
	for i := 1; i < len(history); i++ {
		if history[i].Timestamp.After(history[i-1].Timestamp) {
			t.Errorf("Expected pricing history to be ordered by timestamp (newest first), but entry %d (%v) is after entry %d (%v)", 
				i, history[i].Timestamp, i-1, history[i-1].Timestamp)
		}
	}
	
	// Get pricing statistics
	stats, err := pricingManager.GetPricingStats(ctx)
	if err != nil {
		t.Fatalf("Failed to get pricing stats: %v", err)
	}
	
	if stats.HistoryEntries != len(history) {
		t.Errorf("Expected stats to show %d history entries, got %d", len(history), stats.HistoryEntries)
	}
	
	if stats.AvgCPUMultiplier <= 0 {
		t.Error("Expected positive average CPU multiplier in stats")
	}
}

// Helper functions to create resource pools with different availability levels

func createHighAvailabilityPool(nodeID string) *models.ResourcePool {
	return &models.ResourcePool{
		NodeID: nodeID,
		Capacity: models.ResourceCapacity{
			TotalCPU:       16,
			TotalMemoryMB:  32768,
			TotalStorageMB: 204800,
		},
		Usage: models.ResourceUsage{
			LXDCPU:              2,
			LXDMemoryMB:         4096,
			LXDStorageMB:        20480,
			FirecrackerCPU:      1,
			FirecrackerMemoryMB: 1024,
			FirecrackerStorageMB: 5120,
		},
		LastUpdated: time.Now(),
	}
}

func createMediumAvailabilityPool(nodeID string) *models.ResourcePool {
	return &models.ResourcePool{
		NodeID: nodeID,
		Capacity: models.ResourceCapacity{
			TotalCPU:       8,
			TotalMemoryMB:  16384,
			TotalStorageMB: 102400,
		},
		Usage: models.ResourceUsage{
			LXDCPU:              2,
			LXDMemoryMB:         4096,
			LXDStorageMB:        25600,
			FirecrackerCPU:      1,
			FirecrackerMemoryMB: 1024,
			FirecrackerStorageMB: 5120,
		},
		LastUpdated: time.Now(),
	}
}

func createLowAvailabilityPool(nodeID string) *models.ResourcePool {
	return &models.ResourcePool{
		NodeID: nodeID,
		Capacity: models.ResourceCapacity{
			TotalCPU:       4,
			TotalMemoryMB:  8192,
			TotalStorageMB: 51200,
		},
		Usage: models.ResourceUsage{
			LXDCPU:              3,
			LXDMemoryMB:         6144,
			LXDStorageMB:        40960,
			FirecrackerCPU:      1,
			FirecrackerMemoryMB: 1024,
			FirecrackerStorageMB: 5120,
		},
		LastUpdated: time.Now(),
	}
}

// Helper function for floating point comparison
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}