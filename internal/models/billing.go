package models

import (
	"sync"
	"time"
)

// BillingState represents the billing state of a VM
type BillingState string

const (
	BillingStateInactive    BillingState = "inactive"     // VM not started yet or stopped
	BillingStateActive      BillingState = "active"       // VM is running, billing is active
	BillingStatePaused      BillingState = "paused"       // VM is paused (billing depends on config)
	BillingStateError       BillingState = "error"        // VM in error state (no billing)
	BillingStateNotBillable BillingState = "not_billable" // VM not configured for billing
)

// BillingInfo contains billing configuration for a VM instance
// This is stored with the VM and set during creation
type BillingInfo struct {
	PricePerHour   float64    `json:"price_per_hour"`       // Price in specified currency (e.g., Tomans)
	Currency       string     `json:"currency"`             // Currency code (default: "IRR" for Tomans)
	BillingEnabled bool       `json:"billing_enabled"`      // Whether billing is enabled for this VM
	CreatedAt      time.Time  `json:"created_at"`           // When VM was created
	StartedAt      *time.Time `json:"started_at,omitempty"` // When billing started (first VM start)
}

// BillingRecord tracks the billing state for a single VM
// This is the in-memory record that tracks usage
type BillingRecord struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name,omitempty"`

	// Pricing configuration
	PricePerHour float64 `json:"price_per_hour"`
	Currency     string  `json:"currency"`

	// Time tracking
	CreatedAt      time.Time  `json:"created_at"`
	FirstStartedAt *time.Time `json:"first_started_at,omitempty"` // First time VM was started
	LastStartedAt  *time.Time `json:"last_started_at,omitempty"`  // Last time VM was started/resumed
	LastStoppedAt  *time.Time `json:"last_stopped_at,omitempty"`  // Last time VM was stopped/paused

	// Accumulated totals (persisted)
	TotalRunningSeconds int64   `json:"total_running_seconds"` // Total seconds VM has been running
	TotalCost           float64 `json:"total_cost"`            // Total cost accumulated

	// Checkpoint tracking
	LastCheckpoint      time.Time `json:"last_checkpoint"`       // Last checkpoint time
	CostAtCheckpoint    float64   `json:"cost_at_checkpoint"`    // Cost at last checkpoint
	SecondsAtCheckpoint int64     `json:"seconds_at_checkpoint"` // Running seconds at last checkpoint

	// Current state
	State BillingState `json:"state"`

	// Mutex for thread-safe updates
	mu sync.RWMutex `json:"-"`
}

// BillingReport is the response for billing API queries
type BillingReport struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name,omitempty"`

	// Pricing configuration
	PricePerHour float64 `json:"price_per_hour"`
	Currency     string  `json:"currency"`

	// Time tracking
	CreatedAt      time.Time  `json:"created_at"`
	FirstStartedAt *time.Time `json:"first_started_at,omitempty"`
	CurrentTime    time.Time  `json:"current_time"`

	// Total usage (from beginning)
	TotalRunningHours float64 `json:"total_running_hours"`
	TotalCost         float64 `json:"total_cost"`

	// Since last checkpoint
	LastCheckpoint       time.Time `json:"last_checkpoint"`
	HoursSinceCheckpoint float64   `json:"hours_since_checkpoint"`
	CostSinceCheckpoint  float64   `json:"cost_since_checkpoint"`

	// Current state
	CurrentStatus   string `json:"current_status"`
	IsBillingActive bool   `json:"is_billing_active"`
}

// NewBillingRecord creates a new billing record for a VM
func NewBillingRecord(vmID, vmName string, pricePerHour float64, currency string) *BillingRecord {
	now := time.Now()
	return &BillingRecord{
		VMID:                vmID,
		VMName:              vmName,
		PricePerHour:        pricePerHour,
		Currency:            currency,
		CreatedAt:           now,
		TotalRunningSeconds: 0,
		TotalCost:           0,
		LastCheckpoint:      now,
		CostAtCheckpoint:    0,
		SecondsAtCheckpoint: 0,
		State:               BillingStateInactive,
	}
}

// StartBilling marks the VM as running and starts billing
func (r *BillingRecord) StartBilling() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Set first started time if not set
	if r.FirstStartedAt == nil {
		r.FirstStartedAt = &now
	}

	r.LastStartedAt = &now
	r.State = BillingStateActive
}

