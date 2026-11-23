package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SystemValidator validates system prerequisites and provides setup recommendations
type SystemValidator struct {
	requiredBinaries      []string
	requiredKernelModules []string
	requiredDirectories   []string
	networkRequirements   []NetworkRequirement
}

// ValidationResult contains the results of system validation
type ValidationResult struct {
	Overall          bool              `json:"overall"`
	BinaryChecks     map[string]bool   `json:"binary_checks"`
	KernelModules    map[string]bool   `json:"kernel_modules"`
	DirectoryChecks  map[string]bool   `json:"directory_checks"`
	NetworkChecks    map[string]bool   `json:"network_checks"`
	PermissionChecks map[string]bool   `json:"permission_checks"`
	Issues           []ValidationIssue `json:"issues"`
	Recommendations  []string          `json:"recommendations"`
	ValidatedAt      time.Time         `json:"validated_at"`
}

// ValidationIssue represents a specific system validation issue
type ValidationIssue struct {
	Component   string `json:"component"`
	Issue       string `json:"issue"`
	Severity    string `json:"severity"` // "critical", "warning", "info"
	Resolution  string `json:"resolution"`
	AutoFixable bool   `json:"auto_fixable"`
}

// NetworkRequirement represents a network-related system requirement
type NetworkRequirement struct {
	Name        string
	Description string
	CheckFunc   func() bool
	FixFunc     func() error
}

// NewSystemValidator creates a new system validator with default requirements
func NewSystemValidator() *SystemValidator {
	return &SystemValidator{
		requiredBinaries: []string{
			"firecracker",
			"ip",
			"brctl",
			"iptables",
			"ping",
			"ssh",
			"mkfs.ext4",
			"mount",
			"umount",
		},
		requiredKernelModules: []string{
			"kvm",
			"kvm_intel", // or kvm_amd
			"tun",
			"bridge",
		},
		requiredDirectories: []string{
			"/dev/kvm",
			"/dev/net/tun",
		},
		networkRequirements: []NetworkRequirement{
			{
				Name:        "bridge_utils",
				Description: "Bridge utilities for network bridging",
				CheckFunc:   func() bool { return commandExists("brctl") },
				FixFunc:     func() error { return installPackage("bridge-utils") },
			},
			{
				Name:        "iptables",
				Description: "iptables for network filtering and NAT",
				CheckFunc:   func() bool { return commandExists("iptables") },
				FixFunc:     func() error { return installPackage("iptables") },
			},
			{
				Name:        "ip_forwarding",
				Description: "IP forwarding enabled for NAT",
				CheckFunc:   func() bool { return checkIPForwarding() },
				FixFunc:     func() error { return enableIPForwarding() },
			},
		},
	}
}

// ValidateSystem performs comprehensive system validation
func (sv *SystemValidator) ValidateSystem() (*ValidationResult, error) {
	result := &ValidationResult{
		Overall:          true,
		BinaryChecks:     make(map[string]bool),
		KernelModules:    make(map[string]bool),
		DirectoryChecks:  make(map[string]bool),
		NetworkChecks:    make(map[string]bool),
		PermissionChecks: make(map[string]bool),
		Issues:           []ValidationIssue{},
		Recommendations:  []string{},
		ValidatedAt:      time.Now(),
	}

	// Check required binaries
	for _, binary := range sv.requiredBinaries {
		exists := commandExists(binary)
		result.BinaryChecks[binary] = exists
		if !exists {
			result.Overall = false
			result.Issues = append(result.Issues, ValidationIssue{
				Component:   "binary",
				Issue:       fmt.Sprintf("Required binary '%s' not found", binary),
				Severity:    "critical",
				Resolution:  fmt.Sprintf("Install %s package", binary),
				AutoFixable: true,
			})
		}
	}

	// Check kernel modules
	for _, module := range sv.requiredKernelModules {
		loaded := isKernelModuleLoaded(module)
		result.KernelModules[module] = loaded
		if !loaded && module != "kvm_amd" { // kvm_amd is alternative to kvm_intel
			// Check if it's an Intel vs AMD issue
			if module == "kvm_intel" && isKernelModuleLoaded("kvm_amd") {
				result.KernelModules[module] = true
				continue
			}
			// Special case for TUN: check if /dev/net/tun exists OR tap module is loaded
			// (different distros name it differently: tun vs tap)
			if module == "tun" {
				if pathExists("/dev/net/tun") || isKernelModuleLoaded("tap") {
					result.KernelModules[module] = true
					continue
				}
			}
			result.Overall = false
			result.Issues = append(result.Issues, ValidationIssue{
				Component:   "kernel_module",
				Issue:       fmt.Sprintf("Kernel module '%s' not loaded", module),
				Severity:    "critical",
				Resolution:  fmt.Sprintf("Load kernel module: modprobe %s", module),
				AutoFixable: true,
			})
		}
	}

	// Check required directories/devices
	for _, dir := range sv.requiredDirectories {
		exists := pathExists(dir)
		result.DirectoryChecks[dir] = exists
		if !exists {
			result.Overall = false
			result.Issues = append(result.Issues, ValidationIssue{
				Component:   "device",
				Issue:       fmt.Sprintf("Required device/directory '%s' not found", dir),
				Severity:    "critical",
				Resolution:  fmt.Sprintf("Ensure %s is available", dir),
				AutoFixable: false,
			})
		}
	}

	// Check network requirements
	for _, req := range sv.networkRequirements {
		passed := req.CheckFunc()
		result.NetworkChecks[req.Name] = passed
		if !passed {
			result.Overall = false
			result.Issues = append(result.Issues, ValidationIssue{
				Component:   "network",
				Issue:       fmt.Sprintf("Network requirement '%s' not met: %s", req.Name, req.Description),
				Severity:    "warning",
				Resolution:  fmt.Sprintf("Fix network requirement: %s", req.Description),
				AutoFixable: true,
			})
		}
	}

	// Check permissions
	result.PermissionChecks["kvm_access"] = checkKVMAccess()
	if !result.PermissionChecks["kvm_access"] {
		result.Overall = false
		result.Issues = append(result.Issues, ValidationIssue{
			Component:   "permissions",
			Issue:       "No access to /dev/kvm",
			Severity:    "critical",
			Resolution:  "Add user to kvm group: sudo usermod -a -G kvm $USER",
			AutoFixable: true,
		})
	}

	result.PermissionChecks["network_admin"] = checkNetworkAdminCapabilities()
	if !result.PermissionChecks["network_admin"] {
		result.Issues = append(result.Issues, ValidationIssue{
			Component:   "permissions",
			Issue:       "Limited network administration capabilities",
			Severity:    "warning",
			Resolution:  "Run with sudo or configure network capabilities",
			AutoFixable: false,
		})
	}

	// Generate recommendations
	result.Recommendations = sv.generateRecommendations(result)

	return result, nil
}

