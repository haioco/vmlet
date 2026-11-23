package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Migration defaults.
const (
	defaultMaxMemIterations = 10
	defaultMaxDirtyBytes    = 64 << 20 // 64 MiB
	defaultMaxDiskIters     = 5
	minDirtyBytes           = 4 << 20 // never loop below 4 MiB of dirt
	restoreConfirmTimeout   = 180 * time.Second
)

// StartLive kicks off a migration of vmID to the peer described by req.
// It validates preconditions synchronously (caller gets an immediate error
// for anything user-fixable) and then continues in the background.
func (s *Service) StartLive(vmID string, req *MigrationRequest) (*MigrationStatus, error) {
	vm, ok := s.hooks.GetVM(vmID)
	if !ok {
		return nil, ErrVMNotFound
	}
	if !vm.IsRunning() || !s.hooks.IsVMProcessRunning(vmID) {
		return nil, ErrVMNotRunning
	}
	if !s.zfs.VMExists(vmID) {
		return nil, ErrNoZvol
	}
	if req.TargetBaseURL == "" {
		return nil, fmt.Errorf("target_base_url is required")
	}
	mode, err := normalizeMode(req.Mode)
	if err != nil {
		return nil, err
	}
	peerToken := req.TargetToken
	if peerToken == "" {
		peerToken = s.token() // shared per-node secret
	}
	if peerToken == "" {
		return nil, fmt.Errorf("no migration token configured on this node (MICROVM_MIGRATION_TOKEN)")
	}

	s.mu.Lock()
	if prev, busy := s.outgoing[vmID]; busy && !prev.finished {
		s.mu.Unlock()
		return nil, ErrMigrationInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &MigrationStatus{
		VMID:       vmID,
		SourceNode: s.hooks.NodeDomain,
		TargetNode: req.TargetBaseURL,
		Phase:      PhasePreflight,
		Mode:       mode,
		StartedAt:  now(),
		UpdatedAt:  now(),
	}
	og := &outgoingMigration{
		status: st,
		req:    req,
		vm:     vm,
		zfs:    s.zfs,
		peer:   NewPeerClient(req.TargetBaseURL, peerToken),
		stopCh: make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
	s.outgoing[vmID] = og
	s.mu.Unlock()

	// Synchronous preflight so the caller sees reachability/compatibility
	// errors at once.
	res, err := s.preflight(og)
	if err != nil {
		s.failOutgoing(og, err)
		return og.status, err
	}

	// Resolve the cutover strategy. vCPU state is not portable across CPU
	// generations, so "auto" only attempts a live restore when the models
	// match exactly.
	if mode == ModeAuto {
		if res.CPUSameModel {
			mode = ModeLive
		} else {
			mode = ModeCold
			logf(vmID, "CPU models differ (%s → %s) — using cold migration",
				res.Source.CPUModel, res.Target.CPUModel)
		}
	}
	if mode == ModeLive && !res.CPUSameModel {
		err := fmt.Errorf("live migration requires identical CPU models (source %q, target %q) — use mode=cold or mode=auto",
			res.Source.CPUModel, res.Target.CPUModel)
		s.failOutgoing(og, err)
		return og.status, err
	}
	s.mu.Lock()
	og.cold = mode == ModeCold
	og.status.Mode = mode
	og.status.UpdatedAt = now()
	s.mu.Unlock()

	if err := s.peerPrepare(og); err != nil {
		s.failOutgoing(og, err)
		return og.status, err
	}
	if req.DryRun {
		_ = s.abortOutgoing(vmID, "dry run")
		return og.status, nil
	}

	go s.runMigration(og)
	return og.status, nil
}

func normalizeMode(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeLive:
		return ModeLive, nil
	case ModeCold:
		return ModeCold, nil
	}
	return "", fmt.Errorf("invalid mode %q (want auto, live or cold)", m)
}

// AbortSource tears down an in-flight source-side migration, restoring the
// VM on the source node if the cutover had already stopped it.
func (s *Service) AbortSource(vmID string) error {
	return s.abortOutgoing(vmID, "aborted by operator")
}

func (s *Service) abortOutgoing(vmID, reason string) error {
	s.mu.Lock()
	og, ok := s.outgoing[vmID]
	if !ok {
		s.mu.Unlock()
		return ErrNoPending
	}
	if og.finished {
		// Completed migration — nothing to roll back.
		delete(s.outgoing, vmID)
		s.mu.Unlock()
		return ErrNoPending
	}
	og.finished = true
	og.status.Phase = PhaseAborted
	og.status.LastError = reason
	og.status.UpdatedAt = now()
	frozen, paused := og.frozenSnapshot, og.pausedSnapshot
	delete(s.outgoing, vmID)
	s.mu.Unlock()

	og.cancel() // stop the orchestrator goroutine
	_ = og.peer.Abort(vmID)
	s.recoverSource(og, reason, frozen, paused)
	_ = s.zfs.DeleteSnapshots(vmID)
	_ = os.RemoveAll(s.MigrationDir(vmID))
	logf(vmID, "aborted: %s", reason)
	return nil
}

func (s *Service) failOutgoing(og *outgoingMigration, err error) {
	s.mu.Lock()
	if og.finished {
		s.mu.Unlock()
		return
	}
	og.finished = true
	og.status.Phase = PhaseFailed
	og.status.LastError = err.Error()
	og.status.UpdatedAt = now()
	frozen, paused := og.frozenSnapshot, og.pausedSnapshot
	delete(s.outgoing, og.vm.ID)
	s.mu.Unlock()

	og.cancel()
	_ = og.peer.Abort(og.vm.ID)
	s.recoverSource(og, err.Error(), frozen, paused)
	_ = s.zfs.DeleteSnapshots(og.vm.ID)
	_ = os.RemoveAll(s.MigrationDir(og.vm.ID))
	logf(og.vm.ID, "failed: %v", err)
}

// recoverSource brings the source VM back after a failed/aborted attempt: it
// resumes a frozen guest, or restarts a process stopped during cutover.
func (s *Service) recoverSource(og *outgoingMigration, why string, frozen, paused bool) {
	vmID := og.vm.ID
	if frozen && !paused {
		fc := NewSnapshotClient(s.hooks.SocketPath(vmID))
		ctx, cancel := restoreCTX()
		rErr := fc.Resume(ctx)
		cancel()
		if rErr == nil {
			s.setFrozen(og, false)
			logf(vmID, "guest resumed after failed migration: %s", why)
		} else {
			logf(vmID, "⚠️  resume failed (%v) — cold-restarting the VM", rErr)
			_ = s.hooks.StopSourceVM(og.vm)
			paused = true
		}
	}
	if paused {
		if rbErr := s.hooks.RestartSourceVM(og.vm); rbErr != nil {
			logf(vmID, "ROLLBACK FAILED after %s — VM left stopped: %v", why, rbErr)
		} else {
			s.setPaused(og, false)
			logf(vmID, "rolled back: VM restarted on source node (%s)", why)
		}
	}
}

// preflight compares this node with the target, enforces capacity and stores
// the result. CPU-generation incompatibilities are reported but not fatal
// here — the mode decides whether they matter (cold migration ignores them).
func (s *Service) preflight(og *outgoingMigration) (*PreflightResult, error) {
	peerInfo, err := og.peer.Info()
	if err != nil {
		return nil, fmt.Errorf("cannot reach target node: %w", err)
	}
	res := CompareNodes(s.servedInfo, peerInfo)

	memOK, diskOK, capErrs, capWarns := CheckCapacity(peerInfo,
		og.vm.Resources.MemoryMB, og.vm.Resources.StorageMB)
	res.MemCapacityOK = memOK
	res.DiskCapacityOK = diskOK
	res.Errors = append(res.Errors, capErrs...)
	res.Warnings = append(res.Warnings, capWarns...)
	res.OK = len(hardErrors(res.Errors)) == 0

	s.mu.Lock()
	og.status.Preflight = res
	og.status.UpdatedAt = now()
	s.mu.Unlock()

	if hard := hardErrors(res.Errors); len(hard) > 0 {
		return res, fmt.Errorf("preflight failed: %v", hard)
	}
	logf(og.vm.ID, "preflight OK (target %s, fc %s, cpu %s, mem_avail %d MB)",
		peerInfo.NodeDomain, peerInfo.FirecrackerVer, peerInfo.CPUModel, peerInfo.MemAvailableMB)
	for _, w := range res.Warnings {
		logf(og.vm.ID, "preflight warning: %s", w)
	}
	return res, nil
}

// hardErrors filters out the CPU-architecture complaints (only relevant to a
// live restore; a cold migration boots a fresh guest).
func hardErrors(errs []string) []string {
	var out []string
	for _, e := range errs {
		if !strings.Contains(e, "CPU") {
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) peerPrepare(og *outgoingMigration) error {
	// Ship the source firecracker config.json so the target can rebuild the
	// exact drive topology at restore time.
	workDir := s.hooks.VMWorkDir(og.vm.ID)
	cfg, err := os.ReadFile(filepath.Join(workDir, "config.json"))
	if err != nil {
		return fmt.Errorf("read source config.json: %w", err)
	}

	payload := &PreparePayload{VM: og.vm, SourceConfig: base64Std(cfg)}
	if err := og.peer.Prepare(payload); err != nil {
		return fmt.Errorf("target prepare: %w", err)
	}

	// Ship auxiliary drive files (cloudinit seed, small data images) listed
	// in the config. Anything file-backed that is too large to stream is a
	// hard stop — only ZFS-backed rootfs migration is supported.
	if err := s.shipAuxDrives(og, workDir); err != nil {
		_ = og.peer.Abort(og.vm.ID)
		return err
	}

	s.setPhase(og, PhasePrepare)
	return nil
}

const auxDriveMaxBytes = 256 << 20 // 256 MiB cap for file-shipped drives

func (s *Service) shipAuxDrives(og *outgoingMigration, workDir string) error {
	var cfg struct {
		Drives []struct {
			DriveID      string `json:"drive_id"`
			PathOnHost   string `json:"path_on_host"`
			IsRootDevice bool   `json:"is_root_device"`
		} `json:"drives"`
	}
	raw, err := os.ReadFile(filepath.Join(workDir, "config.json"))
	if err != nil {
		return fmt.Errorf("re-read config.json for aux drives: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json drives: %w", err)
	}
	for _, d := range cfg.Drives {
		if d.IsRootDevice || d.PathOnHost == "" {
			continue
		}
		fi, err := os.Stat(d.PathOnHost)
		if err != nil {
			// Cloud-init seed may not exist for some images; the target
			// stubs read-only drives that are missing.
			if strings.Contains(d.PathOnHost, "cloudinit") || strings.Contains(d.PathOnHost, "seed") {
				continue
			}
			return fmt.Errorf("aux drive %s: %w", d.DriveID, err)
		}
		if fi.Size() > auxDriveMaxBytes {
			return fmt.Errorf("aux drive %s (%s) is %d bytes — file-backed drives larger than %d bytes are not live-migratable",
				d.DriveID, d.PathOnHost, fi.Size(), auxDriveMaxBytes)
		}
		f, err := os.Open(d.PathOnHost)
		if err != nil {
			return err
		}
		rel := filepath.Base(d.PathOnHost)
		_, sendErr := og.peer.SendFile(og.ctx, og.vm.ID, rel, f)
		f.Close()
		if sendErr != nil {
			return fmt.Errorf("ship aux drive %s: %w", d.DriveID, sendErr)
		}
		logf(og.vm.ID, "shipped aux drive file %s", rel)
	}
	return nil
}

func (s *Service) setPhase(og *outgoingMigration, p Phase) {
	s.mu.Lock()
	og.status.Phase = p
	og.status.UpdatedAt = now()
	s.mu.Unlock()
	logf(og.vm.ID, "phase -> %s", p)
}

func (s *Service) addBytes(og *outgoingMigration, kind string, n int64) {
	s.mu.Lock()
	switch kind {
	case "disk":
		og.status.DiskBytesSent += n
	case "mem":
		og.status.MemBytesSent += n
	}
	og.status.UpdatedAt = now()
	s.mu.Unlock()
}

func (s *Service) setFrozen(og *outgoingMigration, v bool) {
	s.mu.Lock()
	og.frozenSnapshot = v
	s.mu.Unlock()
}

func (s *Service) setPaused(og *outgoingMigration, v bool) {
	s.mu.Lock()
	og.pausedSnapshot = v
	s.mu.Unlock()
}

// runMigration dispatches to the live or cold state machine and converts a
// panic into a normal failure (with rollback).
func (s *Service) runMigration(og *outgoingMigration) {
	defer func() {
		if r := recover(); r != nil {
			s.failOutgoing(og, fmt.Errorf("panic during migration: %v", r))
		}
	}()
	if og.cold {
		s.runCold(og)
		return
	}
	s.runLive(og)
}

// diskPreCopy snapshots the zvol and streams the full image plus a bounded
// number of increments while the VM keeps serving.
func (s *Service) diskPreCopy(og *outgoingMigration) (string, error) {
	vmID := og.vm.ID
	s.setPhase(og, PhaseDiskPreCopy)

	base := migSnapName("base", int(time.Now().Unix()))
	if err := s.zfs.Snapshot(vmID, base); err != nil {
		return "", err
	}
	n, err := og.shipDiskFull(og.ctx, base)
	if err != nil {
		return "", fmt.Errorf("full disk send: %w", err)
	}
	s.addBytes(og, "disk", n)
	last := base

	for i := 0; i < defaultMaxDiskIters; i++ {
		if err := og.ctx.Err(); err != nil {
			return "", err
		}
		snap := migSnapName("disk", i+1)
		if err := s.zfs.Snapshot(vmID, snap); err != nil {
			return "", err
		}
		est := s.zfs.EstimatedIncrementalBytes(vmID, last, snap)
		if est > 0 && est < minDirtyBytes {
			// negligible dirt — the final cutover delta will be tiny
			_ = execZfs("destroy", s.zfs.VM(vmID)+"@"+snap)
			break
		}
		n, err := og.shipDiskIncremental(og.ctx, last, snap)
		if err != nil {
			return "", fmt.Errorf("incremental disk send %s: %w", snap, err)
		}
		s.addBytes(og, "disk", n)
		last = snap
		s.mu.Lock()
		og.status.DiskIterations = i + 1
		s.mu.Unlock()
	}
	return last, nil
}

func (s *Service) beginCutover(og *outgoingMigration) {
	s.setPhase(og, PhaseCutover)
	s.mu.Lock()
	og.status.PausedAt = ptrTime(now())
	s.mu.Unlock()
}

// stopAndShipFinalDisk stops the source process (the guest must already be
// quiesced for the live path) and ships the last disk delta.
func (s *Service) stopAndShipFinalDisk(og *outgoingMigration, last string) (string, error) {
	vmID := og.vm.ID
	if err := s.hooks.StopSourceVM(og.vm); err != nil {
		return "", fmt.Errorf("stop source VM: %w", err)
	}
	s.mu.Lock()
	og.pausedSnapshot = true
	og.frozenSnapshot = false
	s.mu.Unlock()

	final := migSnapName("final", int(time.Now().Unix()))
	if err := s.zfs.Snapshot(vmID, final); err != nil {
		return "", err
	}
	n, err := og.shipDiskIncremental(og.ctx, last, final)
	if err != nil {
		return "", fmt.Errorf("final disk send: %w", err)
	}
	s.addBytes(og, "disk", n)
	return final, nil
}

// runCold migrates without carrying memory state: ship the disk, stop, final
// delta, and boot the guest fresh on the target.
func (s *Service) runCold(og *outgoingMigration) {
	vmID := og.vm.ID
	last, err := s.diskPreCopy(og)
	if err != nil {
		s.failOutgoing(og, err)
		return
	}

	s.beginCutover(og)
	final, err := s.stopAndShipFinalDisk(og, last)
	if err != nil {
		s.failOutgoing(og, err)
		return
	}

	ingress, egress := s.hooks.TrafficSnapshot(vmID)
	payload := &CutoverPayload{
		VMID:                vmID,
		FinalDiskFrom:       last,
		FinalDiskTo:         final,
		TrafficIngressBytes: ingress,
		TrafficEgressBytes:  egress,
		ColdBoot:            true,
	}
	if err := og.peer.Restore(vmID, payload); err != nil {
		s.failOutgoing(og, fmt.Errorf("target cold restore: %w", err))
		return
	}
	s.finishOutgoing(og)
}

// runLive ships disk pre-copies, then iterates memory snapshots, then freezes
// the guest for the final delta and restore.
func (s *Service) runLive(og *outgoingMigration) {
	vmID := og.vm.ID
	last, err := s.diskPreCopy(og)
	if err != nil {
		s.failOutgoing(og, err)
		return
	}

	maxIters := og.req.MaxMemIterations
	if maxIters <= 0 {
		maxIters = defaultMaxMemIterations
	}
	maxDirty := og.req.MaxDirtyBytes
	if maxDirty <= 0 {
		maxDirty = defaultMaxDirtyBytes
	}

	migDir := s.MigrationDir(vmID)
	if err := os.MkdirAll(migDir, 0755); err != nil {
		s.failOutgoing(og, err)
		return
	}
	memFile := filepath.Join(migDir, "mem.snap")
	vmState := filepath.Join(migDir, "vmstate.snap")
	fc := NewSnapshotClient(s.hooks.SocketPath(vmID))

	s.setPhase(og, PhaseMemPreCopy)

	// Full memory snapshot first; the mem file is a full-size RAM image the
	// target receives densely once. Every later diff patches it by offset.
	if err := s.takeSnapshot(og, fc, "Full", vmState, memFile, false); err != nil {
		s.failOutgoing(og, fmt.Errorf("full mem snapshot: %w", err))
		return
	}
	memSize := fileSize(memFile)
	if memSize <= 0 {
		s.failOutgoing(og, fmt.Errorf("memory snapshot file empty after full snapshot"))
		return
	}
	if n, err := og.shipFileRange(og.ctx, memFile, 0, memSize); err != nil {
		s.failOutgoing(og, fmt.Errorf("ship full mem: %w", err))
		return
	} else {
		s.addBytes(og, "mem", n)
	}
	if n, err := og.shipVMState(og.ctx, vmState); err != nil {
		s.failOutgoing(og, fmt.Errorf("ship vmstate: %w", err))
		return
	} else {
		s.addBytes(og, "mem", n)
	}
	s.mu.Lock()
	og.status.MemIterations = 1
	s.mu.Unlock()

	// Iterative diff snapshots until the dirty set is small enough.
	for i := 2; i <= maxIters; i++ {
		if err := og.ctx.Err(); err != nil {
			s.failOutgoing(og, err)
			return
		}
		if err := s.takeSnapshot(og, fc, "Diff", vmState, memFile, false); err != nil {
			s.failOutgoing(og, fmt.Errorf("diff mem snapshot #%d: %w", i, err))
			return
		}
		n, err := og.shipMemExtents(og.ctx, memFile)
		if err != nil {
			s.failOutgoing(og, fmt.Errorf("ship mem delta #%d: %w", i, err))
			return
		}
		s.addBytes(og, "mem", n)
		if n, err := og.shipVMState(og.ctx, vmState); err != nil {
			s.failOutgoing(og, fmt.Errorf("ship vmstate #%d: %w", i, err))
			return
		} else {
			s.addBytes(og, "mem", n)
		}
		s.mu.Lock()
		og.status.MemIterations = i
		og.status.LastDiffBytes = n
		s.mu.Unlock()

		if n < maxDirty {
			logf(vmID, "mem pre-copy converged after %d iterations (delta=%d bytes < %d)", i, n, maxDirty)
			break
		}
	}

	// ---- Cutover: freeze, ship the exact final state, stop, ship disk ----
	// Keeping the guest frozen after the final snapshot removes the
	// memory/disk skew: no guest writes exist between the memory image and
	// the disk delta taken a moment later.
	s.beginCutover(og)
	if err := s.takeSnapshot(og, fc, "Diff", vmState, memFile, true); err != nil {
		s.failOutgoing(og, fmt.Errorf("final mem snapshot: %w", err))
		return
	}
	if n, err := og.shipMemExtents(og.ctx, memFile); err != nil {
		s.failOutgoing(og, fmt.Errorf("ship final mem delta: %w", err))
		return
	} else {
		s.addBytes(og, "mem", n)
	}
	if n, err := og.shipVMState(og.ctx, vmState); err != nil {
		s.failOutgoing(og, fmt.Errorf("ship final vmstate: %w", err))
		return
	} else {
		s.addBytes(og, "mem", n)
	}

	final, err := s.stopAndShipFinalDisk(og, last)
	if err != nil {
		s.failOutgoing(og, err)
		return
	}

	ingress, egress := s.hooks.TrafficSnapshot(vmID)
	payload := &CutoverPayload{
		VMID:                vmID,
		FinalDiskFrom:       last,
		FinalDiskTo:         final,
		TrafficIngressBytes: ingress,
		TrafficEgressBytes:  egress,
	}
	if err := og.peer.Restore(vmID, payload); err != nil {
		s.failOutgoing(og, fmt.Errorf("target restore: %w", err))
		return
	}
	s.finishOutgoing(og)
}

// waitTargetRunning polls the peer until the VM reports running and returns
// the peer's mode ("live" / "cold" / "cold_boot_fallback").
func (s *Service) waitTargetRunning(og *outgoingMigration) (string, error) {
	deadline := time.Now().Add(restoreConfirmTimeout)
	for {
		st, err := og.peer.Status(og.vm.ID)
		if err == nil && st.Phase == PhaseRunning {
			return st.Mode, nil
		}
		select {
		case <-og.ctx.Done():
			return "", fmt.Errorf("aborted while waiting for target")
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("target did not reach running state in time")
		}
	}
}

// finishOutgoing confirms the target, retires the source VM and records the
// final status.
func (s *Service) finishOutgoing(og *outgoingMigration) {
	vmID := og.vm.ID

	peerMode, err := s.waitTargetRunning(og)
	if err != nil {
		s.failOutgoing(og, err)
		return
	}
	s.setPhase(og, PhaseRunning)

	if err := og.peer.Confirm(vmID); err != nil {
		// The VM IS running on the target; a confirm failure is a bookkeeping
		// problem — surface it but do not kill either side.
		logf(vmID, "⚠️  target confirm failed (VM stays running on target): %v", err)
	}

	// Drop the pre-copy bookkeeping snapshots first (DeleteSnapshots removes
	// every mig-* snapshot, so the rollback one is created AFTER it), then
	// keep one clean rollback snapshot for retention.
	_ = s.zfs.DeleteSnapshots(vmID)
	rollback := "mig-rollback-" + strconv.FormatInt(time.Now().Unix(), 10)
	if err := s.zfs.Snapshot(vmID, rollback); err != nil {
		logf(vmID, "⚠️  could not create rollback snapshot %s: %v", rollback, err)
	}
	if err := s.hooks.CleanupSourceNetwork(og.vm); err != nil {
		logf(vmID, "⚠️  source network cleanup incomplete: %v", err)
	}
	if err := s.hooks.RetireSourceRecord(og.vm, og.req.TargetBaseURL); err != nil {
		logf(vmID, "⚠️  source record retirement incomplete: %v", err)
	}
	_ = os.RemoveAll(s.MigrationDir(vmID))

	s.mu.Lock()
	resume := now()
	og.finished = true
	switch {
	case peerMode != "":
		og.status.Mode = peerMode
	case og.cold:
		og.status.Mode = ModeCold
	default:
		og.status.Mode = ModeLive
	}
	og.status.Phase = PhaseDone
	og.status.ResumeAt = &resume
	if og.status.PausedAt != nil {
		og.status.PauseDurationMS = resume.Sub(*og.status.PausedAt).Milliseconds()
	}
	og.status.UpdatedAt = now()
	s.mu.Unlock()

	logf(vmID, "✅ migration complete (mode=%s, pause=%d ms, disk=%d B, mem=%d B)",
		og.status.Mode, og.status.PauseDurationMS, og.status.DiskBytesSent, og.status.MemBytesSent)
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// takeSnapshot pauses the VM, takes a Full or Diff snapshot and (unless
// keepFrozen) resumes it. FC v1.13 refuses snapshot creation on a running VM.
// Freeze state is tracked so failure paths can recover a half-paused VM.
func (s *Service) takeSnapshot(og *outgoingMigration, fc *SnapshotClient, snapshotType, vmState, memFile string, keepFrozen bool) error {
	// The FULL snapshot writes the whole guest RAM through the (possibly
	// saturated) host pool — give it a long leash. Diff snapshots only write
	// the dirty delta and stay quick.
	deadline := 10 * time.Minute
	if snapshotType == "Full" {
		deadline = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(og.ctx, deadline)
	defer cancel()

	if err := fc.Pause(ctx); err != nil {
		return fmt.Errorf("pause before %s snapshot: %w", snapshotType, err)
	}
	s.setFrozen(og, true)
	if err := fc.CreateSnapshot(ctx, &CreateSnapshotParams{
		SnapshotType: snapshotType,
		SnapshotPath: vmState,
		MemFilePath:  memFile,
	}); err != nil {
		// best-effort resume even if the snapshot failed
		rctx, rcancel := restoreCTX()
		_ = fc.Resume(rctx)
		rcancel()
		s.setFrozen(og, false)
		return fmt.Errorf("%s snapshot: %w", snapshotType, err)
	}
	if keepFrozen {
		return nil
	}
	if err := fc.Resume(ctx); err != nil {
		// frozen state stays set — failure paths must recover
		return fmt.Errorf("resume after %s snapshot: %w", snapshotType, err)
	}
	s.setFrozen(og, false)
	return nil
}

func ptrTime(t time.Time) *time.Time { return &t }
