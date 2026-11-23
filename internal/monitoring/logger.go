package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// LogLevel represents different logging levels
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelFatal
)

// String returns the string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	case LogLevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// LogEntry represents a structured log entry
type LogEntry struct {
	Timestamp   time.Time              `json:"timestamp"`
	Level       string                 `json:"level"`
	Component   string                 `json:"component"`
	Operation   string                 `json:"operation,omitempty"`
	Message     string                 `json:"message"`
	Fields      map[string]interface{} `json:"fields,omitempty"`
	Error       string                 `json:"error,omitempty"`
	StackTrace  string                 `json:"stack_trace,omitempty"`
	RequestID   string                 `json:"request_id,omitempty"`
	MicroVMID   string                 `json:"microvm_id,omitempty"`
	NodeID      string                 `json:"node_id,omitempty"`
	Duration    time.Duration          `json:"duration,omitempty"`
}

// Logger provides structured logging capabilities
type Logger struct {
	level      LogLevel
	component  string
	output     io.Writer
	mu         sync.Mutex
	fields     map[string]interface{}
	requestID  string
	microvmID  string
	nodeID     string
}

// NewLogger creates a new logger instance
func NewLogger(component string, level LogLevel, output io.Writer) *Logger {
	if output == nil {
		output = os.Stdout
	}

	return &Logger{
		level:     level,
		component: component,
		output:    output,
		fields:    make(map[string]interface{}),
	}
}

// NewFileLogger creates a logger that writes to a file
func NewFileLogger(component string, level LogLevel, logFile string) (*Logger, error) {
	// Create log directory if it doesn't exist
	dir := filepath.Dir(logFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	// Open log file for appending
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	return NewLogger(component, level, file), nil
}

// WithField adds a field to the logger context
func (l *Logger) WithField(key string, value interface{}) *Logger {
	newLogger := *l
	newLogger.fields = make(map[string]interface{})
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}
	newLogger.fields[key] = value
	return &newLogger
}

// WithFields adds multiple fields to the logger context
func (l *Logger) WithFields(fields map[string]interface{}) *Logger {
	newLogger := *l
	newLogger.fields = make(map[string]interface{})
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}
	for k, v := range fields {
		newLogger.fields[k] = v
	}
	return &newLogger
}

// WithRequestID adds a request ID to the logger context
func (l *Logger) WithRequestID(requestID string) *Logger {
	newLogger := *l
	newLogger.requestID = requestID
	return &newLogger
}

// WithMicroVMID adds a microVM ID to the logger context
func (l *Logger) WithMicroVMID(microvmID string) *Logger {
	newLogger := *l
	newLogger.microvmID = microvmID
	return &newLogger
}

// WithNodeID adds a node ID to the logger context
func (l *Logger) WithNodeID(nodeID string) *Logger {
	newLogger := *l
	newLogger.nodeID = nodeID
	return &newLogger
}

// Debug logs a debug message
func (l *Logger) Debug(message string) {
	l.log(LogLevelDebug, message, "", nil, 0)
}

// Debugf logs a formatted debug message
func (l *Logger) Debugf(format string, args ...interface{}) {
	l.log(LogLevelDebug, fmt.Sprintf(format, args...), "", nil, 0)
}

// Info logs an info message
func (l *Logger) Info(message string) {
	l.log(LogLevelInfo, message, "", nil, 0)
}

// Infof logs a formatted info message
func (l *Logger) Infof(format string, args ...interface{}) {
	l.log(LogLevelInfo, fmt.Sprintf(format, args...), "", nil, 0)
}

// Warn logs a warning message
func (l *Logger) Warn(message string) {
	l.log(LogLevelWarn, message, "", nil, 0)
}

// Warnf logs a formatted warning message
func (l *Logger) Warnf(format string, args ...interface{}) {
	l.log(LogLevelWarn, fmt.Sprintf(format, args...), "", nil, 0)
}

// Error logs an error message
func (l *Logger) Error(message string, err error) {
	stackTrace := ""
	if err != nil {
		stackTrace = l.getStackTrace()
	}
	l.log(LogLevelError, message, l.formatError(err), nil, stackTrace)
}

// Errorf logs a formatted error message
func (l *Logger) Errorf(format string, err error, args ...interface{}) {
	stackTrace := ""
	if err != nil {
		stackTrace = l.getStackTrace()
	}
	l.log(LogLevelError, fmt.Sprintf(format, args...), l.formatError(err), nil, stackTrace)
}

