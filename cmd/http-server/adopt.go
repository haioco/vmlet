package main

// adopt.go — the HTTP surface of the read-only adopter.
//
// This lets vmlet take over management of VMs that already exist on a host
// running the previous platform (haioflash) WITHOUT touching the running VMs:
//
//	POST /admin/adopt/scan    dry run: reports what is on disk and the plan
//	POST /admin/adopt/apply   writes the accepted records into vmlet's store
//
// Both endpoints only ever read the host's ZFS/work-dir/systemd state and write
// to vmlet's own SQLite. No VM is started, stopped or restarted, no systemd unit
// is created or modified, and no fc-* device is created — the VMs keep running
// exactly as they are. See internal/adopt for the safety property and the
// read-only command allowlist this rests on.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/adopt"
)

// Environment knobs for the adopter. They live apart from vmlet's own settings
// (MICROVM_WORK_DIR, MICROVM_ZFS_DATASET, ...) because on an adopted host they
// point at the previous platform's layout, not vmlet's.
const (
	// AdoptScanEnvVar / -adopt-scan: opt-in startup report. Discovery must not
	// run implicitly on every boot.
	AdoptScanEnvVar = "MICROVM_ADOPT_SCAN"
	// AdoptWorkDirEnvVar: per-VM config.json directory.
	AdoptWorkDirEnvVar = "MICROVM_ADOPT_WORK_DIR"
	// AdoptDatasetEnvVar: ZFS dataset holding the root disks.
	AdoptDatasetEnvVar = "MICROVM_ADOPT_ZFS_DATASET"
	// AdoptUnitDirEnvVar: static unit directory.
	AdoptUnitDirEnvVar = "MICROVM_ADOPT_UNIT_DIR"
	// AdoptBridgeEnvVar: bridge the VMs are already attached to.
	AdoptBridgeEnvVar = "MICROVM_ADOPT_BRIDGE"
	// AdoptResolveIPsEnvVar: enrich records with the guest IP from the host
	// neighbour table (default true).
	AdoptResolveIPsEnvVar = "MICROVM_ADOPT_RESOLVE_IPS"
	// AdoptCustomerIDEnvVar: customer stamp for records with no billing owner.
	AdoptCustomerIDEnvVar = "MICROVM_ADOPT_CUSTOMER_ID"
	// AdoptNodeIDEnvVar: node stamp for adopted records.
	AdoptNodeIDEnvVar = "MICROVM_ADOPT_NODE_ID"
)

// adoptScanOptions builds the read-only scan options from the environment.
func adoptScanOptions() adopt.ScanOptions {
	opts := adopt.DefaultScanOptions()
	if v := os.Getenv(AdoptWorkDirEnvVar); v != "" {
		opts.WorkDir = v
	}
	if v := os.Getenv(AdoptDatasetEnvVar); v != "" {
		opts.DatasetRoot = v
	}
	if v := os.Getenv(AdoptUnitDirEnvVar); v != "" {
		opts.UnitDir = v
	}
	if v := os.Getenv(AdoptResolveIPsEnvVar); v != "" {
		opts.SkipIPResolution = strings.EqualFold(v, "false") || v == "0" || strings.EqualFold(v, "no")
	}
	return opts
}

// adoptApplyOptions builds the conversion options from the environment.
func (s *HTTPServer) adoptApplyOptions() adopt.ApplyOptions {
	return adopt.ApplyOptions{
		NodeID:     firstNonEmptyEnv(AdoptNodeIDEnvVar, "local-node"),
		CustomerID: os.Getenv(AdoptCustomerIDEnvVar),
		BridgeName: firstNonEmptyEnv(AdoptBridgeEnvVar, adopt.DefaultBridge),
	}
}

// firstNonEmptyEnv returns the first environment variable that is set.
func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// registeredVMIDs returns the VM ids vmlet already has records for.
func (s *HTTPServer) registeredVMIDs() []string {
	vms := s.snapshotVMMap()
	ids := make([]string, 0, len(vms))
	for id := range vms {
		ids = append(ids, id)
	}
	return ids
}

