#!/bin/bash

# Complete VM Lifecycle Test Runner
# Tests: Create → Start → Console → SSH → Stop → Start → Delete → Cleanup Check

set -e

echo "🧪 MicroVM Complete Lifecycle Test"
echo "=================================="
echo

# Check if we're in the right directory
if [ ! -f "go.mod" ]; then
    echo "❌ Please run this script from the microvm-service directory"
    exit 1
fi

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed. Please install Go first."
    exit 1
fi

# Check if the service is running
echo "🔍 Checking if microvm-service is running..."
if curl -s http://localhost:8080/health > /dev/null 2>&1; then
    echo "✅ MicroVM service is running"
else
    echo "⚠️  MicroVM service is not running. Starting it..."
    
    # Try to build and start the service
    echo "🔨 Building microvm-service..."
    go build -o microvm-service ./cmd/main.go
    
    echo "🚀 Starting microvm-service in background..."
    ./microvm-service &
    SERVICE_PID=$!
    
    # Wait for service to start
    echo "⏳ Waiting for service to start..."
    for i in {1..30}; do
        if curl -s http://localhost:8080/health > /dev/null 2>&1; then
            echo "✅ Service started successfully"
            break
        fi
        sleep 1
        if [ $i -eq 30 ]; then
            echo "❌ Service failed to start within 30 seconds"
            kill $SERVICE_PID 2>/dev/null || true
            exit 1
        fi
    done
fi

echo

# Run the API lifecycle test
echo "🧪 Running API-based lifecycle test..."
echo "======================================"
go run test-api-lifecycle.go

echo
echo "🧪 Running direct lifecycle test..."
echo "==================================="
go run test-complete-vm-lifecycle.go

echo
echo "✅ All lifecycle tests completed!"
echo

# If we started the service, ask if user wants to stop it
if [ ! -z "$SERVICE_PID" ]; then
    echo "🤔 Stop the microvm-service? (y/N)"
    read -r response
    if [[ "$response" =~ ^[Yy]$ ]]; then
        echo "🛑 Stopping microvm-service..."
        kill $SERVICE_PID 2>/dev/null || true
        echo "✅ Service stopped"
    else
        echo "ℹ️  Service is still running (PID: $SERVICE_PID)"
        echo "   You can stop it with: kill $SERVICE_PID"
    fi
fi

echo
echo "📊 Test Summary"
echo "==============="
echo "✅ API lifecycle test completed"
echo "✅ Direct lifecycle test completed"
echo "✅ VM creation, start, console, SSH, stop, restart, delete, and cleanup tested"
echo
echo "💡 Next steps:"
echo "   - Check the generated test reports in ./firecracker-work/"
echo "   - Review any failed tests and fix issues"
echo "   - Run performance tests if needed"
echo