package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// SnapshotClient talks to a running Firecracker's API socket for the snapshot
// operations needed by migration. It deliberately does NOT use the SDK: the
// calls are few and the socket path layout is node-local.
type SnapshotClient struct {
	socketPath string
	client     *http.Client
}

// NewSnapshotClient builds a client for the given Firecracker API unix socket.
func NewSnapshotClient(socketPath string) *SnapshotClient {
	return &SnapshotClient{
		socketPath: socketPath,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 5 * time.Minute, // full snapshot of a large VM can take a while
		},
	}
}

func (c *SnapshotClient) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("firecracker API %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(out), 512))
	}
	return out, nil
}

// InstanceInfo returns the running instance description (state, vmm version).
func (c *SnapshotClient) InstanceInfo(ctx context.Context) (map[string]interface{}, error) {
	out, err := c.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// CreateSnapshotParams is the body of PUT /snapshot/create.
// FullSnapshot writes vm_state_file + mem_file_path; DiffSnapshot appends to
// the mem_file and writes only the vm_state file. The VM is paused while the
// snapshot is being taken and is resumed automatically when the call returns.
type CreateSnapshotParams struct {
	SnapshotType string `json:"snapshot_type"` // "Full" | "Diff"
	SnapshotPath string `json:"snapshot_path"` // vm_state file
	MemFilePath  string `json:"mem_file_path"`
	Version      uint16 `json:"version,omitempty"`
}

// Pause freezes the guest vCPUs (required before CreateSnapshot in v1.13).
func (c *SnapshotClient) Pause(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
	return err
}

// Resume releases the guest vCPUs after a snapshot or between diff rounds.
func (c *SnapshotClient) Resume(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
	return err
}

// CreateSnapshot takes a full or diff snapshot of the running VM.
func (c *SnapshotClient) CreateSnapshot(ctx context.Context, params *CreateSnapshotParams) error {
	_, err := c.do(ctx, http.MethodPut, "/snapshot/create", params)
	return err
}

// LoadSnapshotParams is the body of PUT /snapshot/load (v1.13.1 schema).
// The target process must be started with just --api-sock (no --config-file)
// and this is its first and only configuration call.
//
// Drives are NOT passed here: the vmstate file embeds each block device's
// disk_path and Firecracker re-opens those paths verbatim on restore — the
// target must therefore have identical paths (our ZFS layout guarantees
// /dev/zvol/default/vmlet-microvms/<vm-id> on every node). The vsock UDS
// path is likewise embedded and must exist on the target.
type LoadSnapshotParams struct {
	SnapshotPath string `json:"snapshot_path"`
	MemBackend   struct {
		BackendPath string `json:"backend_path"`
		BackendType string `json:"backend_type"`
	} `json:"mem_backend"`
	ResumeVM         bool              `json:"resume_vm"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
}

// NetworkOverride renames a restored interface's host TAP (v1.13.1).
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// LoadSnapshot restores a snapshot. The Firecracker process this client talks
// to must be freshly started and never configured.
func (c *SnapshotClient) LoadSnapshot(ctx context.Context, params *LoadSnapshotParams) error {
	_, err := c.do(ctx, http.MethodPut, "/snapshot/load", params)
	return err
}

// SnapshotSize returns the byte size of a snapshot artifact pair on disk.
func SnapshotSize(vmStatePath, memFilePath string) (vmStateBytes, memBytes int64, err error) {
	for p, out := range map[string]*int64{vmStatePath: &vmStateBytes, memFilePath: &memBytes} {
		if p == "" {
			continue
		}
		fi, serr := os.Stat(p)
		if serr != nil {
			return 0, 0, serr
		}
		*out = fi.Size()
	}
	return vmStateBytes, memBytes, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
