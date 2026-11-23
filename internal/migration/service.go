package migration

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/haioco/vmlet/internal/models"
)

// Hooks are the node-lifecycle integration points the migration package
// needs. The cmd/http-server wiring supplies them from the HTTPServer and
// firecracker.Manager; the package itself stays independent of both.
type Hooks struct {
	// NodeDomain is this node's public domain (MICROVM_NODE_DOMAIN).
	NodeDomain string

	// ---- source-side ----

	// GetVM returns the live VM record.
	GetVM func(vmID string) (*models.MicroVMInstance, bool)

	// SocketPath returns the Firecracker API socket path for a VM.
	SocketPath func(vmID string) string

	// VMWorkDir returns the per-VM work directory (config.json, logs, FIFO).
	VMWorkDir func(vmID string) string

	// IsVMProcessRunning reports whether the firecracker process is alive.
	IsVMProcessRunning func(vmID string) bool

	// StopSourceVM stops the VM's firecracker process without touching disk
	// or record (systemctl stop firecracker-vm@<id>).
	StopSourceVM func(vm *models.MicroVMInstance) error

	// RestartSourceVM cold-boots the VM again on the source node (rollback
	// path when the cutover failed after the process was stopped).
	RestartSourceVM func(vm *models.MicroVMInstance) error

	// CleanupSourceNetwork tears down host-side plumbing (antispoof, NAT
	// fallback, IPv4 addon interfaces) once the VM has moved.
	CleanupSourceNetwork func(vm *models.MicroVMInstance) error

	// RetireSourceRecord removes the VM from the node's map/DB/billing after
	// a confirmed migration (the disk + @mig-rollback snapshot stay until
	// Retire() is called or retention cleanup runs).
	RetireSourceRecord func(vm *models.MicroVMInstance, targetNode string) error

	// TrafficSnapshot returns the VM's billing counters at call time.
	TrafficSnapshot func(vmID string) (ingress, egress uint64)

	// ---- target-side ----

	// VMIDTaken reports whether a VM ID already exists on this node
	// (record, zvol or running process) — used by prepare.
	VMIDTaken func(vmID string) bool

	// SetupTargetNetwork recreates the host-side network for the incoming
	// VM (TAP + bridge + bandwidth + antispoof + NDP/NAT), preserving its
	// MAC/IP. Returns the TAP device name.
	SetupTargetNetwork func(vm *models.MicroVMInstance) (tapName string, err error)

	// TeardownTargetNetwork rolls back SetupTargetNetwork (abort path).
	TeardownTargetNetwork func(vm *models.MicroVMInstance, tapName string) error

	// StartRestoreProcess starts a bare Firecracker process for the restore
	// (api-sock only, no config file) and returns once the socket is up.
	StartRestoreProcess func(vmID string, socketPath string) error

	// StopRestoreProcess stops a restore process started above (abort path).
	StopRestoreProcess func(vmID string) error

	// AdoptVM registers the restored VM in the node map + SQLite, restores
	// traffic counters, invalidates caches and (re)builds config.json so the
	// VM can cold-boot locally after its next stop/start.
	AdoptVM func(vm *models.MicroVMInstance, tapName string, ingress, egress uint64) error

	// DropAdopted reverses AdoptVM (abort after restore path).
	DropAdopted func(vm *models.MicroVMInstance) error

	// ColdBoot starts the VM from its on-disk state (config.json + zvol) —
	// the cold-migration path and the fallback when a snapshot restore does
	// not survive on this CPU.
	ColdBoot func(vm *models.MicroVMInstance) error

	// VerifyVMAlive reports whether the VM's firecracker process exists and
	// the guest is actually running (not just a lingering socket).
	VerifyVMAlive func(vmID string) bool
}

// Service is one node's migration endpoint: it runs the source-side
// orchestration for outgoing migrations and the target-side receiver for
// incoming ones.
type Service struct {
	hooks   *Hooks
	zfs     *ZFSOps
	tokenFn func() string

	mu         sync.Mutex
	outgoing   map[string]*outgoingMigration // vmID -> source-side state
	incoming   map[string]*incomingMigration // vmID -> target-side state
	servedInfo *NodeInfo
}

// outgoingMigration is the source-side state machine instance.
type outgoingMigration struct {
	status *MigrationStatus
	peer   *PeerClient
	req    *MigrationRequest
	vm     *models.MicroVMInstance
	zfs    *ZFSOps
	cold   bool // cold migration (no memory state shipped)
	stopCh chan struct{}

	// The *Snapshot fields mirror VM state for failure-path recovery and are
	// guarded by Service.mu.
	frozenSnapshot bool // guest vCPUs paused for a snapshot, not resumed yet
	pausedSnapshot bool // source process stopped (post-cutover)

	ctx      context.Context
	cancel   context.CancelFunc
	finished bool // claimed by success/abort — no further rollback allowed
}

// incomingMigration is the target-side receive state.
type incomingMigration struct {
	vm        *models.MicroVMInstance
	phase     Phase
	mode      string // "live" | "cold" | "cold_boot_fallback"
	tapName   string
	restored  bool
	confirmed bool
	ingress   uint64
	egress    uint64
	lastDisk  string // last received disk snapshot name
}

// NewService builds the node-local migration service. tokenFn returns this
// node's shared migration secret; the source side uses it to authenticate to
// the peer (all nodes share one token).
func NewService(hooks *Hooks, tokenFn func() string) (*Service, error) {
	if hooks == nil {
		return nil, ErrNilHooks
	}
	info, err := GatherNodeInfo(hooks.NodeDomain)
	if err != nil {
		return nil, err
	}
	return &Service{
		hooks:      hooks,
		tokenFn:    tokenFn,
		zfs:        NewZFSOps(),
		outgoing:   make(map[string]*outgoingMigration),
		incoming:   make(map[string]*incomingMigration),
		servedInfo: info,
	}, nil
}

// MigrationDir resolves where per-VM snapshot artifacts (mem.snap,
// vmstate.snap) live. Defaults to <workdir>/mig — set MICROVM_MIGRATION_DIR
// to move them onto a roomy dataset when the workdir sits on a small rootfs.
func (s *Service) MigrationDir(vmID string) string {
	if d := os.Getenv("MICROVM_MIGRATION_DIR"); d != "" {
		return filepath.Join(d, vmID)
	}
	return filepath.Join(s.hooks.VMWorkDir(vmID), "mig")
}

// Info returns this node's fingerprint (served via GET /migration/info).
func (s *Service) Info() *NodeInfo {
	return s.servedInfo
}

// OutgoingStatus returns the source-side status of a VM's migration, if any.
func (s *Service) OutgoingStatus(vmID string) (*MigrationStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	og, ok := s.outgoing[vmID]
	if !ok {
		return nil, false
	}
	st := *og.status
	return &st, true
}

// IncomingStatus returns the target-side status of a received migration.
func (s *Service) IncomingStatus(vmID string) (*MigrationStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.incoming[vmID]
	if !ok {
		return nil, false
	}
	return &MigrationStatus{
		VMID:      vmID,
		Phase:     in.phase,
		Mode:      in.mode,
		UpdatedAt: now(),
	}, true
}

// token returns the configured node-to-node shared secret.
func (s *Service) token() string {
	if s.tokenFn == nil {
		return ""
	}
	return s.tokenFn()
}
