# MicroVM Service API Documentation

## Overview

The MicroVM Service provides a comprehensive API for managing Firecracker microVMs with bootable images and network connectivity. The service creates fully functional Alpine Linux VMs with SSH access, automatic networking, and cloud-init configuration. This document describes all available endpoints, data structures, and usage examples.

## New Features

### Bootable Images
- **Alpine Linux Support**: Automatic creation of minimal Alpine Linux root filesystems
- **Cloud-init Integration**: Automatic VM configuration with SSH keys, networking, and user accounts
- **Image Caching**: Efficient base image caching with copy-on-write overlays for individual VMs
- **Boot Validation**: Comprehensive boot monitoring and SSH connectivity validation

### Enhanced Networking
- **Automatic Bridge Setup**: NAT bridge creation and configuration for VM networking
- **DHCP Management**: Automatic IP address allocation and DNS configuration
- **SSH Access**: Automatic SSH key generation and port forwarding setup
- **Network Testing**: Built-in connectivity validation and health checking

## Service Interface

### Base Service: MicroVMService

The main service interface provides methods for microVM lifecycle management, resource monitoring, and pricing information.

#### Methods

##### provisionMicroVM

Creates a new microVM with bootable Alpine Linux image and automatic network configuration.

**Request**: `ProvisionMicroVMRequest`
```protobuf
message ProvisionMicroVMRequest {
  ResourceSpecs resource_specs = 1;
  NetworkConfig network_config = 2;
  StorageConfig storage_config = 3;
  ImageConfig image_config = 4;        // NEW: Image configuration
  CloudInitConfig cloudinit_config = 5; // NEW: Cloud-init settings
  string customer_id = 6;
  map<string, string> metadata = 7;
}
```

**Response**: `MicroVMInstance`
```protobuf
message MicroVMInstance {
  string id = 1;
  string node_id = 2;
  MicroVMStatus status = 3;
  ResourceSpecs resources = 4;
  NetworkInfo network = 5;
  StorageInfo storage = 6;
  ImageInfo image = 7;                 // NEW: Image information
  SSHInfo ssh = 8;                     // NEW: SSH access information
  PricingInfo pricing = 9;
  MicroVMMetadata metadata = 10;
}
```

**Example Usage**:
```go
req := &pb.ProvisionMicroVMRequest{
    ResourceSpecs: &pb.ResourceSpecs{
        Vcpus:     1,
        MemoryMb:  128,
        StorageMb: 1024,
    },
    NetworkConfig: &pb.NetworkConfig{
        AssignmentMethod: "dhcp",
        EnableSsh:        true,
    },
    ImageConfig: &pb.ImageConfig{
        Distribution: "alpine",
        Version:      "3.18",
        Packages:     []string{"curl", "wget"},
    },
    CloudinitConfig: &pb.CloudInitConfig{
        Hostname:     "my-vm",
        Username:     "alpine",
        SshKeys:      []string{"ssh-rsa AAAAB3..."},
        RunCommands:  []string{"apk update", "apk add htop"},
    },
    CustomerId: "customer-123",
}

response, err := client.ProvisionMicroVM(ctx, req)
if err != nil {
    log.Fatal(err)
}

// VM is automatically configured with SSH access
fmt.Printf("VM created: %s\n", response.Id)
fmt.Printf("SSH command: ssh -i %s -p %d %s@localhost\n", 
    response.Ssh.KeyPath, response.Ssh.Port, response.Ssh.Username)
```

##### getMicroVMStatus

Retrieves current status and information for a specific microVM.

**Request**: `GetMicroVMStatusRequest`
```protobuf
message GetMicroVMStatusRequest {
  string microvm_id = 1;
}
```

**Response**: `MicroVMStatus`
```protobuf
message MicroVMStatus {
  string status = 1; // creating, running, stopping, stopped, error
  ResourceUsage current_usage = 2;
  NetworkInfo network = 3;
  int64 uptime_seconds = 4;
  string last_error = 5;
}
```

