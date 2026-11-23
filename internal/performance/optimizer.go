package performance

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// Optimizer handles performance optimization for microVM operations
type Optimizer struct {
	config           *Config
	connectionPool   *ConnectionPool
	cacheManager     *CacheManager
	preloadManager   *PreloadManager
	startupOptimizer *StartupOptimizer
	mutex            sync.RWMutex
	metrics          *OptimizationMetrics
}

// Config holds configuration for the performance optimizer
type Config struct {
	// Connection pooling
	MaxConnections     int           `json:"max_connections"`
	ConnectionTimeout  time.Duration `json:"connection_timeout"`
	IdleTimeout        time.Duration `json:"idle_timeout"`
	
	// Caching
	CacheSize          int           `json:"cache_size"`
	CacheTTL           time.Duration `json:"cache_ttl"`
	EnableResourceCache bool         `json:"enable_resource_cache"`
	EnableConfigCache   bool         `json:"enable_config_cache"`
	
	// Startup optimization
	PreloadKernels     bool          `json:"preload_kernels"`
	PreloadRootFS      bool          `json:"preload_rootfs"`
	ParallelStartup    bool          `json:"parallel_startup"`
	MaxParallelStarts  int           `json:"max_parallel_starts"`
	StartupTimeoutSec  int           `json:"startup_timeout_sec"`
	
	// Resource optimization
	EnableResourcePreallocation bool `json:"enable_resource_preallocation"`
	PreallocationBuffer        int  `json:"preallocation_buffer"`
}

// OptimizationMetrics tracks performance metrics
type OptimizationMetrics struct {
	StartupTimes        []time.Duration `json:"startup_times"`
	CacheHitRate        float64         `json:"cache_hit_rate"`
	ConnectionPoolUsage float64         `json:"connection_pool_usage"`
	PreloadEffectiveness float64        `json:"preload_effectiveness"`
	AverageStartupTime  time.Duration   `json:"average_startup_time"`
	LastOptimization    time.Time       `json:"last_optimization"`
	mutex               sync.RWMutex
}

