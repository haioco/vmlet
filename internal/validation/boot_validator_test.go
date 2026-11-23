package validation

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewBootValidator(t *testing.T) {
	validator := NewBootValidator()
	
	if validator == nil {
		t.Fatal("NewBootValidator returned nil")
	}
	
	if validator.consoleMonitor == nil {
		t.Error("Console monitor should not be nil")
	}
	
	if validator.sshChecker == nil {
		t.Error("SSH checker should not be nil")
	}
	
	if validator.networkTester == nil {
		t.Error("Network tester should not be nil")
	}
	
	if validator.bootTimeout != 60*time.Second {
		t.Errorf("Expected boot timeout 60s, got %v", validator.bootTimeout)
	}
	
	if validator.sshTimeout != 30*time.Second {
		t.Errorf("Expected SSH timeout 30s, got %v", validator.sshTimeout)
	}
}

func TestNewBootValidatorWithConfig(t *testing.T) {
	bootTimeout := 45 * time.Second
	sshTimeout := 20 * time.Second
	
	validator := NewBootValidatorWithConfig(bootTimeout, sshTimeout)
	
	if validator == nil {
		t.Fatal("NewBootValidatorWithConfig returned nil")
	}
	
	if validator.bootTimeout != bootTimeout {
		t.Errorf("Expected boot timeout %v, got %v", bootTimeout, validator.bootTimeout)
	}
	
	if validator.sshTimeout != sshTimeout {
		t.Errorf("Expected SSH timeout %v, got %v", sshTimeout, validator.sshTimeout)
	}
}

