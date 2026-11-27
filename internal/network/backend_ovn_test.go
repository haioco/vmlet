package network

import (
	"testing"

	"github.com/haioco/vmlet/internal/ovn"
)

func TestNewOVNBackend_ValidatesConfig(t *testing.T) {
	if _, err := NewOVNBackend(nil); err == nil {
		t.Fatal("NewOVNBackend accepted a nil config")
	}

	backend, err := NewOVNBackend(&NetworkConfig{BridgeName: "br-int", BridgeCIDR: "10.0.0.0/24"})
	if err != nil {
		t.Fatalf("NewOVNBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("NewOVNBackend returned nil")
	}
}

func TestOVNBackend_RequiresInitialisation(t *testing.T) {
	// An uninitialised backend must say so, not nil-pointer.
	backend := &OVNBackend{vmPorts: map[string]string{}}

	if _, err := backend.AllocateVMNetwork("vm-1", "02:fc:00:00:00:01"); err == nil {
		t.Error("AllocateVMNetwork should fail before Initialize")
	}
	if err := backend.ValidateNetworkSetup(); err == nil {
		t.Error("ValidateNetworkSetup should fail before Initialize")
	}
	if _, err := backend.GetNetworkStatus(); err == nil {
		t.Error("GetNetworkStatus should fail before Initialize")
	}
	if err := backend.TestVMConnectivity("vm-1"); err == nil {
		t.Error("TestVMConnectivity should fail before Initialize")
	}
	if _, err := backend.GetVMNetworkInfo("vm-1"); err == nil {
		t.Error("GetVMNetworkInfo should fail before Initialize")
	}
	if err := backend.Shutdown(); err != nil {
		t.Errorf("Shutdown on an uninitialised backend should be a no-op: %v", err)
	}
}

func TestOVNBackend_RejectsEmptyVMID(t *testing.T) {
	backend := &OVNBackend{vmPorts: map[string]string{}}
	if _, err := backend.AllocateVMNetwork("", "02:fc:00:00:00:01"); err == nil {
		t.Fatal("AllocateVMNetwork accepted an empty VM id")
	}
}

func TestOVNBackend_SubnetDerivation(t *testing.T) {
	cases := []struct {
		name        string
		config      NetworkConfig
		wantSubnet  string
		wantGateway string
	}{
		{
			name:        "explicit cidr and ip",
			config:      NetworkConfig{BridgeName: "br-int", BridgeCIDR: "10.1.0.0/24", BridgeIP: "10.1.0.254"},
			wantSubnet:  "10.1.0.0/24",
			wantGateway: "10.1.0.254",
		},
		{
			name:        "gateway derived from the subnet",
			config:      NetworkConfig{BridgeName: "br-int", BridgeCIDR: "10.2.0.0/24"},
			wantSubnet:  "10.2.0.0/24",
			wantGateway: "10.2.0.1",
		},
		{
			name:        "no network config falls back to defaults",
			config:      NetworkConfig{},
			wantSubnet:  "10.0.0.0/24",
			wantGateway: "10.0.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &OVNBackend{config: &tc.config, vmPorts: map[string]string{}}
			subnet, gateway, err := b.subnet()
			if err != nil {
				t.Fatalf("subnet: %v", err)
			}
			if subnet != tc.wantSubnet {
				t.Errorf("subnet = %q, want %q", subnet, tc.wantSubnet)
			}
			if gateway != tc.wantGateway {
				t.Errorf("gateway = %q, want %q", gateway, tc.wantGateway)
			}
		})
	}
}

func TestOVNBackend_SubnetRejectsBadCIDR(t *testing.T) {
	b := &OVNBackend{
		config:  &NetworkConfig{BridgeName: "br-int", BridgeCIDR: "not-a-cidr"},
		vmPorts: map[string]string{},
	}
	if _, _, err := b.subnet(); err == nil {
		t.Fatal("subnet accepted an invalid CIDR")
	}
}

func TestOVNBackend_NetworkNameDefaults(t *testing.T) {
	b := &OVNBackend{vmPorts: map[string]string{}}
	if got := b.networkName(); got != "default" {
		t.Errorf("networkName() = %q, want \"default\"", got)
	}

	b.network = "alpha"
	if got := b.networkName(); got != "alpha" {
		t.Errorf("networkName() = %q, want alpha", got)
	}
}

