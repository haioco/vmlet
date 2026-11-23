package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// PeerClient calls the migration endpoints of another node's vmlet.
type PeerClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewPeerClient builds a client for a target node. baseURL is e.g.
// "http://94.182.94.8:8080"; token is the shared MICROMVM_MIGRATION_TOKEN.
//
// The timeout covers whole-body streams (a full zfs send of a busy pool can
// legitimately take an hour); tune with MICROVM_MIGRATION_HTTP_TIMEOUT_MIN
// (minutes, default 240).
func NewPeerClient(baseURL, token string) *PeerClient {
	mins := 240
	if v := os.Getenv("MICROVM_MIGRATION_HTTP_TIMEOUT_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mins = n
		}
	}
	return &PeerClient{
		baseURL: baseURL,
		token:   token,
		client: &http.Client{
			Timeout: time.Duration(mins) * time.Minute,
		},
	}
}

func (p *PeerClient) do(method, path string, body interface{}, out interface{}) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.baseURL+path, rd)
	if err != nil {
		return err
	}
	if p.token != "" {
		req.Header.Set("X-Migration-Token", p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("peer %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(data), 512))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Info fetches the peer's NodeInfo.
func (p *PeerClient) Info() (*NodeInfo, error) {
	var info NodeInfo
	if err := p.do(http.MethodGet, "/migration/info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Prepare asks the peer to reserve resources for an incoming VM, shipping the
// source config.json alongside so the target can rebuild the drive topology.
func (p *PeerClient) Prepare(payload *PreparePayload) error {
	return p.do(http.MethodPost, "/migration/prepare", payload, nil)
}

// SendFile ships an auxiliary per-VM file (relative path under the VM's work
// dir) to the peer.
func (p *PeerClient) SendFile(ctx context.Context, vmID, relPath string, r io.Reader) (int64, error) {
	return p.streamTo(ctx, http.MethodPut, "/migration/"+vmID+"/file",
		map[string]string{
			"Content-Type": "application/octet-stream",
			"X-File-Path":  relPath,
		}, r)
}

// Abort asks the peer to release everything it reserved for this VM.
func (p *PeerClient) Abort(vmID string) error {
	return p.do(http.MethodPost, "/migration/"+vmID+"/abort", nil, nil)
}

// Confirm tells the peer the migration succeeded (portal can now be pointed
// at the target). The source keeps its rollback snapshot until RetireSource.
func (p *PeerClient) Confirm(vmID string) error {
	return p.do(http.MethodPost, "/migration/"+vmID+"/confirm", nil, nil)
}

// Status fetches the peer-side migration status for a VM it received.
func (p *PeerClient) Status(vmID string) (*MigrationStatus, error) {
	var st MigrationStatus
	if err := p.do(http.MethodGet, "/migration/"+vmID+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Restore asks the peer to load the snapshot + resume the VM.
func (p *PeerClient) Restore(vmID string, payload *CutoverPayload) error {
	return p.do(http.MethodPost, "/migration/"+vmID+"/restore", payload, nil)
}

// streamTo sends r as a chunked body to path, returning the bytes shipped.
// The peer consumes the stream directly (zfs receive / file append), so the
// client must not buffer it.
func (p *PeerClient) streamTo(ctx context.Context, method, path string, headers map[string]string, r io.Reader) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, r)
	if err != nil {
		return 0, err
	}
	if p.token != "" {
		req.Header.Set("X-Migration-Token", p.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("peer %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(data), 512))
	}
	// The peer reports the consumed byte count via X-Bytes-Received.
	var n int64
	fmt.Sscanf(resp.Header.Get("X-Bytes-Received"), "%d", &n)
	return n, nil
}

// SendDiskStream ships a zfs send stream to the peer's disk channel.
// toSnap is the snapshot the stream ends at; fromSnap empty = full stream.
func (p *PeerClient) SendDiskStream(ctx context.Context, vmID, toSnap, fromSnap string, r io.Reader) (int64, error) {
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"X-To-Snap":    toSnap,
	}
	if fromSnap != "" {
		headers["X-From-Snap"] = fromSnap
	}
	return p.streamTo(ctx, http.MethodPut, "/migration/"+vmID+"/disk", headers, r)
}

// SendMemStream ships a (range of a) memory snapshot file to the peer.
// offset is the byte offset in the destination memory file the payload
// belongs at; the peer appends/patches accordingly.
func (p *PeerClient) SendMemStream(ctx context.Context, vmID string, offset int64, r io.Reader) (int64, error) {
	return p.streamTo(ctx, http.MethodPut, "/migration/"+vmID+"/memory",
		map[string]string{
			"Content-Type": "application/octet-stream",
			"X-Offset":     strconv.FormatInt(offset, 10),
		}, r)
}

// SendMemExtents ships the data extents of a diff memory snapshot. The body is
// a sequence of frames — 8-byte big-endian offset, 8-byte big-endian length,
// then length raw bytes — which the peer writes at the given offsets. This is
// how Firecracker diff snapshots work: they are sparse files that write dirty
// pages in place at their guest-physical offsets (vstate/memory.rs), so the
// delta must be applied positionally, not appended.
func (p *PeerClient) SendMemExtents(ctx context.Context, vmID string, r io.Reader) (int64, error) {
	return p.streamTo(ctx, http.MethodPut, "/migration/"+vmID+"/memextents",
		map[string]string{"Content-Type": "application/octet-stream"}, r)
}

// SendVMStateStream ships the vm_state file of the latest snapshot.
func (p *PeerClient) SendVMStateStream(ctx context.Context, vmID string, r io.Reader) (int64, error) {
	return p.streamTo(ctx, http.MethodPut, "/migration/"+vmID+"/vmstate",
		map[string]string{"Content-Type": "application/octet-stream"}, r)
}
