package migration

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImportVM (cold path) adopts an export archive on this node: the disk stream
// is received into a fresh zvol, the VM record is restored (traffic counters
// included), the host network is rebuilt with the same MAC/IP and the VM is
// enqueued for a normal cold boot.
func (s *Service) ImportVM(archive io.Reader) (*ImportResult, error) {
	tmpDir, err := os.MkdirTemp("", "vmlet-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	manifest, diskPath, err := extractArchive(archive, tmpDir)
	if err != nil {
		return nil, err
	}
	if manifest.VM == nil || manifest.VM.ID == "" {
		return nil, fmt.Errorf("manifest has no VM record")
	}
	if manifest.DiskFormat != "zfs-send" {
		return nil, fmt.Errorf("unsupported disk format %q", manifest.DiskFormat)
	}
	if s.hooks.VMIDTaken(manifest.VM.ID) {
		return nil, ErrVMIDTaken
	}
	if s.zfs.VMExists(manifest.VM.ID) {
		return nil, ErrVMIDTaken
	}

	// integrity before touching the pool
	if manifest.DiskSHA256 != "" {
		if got := sha256File(diskPath); got != manifest.DiskSHA256 {
			return nil, fmt.Errorf("disk checksum mismatch: manifest %s, got %s", manifest.DiskSHA256, got)
		}
	}

	// disk → zvol
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := s.zfs.Receive(manifest.VM.ID, false, f); err != nil {
		return nil, fmt.Errorf("zfs receive: %w", err)
	}

	// network + record adoption + queue a normal boot
	vm := manifest.VM
	vm.Status = "creating"
	vm.NodeID = s.hooks.NodeDomain
	tapName, err := s.hooks.SetupTargetNetwork(vm)
	if err != nil {
		_ = s.zfs.DestroyVM(vm.ID)
		return nil, fmt.Errorf("network setup: %w", err)
	}
	if err := s.hooks.AdoptVM(vm, tapName, manifest.TrafficIngressBytes, manifest.TrafficEgressBytes); err != nil {
		_ = s.hooks.TeardownTargetNetwork(vm, tapName)
		_ = s.zfs.DestroyVM(vm.ID)
		return nil, fmt.Errorf("adopt record: %w", err)
	}

	logf(vm.ID, "cold import complete (from %s)", manifest.SourceNode)
	return &ImportResult{
		VMID:    vm.ID,
		TAPName: tapName,
	}, nil
}

// ImportResult reports the outcome of a cold import.
type ImportResult struct {
	VMID    string `json:"vm_id"`
	TAPName string `json:"tap_name"`
}

// extractArchive unpacks manifest.json + disk file from a tar.gz stream.
func extractArchive(r io.Reader, dir string) (*Manifest, string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var manifest *Manifest
	diskPath := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		switch hdr.Name {
		case "manifest.json":
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, "", fmt.Errorf("manifest parse: %w", err)
			}
			manifest = &m
		case "disk.zfs-send", "disk.raw":
			p := filepath.Join(dir, "disk")
			f, err := os.Create(p)
			if err != nil {
				return nil, "", err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return nil, "", err
			}
			f.Close()
			diskPath = p
		}
	}
	if manifest == nil {
		return nil, "", fmt.Errorf("archive has no manifest.json")
	}
	if diskPath == "" {
		return nil, "", fmt.Errorf("archive has no disk file")
	}
	return manifest, diskPath, nil
}

// sha256File hashes a file for import/export integrity checks.
func sha256File(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
