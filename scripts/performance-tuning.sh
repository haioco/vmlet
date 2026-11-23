#!/bin/bash

# Performance tuning script for MicroVM Service
# This script optimizes system settings for production performance

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

# Check if running as root
check_root() {
    if [[ $EUID -ne 0 ]]; then
        error "This script must be run as root"
        exit 1
    fi
}

# Optimize kernel parameters
optimize_kernel_params() {
    log "Optimizing kernel parameters..."
    
    # Create sysctl configuration for microvm service
    cat > /etc/sysctl.d/99-microvm-service.conf << 'EOF'
# MicroVM Service Performance Optimizations

# Network optimizations
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.core.rmem_default = 65536
net.core.wmem_default = 65536
net.core.netdev_max_backlog = 5000
net.core.somaxconn = 65535
net.ipv4.tcp_rmem = 4096 65536 134217728
net.ipv4.tcp_wmem = 4096 65536 134217728
net.ipv4.tcp_congestion_control = bbr
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.tcp_tw_reuse = 1

# File descriptor limits
fs.file-max = 2097152
fs.nr_open = 2097152

# Virtual memory optimizations
vm.swappiness = 10
vm.dirty_ratio = 15
vm.dirty_background_ratio = 5
vm.vfs_cache_pressure = 50

# Process limits
kernel.pid_max = 4194304
kernel.threads-max = 4194304

# Firecracker specific optimizations
net.bridge.bridge-nf-call-iptables = 0
net.bridge.bridge-nf-call-ip6tables = 0
net.bridge.bridge-nf-call-arptables = 0

# KVM optimizations
kernel.sched_rt_runtime_us = 950000
kernel.sched_rt_period_us = 1000000
EOF
    
    # Apply sysctl settings
    sysctl -p /etc/sysctl.d/99-microvm-service.conf
    
    success "Kernel parameters optimized"
}

# Optimize systemd limits
optimize_systemd_limits() {
    log "Optimizing systemd limits..."
    
    # Create systemd system configuration
    mkdir -p /etc/systemd/system.conf.d
    cat > /etc/systemd/system.conf.d/microvm-limits.conf << 'EOF'
[Manager]
DefaultLimitNOFILE=65536
DefaultLimitNPROC=65536
DefaultLimitCORE=infinity
DefaultLimitMEMLOCK=infinity
EOF
    
    # Create user limits
    cat > /etc/security/limits.d/microvm-service.conf << 'EOF'
# MicroVM Service limits
microvm soft nofile 65536
microvm hard nofile 65536
microvm soft nproc 65536
microvm hard nproc 65536
microvm soft memlock unlimited
microvm hard memlock unlimited

# Root limits for firecracker
root soft nofile 65536
root hard nofile 65536
root soft nproc 65536
root hard nproc 65536
EOF
    
    success "Systemd limits optimized"
}

# Optimize CPU settings
optimize_cpu() {
    log "Optimizing CPU settings..."
    
    # Set CPU governor to performance
    if [[ -d /sys/devices/system/cpu/cpu0/cpufreq ]]; then
        echo performance > /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || true
        
        # Apply to all CPUs
        for cpu in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
            if [[ -w "$cpu" ]]; then
                echo performance > "$cpu"
            fi
        done
        
        success "CPU governor set to performance"
    else
        warning "CPU frequency scaling not available"
    fi
    
    # Disable CPU idle states for better latency (optional)
    if [[ -f /sys/devices/system/cpu/cpu0/cpuidle/state1/disable ]]; then
        read -p "Disable CPU idle states for lower latency? (may increase power consumption) (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            for state in /sys/devices/system/cpu/cpu*/cpuidle/state*/disable; do
                if [[ -w "$state" ]]; then
                    echo 1 > "$state"
                fi
            done
            success "CPU idle states disabled"
        fi
    fi
}

# Optimize memory settings
optimize_memory() {
    log "Optimizing memory settings..."
    
    # Configure transparent huge pages
    if [[ -f /sys/kernel/mm/transparent_hugepage/enabled ]]; then
        echo madvise > /sys/kernel/mm/transparent_hugepage/enabled
        echo madvise > /sys/kernel/mm/transparent_hugepage/defrag
        success "Transparent huge pages configured"
    fi
    
    # Configure NUMA balancing
    if [[ -f /proc/sys/kernel/numa_balancing ]]; then
        echo 0 > /proc/sys/kernel/numa_balancing
        success "NUMA balancing disabled for consistent performance"
    fi
}

