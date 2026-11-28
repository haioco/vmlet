package firecracker

import (
	"testing"

	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/network"
)

func TestCheckNetworkMode(t *testing.T) {
	bridge := &Manager{} // no OVN overlay
	if err := bridge.CheckNetworkMode(models.NetworkModeOVN); err == nil {
		t.Error("CheckNetworkMode(ovn) accepted a host with no overlay")
	}
	for _, m := range []models.NetworkMode{models.NetworkModeNAT, models.NetworkModeDHCP, models.NetworkModeMACVLAN, models.NetworkModeIPv6Only} {
		if err := bridge.CheckNetworkMode(m); err != nil {
			t.Errorf("CheckNetworkMode(%s) = %v, want nil", m, err)
		}
	}
}

func TestSupportedAndDefaultNetworkModes(t *testing.T) {
	bridge := &Manager{}
	modes := bridge.SupportedNetworkModes()
	if bridge.DefaultNetworkMode() != models.NetworkModeNAT {
		t.Errorf("DefaultNetworkMode on a bridge host = %s, want nat", bridge.DefaultNetworkMode())
	}
	for _, m := range modes {
		if m == string(models.NetworkModeOVN) {
			t.Fatal("SupportedNetworkModes advertised ovn on a host with no overlay")
		}
	}

	ovnBackend, err := network.NewOVNBackend(&network.NetworkConfig{})
	if err != nil {
		t.Fatalf("NewOVNBackend: %v", err)
	}
	ovn := &Manager{enhancedNetwork: ovnBackend, ovnNetwork: ovnBackend.(*network.OVNBackend)}
	if err := ovn.CheckNetworkMode(models.NetworkModeOVN); err != nil {
		t.Errorf("CheckNetworkMode(ovn) on an overlay host = %v, want nil", err)
	}
	if ovn.DefaultNetworkMode() != models.NetworkModeOVN {
		t.Errorf("DefaultNetworkMode on an OVN host = %s, want ovn", ovn.DefaultNetworkMode())
	}
	if !containsString(ovn.SupportedNetworkModes(), string(models.NetworkModeOVN)) {
		t.Errorf("SupportedNetworkModes on an OVN host = %v, want ovn included", ovn.SupportedNetworkModes())
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
