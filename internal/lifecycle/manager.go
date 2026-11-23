package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/metrics"
	"github.com/haioco/vmlet/internal/models"
)

// MonitoringInterface defines the interface for monitoring integration
type MonitoringInterface interface {
	LogMicroVMOperation(operation string, microvmID string, fn func() error) error
	HandleHealthCheckFailure(component string, microvmID string, failureCount int, lastError string)
	GetLogger() LoggerInterface
	GetMetrics() MetricsInterface
}

// LoggerInterface defines the interface for logging
type LoggerInterface interface {
	WithMicroVMID(microvmID string) LoggerInterface
	Info(message string)
	Warn(message string)
	Error(message string, err error)
	WithField(key string, value interface{}) LoggerInterface
	WithFields(fields map[string]interface{}) LoggerInterface
}

// MetricsInterface defines the interface for metrics collection
type MetricsInterface interface {
	Counter(name string, labels map[string]string, description string)
	Gauge(name string, value float64, labels map[string]string, description string)
	Histogram(name string, value float64, labels map[string]string, description string)
}

// Manager handles microVM lifecycle management
type Manager struct {
	microvmManager   api.MicroVMManagerInterface
	storage          api.StorageInterface
	healthChecker    *HealthChecker
	metricsCollector *MetricsCollector
	idleDetector     *IdleDetector
	recoveryManager  *RecoveryManager
	monitoring       MonitoringInterface
	
	// Configuration
	config *Config
	
	// State management
	mu              sync.RWMutex
	runningMicroVMs map[string]*models.MicroVMInstance
	stopChan        chan struct{}
	wg              sync.WaitGroup
}

// Config holds configuration for lifecycle management
type Config struct {
	// Health checking configuration
	HealthCheckInterval    time.Duration `json:"health_check_interval"`
	HealthCheckTimeout     time.Duration `json:"health_check_timeout"`
	
	// Metrics collection configuration
	MetricsInterval        time.Duration `json:"metrics_interval"`
	MetricsRetentionPeriod time.Duration `json:"metrics_retention_period"`
	
	// Idle detection configuration
	IdleThreshold          time.Duration `json:"idle_threshold"`
	IdleCheckInterval      time.Duration `json:"idle_check_interval"`
	
	// Recovery configuration
	MaxRecoveryAttempts    int           `json:"max_recovery_attempts"`
	RecoveryBackoffBase    time.Duration `json:"recovery_backoff_base"`
	RecoveryTimeout        time.Duration `json:"recovery_timeout"`
	
	// Maintenance configuration
	MaintenanceWindow      time.Duration `json:"maintenance_window"`
	GracefulShutdownTimeout time.Duration `json:"graceful_shutdown_timeout"`
}

// DefaultConfig returns default configuration for lifecycle management
func DefaultConfig() *Config {
	return &Config{
		HealthCheckInterval:     30 * time.Second,
		HealthCheckTimeout:      10 * time.Second,
		MetricsInterval:         60 * time.Second,
		MetricsRetentionPeriod:  24 * time.Hour,
		IdleThreshold:           24 * time.Hour,
		IdleCheckInterval:       1 * time.Hour,
		MaxRecoveryAttempts:     3,
		RecoveryBackoffBase:     30 * time.Second,
		RecoveryTimeout:         5 * time.Minute,
		MaintenanceWindow:       2 * time.Hour,
		GracefulShutdownTimeout: 30 * time.Second,
	}
}

// NewManager creates a new lifecycle manager
func NewManager(
	microvmManager api.MicroVMManagerInterface,
	storage api.StorageInterface,
	monitoring MonitoringInterface,
	config *Config,
) (*Manager, error) {
	if microvmManager == nil {
		return nil, fmt.Errorf("microvmManager cannot be nil")
	}
	if storage == nil {
		return nil, fmt.Errorf("storage cannot be nil")
	}
	if monitoring == nil {
		return nil, fmt.Errorf("monitoring cannot be nil")
	}
	if config == nil {
		config = DefaultConfig()
	}

	healthChecker := NewHealthChecker(microvmManager, config)
	metricsCollector := NewMetricsCollector(config)
	idleDetector := NewIdleDetector(config)
	recoveryManager := NewRecoveryManager(microvmManager, config)

	return &Manager{
		microvmManager:   microvmManager,
		storage:          storage,
		healthChecker:    healthChecker,
		metricsCollector: metricsCollector,
		idleDetector:     idleDetector,
		recoveryManager:  recoveryManager,
		monitoring:       monitoring,
		config:           config,
		runningMicroVMs:  make(map[string]*models.MicroVMInstance),
		stopChan:         make(chan struct{}),
	}, nil
}

