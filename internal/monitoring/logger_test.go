package monitoring

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	assert.NotNil(t, logger)
	assert.Equal(t, "test", logger.component)
	assert.Equal(t, LogLevelInfo, logger.level)
}

func TestLogLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelWarn, &buf)

	// Debug and Info should not be logged
	logger.Debug("debug message")
	logger.Info("info message")
	assert.Empty(t, buf.String())

	// Warn should be logged
	logger.Warn("warn message")
	assert.Contains(t, buf.String(), "warn message")
	assert.Contains(t, buf.String(), "WARN")
}

func TestLogWithFields(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	logger.WithField("key1", "value1").WithField("key2", 42).Info("test message")

	var entry LogEntry
	err := json.Unmarshal(buf.Bytes(), &entry)
	assert.NoError(t, err)

	assert.Equal(t, "INFO", entry.Level)
	assert.Equal(t, "test", entry.Component)
	assert.Equal(t, "test message", entry.Message)
	assert.Equal(t, "value1", entry.Fields["key1"])
	assert.Equal(t, float64(42), entry.Fields["key2"]) // JSON unmarshals numbers as float64
}

func TestLogWithContext(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	logger.WithRequestID("req-123").WithMicroVMID("vm-456").WithNodeID("node-789").Info("test message")

	var entry LogEntry
	err := json.Unmarshal(buf.Bytes(), &entry)
	assert.NoError(t, err)

	assert.Equal(t, "req-123", entry.RequestID)
	assert.Equal(t, "vm-456", entry.MicroVMID)
	assert.Equal(t, "node-789", entry.NodeID)
}

func TestLogError(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelError, &buf)

	testErr := assert.AnError
	logger.Error("error occurred", testErr)

	var entry LogEntry
	err := json.Unmarshal(buf.Bytes(), &entry)
	assert.NoError(t, err)

	assert.Equal(t, "ERROR", entry.Level)
	assert.Equal(t, "error occurred", entry.Message)
	assert.Equal(t, testErr.Error(), entry.Error)
	assert.NotEmpty(t, entry.StackTrace)
}

func TestLogOperation(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	// Test successful operation
	err := logger.LogOperation("test_op", func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	assert.NoError(t, err)
	
	// Should have two log entries: start and completion
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Len(t, lines, 2)

	// Check start log
	var startEntry LogEntry
	err = json.Unmarshal([]byte(lines[0]), &startEntry)
	assert.NoError(t, err)
	assert.Contains(t, startEntry.Message, "started")

	// Check completion log
	var endEntry LogEntry
	err = json.Unmarshal([]byte(lines[1]), &endEntry)
	assert.NoError(t, err)
	assert.Contains(t, endEntry.Message, "completed successfully")
	assert.Contains(t, endEntry.Fields, "duration_ms")
}

func TestLogOperationWithError(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	testErr := assert.AnError
	err := logger.LogOperation("test_op", func() error {
		return testErr
	})

	assert.Equal(t, testErr, err)
	
	// Should have two log entries: start and failure
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Len(t, lines, 2)

	// Check failure log
	var endEntry LogEntry
	err = json.Unmarshal([]byte(lines[1]), &endEntry)
	assert.NoError(t, err)
	assert.Contains(t, endEntry.Message, "failed")
	assert.Equal(t, "ERROR", endEntry.Level)
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected LogLevel
		hasError bool
	}{
		{"debug", LogLevelDebug, false},
		{"DEBUG", LogLevelDebug, false},
		{"info", LogLevelInfo, false},
		{"INFO", LogLevelInfo, false},
		{"warn", LogLevelWarn, false},
		{"warning", LogLevelWarn, false},
		{"error", LogLevelError, false},
		{"ERROR", LogLevelError, false},
		{"fatal", LogLevelFatal, false},
		{"FATAL", LogLevelFatal, false},
		{"invalid", LogLevelInfo, true}, // Should default to info with error
	}

	for _, test := range tests {
		level, err := ParseLogLevel(test.input)
		if test.hasError {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
		}
		assert.Equal(t, test.expected, level)
	}
}

func TestNewFileLogger(t *testing.T) {
	// Create a temporary file
	tmpFile := "/tmp/test_log_" + time.Now().Format("20060102150405") + ".log"
	defer os.Remove(tmpFile)

	logger, err := NewFileLogger("test", LogLevelInfo, tmpFile)
	assert.NoError(t, err)
	assert.NotNil(t, logger)

	// Test logging to file
	logger.Info("test message")

	// Close logger to flush
	err = logger.Close()
	assert.NoError(t, err)

	// Read file content
	content, err := os.ReadFile(tmpFile)
	assert.NoError(t, err)
	assert.Contains(t, string(content), "test message")
}

func TestLogLevelString(t *testing.T) {
	tests := []struct {
		level    LogLevel
		expected string
	}{
		{LogLevelDebug, "DEBUG"},
		{LogLevelInfo, "INFO"},
		{LogLevelWarn, "WARN"},
		{LogLevelError, "ERROR"},
		{LogLevelFatal, "FATAL"},
		{LogLevel(999), "UNKNOWN"},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, test.level.String())
	}
}

func TestLoggerSetLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	// Initially at Info level
	logger.Debug("debug1")
	logger.Info("info1")
	assert.Contains(t, buf.String(), "info1")
	assert.NotContains(t, buf.String(), "debug1")

	// Change to Debug level
	logger.SetLevel(LogLevelDebug)
	buf.Reset()

	logger.Debug("debug2")
	logger.Info("info2")
	assert.Contains(t, buf.String(), "debug2")
	assert.Contains(t, buf.String(), "info2")
}

func TestLoggerConcurrency(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test", LogLevelInfo, &buf)

	// Test concurrent logging
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(id int) {
			logger.WithField("goroutine", id).Info("concurrent log")
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should have 10 log entries
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Len(t, lines, 10)
}