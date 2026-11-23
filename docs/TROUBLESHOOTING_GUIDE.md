# MicroVM Service Troubleshooting Guide

## Overview

This guide provides comprehensive troubleshooting procedures for common issues encountered with the MicroVM Service. It includes diagnostic steps, common solutions, and escalation procedures.

## Quick Diagnostic Commands

### Service Health Check
```bash
# Check service status
sudo systemctl status microvm-service

# Test API endpoint
curl -f http://localhost:8080/health || echo "API not responding"

# Check resource availability
curl -s http://localhost:8080/api/v1/resources/availability | jq '.total_available'

# Verify firecracker installation
firecracker --version && echo "Firecracker OK" || echo "Firecracker missing"

# Check LXD connectivity
lxc list > /dev/null && echo "LXD OK" || echo "LXD connection failed"
```

### Log Analysis
```bash
# Recent service logs
sudo journalctl -u microvm-service -n 50 --no-pager

# Error logs only
sudo journalctl -u microvm-service -p err --no-pager

# Follow live logs
sudo journalctl -u microvm-service -f

# Application logs
tail -f /var/log/microvm-service/service.log | grep -i error
```

## Common Issues and Solutions

### 1. Service Startup Issues

#### Issue: Service fails to start
**Symptoms:**
- `systemctl status microvm-service` shows failed state
- Error in logs: "Failed to bind to address"

**Diagnostic Steps:**
```bash
# Check if port is already in use
sudo netstat -tlnp | grep :8080

# Verify configuration file
microvm-service --config=/etc/microvm-service/config.json --validate

# Check file permissions
ls -la /etc/microvm-service/config.json
ls -la /var/lib/microvm-service
```

**Solutions:**
```bash
# Kill process using port 8080
sudo lsof -ti:8080 | xargs sudo kill -9

# Fix configuration file permissions
sudo chown microvm-service:microvm-service /etc/microvm-service/config.json
sudo chmod 644 /etc/microvm-service/config.json

# Fix data directory permissions
sudo chown -R microvm-service:microvm-service /var/lib/microvm-service
sudo chmod -R 755 /var/lib/microvm-service

# Restart service
sudo systemctl restart microvm-service
```

#### Issue: Configuration validation errors
**Symptoms:**
- Service fails to start with "Invalid configuration" error
- Missing required configuration fields

**Diagnostic Steps:**
```bash
# Validate configuration syntax
jq . /etc/microvm-service/config.json

# Check for required fields
grep -E "(server|firecracker|lxd)" /etc/microvm-service/config.json
```

**Solutions:**
```bash
# Copy default configuration
sudo cp /usr/share/microvm-service/config.json.example /etc/microvm-service/config.json

# Edit configuration with proper values
sudo nano /etc/microvm-service/config.json

# Validate configuration
microvm-service --config=/etc/microvm-service/config.json --validate
```

### 2. Firecracker Integration Issues

#### Issue: Firecracker binary not found
**Symptoms:**
- Error: "firecracker binary not found at /usr/local/bin/firecracker"
- MicroVM creation fails immediately

**Diagnostic Steps:**
```bash
# Check firecracker installation
which firecracker
firecracker --version

# Verify binary permissions
ls -la /usr/local/bin/firecracker
```

**Solutions:**
```bash
# Install firecracker
wget https://github.com/firecracker-microvm/firecracker/releases/latest/download/firecracker-v1.13.1-x86_64.tgz
tar -xzf firecracker-v1.13.1-x86_64.tgz
sudo mv release-v1.13.1-x86_64/firecracker-v1.13.1-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

# Update configuration path if needed
sudo nano /etc/microvm-service/config.json
# Set "binary_path": "/usr/local/bin/firecracker"
```

#### Issue: KVM device access denied
**Symptoms:**
- Error: "Failed to open /dev/kvm: Permission denied"
- MicroVM creation fails with virtualization error

**Diagnostic Steps:**
```bash
# Check KVM device permissions
ls -la /dev/kvm

# Verify user group membership
groups microvm-service

# Check if KVM is loaded
lsmod | grep kvm
```

**Solutions:**
```bash
# Add user to kvm group
sudo usermod -a -G kvm microvm-service

# Set proper permissions on KVM device
sudo chmod 666 /dev/kvm

# Load KVM modules if not loaded
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd for AMD processors

# Restart service
sudo systemctl restart microvm-service
```

#### Issue: Firecracker socket creation fails
**Symptoms:**
- Error: "Failed to create firecracker socket"
- Socket directory permission errors

