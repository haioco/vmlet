package adopt

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Defaults for ScanOptions on a haioflash host. Every one of them can be
// overridden; the scan degrades gracefully (with warnings) when a source is
// missing rather than failing the whole run.
const (
	// DefaultWorkDir is where the previous platform keeps per-VM firecracker
	// configs: <WorkDir>/<vm-id>/config.json.
	DefaultWorkDir = "/opt/haioflash/work/microvms"

	// DefaultDatasetRoot is the ZFS dataset holding the VM root disks
	// (/dev/zvol/default/haioflash-microvms/vm-<id>). A filesystem path
	// ("/opt/haioflash-microvms") is accepted too and resolved by mountpoint,
	// then by matching the trailing dataset name.
	DefaultDatasetRoot = "default/haioflash-microvms"

	// DefaultUnitDir is where per-VM static systemd units live
	// (firecracker-vm-<id>.service). Template instances
	// (firecracker-vm@<id>.service) are queried through systemctl instead.
	DefaultUnitDir = "/etc/systemd/system"

	// DefaultBridge is the bridge the adopted VMs are already attached to.
	DefaultBridge = "br0"

	// vmIDPrefix keeps the scan focused on VM ids: anything else found under
	// the dataset root or the unit directory is not a VM.
	vmIDPrefix = "vm-"
)

// GuestInterface is one network interface of a discovered VM, copied verbatim
// from the VM's firecracker config.json.
type GuestInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// Discovery describes one VM found on disk. It is a read-only snapshot: nothing
// in it implies vmlet has changed, or will change, anything about the VM.
type Discovery struct {
	ID         string `json:"id"`
	WorkDir    string `json:"work_dir"`
	ConfigPath string `json:"config_path"`

	// ConfigReadable is false when config.json is missing or unparseable. The
	// VM is still reported, with a warning, so an operator can see it.
	ConfigReadable bool   `json:"config_readable"`
	RootDiskPath   string `json:"root_disk_path"`
	RootDiskExists bool   `json:"root_disk_exists"`
	RootDiskBytes  uint64 `json:"root_disk_bytes,omitempty"`

	KernelPath string `json:"kernel_path,omitempty"`
	BootArgs   string `json:"boot_args,omitempty"`
	VCPUs      uint32 `json:"vcpus"`
	MemoryMB   uint32 `json:"memory_mb"`

	Interfaces []GuestInterface `json:"interfaces"`

	// IPAddress is best effort: the previous platform boots guests with cloud-init
	// and no ip= argument, so the guest address is not in config.json. With IP
	// resolution left on (ScanOptions.SkipIPResolution false) it is read from the
	// host's neighbour table by guest MAC, so a VM that has never sent a packet
	// stays blank and is reported as such.
	IPAddress      string `json:"ip_address,omitempty"`
	IPAddressKnown bool   `json:"ip_address_known"`

	// FirecrackerBin is the binary the VM's systemd unit pins. Two different
	// binaries are in play on an adopted host; a blanket upgrade breaks the VMs
	// pinned to the other one, so the pin is recorded, not changed.
	//
	// It is the unit's pin, not the binary of the live process: a VM started by
	// hand, or by an older unit file, can run a different binary than its unit
	// now says. On node62, 9 of the 42 running VMs execute
	// /usr/local/bin/firecracker while their units pin
	// /opt/haioflash/bin/firecracker.
	FirecrackerBin string `json:"firecracker_bin,omitempty"`

	UnitName      string `json:"unit_name,omitempty"`
	UnitFile      string `json:"unit_file,omitempty"`
	UnitPresent   bool   `json:"unit_present"`
	UnitEnabled   bool   `json:"unit_enabled"`
	UnitFileState string `json:"unit_file_state,omitempty"`
	UnitActive    bool   `json:"unit_active"`

	// Running is what the host says today (systemctl is-active). vmlet does
	// not start or stop anything to find this out.
	Running bool `json:"running"`

	Warnings []string `json:"warnings,omitempty"`
}

// VMConfig is the subset of firecracker's config.json the adopter needs.
type VMConfig struct {
	BootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	} `json:"boot-source"`
	Drives []struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	} `json:"drives"`
	MachineConfig struct {
		VcpuCount  uint32 `json:"vcpu_count"`
		MemSizeMib uint32 `json:"mem_size_mib"`
	} `json:"machine-config"`
	NetworkInterfaces []GuestInterface `json:"network-interfaces"`
}

