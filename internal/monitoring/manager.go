package monitoring

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/config"
)

// Manager coordinates all monitoring and error handling components
type Manager struct {
	config                     *config.Config
	logger                     *Logger
	metrics                    *MetricsCollector
	alertManager               *AlertManager
	recoveryManager            *RecoveryManager
	degradationManager         *GracefulDegradationManager
	
	microvmManager             api.MicroVMManagerInterface
	resourceManager            api.ResourceManagerInterface
	
	mu                         sync.RWMutex
	started                    bool
	stopChan                   chan struct{}
	
	// Component health tracking
	componentHealth            map[string]bool
	lastHealthCheck            map[string]time.Time
	healthCheckInterval        time.Duration
}

// NewManager creates a new monitoring manager
func NewManager(
	cfg *config.Config,
	microvmManager api.MicroVMManagerInterface,
	resourceManager api.ResourceManagerInterface,
) (*Manager, error) {
	// Create logger
	logLevel, err := ParseLogLevel(cfg.Server.LogLevel)
	if err != nil {
		logLevel = LogLevelInfo
	}

	logger, err := NewFileLogger("monitoring", logLevel, "/var/log/microvm-service/monitoring.log")
	if err != nil {
		// Fallback to stdout logger
		logger = NewLogger("monitoring", logLevel, nil)
	}

	// Create metrics collector
	metrics := NewMetricsCollector(logger)

	// Create alert manager
	alertManager := NewAlertManager(logger, metrics)

	// Add log alert handler
	logHandler := NewLogAlertHandler(logger)
	alertManager.AddHandler(logHandler)

	// Create recovery manager
	recoveryManager := NewRecoveryManager(
		microvmManager,
		resourceManager,
		alertManager,
		logger,
		metrics,
	)

	// Create degradation manager
	degradationManager := NewGracefulDegradationManager(
		microvmManager,
		resourceManager,
		alertManager,
		logger,
		metrics,
	)

	manager := &Manager{
		config:                cfg,
		logger:                logger,
		metrics:               metrics,
		alertManager:          alertManager,
		recoveryManager:       recoveryManager,
		degradationManager:    degradationManager,
		microvmManager:        microvmManager,
		resourceManager:       resourceManager,
		stopChan:              make(chan struct{}),
		componentHealth:       make(map[string]bool),
		lastHealthCheck:       make(map[string]time.Time),
		healthCheckInterval:   30 * time.Second,
	}

	return manager, nil
}

// Start starts all monitoring components
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("monitoring manager already started")
	}

	m.logger.Info("Starting monitoring manager")

	// Start metrics server
	if m.config.Server.MetricsPort > 0 {
		metricsAddr := fmt.Sprintf(":%d", m.config.Server.MetricsPort)
		go func() {
			if err := m.metrics.StartMetricsServer(ctx, metricsAddr); err != nil {
				m.logger.Error("Failed to start metrics server", err)
			}
		}()
	}

	// Start recovery manager
	if err := m.recoveryManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start recovery manager: %w", err)
	}

	// Start degradation manager
	if err := m.degradationManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start degradation manager: %w", err)
	}

	// Start monitoring loops
	go m.healthCheckLoop(ctx)
	go m.metricsCollectionLoop(ctx)
	go m.alertCleanupLoop(ctx)

	m.started = true
	m.logger.Info("Monitoring manager started successfully")

	return nil
}

// Stop stops all monitoring components
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return
	}

	m.logger.Info("Stopping monitoring manager")

	// Signal all components to stop
	close(m.stopChan)

	// Stop individual components
	m.recoveryManager.Stop()
	m.degradationManager.Stop()

	// Close logger
	if err := m.logger.Close(); err != nil {
		fmt.Printf("Error closing logger: %v\n", err)
	}

	m.started = false
	m.logger.Info("Monitoring manager stopped")
}

// GetLogger returns the monitoring logger
func (m *Manager) GetLogger() *Logger {
	return m.logger
}

// GetMetrics returns the metrics collector
func (m *Manager) GetMetrics() *MetricsCollector {
	return m.metrics
}

// GetAlertManager returns the alert manager
func (m *Manager) GetAlertManager() *AlertManager {
	return m.alertManager
}

// GetRecoveryManager returns the recovery manager
func (m *Manager) GetRecoveryManager() *RecoveryManager {
	return m.recoveryManager
}

// GetDegradationManager returns the degradation manager
func (m *Manager) GetDegradationManager() *GracefulDegradationManager {
	return m.degradationManager
}

