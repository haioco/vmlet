# Resource Management Package

This package provides comprehensive resource monitoring and management for the MicroVM service, integrating with LXD to track resource usage and manage microVM allocations.

## Features

- **LXD Integration**: Real-time monitoring of LXD resource usage through HTTP API
- **Resource Pool Management**: Maintains 20% buffer for LXD workloads as per requirements
- **Conflict Detection**: Identifies resource conflicts and overload conditions
- **Dynamic Allocation**: Allocates and deallocates resources for microVMs
- **Health Monitoring**: Tracks service health and LXD connectivity
- **Mock Support**: Includes mock LXD client for development and testing

## Components

### LXD Client (`lxd_client.go`)

Provides interface for interacting with LXD API:

- `HTTPLXDClient`: Production HTTP-based client
- `MockLXDClient`: Development/testing mock client

```go
// Create HTTP client for production
client := NewHTTPLXDClient("http://localhost:8443", "default")

// Create mock client for development
client := NewMockLXDClient()
```

### Resource Manager (`manager.go`)

Core resource management functionality:

- Resource monitoring with 30-second update intervals
- Conflict detection every 15 seconds
- Resource allocation/deallocation tracking
- Event-driven conflict reporting

```go
manager := NewManager(lxdClient, config, "node-1")
ctx := context.Background()

// Start monitoring
if err := manager.Start(ctx); err != nil {
    log.Fatal(err)
}
defer manager.Stop()

// Allocate resources
specs := models.ResourceSpecs{
    VCPUs:     2,
    MemoryMB:  2048,
    StorageMB: 20480,
}

if err := manager.AllocateResources("vm-1", specs); err != nil {
    log.Printf("Allocation failed: %v", err)
}
```

### Resource Service (`service.go`)

High-level service interface:

- Service lifecycle management
- Resource validation
- Conflict event processing
- Health status reporting

```go
service, err := NewService(config, "node-1")
if err != nil {
    log.Fatal(err)
}

// Start service
if err := service.Start(ctx); err != nil {
    log.Fatal(err)
}
defer service.Stop()

// Check if microVM can be accepted
canAccept, reason, err := service.CanAcceptMicroVM(specs)
if canAccept {
    err := service.AllocateResourcesForMicroVM("vm-1", specs)
}
```

## Configuration

Resource management is configured through the main service configuration:

```json
{
  "resources": {
    "buffer_percentage": 0.20,
    "max_vcpus_per_microvm": 16,
    "max_memory_mb_per_microvm": 65536,
    "max_storage_mb_per_microvm": 102400,
    "monitoring_interval_sec": 30,
    "overload_threshold": 0.80
  },
  "lxd": {
    "enabled": true,
    "server_url": "http://localhost:8443",
    "project_name": "default",
    "poll_interval_sec": 30
  }
}
```

## Resource Allocation Logic

The system maintains a 20% buffer for LXD workloads:

```
Available Resources = Total Capacity - LXD Usage - 20% Buffer - Firecracker Allocated
```

### Allocation Process

1. **Validation**: Check resource specifications against limits
2. **Availability Check**: Verify sufficient resources are available
3. **Overload Prevention**: Ensure allocation won't cause node overload
4. **Allocation**: Update resource tracking and availability
5. **Monitoring**: Continue monitoring for conflicts

### Conflict Detection

The system detects several types of conflicts:

- **Overload**: Node utilization exceeds threshold (default 80%)
- **Insufficient Resources**: Not enough resources for allocation
- **LXD Unhealthy**: LXD API is unresponsive
- **Allocation Failure**: Resource allocation failed

## Monitoring and Health

### Resource Utilization Stats

```go
stats, err := service.GetResourceUtilization()
if err == nil {
    fmt.Printf("CPU Utilization: %.1f%%\n", stats.CPUUtilization*100)
    fmt.Printf("Memory Utilization: %.1f%%\n", stats.MemoryUtilization*100)
    fmt.Printf("Overall Utilization: %.1f%%\n", stats.OverallUtilization*100)
    fmt.Printf("Allocated MicroVMs: %d\n", stats.AllocatedMicroVMs)
}
```

### Health Status

```go
health := service.GetHealthStatus()
fmt.Printf("Status: %s\n", health.Status)
fmt.Printf("Recent Conflicts: %d\n", health.RecentConflicts)
```

### Conflict Events

```go
conflicts := service.GetRecentConflicts(10)
for _, conflict := range conflicts {
    fmt.Printf("[%s] %s: %s\n", 
        conflict.Severity, conflict.Type, conflict.Message)
}
```

## Development and Testing

### Mock LXD Client

For development and testing, use the mock LXD client:

```go
cfg := config.DefaultConfig()
cfg.Development.SimulateLXD = true

service, err := NewService(cfg, "test-node")
```

### Running Tests

```bash
go test ./internal/resource -v
```

### Example Usage

See `example_test.go` for a complete usage example.

## Integration Requirements

This package satisfies the following requirements from the specification:

- **2.1**: Continuous monitoring of CPU, memory, and storage utilization on LXD nodes
- **2.2**: Marking excess resources as available when utilization falls below 80%
- **2.3**: Reserving 20% of total resources for LXD workload bursts
- **2.4**: Real-time resource availability updates as LXD workloads change
- **2.5**: Graceful resource reclamation when LXD usage exceeds 80%

## Error Handling

The package provides comprehensive error handling:

- `ErrInsufficientResources`: Not enough resources available
- `ErrNodeOverloaded`: Allocation would cause overload
- `ErrInvalidResourceSpecs`: Invalid resource specifications
- Connection errors for LXD API failures
- Timeout errors for unresponsive operations

## Performance Considerations

- Resource monitoring runs every 30 seconds (configurable)
- Conflict detection runs every 15 seconds
- LXD API calls have 10-second timeout
- Conflict event buffer holds up to 100 events
- Conflict log maintains last 1000 events

## Future Enhancements

- Multi-node resource aggregation
- Predictive resource allocation
- Integration with container orchestration systems
- Advanced conflict resolution strategies
- Resource usage forecasting