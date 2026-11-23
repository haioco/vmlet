package migration

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

const (
	// restoreVerifyDelay lets a resumed guest prove it survived before the
	// source is told the restore succeeded.
	restoreVerifyDelay = 4 * time.Second
	// coldBootTimeout bounds the wait for a cold-booted VM to come alive.
	coldBootTimeout = 90 * time.Second
)

// receiver file layout (under the peer's per-VM work dir):
//
//	<migdir>/mem.snap      — guest memory (assembled from full + deltas)
//	<migdir>/vmstate.snap  — latest vm_state file
//	<migdir>/files.json    — map of shipped auxiliary files (config.json,
//	                         cloudinit.iso, data images) → relative paths
//
// The disk zvol itself lands in the node's ZFS dataset directly.

func (s *Service) incomingState(vmID string) (*incomingMigration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.incoming[vmID]
	return in, ok
}

// Prepare (target side) registers an incoming VM: validates the ID is free,
// creates the work dirs and stores the record + its source config.json so the
// drive topology can be rebuilt exactly at restore time.
func (s *Service) Prepare(vm *models.MicroVMInstance, sourceConfigJSON []byte) error {
	if vm == nil || vm.ID == "" {
		return fmt.Errorf("prepare: empty VM record")
	}
	s.mu.Lock()
	if _, exists := s.incoming[vm.ID]; exists {
		s.mu.Unlock()
		return ErrMigrationInFlight
	}
	s.mu.Unlock()
	if s.hooks.VMIDTaken(vm.ID) {
		return ErrVMIDTaken
	}
	// A zvol left behind by an aborted/failed migration would make a fresh
	// full receive fail; VMIDTaken is false, so nothing can be using it.
	if s.zfs.VMExists(vm.ID) {
		if err := s.zfs.DestroyVM(vm.ID); err != nil {
			return fmt.Errorf("clean stale zvol: %w", err)
		}
	}

	migDir := s.MigrationDir(vm.ID)
	if err := os.MkdirAll(migDir, 0755); err != nil {
		return err
	}
	if len(sourceConfigJSON) > 0 {
		workDir := s.hooks.VMWorkDir(vm.ID)
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workDir, "config.json"), sourceConfigJSON, 0644); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.incoming[vm.ID] = &incomingMigration{
		vm:    vm,
		phase: PhasePrepare,
	}
	s.mu.Unlock()
	logf(vm.ID, "prepared to receive migration from %s", vm.NodeID)
	return nil
}

// ReceiveDisk (target side) streams a zfs send payload into the VM's zvol.
// fromSnap empty → full receive; otherwise incremental with -F.
func (s *Service) ReceiveDisk(vmID, toSnap, fromSnap string, body io.Reader) (int64, error) {
	in, ok := s.incomingState(vmID)
	if !ok {
		return 0, ErrNoPending
	}

	nw := &countingReader{r: body}
	if err := s.zfs.Receive(vmID, fromSnap != "", nw); err != nil {
		return nw.n, fmt.Errorf("zfs receive on target: %w", err)
	}
	s.mu.Lock()
	in.lastDisk = toSnap
	if fromSnap != "" {
		in.phase = PhaseDiskPreCopy
	}
	s.mu.Unlock()
	logf(vmID, "received disk stream (to=%s, from=%s, %d bytes)", toSnap, fromSnap, nw.n)
	return nw.n, nil
}

// ReceiveMem (target side) writes a memory-file payload at offset — a full
// ship (offset 0) or a delta (offset == current size).
func (s *Service) ReceiveMem(vmID string, offset int64, body io.Reader) (int64, error) {
	if _, ok := s.incomingState(vmID); !ok {
		return 0, ErrNoPending
	}
	n, err := writeRange(s.memFilePath(vmID), offset, body)
	if err == nil {
		logf(vmID, "received mem range at offset %d (%d bytes)", offset, n)
	}
	return n, err
}