**Diagnostic Steps:**
```bash
# Check socket directory
ls -la /var/run/firecracker-containerd/

# Verify containerd is running
sudo systemctl status containerd

# Check for socket conflicts
sudo lsof /var/run/firecracker-containerd/*.sock
```

**Solutions:**
```bash
# Create socket directory
sudo mkdir -p /var/run/firecracker-containerd
sudo chown microvm-service:microvm-service /var/run/firecracker-containerd
sudo chmod 755 /var/run/firecracker-containerd

# Clean up old sockets
sudo rm -f /var/run/firecracker-containerd/*.sock

# Restart containerd and service
sudo systemctl restart containerd
sudo systemctl restart microvm-service
```

### 3. LXD Integration Issues

#### Issue: LXD connection failed
**Symptoms:**
- Error: "Failed to connect to LXD daemon"
- Resource monitoring not working

**Diagnostic Steps:**
```bash
# Check LXD status
sudo systemctl status lxd

# Test LXD connection
lxc list

# Check socket permissions
ls -la /var/snap/lxd/common/lxd/unix.socket
```

**Solutions:**
```bash
# Add user to lxd group
sudo usermod -a -G lxd microvm-service

# Restart LXD service
sudo systemctl restart lxd

# Initialize LXD if needed
sudo lxd init --auto

# Update socket path in configuration
sudo nano /etc/microvm-service/config.json
# Verify "socket_path": "/var/snap/lxd/common/lxd/unix.socket"
```

#### Issue: Resource monitoring inaccurate
**Symptoms:**
- Incorrect resource availability reported
- Resource conflicts not detected

**Diagnostic Steps:**
```bash
# Check LXD resource usage
lxc info

# Compare with service reporting
curl -s http://localhost:8080/api/v1/resources/availability | jq

# Check monitoring intervals
grep -A5 "resource_management" /etc/microvm-service/config.json
```

**Solutions:**
```bash
# Restart resource monitoring
curl -X POST http://localhost:8080/api/v1/resources/refresh

# Adjust monitoring intervals
sudo nano /etc/microvm-service/config.json
# Set shorter intervals:
# "monitoring_interval": "15s"
# "availability_update_interval": "30s"

# Restart service
sudo systemctl restart microvm-service
```

### 4. Network Configuration Issues

#### Issue: Network bridge not created
**Symptoms:**
- Error: "Network bridge br-microvm not found"
- MicroVM network configuration fails

**Diagnostic Steps:**
```bash
# Check bridge existence
ip link show br-microvm

# Check bridge configuration
ip addr show br-microvm

# Verify iptables rules
sudo iptables -t nat -L | grep microvm
```

**Solutions:**
```bash
# Create network bridge
sudo ip link add name br-microvm type bridge
sudo ip addr add 192.168.100.1/24 dev br-microvm
sudo ip link set br-microvm up

# Configure NAT rules
sudo iptables -t nat -A POSTROUTING -s 192.168.100.0/24 -o eth0 -j MASQUERADE
sudo iptables -A FORWARD -i br-microvm -o eth0 -j ACCEPT
sudo iptables -A FORWARD -i eth0 -o br-microvm -m state --state RELATED,ESTABLISHED -j ACCEPT

# Save iptables rules
sudo iptables-save | sudo tee /etc/iptables/rules.v4

# Run bridge setup script
sudo /usr/share/microvm-service/scripts/setup-nat-bridge.sh
```

#### Issue: DHCP not working for microVMs
**Symptoms:**
- MicroVMs don't receive IP addresses
- Network connectivity fails

**Diagnostic Steps:**
```bash
# Check dnsmasq status
sudo systemctl status dnsmasq

# Monitor DHCP requests
sudo tcpdump -i br-microvm port 67 or port 68

# Check dnsmasq configuration
cat /etc/dnsmasq.d/microvm-dhcp.conf
```

**Solutions:**
```bash
# Install and configure dnsmasq
sudo apt-get install dnsmasq

# Create DHCP configuration
sudo tee /etc/dnsmasq.d/microvm-dhcp.conf << EOF
interface=br-microvm
dhcp-range=192.168.100.10,192.168.100.200,12h
dhcp-option=3,192.168.100.1
dhcp-option=6,8.8.8.8,8.8.4.4
EOF

# Restart dnsmasq
sudo systemctl restart dnsmasq
sudo systemctl enable dnsmasq
```

#### Issue: SSH access to microVMs fails
**Symptoms:**
- Cannot SSH to microVM IP addresses
- Connection timeout or refused

