package monitoring

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AlertSeverity represents the severity level of an alert
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityError    AlertSeverity = "error"
	AlertSeverityCritical AlertSeverity = "critical"
)

// AlertType represents different types of alerts
type AlertType string

const (
	AlertTypeResourceConflict    AlertType = "resource_conflict"
	AlertTypeHealthCheckFailure  AlertType = "health_check_failure"
	AlertTypeServiceDegradation  AlertType = "service_degradation"
	AlertTypeOperationFailure    AlertType = "operation_failure"
	AlertTypePerformanceIssue    AlertType = "performance_issue"
	AlertTypeSecurityIssue       AlertType = "security_issue"
	AlertTypeConfigurationError  AlertType = "configuration_error"
)

// Alert represents a system alert
type Alert struct {
	ID          string                 `json:"id"`
	Type        AlertType              `json:"type"`
	Severity    AlertSeverity          `json:"severity"`
	Title       string                 `json:"title"`
	Message     string                 `json:"message"`
	Component   string                 `json:"component"`
	NodeID      string                 `json:"node_id,omitempty"`
	MicroVMID   string                 `json:"microvm_id,omitempty"`
	Timestamp   time.Time              `json:"timestamp"`
	Resolved    bool                   `json:"resolved"`
	ResolvedAt  *time.Time             `json:"resolved_at,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Count       int                    `json:"count"`
	LastSeen    time.Time              `json:"last_seen"`
}

// AlertRule defines conditions for triggering alerts
type AlertRule struct {
	Name        string        `json:"name"`
	Type        AlertType     `json:"type"`
	Severity    AlertSeverity `json:"severity"`
	Component   string        `json:"component"`
	Condition   string        `json:"condition"`
	Threshold   float64       `json:"threshold,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
	Enabled     bool          `json:"enabled"`
	Cooldown    time.Duration `json:"cooldown,omitempty"`
}

// AlertHandler defines the interface for handling alerts
type AlertHandler interface {
	HandleAlert(ctx context.Context, alert *Alert) error
}

// AlertManager manages system alerts and notifications
type AlertManager struct {
	mu           sync.RWMutex
	alerts       map[string]*Alert
	rules        map[string]*AlertRule
	handlers     []AlertHandler
	logger       *Logger
	metrics      *MetricsCollector
	alertCounter int
}

// NewAlertManager creates a new alert manager
func NewAlertManager(logger *Logger, metrics *MetricsCollector) *AlertManager {
	am := &AlertManager{
		alerts:   make(map[string]*Alert),
		rules:    make(map[string]*AlertRule),
		handlers: make([]AlertHandler, 0),
		logger:   logger,
		metrics:  metrics,
	}

	// Add default alert rules
	am.addDefaultRules()

	return am
}

// AddHandler adds an alert handler
func (am *AlertManager) AddHandler(handler AlertHandler) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.handlers = append(am.handlers, handler)
}

// AddRule adds an alert rule
func (am *AlertManager) AddRule(rule *AlertRule) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.rules[rule.Name] = rule
}

// RemoveRule removes an alert rule
func (am *AlertManager) RemoveRule(name string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	delete(am.rules, name)
}

// TriggerAlert creates and processes a new alert
func (am *AlertManager) TriggerAlert(alertType AlertType, severity AlertSeverity, component, title, message string, metadata map[string]interface{}) {
	am.mu.Lock()
	am.alertCounter++
	alertID := fmt.Sprintf("alert_%d_%d", time.Now().Unix(), am.alertCounter)
	am.mu.Unlock()

	alert := &Alert{
		ID:        alertID,
		Type:      alertType,
		Severity:  severity,
		Title:     title,
		Message:   message,
		Component: component,
		Timestamp: time.Now(),
		Resolved:  false,
		Metadata:  metadata,
		Count:     1,
		LastSeen:  time.Now(),
	}

	// Extract context information from metadata
	if metadata != nil {
		if nodeID, ok := metadata["node_id"].(string); ok {
			alert.NodeID = nodeID
		}
		if microvmID, ok := metadata["microvm_id"].(string); ok {
			alert.MicroVMID = microvmID
		}
	}

	am.processAlert(alert)
}

// TriggerResourceConflictAlert triggers a resource conflict alert
func (am *AlertManager) TriggerResourceConflictAlert(conflictType string, nodeID string, utilization float64, message string) {
	severity := AlertSeverityWarning
	if utilization > 0.90 {
		severity = AlertSeverityCritical
	} else if utilization > 0.85 {
		severity = AlertSeverityError
	}

	metadata := map[string]interface{}{
		"node_id":      nodeID,
		"utilization":  utilization,
		"conflict_type": conflictType,
	}

	am.TriggerAlert(
		AlertTypeResourceConflict,
		severity,
		"resource_manager",
		fmt.Sprintf("Resource Conflict: %s", conflictType),
		message,
		metadata,
	)
}

