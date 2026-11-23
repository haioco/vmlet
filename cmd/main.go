package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/firecracker"
	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/performance"
	"github.com/haioco/vmlet/internal/pricing"
	"github.com/haioco/vmlet/internal/production"
)

func main() {
	// Parse command line flags
	var configPath = flag.String("config", "", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Create context for service operations
	ctx := context.Background()

	// Initialize performance optimizer
	perfConfig := &performance.Config{
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
	
	optimizer, err := performance.NewOptimizer(perfConfig)
	if err != nil {
		log.Printf("Warning: Failed to initialize performance optimizer: %v", err)
		optimizer = nil
	} else {
		fmt.Printf("⚡ Performance optimizer initialized\n")
		
		// Apply production optimizations if not in development mode
		if !cfg.Development.SimulateLXD {
			if err := optimizer.OptimizeForProduction(ctx); err != nil {
				log.Printf("Warning: Failed to apply production optimizations: %v", err)
			} else {
				fmt.Printf("🚀 Production optimizations applied\n")
			}
		}
	}

	// Initialize backup manager for production
	var backupManager *production.BackupManager
	if !cfg.Development.SimulateLXD {
		backupConfig := production.DefaultBackupConfig()
		backupManager, err = production.NewBackupManager(backupConfig)
		if err != nil {
			log.Printf("Warning: Failed to initialize backup manager: %v", err)
		} else {
			fmt.Printf("💾 Backup manager initialized\n")
		}
	}

	// Initialize service components
	resourceManager := &MockResourceManager{}
	
	// Initialize pricing engine with real implementation
	baseRates := models.BaseRates{
		CPUPerHour:       cfg.Pricing.BaseCPURate,
		MemoryMBPerHour:  cfg.Pricing.BaseMemoryRate,
		StorageMBPerHour: cfg.Pricing.BaseStorageRate,
	}
	pricingManager := pricing.NewPricingManager(resourceManager, baseRates)
	
	// Start pricing manager
	if err := pricingManager.Start(ctx); err != nil {
		log.Printf("Warning: Failed to start pricing manager: %v", err)
	} else {
		fmt.Printf("💰 Pricing engine started with %d minute update interval\n", cfg.Pricing.UpdateIntervalMin)
	}
	
	pricingEngine := pricingManager.GetEngine()
	
	// Initialize firecracker manager
	var microvmManager api.MicroVMManagerInterface
	if cfg.Development.SimulateLXD {
		// Use local development configuration
		fcConfig := firecracker.LocalDevConfig()
		fcManager, err := firecracker.NewMicroVMManagerAdapter(fcConfig)
		if err != nil {
			log.Printf("Warning: Failed to initialize firecracker manager, using mock: %v", err)
			microvmManager = &MockMicroVMManager{}
		} else {
			microvmManager = fcManager
			fmt.Printf("🔥 Firecracker manager initialized for local development\n")
		}
	} else {
		// Use production configuration
		fcConfig := &firecracker.Config{
			WorkDir:        "/var/lib/firecracker",
			FirecrackerBin: cfg.Firecracker.BinaryPath,
			JailerBin:      cfg.Firecracker.JailerPath,
			KernelPath:     cfg.Firecracker.KernelImagePath,
			UseJailer:      cfg.Firecracker.EnableJailer,
			JailerUID:      1000,
			JailerGID:      1000,
		}
		fcManager, err := firecracker.NewMicroVMManagerAdapter(fcConfig)
		if err != nil {
			log.Printf("Warning: Failed to initialize firecracker manager, using mock: %v", err)
			microvmManager = &MockMicroVMManager{}
		} else {
			microvmManager = fcManager
			fmt.Printf("🔥 Firecracker manager initialized for production\n")
		}
	}
	
	storage := &MockStorage{}

	// Create service
	service := api.NewMicroVMService(resourceManager, pricingEngine, microvmManager, storage)

	// Print startup information
	fmt.Printf("🚀 MicroVM Service starting...\n")
	fmt.Printf("📋 Configuration loaded from: %s\n", getConfigSource(*configPath))
	fmt.Printf("🔧 Firecracker binary: %s\n", cfg.Firecracker.BinaryPath)
	fmt.Printf("🌐 Default network mode: %s\n", cfg.Networking.DefaultMode)
	fmt.Printf("💾 Default storage type: %s\n", cfg.Storage.DefaultType)
	fmt.Printf("🏠 Server will listen on: %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("📡 gRPC server will listen on: %s:%d\n", cfg.Server.Host, cfg.Server.GRPCPort)

	if cfg.Development.SimulateLXD {
		fmt.Printf("🧪 Running in development mode (LXD simulation enabled)\n")
	}

	// Test the service with a sample operation
	fmt.Printf("\n🧪 Testing service functionality...\n")
	testService(service)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Ensure components are stopped on shutdown
	defer func() {
		if pricingManager.IsStarted() {
			pricingManager.Stop()
			fmt.Printf("💰 Pricing engine stopped\n")
		}
		
		if optimizer != nil {
			if err := optimizer.Shutdown(ctx); err != nil {
				log.Printf("Warning: Error shutting down optimizer: %v", err)
			} else {
				fmt.Printf("⚡ Performance optimizer stopped\n")
			}
		}
		
		if backupManager != nil && cfg.Development.SimulateLXD == false {
			// Create shutdown backup
			if _, err := backupManager.CreateBackup(ctx, "shutdown"); err != nil {
				log.Printf("Warning: Failed to create shutdown backup: %v", err)
			} else {
				fmt.Printf("💾 Shutdown backup created\n")
			}
		}
	}()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Printf("\n🛑 Shutdown signal received, stopping service...\n")
		cancel()
	}()

	// In a real implementation, we would start HTTP and gRPC servers here
	fmt.Printf("\n✅ Service initialized successfully!\n")
	fmt.Printf("📝 Next steps:\n")
	fmt.Printf("   1. Implement resource monitoring (Task 3)\n")
	fmt.Printf("   2. Implement firecracker integration (Task 4)\n")
	fmt.Printf("   3. Start HTTP/gRPC servers\n")
	fmt.Printf("\n⏳ Press Ctrl+C to stop the service\n")

	// Wait for shutdown signal
	<-ctx.Done()
	fmt.Printf("👋 Service stopped gracefully\n")
}

func getConfigSource(configPath string) string {
	if configPath == "" {
		return "default configuration"
	}
	return configPath
}

func testService(service *api.MicroVMService) {
	ctx := context.Background()

	// Test provision request
	req := &api.ProvisionMicroVMRequest{
		Resources: api.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 2048,
		},
		Storage: api.StorageConfig{
			Type: "local",
		},
		Network: api.NetworkConfig{
			Mode: "nat",
		},
		CustomerID: "test-customer-123",
	}

	fmt.Printf("  📋 Testing microVM provisioning...\n")
	resp, err := service.ProvisionMicroVM(ctx, req)
	if err != nil {
		fmt.Printf("  ❌ Provision test failed: %v\n", err)
		return
	}

	if resp.ErrorMessage != "" {
		fmt.Printf("  ❌ Provision test failed: %s\n", resp.ErrorMessage)
		return
	}

	fmt.Printf("  ✅ MicroVM provisioned successfully!\n")
	fmt.Printf("     ID: %s\n", resp.Microvm.ID)
	fmt.Printf("     Status: %s\n", resp.Microvm.Status)
	fmt.Printf("     Resources: %d vCPUs, %d MB RAM, %d MB storage\n",
		resp.Microvm.Resources.VCPUs,
		resp.Microvm.Resources.MemoryMB,
		resp.Microvm.Resources.StorageMB)

	// Test pricing
	fmt.Printf("  💰 Testing pricing calculation...\n")
	pricingResp, err := service.GetCurrentPricing(ctx, &api.GetCurrentPricingRequest{})
	if err != nil {
		fmt.Printf("  ❌ Pricing test failed: %v\n", err)
		return
	}

	if pricingResp.ErrorMessage != "" {
		fmt.Printf("  ❌ Pricing test failed: %s\n", pricingResp.ErrorMessage)
		return
	}

	fmt.Printf("  ✅ Pricing calculated successfully!\n")
	fmt.Printf("     Availability tier: %s\n", pricingResp.Pricing.AvailabilityTier)
	fmt.Printf("     CPU rate: $%.4f/hour\n", pricingResp.Pricing.BaseRates.CPUPerHour)
}

// Mock implementations for testing

type MockResourceManager struct{}

func (m *MockResourceManager) FindSuitableNode(ctx context.Context, specs api.ResourceSpecs) (*api.NodeInfo, error) {
	return &api.NodeInfo{
		NodeID: "mock-node-1",
		ResourcePool: &models.ResourcePool{
			NodeID: "mock-node-1",
			Capacity: models.ResourceCapacity{
				TotalCPU:      8,
				TotalMemoryMB: 16384,
				TotalStorageMB: 102400,
			},
			Availability: models.ResourceAvailability{
				AvailableCPU:      4,
				AvailableMemoryMB: 8192,
				AvailableStorageMB: 51200,
			},
		},
		AvailabilityTier: models.AvailabilityTierHigh,
	}, nil
}

func (m *MockResourceManager) AllocateResources(ctx context.Context, nodeID string, specs api.ResourceSpecs) error {
	return nil
}

func (m *MockResourceManager) DeallocateResources(ctx context.Context, nodeID string, specs api.ResourceSpecs) error {
	return nil
}

func (m *MockResourceManager) GetResourceAvailability(ctx context.Context, nodeID string) ([]*models.ResourcePool, error) {
	return []*models.ResourcePool{
		{
			NodeID: "mock-node-1",
			Capacity: models.ResourceCapacity{
				TotalCPU:      8,
				TotalMemoryMB: 16384,
				TotalStorageMB: 102400,
			},
			Availability: models.ResourceAvailability{
				AvailableCPU:      4,
				AvailableMemoryMB: 8192,
				AvailableStorageMB: 51200,
			},
		},
	}, nil
}

func (m *MockResourceManager) UpdateResourceUsage(ctx context.Context, nodeID string) error {
	return nil
}

type MockPricingEngine struct{}

func (m *MockPricingEngine) CalculatePrice(ctx context.Context, specs api.ResourceSpecs, nodeID string) (*models.PricingInfo, error) {
	return &models.PricingInfo{
		HourlyRate: 0.25,
		Currency:   "USD",
	}, nil
}

func (m *MockPricingEngine) GetCurrentPricing(ctx context.Context, nodeID string) (*models.CurrentPricing, error) {
	return &models.CurrentPricing{
		BaseRates: models.BaseRates{
			CPUPerHour:      0.05,
			MemoryMBPerHour: 0.0001,
			StorageMBPerHour: 0.00005,
		},
		CurrentMultipliers: models.PricingMultipliers{
			CPUMultiplier:     1.3,
			MemoryMultiplier:  1.3,
			StorageMultiplier: 1.3,
		},
		AvailabilityTier: models.AvailabilityTierHigh,
	}, nil
}

func (m *MockPricingEngine) UpdatePricing(ctx context.Context) error {
	return nil
}

type MockMicroVMManager struct{}

func (m *MockMicroVMManager) CreateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return nil
}