// Start begins lifecycle management operations
func (m *Manager) Start(ctx context.Context) error {
	// Load existing running microVMs from storage
	if err := m.loadRunningMicroVMs(ctx); err != nil {
		return fmt.Errorf("failed to load running microVMs: %w", err)
	}

	// Start health checking
	m.wg.Add(1)
	go m.runHealthChecking(ctx)

	// Start metrics collection
	m.wg.Add(1)
	go m.runMetricsCollection(ctx)

	// Start idle detection
	m.wg.Add(1)
	go m.runIdleDetection(ctx)

	// Start recovery management
	m.wg.Add(1)
	go m.runRecoveryManagement(ctx)

	return nil
}

// Stop stops lifecycle management operations
func (m *Manager) Stop() error {
	close(m.stopChan)
	m.wg.Wait()
	return nil
}

// RegisterMicroVM registers a microVM for lifecycle management
func (m *Manager) RegisterMicroVM(microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.runningMicroVMs[microvm.ID] = microvm
	return nil
}

// UnregisterMicroVM removes a microVM from lifecycle management
func (m *Manager) UnregisterMicroVM(microvmID string) error {
	if microvmID == "" {
		return fmt.Errorf("microvmID cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.runningMicroVMs, microvmID)
	return nil
}

// GetMicroVMMetrics returns current metrics for a microVM
func (m *Manager) GetMicroVMMetrics(microvmID string) (*MicroVMMetrics, error) {
	return m.metricsCollector.GetMetrics(microvmID)
}

// GetMicroVMHealth returns current health status for a microVM
func (m *Manager) GetMicroVMHealth(microvmID string) (*HealthStatus, error) {
	return m.healthChecker.GetHealthStatus(microvmID)
}

// TriggerRecovery manually triggers recovery for a microVM
func (m *Manager) TriggerRecovery(ctx context.Context, microvmID string) error {
	m.mu.RLock()
	microvm, exists := m.runningMicroVMs[microvmID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("microvm %s not found in lifecycle management", microvmID)
	}

	return m.recoveryManager.RecoverMicroVM(ctx, microvm)
}

// GracefulShutdown performs graceful shutdown of a microVM for maintenance
func (m *Manager) GracefulShutdown(ctx context.Context, microvmID string) error {
	m.mu.RLock()
	microvm, exists := m.runningMicroVMs[microvmID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("microvm %s not found in lifecycle management", microvmID)
	}

	return m.monitoring.LogMicroVMOperation("graceful_shutdown", microvmID, func() error {
		// Create timeout context for graceful shutdown
		shutdownCtx, cancel := context.WithTimeout(ctx, m.config.GracefulShutdownTimeout)
		defer cancel()

		// Attempt graceful shutdown
		if err := m.microvmManager.StopMicroVM(shutdownCtx, microvm, false); err != nil {
			// If graceful shutdown fails, force shutdown
			return m.microvmManager.StopMicroVM(ctx, microvm, true)
		}

		return nil
	})
}

// RestartMicroVM performs a graceful restart of a microVM
func (m *Manager) RestartMicroVM(ctx context.Context, microvmID string) error {
	m.mu.RLock()
	microvm, exists := m.runningMicroVMs[microvmID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("microvm %s not found in lifecycle management", microvmID)
	}

	return m.monitoring.LogMicroVMOperation("restart", microvmID, func() error {
		// Graceful shutdown
		if err := m.GracefulShutdown(ctx, microvmID); err != nil {
			return fmt.Errorf("failed to shutdown microvm for restart: %w", err)
		}

		// Wait a moment for cleanup
		time.Sleep(2 * time.Second)

		// Start the microVM again
		if err := m.microvmManager.StartMicroVM(ctx, microvm); err != nil {
			return fmt.Errorf("failed to start microvm after restart: %w", err)
		}

		return nil
	})
}

// loadRunningMicroVMs loads running microVMs from storage
func (m *Manager) loadRunningMicroVMs(ctx context.Context) error {
	filters := api.StorageFilters{
		Status: models.MicroVMStatusRunning,
	}

	microvms, err := m.storage.ListMicroVMs(ctx, filters)
	if err != nil {
		return fmt.Errorf("failed to list running microVMs: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, microvm := range microvms {
		m.runningMicroVMs[microvm.ID] = microvm
	}

	return nil
}

// runHealthChecking runs the health checking loop
func (m *Manager) runHealthChecking(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.performHealthChecks(ctx)
		}
	}
}

// runMetricsCollection runs the metrics collection loop
func (m *Manager) runMetricsCollection(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.collectMetrics(ctx)
		}
	}
}

