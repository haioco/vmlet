package ovn

import (
	"fmt"
	"net"
)

// parseCIDR splits a CIDR string into its IP and network parts.
func parseCIDR(cidr string) (net.IP, *net.IPNet, error) {
	return net.ParseCIDR(cidr)
}

// firstUsable returns the first address of a subnet (the network address plus
// one), which is conventionally the gateway.
func firstUsable(ipNet *net.IPNet) net.IP {
	ip := ipNet.IP
	out := make(net.IP, len(ip))
	copy(out, ip)
	incIP(out)
	return out
}

// incIP increments an IP in place, leaving the length untouched.
func incIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

// netmaskToDotted renders a subnet's mask in dotted-decimal form.
func netmaskToDotted(ipNet *net.IPNet) string {
	ones, _ := ipNet.Mask.Size()
	if ones == 0 || ones > 32 {
		return "0.0.0.0"
	}
	mask := net.CIDRMask(ones, 32)
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}
