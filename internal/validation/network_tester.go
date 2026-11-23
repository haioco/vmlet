package validation

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// NetworkTestResult contains the result of network connectivity tests
type NetworkTestResult struct {
	Success           bool
	PingTest          bool
	DNSResolution     bool
	InternetAccess    bool
	SSHPortAccess     bool
	ErrorMessage      string
	TestDetails       map[string]interface{}
	Duration          time.Duration
	TestedAt          time.Time
}

// HealthState represents the health status of a component
type HealthState string

const (
	HealthStateHealthy   HealthState = "healthy"
	HealthStateUnhealthy HealthState = "unhealthy"
	HealthStateUnknown   HealthState = "unknown"
)

// HealthStatus contains comprehensive health information
type HealthStatus struct {
	Overall     HealthState
	SSH         HealthState
	Network     HealthState
	Filesystem  HealthState
	LastCheck   time.Time
	Details     map[string]interface{}
}

// NetworkTester performs comprehensive network connectivity testing
type NetworkTester struct {
	timeout       time.Duration
	pingCount     int
	testHosts     []string
	dnsServers    []string
}

// NewNetworkTester creates a new network connectivity tester
func NewNetworkTester(timeout time.Duration) *NetworkTester {
	return &NetworkTester{
		timeout:   timeout,
		pingCount: 3,
		testHosts: []string{
			"8.8.8.8",      // Google DNS
			"1.1.1.1",      // Cloudflare DNS
			"google.com",   // Domain name test
		},
		dnsServers: []string{
			"8.8.8.8",
			"8.8.4.4",
			"1.1.1.1",
		},
	}
}

// TestNetworkConnectivity performs comprehensive network connectivity tests
func (nt *NetworkTester) TestNetworkConnectivity(ctx context.Context, vmIP string) (*NetworkTestResult, error) {
	startTime := time.Now()
	
	result := &NetworkTestResult{
		Success:     false,
		TestDetails: make(map[string]interface{}),
		TestedAt:    time.Now(),
	}

	// Test ICMP ping connectivity
	pingResult := nt.testPingConnectivity(ctx, vmIP)
	result.PingTest = pingResult
	result.TestDetails["ping"] = pingResult

	// Test SSH port accessibility
	sshResult := nt.testSSHPortAccess(ctx, vmIP, 22)
	result.SSHPortAccess = sshResult
	result.TestDetails["ssh_port"] = sshResult

	// For VM internal tests, we need SSH access
	// These tests would be performed inside the VM via SSH
	result.DNSResolution = true // Placeholder - would need SSH to test
	result.InternetAccess = true // Placeholder - would need SSH to test
	result.TestDetails["dns"] = "requires_ssh_access"
	result.TestDetails["internet"] = "requires_ssh_access"

	// Overall success if ping and SSH port are accessible
	result.Success = result.PingTest && result.SSHPortAccess
	
	if !result.Success {
		result.ErrorMessage = "Network connectivity tests failed"
	}

	result.Duration = time.Since(startTime)
	return result, nil
}

// TestVMInternalConnectivity tests connectivity from inside the VM via SSH
func (nt *NetworkTester) TestVMInternalConnectivity(ctx context.Context, sshConfig *SSHConfig) (*NetworkTestResult, error) {
	startTime := time.Now()
	
	result := &NetworkTestResult{
		Success:     false,
		TestDetails: make(map[string]interface{}),
		TestedAt:    time.Now(),
	}

	// Parse private key
	signer, err := ssh.ParsePrivateKey([]byte(sshConfig.PrivateKey))
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("Failed to parse private key: %v", err)
		result.Duration = time.Since(startTime)
		return result, nil
	}

	// Create SSH client config
	clientConfig := &ssh.ClientConfig{
		User: sshConfig.Username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshConfig.Timeout,
	}

	address := fmt.Sprintf("%s:%d", sshConfig.Host, sshConfig.Port)
	client, err := ssh.Dial("tcp", address, clientConfig)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("SSH connection failed: %v", err)
		result.Duration = time.Since(startTime)
		return result, nil
	}
	defer client.Close()

	// Test DNS resolution
	dnsResult := nt.testDNSResolutionViaSSH(client)
	result.DNSResolution = dnsResult
	result.TestDetails["dns_resolution"] = dnsResult

	// Test internet connectivity
	internetResult := nt.testInternetConnectivityViaSSH(client)
	result.InternetAccess = internetResult
	result.TestDetails["internet_access"] = internetResult

	// Test ping from VM
	pingResult := nt.testPingViaSSH(client, "8.8.8.8")
	result.PingTest = pingResult
	result.TestDetails["vm_ping"] = pingResult

	result.Success = result.DNSResolution && result.InternetAccess && result.PingTest
	
	if !result.Success {
		result.ErrorMessage = "VM internal connectivity tests failed"
	}

	result.Duration = time.Since(startTime)
	return result, nil
}