##### stopMicroVM

Gracefully stops a running microVM.

**Request**: `StopMicroVMRequest`
```protobuf
message StopMicroVMRequest {
  string microvm_id = 1;
  bool force = 2; // Force stop if graceful shutdown fails
  int32 timeout_seconds = 3; // Timeout for graceful shutdown
}
```

**Response**: `OperationResult`
```protobuf
message OperationResult {
  bool success = 1;
  string message = 2;
  string operation_id = 3;
}
```

##### deleteMicroVM

Permanently removes a microVM and releases its resources.

**Request**: `DeleteMicroVMRequest`
```protobuf
message DeleteMicroVMRequest {
  string microvm_id = 1;
  bool preserve_data = 2; // Keep storage volumes
}
```

**Response**: `OperationResult`

##### getCurrentPricing

Retrieves current pricing information for microVM resources.

**Request**: `GetCurrentPricingRequest`
```protobuf
message GetCurrentPricingRequest {
  string node_id = 1; // Optional: specific node pricing
}
```

**Response**: `PricingInfo`
```protobuf
message PricingInfo {
  BaseRates base_rates = 1;
  CurrentMultipliers current_multipliers = 2;
  string availability_tier = 3; // high, medium, low
  int64 next_update_at = 4;
  string currency = 5;
}
```

##### getResourceAvailability

Returns current resource availability across all nodes.

**Request**: `GetResourceAvailabilityRequest`
```protobuf
message GetResourceAvailabilityRequest {
  string node_id = 1; // Optional: specific node
}
```

**Response**: `ResourceAvailability`
```protobuf
message ResourceAvailability {
  repeated NodeResourceInfo nodes = 1;
  ResourceSummary total_available = 2;
  int64 last_updated = 3;
}
```

##### listMicroVMs

Lists all microVMs with optional filtering.

**Request**: `ListMicroVMsRequest`
```protobuf
message ListMicroVMsRequest {
  string customer_id = 1; // Filter by customer
  string status = 2; // Filter by status
  string node_id = 3; // Filter by node
  int32 page_size = 4;
  string page_token = 5;
}
```

**Response**: `ListMicroVMsResponse`
```protobuf
message ListMicroVMsResponse {
  repeated MicroVMInstance microvms = 1;
  string next_page_token = 2;
  int32 total_count = 3;
}
```

### Image Management Methods

##### buildBaseImage

Creates or updates a base image for the specified distribution.

**Request**: `BuildBaseImageRequest`
```protobuf
message BuildBaseImageRequest {
  string distribution = 1; // alpine, ubuntu
  string version = 2;      // 3.18, 22.04
  repeated string packages = 3; // Additional packages to install
  bool force_rebuild = 4;  // Force rebuild even if image exists
}
```

**Response**: `BaseImageInfo`
```protobuf
message BaseImageInfo {
  string id = 1;
  string distribution = 2;
  string version = 3;
  string path = 4;
  int64 size_bytes = 5;
  string checksum = 6;
  int64 created_at = 7;
  repeated string installed_packages = 8;
}
```

##### listBaseImages

Lists available base images.

**Request**: `ListBaseImagesRequest`
```protobuf
message ListBaseImagesRequest {
  string distribution = 1; // Optional filter
}
```

**Response**: `ListBaseImagesResponse`
```protobuf
message ListBaseImagesResponse {
  repeated BaseImageInfo images = 1;
}
```

##### validateVMBoot

Validates that a VM has booted successfully and is accessible via SSH.

**Request**: `ValidateVMBootRequest`
```protobuf
message ValidateVMBootRequest {
  string microvm_id = 1;
  int32 timeout_seconds = 2; // Boot validation timeout
}
```

