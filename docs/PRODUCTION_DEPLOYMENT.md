# MicroVM Service Production Deployment Guide

This guide covers the deployment and configuration of the MicroVM Service for production environments.

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Performance Optimization](#performance-optimization)
3. [Production Deployment](#production-deployment)
4. [Configuration Management](#configuration-management)
5. [Backup and Recovery](#backup-and-recovery)
6. [Monitoring and Maintenance](#monitoring-and-maintenance)
7. [Troubleshooting](#troubleshooting)

## Prerequisites

### System Requirements

- **Operating System**: Ubuntu 20.04+ or CentOS 8+
- **CPU**: Minimum 4 cores, recommended 8+ cores
- **Memory**: Minimum 8GB RAM, recommended 16GB+
- **Storage**: Minimum 100GB SSD, recommended 500GB+ NVMe
- **Network**: Gigabit Ethernet or better

### Required Software

- **Firecracker**: v1.0.0 or later
- **containerd**: v1.6.0 or later
- **firecracker-containerd**: Latest version
- **LXD**: v5.0+ (for resource monitoring)
- **Go**: v1.19+ (for building from source)

### Installation Commands

```bash
# Install firecracker
wget https://github.com/firecracker-microvm/firecracker/releases/latest/download/firecracker-v1.13.1-x86_64.tgz
tar -xzf firecracker-v1.13.1-x86_64.tgz
sudo cp release-v1.13.1-x86_64/firecracker-v1.13.1-x86_64 /usr/local/bin/firecracker
sudo cp release-v1.13.1-x86_64/jailer-v1.13.1-x86_64 /usr/local/bin/jailer
sudo chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer

# Install containerd
sudo apt-get update
sudo apt-get install -y containerd

# Install firecracker-containerd
# Follow instructions at: https://github.com/firecracker-microvm/firecracker-containerd
```

## Performance Optimization

### Automatic Performance Tuning

Run the performance tuning script to optimize system settings:

```bash
sudo ./scripts/performance-tuning.sh
```

This script will:
- Optimize kernel parameters for network and I/O performance
- Configure CPU governor for maximum performance
- Set appropriate system limits
- Configure storage I/O schedulers
- Enable performance monitoring

### Manual Optimizations

#### Kernel Parameters

Add to `/etc/sysctl.d/99-microvm-service.conf`:

```ini
# Network optimizations
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.core.somaxconn = 65535
net.ipv4.tcp_congestion_control = bbr

# File descriptor limits
fs.file-max = 2097152

# Virtual memory optimizations
vm.swappiness = 10
vm.dirty_ratio = 15
```

#### CPU Optimization

```bash
# Set CPU governor to performance
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor

# Disable CPU idle states for ultra-low latency (optional)
echo 1 | sudo tee /sys/devices/system/cpu/cpu*/cpuidle/state*/disable
```

#### Memory Optimization

```bash
# Configure transparent huge pages
echo madvise | sudo tee /sys/kernel/mm/transparent_hugepage/enabled
echo madvise | sudo tee /sys/kernel/mm/transparent_hugepage/defrag

# Disable NUMA balancing for consistent performance
echo 0 | sudo tee /proc/sys/kernel/numa_balancing
```

## Production Deployment

### Automated Deployment

Use the deployment script for automated installation:

```bash
# Make the script executable
chmod +x ./scripts/deploy-production.sh

# Run deployment (requires root)
sudo ./scripts/deploy-production.sh
```

### Manual Deployment Steps

1. **Build the service**:
   ```bash
   go build -o microvm-service ./cmd/main.go
   ```

2. **Create system user**:
   ```bash
   sudo useradd --system --home-dir /var/lib/microvm-service microvm
   ```

3. **Create directories**:
   ```bash
   sudo mkdir -p /opt/microvm-service
   sudo mkdir -p /etc/microvm-service
   sudo mkdir -p /var/lib/microvm-service
   sudo mkdir -p /var/log/microvm-service
   ```

4. **Install binary and configuration**:
   ```bash
   sudo cp microvm-service /opt/microvm-service/
   sudo cp configs/production.json /etc/microvm-service/config.json
   sudo chown -R microvm:microvm /var/lib/microvm-service
   ```

5. **Create systemd service**:
   ```bash
   sudo cp scripts/microvm-service.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable microvm-service
   ```

### Service Configuration

The production configuration includes optimized settings:

- **Startup timeout**: 30 seconds maximum
- **Resource monitoring**: 15-second intervals
- **Connection pooling**: Up to 100 concurrent connections
- **Caching**: 15-minute TTL with 2000 item capacity
- **Parallel startup**: Up to 10 concurrent microVM starts

## Configuration Management

### Configuration Files

- **Main config**: `/etc/microvm-service/config.json`
- **Performance config**: `/etc/microvm-service/performance.json`
- **Backup config**: `/etc/microvm-service/backup.json`

### Dynamic Configuration Reload

The service supports configuration hot-reloading:

```bash
# Reload configuration without restart
sudo systemctl reload microvm-service

# Or send SIGHUP signal
sudo kill -HUP $(pgrep microvm-service)
```

### Configuration Validation

Validate configuration before applying:

```bash
/opt/microvm-service/microvm-service -config /etc/microvm-service/config.json -validate
```

### Environment-Specific Configurations

Create different configurations for different environments:

```bash
# Development
cp configs/local-dev.json /etc/microvm-service/config-dev.json

# Staging
cp configs/production.json /etc/microvm-service/config-staging.json

# Production
cp configs/production.json /etc/microvm-service/config-prod.json
```

## Backup and Recovery

### Automated Backups

The service includes automated backup functionality:

- **Full backups**: Daily at 2 AM
- **Configuration backups**: Before each deployment
- **Retention**: 30 days
- **Compression**: gzip compression enabled
- **Verification**: Automatic backup verification

### Manual Backup

Create manual backups:

```bash
# Create full backup
sudo systemctl stop microvm-service
sudo tar -czf /var/backups/microvm-service-manual-$(date +%Y%m%d).tar.gz \
    /etc/microvm-service \
    /var/lib/microvm-service \
    /opt/microvm-service
sudo systemctl start microvm-service
```

### Disaster Recovery

1. **Stop the service**:
   ```bash
   sudo systemctl stop microvm-service
   ```

2. **Restore from backup**:
   ```bash
   sudo tar -xzf /var/backups/microvm-service-backup.tar.gz -C /
   ```

3. **Verify configuration**:
   ```bash
   /opt/microvm-service/microvm-service -config /etc/microvm-service/config.json -validate
   ```

4. **Start the service**:
   ```bash
   sudo systemctl start microvm-service
   ```

### Backup Monitoring

Monitor backup status:

```bash
# Check backup logs
sudo journalctl -u microvm-service | grep -i backup

# List available backups
ls -la /var/backups/microvm-service/

# Verify backup integrity
sudo tar -tzf /var/backups/microvm-service/latest-backup.tar.gz > /dev/null
```

## Monitoring and Maintenance

### Service Monitoring

Monitor service health:

```bash
# Service status
sudo systemctl status microvm-service

# Service logs
sudo journalctl -u microvm-service -f

# Performance metrics
curl http://localhost:8081/metrics
```

### Performance Monitoring

The service includes built-in performance monitoring:

- **Startup times**: Average microVM startup time tracking
- **Resource utilization**: CPU, memory, and storage usage
- **Cache performance**: Hit rates and efficiency metrics
- **Connection pooling**: Usage and performance statistics

### Health Checks

Implement health checks:

```bash
# HTTP health check
curl -f http://localhost:8080/health || exit 1

# gRPC health check
grpc_health_probe -addr=localhost:9090

# Process health check
pgrep -f microvm-service > /dev/null || exit 1
```

### Log Management

Configure log rotation:

```bash
# Log rotation is automatically configured during deployment
# Logs are rotated daily and kept for 30 days
# Location: /var/log/microvm-service/
```

### Maintenance Tasks

Regular maintenance tasks:

1. **Update dependencies**:
   ```bash
   # Update firecracker
   sudo systemctl stop microvm-service
   # Download and install new version
   sudo systemctl start microvm-service
   ```

2. **Clean up old data**:
   ```bash
   # Clean up old microVM data
   sudo find /var/lib/microvm-service/firecracker-work -mtime +7 -delete
   
   # Clean up old logs
   sudo find /var/log/microvm-service -name "*.log" -mtime +30 -delete
   ```

3. **Performance tuning**:
   ```bash
   # Re-run performance tuning after system updates
   sudo ./scripts/performance-tuning.sh
   ```

## Troubleshooting

### Common Issues

#### Service Won't Start

1. Check configuration:
   ```bash
   /opt/microvm-service/microvm-service -config /etc/microvm-service/config.json -validate
   ```

2. Check permissions:
   ```bash
   sudo chown -R microvm:microvm /var/lib/microvm-service
   ```

3. Check dependencies:
   ```bash
   systemctl status firecracker-containerd
   systemctl status containerd
   ```

#### Poor Performance

1. Check system resources:
   ```bash
   top
   free -h
   df -h
   ```

2. Verify performance optimizations:
   ```bash
   cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
   sysctl net.core.somaxconn
   ```

3. Check microVM startup times:
   ```bash
   curl http://localhost:8081/metrics | grep startup_time
   ```

#### Network Issues

1. Check firewall rules:
   ```bash
   sudo ufw status
   sudo iptables -L
   ```

2. Verify network configuration:
   ```bash
   ip link show
   brctl show
   ```

3. Test connectivity:
   ```bash
   telnet localhost 8080
   telnet localhost 9090
   ```

### Debug Mode

Enable debug logging:

```bash
# Edit configuration
sudo nano /etc/microvm-service/config.json

# Set log_level to "debug"
{
  "server": {
    "log_level": "debug"
  }
}

# Restart service
sudo systemctl restart microvm-service
```

### Performance Analysis

Analyze performance issues:

```bash
# CPU profiling
curl http://localhost:8081/debug/pprof/profile > cpu.prof
go tool pprof cpu.prof

# Memory profiling
curl http://localhost:8081/debug/pprof/heap > mem.prof
go tool pprof mem.prof

# Trace analysis
curl http://localhost:8081/debug/pprof/trace > trace.out
go tool trace trace.out
```

### Support Information

When reporting issues, include:

1. **System information**:
   ```bash
   uname -a
   lsb_release -a
   free -h
   df -h
   ```

2. **Service logs**:
   ```bash
   sudo journalctl -u microvm-service --since "1 hour ago"
   ```

3. **Configuration**:
   ```bash
   sudo cat /etc/microvm-service/config.json
   ```

4. **Performance metrics**:
   ```bash
   curl http://localhost:8081/metrics
   ```

## Security Considerations

### Firewall Configuration

```bash
# Allow only necessary ports
sudo ufw allow 8080/tcp  # HTTP API
sudo ufw allow 9090/tcp  # gRPC API
sudo ufw allow 8081/tcp  # Metrics (restrict to monitoring network)
```

### SSL/TLS Configuration

Enable TLS in production:

```json
{
  "server": {
    "tls_enabled": true,
    "cert_file": "/etc/ssl/certs/microvm-service.crt",
    "key_file": "/etc/ssl/private/microvm-service.key"
  }
}
```

### Access Control

Implement proper access controls:

- Use firewall rules to restrict access
- Enable authentication for API endpoints
- Use service accounts with minimal privileges
- Regularly rotate certificates and keys

## Scaling Considerations

### Horizontal Scaling

For high availability:

1. Deploy multiple instances behind a load balancer
2. Use shared storage for microVM data
3. Implement distributed resource management
4. Configure health checks and failover

### Vertical Scaling

For single-node scaling:

1. Increase CPU and memory resources
2. Use faster storage (NVMe SSDs)
3. Optimize network configuration
4. Tune kernel parameters for higher loads

### Resource Planning

Plan resources based on expected load:

- **CPU**: 1 core per 10-20 microVMs
- **Memory**: 2GB base + microVM memory requirements
- **Storage**: 10GB base + microVM storage requirements
- **Network**: 1Gbps per 100 microVMs

This completes the production deployment guide. The service is now optimized for production use with proper monitoring, backup, and recovery procedures.