// ReceiveVMState (target side) stores the latest vm_state file.
func (s *Service) ReceiveVMState(vmID string, body io.Reader) (int64, error) {
	if _, ok := s.incomingState(vmID); !ok {
		return 0, ErrNoPending
	}
	n, err := writeRange(s.vmStatePath(vmID), 0, body)
	if err == nil {
		logf(vmID, "received vm_state (%d bytes)", n)
	}
	return n, err
}

// ReceiveFile (target side) stores an auxiliary per-VM file (config.json,
// cloudinit.iso, data image) relative to the VM's work dir.
func (s *Service) ReceiveFile(vmID, relPath string, body io.Reader) (int64, error) {
	if _, ok := s.incomingState(vmID); !ok {
		return 0, ErrNoPending
	}
	clean := filepath.Clean(relPath)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return 0, fmt.Errorf("invalid file path %q", relPath)
	}
	dst := filepath.Join(s.hooks.VMWorkDir(vmID), clean)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return 0, err
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, body)
	if err == nil {
		logf(vmID, "received file %s (%d bytes)", clean, n)
	}
	return n, err
}

// ReceiveMemExtents (target side) applies the data extents of a diff memory
// snapshot. Firecracker diff snapshots are sparse files whose dirty pages sit
// at their guest-physical offsets, so each frame is written positionally over
// the existing memory image (never appended).
func (s *Service) ReceiveMemExtents(vmID string, body io.Reader) (int64, error) {
	if _, ok := s.incomingState(vmID); !ok {
		return 0, ErrNoPending
	}
	n, extents, err := writeExtents(s.memFilePath(vmID), body)
	if err == nil {
		logf(vmID, "received %d memory extents (%d bytes)", extents, n)
	}
	return n, err
}

// writeExtents applies framed extent data to the memory file and returns the
// payload byte count plus the number of extents applied.
func writeExtents(path string, body io.Reader) (int64, int, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var hdr [16]byte
	var total int64
	extents := 0
	for {
		if _, err := io.ReadFull(body, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return total, extents, nil
			}
			return total, extents, fmt.Errorf("read extent header: %w", err)
		}
		offset := int64(binary.BigEndian.Uint64(hdr[0:8]))
		length := int64(binary.BigEndian.Uint64(hdr[8:16]))
		if length < 0 || offset < 0 {
			return total, extents, fmt.Errorf("invalid extent (offset=%d length=%d)", offset, length)
		}
		if length == 0 {
			extents++
			continue
		}
		if _, err := io.Copy(io.NewOffsetWriter(f, offset), io.LimitReader(body, length)); err != nil {
			return total, extents, fmt.Errorf("write extent at %d: %w", offset, err)
		}
		total += length
		extents++
	}
}

