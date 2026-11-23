package validation

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHConfig contains SSH connection configuration
type SSHConfig struct {
	Host       string
	Port       int
	Username   string
	PrivateKey string
	Timeout    time.Duration
}

// SSHResult contains the result of SSH connectivity check
type SSHResult struct {
	Success        bool
	ConnectedAt    time.Time
	Duration       time.Duration
	ErrorMessage   string
	ShellAccess    bool
	CommandTest    bool
	ServiceReady   bool
}

// SSHChecker validates SSH connectivity and access
type SSHChecker struct {
	maxRetries    int
	retryInterval time.Duration
}

// NewSSHChecker creates a new SSH connectivity checker
func NewSSHChecker(maxRetries int, retryInterval time.Duration) *SSHChecker {
	return &SSHChecker{
		maxRetries:    maxRetries,
		retryInterval: retryInterval,
	}
}

// ValidateSSHAccess validates SSH connectivity with key-based authentication
func (sc *SSHChecker) ValidateSSHAccess(ctx context.Context, config *SSHConfig) (*SSHResult, error) {
	startTime := time.Now()
	
	result := &SSHResult{
		Success: false,
	}

	// Parse private key
	signer, err := ssh.ParsePrivateKey([]byte(config.PrivateKey))
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("Failed to parse private key: %v", err)
		result.Duration = time.Since(startTime)
		return result, nil
	}

	// Create SSH client config
	sshConfig := &ssh.ClientConfig{
		User: config.Username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // For testing purposes
		Timeout:         config.Timeout,
	}

	address := fmt.Sprintf("%s:%d", config.Host, config.Port)

	// Retry logic for SSH connection
	for attempt := 0; attempt < sc.maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			result.ErrorMessage = "Context cancelled during SSH validation"
			result.Duration = time.Since(startTime)
			return result, ctx.Err()
		default:
		}

		// Test TCP connectivity first
		if !sc.testTCPConnectivity(address, config.Timeout) {
			if attempt < sc.maxRetries-1 {
				time.Sleep(sc.retryInterval)
				continue
			}
			result.ErrorMessage = fmt.Sprintf("TCP connection to %s failed after %d attempts", address, sc.maxRetries)
			result.Duration = time.Since(startTime)
			return result, nil
		}

		// Attempt SSH connection
		client, err := ssh.Dial("tcp", address, sshConfig)
		if err != nil {
			if attempt < sc.maxRetries-1 {
				time.Sleep(sc.retryInterval)
				continue
			}
			result.ErrorMessage = fmt.Sprintf("SSH connection failed after %d attempts: %v", sc.maxRetries, err)
			result.Duration = time.Since(startTime)
			return result, nil
		}

		// Connection successful
		result.Success = true
		result.ConnectedAt = time.Now()
		result.ServiceReady = true

		// Test shell access
		shellResult := sc.testShellAccess(client)
		result.ShellAccess = shellResult

		// Test basic command execution
		commandResult := sc.testCommandExecution(client)
		result.CommandTest = commandResult

		client.Close()
		result.Duration = time.Since(startTime)
		return result, nil
	}

	result.ErrorMessage = fmt.Sprintf("SSH validation failed after %d attempts", sc.maxRetries)
	result.Duration = time.Since(startTime)
	return result, nil
}

// testTCPConnectivity tests basic TCP connectivity to the SSH port
func (sc *SSHChecker) testTCPConnectivity(address string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// testShellAccess tests if we can create an SSH session
func (sc *SSHChecker) testShellAccess(client *ssh.Client) bool {
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()

	// Request a pseudo-terminal
	err = session.RequestPty("xterm", 80, 24, ssh.TerminalModes{})
	if err != nil {
		return false
	}

	return true
}

// testCommandExecution tests if we can execute basic commands
func (sc *SSHChecker) testCommandExecution(client *ssh.Client) bool {
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()

	// Execute a simple command
	output, err := session.Output("echo 'ssh-test-success'")
	if err != nil {
		return false
	}

	return string(output) == "ssh-test-success\n"
}

// WaitForSSHReady waits for SSH service to become ready within the specified timeout
func (sc *SSHChecker) WaitForSSHReady(ctx context.Context, config *SSHConfig) (*SSHResult, error) {
	// Create a context with 30-second timeout as per requirements
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Use shorter retry intervals for faster detection
	originalRetryInterval := sc.retryInterval
	sc.retryInterval = 2 * time.Second
	defer func() {
		sc.retryInterval = originalRetryInterval
	}()

	return sc.ValidateSSHAccess(timeoutCtx, config)
}

// CheckSSHServiceStatus checks if SSH service is running and accessible
func (sc *SSHChecker) CheckSSHServiceStatus(config *SSHConfig) (*SSHResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	return sc.ValidateSSHAccess(ctx, config)
}

// GetSSHConnectionCommand generates an SSH connection command string
func (sc *SSHChecker) GetSSHConnectionCommand(config *SSHConfig, privateKeyPath string) string {
	return fmt.Sprintf("ssh -i %s -p %d %s@%s", 
		privateKeyPath, config.Port, config.Username, config.Host)
}