**Diagnostic Steps:**
```bash
# Check microVM status
curl -s http://localhost:8080/api/v1/microvms | jq '.[] | {id, status, network}'

# Test network connectivity
ping -c 3 192.168.100.10

# Check SSH service in microVM
# (requires console access or cloud-init logs)
```

**Solutions:**
```bash
# Verify SSH keys are properly injected
curl -s http://localhost:8080/api/v1/microvms/VM_ID | jq '.network.public_key'

# Check firewall rules
sudo iptables -L | grep 22

# Test with different SSH key
ssh -i /path/to/private/key root@192.168.100.10

# Check microVM console logs
sudo cat /var/lib/microvm-service/firecracker-work/VM_ID/console.log
```

### 5. Storage Issues

#### Issue: Storage provisioning fails
**Symptoms:**
- Error: "Failed to create storage volume"
- MicroVM creation fails at storage step

**Diagnostic Steps:**
```bash
# Check storage directory
ls -la /var/lib/microvm-service/storage/

# Check disk space
df -h /var/lib/microvm-service

# Verify storage configuration
grep -A10 "storage" /etc/microvm-service/config.json
```

**Solutions:**
```bash
# Create storage directory
sudo mkdir -p /var/lib/microvm-service/storage
sudo chown microvm-service:microvm-service /var/lib/microvm-service/storage

# Clean up old storage volumes
sudo find /var/lib/microvm-service/storage -name "*.img" -mtime +30 -delete

# Check and free disk space
sudo apt-get autoremove
sudo apt-get autoclean
```

#### Issue: S3 storage integration fails
**Symptoms:**
- Error: "S3 authentication failed"
- S3-backed storage not mounting

**Diagnostic Steps:**
```bash
# Test S3 credentials
aws s3 ls s3://your-bucket-name

# Check s3fs installation
which s3fs
s3fs --version

# Verify S3 configuration
grep -A10 "s3" /etc/microvm-service/config.json
```

**Solutions:**
```bash
# Install s3fs-fuse
sudo apt-get install s3fs

# Configure S3 credentials
echo "ACCESS_KEY:SECRET_KEY" | sudo tee /etc/passwd-s3fs
sudo chmod 600 /etc/passwd-s3fs

# Test S3 mount
sudo mkdir -p /mnt/s3-test
sudo s3fs your-bucket-name /mnt/s3-test -o passwd_file=/etc/passwd-s3fs

# Update service configuration with correct S3 settings
sudo nano /etc/microvm-service/config.json
```

### 6. Performance Issues

#### Issue: Slow microVM provisioning
**Symptoms:**
- MicroVM creation takes longer than 30 seconds
- Timeout errors during provisioning

**Diagnostic Steps:**
```bash
# Monitor provisioning time
time curl -X POST http://localhost:8080/api/v1/microvms -d '{"resource_specs":{"vcpus":1,"memory_mb":512,"storage_mb":1024}}'

# Check system load
top
iostat 1 5

# Monitor firecracker startup
sudo strace -p $(pgrep firecracker) -e trace=file
```

**Solutions:**
```bash
# Optimize kernel image
# Use smaller, optimized kernel
sudo cp /path/to/optimized/vmlinux /var/lib/microvm-service/vmlinux

# Pre-create storage volumes
sudo /usr/share/microvm-service/scripts/precreate-volumes.sh

# Increase resource limits
sudo systemctl edit microvm-service
# Add:
# [Service]
# LimitNOFILE=65536
# LimitNPROC=8192

# Tune system parameters
echo 'vm.swappiness=10' | sudo tee -a /etc/sysctl.conf
echo 'net.core.somaxconn=1024' | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

#### Issue: High memory usage
**Symptoms:**
- Service consuming excessive memory
- System running out of memory

**Diagnostic Steps:**
```bash
# Check memory usage
free -h
ps aux | grep microvm-service
pmap $(pgrep microvm-service)

# Monitor memory over time
watch -n 5 'free -h && ps aux | grep microvm-service'
```

**Solutions:**
```bash
# Tune garbage collection
export GOGC=50  # More aggressive GC

# Limit concurrent operations
# Edit configuration to reduce concurrent microVM operations
sudo nano /etc/microvm-service/config.json

# Add memory limits to systemd service
sudo systemctl edit microvm-service
# Add:
# [Service]
# MemoryLimit=2G

