package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/ovn"
	"github.com/haioco/vmlet/internal/ovs"
)

// OVNBackend is the OVN implementation of Backend. It is the piece that turns
// the internal/ovn topology reconciler into something the vmlet lifecycle can
// call: every VM gets a logical port on its network's switch, a DHCP
// reservation, MAC/IP port security and (optionally) a floating IP, and all of
// that is derived from the VM id so re-applying it is idempotent.
// OVNNetwork is one VPC: a logical switch + subnet + gateway + DNS.
type OVNNetwork struct {
	ID         string
	Name       string
	CIDR       string
	Gateway    string
	DNSServers []string
	// ExternalIP is the address VM traffic is masqueraded to for outbound
	// internet. Empty disables SNAT for this VPC.
	ExternalIP string
}

// PortForward maps an external port on a public IP to an internal port on a VM.
type PortForward struct {
	VMID         string
	ExternalIP   string
	ExternalPort int
	InternalPort int
	Protocol     string // "tcp" or "udp"
}

type OVNBackend struct {
	config *NetworkConfig

	mu             sync.RWMutex
	ovn            *ovn.Client
	ovs            *ovs.Client
	network        string
	networks       map[string]*OVNNetwork           // network ID -> definition
	vmNetwork      map[string]string                // vmID -> network ID
	vmStaticIP     map[string]string                // vmID -> user-requested static IP
	vmPortForwards map[string][]PortForward         // vmID -> port forwards
	vmSecRules     map[string][]models.SecurityRule // vmID -> custom ACL rules
	// vmPorts maps a VM id to the logical port name we created for it, so
	// release does not have to re-derive (and cannot drift).
	vmPorts map[string]string
	// vmFloating maps a VM id to its public addresses.
	vmFloating map[string]map[string]string
	// vmMACs caches each VM's MAC for the DNAT rows' ARP answers.
	vmMACs map[string]string
	// vmAddresses caches each VM's private address.
	vmAddresses map[string]string
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
	return &OVNBackend{
		config:         config,
		vmPorts:        map[string]string{},
		vmFloating:     map[string]map[string]string{},
		vmMACs:         map[string]string{},
		vmAddresses:    map[string]string{},
		networks:       map[string]*OVNNetwork{},
		vmNetwork:      map[string]string{},
		vmStaticIP:     map[string]string{},
		vmPortForwards: map[string][]PortForward{},
		vmSecRules:     map[string][]models.SecurityRule{},
	}, nil
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

// PeerNetworks connects two VPCs so their routers exchange routes: a pair of
// logical router ports, one on each router, with no SNAT, which is how OVN
// expresses inter-VPC routing (the AWS VPC peering equivalent).
func (b *OVNBackend) PeerNetworks(ctx context.Context, aID, bID string) error {
	a := b.networkDef(aID).Name
	bb := b.networkDef(bID).Name
	if a == "" || bb == "" {
		return fmt.Errorf("network: both VPCs must exist to peer them")
	}
	if a == bb {
		return fmt.Errorf("network: cannot peer a VPC with itself")
	}
	subnetA, _, _, _, err := b.networkSubnetFor(aID)
	if err != nil {
		return err
	}
	subnetA2, _, _, _, err := b.networkSubnetFor(bID)
	if err != nil {
		return err
	}
	_ = subnetA2

	return b.withClient(func(c *ovn.Client) error {
		_, err := c.PeerRouters(ctx, ovn.GatewayRouterName(a), ovn.GatewayRouterName(bb), subnetA, subnetA2)
		return err
	})
}

// SetNetworks replaces the set of VPCs this backend serves. Called by the
// manager after it loads networks from storage. A network with an empty ID is
// treated as the default ("default").
func (b *OVNBackend) SetNetworks(networks []*OVNNetwork) error {
	b.mu.Lock()
	b.networks = map[string]*OVNNetwork{}
	for _, n := range networks {
		if n == nil {
			continue
		}
		if n.ID == "" {
			n.ID = "default"
		}
		if n.Name == "" {
			n.Name = n.ID
		}
		b.networks[n.ID] = n
	}
	// Ensure a default network always exists.
	if _, ok := b.networks["default"]; !ok {
		b.networks["default"] = &OVNNetwork{
			ID:   "default",
			Name: "default",
		}
	}

	connected := b.ovn != nil
	b.mu.Unlock()

	// Bring the database up to date right away, so a VPC is usable as soon as it
	// is created rather than at the next restart. reconcileTopology takes its
	// own read lock, hence the unlock first.
	if connected {
		return b.reconcileTopology(context.Background())
	}
	return nil
}

// SetSecurityRules stages a VM's custom security-group rules. The manager calls
// this before allocation so the rules are re-applied on every boot and survive
// a restart of this process.
func (b *OVNBackend) SetSecurityRules(vmID string, rules []models.SecurityRule) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(rules) == 0 {
		delete(b.vmSecRules, vmID)
		return
	}
	b.vmSecRules[vmID] = rules
}

