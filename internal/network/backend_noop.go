package network

// noopBackend is a backend that performs no host networking operations. It is
// registered as "noop" and is intended for tests and deployments where VM
// networking is managed entirely outside vmlet.
type noopBackend struct{}

// NewNoopBackend returns a backend that accepts all operations without
// touching bridges, DHCP, iptables or SSH state.
func NewNoopBackend() Backend {
	return &noopBackend{}
}

func (*noopBackend) Initialize() error { return nil }
func (*noopBackend) Shutdown() error   { return nil }

func (*noopBackend) AllocateVMNetwork(vmID, macAddress, networkID string) (*VMNetworkInfo, error) {
	return &VMNetworkInfo{VMID: vmID, MACAddress: macAddress}, nil
}

func (*noopBackend) ReleaseVMNetwork(string, string) error { return nil }

func (*noopBackend) ValidateNetworkSetup() error { return nil }

func (*noopBackend) GetNetworkStatus() (*NetworkStatus, error) {
	return &NetworkStatus{}, nil
}

func (*noopBackend) TestVMConnectivity(string) error { return nil }

func (*noopBackend) GetVMNetworkInfo(vmID string) (*VMNetworkInfo, error) {
	return &VMNetworkInfo{VMID: vmID}, nil
}