# Restart service
sudo systemctl restart microvm-service
```

### 7. API and Client Issues

#### Issue: API requests timing out
**Symptoms:**
- Client requests hang or timeout
- HTTP 504 Gateway Timeout errors

**Diagnostic Steps:**
```bash
# Test API response time
time curl -w "@curl-format.txt" http://localhost:8080/health

# Check active connections
sudo netstat -an | grep :8080

# Monitor API logs
tail -f /var/log/microvm-service/service.log | grep -i "request\|response"
```

**Solutions:**
```bash
# Increase timeout values
sudo nano /etc/microvm-service/config.json
# Update server timeouts:
# "read_timeout": "60s"
# "write_timeout": "60s"

# Optimize connection handling
# Add connection pooling configuration
# "max_connections": 1000
# "idle_timeout": "30s"

# Restart service
sudo systemctl restart microvm-service
```

#### Issue: Authentication failures
**Symptoms:**
- HTTP 401 Unauthorized errors
- API key validation fails

**Diagnostic Steps:**
```bash
# Check API key format
echo "API_KEY" | base64 -d

# Verify API key file
ls -la /etc/microvm-service/api-keys/
cat /etc/microvm-service/api-keys/admin.key

# Test authentication
curl -H "Authorization: Bearer $(cat /etc/microvm-service/api-keys/admin.key)" http://localhost:8080/api/v1/microvms
```

**Solutions:**
```bash
# Regenerate API keys
openssl rand -hex 32 | sudo tee /etc/microvm-service/api-keys/admin.key
sudo chmod 600 /etc/microvm-service/api-keys/admin.key

# Update client configuration
# Ensure clients use correct API key format

# Restart service to reload keys
sudo systemctl restart microvm-service
```

## Advanced Troubleshooting

### Debug Mode

#### Enable Debug Logging
```bash
# Temporary debug mode
sudo systemctl edit microvm-service
# Add:
# [Service]
# Environment="LOG_LEVEL=debug"

# Or edit configuration file
sudo nano /etc/microvm-service/config.json
# Set: "log_level": "debug"

sudo systemctl restart microvm-service
```

#### Trace Network Issues
```bash
# Monitor network traffic
sudo tcpdump -i br-microvm -w /tmp/microvm-traffic.pcap

# Analyze with wireshark
wireshark /tmp/microvm-traffic.pcap

# Monitor iptables
sudo iptables -I FORWARD -j LOG --log-prefix "MICROVM-FORWARD: "
sudo tail -f /var/log/kern.log | grep MICROVM-FORWARD
```

#### Profile Performance
```bash
# Enable Go profiling
curl http://localhost:8080/debug/pprof/profile?seconds=30 > cpu.prof

# Analyze with go tool
go tool pprof cpu.prof

# Memory profiling
curl http://localhost:8080/debug/pprof/heap > mem.prof
go tool pprof mem.prof
```

### System-Level Debugging

#### Check System Resources
```bash
# Monitor system calls
sudo strace -p $(pgrep microvm-service) -f -e trace=network,file

# Check file descriptors
sudo lsof -p $(pgrep microvm-service)

# Monitor disk I/O
sudo iotop -p $(pgrep microvm-service)

# Check cgroup limits
cat /sys/fs/cgroup/memory/system.slice/microvm-service.service/memory.usage_in_bytes
```

#### Kernel-Level Issues
```bash
# Check kernel messages
dmesg | grep -i "firecracker\|kvm\|microvm"

# Monitor kernel modules
lsmod | grep -E "kvm|tun|bridge"

# Check virtualization support
cat /proc/cpuinfo | grep -E "vmx|svm"
```

## Recovery Procedures

### Service Recovery

#### Graceful Recovery
```bash
# Stop service gracefully
sudo systemctl stop microvm-service

# Clean up resources
sudo pkill -f firecracker
sudo ip link delete br-microvm 2>/dev/null || true

# Restart dependencies
sudo systemctl restart lxd
sudo systemctl restart containerd

# Start service
sudo systemctl start microvm-service
```

#### Emergency Recovery
```bash
# Force kill all processes
sudo pkill -9 -f microvm-service
sudo pkill -9 -f firecracker

# Clean up network
sudo ip link delete br-microvm 2>/dev/null || true
sudo iptables -t nat -F
sudo iptables -F FORWARD