// SetStaticIP records a user-requested static IP for a VM. Called before
// AllocateVMNetwork; the allocator uses it instead of auto-allocating.
func (b *OVNBackend) SetStaticIP(vmID, ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ip == "" {
		delete(b.vmStaticIP, vmID)
	} else {
		b.vmStaticIP[vmID] = ip
	}
}

// networkForVM returns the network ID a VM belongs to, defaulting to "default".
func (b *OVNBackend) networkForVM(vmID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if id := b.vmNetwork[vmID]; id != "" {
		return id
	}
	return "default"
}

// networkDef returns the definition for a network ID, falling back to the
// default network.
func (b *OVNBackend) networkDef(id string) *OVNNetwork {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if id == "" {
		id = "default"
	}
	if n, ok := b.networks[id]; ok {
		return n
	}
	if n, ok := b.networks["default"]; ok {
		return n
	}
	return &OVNNetwork{ID: "default", Name: "default"}
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

// subnet is the default network's CIDR (backward compat).
func (b *OVNBackend) subnet() (string, string, error) {
	cidr, gw, _, err := b.subnetFor("default")
	return cidr, gw, err
}

// subnetFor returns the CIDR, gateway and DNS servers for a network ID.
func (b *OVNBackend) networkSubnetFor(networkID string) (string, string, []string, string, error) {
	b.ensureConfig()
	n := b.networkDef(networkID)
	externalIP := strings.TrimSpace(n.ExternalIP)

	// For the default network, fall back to the overlay config fields (which
	// themselves fall back to the bridge fields) so existing deployments and
	// tests that set BridgeCIDR/BridgeIP keep working, while a host running
	// both backends can keep the two subnets disjoint.
	cidr := strings.TrimSpace(n.CIDR)
	if cidr == "" && networkID == "default" {
		cidr = strings.TrimSpace(b.overlayCIDR())
	}
	if cidr == "" {
		cidr = "10.0.0.0/24"
	}
	gateway := strings.TrimSpace(n.Gateway)
	if gateway == "" && networkID == "default" {
		gateway = strings.TrimSpace(b.overlayIP())
	}

	ip, ipNet, err := ovn.ParseCIDR(cidr)
	if err != nil {
		return "", "", nil, "", fmt.Errorf("network: invalid OVN subnet %q: %w", cidr, err)
	}
	if gateway == "" {
		gateway = ovn.FirstUsable(ipNet).String()
	}
	ones, _ := ipNet.Mask.Size()

	dns := n.DNSServers
	if len(dns) == 0 {
		dns = []string{"8.8.8.8", "1.1.1.1"}
	}
	return fmt.Sprintf("%s/%d", ip.String(), ones), gateway, dns, externalIP, nil
}

// subnetFor is the legacy 3-value form.
func (b *OVNBackend) subnetFor(networkID string) (string, string, []string, error) {
	cidr, gw, dns, _, err := b.networkSubnetFor(networkID)
	return cidr, gw, dns, err
}

// overlayCIDR is the default network's subnet, preferring the explicit
// OverlayCIDR, then MICROVM_OVN_NETWORK_CIDR, then the shared bridge fields.
// The env var is read here as well as in config so a host can override the
// overlay without touching the NAT subnet when both backends run together.
func (b *OVNBackend) overlayCIDR() string {
	b.ensureConfig()
	if c := strings.TrimSpace(b.config.OverlayCIDR); c != "" {
		return c
	}
	if c := strings.TrimSpace(os.Getenv(OverlayCIDREnvVar)); c != "" {
		return c
	}
	return strings.TrimSpace(b.config.BridgeCIDR)
}

// overlayIP is the default network's gateway address, resolved the same way.
func (b *OVNBackend) overlayIP() string {
	b.ensureConfig()
	if ip := strings.TrimSpace(b.config.OverlayIP); ip != "" {
		return ip
	}
	if ip := strings.TrimSpace(os.Getenv(OverlayIPEnvVar)); ip != "" {
		return ip
	}
	return strings.TrimSpace(b.config.BridgeIP)
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
	b.mu.RLock()
	nets := make([]*OVNNetwork, 0, len(b.networks))
	for _, n := range b.networks {
		nets = append(nets, n)
	}
	b.mu.RUnlock()

	if len(nets) == 0 {
		// Fall back to the default network from env/config.
		nets = append(nets, b.networkDef("default"))
	}

	defs := make([]ovn.NetworkDefinition, 0, len(nets))
	for _, n := range nets {
		cidr, gateway, dns, err := b.subnetFor(n.ID)
		if err != nil {
			return err
		}
		defs = append(defs, ovn.NetworkDefinition{
			Name:       n.Name,
			CIDR:       cidr,
			GatewayIP:  gateway,
			DNSServers: dns,
		})
	}

	topology := ovn.TopologyFromNetworks(defs)
	topology.Prune = ovn.PruneEnabled()
	return b.withClient(func(c *ovn.Client) error {
		_, err := c.ReconcileTopology(ctx, topology)
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
func (b *OVNBackend) AllocateVMNetwork(vmID, macAddress, networkID string) (*VMNetworkInfo, error) {
	if vmID == "" {
		return nil, fmt.Errorf("network: vm id must not be empty")
	}
	ctx := context.Background()

	// Check the connection before touching any derived config, so an
	// uninitialised backend reports that instead of misbehaving.
	if err := b.requireClient(); err != nil {
		return nil, err
	}

	if networkID == "" {
		networkID = "default"
	}
	network := b.networkDef(networkID).Name
	if network == "" {
		network = networkID
	}

	subnet, gateway, dns, externalIP, err := b.networkSubnetFor(networkID)
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

		// DHCP: use a static IP if the user provided one, otherwise allocate
		// from the subnet, skipping in-use addresses and the gateway.
		b.mu.RLock()
		staticIP := b.vmStaticIP[vmID]
		b.mu.RUnlock()
		if staticIP != "" {
			// Validate before using it: an address outside the subnet, or one
			// another VM already holds, would break this VM's connectivity or
			// silently take over its neighbour's.
			inUse := b.allocatedIPs(ctx, c, network)
			if err := ovn.ValidateSubnetIP(subnet, gateway, staticIP, inUse); err != nil {
				return fmt.Errorf("network: vm %s static address %s: %w", vmID, staticIP, err)
			}
			ip = staticIP
		} else {
			ip, err = ovn.AllocateSubnetIP(subnet, b.allocatedIPs(ctx, c, network))
			if err != nil {
				return err
			}
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
			DNSServers: dns,
		}, network)
		if err != nil {
			return err
		}
		if err := c.EnsureDHCPReservation(ctx, ovn.LogicalPortName(vmID), dhcpUUID); err != nil {
			return err
		}

		// Anti-spoofing, honouring the same switches as the bridge backend.
		if err := c.EnsurePortSecurity(ctx, ovn.LogicalPortName(vmID), ovn.PortSecurityFor(macAddress, ip)); err != nil {
			return err
		}

		// Security group ACLs. These are the OVN replacement for the ebtables
		// rules the bridge backend installs per TAP; the default set is
		// allow-only and scoped to the VM's own address, so it adds no deny
		// behaviour the bridge path does not already have.
		for _, rule := range ovn.DefaultVMRules(ip) {
			if err := c.EnsureACL(ctx, ovn.VMSwitchName(network), rule, vmID); err != nil {
				return err
			}
		}

		// Staged custom rules: they must be re-installed on every allocation,
		// not only when the operator POSTs them, or a restart silently drops
		// a VM's deny rules.
		b.mu.RLock()
		custom := b.vmSecRules[vmID]
		b.mu.RUnlock()
		if customRules, err := ToOVNRules(custom); err != nil {
			return err
		} else {
			for _, rule := range customRules {
				if err := c.EnsureACL(ctx, ovn.VMSwitchName(network), rule, vmID); err != nil {
					return err
				}
			}
		}

		// Outbound masquerade: without this a VM with no public address has no
		// path to the internet, which is the AWS NAT Gateway / GCP Cloud NAT
		// role. The whole subnet translates to the VPC's external address.
		if externalIP != "" {
			// The desired set must be complete: ReconcileNAT prunes every
			// vmlet-managed NAT row that is not in it, so passing only the SNAT
			// rule would delete every VM's floating IP.
			if _, err := c.ReconcileNAT(ctx, b.floatingSpecs(""), []ovn.SNATRule{{
				Router:     ovn.GatewayRouterName(network),
				LogicalIP:  subnet,
				ExternalIP: externalIP,
			}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.vmPorts[vmID] = ovn.LogicalPortName(vmID)
	b.vmMACs[vmID] = macAddress
	b.vmAddresses[vmID] = ip
	b.vmNetwork[vmID] = networkID
	delete(b.vmStaticIP, vmID)
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

	b.mu.RLock()
	networkID := b.vmNetwork[vmID]
	portName := b.vmPorts[vmID]
	b.mu.RUnlock()
	if networkID == "" {
		networkID = "default"
	}
	network := b.networkDef(networkID).Name
	if network == "" {
		network = networkID
	}
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
		// Reconcile the VMs that remain: the cache still holds this VM's rows
		// (they are dropped below), so skipping it leaves its NAT rules desired
		// nowhere and pruned while its neighbours' are kept. The SNAT row is
		// desired independently of any VM and must survive release, so it is
		// re-derived here rather than passed as an empty set.
		snatRules, serr := b.snatRules(ctx, c, network)
		if serr != nil {
			return serr
		}
		if _, err := c.ReconcileNAT(ctx, b.floatingSpecs(vmID), snatRules); err != nil {
			return err
		}
		if err := c.PruneDNSRecords(ctx, map[string]string{}); err != nil {
			return err
		}
		return c.DeleteLogicalSwitchPort(ctx, portName)
	})

	b.mu.Lock()
	delete(b.vmPorts, vmID)
	delete(b.vmMACs, vmID)
	delete(b.vmAddresses, vmID)
	delete(b.vmFloating, vmID)
	delete(b.vmNetwork, vmID)
	delete(b.vmSecRules, vmID)
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

// AttachFloatingIP maps a public address onto a VM's private address on the
// logical router, as a DNAT row carrying the VM's MAC so OVN answers ARP for
// the external address. The VM's port must already exist: allocating first is
// what gives us the private address the DNAT targets.
func (b *OVNBackend) AttachFloatingIP(vmID, externalIP string) error {
	if vmID == "" || externalIP == "" {
		return fmt.Errorf("network: vm id and external address are both required")
	}
	ctx := context.Background()

	internalIP, _, err := b.vmEndpoint(ctx, vmID)
	if err != nil {
		return err
	}

	// Record before reconciling. The desired set is derived from this cache,
	// so recording afterwards would reconcile to the previous state and leave
	// the new address unmapped - and, worse, prune the rows that already exist.
	b.mu.Lock()
	if b.vmFloating[vmID] == nil {
		b.vmFloating[vmID] = map[string]string{}
	}
	b.vmFloating[vmID][externalIP] = internalIP
	b.mu.Unlock()

	return b.withClient(func(c *ovn.Client) error {
		_, err := c.ReconcileNAT(ctx, b.floatingSpecs(""), nil)
		return err
	})
}

// DetachFloatingIP withdraws a public address from a VM. Idempotent: detaching
// an address the VM does not hold is a no-op.
func (b *OVNBackend) DetachFloatingIP(vmID, externalIP string) error {
	ctx := context.Background()

	b.mu.Lock()
	port := b.vmPorts[vmID]
	_, held := b.vmFloating[vmID][externalIP]
	delete(b.vmFloating[vmID], externalIP)
	b.mu.Unlock()

	// A VM with no logical port cannot carry a NAT rule, so there is nothing
	// to withdraw. Checking before the cache avoids a pointless DB round trip
	// (and a misleading "not initialised" error) for a never-allocated VM.
	if port == "" {
		return nil
	}

	if !held {
		// Nothing cached: reconcile from the database so a rule left by an
		// earlier process is still withdrawn.
		fromDB, err := b.floatingInDB(ctx, vmID, externalIP)
		if err != nil {
			return err
		}
		if !fromDB {
			return nil
		}
	}

	return b.withClient(func(c *ovn.Client) error {
		_, err := c.ReconcileNAT(ctx, b.floatingSpecs(""), nil)
		return err
	})
}

// ListFloatingIPs returns the public addresses mapped onto a VM.
func (b *OVNBackend) ListFloatingIPs(vmID string) map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.vmFloating[vmID]))
	for pub, priv := range b.vmFloating[vmID] {
		out[pub] = priv
	}
	return out
}

// vmEndpoint returns a VM's private address and MAC, preferring the cache and
// falling back to OVN so it survives a restart of this process.
func (b *OVNBackend) vmEndpoint(ctx context.Context, vmID string) (string, string, error) {
	b.mu.RLock()
	ip := b.vmAddresses[vmID]
	mac := b.vmMACs[vmID]
	port := b.vmPorts[vmID]
	b.mu.RUnlock()

	if ip != "" && mac != "" {
		return ip, mac, nil
	}
	if port == "" {
		return "", "", fmt.Errorf("network: vm %s has no OVN port; allocate it first", vmID)
	}

	if err := b.withClient(func(c *ovn.Client) error {
		ports, err := c.ListSwitchPorts(ctx)
		if err != nil {
			return err
		}
		for _, p := range ports {
			if p.Name != port {
				continue
			}
			pub, priv := "", ""
			if len(p.Addresses) > 0 {
				parts := strings.SplitN(p.Addresses[0], " ", 2)
				if len(parts) > 0 && parts[0] != "" {
					mac = parts[0]
				}
				if len(parts) == 2 {
					priv = strings.TrimSpace(parts[1])
				}
				ip = priv
			}
			_ = pub
			return nil
		}
		return fmt.Errorf("network: logical port %s not found", port)
	}); err != nil {
		return "", "", err
	}
	if ip == "" || mac == "" {
		return "", "", fmt.Errorf("network: vm %s has no address yet", vmID)
	}

	// Cache what the database told us. floatingSpecs derives each DNAT row's MAC
	// from this map, so a restart that reads the address from OVN but does not
	// remember it would silently omit the row.
	b.mu.Lock()
	if b.vmMACs[vmID] == "" {
		b.vmMACs[vmID] = mac
	}
	if b.vmAddresses[vmID] == "" {
		b.vmAddresses[vmID] = ip
	}
	b.mu.Unlock()

	return ip, mac, nil
}

// floatingSpecs builds the DNAT set for every address this host has mapped,
// which is what ReconcileNAT diffs against to add and prune.
//
// Each row carries the MAC and private address of the VM it targets. The rows
// share one logical router, but they are per-VM resources, and one shared MAC
// would have OVN answer ARP for the external address with the wrong guest's
// hardware address.
//
// skipVMID omits one VM's addresses: releasing a VM must reconcile the VMs that
// remain rather than flush the router, or every other VM's public address goes
// with it.
func (b *OVNBackend) floatingSpecs(skipVMID string) []ovn.FloatingIP {
	b.mu.RLock()
	defer b.mu.RUnlock()

	router := ovn.GatewayRouterName(b.networkName())
	var out []ovn.FloatingIP
	for vmID, addrs := range b.vmFloating {
		if vmID == skipVMID {
			continue
		}
		mac := b.vmMACs[vmID]
		if mac == "" {
			continue
		}
		for pub, priv := range addrs {
			if pub == "" || priv == "" {
				continue
			}
			out = append(out, ovn.FloatingIP{
				ExternalIP: pub,
				InternalIP: priv,
				MAC:        mac,
				Router:     router,
			})
		}
	}
	return out
}

// floatingInDB reports whether a DNAT row exists for this VM and address.
func (b *OVNBackend) floatingInDB(ctx context.Context, vmID, externalIP string) (bool, error) {
	found := false
	err := b.withClient(func(c *ovn.Client) error {
		rows, err := c.ListNATs(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Type == ovn.NATTypeDNAT &&
				row.ExternalIP == externalIP &&
				row.ExternalIDs["vmlet-vm"] == vmID {
				found = true
			}
		}
		return nil
	})
	return found, err
}

// AttachPortForward creates a DNAT rule mapping externalIP:externalPort to the
// VM's internal address:internalPort.
func (b *OVNBackend) AttachPortForward(pf PortForward) error {
	if pf.VMID == "" || pf.ExternalIP == "" || pf.ExternalPort == 0 || pf.InternalPort == 0 {
		return fmt.Errorf("network: vm, external address, and both ports are required")
	}
	ctx := context.Background()

	internalIP, _, err := b.vmEndpoint(ctx, pf.VMID)
	if err != nil {
		return err
	}

	b.mu.RLock()
	networkID := b.vmNetwork[pf.VMID]
	mac := b.vmMACs[pf.VMID]
	b.mu.RUnlock()
	if networkID == "" {
		networkID = "default"
	}
	router := ovn.GatewayRouterName(b.networkDef(networkID).Name)

	proto := pf.Protocol
	if proto == "" {
		proto = "tcp"
	}

	err = b.withClient(func(c *ovn.Client) error {
		return c.EnsurePortForward(ctx, ovn.PortForwardSpec{
			ExternalIP:   pf.ExternalIP,
			ExternalPort: pf.ExternalPort,
			InternalIP:   internalIP,
			InternalPort: pf.InternalPort,
			Protocol:     proto,
			MAC:          mac,
			Router:       router,
			VMID:         pf.VMID,
		})
	})
	if err != nil {
		return err
	}

	b.mu.Lock()
	b.vmPortForwards[pf.VMID] = append(b.vmPortForwards[pf.VMID], pf)
	b.mu.Unlock()
	return nil
}

// DetachPortForward removes a port forward rule.
func (b *OVNBackend) DetachPortForward(vmID, externalIP string, externalPort int) error {
	ctx := context.Background()

	err := b.withClient(func(c *ovn.Client) error {
		return c.DropPortForward(ctx, vmID, externalIP, externalPort)
	})
	if err != nil {
		return err
	}

	b.mu.Lock()
	var kept []PortForward
	for _, pf := range b.vmPortForwards[vmID] {
		if pf.ExternalIP == externalIP && pf.ExternalPort == externalPort {
			continue
		}
		kept = append(kept, pf)
	}
	b.vmPortForwards[vmID] = kept
	b.mu.Unlock()
	return nil
}

// ListPortForwards returns the port forwards for a VM.
func (b *OVNBackend) ListPortForwards(vmID string) []PortForward {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]PortForward, len(b.vmPortForwards[vmID]))
	copy(out, b.vmPortForwards[vmID])
	return out
}