# Optimize I/O settings
optimize_io() {
    log "Optimizing I/O settings..."
    
    # Find storage devices
    for device in $(lsblk -d -n -o NAME | grep -E '^(sd|nvme|vd)'); do
        device_path="/dev/$device"
        
        # Set I/O scheduler
        if [[ -f "/sys/block/$device/queue/scheduler" ]]; then
            # Use mq-deadline for SSDs, deadline for HDDs
            if [[ $(cat "/sys/block/$device/queue/rotational") == "0" ]]; then
                echo mq-deadline > "/sys/block/$device/queue/scheduler" 2>/dev/null || true
                log "Set mq-deadline scheduler for SSD: $device"
            else
                echo deadline > "/sys/block/$device/queue/scheduler" 2>/dev/null || true
                log "Set deadline scheduler for HDD: $device"
            fi
        fi
        
        # Optimize queue depth
        if [[ -f "/sys/block/$device/queue/nr_requests" ]]; then
            echo 256 > "/sys/block/$device/queue/nr_requests"
        fi
        
        # Optimize read-ahead
        if [[ -f "/sys/block/$device/queue/read_ahead_kb" ]]; then
            echo 128 > "/sys/block/$device/queue/read_ahead_kb"
        fi
    done
    
    success "I/O settings optimized"
}

# Optimize network settings
optimize_network() {
    log "Optimizing network settings..."
    
    # Optimize network interfaces
    for interface in $(ip link show | grep -E '^[0-9]+:' | cut -d: -f2 | tr -d ' ' | grep -v lo); do
        # Increase ring buffer sizes if supported
        ethtool -G "$interface" rx 4096 tx 4096 2>/dev/null || true
        
        # Enable hardware offloading if supported
        ethtool -K "$interface" gso on gro on tso on 2>/dev/null || true
        
        log "Optimized network interface: $interface"
    done
    
    success "Network settings optimized"
}

# Setup performance monitoring
setup_monitoring() {
    log "Setting up performance monitoring..."
    
    # Create performance monitoring script
    cat > /usr/local/bin/microvm-perf-monitor << 'EOF'
#!/bin/bash

# MicroVM Service Performance Monitor
# This script collects performance metrics

METRICS_DIR="/var/log/microvm-service/metrics"
mkdir -p "$METRICS_DIR"

# Collect system metrics
{
    echo "Timestamp: $(date)"
    echo "Load Average: $(cat /proc/loadavg)"
    echo "Memory Usage: $(free -m | grep Mem)"
    echo "CPU Usage: $(top -bn1 | grep "Cpu(s)" | sed "s/.*, *\([0-9.]*\)%* id.*/\1/" | awk '{print 100 - $1"%"}')"
    echo "Disk Usage: $(df -h / | tail -1)"
    echo "Network Connections: $(ss -s)"
    echo "MicroVM Service Status: $(systemctl is-active microvm-service)"
    echo "---"
} >> "$METRICS_DIR/system-metrics-$(date +%Y%m%d).log"

# Collect firecracker specific metrics
if pgrep firecracker > /dev/null; then
    {
        echo "Firecracker Processes: $(pgrep firecracker | wc -l)"
        echo "Firecracker Memory: $(ps -o pid,vsz,rss,comm -C firecracker --no-headers | awk '{sum+=$3} END {print sum " KB"}')"
    } >> "$METRICS_DIR/firecracker-metrics-$(date +%Y%m%d).log"
fi

# Rotate logs older than 7 days
find "$METRICS_DIR" -name "*.log" -mtime +7 -delete
EOF
    
    chmod +x /usr/local/bin/microvm-perf-monitor
    
    # Create cron job for monitoring
    cat > /etc/cron.d/microvm-perf-monitor << 'EOF'
# MicroVM Service Performance Monitoring
*/5 * * * * root /usr/local/bin/microvm-perf-monitor
EOF
    
    success "Performance monitoring setup completed"
}

# Create performance tuning service
create_tuning_service() {
    log "Creating performance tuning service..."
    
    # Create tuning script
    cat > /usr/local/bin/microvm-tune-system << 'EOF'
#!/bin/bash

# Apply runtime performance tunings

# Set CPU governor
for cpu in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    if [[ -w "$cpu" ]]; then
        echo performance > "$cpu"
    fi
done

# Optimize network queues
for interface in $(ip link show | grep -E '^[0-9]+:' | cut -d: -f2 | tr -d ' ' | grep -v lo); do
    ethtool -G "$interface" rx 4096 tx 4096 2>/dev/null || true
    ethtool -K "$interface" gso on gro on tso on 2>/dev/null || true
done

# Set I/O schedulers
for device in $(lsblk -d -n -o NAME | grep -E '^(sd|nvme|vd)'); do
    if [[ -f "/sys/block/$device/queue/scheduler" ]]; then
        if [[ $(cat "/sys/block/$device/queue/rotational") == "0" ]]; then
            echo mq-deadline > "/sys/block/$device/queue/scheduler" 2>/dev/null || true
        else
            echo deadline > "/sys/block/$device/queue/scheduler" 2>/dev/null || true
        fi
    fi
done
EOF
    
    chmod +x /usr/local/bin/microvm-tune-system
    
    # Create systemd service
    cat > /etc/systemd/system/microvm-tuning.service << 'EOF'
[Unit]
Description=MicroVM Service Performance Tuning
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/microvm-tune-system
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
    
    systemctl daemon-reload
    systemctl enable microvm-tuning.service
    systemctl start microvm-tuning.service
    
    success "Performance tuning service created and enabled"
}