// RootDrive returns the VM's root device from its config, falling back to the
// first drive. An empty drives list is not an error here: the caller records a
// warning and continues.
func (c *VMConfig) RootDrive() (pathOnHost string, ok bool) {
	if len(c.Drives) == 0 {
		return "", false
	}
	for _, d := range c.Drives {
		if d.IsRootDevice {
			return d.PathOnHost, true
		}
	}
	return c.Drives[0].PathOnHost, true
}

// Healthy reports whether the discovery is complete enough for vmlet to take
// the VM over: a parseable config.json, a root disk that exists, and a systemd
// unit vmlet can query. A VM failing any of those is planned as skip-unhealthy.
func (d *Discovery) Healthy() bool {
	return d.ConfigReadable && d.RootDiskExists && d.UnitPresent
}

// PrimaryInterface returns the first network interface, or a zero value.
func (d *Discovery) PrimaryInterface() GuestInterface {
	if len(d.Interfaces) == 0 {
		return GuestInterface{}
	}
	return d.Interfaces[0]
}

// ScanOptions configures a read-only scan of an adopted host.
type ScanOptions struct {
	// WorkDir holds the per-VM config.json files (default DefaultWorkDir).
	WorkDir string

	// DatasetRoot is the ZFS dataset holding the root disks. Accepts a dataset
	// name ("default/haioflash-microvms") or a filesystem path
	// ("/opt/haioflash-microvms"), resolved by mountpoint / name.
	DatasetRoot string

	// UnitDir holds static per-VM unit files (default DefaultUnitDir).
	UnitDir string

	// SkipIPResolution disables the best-effort guest IP lookup (ip neigh show,
	// read-only). Left false, the IP is resolved from the host neighbour table
	// by guest MAC; the previous platform boots guests with cloud-init and no
	// ip= argument, so config.json alone does not carry the address.
	SkipIPResolution bool

	// CommandTimeout bounds each read-only command (default DefaultCommandTimeout).
	CommandTimeout time.Duration

	// Runner injects the command runner — tests substitute a recorder.
	Runner CommandRunner
}

// DefaultScanOptions returns the options for a haioflash host.
func DefaultScanOptions() ScanOptions {
	return ScanOptions{
		WorkDir:          DefaultWorkDir,
		DatasetRoot:      DefaultDatasetRoot,
		UnitDir:          DefaultUnitDir,
		SkipIPResolution: false,
		CommandTimeout:   DefaultCommandTimeout,
	}
}

// withDefaults fills in empty fields.
func (o ScanOptions) withDefaults() ScanOptions {
	d := DefaultScanOptions()
	if o.WorkDir == "" {
		o.WorkDir = d.WorkDir
	}
	if o.DatasetRoot == "" {
		o.DatasetRoot = d.DatasetRoot
	}
	if o.UnitDir == "" {
		o.UnitDir = d.UnitDir
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = d.CommandTimeout
	}
	return o
}

// runner returns the configured runner or the production one.
func (o ScanOptions) runner() CommandRunner {
	if o.Runner != nil {
		return o.Runner
	}
	return execRunner{timeout: o.CommandTimeout}
}

// ParseConfig parses a firecracker config.json. Malformed JSON is an error;
// valid JSON with missing sections is accepted (the caller warns).
func ParseConfig(data []byte) (*VMConfig, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("config.json is empty")
	}
	var cfg VMConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config.json: %w", err)
	}
	return &cfg, nil
}

// ScanReport carries the scan-level information that does not belong to any
// single VM: what the scan could and could not read.
type ScanReport struct {
	// Warnings are problems with the sources themselves (zfs unavailable, work
	// dir missing, unit dir unreadable), not with a VM.
	Warnings []string `json:"warnings,omitempty"`
	// ZFSDatasets is the number of VM datasets the ZFS list produced.
	ZFSDatasets int `json:"zfs_datasets"`
	// WorkDirs is the number of per-VM directories found under WorkDir.
	WorkDirs int `json:"work_dirs"`
	// UnitFiles is the number of static unit files found under UnitDir.
	UnitFiles int `json:"unit_files"`
	// DatasetRoot is the dataset name the scan resolved to (empty if ZFS was
	// not usable).
	DatasetRoot string `json:"dataset_root,omitempty"`
}

