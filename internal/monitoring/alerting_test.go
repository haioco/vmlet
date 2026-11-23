package monitoring

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewAlertManager(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	
	am := NewAlertManager(logger, metrics)
	
	assert.NotNil(t, am)
	assert.NotNil(t, am.alerts)
	assert.NotNil(t, am.rules)
	assert.NotNil(t, am.handlers)
	assert.Len(t, am.rules, 5) // Should have default rules
}

func TestTriggerAlert(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger an alert
	am.TriggerAlert(
		AlertTypeResourceConflict,
		AlertSeverityWarning,
		"test_component",
		"Test Alert",
		"This is a test alert",
		map[string]interface{}{
			"test_key": "test_value",
		},
	)

	// Check that alert was created
	activeAlerts := am.GetActiveAlerts()
	assert.Len(t, activeAlerts, 1)

	alert := activeAlerts[0]
	assert.Equal(t, AlertTypeResourceConflict, alert.Type)
	assert.Equal(t, AlertSeverityWarning, alert.Severity)
	assert.Equal(t, "test_component", alert.Component)
	assert.Equal(t, "Test Alert", alert.Title)
	assert.Equal(t, "This is a test alert", alert.Message)
	assert.False(t, alert.Resolved)
	assert.Equal(t, 1, alert.Count)
	assert.Equal(t, "test_value", alert.Metadata["test_key"])
}

func TestTriggerDuplicateAlert(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger the same alert twice
	for i := 0; i < 2; i++ {
		am.TriggerAlert(
			AlertTypeResourceConflict,
			AlertSeverityWarning,
			"test_component",
			"Test Alert",
			"This is a test alert",
			nil,
		)
	}

	// Should only have one alert with count = 2
	activeAlerts := am.GetActiveAlerts()
	assert.Len(t, activeAlerts, 1)
	assert.Equal(t, 2, activeAlerts[0].Count)
}

func TestResolveAlert(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger an alert
	am.TriggerAlert(
		AlertTypeResourceConflict,
		AlertSeverityWarning,
		"test_component",
		"Test Alert",
		"This is a test alert",
		nil,
	)

	activeAlerts := am.GetActiveAlerts()
	assert.Len(t, activeAlerts, 1)
	alertID := activeAlerts[0].ID

	// Resolve the alert
	am.ResolveAlert(alertID)

	// Should have no active alerts
	activeAlerts = am.GetActiveAlerts()
	assert.Len(t, activeAlerts, 0)
}

func TestTriggerResourceConflictAlert(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Test different utilization levels
	tests := []struct {
		utilization     float64
		expectedSeverity AlertSeverity
	}{
		{0.85, AlertSeverityError},
		{0.90, AlertSeverityCritical},
		{0.95, AlertSeverityCritical},
		{0.80, AlertSeverityWarning},
	}

	for _, test := range tests {
		am.TriggerResourceConflictAlert("high_utilization", "node-1", test.utilization, "Test message")
		
		activeAlerts := am.GetActiveAlerts()
		if len(activeAlerts) > 0 {
			// Find the alert for this test (there might be multiple from previous iterations)
			found := false
			for _, alert := range activeAlerts {
				if alert.Metadata["utilization"] == test.utilization {
					assert.Equal(t, test.expectedSeverity, alert.Severity)
					found = true
					break
				}
			}
			assert.True(t, found, "Alert not found for utilization %f", test.utilization)
		}
	}
}

func TestTriggerHealthCheckAlert(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Test different failure counts
	tests := []struct {
		failureCount     int
		expectedSeverity AlertSeverity
	}{
		{1, AlertSeverityWarning},
		{3, AlertSeverityError},
		{5, AlertSeverityCritical},
		{10, AlertSeverityCritical},
	}

	for _, test := range tests {
		am.TriggerHealthCheckAlert("health_checker", "vm-123", test.failureCount, "Connection timeout")
		
		activeAlerts := am.GetActiveAlerts()
		if len(activeAlerts) > 0 {
			// Find the alert for this test
			found := false
			for _, alert := range activeAlerts {
				if alert.Metadata["failure_count"] == test.failureCount {
					assert.Equal(t, test.expectedSeverity, alert.Severity)
					found = true
					break
				}
			}
			assert.True(t, found, "Alert not found for failure count %d", test.failureCount)
		}
	}
}