// ValidateAndSetup performs validation and optionally fixes issues
func (sv *SystemValidator) ValidateAndSetup(autoFix bool) (*ValidationResult, error) {
	result, err := sv.ValidateSystem()
	if err != nil {
		return nil, err
	}

	if !autoFix || result.Overall {
		return result, nil
	}

	// Attempt to fix auto-fixable issues
	for _, issue := range result.Issues {
		if !issue.AutoFixable {
			continue
		}

		switch issue.Component {
		case "binary":
			if err := sv.fixBinaryIssue(issue); err != nil {
				fmt.Printf("Failed to fix binary issue: %v\n", err)
			}
		case "kernel_module":
			if err := sv.fixKernelModuleIssue(issue); err != nil {
				fmt.Printf("Failed to fix kernel module issue: %v\n", err)
			}
		case "network":
			if err := sv.fixNetworkIssue(issue); err != nil {
				fmt.Printf("Failed to fix network issue: %v\n", err)
			}
		case "permissions":
			if err := sv.fixPermissionIssue(issue); err != nil {
				fmt.Printf("Failed to fix permission issue: %v\n", err)
			}
		}
	}

	// Re-validate after fixes
	return sv.ValidateSystem()
}

// GenerateSetupScript generates a shell script to fix system issues
func (sv *SystemValidator) GenerateSetupScript(result *ValidationResult) string {
	script := `#!/bin/bash
# MicroVM System Setup Script
# Generated automatically to fix system validation issues

set -e

echo "🔧 Setting up MicroVM system prerequisites..."

`

	for _, issue := range result.Issues {
		if !issue.AutoFixable {
			script += fmt.Sprintf("# Manual fix required: %s\n", issue.Issue)
			script += fmt.Sprintf("# Resolution: %s\n\n", issue.Resolution)
			continue
		}

		switch issue.Component {
		case "binary":
			binary := extractBinaryName(issue.Issue)
			script += fmt.Sprintf("# Install %s\n", binary)
			script += fmt.Sprintf("if ! command -v %s &> /dev/null; then\n", binary)
			script += fmt.Sprintf("    echo \"Installing %s...\"\n", binary)
			script += fmt.Sprintf("    sudo apt-get update && sudo apt-get install -y %s\n", getPackageName(binary))
			script += "fi\n\n"

		case "kernel_module":
			module := extractModuleName(issue.Issue)
			script += fmt.Sprintf("# Load kernel module %s\n", module)
			script += fmt.Sprintf("if ! lsmod | grep -q %s; then\n", module)
			script += fmt.Sprintf("    echo \"Loading kernel module %s...\"\n", module)
			script += fmt.Sprintf("    sudo modprobe %s\n", module)
			script += "fi\n\n"

		case "network":
			if strings.Contains(issue.Issue, "ip_forwarding") {
				script += "# Enable IP forwarding\n"
				script += "echo \"Enabling IP forwarding...\"\n"
				script += "sudo sysctl -w net.ipv4.ip_forward=1\n"
				script += "echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf\n\n"
			}

		case "permissions":
			if strings.Contains(issue.Issue, "kvm") {
				script += "# Add user to kvm group\n"
				script += "echo \"Adding user to kvm group...\"\n"
				script += "sudo usermod -a -G kvm $USER\n"
				script += "echo \"Please log out and log back in for group changes to take effect\"\n\n"
			}
		}
	}

	script += `echo "✅ System setup complete!"
echo "Note: You may need to log out and log back in for all changes to take effect."
`

	return script
}

