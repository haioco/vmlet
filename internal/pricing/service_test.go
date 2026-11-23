package pricing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
)

// MockResourceManager implements ResourceManagerInterface for testing
type MockResourceManager struct {
	resourcePools []*models.ResourcePool
}

func (m *MockResourceManager) GetAllResourcePools(ctx context.Context) ([]*models.ResourcePool, error) {
	return m.resourcePools, nil
}

func (m *MockResourceManager) GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error) {
	for _, pool := range m.resourcePools {
		if pool.NodeID == nodeID {
			return pool, nil
		}
	}
	return nil, fmt.Errorf("node not found: %s", nodeID)
}

func createTestResourcePools() []*models.ResourcePool {
	return []*models.ResourcePool{
		{
			NodeID: "node1",
			Capacity: models.ResourceCapacity{
				TotalCPU:      8,
				TotalMemoryMB: 16384,
				TotalStorageMB: 102400,
			},
			Usage: models.ResourceUsage{
				LXDCPU:              2,
				LXDMemoryMB:         4096,
				LXDStorageMB:        20480,
				FirecrackerCPU:      1,
				FirecrackerMemoryMB: 2048,
				FirecrackerStorageMB: 10240,
			},
			LastUpdated: time.Now(),
		},
		{
			NodeID: "node2",
			Capacity: models.ResourceCapacity{
				TotalCPU:      16,
				TotalMemoryMB: 32768,
				TotalStorageMB: 204800,
			},
			Usage: models.ResourceUsage{
				LXDCPU:              8,
				LXDMemoryMB:         16384,
				LXDStorageMB:        81920,
				FirecrackerCPU:      2,
				FirecrackerMemoryMB: 4096,
				FirecrackerStorageMB: 20480,
			},
			LastUpdated: time.Now(),
		},
	}
}

func TestNewService(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	
	if service == nil {
		t.Fatal("Expected service to be created, got nil")
	}
	
	if service.config != cfg {
		t.Error("Expected config to be set correctly")
	}
	
	if service.engine == nil {
		t.Error("Expected pricing engine to be initialized")
	}
}

func TestServiceStart(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Expected service to start successfully, got error: %v", err)
	}
	
	// Check that pricing was initialized
	if service.currentPricing == nil {
		t.Error("Expected current pricing to be initialized after start")
	}
	
	// Stop the service
	service.Stop()
}

func TestCalculatePrice(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer service.Stop()
	
	specs := models.ResourceSpecs{
		VCPUs:     2,
		MemoryMB:  4096,
		StorageMB: 20480,
	}
	
	pricing, err := service.CalculatePrice(ctx, specs, "node1")
	if err != nil {
		t.Fatalf("Expected price calculation to succeed, got error: %v", err)
	}
	
	if pricing == nil {
		t.Fatal("Expected pricing info to be returned")
	}
	
	if pricing.HourlyRate <= 0 {
		t.Error("Expected hourly rate to be positive")
	}
	
	if pricing.Currency != "USD" {
		t.Errorf("Expected currency to be USD, got %s", pricing.Currency)
	}
}

func TestGetCurrentPricing(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer service.Stop()
	
	pricing, err := service.GetCurrentPricing(ctx, "")
	if err != nil {
		t.Fatalf("Expected to get current pricing, got error: %v", err)
	}
	
	if pricing == nil {
		t.Fatal("Expected pricing to be returned")
	}
	
	// Check that pricing has all required fields
	if pricing.BaseRates.CPUPerHour != cfg.BaseCPURate {
		t.Errorf("Expected CPU rate to be %f, got %f", cfg.BaseCPURate, pricing.BaseRates.CPUPerHour)
	}
	
	if pricing.CurrentMultipliers.CPUMultiplier <= 0 {
		t.Error("Expected CPU multiplier to be positive")
	}
	
	// Check that availability tier is set
	validTiers := map[models.AvailabilityTier]bool{
		models.AvailabilityTierHigh:   true,
		models.AvailabilityTierMedium: true,
		models.AvailabilityTierLow:    true,
	}
	
	if !validTiers[pricing.AvailabilityTier] {
		t.Errorf("Expected valid availability tier, got %s", pricing.AvailabilityTier)
	}
}