**Response**: `BootValidationResult`
```protobuf
message BootValidationResult {
  bool success = 1;
  int32 boot_time_seconds = 2;
  bool ssh_accessible = 3;
  bool network_connectivity = 4;
  repeated string boot_stages = 5;
  string console_output = 6;
  string error_message = 7;
}
```

### Network Management Methods

##### setupHostNetworking

Configures host networking infrastructure (bridge, DHCP, NAT).

**Request**: `SetupHostNetworkingRequest`
```protobuf
message SetupHostNetworkingRequest {
  NetworkBridgeConfig bridge_config = 1;
  DHCPConfig dhcp_config = 2;
  bool force_reconfigure = 3;
}
```

**Response**: `NetworkSetupResult`
```protobuf
message NetworkSetupResult {
  bool success = 1;
  string bridge_name = 2;
  string bridge_ip = 3;
  string dhcp_range = 4;
  repeated string nat_rules = 5;
  string message = 6;
}
```

##### testNetworkConnectivity

Tests network connectivity for a specific VM.

**Request**: `TestNetworkConnectivityRequest`
```protobuf
message TestNetworkConnectivityRequest {
  string microvm_id = 1;
  repeated string test_targets = 2; // IPs or hostnames to test
}
```

**Response**: `NetworkConnectivityResult`
```protobuf
message NetworkConnectivityResult {
  bool overall_success = 1;
  bool gateway_reachable = 2;
  bool internet_accessible = 3;
  bool dns_working = 4;
  repeated ConnectivityTest individual_tests = 5;
}
```

## Data Structures

### ResourceSpecs
```protobuf
message ResourceSpecs {
  int32 vcpus = 1;
  int32 memory_mb = 2;
  int32 storage_mb = 3;
}
```

### NetworkConfig
```protobuf
message NetworkConfig {
  string assignment_method = 1; // dhcp, macvlan, nat, ovn
  string ip_address = 2; // Optional: specific IP
  string mac_address = 3; // Optional: specific MAC
  repeated string dns_servers = 4;
  string gateway = 5;
}
```

### StorageConfig
```protobuf
message StorageConfig {
  string type = 1; // local, s3-fuse, s3-nbd, hybrid
  S3Config s3_config = 2; // Required for S3 storage types
  bool enable_snapshots = 3;
  int32 snapshot_retention_days = 4;
}
```

### S3Config
```protobuf
message S3Config {
  string bucket = 1;
  string region = 2;
  string access_key = 3;
  string secret_key = 4;
  string endpoint = 5; // Optional: custom S3 endpoint
}
```

### ImageConfig
```protobuf
message ImageConfig {
  string distribution = 1; // alpine, ubuntu
  string version = 2;      // 3.18, 22.04, latest
  repeated string packages = 3; // Additional packages to install
  string kernel_path = 4;  // Optional: custom kernel
  bool enable_cloudinit = 5; // Enable cloud-init (default: true)
}
```

### CloudInitConfig
```protobuf
message CloudInitConfig {
  string hostname = 1;
  string username = 2;     // Default: alpine
  string password = 3;     // Optional: user password
  repeated string ssh_keys = 4; // SSH public keys
  repeated string packages = 5; // Packages to install via cloud-init
  repeated string run_commands = 6; // Commands to run on first boot
  map<string, string> write_files = 7; // Files to create (path -> content)
  NetworkCloudInitConfig network = 8; // Network configuration
}
```

### NetworkCloudInitConfig
```protobuf
message NetworkCloudInitConfig {
  string ip_address = 1;   // Static IP (optional)
  string gateway = 2;      // Gateway IP
  repeated string dns_servers = 3; // DNS servers
  string domain = 4;       // DNS domain
}
```

### ImageInfo
```protobuf
message ImageInfo {
  string base_image_id = 1;
  string distribution = 2;
  string version = 3;
  string rootfs_path = 4;
  string kernel_path = 5;
  int64 size_bytes = 6;
  string checksum = 7;
  bool cloudinit_enabled = 8;
  repeated string installed_packages = 9;
}
```

