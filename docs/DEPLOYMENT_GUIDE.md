# MicroVM Service Deployment Guide

## Overview

This guide covers the complete deployment process for the MicroVM Service, from initial setup to production configuration. The service integrates with existing LXD infrastructure to provide Firecracker-based microVM capabilities.

## Prerequisites

### System Requirements

#### Hardware Requirements
- **CPU**: x86_64 architecture with virtualization support (Intel VT-x or AMD-V)
- **Memory**: Minimum 8GB RAM (16GB+ recommended for production)
- **Storage**: 50GB+ available disk space
- **Network**: Dedicated network interface for microVM networking

#### Software Requirements
- **Operating System**: Ubuntu 20.04+ or CentOS 8+
- **Kernel**: Linux kernel 4.14+ with KVM support
- **LXD**: Version 4.0+ already installed and configured
- **Docker**: Version 20.10+ (for containerized deployment)
- **Go**: Version 1.19+ (for source builds)
- **MySQL**: Version 8.0+ (required for Managed SQL service)

### MySQL Database Setup (Required for Managed SQL)

The Managed SQL service requires a MySQL database for storing instance metadata and firewall rules.

```bash
# Install MySQL Server
sudo apt update
sudo apt install -y mysql-server

# Secure the installation
sudo mysql_secure_installation

# Create the microvm database and tables
sudo mysql -e "CREATE DATABASE IF NOT EXISTS microvm;"

# Create required tables
sudo mysql microvm -e "
CREATE TABLE IF NOT EXISTS managed_sql (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    name VARCHAR(255) NOT NULL,
    region_id VARCHAR(50) NOT NULL,
    vm_id VARCHAR(50),
    public_ip VARCHAR(50),
    role VARCHAR(20) DEFAULT 'standalone',
    dr_primary_id VARCHAR(36),
    dr_replica_id VARCHAR(36),
    plan VARCHAR(50) NOT NULL,
    vcpus INT NOT NULL,
    memory_mb INT NOT NULL,
    disk_gb INT NOT NULL,
    price_per_hour DECIMAL(10,2),
    currency VARCHAR(10) DEFAULT 'IRR',
    sql_version VARCHAR(20),
    edition VARCHAR(50),
    sa_password_encrypted TEXT,
    status VARCHAR(50) DEFAULT 'pending',
    health VARCHAR(50) DEFAULT 'unknown',
    dns_name VARCHAR(255),
    external_id VARCHAR(255),
    dr_pair_id VARCHAR(36),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    INDEX idx_user_id (user_id),
    INDEX idx_status (status),
    INDEX idx_region (region_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sql_firewall_rules (
    id VARCHAR(36) PRIMARY KEY,
    instance_id VARCHAR(36) NOT NULL,
    ip_address VARCHAR(50) NOT NULL,
    port INT DEFAULT 1433,
    description VARCHAR(255),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_instance_id (instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"

# Create MySQL user for vmlet service (optional, can use root)
sudo mysql -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH mysql_native_password BY 'your-secure-password';"
sudo mysql -e "FLUSH PRIVILEGES;"
```

#### Configure vmlet Service Environment

Add the MySQL password to the vmlet systemd service:

```bash
# Create systemd override directory
sudo mkdir -p /etc/systemd/system/vmlet.service.d/

# Add MySQL password environment variable
sudo tee /etc/systemd/system/vmlet.service.d/override.conf << EOF
[Service]
Environment=MYSQL_PASSWORD=your-secure-password
EOF

# Reload systemd and restart service
sudo systemctl daemon-reload
sudo systemctl restart vmlet
```

#### Verify MySQL Connection

Check the service logs to confirm MySQL connection:

```bash
journalctl -u vmlet --since "1 minute ago" | grep -i mysql
# Expected output: ✅ MySQL connected: root@localhost:3306/microvm
```

### Network Prerequisites

#### Network Configuration
```bash
# Verify KVM support
lsmod | grep kvm

# Check virtualization support
egrep -c '(vmx|svm)' /proc/cpuinfo

# Verify network interfaces
ip link show
```