// Scan inventories the VMs that exist on an adopted host. See ScanReported for
// the same scan with its report.
func Scan(opts ScanOptions) ([]Discovery, error) {
	discoveries, _, err := ScanReported(opts)
	return discoveries, err
}

// ScanReported inventories the VMs that exist on an adopted host.
//
// Sources, in order of authority, are merged (a VM found by any of them is
// reported):
//
//  1. the ZFS dataset list under DatasetRoot (zfs list -H -o name,volsize -r),
//  2. the per-VM directories under WorkDir,
//  3. the static unit files under UnitDir (firecracker-vm-<id>.service).
//
// Per VM it then reads config.json, stats the root disk, resolves the VM's
// systemd unit (static file, or template instance queried through systemctl)
// and, optionally, the guest IP from the host neighbour table.
//
// Every shell command goes through the read-only CommandRunner; nothing is
// started, stopped, restarted or reconfigured. A broken config, a missing disk
// or an absent unit becomes a warning on that discovery, never an abort, and a
// source that cannot be read becomes a warning in the report rather than a
// failed scan.
func ScanReported(opts ScanOptions) ([]Discovery, ScanReport, error) {
	o := opts.withDefaults()
	runner := o.runner()

	report := ScanReport{}
	// Resolve the dataset once: the two zfs listings below use the same name,
	// and a path-shaped DatasetRoot would otherwise be resolved three times.
	dataset := o.resolveDatasetRoot(runner)
	report.DatasetRoot = dataset

	ids, counts, warnings, err := o.candidateVMIDs(runner, dataset)
	if err != nil {
		return nil, report, err
	}
	report.Warnings = warnings
	report.ZFSDatasets, report.WorkDirs, report.UnitFiles = counts.zfs, counts.workDir, counts.unitFiles

	sizes := o.datasetSizes(runner, dataset)

	var neigh map[string]string
	if !o.SkipIPResolution {
		neigh = neighbourTable(runner)
	}

	units := o.resolveUnits(runner, ids)

	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	discoveries := make([]Discovery, 0, len(sorted))
	for _, id := range sorted {
		d := o.discover(id, units[id], sizes[id], neigh, dataset)
		d.Warnings = append(warningsFor(id, d), d.Warnings...)
		discoveries = append(discoveries, d)
	}
	return discoveries, report, nil
}

// warningsFor appends the per-scan warnings that apply to this VM.
func warningsFor(id string, d Discovery) []string {
	var out []string
	// vmlet detects liveness through the template unit name (see
	// firecracker.Manager.IsVMProcessRunning); a static unit will look "stopped"
	// to vmlet even while the VM runs. UnitFile is set only when a static unit
	// file was found on disk.
	if d.UnitFile != "" {
		out = append(out, fmt.Sprintf("static unit %s: vmlet polls firecracker-vm@%s.service for liveness",
			d.UnitName, id))
	}
	if len(d.Interfaces) > 1 {
		out = append(out, fmt.Sprintf("%d network interfaces in config.json; only the first is recorded", len(d.Interfaces)))
	}
	return out
}

