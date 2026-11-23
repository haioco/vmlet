# MicroVM Bootable Images Troubleshooting Guide

## Common Issues and Solutions

### Boot Issues

#### VM Fails to Boot - Kernel Panic
**Symptoms:**
- VM starts but immediately crashes with kernel panic
- Console shows "Kernel panic - not syncing"

**Causes:**
- Corrupted or incompatible kernel image
- Missing required kernel modules
- Incorrect boot parameters

**Solutions:**
1. Verify kernel image integrity:
   ```bash
   file /var/lib/microvm-service/images/vmlinux
   ```

2. Check kernel version compatibility:
   ```bash
   # Ensure kernel is 4.14+ and built for microVMs
   strings /var/lib/microvm-service/images/vmlinux | grep "Linux version"
   ```

3. Rebuild base image with correct kernel:
   ```bash
   curl -X POST http://localhost:8080/api/v1/images/rebuild \
     -H "Content-Type: application/json" \
     -d '{"distro": "alpine", "force": true}'
   ```

#### VM Boots but Hangs at Init
**Symptoms:**
- VM starts, kernel loads successfully
- Boot process stops at init system startup
- No SSH access available

**Causes:**
- Missing init system in rootfs
- Incorrect init configuration
- Missing essential system files

**Solutions:**
1. Check rootfs contents:
   ```bash
   sudo mount -o loop /var/lib/microvm-service/images/alpine-base.img /mnt
   ls -la /mnt/sbin/init
   sudo umount /mnt
   ```

2. Verify cloud-init installation:
   ```bash
   # Check if cloud-init is properly installed
   sudo mount -o loop /var/lib/microvm-service/images/alpine-base.img /mnt
   ls -la /mnt/etc/cloud/
   sudo umount /mnt
   ```

3. Recreate base image with proper init:
   ```bash
   rm /var/lib/microvm-service/images/alpine-base.img
   # Restart service to trigger image rebuild
   sudo systemctl restart microvm-service
   ```

#### SSH Service Not Starting
**Symptoms:**
- VM boots successfully
- SSH connection attempts fail
- Port forwarding appears correct

**Causes:**
- SSH daemon not installed or configured
- SSH keys not properly injected
- Cloud-init configuration errors

**Solutions:**
1. Check SSH daemon in image:
   ```bash
   sudo mount -o loop /var/lib/microvm-service/images/alpine-base.img /mnt
   ls -la /mnt/etc/ssh/
   cat /mnt/etc/ssh/sshd_config
   sudo umount /mnt
   ```

2. Verify cloud-init SSH configuration:
   ```bash
   # Check cloud-init logs in VM console
   # Look for SSH key injection messages
   ```

3. Manually test SSH key:
   ```bash
   # Verify key pair generation
   ls -la /var/lib/microvm-service/ssh-keys/
   ssh-keygen -l -f /var/lib/microvm-service/ssh-keys/vm-id_rsa.pub
   ```

### Network Issues

#### Bridge Network Not Created
**Symptoms:**
- Service starts but VMs have no network
- Bridge interface missing: `ip addr show fc-nat-br0` fails

**Causes:**
- Insufficient permissions for bridge creation
- Bridge utilities not installed
- Conflicting network configuration

**Solutions:**
1. Check bridge utilities:
   ```bash
   which brctl
   sudo brctl show
   ```

2. Verify permissions:
   ```bash
   # Service user needs CAP_NET_ADMIN
   sudo setcap cap_net_admin+ep /usr/local/bin/microvm-service
   ```

3. Manual bridge creation test:
   ```bash
   sudo ip link add fc-nat-br0 type bridge
   sudo ip addr add 192.168.100.1/24 dev fc-nat-br0
   sudo ip link set fc-nat-br0 up
   ```

#### DHCP Server Not Responding
**Symptoms:**
- VMs boot but don't get IP addresses
- Network interface shows no IP in VM

**Causes:**
- dnsmasq not running or misconfigured
- DHCP range conflicts
- Firewall blocking DHCP traffic

**Solutions:**
1. Check dnsmasq status:
   ```bash
   sudo systemctl status dnsmasq
   sudo journalctl -u dnsmasq -f
   ```

2. Verify DHCP configuration:
   ```bash
   cat /etc/dnsmasq.d/microvm-service.conf
   ```

3. Test DHCP manually:
   ```bash
   # Create test interface and request DHCP
   sudo ip tuntap add mode tap tap-test
   sudo ip link set tap-test master fc-nat-br0
   sudo ip link set tap-test up
   sudo dhclient tap-test
   ```

#### SSH Port Forwarding Not Working
**Symptoms:**
- VM has IP address and SSH is running
- Cannot connect from host via forwarded port

**Causes:**
- iptables rules not applied
- Port conflicts
- Firewall blocking forwarded ports

**Solutions:**
1. Check iptables rules:
   ```bash
   sudo iptables -t nat -L PREROUTING -n
   sudo iptables -L FORWARD -n
   ```

2. Verify port allocation:
   ```bash
   netstat -tlnp | grep :22[0-9][0-9]
   ```

3. Test port forwarding manually:
   ```bash
   # Add manual rule for testing
   sudo iptables -t nat -A PREROUTING -p tcp --dport 2200 \
     -j DNAT --to-destination 192.168.100.10:22
   ```

### Image Creation Issues

#### Insufficient Disk Space
**Symptoms:**
- Image creation fails with "No space left on device"
- Service logs show disk space errors

**Solutions:**
1. Check available space:
   ```bash
   df -h /var/lib/microvm-service/
   ```