#### Firewall Configuration
```bash
# Allow gRPC service port
sudo ufw allow 8080/tcp

# Allow SSH access to microVMs (if using NAT)
sudo ufw allow 2200:2299/tcp

# Allow microVM network traffic
sudo ufw allow in on br-microvm
```

## Installation Methods

### Method 1: Binary Installation (Recommended)

#### Download and Install
```bash
# Download latest release
wget https://github.com/your-org/microvm-service/releases/latest/download/microvm-service-linux-amd64.tar.gz

# Extract binary
tar -xzf microvm-service-linux-amd64.tar.gz

# Install binary
sudo mv microvm-service /usr/local/bin/
sudo chmod +x /usr/local/bin/microvm-service

# Create service user
sudo useradd -r -s /bin/false microvm-service

# Create directories
sudo mkdir -p /etc/microvm-service
sudo mkdir -p /var/lib/microvm-service
sudo mkdir -p /var/log/microvm-service

# Set permissions
sudo chown -R microvm-service:microvm-service /var/lib/microvm-service
sudo chown -R microvm-service:microvm-service /var/log/microvm-service
```

#### Install Firecracker and Containerd
```bash
# Install Firecracker
wget https://github.com/firecracker-microvm/firecracker/releases/latest/download/firecracker-v1.13.1-x86_64.tgz
tar -xzf firecracker-v1.13.1-x86_64.tgz
sudo mv release-v1.13.1-x86_64/firecracker-v1.13.1-x86_64 /usr/local/bin/firecracker
sudo mv release-v1.13.1-x86_64/jailer-v1.13.1-x86_64 /usr/local/bin/jailer

# Install containerd
wget https://github.com/containerd/containerd/releases/latest/download/containerd-1.7.8-linux-amd64.tar.gz
sudo tar -C /usr/local -xzf containerd-1.7.8-linux-amd64.tar.gz

# Install firecracker-containerd
git clone https://github.com/firecracker-microvm/firecracker-containerd.git
cd firecracker-containerd
make
sudo make install
```

### Method 2: Docker Deployment

#### Docker Compose Configuration
```yaml
# docker-compose.yml
version: '3.8'

services:
  microvm-service:
    image: your-org/microvm-service:latest
    container_name: microvm-service
    restart: unless-stopped
    privileged: true
    network_mode: host
    volumes:
      - /etc/microvm-service:/etc/microvm-service:ro
      - /var/lib/microvm-service:/var/lib/microvm-service
      - /var/log/microvm-service:/var/log/microvm-service
      - /var/run/lxd:/var/run/lxd
      - /dev/kvm:/dev/kvm
      - /sys/fs/cgroup:/sys/fs/cgroup:ro
    environment:
      - CONFIG_PATH=/etc/microvm-service/config.json
    ports:
      - "8080:8080"
    depends_on:
      - firecracker-containerd

  firecracker-containerd:
    image: your-org/firecracker-containerd:latest
    container_name: firecracker-containerd
    restart: unless-stopped
    privileged: true
    network_mode: host
    volumes:
      - /var/run/firecracker-containerd:/var/run/firecracker-containerd
      - /var/lib/firecracker-containerd:/var/lib/firecracker-containerd
      - /dev/kvm:/dev/kvm
```

#### Deploy with Docker Compose
```bash
# Create configuration directory
sudo mkdir -p /etc/microvm-service

# Copy configuration files
sudo cp configs/production.json /etc/microvm-service/config.json

# Deploy services
docker-compose up -d

# Verify deployment
docker-compose ps
docker-compose logs microvm-service
```

### Method 3: Source Build

#### Build from Source
```bash
# Clone repository
git clone https://github.com/your-org/microvm-service.git
cd microvm-service

# Install dependencies
go mod download

# Build binary
make build

# Run tests
make test

# Install binary
sudo make install
```

## Configuration

### Main Configuration File

Create `/etc/microvm-service/config.json`:

