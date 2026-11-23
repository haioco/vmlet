package validation

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestNewSSHChecker(t *testing.T) {
	maxRetries := 5
	retryInterval := 2 * time.Second
	
	checker := NewSSHChecker(maxRetries, retryInterval)
	
	if checker == nil {
		t.Fatal("NewSSHChecker returned nil")
	}
	
	if checker.maxRetries != maxRetries {
		t.Errorf("Expected maxRetries %d, got %d", maxRetries, checker.maxRetries)
	}
	
	if checker.retryInterval != retryInterval {
		t.Errorf("Expected retryInterval %v, got %v", retryInterval, checker.retryInterval)
	}
}

func TestSSHChecker_ValidateSSHAccess_InvalidKey(t *testing.T) {
	checker := NewSSHChecker(3, 1*time.Second)
	
	config := &SSHConfig{
		Host:       "localhost",
		Port:       22,
		Username:   "test",
		PrivateKey: "invalid-key",
		Timeout:    5 * time.Second,
	}
	
	ctx := context.Background()
	result, err := checker.ValidateSSHAccess(ctx, config)
	
	if err != nil {
		t.Fatalf("ValidateSSHAccess failed: %v", err)
	}
	
	if result.Success {
		t.Error("Expected SSH validation to fail with invalid key")
	}
	
	if result.ErrorMessage == "" {
		t.Error("Expected error message for invalid key")
	}
	
	if result.Duration == 0 {
		t.Error("Duration should be greater than 0")
	}
}

func TestSSHChecker_ValidateSSHAccess_ConnectionFailure(t *testing.T) {
	checker := NewSSHChecker(2, 100*time.Millisecond) // Fast retries for testing
	
	// Generate a valid private key for testing
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	config := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP for testing
		Port:       22,
		Username:   "test",
		PrivateKey: privateKey,
		Timeout:    1 * time.Second, // Short timeout
	}
	
	ctx := context.Background()
	result, err := checker.ValidateSSHAccess(ctx, config)
	
	if err != nil {
		t.Fatalf("ValidateSSHAccess failed: %v", err)
	}
	
	if result.Success {
		t.Error("Expected SSH validation to fail with unreachable host")
	}
	
	if result.ErrorMessage == "" {
		t.Error("Expected error message for connection failure")
	}
	
	if !result.ServiceReady {
		// This is expected for connection failure
	}
}

func TestSSHChecker_WaitForSSHReady_Timeout(t *testing.T) {
	checker := NewSSHChecker(2, 100*time.Millisecond)
	
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	config := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP
		Port:       22,
		Username:   "test",
		PrivateKey: privateKey,
		Timeout:    1 * time.Second,
	}
	
	ctx := context.Background()
	result, err := checker.WaitForSSHReady(ctx, config)
	
	if err != nil {
		t.Fatalf("WaitForSSHReady failed: %v", err)
	}
	
	if result.Success {
		t.Error("Expected SSH ready check to fail")
	}
	
	// Should complete within reasonable time (30 seconds + some buffer)
	if result.Duration > 35*time.Second {
		t.Errorf("WaitForSSHReady took too long: %v", result.Duration)
	}
}

func TestSSHChecker_GetSSHConnectionCommand(t *testing.T) {
	checker := NewSSHChecker(3, 1*time.Second)
	
	config := &SSHConfig{
		Host:     "192.168.1.100",
		Port:     2222,
		Username: "alpine",
	}
	
	privateKeyPath := "/path/to/key.pem"
	
	command := checker.GetSSHConnectionCommand(config, privateKeyPath)
	expected := "ssh -i /path/to/key.pem -p 2222 alpine@192.168.1.100"
	
	if command != expected {
		t.Errorf("Expected command %s, got %s", expected, command)
	}
}

func TestSSHChecker_ContextCancellation(t *testing.T) {
	checker := NewSSHChecker(2, 100*time.Millisecond) // Fewer retries, faster intervals
	
	privateKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate test private key: %v", err)
	}
	
	config := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP
		Port:       22,
		Username:   "test",
		PrivateKey: privateKey,
		Timeout:    1 * time.Second,
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	
	result, err := checker.ValidateSSHAccess(ctx, config)
	
	// Should handle context cancellation or return unsuccessful result
	if err != nil || (result != nil && !result.Success) {
		// This is expected behavior
	} else {
		t.Error("Expected either error or unsuccessful result due to context cancellation")
	}
	
	if result != nil && result.Duration > 3*time.Second {
		t.Errorf("Should have completed reasonably quickly, took %v", result.Duration)
	}
}

// generateTestPrivateKey generates a valid RSA private key for testing
func generateTestPrivateKey() (string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	
	return string(pem.EncodeToMemory(privateKeyPEM)), nil
}

func TestSSHResult_Fields(t *testing.T) {
	result := &SSHResult{
		Success:      true,
		ConnectedAt:  time.Now(),
		Duration:     5 * time.Second,
		ErrorMessage: "",
		ShellAccess:  true,
		CommandTest:  true,
		ServiceReady: true,
	}
	
	if !result.Success {
		t.Error("Expected Success to be true")
	}
	
	if result.ConnectedAt.IsZero() {
		t.Error("ConnectedAt should not be zero")
	}
	
	if result.Duration != 5*time.Second {
		t.Errorf("Expected Duration 5s, got %v", result.Duration)
	}
	
	if !result.ShellAccess {
		t.Error("Expected ShellAccess to be true")
	}
	
	if !result.CommandTest {
		t.Error("Expected CommandTest to be true")
	}
	
	if !result.ServiceReady {
		t.Error("Expected ServiceReady to be true")
	}
}