### SSHInfo
```protobuf
message SSHInfo {
  string username = 1;     // SSH username (usually 'alpine')
  int32 port = 2;         // SSH port on host (forwarded)
  string key_path = 3;    // Path to private SSH key
  string public_key = 4;  // SSH public key content
  bool accessible = 5;    // SSH accessibility status
  string connection_command = 6; // Ready-to-use SSH command
}
```

### NetworkBridgeConfig
```protobuf
message NetworkBridgeConfig {
  string name = 1;        // Bridge name (default: fc-nat-br0)
  string ip_cidr = 2;     // Bridge IP and subnet (default: 192.168.100.1/24)
  bool enable_nat = 3;    // Enable NAT (default: true)
  string external_interface = 4; // External interface for NAT
}
```

### DHCPConfig
```protobuf
message DHCPConfig {
  string range_start = 1; // DHCP range start (default: 192.168.100.10)
  string range_end = 2;   // DHCP range end (default: 192.168.100.100)
  string lease_time = 3;  // Lease time (default: 12h)
  repeated string dns_servers = 4; // DNS servers (default: 8.8.8.8, 8.8.4.4)
  string domain = 5;      // DNS domain
}
```

### ConnectivityTest
```protobuf
message ConnectivityTest {
  string target = 1;      // Target IP or hostname
  string test_type = 2;   // ping, tcp, http
  bool success = 3;
  int32 latency_ms = 4;
  string error_message = 5;
}
```

## Error Handling

### Error Codes

| Code | Description | Retry Recommended |
|------|-------------|-------------------|
| `RESOURCE_UNAVAILABLE` | Insufficient resources | Yes, after delay |
| `INVALID_CONFIGURATION` | Invalid request parameters | No |
| `NODE_UNAVAILABLE` | Target node is offline | Yes, different node |
| `NETWORK_ERROR` | Network configuration failed | Yes |
| `STORAGE_ERROR` | Storage provisioning failed | Yes |
| `TIMEOUT` | Operation timed out | Yes |
| `INTERNAL_ERROR` | Unexpected service error | Yes, after delay |

### Error Response Format
```protobuf
message ErrorResponse {
  string code = 1;
  string message = 2;
  map<string, string> details = 3;
  repeated string suggestions = 4;
}
```

### Bootable Images Specific Errors

| Code | Description | Common Causes | Solutions |
|------|-------------|---------------|-----------|
| `IMAGE_BUILD_FAILED` | Base image creation failed | Network issues, package conflicts | Check internet connectivity, retry with different packages |
| `BOOT_TIMEOUT` | VM failed to boot within timeout | Insufficient resources, corrupted image | Increase timeout, rebuild base image |
| `SSH_UNAVAILABLE` | SSH service not accessible | Network misconfiguration, SSH daemon issues | Check network setup, validate SSH configuration |
| `NETWORK_SETUP_FAILED` | Host networking configuration failed | Permission issues, conflicting configuration | Run with proper privileges, check existing network config |
| `CLOUDINIT_ERROR` | Cloud-init configuration failed | Invalid cloud-init data, missing dependencies | Validate cloud-init configuration, check VM console logs |

## Authentication and Authorization

### API Key Authentication
```
Authorization: Bearer <api-key>
```

### Customer Isolation
- Each customer can only access their own microVMs
- Admin users can access all resources
- Node-level access controls for maintenance operations

## Rate Limiting

| Operation | Limit | Window |
|-----------|-------|--------|
| Provision MicroVM | 10 requests | 1 minute |
| Status Queries | 100 requests | 1 minute |
| List Operations | 50 requests | 1 minute |
| Management Operations | 20 requests | 1 minute |

## SDK Examples

