package migration

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// GatherNodeInfo collects the local node's compatibility fingerprint.
func GatherNodeInfo(nodeDomain string) (*NodeInfo, error) {
	info := &NodeInfo{NodeDomain: nodeDomain}

	ver, err := firecrackerVersion()
	if err != nil {
		return nil, fmt.Errorf("firecracker version: %w", err)
	}
	info.FirecrackerVer = ver

	if err := readCPUInfo(info); err != nil {
		return nil, err
	}

	kb, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err == nil {
		info.KernelVersion = strings.TrimSpace(string(kb))
	}

	if mem, err := readMemTotalMB(); err == nil {
		info.TotalMemMB = mem
	}
	if avail, err := readMemAvailableMB(); err == nil {
		info.MemAvailableMB = avail
	}

	// ZFS config (optional — file-backed nodes simply leave these empty)
	info.ZFSPool = os.Getenv("MICROVM_ZFS_POOL")
	if info.ZFSPool == "" {
		info.ZFSPool = "default"
	}
	info.ZFSDataset = os.Getenv("MICROVM_ZFS_DATASET")
	if info.ZFSDataset == "" {
		info.ZFSDataset = info.ZFSPool + "/vmlet-microvms"
	}
	if out, err := exec.Command("zfs", "list", "-Hpo", "avail", info.ZFSDataset).Output(); err == nil {
		if b, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil {
			info.ZFSAvailBytes = b
		}
	}

	info.NumVCPUs = runtime.NumCPU()
	info.MaxVCPUs = runtime.NumCPU()

	return info, nil
}

func firecrackerVersion() (string, error) {
	bin := os.Getenv("MICROVM_FIRECRACKER_BIN")
	if bin == "" {
		bin = "/usr/local/bin/firecracker"
		if _, err := os.Stat(bin); err != nil {
			bin = "firecracker"
		}
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "", err
	}
	// Output: "Firecracker v1.13.1\n<timestamp> ..."
	first := strings.SplitN(string(out), "\n", 2)[0]
	re := regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)
	m := re.FindString(first)
	if m == "" {
		return "", fmt.Errorf("cannot parse firecracker version from %q", first)
	}
	return m, nil
}

func readCPUInfo(info *NodeInfo) error {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "model name"):
			info.CPUModel = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		case strings.HasPrefix(line, "vendor_id"):
			info.CPUVendor = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		case strings.HasPrefix(line, "flags"):
			info.CPUFlags = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
		if info.CPUModel != "" && info.CPUVendor != "" && info.CPUFlags != "" {
			break
		}
	}
	if info.CPUModel == "" {
		return fmt.Errorf("could not read CPU model from /proc/cpuinfo")
	}
	return nil
}

func readMemAvailableMB() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return kb / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemAvailable not found")
}

// CheckCapacity verifies the target has room for this specific VM. Memory is
// checked against MemAvailable (not total) with a headroom margin, disk
// against the ZFS dataset's available space.
func CheckCapacity(dst *NodeInfo, vmMemMB uint32, vmStorageMB uint32) (memOK, diskOK bool, errs, warns []string) {
	memOK, diskOK = true, true

	if dst.MemAvailableMB > 0 {
		// Keep 1 GiB of host headroom for the node's own services and the
		// target-side restore buffers.
		need := int64(vmMemMB) + 1024
		if dst.MemAvailableMB < need {
			memOK = false
			errs = append(errs, fmt.Sprintf(
				"target has only %d MB memory available, VM needs %d MB + 1024 MB headroom",
				dst.MemAvailableMB, vmMemMB))
		} else if dst.MemAvailableMB < need*2 {
			warns = append(warns, fmt.Sprintf(
				"target memory headroom is thin: %d MB available for a %d MB VM",
				dst.MemAvailableMB, vmMemMB))
		}
	} else {
		warns = append(warns, "target memory availability unknown — capacity not enforced")
	}

	if dst.ZFSAvailBytes > 0 {
		need := int64(vmStorageMB) * 1024 * 1024
		if dst.ZFSAvailBytes < need {
			diskOK = false
			errs = append(errs, fmt.Sprintf(
				"target ZFS dataset has %d bytes available, VM disk needs %d bytes",
				dst.ZFSAvailBytes, need))
		}
	}
	return memOK, diskOK, errs, warns
}

func readMemTotalMB() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return kb / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found")
}

// CompareNodes validates that a VM running on src can be restored on dst.
// The Firecracker version must match exactly (snapshot format is
// version-bound) and the target CPU must be same-vendor with at least the
// source CPU's feature flags (a snapshot taken on a richer CPU cannot run on a
// poorer one).
func CompareNodes(src, dst *NodeInfo) *PreflightResult {
	r := &PreflightResult{Source: *src, Target: *dst}
	r.FCVersionMatch = src.FirecrackerVer == dst.FirecrackerVer
	r.CPUSameVendor = src.CPUVendor == dst.CPUVendor
	r.CPUSameModel = src.CPUModel == dst.CPUModel
	r.TargetHasZFS = dst.ZFSPool != "" && dst.ZFSDataset != "" && dst.ZFSAvailBytes > 0

	if !r.FCVersionMatch {
		r.Errors = append(r.Errors, fmt.Sprintf(
			"Firecracker version mismatch: source %s, target %s — snapshots cannot be restored across versions",
			src.FirecrackerVer, dst.FirecrackerVer))
	}
	if !r.CPUSameVendor {
		r.Errors = append(r.Errors, fmt.Sprintf(
			"CPU vendor mismatch: source %s (%s), target %s (%s) — restore requires a common CPU template",
			src.CPUVendor, src.CPUModel, dst.CPUVendor, dst.CPUModel))
	} else if !r.CPUSameModel {
		if !flagsSubset(src.CPUFlags, dst.CPUFlags) {
			r.Errors = append(r.Errors, fmt.Sprintf(
				"target CPU (%s) lacks features present on source CPU (%s)",
				dst.CPUModel, src.CPUModel))
		} else {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"CPU models differ (source %s → target %s) but flags are compatible; test a restore before fleet-wide use",
				src.CPUModel, dst.CPUModel))
		}
	}
	if !r.TargetHasZFS {
		r.Errors = append(r.Errors, "target node has no usable ZFS pool/dataset for disk receive")
	}
	r.TargetCapacityOK = dst.TotalMemMB > 0
	if dst.TotalMemMB <= 0 {
		r.Warnings = append(r.Warnings, "could not determine target memory capacity")
	}

	r.OK = len(r.Errors) == 0
	return r
}

// flagsSubset reports whether every flag in a (space-separated) is present in b.
func flagsSubset(a, b string) bool {
	bset := map[string]struct{}{}
	for _, f := range strings.Fields(b) {
		bset[f] = struct{}{}
	}
	for _, f := range strings.Fields(a) {
		if _, ok := bset[f]; !ok {
			return false
		}
	}
	return true
}
