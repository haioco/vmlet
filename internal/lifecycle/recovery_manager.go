package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/metrics"
	"github.com/haioco/vmlet/internal/models"
)

// RecoveryAction represents different types of recovery actions
type RecoveryAction string

const (
	RecoveryActionRestart    RecoveryAction = "restart"
	RecoveryActionRecreate   RecoveryAction = "recreate"
	RecoveryActionMigrate    RecoveryAction = "migrate"
	RecoveryActionManual     RecoveryAction = "manual"
)

// RecoveryAttempt represents a single recovery attempt
type RecoveryAttempt struct {
	Timestamp    time.Time      `json:"timestamp"`
	Action       RecoveryAction `json:"action"`
	Success      bool           `json:"success"`
	Error        string         `json:"error,omitempty"`
	Duration     time.Duration  `json:"duration"`
}

// RecoveryStatus represents the recovery status of a microVM
type RecoveryStatus struct {
	MicroVMID        string            `json:"microvm_id"`
	IsRecovering     bool              `json:"is_recovering"`
	RecoveryStarted  time.Time         `json:"recovery_started,omitempty"`
	AttemptCount     int               `json:"attempt_count"`
	MaxAttempts      int               `json:"max_attempts"`
	LastAttempt      *RecoveryAttempt  `json:"last_attempt,omitempty"`
	Attempts         []RecoveryAttempt `json:"attempts"`
	NextAttemptAt    time.Time         `json:"next_attempt_at,omitempty"`
	RecoveryDisabled bool              `json:"recovery_disabled"`
}

// CircuitBreaker prevents recovery thrashing by stopping auto-recovery after repeated failures
type CircuitBreaker struct {
	FailureCount    int
	FailureTimes    []time.Time
	Open            bool
	OpenSince       time.Time
	MaxFailures     int
	CooldownPeriod  time.Duration
	Window          time.Duration
}

func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		MaxFailures:    3,
		CooldownPeriod: 30 * time.Minute,
		Window:         time.Hour,
	}
}

