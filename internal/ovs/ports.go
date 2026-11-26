package ovs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// BridgeExists reports whether an OVS bridge is configured.
func (c *Client) BridgeExists(ctx context.Context, bridge string) (bool, error) {
	if bridge == "" {
		return false, fmt.Errorf("ovs: bridge name must not be empty")
	}
	_, err := c.run(ctx, "br-exists", bridge)
	if err != nil {
		if isExitCode(err, 2) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EnsureBridge creates the bridge when it does not exist yet. Idempotent.
func (c *Client) EnsureBridge(ctx context.Context, bridge string) error {
	if bridge == "" {
		return fmt.Errorf("ovs: bridge name must not be empty")
	}
	exists, err := c.BridgeExists(ctx, bridge)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// --may-exist keeps a concurrent creation from failing.
	if _, err := c.run(ctx, "--may-exist", "add-br", bridge); err != nil {
		return fmt.Errorf("ovs: creating bridge %q: %w", bridge, err)
	}
	return nil
}

// DeleteBridge removes a bridge, ignoring a missing one. Idempotent.
func (c *Client) DeleteBridge(ctx context.Context, bridge string) error {
	if bridge == "" {
		return fmt.Errorf("ovs: bridge name must not be empty")
	}
	if _, err := c.run(ctx, "--if-exists", "del-br", bridge); err != nil {
		return fmt.Errorf("ovs: deleting bridge %q: %w", bridge, err)
	}
	return nil
}

// PortExists reports whether a named port is attached to the bridge.
func (c *Client) PortExists(ctx context.Context, bridge, port string) (bool, error) {
	if bridge == "" || port == "" {
		return false, fmt.Errorf("ovs: bridge and port names are required")
	}
	out, err := c.run(ctx, "list-ports", bridge)
	if err != nil {
		return false, fmt.Errorf("ovs: listing ports of %q: %w", bridge, err)
	}
	for _, name := range splitLines(out) {
		if name == port {
			return true, nil
		}
	}
	return false, nil
}

// ListPorts returns the port names attached to the bridge.
func (c *Client) ListPorts(ctx context.Context, bridge string) ([]string, error) {
	if bridge == "" {
		return nil, fmt.Errorf("ovs: bridge name must not be empty")
	}
	out, err := c.run(ctx, "list-ports", bridge)
	if err != nil {
		return nil, fmt.Errorf("ovs: listing ports of %q: %w", bridge, err)
	}
	return splitLines(out), nil
}

// AttachPort connects an interface to the bridge and tags it for OVN by
// setting the iface-id external_ids key. Idempotent: re-running it against a
// correctly wired port issues only set commands, never a duplicate add.
func (c *Client) AttachPort(ctx context.Context, bridge, port, ifaceID string) error {
	if bridge == "" || port == "" {
		return fmt.Errorf("ovs: bridge and port names are required")
	}
	if ifaceID == "" {
		return fmt.Errorf("ovs: iface-id must not be empty (it is the logical port name)")
	}

	exists, err := c.PortExists(ctx, bridge, port)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := c.run(ctx, "--may-exist", "add-port", bridge, port); err != nil {
			return fmt.Errorf("ovs: attaching %q to %q: %w", port, bridge, err)
		}
	}

	// OVN's ovn-controller binds logical ports through interface external_ids.
	if err := c.setExternalID(ctx, "interface", port, "iface-id", ifaceID); err != nil {
		return err
	}
	if err := c.setExternalID(ctx, "interface", port, ManagedByKey, "true"); err != nil {
		return err
	}
	return nil
}

// DetachPort removes a port from its bridge. Idempotent.
func (c *Client) DetachPort(ctx context.Context, bridge, port string) error {
	if port == "" {
		return fmt.Errorf("ovs: port name must not be empty")
	}
	if _, err := c.run(ctx, "--if-exists", "del-port", bridge, port); err != nil {
		return fmt.Errorf("ovs: detaching %q: %w", port, err)
	}
	return nil
}

// GetExternalID reads one external_ids key from a table row.
// The boolean is false when the key is absent (ovs-vsctl exits with 1).
func (c *Client) GetExternalID(ctx context.Context, table, row, key string) (string, bool, error) {
	if row == "" || key == "" {
		return "", false, fmt.Errorf("ovs: row and key are required")
	}
	out, err := c.run(ctx, "get", table, row, "external_ids:"+key)
	if err != nil {
		if isExitCode(err, 1) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("ovs: reading external_ids:%s of %s %s: %w", key, table, row, err)
	}
	return unquoteRowValue(strings.TrimSpace(out))
}

// unquoteRowValue unwraps the quotes ovs-vsctl prints around string values.
func unquoteRowValue(value string) (string, bool, error) {
	if value == "" {
		return "", false, nil
	}
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return strings.Trim(value, `"`), true, nil
		}
		return unquoted, true, nil
	}
	return value, true, nil
}

// VerifyPort checks that the port is wired exactly as requested and returns a
// descriptive error otherwise. This is the assertion used by callers and tests
// to prove "a VM TAP landed on br-int with the right iface-id".
func (c *Client) VerifyPort(ctx context.Context, bridge, port, ifaceID string) error {
	exists, err := c.PortExists(ctx, bridge, port)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("ovs: port %q is not on bridge %q", port, bridge)
	}
	got, found, err := c.GetExternalID(ctx, "interface", port, InterfaceIDKey)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("ovs: port %q on %q has no %s external_ids key", port, bridge, InterfaceIDKey)
	}
	if got != ifaceID {
		return fmt.Errorf("ovs: port %q on %q has %s=%q, want %q", port, bridge, InterfaceIDKey, got, ifaceID)
	}
	return nil
}

func (c *Client) setExternalID(ctx context.Context, table, row, key, value string) error {
	if _, err := c.run(ctx, "set", table, row, "external_ids:"+key+"="+value); err != nil {
		return fmt.Errorf("ovs: setting %s external_ids:%s on %s %s: %w", table, key, table, row, err)
	}
	return nil
}

// isExitCode reports whether err carries the given ovs-vsctl exit status.
func isExitCode(err error, code int) bool {
	var exitErr *ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func splitLines(out string) []string {
	var result []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// BridgeInfo describes an OVS bridge.
type BridgeInfo struct {
	// Name is the bridge's name.
	Name string
	// Ports are the ports attached to the bridge.
	Ports []string
}

// GetBridgeInfo reads a bridge and its ports. It is the OVS equivalent of the
// bridge backend's bridge introspection, so the generic NetworkStatus can be
// filled from either backend.
func (c *Client) GetBridgeInfo(ctx context.Context, bridge string) (*BridgeInfo, error) {
	if bridge == "" {
		return nil, fmt.Errorf("ovs: bridge name must not be empty")
	}
	exists, err := c.BridgeExists(ctx, bridge)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("ovs: bridge %q does not exist", bridge)
	}
	ports, err := c.ListPorts(ctx, bridge)
	if err != nil {
		return nil, err
	}
	return &BridgeInfo{Name: bridge, Ports: ports}, nil
}
