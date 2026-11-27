package network

import (
	"strings"
	"testing"
)

// cleanupBackendName removes any pre-existing registration before a test and
// restores that state afterwards, so tests never leak entries into the registry.
func cleanupBackendName(t *testing.T, name string) {
	t.Helper()
	backendMu.Lock()
	previous, existed := backendFactories[name]
	delete(backendFactories, name)
	backendMu.Unlock()

	t.Cleanup(func() {
		backendMu.Lock()
		if existed {
			backendFactories[name] = previous
		} else {
			delete(backendFactories, name)
		}
		backendMu.Unlock()
	})
}

func TestSelectBackendName(t *testing.T) {
	tests := []struct {
		name   string
		config *NetworkConfig
		env    string
		setEnv bool
		want   string
	}{
		{
			name: "default is bridge",
			want: BackendBridge,
		},
		{
			name:   "environment variable is honored",
			env:    BackendOVN,
			setEnv: true,
			want:   BackendOVN,
		},
		{
			name:   "environment value is normalized",
			env:    "  NOOP  ",
			setEnv: true,
			want:   BackendNoop,
		},
		{
			name:   "explicit config overrides environment",
			config: &NetworkConfig{Backend: BackendNoop},
			env:    BackendOVN,
			setEnv: true,
			want:   BackendNoop,
		},
		{
			name:   "blank config falls through to environment",
			config: &NetworkConfig{Backend: "   "},
			env:    BackendNoop,
			setEnv: true,
			want:   BackendNoop,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(BackendEnvVar, "")
			if tt.setEnv {
				t.Setenv(BackendEnvVar, tt.env)
			}
			if got := SelectBackendName(tt.config); got != tt.want {
				t.Fatalf("SelectBackendName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewNetworkBackendSelection(t *testing.T) {
	t.Run("default returns the bridge implementation", func(t *testing.T) {
		t.Setenv(BackendEnvVar, "")
		backend, err := NewNetworkBackend(&NetworkConfig{ConfigDir: t.TempDir()})
		if err != nil {
			t.Fatalf("NewNetworkBackend() error: %v", err)
		}
		if _, ok := backend.(*NetworkManager); !ok {
			t.Fatalf("backend = %T, want *NetworkManager", backend)
		}
	})

	t.Run("environment selects the noop implementation", func(t *testing.T) {
		t.Setenv(BackendEnvVar, BackendNoop)
		backend, err := NewNetworkBackend(&NetworkConfig{ConfigDir: t.TempDir()})
		if err != nil {
			t.Fatalf("NewNetworkBackend() error: %v", err)
		}
		if _, ok := backend.(*noopBackend); !ok {
			t.Fatalf("backend = %T, want *noopBackend", backend)
		}
	})

	t.Run("explicit config overrides environment", func(t *testing.T) {
		t.Setenv(BackendEnvVar, BackendBridge)
		backend, err := NewNetworkBackend(&NetworkConfig{Backend: BackendNoop})
		if err != nil {
			t.Fatalf("NewNetworkBackend() error: %v", err)
		}
		if _, ok := backend.(*noopBackend); !ok {
			t.Fatalf("backend = %T, want *noopBackend", backend)
		}
	})

	t.Run("ovn is a real backend", func(t *testing.T) {
		// Selection must now succeed: item 1.1 reserved the name and the OVN
		// backend implementation filled it in. Connecting to OVS/OVN still
		// happens in Initialize(), not here, so construction cannot fail.
		t.Setenv(BackendEnvVar, BackendOVN)
		backend, err := NewNetworkBackend(&NetworkConfig{BridgeCIDR: "10.0.0.0/24"})
		if err != nil {
			t.Fatalf("NewNetworkBackend() error: %v", err)
		}
		if _, ok := backend.(*OVNBackend); !ok {
			t.Fatalf("backend = %T, want *OVNBackend", backend)
		}
		// ...and using it without Initialize must say so, not panic.
		if err := backend.ValidateNetworkSetup(); err == nil {
			t.Fatal("ValidateNetworkSetup should fail before Initialize")
		}
	})

	t.Run("unknown backend lists registered names", func(t *testing.T) {
		t.Setenv(BackendEnvVar, "does-not-exist")
		_, err := NewNetworkBackend(&NetworkConfig{})
		if err == nil || !strings.Contains(err.Error(), BackendBridge) {
			t.Fatalf("error = %v, want unknown-backend error listing the bridge", err)
		}
	})

	t.Run("empty name is rejected", func(t *testing.T) {
		if _, err := NewBackend(&NetworkConfig{}, "  "); err == nil {
			t.Fatal("NewBackend accepted an empty backend name")
		}
	})
}

func TestRegisterBackend(t *testing.T) {
	factory := func(*NetworkConfig) (Backend, error) { return NewNoopBackend(), nil }
	cleanupBackendName(t, "test-custom")
	cleanupBackendName(t, "test-duplicate")

	if err := RegisterBackend("test-custom", factory); err != nil {
		t.Fatalf("RegisterBackend(test-custom) error: %v", err)
	}

	backend, err := NewBackend(&NetworkConfig{}, "test-custom")
	if err != nil {
		t.Fatalf("NewBackend(test-custom) error: %v", err)
	}
	if backend == nil {
		t.Fatal("NewBackend returned nil")
	}

	if err := RegisterBackend("test-duplicate", factory); err != nil {
		t.Fatalf("RegisterBackend(test-duplicate) error: %v", err)
	}
	if err := RegisterBackend("test-duplicate", factory); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate RegisterBackend error = %v, want 'already registered'", err)
	}
	if err := RegisterBackend("  ", factory); err == nil {
		t.Fatal("RegisterBackend accepted an empty name")
	}
	if err := RegisterBackend("nil-factory", nil); err == nil {
		t.Fatal("RegisterBackend accepted a nil factory")
	}

	names := strings.Join(ListBackends(), ",")
	for _, want := range []string{BackendBridge, BackendNoop, BackendOVN, "test-custom", "test-duplicate"} {
		if !strings.Contains(names, want) {
			t.Fatalf("ListBackends() = %q, want it to contain %q", names, want)
		}
	}
}

func TestNoopBackend(t *testing.T) {
	var backend Backend = NewNoopBackend()
	config := &NetworkConfig{}

	if err := backend.Initialize(); err != nil {
		t.Fatalf("Initialize() error: %v", err)
	}
	if err := backend.ValidateNetworkSetup(); err != nil {
		t.Fatalf("ValidateNetworkSetup() error: %v", err)
	}
	if err := backend.TestVMConnectivity("vm-1"); err != nil {
		t.Fatalf("TestVMConnectivity() error: %v", err)
	}
	if err := backend.ReleaseVMNetwork("vm-1", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ReleaseVMNetwork() error: %v", err)
	}
	if err := backend.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	info, err := backend.AllocateVMNetwork("vm-1", "aa:bb:cc:dd:ee:ff", "")
	if err != nil {
		t.Fatalf("AllocateVMNetwork() error: %v", err)
	}
	if info.VMID != "vm-1" || info.MACAddress != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("AllocateVMNetwork() = %+v, want the VM id and MAC preserved", info)
	}

	status, err := backend.GetNetworkStatus()
	if err != nil {
		t.Fatalf("GetNetworkStatus() error: %v", err)
	}
	if status == nil {
		t.Fatal("GetNetworkStatus() returned nil")
	}

	vmInfo, err := backend.GetVMNetworkInfo("vm-1")
	if err != nil {
		t.Fatalf("GetVMNetworkInfo() error: %v", err)
	}
	if vmInfo.VMID != "vm-1" {
		t.Fatalf("GetVMNetworkInfo() = %+v, want VMID vm-1", vmInfo)
	}

	// The noop backend must also be constructible through the registry.
	if _, err := NewBackend(config, BackendNoop); err != nil {
		t.Fatalf("NewBackend(noop) error: %v", err)
	}
}
