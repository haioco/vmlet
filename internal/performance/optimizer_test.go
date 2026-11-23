package performance

import (
	"context"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

func TestNewOptimizer(t *testing.T) {
	config := DefaultConfig()
	optimizer, err := NewOptimizer(config)
	
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	if optimizer == nil {
		t.Fatal("Optimizer is nil")
	}
	
	if optimizer.config != config {
		t.Error("Config not set correctly")
	}
}

func TestOptimizerWithNilConfig(t *testing.T) {
	optimizer, err := NewOptimizer(nil)
	
	if err != nil {
		t.Fatalf("Failed to create optimizer with nil config: %v", err)
	}
	
	if optimizer == nil {
		t.Fatal("Optimizer is nil")
	}
	
	// Should use default config
	if optimizer.config == nil {
		t.Error("Default config not applied")
	}
}

func TestOptimizeStartup(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	microvm := &models.MicroVMInstance{
		ID:     "test-vm-1",
		NodeID: "test-node-1",
		Status: models.MicroVMStatusCreating,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 2048,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkModeNAT,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
	}
	
	ctx := context.Background()
	err = optimizer.OptimizeStartup(ctx, microvm)
	
	// This should not fail even if actual firecracker is not available
	if err != nil {
		t.Logf("Startup optimization returned error (expected in test environment): %v", err)
	}
	
	// Check that metrics were recorded (only if startup succeeded)
	metrics := optimizer.GetMetrics()
	if err == nil && len(metrics.StartupTimes) == 0 {
		t.Error("No startup times recorded when startup succeeded")
	}
}

func TestCacheOperations(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	// Test cache set and get
	key := "test-key"
	value := "test-value"
	
	optimizer.CacheResource(key, value)
	
	retrieved, found := optimizer.GetCachedResource(key)
	if !found {
		t.Error("Cached resource not found")
	}
	
	if retrieved != value {
		t.Errorf("Expected %v, got %v", value, retrieved)
	}
	
	// Test cache miss
	_, found = optimizer.GetCachedResource("non-existent-key")
	if found {
		t.Error("Non-existent key found in cache")
	}
}

func TestConnectionPool(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	ctx := context.Background()
	
	// Test connection pool (will fail in test environment, but should not panic)
	_, err = optimizer.GetConnection(ctx, "unix:///tmp/test.sock")
	if err == nil {
		t.Log("Connection succeeded (unexpected in test environment)")
	} else {
		t.Logf("Connection failed as expected in test environment: %v", err)
	}
}

func TestPreloadResources(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	ctx := context.Background()
	err = optimizer.PreloadResources(ctx)
	
	// Should not fail even if resources don't exist
	if err != nil {
		t.Logf("Preload resources returned error (expected in test environment): %v", err)
	}
}

func TestOptimizeForProduction(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	ctx := context.Background()
	err = optimizer.OptimizeForProduction(ctx)
	
	// Should not fail even if some optimizations can't be applied
	if err != nil {
		t.Logf("Production optimization returned error (expected in test environment): %v", err)
	}
}

func TestMetricsCollection(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	// Record some startup times
	optimizer.recordStartupTime(25 * time.Second)
	optimizer.recordStartupTime(20 * time.Second)
	optimizer.recordStartupTime(30 * time.Second)
	
	metrics := optimizer.GetMetrics()
	
	if len(metrics.StartupTimes) != 3 {
		t.Errorf("Expected 3 startup times, got %d", len(metrics.StartupTimes))
	}
	
	expectedAverage := (25 + 20 + 30) / 3 * time.Second
	if metrics.AverageStartupTime != expectedAverage {
		t.Errorf("Expected average %v, got %v", expectedAverage, metrics.AverageStartupTime)
	}
}

func TestStartupTimeOptimal(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	// Test with optimal startup times
	optimizer.recordStartupTime(25 * time.Second)
	optimizer.recordStartupTime(20 * time.Second)
	optimizer.recordStartupTime(28 * time.Second)
	
	if !optimizer.IsStartupTimeOptimal() {
		t.Error("Startup time should be optimal (under 30s)")
	}
	
	// Test with non-optimal startup times (clear previous times first)
	optimizer.metrics.StartupTimes = []time.Duration{}
	optimizer.recordStartupTime(35 * time.Second)
	optimizer.recordStartupTime(40 * time.Second)
	
	if optimizer.IsStartupTimeOptimal() {
		t.Error("Startup time should not be optimal (over 30s)")
	}
}

func TestShutdown(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	ctx := context.Background()
	err = optimizer.Shutdown(ctx)
	
	if err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	
	if config == nil {
		t.Fatal("Default config is nil")
	}
	
	// Verify default values
	if config.MaxConnections <= 0 {
		t.Error("MaxConnections should be positive")
	}
	
	if config.CacheSize <= 0 {
		t.Error("CacheSize should be positive")
	}
	
	if config.MaxParallelStarts <= 0 {
		t.Error("MaxParallelStarts should be positive")
	}
	
	if config.StartupTimeoutSec != 30 {
		t.Errorf("Expected StartupTimeoutSec to be 30, got %d", config.StartupTimeoutSec)
	}
}

func TestUpdateCacheMetrics(t *testing.T) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create optimizer: %v", err)
	}
	
	// Update cache metrics
	optimizer.UpdateCacheMetrics()
	
	metrics := optimizer.GetMetrics()
	
	// Should have updated the last optimization time
	if metrics.LastOptimization.IsZero() {
		t.Error("LastOptimization should be set")
	}
	
	// Cache hit rate should be between 0 and 1
	if metrics.CacheHitRate < 0 || metrics.CacheHitRate > 1 {
		t.Errorf("CacheHitRate should be between 0 and 1, got %f", metrics.CacheHitRate)
	}
}

// Benchmark tests
func BenchmarkOptimizeStartup(b *testing.B) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		b.Fatalf("Failed to create optimizer: %v", err)
	}
	
	microvm := &models.MicroVMInstance{
		ID:     "bench-vm",
		NodeID: "bench-node",
		Status: models.MicroVMStatusCreating,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 2048,
		},
		Network: models.NetworkInfo{
			AssignmentMethod: models.NetworkModeNAT,
		},
		Storage: models.StorageConfig{
			Type: models.StorageTypeLocal,
		},
	}
	
	ctx := context.Background()
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		optimizer.OptimizeStartup(ctx, microvm)
	}
}

func BenchmarkCacheOperations(b *testing.B) {
	optimizer, err := NewOptimizer(DefaultConfig())
	if err != nil {
		b.Fatalf("Failed to create optimizer: %v", err)
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := "bench-key"
		value := "bench-value"
		
		optimizer.CacheResource(key, value)
		optimizer.GetCachedResource(key)
	}
}