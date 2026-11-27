package firecracker

import (
	"strings"
	"testing"
)

// TestOVNTAPNameCollides is the regression test for two VMs whose ids share a
// prefix silently getting the same TAP device.
func TestOVNTAPNameIsUniquePerID(t *testing.T) {
	seen := map[string]string{}
	for _, id := range []string{
		"ubuntu-ovn-1", "ubuntu-ovn-2", "ubuntu-ovn-10",
		"vm-a", "vm-b", "vm-abcd", "with spaces", "unicode-\u00e9name",
		strings.Repeat("x", 200), "a",
	} {
		name := ovnTAPName(id)
		if len(name) > 15 {
			t.Errorf("ovnTAPName(%q) = %q is %d chars, over the IFNAMSIZ limit of 15", id, name, len(name))
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("ovnTAPName(%q) and ovnTAPName(%q) both produced %q", id, prev, name)
		}
		seen[name] = id
	}
}

// TestOVNTAPNameIsStable matters because the iface-id binding and the recorded
// TAPDevice must agree across restarts.
func TestOVNTAPNameIsStable(t *testing.T) {
	if ovnTAPName("ubuntu-ovn-1") != ovnTAPName("ubuntu-ovn-1") {
		t.Error("ovnTAPName is not deterministic")
	}
}