func (cb *CircuitBreaker) RecordFailure() bool {
	now := time.Now()
	cb.FailureTimes = append(cb.FailureTimes, now)
	cb.FailureCount++

	cutoff := now.Add(-cb.Window)
	valid := cb.FailureTimes[:0]
	for _, t := range cb.FailureTimes {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	cb.FailureTimes = valid

	if !cb.Open && cb.FailureCount >= cb.MaxFailures {
		cb.Open = true
		cb.OpenSince = now
		return true
	}
	return false
}

func (cb *CircuitBreaker) RecordSuccess() {
	cb.FailureCount = 0
	cb.FailureTimes = nil
	cb.Open = false
	cb.OpenSince = time.Time{}
}

func (cb *CircuitBreaker) IsOpen() bool {
	if !cb.Open {
		return false
	}
	if time.Since(cb.OpenSince) > cb.CooldownPeriod {
		cb.Open = false
		cb.FailureCount = 0
		cb.FailureTimes = nil
		return false
	}
	return true
}

// RecoveryManager handles automatic recovery of unresponsive microVMs
type RecoveryManager struct {
	microvmManager api.MicroVMManagerInterface
	config         *Config
	
	mu             sync.RWMutex
	recoveryStatus map[string]*RecoveryStatus
	activeRecovery map[string]context.CancelFunc
	circuitBreakers map[string]*CircuitBreaker
}

// NewRecoveryManager creates a new recovery manager
func NewRecoveryManager(microvmManager api.MicroVMManagerInterface, config *Config) *RecoveryManager {
	return &RecoveryManager{
		microvmManager:  microvmManager,
		config:          config,
		recoveryStatus:  make(map[string]*RecoveryStatus),
		activeRecovery:  make(map[string]context.CancelFunc),
		circuitBreakers: make(map[string]*CircuitBreaker),
	}
}

// RecoverMicroVM attempts to recover an unresponsive microVM
func (rm *RecoveryManager) RecoverMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	if microvm == nil {
		return fmt.Errorf("microvm cannot be nil")
	}

	rm.mu.Lock()
	
	// Check if recovery is already in progress
	if cancel, exists := rm.activeRecovery[microvm.ID]; exists {
		rm.mu.Unlock()
		// Cancel existing recovery and start new one
		cancel()
		time.Sleep(1 * time.Second) // Brief pause to allow cleanup
		rm.mu.Lock()
	}

	// Get or create recovery status
	status, exists := rm.recoveryStatus[microvm.ID]
	if !exists {
		status = &RecoveryStatus{
			MicroVMID:   microvm.ID,
			MaxAttempts: rm.config.MaxRecoveryAttempts,
		}
		rm.recoveryStatus[microvm.ID] = status
	}

	// Check if recovery is disabled or max attempts reached
	if status.RecoveryDisabled {
		rm.mu.Unlock()
		return fmt.Errorf("recovery is disabled for microVM %s", microvm.ID)
	}

	if status.AttemptCount >= status.MaxAttempts {
		rm.mu.Unlock()
		return fmt.Errorf("maximum recovery attempts (%d) reached for microVM %s", status.MaxAttempts, microvm.ID)
	}

	// Check circuit breaker
	cb, exists := rm.circuitBreakers[microvm.ID]
	if !exists {
		cb = NewCircuitBreaker()
		rm.circuitBreakers[microvm.ID] = cb
	}
	if cb.IsOpen() {
		rm.mu.Unlock()
		return fmt.Errorf("recovery circuit breaker is open for microVM %s (cooldown: %v)", microvm.ID, cb.CooldownPeriod)
	}

	// Mark recovery as in progress
	status.IsRecovering = true
	if status.RecoveryStarted.IsZero() {
		status.RecoveryStarted = time.Now()
	}

	// Create recovery context with timeout
	recoveryCtx, cancel := context.WithTimeout(ctx, rm.config.RecoveryTimeout)
	rm.activeRecovery[microvm.ID] = cancel

	rm.mu.Unlock()

	// Perform recovery in a goroutine
	go rm.performRecovery(recoveryCtx, microvm, status)

	return nil
}

// performRecovery performs the actual recovery process
func (rm *RecoveryManager) performRecovery(ctx context.Context, microvm *models.MicroVMInstance, status *RecoveryStatus) {
	defer func() {
		rm.mu.Lock()
		status.IsRecovering = false
		delete(rm.activeRecovery, microvm.ID)
		rm.mu.Unlock()
	}()

	// Determine recovery action based on attempt count
	action := rm.determineRecoveryAction(status.AttemptCount)
	
	// Calculate backoff delay
	backoffDelay := rm.calculateBackoffDelay(status.AttemptCount)
	if backoffDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDelay):
		}
	}

	// Create recovery attempt
	attempt := RecoveryAttempt{
		Timestamp: time.Now(),
		Action:    action,
	}

	// Perform the recovery action
	start := time.Now()
	err := rm.executeRecoveryAction(ctx, microvm, action)
	attempt.Duration = time.Since(start)

	if err != nil {
		attempt.Success = false
		attempt.Error = err.Error()
	} else {
		attempt.Success = true
	}

	// Update recovery status
	rm.mu.Lock()
	status.AttemptCount++
	status.LastAttempt = &attempt
	status.Attempts = append(status.Attempts, attempt)

	if attempt.Success {
		if cb, exists := rm.circuitBreakers[microvm.ID]; exists {
			cb.RecordSuccess()
		}
		status.RecoveryStarted = time.Time{}
		status.NextAttemptAt = time.Time{}
	} else {
		if cb, exists := rm.circuitBreakers[microvm.ID]; exists {
			if cb.RecordFailure() {
				fmt.Printf("🔌 Recovery circuit breaker OPENED for microVM %s after %d failures (cooldown: %v)\n",
					microvm.ID, cb.MaxFailures, cb.CooldownPeriod)
			}
		}
		if status.AttemptCount < status.MaxAttempts {
			// Schedule next attempt
			nextBackoff := rm.calculateBackoffDelay(status.AttemptCount)
			status.NextAttemptAt = time.Now().Add(nextBackoff)
		}
	}
	rm.mu.Unlock()

	if attempt.Success {
		metrics.IncCounter(metrics.RecoveryAttempts, map[string]string{
			"vm_id": microvm.ID, "action": string(action), "result": "success",
		})
		fmt.Printf("Recovery successful for microVM %s using action %s\n", microvm.ID, action)
	} else {
		metrics.IncCounter(metrics.RecoveryAttempts, map[string]string{
			"vm_id": microvm.ID, "action": string(action), "result": "failure",
		})
		fmt.Printf("Recovery attempt %d failed for microVM %s: %v\n", status.AttemptCount, microvm.ID, err)
	}
}

