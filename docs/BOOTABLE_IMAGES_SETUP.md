# MicroVM Bootable Images Setup Guide

## Overview

This guide provides step-by-step instructions for setting up the MicroVM service with bootable image and network configuration capabilities. The enhanced service creates fully functional Alpine Linux VMs with SSH access and proper networking.

## Host System Requirements

### Hardware Requirements
- x86_64 processor with virtualization support (Intel VT-x or AMD-V)
- Minimum 4GB RAM (8GB recommended for multiple VMs)
- At least 10GB free disk space for images and overlays
- Network interface for bridge networking

### Software Requirements
- Linux kernel 4.14+ with KVM support
- Root or sudo access for network configuration
- Required packages:
  ```bash
  # Ubuntu/Debian
  sudo apt-get update
  sudo apt-get install -y \
    bridge-utils \
    dnsmasq \
    iptables \
    debootstrap \
    cloud-init \
    qemu-utils \
    curl \
    wget

  # CentOS/RHEL/Fedora
  sudo yum install -y \
    bridge-utils \
    dnsmasq \
    iptables \
    debootstrap \
    cloud-init \
    qemu-img \
    curl \
    wget
  ```

### Firecracker Installation
1. Download Firecracker binary:
   ```bash
   curl -LOJ https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-x86_64.tgz
   tar -xzf firecracker-v1.13.1-x86_64.tgz
   sudo cp release-v1.13.1-x86_64/firecracker-v1.13.1-x86_64 /usr/local/bin/firecracker
   sudo chmod +x /usr/local/bin/firecracker
   ```

2. Verify installation:
   ```bash
   firecracker --version
   ```

## Step-by-Step Setup

### 1. System Preparation

#### Enable KVM Support
```bash
# Check if KVM is available
lsmod | grep kvm

# If not loaded, load KVM modules
sudo modprobe kvm
sudo modprobe kvm_intel  # For Intel processors
# OR
sudo modprobe kvm_amd    # For AMD processors

# Make KVM persistent
echo 'kvm' | sudo tee -a /etc/modules
echo 'kvm_intel' | sudo tee -a /etc/modules  # Intel only
```

#### Configure User Permissions
```bash
# Add user to kvm group
sudo usermod -a -G kvm $USER

# Create microvm service user (optional)
sudo useradd -r -s /bin/false microvm-service
sudo usermod -a -G kvm microvm-service
```

### 2. Network Configuration

#### Enable IP Forwarding
```bash
# Temporary enable
sudo sysctl net.ipv4.ip_forward=1

# Permanent enable
echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

#### Configure Firewall (if using UFW)
```bash
# Allow SSH forwarding ports
sudo ufw allow 2200:2299/tcp

# Allow bridge traffic
sudo ufw allow in on fc-nat-br0
sudo ufw allow out on fc-nat-br0
```

### 3. Service Configuration

#### Create Service Directories
```bash
sudo mkdir -p /var/lib/microvm-service/{images,overlays,ssh-keys,configs}
sudo mkdir -p /var/log/microvm-service
sudo chown -R microvm-service:microvm-service /var/lib/microvm-service
sudo chown -R microvm-service:microvm-service /var/log/microvm-service
```

#### Configure Service Settings
Create `/etc/microvm-service/config.json`:
```json
{
  "firecracker": {
    "binary_path": "/usr/local/bin/firecracker",
    "work_dir": "/var/lib/microvm-service"
  },
  "network": {
    "bridge_name": "fc-nat-br0",
    "bridge_ip": "192.168.100.1/24",
    "dhcp_range": "192.168.100.10,192.168.100.100",
    "ssh_port_range": "2200-2299"
  },
  "images": {
    "cache_dir": "/var/lib/microvm-service/images",
    "overlay_dir": "/var/lib/microvm-service/overlays",
    "default_distro": "alpine",
    "cleanup_interval": "24h"
  },
  "logging": {
    "level": "info",
    "file": "/var/log/microvm-service/service.log"
  }
}
```

### 4. Service Installation

#### Build and Install Service
```bash
cd microvm-service
go build -o microvm-service cmd/main.go
sudo cp microvm-service /usr/local/bin/
sudo chmod +x /usr/local/bin/microvm-service
```

#### Create Systemd Service
Create `/etc/systemd/system/microvm-service.service`:
```ini
[Unit]
Description=MicroVM Service with Bootable Images
After=network.target

