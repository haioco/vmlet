package ovn

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := VMState{ID: "vm-1", Network: "alpha", MAC: "02:fc:00:00:00:01", TAP: "fc-tap-ab12", OldBridge: "lxdbr0"}
	if errs := Validate(valid); len(errs) != 0 {
		t.Fatalf("Validate(valid) = %v, want none", errs)
	}

	for _, tc := range []struct {
		name    string
		vm      VMState
		wantMsg string
	}{
		{"missing id", VMState{Network: "a", MAC: "m", TAP: "t", OldBridge: "b"}, "vm id is required"},
		{"missing tap", VMState{ID: "vm-1", Network: "a", MAC: "m", OldBridge: "b"}, "tap device is required"},
		{"missing bridge", VMState{ID: "vm-1", Network: "a", MAC: "m", TAP: "t"}, "current bridge is required"},
		{"missing network", VMState{ID: "vm-1", MAC: "m", TAP: "t", OldBridge: "b"}, "network is required"},
		{"missing mac", VMState{ID: "vm-1", Network: "a", TAP: "t", OldBridge: "b"}, "MAC address is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate(tc.vm)
			if len(errs) == 0 {
				t.Fatal("Validate accepted an invalid state")
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.ValidationError(), tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("errors = %v, want one mentioning %q", errs, tc.wantMsg)
			}
		})
	}
}

func TestPlanMigration_OrderIsSafe(t *testing.T) {
	vm := VMState{
		ID:        "vm-1",
		Network:   "alpha",
		MAC:       "02:fc:00:00:00:01",
		IP:        "10.0.0.10",
		Backend:   BackendBridge,
		TAP:       "fc-tap-ab12",
		OldBridge: "lxdbr0",
	}
	plan := PlanMigration([]VMState{vm})

	if plan.Empty() {
		t.Fatal("plan is empty")
	}
	if len(plan.VMErrors) != 0 {
		t.Fatalf("unexpected VM errors: %v", plan.VMErrors)
	}

	order := make([]string, len(plan.Steps))
	for i, step := range plan.Steps {
		order[i] = step.Name
	}
	want := []string{"ovs-detach", "ovs-attach", "ovn-port", "ovn-dhcp", "ovn-security", "cleanup-bridge"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("step order =\n  %v\nwant\n  %v", order, want)
	}

	// The tap must come off the old bridge before it appears on br-int,
	// otherwise the guest would briefly be on two networks.
	detach := indexOf(order, "ovs-detach")
	attach := indexOf(order, "ovs-attach")
	if detach < 0 || attach < 0 || detach > attach {
		t.Fatalf("ovs-detach (idx %d) must precede ovs-attach (idx %d)", detach, attach)
	}

	// The logical port must exist before DHCP/security bind to it.
	port := indexOf(order, "ovn-port")
	dhcp := indexOf(order, "ovn-dhcp")
	if port > dhcp {
		t.Fatal("ovn-port must precede ovn-dhcp")
	}

	// Bridge cleanup is last and is the only TODO step: the ebtables rules are
	// owned by the bridge backend, which vmlet does not own.
	if last := order[len(order)-1]; last != "cleanup-bridge" {
		t.Errorf("last step = %q, want cleanup-bridge", last)
	}
	todoCount := 0
	for _, step := range plan.Steps {
		if step.TODO {
			todoCount++
		}
	}
	if todoCount != 1 {
		t.Errorf("TODO steps = %d, want exactly 1 (cleanup-bridge)", todoCount)
	}
}

func TestPlanMigration_KeepsIdentity(t *testing.T) {
	vm := VMState{
		ID: "vm-1", Network: "alpha", MAC: "02:fc:00:00:00:01", IP: "10.0.0.10",
		Backend: BackendBridge, TAP: "fc-tap-ab12", OldBridge: "lxdbr0",
	}
	plan := PlanMigration([]VMState{vm})
	described := plan.Describe()

	// The port name and the switch the VM lands on must stay the same as a
	// fresh OVN create of that VM, so an IP change never happens.
	if !strings.Contains(described, ovnPortName("vm-1")) {
		t.Errorf("plan does not use the stable port name %q:\n%s", ovnPortName("vm-1"), described)
	}
	if !strings.Contains(described, VMSwitchName("alpha")) {
		t.Errorf("plan does not name the VM's switch %q:\n%s", VMSwitchName("alpha"), described)
	}
	if !strings.Contains(described, IfaceIDKey) {
		t.Errorf("plan does not set the iface-id binding:\n%s", described)
	}
}

func TestPlanMigration_AlreadyOnOVN(t *testing.T) {
	vm := VMState{
		ID: "vm-1", Network: "alpha", MAC: "02:fc:00:00:00:01", IP: "10.0.0.10",
		Backend: BackendOVN, TAP: "fc-tap-ab12", OldBridge: "",
	}
	plan := PlanMigration([]VMState{vm})

	if len(plan.Steps) != 1 || plan.Steps[0].Name != "reconcile" {
		t.Fatalf("steps = %+v, want a single reconcile step", plan.Steps)
	}
	if len(plan.VMErrors) != 0 {
		t.Fatalf("unexpected errors: %v", plan.VMErrors)
	}
}

func TestPlanMigration_RefusesUnmigratableVMs(t *testing.T) {
	plan := PlanMigration([]VMState{
		{ID: "vm-ok", Network: "alpha", MAC: "02:fc:00:00:00:01", TAP: "fc-tap-ab12", OldBridge: "lxdbr0"},
		{ID: "vm-bad", Network: "alpha", MAC: "02:fc:00:00:00:02", OldBridge: "lxdbr0"},
	})

	if len(plan.VMErrors) != 1 {
		t.Fatalf("VM errors = %d, want 1", len(plan.VMErrors))
	}
	if plan.VMErrors[0].VMID != "vm-bad" {
		t.Errorf("error VM = %q, want vm-bad", plan.VMErrors[0].VMID)
	}

	// The good VM is still planned; the bad one is skipped entirely rather than
	// being half-migrated.
	names := map[string]bool{}
	for _, step := range plan.Steps {
		names[step.Name] = true
	}
	if !names["ovs-attach"] {
		t.Error("the migratable VM was not planned")
	}
	for _, step := range plan.Steps {
		if strings.Contains(step.Description, "vm-bad") {
			t.Errorf("the unmigratable VM was planned: %q", step.Description)
		}
	}
}

func TestPlanMigration_EmptyInput(t *testing.T) {
	plan := PlanMigration(nil)
	if !plan.Empty() {
		t.Errorf("plan should be empty, got %q", plan.Describe())
	}
}

func TestPlanDescribe(t *testing.T) {
	plan := PlanMigration([]VMState{
		{ID: "vm-1", Network: "alpha", MAC: "02:fc:00:00:00:01", IP: "10.0.0.10",
			Backend: BackendBridge, TAP: "fc-tap-ab12", OldBridge: "lxdbr0"},
	})
	rendered := plan.Describe()
	// Steps are numbered, the TODO step is marked, and errors appear.
	if !strings.Contains(rendered, "1. [ ] ovs-detach") {
		t.Errorf("rendering missing the first step:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[!]") {
		t.Errorf("TODO step not marked:\n%s", rendered)
	}
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}