```json
{
  "server": {
    "address": "0.0.0.0:8080",
    "tls": {
      "enabled": false,
      "cert_file": "/etc/microvm-service/tls/server.crt",
      "key_file": "/etc/microvm-service/tls/server.key"
    }
  },
  "firecracker": {
    "binary_path": "/usr/local/bin/firecracker",
    "jailer_path": "/usr/local/bin/jailer",
    "kernel_image_path": "/var/lib/microvm-service/vmlinux",
    "rootfs_path": "/var/lib/microvm-service/rootfs",
    "work_dir": "/var/lib/microvm-service/firecracker-work",
    "socket_dir": "/var/run/firecracker-containerd"
  },
  "lxd": {
    "socket_path": "/var/snap/lxd/common/lxd/unix.socket",
    "timeout": "30s",
    "retry_attempts": 3
  },
  "resource_management": {
    "buffer_percentage": 20,
    "monitoring_interval": "30s",
    "availability_update_interval": "60s",
    "conflict_detection_interval": "15s"
  },
  "pricing": {
    "update_interval": "5m",
    "base_rates": {
      "cpu_per_hour": 0.05,
      "memory_mb_per_hour": 0.001,
      "storage_mb_per_hour": 0.0001
    },
    "availability_tiers": {
      "high": {"threshold": 0.6, "discount": 0.4},
      "medium": {"threshold": 0.3, "discount": 0.2},
      "low": {"threshold": 0.0, "discount": 0.0}
    }
  },
  "networking": {
    "default_mode": "dhcp",
    "nat_bridge": {
      "name": "br-microvm",
      "subnet": "192.168.100.0/24",
      "gateway": "192.168.100.1"
    },
    "dhcp": {
      "server": "192.168.1.1",
      "dns_servers": ["8.8.8.8", "8.8.4.4"]
    }
  },
  "storage": {
    "default_type": "local",
    "local": {
      "base_path": "/var/lib/microvm-service/storage"
    },
    "s3": {
      "region": "us-west-2",
      "bucket_prefix": "microvm-storage-"
    }
  },
  "monitoring": {
    "enabled": true,
    "metrics_port": 9090,
    "health_check_interval": "30s",
    "log_level": "info",
    "log_file": "/var/log/microvm-service/service.log"
  }
}
```

### Network Configuration

#### NAT Bridge Setup
```bash
#!/bin/bash
# setup-nat-bridge.sh

# Create bridge interface
sudo ip link add name br-microvm type bridge
sudo ip addr add 192.168.100.1/24 dev br-microvm
sudo ip link set br-microvm up

# Enable IP forwarding
echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf
sudo sysctl -p

# Configure iptables for NAT
sudo iptables -t nat -A POSTROUTING -s 192.168.100.0/24 -o eth0 -j MASQUERADE
sudo iptables -A FORWARD -i br-microvm -o eth0 -j ACCEPT
sudo iptables -A FORWARD -i eth0 -o br-microvm -m state --state RELATED,ESTABLISHED -j ACCEPT

# Save iptables rules
sudo iptables-save | sudo tee /etc/iptables/rules.v4
```

#### DHCP Configuration (for DHCP mode)
```bash
# Install dnsmasq for DHCP
sudo apt-get install dnsmasq

# Configure dnsmasq
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

### Systemd Service Configuration

Create `/etc/systemd/system/microvm-service.service`:

```ini
[Unit]
Description=MicroVM Service
Documentation=https://github.com/your-org/microvm-service
After=network.target lxd.service containerd.service
Wants=network.target
Requires=lxd.service

[Service]
Type=simple
User=microvm-service
Group=microvm-service
ExecStart=/usr/local/bin/microvm-service --config=/etc/microvm-service/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=microvm-service

# Security settings
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/microvm-service /var/log/microvm-service /var/run/firecracker-containerd

# Resource limits
LimitNOFILE=65536
LimitNPROC=4096

[Install]
WantedBy=multi-user.target
```

## Deployment Steps

### Step 1: Pre-deployment Validation

```bash
# Validate system requirements
./scripts/validate-system.sh

# Check LXD connectivity
lxc list

# Verify firecracker installation
firecracker --version

# Test network configuration
ping -c 3 192.168.100.1
```

### Step 2: Service Deployment

```bash
# Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable microvm-service
sudo systemctl start microvm-service

