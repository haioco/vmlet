# Local Testing Environment

This document describes the local testing environment for the MicroVM service, which allows you to test firecracker microVM functionality without requiring production LXD infrastructure.

## Overview

The local testing environment provides:

- **NAT Networking**: Local NAT bridge (192.168.100.0/24) for microVM connectivity
- **Test Images**: Minimal Alpine and Ubuntu rootfs images for testing
- **Port Forwarding**: SSH access to microVMs through port forwarding
- **Resource Simulation**: Simulated LXD resource usage patterns
- **Test Scenarios**: Comprehensive test scenarios for microVM operations

## Prerequisites

### Required Dependencies

- **Go 1.19+**: For building the test runner
- **iptables**: For NAT and port forwarding rules
- **iproute2** (`ip` command): For network interface management
- **wget**: For downloading kernel images
- **coreutils** (`dd`): For creating disk images
- **e2fsprogs** (`mkfs.ext4`): For formatting filesystems

### Optional Dependencies

- **dnsmasq**: For DHCP services on the NAT bridge
- **debootstrap**: For creating Ubuntu rootfs images
- **firecracker**: For actual microVM execution (if available)

### Installation on Ubuntu/Debian

```bash
sudo apt-get update
sudo apt-get install -y \
    golang-go \
    iptables \
    iproute2 \
    wget \
    coreutils \
    e2fsprogs \
    dnsmasq \
    debootstrap
```

## Quick Start

### 1. Setup Test Environment

```bash
# Setup everything (requires sudo for network configuration)
sudo ./scripts/run-local-tests.sh setup

# Or setup without network (manual network setup required)
./scripts/run-local-tests.sh setup
```

### 2. Run All Tests

```bash
./scripts/run-local-tests.sh test
```

### 3. Run Specific Test Scenario

```bash
./scripts/run-local-tests.sh test basic-lifecycle
```

### 4. List Available Scenarios

```bash
./scripts/run-local-tests.sh list
```

### 5. Cleanup

```bash
# Cleanup environment (keep images)
./scripts/run-local-tests.sh cleanup

# Full cleanup (remove images too)
./scripts/run-local-tests.sh cleanup full
```

## Manual Setup

If you prefer to set up components manually:

### 1. Build Test Runner

```bash
cd microvm-service
go build -o test-runner ./cmd/test-runner/main.go
```

### 2. Setup NAT Bridge

```bash
sudo ./scripts/setup-nat-bridge.sh setup
```

### 3. Create Test Images

```bash
./scripts/create-test-images.sh all
```

### 4. Run Tests

```bash
./test-runner -config configs/local-dev.json -workdir ./firecracker-work
```

## Test Scenarios

### Basic Lifecycle
Tests fundamental microVM operations:
- Create microVM
- Start microVM
- Stop microVM
- Delete microVM

### Network Connectivity
Tests network configuration:
- NAT networking setup
- IP address assignment
- Network connectivity verification

### Resource Allocation
Tests resource management:
- Small microVM creation (1 CPU, 128MB RAM)
- Large microVM creation (2 CPU, 512MB RAM)
- Resource allocation verification

### SSH Access
Tests SSH connectivity:
- Port forwarding configuration
- SSH service availability
- Connection command generation

### Multiple MicroVMs
Tests concurrent operations:
- Multiple microVM creation
- Simultaneous startup
- Resource isolation

### Error Handling
Tests error scenarios:
- Invalid image paths
- Duplicate microVM IDs
- Resource conflicts

## Configuration

### Test Environment Configuration

The test environment can be configured through command-line options:

```bash
./test-runner \
    -config configs/local-dev.json \
    -workdir ./firecracker-work \
    -cpu 8 \
    -memory 8192 \
    -storage 100000 \
    -patterns true \
    -timeout 30m
```

### Resource Simulation

The resource simulator supports different usage patterns:

- **Low Usage**: 10-30% CPU, 20-40% memory
- **Medium Usage**: 40-60% CPU, 50-70% memory
- **High Usage**: 70-90% CPU, 80-95% memory
- **Burst Usage**: 85-98% CPU, 90-99% memory
- **Idle Period**: 5-15% CPU, 10-25% memory

### Network Configuration

NAT bridge configuration:
- **Bridge Name**: fc-nat-br0
- **Subnet**: 192.168.100.0/24
- **Gateway**: 192.168.100.1
- **DHCP Range**: 192.168.100.10-192.168.100.200
- **SSH Port Range**: 2200-2300

## Troubleshooting

### Network Setup Issues