func TestBootValidator_ValidateVMBoot_SuccessfulBoot(t *testing.T) {
	validator := NewBootValidator()
	
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
	
	config := &ValidationConfig{
		BootTimeout: 10 * time.Second,
		SSHTimeout:  5 * time.Second,
		VMIP:        "127.0.0.1", // Use localhost for testing
	}
	
	result, err := validator.ValidateVMBoot(ctx, reader, config)
	
	if err != nil {
		t.Fatalf("ValidateVMBoot failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	if result.BootResult == nil {
		t.Error("Boot result should not be nil")
	}
	
	if !result.BootResult.Success {
		t.Errorf("Expected successful boot, got failure: %s", result.BootResult.ErrorMessage)
	}
	
	if result.TotalDuration == 0 {
		t.Error("Total duration should be greater than 0")
	}
	
	if result.ValidatedAt.IsZero() {
		t.Error("ValidatedAt should not be zero")
	}
}

func TestBootValidator_ValidateVMBoot_BootFailure(t *testing.T) {
	validator := NewBootValidator()
	
	// Simulate failed boot console output (incomplete)
	consoleOutput := `
Linux version 5.10.0
Starting kernel...
`
	
	reader := strings.NewReader(consoleOutput)
	ctx := context.Background()
	
	config := &ValidationConfig{
		BootTimeout: 1 * time.Second, // Short timeout to force failure
		SSHTimeout:  5 * time.Second,
	}
	
	result, err := validator.ValidateVMBoot(ctx, reader, config)
	
	if err != nil {
		t.Fatalf("ValidateVMBoot failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	if result.Success {
		t.Error("Expected validation to fail due to boot failure")
	}
	
	if result.BootResult == nil {
		t.Error("Boot result should not be nil")
	}
	
	if result.BootResult.Success {
		t.Error("Expected boot result to indicate failure")
	}
	
	if result.ErrorMessage == "" {
		t.Error("Expected error message for boot failure")
	}
}

func TestBootValidator_ValidateVMBoot_WithSSHConfig(t *testing.T) {
	// Create validator with fast timeouts for testing
	validator := NewBootValidatorWithConfig(5*time.Second, 1*time.Second)
	validator.sshChecker = NewSSHChecker(2, 100*time.Millisecond) // Fast retries
	
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Generate test private key
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	config := &ValidationConfig{
		BootTimeout: 5 * time.Second,
		SSHTimeout:  1 * time.Second, // Shorter timeout for testing
		SSHConfig: &SSHConfig{
			Host:       "192.0.2.1", // Non-routable IP for testing
			Port:       22,
			Username:   "test",
			PrivateKey: privateKey,
			Timeout:    500 * time.Millisecond, // Very short timeout
		},
		VMIP: "192.0.2.1",
	}
	
	result, err := validator.ValidateVMBoot(ctx, reader, config)
	
	if err != nil {
		t.Fatalf("ValidateVMBoot failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	// Boot should succeed but SSH should fail
	if result.BootResult == nil || !result.BootResult.Success {
		t.Error("Boot should have succeeded")
	}
	
	if result.SSHResult == nil {
		t.Error("SSH result should not be nil")
	}
	
	if result.SSHResult.Success {
		t.Error("SSH should have failed with non-routable IP")
	}
	
	if result.NetworkResult == nil {
		t.Error("Network result should not be nil")
	}
	
	// Overall validation should fail due to SSH failure
	if result.Success {
		t.Error("Overall validation should fail due to SSH failure")
	}
}

func TestBootValidator_MonitorBoot(t *testing.T) {
	validator := NewBootValidator()
	
	consoleOutput := `
Linux version 5.10.0
init: starting service networking
Alpine Linux login:
`
	
	reader := strings.NewReader(consoleOutput)
	ctx := context.Background()
	
	result, err := validator.MonitorBoot(ctx, reader)
	
	if err != nil {
		t.Fatalf("MonitorBoot failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	if !result.Success {
		t.Errorf("Expected successful boot monitoring, got failure: %s", result.ErrorMessage)
	}
}

func TestBootValidator_ValidateSSHAccess(t *testing.T) {
	// Create validator with fast timeouts for testing
	validator := NewBootValidatorWithConfig(5*time.Second, 1*time.Second)
	validator.sshChecker = NewSSHChecker(2, 100*time.Millisecond) // Fast retries
	
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	config := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP
		Port:       22,
		Username:   "test",
		PrivateKey: privateKey,
		Timeout:    500 * time.Millisecond, // Short timeout for testing
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	
	result, err := validator.ValidateSSHAccess(ctx, config)
	
	if err != nil {
		t.Fatalf("ValidateSSHAccess failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	if result.Success {
		t.Error("Expected SSH validation to fail with non-routable IP")
	}
}

func TestBootValidator_CheckVMHealth(t *testing.T) {
	// Create validator with fast timeouts for testing
	validator := NewBootValidatorWithConfig(5*time.Second, 1*time.Second)
	validator.sshChecker = NewSSHChecker(2, 100*time.Millisecond) // Fast retries
	
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	sshConfig := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP
		Port:       22,
		Username:   "test",
		PrivateKey: privateKey,
		Timeout:    500 * time.Millisecond, // Short timeout for testing
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	health, err := validator.CheckVMHealth(ctx, "192.0.2.1", sshConfig)
	
	if err != nil {
		t.Fatalf("CheckVMHealth failed: %v", err)
	}
	
	if health == nil {
		t.Fatal("Health status should not be nil")
	}
	
	if health.Overall == HealthStateHealthy {
		t.Error("Expected overall health to not be healthy with non-routable IP")
	}
}

func TestBootValidator_GetBootProgress(t *testing.T) {
	validator := NewBootValidator()
	
	// Initially, progress should be 0
	progress := validator.GetBootProgress()
	if progress != 0 {
		t.Errorf("Expected initial progress 0, got %f", progress)
	}
}

func TestBootValidator_Reset(t *testing.T) {
	validator := NewBootValidator()
	
	// Reset should not panic
	validator.Reset()
	
	// Progress should be 0 after reset
	progress := validator.GetBootProgress()
	if progress != 0 {
		t.Errorf("Expected progress 0 after reset, got %f", progress)
	}
}

func TestBootValidator_SetBootTimeout(t *testing.T) {
	validator := NewBootValidator()
	
	newTimeout := 90 * time.Second
	validator.SetBootTimeout(newTimeout)
	
	if validator.bootTimeout != newTimeout {
		t.Errorf("Expected boot timeout %v, got %v", newTimeout, validator.bootTimeout)
	}
}

func TestBootValidator_SetSSHTimeout(t *testing.T) {
	validator := NewBootValidator()
	
	newTimeout := 45 * time.Second
	validator.SetSSHTimeout(newTimeout)
	
	if validator.sshTimeout != newTimeout {
		t.Errorf("Expected SSH timeout %v, got %v", newTimeout, validator.sshTimeout)
	}
}

func TestValidationResult_Fields(t *testing.T) {
	result := &ValidationResult{
		Success:       true,
		BootResult:    &BootResult{Success: true},
		SSHResult:     &SSHResult{Success: true},
		NetworkResult: &NetworkTestResult{Success: true},
		HealthStatus:  &HealthStatus{Overall: HealthStateHealthy},
		ErrorMessage:  "",
		TotalDuration: 10 * time.Second,
		ValidatedAt:   time.Now(),
	}
	
	if !result.Success {
		t.Error("Expected Success to be true")
	}
	
	if result.BootResult == nil {
		t.Error("BootResult should not be nil")
	}
	
	if result.SSHResult == nil {
		t.Error("SSHResult should not be nil")
	}
	
	if result.NetworkResult == nil {
		t.Error("NetworkResult should not be nil")
	}
	
	if result.HealthStatus == nil {
		t.Error("HealthStatus should not be nil")
	}
	
	if result.TotalDuration != 10*time.Second {
		t.Errorf("Expected TotalDuration 10s, got %v", result.TotalDuration)
	}
	
	if result.ValidatedAt.IsZero() {
		t.Error("ValidatedAt should not be zero")
	}
}