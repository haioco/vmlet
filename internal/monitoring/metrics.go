package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// MetricType represents different types of metrics
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
	MetricTypeSummary   MetricType = "summary"
)

// Metric represents a single metric
type Metric struct {
	Name        string                 `json:"name"`
	Type        MetricType             `json:"type"`
	Value       float64                `json:"value"`
	Labels      map[string]string      `json:"labels,omitempty"`
	Timestamp   time.Time              `json:"timestamp"`
	Description string                 `json:"description,omitempty"`
}

// MetricsCollector collects and manages system metrics
type MetricsCollector struct {
	mu      sync.RWMutex
	metrics map[string]*Metric
	logger  *Logger
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(logger *Logger) *MetricsCollector {
	return &MetricsCollector{
		metrics: make(map[string]*Metric),
		logger:  logger,
	}
}

// Counter increments a counter metric
func (mc *MetricsCollector) Counter(name string, labels map[string]string, description string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	key := mc.getMetricKey(name, labels)
	metric, exists := mc.metrics[key]
	if !exists {
		metric = &Metric{
			Name:        name,
			Type:        MetricTypeCounter,
			Value:       0,
			Labels:      labels,
			Description: description,
		}
		mc.metrics[key] = metric
	}

	metric.Value++
	metric.Timestamp = time.Now()
}

// Gauge sets a gauge metric value
func (mc *MetricsCollector) Gauge(name string, value float64, labels map[string]string, description string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	key := mc.getMetricKey(name, labels)
	metric := &Metric{
		Name:        name,
		Type:        MetricTypeGauge,
		Value:       value,
		Labels:      labels,
		Timestamp:   time.Now(),
		Description: description,
	}
	mc.metrics[key] = metric
}

// Histogram records a value in a histogram metric
func (mc *MetricsCollector) Histogram(name string, value float64, labels map[string]string, description string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	key := mc.getMetricKey(name, labels)
	metric, exists := mc.metrics[key]
	if !exists {
		metric = &Metric{
			Name:        name,
			Type:        MetricTypeHistogram,
			Value:       0,
			Labels:      labels,
			Description: description,
		}
		mc.metrics[key] = metric
	}

	// For simplicity, we'll just track the latest value
	// In a production system, you'd want to maintain buckets
	metric.Value = value
	metric.Timestamp = time.Now()
}

// GetMetrics returns all current metrics
func (mc *MetricsCollector) GetMetrics() map[string]*Metric {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	// Return a copy to prevent external modifications
	result := make(map[string]*Metric)
	for k, v := range mc.metrics {
		metricCopy := *v
		if v.Labels != nil {
			metricCopy.Labels = make(map[string]string)
			for lk, lv := range v.Labels {
				metricCopy.Labels[lk] = lv
			}
		}
		result[k] = &metricCopy
	}
	return result
}

// GetMetric returns a specific metric
func (mc *MetricsCollector) GetMetric(name string, labels map[string]string) (*Metric, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	key := mc.getMetricKey(name, labels)
	metric, exists := mc.metrics[key]
	if !exists {
		return nil, false
	}

	// Return a copy
	metricCopy := *metric
	if metric.Labels != nil {
		metricCopy.Labels = make(map[string]string)
		for k, v := range metric.Labels {
			metricCopy.Labels[k] = v
		}
	}
	return &metricCopy, true
}

// RecordMicroVMOperation records metrics for microVM operations
func (mc *MetricsCollector) RecordMicroVMOperation(operation string, microvmID string, duration time.Duration, success bool) {
	labels := map[string]string{
		"operation":   operation,
		"microvm_id":  microvmID,
	}

	// Record operation counter
	if success {
		labels["status"] = "success"
	} else {
		labels["status"] = "failure"
	}
	mc.Counter("microvm_operations_total", labels, "Total number of microVM operations")

	// Record operation duration
	mc.Histogram("microvm_operation_duration_seconds", duration.Seconds(), 
		map[string]string{"operation": operation}, "Duration of microVM operations")
}

// RecordResourceUsage records resource usage metrics
func (mc *MetricsCollector) RecordResourceUsage(nodeID string, usage models.ResourceUsage, capacity models.ResourceCapacity) {
	labels := map[string]string{"node_id": nodeID}

	// CPU metrics
	mc.Gauge("node_cpu_total", float64(capacity.TotalCPU), labels, "Total CPU cores available")
	mc.Gauge("node_cpu_used_lxd", float64(usage.LXDCPU), labels, "CPU cores used by LXD")
	mc.Gauge("node_cpu_used_firecracker", float64(usage.FirecrackerCPU), labels, "CPU cores used by Firecracker")

	// Memory metrics
	mc.Gauge("node_memory_total_mb", float64(capacity.TotalMemoryMB), labels, "Total memory in MB")
	mc.Gauge("node_memory_used_lxd_mb", float64(usage.LXDMemoryMB), labels, "Memory used by LXD in MB")
	mc.Gauge("node_memory_used_firecracker_mb", float64(usage.FirecrackerMemoryMB), labels, "Memory used by Firecracker in MB")

	// Storage metrics
	mc.Gauge("node_storage_total_mb", float64(capacity.TotalStorageMB), labels, "Total storage in MB")
	mc.Gauge("node_storage_used_lxd_mb", float64(usage.LXDStorageMB), labels, "Storage used by LXD in MB")
	mc.Gauge("node_storage_used_firecracker_mb", float64(usage.FirecrackerStorageMB), labels, "Storage used by Firecracker in MB")

	// Utilization percentages
	totalCPU := usage.LXDCPU + usage.FirecrackerCPU
	totalMemory := usage.LXDMemoryMB + usage.FirecrackerMemoryMB
	totalStorage := usage.LXDStorageMB + usage.FirecrackerStorageMB

	if capacity.TotalCPU > 0 {
		mc.Gauge("node_cpu_utilization_percent", float64(totalCPU)/float64(capacity.TotalCPU)*100, labels, "CPU utilization percentage")
	}
	if capacity.TotalMemoryMB > 0 {
		mc.Gauge("node_memory_utilization_percent", float64(totalMemory)/float64(capacity.TotalMemoryMB)*100, labels, "Memory utilization percentage")
	}
	if capacity.TotalStorageMB > 0 {
		mc.Gauge("node_storage_utilization_percent", float64(totalStorage)/float64(capacity.TotalStorageMB)*100, labels, "Storage utilization percentage")
	}
}

// RecordPricingMetrics records pricing-related metrics
func (mc *MetricsCollector) RecordPricingMetrics(tier models.AvailabilityTier, cpuRate, memoryRate, storageRate float64) {
	labels := map[string]string{"tier": string(tier)}

	mc.Gauge("pricing_cpu_rate", cpuRate, labels, "Current CPU pricing rate")
	mc.Gauge("pricing_memory_rate", memoryRate, labels, "Current memory pricing rate")
	mc.Gauge("pricing_storage_rate", storageRate, labels, "Current storage pricing rate")
}

// RecordHealthCheck records health check metrics
func (mc *MetricsCollector) RecordHealthCheck(component string, success bool, duration time.Duration) {
	labels := map[string]string{
		"component": component,
	}

	if success {
		labels["status"] = "success"
	} else {
		labels["status"] = "failure"
	}

	mc.Counter("health_checks_total", labels, "Total number of health checks")
	mc.Histogram("health_check_duration_seconds", duration.Seconds(), 
		map[string]string{"component": component}, "Duration of health checks")
}

// RecordAlert records alert metrics
func (mc *MetricsCollector) RecordAlert(alertType string, severity string, component string) {
	labels := map[string]string{
		"type":      alertType,
		"severity":  severity,
		"component": component,
	}

	mc.Counter("alerts_total", labels, "Total number of alerts generated")
}

// getMetricKey generates a unique key for a metric
func (mc *MetricsCollector) getMetricKey(name string, labels map[string]string) string {
	key := name
	if labels != nil {
		for k, v := range labels {
			key += fmt.Sprintf("_%s_%s", k, v)
		}
	}
	return key
}

// ServeHTTP serves metrics in Prometheus format
func (mc *MetricsCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metrics := mc.GetMetrics()

	switch r.Header.Get("Accept") {
	case "application/json":
		mc.serveJSON(w, metrics)
	default:
		mc.servePrometheus(w, metrics)
	}
}

// serveJSON serves metrics in JSON format
func (mc *MetricsCollector) serveJSON(w http.ResponseWriter, metrics map[string]*Metric) {
	w.Header().Set("Content-Type", "application/json")
	
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal metrics: %v", err), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

// servePrometheus serves metrics in Prometheus format
func (mc *MetricsCollector) servePrometheus(w http.ResponseWriter, metrics map[string]*Metric) {
	w.Header().Set("Content-Type", "text/plain")

	for _, metric := range metrics {
		// Write help comment
		if metric.Description != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", metric.Name, metric.Description)
		}

		// Write type comment
		fmt.Fprintf(w, "# TYPE %s %s\n", metric.Name, metric.Type)

		// Write metric value
		if len(metric.Labels) > 0 {
			fmt.Fprintf(w, "%s{", metric.Name)
			first := true
			for k, v := range metric.Labels {
				if !first {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, "%s=\"%s\"", k, v)
				first = false
			}
			fmt.Fprintf(w, "} %g %d\n", metric.Value, metric.Timestamp.Unix()*1000)
		} else {
			fmt.Fprintf(w, "%s %g %d\n", metric.Name, metric.Value, metric.Timestamp.Unix()*1000)
		}
	}
}

// StartMetricsServer starts an HTTP server to serve metrics
func (mc *MetricsCollector) StartMetricsServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", mc)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server in a goroutine
	go func() {
		mc.logger.Infof("Starting metrics server on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			mc.logger.Error("Metrics server error", err)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()

	// Shutdown server gracefully
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx)
}

// Reset clears all metrics
func (mc *MetricsCollector) Reset() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.metrics = make(map[string]*Metric)
}