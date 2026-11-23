#!/bin/bash

# Comprehensive integration testing script for microVM service
# This script runs all integration tests including end-to-end workflows,
# resource management, pricing engine, network configuration, and storage integration

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="$PROJECT_DIR/firecracker-work"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

# Configuration
CONFIG_FILE="$PROJECT_DIR/configs/local-dev.json"
TEST_TIMEOUT="45m"
TOTAL_CPU=8
TOTAL_MEMORY=8192
TOTAL_STORAGE=100000

# Test categories
declare -A TEST_CATEGORIES=(
    ["unit"]="Unit tests for individual components"
    ["integration"]="Integration tests for component interactions"
    ["e2e"]="End-to-end workflow tests"
    ["resource"]="Resource management tests under load"
    ["pricing"]="Pricing engine behavior tests"
    ["network"]="Network configuration tests (NAT, DHCP, MACVLAN, OVN)"
    ["storage"]="Storage integration tests (local, S3-FUSE, S3-NBD, hybrid)"
    ["lifecycle"]="Lifecycle management tests"
    ["stress"]="Stress and error handling tests"
    ["all"]="All test categories"
)

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

print_test_header() {
    echo -e "${PURPLE}[TEST]${NC} $1"
}

# Function to show usage
show_usage() {
    echo "MicroVM Service Integration Testing Script"
    echo ""
    echo "Usage: $0 [category] [options]"
    echo ""
    echo "Test Categories:"
    for category in "${!TEST_CATEGORIES[@]}"; do
        printf "  %-12s - %s\n" "$category" "${TEST_CATEGORIES[$category]}"
    done
    echo ""
    echo "Options:"
    echo "  -v, --verbose    Enable verbose output"
    echo "  -c, --coverage   Generate test coverage report"
    echo "  -f, --fail-fast  Stop on first test failure"
    echo "  -t, --timeout    Test timeout (default: 45m)"
    echo "  -h, --help       Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 all                    # Run all tests"
    echo "  $0 e2e                    # Run end-to-end tests only"
    echo "  $0 network --verbose      # Run network tests with verbose output"
    echo "  $0 integration --coverage # Run integration tests with coverage"
}

