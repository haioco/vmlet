package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// StartFirecrackerDaemon starts firecracker as a completely independent daemon
// that will survive parent process termination
func StartFirecrackerDaemon(firecrackerBin, configPath, socketPath, logFile, pidFile string) error {
	// Double fork to create a true daemon process
	// First fork
	cmd := exec.Command("sh", "-c", fmt.Sprintf(`
		# Remove any existing socket
		rm -f "%s"
		
		# Double fork to create daemon
		(
			# First fork - creates child that will become session leader
			if [ "$$" != "1" ]; then
				# Second fork - creates grandchild that is not session leader
				# This ensures the process cannot acquire a controlling terminal
				exec setsid sh -c '
					# Change to root directory to avoid keeping directories busy
					cd /
					
					# Close standard file descriptors
					exec 0</dev/null
					exec 1>>"%s"
					exec 2>>"%s"
					
					# Start firecracker in background
					exec "%s" --api-sock "%s" --config-file "%s" &
					
					# Save PID of firecracker process
					echo $! > "%s"
					
					# Wait a moment to ensure firecracker starts
					sleep 1
				' &
			fi
		) &
		
		# Wait for the daemon to start
		sleep 2
	`, socketPath, logFile, logFile, firecrackerBin, socketPath, configPath, pidFile))

	// Execute the daemon creation script
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start firecracker daemon: %w, output: %s", err, string(output))
	}

	// Verify the PID file was created
	if _, err := os.Stat(pidFile); os.IsNotExist(err) {
		return fmt.Errorf("firecracker daemon failed to start - no PID file created")
	}

	return nil
}

// StartFirecrackerDaemonGo starts firecracker as a daemon using Go's process management
func StartFirecrackerDaemonGo(firecrackerBin, configPath, socketPath, logFile, pidFile string) error {
	// Remove existing socket
	os.Remove(socketPath)

	// Create log file
	logWriter, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	defer logWriter.Close()

	// Create the command
	cmd := exec.Command(firecrackerBin,
		"--api-sock", socketPath,
		"--config-file", configPath,
	)

	// Set up process attributes for maximum independence
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,  // Create new session
		Setpgid:    true,  // Create new process group
		Pgid:       0,     // Use process PID as group ID
		Noctty:     true,  // Don't assign controlling terminal
		Foreground: false, // Run in background
	}

	// Redirect output to log file
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.Stdin = nil

	// Start the process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start firecracker process: %w", err)
	}

	// Write PID to file
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Release the process - this is critical for independence
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("failed to release process: %w", err)
	}

	return nil
}