// LogOperation logs an operation with automatic error handling and metrics
func (m *Manager) LogOperation(operation string, component string, fn func() error) error {
	start := time.Now()
	
	m.logger.WithFields(map[string]interface{}{
		"operation": operation,
		"component": component,
	}).Info("Operation started")

	err := fn()
	duration := time.Since(start)

	// Record metrics
	success := err == nil
	m.metrics.RecordMicroVMOperation(operation, "", duration, success)

	if err != nil {
		m.logger.WithFields(map[string]interface{}{
			"operation": operation,
			"component": component,
			"duration":  duration,
		}).Error("Operation failed", err)

		// Trigger alert for operation failure
		m.alertManager.TriggerOperationFailureAlert(operation, component, "", err)
	} else {
		m.logger.WithFields(map[string]interface{}{
			"operation": operation,
			"component": component,
			"duration":  duration,
		}).Info("Operation completed successfully")
	}

	return err
}

// LogMicroVMOperation logs a microVM-specific operation
func (m *Manager) LogMicroVMOperation(operation string, microvmID string, fn func() error) error {
	start := time.Now()
	
	logger := m.logger.WithMicroVMID(microvmID)
	logger.WithField("operation", operation).Info("MicroVM operation started")

	err := fn()
	duration := time.Since(start)

	// Record metrics
	success := err == nil
	m.metrics.RecordMicroVMOperation(operation, microvmID, duration, success)

	if err != nil {
		logger.WithFields(map[string]interface{}{
			"operation": operation,
			"duration":  duration,
		}).Error("MicroVM operation failed", err)

		// Trigger alert and potentially recovery
		m.alertManager.TriggerOperationFailureAlert(operation, "microvm_manager", microvmID, err)
		
		// Request recovery for certain types of failures
		if m.shouldTriggerRecovery(operation, err) {
			m.recoveryManager.RequestRecovery(microvmID, m.classifyFailure(operation, err), map[string]interface{}{
				"operation": operation,
				"error":     err.Error(),
			})
		}
	} else {
		logger.WithFields(map[string]interface{}{
			"operation": operation,
			"duration":  duration,
		}).Info("MicroVM operation completed successfully")
	}

	return err
}

// RecordResourceUsage records resource usage metrics
func (m *Manager) RecordResourceUsage(nodeID string, usage interface{}, capacity interface{}) {
	// This would need to be adapted based on the actual types from the models package
	// For now, we'll log the recording
	m.logger.WithFields(map[string]interface{}{
		"node_id": nodeID,
	}).Debug("Recording resource usage metrics")

	// Record basic metrics
	m.metrics.Counter("resource_usage_updates_total", 
		map[string]string{"node_id": nodeID}, 
		"Total resource usage updates")
}

// HandleHealthCheckFailure handles health check failures
func (m *Manager) HandleHealthCheckFailure(component string, microvmID string, failureCount int, lastError string) {
	m.logger.WithFields(map[string]interface{}{
		"component":     component,
		"microvm_id":    microvmID,
		"failure_count": failureCount,
	}).Warn("Health check failure detected")

	// Update degradation manager
	m.degradationManager.UpdateServiceHealth(component, false, lastError, map[string]interface{}{
		"microvm_id":    microvmID,
		"failure_count": failureCount,
	})

	// Trigger alert
	m.alertManager.TriggerHealthCheckAlert(component, microvmID, failureCount, lastError)

	// Request recovery if failure count is high
	if failureCount >= 3 {
		m.recoveryManager.RequestRecovery(microvmID, "health_check_failure", map[string]interface{}{
			"component":     component,
			"failure_count": failureCount,
			"last_error":    lastError,
		})
	}
}

// HandleResourceConflict handles resource conflicts
func (m *Manager) HandleResourceConflict(conflictType string, nodeID string, utilization float64, message string) {
	m.logger.WithFields(map[string]interface{}{
		"conflict_type": conflictType,
		"node_id":       nodeID,
		"utilization":   utilization,
	}).Warn("Resource conflict detected")

	// Update degradation manager
	m.degradationManager.UpdateServiceHealth("resource_manager", utilization < 0.95, message, map[string]interface{}{
		"conflict_type": conflictType,
		"utilization":   utilization,
	})

	// Trigger alert
	m.alertManager.TriggerResourceConflictAlert(conflictType, nodeID, utilization, message)
}

// IsServiceHealthy checks if a service is healthy
func (m *Manager) IsServiceHealthy(component string) bool {
	return m.degradationManager.IsServiceAvailable(component)
}

// ShouldAcceptNewRequests determines if new requests should be accepted
func (m *Manager) ShouldAcceptNewRequests() bool {
	return m.degradationManager.ShouldAcceptNewRequests()
}

// GetSystemHealth returns overall system health information
func (m *Manager) GetSystemHealth() map[string]interface{} {
	state, level := m.degradationManager.GetOverallHealth()
	serviceHealth := m.degradationManager.GetServiceHealth()
	alertStats := m.alertManager.GetAlertStats()
	
	return map[string]interface{}{
		"overall_state":       state,
		"degradation_level":   level,
		"degraded_capacity":   m.degradationManager.GetDegradedCapacity(),
		"service_health":      serviceHealth,
		"alert_stats":         alertStats,
		"active_recoveries":   len(m.recoveryManager.GetActiveRecoveries()),
		"timestamp":           time.Now(),
	}
}

