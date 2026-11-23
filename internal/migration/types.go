// Package migration implements cross-node VM migration for Vmlet.
//
// Two flows share the same primitives:
//
//   - Live (near-live) migration: ZFS incremental disk pre-copy while the VM
//     keeps running, followed by a short cutover pause during which the final
//     Firecracker memory snapshot + last disk increment are shipped to the
//     target node and the VM is restored there with its MAC/IP preserved.
//   - Cold export/import: a stopped VM (or a post-crash recovery) is packaged
//     as a portable archive (ZFS send stream or raw image + manifest) that an
//     operator transfers between nodes manually.
//
// Safety invariant (cutover ordering): the disk state on the target must never
// be OLDER than the memory snapshot state. Writes the guest performed after
// the memory snapshot but before the source process is killed are durable on
// disk and are simply replayed as if after a crash; losing them is not an
// option because the guest may already have acknowledged them. Therefore the
// cutover always takes the memory snapshot BEFORE the final disk snapshot, and
// kills the source process before the final disk snapshot is taken.
package migration

import (
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// Phase is the coarse-grained step of a live migration.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhasePreflight   Phase = "preflight"    // peer reachability + compatibility checks
	PhasePrepare     Phase = "prepare"      // target-side reservation (zvol, TAP, dirs)
	PhaseDiskPreCopy Phase = "disk_precopy" // full + incremental ZFS sends, VM keeps running
	PhaseMemPreCopy  Phase = "mem_precopy"  // iterative diff memory snapshots, VM keeps running
	PhaseCutover     Phase = "cutover"      // source stopped, final delta shipped, target restored
	PhaseRunning     Phase = "running"      // VM resumed on target, awaiting confirmation
	PhaseConfirm     Phase = "confirm"      // source cleanup (disk + record + unit)
	PhaseDone        Phase = "done"
	PhaseAborted     Phase = "aborted"
	PhaseFailed      Phase = "failed"
)

// Mode values reported in MigrationStatus.Mode / accepted in
// MigrationRequest.Mode.
const (
	ModeAuto             = "auto"               // cold when CPU generations differ
	ModeLive             = "live"               // memory snapshot + resume on target
	ModeCold             = "cold"               // stop, ship disk, boot fresh on target
	ModeColdBootFallback = "cold_boot_fallback" // restore died; booted fresh on target
)

// MigrationStatus is the serializable state of an in-flight migration,
// reported by GET /vms/{id}/migration and used by the portal for progress UI.
type MigrationStatus struct {
	VMID            string     `json:"vm_id"`
	SourceNode      string     `json:"source_node"`
	TargetNode      string     `json:"target_node"`
	Phase           Phase      `json:"phase"`
	StartedAt       time.Time  `json:"started_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastError       string     `json:"last_error,omitempty"`
	PausedAt        *time.Time `json:"paused_at,omitempty"` // start of the cutover pause
	ResumeAt        *time.Time `json:"resume_at,omitempty"` // guest running again on target
	PauseDurationMS int64      `json:"pause_duration_ms,omitempty"`

	// Mode records how the guest ended up on the target:
	//   "live"                — memory snapshot restored, guest processes kept
	//   "cold"                — disk-only move, guest booted fresh on target
	//   "cold_boot_fallback"  — live restore crashed right after load, the
	//                           target cold-booted the migrated disk instead
	Mode string `json:"mode,omitempty"`

	// Progress counters
	DiskBytesSent  int64 `json:"disk_bytes_sent"`
	MemBytesSent   int64 `json:"mem_bytes_sent"`
	DiskIterations int   `json:"disk_iterations"`
	MemIterations  int   `json:"mem_iterations"`
	LastDiffBytes  int64 `json:"last_diff_bytes"`

	// Compatibility info captured at preflight
	Preflight *PreflightResult `json:"preflight,omitempty"`
}

// NodeInfo describes a node for compatibility checks. It is served by
// GET /migration/info on every node.
type NodeInfo struct {
	NodeDomain     string `json:"node_domain"`
	FirecrackerVer string `json:"firecracker_version"`
	CPUModel       string `json:"cpu_model"`
	CPUVendor      string `json:"cpu_vendor"`
	CPUFlags       string `json:"cpu_flags"`
	KernelVersion  string `json:"kernel_version"`
	TotalMemMB     int64  `json:"total_mem_mb"`
	MemAvailableMB int64  `json:"mem_available_mb"`
	ZFSPool        string `json:"zfs_pool,omitempty"`
	ZFSDataset     string `json:"zfs_dataset,omitempty"`
	ZFSAvailBytes  int64  `json:"zfs_avail_bytes,omitempty"`
	NumVCPUs       int    `json:"num_vcpus"`
	MaxVCPUs       int    `json:"max_vcpus"`
}

// PreflightResult is the outcome of comparing the source node with a target
// node before starting a migration.
type PreflightResult struct {
	OK               bool     `json:"ok"`
	Errors           []string `json:"errors,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	Source           NodeInfo `json:"source"`
	Target           NodeInfo `json:"target"`
	FCVersionMatch   bool     `json:"fc_version_match"`
	CPUSameVendor    bool     `json:"cpu_same_vendor"`
	CPUSameModel     bool     `json:"cpu_same_model"`
	TargetHasZFS     bool     `json:"target_has_zfs"`
	TargetCapacityOK bool     `json:"target_capacity_ok"`
	MemCapacityOK    bool     `json:"mem_capacity_ok"`
	DiskCapacityOK   bool     `json:"disk_capacity_ok"`
}

