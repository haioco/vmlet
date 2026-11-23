package firecracker

import (
	"os"
	"strconv"
	"strings"
)

// StatsPollingIntervalS returns the Firecracker balloon statistics polling interval
// (in seconds) to embed in a new VM's config.
//
// The control plane fetches GET /balloon/statistics on its own schedule (2-30 s), so
// Firecracker does not need to ask the guest for balloon stats every second — each
// sample wakes the guest device. Default 10 s; override with
// MICROVM_BALLOON_STATS_POLLING_S (0 keeps Firecracker's own default untouched only
// when explicitly set to 0).
func StatsPollingIntervalS() uint32 {
	const def = 10
	raw := strings.TrimSpace(os.Getenv("MICROVM_BALLOON_STATS_POLLING_S"))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	if n > 300 {
		n = 300
	}
	return uint32(n)
}

// LogLevel returns the Firecracker logger level for new VM configs.
//
// Firecracker logs every HTTP API request at Info ("The API server received a Get
// request on /balloon/statistics"), which the balloon pollers turned into gigabytes
// of console.log. Warning keeps real errors while dropping per-request noise.
// Override with MICROVM_FC_LOG_LEVEL.
func LogLevel() string {
	if v := strings.TrimSpace(os.Getenv("MICROVM_FC_LOG_LEVEL")); v != "" {
		return v
	}
	return "Warning"
}
