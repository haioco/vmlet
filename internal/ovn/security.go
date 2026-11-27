package ovn

import (
	"fmt"
	"os"
	"strings"
)

// Environment variables mirroring the bridge backend's anti-spoof switches, so
// operators who tuned ebtables behaviour get the same knobs on the OVN backend.
const (
	EnvAntispoofEnable   = "MICROVM_ANTISPOOF_ENABLE"
	EnvAntispoofIPEnable = "MICROVM_ANTISPOOF_IP_ENABLE"
	EnvAntispoofStrict   = "MICROVM_ANTISPOOF_STRICT"
)

// PortSecurity describes the MAC/IP anti-spoof binding for one VM port.
// When Ipv4Address and Ipv6Address are empty they are not pinned.
type PortSecurity struct {
	MACAddress  string
	Ipv4Address string
	Ipv6Address string
}

// Values returns the port_security column value OVN expects, as a set of
// "mac ip" strings. One element is produced per address family that is bound.
func (p PortSecurity) Values() []string {
	var out []string
	if p.Ipv4Address != "" {
		out = append(out, fmt.Sprintf("%s %s", p.MACAddress, p.Ipv4Address))
	}
	if p.Ipv6Address != "" {
		out = append(out, fmt.Sprintf("%s %s", p.MACAddress, p.Ipv6Address))
	}
	return out
}

// SecurityRule is one declarative rule of a security group.
type SecurityRule struct {
	// Direction is "ingress" (from the logical port) or "egress" (to it).
	Direction string
	// Protocol is "tcp", "udp" or "" for any protocol.
	Protocol string
	// PortMin/PortMax bound the destination port; 0 means any.
	PortMin int
	PortMax int
	// Peer is an OVN match fragment restricting the other endpoint, e.g.
	// "10.0.0.0/24" or "sg-alpha". Empty means any peer.
	Peer string
	// AllowRule drops the traffic when false (a "deny" rule).
	AllowRule bool
	// MatchExpr overrides the expression Match() derives, for a rule scoped to
	// a specific port address that Peer cannot express.
	MatchExpr string
}