// NewOptimizer creates a new performance optimizer
func NewOptimizer(config *Config) (*Optimizer, error) {
	if config == nil {
		config = DefaultConfig()
	}

	connectionPool, err := NewConnectionPool(config.MaxConnections, config.ConnectionTimeout, config.IdleTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	cacheManager, err := NewCacheManager(config.CacheSize, config.CacheTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache manager: %w", err)
	}

	preloadManager, err := NewPreloadManager(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create preload manager: %w", err)
	}

	startupOptimizer, err := NewStartupOptimizer(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create startup optimizer: %w", err)
	}

	return &Optimizer{
		config:           config,
		connectionPool:   connectionPool,
		cacheManager:     cacheManager,
		preloadManager:   preloadManager,
		startupOptimizer: startupOptimizer,
		metrics:          &OptimizationMetrics{},
	}, nil
}

// DefaultConfig returns default optimization configuration
func DefaultConfig() *Config {
	return &Config{
		MaxConnections:              50,
		ConnectionTimeout:           30 * time.Second,
		IdleTimeout:                 5 * time.Minute,
		CacheSize:                   1000,
		CacheTTL:                    10 * time.Minute,
		EnableResourceCache:         true,
		EnableConfigCache:           true,
		PreloadKernels:              true,
		PreloadRootFS:               true,
		ParallelStartup:             true,
		MaxParallelStarts:           5,
		StartupTimeoutSec:           30,
		EnableResourcePreallocation: true,
		PreallocationBuffer:         3,
	}
}

// OptimizeStartup optimizes microVM startup process
func (o *Optimizer) OptimizeStartup(ctx context.Context, microvm *models.MicroVMInstance) error {
	startTime := time.Now()
	
	// Use startup optimizer for fast boot
	if err := o.startupOptimizer.OptimizeStartup(ctx, microvm); err != nil {
		return fmt.Errorf("startup optimization failed: %w", err)
	}
	
	// Record startup time
	startupTime := time.Since(startTime)
	o.recordStartupTime(startupTime)
	
	log.Printf("MicroVM %s startup optimized in %v", microvm.ID, startupTime)
	return nil
}

// GetCachedResource retrieves a cached resource
func (o *Optimizer) GetCachedResource(key string) (interface{}, bool) {
	return o.cacheManager.Get(key)
}

// CacheResource stores a resource in cache
func (o *Optimizer) CacheResource(key string, value interface{}) {
	o.cacheManager.Set(key, value)
}

// GetConnection gets a connection from the pool
func (o *Optimizer) GetConnection(ctx context.Context, target string) (Connection, error) {
	return o.connectionPool.GetConnection(ctx, target)
}

// ReleaseConnection returns a connection to the pool
func (o *Optimizer) ReleaseConnection(conn Connection) {
	o.connectionPool.ReleaseConnection(conn)
}

// PreloadResources preloads commonly used resources
func (o *Optimizer) PreloadResources(ctx context.Context) error {
	return o.preloadManager.PreloadResources(ctx)
}

// GetMetrics returns current optimization metrics
func (o *Optimizer) GetMetrics() *OptimizationMetrics {
	o.metrics.mutex.RLock()
	defer o.metrics.mutex.RUnlock()
	
	// Return a copy
	metrics := *o.metrics
	return &metrics
}

// recordStartupTime records a startup time for metrics
func (o *Optimizer) recordStartupTime(duration time.Duration) {
	o.metrics.mutex.Lock()
	defer o.metrics.mutex.Unlock()
	
	o.metrics.StartupTimes = append(o.metrics.StartupTimes, duration)
	
	// Keep only last 100 startup times
	if len(o.metrics.StartupTimes) > 100 {
		o.metrics.StartupTimes = o.metrics.StartupTimes[1:]
	}
	
	// Calculate average
	total := time.Duration(0)
	for _, t := range o.metrics.StartupTimes {
		total += t
	}
	o.metrics.AverageStartupTime = total / time.Duration(len(o.metrics.StartupTimes))
}

// UpdateCacheMetrics updates cache hit rate metrics
func (o *Optimizer) UpdateCacheMetrics() {
	o.metrics.mutex.Lock()
	defer o.metrics.mutex.Unlock()
	
	o.metrics.CacheHitRate = o.cacheManager.GetHitRate()
	o.metrics.ConnectionPoolUsage = o.connectionPool.GetUsageRate()
	o.metrics.PreloadEffectiveness = o.preloadManager.GetEffectiveness()
	o.metrics.LastOptimization = time.Now()
}

// IsStartupTimeOptimal checks if startup times meet the 30-second requirement
func (o *Optimizer) IsStartupTimeOptimal() bool {
	o.metrics.mutex.RLock()
	defer o.metrics.mutex.RUnlock()
	
	return o.metrics.AverageStartupTime <= 30*time.Second
}

// OptimizeForProduction applies production-specific optimizations
func (o *Optimizer) OptimizeForProduction(ctx context.Context) error {
	log.Println("Applying production optimizations...")
	
	// Preload resources
	if err := o.PreloadResources(ctx); err != nil {
		log.Printf("Warning: Failed to preload resources: %v", err)
	}
	
	// Warm up connection pool
	if err := o.connectionPool.WarmUp(ctx); err != nil {
		log.Printf("Warning: Failed to warm up connection pool: %v", err)
	}
	
	// Initialize cache with common resources
	if err := o.cacheManager.WarmUp(ctx); err != nil {
		log.Printf("Warning: Failed to warm up cache: %v", err)
	}
	
	log.Println("Production optimizations applied successfully")
	return nil
}

// Shutdown gracefully shuts down the optimizer
func (o *Optimizer) Shutdown(ctx context.Context) error {
	log.Println("Shutting down performance optimizer...")
	
	var errs []error
	
	if err := o.connectionPool.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("connection pool shutdown error: %w", err))
	}
	
	if err := o.cacheManager.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("cache manager shutdown error: %w", err))
	}
	
	if err := o.preloadManager.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("preload manager shutdown error: %w", err))
	}
	
	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}
	
	log.Println("Performance optimizer shut down successfully")
	return nil
}