// StopBilling marks the VM as stopped and accumulates the running time
func (r *BillingRecord) StopBilling() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != BillingStateActive && r.State != BillingStatePaused {
		return // Not billing, nothing to stop
	}

	now := time.Now()

	// Calculate running time since last start
	if r.LastStartedAt != nil {
		elapsed := now.Sub(*r.LastStartedAt)
		r.TotalRunningSeconds += int64(elapsed.Seconds())
		r.TotalCost = r.calculateCost(r.TotalRunningSeconds)
	}

	r.LastStoppedAt = &now
	r.State = BillingStateInactive
}

// PauseBilling marks the VM as paused
func (r *BillingRecord) PauseBilling(billWhilePaused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != BillingStateActive {
		return
	}

	now := time.Now()

	if !billWhilePaused {
		// Stop accumulating time
		if r.LastStartedAt != nil {
			elapsed := now.Sub(*r.LastStartedAt)
			r.TotalRunningSeconds += int64(elapsed.Seconds())
			r.TotalCost = r.calculateCost(r.TotalRunningSeconds)
		}
		r.LastStoppedAt = &now
	}

	r.State = BillingStatePaused
}

// ResumeBilling resumes billing after pause
func (r *BillingRecord) ResumeBilling() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != BillingStatePaused {
		return
	}

	now := time.Now()
	r.LastStartedAt = &now
	r.State = BillingStateActive
}

// SetError marks the VM as in error state (no billing)
func (r *BillingRecord) SetError() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Accumulate any running time before error
	if r.State == BillingStateActive && r.LastStartedAt != nil {
		now := time.Now()
		elapsed := now.Sub(*r.LastStartedAt)
		r.TotalRunningSeconds += int64(elapsed.Seconds())
		r.TotalCost = r.calculateCost(r.TotalRunningSeconds)
		r.LastStoppedAt = &now
	}

	r.State = BillingStateError
}

// CreateCheckpoint saves the current state as checkpoint
func (r *BillingRecord) CreateCheckpoint() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	currentSeconds := r.TotalRunningSeconds

	// If currently running, add time since last start
	if r.State == BillingStateActive && r.LastStartedAt != nil {
		elapsed := now.Sub(*r.LastStartedAt)
		currentSeconds += int64(elapsed.Seconds())
	}

	r.LastCheckpoint = now
	r.SecondsAtCheckpoint = currentSeconds
	r.CostAtCheckpoint = r.calculateCost(currentSeconds)
}

// GetReport generates a billing report
func (r *BillingRecord) GetReport() *BillingReport {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	currentSeconds := r.TotalRunningSeconds

	// If currently running, add time since last start
	if r.State == BillingStateActive && r.LastStartedAt != nil {
		elapsed := now.Sub(*r.LastStartedAt)
		currentSeconds += int64(elapsed.Seconds())
	}

	currentCost := r.calculateCost(currentSeconds)

	// Calculate since last checkpoint
	secondsSinceCheckpoint := currentSeconds - r.SecondsAtCheckpoint
	costSinceCheckpoint := currentCost - r.CostAtCheckpoint

	return &BillingReport{
		VMID:                 r.VMID,
		VMName:               r.VMName,
		PricePerHour:         r.PricePerHour,
		Currency:             r.Currency,
		CreatedAt:            r.CreatedAt,
		FirstStartedAt:       r.FirstStartedAt,
		CurrentTime:          now,
		TotalRunningHours:    float64(currentSeconds) / 3600.0,
		TotalCost:            currentCost,
		LastCheckpoint:       r.LastCheckpoint,
		HoursSinceCheckpoint: float64(secondsSinceCheckpoint) / 3600.0,
		CostSinceCheckpoint:  costSinceCheckpoint,
		CurrentStatus:        string(r.State),
		IsBillingActive:      r.State == BillingStateActive,
	}
}

// calculateCost calculates cost from seconds
func (r *BillingRecord) calculateCost(seconds int64) float64 {
	hours := float64(seconds) / 3600.0
	return hours * r.PricePerHour
}

// GetCurrentCost returns the current total cost (including running time)
func (r *BillingRecord) GetCurrentCost() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	currentSeconds := r.TotalRunningSeconds

	if r.State == BillingStateActive && r.LastStartedAt != nil {
		elapsed := time.Since(*r.LastStartedAt)
		currentSeconds += int64(elapsed.Seconds())
	}

	return r.calculateCost(currentSeconds)
}

// GetTotalRunningHours returns total running hours
func (r *BillingRecord) GetTotalRunningHours() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	currentSeconds := r.TotalRunningSeconds

	if r.State == BillingStateActive && r.LastStartedAt != nil {
		elapsed := time.Since(*r.LastStartedAt)
		currentSeconds += int64(elapsed.Seconds())
	}

	return float64(currentSeconds) / 3600.0
}