# Verify service status
sudo systemctl status microvm-service

# Check service logs
sudo journalctl -u microvm-service -f
```

### Step 3: Post-deployment Verification

```bash
# Test API connectivity
curl -X GET http://localhost:8080/health

# Verify resource monitoring
curl -X GET http://localhost:8080/api/v1/resources/availability

# Test microVM provisioning
./scripts/test-provisioning.sh
```

## Production Configuration

### High Availability Setup

#### Load Balancer Configuration (HAProxy)
```
# /etc/haproxy/haproxy.cfg
global
    daemon
    maxconn 4096

defaults
    mode http
    timeout connect 5000ms
    timeout client 50000ms
    timeout server 50000ms

frontend microvm_frontend
    bind *:8080
    default_backend microvm_backend

backend microvm_backend
    balance roundrobin
    option httpchk GET /health
    server node1 10.0.1.10:8080 check
    server node2 10.0.1.11:8080 check
    server node3 10.0.1.12:8080 check
```

#### Database Configuration (PostgreSQL)
```sql
-- Create database and user
CREATE DATABASE microvm_service;
CREATE USER microvm_service WITH PASSWORD 'secure_password';
GRANT ALL PRIVILEGES ON DATABASE microvm_service TO microvm_service;

-- Create tables
\c microvm_service;

