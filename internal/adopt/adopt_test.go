package adopt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// node62ConfigJSON is the config.json of vm-02823a77 on node62, verbatim. It is
// the fixture the parser is written against.
const node62ConfigJSON = `{
  "balloon": {
    "amount_mib": 0,
    "deflate_on_oom": true,
    "stats_polling_interval_s": 1
  },
  "boot-source": {
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off nomodules root=/dev/vda rw net.ifnames=0 biosdevname=0 random.trust_cpu=on ds=nocloud;s=/var/lib/cloud/seed/nocloud/ cloud-init.dslist=NoCloud",
    "kernel_image_path": "/opt/haioflash/kernels/vmlinux"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "is_read_only": false,
      "is_root_device": true,
      "path_on_host": "/dev/zvol/default/haioflash-microvms/vm-02823a77"
    }
  ],
  "machine-config": {
    "mem_size_mib": 512,
    "vcpu_count": 1
  },
  "network-interfaces": [
    {
      "guest_mac": "00:d6:5e:9f:39:f0",
      "host_dev_name": "fc-aac0",
      "iface_id": "eth0"
    }
  ]
}
`

// recordedCommand is one command a runner was asked to execute.
type recordedCommand struct {
	Name string
	Args []string
}

// String renders the command as a shell line.
func (c recordedCommand) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// fakeRunner records every command and answers from a canned table. It is the
// seam the "never mutates" test runs through.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []recordedCommand
	handler func(name string, args []string) (string, error)
}

// Run records the call and delegates to the handler.
func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCommand{Name: name, Args: append([]string(nil), args...)})
	f.mu.Unlock()
	if f.handler == nil {
		return "", fmt.Errorf("fakeRunner: unhandled command %s", strings.Join(args, " "))
	}
	return f.handler(name, args)
}

// commands returns the recorded commands.
func (f *fakeRunner) commands() []recordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCommand(nil), f.calls...)
}

// fakeStore records the VMs it was asked to persist.
type fakeStore struct {
	mu    sync.Mutex
	saved map[string]*models.MicroVMInstance
	err   map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: map[string]*models.MicroVMInstance{}, err: map[string]error{}}
}

func (s *fakeStore) SaveVM(vm *models.MicroVMInstance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.err[vm.ID]; ok {
		return err
	}
	s.saved[vm.ID] = vm
	return nil
}

func (s *fakeStore) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.saved))
	for id := range s.saved {
		out = append(out, id)
	}
	return out
}

// hostFixture lays out a directory tree that looks like an adopted host:
// per-VM work dirs with config.json (one good, one corrupt, one absent), a ZFS
// dataset list, a neighbour table and systemd units.
type hostFixture struct {
	workDir  string
	unitDir  string
	runner   *fakeRunner
	vmIDs    []string
	zvolPath string
}

// newHostFixture builds the tree and the runner that answers for it.
func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	root := t.TempDir()
	workDir := filepath.Join(root, "opt", "haioflash", "work", "microvms")
	unitDir := filepath.Join(root, "etc", "systemd", "system")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work dir: %v", err)
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}

	zvol := filepath.Join(root, "dev", "zvol", "default", "haioflash-microvms", "vm-aaa")
	if err := os.MkdirAll(filepath.Dir(zvol), 0o755); err != nil {
		t.Fatalf("mkdir zvol dir: %v", err)
	}
	if err := os.WriteFile(zvol, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write zvol stand-in: %v", err)
	}

	writeConfig := func(vmID, body string) {
		dir := filepath.Join(workDir, vmID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", vmID, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write config for %s: %v", vmID, err)
		}
	}

	// vm-aaa: healthy, disk present on the host.
	writeConfig("vm-aaa", strings.ReplaceAll(node62ConfigJSON, "/dev/zvol/default/haioflash-microvms/vm-02823a77", zvol))
	// vm-bbb: corrupt config.json — must be reported, not abort the scan.
	writeConfig("vm-bbb", "{ this is not json")
	// vm-ccc: no config.json at all (directory only) — deleted by hand, say.
	if err := os.MkdirAll(filepath.Join(workDir, "vm-ccc"), 0o755); err != nil {
		t.Fatalf("mkdir vm-ccc: %v", err)
	}
	// vm-ddd: config that points at a disk that no longer exists.
	writeConfig("vm-ddd", strings.ReplaceAll(node62ConfigJSON,
		"/dev/zvol/default/haioflash-microvms/vm-02823a77",
		filepath.Join(root, "dev", "zvol", "default", "haioflash-microvms", "vm-gone")))
	// vm-eee: already known to vmlet (static, enabled unit on disk).
	writeConfig("vm-eee", strings.ReplaceAll(node62ConfigJSON,
		"/dev/zvol/default/haioflash-microvms/vm-02823a77", zvol))
	if err := os.WriteFile(filepath.Join(unitDir, "firecracker-vm-eee.service"),
		[]byte("[Service]\nExecStart=/opt/haioflash/bin/firecracker --api-sock firecracker.sock --config-file config.json\n"), 0o644); err != nil {
		t.Fatalf("write static unit: %v", err)
	}

	f := &hostFixture{workDir: workDir, unitDir: unitDir, zvolPath: zvol,
		vmIDs: []string{"vm-aaa", "vm-bbb", "vm-ccc", "vm-ddd", "vm-eee"}}
	f.runner = &fakeRunner{handler: f.handle}
	return f
}