// determineRecoveryAction determines the appropriate recovery action based on attempt count
func (rm *RecoveryManager) determineRecoveryAction(attemptCount int) RecoveryAction {
	switch attemptCount {
	case 0:
		// First attempt: try simple restart
		return RecoveryActionRestart
	case 1:
		// Second attempt: try recreating the microVM
		return RecoveryActionRecreate
	case 2:
		// Third attempt: try migration to different node (if available)
		return RecoveryActionMigrate
	default:
		// Final attempt: mark for manual intervention
		return RecoveryActionManual
	}
}

// executeRecoveryAction executes the specified recovery action
func (rm *RecoveryManager) executeRecoveryAction(ctx context.Context, microvm *models.MicroVMInstance, action RecoveryAction) error {
	switch action {
	case RecoveryActionRestart:
		return rm.executeRestart(ctx, microvm)
	case RecoveryActionRecreate:
		return rm.executeRecreate(ctx, microvm)
	case RecoveryActionMigrate:
		return rm.executeMigrate(ctx, microvm)
	case RecoveryActionManual:
		return rm.executeManualIntervention(ctx, microvm)
	default:
		return fmt.Errorf("unknown recovery action: %s", action)
	}
}

// executeRestart attempts to restart the microVM
func (rm *RecoveryManager) executeRestart(ctx context.Context, microvm *models.MicroVMInstance) error {
	// Try to stop the microVM gracefully first
	if err := rm.microvmManager.StopMicroVM(ctx, microvm, false); err != nil {
		// If graceful stop fails, force stop
		if err := rm.microvmManager.StopMicroVM(ctx, microvm, true); err != nil {
			return fmt.Errorf("failed to stop microVM for restart: %w", err)
		}
	}

	// Wait a moment for cleanup
	time.Sleep(2 * time.Second)

	// Start the microVM again
	if err := rm.microvmManager.StartMicroVM(ctx, microvm); err != nil {
		return fmt.Errorf("failed to start microVM after restart: %w", err)
	}

	// Verify the microVM is running
	status, err := rm.microvmManager.GetMicroVMStatus(ctx, microvm.ID)
	if err != nil {
		return fmt.Errorf("failed to verify microVM status after restart: %w", err)
	}

	if status != models.MicroVMStatusRunning {
		return fmt.Errorf("microVM status is %s after restart, expected running", status)
	}

	return nil
}

// executeRecreate attempts to recreate the microVM
func (rm *RecoveryManager) executeRecreate(ctx context.Context, microvm *models.MicroVMInstance) error {
	// Safety first: snapshot the disk before anything destructive.
	// The snapshot name is recorded so an operator can roll back if recreate fails.
	if snapName, err := rm.snapshotForRecovery(microvm); err != nil {
		fmt.Printf("Warning: pre-recovery snapshot failed for microVM %s: %v (proceeding anyway)\n", microvm.ID, err)
	} else if snapName != "" {
		fmt.Printf("Pre-recovery snapshot taken for microVM %s: %s\n", microvm.ID, snapName)
		if microvm.Metadata.Labels == nil {
			microvm.Metadata.Labels = map[string]string{}
		}
		microvm.Metadata.Labels["last_recovery_snapshot"] = snapName
	}

	// Delete the existing microVM
	if err := rm.microvmManager.DeleteMicroVM(ctx, microvm); err != nil {
		return fmt.Errorf("failed to delete microVM for recreation: %w", err)
	}

	// Wait for cleanup
	time.Sleep(5 * time.Second)

	// Create the microVM again
	if err := rm.microvmManager.CreateMicroVM(ctx, microvm); err != nil {
		return fmt.Errorf("failed to recreate microVM: %w", err)
	}

	return nil
}

