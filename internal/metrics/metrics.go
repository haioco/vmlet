package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metric type descriptors for exposition output.
const (
	TypeCounter = "counter"
	TypeGauge   = "gauge"
)

// series holds one labeled time series.
type series struct {
	labels map[string]string
	value  float64
}

// metric is a named collection of series with a type and help text.
type metric struct {
	typ    string
	help   string
	series map[string]*series // labelsKey -> series
}

// Registry is a minimal thread-safe Prometheus exposition registry.
// Dependency-free by design: no client_golang import.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metric
	order   []string // registration order for stable output
}

// Default is the process-wide registry used by all packages.
var Default = NewRegistry()

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]*metric)}
}

// Register declares a metric name with its type and help text. No-op if present.
func (r *Registry) Register(name, typ, help string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.metrics[name]; ok {
		return
	}
	r.metrics[name] = &metric{typ: typ, help: help, series: make(map[string]*series)}
	r.order = append(r.order, name)
}

func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte(0)
		sb.WriteString(labels[k])
		sb.WriteByte(0)
	}
	return sb.String()
}

// IncCounter adds delta to a counter series, creating it if needed.
func (r *Registry) IncCounter(name string, delta float64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return // unregistered: never silently create (keeps surface explicit)
	}
	key := labelsKey(labels)
	s, ok := m.series[key]
	if !ok {
		cp := make(map[string]string, len(labels))
		for k, v := range labels {
			cp[k] = v
		}
		s = &series{labels: cp}
		m.series[key] = s
	}
	s.value += delta
}

// SetGauge sets a gauge series, creating it if needed.
func (r *Registry) SetGauge(name string, value float64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return
	}
	key := labelsKey(labels)
	s, ok := m.series[key]
	if !ok {
		cp := make(map[string]string, len(labels))
		for k, v := range labels {
			cp[k] = v
		}
		s = &series{labels: cp}
		m.series[key] = s
	}
	s.value = value
}

// DeleteSeries removes every series of a metric whose labels match all given pairs.
// Use with {"vm_id": id} when a VM is deleted to bound cardinality.
func (r *Registry) DeleteSeries(name string, match map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return
	}
	for key, s := range m.series {
		hit := true
		for k, v := range match {
			if s.labels[k] != v {
				hit = false
				break
			}
		}
		if hit {
			delete(m.series, key)
		}
	}
}

// DeleteVM removes all per-VM series for a VM ID across every metric.
func (r *Registry) DeleteVM(vmID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.metrics {
		for key, s := range m.series {
			if s.labels["vm_id"] == vmID {
				delete(m.series, key)
			}
		}
	}
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	v = strings.ReplaceAll(v, "\n", "\\n")
	return v
}

// Render writes the registry in Prometheus text exposition format (0.0.4).
func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	for _, name := range r.order {
		m := r.metrics[name]
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", name, m.help, name, m.typ)

		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := m.series[k]
			labelNames := make([]string, 0, len(s.labels))
			for ln := range s.labels {
				labelNames = append(labelNames, ln)
			}
			sort.Strings(labelNames)
			sb.WriteString(name)
			if len(labelNames) > 0 {
				sb.WriteByte('{')
				for i, ln := range labelNames {
					if i > 0 {
						sb.WriteByte(',')
					}
					fmt.Fprintf(&sb, "%s=\"%s\"", ln, escapeLabelValue(s.labels[ln]))
				}
				sb.WriteByte('}')
			}
			sb.WriteByte(' ')
			fmt.Fprintf(&sb, "%v\n", s.value)
		}
	}
	return sb.String()
}

// Handler serves the default registry at GET /metrics.
func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(Default.Render()))
}

// Package-level helpers on the default registry.

func Register(name, typ, help string) { Default.Register(name, typ, help) }

func IncCounter(name string, labels map[string]string) { Default.IncCounter(name, 1, labels) }

func SetGauge(name string, value float64, labels map[string]string) {
	Default.SetGauge(name, value, labels)
}

func DeleteVM(vmID string) { Default.DeleteVM(vmID) }

// Metric names shared across packages.
const (
	VMsTotal                = "microvm_vms_total"
	HealthCheckFailures     = "microvm_health_check_failures_total"
	RecoveryAttempts        = "microvm_recovery_attempts_total"
	BalloonErrors           = "microvm_balloon_errors_total"
	HostMemoryFreeMB        = "microvm_host_memory_free_mb"
	LastBootDurationSeconds = "microvm_last_boot_duration_seconds"
	StatusSyncs             = "microvm_status_syncs_total"
)

// RegisterAll declares every metric this service exposes. Call once at startup.
func RegisterAll() {
	Register(VMsTotal, TypeGauge, "Number of VMs by status")
	Register(HealthCheckFailures, TypeCounter, "Failed health checks by VM")
	Register(RecoveryAttempts, TypeCounter, "Recovery attempts by VM, action and result")
	Register(BalloonErrors, TypeCounter, "Balloon API errors by VM")
	Register(HostMemoryFreeMB, TypeGauge, "Host free memory in MB")
	Register(LastBootDurationSeconds, TypeGauge, "Last VM boot duration in seconds")
	Register(StatusSyncs, TypeCounter, "VM status sync operations by result")
}