// handle answers the read-only commands the scanner issues for the fixture.
func (h *hostFixture) handle(name string, args []string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "zfs" && strings.Contains(joined, "-o name,mountpoint"):
		return "default/haioflash-microvms\tnone\n", nil
	case name == "zfs" && strings.Contains(joined, "-o name,volsize"):
		return "default/haioflash-microvms\t-\n" +
			"default/haioflash-microvms/vm-aaa\t10G\n" +
			"default/haioflash-microvms/vm-eee\t10G\n", nil
	case name == "zfs": // -o name -r <dataset>
		return "default/haioflash-microvms\n" +
			"default/haioflash-microvms/vm-aaa\n" +
			"default/haioflash-microvms/vm-eee\n", nil
	case name == "ip":
		return "94.182.172.124 dev br0 lladdr 00:D6:5E:9F:39:F0 STALE\n" +
			"94.182.172.99 dev br0 FAILED\n", nil
	case name == "systemctl":
		return h.systemdShow(args), nil
	}
	return "", fmt.Errorf("unexpected command %s %s", name, joined)
}

// systemdShow renders a `systemctl show` answer for the fixture's units. Only
// arguments that look like a unit name are answered, so flags are ignored. Both
// the static name (firecracker-vm-<id>.service) and the template instance
// (firecracker-vm@<id>.service) are answered.
func (h *hostFixture) systemdShow(args []string) string {
	var b strings.Builder
	for _, a := range args {
		id, ok := staticIDFromUnitName(a)
		if !ok {
			id, ok = strings.CutPrefix(a, "firecracker-vm@")
			if !ok {
				continue
			}
			id = strings.TrimSuffix(id, ".service")
		}
		switch id {
		case "vm-aaa":
			writeUnit(&b, a, "active", "disabled", "/opt/haioflash/bin/firecracker --api-sock /run/microvmsvc/"+id+".sock")
		case "vm-eee":
			writeUnit(&b, a, "inactive", "enabled", "/opt/haioflash/bin/firecracker --api-sock firecracker.sock")
		default:
			writeUnit(&b, a, "inactive", "disabled", "/opt/haioflash/bin/firecracker --api-sock /run/microvmsvc/"+id+".sock")
		}
	}
	return b.String()
}

// writeUnit renders one unit block of `systemctl show` output.
func writeUnit(b *strings.Builder, name, activeState, fileState, execStart string) {
	fmt.Fprintf(b, "ExecStart={ path=%s ; argv[]=%s --config-file /opt/haioflash/work/microvms/x/config.json ; ignore_errors=no }\n", execStart, execStart)
	fmt.Fprintf(b, "Id=%s\n", name)
	fmt.Fprintf(b, "LoadState=loaded\n")
	fmt.Fprintf(b, "ActiveState=%s\n", activeState)
	fmt.Fprintf(b, "UnitFileState=%s\n", fileState)
	fmt.Fprintf(b, "FragmentPath=/etc/systemd/system/firecracker-vm@.service\n")
	b.WriteString("\n")
}