// discover builds one discovery for a VM id. dataset is the resolved ZFS
// dataset name, used only to guess a root disk path when config.json has none.
func (o ScanOptions) discover(id string, unit *unitInfo, sizeBytes uint64, neigh map[string]string, dataset string) Discovery {
	d := Discovery{
		ID:            id,
		WorkDir:       filepath.Join(o.WorkDir, id),
		RootDiskBytes: sizeBytes,
	}
	d.ConfigPath = filepath.Join(d.WorkDir, "config.json")

	if data, err := os.ReadFile(d.ConfigPath); err != nil {
		d.Warnings = append(d.Warnings, fmt.Sprintf("config.json unreadable: %v", err))
	} else if cfg, err := ParseConfig(data); err != nil {
		d.Warnings = append(d.Warnings, err.Error())
	} else {
		d.ConfigReadable = true
		d.KernelPath = cfg.BootSource.KernelImagePath
		d.BootArgs = cfg.BootSource.BootArgs
		d.VCPUs = cfg.MachineConfig.VcpuCount
		d.MemoryMB = cfg.MachineConfig.MemSizeMib
		d.Interfaces = cfg.NetworkInterfaces
		if root, ok := cfg.RootDrive(); ok {
			d.RootDiskPath = root
		}
	}

	if d.RootDiskPath == "" {
		// No drive named in the config (or no config at all): the conventional
		// zvol path is the best guess, and a guess is reported as such.
		d.RootDiskPath = filepath.Join("/dev/zvol", dataset, id)
		if d.ConfigReadable {
			d.Warnings = append(d.Warnings, "no drives in config.json; root disk path inferred from the dataset")
		}
	}
	if fi, err := os.Stat(d.RootDiskPath); err != nil {
		d.Warnings = append(d.Warnings, fmt.Sprintf("root disk missing: %s", d.RootDiskPath))
	} else {
		d.RootDiskExists = true
		if d.RootDiskBytes == 0 && fi.Size() > 0 {
			d.RootDiskBytes = uint64(fi.Size()) //nolint:gosec // stat size is non-negative
		}
		if fi.Mode()&os.ModeDevice == 0 {
			// A flat rootfs file instead of a zvol/partition: legal on hosts that
			// do not run ZFS, so it is recorded as a warning, not a defect.
			d.Warnings = append(d.Warnings, fmt.Sprintf("root disk is not a block device: %s", d.RootDiskPath))
		}
	}
	if d.ConfigReadable && d.VCPUs == 0 {
		d.Warnings = append(d.Warnings, "vcpu_count is 0 in config.json")
	}
	if d.ConfigReadable && d.MemoryMB == 0 {
		d.Warnings = append(d.Warnings, "mem_size_mib is 0 in config.json")
	}
	if d.ConfigReadable && d.KernelPath == "" {
		d.Warnings = append(d.Warnings, "kernel_image_path is empty in config.json")
	}
	if d.ConfigReadable && len(d.Interfaces) == 0 {
		d.Warnings = append(d.Warnings, "no network-interfaces in config.json")
	}

	// Guest IP: best effort, from the host neighbour table.
	if !o.SkipIPResolution {
		if ip, ok := lookupNeighbour(neigh, d.PrimaryInterface().GuestMAC); ok {
			d.IPAddress, d.IPAddressKnown = ip, true
		} else if len(d.Interfaces) > 0 {
			d.Warnings = append(d.Warnings, fmt.Sprintf("guest IP unknown: no neighbour entry for MAC %s", d.PrimaryInterface().GuestMAC))
		}
	}

	// Systemd unit: static file wins, template instance otherwise.
	o.applyUnit(&d, unit)
	return d
}

// sourceCounts records how many ids each enumeration source produced, so a scan
// can show where its VMs came from (a host that lost its ZFS datasets still
// reports its work directories).
type sourceCounts struct {
	zfs       int
	workDir   int
	unitFiles int
}

// candidateVMIDs returns the set of VM ids found on disk, the per-source counts
// and any warnings about the sources themselves. It never fails: a source that
// cannot be read is reported and the scan continues with the others.
func (o ScanOptions) candidateVMIDs(runner CommandRunner, dataset string) (map[string]struct{}, sourceCounts, []string, error) {
	ids := make(map[string]struct{})
	var warnings []string
	var counts sourceCounts

	// 1. ZFS dataset list (or the work dir, if ZFS is unavailable).
	names, dsWarnings := o.datasetNames(runner, dataset)
	warnings = append(warnings, dsWarnings...)
	counts.zfs = len(names)
	for _, n := range names {
		ids[n] = struct{}{}
	}

	// 2. Per-VM work directories.
	dirs, err := os.ReadDir(o.WorkDir)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("work dir unreadable: %v", err))
	} else {
		for _, e := range dirs {
			if e.IsDir() {
				ids[e.Name()] = struct{}{}
				counts.workDir++
			}
		}
	}

	// 3. Static unit files.
	unitIDs, unitWarnings := collectUnitFileIDs(o.UnitDir)
	warnings = append(warnings, unitWarnings...)
	counts.unitFiles = len(unitIDs)
	for _, id := range unitIDs {
		ids[id] = struct{}{}
	}

	if len(ids) == 0 {
		return nil, counts, warnings, fmt.Errorf("no VMs found (zfs dataset %q, work dir %q, unit dir %q)", o.DatasetRoot, o.WorkDir, o.UnitDir)
	}
	return ids, counts, warnings, nil
}