### Go Client - Complete VM Lifecycle with SSH Access
```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"
    
    "google.golang.org/grpc"
    pb "github.com/your-org/microvm-service/api"
)

func main() {
    conn, err := grpc.Dial("localhost:8080", grpc.WithInsecure())
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()
    
    client := pb.NewMicroVMServiceClient(conn)
    ctx := context.Background()
    
    // 1. Setup host networking (one-time setup)
    setupReq := &pb.SetupHostNetworkingRequest{
        BridgeConfig: &pb.NetworkBridgeConfig{
            Name:    "fc-nat-br0",
            IpCidr:  "192.168.100.1/24",
            EnableNat: true,
        },
        DhcpConfig: &pb.DHCPConfig{
            RangeStart: "192.168.100.10",
            RangeEnd:   "192.168.100.100",
            LeaseTime:  "12h",
            DnsServers: []string{"8.8.8.8", "8.8.4.4"},
        },
    }
    
    setupResult, err := client.SetupHostNetworking(ctx, setupReq)
    if err != nil {
        log.Printf("Network setup warning: %v", err)
    } else {
        log.Printf("Network setup: %s", setupResult.Message)
    }
    
    // 2. Create bootable VM with Alpine Linux
    req := &pb.ProvisionMicroVMRequest{
        ResourceSpecs: &pb.ResourceSpecs{
            Vcpus:     1,
            MemoryMb:  128,
            StorageMb: 1024,
        },
        NetworkConfig: &pb.NetworkConfig{
            AssignmentMethod: "dhcp",
            EnableSsh:        true,
        },
        ImageConfig: &pb.ImageConfig{
            Distribution: "alpine",
            Version:      "3.18",
            Packages:     []string{"curl", "htop", "nano"},
        },
        CloudinitConfig: &pb.CloudInitConfig{
            Hostname:    "my-alpine-vm",
            Username:    "alpine",
            SshKeys:     []string{"ssh-rsa AAAAB3NzaC1yc2E... your-key-here"},
            RunCommands: []string{
                "apk update",
                "apk add git",
                "echo 'Welcome to Alpine Linux!' > /etc/motd",
            },
        },
        CustomerId: "customer-456",
    }
    
    microvm, err := client.ProvisionMicroVM(ctx, req)
    if err != nil {
        log.Fatal(err)
    }
    
    log.Printf("Created microVM: %s", microvm.Id)
    
    // 3. Wait for boot and validate SSH access
    bootReq := &pb.ValidateVMBootRequest{
        MicrovmId:      microvm.Id,
        TimeoutSeconds: 60,
    }
    
    bootResult, err := client.ValidateVMBoot(ctx, bootReq)
    if err != nil {
        log.Fatal(err)
    }
    
    if bootResult.Success {
        log.Printf("VM booted successfully in %d seconds", bootResult.BootTimeSeconds)
        log.Printf("SSH accessible: %t", bootResult.SshAccessible)
        log.Printf("Network connectivity: %t", bootResult.NetworkConnectivity)
        
        // Display SSH connection information
        fmt.Printf("\nSSH Connection Info:\n")
        fmt.Printf("Command: %s\n", microvm.Ssh.ConnectionCommand)
        fmt.Printf("Username: %s\n", microvm.Ssh.Username)
        fmt.Printf("Port: %d\n", microvm.Ssh.Port)
        fmt.Printf("Key: %s\n", microvm.Ssh.KeyPath)
    } else {
        log.Printf("VM boot failed: %s", bootResult.ErrorMessage)
    }
    
    // 4. Test network connectivity
    netTestReq := &pb.TestNetworkConnectivityRequest{
        MicrovmId:    microvm.Id,
        TestTargets:  []string{"8.8.8.8", "google.com"},
    }
    
    netResult, err := client.TestNetworkConnectivity(ctx, netTestReq)
    if err != nil {
        log.Printf("Network test error: %v", err)
    } else {
        log.Printf("Network connectivity: %t", netResult.OverallSuccess)
        log.Printf("Internet access: %t", netResult.InternetAccessible)
        log.Printf("DNS working: %t", netResult.DnsWorking)
    }
    
    // 5. Keep VM running or clean up
    fmt.Println("\nPress Enter to stop and delete VM, or Ctrl+C to keep running...")
    fmt.Scanln()
    
    // Stop and delete VM
    stopReq := &pb.StopMicroVMRequest{
        MicrovmId:      microvm.Id,
        TimeoutSeconds: 30,
    }
    
    _, err = client.StopMicroVM(ctx, stopReq)
    if err != nil {
        log.Printf("Stop error: %v", err)
    }
    
    deleteReq := &pb.DeleteMicroVMRequest{
        MicrovmId: microvm.Id,
    }
    
    _, err = client.DeleteMicroVM(ctx, deleteReq)
    if err != nil {
        log.Printf("Delete error: %v", err)
    } else {
        log.Println("VM cleaned up successfully")
    }
}
```