# Validate optimizations
validate_optimizations() {
    log "Validating performance optimizations..."
    
    # Check sysctl settings
    if sysctl net.core.somaxconn | grep -q 65535; then
        success "Network optimizations applied"
    else
        warning "Network optimizations may not be fully applied"
    fi
    
    # Check file limits
    if [[ $(ulimit -n) -ge 65536 ]]; then
        success "File descriptor limits optimized"
    else
        warning "File descriptor limits may need adjustment"
    fi
    
    # Check CPU governor
    if [[ -f /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor ]]; then
        governor=$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor)
        if [[ "$governor" == "performance" ]]; then
            success "CPU governor set to performance"
        else
            warning "CPU governor is set to: $governor"
        fi
    fi
    
    success "Optimization validation completed"
}

# Generate performance report
generate_report() {
    log "Generating performance report..."
    
    report_file="/tmp/microvm-performance-report-$(date +%Y%m%d-%H%M%S).txt"
    
    {
        echo "MicroVM Service Performance Report"
        echo "Generated: $(date)"
        echo "========================================"
        echo
        
        echo "System Information:"
        echo "  OS: $(lsb_release -d 2>/dev/null | cut -f2 || echo "Unknown")"
        echo "  Kernel: $(uname -r)"
        echo "  CPU: $(lscpu | grep "Model name" | cut -d: -f2 | xargs)"
        echo "  Memory: $(free -h | grep Mem | awk '{print $2}')"
        echo "  Storage: $(df -h / | tail -1 | awk '{print $2}')"
        echo
        
        echo "Performance Settings:"
        echo "  CPU Governor: $(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo "N/A")"
        echo "  Swappiness: $(sysctl vm.swappiness | cut -d= -f2 | xargs)"
        echo "  File Max: $(sysctl fs.file-max | cut -d= -f2 | xargs)"
        echo "  TCP Congestion Control: $(sysctl net.ipv4.tcp_congestion_control | cut -d= -f2 | xargs)"
        echo
        
        echo "Current Load:"
        echo "  Load Average: $(cat /proc/loadavg)"
        echo "  Memory Usage: $(free | grep Mem | awk '{printf "%.1f%%", $3/$2 * 100.0}')"
        echo "  Disk Usage: $(df / | tail -1 | awk '{print $5}')"
        echo
        
        echo "Network Interfaces:"
        ip link show | grep -E '^[0-9]+:' | while read line; do
            interface=$(echo "$line" | cut -d: -f2 | xargs)
            if [[ "$interface" != "lo" ]]; then
                echo "  $interface: $(ethtool "$interface" 2>/dev/null | grep Speed | cut -d: -f2 | xargs || echo "Unknown speed")"
            fi
        done
        echo
        
        echo "Storage Devices:"
        lsblk -d -o NAME,SIZE,TYPE,MODEL | grep -E '^(sd|nvme|vd)'
        echo
        
        echo "Optimization Status:"
        echo "  Sysctl optimizations: $(test -f /etc/sysctl.d/99-microvm-service.conf && echo "Applied" || echo "Not applied")"
        echo "  Systemd limits: $(test -f /etc/systemd/system.conf.d/microvm-limits.conf && echo "Applied" || echo "Not applied")"
        echo "  Performance monitoring: $(test -f /etc/cron.d/microvm-perf-monitor && echo "Enabled" || echo "Disabled")"
        echo "  Tuning service: $(systemctl is-enabled microvm-tuning.service 2>/dev/null || echo "Not enabled")"
        
    } > "$report_file"
    
    success "Performance report generated: $report_file"
    
    # Display summary
    echo
    log "Performance Report Summary:"
    cat "$report_file"
}

# Main function
main() {
    log "Starting MicroVM Service performance tuning..."
    
    check_root
    optimize_kernel_params
    optimize_systemd_limits
    optimize_cpu
    optimize_memory
    optimize_io
    optimize_network
    setup_monitoring
    create_tuning_service
    validate_optimizations
    generate_report
    
    success "Performance tuning completed!"
    
    log "Recommendations:"
    log "  1. Reboot the system to ensure all optimizations take effect"
    log "  2. Monitor performance metrics in /var/log/microvm-service/metrics/"
    log "  3. Adjust settings based on your specific workload requirements"
    log "  4. Consider enabling CPU idle state disabling for ultra-low latency"
    
    warning "Some optimizations may require a system reboot to take full effect"
}

# Run main function
main "$@"