// snapshotForRecovery takes a ZFS snapshot of the VM disk before destructive
// recovery. Returns the snapshot name, or "" when there is nothing to snapshot
// (non-ZFS host or no existing zvol). Never fails fatally — callers log and proceed.
func (rm *RecoveryManager) snapshotForRecovery(microvm *models.MicroVMInstance) (string, error) {
	if os.Getenv("MICROVM_USE_ZFS") != "true" {
		return "", fmt.Errorf("ZFS not enabled, skipping snapshot")
	}

	pool := os.Getenv("MICROVM_ZFS_POOL")
	if pool == "" {
		pool = "default"
	}
	dataset := os.Getenv("MICROVM_ZFS_DATASET")
	if dataset == "" {
		dataset = fmt.Sprintf("%s/vmlet-microvms", pool)
	}

	zvolPath := fmt.Sprintf("/dev/zvol/%s/%s", dataset, microvm.ID)
	if _, err := os.Stat(zvolPath); err != nil {
		return "", fmt.Errorf("no existing zvol at %s, nothing to snapshot", zvolPath)
	}

	snapName := fmt.Sprintf("%s/%s@pre-recovery-%d", dataset, microvm.ID, time.Now().Unix())
	if out, err := exec.Command("zfs", "snapshot", snapName).CombinedOutput(); err != nil {
		return "", fmt.Errorf("zfs snapshot failed: %v (output: %s)", err, string(out))
	}
	return snapName, nil
}

// executeMigrate attempts to migrate the microVM to a different node
func (rm *RecoveryManager) executeMigrate(ctx context.Context, microvm *models.MicroVMInstance) error {
	// For now, migration is not implemented as it requires coordination
	// with the resource manager to find alternative nodes
	// This would be a complex operation involving:
	// 1. Finding a suitable alternative node
	// 2. Transferring storage (if not shared)
	// 3. Recreating the microVM on the new node
	// 4. Updating network configuration
	
	return fmt.Errorf("migration recovery not yet implemented")
}

// executeManualIntervention marks the microVM for manual intervention
func (rm *RecoveryManager) executeManualIntervention(ctx context.Context, microvm *models.MicroVMInstance) error {
	// Mark the microVM as requiring manual intervention
	// In a real implementation, this would:
	// 1. Send alerts to administrators
	// 2. Create support tickets
	// 3. Update monitoring systems
	// 4. Possibly disable the microVM to prevent further issues
	
	fmt.Printf("MANUAL INTERVENTION REQUIRED: MicroVM %s (Customer: %s) requires manual recovery\n",
		microvm.ID, microvm.Metadata.CustomerID)
	
	// Disable automatic recovery for this microVM
	rm.mu.Lock()
	if status, exists := rm.recoveryStatus[microvm.ID]; exists {
		status.RecoveryDisabled = true
	}
	rm.mu.Unlock()
	
	return fmt.Errorf("microVM marked for manual intervention")
}

// calculateBackoffDelay calculates the backoff delay for recovery attempts
func (rm *RecoveryManager) calculateBackoffDelay(attemptCount int) time.Duration {
	if attemptCount == 0 {
		return 0 // No delay for first attempt
	}

	// Exponential backoff: base * 2^(attempt-1)
	multiplier := 1 << (attemptCount - 1) // 2^(attempt-1)
	delay := rm.config.RecoveryBackoffBase * time.Duration(multiplier)
	
	// Cap the maximum delay at 10 minutes
	maxDelay := 10 * time.Minute
	if delay > maxDelay {
		delay = maxDelay
	}
	
	return delay
}

