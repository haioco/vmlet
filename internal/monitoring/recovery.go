package monitoring

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
)

// RecoveryAction represents different types of recovery actions
type RecoveryAction string

const (
	RecoveryActionRestart     RecoveryAction = "restart"
	RecoveryActionRecreate    RecoveryAction = "recreate"
	RecoveryActionMigrate     RecoveryAction = "migrate"
	RecoveryActionAlert       RecoveryAction = "alert"
	RecoveryActionGracefulStop RecoveryAction = "graceful_stop"
)

// RecoveryStrategy defines how to handle different types of failures
type RecoveryStrategy struct {
	MaxRetries      int           `json:"max_retries"`
	RetryInterval   time.Duration `json:"retry_interval"`
	BackoffFactor   float64       `json:"backoff_factor"`
	MaxBackoff      time.Duration `json:"max_backoff"`
	Actions         []RecoveryAction `json:"actions"`
	Timeout         time.Duration `json:"timeout"`
	RequireManualApproval bool    `json:"require_manual_approval"`
}

// RecoveryAttempt tracks a recovery attempt
type RecoveryAttempt struct {
	ID          string         `json:"id"`
	MicroVMID   string         `json:"microvm_id"`
	FailureType string         `json:"failure_type"`
	Action      RecoveryAction `json:"action"`
	StartTime   time.Time      `json:"start_time"`
	EndTime     *time.Time     `json:"end_time,omitempty"`
	Success     bool           `json:"success"`
	Error       string         `json:"error,omitempty"`
	Attempt     int            `json:"attempt"`
}

// RecoveryManager handles automatic recovery of failed microVMs and services
type RecoveryManager struct {
	microvmManager   api.MicroVMManagerInterface
	resourceManager  api.ResourceManagerInterface
	alertManager     *AlertManager
	logger           *Logger
	metrics          *MetricsCollector

	mu               sync.RWMutex
	strategies       map[string]*RecoveryStrategy
	activeRecoveries map[string]*RecoveryAttempt
	recoveryHistory  []*RecoveryAttempt
	
	stopChan         chan struct{}
	recoveryQueue    chan RecoveryRequest
}

// RecoveryRequest represents a request for recovery
type RecoveryRequest struct {
	MicroVMID   string
	FailureType string
	Context     map[string]interface{}
	Priority    int
}

// NewRecoveryManager creates a new recovery manager
func NewRecoveryManager(
	microvmManager api.MicroVMManagerInterface,
	resourceManager api.ResourceManagerInterface,
	alertManager *AlertManager,
	logger *Logger,
	metrics *MetricsCollector,
) *RecoveryManager {
	rm := &RecoveryManager{
		microvmManager:   microvmManager,
		resourceManager:  resourceManager,
		alertManager:     alertManager,
		logger:           logger,
		metrics:          metrics,
		strategies:       make(map[string]*RecoveryStrategy),
		activeRecoveries: make(map[string]*RecoveryAttempt),
		recoveryHistory:  make([]*RecoveryAttempt, 0),
		stopChan:         make(chan struct{}),
		recoveryQueue:    make(chan RecoveryRequest, 100),
	}

	// Add default recovery strategies
	rm.addDefaultStrategies()

	return rm
}

// Start starts the recovery manager
func (rm *RecoveryManager) Start(ctx context.Context) error {
	rm.logger.Info("Starting recovery manager")

	// Start recovery worker
	go rm.recoveryWorker(ctx)

	return nil
}

// Stop stops the recovery manager
func (rm *RecoveryManager) Stop() {
	rm.logger.Info("Stopping recovery manager")
	close(rm.stopChan)
}

// RequestRecovery requests recovery for a failed microVM
func (rm *RecoveryManager) RequestRecovery(microvmID, failureType string, context map[string]interface{}) {
	request := RecoveryRequest{
		MicroVMID:   microvmID,
		FailureType: failureType,
		Context:     context,
		Priority:    1, // Default priority
	}

	select {
	case rm.recoveryQueue <- request:
		rm.logger.WithFields(map[string]interface{}{
			"microvm_id":   microvmID,
			"failure_type": failureType,
		}).Info("Recovery requested")
	default:
		rm.logger.WithFields(map[string]interface{}{
			"microvm_id":   microvmID,
			"failure_type": failureType,
		}).Warn("Recovery queue full, dropping request")
	}
}

// AddStrategy adds a recovery strategy
func (rm *RecoveryManager) AddStrategy(failureType string, strategy *RecoveryStrategy) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.strategies[failureType] = strategy
}

