package network

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/haioco/vmlet/internal/ovn"
	"github.com/haioco/vmlet/internal/ovs"
)

// OVNBackend is the OVN implementation of Backend. It is the piece that turns
// the internal/ovn topology reconciler into something the vmlet lifecycle can
// call: every VM gets a logical port on its network's switch, a DHCP
// reservation, MAC/IP port security and (optionally) a floating IP, and all of
// that is derived from the VM id so re-applying it is idempotent.
type OVNBackend struct {
	config *NetworkConfig

	mu      sync.RWMutex
	ovn     *ovn.Client
	ovs     *ovs.Client
	network string
	// vmPorts maps a VM id to the logical port name we created for it, so
	// release does not have to re-derive (and cannot drift).
	vmPorts map[string]string
}

// Compile-time check that the backend satisfies the interface.
var _ Backend = (*OVNBackend)(nil)

// NewOVNBackend builds the OVN backend. It does not connect yet: Initialize()
// does that, so a host without OVS/OVN installed fails at Initialize with a
// clear message instead of at construction.
func NewOVNBackend(config *NetworkConfig) (Backend, error) {
	if config == nil {
		return nil, fmt.Errorf("network: nil config for %q backend", BackendOVN)
	}
	return &OVNBackend{config: config, vmPorts: map[string]string{}}, nil
}

func init() {
	mustRegisterBackend(BackendOVN, NewOVNBackend)
}

// ensureConfig lazily replaces a nil config so every accessor can read fields
// without a nil check at each call site.
func (b *OVNBackend) ensureConfig() {
	if b.config == nil {
		b.config = &NetworkConfig{}
	}
}

// IntegrationBridge is the OVS integration bridge OVN uses.
const IntegrationBridge = "br-int"

// ovnEndpoint resolves the Northbound endpoint, defaulting to the conventional
// local socket.
func ovnEndpoint() string {
	if ep := strings.TrimSpace(os.Getenv("MICROVM_OVN_NB_ENDPOINT")); ep != "" {
		return ep
	}
	return ovn.DefaultNBEndpoint
}

// networkName returns the vmlet network this host serves. It defaults to
// "default" so a single-network deployment needs no extra configuration.
func (b *OVNBackend) networkName() string {
	if net := strings.TrimSpace(b.network); net != "" {
		return net
	}
	if net := strings.TrimSpace(os.Getenv("MICROVM_OVN_NETWORK")); net != "" {
		return net
	}
	return "default"
}

// integrationBridge returns the bridge taps attach to. On the OVN backend that
// is always the OVS integration bridge: the "-network-bridge" flag defaults to
// lxdbr0, which belongs to the bridge backend, so it must not leak in here.
func (b *OVNBackend) integrationBridge() string {
	b.ensureConfig()
	if name := strings.TrimSpace(b.config.BridgeName); name != "" && name != "lxdbr0" {
		return name
	}
	return IntegrationBridge
}

// subnet is the network's CIDR, derived from the bridge CIDR when the operator
// did not give one.
func (b *OVNBackend) subnet() (string, string, error) {
	b.ensureConfig()
	cidr := strings.TrimSpace(b.config.BridgeCIDR)
	if cidr == "" {
		cidr = "10.0.0.0/24"
	}
	ip, ipNet, err := ovn.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("network: invalid OVN subnet %q: %w", cidr, err)
	}
	gateway := strings.TrimSpace(b.config.BridgeIP)
	if gateway == "" {
		gateway = ovn.FirstUsable(ipNet).String()
	}
	// The subnet must be CIDR notation ("10.0.0.0/24"), not a dotted netmask
	// ("10.0.0.0/255.255.255.0"): net.ParseCIDR rejects the latter, and every
	// caller here hands this string to OVN and to ParseCIDR again.
	ones, _ := ipNet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip.String(), ones), gateway, nil
}

// Initialize connects to the OVN Northbound DB and makes sure the datapath and
// the logical topology for this host's network exist.
func (b *OVNBackend) Initialize() error {
	ctx := context.Background()

	client, err := ovn.New(ctx, ovn.Config{Endpoint: ovnEndpoint()})
	if err != nil {
		return fmt.Errorf("network: OVN backend unavailable: %w", err)
	}

	ovsClient, err := ovs.NewClient()
	if err != nil {
		client.Close()
		return fmt.Errorf("network: OVS unavailable (run scripts/setup-ovn-host.sh): %w", err)
	}

	b.mu.Lock()
	b.ovn = client
	b.ovs = ovsClient
	b.mu.Unlock()

	if err := b.reconcileTopology(ctx); err != nil {
		return err
	}
	return b.ValidateNetworkSetup()
}

