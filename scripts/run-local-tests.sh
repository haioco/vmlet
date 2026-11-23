#!/bin/bash

# Comprehensive local testing script for microVM service
# This script sets up the complete testing environment and runs all tests

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="$PROJECT_DIR/firecracker-work"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
CONFIG_FILE="$PROJECT_DIR/configs/local-dev.json"
TEST_TIMEOUT="30m"
TOTAL_CPU=8
TOTAL_MEMORY=8192
TOTAL_STORAGE=100000

# Function to print colored output
print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to check prerequisites
check_prerequisites() {
    print_status "Checking prerequisites..."
    
    local missing_deps=()
    
    # Check for required commands
    local required_commands=("go" "iptables" "ip" "wget" "dd" "mkfs.ext4")
    
    for cmd in "${required_commands[@]}"; do
        if ! command -v "$cmd" &> /dev/null; then
            missing_deps+=("$cmd")
        fi
    done
    
    # Check for optional commands
    local optional_commands=("dnsmasq" "debootstrap")
    local missing_optional=()
    
    for cmd in "${optional_commands[@]}"; do
        if ! command -v "$cmd" &> /dev/null; then
            missing_optional+=("$cmd")
        fi
    done
    
    if [ ${#missing_deps[@]} -ne 0 ]; then
        print_error "Missing required dependencies: ${missing_deps[*]}"
        print_error "Please install the missing dependencies and try again"
        return 1
    fi
    
    if [ ${#missing_optional[@]} -ne 0 ]; then
        print_warning "Missing optional dependencies: ${missing_optional[*]}"
        print_warning "Some features may not work properly"
    fi
    
    # Check if running as root for network setup
    if [[ $EUID -eq 0 ]]; then
        print_status "Running as root - network setup will be performed"
    else
        print_warning "Not running as root - network setup may require manual intervention"
    fi
    
    print_success "Prerequisites check completed"
    return 0
}

# Function to build the test runner
build_test_runner() {
    print_status "Building test runner..."
    
    cd "$PROJECT_DIR"
    
    # Build the main service
    if ! go build -o microvm-service ./cmd/main.go; then
        print_error "Failed to build main service"
        return 1
    fi
    
    # Build the test runner
    if ! go build -o test-runner ./cmd/test-runner/main.go; then
        print_error "Failed to build test runner"
        return 1
    fi
    
    print_success "Test runner built successfully"
    return 0
}

# Function to setup NAT bridge
setup_nat_bridge() {
    print_status "Setting up NAT bridge..."
    
    if [[ $EUID -eq 0 ]]; then
        # Running as root, can setup bridge directly
        if "$SCRIPT_DIR/setup-nat-bridge.sh" setup; then
            print_success "NAT bridge setup completed"
        else
            print_error "NAT bridge setup failed"
            return 1
        fi
    else
        # Not running as root, provide instructions
        print_warning "NAT bridge setup requires root privileges"
        print_status "Please run the following command in another terminal:"
        print_status "sudo $SCRIPT_DIR/setup-nat-bridge.sh setup"
        
        # Wait for user confirmation
        read -p "Press Enter after running the NAT bridge setup command..."
        
        # Verify bridge exists
        if ip link show fc-nat-br0 >/dev/null 2>&1; then
            print_success "NAT bridge verified"
        else
            print_error "NAT bridge not found - setup may have failed"
            return 1
        fi
    fi
    
    return 0
}

# Function to create test images
create_test_images() {
    print_status "Creating test images..."
    
    # Check if images already exist
    if [[ -f "$WORK_DIR/images/alpine-rootfs.img" && 
          -f "$WORK_DIR/images/ubuntu-rootfs.img" && 
          -f "$WORK_DIR/images/vmlinux" ]]; then
        print_status "Test images already exist, skipping creation"
        return 0
    fi
    
    # Create images directory
    mkdir -p "$WORK_DIR/images"
    
    # For now, create placeholder images for testing
    # In a real implementation, you would run the create-test-images.sh script
    print_status "Creating placeholder test images..."
    
    # Create placeholder Alpine image
    dd if=/dev/zero of="$WORK_DIR/images/alpine-rootfs.img" bs=1M count=512 2>/dev/null
    mkfs.ext4 -F "$WORK_DIR/images/alpine-rootfs.img" >/dev/null 2>&1
    
    # Create placeholder Ubuntu image
    dd if=/dev/zero of="$WORK_DIR/images/ubuntu-rootfs.img" bs=1M count=1024 2>/dev/null
    mkfs.ext4 -F "$WORK_DIR/images/ubuntu-rootfs.img" >/dev/null 2>&1
    
    # Download or create placeholder kernel
    if ! wget -q -O "$WORK_DIR/images/vmlinux" "https://github.com/firecracker-microvm/firecracker/releases/download/v1.4.1/vmlinux.bin" 2>/dev/null; then
        print_warning "Failed to download kernel, creating placeholder"
        echo "placeholder kernel" > "$WORK_DIR/images/vmlinux"
    fi
    
    print_success "Test images created"
    return 0
}

# Function to run specific test scenario
run_scenario() {
    local scenario="$1"
    print_status "Running scenario: $scenario"
    
    cd "$PROJECT_DIR"
    
    if ./test-runner \
        -config "$CONFIG_FILE" \
        -workdir "$WORK_DIR" \
        -scenario "$scenario" \
        -cpu "$TOTAL_CPU" \
        -memory "$TOTAL_MEMORY" \
        -storage "$TOTAL_STORAGE" \
        -timeout "$TEST_TIMEOUT"; then
        print_success "Scenario '$scenario' completed successfully"
        return 0
    else
        print_error "Scenario '$scenario' failed"
        return 1
    fi
}

# Function to run all tests
run_all_tests() {
    print_status "Running all test scenarios..."
    
    cd "$PROJECT_DIR"
    
    if ./test-runner \
        -config "$CONFIG_FILE" \
        -workdir "$WORK_DIR" \
        -cpu "$TOTAL_CPU" \
        -memory "$TOTAL_MEMORY" \
        -storage "$TOTAL_STORAGE" \
        -timeout "$TEST_TIMEOUT"; then
        print_success "All tests completed successfully"
        return 0
    else
        print_error "Some tests failed"
        return 1
    fi
}

# Function to cleanup test environment
cleanup_environment() {
    print_status "Cleaning up test environment..."
    
    cd "$PROJECT_DIR"
    
    # Run test runner cleanup
    if [[ -f "./test-runner" ]]; then
        ./test-runner -cleanup || true
    fi
    
    # Cleanup NAT bridge if running as root
    if [[ $EUID -eq 0 ]]; then
        "$SCRIPT_DIR/setup-nat-bridge.sh" cleanup || true
    else
        print_warning "NAT bridge cleanup requires root privileges"
        print_status "Run: sudo $SCRIPT_DIR/setup-nat-bridge.sh cleanup"
    fi
    
    # Remove test images if requested
    if [[ "$1" == "full" ]]; then
        print_status "Removing test images..."
        rm -rf "$WORK_DIR/images"
    fi
    
    print_success "Cleanup completed"
}

# Function to show usage
show_usage() {
    echo "MicroVM Service Local Testing Script"
    echo ""
    echo "Usage: $0 [command] [options]"
    echo ""
    echo "Commands:"
    echo "  setup           - Setup test environment only"
    echo "  test [scenario] - Run tests (all or specific scenario)"
    echo "  cleanup [full]  - Cleanup test environment (full removes images)"
    echo "  list            - List available test scenarios"
    echo "  help            - Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 setup                    # Setup test environment"
    echo "  $0 test                     # Run all tests"
    echo "  $0 test basic-lifecycle     # Run specific scenario"
    echo "  $0 list                     # List scenarios"
    echo "  $0 cleanup                  # Cleanup environment"
    echo "  $0 cleanup full             # Cleanup including images"
    echo ""
    echo "Available scenarios:"
    echo "  - basic-lifecycle"
    echo "  - network-connectivity"
    echo "  - resource-allocation"
    echo "  - ssh-access"
    echo "  - multiple-microvms"
    echo "  - error-handling"
}

# Function to list scenarios
list_scenarios() {
    print_status "Available test scenarios:"
    echo "1. basic-lifecycle - Test create, start, stop, delete operations"
    echo "2. network-connectivity - Test network configuration and connectivity"
    echo "3. resource-allocation - Test resource allocation and limits"
    echo "4. ssh-access - Test SSH access through port forwarding"
    echo "5. multiple-microvms - Test multiple microVMs simultaneously"
    echo "6. error-handling - Test error handling for invalid operations"
}

# Main execution
main() {
    local command="${1:-test}"
    local arg="${2:-}"
    
    case "$command" in
        "setup")
            print_status "Setting up test environment..."
            check_prerequisites || exit 1
            build_test_runner || exit 1
            setup_nat_bridge || exit 1
            create_test_images || exit 1
            print_success "Test environment setup completed"
            ;;
            
        "test")
            print_status "Running tests..."
            check_prerequisites || exit 1
            build_test_runner || exit 1
            setup_nat_bridge || exit 1
            create_test_images || exit 1
            
            if [[ -n "$arg" ]]; then
                run_scenario "$arg" || exit 1
            else
                run_all_tests || exit 1
            fi
            ;;
            
        "cleanup")
            cleanup_environment "$arg"
            ;;
            
        "list")
            list_scenarios
            ;;
            
        "help"|"-h"|"--help")
            show_usage
            ;;
            
        *)
            print_error "Unknown command: $command"
            show_usage
            exit 1
            ;;
    esac
}

# Trap to cleanup on exit
trap 'cleanup_environment' EXIT

# Run main function
main "$@"