// GetRecoveryStatus returns the recovery status for a microVM
func (rm *RecoveryManager) GetRecoveryStatus(microvmID string) (*RecoveryStatus, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	status, exists := rm.recoveryStatus[microvmID]
	if !exists {
		return nil, fmt.Errorf("no recovery status found for microVM %s", microvmID)
	}

	// Return a copy to avoid race conditions
	statusCopy := *status
	statusCopy.Attempts = make([]RecoveryAttempt, len(status.Attempts))
	copy(statusCopy.Attempts, status.Attempts)
	if status.LastAttempt != nil {
		lastAttemptCopy := *status.LastAttempt
		statusCopy.LastAttempt = &lastAttemptCopy
	}

	return &statusCopy, nil
}

// EnableRecovery enables automatic recovery for a microVM
func (rm *RecoveryManager) EnableRecovery(microvmID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	status, exists := rm.recoveryStatus[microvmID]
	if !exists {
		status = &RecoveryStatus{
			MicroVMID:   microvmID,
			MaxAttempts: rm.config.MaxRecoveryAttempts,
		}
		rm.recoveryStatus[microvmID] = status
	}

	status.RecoveryDisabled = false
	return nil
}

// DisableRecovery disables automatic recovery for a microVM
func (rm *RecoveryManager) DisableRecovery(microvmID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	status, exists := rm.recoveryStatus[microvmID]
	if !exists {
		status = &RecoveryStatus{
			MicroVMID:        microvmID,
			MaxAttempts:      rm.config.MaxRecoveryAttempts,
			RecoveryDisabled: true,
		}
		rm.recoveryStatus[microvmID] = status
	} else {
		status.RecoveryDisabled = true
	}

	// Cancel any active recovery
	if cancel, exists := rm.activeRecovery[microvmID]; exists {
		cancel()
		delete(rm.activeRecovery, microvmID)
	}

	return nil
}

// ResetRecoveryStatus resets the recovery status for a microVM
func (rm *RecoveryManager) ResetRecoveryStatus(microvmID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Cancel any active recovery
	if cancel, exists := rm.activeRecovery[microvmID]; exists {
		cancel()
		delete(rm.activeRecovery, microvmID)
	}

	// Reset recovery status
	status := &RecoveryStatus{
		MicroVMID:   microvmID,
		MaxAttempts: rm.config.MaxRecoveryAttempts,
	}
	rm.recoveryStatus[microvmID] = status

	return nil
}

// RemoveRecoveryStatus removes recovery status tracking for a microVM
func (rm *RecoveryManager) RemoveRecoveryStatus(microvmID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Cancel any active recovery
	if cancel, exists := rm.activeRecovery[microvmID]; exists {
		cancel()
		delete(rm.activeRecovery, microvmID)
	}

	delete(rm.recoveryStatus, microvmID)
	delete(rm.circuitBreakers, microvmID)
}

// GetRecoveryStatistics returns statistics about recovery operations
func (rm *RecoveryManager) GetRecoveryStatistics() map[string]interface{} {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	totalMicroVMs := len(rm.recoveryStatus)
	recoveringCount := 0
	disabledCount := 0
	successfulRecoveries := 0
	failedRecoveries := 0
	totalAttempts := 0

	for _, status := range rm.recoveryStatus {
		if status.IsRecovering {
			recoveringCount++
		}
		if status.RecoveryDisabled {
			disabledCount++
		}
		
		totalAttempts += status.AttemptCount
		
		for _, attempt := range status.Attempts {
			if attempt.Success {
				successfulRecoveries++
			} else {
				failedRecoveries++
			}
		}
	}

	var successRate float64
	if totalAttempts > 0 {
		successRate = float64(successfulRecoveries) / float64(totalAttempts) * 100
	}

	return map[string]interface{}{
		"total_microvms":         totalMicroVMs,
		"currently_recovering":   recoveringCount,
		"recovery_disabled":      disabledCount,
		"total_attempts":         totalAttempts,
		"successful_recoveries":  successfulRecoveries,
		"failed_recoveries":      failedRecoveries,
		"success_rate_percent":   successRate,
	}
}