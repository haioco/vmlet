package adopt

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// Labels the adopter writes on every record it creates. The one that matters
// most is external_rootfs_path: it points vmlet at the root disk the VM already
// runs on, so no disk is ever created, copied or moved.
const (
	// LabelExternalRootfs is read by the firecracker manager and by
	// regenerateVMConfigJSON to resolve an adopted VM's root device.
	LabelExternalRootfs = "external_rootfs_path"
	// LabelAdoptedFrom records which platform the VM came from.
	LabelAdoptedFrom = "adopted_from"
	// LabelAdoptedAt is when the record was created.
	LabelAdoptedAt = "adopted_at"
	// LabelAdoptedUnit is the systemd unit that already manages the VM.
	LabelAdoptedUnit = "haioflash_unit"
	// LabelUnitEnabled records whether that unit was enabled (boot-time start).
	LabelUnitEnabled = "unit_enabled"
	// LabelFirecrackerBin records which firecracker binary the VM pins.
	LabelFirecrackerBin = "firecracker_bin"
	// LabelKernelPath records the kernel the VM boots with today.
	LabelKernelPath = "kernel_path"
	// LabelBootRootDevice records the guest's root device from boot_args.
	LabelBootRootDevice = "boot_root_device"
	// LabelIPSource records where the IP address came from.
	LabelIPSource = "ip_source"
	// LabelRunningWhenAdopted records the state at adoption time.
	LabelRunningWhenAdopted = "running_when_adopted"
	// LabelHostDevs records every host-side device the VM already uses.
	LabelHostDevs = "host_devs"

	// DefaultCustomerID is used when the caller does not supply one: billing
	// has no catalog on an adopted host, so there is nothing better to record.
	DefaultCustomerID = "haioflash-adopted"

	// DefaultNodeID matches the id the node's create path uses for local VMs.
	DefaultNodeID = "local-node"

	// FallbackVCPUs / FallbackMemoryMB / FallbackStorageMB are used only when
	// config.json is silent (0), because a record with no resources at all is
	// useless to the panel. A value that is merely unusual (a 64 MB guest, a
	// 32-vCPU machine) is kept as found: the record reports the host, it does
	// not decide what the host should be.
	FallbackVCPUs     = 1
	FallbackMemoryMB  = 512
	FallbackStorageMB = 1024
)

// VMStore is the persistence surface the adopter needs. It is an interface
// deliberately: internal/storage implements it (SQLiteStorage.SaveVM) and this
// package never imports internal/storage, so no cycle can form.
type VMStore interface {
	SaveVM(vm *models.MicroVMInstance) error
}

// ApplyOptions controls how discoveries become vmlet VM records.
type ApplyOptions struct {
	// NodeID is stamped on every record (the local node).
	NodeID string
	// CustomerID is stamped on every record.
	CustomerID string
	// BridgeName is the bridge the VMs are already attached to (default br0).
	BridgeName string
	// Store receives the records. Nil means "build the records only" (dry run
	// of the conversion step).
	Store VMStore
	// Now fixes the clock for reproducible records; zero means time.Now().
	Now time.Time
}

// Applied records one successfully adopted VM.
type Applied struct {
	VMID     string                  `json:"vm_id"`
	Instance *models.MicroVMInstance `json:"-"`
}

// Failure records one VM that could not be adopted, without aborting the rest.
type Failure struct {
	VMID  string `json:"vm_id"`
	Error string `json:"error"`
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	Applied []Applied          `json:"applied"`
	Failed  []Failure          `json:"failed,omitempty"`
	Actions []Action           `json:"-"`
	Counts  map[ActionKind]int `json:"counts"`
}

// Apply converts the healthy discoveries into vmlet VM records and persists
// them. It runs the same Plan first, so a VM vmlet already knows is never
// overwritten and an unhealthy VM is never registered.
//
// This is the only step that writes, and it writes only to vmlet's own store.
// No VM is started, stopped or restarted, no systemd unit is touched, and no
// fc-* device is created or removed: the VMs keep running exactly as they were.
func Apply(discoveries []Discovery, existing []string, opts ApplyOptions) ApplyResult {
	actions := Plan(discoveries, existing)
	result := ApplyResult{Actions: actions, Counts: Counts(actions)}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	for _, action := range actions {
		if action.Kind != ActionAdopt || action.Discovery == nil {
			continue
		}
		d := *action.Discovery
		vm := ToMicroVM(d, ApplyOptions{
			NodeID:     opts.NodeID,
			CustomerID: opts.CustomerID,
			BridgeName: opts.BridgeName,
			Now:        now,
		})
		if opts.Store != nil {
			if err := opts.Store.SaveVM(vm); err != nil {
				result.Failed = append(result.Failed, Failure{VMID: d.ID, Error: err.Error()})
				continue
			}
		}
		result.Applied = append(result.Applied, Applied{VMID: d.ID, Instance: vm})
	}
	return result
}

