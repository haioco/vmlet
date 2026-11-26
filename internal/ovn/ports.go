package ovn

import (
	"fmt"
	"net"
	"strings"
)

// LogicalPortName is the OVN logical switch port name for a VM. It mirrors the
// name internal/firecracker uses as the OVS iface-id, so the NB port and the
// datapath binding always agree.
func LogicalPortName(vmID string) string {
	return "fc-port-" + sanitize(vmID)
}

// LogicalPortAddresses renders the Logical_Switch_Port.addresses column: a set
// of "mac ip" strings. With no IP the port carries only its MAC (DHCP will
// assign it), otherwise the pair pins both.
func LogicalPortAddresses(mac, ip string) []string {
	if mac == "" {
		return nil
	}
	if ip == "" {
		return []string{mac}
	}
	return []string{mac + " " + ip}
}

// SplitLogicalPortAddress recovers the MAC and IP from an addresses column.
func SplitLogicalPortAddress(addresses []string) (mac, ip string) {
	if len(addresses) == 0 {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(addresses[0]), " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", ""
	}
	mac = parts[0]
	if len(parts) == 2 {
		ip = strings.TrimSpace(parts[1])
	}
	return mac, ip
}

// AddressIPs returns just the IPs from a set of "mac ip" strings.
func AddressIPs(addresses []string) []string {
	var out []string
	for _, address := range addresses {
		if _, ip := SplitLogicalPortAddress([]string{address}); ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

// AllocateSubnetIP picks the next free address in a subnet, skipping the
// network and broadcast addresses and any address already in use. Deterministic
// on a given used-set, which keeps reconciles idempotent.
func AllocateSubnetIP(cidr string, used []string) (string, error) {
	_, ipNet, err := ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("ovn: invalid subnet %q: %w", cidr, err)
	}

	taken := make(map[string]bool, len(used))
	for _, ip := range used {
		taken[ip] = true
	}

	// Start at the network address and advance past it and the gateway,
	// which is the first usable address on the subnet and already spoken for
	// by the logical router.
	candidate := make(net.IP, len(ipNet.IP))
	copy(candidate, ipNet.IP)
	for i := 0; i < 2; i++ {
		incIP(candidate)
		for isNetworkOrBroadcast(ipNet, candidate) {
			incIP(candidate)
		}
	}

	for ipNet.Contains(candidate) {
		if !taken[candidate.String()] && !isBroadcast(ipNet, candidate) {
			return candidate.String(), nil
		}
		incIP(candidate)
	}
	return "", fmt.Errorf("ovn: subnet %q has no free addresses", cidr)
}

// PortSecurityFor builds the MAC/IP anti-spoof binding for a VM, honouring the
// MICROVM_ANTISPOOF_* switches exactly as the bridge backend does.
func PortSecurityFor(mac, ip string) PortSecurity {
	return portSecurityForVM(mac, ip, "")
}

// ParseCIDR splits a CIDR into its IP and network.
func ParseCIDR(cidr string) (net.IP, *net.IPNet, error) {
	return parseCIDR(cidr)
}

// FirstUsable returns the first usable address of a subnet.
func FirstUsable(ipNet *net.IPNet) net.IP {
	return firstUsable(ipNet)
}

// NetmaskToDotted renders a subnet mask in dotted-decimal notation.
func NetmaskToDotted(ipNet *net.IPNet) string {
	return netmaskToDotted(ipNet)
}

func isNetworkOrBroadcast(ipNet *net.IPNet, ip net.IP) bool {
	return ipNet.IP.Equal(ip) || isBroadcast(ipNet, ip)
}

func isBroadcast(ipNet *net.IPNet, ip net.IP) bool {
	broadcast := make(net.IP, len(ipNet.IP))
	copy(broadcast, ipNet.IP)
	ones, bits := ipNet.Mask.Size()
	for i := range broadcast {
		broadcast[i] |= ^ipNet.Mask[i]
	}
	_ = ones
	_ = bits
	return broadcast.Equal(ip)
}
