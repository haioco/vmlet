package validation

import (
	"context"
	"testing"
	"time"
)

func TestNewNetworkTester(t *testing.T) {
	timeout := 15 * time.Second
	tester := NewNetworkTester(timeout)
	
	if tester == nil {
		t.Fatal("NewNetworkTester returned nil")
	}
	
	if tester.timeout != timeout {
		t.Errorf("Expected timeout %v, got %v", timeout, tester.timeout)
	}
	
	if tester.pingCount != 3 {
		t.Errorf("Expected pingCount 3, got %d", tester.pingCount)
	}
	
	if len(tester.testHosts) == 0 {
		t.Error("Test hosts should not be empty")
	}
	
	if len(tester.dnsServers) == 0 {
		t.Error("DNS servers should not be empty")
	}
	
	// Check that expected test hosts are present
	expectedHosts := []string{"8.8.8.8", "1.1.1.1", "google.com"}
	for _, expected := range expectedHosts {
		found := false
		for _, host := range tester.testHosts {
			if host == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected test host %s not found", expected)
		}
	}
}

func TestNetworkTester_TestNetworkConnectivity_InvalidIP(t *testing.T) {
	tester := NewNetworkTester(2 * time.Second)
	ctx := context.Background()
	
	// Test with non-routable IP
	result, err := tester.TestNetworkConnectivity(ctx, "192.0.2.1")
	
	if err != nil {
		t.Fatalf("TestNetworkConnectivity failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	if result.Success {
		t.Error("Expected network connectivity test to fail with non-routable IP")
	}
	
	if result.TestDetails == nil {
		t.Error("TestDetails should not be nil")
	}
	
	if result.Duration == 0 {
		t.Error("Duration should be greater than 0")
	}
	
	if result.TestedAt.IsZero() {
		t.Error("TestedAt should not be zero")
	}
}

func TestNetworkTester_TestNetworkConnectivity_LocalhostPing(t *testing.T) {
	tester := NewNetworkTester(5 * time.Second)
	ctx := context.Background()
	
	// Test with localhost (should be reachable)
	result, err := tester.TestNetworkConnectivity(ctx, "127.0.0.1")
	
	if err != nil {
		t.Fatalf("TestNetworkConnectivity failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	// Ping to localhost should succeed
	if !result.PingTest {
		t.Error("Expected ping test to succeed for localhost")
	}
	
	// SSH port test will likely fail (no SSH server on localhost:22 in test)
	// but that's expected in test environment
	
	if result.TestDetails == nil {
		t.Error("TestDetails should not be nil")
	}
	
	if _, exists := result.TestDetails["ping"]; !exists {
		t.Error("TestDetails should contain ping result")
	}
	
	if _, exists := result.TestDetails["ssh_port"]; !exists {
		t.Error("TestDetails should contain ssh_port result")
	}
}

func TestNetworkTester_CheckVMHealth_InvalidConfig(t *testing.T) {
	tester := NewNetworkTester(1 * time.Second) // Shorter timeout for testing
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	// Test with invalid SSH config
	sshConfig := &SSHConfig{
		Host:       "192.0.2.1", // Non-routable IP
		Port:       22,
		Username:   "test",
		PrivateKey: "invalid-key",
		Timeout:    500 * time.Millisecond, // Short timeout
	}
	
	health, err := tester.CheckVMHealth(ctx, "192.0.2.1", sshConfig)
	
	if err != nil {
		t.Fatalf("CheckVMHealth failed: %v", err)
	}
	
	if health == nil {
		t.Fatal("Health status should not be nil")
	}
	
	if health.Overall == HealthStateHealthy {
		t.Error("Expected overall health to not be healthy with invalid config")
	}
	
	if health.SSH == HealthStateHealthy {
		t.Error("Expected SSH health to not be healthy with invalid config")
	}
	
	if health.Network == HealthStateHealthy {
		t.Error("Expected network health to not be healthy with non-routable IP")
	}
	
	if health.LastCheck.IsZero() {
		t.Error("LastCheck should not be zero")
	}
	
	if health.Details == nil {
		t.Error("Details should not be nil")
	}
}

func TestHealthState_Constants(t *testing.T) {
	if HealthStateHealthy != "healthy" {
		t.Errorf("Expected HealthStateHealthy to be 'healthy', got %s", HealthStateHealthy)
	}
	
	if HealthStateUnhealthy != "unhealthy" {
		t.Errorf("Expected HealthStateUnhealthy to be 'unhealthy', got %s", HealthStateUnhealthy)
	}
	
	if HealthStateUnknown != "unknown" {
		t.Errorf("Expected HealthStateUnknown to be 'unknown', got %s", HealthStateUnknown)
	}
}

func TestNetworkTestResult_Fields(t *testing.T) {
	result := &NetworkTestResult{
		Success:        true,
		PingTest:       true,
		DNSResolution:  true,
		InternetAccess: true,
		SSHPortAccess:  true,
		ErrorMessage:   "",
		TestDetails:    make(map[string]interface{}),
		Duration:       5 * time.Second,
		TestedAt:       time.Now(),
	}
	
	if !result.Success {
		t.Error("Expected Success to be true")
	}
	
	if !result.PingTest {
		t.Error("Expected PingTest to be true")
	}
	
	if !result.DNSResolution {
		t.Error("Expected DNSResolution to be true")
	}
	
	if !result.InternetAccess {
		t.Error("Expected InternetAccess to be true")
	}
	
	if !result.SSHPortAccess {
		t.Error("Expected SSHPortAccess to be true")
	}
	
	if result.Duration != 5*time.Second {
		t.Errorf("Expected Duration 5s, got %v", result.Duration)
	}
	
	if result.TestedAt.IsZero() {
		t.Error("TestedAt should not be zero")
	}
	
	if result.TestDetails == nil {
		t.Error("TestDetails should not be nil")
	}
}

func TestHealthStatus_Fields(t *testing.T) {
	health := &HealthStatus{
		Overall:    HealthStateHealthy,
		SSH:        HealthStateHealthy,
		Network:    HealthStateHealthy,
		Filesystem: HealthStateHealthy,
		LastCheck:  time.Now(),
		Details:    make(map[string]interface{}),
	}
	
	if health.Overall != HealthStateHealthy {
		t.Errorf("Expected Overall to be %s, got %s", HealthStateHealthy, health.Overall)
	}
	
	if health.SSH != HealthStateHealthy {
		t.Errorf("Expected SSH to be %s, got %s", HealthStateHealthy, health.SSH)
	}
	
	if health.Network != HealthStateHealthy {
		t.Errorf("Expected Network to be %s, got %s", HealthStateHealthy, health.Network)
	}
	
	if health.Filesystem != HealthStateHealthy {
		t.Errorf("Expected Filesystem to be %s, got %s", HealthStateHealthy, health.Filesystem)
	}
	
	if health.LastCheck.IsZero() {
		t.Error("LastCheck should not be zero")
	}
	
	if health.Details == nil {
		t.Error("Details should not be nil")
	}
}

func TestNetworkTester_ContextCancellation(t *testing.T) {
	tester := NewNetworkTester(30 * time.Second) // Long timeout
	
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	
	// Test with a slow/non-responsive target
	result, err := tester.TestNetworkConnectivity(ctx, "192.0.2.1")
	
	if err != nil {
		t.Fatalf("TestNetworkConnectivity failed: %v", err)
	}
	
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	
	// Should complete quickly due to context cancellation
	if result.Duration > 1*time.Second {
		t.Errorf("Should have completed quickly due to context cancellation, took %v", result.Duration)
	}
}