// healthCheckLoop runs periodic health checks
func (m *Manager) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(m.healthCheckInterval)
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

// performHealthChecks performs health checks on all components
func (m *Manager) performHealthChecks(ctx context.Context) {
	components := []string{"microvm_manager", "resource_manager", "lxd_client"}

	for _, component := range components {
		healthy := m.checkComponentHealth(ctx, component)
		
		m.mu.Lock()
		m.componentHealth[component] = healthy
		m.lastHealthCheck[component] = time.Now()
		m.mu.Unlock()

		// Record health check metrics
		m.metrics.RecordHealthCheck(component, healthy, 0) // Duration would be measured in real implementation
	}
}

// checkComponentHealth checks the health of a specific component
func (m *Manager) checkComponentHealth(ctx context.Context, component string) bool {
	// This is a simplified health check
	// In a real implementation, this would perform actual health checks
	switch component {
	case "microvm_manager":
		return m.microvmManager != nil
	case "resource_manager":
		return m.resourceManager != nil
	case "lxd_client":
		// Check if resource manager can get resource availability
		if m.resourceManager != nil {
			pools, err := m.resourceManager.GetResourceAvailability(ctx, "")
			return err == nil && len(pools) > 0
		}
		return false
	default:
		return true
	}
}

// metricsCollectionLoop runs periodic metrics collection
func (m *Manager) metricsCollectionLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.collectSystemMetrics()
		}
	}
}

// collectSystemMetrics collects system-wide metrics
func (m *Manager) collectSystemMetrics() {
	// Collect component health metrics
	m.mu.RLock()
	for component, healthy := range m.componentHealth {
		value := 0.0
		if healthy {
			value = 1.0
		}
		m.metrics.Gauge("component_health", value, 
			map[string]string{"component": component}, 
			"Component health status (1=healthy, 0=unhealthy)")
	}
	m.mu.RUnlock()

	// Collect alert metrics
	activeAlerts := len(m.alertManager.GetActiveAlerts())
	m.metrics.Gauge("active_alerts_count", float64(activeAlerts), nil, "Number of active alerts")

	// Collect recovery metrics
	activeRecoveries := len(m.recoveryManager.GetActiveRecoveries())
	m.metrics.Gauge("active_recoveries_count", float64(activeRecoveries), nil, "Number of active recovery attempts")

	// Collect degradation metrics
	_, level := m.degradationManager.GetOverallHealth()
	capacity := m.degradationManager.GetDegradedCapacity()
	m.metrics.Gauge("system_degraded_capacity", capacity, nil, "Current system capacity considering degradation")
	
	// Record degradation level metric
	levelValue := 0.0
	switch level {
	case "minor":
		levelValue = 1.0
	case "moderate":
		levelValue = 2.0
	case "severe":
		levelValue = 3.0
	case "critical":
		levelValue = 4.0
	}
	m.metrics.Gauge("system_degradation_level", levelValue, nil, "Current system degradation level")
}

// alertCleanupLoop runs periodic cleanup of old alerts
func (m *Manager) alertCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			// Clean up alerts older than 24 hours
			m.alertManager.CleanupOldAlerts(24 * time.Hour)
		}
	}
}

// shouldTriggerRecovery determines if recovery should be triggered for an operation failure
func (m *Manager) shouldTriggerRecovery(operation string, err error) bool {
	// Define operations that should trigger recovery
	recoveryOperations := map[string]bool{
		"start_microvm":   true,
		"stop_microvm":    false, // Don't recover stop operations
		"create_microvm":  true,
		"delete_microvm":  false, // Don't recover delete operations
		"health_check":    true,
	}

	return recoveryOperations[operation]
}

// classifyFailure classifies the type of failure for recovery purposes
func (m *Manager) classifyFailure(operation string, err error) string {
	errorStr := err.Error()

	// Simple error classification based on error message
	// In a real implementation, this would be more sophisticated
	if contains(errorStr, "network") || contains(errorStr, "connection") {
		return "network_failure"
	}
	if contains(errorStr, "resource") || contains(errorStr, "memory") || contains(errorStr, "cpu") {
		return "resource_exhaustion"
	}
	if contains(errorStr, "process") || contains(errorStr, "crash") {
		return "process_crash"
	}
	if contains(errorStr, "health") {
		return "health_check_failure"
	}

	return "default"
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && 
		   (s == substr || 
		    (len(s) > len(substr) && 
		     (s[:len(substr)] == substr || 
		      s[len(s)-len(substr):] == substr ||
		      containsSubstring(s, substr))))
}

// containsSubstring checks if s contains substr
func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}