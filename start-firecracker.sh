#!/bin/bash
# Start MicroVM HTTP Server with Firecracker enabled
# Requires root privileges for network management

set -e

cd "$(dirname "$0")"

echo "🔥 Starting MicroVM HTTP Server with Firecracker..."
echo "⚠️  This requires root privileges for network management"
echo ""

# Stop any running instances
pkill -f http-server 2>/dev/null || true
sleep 1

# Start with Firecracker enabled
export USE_FIRECRACKER=true

echo "Starting server..."
./http-server > /tmp/microvm-http.log 2>&1 &

echo "Waiting for server to initialize..."
sleep 5

# Show logs
echo ""
echo "=== Server Log ==="
tail -30 /tmp/microvm-http.log

echo ""
echo "=== Status ==="
if ps aux | grep -v grep | grep http-server > /dev/null; then
    echo "✅ Server is running"
    echo "📡 API: http://localhost:8081/api/v1/"
    echo "🏥 Health: http://localhost:8081/health"
else
    echo "❌ Server failed to start. Check /tmp/microvm-http.log for details"
    exit 1
fi