func (m *MockMicroVMManager) StartMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return nil
}

func (m *MockMicroVMManager) StopMicroVM(ctx context.Context, microvm *models.MicroVMInstance, force bool) error {
	return nil
}

func (m *MockMicroVMManager) DeleteMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return nil
}

func (m *MockMicroVMManager) GetMicroVMStatus(ctx context.Context, microvmID string) (models.MicroVMStatus, error) {
	return models.MicroVMStatusRunning, nil
}

type MockStorage struct{}

func (m *MockStorage) StoreMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return nil
}

func (m *MockStorage) GetMicroVM(ctx context.Context, microvmID string) (*models.MicroVMInstance, error) {
	return &models.MicroVMInstance{
		ID:     microvmID,
		NodeID: "mock-node-1",
		Status: models.MicroVMStatusRunning,
	}, nil
}

func (m *MockStorage) UpdateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return nil
}

func (m *MockStorage) DeleteMicroVM(ctx context.Context, microvmID string) error {
	return nil
}

func (m *MockStorage) ListMicroVMs(ctx context.Context, filters api.StorageFilters) ([]*models.MicroVMInstance, error) {
	return []*models.MicroVMInstance{}, nil
}

func (m *MockStorage) StoreResourcePool(ctx context.Context, pool *models.ResourcePool) error {
	return nil
}

func (m *MockStorage) GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error) {
	return &models.ResourcePool{
		NodeID: nodeID,
	}, nil
}

func (m *MockStorage) ListResourcePools(ctx context.Context) ([]*models.ResourcePool, error) {
	return []*models.ResourcePool{}, nil
}