// GetActiveRecoveries returns currently active recovery attempts
func (rm *RecoveryManager) GetActiveRecoveries() []*RecoveryAttempt {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var active []*RecoveryAttempt
	for _, attempt := range rm.activeRecoveries {
		// Return a copy
		attemptCopy := *attempt
		active = append(active, &attemptCopy)
	}

	return active
}

// GetRecoveryHistory returns recovery history
func (rm *RecoveryManager) GetRecoveryHistory(since time.Time) []*RecoveryAttempt {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var history []*RecoveryAttempt
	for _, attempt := range rm.recoveryHistory {
		if attempt.StartTime.After(since) {
			// Return a copy
			attemptCopy := *attempt
			history = append(history, &attemptCopy)
		}
	}

	return history
}

// recoveryWorker processes recovery requests
func (rm *RecoveryManager) recoveryWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-rm.stopChan:
			return
		case request := <-rm.recoveryQueue:
			rm.processRecoveryRequest(ctx, request)
		}
	}
}

// processRecoveryRequest processes a single recovery request
func (rm *RecoveryManager) processRecoveryRequest(ctx context.Context, request RecoveryRequest) {
	rm.mu.RLock()
	strategy, exists := rm.strategies[request.FailureType]
	if !exists {
		strategy = rm.strategies["default"]
	}
	rm.mu.RUnlock()

	if strategy == nil {
		rm.logger.WithFields(map[string]interface{}{
			"microvm_id":   request.MicroVMID,
			"failure_type": request.FailureType,
		}).Warn("No recovery strategy found")
		return
	}

	// Check if recovery is already in progress
	rm.mu.RLock()
	if _, inProgress := rm.activeRecoveries[request.MicroVMID]; inProgress {
		rm.mu.RUnlock()
		rm.logger.WithField("microvm_id", request.MicroVMID).Info("Recovery already in progress")
		return
	}
	rm.mu.RUnlock()

	// Start recovery
	rm.executeRecovery(ctx, request, strategy)
}

