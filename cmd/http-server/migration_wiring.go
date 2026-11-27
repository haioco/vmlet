package main

// migration_wiring.go — connects the internal/migration package to this
// node's HTTPServer and firecracker.Manager. Everything here is node-local
// glue; the migration logic itself lives in internal/migration.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/firecracker"
	"github.com/haioco/vmlet/internal/migration"
	"github.com/haioco/vmlet/internal/models"
)

// migrationToken returns the shared node-to-node secret; empty disables the
// peer endpoints (fail closed).
func migrationToken() string {
	return os.Getenv("MICROVM_MIGRATION_TOKEN")
}

// buildMigrationHandler wires the migration service into this node.
func (s *HTTPServer) buildMigrationHandler() (*migration.Handler, error) {
	targetNM, err := firecracker.NewNetworkManager()
	if err != nil {
		return nil, fmt.Errorf("migration: network manager: %w", err)
	}

	hooks := &migration.Hooks{
		NodeDomain: s.nodeDomain,

		// ---------- source side ----------
		GetVM:              s.getVM,
		SocketPath:         s.manager.SocketPathFor,
		VMWorkDir:          func(vmID string) string { return filepath.Join(resolveWorkDir(), "microvms", vmID) },
		IsVMProcessRunning: s.manager.IsVMProcessRunning,

		StopSourceVM: func(vm *models.MicroVMInstance) error {
			// Retire the record FIRST: the node's reboot watcher restarts
			// dying VMs that still exist in the map with auto_start set —
			// for a migrated VM that would cold-boot the OLD disk on the
			// source and put a duplicate MAC/IP on the L2. With the record
			// gone the watcher ignores the exit.
			s.deleteVMEntry(vm.ID)
			if s.vmStateStorage != nil {
				_ = s.vmStateStorage.DeleteVM(vm.ID)
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// force=true: cutover must proceed even if the unit is in a
			// transitional state; the disk is untouched either way.
			return s.manager.StopMicroVM(ctx, vm, true)
		},

		RestartSourceVM: func(vm *models.MicroVMInstance) error {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := s.manager.StartMicroVM(ctx, vm); err != nil {
				return err
			}
			// Rollback path: the record was retired by StopSourceVM — bring
			// it back so the node tracks the VM again.
			s.setVM(vm.ID, vm)
			if s.vmStateStorage != nil {
				_ = s.vmStateStorage.SaveVM(vm)
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()
			// The host-side NAT/NDP plumbing was never torn down before
			// cutover, but re-assert the IPv4 fallback NAT (idempotent).
			if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
				if publicIP, err := s.getPublicIPv4(); err == nil && publicIP != "" {
					_ = setupIPv4FallbackNAT(vm.Network.IPv4NATAddress, publicIP)
				}
			}
			return nil
		},

		CleanupSourceNetwork: func(vm *models.MicroVMInstance) error {
			var firstErr error
			if s.reservationMgr != nil && vm.Network.TAPDevice != "" {
				if err := s.reservationMgr.RemoveSpoofingProtection(
					vm.Network.TAPDevice, vm.Network.MACAddress, vm.Network.IPAddress,
				); err != nil {
					firstErr = err
				}
			}
			if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
				cleanupIPv4FallbackNAT(vm.Network.IPv4NATAddress)
			}
			for _, addon := range vm.Network.IPv4Addons {
				_ = removeIPv4Addon(addon.InterfaceID, addon.IPv4Address, vm.Network.IPv4NATAddress, addon.RoutingTable)
			}
			// TAP + bridge membership: the network manager owns removal.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := targetNM.CleanupNetwork(ctx, vm.ID, vm.Network.AssignmentMethod); err != nil && firstErr == nil {
				firstErr = err
			}
			return firstErr
		},

		RetireSourceRecord: func(vm *models.MicroVMInstance, targetNode string) error {
			// Keep the zvol + its @mig-rollback snapshot for retention;
			// remove the live record so the node stops tracking/billing it.
			if vm.Metadata.Labels == nil {
				vm.Metadata.Labels = map[string]string{}
			}
			vm.Metadata.Labels["migrated_to"] = targetNode
			vm.Metadata.Labels["migrated_at"] = time.Now().UTC().Format(time.RFC3339)
			s.deleteVMEntry(vm.ID)
			if s.vmStateStorage != nil {
				if err := s.vmStateStorage.DeleteVM(vm.ID); err != nil {
					return err
				}
			}
			if s.balloonManager != nil {
				_ = s.balloonManager.DeleteProfile(vm.ID)
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()
			return nil
		},

		TrafficSnapshot: func(vmID string) (uint64, uint64) {
			vm, ok := s.getVM(vmID)
			if !ok {
				return 0, 0
			}
			return vm.TrafficIngressBytes, vm.TrafficEgressBytes
		},

		// ---------- target side ----------
		VMIDTaken: func(vmID string) bool {
			if _, ok := s.getVM(vmID); ok {
				return true
			}
			if s.manager.IsVMProcessRunning(vmID) {
				return true
			}
			// Stale config.json/work dirs from an aborted migration are
			// cleaned by Prepare, so they do not block a new attempt.
			return false
		},

		SetupTargetNetwork: func(vm *models.MicroVMInstance) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			info, err := targetNM.ConfigureNetwork(ctx,
				vm.ID,
				vm.Network.AssignmentMethod,
				vm.Network.MACAddress, // preserved identity
				vm.Network.BandwidthIngress,
				vm.Network.BandwidthEgress,
				vm.Network.IPv6Address, // preserved identity
				vm.Network.IPv6Gateway,
				vm.Network.IPv6PrefixLength,
				vm.Network.NetworkID,
			)
			if err != nil {
				return "", err
			}
			tap := vm.Network.TAPDevice
			if info != nil && info.TAPDevice != "" {
				tap = info.TAPDevice
				vm.Network.TAPDevice = info.TAPDevice
				if info.BridgeName != "" {
					vm.Network.BridgeName = info.BridgeName
				}
			}
			if tap == "" {
				return "", fmt.Errorf("network setup returned no TAP device")
			}

			// Antispoofing for the preserved MAC/IP pair.
			if err := targetNM.ApplyAntiSpoofing(tap, vm.Network.MACAddress, vm.Network.IPAddress, vm.Network.IPv6Address); err != nil {
				return tap, fmt.Errorf("antispoofing: %w", err)
			}

			// IPv4 NAT fallback for IPv6-only VMs.
			if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
				if publicIP, err := s.getPublicIPv4(); err == nil && publicIP != "" {
					if err := setupIPv4FallbackNAT(vm.Network.IPv4NATAddress, publicIP); err != nil {
						return tap, fmt.Errorf("ipv4 fallback NAT: %w", err)
					}
				}
			}

			// IPv4 addons — re-home the policy-routed public addresses.
			for _, addon := range vm.Network.IPv4Addons {
				if !addon.Enabled || vm.Network.IPv4NATAddress == "" {
					continue
				}
				gateway := ""
				parts := strings.Split(addon.IPv4Address, ".")
				if len(parts) == 4 {
					gateway = fmt.Sprintf("%s.%s.%s.254", parts[0], parts[1], parts[2])
				}
				if err := configureIPv4Addon(addon.InterfaceID, addon.IPv4Address, addon.MACAddress, gateway, vm.Network.IPv4NATAddress, addon.RoutingTable); err != nil {
					return tap, fmt.Errorf("ipv4 addon %s: %w", addon.IPv4Address, err)
				}
			}
			return tap, nil
		},

		TeardownTargetNetwork: func(vm *models.MicroVMInstance, tapName string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if s.reservationMgr != nil && tapName != "" {
				_ = s.reservationMgr.RemoveSpoofingProtection(tapName, vm.Network.MACAddress, vm.Network.IPAddress)
			}
			if vm.Network.IPv4NAT && vm.Network.IPv4NATAddress != "" {
				cleanupIPv4FallbackNAT(vm.Network.IPv4NATAddress)
			}
			for _, addon := range vm.Network.IPv4Addons {
				_ = removeIPv4Addon(addon.InterfaceID, addon.IPv4Address, vm.Network.IPv4NATAddress, addon.RoutingTable)
			}
			return targetNM.CleanupNetwork(ctx, vm.ID, vm.Network.AssignmentMethod)
		},

		StartRestoreProcess: func(vmID, socketPath string) error {
			fcBin := os.Getenv("MICROVM_FIRECRACKER_BIN")
			if fcBin == "" {
				fcBin = "/usr/local/bin/firecracker"
				if _, err := os.Stat(fcBin); err != nil {
					fcBin = "firecracker"
				}
			}
			if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
				return err
			}
			os.Remove(socketPath)

			unit := "firecracker-mig-" + vmID
			out, err := exec.Command("systemd-run", "--collect", "--unit="+unit,
				fcBin, "--api-sock", socketPath).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemd-run %s: %v (%s)", unit, err, strings.TrimSpace(string(out)))
			}
			// Wait for the API socket to answer.
			client := migration.NewSnapshotClient(socketPath)
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := client.InstanceInfo(context.Background()); err == nil {
					return nil
				}
				time.Sleep(250 * time.Millisecond)
			}
			return fmt.Errorf("restore process did not expose its API socket in time")
		},

		StopRestoreProcess: func(vmID string) error {
			unit := "firecracker-mig-" + vmID
			out, err := exec.Command("systemctl", "stop", unit).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl stop %s: %v (%s)", unit, err, strings.TrimSpace(string(out)))
			}
			return nil
		},

		AdoptVM: func(vm *models.MicroVMInstance, tapName string, ingress, egress uint64) error {
			vm.Network.TAPDevice = tapName
			vm.NodeID = s.nodeDomain
			vm.Status = models.MicroVMStatusRunning
			vm.TrafficIngressBytes = ingress
			vm.TrafficEgressBytes = egress
			vm.UpdateLastActivity()

			// Rebuild config.json from the record so the VM can cold-boot
			// locally after its next stop/start.
			regenerateVMConfigJSON(vm)

			s.setVM(vm.ID, vm)
			if s.vmStateStorage != nil {
				if err := s.vmStateStorage.SaveVM(vm); err != nil {
					return err
				}
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()
			return nil
		},

		DropAdopted: func(vm *models.MicroVMInstance) error {
			s.deleteVMEntry(vm.ID)
			if s.vmStateStorage != nil {
				_ = s.vmStateStorage.DeleteVM(vm.ID)
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()
			return nil
		},

		// ColdBoot starts the VM from its migrated disk + config (cold
		// migration, and the fallback when a snapshot restore dies).
		ColdBoot: func(vm *models.MicroVMInstance) error {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			if err := s.manager.StartMicroVM(ctx, vm); err != nil {
				return err
			}
			s.setVM(vm.ID, vm)
			if s.vmStateStorage != nil {
				_ = s.vmStateStorage.SaveVM(vm)
			}
			s.invalidateTAPCache(vm.ID)
			s.invalidateVMCache(vm.ID)
			s.persistVMState()
			return nil
		},

		// VerifyVMAlive requires both the process and a responsive API
		// reporting state=Running (a triple-faulted guest leaves neither).
		VerifyVMAlive: func(vmID string) bool {
			if !s.manager.IsVMProcessRunning(vmID) {
				return false
			}
			sock := s.manager.SocketPathFor(vmID)
			if sock == "" {
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			info, err := migration.NewSnapshotClient(sock).InstanceInfo(ctx)
			if err != nil {
				return false
			}
			state, _ := info["state"].(string)
			return state == "Running" || state == "Paused"
		},
	}

	svc, err := migration.NewService(hooks, migrationToken)
	if err != nil {
		return nil, err
	}
	s.sweepOrphanMigrations()
	return migration.NewHandler(svc, migrationToken), nil
}

// sweepOrphanMigrations clears leftovers of a previous process: bare restore
// units whose VM was never adopted (these are not restarted by the node's
// reboot watcher) and per-VM snapshot artifact dirs. Adopted VMs reuse the
// firecracker-mig-* unit name, so tracked VMs are always left alone.
func (s *HTTPServer) sweepOrphanMigrations() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--no-pager", "firecracker-mig-*").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			unit := fields[0]
			vmID := strings.TrimPrefix(unit, "firecracker-mig-")
			if vmID == "" || vmID == unit {
				continue
			}
			if _, ok := s.getVM(vmID); ok {
				continue // adopted VM — this unit is now its real process
			}
			_ = exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
			fmt.Printf("🧹 migration sweep: stopped orphan restore unit %s\n", unit)
		}
	}

	if dir := os.Getenv("MICROVM_MIGRATION_DIR"); dir != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := s.getVM(e.Name()); ok {
				continue
			}
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
				fmt.Printf("🧹 migration sweep: removed stale artifacts for %s\n", e.Name())
			}
		}
		return
	}

	// Default layout: <workdir>/microvms/<vmID>/mig.
	root := filepath.Join(resolveWorkDir(), "microvms")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := s.getVM(e.Name()); ok {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name(), "mig"))
	}
}