[Service]
Type=simple
User=microvm-service
Group=microvm-service
ExecStart=/usr/local/bin/microvm-service
Restart=always
RestartSec=5
Environment=CONFIG_FILE=/etc/microvm-service/config.json

[Install]
WantedBy=multi-user.target
```

#### Enable and Start Service
```bash
sudo systemctl daemon-reload
sudo systemctl enable microvm-service
sudo systemctl start microvm-service
sudo systemctl status microvm-service
```

## Configuration Examples

### Development Environment
For local development with minimal security:
```json
{
  "firecracker": {
    "binary_path": "./firecracker",
    "work_dir": "./firecracker-work"
  },
  "network": {
    "bridge_name": "fc-dev-br0",
    "bridge_ip": "192.168.200.1/24",
    "dhcp_range": "192.168.200.10,192.168.200.50",
    "ssh_port_range": "3000-3099"
  },
  "images": {
    "cache_dir": "./images",
    "overlay_dir": "./overlays",
    "default_distro": "alpine",
    "cleanup_interval": "1h"
  }
}
```

### Production Environment
For production with enhanced security:
```json
{
  "firecracker": {
    "binary_path": "/usr/local/bin/firecracker",
    "work_dir": "/var/lib/microvm-service",
    "jailer_enabled": true,
    "chroot_base": "/srv/jailer"
  },
  "network": {
    "bridge_name": "fc-prod-br0",
    "bridge_ip": "10.100.0.1/24",
    "dhcp_range": "10.100.0.10,10.100.0.200",
    "ssh_port_range": "10000-10999",
    "firewall_enabled": true
  },
  "images": {
    "cache_dir": "/var/lib/microvm-service/images",
    "overlay_dir": "/var/lib/microvm-service/overlays",
    "default_distro": "alpine",
    "cleanup_interval": "12h",
    "integrity_check": true
  },
  "security": {
    "ssh_key_rotation": "7d",
    "vm_isolation": true,
    "audit_logging": true
  }
}
```

### Multi-Tenant Environment
For hosting multiple isolated environments:
```json
{
  "firecracker": {
    "binary_path": "/usr/local/bin/firecracker",
    "work_dir": "/var/lib/microvm-service",
    "jailer_enabled": true
  },
  "network": {
    "bridge_name": "fc-tenant-br0",
    "bridge_ip": "172.16.0.1/16",
    "dhcp_range": "172.16.1.10,172.16.255.200",
    "ssh_port_range": "20000-29999",
    "tenant_isolation": true
  },
  "images": {
    "cache_dir": "/var/lib/microvm-service/images",
    "overlay_dir": "/var/lib/microvm-service/overlays",
    "per_tenant_cache": true,
    "quota_enabled": true,
    "max_images_per_tenant": 10
  },
  "resources": {
    "max_vms_per_tenant": 50,
    "cpu_quota": "80%",
    "memory_quota": "16GB"
  }
}
```

## Verification Steps

### 1. System Validation
Run the diagnostic script to verify setup:
```bash
./scripts/validate-system-setup.sh
```

### 2. Network Connectivity Test
```bash
# Check bridge configuration
ip addr show fc-nat-br0

# Verify DHCP service
sudo systemctl status dnsmasq

# Test port forwarding
sudo iptables -t nat -L PREROUTING -n
```

### 3. Image Creation Test
```bash
# Test Alpine image creation
curl -X POST http://localhost:8080/api/v1/images/build \
  -H "Content-Type: application/json" \
  -d '{"distro": "alpine", "version": "3.18"}'
```

### 4. VM Creation and SSH Test
```bash
# Create test VM
curl -X POST http://localhost:8080/api/v1/vms \
  -H "Content-Type: application/json" \
  -d '{"name": "test-vm", "resources": {"vcpus": 1, "memory": 128}}'

# Test SSH access (replace with actual values)
ssh -i /var/lib/microvm-service/ssh-keys/test-vm_rsa \
    -p 2200 alpine@localhost
```

## Next Steps

1. **Configure Monitoring**: Set up monitoring for VM health and resource usage
2. **Backup Strategy**: Implement backup procedures for VM images and configurations
3. **Security Hardening**: Apply additional security measures for production use
4. **Performance Tuning**: Optimize settings based on workload requirements
5. **Integration**: Connect with existing infrastructure and CI/CD pipelines

For troubleshooting common issues, see the [Troubleshooting Guide](BOOTABLE_IMAGES_TROUBLESHOOTING.md).