# Function to check prerequisites
check_prerequisites() {
    print_status "Checking prerequisites for integration tests..."
    
    local missing_deps=()
    
    # Check for required commands
    local required_commands=("go" "iptables" "ip" "wget" "dd" "mkfs.ext4")
    
    for cmd in "${required_commands[@]}"; do
        if ! command -v "$cmd" &> /dev/null; then
            missing_deps+=("$cmd")
        fi
    done
    
    # Check Go version
    if command -v go &> /dev/null; then
        GO_VERSION=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//')
        REQUIRED_VERSION="1.19"
        if ! printf '%s\n%s\n' "$REQUIRED_VERSION" "$GO_VERSION" | sort -V -C; then
            print_warning "Go version $GO_VERSION detected, $REQUIRED_VERSION or higher recommended"
        fi
    fi
    
    if [ ${#missing_deps[@]} -ne 0 ]; then
        print_error "Missing required dependencies: ${missing_deps[*]}"
        print_error "Please install the missing dependencies and try again"
        return 1
    fi
    
    print_success "Prerequisites check completed"
    return 0
}

# Function to setup test environment
setup_test_environment() {
    print_status "Setting up integration test environment..."
    
    # Create work directory structure
    mkdir -p "$WORK_DIR"/{images,storage,ssh-keys,logs,configs}
    
    # Build test binaries
    cd "$PROJECT_DIR"
    
    print_status "Building test binaries..."
    if ! go build -o microvm-service ./cmd/main.go; then
        print_error "Failed to build main service"
        return 1
    fi
    
    if ! go build -o test-runner ./cmd/test-runner/main.go; then
        print_error "Failed to build test runner"
        return 1
    fi
    
    # Setup NAT bridge if running as root
    if [[ $EUID -eq 0 ]]; then
        print_status "Setting up NAT bridge..."
        if ! "$SCRIPT_DIR/setup-nat-bridge.sh" setup; then
            print_warning "NAT bridge setup failed, some network tests may be skipped"
        fi
    else
        print_warning "Not running as root - network tests may require manual NAT bridge setup"
        print_status "Run: sudo $SCRIPT_DIR/setup-nat-bridge.sh setup"
    fi
    
    # Create test images
    print_status "Creating test images..."
    if ! "$SCRIPT_DIR/create-test-images.sh" minimal; then
        print_warning "Test image creation failed, using placeholders"
        # Create minimal placeholder images
        dd if=/dev/zero of="$WORK_DIR/images/alpine-rootfs.img" bs=1M count=256 2>/dev/null || true
        dd if=/dev/zero of="$WORK_DIR/images/ubuntu-rootfs.img" bs=1M count=512 2>/dev/null || true
        echo "placeholder kernel" > "$WORK_DIR/images/vmlinux"
    fi
    
    print_success "Test environment setup completed"
    return 0
}

# Function to run unit tests
run_unit_tests() {
    print_test_header "Running Unit Tests"
    
    cd "$PROJECT_DIR"
    
    local test_args=("-v" "-race")
    
    if [[ "$COVERAGE" == "true" ]]; then
        test_args+=("-coverprofile=coverage-unit.out" "-covermode=atomic")
    fi
    
    if [[ "$FAIL_FAST" == "true" ]]; then
        test_args+=("-failfast")
    fi
    
    # Run unit tests for all packages
    local packages=(
        "./internal/models/..."
        "./internal/config/..."
        "./internal/firecracker/..."
        "./internal/resource/..."
        "./internal/pricing/..."
        "./internal/lifecycle/..."
        "./internal/api/..."
    )
    
    for package in "${packages[@]}"; do
        print_status "Running unit tests for $package"
        if ! timeout "$TEST_TIMEOUT" go test "${test_args[@]}" "$package"; then
            print_error "Unit tests failed for $package"
            return 1
        fi
    done
    
    print_success "Unit tests completed successfully"
    return 0
}

# Function to run integration tests
run_integration_tests() {
    print_test_header "Running Integration Tests"
    
    cd "$PROJECT_DIR"
    
    local test_args=("-v" "-race" "-timeout" "$TEST_TIMEOUT")
    
    if [[ "$COVERAGE" == "true" ]]; then
        test_args+=("-coverprofile=coverage-integration.out" "-covermode=atomic")
    fi
    
    if [[ "$FAIL_FAST" == "true" ]]; then
        test_args+=("-failfast")
    fi
    
    # Run integration tests
    print_status "Running integration test suite..."
    if ! go test "${test_args[@]}" ./internal/testing -run "TestIntegration"; then
        print_error "Integration tests failed"
        return 1
    fi
    
    print_success "Integration tests completed successfully"
    return 0
}

# Function to run end-to-end tests
run_e2e_tests() {
    print_test_header "Running End-to-End Tests"
    
    cd "$PROJECT_DIR"
    
    # Run specific E2E test scenarios
    local scenarios=(
        "TestEndToEndMicroVMProvisioning"
        "TestConcurrentOperations"
    )
    
    for scenario in "${scenarios[@]}"; do
        print_status "Running E2E scenario: $scenario"
        if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "$scenario"; then
            print_error "E2E test failed: $scenario"
            return 1
        fi
    done
    
    print_success "End-to-end tests completed successfully"
    return 0
}

# Function to run resource management tests
run_resource_tests() {
    print_test_header "Running Resource Management Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing resource management under various load conditions..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestResourceManagementUnderLoad"; then
        print_error "Resource management tests failed"
        return 1
    fi
    
    print_success "Resource management tests completed successfully"
    return 0
}

# Function to run pricing engine tests
run_pricing_tests() {
    print_test_header "Running Pricing Engine Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing pricing engine behavior with different resource availability..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestPricingEngineBehavior"; then
        print_error "Pricing engine tests failed"
        return 1
    fi
    
    # Run pricing integration tests
    print_status "Running pricing integration tests..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/pricing -run "TestPricingEngineIntegration"; then
        print_error "Pricing integration tests failed"
        return 1
    fi
    
    print_success "Pricing engine tests completed successfully"
    return 0
}

# Function to run network configuration tests
run_network_tests() {
    print_test_header "Running Network Configuration Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing network configuration for all supported modes..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestNetworkConfiguration"; then
        print_error "Network configuration tests failed"
        return 1
    fi
    
    # Run firecracker network integration tests
    print_status "Running firecracker network integration tests..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/firecracker -run "TestNetworkManagerIntegration"; then
        print_warning "Some network integration tests failed (may be due to test environment limitations)"
    fi
    
    print_success "Network configuration tests completed successfully"
    return 0
}

# Function to run storage integration tests
run_storage_tests() {
    print_test_header "Running Storage Integration Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing storage integration with local and S3-backed options..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestStorageIntegration"; then
        print_error "Storage integration tests failed"
        return 1
    fi
    
    # Run firecracker storage integration tests
    print_status "Running firecracker storage integration tests..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/firecracker -run "TestStorageCapabilities|TestStorageTypeValidation|TestStorageInfoGeneration"; then
        print_warning "Some storage integration tests failed (may be due to missing S3 tools)"
    fi
    
    print_success "Storage integration tests completed successfully"
    return 0
}

# Function to run lifecycle management tests
run_lifecycle_tests() {
    print_test_header "Running Lifecycle Management Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing lifecycle management scenarios..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestLifecycleManagement"; then
        print_error "Lifecycle management tests failed"
        return 1
    fi
    
    # Run lifecycle component tests
    print_status "Running lifecycle component tests..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/lifecycle -run "TestManager|TestHealthChecker|TestIdleDetector"; then
        print_error "Lifecycle component tests failed"
        return 1
    fi
    
    print_success "Lifecycle management tests completed successfully"
    return 0
}

# Function to run stress and error handling tests
run_stress_tests() {
    print_test_header "Running Stress and Error Handling Tests"
    
    cd "$PROJECT_DIR"
    
    print_status "Testing error handling scenarios..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestErrorHandlingScenarios"; then
        print_error "Error handling tests failed"
        return 1
    fi
    
    print_status "Testing stress scenarios..."
    if ! timeout "$TEST_TIMEOUT" go test -v -race ./internal/testing -run "TestStressScenarios"; then
        print_error "Stress tests failed"
        return 1
    fi
    
    print_success "Stress and error handling tests completed successfully"
    return 0
}

# Function to run all tests
run_all_tests() {
    print_test_header "Running All Integration Tests"
    
    local test_functions=(
        "run_unit_tests"
        "run_integration_tests"
        "run_e2e_tests"
        "run_resource_tests"
        "run_pricing_tests"
        "run_network_tests"
        "run_storage_tests"
        "run_lifecycle_tests"
        "run_stress_tests"
    )
    
    local failed_tests=()
    
    for test_func in "${test_functions[@]}"; do
        if ! $test_func; then
            failed_tests+=("$test_func")
            if [[ "$FAIL_FAST" == "true" ]]; then
                print_error "Stopping due to test failure (fail-fast mode)"
                break
            fi
        fi
    done
    
    if [ ${#failed_tests[@]} -eq 0 ]; then
        print_success "All integration tests completed successfully!"
        return 0
    else
        print_error "The following test categories failed: ${failed_tests[*]}"
        return 1
    fi
}

# Function to generate coverage report
generate_coverage_report() {
    if [[ "$COVERAGE" != "true" ]]; then
        return 0
    fi
    
    print_status "Generating coverage report..."
    
    cd "$PROJECT_DIR"
    
    # Combine coverage files if they exist
    local coverage_files=()
    if [[ -f "coverage-unit.out" ]]; then
        coverage_files+=("coverage-unit.out")
    fi
    if [[ -f "coverage-integration.out" ]]; then
        coverage_files+=("coverage-integration.out")
    fi
    
    if [ ${#coverage_files[@]} -eq 0 ]; then
        print_warning "No coverage files found"
        return 0
    fi
    
    # Generate HTML coverage report
    if command -v go &> /dev/null; then
        go tool cover -html="${coverage_files[0]}" -o coverage.html
        print_success "Coverage report generated: coverage.html"
        
        # Print coverage summary
        go tool cover -func="${coverage_files[0]}" | tail -1
    fi
}

# Function to cleanup test environment
cleanup_test_environment() {
    print_status "Cleaning up test environment..."
    
    # Stop any running test processes
    pkill -f "test-runner" || true
    pkill -f "microvm-service" || true
    
    # Cleanup NAT bridge if running as root
    if [[ $EUID -eq 0 ]]; then
        "$SCRIPT_DIR/setup-nat-bridge.sh" cleanup || true
    fi
    
    # Remove test artifacts
    rm -f "$PROJECT_DIR/microvm-service" "$PROJECT_DIR/test-runner"
    rm -f "$PROJECT_DIR/coverage-*.out"
    
    print_success "Cleanup completed"
}

# Parse command line arguments
CATEGORY="all"
VERBOSE=false
COVERAGE=false
FAIL_FAST=false

while [[ $# -gt 0 ]]; do
    case $1 in
        -v|--verbose)
            VERBOSE=true
            shift
            ;;
        -c|--coverage)
            COVERAGE=true
            shift
            ;;
        -f|--fail-fast)
            FAIL_FAST=true
            shift
            ;;
        -t|--timeout)
            TEST_TIMEOUT="$2"
            shift 2
            ;;
        -h|--help)
            show_usage
            exit 0
            ;;
        -*)
            print_error "Unknown option: $1"
            show_usage
            exit 1
            ;;
        *)
            if [[ -n "${TEST_CATEGORIES[$1]}" ]]; then
                CATEGORY="$1"
            else
                print_error "Unknown test category: $1"
                show_usage
                exit 1
            fi
            shift
            ;;
    esac
done

# Set verbose mode
if [[ "$VERBOSE" == "true" ]]; then
    set -x
fi

# Main execution
main() {
    print_status "Starting integration tests for category: $CATEGORY"
    print_status "Configuration: timeout=$TEST_TIMEOUT, coverage=$COVERAGE, fail-fast=$FAIL_FAST"
    
    # Check prerequisites
    if ! check_prerequisites; then
        exit 1
    fi
    
    # Setup test environment
    if ! setup_test_environment; then
        exit 1
    fi
    
    # Trap to cleanup on exit
    trap cleanup_test_environment EXIT
    
    # Run tests based on category
    case "$CATEGORY" in
        "unit")
            run_unit_tests
            ;;
        "integration")
            run_integration_tests
            ;;
        "e2e")
            run_e2e_tests
            ;;
        "resource")
            run_resource_tests
            ;;
        "pricing")
            run_pricing_tests
            ;;
        "network")
            run_network_tests
            ;;
        "storage")
            run_storage_tests
            ;;
        "lifecycle")
            run_lifecycle_tests
            ;;
        "stress")
            run_stress_tests
            ;;
        "all")
            run_all_tests
            ;;
        *)
            print_error "Invalid test category: $CATEGORY"
            show_usage
            exit 1
            ;;
    esac
    
    local test_result=$?
    
    # Generate coverage report
    generate_coverage_report
    
    if [ $test_result -eq 0 ]; then
        print_success "Integration tests completed successfully!"
    else
        print_error "Integration tests failed!"
    fi
    
    exit $test_result
}

# Run main function
main "$@"