// datasetNames lists the VM ids in the ZFS dataset root. When ZFS is missing or
// the dataset does not exist it returns no ids and a warning; the work dir
// enumeration covers the gap. dataset is the already-resolved dataset name.
func (o ScanOptions) datasetNames(runner CommandRunner, dataset string) ([]string, []string) {
	if dataset == "" {
		return nil, []string{fmt.Sprintf("ZFS dataset root %q could not be resolved; relying on the work directory", o.DatasetRoot)}
	}
	out, err := runner.Run("zfs", "list", "-H", "-o", "name", "-r", dataset)
	if err != nil {
		// Fall back to the non-recursive listing: it yields the dataset itself,
		// which the leaf filter below drops, so a host without -r support simply
		// reports nothing here and relies on the work directory.
		out, err = runner.Run("zfs", "list", "-H", "-o", "name", dataset)
		if err != nil {
			return nil, []string{fmt.Sprintf("zfs list %s failed: %v", dataset, err)}
		}
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		leaf := name[strings.LastIndex(name, "/")+1:]
		if !strings.HasPrefix(leaf, vmIDPrefix) || strings.Contains(leaf, "@") {
			continue // snapshots and non-VM children are not VMs
		}
		ids = append(ids, leaf)
	}
	return ids, nil
}

// datasetSizes returns each VM's root disk size in bytes, from one batched
// zfs list. Missing or unparseable sizes are simply absent (0). dataset is the
// already-resolved dataset name.
func (o ScanOptions) datasetSizes(runner CommandRunner, dataset string) map[string]uint64 {
	sizes := make(map[string]uint64)
	if dataset == "" {
		return sizes
	}
	out, err := runner.Run("zfs", "list", "-H", "-o", "name,volsize", "-r", dataset)
	if err != nil {
		return sizes
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		leaf := name[strings.LastIndex(name, "/")+1:]
		if !strings.HasPrefix(leaf, vmIDPrefix) {
			continue
		}
		if n, ok := parseZFSHumanSize(strings.Join(fields[1:], "")); ok {
			sizes[leaf] = n
		}
	}
	return sizes
}

// resolveDatasetRoot turns the configured value into a usable dataset name.
// A value that already looks like a dataset name is used as-is; a filesystem
// path is resolved by mountpoint, then by the dataset whose final name element
// matches the path's base name (that is where /dev/zvol/<dataset>/vm-<id> comes
// from). An unresolvable value returns "" so the caller can fall back to the
// work directory.
func (o ScanOptions) resolveDatasetRoot(runner CommandRunner) string {
	root := strings.TrimSpace(o.DatasetRoot)
	if root == "" {
		return ""
	}
	if !strings.HasPrefix(root, "/") {
		return root
	}
	base := filepath.Base(root)
	out, err := runner.Run("zfs", "list", "-H", "-o", "name,mountpoint")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if fields[1] == root || filepath.Base(fields[0]) == base {
			return fields[0]
		}
	}
	return ""
}

// neighbourTable maps lowercased MAC → IP from `ip neigh show`. Unresolved
// (FAILED) entries carry no lladdr and are skipped.
func neighbourTable(runner CommandRunner) map[string]string {
	out, err := runner.Run("ip", "-o", "neigh", "show")
	if err != nil {
		return nil
	}
	macs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		ipStr := fields[0]
		if net.ParseIP(ipStr) == nil || strings.HasPrefix(ipStr, "fe80:") {
			continue
		}
		idx := -1
		for i, f := range fields {
			if f == "lladdr" {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(fields) {
			continue
		}
		mac := strings.ToLower(fields[idx+1])
		if _, exists := macs[mac]; !exists {
			macs[mac] = ipStr
		}
	}
	return macs
}

// lookupNeighbour resolves a guest MAC to an IP.
func lookupNeighbour(table map[string]string, mac string) (string, bool) {
	if len(table) == 0 || mac == "" {
		return "", false
	}
	ip, ok := table[strings.ToLower(mac)]
	return ip, ok
}

// collectUnitFileIDs lists the ids of static unit files found in UnitDir (and in
// its multi-user.target.wants directory).
func collectUnitFileIDs(unitDir string) ([]string, []string) {
	var warnings []string
	seen := make(map[string]struct{})
	dirs := []string{unitDir, filepath.Join(unitDir, "multi-user.target.wants")}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // not present on every host; not fatal
		}
		for _, e := range entries {
			id, ok := parseStaticUnitName(e.Name())
			if !ok {
				continue
			}
			seen[id] = struct{}{}
		}
	}
	if _, err := os.Stat(unitDir); err != nil {
		warnings = append(warnings, fmt.Sprintf("unit dir unreadable: %v", err))
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, warnings
}

