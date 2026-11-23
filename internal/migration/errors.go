package migration

import (
	"errors"
	"time"
)

var (
	ErrNilHooks          = errors.New("migration: hooks must not be nil")
	ErrVMNotRunning      = errors.New("migration: VM is not running")
	ErrVMNotFound        = errors.New("migration: VM not found")
	ErrNoZvol            = errors.New("migration: VM has no ZFS zvol on this node; only ZFS-backed VMs can be live-migrated")
	ErrMigrationInFlight = errors.New("migration: a migration for this VM is already in flight")
	ErrVMIDTaken         = errors.New("migration: VM ID already exists on this node")
	ErrNoPending         = errors.New("migration: no pending migration for this VM on this node")
	ErrTokenRequired     = errors.New("migration: X-Migration-Token missing or invalid")
	ErrNotPrepared       = errors.New("migration: disk channel not prepared (call /migration/prepare first)")
	ErrAlreadyRestored   = errors.New("migration: VM already restored on this node")
	ErrFCVersionMismatch = errors.New("migration: firecracker version mismatch between nodes")
)

func now() time.Time { return time.Now().UTC() }
