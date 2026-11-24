package ovs

import (
	"context"
	"testing"
)

// bridgeExistsIn reports whether the fake has the bridge configured.
func (c *Client) bridgeExistsIn(f *fakeOVS, bridge string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.bridges[bridge]
	return ok
}

func TestEnsureBridge(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if !c.bridgeExistsIn(f, "br-int") {
		t.Fatal("br-int was not created")
	}

	// Second call must not run add-br again.
	before := f.countContains("br-exists")
	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge (repeat): %v", err)
	}
	if created := f.countContains("add-br"); created != 1 {
		t.Fatalf("add-br ran %d times, want 1", created)
	}
	if f.countContains("br-exists") <= before {
		t.Fatal("expected br-exists probes on the repeat call")
	}

	if _, err := c.BridgeExists(ctx, ""); err == nil {
		t.Fatal("BridgeExists accepted an empty bridge name")
	}
}

func TestAttachPort_WiresIfaceID(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1"); err != nil {
		t.Fatalf("AttachPort: %v", err)
	}

	// The acceptance criterion: the TAP is on br-int with the right iface-id.
	if err := c.VerifyPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1"); err != nil {
		t.Fatalf("VerifyPort: %v", err)
	}

	ports, err := c.ListPorts(ctx, "br-int")
	if err != nil {
		t.Fatalf("ListPorts: %v", err)
	}
	if len(ports) != 1 || ports[0] != "fc-tap-ab12" {
		t.Fatalf("ListPorts = %v, want [fc-tap-ab12]", ports)
	}

	id, found, err := c.GetExternalID(ctx, "interface", "fc-tap-ab12", InterfaceIDKey)
	if err != nil || !found {
		t.Fatalf("GetExternalID(iface-id) = (%q, %v, %v)", id, found, err)
	}
	if id != "lsp-vm-1" {
		t.Fatalf("iface-id = %q, want %q", id, "lsp-vm-1")
	}
}

func TestAttachPort_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1"); err != nil {
		t.Fatalf("AttachPort (first): %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1"); err != nil {
		t.Fatalf("AttachPort (second): %v", err)
	}

	if got := f.countContains("add-port"); got != 1 {
		t.Fatalf("add-port ran %d times, want 1", got)
	}
	ports, err := c.ListPorts(ctx, "br-int")
	if err != nil {
		t.Fatalf("ListPorts: %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("ports = %v, want exactly one entry", ports)
	}
}

func TestAttachPort_MovesIfaceID(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)
	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}

	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-old"); err != nil {
		t.Fatalf("AttachPort(stale): %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-new"); err != nil {
		t.Fatalf("AttachPort(stale): %v", err)
	}
	if err := c.VerifyPort(ctx, "br-int", "fc-tap-ab12", "lsp-new"); err != nil {
		t.Fatalf("VerifyPort after rebind: %v", err)
	}
	if f.countContains("add-port") != 1 {
		t.Fatalf("add-port ran %d times, want 1 (port already present)", f.countContains("add-port"))
	}
}

func TestDetachPort(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)
	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1"); err != nil {
		t.Fatalf("AttachPort: %v", err)
	}

	if err := c.DetachPort(ctx, "br-int", "fc-tap-ab12"); err != nil {
		t.Fatalf("DetachPort: %v", err)
	}
	exists, err := c.PortExists(ctx, "br-int", "fc-tap-ab12")
	if err != nil {
		t.Fatalf("PortExists: %v", err)
	}
	if exists {
		t.Fatal("port still present after DetachPort")
	}

	// Detaching again must be a no-op.
	if err := c.DetachPort(ctx, "br-int", "fc-tap-ab12"); err != nil {
		t.Fatalf("DetachPort (repeat): %v", err)
	}
}

func TestVerifyPort_ReportsMismatch(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)
	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-actual"); err != nil {
		t.Fatalf("AttachPort: %v", err)
	}

	if err := c.VerifyPort(ctx, "br-int", "fc-tap-ab12", "lsp-expected"); err == nil {
		t.Fatal("VerifyPort accepted a mismatched iface-id")
	}
	if err := c.VerifyPort(ctx, "br-int", "fc-tap-missing", "lsp-x"); err == nil {
		t.Fatal("VerifyPort accepted a missing port")
	}
}

func TestGetExternalID_MissingKey(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	_, found, err := c.GetExternalID(ctx, "interface", "fc-tap-ab12", InterfaceIDKey)
	if err != nil {
		t.Fatalf("GetExternalID: %v", err)
	}
	if found {
		t.Fatal("expected not-found for a key that does not exist")
	}
}

func TestAttachPort_PropagatesCommandErrors(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)
	if err := c.EnsureBridge(ctx, "br-int"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}

	f.failNext("add-port", 1)
	err := c.AttachPort(ctx, "br-int", "fc-tap-ab12", "lsp-vm-1")
	if err == nil {
		t.Fatal("AttachPort ignored a failing add-port")
	}
}

func TestNewClientValidatesBinary(t *testing.T) {
	if _, err := NewClientWithBinary("   "); err == nil {
		t.Fatal("NewClientWithBinary accepted an empty binary")
	}
	if _, err := NewClientWithBinary("ovs-vsctl-does-not-exist"); err == nil {
		t.Fatal("NewClientWithBinary accepted a missing binary")
	}
	c := NewClientWithRunner("", nil)
	if c.Binary() != DefaultBinary {
		t.Fatalf("Binary() = %q, want the default %q", c.Binary(), DefaultBinary)
	}
}