// testPingConnectivity tests ICMP ping connectivity to the VM
func (nt *NetworkTester) testPingConnectivity(ctx context.Context, target string) bool {
	ctx, cancel := context.WithTimeout(ctx, nt.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ping", "-c", fmt.Sprintf("%d", nt.pingCount), "-W", "2", target)
	err := cmd.Run()
	return err == nil
}

// testSSHPortAccess tests if SSH port is accessible
func (nt *NetworkTester) testSSHPortAccess(ctx context.Context, host string, port int) bool {
	address := fmt.Sprintf("%s:%d", host, port)
	
	dialer := &net.Dialer{
		Timeout: nt.timeout,
	}
	
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// testDNSResolutionViaSSH tests DNS resolution from inside the VM
func (nt *NetworkTester) testDNSResolutionViaSSH(client *ssh.Client) bool {
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()

	// Test DNS resolution
	output, err := session.Output("nslookup google.com")
	if err != nil {
		return false
	}

	return strings.Contains(string(output), "Address:")
}

// testInternetConnectivityViaSSH tests internet connectivity from inside the VM
func (nt *NetworkTester) testInternetConnectivityViaSSH(client *ssh.Client) bool {
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()

	// Test HTTP connectivity
	output, err := session.Output("wget -q --spider http://google.com && echo 'success' || echo 'failed'")
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(output)) == "success"
}

// testPingViaSSH tests ping from inside the VM
func (nt *NetworkTester) testPingViaSSH(client *ssh.Client, target string) bool {
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()

	cmd := fmt.Sprintf("ping -c 2 -W 2 %s", target)
	err = session.Run(cmd)
	return err == nil
}

// CheckVMHealth performs comprehensive VM health checks
func (nt *NetworkTester) CheckVMHealth(ctx context.Context, vmIP string, sshConfig *SSHConfig) (*HealthStatus, error) {
	health := &HealthStatus{
		Overall:   HealthStateUnknown,
		SSH:       HealthStateUnknown,
		Network:   HealthStateUnknown,
		Filesystem: HealthStateUnknown,
		LastCheck: time.Now(),
		Details:   make(map[string]interface{}),
	}

	// Test network connectivity
	networkResult, err := nt.TestNetworkConnectivity(ctx, vmIP)
	if err != nil {
		health.Network = HealthStateUnhealthy
		health.Details["network_error"] = err.Error()
	} else if networkResult.Success {
		health.Network = HealthStateHealthy
		health.Details["network"] = networkResult.TestDetails
	} else {
		health.Network = HealthStateUnhealthy
		health.Details["network_error"] = networkResult.ErrorMessage
	}

	// Test SSH connectivity
	sshChecker := NewSSHChecker(2, 500*time.Millisecond) // Faster retries for health checks
	sshResult, err := sshChecker.ValidateSSHAccess(ctx, sshConfig)
	if err != nil {
		health.SSH = HealthStateUnhealthy
		health.Details["ssh_error"] = err.Error()
	} else if sshResult.Success {
		health.SSH = HealthStateHealthy
		health.Details["ssh"] = map[string]interface{}{
			"connected_at":  sshResult.ConnectedAt,
			"shell_access":  sshResult.ShellAccess,
			"command_test":  sshResult.CommandTest,
		}
	} else {
		health.SSH = HealthStateUnhealthy
		health.Details["ssh_error"] = sshResult.ErrorMessage
	}

	// Test VM internal connectivity if SSH is available
	if health.SSH == HealthStateHealthy {
		internalResult, err := nt.TestVMInternalConnectivity(ctx, sshConfig)
		if err != nil {
			health.Details["internal_connectivity_error"] = err.Error()
		} else {
			health.Details["internal_connectivity"] = internalResult.TestDetails
		}

		// Test filesystem (basic check via SSH)
		health.Filesystem = nt.testFilesystemViaSSH(sshConfig)
		health.Details["filesystem"] = health.Filesystem
	}

	// Determine overall health
	if health.SSH == HealthStateHealthy && health.Network == HealthStateHealthy {
		health.Overall = HealthStateHealthy
	} else if health.SSH == HealthStateUnhealthy || health.Network == HealthStateUnhealthy {
		health.Overall = HealthStateUnhealthy
	} else {
		health.Overall = HealthStateUnknown
	}

	return health, nil
}

// testFilesystemViaSSH performs basic filesystem health check via SSH
func (nt *NetworkTester) testFilesystemViaSSH(sshConfig *SSHConfig) HealthState {
	// Parse private key
	signer, err := ssh.ParsePrivateKey([]byte(sshConfig.PrivateKey))
	if err != nil {
		return HealthStateUnhealthy
	}

	// Create SSH client config
	clientConfig := &ssh.ClientConfig{
		User: sshConfig.Username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshConfig.Timeout,
	}

	address := fmt.Sprintf("%s:%d", sshConfig.Host, sshConfig.Port)
	client, err := ssh.Dial("tcp", address, clientConfig)
	if err != nil {
		return HealthStateUnhealthy
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return HealthStateUnhealthy
	}
	defer session.Close()

	// Test basic filesystem operations
	err = session.Run("df / && ls /tmp && touch /tmp/health-check && rm /tmp/health-check")
	if err != nil {
		return HealthStateUnhealthy
	}

	return HealthStateHealthy
}