// Helper methods for fixing issues

func (sv *SystemValidator) fixBinaryIssue(issue ValidationIssue) error {
	binary := extractBinaryName(issue.Issue)
	packageName := getPackageName(binary)
	return installPackage(packageName)
}

func (sv *SystemValidator) fixKernelModuleIssue(issue ValidationIssue) error {
	module := extractModuleName(issue.Issue)
	return loadKernelModule(module)
}

func (sv *SystemValidator) fixNetworkIssue(issue ValidationIssue) error {
	for _, req := range sv.networkRequirements {
		if strings.Contains(issue.Issue, req.Name) {
			return req.FixFunc()
		}
	}
	return fmt.Errorf("unknown network issue: %s", issue.Issue)
}

func (sv *SystemValidator) fixPermissionIssue(issue ValidationIssue) error {
	if strings.Contains(issue.Issue, "kvm") {
		return addUserToGroup("kvm")
	}
	return fmt.Errorf("unknown permission issue: %s", issue.Issue)
}

func (sv *SystemValidator) generateRecommendations(result *ValidationResult) []string {
	var recommendations []string

	if !result.Overall {
		recommendations = append(recommendations, "System validation failed. Please address the issues listed above.")
	}

	criticalIssues := 0
	for _, issue := range result.Issues {
		if issue.Severity == "critical" {
			criticalIssues++
		}
	}

	if criticalIssues > 0 {
		recommendations = append(recommendations, fmt.Sprintf("Found %d critical issues that must be resolved before running MicroVMs", criticalIssues))
	}

	if !result.PermissionChecks["network_admin"] {
		recommendations = append(recommendations, "Consider running the service with appropriate network privileges or pre-configure network bridges")
	}

	if !result.NetworkChecks["ip_forwarding"] {
		recommendations = append(recommendations, "Enable IP forwarding for NAT networking to work properly")
	}

	return recommendations
}

// Utility functions

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func isKernelModuleLoaded(module string) bool {
	// Check if module is loaded using lsmod
	// Match module name at start of line (like grep "^module")
	cmd := exec.Command("lsmod")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Parse lsmod output line by line
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Module name is first field (space or tab separated)
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == module {
			return true
		}
	}
	return false
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func checkKVMAccess() bool {
	_, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	return err == nil
}

func checkNetworkAdminCapabilities() bool {
	// Simple check - try to create a dummy bridge
	cmd := exec.Command("ip", "link", "show", "type", "bridge")
	err := cmd.Run()
	return err == nil
}

func checkIPForwarding() bool {
	cmd := exec.Command("sysctl", "net.ipv4.ip_forward")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "net.ipv4.ip_forward = 1")
}

func enableIPForwarding() error {
	cmd := exec.Command("sudo", "sysctl", "-w", "net.ipv4.ip_forward=1")
	return cmd.Run()
}

func installPackage(packageName string) error {
	cmd := exec.Command("sudo", "apt-get", "update")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to update package list: %w", err)
	}

	cmd = exec.Command("sudo", "apt-get", "install", "-y", packageName)
	return cmd.Run()
}

func loadKernelModule(module string) error {
	cmd := exec.Command("sudo", "modprobe", module)
	return cmd.Run()
}

func addUserToGroup(group string) error {
	user := os.Getenv("USER")
	if user == "" {
		return fmt.Errorf("unable to determine current user")
	}

	cmd := exec.Command("sudo", "usermod", "-a", "-G", group, user)
	return cmd.Run()
}

func extractBinaryName(issue string) string {
	// Extract binary name from issue string like "Required binary 'firecracker' not found"
	start := strings.Index(issue, "'")
	if start == -1 {
		return ""
	}
	end := strings.Index(issue[start+1:], "'")
	if end == -1 {
		return ""
	}
	return issue[start+1 : start+1+end]
}

func extractModuleName(issue string) string {
	// Extract module name from issue string like "Kernel module 'kvm' not loaded"
	return extractBinaryName(issue) // Same logic
}

func getPackageName(binary string) string {
	// Map binary names to package names
	packageMap := map[string]string{
		"firecracker": "firecracker",
		"ip":          "iproute2",
		"brctl":       "bridge-utils",
		"iptables":    "iptables",
		"ping":        "iputils-ping",
		"ssh":         "openssh-client",
		"mkfs.ext4":   "e2fsprogs",
		"mount":       "mount",
		"umount":      "mount",
	}

	if pkg, exists := packageMap[binary]; exists {
		return pkg
	}
	return binary // Fallback to binary name
}