# Reset to known good state
sudo cp /etc/microvm-service/config.json.backup /etc/microvm-service/config.json
sudo systemctl restart microvm-service
```

### Data Recovery

#### Backup Recovery
```bash
# Restore from backup
sudo systemctl stop microvm-service
sudo cp -r /var/backups/microvm-service/latest/* /var/lib/microvm-service/
sudo chown -R microvm-service:microvm-service /var/lib/microvm-service
sudo systemctl start microvm-service
```

#### MicroVM Recovery
```bash
# List orphaned microVMs
sudo find /var/lib/microvm-service/firecracker-work -name "*.pid" -exec test ! -d /proc/$(cat {}) \; -print

# Clean up orphaned resources
sudo /usr/share/microvm-service/scripts/cleanup-orphaned.sh

# Rebuild microVM database
curl -X POST http://localhost:8080/api/v1/admin/rebuild-database
```

## Escalation Procedures

### When to Escalate

1. **Critical Service Outage**: Service down for more than 15 minutes
2. **Data Loss**: MicroVM data corruption or loss
3. **Security Incident**: Unauthorized access or security breach
4. **Resource Exhaustion**: System resources critically low
5. **Multiple Node Failure**: More than 50% of nodes affected

### Escalation Contacts

#### Internal Team
- **Level 1**: Operations Team (ops@company.com)
- **Level 2**: Engineering Team (engineering@company.com)
- **Level 3**: Architecture Team (architecture@company.com)

#### External Support
- **LXD Support**: Canonical Support
- **Firecracker Issues**: AWS Firecracker GitHub Issues
- **Infrastructure**: Cloud Provider Support

### Information to Collect

#### System Information
```bash
# System details
uname -a
lsb_release -a
free -h
df -h

# Service information
systemctl status microvm-service
journalctl -u microvm-service -n 100 --no-pager

# Configuration
sudo tar -czf /tmp/microvm-config.tar.gz /etc/microvm-service/

# Logs
sudo tar -czf /tmp/microvm-logs.tar.gz /var/log/microvm-service/
```

#### Performance Data
```bash
# Resource usage
top -b -n 1
iostat -x 1 5
netstat -i

# Network configuration
ip addr show
ip route show
iptables -t nat -L
```

## Prevention and Monitoring

### Proactive Monitoring

#### Health Checks
```bash
# Automated health check script
#!/bin/bash
# /usr/local/bin/microvm-health-check.sh

# Check service status
if ! systemctl is-active --quiet microvm-service; then
    echo "CRITICAL: MicroVM service is not running"
    exit 2
fi

# Check API response
if ! curl -f -s http://localhost:8080/health > /dev/null; then
    echo "CRITICAL: API not responding"
    exit 2
fi

# Check resource availability
AVAILABLE=$(curl -s http://localhost:8080/api/v1/resources/availability | jq -r '.total_available.cpu')
if [ "$AVAILABLE" -lt 1 ]; then
    echo "WARNING: Low CPU resources available"
    exit 1
fi

echo "OK: All checks passed"
exit 0
```

#### Monitoring Alerts
```yaml
# prometheus-alerts.yml
groups:
  - name: microvm-service
    rules:
      - alert: MicroVMServiceDown
        expr: up{job="microvm-service"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "MicroVM Service is down"
          
      - alert: HighResourceUsage
        expr: resource_utilization_cpu > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High CPU resource usage"
          
      - alert: MicroVMCreationFailing
        expr: rate(microvm_creation_failures_total[5m]) > 0.1
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "High rate of microVM creation failures"
```

### Best Practices

#### Regular Maintenance
- Monitor logs daily for errors and warnings
- Update system packages weekly
- Review resource usage trends monthly
- Test backup and recovery procedures quarterly

#### Configuration Management
- Use version control for configuration files
- Test configuration changes in staging environment
- Document all configuration modifications
- Maintain configuration backups

#### Capacity Planning
- Monitor resource usage trends
- Plan for peak usage periods
- Scale infrastructure proactively
- Maintain adequate resource buffers

## Additional Resources

### Documentation Links
- [Deployment Guide](DEPLOYMENT_GUIDE.md)
- [API Documentation](API_DOCUMENTATION.md)
- [Monitoring Guide](MONITORING_GUIDE.md)
- [Security Guide](SECURITY_GUIDE.md)

### External Resources
- [Firecracker Documentation](https://firecracker-microvm.github.io/)
- [LXD Documentation](https://linuxcontainers.org/lxd/docs/master/)
- [Containerd Documentation](https://containerd.io/docs/)

### Support Channels
- GitHub Issues: https://github.com/your-org/microvm-service/issues
- Community Forum: https://community.microvm-service.com
- Documentation: https://docs.microvm-service.com