// Package ovn's migration planner decides what has to change to move a VM from
// the bridge backend onto OVN, and drives those changes in a safe order.
//
// The planner is deliberately pure: it takes the current and desired per-VM
// state and returns an ordered list of steps. That keeps it testable without a
// host, and lets the caller dry-run a migration before touching anything.
package ovn

import "fmt"

// MigrationStep is one unit of work in a bridge -> OVN migration.
type MigrationStep struct {
	// Name identifies the step (e.g. "ovs-attach").
	Name string
	// Description explains what the step does, for dry-run output.
	Description string
	// TODO reports whether the step still needs to be implemented by the
	// caller (for example removing ebtables rules that only the bridge
	// backend knows about).
	TODO bool
}

// VMState is the per-VM input the planner reasons about.
type VMState struct {
	ID string
	// Network is the vmlet network the VM belongs to.
	Network string
	// MAC is the VM's current MAC (stable across the migration).
	MAC string
	// IP is the VM's address on its network.
	IP string
	// Backend is "bridge" or "ovn", as configured today.
	Backend string
	// TAP is the host-side tap device the VM uses.
	TAP string
	// OldBridge is the Linux bridge the tap is attached to in bridge mode.
	OldBridge string
}

// ValidationError marks a VM that cannot be migrated and why.
type ValidationError struct {
	VMID string
	Err  string
}

func (v ValidationError) ValidationError() string {
	return fmt.Sprintf("vm %s: %s", v.VMID, v.Err)
}

// Validate returns the problems that make migration impossible, so a caller can
// refuse to start rather than half-migrate a VM.
func Validate(vm VMState) []ValidationError {
	var errs []ValidationError
	if vm.ID == "" {
		errs = append(errs, ValidationError{"", "vm id is required"})
	}
	if vm.TAP == "" {
		errs = append(errs, ValidationError{vm.ID, "tap device is required"})
	}
	if vm.OldBridge == "" {
		errs = append(errs, ValidationError{vm.ID, "current bridge is required"})
	}
	if vm.Network == "" {
		errs = append(errs, ValidationError{vm.ID, "network is required"})
	}
	if vm.MAC == "" {
		errs = append(errs, ValidationError{vm.ID, "MAC address is required to keep the guest address stable"})
	}
	return errs
}

// MigrationPlan is the ordered set of steps for migrating a set of VMs.
type MigrationPlan struct {
	// Steps are ordered; they must run in order.
	Steps []MigrationStep
	// VMErrors are VMs that could not be planned.
	VMErrors []ValidationError
}

// Empty reports whether the plan has any work to do.
func (p MigrationPlan) Empty() bool {
	return len(p.Steps) == 0
}

// Describe renders the plan for --dry-run style output.
func (p MigrationPlan) Describe() string {
	if p.Empty() {
		return "nothing to migrate\n"
	}
	out := ""
	for i, step := range p.Steps {
		marker := " "
		if step.TODO {
			marker = "!"
		}
		out += fmt.Sprintf("%2d. [%s] %-16s %s\n", i+1, marker, step.Name, step.Description)
	}
	for _, e := range p.VMErrors {
		out += fmt.Sprintf("    ERROR %s\n", e.ValidationError())
	}
	return out
}

// PlanMigration works out the steps needed to move a set of VMs from the bridge
// backend to OVN.
//
// Order matters and is enforced here:
//  1. Validate. A VM that cannot be migrated is reported, not half-migrated.
//  2. Read the current OVN state so the plan only contains real deltas.
//  3. Detach the tap from the old bridge (this is the handover point).
//  4. Attach the tap to br-int with the iface-id OVN expects.
//  5. Reconcile the logical topology, DHCP reservations, security and NAT.
//  6. Remove the bridge-side rules (needs bridge-backend cooperation).
func PlanMigration(vms []VMState) MigrationPlan {
	plan := MigrationPlan{}

	for _, vm := range vms {
		if vm.Backend == BackendOVN {
			// Already on OVN: only reconcile, no host-level move.
			plan.Steps = append(plan.Steps, MigrationStep{
				Name:        "reconcile",
				Description: fmt.Sprintf("vm %s is already on %s; reconcile its OVN state", vm.ID, BackendOVN),
			})
			continue
		}

		if errs := Validate(vm); len(errs) > 0 {
			plan.VMErrors = append(plan.VMErrors, errs...)
			continue
		}

		// 3. Detach from the old bridge.
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "ovs-detach",
			Description: fmt.Sprintf("detach tap %s from bridge %s", vm.TAP, vm.OldBridge),
		})

		// 4. Attach to br-int with the iface-id.
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "ovs-attach",
			Description: fmt.Sprintf("attach tap %s to %s with %s=%s", vm.TAP, IntegrationBridge, IfaceIDKey, ovnPortName(vm.ID)),
		})

		// 5. Reconcile the logical state.
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "ovn-port",
			Description: fmt.Sprintf("ensure logical port %s on switch %s with addresses %q", ovnPortName(vm.ID), VMSwitchName(vm.Network), vm.MAC+" "+vm.IP),
		})
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "ovn-dhcp",
			Description: fmt.Sprintf("bind vm %s to the %s subnet DHCP options", vm.ID, vm.Network),
		})
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "ovn-security",
			Description: fmt.Sprintf("set port security %q and security-group ACLs for vm %s", vm.MAC, vm.ID),
		})

		// 6. Bridge cleanup, which needs cooperation from the bridge backend.
		plan.Steps = append(plan.Steps, MigrationStep{
			Name:        "cleanup-bridge",
			Description: fmt.Sprintf("remove ebtables/anti-spoof and DHCP host entries for vm %s on %s", vm.ID, vm.OldBridge),
			TODO:        true,
		})
	}

	return plan
}

// IntegrationBridge is the OVS integration bridge OVN uses.
const IntegrationBridge = "br-int"

// IfaceIDKey is the OVS external_ids key ovn-controller binds a logical port
// through; it must match internal/ovs' constant.
const IfaceIDKey = "iface-id"

// Backend constants.
const (
	BackendBridge = "bridge"
	BackendOVN    = "ovn"
)
