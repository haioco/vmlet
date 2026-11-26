package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemTemplateName is the unit that runs one VM per instance.
const systemTemplateName = "firecracker-vm@.service"

// systemdUnitDir is where vmlet installs its template unit. /etc takes
// precedence over /lib, so an install there overrides the distro packaging.
const systemdUnitDir = "/etc/systemd/system"

// ensureSystemdTemplate writes systemTemplateName with the work dir and
// Firecracker binary this manager was configured with, then reloads systemd.
//
// Without this a host inherits whatever template happens to be present, which
// on a machine that previously ran another microVM platform points at a
// directory that no longer exists. systemd then fails with "208/STDIN"
// (its StandardInput= file is missing), which says nothing about the real
// cause. Regenerating it from live configuration on every start makes the
// unit self-healing across renames of the work directory and binary.
func (m *Manager) ensureSystemdTemplate() error {
	unitPath := filepath.Join(systemdUnitDir, systemTemplateName)

	rendered, err := renderSystemdTemplate(m.workDir, m.firecrackerBin)
	if err != nil {
		return err
	}

	// Skip the write (and the systemctl overhead) when nothing changed, so a
	// busy host does not reload systemd on every VM start.
	if existing, err := os.ReadFile(unitPath); err == nil && string(existing) == rendered {
		return nil
	}

	info, err := os.Stat(systemdUnitDir)
	if err != nil || !info.IsDir() {
		if err == nil {
			return fmt.Errorf("%s is not a directory", systemdUnitDir)
		}
		// Non-systemd host (container, dev box): skip silently.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", systemdUnitDir, err)
	}

	if err := os.WriteFile(unitPath, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.Command("systemctl", "daemon-reload")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// renderSystemdTemplate substitutes the manager's paths into the shipped unit.
func renderSystemdTemplate(workDir, firecrackerBin string) (string, error) {
	templatePath := filepath.Join("systemd", systemTemplateName)
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		// Fall back to a built-in copy so a binary run from another directory
		// still provisions a working unit.
		raw = []byte(builtinSystemdTemplate)
	}

	if workDir == "" {
		workDir = "/opt/vmlet/data"
	}
	if firecrackerBin == "" {
		firecrackerBin = "/usr/local/bin/firecracker"
	}

	rendered := strings.ReplaceAll(string(raw), "@WORK_DIR@", workDir)
	rendered = strings.ReplaceAll(rendered, "@FIRECRACKER_BIN@", firecrackerBin)
	return rendered, nil
}

// builtinSystemdTemplate is the fallback copy of systemd/firecracker-vm@.service
// used when the repo's unit file is not alongside the binary.
const builtinSystemdTemplate = "[Unit]\nDescription=Firecracker VM %i\nAfter=network.target\n\n[Service]\nType=simple\nExecStart=@FIRECRACKER_BIN@ --api-sock /run/microvmsvc/%i.sock --config-file @WORK_DIR@/microvms/%i/config.json\nStandardInput=file:@WORK_DIR@/microvms/%i/console.in\nStandardOutput=journal\nStandardError=journal\nKillMode=process\nTimeoutStopSec=30\nRestart=no\n"