const (
	// unitPrefixStatic is the prefix of the static per-VM unit files the previous
	// platform installs. They are named firecracker-<vm-id>.service, so the VM id
	// keeps its own "vm-" prefix: firecracker-vm-46d812e1.service manages
	// vm-46d812e1.
	unitPrefixStatic = "firecracker-"
	// unitInstanceFmt is vmlet's (and the newer platform's) template instance
	// unit name: firecracker-vm@<id>.service.
	unitInstanceFmt = "firecracker-vm@%s.service"
	// unitStaticFmt is the static unit name for a VM id.
	unitStaticFmt = "firecracker-%s.service"
	// systemdChunkSize bounds how many unit names go into one systemctl show.
	systemdChunkSize = 50
)

// parseStaticUnitName extracts the VM id from a static unit file name
// (firecracker-vm-<id>.service). Template files (firecracker-vm@.service) and
// backups (.bak-*) are rejected, as is anything that is not a VM id.
func parseStaticUnitName(name string) (string, bool) {
	if !strings.HasPrefix(name, unitPrefixStatic) || !strings.HasSuffix(name, ".service") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, unitPrefixStatic), ".service")
	if !strings.HasPrefix(id, vmIDPrefix) || len(id) <= len(vmIDPrefix) || strings.ContainsAny(id, "@.") {
		return "", false
	}
	return id, true
}

// unitInfo is the read-only systemd view of one unit.
type unitInfo struct {
	Name         string
	Present      bool
	Enabled      bool
	FileState    string
	Active       bool
	ExecStart    string
	FragmentPath string
}

// resolveUnits returns the unit vmlet should use for each VM id: the static
// unit when one exists on disk, otherwise the template instance.
func (o ScanOptions) resolveUnits(runner CommandRunner, ids map[string]struct{}) map[string]*unitInfo {
	units := make(map[string]*unitInfo, len(ids))
	byName := make(map[string]*unitInfo, len(ids))
	var toQuery []string

	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	for _, id := range sorted {
		staticName := fmt.Sprintf(unitStaticFmt, id)
		staticPath := filepath.Join(o.UnitDir, staticName)
		if _, err := os.Stat(staticPath); err == nil {
			u := &unitInfo{Name: staticName, Present: true, FragmentPath: staticPath}
			units[id] = u
			byName[staticName] = u
			toQuery = append(toQuery, staticName)
			continue
		}
		instance := fmt.Sprintf(unitInstanceFmt, id)
		u := &unitInfo{Name: instance}
		units[id] = u
		byName[instance] = u
		toQuery = append(toQuery, instance)
	}

	for _, found := range querySystemd(runner, toQuery) {
		u, ok := byName[found.Name]
		if !ok {
			continue
		}
		u.Present = u.Present || found.Present
		u.Enabled = found.Enabled
		u.FileState = found.FileState
		u.Active = found.Active
		if found.ExecStart != "" {
			u.ExecStart = found.ExecStart
		}
	}

	// Fill in the pinned binary for every unit, preferring the static unit file
	// (the literal ExecStart line the operator wrote) and falling back to what
	// systemd reports for the rendered instance.
	for _, u := range units {
		if u.ExecStart == "" && u.FragmentPath != "" {
			u.ExecStart = execStartFromUnitFile(u.FragmentPath)
		}
	}
	return units
}

// applyUnit copies the resolved unit state onto the discovery.
func (o ScanOptions) applyUnit(d *Discovery, u *unitInfo) {
	if u == nil {
		d.Warnings = append(d.Warnings, "no systemd unit found for this VM")
		return
	}
	d.UnitName = u.Name
	d.UnitFile = u.FragmentPath
	d.UnitPresent = u.Present
	d.UnitEnabled = u.Enabled
	d.UnitFileState = u.FileState
	d.UnitActive = u.Active
	d.Running = u.Active
	if bin := pinnedBinary(u.ExecStart, u.FragmentPath); bin != "" {
		d.FirecrackerBin = bin
	} else {
		d.Warnings = append(d.Warnings, fmt.Sprintf("no firecracker binary pinned by unit %s", u.Name))
	}
	if !u.Present {
		d.Warnings = append(d.Warnings, fmt.Sprintf("systemd unit %s not found", u.Name))
	}
}