CREATE TABLE microvms (
    id UUID PRIMARY KEY,
    customer_id VARCHAR(255) NOT NULL,
    node_id VARCHAR(255) NOT NULL,
    status VARCHAR(50) NOT NULL,
    resources JSONB NOT NULL,
    network JSONB NOT NULL,
    storage JSONB NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_microvms_customer ON microvms(customer_id);
CREATE INDEX idx_microvms_node ON microvms(node_id);
CREATE INDEX idx_microvms_status ON microvms(status);
```

### Security Configuration

#### TLS Certificate Setup
```bash
# Generate self-signed certificate (development)
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes

# Or use Let's Encrypt (production)
certbot certonly --standalone -d microvm.yourdomain.com

# Update configuration
sudo cp server.crt /etc/microvm-service/tls/
sudo cp server.key /etc/microvm-service/tls/
sudo chown microvm-service:microvm-service /etc/microvm-service/tls/*
sudo chmod 600 /etc/microvm-service/tls/server.key
```

#### API Key Management
```bash
# Generate API keys
openssl rand -hex 32 > /etc/microvm-service/api-keys/admin.key
openssl rand -hex 32 > /etc/microvm-service/api-keys/customer.key

# Set permissions
sudo chown -R microvm-service:microvm-service /etc/microvm-service/api-keys/
sudo chmod 600 /etc/microvm-service/api-keys/*.key
```

### Monitoring Setup

#### Prometheus Configuration
```yaml
# prometheus.yml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'microvm-service'
    static_configs:
      - targets: ['localhost:9090']
    metrics_path: /metrics
    scrape_interval: 30s
```

#### Grafana Dashboard
```json
{
  "dashboard": {
    "title": "MicroVM Service Dashboard",
    "panels": [
      {
        "title": "Active MicroVMs",
        "type": "stat",
        "targets": [
          {
            "expr": "microvm_total{status=\"running\"}"
          }
        ]
      },
      {
        "title": "Resource Utilization",
        "type": "graph",
        "targets": [
          {
            "expr": "resource_utilization_cpu"
          },
          {
            "expr": "resource_utilization_memory"
          }
        ]
      }
    ]
  }
}
```

## Backup and Recovery

### Backup Procedures
```bash
#!/bin/bash
# backup-microvm-service.sh

BACKUP_DIR="/var/backups/microvm-service"
DATE=$(date +%Y%m%d_%H%M%S)

# Create backup directory
mkdir -p "$BACKUP_DIR/$DATE"

# Backup configuration
cp -r /etc/microvm-service "$BACKUP_DIR/$DATE/"

# Backup data
tar -czf "$BACKUP_DIR/$DATE/data.tar.gz" /var/lib/microvm-service

# Backup database
pg_dump microvm_service > "$BACKUP_DIR/$DATE/database.sql"

# Cleanup old backups (keep 30 days)
find "$BACKUP_DIR" -type d -mtime +30 -exec rm -rf {} \;
```

### Recovery Procedures
```bash
#!/bin/bash
# restore-microvm-service.sh

BACKUP_DATE=$1
BACKUP_DIR="/var/backups/microvm-service/$BACKUP_DATE"

if [ ! -d "$BACKUP_DIR" ]; then
    echo "Backup directory not found: $BACKUP_DIR"
    exit 1
fi

# Stop service
sudo systemctl stop microvm-service

# Restore configuration
sudo cp -r "$BACKUP_DIR/microvm-service" /etc/

# Restore data
sudo tar -xzf "$BACKUP_DIR/data.tar.gz" -C /

# Restore database
psql microvm_service < "$BACKUP_DIR/database.sql"

# Start service
sudo systemctl start microvm-service
```

## Troubleshooting

### Common Issues

#### Service Won't Start
```bash
# Check service status
sudo systemctl status microvm-service

# Check logs
sudo journalctl -u microvm-service -n 50

# Verify configuration
microvm-service --config=/etc/microvm-service/config.json --validate

# Check permissions
ls -la /var/lib/microvm-service
ls -la /etc/microvm-service
```

#### Network Connectivity Issues
```bash
# Check bridge configuration
ip link show br-microvm
ip addr show br-microvm

# Verify iptables rules
sudo iptables -t nat -L
sudo iptables -L FORWARD

# Test DHCP
sudo tcpdump -i br-microvm port 67 or port 68
```

#### Resource Allocation Failures
```bash
# Check LXD status
lxc info
lxc list

# Verify resource availability
curl http://localhost:8080/api/v1/resources/availability

# Check firecracker processes
ps aux | grep firecracker
```

### Log Analysis

#### Important Log Locations
- Service logs: `/var/log/microvm-service/service.log`
- System logs: `journalctl -u microvm-service`
- Firecracker logs: `/var/lib/microvm-service/firecracker-work/*/firecracker.log`
- LXD logs: `journalctl -u lxd`

#### Log Monitoring Commands
```bash
# Monitor service logs
tail -f /var/log/microvm-service/service.log

# Monitor system logs
journalctl -u microvm-service -f

# Search for errors
grep -i error /var/log/microvm-service/service.log

# Monitor resource usage
watch -n 5 'curl -s http://localhost:8080/api/v1/resources/availability | jq'
```

## Maintenance

### Regular Maintenance Tasks

#### Daily Tasks
```bash
# Check service health
curl http://localhost:8080/health

# Monitor resource usage
df -h /var/lib/microvm-service
free -h

# Check for failed microVMs
curl http://localhost:8080/api/v1/microvms?status=error
```

#### Weekly Tasks
```bash
# Update system packages
sudo apt update && sudo apt upgrade

# Rotate logs
sudo logrotate /etc/logrotate.d/microvm-service

# Clean up old firecracker work directories
find /var/lib/microvm-service/firecracker-work -type d -mtime +7 -exec rm -rf {} \;
```

#### Monthly Tasks
```bash
# Review and optimize configuration
# Update firecracker and containerd versions
# Analyze performance metrics
# Review security logs
```

### Scaling Considerations

#### Horizontal Scaling
- Deploy service on multiple LXD nodes
- Use load balancer for request distribution
- Implement shared storage for microVM data
- Configure database clustering

#### Vertical Scaling
- Increase memory allocation for service
- Optimize resource monitoring intervals
- Tune garbage collection settings
- Increase connection pool sizes

## Support and Documentation

### Additional Resources
- [API Documentation](API_DOCUMENTATION.md)
- [Troubleshooting Guide](TROUBLESHOOTING_GUIDE.md)
- [Monitoring Guide](MONITORING_GUIDE.md)
- [Security Guide](SECURITY_GUIDE.md)

### Getting Help
- GitHub Issues: https://github.com/your-org/microvm-service/issues
- Documentation: https://docs.microvm-service.com
- Community Forum: https://community.microvm-service.com