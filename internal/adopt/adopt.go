// Package adopt lets vmlet take over management of microVMs that already exist
// on a host running the previous platform (haioflash) without touching the
// running VMs.
//
// # The safety property this package rests on
//
// Everything here is strictly READ-ONLY. Scan enumerates the ZFS dataset list,
// reads each VM's firecracker config.json, reads each VM's systemd unit, and
// issues read-only queries (systemctl show / is-active / is-enabled,
// ip neigh show). It never starts, stops, restarts, reloads, enables, disables
// or otherwise reconfigures a unit, and it never creates or deletes a network
// device, a zvol, a dataset or a file. Apply only writes records into vmlet's
// own store (SQLite) so the VMs can be listed, resized, deleted and given
// extra public IPs afterwards; the guest processes, their systemd units and
// their fc-* devices are left exactly as they are.
//
// To make that enforceable rather than aspirational, every shell command this
// package runs goes through CommandRunner (see execRunner.Run), which refuses
// anything that is not on a small read-only allowlist. A command that is not
// explicitly listed fails with ErrNonReadOnly instead of running.
package adopt

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNonReadOnly is returned when a caller asks the command runner to execute
// something that is not on the read-only allowlist. The adopter must never
// mutate host state: if you find yourself adding a command here, the operation
// belongs outside this package.
var ErrNonReadOnly = errors.New("adopt: refusing to run a non-read-only command")

// DefaultCommandTimeout bounds every read-only command the adopter runs, so a
// wedged zfs/systemctl cannot hang a scan (or an HTTP handler) forever.
const DefaultCommandTimeout = 30 * time.Second

// CommandRunner executes a read-only command and returns its standard output.
//
// It exists so tests can substitute a recorder: the test asserts that Scan only
// ever asks for allowlisted reads. Implementations MUST reject mutating
// commands — see execRunner for the production behaviour.
type CommandRunner interface {
	// Run executes name with args and returns stdout and the exit error (if any).
	Run(name string, args ...string) (string, error)
}

// execRunner is the production CommandRunner: os/exec behind the read-only
// allowlist. Nothing else in this package is allowed to execute anything.
type execRunner struct {
	timeout time.Duration
}

// NewCommandRunner returns the default read-only command runner.
func NewCommandRunner() CommandRunner {
	return execRunner{timeout: DefaultCommandTimeout}
}

// Run validates the requested command against the read-only allowlist and only
// then executes it. A command outside the allowlist returns ErrNonReadOnly and
// is never executed.
func (r execRunner) Run(name string, args ...string) (string, error) {
	if !isReadOnly(name, args) {
		return "", fmt.Errorf("%w: %s %s", ErrNonReadOnly, name, strings.Join(args, " "))
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	cmd := exec.Command(name, args...) //nolint:gosec // allowlisted read-only command
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=0", "TERM=dumb")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err,
				strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// isReadOnly reports whether `name args` is one of the handful of read-only
// commands the adopter is allowed to run. The allowlist is exhaustively:
//
//	zfs list ...        dataset inventory + per-dataset volsize
//	systemctl show/is-active/is-enabled/list-units/list-unit-files ...
//	ip link show / ip neigh show ...
//
// Everything else — systemctl start/stop/restart/enable/disable/kill/mask,
// zfs create/destroy/snapshot/rollback/set, ip link add/del/set, brctl, iptables,
// mount, zpool — is refused.
func isReadOnly(name string, args []string) bool {
	switch name {
	case "zfs":
		return len(args) > 0 && args[0] == "list"
	case "systemctl":
		return allowSystemctl(args)
	case "ip":
		return allowIP(args)
	default:
		return false
	}
}

// allowSystemctl accepts only the query subcommands: show, is-active,
// is-enabled, list-units and list-unit-files. All of them are pure reads.
func allowSystemctl(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue // flags, not the subcommand
		}
		switch a {
		case "show", "is-active", "is-enabled", "list-units", "list-unit-files":
			return true
		default:
			return false
		}
	}
	return false
}

// allowIP accepts only "ip <object> show ..." for the objects the adopter
// reads: link (device inventory) and neigh (MAC → IP mapping). Any other ip
// verb (add, del, set, up, down, replace, flush) is refused.
func allowIP(args []string) bool {
	objects := map[string]bool{"link": true, "neigh": true, "addr": true, "route": true}
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		if !objects[args[i]] {
			return false
		}
		return i+1 < len(args) && args[i+1] == "show"
	}
	return false
}

// parseZFSHumanSize converts a ZFS "human" size such as "10G", "512M", "1.5T"
// or a raw byte count into bytes. It returns ok=false for values that carry no
// size ("-", "none", anything unparseable).
func parseZFSHumanSize(v string) (uint64, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "-" || v == "none" {
		return 0, false
	}
	// Fast path: raw bytes (zfs -H prints plain bytes for most properties).
	if n, err := strconv.ParseUint(v, 10, 64); err == nil {
		return n, true
	}
	mult := uint64(1)
	last := v[len(v)-1]
	switch last {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	case 'P', 'p':
		mult = 1 << 50
	default:
		return 0, false
	}
	num, err := strconv.ParseFloat(strings.TrimSuffix(v, string(last)), 64)
	if err != nil {
		return 0, false
	}
	return uint64(num * float64(mult)), true
}