// runIdleDetection runs the idle detection loop
func (m *Manager) runIdleDetection(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.IdleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.detectIdleMicroVMs(ctx)
		}
	}
}

// runRecoveryManagement runs the recovery management loop
func (m *Manager) runRecoveryManagement(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(30 * time.Second) // Check for recovery needs every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkRecoveryNeeds(ctx)
		}
	}
}

// performHealthChecks performs health checks on all running microVMs
func (m *Manager) performHealthChecks(ctx context.Context) {
	m.mu.RLock()
	microvms := make([]*models.MicroVMInstance, 0, len(m.runningMicroVMs))
	for _, microvm := range m.runningMicroVMs {
		microvms = append(microvms, microvm)
	}
	m.mu.RUnlock()

	for _, microvm := range microvms {
		err := m.monitoring.LogMicroVMOperation("health_check", microvm.ID, func() error {
			return m.healthChecker.CheckHealth(ctx, microvm)
		})

		if err != nil {
			metrics.IncCounter(metrics.HealthCheckFailures, map[string]string{"vm_id": microvm.ID})
			// Get health status to determine failure count
			if status, statusErr := m.healthChecker.GetHealthStatus(microvm.ID); statusErr == nil {
				m.monitoring.HandleHealthCheckFailure("health_checker", microvm.ID, status.FailureCount, err.Error())
			}
		}
	}
}

// collectMetrics collects metrics for all running microVMs
func (m *Manager) collectMetrics(ctx context.Context) {
	m.mu.RLock()
	microvms := make([]*models.MicroVMInstance, 0, len(m.runningMicroVMs))
	for _, microvm := range m.runningMicroVMs {
		microvms = append(microvms, microvm)
	}
	m.mu.RUnlock()

	logger := m.monitoring.GetLogger()
	metrics := m.monitoring.GetMetrics()

	for _, microvm := range microvms {
		if err := m.metricsCollector.CollectMetrics(ctx, microvm); err != nil {
			logger.WithMicroVMID(microvm.ID).Error("Metrics collection failed", err)
		} else {
			// Record successful metrics collection
			metrics.Counter("lifecycle_metrics_collected_total", 
				map[string]string{"microvm_id": microvm.ID}, 
				"Total metrics collections performed")
		}
	}
}

// detectIdleMicroVMs detects idle microVMs and sends notifications
func (m *Manager) detectIdleMicroVMs(ctx context.Context) {
	m.mu.RLock()
	microvms := make([]*models.MicroVMInstance, 0, len(m.runningMicroVMs))
	for _, microvm := range m.runningMicroVMs {
		microvms = append(microvms, microvm)
	}
	m.mu.RUnlock()

	for _, microvm := range microvms {
		if m.idleDetector.IsIdle(microvm) {
			// Send idle notification
			if err := m.sendIdleNotification(ctx, microvm); err != nil {
				fmt.Printf("Failed to send idle notification for microVM %s: %v\n", microvm.ID, err)
			}
		}
	}
}

// checkRecoveryNeeds checks if any microVMs need recovery
func (m *Manager) checkRecoveryNeeds(ctx context.Context) {
	unhealthyMicroVMs := m.healthChecker.GetUnhealthyMicroVMs()
	
	for _, microvmID := range unhealthyMicroVMs {
		m.mu.RLock()
		microvm, exists := m.runningMicroVMs[microvmID]
		m.mu.RUnlock()

		if exists {
			if err := m.recoveryManager.RecoverMicroVM(ctx, microvm); err != nil {
				fmt.Printf("Recovery failed for microVM %s: %v\n", microvmID, err)
			}
		}
	}
}

// sendIdleNotification sends an idle notification for a microVM
func (m *Manager) sendIdleNotification(ctx context.Context, microvm *models.MicroVMInstance) error {
	logger := m.monitoring.GetLogger().WithMicroVMID(microvm.ID)
	metrics := m.monitoring.GetMetrics()

	logger.WithFields(map[string]interface{}{
		"customer_id":    microvm.Metadata.CustomerID,
		"idle_threshold": m.config.IdleThreshold,
	}).Info("MicroVM detected as idle - cost optimization opportunity")

	// Record idle detection metric
	metrics.Counter("lifecycle_idle_detections_total", 
		map[string]string{"customer_id": microvm.Metadata.CustomerID}, 
		"Total idle microVM detections")

	// For now, just log the notification
	// In a real implementation, this would send notifications via email, webhook, etc.
	
	return nil
}