// SecurityRule names. Directions must be OVN's own values: the ACL table's
// direction column is an enum of "from-lport" / "to-lport", not
// "ingress"/"egress".
const (
	DirectionIngress = "from-lport"
	DirectionEgress  = "to-lport"

	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// SecurityGroup is a declarative set of rules that follows a VM.
type SecurityGroup struct {
	Name  string
	Rules []SecurityRule
}

// Match builds the OVN match expression for the rule, including the direction
// so one ACL row can express the rule. OVN ACLs on a logical switch use the
// reserved "filterport" address set to mean "the port the ACL is bound to".
func (r SecurityRule) Match() string {
	if r.MatchExpr != "" {
		return r.MatchExpr
	}
	direction := "inport"
	if r.Direction == DirectionEgress {
		direction = "outport"
	}
	out := fmt.Sprintf("%s == @filterport && ip", direction)

	if r.Peer != "" {
		out += fmt.Sprintf(" && %s", r.Peer)
	}

	if r.Protocol == ProtocolTCP || r.Protocol == ProtocolUDP {
		out += fmt.Sprintf(" && %s", r.Protocol)
		if r.PortMin > 0 || r.PortMax > 0 {
			field := "dst_port"
			if r.Direction == DirectionEgress {
				field = "src_port"
			}
			if r.PortMin == r.PortMax {
				out += fmt.Sprintf(" && %s == %d", field, r.PortMin)
			} else {
				out += fmt.Sprintf(" && %s >= %d && %s <= %d", field, r.PortMin, field, r.PortMax)
			}
		}
	}
	return out
}

// Action is "allow" or "drop"; OVN writes them in lowercase.
func (r SecurityRule) Action() string {
	if r.AllowRule {
		return "allow"
	}
	return "drop"
}

// Priority is the ACL priority OVN assigns a rule. Deny rules sit above allows
// so an explicit drop is evaluated first (OVN default is 1000).
func (r SecurityRule) Priority() int {
	if r.AllowRule {
		return 1000
	}
	return 2000
}

// Loggable reports whether the rule should emit a log entry when it matches.
func (r SecurityRule) Loggable() bool {
	return !r.AllowRule
}

// aclExternalIDs marks a row as vmlet-managed so reconcile loops can find and
// prune the rules derived from VM state.
func aclExternalIDs(group, vmID, switchName string) map[string]string {
	return map[string]string{
		"vmlet-managed": "true",
		"vmlet-vm":      vmID,
		"vmlet-group":   group,
		"vmlet-switch":  switchName,
	}
}

// portSecurityForVM builds the anti-spoof binding for a VM, honouring
// MICROVM_ANTISPOOF_ENABLE / _IP_ENABLE. When ENABLE is false no addresses are
// pinned (OVN still protects the MAC against the port's own dynamic MAC).
func portSecurityForVM(mac, ipv4, ipv6 string) PortSecurity {
	ps := PortSecurity{MACAddress: mac}
	if !isEnvDisabled(EnvAntispoofEnable) && envFlagEnabled(EnvAntispoofIPEnable) {
		ps.Ipv4Address = ipv4
		ps.Ipv6Address = ipv6
	}
	return ps
}

// antispoofDisabled reports whether the OVN backend should skip anti-spoofing
// entirely (the ebtables parity knob).
func antispoofDisabled() bool {
	return isEnvDisabled(EnvAntispoofEnable)
}

// isEnvDisabled reports whether an env var is explicitly set to a false-ish
// value ("false", "0", "no"). Unset means "enabled", matching the ebtables
// backend's semantics.
func isEnvDisabled(name string) bool {
	flag := envFlagValue(os.Getenv(name))
	return flag != nil && !*flag
}

// envFlagEnabled reports whether an env var is explicitly set to a true-ish
// value ("true", "1", "yes"). Unset means "disabled", which is how opt-in
// features such as IP anti-spoofing behave today.
func envFlagEnabled(name string) bool {
	flag := envFlagValue(os.Getenv(name))
	return flag != nil && *flag
}

// envFlagValue parses an env value; nil means unset.
func envFlagValue(value string) *bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return nil
	case "1", "true", "yes", "on":
		enabled := true
		return &enabled
	case "0", "false", "no", "off":
		disabled := false
		return &disabled
	default:
		return nil
	}
}

// DefaultVMRules is the baseline ACL set every VM port carries on the OVN
// backend. Both rules are scoped to the VM's own address, which is what keeps
// one VM's rows distinct from its neighbours': ACLs live on the logical switch
// and are shared by every port on it, so a match that is identical across VMs
// would make EnsureACL converge them onto a single row and DropVMACLs free it
// out from under the others.
//
// The rules are deliberately allow-only. OVN applies no implicit deny, so this
// baseline is behaviour-identical to bridge mode, where the ebtables rules are
// "allow ARP, then drop a foreign source MAC" - anti-spoofing, not a firewall.
// That drop half belongs to port_security on the logical port, which OVN
// enforces in the datapath before the ACL pipeline; restating it here would
// also drop the VM's own DHCP discovery, whose source address is 0.0.0.0 until
// it has been offered one.
func DefaultVMRules(ipv4 string) []SecurityRule {
	if ipv4 == "" {
		return nil
	}
	return []SecurityRule{
		{
			Direction: DirectionIngress,
			AllowRule: true,
			MatchExpr: fmt.Sprintf("inport == @filterport && ip4.dst == %s", ipv4),
		},
		{
			Direction: DirectionEgress,
			AllowRule: true,
			MatchExpr: fmt.Sprintf("outport == @filterport && ip4.src == %s", ipv4),
		},
	}
}