// MigrationRequest is the body of POST /vms/{id}/migrate.
type MigrationRequest struct {
	// TargetBaseURL is the base API URL of the destination node's vmlet
	// service, e.g. "http://94.182.94.8:9090".
	TargetBaseURL string `json:"target_base_url"`

	// TargetToken overrides the shared per-node secret for this migration
	// (rarely needed; by default the source uses its own configured
	// MICROMVM_MIGRATION_TOKEN, which all nodes share). Excluded from body
	// JSON so it can never leak through the portal-facing endpoint.
	TargetToken string `json:"-"`

	// MaxMemIterations caps diff-snapshot pre-copy loops before forcing cutover.
	// 0 = default (10). 1 = single-pass (no memory pre-copy).
	MaxMemIterations int `json:"max_mem_iterations,omitempty"`

	// MaxDirtyBytes is the dirty-memory threshold below which cutover is
	// triggered, in bytes. 0 = default (64 MiB).
	MaxDirtyBytes int64 `json:"max_dirty_bytes,omitempty"`

	// DiskRateLimitKB throttles zfs send during pre-copy (KB/s, 0 = unlimited).
	DiskRateLimitKB int `json:"disk_rate_limit_kb,omitempty"`

	// Mode selects the cutover strategy:
	//   "auto" (default) — live restore when the CPUs are identical, otherwise
	//                      a disk-only move with a fresh boot on the target
	//   "live"           — require a memory-snapshot restore (identical CPUs)
	//   "cold"           — disk-only move + fresh boot (always safe, slower to
	//                      resume but deterministic; no memory compatibility risk)
	Mode string `json:"mode,omitempty"`

	// DryRun performs preflight + prepare only, then aborts and cleans up.
	DryRun bool `json:"dry_run,omitempty"`
}

// PrepareRequest is the body of POST /migration/prepare (target side).
type PreparePayload struct {
	VM *models.MicroVMInstance `json:"vm"`
	// SourceConfig is the base64 of the source node's firecracker config.json
	// for this VM — the target needs the exact drive topology at restore.
	SourceConfig string `json:"source_config"`
}

// DiskChunkHeader precedes each zfs send stream on the replication channel.
type DiskChunkHeader struct {
	VMID        string `json:"vm_id"`
	Snapshot    string `json:"snapshot"` // full: base snapshot; incremental: "base@from@to"
	Incremental bool   `json:"incremental"`
	FromSnap    string `json:"from_snap,omitempty"`
	ToSnap      string `json:"to_snap"`
	BytesHint   int64  `json:"bytes_hint,omitempty"`
}

// CutoverPayload describes the files shipped to the target at cutover time.
type CutoverPayload struct {
	VMID                string `json:"vm_id"`
	MemFileSHA          string `json:"mem_file_sha256"`
	VmStateSHA          string `json:"vm_state_sha256"`
	FinalDiskFrom       string `json:"final_disk_from"` // last pre-copied zfs snapshot
	FinalDiskTo         string `json:"final_disk_to"`   // cutover zfs snapshot
	TrafficIngressBytes uint64 `json:"traffic_ingress_bytes"`
	TrafficEgressBytes  uint64 `json:"traffic_egress_bytes"`
	// ColdBoot asks the target to skip the snapshot load entirely and boot
	// the received disk as a fresh VM (heterogeneous CPUs / explicit cold mode).
	ColdBoot bool `json:"cold_boot,omitempty"`
}

// Manifest is the metadata file bundled in cold exports.
type Manifest struct {
	Version             int                     `json:"version"`
	CreatedAt           time.Time               `json:"created_at"`
	SourceNode          string                  `json:"source_node"`
	VM                  *models.MicroVMInstance `json:"vm"`
	DiskFile            string                  `json:"disk_file"`
	DiskSHA256          string                  `json:"disk_sha256"`
	DiskFormat          string                  `json:"disk_format"` // "zfs-send" | "raw"
	DiskBytes           int64                   `json:"disk_bytes"`
	TrafficIngressBytes uint64                  `json:"traffic_ingress_bytes"`
	TrafficEgressBytes  uint64                  `json:"traffic_egress_bytes"`
}

const ManifestVersion = 1