// TriggerHealthCheckAlert triggers a health check failure alert
func (am *AlertManager) TriggerHealthCheckAlert(component, microvmID string, failureCount int, lastError string) {
	severity := AlertSeverityWarning
	if failureCount >= 5 {
		severity = AlertSeverityCritical
	} else if failureCount >= 3 {
		severity = AlertSeverityError
	}

	metadata := map[string]interface{}{
		"microvm_id":    microvmID,
		"failure_count": failureCount,
		"last_error":    lastError,
	}

	am.TriggerAlert(
		AlertTypeHealthCheckFailure,
		severity,
		component,
		fmt.Sprintf("Health Check Failure: %s", component),
		fmt.Sprintf("Health check failed %d times for microVM %s: %s", failureCount, microvmID, lastError),
		metadata,
	)
}

// TriggerServiceDegradationAlert triggers a service degradation alert
func (am *AlertManager) TriggerServiceDegradationAlert(service string, reason string, impact string) {
	metadata := map[string]interface{}{
		"service": service,
		"reason":  reason,
		"impact":  impact,
	}

	am.TriggerAlert(
		AlertTypeServiceDegradation,
		AlertSeverityError,
		service,
		fmt.Sprintf("Service Degradation: %s", service),
		fmt.Sprintf("Service %s is degraded: %s. Impact: %s", service, reason, impact),
		metadata,
	)
}

// TriggerOperationFailureAlert triggers an operation failure alert
func (am *AlertManager) TriggerOperationFailureAlert(operation, component, microvmID string, err error) {
	metadata := map[string]interface{}{
		"operation":  operation,
		"microvm_id": microvmID,
		"error":      err.Error(),
	}

	am.TriggerAlert(
		AlertTypeOperationFailure,
		AlertSeverityError,
		component,
		fmt.Sprintf("Operation Failure: %s", operation),
		fmt.Sprintf("Operation %s failed for microVM %s: %v", operation, microvmID, err),
		metadata,
	)
}

// ResolveAlert marks an alert as resolved
func (am *AlertManager) ResolveAlert(alertID string) {
	am.mu.Lock()
	defer am.mu.Unlock()

	if alert, exists := am.alerts[alertID]; exists {
		now := time.Now()
		alert.Resolved = true
		alert.ResolvedAt = &now

		am.logger.WithField("alert_id", alertID).Info("Alert resolved")
		am.metrics.Counter("alerts_resolved_total", 
			map[string]string{"type": string(alert.Type), "severity": string(alert.Severity)}, 
			"Total number of resolved alerts")
	}
}

// GetActiveAlerts returns all active (unresolved) alerts
func (am *AlertManager) GetActiveAlerts() []*Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()

	var active []*Alert
	for _, alert := range am.alerts {
		if !alert.Resolved {
			// Return a copy to prevent external modifications
			alertCopy := *alert
			if alert.Metadata != nil {
				alertCopy.Metadata = make(map[string]interface{})
				for k, v := range alert.Metadata {
					alertCopy.Metadata[k] = v
				}
			}
			active = append(active, &alertCopy)
		}
	}

	return active
}

// GetAlertHistory returns alert history with optional filtering
func (am *AlertManager) GetAlertHistory(since time.Time, alertType AlertType, severity AlertSeverity) []*Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()

	var history []*Alert
	for _, alert := range am.alerts {
		// Apply filters
		if alert.Timestamp.Before(since) {
			continue
		}
		if alertType != "" && alert.Type != alertType {
			continue
		}
		if severity != "" && alert.Severity != severity {
			continue
		}

		// Return a copy
		alertCopy := *alert
		if alert.Metadata != nil {
			alertCopy.Metadata = make(map[string]interface{})
			for k, v := range alert.Metadata {
				alertCopy.Metadata[k] = v
			}
		}
		history = append(history, &alertCopy)
	}

	return history
}

// processAlert processes a new alert
func (am *AlertManager) processAlert(alert *Alert) {
	am.mu.Lock()
	
	// Check for duplicate alerts (same type, component, and similar metadata)
	duplicateKey := am.getDuplicateKey(alert)
	if existing, exists := am.alerts[duplicateKey]; exists && !existing.Resolved {
		// Update existing alert
		existing.Count++
		existing.LastSeen = time.Now()
		existing.Message = alert.Message // Update with latest message
		am.mu.Unlock()
		
		am.logger.WithFields(map[string]interface{}{
			"alert_id": existing.ID,
			"count":    existing.Count,
		}).Info("Duplicate alert suppressed, count updated")
		return
	}

	// Store new alert
	am.alerts[alert.ID] = alert
	am.alerts[duplicateKey] = alert // Also store by duplicate key for future lookups
	am.mu.Unlock()

	// Log the alert
	am.logger.WithFields(map[string]interface{}{
		"alert_id":  alert.ID,
		"type":      alert.Type,
		"severity":  alert.Severity,
		"component": alert.Component,
	}).Warn("Alert triggered: " + alert.Title)

	// Record metrics
	am.metrics.RecordAlert(string(alert.Type), string(alert.Severity), alert.Component)

	// Process through handlers
	ctx := context.Background()
	for _, handler := range am.handlers {
		go func(h AlertHandler) {
			if err := h.HandleAlert(ctx, alert); err != nil {
				am.logger.WithField("alert_id", alert.ID).Error("Alert handler failed", err)
			}
		}(handler)
	}
}

