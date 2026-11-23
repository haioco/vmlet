#!/bin/bash

# MicroVM Service System Validation Script
# This script validates that the host system is properly configured
# for running microVMs with bootable images and network connectivity

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Counters
CHECKS_PASSED=0
CHECKS_FAILED=0
WARNINGS=0

# Helper functions
print_header() {
    echo -e "${BLUE}=== $1 ===${NC}"
}

print_success() {
    echo -e "${GREEN}✓ $1${NC}"
    ((CHECKS_PASSED++))
}

print_error() {
    echo -e "${RED}✗ $1${NC}"
    ((CHECKS_FAILED++))
}

print_warning() {
    echo -e "${YELLOW}⚠ $1${NC}"
    ((WARNINGS++))
}

print_info() {
    echo -e "${BLUE}ℹ $1${NC}"
}

check_command() {
    if command -v "$1" >/dev/null 2>&1; then
        print_success "$1 is installed"
        return 0
    else
        print_error "$1 is not installed"
        return 1
    fi
}

check_file() {
    if [ -f "$1" ]; then
        print_success "File exists: $1"
        return 0
    else
        print_error "File missing: $1"
        return 1
    fi
}

check_directory() {
    if [ -d "$1" ]; then
        print_success "Directory exists: $1"
        return 0
    else
        print_error "Directory missing: $1"
        return 1
    fi
}

# Main validation functions
check_system_requirements() {
    print_header "System Requirements"
    
    # Check architecture
    ARCH=$(uname -m)
    if [ "$ARCH" = "x86_64" ]; then
        print_success "Architecture: $ARCH"
    else
        print_error "Unsupported architecture: $ARCH (x86_64 required)"
    fi
    
    # Check kernel version
    KERNEL_VERSION=$(uname -r)
    KERNEL_MAJOR=$(echo "$KERNEL_VERSION" | cut -d. -f1)
    KERNEL_MINOR=$(echo "$KERNEL_VERSION" | cut -d. -f2)
    
    if [ "$KERNEL_MAJOR" -gt 4 ] || ([ "$KERNEL_MAJOR" -eq 4 ] && [ "$KERNEL_MINOR" -ge 14 ]); then
        print_success "Kernel version: $KERNEL_VERSION"
    else
        print_error "Kernel version too old: $KERNEL_VERSION (4.14+ required)"
    fi
    
    # Check memory
    MEMORY_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
    MEMORY_GB=$((MEMORY_KB / 1024 / 1024))
    
    if [ "$MEMORY_GB" -ge 4 ]; then
        print_success "Memory: ${MEMORY_GB}GB"
    elif [ "$MEMORY_GB" -ge 2 ]; then
        print_warning "Memory: ${MEMORY_GB}GB (4GB+ recommended)"
    else
        print_error "Memory: ${MEMORY_GB}GB (4GB+ required)"
    fi
    
    # Check disk space
    DISK_SPACE=$(df /var/lib 2>/dev/null | tail -1 | awk '{print $4}' || echo "0")
    DISK_SPACE_GB=$((DISK_SPACE / 1024 / 1024))
    
    if [ "$DISK_SPACE_GB" -ge 10 ]; then
        print_success "Disk space: ${DISK_SPACE_GB}GB available"
    elif [ "$DISK_SPACE_GB" -ge 5 ]; then
        print_warning "Disk space: ${DISK_SPACE_GB}GB available (10GB+ recommended)"
    else
        print_error "Disk space: ${DISK_SPACE_GB}GB available (10GB+ required)"
    fi
}

check_virtualization_support() {
    print_header "Virtualization Support"
    
    # Check CPU virtualization features
    if grep -q "vmx\|svm" /proc/cpuinfo; then
        print_success "CPU virtualization support detected"
    else
        print_error "CPU virtualization support not detected"
    fi
    
    # Check KVM availability
    if [ -e /dev/kvm ]; then
        print_success "/dev/kvm device exists"
        
        # Check KVM permissions
        if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
            print_success "KVM device is accessible"
        else
            print_error "KVM device permissions incorrect (add user to kvm group)"
        fi
    else
        print_error "/dev/kvm device not found (load kvm modules)"
    fi
    
    # Check KVM modules
    if lsmod | grep -q "kvm"; then
        print_success "KVM modules loaded"
    else
        print_error "KVM modules not loaded"
    fi
}