### Python Client
```python
import grpc
import microvm_pb2
import microvm_pb2_grpc

def create_microvm():
    channel = grpc.insecure_channel('localhost:8080')
    client = microvm_pb2_grpc.MicroVMServiceStub(channel)
    
    request = microvm_pb2.ProvisionMicroVMRequest(
        resource_specs=microvm_pb2.ResourceSpecs(
            vcpus=1,
            memory_mb=512,
            storage_mb=2048
        ),
        network_config=microvm_pb2.NetworkConfig(
            assignment_method="dhcp"
        ),
        storage_config=microvm_pb2.StorageConfig(
            type="local"
        ),
        customer_id="customer-789"
    )
    
    response = client.ProvisionMicroVM(request)
    print(f"Created microVM: {response.id}")

if __name__ == "__main__":
    create_microvm()
```

## Monitoring and Metrics

### Health Check Endpoint
```
GET /health
```

Response:
```json
{
  "status": "healthy",
  "version": "1.0.0",
  "uptime": 3600,
  "components": {
    "firecracker": "healthy",
    "lxd": "healthy",
    "storage": "healthy"
  }
}
```

### Metrics Endpoint
```
GET /metrics
```

Prometheus-compatible metrics including:
- `microvm_total` - Total number of microVMs
- `microvm_by_status` - MicroVMs grouped by status
- `resource_utilization` - Resource usage percentages
- `pricing_multiplier` - Current pricing multipliers
- `api_request_duration` - Request latency histograms

## Webhooks

### Event Notifications

The service can send webhook notifications for important events:

```json
{
  "event_type": "microvm.created",
  "microvm_id": "vm-123",
  "customer_id": "customer-456",
  "timestamp": "2023-11-02T10:30:00Z",
  "data": {
    "status": "running",
    "ip_address": "192.168.1.100",
    "ssh_port": 22
  }
}
```

Supported events:
- `microvm.created`
- `microvm.started`
- `microvm.stopped`
- `microvm.deleted`
- `microvm.error`
- `pricing.updated`
- `resource.low`
- `image.built`
- `network.configured`
- `boot.completed`
- `ssh.accessible`

## Troubleshooting

### Common Issues and Solutions

#### VM Boot Failures

**Problem**: VM fails to boot or times out during boot process
```json
{
  "code": "BOOT_TIMEOUT",
  "message": "VM failed to boot within 60 seconds"
}
```

**Solutions**:
1. Check host system requirements:
   ```bash
   curl -X GET http://localhost:8080/api/v1/system/validate
   ```

2. Verify base image integrity:
   ```bash
   curl -X GET http://localhost:8080/api/v1/images/alpine/3.18/validate
   ```

3. Increase boot timeout:
   ```go
   bootReq := &pb.ValidateVMBootRequest{
       MicrovmId:      vmId,
       TimeoutSeconds: 120, // Increased timeout
   }
   ```

#### SSH Access Issues