// fixtureOptions points ScanOptions at the fixture.
func (h *hostFixture) options() ScanOptions {
	return ScanOptions{
		WorkDir:          h.workDir,
		DatasetRoot:      "default/haioflash-microvms",
		UnitDir:          h.unitDir,
		SkipIPResolution: false,
		CommandTimeout:   5 * time.Second,
		Runner:           h.runner,
	}
}

func TestParseConfigNode62Fixture(t *testing.T) {
	cfg, err := ParseConfig([]byte(node62ConfigJSON))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	got := cfg.BootSource
	if got.KernelImagePath != "/opt/haioflash/kernels/vmlinux" {
		t.Errorf("kernel = %q", got.KernelImagePath)
	}
	if !strings.Contains(got.BootArgs, "ds=nocloud") {
		t.Errorf("boot args missing NoCloud: %q", got.BootArgs)
	}
	if cfg.MachineConfig.VcpuCount != 1 || cfg.MachineConfig.MemSizeMib != 512 {
		t.Errorf("machine-config = %+v", cfg.MachineConfig)
	}
	root, ok := cfg.RootDrive()
	if !ok {
		t.Fatalf("RootDrive: no root device found")
	}
	if root != "/dev/zvol/default/haioflash-microvms/vm-02823a77" {
		t.Errorf("root drive = %q", root)
	}
	if len(cfg.NetworkInterfaces) != 1 {
		t.Fatalf("interfaces = %d", len(cfg.NetworkInterfaces))
	}
	iface := cfg.NetworkInterfaces[0]
	if iface.GuestMAC != "00:d6:5e:9f:39:f0" || iface.HostDevName != "fc-aac0" || iface.IfaceID != "eth0" {
		t.Errorf("interface = %+v", iface)
	}
}