// snatRules re-derives the SNAT rows desired for a network from the current
// database, so a release reconcile does not drop a VM's outbound masquerade.
func (b *OVNBackend) snatRules(ctx context.Context, c *ovn.Client, network string) ([]ovn.SNATRule, error) {
	subnet, _, _, externalIP, err := b.networkSubnetFor(networkNameToID(b, network))
	if err != nil {
		return nil, err
	}
	if externalIP == "" {
		return nil, nil
	}
	return []ovn.SNATRule{{
		Router:     ovn.GatewayRouterName(network),
		LogicalIP:  subnet,
		ExternalIP: externalIP,
	}}, nil
}

// networkNameToID maps a network name back to its ID for config lookup.
func networkNameToID(b *OVNBackend, name string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for id, n := range b.networks {
		if n.Name == name {
			return id
		}
	}
	return "default"
}

// DetectExternalIP returns the address this host would present to the internet,
// which is what a VPC's outbound traffic masquerades to. It uses a UDP connect
// (no packets are sent) to read the kernel's route choice; it returns "" when
// there is no route out, so callers treat an empty result as "SNAT disabled".
func DetectExternalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return ""
	}
	return addr.IP.String()
}

// EnsureSNATDefaults fills in the external address for networks that have none,
// so a VPC created without an explicit one still gets outbound internet.
func (b *OVNBackend) EnsureSNATDefaults() string {
	detected := DetectExternalIP()
	if detected == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.networks {
		if strings.TrimSpace(n.ExternalIP) == "" {
			n.ExternalIP = detected
		}
	}
	return detected
}