func TestGetAlertHistory(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger alerts with different types and severities
	am.TriggerAlert(AlertTypeResourceConflict, AlertSeverityWarning, "comp1", "Alert 1", "Message 1", nil)
	am.TriggerAlert(AlertTypeHealthCheckFailure, AlertSeverityError, "comp2", "Alert 2", "Message 2", nil)
	am.TriggerAlert(AlertTypeResourceConflict, AlertSeverityCritical, "comp3", "Alert 3", "Message 3", nil)

	// Get all history
	allHistory := am.GetAlertHistory(time.Now().Add(-1*time.Hour), "", "")
	assert.Len(t, allHistory, 3)

	// Filter by type
	resourceHistory := am.GetAlertHistory(time.Now().Add(-1*time.Hour), AlertTypeResourceConflict, "")
	assert.Len(t, resourceHistory, 2)

	// Filter by severity
	errorHistory := am.GetAlertHistory(time.Now().Add(-1*time.Hour), "", AlertSeverityError)
	assert.Len(t, errorHistory, 1)

	// Filter by both type and severity
	criticalResourceHistory := am.GetAlertHistory(time.Now().Add(-1*time.Hour), AlertTypeResourceConflict, AlertSeverityCritical)
	assert.Len(t, criticalResourceHistory, 1)

	// Filter by time (future time should return no results)
	futureHistory := am.GetAlertHistory(time.Now().Add(1*time.Hour), "", "")
	assert.Len(t, futureHistory, 0)
}

func TestGetAlertStats(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger some alerts
	am.TriggerAlert(AlertTypeResourceConflict, AlertSeverityWarning, "comp1", "Alert 1", "Message 1", nil)
	am.TriggerAlert(AlertTypeHealthCheckFailure, AlertSeverityError, "comp2", "Alert 2", "Message 2", nil)
	
	// Resolve one alert
	activeAlerts := am.GetActiveAlerts()
	if len(activeAlerts) > 0 {
		am.ResolveAlert(activeAlerts[0].ID)
	}

	stats := am.GetAlertStats()
	
	assert.Equal(t, 2, stats["total_alerts"])
	assert.Equal(t, 1, stats["active_alerts"])
	assert.Equal(t, 1, stats["resolved_alerts"])
	
	severityMap := stats["by_severity"].(map[string]int)
	assert.Equal(t, 1, severityMap["warning"])
	assert.Equal(t, 1, severityMap["error"])
	
	typeMap := stats["by_type"].(map[string]int)
	assert.Equal(t, 1, typeMap["resource_conflict"])
	assert.Equal(t, 1, typeMap["health_check_failure"])
	
	componentMap := stats["by_component"].(map[string]int)
	assert.Equal(t, 1, componentMap["comp1"])
	assert.Equal(t, 1, componentMap["comp2"])
}

func TestCleanupOldAlerts(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	// Trigger and resolve an alert
	am.TriggerAlert(AlertTypeResourceConflict, AlertSeverityWarning, "comp1", "Alert 1", "Message 1", nil)
	
	activeAlerts := am.GetActiveAlerts()
	assert.Len(t, activeAlerts, 1)
	
	alertID := activeAlerts[0].ID
	am.ResolveAlert(alertID)

	// Manually set resolved time to past
	am.mu.Lock()
	if alert, exists := am.alerts[alertID]; exists && alert.ResolvedAt != nil {
		pastTime := time.Now().Add(-25 * time.Hour) // 25 hours ago
		alert.ResolvedAt = &pastTime
	}
	am.mu.Unlock()

	// Cleanup alerts older than 24 hours
	am.CleanupOldAlerts(24 * time.Hour)

	// Alert should be cleaned up
	am.mu.RLock()
	_, exists := am.alerts[alertID]
	am.mu.RUnlock()
	assert.False(t, exists)
}

func TestLogAlertHandler(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	handler := NewLogAlertHandler(logger)

	alert := &Alert{
		ID:        "test-alert-1",
		Type:      AlertTypeResourceConflict,
		Severity:  AlertSeverityWarning,
		Title:     "Test Alert",
		Message:   "This is a test alert",
		Component: "test_component",
		Timestamp: time.Now(),
	}

	err := handler.HandleAlert(context.Background(), alert)
	assert.NoError(t, err)
}

func TestAddRemoveAlertHandler(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	initialHandlerCount := len(am.handlers)

	// Add a handler
	handler := NewLogAlertHandler(logger)
	am.AddHandler(handler)

	assert.Len(t, am.handlers, initialHandlerCount+1)
}

func TestAddRemoveAlertRule(t *testing.T) {
	logger := NewLogger("test", LogLevelInfo, nil)
	metrics := NewMetricsCollector(logger)
	am := NewAlertManager(logger, metrics)

	initialRuleCount := len(am.rules)

	// Add a rule
	rule := &AlertRule{
		Name:      "test_rule",
		Type:      AlertTypeResourceConflict,
		Severity:  AlertSeverityWarning,
		Component: "test_component",
		Condition: "test_condition",
		Enabled:   true,
	}
	am.AddRule(rule)

	assert.Len(t, am.rules, initialRuleCount+1)

	// Remove the rule
	am.RemoveRule("test_rule")

	assert.Len(t, am.rules, initialRuleCount)
}