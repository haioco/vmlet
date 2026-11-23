package migration

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// ZFSOps wraps the zfs(8) commands used by migration. All operations are
// scoped to the vmlet-microvms dataset of the local pool.
type ZFSOps struct {
	Dataset string // e.g. "default/vmlet-microvms"
}

// NewZFSOps builds a ZFSOps from the node's env config.
func NewZFSOps() *ZFSOps {
	pool := os.Getenv("MICROVM_ZFS_POOL")
	if pool == "" {
		pool = "default"
	}
	ds := os.Getenv("MICROVM_ZFS_DATASET")
	if ds == "" {
		ds = pool + "/vmlet-microvms"
	}
	return &ZFSOps{Dataset: ds}
}

// VM returns the dataset name of a VM's zvol.
func (z *ZFSOps) VM(vmID string) string {
	return z.Dataset + "/" + vmID
}

// VMExists reports whether the VM's zvol exists locally.
func (z *ZFSOps) VMExists(vmID string) bool {
	return exec.Command("zfs", "list", z.VM(vmID)).Run() == nil
}

// Snapshot creates an atomic snapshot of the VM zvol (crash-consistent even
// while the VM runs).
func (z *ZFSOps) Snapshot(vmID, name string) error {
	out, err := exec.Command("zfs", "snapshot", z.VM(vmID)+"@"+name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs snapshot: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HasSnapshot reports whether a named snapshot exists for the VM.
func (z *ZFSOps) HasSnapshot(vmID, name string) bool {
	out, err := exec.Command("zfs", "list", "-t", "snapshot", "-Hpo", "name", z.Dataset).Output()
	if err != nil {
		return false
	}
	want := z.VM(vmID) + "@" + name
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// SendFull writes a full send stream of the given snapshot to w.
func (z *ZFSOps) SendFull(vmID, snap string, w io.Writer) error {
	args := sendArgs()
	args = append(args, z.VM(vmID)+"@"+snap)
	cmd := exec.Command("zfs", args...)
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs send (full %s): %v (%s)", snap, err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// SendIncremental writes an incremental stream (fromSnap → toSnap) to w.
func (z *ZFSOps) SendIncremental(vmID, fromSnap, toSnap string, w io.Writer) error {
	args := append(sendArgs(), "-i", z.VM(vmID)+"@"+fromSnap, z.VM(vmID)+"@"+toSnap)
	cmd := exec.Command("zfs", args...)
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs send (-i %s %s): %v (%s)", fromSnap, toSnap, err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// sendArgs builds the common `zfs send` flags. Compressed send ships the
// pool's already-compressed blocks instead of re-inflating them, which
// typically cuts migration time several-fold on database workloads. Enable
// with MICROVM_MIGRATION_SEND_COMPRESSED=1 (receive side handles it natively).
func sendArgs() []string {
	args := []string{"send"}
	if v := os.Getenv("MICROVM_MIGRATION_SEND_COMPRESSED"); v == "1" || strings.EqualFold(v, "true") {
		args = append(args, "-c")
	}
	return args
}

// Receive streams a zfs send stream into a NEW zvol named vmID on this node.
// receiveTarget controls zfs receive semantics:
//
//	false → plain `zfs receive <ds>` (expects a full stream first)
//	true  → `zfs receive -F <ds>` (force-rollback receive for incrementals)
func (z *ZFSOps) Receive(vmID string, force bool, r io.Reader) error {
	args := []string{"receive"}
	if force {
		args = append(args, "-F")
	}
	args = append(args, z.VM(vmID))
	cmd := exec.Command("zfs", args...)
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs receive %s: %v (%s)", z.VM(vmID), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReceiveToStream is like Receive but returns the running command so the HTTP
// handler can stream the request body straight into zfs receive.
func (z *ZFSOps) ReceiveToStream(vmID string, force bool) (*exec.Cmd, io.WriteCloser, error) {
	args := []string{"receive"}
	if force {
		args = append(args, "-F")
	}
	args = append(args, z.VM(vmID))
	cmd := exec.Command("zfs", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, stdin, nil
}

// EstimatedStreamBytes returns the approximated size of a full send stream
// for the snapshot, used for progress reporting.
func (z *ZFSOps) EstimatedStreamBytes(vmID, snap string) int64 {
	out, err := exec.Command("zfs", "list", "-Hpo", "used", z.VM(vmID)+"@"+snap).Output()
	if err != nil {
		return 0
	}
	var n int64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// EstimatedIncrementalBytes returns the space written between the previous
// snapshot and toSnap — i.e. the real size of the incremental send stream.
// (`used` of a snapshot is the total space it references and is useless here.)
func (z *ZFSOps) EstimatedIncrementalBytes(vmID, fromSnap, toSnap string) int64 {
	out, err := exec.Command("zfs", "get", "-Hpo", "value", "written", z.VM(vmID)+"@"+toSnap).Output()
	if err != nil {
		return 0
	}
	var n int64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// DeleteSnapshots removes migration bookkeeping snapshots (mig-*) but never
// touches user-visible rollback snapshots (snap-*).
func (z *ZFSOps) DeleteSnapshots(vmID string) error {
	out, err := exec.Command("zfs", "list", "-t", "snapshot", "-Hpo", "name", z.VM(vmID)).Output()
	if err != nil {
		// dataset may already be gone — nothing to clean
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		short := name[strings.LastIndexByte(name, '@')+1:]
		if strings.HasPrefix(short, "mig-") {
			_ = exec.Command("zfs", "destroy", name).Run()
		}
	}
	return nil
}

// DestroyVM destroys the VM's zvol entirely (source retirement after a
// confirmed migration).
func (z *ZFSOps) DestroyVM(vmID string) error {
	out, err := exec.Command("zfs", "destroy", "-r", z.VM(vmID)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs destroy %s: %v (%s)", z.VM(vmID), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// execZfs runs a zfs subcommand ignoring output errors (best-effort paths).
func execZfs(args ...string) error {
	return exec.Command("zfs", args...).Run()
}