check_required_packages() {
    print_header "Required Packages"
    
    # Essential commands
    check_command "firecracker" || print_info "Install from: https://github.com/firecracker-microvm/firecracker/releases"
    check_command "brctl" || print_info "Install bridge-utils package"
    check_command "dnsmasq" || print_info "Install dnsmasq package"
    check_command "iptables" || print_info "Install iptables package"
    check_command "debootstrap" || print_info "Install debootstrap package"
    check_command "cloud-init" || print_info "Install cloud-init package"
    check_command "qemu-img" || print_info "Install qemu-utils package"
    
    # Optional but recommended
    check_command "curl" || print_warning "curl recommended for downloads"
    check_command "wget" || print_warning "wget recommended for downloads"
    check_command "ssh" || print_warning "ssh client recommended for VM access"
}

check_network_configuration() {
    print_header "Network Configuration"
    
    # Check IP forwarding
    IP_FORWARD=$(cat /proc/sys/net/ipv4/ip_forward)
    if [ "$IP_FORWARD" = "1" ]; then
        print_success "IP forwarding enabled"
    else
        print_error "IP forwarding disabled (enable with: sysctl net.ipv4.ip_forward=1)"
    fi
    
    # Check bridge support
    if [ -f /proc/net/bridge ]; then
        print_success "Bridge support available"
    else
        print_error "Bridge support not available (load bridge module)"
    fi
    
    # Check existing bridge
    if brctl show 2>/dev/null | grep -q "fc-nat-br0"; then
        print_success "MicroVM bridge (fc-nat-br0) exists"
        
        # Check bridge IP
        BRIDGE_IP=$(ip addr show fc-nat-br0 2>/dev/null | grep "inet " | awk '{print $2}' | head -1)
        if [ -n "$BRIDGE_IP" ]; then
            print_success "Bridge IP configured: $BRIDGE_IP"
        else
            print_warning "Bridge exists but no IP configured"
        fi
    else
        print_info "MicroVM bridge not configured (will be created automatically)"
    fi
    
    # Check DHCP service
    if systemctl is-active --quiet dnsmasq 2>/dev/null; then
        print_success "DHCP service (dnsmasq) is running"
    else
        print_info "DHCP service not running (will be configured automatically)"
    fi
    
    # Check iptables
    if iptables -t nat -L >/dev/null 2>&1; then
        print_success "iptables NAT table accessible"
        
        # Check for existing NAT rules
        if iptables -t nat -L POSTROUTING -n | grep -q "MASQUERADE"; then
            print_success "NAT rules configured"
        else
            print_info "NAT rules not configured (will be added automatically)"
        fi
    else
        print_error "iptables not accessible (check permissions)"
    fi
}

check_service_configuration() {
    print_header "Service Configuration"
    
    # Check service directories
    check_directory "/var/lib/microvm-service" || print_info "Service directory will be created"
    check_directory "/var/log/microvm-service" || print_info "Log directory will be created"
    check_directory "/etc/microvm-service" || print_info "Config directory should be created"
    
    # Check configuration file
    if check_file "/etc/microvm-service/config.json"; then
        print_success "Service configuration exists"
    else
        print_info "Service configuration not found (use example config)"
    fi
    
    # Check service binary
    if check_file "/usr/local/bin/microvm-service"; then
        print_success "Service binary installed"
    else
        print_info "Service binary not installed (build and install from source)"
    fi
    
    # Check systemd service
    if systemctl list-unit-files | grep -q "microvm-service.service"; then
        print_success "Systemd service configured"
        
        if systemctl is-active --quiet microvm-service; then
            print_success "Service is running"
        else
            print_info "Service is not running"
        fi
    else
        print_info "Systemd service not configured"
    fi
}