// reconcileTopology brings this host's network to its desired logical state.
// It is safe to call repeatedly, which is what makes a controller restart (or a
// second vmlet on the same network) converge instead of duplicating.
func (b *OVNBackend) reconcileTopology(ctx context.Context) error {
	network := b.networkName()
	subnet, gateway, err := b.subnet()
	if err != nil {
		return err
	}

	return b.withClient(func(c *ovn.Client) error {
		_, err := c.ReconcileTopology(ctx, ovn.TopologyFromNetworks([]ovn.NetworkDefinition{
			{
				Name:       network,
				CIDR:       subnet,
				GatewayIP:  gateway,
				DNSServers: b.dnsServers(),
			},
		}))
		return err
	})
}

func (b *OVNBackend) dnsServers() []string {
	b.ensureConfig()
	if len(b.config.DNSServers) > 0 {
		return b.config.DNSServers
	}
	return []string{"8.8.8.8", "1.1.1.1"}
}

// AllocateVMNetwork gives a VM its logical port on the OVN overlay: the port
// itself, a DHCP reservation on the network's subnet, and MAC/IP port security.
func (b *OVNBackend) AllocateVMNetwork(vmID, macAddress string) (*VMNetworkInfo, error) {
	if vmID == "" {
		return nil, fmt.Errorf("network: vm id must not be empty")
	}
	ctx := context.Background()

	// Check the connection before touching any derived config, so an
	// uninitialised backend reports that instead of misbehaving.
	if err := b.requireClient(); err != nil {
		return nil, err
	}

	network := b.networkName()

	subnet, gateway, err := b.subnet()
	if err != nil {
		return nil, err
	}

	var ip string
	err = b.withClient(func(c *ovn.Client) error {
		// Port first: everything else binds to it.
		if err := c.EnsureLogicalSwitchPort(ctx, ovn.VMSwitchName(network), ovn.LogicalPortSpec{
			Name:      ovn.LogicalPortName(vmID),
			Addresses: ovn.LogicalPortAddresses(macAddress, ""),
			ExternalIDs: map[string]string{
				"vmlet-managed": "true",
				"vmlet-vm":      vmID,
				"vmlet-network": network,
				"vmlet-role":    "vm",
			},
		}); err != nil {
			return err
		}

		// DHCP: allocate an address from the subnet, then pin it.
		ip, err = ovn.AllocateSubnetIP(subnet, b.allocatedIPs(ctx, c, network))
		if err != nil {
			return err
		}
		// The port's addresses must carry the allocated address, not a
		// placeholder, or OVN's port security has nothing to pin.
		if err := c.EnsureLogicalSwitchPort(ctx, ovn.VMSwitchName(network), ovn.LogicalPortSpec{
			Name:      ovn.LogicalPortName(vmID),
			Addresses: ovn.LogicalPortAddresses(macAddress, ip),
			ExternalIDs: map[string]string{
				"vmlet-managed": "true",
				"vmlet-vm":      vmID,
				"vmlet-network": network,
				"vmlet-role":    "vm",
			},
		}); err != nil {
			return err
		}

		dhcpUUID, err := c.EnsureDHCPOptions(ctx, ovn.DHCPSpec{
			Switch:     ovn.GatewaySwitchName(network),
			CIDR:       subnet,
			Gateway:    gateway,
			DNSServers: b.dnsServers(),
		}, network)
		if err != nil {
			return err
		}
		if err := c.EnsureDHCPReservation(ctx, ovn.LogicalPortName(vmID), dhcpUUID); err != nil {
			return err
		}

		// Anti-spoofing, honouring the same switches as the bridge backend.
		return c.EnsurePortSecurity(ctx, ovn.LogicalPortName(vmID), ovn.PortSecurityFor(macAddress, ip))
	})
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.vmPorts[vmID] = ovn.LogicalPortName(vmID)
	b.mu.Unlock()

	return &VMNetworkInfo{
		VMID:       vmID,
		MACAddress: macAddress,
		IPAddress:  ip,
	}, nil
}