// ApplySecurityRules installs a VM's custom security-group rules. The baseline
// rules are kept: they are the allow-only floor every VM carries, and the
// custom rules sit above them by priority.
func (b *OVNBackend) ApplySecurityRules(vmID string, rules []models.SecurityRule) error {
	if vmID == "" {
		return fmt.Errorf("network: vm id must not be empty")
	}
	ctx := context.Background()
	if err := b.requireClient(); err != nil {
		return err
	}

	b.mu.RLock()
	networkID := b.vmNetwork[vmID]
	address := b.vmAddresses[vmID]
	b.mu.RUnlock()
	if networkID == "" {
		networkID = "default"
	}
	network := b.networkDef(networkID).Name
	if network == "" {
		network = networkID
	}

	ovnRules, err := ToOVNRules(rules)
	if err != nil {
		return err
	}

	return b.withClient(func(c *ovn.Client) error {
		// Drop first: this replaces the rule set wholesale, and DropVMACLs is
		// keyed by this VM's external_ids, so it cannot touch a neighbour's rows.
		// Installing first would leave rules the caller removed behind.
		if err := c.DropVMACLs(ctx, vmID); err != nil {
			return err
		}
		// Baseline floor, then the caller's rules.
		all := ovn.DefaultVMRules(address)
		all = append(all, ovnRules...)
		for _, rule := range all {
			if err := c.EnsureACL(ctx, ovn.VMSwitchName(network), rule, vmID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ApplyNetworkFirewall installs the switch-level (network ACL) rule set for a
// VPC. These are stateless and evaluated for all traffic on the logical switch,
// so they need no VM id and no address scoping.
func (b *OVNBackend) ApplyNetworkFirewall(networkName string, rules []models.SecurityRule) error {
	if networkName == "" {
		return fmt.Errorf("network: network name is required")
	}
	ctx := context.Background()
	if err := b.requireClient(); err != nil {
		return err
	}

	ovnRules, err := ToOVNRules(rules)
	if err != nil {
		return err
	}

	return b.withClient(func(c *ovn.Client) error {
		if err := c.DropNetworkACLs(ctx, networkName); err != nil {
			return err
		}
		return c.EnsureNetworkACLs(ctx, networkName, ovnRules)
	})
}

// ovnClient returns the live Northbound client, for callers that need direct
// reads (tests, status endpoints). It is nil before Initialize.
func (b *OVNBackend) ovnClient() *ovn.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ovn
}

// Reconcile brings the logical topology in the database up to date with the
// network set. Safe to call repeatedly. Callers that immediately depend on the
// topology (peering, or attaching a VM to a just-created VPC) use this rather
// than assuming SetNetworks already converged.
func (b *OVNBackend) Reconcile() error {
	b.mu.RLock()
	connected := b.ovn != nil
	b.mu.RUnlock()
	if !connected {
		return fmt.Errorf("network: OVN backend is not initialised")
	}
	return b.reconcileTopology(context.Background())
}
