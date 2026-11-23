#!/bin/bash

# MicroVM Lifecycle Demo Script
# Demonstrates creating, booting, and accessing a microVM with SSH

set -e

# Configuration
SERVICE_URL="http://localhost:8080"
VM_NAME="demo-vm-$(date +%s)"
SSH_TIMEOUT=30
BOOT_TIMEOUT=60

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

# Check if service is running
check_service() {
    log_info "Checking if MicroVM service is running..."
    
    if curl -s "$SERVICE_URL/health" >/dev/null 2>&1; then
        log_success "MicroVM service is running"
    else
        log_error "MicroVM service is not running or not accessible at $SERVICE_URL"
        log_info "Please start the service with: sudo systemctl start microvm-service"
        exit 1
    fi
}

# Create VM
create_vm() {
    log_info "Creating VM: $VM_NAME"
    
    VM_CONFIG=$(cat <<EOF
{
    "name": "$VM_NAME",
    "resources": {
        "vcpus": 1,
        "memory": 128
    },
    "image": {
        "distribution": "alpine",
        "version": "3.18"
    },
    "network": {
        "enable_ssh": true
    }
}
EOF
)
    
    RESPONSE=$(curl -s -X POST "$SERVICE_URL/api/v1/vms" \
        -H "Content-Type: application/json" \
        -d "$VM_CONFIG")
    
    VM_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
    
    if [ -n "$VM_ID" ]; then
        log_success "VM created with ID: $VM_ID"
        echo "$VM_ID" > /tmp/demo-vm-id
    else
        log_error "Failed to create VM"
        echo "Response: $RESPONSE"
        exit 1
    fi
}

# Wait for VM to boot
wait_for_boot() {
    log_info "Waiting for VM to boot (timeout: ${BOOT_TIMEOUT}s)..."
    
    local start_time=$(date +%s)
    local timeout=$((start_time + BOOT_TIMEOUT))
    
    while [ $(date +%s) -lt $timeout ]; do
        STATUS=$(curl -s "$SERVICE_URL/api/v1/vms/$VM_ID/status" | jq -r '.status // empty')
        
        case "$STATUS" in
            "running")
                log_success "VM is running and ready"
                return 0
                ;;
            "booting")
                echo -n "."
                sleep 2
                ;;
            "failed")
                log_error "VM boot failed"
                return 1
                ;;
            *)
                echo -n "."
                sleep 2
                ;;
        esac
    done
    
    log_error "VM boot timed out after ${BOOT_TIMEOUT} seconds"
    return 1
}

# Get VM information
get_vm_info() {
    log_info "Retrieving VM information..."
    
    VM_INFO=$(curl -s "$SERVICE_URL/api/v1/vms/$VM_ID")
    
    VM_IP=$(echo "$VM_INFO" | jq -r '.ip_address // empty')
    SSH_PORT=$(echo "$VM_INFO" | jq -r '.ssh_port // empty')
    SSH_KEY_PATH=$(echo "$VM_INFO" | jq -r '.ssh_key_path // empty')
    
    if [ -n "$VM_IP" ] && [ -n "$SSH_PORT" ] && [ -n "$SSH_KEY_PATH" ]; then
        log_success "VM Information:"
        echo "  Name: $VM_NAME"
        echo "  ID: $VM_ID"
        echo "  IP Address: $VM_IP"
        echo "  SSH Port: $SSH_PORT"
        echo "  SSH Key: $SSH_KEY_PATH"
    else
        log_error "Failed to retrieve complete VM information"
        echo "Response: $VM_INFO"
        return 1
    fi
}

# Test SSH connectivity
test_ssh() {
    log_info "Testing SSH connectivity..."
    
    # Wait a bit for SSH service to be ready
    sleep 5
    
    local start_time=$(date +%s)
    local timeout=$((start_time + SSH_TIMEOUT))
    
    while [ $(date +%s) -lt $timeout ]; do
        if ssh -i "$SSH_KEY_PATH" -p "$SSH_PORT" -o ConnectTimeout=5 \
           -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
           alpine@localhost "echo 'SSH connection successful'" 2>/dev/null; then
            log_success "SSH connection established"
            return 0
        fi
        
        echo -n "."
        sleep 2
    done
    
    log_error "SSH connection failed after ${SSH_TIMEOUT} seconds"
    return 1
}