func TestOVNBackend_BridgeDefaultsToBrInt(t *testing.T) {
	b := &OVNBackend{config: &NetworkConfig{}, vmPorts: map[string]string{}}
	if got := b.integrationBridge(); got != "br-int" {
		t.Errorf("integrationBridge() = %q, want br-int", got)
	}

	b.config = &NetworkConfig{BridgeName: "custom-br"}
	if got := b.integrationBridge(); got != "custom-br" {
		t.Errorf("integrationBridge() = %q, want custom-br", got)
	}
}

func TestOVNBackend_RegisteredAsSelectable(t *testing.T) {
	// The registry must resolve "ovn", which is the whole point of item 1.1.
	backend, err := NewBackend(&NetworkConfig{BridgeCIDR: "10.0.0.0/24"}, BackendOVN)
	if err != nil {
		t.Fatalf("NewBackend(ovn): %v", err)
	}
	if _, ok := backend.(*OVNBackend); !ok {
		t.Fatalf("factory returned %T, want *OVNBackend", backend)
	}
}

func TestOVNBackend_SatisfiesInterface(t *testing.T) {
	var _ Backend = (*OVNBackend)(nil)
}

func TestOVNEndpointDefault(t *testing.T) {
	if got := ovn.DefaultNBEndpoint; got != "unix:/var/run/ovn/ovnnb_db.sock" {
		t.Errorf("DefaultNBEndpoint = %q", got)
	}
}

func TestOVNBackend_FloatingIPValidation(t *testing.T) {
	b := &OVNBackend{vmPorts: map[string]string{}, vmFloating: map[string]map[string]string{}, vmMACs: map[string]string{}, vmAddresses: map[string]string{}}

	if err := b.AttachFloatingIP("", "1.2.3.4"); err == nil {
		t.Error("AttachFloatingIP accepted an empty VM id")
	}
	if err := b.AttachFloatingIP("vm-1", ""); err == nil {
		t.Error("AttachFloatingIP accepted an empty address")
	}
	// No OVN port means the VM was never allocated.
	if err := b.AttachFloatingIP("vm-missing", "1.2.3.4"); err == nil {
		t.Error("AttachFloatingIP accepted a VM with no OVN port")
	}
	if got := b.ListFloatingIPs("vm-1"); len(got) != 0 {
		t.Errorf("ListFloatingIPs on a fresh backend = %v, want empty", got)
	}
	// Detach of an unheld address is a no-op, not an error.
	if err := b.DetachFloatingIP("vm-1", "1.2.3.4"); err != nil {
		t.Errorf("DetachFloatingIP of an unheld address: %v", err)
	}
}

func TestOVNBackend_FloatingSpecsUseOVNNames(t *testing.T) {
	b := &OVNBackend{
		config:      &NetworkConfig{BridgeName: "br-int", BridgeCIDR: "10.0.0.0/24"},
		network:     "alpha",
		vmPorts:     map[string]string{},
		vmFloating:  map[string]map[string]string{"vm-1": {"1.2.3.4": "10.0.0.10"}},
		vmMACs:      map[string]string{},
		vmAddresses: map[string]string{},
	}

	specs := b.floatingSpecs("", "")
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	got := specs[0]
	if got.ExternalIP != "1.2.3.4" || got.InternalIP != "10.0.0.10" {
		t.Errorf("spec addresses = %q -> %q, want 1.2.3.4 -> 10.0.0.10", got.ExternalIP, got.InternalIP)
	}
	// The DNAT must land on the network's logical router, not a hardcoded one.
	if got.Router != ovn.GatewayRouterName("alpha") {
		t.Errorf("router = %q, want %q", got.Router, ovn.GatewayRouterName("alpha"))
	}
}

func TestOVNBackend_ListFloatingIsACopy(t *testing.T) {
	b := &OVNBackend{
		vmPorts: map[string]string{}, vmFloating: map[string]map[string]string{"vm-1": {"1.2.3.4": "10.0.0.10"}},
		vmMACs: map[string]string{}, vmAddresses: map[string]string{},
	}
	got := b.ListFloatingIPs("vm-1")
	got["mutated"] = "x"
	if _, leaked := b.vmFloating["vm-1"]["mutated"]; leaked {
		t.Error("ListFloatingIPs handed out its internal map")
	}
}