func TestPricingHistory(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer service.Stop()
	
	// Force a few pricing updates to create history
	for i := 0; i < 3; i++ {
		err := service.UpdatePricing(ctx)
		if err != nil {
			t.Fatalf("Failed to update pricing: %v", err)
		}
		time.Sleep(10 * time.Millisecond) // Small delay to ensure different timestamps
	}
	
	history, err := service.GetPricingHistory(ctx, 1)
	if err != nil {
		t.Fatalf("Expected to get pricing history, got error: %v", err)
	}
	
	if len(history) == 0 {
		t.Error("Expected pricing history to contain entries")
	}
	
	// Check that history entries have required fields
	for _, entry := range history {
		if entry.Timestamp.IsZero() {
			t.Error("Expected history entry to have timestamp")
		}
		
		if entry.TotalNodes != len(mockRM.resourcePools) {
			t.Errorf("Expected total nodes to be %d, got %d", len(mockRM.resourcePools), entry.TotalNodes)
		}
		
		if entry.AvgUtilization < 0 || entry.AvgUtilization > 1 {
			t.Errorf("Expected average utilization to be between 0 and 1, got %f", entry.AvgUtilization)
		}
	}
}

func TestPricingTierInfo(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer service.Stop()
	
	tierInfo := service.GetPricingTierInfo()
	
	// Check that all tiers are defined
	if tierInfo.HighTier.Name == "" {
		t.Error("Expected high tier to have a name")
	}
	
	if tierInfo.MediumTier.Name == "" {
		t.Error("Expected medium tier to have a name")
	}
	
	if tierInfo.LowTier.Name == "" {
		t.Error("Expected low tier to have a name")
	}
	
	// Check discount percentages
	if tierInfo.HighTier.DiscountPercentage != cfg.HighTierDiscount*100 {
		t.Errorf("Expected high tier discount to be %f, got %f", cfg.HighTierDiscount*100, tierInfo.HighTier.DiscountPercentage)
	}
	
	if tierInfo.MediumTier.DiscountPercentage != cfg.MediumTierDiscount*100 {
		t.Errorf("Expected medium tier discount to be %f, got %f", cfg.MediumTierDiscount*100, tierInfo.MediumTier.DiscountPercentage)
	}
	
	// Low tier should have negative discount (premium)
	if tierInfo.LowTier.DiscountPercentage != -cfg.LowTierPremium*100 {
		t.Errorf("Expected low tier premium to be %f, got %f", -cfg.LowTierPremium*100, tierInfo.LowTier.DiscountPercentage)
	}
}

func TestShouldUpdatePricing(t *testing.T) {
	mockRM := &MockResourceManager{
		resourcePools: createTestResourcePools(),
	}
	
	cfg := &config.PricingConfig{
		BaseCPURate:        0.05,
		BaseMemoryRate:     0.0001,
		BaseStorageRate:    0.00005,
		ProfitMargin:       0.30,
		UpdateIntervalMin:  5,
		HighTierDiscount:   0.40,
		MediumTierDiscount: 0.20,
		LowTierPremium:     0.25,
		Currency:           "USD",
	}
	
	service := NewService(mockRM, cfg)
	ctx := context.Background()
	
	// Calculate availability for test pools
	for _, pool := range mockRM.resourcePools {
		pool.CalculateAvailability()
	}
	
	err := service.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer service.Stop()
	
	// Right after start, should not need update
	shouldUpdate, reason := service.ShouldUpdatePricing(ctx)
	if shouldUpdate {
		t.Errorf("Expected not to need update right after start, but got: %s", reason)
	}
	
	// Simulate time passing by setting last update to past
	service.mutex.Lock()
	service.lastUpdate = time.Now().Add(-10 * time.Minute)
	service.mutex.Unlock()
	
	// Now should need update
	shouldUpdate, reason = service.ShouldUpdatePricing(ctx)
	if !shouldUpdate {
		t.Error("Expected to need update after time interval passed")
	}
	
	if reason != "scheduled update interval reached" {
		t.Errorf("Expected reason to be about time interval, got: %s", reason)
	}
}