**Problem**: VM boots but SSH is not accessible
```json
{
  "code": "SSH_UNAVAILABLE",
  "message": "SSH service not accessible on port 2200"
}
```

**Solutions**:
1. Verify network configuration:
   ```bash
   curl -X POST http://localhost:8080/api/v1/network/test \
     -d '{"microvm_id": "vm-123"}'
   ```

2. Check SSH key configuration:
   ```bash
   # Verify SSH key exists and has correct permissions
   ls -la /var/lib/microvm-service/ssh-keys/
   ```

3. Test SSH connectivity manually:
   ```bash
   ssh -i /path/to/key -p 2200 -o ConnectTimeout=10 alpine@localhost
   ```

#### Network Configuration Failures

**Problem**: Host networking setup fails
```json
{
  "code": "NETWORK_SETUP_FAILED",
  "message": "Failed to create bridge interface"
}
```

**Solutions**:
1. Check system permissions:
   ```bash
   # Service needs CAP_NET_ADMIN capability
   sudo setcap cap_net_admin+ep /usr/local/bin/microvm-service
   ```

2. Verify required packages:
   ```bash
   # Install bridge utilities
   sudo apt-get install bridge-utils dnsmasq iptables
   ```

3. Manual network setup:
   ```bash
   # Create bridge manually for testing
   sudo ip link add fc-nat-br0 type bridge
   sudo ip addr add 192.168.100.1/24 dev fc-nat-br0
   sudo ip link set fc-nat-br0 up
   ```

#### Image Build Failures

**Problem**: Base image creation fails
```json
{
  "code": "IMAGE_BUILD_FAILED",
  "message": "Failed to download Alpine packages"
}
```

**Solutions**:
1. Check internet connectivity:
   ```bash
   curl -I https://dl-cdn.alpinelinux.org/alpine/
   ```

2. Verify disk space:
   ```bash
   df -h /var/lib/microvm-service/
   ```

3. Force image rebuild:
   ```go
   buildReq := &pb.BuildBaseImageRequest{
       Distribution: "alpine",
       Version:      "3.18",
       ForceRebuild: true,
   }
   ```

### Diagnostic Commands

#### System Health Check
```bash
# Check overall system health
curl -X GET http://localhost:8080/health

# Validate system requirements
curl -X GET http://localhost:8080/api/v1/system/validate

# Check network configuration
curl -X GET http://localhost:8080/api/v1/network/status
```

#### VM Diagnostics
```bash
# Get VM status and logs
curl -X GET http://localhost:8080/api/v1/vms/{vm-id}/status
curl -X GET http://localhost:8080/api/v1/vms/{vm-id}/console-logs

# Test VM connectivity
curl -X POST http://localhost:8080/api/v1/vms/{vm-id}/test-connectivity
```

#### Image Management
```bash
# List available images
curl -X GET http://localhost:8080/api/v1/images

# Validate specific image
curl -X GET http://localhost:8080/api/v1/images/{distribution}/{version}/validate

# Rebuild image
curl -X POST http://localhost:8080/api/v1/images/{distribution}/{version}/rebuild
```

### Performance Optimization

#### Boot Time Optimization
- Use SSD storage for image directory
- Pre-build base images during off-peak hours
- Configure appropriate VM memory allocation
- Enable image caching and overlays

#### Network Performance
- Use dedicated network interfaces for VM traffic
- Configure appropriate MTU sizes
- Monitor network bandwidth usage
- Implement QoS policies if needed

### Support Resources

For additional help:
- Setup Guide: [BOOTABLE_IMAGES_SETUP.md](BOOTABLE_IMAGES_SETUP.md)
- Troubleshooting Guide: [BOOTABLE_IMAGES_TROUBLESHOOTING.md](BOOTABLE_IMAGES_TROUBLESHOOTING.md)
- Example Scripts: [examples/](../examples/)
- System Validation: `scripts/validate-system-setup.sh`