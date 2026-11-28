package network

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Network backend identifiers. "bridge" is the current single-node backend;
// "ovn" is reserved for the Phase 1 OVN overlay (roadmap item 1.2).
const (
	BackendBridge = "bridge"
	BackendOVN    = "ovn"
	BackendNoop   = "noop"

	// BackendEnvVar selects the default network backend at process start. It is
	// a default, not an exclusive choice: a host can also run the OVN overlay
	// alongside the bridge backend and let each VM pick.
	BackendEnvVar = "MICROVM_NETWORK_BACKEND"

	// OverlayIPEnvVar and OverlayCIDREnvVar set the OVN overlay's subnet, kept
	// separate from the NAT bridge subnet on a host that runs both.
	OverlayIPEnvVar   = "MICROVM_OVN_NETWORK_IP"
	OverlayCIDREnvVar = "MICROVM_OVN_NETWORK_CIDR"
)

// Backend is the contract every network implementation must satisfy. It covers
// host-level setup/teardown and the per-VM network lifecycle, so callers do not
// depend on how a VM's connectivity is actually realized (bridge, OVN, ...).
type Backend interface {
	Initialize() error
	Shutdown() error
	AllocateVMNetwork(vmID, macAddress, networkID string) (*VMNetworkInfo, error)
	ReleaseVMNetwork(vmID, macAddress string) error
	ValidateNetworkSetup() error
	GetNetworkStatus() (*NetworkStatus, error)
	TestVMConnectivity(vmID string) error
	GetVMNetworkInfo(vmID string) (*VMNetworkInfo, error)
}

// BackendFactory constructs a backend from network configuration.
type BackendFactory func(*NetworkConfig) (Backend, error)

var (
	backendMu        sync.RWMutex
	backendFactories = map[string]BackendFactory{}
)

// RegisterBackend makes a backend selectable by name. Registering the same name
// twice is an error so a typo cannot silently replace a built-in backend.
func RegisterBackend(name string, factory BackendFactory) error {
	name = normalizeBackendName(name)
	if name == "" {
		return fmt.Errorf("network: backend name must not be empty")
	}
	if factory == nil {
		return fmt.Errorf("network: factory for backend %q must not be nil", name)
	}

	backendMu.Lock()
	defer backendMu.Unlock()
	if _, exists := backendFactories[name]; exists {
		return fmt.Errorf("network: backend %q is already registered", name)
	}
	backendFactories[name] = factory
	return nil
}

// ListBackends returns the registered backend names in stable order.
func ListBackends() []string {
	backendMu.RLock()
	defer backendMu.RUnlock()
	names := make([]string, 0, len(backendFactories))
	for name := range backendFactories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SelectBackendName resolves the effective backend name with the precedence:
// explicit NetworkConfig.Backend, then MICROVM_NETWORK_BACKEND, then "bridge".
// The bridge default keeps single-node installs unchanged until OVN is GA.
func SelectBackendName(config *NetworkConfig) string {
	if config != nil {
		if name := normalizeBackendName(config.Backend); name != "" {
			return name
		}
	}
	if name := normalizeBackendName(os.Getenv(BackendEnvVar)); name != "" {
		return name
	}
	return BackendBridge
}

// NewBackend constructs the named backend. An unknown name is reported with the
// list of registered backends to make configuration mistakes easy to diagnose.
func NewBackend(config *NetworkConfig, name string) (Backend, error) {
	name = normalizeBackendName(name)
	if name == "" {
		return nil, fmt.Errorf("network: backend name must not be empty")
	}

	backendMu.RLock()
	factory, ok := backendFactories[name]
	backendMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("network: unknown backend %q (registered: %s)", name, strings.Join(ListBackends(), ", "))
	}
	return factory(config)
}

// NewNetworkBackend constructs the backend selected by SelectBackendName.
func NewNetworkBackend(config *NetworkConfig) (Backend, error) {
	return NewBackend(config, SelectBackendName(config))
}

func normalizeBackendName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func init() {
	// The existing bridge implementation becomes the default pluggable backend.
	// NewNetworkManager is unchanged, so bridge behavior is preserved verbatim.
	mustRegisterBackend(BackendBridge, func(config *NetworkConfig) (Backend, error) {
		if config == nil {
			return nil, fmt.Errorf("network: nil config for %q backend", BackendBridge)
		}
		return NewNetworkManager(config), nil
	})

	// A no-op backend is useful for tests and hosts that manage VM networking
	// externally. It is a second, fully functional registrable implementation.
	mustRegisterBackend(BackendNoop, func(*NetworkConfig) (Backend, error) {
		return NewNoopBackend(), nil
	})

	// The OVN backend is implemented in backend_ovn.go and registers itself
	// there; it is deliberately not registered here so the two cannot drift.
}

func mustRegisterBackend(name string, factory BackendFactory) {
	if err := RegisterBackend(name, factory); err != nil {
		panic(err)
	}
}
