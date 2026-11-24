package ovn

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// NBctlBackend applies topology changes to the OVN Northbound DB by shelling
// out to ovn-nbctl. It is the fallback for hosts where the libovsdb JSON-RPC
// endpoint is not reachable, and a simple reference implementation of what
// Client.ReconcileTopology does over the OVSDB protocol.
type NBctlBackend struct {
	bin string
	// db is the "db=..." argument passed to ovn-nbctl (empty = default socket).
	db string
	// mu serializes the shell-outs.
	mu sync.Mutex
}

// NewNBctlBackend builds a backend using the given binary and database
// (both may be empty to use "ovn-nbctl" and the local default socket).
func NewNBctlBackend(bin, db string) (*NBctlBackend, error) {
	if bin == "" {
		bin = "ovn-nbctl"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("ovn: %s is not available: %w", bin, err)
	}
	return &NBctlBackend{bin: bin, db: db}, nil
}

// EnsureLogicalSwitch creates the logical switch when missing.
func (b *NBctlBackend) EnsureLogicalSwitch(ctx context.Context, name string, otherConfig, externalIDs map[string]string) error {
	if err := b.run(ctx, "ls-add", name); err != nil {
		return err
	}
	for k, v := range otherConfig {
		if err := b.run(ctx, "set", "Logical_Switch", name,
			fmt.Sprintf("other_config:%s=%s", k, v)); err != nil {
			return err
		}
	}
	for k, v := range externalIDs {
		if err := b.run(ctx, "set", "Logical_Switch", name,
			fmt.Sprintf("external_ids:%s=%s", k, v)); err != nil {
			return err
		}
	}
	return nil
}

// EnsureLogicalSwitchPort binds a port to a logical switch.
func (b *NBctlBackend) EnsureLogicalSwitchPort(ctx context.Context, switchName string, port LogicalPortSpec) error {
	addr := strings.Join(port.Addresses, " ")
	if addr == "" {
		addr = "dynamic"
	}
	if port.Type == "" {
		return b.runf(ctx, "lsp-add %s %s %s", switchName, port.Name, addr)
	}
	return b.runf(ctx, "lsp-add %s %s %s %s", switchName, port.Name, addr, port.Type)
}

// DeleteLogicalSwitchPort removes a port from its logical switch.
func (b *NBctlBackend) DeleteLogicalSwitchPort(ctx context.Context, portName string) error {
	return b.run(ctx, "lsp-del", portName)
}

// EnsureLogicalRouter creates the logical router when missing.
func (b *NBctlBackend) EnsureLogicalRouter(ctx context.Context, name string, otherConfig, externalIDs map[string]string) error {
	if err := b.run(ctx, "lr-add", name); err != nil {
		return err
	}
	for k, v := range otherConfig {
		if err := b.run(ctx, "set", "Logical_Router", name,
			fmt.Sprintf("other_config:%s=%s", k, v)); err != nil {
			return err
		}
	}
	for k, v := range externalIDs {
		if err := b.run(ctx, "set", "Logical_Router", name,
			fmt.Sprintf("external_ids:%s=%s", k, v)); err != nil {
			return err
		}
	}
	return nil
}

func (b *NBctlBackend) run(ctx context.Context, args ...string) error {
	return b.runf(ctx, "%s", strings.Join(args, " "))
}

// runf builds an ovn-nbctl command from a format string. The whole command
// stays a single argv entry, which ovn-nbctl splits itself.
func (b *NBctlBackend) runf(ctx context.Context, format string, args ...interface{}) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	full := b.bin
	if b.db != "" {
		full = fmt.Sprintf("%s --db=%s", full, b.db)
	}
	cmdline := fmt.Sprintf(format, args...)
	full = full + " " + cmdline

	cmd := exec.CommandContext(ctx, "sh", "-c", full)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ovn: %q failed: %w: %s", full, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
