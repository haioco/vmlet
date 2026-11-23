package performance

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Connection represents a pooled connection
type Connection interface {
	Close() error
	IsHealthy() bool
	LastUsed() time.Time
	Target() string
}

// ConnectionPool manages a pool of reusable connections
type ConnectionPool struct {
	maxConnections int
	timeout        time.Duration
	idleTimeout    time.Duration
	connections    map[string][]*PooledConnection
	mutex          sync.RWMutex
	totalCreated   int64
	totalReused    int64
}

// PooledConnection wraps a connection with pool metadata
type PooledConnection struct {
	conn       net.Conn
	target     string
	lastUsed   time.Time
	inUse      bool
	created    time.Time
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(maxConnections int, timeout, idleTimeout time.Duration) (*ConnectionPool, error) {
	if maxConnections <= 0 {
		return nil, fmt.Errorf("maxConnections must be positive")
	}

	return &ConnectionPool{
		maxConnections: maxConnections,
		timeout:        timeout,
		idleTimeout:    idleTimeout,
		connections:    make(map[string][]*PooledConnection),
	}, nil
}

// GetConnection gets a connection from the pool or creates a new one
func (cp *ConnectionPool) GetConnection(ctx context.Context, target string) (Connection, error) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	// Try to reuse an existing connection
	if conns, exists := cp.connections[target]; exists {
		for _, conn := range conns {
			if !conn.inUse && conn.IsHealthy() && time.Since(conn.lastUsed) < cp.idleTimeout {
				conn.inUse = true
				conn.lastUsed = time.Now()
				cp.totalReused++
				return conn, nil
			}
		}
		
		// Clean up unhealthy or expired connections
		cp.connections[target] = cp.cleanupConnections(conns)
	}

	// Create new connection if under limit
	if cp.getTotalConnections() < cp.maxConnections {
		conn, err := cp.createConnection(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("failed to create connection: %w", err)
		}
		
		cp.addConnection(target, conn)
		cp.totalCreated++
		return conn, nil
	}

	return nil, fmt.Errorf("connection pool exhausted")
}

// ReleaseConnection returns a connection to the pool
func (cp *ConnectionPool) ReleaseConnection(conn Connection) {
	if pooledConn, ok := conn.(*PooledConnection); ok {
		cp.mutex.Lock()
		defer cp.mutex.Unlock()
		
		pooledConn.inUse = false
		pooledConn.lastUsed = time.Now()
	}
}

// WarmUp pre-creates connections for common targets
func (cp *ConnectionPool) WarmUp(ctx context.Context) error {
	// Common targets for firecracker operations
	targets := []string{
		"unix:///run/firecracker-containerd/containerd.sock",
		"unix:///var/lib/lxd/unix.socket",
	}

	for _, target := range targets {
		// Pre-create 2 connections per target
		for i := 0; i < 2; i++ {
			if _, err := cp.GetConnection(ctx, target); err != nil {
				return fmt.Errorf("failed to warm up connection to %s: %w", target, err)
			}
		}
	}

	return nil
}

// GetUsageRate returns the current usage rate of the pool
func (cp *ConnectionPool) GetUsageRate() float64 {
	cp.mutex.RLock()
	defer cp.mutex.RUnlock()

	total := cp.getTotalConnections()
	if total == 0 {
		return 0.0
	}

	inUse := 0
	for _, conns := range cp.connections {
		for _, conn := range conns {
			if conn.inUse {
				inUse++
			}
		}
	}

	return float64(inUse) / float64(total)
}

// Shutdown closes all connections in the pool
func (cp *ConnectionPool) Shutdown(ctx context.Context) error {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	var errs []error
	for target, conns := range cp.connections {
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("failed to close connection to %s: %w", target, err))
			}
		}
	}

	cp.connections = make(map[string][]*PooledConnection)

	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}

	return nil
}

// createConnection creates a new connection to the target
func (cp *ConnectionPool) createConnection(ctx context.Context, target string) (*PooledConnection, error) {
	dialer := &net.Dialer{
		Timeout: cp.timeout,
	}

	conn, err := dialer.DialContext(ctx, "unix", target)
	if err != nil {
		return nil, err
	}

	return &PooledConnection{
		conn:     conn,
		target:   target,
		lastUsed: time.Now(),
		inUse:    true,
		created:  time.Now(),
	}, nil
}

// addConnection adds a connection to the pool
func (cp *ConnectionPool) addConnection(target string, conn *PooledConnection) {
	if _, exists := cp.connections[target]; !exists {
		cp.connections[target] = make([]*PooledConnection, 0)
	}
	cp.connections[target] = append(cp.connections[target], conn)
}

// getTotalConnections returns the total number of connections
func (cp *ConnectionPool) getTotalConnections() int {
	total := 0
	for _, conns := range cp.connections {
		total += len(conns)
	}
	return total
}

// cleanupConnections removes unhealthy or expired connections
func (cp *ConnectionPool) cleanupConnections(conns []*PooledConnection) []*PooledConnection {
	var healthy []*PooledConnection
	
	for _, conn := range conns {
		if conn.IsHealthy() && time.Since(conn.lastUsed) < cp.idleTimeout {
			healthy = append(healthy, conn)
		} else {
			conn.Close()
		}
	}
	
	return healthy
}

// PooledConnection methods

// Close closes the underlying connection
func (pc *PooledConnection) Close() error {
	if pc.conn != nil {
		return pc.conn.Close()
	}
	return nil
}

// IsHealthy checks if the connection is healthy
func (pc *PooledConnection) IsHealthy() bool {
	if pc.conn == nil {
		return false
	}
	
	// Simple health check - try to set a deadline
	if err := pc.conn.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
		return false
	}
	
	return true
}

// LastUsed returns when the connection was last used
func (pc *PooledConnection) LastUsed() time.Time {
	return pc.lastUsed
}

// Target returns the connection target
func (pc *PooledConnection) Target() string {
	return pc.target
}