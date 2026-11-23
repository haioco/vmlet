package monitoring

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
)

// ServiceState represents the current state of a service
type ServiceState string

const (
	ServiceStateHealthy   ServiceState = "healthy"
	ServiceStateDegraded  ServiceState = "degraded"
	ServiceStateUnavailable ServiceState = "unavailable"
)

// DegradationLevel represents the level of service degradation
type DegradationLevel string

const (
	DegradationLevelNone     DegradationLevel = "none"
	DegradationLevelMinor    DegradationLevel = "minor"
	DegradationLevelModerate DegradationLevel = "moderate"
	DegradationLevelSevere   DegradationLevel = "severe"
	DegradationLevelCritical DegradationLevel = "critical"
)

// ServiceHealth represents the health status of a service component
type ServiceHealth struct {
	Component       string           `json:"component"`
	State           ServiceState     `json:"state"`
	DegradationLevel DegradationLevel `json:"degradation_level"`
	LastHealthy     time.Time        `json:"last_healthy"`
	LastCheck       time.Time        `json:"last_check"`
	ErrorCount      int              `json:"error_count"`
	LastError       string           `json:"last_error,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

// DegradationRule defines conditions for service degradation
type DegradationRule struct {
	Component       string           `json:"component"`
	Condition       string           `json:"condition"`
	Threshold       float64          `json:"threshold"`
	Duration        time.Duration    `json:"duration"`
	DegradationLevel DegradationLevel `json:"degradation_level"`
	Actions         []string         `json:"actions"`
	Enabled         bool             `json:"enabled"`
}

// GracefulDegradationManager handles graceful service degradation
type GracefulDegradationManager struct {
	microvmManager   api.MicroVMManagerInterface
	resourceManager  api.ResourceManagerInterface
	alertManager     *AlertManager
	logger           *Logger
	metrics          *MetricsCollector

	mu               sync.RWMutex
	serviceHealth    map[string]*ServiceHealth
	degradationRules map[string]*DegradationRule
	degradationState map[string]DegradationLevel
	
	stopChan         chan struct{}
	checkInterval    time.Duration
}

// NewGracefulDegradationManager creates a new graceful degradation manager
func NewGracefulDegradationManager(
	microvmManager api.MicroVMManagerInterface,
	resourceManager api.ResourceManagerInterface,
	alertManager *AlertManager,
	logger *Logger,
	metrics *MetricsCollector,
) *GracefulDegradationManager {
	gdm := &GracefulDegradationManager{
		microvmManager:   microvmManager,
		resourceManager:  resourceManager,
		alertManager:     alertManager,
		logger:           logger,
		metrics:          metrics,
		serviceHealth:    make(map[string]*ServiceHealth),
		degradationRules: make(map[string]*DegradationRule),
		degradationState: make(map[string]DegradationLevel),
		stopChan:         make(chan struct{}),
		checkInterval:    30 * time.Second,
	}

	// Add default degradation rules
	gdm.addDefaultRules()

	return gdm
}

// Start starts the graceful degradation manager
func (gdm *GracefulDegradationManager) Start(ctx context.Context) error {
	gdm.logger.Info("Starting graceful degradation manager")

	// Start monitoring goroutine
	go gdm.monitoringLoop(ctx)

	return nil
}

// Stop stops the graceful degradation manager
func (gdm *GracefulDegradationManager) Stop() {
	gdm.logger.Info("Stopping graceful degradation manager")
	close(gdm.stopChan)
}

// UpdateServiceHealth updates the health status of a service component
func (gdm *GracefulDegradationManager) UpdateServiceHealth(component string, healthy bool, errorMsg string, metadata map[string]interface{}) {
	gdm.mu.Lock()
	defer gdm.mu.Unlock()

	health, exists := gdm.serviceHealth[component]
	if !exists {
		health = &ServiceHealth{
			Component:   component,
			State:       ServiceStateHealthy,
			DegradationLevel: DegradationLevelNone,
			LastHealthy: time.Now(),
			ErrorCount:  0,
		}
		gdm.serviceHealth[component] = health
	}

	health.LastCheck = time.Now()
	health.Metadata = metadata

	if healthy {
		if health.State != ServiceStateHealthy {
			gdm.logger.WithField("component", component).Info("Service recovered")
			gdm.alertManager.TriggerAlert(
				AlertTypeServiceDegradation,
				AlertSeverityInfo,
				component,
				"Service Recovered",
				fmt.Sprintf("Service %s has recovered", component),
				map[string]interface{}{
					"component": component,
					"previous_state": health.State,
				},
			)
		}
		
		health.State = ServiceStateHealthy
		health.DegradationLevel = DegradationLevelNone
		health.LastHealthy = time.Now()
		health.ErrorCount = 0
		health.LastError = ""
		gdm.degradationState[component] = DegradationLevelNone
	} else {
		health.ErrorCount++
		health.LastError = errorMsg
		
		// Determine degradation level based on error count and time since last healthy
		timeSinceHealthy := time.Since(health.LastHealthy)
		newLevel := gdm.calculateDegradationLevel(component, health.ErrorCount, timeSinceHealthy)
		
		if newLevel != health.DegradationLevel {
			gdm.handleDegradationLevelChange(component, health.DegradationLevel, newLevel)
			health.DegradationLevel = newLevel
			gdm.degradationState[component] = newLevel
		}

		// Update service state based on degradation level
		switch newLevel {
		case DegradationLevelNone, DegradationLevelMinor:
			health.State = ServiceStateHealthy
		case DegradationLevelModerate, DegradationLevelSevere:
			health.State = ServiceStateDegraded
		case DegradationLevelCritical:
			health.State = ServiceStateUnavailable
		}
	}

	// Record metrics
	stateValue := 0.0
	switch health.State {
	case ServiceStateHealthy:
		stateValue = 1.0
	case ServiceStateDegraded:
		stateValue = 0.5
	case ServiceStateUnavailable:
		stateValue = 0.0
	}

	gdm.metrics.Gauge("service_health_state", stateValue, 
		map[string]string{"component": component}, 
		"Service health state (1=healthy, 0.5=degraded, 0=unavailable)")
}

// GetServiceHealth returns the current health status of all services
func (gdm *GracefulDegradationManager) GetServiceHealth() map[string]*ServiceHealth {
	gdm.mu.RLock()
	defer gdm.mu.RUnlock()

	result := make(map[string]*ServiceHealth)
	for k, v := range gdm.serviceHealth {
		// Return a copy
		healthCopy := *v
		if v.Metadata != nil {
			healthCopy.Metadata = make(map[string]interface{})
			for mk, mv := range v.Metadata {
				healthCopy.Metadata[mk] = mv
			}
		}
		result[k] = &healthCopy
	}

	return result
}

// GetOverallHealth returns the overall system health
func (gdm *GracefulDegradationManager) GetOverallHealth() (ServiceState, DegradationLevel) {
	gdm.mu.RLock()
	defer gdm.mu.RUnlock()

	if len(gdm.serviceHealth) == 0 {
		return ServiceStateHealthy, DegradationLevelNone
	}

	worstState := ServiceStateHealthy
	worstDegradation := DegradationLevelNone

	for _, health := range gdm.serviceHealth {
		// Determine worst state
		switch health.State {
		case ServiceStateUnavailable:
			worstState = ServiceStateUnavailable
		case ServiceStateDegraded:
			if worstState == ServiceStateHealthy {
				worstState = ServiceStateDegraded
			}
		}

		// Determine worst degradation level
		if gdm.isDegradationWorse(health.DegradationLevel, worstDegradation) {
			worstDegradation = health.DegradationLevel
		}
	}

	return worstState, worstDegradation
}

// IsServiceAvailable checks if a service is available for new requests
func (gdm *GracefulDegradationManager) IsServiceAvailable(component string) bool {
	gdm.mu.RLock()
	defer gdm.mu.RUnlock()

	health, exists := gdm.serviceHealth[component]
	if !exists {
		return true // Assume available if not tracked
	}

	return health.State != ServiceStateUnavailable
}

// ShouldAcceptNewRequests determines if the system should accept new requests
func (gdm *GracefulDegradationManager) ShouldAcceptNewRequests() bool {
	state, level := gdm.GetOverallHealth()
	
	// Don't accept new requests if system is unavailable or critically degraded
	if state == ServiceStateUnavailable || level == DegradationLevelCritical {
		return false
	}

	return true
}

// GetDegradedCapacity returns the current capacity considering degradation
func (gdm *GracefulDegradationManager) GetDegradedCapacity() float64 {
	_, level := gdm.GetOverallHealth()
	
	switch level {
	case DegradationLevelNone:
		return 1.0
	case DegradationLevelMinor:
		return 0.9
	case DegradationLevelModerate:
		return 0.7
	case DegradationLevelSevere:
		return 0.5
	case DegradationLevelCritical:
		return 0.2
	default:
		return 1.0
	}
}

// monitoringLoop runs the main monitoring loop
func (gdm *GracefulDegradationManager) monitoringLoop(ctx context.Context) {
	ticker := time.NewTicker(gdm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gdm.stopChan:
			return
		case <-ticker.C:
			gdm.performHealthChecks(ctx)
		}
	}
}

// performHealthChecks performs health checks on all monitored services
func (gdm *GracefulDegradationManager) performHealthChecks(ctx context.Context) {
	// Check LXD connectivity
	gdm.checkLXDHealth(ctx)
	
	// Check firecracker manager health
	gdm.checkFirecrackerHealth(ctx)
	
	// Check resource manager health
	gdm.checkResourceManagerHealth(ctx)
	
	// Check overall system resources
	gdm.checkSystemResources(ctx)
}

// checkLXDHealth checks the health of LXD integration
func (gdm *GracefulDegradationManager) checkLXDHealth(ctx context.Context) {
	healthy := true
	errorMsg := ""
	metadata := make(map[string]interface{})

	// This would typically check LXD API connectivity
	// For now, we'll simulate the check
	if gdm.resourceManager != nil {
		// Try to get resource availability to test connectivity
		pools, err := gdm.resourceManager.GetResourceAvailability(ctx, "")
		if err != nil || len(pools) == 0 {
			healthy = false
			errorMsg = "Failed to get resource availability from LXD"
		} else {
			// Check if any pool is recently updated
			recentUpdate := false
			for _, pool := range pools {
				timeSinceUpdate := time.Since(pool.LastUpdated)
				if timeSinceUpdate < 2*time.Minute {
					recentUpdate = true
					metadata["last_update"] = pool.LastUpdated
					metadata["time_since_update"] = timeSinceUpdate.String()
					break
				}
			}
			if !recentUpdate {
				healthy = false
				errorMsg = "No resource pools updated recently"
			}
		}
	}

	gdm.UpdateServiceHealth("lxd", healthy, errorMsg, metadata)
}

// checkFirecrackerHealth checks the health of firecracker integration
func (gdm *GracefulDegradationManager) checkFirecrackerHealth(ctx context.Context) {
	healthy := true
	errorMsg := ""
	metadata := make(map[string]interface{})

	// This would typically check firecracker binary and containerd socket
	// For now, we'll simulate the check
	metadata["check_type"] = "simulated"
	
	gdm.UpdateServiceHealth("firecracker", healthy, errorMsg, metadata)
}

// checkResourceManagerHealth checks the health of resource management
func (gdm *GracefulDegradationManager) checkResourceManagerHealth(ctx context.Context) {
	healthy := true
	errorMsg := ""
	metadata := make(map[string]interface{})

	if gdm.resourceManager != nil {
		pools, err := gdm.resourceManager.GetResourceAvailability(ctx, "")
		if err == nil && len(pools) > 0 {
			// Check utilization across all pools
			totalUtilization := 0.0
			for _, pool := range pools {
				utilization := pool.GetUtilizationPercentage()
				totalUtilization += utilization
				metadata["utilization"] = utilization
				
				// Consider unhealthy if utilization is too high
				if utilization > 0.95 {
					healthy = false
					errorMsg = fmt.Sprintf("Resource utilization too high: %.1f%%", utilization*100)
					break
				}
			}
			if len(pools) > 0 {
				metadata["avg_utilization"] = totalUtilization / float64(len(pools))
			}
		}
	}

	gdm.UpdateServiceHealth("resource_manager", healthy, errorMsg, metadata)
}

// checkSystemResources checks overall system resource health
func (gdm *GracefulDegradationManager) checkSystemResources(ctx context.Context) {
	healthy := true
	errorMsg := ""
	metadata := make(map[string]interface{})

	if gdm.resourceManager != nil {
		pools, err := gdm.resourceManager.GetResourceAvailability(ctx, "")
		if err == nil && len(pools) > 0 {
			// Check if we have minimum resources available across all pools
			minCPU := uint32(2)
			minMemoryMB := uint32(1024)
			minStorageMB := uint32(5120)

			totalAvailableCPU := uint32(0)
			totalAvailableMemoryMB := uint32(0)
			totalAvailableStorageMB := uint32(0)

			for _, pool := range pools {
				totalAvailableCPU += pool.Availability.AvailableCPU
				totalAvailableMemoryMB += pool.Availability.AvailableMemoryMB
				totalAvailableStorageMB += pool.Availability.AvailableStorageMB
			}

			if totalAvailableCPU < minCPU {
				healthy = false
				errorMsg = fmt.Sprintf("Insufficient CPU available: %d < %d", totalAvailableCPU, minCPU)
			} else if totalAvailableMemoryMB < minMemoryMB {
				healthy = false
				errorMsg = fmt.Sprintf("Insufficient memory available: %dMB < %dMB", totalAvailableMemoryMB, minMemoryMB)
			} else if totalAvailableStorageMB < minStorageMB {
				healthy = false
				errorMsg = fmt.Sprintf("Insufficient storage available: %dMB < %dMB", totalAvailableStorageMB, minStorageMB)
			}

			metadata["available_cpu"] = totalAvailableCPU
			metadata["available_memory_mb"] = totalAvailableMemoryMB
			metadata["available_storage_mb"] = totalAvailableStorageMB
		}
	}

	gdm.UpdateServiceHealth("system_resources", healthy, errorMsg, metadata)
}

// calculateDegradationLevel calculates the degradation level based on error count and time
func (gdm *GracefulDegradationManager) calculateDegradationLevel(component string, errorCount int, timeSinceHealthy time.Duration) DegradationLevel {
	// Simple degradation calculation based on error count and time
	if errorCount == 0 {
		return DegradationLevelNone
	}

	if timeSinceHealthy < 1*time.Minute {
		if errorCount < 3 {
			return DegradationLevelMinor
		} else if errorCount < 5 {
			return DegradationLevelModerate
		} else {
			return DegradationLevelSevere
		}
	} else if timeSinceHealthy < 5*time.Minute {
		if errorCount < 5 {
			return DegradationLevelModerate
		} else if errorCount < 10 {
			return DegradationLevelSevere
		} else {
			return DegradationLevelCritical
		}
	} else {
		// Been unhealthy for more than 5 minutes
		return DegradationLevelCritical
	}
}

// handleDegradationLevelChange handles changes in degradation level
func (gdm *GracefulDegradationManager) handleDegradationLevelChange(component string, oldLevel, newLevel DegradationLevel) {
	gdm.logger.WithFields(map[string]interface{}{
		"component":  component,
		"old_level":  oldLevel,
		"new_level":  newLevel,
	}).Warn("Service degradation level changed")

	// Determine alert severity based on new level
	var severity AlertSeverity
	switch newLevel {
	case DegradationLevelMinor:
		severity = AlertSeverityWarning
	case DegradationLevelModerate:
		severity = AlertSeverityError
	case DegradationLevelSevere, DegradationLevelCritical:
		severity = AlertSeverityCritical
	default:
		severity = AlertSeverityInfo
	}

	// Trigger alert
	gdm.alertManager.TriggerAlert(
		AlertTypeServiceDegradation,
		severity,
		component,
		fmt.Sprintf("Service Degradation: %s", component),
		fmt.Sprintf("Service %s degradation level changed from %s to %s", component, oldLevel, newLevel),
		map[string]interface{}{
			"component":  component,
			"old_level":  oldLevel,
			"new_level":  newLevel,
		},
	)

	// Record metrics
	levelValue := gdm.degradationLevelToFloat(newLevel)
	gdm.metrics.Gauge("service_degradation_level", levelValue, 
		map[string]string{"component": component}, 
		"Service degradation level (0=none, 1=minor, 2=moderate, 3=severe, 4=critical)")
}

// isDegradationWorse compares two degradation levels
func (gdm *GracefulDegradationManager) isDegradationWorse(level1, level2 DegradationLevel) bool {
	levels := map[DegradationLevel]int{
		DegradationLevelNone:     0,
		DegradationLevelMinor:    1,
		DegradationLevelModerate: 2,
		DegradationLevelSevere:   3,
		DegradationLevelCritical: 4,
	}

	return levels[level1] > levels[level2]
}

// degradationLevelToFloat converts degradation level to float for metrics
func (gdm *GracefulDegradationManager) degradationLevelToFloat(level DegradationLevel) float64 {
	switch level {
	case DegradationLevelNone:
		return 0.0
	case DegradationLevelMinor:
		return 1.0
	case DegradationLevelModerate:
		return 2.0
	case DegradationLevelSevere:
		return 3.0
	case DegradationLevelCritical:
		return 4.0
	default:
		return 0.0
	}
}

// addDefaultRules adds default degradation rules
func (gdm *GracefulDegradationManager) addDefaultRules() {
	rules := []*DegradationRule{
		{
			Component:       "lxd",
			Condition:       "connection_failure",
			Threshold:       3,
			Duration:        1 * time.Minute,
			DegradationLevel: DegradationLevelSevere,
			Actions:         []string{"reduce_capacity", "alert"},
			Enabled:         true,
		},
		{
			Component:       "resource_manager",
			Condition:       "high_utilization",
			Threshold:       0.90,
			Duration:        2 * time.Minute,
			DegradationLevel: DegradationLevelModerate,
			Actions:         []string{"throttle_requests", "alert"},
			Enabled:         true,
		},
		{
			Component:       "system_resources",
			Condition:       "low_availability",
			Threshold:       0.05,
			Duration:        30 * time.Second,
			DegradationLevel: DegradationLevelSevere,
			Actions:         []string{"stop_accepting_requests", "alert"},
			Enabled:         true,
		},
	}

	for _, rule := range rules {
		gdm.degradationRules[rule.Component] = rule
	}
}