// adoptRequest is the optional body of POST /admin/adopt/apply. Every field is
// optional; an empty body adopts everything healthy that is not yet registered.
type adoptRequest struct {
	// NodeID / CustomerID / Bridge override the environment for this run.
	NodeID     string `json:"node_id,omitempty"`
	CustomerID string `json:"customer_id,omitempty"`
	Bridge     string `json:"bridge,omitempty"`
	// VMIDs restricts the run to these VM ids. Empty means all of them.
	VMIDs []string `json:"vm_ids,omitempty"`
}

// scanResponse is the body of POST /admin/adopt/scan.
type scanResponse struct {
	Found      int              `json:"found"`
	Registered int              `json:"registered"`
	Sources    adopt.ScanReport `json:"sources"`
	Counts     map[string]int   `json:"counts"`
	Warnings   []string         `json:"warnings,omitempty"`
	Plan       []adopt.Action   `json:"plan"`
	Skipped    []string         `json:"skip_unhealthy,omitempty"`
}

// applyResponse is the body of POST /admin/adopt/apply.
type applyResponse struct {
	Adopted          []string        `json:"adopted"`
	Count            int             `json:"count"`
	Failed           []adopt.Failure `json:"failed,omitempty"`
	SkippedExisting  int             `json:"skipped_existing"`
	SkippedUnhealthy int             `json:"skipped_unhealthy"`
	Warnings         []string        `json:"warnings,omitempty"`
}

// adoptScanHandler is the dry run: it reports what is on disk and what the
// adopter would do, and changes nothing.
func (s *HTTPServer) adoptScanHandler(w http.ResponseWriter, r *http.Request) {
	opts := adoptScanOptions()
	discoveries, report, err := adopt.ScanReported(opts)
	if err != nil {
		http.Error(w, fmt.Sprintf("adoption scan failed: %v", err), http.StatusInternalServerError)
		return
	}

	registered := s.registeredVMIDs()
	actions := adopt.Plan(discoveries, registered)

	var skipped []string
	counts := map[string]int{}
	for _, a := range actions {
		counts[string(a.Kind)]++
		if a.Kind == adopt.ActionSkipUnhealthy {
			skipped = append(skipped, fmt.Sprintf("%s: %s", a.VMID, a.Reason))
		}
	}

	log.Printf("🔎 adoption scan (read-only): found=%d registered=%d adopt=%d skip-existing=%d skip-unhealthy=%d",
		len(discoveries), len(registered), counts[string(adopt.ActionAdopt)],
		counts[string(adopt.ActionSkipExisting)], counts[string(adopt.ActionSkipUnhealthy)])

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(scanResponse{
		Found:      len(discoveries),
		Registered: len(registered),
		Sources:    report,
		Counts:     counts,
		Warnings:   report.Warnings,
		Plan:       actions,
		Skipped:    skipped,
	})
}

