package validation

import (
	"context"
	"fmt"
	"io"
	"time"
)

// ExampleUsage demonstrates how to use the boot validation system
func ExampleUsage() {
	// Create a boot validator with default settings
	validator := NewBootValidator()
	
	// Or create with custom timeouts
	// validator := NewBootValidatorWithConfig(90*time.Second, 45*time.Second)
	
	// Example SSH configuration
	sshConfig := &SSHConfig{
		Host:       "192.168.100.10",
		Port:       2222,
		Username:   "alpine",
		PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
		Timeout:    10 * time.Second,
	}
	
	// Example validation configuration
	config := &ValidationConfig{
		BootTimeout: 60 * time.Second,
		SSHTimeout:  30 * time.Second,
		SSHConfig:   sshConfig,
		VMIP:        "192.168.100.10",
	}
	
	// Simulate console reader (in real usage, this would be from Firecracker)
	var consoleReader io.Reader
	
	// Perform comprehensive VM boot validation
	ctx := context.Background()
	result, err := validator.ValidateVMBoot(ctx, consoleReader, config)
	if err != nil {
		fmt.Printf("Validation failed: %v\n", err)
		return
	}
	
	if result.Success {
		fmt.Println("VM boot validation successful!")
		fmt.Printf("Boot completed in: %v\n", result.BootResult.Duration)
		fmt.Printf("SSH ready in: %v\n", result.SSHResult.Duration)
		fmt.Printf("Total validation time: %v\n", result.TotalDuration)
	} else {
		fmt.Printf("VM boot validation failed: %s\n", result.ErrorMessage)
		
		// Check individual components
		if result.BootResult != nil && !result.BootResult.Success {
			fmt.Printf("Boot failure: %s\n", result.BootResult.ErrorMessage)
		}
		
		if result.SSHResult != nil && !result.SSHResult.Success {
			fmt.Printf("SSH failure: %s\n", result.SSHResult.ErrorMessage)
		}
		
		if result.NetworkResult != nil && !result.NetworkResult.Success {
			fmt.Printf("Network failure: %s\n", result.NetworkResult.ErrorMessage)
		}
	}
	
	// Example of individual component usage
	
	// 1. Monitor boot process only
	bootResult, err := validator.MonitorBoot(ctx, consoleReader)
	if err != nil {
		fmt.Printf("Boot monitoring failed: %v\n", err)
	} else if bootResult.Success {
		fmt.Println("Boot completed successfully")
	}
	
	// 2. Validate SSH access only
	sshResult, err := validator.ValidateSSHAccess(ctx, sshConfig)
	if err != nil {
		fmt.Printf("SSH validation failed: %v\n", err)
	} else if sshResult.Success {
		fmt.Println("SSH access validated")
	}
	
	// 3. Check VM health
	health, err := validator.CheckVMHealth(ctx, "192.168.100.10", sshConfig)
	if err != nil {
		fmt.Printf("Health check failed: %v\n", err)
	} else {
		fmt.Printf("Overall health: %s\n", health.Overall)
		fmt.Printf("SSH health: %s\n", health.SSH)
		fmt.Printf("Network health: %s\n", health.Network)
		fmt.Printf("Filesystem health: %s\n", health.Filesystem)
	}
	
	// 4. Get boot progress during monitoring
	progress := validator.GetBootProgress()
	fmt.Printf("Boot progress: %.1f%%\n", progress)
	
	// 5. Reset validator for new session
	validator.Reset()
}

// ExampleBootStageMonitoring shows how to monitor specific boot stages
func ExampleBootStageMonitoring() {
	_ = NewConsoleMonitor(60 * time.Second)
	
	// In a real scenario, you would read from Firecracker console
	// This example shows what the console output might look like
	_ = `
[    0.000000] Linux version 5.10.0
[    0.123456] Kernel boot complete
[    1.234567] init: starting service networking
[    2.345678] eth0: link up, DHCP bound to 192.168.100.10
[    3.456789] sshd: started successfully on port 22
[    4.567890] cloud-init: finished initialization
[    5.678901] Alpine Linux 3.18 login:
`
	
	// Monitor the boot process
	_ = context.Background()
	// result, err := monitor.MonitorBoot(ctx, strings.NewReader(consoleOutput))
	
	fmt.Println("Boot stages that will be monitored:")
	fmt.Println("1. kernel_boot - Linux kernel initialization")
	fmt.Println("2. init_system - Init system startup")
	fmt.Println("3. network_ready - Network interface configuration")
	fmt.Println("4. ssh_service - SSH daemon startup")
	fmt.Println("5. cloud_init_complete - Cloud-init completion")
	fmt.Println("6. boot_complete - Login prompt available")
}

// ExampleSSHConnectivity shows SSH connectivity validation
func ExampleSSHConnectivity() {
	checker := NewSSHChecker(5, 3*time.Second)
	
	sshConfig := &SSHConfig{
		Host:       "192.168.100.10",
		Port:       2222,
		Username:   "alpine",
		PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
		Timeout:    10 * time.Second,
	}
	
	ctx := context.Background()
	
	// Wait for SSH to become ready (30-second timeout as per requirements)
	result, err := checker.WaitForSSHReady(ctx, sshConfig)
	if err != nil {
		fmt.Printf("SSH ready check failed: %v\n", err)
		return
	}
	
	if result.Success {
		fmt.Println("SSH is ready!")
		fmt.Printf("Connected at: %v\n", result.ConnectedAt)
		fmt.Printf("Shell access: %v\n", result.ShellAccess)
		fmt.Printf("Command test: %v\n", result.CommandTest)
		
		// Generate SSH connection command
		command := checker.GetSSHConnectionCommand(sshConfig, "/path/to/private/key.pem")
		fmt.Printf("Connect with: %s\n", command)
	} else {
		fmt.Printf("SSH not ready: %s\n", result.ErrorMessage)
	}
}

// ExampleNetworkTesting shows network connectivity testing
func ExampleNetworkTesting() {
	tester := NewNetworkTester(10 * time.Second)
	ctx := context.Background()
	
	// Test basic network connectivity
	result, err := tester.TestNetworkConnectivity(ctx, "192.168.100.10")
	if err != nil {
		fmt.Printf("Network test failed: %v\n", err)
		return
	}
	
	fmt.Printf("Network connectivity results:\n")
	fmt.Printf("- Ping test: %v\n", result.PingTest)
	fmt.Printf("- SSH port access: %v\n", result.SSHPortAccess)
	fmt.Printf("- DNS resolution: %v\n", result.DNSResolution)
	fmt.Printf("- Internet access: %v\n", result.InternetAccess)
	
	// Test internal VM connectivity (requires SSH access)
	sshConfig := &SSHConfig{
		Host:       "192.168.100.10",
		Port:       2222,
		Username:   "alpine",
		PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
		Timeout:    10 * time.Second,
	}
	
	internalResult, err := tester.TestVMInternalConnectivity(ctx, sshConfig)
	if err != nil {
		fmt.Printf("Internal connectivity test failed: %v\n", err)
		return
	}
	
	fmt.Printf("Internal connectivity results:\n")
	fmt.Printf("- DNS resolution: %v\n", internalResult.DNSResolution)
	fmt.Printf("- Internet access: %v\n", internalResult.InternetAccess)
	fmt.Printf("- VM ping test: %v\n", internalResult.PingTest)
}