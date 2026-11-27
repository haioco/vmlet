package network

import (
	"context"
	"os"
	"testing"

	"github.com/haioco/vmlet/internal/ovn"
)

// TestOVNBackend_LiveNorthboundDB exercises the whole OVN control path against a
// real Northbound database rather than the in-memory test server, because the
// in-memory one never persists ACL rows and so cannot prove they are installed.
//
// It is opt-in and skipped by default:
//
//	MICROVM_OVN_LIVE_NB=unix:/var/run/ovn/ovnnb_db.sock \
//	  go test ./internal/network/ -run LiveNorthboundDB -v
//
// The test mutates the database: it allocates ports, ACLs and NAT rows on the
// host's vmlet-managed networks, and removes them again at the end. It is meant
// for a host you are willing to reconcile, not one serving real VMs.
func TestOVNBackend_LiveNorthboundDB(t *testing.T) {
	endpoint := os.Getenv("MICROVM_OVN_LIVE_NB")
	if endpoint == "" {
		t.Skip("set MICROVM_OVN_LIVE_NB to run against a real Northbound DB")
	}
	if os.Getenv("MICROVM_OVN_LIVE_CONFIRM") != "yes" {
		t.Skip("set MICROVM_OVN_LIVE_CONFIRM=yes: this writes to that database")
	}

	// The live host serves the "default" network on 10.130.146.0/24. Read the
	// same defaults the flags supply so nothing here is guessed.
	const (
		vmA    = "vm-live-a"
		vmB    = "vm-live-b"
		macA   = "02:fc:00:00:00:0a"
		macB   = "02:fc:00:00:00:0b"
		floatA = "203.0.113.10"
		floatB = "203.0.113.11"
	)

	t.Setenv("MICROVM_OVN_NB_ENDPOINT", endpoint)
	t.Setenv("MICROVM_ANTISPOOF_IP_ENABLE", "true")

	b, err := NewOVNBackend(&NetworkConfig{
		Backend:    BackendOVN,
		BridgeName: IntegrationBridge,
		BridgeCIDR: "10.130.146.0/24",
		BridgeIP:   "10.130.146.1",
	})
	if err != nil {
		t.Fatalf("NewOVNBackend: %v", err)
	}
	ob, ok := b.(*OVNBackend)
	if !ok {
		t.Fatalf("NewOVNBackend returned %T, want *OVNBackend", b)
	}
	if err := b.Initialize(); err != nil {
		t.Fatalf("Initialize against a live NB DB: %v", err)
	}
	defer func() {
		for _, id := range []string{vmA, vmB} {
			if err := b.ReleaseVMNetwork(id, ""); err != nil {
				t.Errorf("cleanup: ReleaseVMNetwork(%s): %v", id, err)
			}
		}
	}()

	// --- Allocation: port, DHCP reservation and ACL rows. ---
	infoA, err := b.AllocateVMNetwork(vmA, macA, "")
	if err != nil {
		t.Fatalf("AllocateVMNetwork(%s): %v", vmA, err)
	}
	if infoA.IPAddress == "" {
		t.Fatalf("AllocateVMNetwork(%s) returned no address", vmA)
	}
	t.Logf("%s: mac=%s ip=%s", vmA, infoA.MACAddress, infoA.IPAddress)

	acls, err := ob.ovnACLs()
	if err != nil {
		t.Fatalf("listing ACLs: %v", err)
	}
	mine := 0
	for _, row := range acls {
		if row.ExternalIDs["vmlet-vm"] == vmA {
			mine++
		}
	}
	if mine == 0 {
		t.Fatalf("no ACL rows bound to %s; foreach over DefaultVMRules produced %d rows in total", vmA, len(acls))
	}
	t.Logf("ACLs bound to %s: %d (of %d rows on the switch)", vmA, mine, len(acls))

	// --- Floating IP: DNAT row on the network's router. ---
	if err := ob.AttachFloatingIP(vmA, floatA); err != nil {
		t.Fatalf("AttachFloatingIP(%s, %s): %v", vmA, floatA, err)
	}
	nats, err := ob.ovnNATs()
	if err != nil {
		t.Fatalf("listing NAT rows: %v", err)
	}
	found := false
	for _, row := range nats {
		if row.ExternalIP == floatA {
			found = true
			if row.LogicalIP != infoA.IPAddress {
				t.Errorf("floating %s targets %q, want %q", floatA, row.LogicalIP, infoA.IPAddress)
			}
			if row.Type != ovn.NATTypeDNAT {
				t.Errorf("floating %s is %q, want %q", floatA, row.Type, ovn.NATTypeDNAT)
			}
			// The MAC is what makes OVN answer ARP for the external address.
			if !ovnNATCarriesMAC(row, macA) {
				t.Errorf("floating %s does not carry %s", floatA, macA)
			}
		}
	}
	if !found {
		t.Fatalf("no NAT row for %s; attach reconciled but installed nothing", floatA)
	}
	t.Logf("DNAT installed: %s -> %s (mac %s)", floatA, infoA.IPAddress, macA)

	// --- A second VM, so release has something to damage. ---
	infoB, err := b.AllocateVMNetwork(vmB, macB, "")
	if err != nil {
		t.Fatalf("AllocateVMNetwork(%s): %v", vmB, err)
	}
	if infoB.IPAddress == infoA.IPAddress {
		t.Fatalf("%s and %s both drew %s; allocation did not skip the addresses in use", vmA, vmB, infoA.IPAddress)
	}
	if err := ob.AttachFloatingIP(vmB, floatB); err != nil {
		t.Fatalf("AttachFloatingIP(%s, %s): %v", vmB, floatB, err)
	}
	t.Logf("%s: mac=%s ip=%s, floating %s", vmB, infoB.MACAddress, infoB.IPAddress, floatB)

	// --- Releasing one VM must not take the other's rows with it. ---
	if err := b.ReleaseVMNetwork(vmA, ""); err != nil {
		t.Fatalf("ReleaseVMNetwork(%s): %v", vmA, err)
	}

	nats, err = ob.ovnNATs()
	if err != nil {
		t.Fatalf("listing NAT rows after release: %v", err)
	}
	if survived := natExternalIPs(nats, floatB); !survived {
		t.Errorf("releasing %s removed %s's floating IP %s: the release path flushed the router", vmA, vmB, floatB)
	} else {
		t.Logf("floating %s survived the release of %s", floatB, vmA)
	}
	if natExternalIPs(nats, floatA) {
		t.Errorf("floating %s of the released VM %s still present", floatA, vmA)
	}

	acls, err = ob.ovnACLs()
	if err != nil {
		t.Fatalf("listing ACLs after release: %v", err)
	}
	for _, row := range acls {
		if row.ExternalIDs["vmlet-vm"] == vmA {
			t.Errorf("ACL row %s of the released VM still present", row.UUID)
		}
		if row.ExternalIDs["vmlet-vm"] == vmB {
			t.Logf("%s's ACL row %s survived", vmB, row.UUID)
		}
	}
}

// ovnACLs lists the ACL rows in the connected database, through the backend's own
// client so the test observes exactly what vmlet sees.
func (b *OVNBackend) ovnACLs() ([]ovn.ACL, error) {
	var rows []ovn.ACL
	err := b.withClient(func(c *ovn.Client) error {
		var listErr error
		rows, listErr = c.ListACLs(context.Background())
		return listErr
	})
	return rows, err
}

func (b *OVNBackend) ovnNATs() ([]ovn.NAT, error) {
	var rows []ovn.NAT
	err := b.withClient(func(c *ovn.Client) error {
		var listErr error
		rows, listErr = c.ListNATs(context.Background())
		return listErr
	})
	return rows, err
}

// ovnNATCarriesMAC reports whether the row answers ARP for the external address
// with the target VM's hardware address. OVN reads this from external_mac.
func ovnNATCarriesMAC(row ovn.NAT, mac string) bool {
	return row.ExternalMAC != nil && *row.ExternalMAC == mac
}

func natExternalIPs(rows []ovn.NAT, ip string) bool {
	for _, row := range rows {
		if row.ExternalIP == ip {
			return true
		}
	}
	return false
}