**Problem**: NAT bridge creation fails
```bash
# Check if bridge exists
ip link show fc-nat-br0

# Manually create bridge
sudo ip link add name fc-nat-br0 type bridge
sudo ip addr add 192.168.100.1/24 dev fc-nat-br0
sudo ip link set dev fc-nat-br0 up
```

**Problem**: Port forwarding not working
```bash
# Check iptables rules
sudo iptables -t nat -L PREROUTING -n

# Enable IP forwarding
sudo sysctl -w net.ipv4.ip_forward=1
```

### Image Creation Issues

**Problem**: Kernel download fails
```bash
# Manually download kernel
wget -O firecracker-work/images/vmlinux \
    https://github.com/firecracker-microvm/firecracker/releases/download/v1.4.1/vmlinux.bin
```

**Problem**: Rootfs creation fails
```bash
# Create minimal placeholder images
dd if=/dev/zero of=firecracker-work/images/alpine-rootfs.img bs=1M count=512
mkfs.ext4 -F firecracker-work/images/alpine-rootfs.img
```

### Permission Issues

**Problem**: Network operations require root
```bash
# Run network setup separately as root
sudo ./scripts/setup-nat-bridge.sh setup

# Then run tests as regular user
./scripts/run-local-tests.sh test
```

### Resource Simulation Issues

**Problem**: Resource simulation not starting
- Check if ports are available
- Verify configuration file exists
- Check for conflicting processes

## Advanced Usage

### Custom Test Scenarios

You can create custom test scenarios by extending the `ScenarioRunner`:

```go
// Add to internal/testing/scenarios.go
func (sr *ScenarioRunner) createCustomScenario() *TestScenario {
    return &TestScenario{
        Name: "Custom Test",
        Description: "Custom test scenario",
        Steps: []TestStep{
            // Define custom test steps
        },
    }
}
```

### Resource Spike Testing

Simulate resource spikes during testing:

```bash
# In another terminal while tests are running
./test-runner -scenario resource-spike -cpu-spike 0.9 -memory-spike 0.95
```

### Debugging

Enable debug logging:

```bash
./test-runner -config configs/local-dev.json -debug
```

View test logs:

```bash
tail -f firecracker-work/logs/test-runner.log
```

## Integration with CI/CD

### GitHub Actions Example

```yaml
name: Local Tests
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v3
        with:
          go-version: '1.19'
      
      - name: Install dependencies
        run: |
          sudo apt-get update
          sudo apt-get install -y iptables iproute2 wget e2fsprogs
      
      - name: Run tests
        run: |
          sudo ./scripts/run-local-tests.sh test
```

### Docker Testing

```dockerfile
FROM ubuntu:22.04

RUN apt-get update && apt-get install -y \
    golang-go iptables iproute2 wget e2fsprogs dnsmasq

COPY . /app
WORKDIR /app

RUN go build -o test-runner ./cmd/test-runner/main.go

CMD ["./scripts/run-local-tests.sh", "test"]
```

## Performance Benchmarks

Expected performance on typical development machine:

- **Test Environment Setup**: 2-5 minutes
- **Basic Lifecycle Test**: 30-60 seconds
- **Network Connectivity Test**: 45-90 seconds
- **Resource Allocation Test**: 20-40 seconds
- **SSH Access Test**: 60-120 seconds
- **Multiple MicroVMs Test**: 90-180 seconds
- **Error Handling Test**: 10-20 seconds

Total test suite execution: 5-10 minutes

## Limitations

### Current Limitations

1. **No Actual Firecracker**: Tests use simulation, not real firecracker
2. **Limited Network Testing**: Basic connectivity only
3. **Placeholder Images**: Minimal test images, not full OS
4. **Local Only**: No distributed testing capabilities

### Future Enhancements

1. **Real Firecracker Integration**: Use actual firecracker binaries
2. **Container Integration**: Test with real container workloads
3. **Performance Testing**: Benchmark microVM performance
4. **Stress Testing**: High-load scenario testing
5. **Network Isolation**: Advanced networking scenarios

## Contributing

To contribute to the testing framework:

1. Add new test scenarios in `internal/testing/scenarios.go`
2. Extend resource simulation in `internal/testing/resource_simulator.go`
3. Add new test utilities in `internal/testing/`
4. Update documentation in `docs/LOCAL_TESTING.md`

## Support

For issues with the local testing environment:

1. Check the troubleshooting section above
2. Review test logs in `firecracker-work/logs/`
3. Run tests with debug logging enabled
4. Check system requirements and dependencies