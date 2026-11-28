// Command vmlet-adopt audits, and optionally registers, the microVMs that
// already exist on a host running the previous platform (haioflash).
//
// Usage:
//
//	vmlet-adopt scan              # read-only: what is on disk, and the plan
//	vmlet-adopt scan -json        # machine-readable output
//	vmlet-adopt plan -existing a,b,c
//	vmlet-adopt apply -db /opt/vmlet/data/vms-state.db   # writes records only
//
// The scan and plan modes are strictly read-only: they read the ZFS dataset
// list, the per-VM work directories and config.json files, the systemd units,
// and the host neighbour table. Nothing is started, stopped, restarted or
// reconfigured, and no network device is created. The apply mode writes VM
// records into vmlet's own SQLite store and nothing else.
//
// The same functionality is exposed by the HTTP server as POST /admin/adopt/scan
// and POST /admin/adopt/apply.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/haioco/vmlet/internal/adopt"
	"github.com/haioco/vmlet/internal/storage"
)

func main() {
	var (
		jsonOut  = flag.Bool("json", false, "emit JSON instead of text")
		existing = flag.String("existing", "", "comma-separated VM ids vmlet already manages (plan/apply only)")
		dbPath   = flag.String("db", "", "SQLite database for apply (default $MICROVM_WORK_DIR/vms-state.db)")
		verbose  = flag.Bool("v", false, "list every discovery, not just the plan outcome")
	)
	flag.Usage = func() {
		// Note for the reader: Go's flag package stops at the first non-flag
		// argument, so the flags must come before the mode.
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] scan|plan|apply\n\nflags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	mode := "scan"
	if flag.NArg() > 0 {
		mode = strings.ToLower(flag.Arg(0))
	}
	if mode != "scan" && mode != "plan" && mode != "apply" {
		fmt.Fprintf(os.Stderr, "unknown mode %q (want scan, plan or apply)\n", mode)
		flag.Usage()
		os.Exit(2)
	}

	opts := adopt.DefaultScanOptions()
	if v := os.Getenv("MICROVM_ADOPT_WORK_DIR"); v != "" {
		opts.WorkDir = v
	}
	if v := os.Getenv("MICROVM_ADOPT_ZFS_DATASET"); v != "" {
		opts.DatasetRoot = v
	}
	if v := os.Getenv("MICROVM_ADOPT_UNIT_DIR"); v != "" {
		opts.UnitDir = v
	}

	discoveries, report, err := adopt.ScanReported(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan failed: %v\n", err)
		os.Exit(1)
	}

	known := splitIDs(*existing)
	actions := adopt.Plan(discoveries, known)
	counts := adopt.Counts(actions)

	switch mode {
	case "scan":
		if *jsonOut {
			emitJSON(map[string]interface{}{
				"mode":        "scan",
				"found":       len(discoveries),
				"registered":  len(known),
				"sources":     report,
				"counts":      counts,
				"warnings":    report.Warnings,
				"discoveries": discoveries,
			})
			return
		}
		fmt.Printf("Read-only scan of %s (dataset %s)\n", opts.WorkDir, report.DatasetRoot)
		fmt.Printf("  sources: zfs datasets=%d work dirs=%d unit files=%d\n",
			report.ZFSDatasets, report.WorkDirs, report.UnitFiles)
		fmt.Printf("  found %d VMs; %d already registered; plan: adopt=%d skip-existing=%d skip-unhealthy=%d\n",
			len(discoveries), len(known), counts[adopt.ActionAdopt],
			counts[adopt.ActionSkipExisting], counts[adopt.ActionSkipUnhealthy])
		for _, w := range report.Warnings {
			fmt.Printf("  warning: %s\n", w)
		}
		for _, a := range actions {
			if a.Kind == adopt.ActionSkipUnhealthy {
				fmt.Printf("  skip %s: %s\n", a.VMID, a.Reason)
			}
		}
		if *verbose {
			for _, d := range discoveries {
				fmt.Printf("  %s running=%v vcpu=%d mem=%dMB mac=%s dev=%s bin=%s unit=%s ip=%s\n",
					d.ID, d.Running, d.VCPUs, d.MemoryMB, d.PrimaryInterface().GuestMAC,
					d.PrimaryInterface().HostDevName, d.FirecrackerBin, d.UnitName, ipOrUnknown(d))
			}
		}
		return

	case "plan":
		if *jsonOut {
			emitJSON(map[string]interface{}{
				"mode": "plan", "found": len(discoveries),
				"registered": len(known), "counts": counts, "plan": actions,
			})
			return
		}
		fmt.Printf("Plan for %d VMs (%d registered): adopt=%d skip-existing=%d skip-unhealthy=%d\n",
			len(discoveries), len(known), counts[adopt.ActionAdopt],
			counts[adopt.ActionSkipExisting], counts[adopt.ActionSkipUnhealthy])
		return

	case "apply":
		if *dbPath == "" {
			workDir := os.Getenv("MICROVM_WORK_DIR")
			if workDir == "" {
				workDir = "./work"
			}
			*dbPath = workDir + "/vms-state.db"
		}
		store, err := storage.NewSQLiteStorage(*dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", *dbPath, err)
			os.Exit(1)
		}
		defer store.Close()

		// SaveVM per adopted VM only: SyncVMs would drop every record that is
		// not on this host's disk, and an adopted node's store may legitimately
		// hold VMs that live elsewhere.
		result := adopt.Apply(discoveries, known, adopt.ApplyOptions{Store: store})
		if *jsonOut {
			emitJSON(map[string]interface{}{
				"mode": "apply", "adopted": result.AdoptedIDs(),
				"count": len(result.Applied), "failed": result.Failed, "counts": counts,
			})
			return
		}
		fmt.Printf("Applied into %s: %s\n", *dbPath, result.String())
		for _, f := range result.Failed {
			fmt.Printf("  failed %s: %s\n", f.VMID, f.Error)
		}
		return
	}
}

// ipOrUnknown renders the recorded IP.
func ipOrUnknown(d adopt.Discovery) string {
	if d.IPAddressKnown {
		return d.IPAddress
	}
	return "unknown"
}

// splitIDs parses a comma-separated id list.
func splitIDs(list string) []string {
	var out []string
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// emitJSON writes v to stdout.
func emitJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
}