// querySystemd batches `systemctl show` over the unit list (chunked so the
// argv stays reasonable) and parses the Key=Value output. A failing systemctl
// (no systemd, container, test harness) yields no information rather than an
// error: the scan reports units as absent with a warning.
func querySystemd(runner CommandRunner, names []string) []unitInfo {
	if len(names) == 0 {
		return nil
	}
	var out []unitInfo
	for start := 0; start < len(names); start += systemdChunkSize {
		end := start + systemdChunkSize
		if end > len(names) {
			end = len(names)
		}
		args := []string{"show", "--no-pager",
			"-p", "Id", "-p", "LoadState", "-p", "ActiveState",
			"-p", "UnitFileState", "-p", "FragmentPath", "-p", "ExecStart"}
		args = append(args, names[start:end]...)
		raw, err := runner.Run("systemctl", args...)
		if err != nil {
			return nil // no systemd view available
		}
		out = append(out, parseSystemdShow(raw)...)
	}
	return out
}

// parseSystemdShow parses the blank-line-separated blocks of `systemctl show`.
// Blocks are delimited by blank lines and properties are read in whatever order
// systemd printed them: real `systemctl show` output puts ExecStart first and
// Id= later, so a parser keyed on Id= would drop the value it needs.
func parseSystemdShow(raw string) []unitInfo {
	var units []unitInfo
	var cur unitInfo
	inBlock := false

	flush := func() {
		if inBlock && cur.Name != "" {
			units = append(units, cur)
		}
		cur = unitInfo{}
		inBlock = false
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key, value := line[:eq], strings.TrimSpace(line[eq+1:])
		inBlock = true
		switch key {
		case "Id":
			cur.Name = value
			cur.Present = true
		case "LoadState":
			cur.Present = value != "" && value != "not-found" && value != "error" && value != "masked"
		case "ActiveState":
			cur.Active = value == "active"
		case "UnitFileState":
			cur.FileState = value
			cur.Enabled = value == "enabled" || value == "enabled-runtime"
		case "FragmentPath":
			cur.FragmentPath = value
		case "ExecStart":
			cur.ExecStart = value
		}
	}
	flush()
	return units
}

// execStartFromUnitFile reads the ExecStart lines from a static unit file.
func execStartFromUnitFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured unit dir
	if err != nil {
		return ""
	}
	var cmds []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		cmds = append(cmds, strings.TrimPrefix(line, "ExecStart="))
	}
	return strings.Join(cmds, " ; ")
}

// pinnedBinary extracts the firecracker binary a unit pins. It understands the
// shapes systemd and unit files produce:
//
//	ExecStart=/usr/local/bin/firecracker --api-sock s                 (unit file)
//	ExecStart={ path=/bin/sh ; argv[]=/bin/sh -c 'exec /opt/haioflash/bin/firecracker ...' ; ... }
//	                                                               (systemctl show)
//
// In every one of them the pinned binary is the absolute path whose base name is
// firecracker, so that is what is returned.
func pinnedBinary(execStart, _ string) string {
	value := normalizeExecStart(execStart)
	for _, tok := range splitCommandTokens(value) {
		for _, part := range strings.Fields(tok) {
			part = strings.Trim(part, `'"`)
			if filepath.Base(part) != "firecracker" {
				continue
			}
			if strings.HasPrefix(part, "/") {
				return part
			}
		}
	}
	return ""
}

// normalizeExecStart flattens `systemctl show`'s ExecStart record
// ("{ path=... ; argv[]=... ; ... }") into a plain command line, leaving a unit
// file's ExecStart value untouched.
func normalizeExecStart(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "ExecStart=")
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return value
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "{"), "}")
	// The record's segments are " ; "-separated; drop the wrappers systemd adds
	// around the command and keep the ones that carry an argv.
	var keep []string
	for _, seg := range strings.Split(inner, " ; ") {
		seg = strings.TrimSpace(seg)
		switch {
		case strings.HasPrefix(seg, "path="), strings.HasPrefix(seg, "argv[]="), strings.HasPrefix(seg, "argv="):
			seg = seg[strings.Index(seg, "=")+1:]
			keep = append(keep, seg)
		case strings.HasPrefix(seg, "/"):
			keep = append(keep, seg)
		}
	}
	return strings.Join(keep, " ")
}

// splitCommandTokens splits a command line on whitespace, keeping quoted
// segments together.
func splitCommandTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