func TestParseConfigToleratesOddInput(t *testing.T) {
	if _, err := ParseConfig(nil); err == nil {
		t.Error("empty config should be an error")
	}
	if _, err := ParseConfig([]byte("{\"machine-config\": {\"vcpu_count\": 2}}")); err != nil {
		t.Errorf("config without drives/interfaces should parse: %v", err)
	}
	cfg, err := ParseConfig([]byte(`{"drives":[{"path_on_host":"/dev/vdb"},{"path_on_host":"/dev/vda","is_root_device":true}]}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	root, ok := cfg.RootDrive()
	if !ok || root != "/dev/vda" {
		t.Errorf("RootDrive should prefer is_root_device, got %q (ok=%v)", root, ok)
	}
}

func TestScanFindsVMsAndRecordsWarnings(t *testing.T) {
	h := newHostFixture(t)
	discoveries, report, err := ScanReported(h.options())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(discoveries) != len(h.vmIDs) {
		t.Fatalf("discoveries = %d, want %d (%v)", len(discoveries), len(h.vmIDs), discoveries)
	}
	byID := indexByID(discoveries)

	healthy := byID["vm-aaa"]
	if !healthy.Healthy() {
		t.Errorf("vm-aaa should be healthy: %+v", healthy)
	}
	if healthy.RootDiskPath != h.zvolPath || !healthy.RootDiskExists {
		t.Errorf("vm-aaa root disk = %q exists=%v", healthy.RootDiskPath, healthy.RootDiskExists)
	}
	if healthy.MemoryMB != 512 || healthy.VCPUs != 1 {
		t.Errorf("vm-aaa resources = %d vcpu / %d MB", healthy.VCPUs, healthy.MemoryMB)
	}
	if got := healthy.PrimaryInterface().HostDevName; got != "fc-aac0" {
		t.Errorf("vm-aaa host_dev_name = %q", got)
	}
	if healthy.IPAddress != "94.182.172.124" || !healthy.IPAddressKnown {
		t.Errorf("vm-aaa IP = %q known=%v", healthy.IPAddress, healthy.IPAddressKnown)
	}
	if healthy.FirecrackerBin != "/opt/haioflash/bin/firecracker" {
		t.Errorf("vm-aaa pinned binary = %q", healthy.FirecrackerBin)
	}
	if healthy.RootDiskBytes != 10<<30 {
		t.Errorf("vm-aaa disk size = %d bytes", healthy.RootDiskBytes)
	}
	if !healthy.Running {
		t.Errorf("vm-aaa should be running")
	}
	if healthy.UnitName != "firecracker-vm@vm-aaa.service" {
		t.Errorf("vm-aaa unit = %q", healthy.UnitName)
	}

	// Corrupt config: reported with a warning, not fatal.
	corrupt := byID["vm-bbb"]
	if corrupt.ConfigReadable {
		t.Error("vm-bbb config should be unreadable")
	}
	if warningFor(corrupt, "config.json") == "" {
		t.Errorf("vm-bbb should warn about config.json: %v", corrupt.Warnings)
	}
	// Missing config directory content and missing disk.
	if byID["vm-ccc"].RootDiskExists {
		t.Error("vm-ccc has no config, so its inferred disk must not be claimed to exist")
	}
	if !strings.Contains(strings.Join(byID["vm-ccc"].Warnings, "|"), "config.json unreadable") {
		t.Errorf("vm-ccc warnings = %v", byID["vm-ccc"].Warnings)
	}
	if byID["vm-ddd"].RootDiskExists {
		t.Error("vm-ddd disk does not exist")
	}

	// Static unit wins over the template instance, and is reported as such.
	static := byID["vm-eee"]
	// The static unit file wins over the template instance: it is the unit that
	// actually manages the VM, and it is what records the pinned binary.
	if static.UnitName != "firecracker-vm-eee.service" {
		t.Errorf("vm-eee unit = %q", static.UnitName)
	}
	if static.UnitFile == "" {
		t.Errorf("vm-eee should record its unit file path: %+v", static)
	}
	if !static.UnitEnabled {
		t.Errorf("vm-eee unit should be enabled: %+v", static)
	}
	if static.FirecrackerBin != "/opt/haioflash/bin/firecracker" {
		t.Errorf("vm-eee pinned binary = %q", static.FirecrackerBin)
	}
	if warningFor(static, "static unit") == "" {
		t.Errorf("vm-eee should warn that vmlet polls the template unit: %v", static.Warnings)
	}

	// Report: all three sources contributed.
	if report.ZFSDatasets != 2 || report.WorkDirs != 5 || report.UnitFiles != 1 {
		t.Errorf("report counts = %+v", report)
	}
}

func TestScanFallsBackToWorkDirWhenZFSIsMissing(t *testing.T) {
	h := newHostFixture(t)
	inner := h.runner
	h.runner = &fakeRunner{handler: func(name string, args []string) (string, error) {
		if name == "zfs" {
			return "", fmt.Errorf("zfs: command not found")
		}
		return inner.handler(name, args)
	}}
	opts := h.options()
	opts.Runner = h.runner

	discoveries, report, err := ScanReported(opts)
	if err != nil {
		t.Fatalf("Scan should still succeed without ZFS: %v", err)
	}
	if len(discoveries) != len(h.vmIDs) {
		t.Errorf("discoveries = %d, want %d", len(discoveries), len(h.vmIDs))
	}
	if report.ZFSDatasets != 0 {
		t.Errorf("zfs datasets should be 0, got %d", report.ZFSDatasets)
	}
	if len(report.Warnings) == 0 {
		t.Error("a missing zfs should be reported as a warning")
	}
}

func TestPlanDiffsAgainstExistingVMs(t *testing.T) {
	h := newHostFixture(t)
	discoveries, _, err := ScanReported(h.options())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	actions := Plan(discoveries, []string{"vm-eee"})
	kinds := map[ActionKind]int{}
	reasons := map[string]string{}
	for _, a := range actions {
		kinds[a.Kind]++
		reasons[a.VMID] = a.Reason
	}
	if kinds[ActionAdopt] != 1 {
		t.Errorf("adopt count = %d, want 1 (%+v)", kinds[ActionAdopt], actions)
	}
	if kinds[ActionSkipExisting] != 1 || reasons["vm-eee"] != "already registered in vmlet" {
		t.Errorf("skip-existing = %d (%q)", kinds[ActionSkipExisting], reasons["vm-eee"])
	}
	if kinds[ActionSkipUnhealthy] != 3 {
		t.Fatalf("skip-unhealthy count = %d, want 3 (%+v)", kinds[ActionSkipUnhealthy], actions)
	}
	if !strings.Contains(reasons["vm-ccc"], "config.json") ||
		!strings.Contains(reasons["vm-ccc"], "root disk") {
		t.Errorf("vm-ccc reason should list every defect: %q", reasons["vm-ccc"])
	}
	if !strings.Contains(reasons["vm-bbb"], "config.json") {
		t.Errorf("vm-bbb reason = %q", reasons["vm-bbb"])
	}
	if !strings.Contains(reasons["vm-ddd"], "root disk") {
		t.Errorf("vm-ddd reason = %q", reasons["vm-ddd"])
	}

	// Deterministic and idempotent.
	again := Plan(discoveries, []string{"vm-eee"})
	if len(again) != len(actions) {
		t.Fatalf("plan is not stable: %d vs %d", len(again), len(actions))
	}
	for i := range actions {
		if actions[i].VMID != again[i].VMID || actions[i].Kind != again[i].Kind {
			t.Fatalf("plan order changed at %d: %+v vs %+v", i, actions[i], again[i])
		}
	}

	// Everything known → nothing adopted.
	all := make([]string, 0, len(discoveries))
	for _, d := range discoveries {
		all = append(all, d.ID)
	}
	if counts := Counts(Plan(discoveries, all)); counts[ActionAdopt] != 0 {
		t.Errorf("adopt count = %d, want 0", counts[ActionAdopt])
	}
}

func TestScanWithoutSystemdReportsMissingUnits(t *testing.T) {
	h := newHostFixture(t)
	inner := h.runner
	h.runner = &fakeRunner{handler: func(name string, args []string) (string, error) {
		if name == "systemctl" {
			return "", fmt.Errorf("systemctl: command not found")
		}
		return inner.handler(name, args)
	}}
	opts := h.options()
	opts.Runner = h.runner

	discoveries, _, err := ScanReported(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byID := indexByID(discoveries)

	// Template instances are invisible without systemd: no unit, no pinned
	// binary, and the VM is not manageable.
	tmpl := byID["vm-aaa"]
	if tmpl.UnitPresent || tmpl.FirecrackerBin != "" {
		t.Errorf("vm-aaa should have no unit without systemd: %+v", tmpl)
	}
	// A static unit file is on disk, so it is still authoritative without
	// systemd: vmlet can read the unit and the binary it pins.
	static := byID["vm-eee"]
	if !static.UnitPresent || static.FirecrackerBin != "/opt/haioflash/bin/firecracker" {
		t.Errorf("vm-eee should keep its static unit: %+v", static)
	}

	// Everything without a unit is unhealthy: no unit means vmlet could not
	// manage the VM. vm-eee keeps its record.
	actions := Plan(discoveries, nil)
	for _, a := range actions {
		switch a.VMID {
		case "vm-eee":
			if a.Kind != ActionAdopt {
				t.Errorf("vm-eee action = %+v", a)
			}
		default:
			if a.Kind != ActionSkipUnhealthy {
				t.Errorf("action = %+v, want skip-unhealthy", a)
			}
			if !strings.Contains(a.Reason, "systemd unit") {
				t.Errorf("reason = %q", a.Reason)
			}
		}
	}
}

func TestDatasetRootAsPathIsResolved(t *testing.T) {
	h := newHostFixture(t)
	opts := h.options()
	// A path-shaped DatasetRoot is resolved against the dataset that carries it
	// (mountpoint first, then the trailing dataset name).
	opts.DatasetRoot = "/opt/haioflash-microvms"

	discoveries, report, err := ScanReported(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.DatasetRoot != "default/haioflash-microvms" {
		t.Errorf("resolved dataset root = %q", report.DatasetRoot)
	}
	if len(discoveries) != len(h.vmIDs) {
		t.Errorf("discoveries = %d, want %d", len(discoveries), len(h.vmIDs))
	}
	for _, d := range discoveries {
		if d.ID != "vm-aaa" && d.ID != "vm-eee" {
			continue
		}
		if d.RootDiskBytes != 10<<30 {
			t.Errorf("%s disk size = %d, want 10 GiB", d.ID, d.RootDiskBytes)
		}
	}
}

func TestScanWithoutIPResolution(t *testing.T) {
	h := newHostFixture(t)
	opts := h.options()
	opts.SkipIPResolution = true

	discoveries, _, err := ScanReported(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byID := indexByID(discoveries)
	if byID["vm-aaa"].IPAddressKnown || byID["vm-aaa"].IPAddress != "" {
		t.Errorf("IP should be left unknown: %+v", byID["vm-aaa"])
	}
	// ip neigh show must not have been issued at all.
	for _, c := range h.runner.commands() {
		if c.Name == "ip" {
			t.Errorf("unexpected ip command with IP resolution off: %s", c)
		}
	}
	// The record still carries the MAC, the device name, and an honest note
	// about where the IP would have come from.
	vm := ToMicroVM(byID["vm-aaa"], ApplyOptions{})
	if vm.Network.MACAddress != "00:d6:5e:9f:39:f0" || vm.Network.TAPDevice != "fc-aac0" {
		t.Errorf("network = %+v", vm.Network)
	}
	if got := vm.Metadata.Labels[LabelIPSource]; got != "unknown" {
		t.Errorf("label %s = %q, want unknown", LabelIPSource, got)
	}
}

func TestScanOnlyIssuesReadOnlyCommands(t *testing.T) {
	h := newHostFixture(t)
	if _, _, err := ScanReported(h.options()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	commands := h.runner.commands()
	if len(commands) == 0 {
		t.Fatal("no commands were issued — the recorder is not wired up")
	}
	seen := map[string]bool{}
	for _, c := range commands {
		if !isReadOnly(c.Name, c.Args) {
			t.Errorf("Scan issued a command outside the read-only allowlist: %s", c)
		}
		seen[c.String()] = true
	}
	// The exact command set the adopter is allowed to run. Anything new here is
	// a change to the safety property and must be justified.
	wantShapes := []string{
		"zfs list -H -o name,volsize -r default/haioflash-microvms",
		"zfs list -H -o name -r default/haioflash-microvms",
		"ip -o neigh show",
		"systemctl show --no-pager -p Id -p LoadState -p ActiveState -p UnitFileState -p FragmentPath -p ExecStart",
	}
	for _, shape := range wantShapes {
		found := false
		for c := range seen {
			if strings.HasPrefix(c, shape) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a command shaped like %q, got: %v", shape, sortedCommandList(seen))
		}
	}

	// And the things that would violate the read-only contract. Matched against
	// whole arguments, never substrings: --no-pager and ExecStart both contain
	// "start", and neither is a mutation.
	forbiddenVerbs := []string{"start", "stop", "restart", "reload", "enable", "disable", "mask",
		"kill", "create", "destroy", "snapshot", "rollback", "mount", "umount",
		"iptables", "brctl", "zpool", "tc", "add", "del", "set", "replace", "flush"}
	for _, c := range commands {
		for _, arg := range c.Args {
			for _, bad := range forbiddenVerbs {
				if arg == bad {
					t.Errorf("Scan used the mutating verb %q: %s", bad, c)
				}
			}
		}
	}
}

func TestScanFailsClosedOnMutatingCommand(t *testing.T) {
	r := execRunner{}
	if _, err := r.Run("systemctl", "stop", "firecracker-vm@vm-aaa.service"); err == nil {
		t.Error("execRunner must refuse systemctl stop")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error should name the reason: %v", err)
	}
	for _, bad := range [][]string{
		{"zfs", "destroy", "default/haioflash-microvms/vm-aaa"},
		{"ip", "link", "add", "fc-dead", "type", "veth"},
		{"mount", "/dev/zvol/default/haioflash-microvms/vm-aaa", "/mnt"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-j", "MASQUERADE"},
		{"systemctl", "restart", "firecracker-vm@vm-aaa.service"},
		{"systemctl", "disable", "firecracker-vm@vm-aaa.service"},
	} {
		if _, err := r.Run(bad[0], bad[1:]...); err == nil {
			t.Errorf("execRunner ran %v", bad)
		}
	}
}

func TestApplyWritesRecordsWithoutTouchingVMs(t *testing.T) {
	h := newHostFixture(t)
	discoveries, _, err := ScanReported(h.options())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	store := newFakeStore()
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	result := Apply(discoveries, []string{"vm-eee"}, ApplyOptions{
		NodeID:     "local-node",
		Store:      store,
		Now:        now,
		BridgeName: "br0",
	})

	ids := store.ids()
	if len(ids) != 1 || ids[0] != "vm-aaa" {
		t.Fatalf("saved ids = %v, want [vm-aaa]", ids)
	}
	if len(result.Failed) != 0 {
		t.Errorf("failures = %+v", result.Failed)
	}
	if result.Counts[ActionSkipUnhealthy] != 3 || result.Counts[ActionSkipExisting] != 1 {
		t.Errorf("counts = %+v", result.Counts)
	}

	vm := store.saved["vm-aaa"]
	if vm.NodeID != "local-node" {
		t.Errorf("NodeID = %q", vm.NodeID)
	}
	if vm.Status != models.MicroVMStatusRunning {
		t.Errorf("Status = %q, want running (the VM is running)", vm.Status)
	}
	if vm.Resources.VCPUs != 1 || vm.Resources.MemoryMB != 512 || vm.Resources.StorageMB != 10240 {
		t.Errorf("Resources = %+v", vm.Resources)
	}
	if vm.Network.AssignmentMethod != models.NetworkModePreWired {
		t.Errorf("AssignmentMethod = %q, want prewired (the VM's unit owns its NIC)", vm.Network.AssignmentMethod)
	}
	if vm.Network.MACAddress != "00:d6:5e:9f:39:f0" {
		t.Errorf("MACAddress = %q", vm.Network.MACAddress)
	}
	if vm.Network.TAPDevice != "fc-aac0" {
		t.Errorf("TAPDevice = %q, want the existing fc-* device", vm.Network.TAPDevice)
	}
	if vm.Network.BridgeName != "br0" {
		t.Errorf("BridgeName = %q", vm.Network.BridgeName)
	}
	if vm.Network.IPAddress != "94.182.172.124" {
		t.Errorf("IPAddress = %q", vm.Network.IPAddress)
	}
	if vm.Metadata.AutoStart {
		t.Error("AutoStart must be false: vmlet must not start a VM it has just taken over")
	}
	if vm.Metadata.CustomerID != DefaultCustomerID {
		t.Errorf("CustomerID = %q", vm.Metadata.CustomerID)
	}
	if !vm.Metadata.CreatedAt.Equal(now) || !vm.Metadata.LastActivity.Equal(now) {
		t.Errorf("timestamps = %v / %v", vm.Metadata.CreatedAt, vm.Metadata.LastActivity)
	}
	wantLabels := map[string]string{
		LabelExternalRootfs:     h.zvolPath,
		LabelAdoptedFrom:        "haioflash",
		LabelAdoptedUnit:        "firecracker-vm@vm-aaa.service",
		LabelFirecrackerBin:     "/opt/haioflash/bin/firecracker",
		LabelKernelPath:         "/opt/haioflash/kernels/vmlinux",
		LabelBootRootDevice:     "/dev/vda",
		LabelIPSource:           "host-neighbour-table",
		LabelRunningWhenAdopted: "true",
	}
	for k, want := range wantLabels {
		if got := vm.Metadata.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if err := vm.Validate(); err != nil {
		t.Errorf("adopted record must be valid: %v", err)
	}

	// Apply is idempotent: running it again with every VM registered adopts nothing.
	again := Apply(discoveries, idsOf(discoveries), ApplyOptions{Store: store, Now: now})
	if len(again.Applied) != 0 || len(again.Failed) != 0 {
		t.Errorf("second apply = %+v", again)
	}
}

func TestApplySurvivesAStoreFailure(t *testing.T) {
	h := newHostFixture(t)
	discoveries, _, err := ScanReported(h.options())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	store := newFakeStore()
	store.err["vm-aaa"] = fmt.Errorf("database is locked")

	result := Apply(discoveries, nil, ApplyOptions{Store: store})
	if len(result.Failed) != 1 || result.Failed[0].VMID != "vm-aaa" {
		t.Fatalf("failures = %+v", result.Failed)
	}
	if !strings.Contains(result.Failed[0].Error, "database is locked") {
		t.Errorf("error = %q", result.Failed[0].Error)
	}
	// The other healthy VM is still adopted: one failure must not abort the run.
	if len(result.Applied) != 1 || result.Applied[0].VMID != "vm-eee" {
		t.Errorf("applied = %+v", result.Applied)
	}
	if len(store.ids()) != 1 || store.ids()[0] != "vm-eee" {
		t.Errorf("store = %v", store.ids())
	}
}

func TestPinnedBinaryAcrossUnitShapes(t *testing.T) {
	cases := []struct{ name, execStart, want string }{
		{"bare", "/usr/local/bin/firecracker --api-sock firecracker.sock --config-file config.json", "/usr/local/bin/firecracker"},
		{"sh wrapper", "/bin/sh -c 'exec 3<>console.in; exec /opt/haioflash/bin/firecracker --api-sock /run/microvmsvc/x.sock'", "/opt/haioflash/bin/firecracker"},
		{"quoted path", `"/opt/haioflash/bin/firecracker" --api-sock s`, "/opt/haioflash/bin/firecracker"},
		{"no binary", "/bin/sh -c 'echo hello'", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := pinnedBinary(tc.execStart, ""); got != tc.want {
			t.Errorf("%s: pinnedBinary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseStaticUnitName(t *testing.T) {
	// node62's static unit is firecracker-vm-46d812e1.service and it manages
	// vm-46d812e1: the id keeps its own vm- prefix.
	if id, ok := parseStaticUnitName("firecracker-vm-46d812e1.service"); !ok || id != "vm-46d812e1" {
		t.Errorf("static unit = %q ok=%v", id, ok)
	}
	for _, name := range []string{"firecracker-vm@.service", "firecracker-vm@.service.bak", "other.service", "firecracker-vm-.service"} {
		if _, ok := parseStaticUnitName(name); ok {
			t.Errorf("%q should not be a static VM unit", name)
		}
	}
}

func TestScanOptionsDefaults(t *testing.T) {
	opts := ScanOptions{}.withDefaults()
	if opts.WorkDir != DefaultWorkDir || opts.DatasetRoot != DefaultDatasetRoot || opts.UnitDir != DefaultUnitDir {
		t.Errorf("defaults = %+v", opts)
	}
	if opts.SkipIPResolution {
		t.Error("IP resolution should be on by default (SkipIPResolution must be false)")
	}
	if got := DefaultScanOptions(); got.SkipIPResolution {
		t.Error("DefaultScanOptions must leave IP resolution on")
	}
}

func TestParseZFSHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"10G", 10 << 30}, {"25G", 25 << 30}, {"512M", 512 << 20}, {"1.5T", 1649267441664}, {"-", 0}, {"none", 0}, {"10737418240", 10 << 30},
	} {
		got, _ := parseZFSHumanSize(tc.in)
		if got != tc.want {
			t.Errorf("parseZFSHumanSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// indexByID indexes discoveries by VM id.
func indexByID(discoveries []Discovery) map[string]Discovery {
	out := make(map[string]Discovery, len(discoveries))
	for _, d := range discoveries {
		out[d.ID] = d
	}
	return out
}

// warningFor returns the first warning containing want, or "".
func warningFor(d Discovery, want string) string {
	for _, w := range d.Warnings {
		if strings.Contains(w, want) {
			return w
		}
	}
	return ""
}

// idsOf returns the VM ids of a discovery list.
func idsOf(discoveries []Discovery) []string {
	out := make([]string, 0, len(discoveries))
	for _, d := range discoveries {
		out = append(out, d.ID)
	}
	return out
}

// staticIDFromUnitName extracts a VM id from a static unit name.
func staticIDFromUnitName(name string) (string, bool) {
	return parseStaticUnitName(name)
}

// sortedCommandList renders the recorded commands for a failure message.
func sortedCommandList(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}