// Restore (target side) performs the cutover boot. Two modes:
//
//   - live (ColdBoot=false): host network, bare firecracker, snapshot load
//     with resume. After a short settle window the process is re-checked; if
//     it died (e.g. cross-CPU-generation vCPU state triple-fault), the VM is
//     cold-booted from the migrated disk instead ("cold_boot_fallback").
//   - cold (ColdBoot=true): no memory state is loaded at all; the VM is
//     adopted and booted fresh from the migrated disk ("cold").
func (s *Service) Restore(vmID string, payload *CutoverPayload) error {
	in, ok := s.incomingState(vmID)
	if !ok {
		return ErrNoPending
	}
	s.mu.Lock()
	restored := in.restored
	s.mu.Unlock()
	if restored {
		return ErrAlreadyRestored
	}
	cold := payload != nil && payload.ColdBoot
	if !cold {
		if _, err := os.Stat(s.memFilePath(vmID)); err != nil {
			return fmt.Errorf("memory snapshot missing on target: %w", err)
		}
		if _, err := os.Stat(s.vmStatePath(vmID)); err != nil {
			return fmt.Errorf("vm_state snapshot missing on target: %w", err)
		}
	}
	if !s.zfs.VMExists(vmID) {
		return fmt.Errorf("zvol %s missing on target", s.zfs.VM(vmID))
	}

	if payload != nil {
		in.ingress = payload.TrafficIngressBytes
		in.egress = payload.TrafficEgressBytes
	}
	s.setIncomingPhase(vmID, PhaseCutover)

	// 1. Host network: same MAC/IP, new TAP on this node.
	tapName, err := s.hooks.SetupTargetNetwork(in.vm)
	if err != nil {
		return fmt.Errorf("target network setup: %w", err)
	}
	s.mu.Lock()
	in.tapName = tapName
	s.mu.Unlock()

	if cold {
		return s.restoreCold(vmID, in, tapName)
	}

	// 2. Bare firecracker with api-sock only.
	socketPath := s.hooks.SocketPath(vmID)
	if err := s.hooks.StartRestoreProcess(vmID, socketPath); err != nil {
		s.teardownTarget(in, tapName)
		return fmt.Errorf("start restore process: %w", err)
	}

	// 3. Snapshot load (v1.13.1 schema): drives + vsock UDS paths are embedded
	// in the vmstate and re-opened verbatim — both exist on this node (zvol
	// received via ZFS, vsock UDS stubbed below, aux files shipped). The TAP
	// is renamed to the one created here via network_overrides.
	if in.vm.Vsock != nil && in.vm.Vsock.Enabled {
		// The vmstate references the source's absolute UDS path under the
		// same per-VM work dir layout — pre-create it so re-open succeeds.
		uds := filepath.Join(s.hooks.VMWorkDir(vmID), "vsock.sock")
		if _, err := os.Stat(uds); err != nil {
			if err := os.MkdirAll(filepath.Dir(uds), 0755); err == nil {
				_ = os.Remove(uds)
				if l, lerr := net.Listen("unix", uds); lerr == nil {
					l.Close()
				}
			}
		}
	}

	var overrides []NetworkOverride
	if tapName != "" {
		overrides = append(overrides, NetworkOverride{IfaceID: "eth0", HostDevName: tapName})
	}

	fc := NewSnapshotClient(socketPath)
	ctx, cancel := restoreCTX()
	defer cancel()
	params := &LoadSnapshotParams{
		SnapshotPath:     s.vmStatePath(vmID),
		ResumeVM:         true,
		NetworkOverrides: overrides,
	}
	params.MemBackend.BackendPath = s.memFilePath(vmID)
	params.MemBackend.BackendType = "File"
	if err := fc.LoadSnapshot(ctx, params); err != nil {
		_ = s.hooks.StopRestoreProcess(vmID)
		_ = s.hooks.TeardownTargetNetwork(in.vm, tapName)
		return fmt.Errorf("snapshot load: %w", err)
	}

	// 4. Adopt: record, counters, config regeneration.
	if err := s.hooks.AdoptVM(in.vm, tapName, in.ingress, in.egress); err != nil {
		s.teardownTarget(in, tapName)
		return fmt.Errorf("adopt VM record: %w", err)
	}

	// 5. Verify the resumed guest actually survived. A triple-faulted guest
	// exits firecracker right after resume, so a single successful load call
	// is not proof of life — fall back to cold booting the migrated disk.
	time.Sleep(restoreVerifyDelay)
	if !s.hooks.VerifyVMAlive(vmID) {
		logf(vmID, "snapshot restore did not survive — falling back to cold boot")
		_ = s.hooks.StopRestoreProcess(vmID)
		if err := s.hooks.ColdBoot(in.vm); err != nil {
			return fmt.Errorf("cold boot fallback: %w", err)
		}
		if !s.waitAlive(vmID, coldBootTimeout) {
			return fmt.Errorf("VM not alive after cold boot fallback")
		}
		s.mu.Lock()
		in.restored = true
		in.phase = PhaseRunning
		in.mode = ModeColdBootFallback
		s.mu.Unlock()
		logf(vmID, "restored via cold boot fallback on target node")
		return nil
	}

	s.mu.Lock()
	in.restored = true
	in.phase = PhaseRunning
	in.mode = ModeLive
	s.mu.Unlock()
	logf(vmID, "restored and running on target node (live)")
	return nil
}