2. Clean up old images:
   ```bash
   # Manual cleanup
   sudo find /var/lib/microvm-service/overlays -mtime +1 -delete
   sudo find /var/lib/microvm-service/images -name "*.tmp" -delete
   ```

3. Configure automatic cleanup:
   ```json
   {
     "images": {
       "cleanup_interval": "6h",
       "max_cache_size": "5GB",
       "auto_cleanup": true
     }
   }
   ```

#### Package Download Failures
**Symptoms:**
- Alpine image creation fails during package installation
- Network timeouts during image build

**Causes:**
- Internet connectivity issues
- Alpine repository mirrors down
- DNS resolution problems

**Solutions:**
1. Test internet connectivity:
   ```bash
   curl -I https://dl-cdn.alpinelinux.org/alpine/
   ```

2. Configure alternative mirrors:
   ```bash
   # Edit Alpine builder configuration
   # Use different mirror in image builder
   ```

3. Use local package cache:
   ```bash
   # Set up local Alpine mirror
   # Configure service to use local mirror
   ```

#### Permission Errors During Image Creation
**Symptoms:**
- Image creation fails with permission denied errors
- Cannot write to image directories

**Solutions:**
1. Check directory permissions:
   ```bash
   ls -la /var/lib/microvm-service/
   sudo chown -R microvm-service:microvm-service /var/lib/microvm-service/
   ```

2. Verify service user capabilities:
   ```bash
   # Service needs to create loop devices
   sudo setcap cap_sys_admin+ep /usr/local/bin/microvm-service
   ```

### Performance Issues

#### Slow VM Boot Times
**Symptoms:**
- VMs take longer than 60 seconds to boot
- Boot timeout errors in logs

**Causes:**
- Insufficient host resources
- Slow disk I/O
- Large image sizes

**Solutions:**
1. Monitor resource usage:
   ```bash
   htop
   iotop
   ```

2. Optimize image size:
   ```bash
   # Remove unnecessary packages from base image
   # Use minimal Alpine installation
   ```

3. Use SSD storage:
   ```bash
   # Move image directory to SSD
   sudo mv /var/lib/microvm-service/images /fast-storage/
   sudo ln -s /fast-storage/images /var/lib/microvm-service/images
   ```

#### High Memory Usage
**Symptoms:**
- Host system running out of memory
- VM creation failures due to memory

**Solutions:**
1. Monitor memory usage:
   ```bash
   free -h
   ps aux --sort=-%mem | head
   ```

2. Configure VM memory limits:
   ```json
   {
     "resources": {
       "default_memory": "64MB",
       "max_memory": "512MB",
       "memory_overcommit": false
     }
   }
   ```

3. Enable memory ballooning:
   ```bash
   # Configure balloon device in VM
   # Implement memory pressure handling
   ```

## Diagnostic Commands

### System Health Check
```bash
#!/bin/bash
# Complete system diagnostic script

echo "=== MicroVM Service Diagnostics ==="

echo "1. Checking Firecracker installation..."
firecracker --version || echo "ERROR: Firecracker not found"

echo "2. Checking KVM support..."
lsmod | grep kvm || echo "ERROR: KVM modules not loaded"

echo "3. Checking network bridge..."
ip addr show fc-nat-br0 || echo "ERROR: Bridge not configured"

echo "4. Checking DHCP service..."
systemctl is-active dnsmasq || echo "ERROR: DHCP service not running"

echo "5. Checking iptables rules..."
iptables -t nat -L PREROUTING -n | grep -q DNAT || echo "WARNING: No DNAT rules found"

echo "6. Checking disk space..."
df -h /var/lib/microvm-service/ | tail -1

echo "7. Checking service status..."
systemctl status microvm-service --no-pager

echo "8. Checking recent logs..."
journalctl -u microvm-service --since "1 hour ago" --no-pager | tail -20

echo "=== Diagnostics Complete ==="
```

### Network Connectivity Test
```bash
#!/bin/bash
# Network diagnostic script

VM_IP="192.168.100.10"  # Replace with actual VM IP
SSH_PORT="2200"         # Replace with actual SSH port

echo "=== Network Connectivity Test ==="

echo "1. Testing bridge connectivity..."
ping -c 3 192.168.100.1 || echo "ERROR: Cannot reach bridge IP"

echo "2. Testing VM connectivity..."
ping -c 3 $VM_IP || echo "ERROR: Cannot reach VM IP"

echo "3. Testing SSH port forwarding..."
nc -zv localhost $SSH_PORT || echo "ERROR: SSH port not accessible"

echo "4. Testing internet from host..."
ping -c 3 8.8.8.8 || echo "ERROR: No internet connectivity"

echo "5. Testing DNS resolution..."
nslookup google.com || echo "ERROR: DNS resolution failed"

echo "=== Network Test Complete ==="
```

## Getting Help

### Log Locations
- Service logs: `/var/log/microvm-service/service.log`
- System logs: `journalctl -u microvm-service`
- VM console logs: `/var/lib/microvm-service/logs/vm-{id}.log`
- Network logs: `journalctl -u dnsmasq`

### Debug Mode
Enable debug logging by updating configuration:
```json
{
  "logging": {
    "level": "debug",
    "console": true,
    "file": "/var/log/microvm-service/debug.log"
  }
}
```

### Support Information
When reporting issues, include:
1. Output of diagnostic script
2. Service configuration file
3. Recent service logs
4. Host system information (`uname -a`, `lscpu`, `free -h`)
5. Network configuration (`ip addr`, `ip route`)

### Community Resources
- GitHub Issues: [microvm-service/issues](https://github.com/your-org/microvm-service/issues)
- Documentation: [docs/](../docs/)
- Examples: [examples/](../examples/)