// executeRecovery executes the recovery strategy
func (rm *RecoveryManager) executeRecovery(ctx context.Context, request RecoveryRequest, strategy *RecoveryStrategy) {
	attemptID := fmt.Sprintf("recovery_%s_%d", request.MicroVMID, time.Now().Unix())
	
	attempt := &RecoveryAttempt{
		ID:          attemptID,
		MicroVMID:   request.MicroVMID,
		FailureType: request.FailureType,
		StartTime:   time.Now(),
		Attempt:     1,
	}

	// Mark recovery as active
	rm.mu.Lock()
	rm.activeRecoveries[request.MicroVMID] = attempt
	rm.mu.Unlock()

	rm.logger.WithFields(map[string]interface{}{
		"attempt_id":   attemptID,
		"microvm_id":   request.MicroVMID,
		"failure_type": request.FailureType,
	}).Info("Starting recovery attempt")

	// Execute recovery actions
	success := false
	var lastError error

	for attemptNum := 1; attemptNum <= strategy.MaxRetries; attemptNum++ {
		attempt.Attempt = attemptNum

		for _, action := range strategy.Actions {
			attempt.Action = action

			// Create timeout context for this action
			actionCtx, cancel := context.WithTimeout(ctx, strategy.Timeout)
			
			err := rm.executeRecoveryAction(actionCtx, request.MicroVMID, action, request.Context)
			cancel()

			if err == nil {
				success = true
				break
			}

			lastError = err
			rm.logger.WithFields(map[string]interface{}{
				"attempt_id": attemptID,
				"microvm_id": request.MicroVMID,
				"action":     action,
				"attempt":    attemptNum,
			}).Error("Recovery action failed", err)

			// Record metrics
			rm.metrics.Counter("recovery_actions_total", 
				map[string]string{
					"action":       string(action),
					"failure_type": request.FailureType,
					"status":       "failure",
				}, 
				"Total recovery actions attempted")
		}

		if success {
			break
		}

		// Wait before retry (with exponential backoff)
		if attemptNum < strategy.MaxRetries {
			backoff := time.Duration(float64(strategy.RetryInterval) * 
				(1 + strategy.BackoffFactor*float64(attemptNum-1)))
			if backoff > strategy.MaxBackoff {
				backoff = strategy.MaxBackoff
			}

			rm.logger.WithFields(map[string]interface{}{
				"attempt_id": attemptID,
				"microvm_id": request.MicroVMID,
				"backoff":    backoff,
			}).Info("Waiting before retry")

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}

	// Complete recovery attempt
	now := time.Now()
	attempt.EndTime = &now
	attempt.Success = success
	if lastError != nil {
		attempt.Error = lastError.Error()
	}

	// Remove from active recoveries and add to history
	rm.mu.Lock()
	delete(rm.activeRecoveries, request.MicroVMID)
	rm.recoveryHistory = append(rm.recoveryHistory, attempt)
	
	// Limit history size
	if len(rm.recoveryHistory) > 1000 {
		rm.recoveryHistory = rm.recoveryHistory[100:]
	}
	rm.mu.Unlock()

	// Log result
	if success {
		rm.logger.WithFields(map[string]interface{}{
			"attempt_id": attemptID,
			"microvm_id": request.MicroVMID,
			"duration":   now.Sub(attempt.StartTime),
		}).Info("Recovery completed successfully")

		rm.metrics.Counter("recovery_attempts_total", 
			map[string]string{
				"failure_type": request.FailureType,
				"status":       "success",
			}, 
			"Total recovery attempts")
	} else {
		rm.logger.WithFields(map[string]interface{}{
			"attempt_id": attemptID,
			"microvm_id": request.MicroVMID,
			"duration":   now.Sub(attempt.StartTime),
		}).Error("Recovery failed after all attempts", lastError)

		rm.metrics.Counter("recovery_attempts_total", 
			map[string]string{
				"failure_type": request.FailureType,
				"status":       "failure",
			}, 
			"Total recovery attempts")

		// Trigger alert for failed recovery
		rm.alertManager.TriggerAlert(
			AlertTypeOperationFailure,
			AlertSeverityCritical,
			"recovery_manager",
			"Recovery Failed",
			fmt.Sprintf("Failed to recover microVM %s after %d attempts: %v", 
				request.MicroVMID, strategy.MaxRetries, lastError),
			map[string]interface{}{
				"microvm_id":   request.MicroVMID,
				"failure_type": request.FailureType,
				"attempts":     strategy.MaxRetries,
			},
		)
	}
}

// executeRecoveryAction executes a specific recovery action
func (rm *RecoveryManager) executeRecoveryAction(ctx context.Context, microvmID string, action RecoveryAction, context map[string]interface{}) error {
	switch action {
	case RecoveryActionRestart:
		return rm.restartMicroVM(ctx, microvmID)
	case RecoveryActionRecreate:
		return rm.recreateMicroVM(ctx, microvmID, context)
	case RecoveryActionMigrate:
		return rm.migrateMicroVM(ctx, microvmID, context)
	case RecoveryActionGracefulStop:
		return rm.gracefulStopMicroVM(ctx, microvmID)
	case RecoveryActionAlert:
		return rm.sendRecoveryAlert(microvmID, context)
	default:
		return fmt.Errorf("unknown recovery action: %s", action)
	}
}

// restartMicroVM attempts to restart a microVM
func (rm *RecoveryManager) restartMicroVM(ctx context.Context, microvmID string) error {
	rm.logger.WithField("microvm_id", microvmID).Info("Attempting to restart microVM")

	// For restart, we need to get the microVM instance first
	// This is a simplified implementation - in reality we'd need to store/retrieve microVM instances
	microvm := &models.MicroVMInstance{ID: microvmID}

	// Stop the microVM
	if err := rm.microvmManager.StopMicroVM(ctx, microvm, false); err != nil {
		rm.logger.WithField("microvm_id", microvmID).Warn("Failed to stop microVM during restart")
		// Continue anyway, as the VM might already be stopped
	}

	// Wait a moment for cleanup
	time.Sleep(2 * time.Second)

	// Start the microVM
	if err := rm.microvmManager.StartMicroVM(ctx, microvm); err != nil {
		return fmt.Errorf("failed to start microVM: %w", err)
	}

	rm.logger.WithField("microvm_id", microvmID).Info("MicroVM restarted successfully")
	return nil
}

// recreateMicroVM attempts to recreate a microVM
func (rm *RecoveryManager) recreateMicroVM(ctx context.Context, microvmID string, context map[string]interface{}) error {
	rm.logger.WithField("microvm_id", microvmID).Info("Attempting to recreate microVM")

	// For recreation, we need the original microVM instance
	// This is a simplified implementation - in reality we'd need to store/retrieve microVM instances
	microvm := &models.MicroVMInstance{ID: microvmID}

	// Delete the existing microVM
	if err := rm.microvmManager.DeleteMicroVM(ctx, microvm); err != nil {
		rm.logger.WithField("microvm_id", microvmID).Warn("Failed to delete microVM during recreation")
	}

	// Wait for cleanup
	time.Sleep(5 * time.Second)

	// Create a new microVM with the same specifications
	// In a real implementation, we'd retrieve the original specs from storage
	newMicroVM := &models.MicroVMInstance{
		ID: microvmID + "-recreated",
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 5120,
		},
	}
	
	if err := rm.microvmManager.CreateMicroVM(ctx, newMicroVM); err != nil {
		return fmt.Errorf("failed to recreate microVM: %w", err)
	}

	rm.logger.WithFields(map[string]interface{}{
		"old_microvm_id": microvmID,
		"new_microvm_id": newMicroVM.ID,
	}).Info("MicroVM recreated successfully")

	return nil
}