// getDuplicateKey generates a key for detecting duplicate alerts
func (am *AlertManager) getDuplicateKey(alert *Alert) string {
	key := fmt.Sprintf("%s_%s_%s", alert.Type, alert.Component, alert.Severity)
	
	// Add context-specific identifiers
	if alert.NodeID != "" {
		key += "_" + alert.NodeID
	}
	if alert.MicroVMID != "" {
		key += "_" + alert.MicroVMID
	}
	
	return key
}

// addDefaultRules adds default alert rules
func (am *AlertManager) addDefaultRules() {
	rules := []*AlertRule{
		{
			Name:      "high_cpu_utilization",
			Type:      AlertTypeResourceConflict,
			Severity:  AlertSeverityWarning,
			Component: "resource_manager",
			Condition: "cpu_utilization > threshold",
			Threshold: 0.80,
			Duration:  5 * time.Minute,
			Enabled:   true,
			Cooldown:  10 * time.Minute,
		},
		{
			Name:      "critical_cpu_utilization",
			Type:      AlertTypeResourceConflict,
			Severity:  AlertSeverityCritical,
			Component: "resource_manager",
			Condition: "cpu_utilization > threshold",
			Threshold: 0.90,
			Duration:  2 * time.Minute,
			Enabled:   true,
			Cooldown:  5 * time.Minute,
		},
		{
			Name:      "high_memory_utilization",
			Type:      AlertTypeResourceConflict,
			Severity:  AlertSeverityWarning,
			Component: "resource_manager",
			Condition: "memory_utilization > threshold",
			Threshold: 0.80,
			Duration:  5 * time.Minute,
			Enabled:   true,
			Cooldown:  10 * time.Minute,
		},
		{
			Name:      "microvm_health_check_failure",
			Type:      AlertTypeHealthCheckFailure,
			Severity:  AlertSeverityError,
			Component: "health_checker",
			Condition: "consecutive_failures >= 3",
			Threshold: 3,
			Duration:  0,
			Enabled:   true,
			Cooldown:  15 * time.Minute,
		},
		{
			Name:      "lxd_connection_failure",
			Type:      AlertTypeServiceDegradation,
			Severity:  AlertSeverityCritical,
			Component: "lxd_client",
			Condition: "connection_failed",
			Duration:  1 * time.Minute,
			Enabled:   true,
			Cooldown:  5 * time.Minute,
		},
	}

	for _, rule := range rules {
		am.rules[rule.Name] = rule
	}
}

// CleanupOldAlerts removes old resolved alerts to prevent memory leaks
func (am *AlertManager) CleanupOldAlerts(maxAge time.Duration) {
	am.mu.Lock()
	defer am.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var toDelete []string

	for id, alert := range am.alerts {
		if alert.Resolved && alert.ResolvedAt != nil && alert.ResolvedAt.Before(cutoff) {
			toDelete = append(toDelete, id)
		}
	}

	for _, id := range toDelete {
		delete(am.alerts, id)
	}

	if len(toDelete) > 0 {
		am.logger.Infof("Cleaned up %d old resolved alerts", len(toDelete))
	}
}

// GetAlertStats returns statistics about alerts
func (am *AlertManager) GetAlertStats() map[string]interface{} {
	am.mu.RLock()
	defer am.mu.RUnlock()

	stats := map[string]interface{}{
		"total_alerts":    len(am.alerts),
		"active_alerts":   0,
		"resolved_alerts": 0,
		"by_severity":     make(map[string]int),
		"by_type":         make(map[string]int),
		"by_component":    make(map[string]int),
	}

	severityMap := stats["by_severity"].(map[string]int)
	typeMap := stats["by_type"].(map[string]int)
	componentMap := stats["by_component"].(map[string]int)

	for _, alert := range am.alerts {
		if alert.Resolved {
			stats["resolved_alerts"] = stats["resolved_alerts"].(int) + 1
		} else {
			stats["active_alerts"] = stats["active_alerts"].(int) + 1
		}

		severityMap[string(alert.Severity)]++
		typeMap[string(alert.Type)]++
		componentMap[alert.Component]++
	}

	return stats
}

// LogAlertHandler is a simple alert handler that logs alerts
type LogAlertHandler struct {
	logger *Logger
}

// NewLogAlertHandler creates a new log alert handler
func NewLogAlertHandler(logger *Logger) *LogAlertHandler {
	return &LogAlertHandler{logger: logger}
}

// HandleAlert handles an alert by logging it
func (h *LogAlertHandler) HandleAlert(ctx context.Context, alert *Alert) error {
	h.logger.WithFields(map[string]interface{}{
		"alert_id":  alert.ID,
		"type":      alert.Type,
		"severity":  alert.Severity,
		"component": alert.Component,
		"node_id":   alert.NodeID,
		"microvm_id": alert.MicroVMID,
	}).Warn(fmt.Sprintf("ALERT: %s - %s", alert.Title, alert.Message))
	
	return nil
}