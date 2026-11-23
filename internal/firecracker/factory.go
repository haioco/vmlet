package firecracker

import (
	"context"
	"fmt"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
)

// Factory creates and configures firecracker components
type Factory struct {
	config *Config
}

// NewFactory creates a new firecracker factory
func NewFactory(config *Config) *Factory {
	return &Factory{
		config: config,
	}
}

// CreateManager creates a new firecracker manager with all dependencies
func (f *Factory) CreateManager() (api.MicroVMManagerInterface, error) {
	return NewManager(f.config, nil) // Pass nil for reservationMgr in factory
}

// DefaultConfig returns a default configuration for firecracker
func DefaultConfig() *Config {
	return &Config{
		WorkDir:        "/var/lib/firecracker",
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		KernelPath:     "/opt/firecracker/vmlinux",
		RootfsPath:     "/opt/firecracker/rootfs.ext4",
		UseJailer:      false, // Disable jailer for development
		JailerUID:      1000,
		JailerGID:      1000,
	}
}

// LocalDevConfig returns a configuration suitable for local development
func LocalDevConfig() *Config {
	return &Config{
		WorkDir:        "./firecracker-work",
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		KernelPath:     "./hello-vmlinux.bin", // Use local kernel
		RootfsPath:     "./rootfs.ext4",
		UseJailer:      false, // Disable jailer for development
		JailerUID:      1000,
		JailerGID:      1000,
	}
}

// ValidateConfig validates a firecracker configuration
func ValidateConfig(config *Config) error {
	if config == nil {
		return fmt.Errorf("config cannot be nil")
	}

	if config.WorkDir == "" {
		return fmt.Errorf("work directory must be specified")
	}

	if config.FirecrackerBin == "" {
		return fmt.Errorf("firecracker binary path must be specified")
	}

	if config.UseJailer && config.JailerBin == "" {
		return fmt.Errorf("jailer binary path must be specified when jailer is enabled")
	}

	return nil
}

// MicroVMManagerAdapter adapts the firecracker Manager to the interface
type MicroVMManagerAdapter struct {
	manager *Manager
}

// NewMicroVMManagerAdapter creates a new adapter
func NewMicroVMManagerAdapter(config *Config) (*MicroVMManagerAdapter, error) {
	manager, err := NewManager(config, nil) // Pass nil for reservationMgr in adapter
	if err != nil {
		return nil, err
	}

	return &MicroVMManagerAdapter{
		manager: manager,
	}, nil
}

// CreateMicroVM implements the interface
func (a *MicroVMManagerAdapter) CreateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return a.manager.CreateMicroVM(ctx, microvm)
}

// StartMicroVM implements the interface
func (a *MicroVMManagerAdapter) StartMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return a.manager.StartMicroVM(ctx, microvm)
}

// StopMicroVM implements the interface
func (a *MicroVMManagerAdapter) StopMicroVM(ctx context.Context, microvm *models.MicroVMInstance, force bool) error {
	return a.manager.StopMicroVM(ctx, microvm, force)
}

// DeleteMicroVM implements the interface
func (a *MicroVMManagerAdapter) DeleteMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	return a.manager.DeleteMicroVM(ctx, microvm)
}

// GetMicroVMStatus implements the interface
func (a *MicroVMManagerAdapter) GetMicroVMStatus(ctx context.Context, microvmID string) (models.MicroVMStatus, error) {
	return a.manager.GetMicroVMStatus(ctx, microvmID)
}