// migrateMicroVM attempts to migrate a microVM to another node
func (rm *RecoveryManager) migrateMicroVM(ctx context.Context, microvmID string, context map[string]interface{}) error {
	rm.logger.WithField("microvm_id", microvmID).Info("Attempting to migrate microVM")
	
	// Migration is not implemented in this version
	// This would require coordination with multiple nodes
	return fmt.Errorf("migration not implemented")
}

// gracefulStopMicroVM gracefully stops a microVM
func (rm *RecoveryManager) gracefulStopMicroVM(ctx context.Context, microvmID string) error {
	rm.logger.WithField("microvm_id", microvmID).Info("Gracefully stopping microVM")

	// Create a microVM instance for the stop operation
	microvm := &models.MicroVMInstance{ID: microvmID}

	if err := rm.microvmManager.StopMicroVM(ctx, microvm, false); err != nil {
		return fmt.Errorf("failed to stop microVM: %w", err)
	}

	rm.logger.WithField("microvm_id", microvmID).Info("MicroVM stopped gracefully")
	return nil
}

// sendRecoveryAlert sends an alert about the recovery need
func (rm *RecoveryManager) sendRecoveryAlert(microvmID string, context map[string]interface{}) error {
	rm.alertManager.TriggerAlert(
		AlertTypeOperationFailure,
		AlertSeverityError,
		"recovery_manager",
		"Manual Recovery Required",
		fmt.Sprintf("MicroVM %s requires manual recovery", microvmID),
		map[string]interface{}{
			"microvm_id": microvmID,
			"context":    context,
		},
	)

	return nil
}

// addDefaultStrategies adds default recovery strategies
func (rm *RecoveryManager) addDefaultStrategies() {
	// Default strategy for unknown failures
	rm.strategies["default"] = &RecoveryStrategy{
		MaxRetries:    3,
		RetryInterval: 30 * time.Second,
		BackoffFactor: 2.0,
		MaxBackoff:    5 * time.Minute,
		Actions:       []RecoveryAction{RecoveryActionRestart, RecoveryActionAlert},
		Timeout:       2 * time.Minute,
	}

	// Health check failure strategy
	rm.strategies["health_check_failure"] = &RecoveryStrategy{
		MaxRetries:    2,
		RetryInterval: 1 * time.Minute,
		BackoffFactor: 1.5,
		MaxBackoff:    3 * time.Minute,
		Actions:       []RecoveryAction{RecoveryActionRestart},
		Timeout:       2 * time.Minute,
	}

	// Process crash strategy
	rm.strategies["process_crash"] = &RecoveryStrategy{
		MaxRetries:    3,
		RetryInterval: 30 * time.Second,
		BackoffFactor: 2.0,
		MaxBackoff:    5 * time.Minute,
		Actions:       []RecoveryAction{RecoveryActionRestart, RecoveryActionRecreate},
		Timeout:       3 * time.Minute,
	}

	// Network failure strategy
	rm.strategies["network_failure"] = &RecoveryStrategy{
		MaxRetries:    2,
		RetryInterval: 45 * time.Second,
		BackoffFactor: 1.5,
		MaxBackoff:    3 * time.Minute,
		Actions:       []RecoveryAction{RecoveryActionRestart},
		Timeout:       2 * time.Minute,
	}

	// Resource exhaustion strategy
	rm.strategies["resource_exhaustion"] = &RecoveryStrategy{
		MaxRetries:    1,
		RetryInterval: 2 * time.Minute,
		BackoffFactor: 1.0,
		MaxBackoff:    2 * time.Minute,
		Actions:       []RecoveryAction{RecoveryActionGracefulStop, RecoveryActionAlert},
		Timeout:       1 * time.Minute,
	}
}