// Fatal logs a fatal message and exits
func (l *Logger) Fatal(message string, err error) {
	stackTrace := l.getStackTrace()
	l.log(LogLevelFatal, message, l.formatError(err), nil, stackTrace)
	os.Exit(1)
}

// Fatalf logs a formatted fatal message and exits
func (l *Logger) Fatalf(format string, err error, args ...interface{}) {
	stackTrace := l.getStackTrace()
	l.log(LogLevelFatal, fmt.Sprintf(format, args...), l.formatError(err), nil, stackTrace)
	os.Exit(1)
}

// LogOperation logs the start and completion of an operation
func (l *Logger) LogOperation(operation string, fn func() error) error {
	start := time.Now()
	l.WithField("operation", operation).Info("Operation started")
	
	err := fn()
	duration := time.Since(start)
	
	if err != nil {
		l.WithField("operation", operation).
			WithField("duration_ms", duration.Milliseconds()).
			Error("Operation failed", err)
	} else {
		l.WithField("operation", operation).
			WithField("duration_ms", duration.Milliseconds()).
			Info("Operation completed successfully")
	}
	
	return err
}

// LogOperationWithContext logs an operation with context cancellation support
func (l *Logger) LogOperationWithContext(ctx context.Context, operation string, fn func(context.Context) error) error {
	start := time.Now()
	l.WithField("operation", operation).Info("Operation started")
	
	err := fn(ctx)
	duration := time.Since(start)
	
	if err != nil {
		if ctx.Err() != nil {
			l.WithField("operation", operation).
				WithField("duration_ms", duration.Milliseconds()).
				Warn("Operation cancelled")
		} else {
			l.WithField("operation", operation).
				WithField("duration_ms", duration.Milliseconds()).
				Error("Operation failed", err)
		}
	} else {
		l.WithField("operation", operation).
			WithField("duration_ms", duration.Milliseconds()).
			Info("Operation completed successfully")
	}
	
	return err
}

// log writes a log entry
func (l *Logger) log(level LogLevel, message, errorStr string, fields map[string]interface{}, stackTrace interface{}) {
	if level < l.level {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry := LogEntry{
		Timestamp: time.Now().UTC(),
		Level:     level.String(),
		Component: l.component,
		Message:   message,
		Fields:    make(map[string]interface{}),
	}

	// Add context fields
	for k, v := range l.fields {
		entry.Fields[k] = v
	}
	if fields != nil {
		for k, v := range fields {
			entry.Fields[k] = v
		}
	}

	// Add context IDs
	if l.requestID != "" {
		entry.RequestID = l.requestID
	}
	if l.microvmID != "" {
		entry.MicroVMID = l.microvmID
	}
	if l.nodeID != "" {
		entry.NodeID = l.nodeID
	}

	// Add error information
	if errorStr != "" {
		entry.Error = errorStr
	}
	if stackTrace != nil {
		if st, ok := stackTrace.(string); ok && st != "" {
			entry.StackTrace = st
		}
	}

	// Marshal to JSON
	data, err := json.Marshal(entry)
	if err != nil {
		// Fallback to standard logging if JSON marshaling fails
		log.Printf("Failed to marshal log entry: %v", err)
		log.Printf("[%s] %s: %s", level.String(), l.component, message)
		return
	}

	// Write to output
	l.output.Write(data)
	l.output.Write([]byte("\n"))
}

// formatError formats an error for logging
func (l *Logger) formatError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// getStackTrace returns the current stack trace
func (l *Logger) getStackTrace() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// SetLevel sets the logging level
func (l *Logger) SetLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// GetLevel returns the current logging level
func (l *Logger) GetLevel() LogLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// Close closes the logger (if it uses a file)
func (l *Logger) Close() error {
	if closer, ok := l.output.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// ParseLogLevel parses a log level string
func ParseLogLevel(level string) (LogLevel, error) {
	switch level {
	case "debug", "DEBUG":
		return LogLevelDebug, nil
	case "info", "INFO":
		return LogLevelInfo, nil
	case "warn", "WARN", "warning", "WARNING":
		return LogLevelWarn, nil
	case "error", "ERROR":
		return LogLevelError, nil
	case "fatal", "FATAL":
		return LogLevelFatal, nil
	default:
		return LogLevelInfo, fmt.Errorf("unknown log level: %s", level)
	}
}