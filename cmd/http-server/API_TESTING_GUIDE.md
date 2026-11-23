# API Testing Guide

This directory contains comprehensive tests for all API endpoints in the MicroVM service.

## Test Files

### `api_test.go`
Core API endpoint tests covering:
- Health and version endpoints
- VM lifecycle (create, list, get, delete, start, stop)
- Image management
- Resource monitoring
- Validation and error handling
- Concurrent operations
- Performance benchmarks

### `api_integration_test.go`
Integration tests for advanced features:
- VM resource updates
- Auto-start configuration
- Console and SSH access
- Traffic monitoring
- Snapshot management
- VM cloning
- Autoscaling
- IPv4 addon management
- Error scenarios
- Response header validation
- Pagination support

## Running Tests

### Quick Test
Run all tests:
```bash
cd microvm-service/cmd/http-server
go test -v
```

### Run Specific Test
```bash
go test -v -run TestHealthEndpoint
go test -v -run TestVMLifecycle
```

### Run Only Short Tests (Fast)
```bash
go test -v -short
```

### Run Integration Tests
```bash
go test -v -run Integration
```

### Run Benchmarks
```bash
go test -bench=. -benchmem
```

### Generate Coverage Report
```bash
go test -coverprofile=coverage.out
go tool cover -html=coverage.out -o coverage.html
# Open coverage.html in browser
```

### Use Test Runner Script
```bash
chmod +x ../../run-api-tests.sh
../../run-api-tests.sh
```

## Test Categories

### Unit Tests
- Individual endpoint functionality
- Request validation
- Response formatting
- Error handling

### Integration Tests
- Complete workflows
- Multi-step operations
- State management
- Concurrent operations

### Benchmarks
- Endpoint performance
- Request throughput
- Memory usage
- Scalability

## Test Structure

Each test follows this pattern:

```go
func TestFeatureName(t *testing.T) {
    // 1. Setup test server
    ts := setupTestServer(t)
    defer ts.cleanup()

    // 2. Prepare test data
    // ...

    // 3. Make HTTP request
    req := httptest.NewRequest("METHOD", "/path", body)
    w := httptest.NewRecorder()
    ts.router.ServeHTTP(w, req)

    // 4. Assert results
    if w.Code != http.StatusOK {
        t.Errorf("Expected status 200, got %d", w.Code)
    }

    // 5. Validate response
    var response ResponseType
    json.NewDecoder(w.Body).Decode(&response)
    // assertions...
}
```

## Mock Data

Tests use mock VMs and data to avoid requiring actual Firecracker processes. Key mocks:

- **Mock VMs**: In-memory VM objects without actual processes
- **Mock Storage**: Temporary directories cleaned up after tests
- **Mock Network**: No actual TAP devices created
- **Mock Managers**: Simplified versions of production managers

## Testing in CI/CD

### GitHub Actions Example
```yaml
name: API Tests

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.21'
      - name: Run tests
        run: |
          cd microvm-service/cmd/http-server
          go test -v -coverprofile=coverage.out
      - name: Upload coverage
        uses: codecov/codecov-action@v3
        with:
          file: ./coverage.out
```

### GitLab CI Example
```yaml
test:
  image: golang:1.21
  script:
    - cd microvm-service/cmd/http-server
    - go test -v -coverprofile=coverage.out
    - go tool cover -func=coverage.out
  artifacts:
    paths:
      - microvm-service/cmd/http-server/coverage.out
```

## Test Coverage Goals

Target coverage by component:
- **Health/Version endpoints**: 100%
- **VM CRUD operations**: >90%
- **Resource management**: >85%
- **Advanced features**: >75%
- **Overall**: >80%

## Writing New Tests

When adding new API endpoints, add corresponding tests:

1. **Create test function** following naming convention `TestFeatureName`
2. **Setup test environment** using `setupTestServer()`
3. **Test success path** with valid inputs
4. **Test error cases** with invalid inputs
5. **Add integration tests** for complex workflows
6. **Add benchmarks** for performance-critical endpoints

Example:
```go
func TestNewFeature(t *testing.T) {
    ts := setupTestServer(t)
    defer ts.cleanup()

    // Test success case
    t.Run("Success", func(t *testing.T) {
        // ...
    })

    // Test validation
    t.Run("InvalidInput", func(t *testing.T) {
        // ...
    })

    // Test error handling
    t.Run("NotFound", func(t *testing.T) {
        // ...
    })
}
```

## Debugging Tests

### Verbose Output
```bash
go test -v
```

### Run Single Test with Logs
```bash
go test -v -run TestSpecificTest 2>&1 | tee test.log
```

### Enable Debug Logging
Set environment variables before running tests:
```bash
export MICROVM_LOG_LEVEL=debug
go test -v
```

### Check Test Logs
Tests write temporary files that are cleaned up. To keep them:
```go
// In test, skip cleanup
// defer ts.cleanup()  // Comment this out
```

## Common Issues

### Tests Failing on Missing Dependencies
Some tests require actual system resources. Mark them as integration tests:
```go
if testing.Short() {
    t.Skip("Skipping integration test in short mode")
}
```

### Race Conditions
Run with race detector:
```bash
go test -race
```

### Memory Leaks
Check for leaks:
```bash
go test -memprofile=mem.prof
go tool pprof mem.prof
```

## Best Practices

1. **Keep tests fast**: Use mocks, avoid I/O
2. **Test isolation**: Each test should be independent
3. **Clear assertions**: Use descriptive error messages
4. **Clean up resources**: Always defer cleanup
5. **Use table-driven tests**: For multiple similar test cases
6. **Test edge cases**: Empty inputs, large values, special characters
7. **Document complex tests**: Add comments explaining logic
8. **Keep tests maintainable**: Refactor shared code into helpers

## Continuous Improvement

- Review test coverage regularly
- Add tests for bug fixes
- Update tests when API changes
- Benchmark performance-critical paths
- Monitor test execution time
- Remove redundant tests

## Resources

- [Go Testing Documentation](https://golang.org/pkg/testing/)
- [HTTP Test Package](https://golang.org/pkg/net/http/httptest/)
- [Testify Assert Library](https://github.com/stretchr/testify)
- [Go Testing Best Practices](https://github.com/golang/go/wiki/TestComments)