// adoptApplyHandler writes the accepted records into vmlet's store. It never
// touches a running VM: no unit is started, stopped or rewritten, and no fc-*
// device is created.
func (s *HTTPServer) adoptApplyHandler(w http.ResponseWriter, r *http.Request) {
	var req adoptRequest
	if r.Body != nil {
		// An empty or absent body is valid: adopt everything on offer.
		_ = json.NewDecoder(r.Body).Decode(&req)
		defer r.Body.Close()
	}

	discoveries, report, err := adopt.ScanReported(adoptScanOptions())
	if err != nil {
		http.Error(w, fmt.Sprintf("adoption scan failed: %v", err), http.StatusInternalServerError)
		return
	}
	if len(req.VMIDs) > 0 {
		want := make(map[string]struct{}, len(req.VMIDs))
		for _, id := range req.VMIDs {
			want[id] = struct{}{}
		}
		filtered := discoveries[:0]
		for _, d := range discoveries {
			if _, ok := want[d.ID]; ok {
				filtered = append(filtered, d)
			}
		}
		discoveries = filtered
	}

	applyOpts := s.adoptApplyOptions()
	if req.NodeID != "" {
		applyOpts.NodeID = req.NodeID
	}
	if req.CustomerID != "" {
		applyOpts.CustomerID = req.CustomerID
	}
	if req.Bridge != "" {
		applyOpts.BridgeName = req.Bridge
	}

	store := s.vmStateStorage
	if store == nil {
		http.Error(w, "SQLite storage is not initialized; cannot persist adopted VMs", http.StatusServiceUnavailable)
		return
	}
	applyOpts.Store = store

	result := adopt.Apply(discoveries, s.registeredVMIDs(), applyOpts)

	// Register the new records in memory so they show up in GET /vms and can be
	// managed (status, resize, delete, public IP add-ons) from now on.
	for _, applied := range result.Applied {
		s.setVM(applied.VMID, applied.Instance)
		s.invalidateTAPCache(applied.VMID)
		s.invalidateVMCache(applied.VMID)
	}
	if len(result.Applied) > 0 {
		s.persistVMState()
	}

	log.Printf("📥 adoption apply (read-only towards VMs): %s store=%s", result.String(), store.GetDBPath())
	for _, f := range result.Failed {
		log.Printf("⚠️  adoption failed for %s: %s", f.VMID, f.Error)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(applyResponse{
		Adopted:          result.AdoptedIDs(),
		Count:            len(result.Applied),
		Failed:           result.Failed,
		SkippedExisting:  result.Counts[adopt.ActionSkipExisting],
		SkippedUnhealthy: result.Counts[adopt.ActionSkipUnhealthy],
		Warnings:         report.Warnings,
	})
}

// adoptScanEnabled reports whether the startup adoption report should run. It is
// opt-in (MICROVM_ADOPT_SCAN / -adopt-scan): discovery must never run implicitly
// on every boot.
func adoptScanEnabled() bool {
	return getEnvBool(AdoptScanEnvVar, false)
}

// reportAdoptionStatus logs how many VMs exist on disk versus how many are
// already registered. Purely informational and read-only: it scans, plans and
// writes nothing.
func (s *HTTPServer) reportAdoptionStatus() {
	if !adoptScanEnabled() {
		return
	}
	start := time.Now()
	discoveries, report, err := adopt.ScanReported(adoptScanOptions())
	if err != nil {
		log.Printf("⚠️  Adoption scan disabled for this report: %v", err)
		return
	}
	registered := s.registeredVMIDs()
	actions := adopt.Plan(discoveries, registered)
	counts := adopt.Counts(actions)

	fmt.Printf("🔎 Adoption scan (read-only, %s): found %d VMs on disk, %d already registered\n",
		time.Since(start).Round(time.Millisecond), len(discoveries), len(registered))
	fmt.Printf("   plan: adopt=%d skip-existing=%d skip-unhealthy=%d (work dir=%s, dataset=%d, unit dir=%d)\n",
		counts[adopt.ActionAdopt], counts[adopt.ActionSkipExisting], counts[adopt.ActionSkipUnhealthy],
		adoptScanOptions().WorkDir, report.ZFSDatasets, report.UnitFiles)
	for _, w := range report.Warnings {
		fmt.Printf("   ⚠️  %s\n", w)
	}
	for _, a := range actions {
		if a.Kind != adopt.ActionSkipUnhealthy {
			continue
		}
		fmt.Printf("   ⏭️  %s: %s\n", a.VMID, a.Reason)
	}
	fmt.Printf("   Run POST /admin/adopt/scan for the full plan and POST /admin/adopt/apply to take them over\n")
}

// Adoption records carry models.NetworkModePreWired and the VM's existing fc-*
// device: the guest already holds a routable public address on a device its own
// systemd unit created on the host bridge, and that mode keeps vmlet's networking
// layer away from it entirely — ConfigureNetwork and CleanupNetwork are no-ops
// for it, so vmlet can neither shadow nor delete the device in use.
// internal/adopt.ToMicroVM sets both.

// Not guarded by these endpoints but worth stating: an adopted record points at
// the host's zvol through the external_rootfs_path label, so a later
// DELETE /vms/{id} on one would reach the host's dataset. The repo has no auth on
// /admin/* routes (the existing convention), so on a real node this port must
// stay unreachable from anything but a trusted network.