// restoreCold adopts the VM and boots it fresh from the received disk.
func (s *Service) restoreCold(vmID string, in *incomingMigration, tapName string) error {
	if err := s.hooks.AdoptVM(in.vm, tapName, in.ingress, in.egress); err != nil {
		s.teardownTarget(in, tapName)
		return fmt.Errorf("adopt VM record: %w", err)
	}
	if err := s.hooks.ColdBoot(in.vm); err != nil {
		return fmt.Errorf("cold boot: %w", err)
	}
	if !s.waitAlive(vmID, coldBootTimeout) {
		return fmt.Errorf("VM not alive after cold boot")
	}
	s.mu.Lock()
	in.restored = true
	in.phase = PhaseRunning
	in.mode = ModeCold
	s.mu.Unlock()
	logf(vmID, "cold-booted and running on target node")
	return nil
}

// waitAlive polls VerifyVMAlive until the timeout expires.
func (s *Service) waitAlive(vmID string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if s.hooks.VerifyVMAlive(vmID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

// teardownTarget undoes the restore process and host network on failure paths.
func (s *Service) teardownTarget(in *incomingMigration, tapName string) {
	_ = s.hooks.StopRestoreProcess(in.vm.ID)
	_ = s.hooks.TeardownTargetNetwork(in.vm, tapName)
}

// setIncomingPhase updates the reported phase under the service lock.
func (s *Service) setIncomingPhase(vmID string, phase Phase) {
	s.mu.Lock()
	if in, ok := s.incoming[vmID]; ok {
		in.phase = phase
	}
	s.mu.Unlock()
}

// ConfirmIncoming is called by the source after it verified the VM runs here.
func (s *Service) ConfirmIncoming(vmID string) error {
	in, ok := s.incomingState(vmID)
	if !ok || !in.restored {
		return ErrNoPending
	}
	if !s.hooks.VerifyVMAlive(vmID) {
		return fmt.Errorf("refusing to confirm: VM %s is not running", vmID)
	}
	in.confirmed = true
	in.phase = PhaseDone
	s.mu.Lock()
	delete(s.incoming, vmID)
	s.mu.Unlock()
	// The VM is now a first-class local VM — drop migration bookkeeping
	// snapshots only if the source confirmed.
	_ = s.zfs.DeleteSnapshots(vmID)
	logf(vmID, "migration confirmed — VM is now local")
	return nil
}

// AbortIncoming rolls back everything the target prepared for this VM.
func (s *Service) AbortIncoming(vmID string) error {
	in, ok := s.incomingState(vmID)
	if !ok {
		return ErrNoPending
	}
	if in.restored {
		// The VM is running HERE; aborting means tearing it down.
		_ = s.hooks.StopRestoreProcess(vmID)
		_ = s.hooks.DropAdopted(in.vm)
	}
	if in.tapName != "" {
		_ = s.hooks.TeardownTargetNetwork(in.vm, in.tapName)
	}
	_ = s.zfs.DestroyVM(vmID)
	// The whole per-VM work dir was created by this migration (Prepare
	// refuses to run when the ID is taken), so removing it is safe and
	// prevents a stale config.json from blocking future migrations.
	_ = os.RemoveAll(s.hooks.VMWorkDir(vmID))
	_ = os.RemoveAll(s.MigrationDir(vmID))
	s.mu.Lock()
	delete(s.incoming, vmID)
	s.mu.Unlock()
	logf(vmID, "incoming migration aborted — target cleaned up")
	return nil
}

func (s *Service) memFilePath(vmID string) string {
	return filepath.Join(s.MigrationDir(vmID), "mem.snap")
}

func (s *Service) vmStatePath(vmID string) string {
	return filepath.Join(s.MigrationDir(vmID), "vmstate.snap")
}

// writeRange writes r into the file at offset, truncating when offset==0
// (full ship) and appending when offset==size (delta ship).
func writeRange(path string, offset int64, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, err
	}
	flag := os.O_WRONLY | os.O_CREATE
	if offset == 0 {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.Copy(f, r)
}

// countingReader counts bytes as they stream through.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
