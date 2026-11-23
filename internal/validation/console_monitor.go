package validation

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// BootStage represents a stage in the VM boot process
type BootStage struct {
	Name      string
	Completed bool
	Timestamp time.Time
	Duration  time.Duration
	Pattern   *regexp.Regexp
}

// BootResult contains the result of boot monitoring
type BootResult struct {
	Success      bool
	Duration     time.Duration
	BootStages   []BootStage
	ErrorMessage string
	ConsoleLog   string
	CompletedAt  time.Time
}

// ConsoleMonitor monitors VM boot progress through console output
type ConsoleMonitor struct {
	bootStages []BootStage
	timeout    time.Duration
	mu         sync.RWMutex
	logBuffer  strings.Builder
}

// NewConsoleMonitor creates a new console monitor with predefined boot stages
func NewConsoleMonitor(timeout time.Duration) *ConsoleMonitor {
	stages := []BootStage{
		{
			Name:    "kernel_boot",
			Pattern: regexp.MustCompile(`Linux version \d+\.\d+`),
		},
		{
			Name:    "init_system",
			Pattern: regexp.MustCompile(`init: starting service|systemd.*started|OpenRC.*started`),
		},
		{
			Name:    "network_ready",
			Pattern: regexp.MustCompile(`network interface.*up|eth0.*up|DHCP.*bound`),
		},
		{
			Name:    "ssh_service",
			Pattern: regexp.MustCompile(`sshd.*started|SSH daemon started|dropbear.*started`),
		},
		{
			Name:    "cloud_init_complete",
			Pattern: regexp.MustCompile(`cloud-init.*finished|Cloud-init.*finished`),
		},
		{
			Name:    "boot_complete",
			Pattern: regexp.MustCompile(`login:|Welcome to|Alpine.*login`),
		},
	}

	return &ConsoleMonitor{
		bootStages: stages,
		timeout:    timeout,
	}
}

// MonitorBoot monitors the VM boot process through console output
func (cm *ConsoleMonitor) MonitorBoot(ctx context.Context, consoleReader io.Reader) (*BootResult, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	startTime := time.Now()
	scanner := bufio.NewScanner(consoleReader)
	
	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, cm.timeout)
	defer cancel()

	// Channel to signal completion
	done := make(chan *BootResult, 1)
	
	go func() {
		result := &BootResult{
			Success:    false,
			BootStages: make([]BootStage, len(cm.bootStages)),
			CompletedAt: time.Now(),
		}
		
		// Copy initial boot stages
		copy(result.BootStages, cm.bootStages)
		
		for scanner.Scan() {
			line := scanner.Text()
			cm.logBuffer.WriteString(line + "\n")
			
			// Check each boot stage
			for i := range result.BootStages {
				if !result.BootStages[i].Completed && result.BootStages[i].Pattern.MatchString(line) {
					result.BootStages[i].Completed = true
					result.BootStages[i].Timestamp = time.Now()
					result.BootStages[i].Duration = time.Since(startTime)
				}
			}
			
			// Check if all critical stages are complete
			if cm.allCriticalStagesComplete(result.BootStages) {
				result.Success = true
				result.Duration = time.Since(startTime)
				result.ConsoleLog = cm.logBuffer.String()
				done <- result
				return
			}
		}
		
		// If we reach here, scanning ended without completion
		if err := scanner.Err(); err != nil {
			result.ErrorMessage = fmt.Sprintf("Console scanning error: %v", err)
		} else {
			result.ErrorMessage = "Console output ended before boot completion"
		}
		
		result.Duration = time.Since(startTime)
		result.ConsoleLog = cm.logBuffer.String()
		done <- result
	}()

	select {
	case result := <-done:
		return result, nil
	case <-timeoutCtx.Done():
		return &BootResult{
			Success:      false,
			Duration:     time.Since(startTime),
			BootStages:   cm.getCurrentStages(),
			ErrorMessage: fmt.Sprintf("Boot timeout after %v", cm.timeout),
			ConsoleLog:   cm.logBuffer.String(),
			CompletedAt:  time.Now(),
		}, nil
	}
}

// allCriticalStagesComplete checks if all critical boot stages are complete
func (cm *ConsoleMonitor) allCriticalStagesComplete(stages []BootStage) bool {
	// Consider kernel_boot, init_system, and boot_complete as critical
	criticalStages := []string{"kernel_boot", "init_system", "boot_complete"}
	
	for _, criticalStage := range criticalStages {
		found := false
		for _, stage := range stages {
			if stage.Name == criticalStage && stage.Completed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// getCurrentStages returns a copy of current boot stages
func (cm *ConsoleMonitor) getCurrentStages() []BootStage {
	stages := make([]BootStage, len(cm.bootStages))
	copy(stages, cm.bootStages)
	return stages
}

// GetBootProgress returns the current boot progress as a percentage
func (cm *ConsoleMonitor) GetBootProgress() float64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	
	completed := 0
	for _, stage := range cm.bootStages {
		if stage.Completed {
			completed++
		}
	}
	
	return float64(completed) / float64(len(cm.bootStages)) * 100
}

// Reset resets the console monitor for a new boot session
func (cm *ConsoleMonitor) Reset() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	
	for i := range cm.bootStages {
		cm.bootStages[i].Completed = false
		cm.bootStages[i].Timestamp = time.Time{}
		cm.bootStages[i].Duration = 0
	}
	
	cm.logBuffer.Reset()
}