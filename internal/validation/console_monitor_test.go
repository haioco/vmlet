package validation

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewConsoleMonitor(t *testing.T) {
	timeout := 30 * time.Second
	monitor := NewConsoleMonitor(timeout)
	
	if monitor == nil {
		t.Fatal("NewConsoleMonitor returned nil")
	}
	
	if monitor.timeout != timeout {
		t.Errorf("Expected timeout %v, got %v", timeout, monitor.timeout)
	}
	
	if len(monitor.bootStages) == 0 {
		t.Error("Boot stages should not be empty")
	}
	
	// Check that all expected boot stages are present
	expectedStages := []string{"kernel_boot", "init_system", "network_ready", "ssh_service", "cloud_init_complete", "boot_complete"}
	for _, expected := range expectedStages {
		found := false
		for _, stage := range monitor.bootStages {
			if stage.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected boot stage %s not found", expected)
		}
	}
}

func TestConsoleMonitor_MonitorBoot_Success(t *testing.T) {
	monitor := NewConsoleMonitor(10 * time.Second)
	
	// Simulate successful boot console output
	consoleOutput := `
Linux version 5.10.0
init: starting service networking
eth0: link up
sshd: started successfully
cloud-init: finished initialization
Alpine Linux login:
`
	
	reader := strings.NewReader(consoleOutput)
	ctx := context.Background()
	
	result, err := monitor.MonitorBoot(ctx, reader)
	
	if err != nil {
		t.Fatalf("MonitorBoot failed: %v", err)
	}
	
	if !result.Success {
		t.Errorf("Expected successful boot, got failure: %s", result.ErrorMessage)
	}
	
	if result.Duration == 0 {
		t.Error("Boot duration should be greater than 0")
	}
	
	if result.ConsoleLog == "" {
		t.Error("Console log should not be empty")
	}
	
	// Check that critical stages were detected
	criticalStages := []string{"kernel_boot", "init_system", "boot_complete"}
	for _, critical := range criticalStages {
		found := false
		for _, stage := range result.BootStages {
			if stage.Name == critical && stage.Completed {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Critical boot stage %s was not completed", critical)
		}
	}
}

func TestConsoleMonitor_MonitorBoot_Timeout(t *testing.T) {
	monitor := NewConsoleMonitor(100 * time.Millisecond) // Very short timeout
	
	// Simulate incomplete boot output that will cause timeout
	consoleOutput := `Linux version 5.10.0`
	
	reader := strings.NewReader(consoleOutput)
	ctx := context.Background()
	
	result, err := monitor.MonitorBoot(ctx, reader)
	
	if err != nil {
		t.Fatalf("MonitorBoot failed: %v", err)
	}
	
	if result.Success {
		t.Error("Expected boot failure due to timeout, got success")
	}
	
	// Accept either timeout or console ended error message
	if !strings.Contains(result.ErrorMessage, "timeout") && !strings.Contains(result.ErrorMessage, "Console output ended") {
		t.Errorf("Expected timeout or console ended error message, got: %s", result.ErrorMessage)
	}
}

func TestConsoleMonitor_MonitorBoot_PartialBoot(t *testing.T) {
	monitor := NewConsoleMonitor(5 * time.Second)
	
	// Simulate partial boot (missing critical stages)
	consoleOutput := `
Linux version 5.10.0
init: starting service networking
`
	
	reader := strings.NewReader(consoleOutput)
	ctx := context.Background()
	
	result, err := monitor.MonitorBoot(ctx, reader)
	
	if err != nil {
		t.Fatalf("MonitorBoot failed: %v", err)
	}
	
	if result.Success {
		t.Error("Expected boot failure due to incomplete stages, got success")
	}
	
	// Check that some stages were completed but not all critical ones
	kernelCompleted := false
	bootCompleted := false
	
	for _, stage := range result.BootStages {
		if stage.Name == "kernel_boot" && stage.Completed {
			kernelCompleted = true
		}
		if stage.Name == "boot_complete" && stage.Completed {
			bootCompleted = true
		}
	}
	
	if !kernelCompleted {
		t.Error("Kernel boot stage should have been completed")
	}
	
	if bootCompleted {
		t.Error("Boot complete stage should not have been completed")
	}
}

func TestConsoleMonitor_GetBootProgress(t *testing.T) {
	monitor := NewConsoleMonitor(30 * time.Second)
	
	// Initially, progress should be 0
	progress := monitor.GetBootProgress()
	if progress != 0 {
		t.Errorf("Expected initial progress 0, got %f", progress)
	}
	
	// Simulate some boot stages completing
	monitor.bootStages[0].Completed = true // kernel_boot
	monitor.bootStages[1].Completed = true // init_system
	
	progress = monitor.GetBootProgress()
	expected := float64(2) / float64(len(monitor.bootStages)) * 100
	if progress != expected {
		t.Errorf("Expected progress %f, got %f", expected, progress)
	}
}

func TestConsoleMonitor_Reset(t *testing.T) {
	monitor := NewConsoleMonitor(30 * time.Second)
	
	// Simulate some completed stages
	monitor.bootStages[0].Completed = true
	monitor.bootStages[0].Timestamp = time.Now()
	monitor.bootStages[0].Duration = 5 * time.Second
	monitor.logBuffer.WriteString("test log")
	
	// Reset the monitor
	monitor.Reset()
	
	// Check that all stages are reset
	for _, stage := range monitor.bootStages {
		if stage.Completed {
			t.Error("Boot stage should not be completed after reset")
		}
		if !stage.Timestamp.IsZero() {
			t.Error("Boot stage timestamp should be zero after reset")
		}
		if stage.Duration != 0 {
			t.Error("Boot stage duration should be zero after reset")
		}
	}
	
	// Check that log buffer is reset
	if monitor.logBuffer.Len() != 0 {
		t.Error("Log buffer should be empty after reset")
	}
}

func TestConsoleMonitor_ContextCancellation(t *testing.T) {
	monitor := NewConsoleMonitor(30 * time.Second)
	
	// Create a context that will be cancelled quickly
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	
	consoleOutput := `Linux version 5.10.0`
	reader := strings.NewReader(consoleOutput)
	
	result, err := monitor.MonitorBoot(ctx, reader)
	
	// Should handle context cancellation gracefully
	// Either return error or result with timeout
	if err != nil || (result != nil && !result.Success) {
		// This is expected behavior
	} else {
		t.Error("Expected either error or unsuccessful result due to context cancellation")
	}
}