# Demonstrate VM operations
demonstrate_operations() {
    log_info "Demonstrating VM operations..."
    
    # Array of commands to run
    local commands=(
        "uname -a"
        "cat /etc/os-release"
        "free -h"
        "df -h"
        "ip addr show"
        "ps aux | head -10"
        "uptime"
    )
    
    for cmd in "${commands[@]}"; do
        echo -e "\n${BLUE}Running: $cmd${NC}"
        ssh -i "$SSH_KEY_PATH" -p "$SSH_PORT" \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            alpine@localhost "$cmd" 2>/dev/null || log_warning "Command failed: $cmd"
    done
    
    log_success "VM operations demonstration completed"
}

# Test network connectivity from VM
test_network() {
    log_info "Testing network connectivity from VM..."
    
    # Test ping to gateway
    if ssh -i "$SSH_KEY_PATH" -p "$SSH_PORT" \
       -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
       alpine@localhost "ping -c 3 192.168.100.1" >/dev/null 2>&1; then
        log_success "VM can ping gateway"
    else
        log_warning "VM cannot ping gateway"
    fi
    
    # Test internet connectivity
    if ssh -i "$SSH_KEY_PATH" -p "$SSH_PORT" \
       -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
       alpine@localhost "ping -c 3 8.8.8.8" >/dev/null 2>&1; then
        log_success "VM has internet connectivity"
    else
        log_warning "VM does not have internet connectivity"
    fi
    
    # Test DNS resolution
    if ssh -i "$SSH_KEY_PATH" -p "$SSH_PORT" \
       -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
       alpine@localhost "nslookup google.com" >/dev/null 2>&1; then
        log_success "VM DNS resolution working"
    else
        log_warning "VM DNS resolution not working"
    fi
}

# Show connection information
show_connection_info() {
    log_info "Connection Information:"
    echo ""
    echo "To connect to your VM manually, use:"
    echo "  ssh -i $SSH_KEY_PATH -p $SSH_PORT alpine@localhost"
    echo ""
    echo "VM Details:"
    echo "  Internal IP: $VM_IP"
    echo "  SSH Port: $SSH_PORT"
    echo "  Username: alpine"
    echo "  Key File: $SSH_KEY_PATH"
    echo ""
}

# Cleanup VM
cleanup_vm() {
    if [ -f /tmp/demo-vm-id ]; then
        local vm_id=$(cat /tmp/demo-vm-id)
        log_info "Cleaning up VM: $vm_id"
        
        if curl -s -X DELETE "$SERVICE_URL/api/v1/vms/$vm_id" >/dev/null 2>&1; then
            log_success "VM cleaned up successfully"
        else
            log_warning "Failed to cleanup VM (may need manual cleanup)"
        fi
        
        rm -f /tmp/demo-vm-id
    fi
}

# Trap to cleanup on exit
trap cleanup_vm EXIT

# Main execution
main() {
    echo -e "${BLUE}"
    echo "MicroVM Lifecycle Demo"
    echo "====================="
    echo -e "${NC}"
    
    # Check dependencies
    if ! command -v curl >/dev/null 2>&1; then
        log_error "curl is required but not installed"
        exit 1
    fi
    
    if ! command -v jq >/dev/null 2>&1; then
        log_error "jq is required but not installed"
        exit 1
    fi
    
    if ! command -v ssh >/dev/null 2>&1; then
        log_error "ssh is required but not installed"
        exit 1
    fi
    
    # Run demo steps
    check_service
    echo
    
    create_vm
    echo
    
    wait_for_boot
    echo
    
    get_vm_info
    echo
    
    test_ssh
    echo
    
    demonstrate_operations
    echo
    
    test_network
    echo
    
    show_connection_info
    
    # Ask user if they want to keep the VM running
    echo -e "${YELLOW}Press Enter to cleanup and exit, or Ctrl+C to keep VM running...${NC}"
    read -r
    
    log_info "Demo completed successfully!"
}

# Handle command line arguments
case "${1:-}" in
    --help|-h)
        echo "Usage: $0 [options]"
        echo ""
        echo "Options:"
        echo "  --help, -h     Show this help message"
        echo "  --service-url  Set service URL (default: $SERVICE_URL)"
        echo "  --timeout      Set boot timeout in seconds (default: $BOOT_TIMEOUT)"
        echo ""
        echo "Environment variables:"
        echo "  MICROVM_SERVICE_URL  Service URL"
        echo "  MICROVM_BOOT_TIMEOUT Boot timeout in seconds"
        exit 0
        ;;
    --service-url)
        SERVICE_URL="$2"
        shift 2
        ;;
    --timeout)
        BOOT_TIMEOUT="$2"
        shift 2
        ;;
esac

# Override with environment variables if set
SERVICE_URL="${MICROVM_SERVICE_URL:-$SERVICE_URL}"
BOOT_TIMEOUT="${MICROVM_BOOT_TIMEOUT:-$BOOT_TIMEOUT}"

# Run main function
main "$@"