package firecracker

import (
	"context"
	"strings"
	"testing"

	"github.com/haioco/vmlet/internal/models"
)

// A pre-wired VM (one adopted from a host whose unit creates its own TAP on a
// bridge holding public ranges) must be invisible to vmlet's networking layer:
// configuring it would shadow the device the guest already uses, and cleaning it
// up would delete a device that is still in use.
func TestPrewiredVMsAreInvisibleToNetworking(t *testing.T) {
	nm, err := NewNetworkManager()
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	const vmID = "vm-prewired"

	if err := nm.CleanupNetwork(context.Background(), vmID, models.NetworkModePreWired); err != nil {
		t.Errorf("CleanupNetwork(prewired) = %v, want a no-op: the TAP is the host's", err)
	}

	_, err = nm.ConfigureNetwork(context.Background(), vmID, models.NetworkModePreWired,
		"", "", "", "", "", 0, "")
	if err == nil {
		t.Fatal("ConfigureNetwork(prewired) succeeded; it must refuse rather than create a second device")
	}
	if !strings.Contains(err.Error(), "pre-wired") {
		t.Errorf("error = %v, want it to explain the VM is pre-wired", err)
	}
}