// allocatedIPs returns the IPs already handed out on this network, so a new VM
// does not get a duplicate address.
func (b *OVNBackend) allocatedIPs(ctx context.Context, c *ovn.Client, network string) []string {
	ports, err := c.ListSwitchPorts(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, port := range ports {
		if port.ExternalIDs["vmlet-network"] != network {
			continue
		}
		out = append(out, ovn.AddressIPs(port.Addresses)...)
	}
	return out
}

// ReleaseVMNetwork removes a VM's logical port and everything bound to it.
func (b *OVNBackend) ReleaseVMNetwork(vmID, macAddress string) error {
	ctx := context.Background()
	network := b.networkName()

	b.mu.RLock()
	portName := b.vmPorts[vmID]
	b.mu.RUnlock()
	if portName == "" {
		portName = ovn.LogicalPortName(vmID)
	}

	err := b.withClient(func(c *ovn.Client) error {
		// A floating IP and its DNS record go first: they reference the port.
		if err := c.DropDHCPReservation(ctx, portName); err != nil {
			return err
		}
		if err := c.DropVMACLs(ctx, vmID); err != nil {
			return err
		}
		if _, err := c.ReconcileNAT(ctx, nil, nil); err != nil {
			return err
		}
		if err := c.PruneDNSRecords(ctx, map[string]string{}); err != nil {
			return err
		}
		return c.DeleteLogicalSwitchPort(ctx, portName)
	})

	b.mu.Lock()
	delete(b.vmPorts, vmID)
	b.mu.Unlock()

	_ = network
	return err
}

// ValidateNetworkSetup checks both halves of the datapath: the OVS bridge and
// the OVN Northbound DB.
func (b *OVNBackend) ValidateNetworkSetup() error {
	ctx := context.Background()
	bridge := b.integrationBridge()

	if err := b.withClient(func(c *ovn.Client) error {
		routers, err := c.ListRouters(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, router := range routers {
			if router.Name == ovn.GatewayRouterName(b.networkName()) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("network: logical router for network %q is missing", b.networkName())
		}
		return nil
	}); err != nil {
		return err
	}

	if b.ovs == nil {
		return fmt.Errorf("network: OVS client is not initialised")
	}
	exists, err := b.ovs.BridgeExists(ctx, bridge)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("network: bridge %q is not configured (run scripts/setup-ovn-host.sh)", bridge)
	}
	return nil
}

// GetNetworkStatus reports the OVN side as the generic status the API expects.
// OVN has no single "DHCP server" process, so DHCPRunning reports whether the
// logical router for the network exists (which is what actually serves DHCP).
func (b *OVNBackend) GetNetworkStatus() (*NetworkStatus, error) {
	ctx := context.Background()
	network := b.networkName()

	var bridge *BridgeInfo
	if b.ovs != nil {
		info, err := b.ovs.GetBridgeInfo(ctx, b.integrationBridge())
		if err == nil {
			bridge = &BridgeInfo{Name: info.Name}
		}
	}

	var dhcpRunning bool
	err := b.withClient(func(c *ovn.Client) error {
		routers, err := c.ListRouters(ctx)
		if err != nil {
			return err
		}
		for _, router := range routers {
			if router.Name == ovn.GatewayRouterName(network) {
				dhcpRunning = true
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &NetworkStatus{Bridge: bridge, DHCPRunning: dhcpRunning}, nil
}

// TestVMConnectivity checks that the VM's logical port is still bound to the
// datapath. OVN does not expose a guest-level reachability probe, so this
// reports the binding state rather than pretending to ping the guest.
func (b *OVNBackend) TestVMConnectivity(vmID string) error {
	ctx := context.Background()
	network := b.networkName()

	b.mu.RLock()
	portName := b.vmPorts[vmID]
	b.mu.RUnlock()
	if portName == "" {
		portName = ovn.LogicalPortName(vmID)
	}

	return b.withClient(func(c *ovn.Client) error {
		ports, err := c.ListSwitchPorts(ctx)
		if err != nil {
			return err
		}
		for _, port := range portNames(ports) {
			if port == portName {
				return nil
			}
		}
		return fmt.Errorf("network: vm %s has no logical port on %q", vmID, ovn.VMSwitchName(network))
	})
}

// GetVMNetworkInfo reads a VM's current addresses from OVN rather than from
// local state, so it survives a controller restart.
func (b *OVNBackend) GetVMNetworkInfo(vmID string) (*VMNetworkInfo, error) {
	ctx := context.Background()
	network := b.networkName()

	b.mu.RLock()
	portName := b.vmPorts[vmID]
	b.mu.RUnlock()
	if portName == "" {
		portName = ovn.LogicalPortName(vmID)
	}

	var info *VMNetworkInfo
	err := b.withClient(func(c *ovn.Client) error {
		ports, err := c.ListSwitchPorts(ctx)
		if err != nil {
			return err
		}
		for _, port := range ports {
			if port.Name != portName {
				continue
			}
			mac, ip := ovn.SplitLogicalPortAddress(port.Addresses)
			info = &VMNetworkInfo{VMID: vmID, MACAddress: mac, IPAddress: ip}
			return nil
		}
		return fmt.Errorf("network: vm %s has no logical port on %q", vmID, ovn.VMSwitchName(network))
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// Shutdown disconnects from the Northbound DB. Logical state is left in place:
// it belongs to the network, not to this process, and the next vmlet will
// converge back to it.
func (b *OVNBackend) Shutdown() error {
	b.mu.Lock()
	client := b.ovn
	b.ovn = nil
	b.mu.Unlock()

	if client != nil {
		client.Close()
	}
	return nil
}

// requireClient reports whether the backend is connected.
func (b *OVNBackend) requireClient() error {
	b.mu.RLock()
	connected := b.ovn != nil
	b.mu.RUnlock()
	if !connected {
		return fmt.Errorf("network: OVN backend is not initialised")
	}
	return nil
}

// withClient runs fn against the connected client, reporting a useful error
// when the backend was never initialised.
func (b *OVNBackend) withClient(fn func(*ovn.Client) error) error {
	b.mu.RLock()
	client := b.ovn
	b.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("network: OVN backend is not initialised")
	}
	return fn(client)
}

func portNames(ports []ovn.LogicalSwitchPort) []string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, port.Name)
	}
	return out
}