check_permissions() {
    print_header "Permissions and Security"
    
    # Check if running as root
    if [ "$EUID" -eq 0 ]; then
        print_success "Running as root (required for setup)"
    else
        print_warning "Not running as root (some checks may fail)"
    fi
    
    # Check user groups
    if groups | grep -q "kvm"; then
        print_success "User is in kvm group"
    else
        print_error "User not in kvm group (add with: usermod -a -G kvm \$USER)"
    fi
    
    # Check sudo access
    if sudo -n true 2>/dev/null; then
        print_success "Sudo access available"
    else
        print_warning "Sudo access not available or requires password"
    fi
    
    # Check file permissions for service directories
    if [ -d "/var/lib/microvm-service" ]; then
        OWNER=$(stat -c '%U' /var/lib/microvm-service)
        if [ "$OWNER" = "microvm-service" ] || [ "$OWNER" = "root" ]; then
            print_success "Service directory ownership correct"
        else
            print_warning "Service directory ownership: $OWNER (should be microvm-service or root)"
        fi
    fi
}

run_connectivity_tests() {
    print_header "Connectivity Tests"
    
    # Test internet connectivity
    if ping -c 1 8.8.8.8 >/dev/null 2>&1; then
        print_success "Internet connectivity available"
    else
        print_error "No internet connectivity (required for image downloads)"
    fi
    
    # Test DNS resolution
    if nslookup dl-cdn.alpinelinux.org >/dev/null 2>&1; then
        print_success "DNS resolution working"
    else
        print_error "DNS resolution failed"
    fi
    
    # Test Alpine repository access
    if curl -s --head https://dl-cdn.alpinelinux.org/alpine/ | head -1 | grep -q "200 OK"; then
        print_success "Alpine repository accessible"
    else
        print_warning "Alpine repository not accessible (may affect image creation)"
    fi
}

generate_recommendations() {
    print_header "Recommendations"
    
    if [ $CHECKS_FAILED -gt 0 ]; then
        echo -e "${RED}Critical issues found that must be resolved:${NC}"
        echo "1. Install missing required packages"
        echo "2. Enable KVM support and load modules"
        echo "3. Configure proper permissions"
        echo "4. Enable IP forwarding"
        echo ""
    fi
    
    if [ $WARNINGS -gt 0 ]; then
        echo -e "${YELLOW}Warnings that should be addressed:${NC}"
        echo "1. Increase system resources if possible"
        echo "2. Configure network settings"
        echo "3. Set up proper user permissions"
        echo ""
    fi
    
    echo -e "${BLUE}Next steps:${NC}"
    echo "1. Review the setup guide: docs/BOOTABLE_IMAGES_SETUP.md"
    echo "2. Install missing packages and configure system"
    echo "3. Run this script again to verify fixes"
    echo "4. Start the microvm-service and test VM creation"
}

print_summary() {
    print_header "Validation Summary"
    
    echo -e "Checks passed: ${GREEN}$CHECKS_PASSED${NC}"
    echo -e "Checks failed: ${RED}$CHECKS_FAILED${NC}"
    echo -e "Warnings: ${YELLOW}$WARNINGS${NC}"
    
    if [ $CHECKS_FAILED -eq 0 ]; then
        echo -e "\n${GREEN}✓ System appears ready for MicroVM service!${NC}"
        exit 0
    else
        echo -e "\n${RED}✗ System requires configuration before running MicroVM service${NC}"
        exit 1
    fi
}

# Main execution
main() {
    echo -e "${BLUE}"
    echo "MicroVM Service System Validation"
    echo "================================="
    echo -e "${NC}"
    
    check_system_requirements
    echo
    check_virtualization_support
    echo
    check_required_packages
    echo
    check_network_configuration
    echo
    check_service_configuration
    echo
    check_permissions
    echo
    run_connectivity_tests
    echo
    generate_recommendations
    echo
    print_summary
}

# Run main function
main "$@"