// ToMicroVM converts one discovery into vmlet's VM model.
//
// The shape matches what the node's create path produces, with two rules
// specific to adoption:
//
//   - the root disk path and the host-side device name (fc-<hex>) are recorded
//     verbatim, so vmlet reuses the existing disk and the existing bridged
//     device instead of creating new ones;
//   - auto_start is false. An adopted VM is already running its own unit; vmlet
//     must not start a second copy of it, and must not decide on its own when it
//     should come back after a host reboot. Set auto_start from the unit's
//     enabled state (recorded in the unit_enabled label) once you want that.
func ToMicroVM(d Discovery, opts ApplyOptions) *models.MicroVMInstance {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	nodeID := firstNonEmpty(opts.NodeID, DefaultNodeID)
	customerID := firstNonEmpty(opts.CustomerID, DefaultCustomerID)
	bridge := firstNonEmpty(opts.BridgeName, DefaultBridge)

	status := models.MicroVMStatusStopped
	if d.Running {
		status = models.MicroVMStatusRunning
	}

	primary := d.PrimaryInterface()
	vcpus := d.VCPUs
	if vcpus == 0 {
		vcpus = FallbackVCPUs
	}
	memoryMB := d.MemoryMB
	if memoryMB == 0 {
		memoryMB = FallbackMemoryMB
	}
	// StorageMB mirrors the size of the disk that is already there (from the
	// zvol's volsize, or the device size on hosts without ZFS). It is a report,
	// not a request: the record is never used to resize or recreate the disk.
	// Only an unknown size falls back to the floor.
	storageMB := bytesToMB(d.RootDiskBytes)
	if storageMB == 0 {
		storageMB = FallbackStorageMB
	}

	vm := &models.MicroVMInstance{
		ID:     d.ID,
		NodeID: nodeID,
		Status: status,
		Resources: models.ResourceSpecs{
			VCPUs:     vcpus,
			MemoryMB:  memoryMB,
			StorageMB: storageMB,
		},
		// The VM's disk already exists as a zvol/partition under the old
		// platform; vmlet keeps using it (external_rootfs_path label).
		Storage: models.StorageConfig{Type: models.StorageTypeLocal},
		Network: models.NetworkInfo{
			// Bridged-public: the guest holds a routable public address on its
			// fc-* device, which is already on the host bridge. This is the mode
			// that tells vmlet "the device exists, do not create one".
			AssignmentMethod: models.NetworkModePreWired,
			MACAddress:       primary.GuestMAC,
			IPAddress:        d.IPAddress,
			TAPDevice:        primary.HostDevName,
			BridgeName:       bridge,
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt:    now,
			LastActivity: now,
			CustomerID:   customerID,
			AutoStart:    false,
			Labels:       adoptionLabels(d, now),
		},
	}
	return vm
}

// adoptionLabels records where the VM came from and everything vmlet needs to
// keep managing it without recreating anything.
func adoptionLabels(d Discovery, now time.Time) map[string]string {
	labels := map[string]string{
		LabelAdoptedFrom:        "haioflash",
		LabelAdoptedAt:          now.UTC().Format(time.RFC3339),
		LabelRunningWhenAdopted: boolLabel(d.Running),
		LabelUnitEnabled:        boolLabel(d.UnitEnabled),
	}
	if d.RootDiskPath != "" {
		labels[LabelExternalRootfs] = d.RootDiskPath
	}
	if d.UnitName != "" {
		labels[LabelAdoptedUnit] = d.UnitName
	}
	if d.FirecrackerBin != "" {
		labels[LabelFirecrackerBin] = d.FirecrackerBin
	}
	if d.KernelPath != "" {
		labels[LabelKernelPath] = d.KernelPath
	}
	if root := rootDeviceFromBootArgs(d.BootArgs); root != "" {
		labels[LabelBootRootDevice] = root
	}
	if d.IPAddressKnown {
		labels[LabelIPSource] = "host-neighbour-table"
	} else {
		labels[LabelIPSource] = "unknown"
	}
	if devs := hostDevNames(d); devs != "" {
		labels[LabelHostDevs] = devs
	}
	return labels
}

// rootDeviceFromBootArgs pulls root= out of a firecracker boot_args string.
func rootDeviceFromBootArgs(bootArgs string) string {
	for _, tok := range strings.Fields(bootArgs) {
		if strings.HasPrefix(tok, "root=") {
			return strings.TrimPrefix(tok, "root=")
		}
	}
	return ""
}

// hostDevNames lists every host-side device the VM already uses.
func hostDevNames(d Discovery) string {
	var names []string
	for _, iface := range d.Interfaces {
		if iface.HostDevName != "" {
			names = append(names, iface.HostDevName)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// AdoptedIDs returns the ids of the records Apply created, sorted.
func (r ApplyResult) AdoptedIDs() []string {
	ids := make([]string, 0, len(r.Applied))
	for _, a := range r.Applied {
		ids = append(ids, a.VMID)
	}
	sort.Strings(ids)
	return ids
}

// FailedIDs returns the ids that could not be adopted, sorted.
func (r ApplyResult) FailedIDs() []string {
	ids := make([]string, 0, len(r.Failed))
	for _, f := range r.Failed {
		ids = append(ids, f.VMID)
	}
	sort.Strings(ids)
	return ids
}

// String renders a one-line summary of an apply run.
func (r ApplyResult) String() string {
	return fmt.Sprintf("adopted=%d failed=%d adopt=%d skip-existing=%d skip-unhealthy=%d",
		len(r.Applied), len(r.Failed),
		r.Counts[ActionAdopt], r.Counts[ActionSkipExisting], r.Counts[ActionSkipUnhealthy])
}

// bytesToMB converts bytes to whole MiB.
func bytesToMB(b uint64) uint32 {
	if b == 0 {
		return 0
	}
	return uint32(b / (1024 * 1024)) //nolint:gosec // bounded by uint32 storage_mb
}

// boolLabel renders a bool for storage in a label.
func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
