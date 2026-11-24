// Package ovs wraps the Open vSwitch commands needed to plug vmlet's
// Firecracker TAP devices into an OVS bridge. It exists so that the OVN
// network backend (roadmap item 1.2) does not have to shell out to
// ovs-vsctl directly, and so tests can exercise the wiring without OVS.
package ovs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// DefaultBinary is the Open vSwitch management utility.
const DefaultBinary = "ovs-vsctl"

// InterfaceIDKey is the OVS external_ids key OVN controllers use to
// bind an interface to a logical port (iface-id = logical port name).
const InterfaceIDKey = "iface-id"

// ManagedBy is the external_ids key/value marking ports owned by vmlet.
const ManagedByKey = "vmlet-managed"

// CommandRunner executes a management command and returns its stdout.
// It is a field type so tests can substitute a fake executor.
type CommandRunner func(ctx context.Context, binary string, args ...string) (string, error)

// Client talks to the local OVS daemon through ovs-vsctl.
type Client struct {
	binary string
	runner CommandRunner
}

// NewClient creates a Client that shells out to ovs-vsctl on PATH.
func NewClient() (*Client, error) {
	return NewClientWithBinary(DefaultBinary)
}

// NewClientWithBinary creates a Client using a specific binary path.
func NewClientWithBinary(binary string) (*Client, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, fmt.Errorf("ovs: binary must not be empty")
	}
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("ovs: %s is not available: %w", binary, err)
	}
	return &Client{binary: binary, runner: execRunner}, nil
}

// NewClientWithRunner creates a Client with a custom command runner, which is
// how tests avoid depending on a real OVS installation.
func NewClientWithRunner(binary string, runner CommandRunner) *Client {
	if strings.TrimSpace(binary) == "" {
		binary = DefaultBinary
	}
	if runner == nil {
		runner = execRunner
	}
	return &Client{binary: binary, runner: runner}
}

// Binary returns the management binary this client shells out to.
func (c *Client) Binary() string { return c.binary }

// ExitError reports that the ovs-vsctl command ran but exited non-zero.
// Callers use ExitCode() to distinguish "not found" (code 1/2) from a real
// failure.
type ExitError struct {
	Binary string
	Args   []string
	Code   int
	Stderr string
	Err    error
}

func (e *ExitError) Error() string {
	joined := strings.Join(e.Args, " ")
	if e.Stderr != "" {
		return fmt.Sprintf("ovs: %s %s: exit %d: %s", e.Binary, joined, e.Code, e.Stderr)
	}
	return fmt.Sprintf("ovs: %s %s: exit %d", e.Binary, joined, e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit status.
func (e *ExitError) ExitCode() int { return e.Code }

func execRunner(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	exitErr := &ExitError{Binary: binary, Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exitErr.Code = ee.ExitCode()
	} else {
		exitErr.Code = -1
	}
	return "", exitErr
}

// run forwards a command to the injected runner.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	return c.runner(ctx, c.binary, args...)
}
