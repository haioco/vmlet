package migration

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExportDir is where cold exports land (MICROVM_EXPORT_DIR, default under the
// node's shared work dir). Layout per export:
//
//	<ExportDir>/<vmID>-<ts>/manifest.json   — Manifest (full VM record)
//	<ExportDir>/<vmID>-<ts>/disk.zfs-send   — full zfs send stream of the disk
type exportDir = string

func (s *Service) ExportDir() string {
	d := os.Getenv("MICROVM_EXPORT_DIR")
	if d == "" {
		d = filepath.Join(filepath.Dir(s.hooks.VMWorkDir("_shared")), "exports")
	}
	return d
}

// ExportVM (cold path) packages a STOPPED VM: one atomic ZFS snapshot of the
// disk, streamed to a file, plus a manifest carrying the full VM record (and
// billing counters) so any compatible node can adopt it.
func (s *Service) ExportVM(vmID string) (string, error) {
	vm, ok := s.hooks.GetVM(vmID)
	if !ok {
		return "", ErrVMNotFound
	}
	if s.hooks.IsVMProcessRunning(vmID) {
		return "", fmt.Errorf("VM is running — stop it before cold export (or use live migration)")
	}
	if !s.zfs.VMExists(vmID) {
		return "", ErrNoZvol
	}

	snap := migSnapName("export", int(time.Now().Unix()))
	if err := s.zfs.Snapshot(vmID, snap); err != nil {
		return "", err
	}
	defer func() { _ = execZfs("destroy", s.zfs.VM(vmID)+"@"+snap) }()

	stamp := time.Now().UTC().Format("20060102-150405")
	dir := filepath.Join(s.ExportDir(), fmt.Sprintf("%s-%s", vmID, stamp))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Disk stream → file (size becomes known for the download tar).
	diskPath := filepath.Join(dir, "disk.zfs-send")
	diskFile, err := os.Create(diskPath)
	if err != nil {
		return "", err
	}
	if err := s.zfs.SendFull(vmID, snap, diskFile); err != nil {
		diskFile.Close()
		return "", err
	}
	diskFile.Close()

	fi, err := os.Stat(diskPath)
	if err != nil {
		return "", err
	}

	ingress, egress := s.hooks.TrafficSnapshot(vmID)
	manifest := &Manifest{
		Version:             ManifestVersion,
		CreatedAt:           now(),
		SourceNode:          s.hooks.NodeDomain,
		VM:                  vm,
		DiskFile:            "disk.zfs-send",
		DiskSHA256:          sha256File(diskPath),
		DiskFormat:          "zfs-send",
		DiskBytes:           fi.Size(),
		TrafficIngressBytes: ingress,
		TrafficEgressBytes:  egress,
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0644); err != nil {
		return "", err
	}

	logf(vmID, "cold export written to %s (%d bytes disk)", dir, fi.Size())
	return dir, nil
}

// ListExports returns all export directories on this node.
func (s *Service) ListExports() ([]string, error) {
	entries, err := os.ReadDir(s.ExportDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// DeleteExport removes an export directory by name (basename only).
func (s *Service) DeleteExport(name string) error {
	// Validate FIRST — never touch ExportDir with a hostile name.
	if name == "" || name == "." || name == ".." ||
		filepath.IsAbs(name) || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("invalid export name")
	}
	return os.RemoveAll(filepath.Join(s.ExportDir(), name))
}

// ServeExportTar streams an export as a single tar.gz (sizes are known from
// the on-disk files, so plain tar works).
func (s *Service) ServeExportTar(name string, w io.Writer) error {
	dir := filepath.Join(s.ExportDir(), filepath.Clean(name))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("export %q not found", name)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, fname := range []string{"manifest.json", "disk.zfs-send"} {
		p := filepath.Join(dir, fname)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     fname,
			Mode:     0644,
			Size:     fi.Size(),
			Typeflag: tar.TypeReg,
		}); err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
