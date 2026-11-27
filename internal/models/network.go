package models

import "time"

// Network (VPC) is a logical network: an OVN switch with a subnet. VMs
// reference NetworkID to pick which VPC they live in. All VMs in one network
// share the same subnet and DHCP.
type Network struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CIDR       string `json:"cidr"`                  // e.g. "10.0.0.0/24"
	Gateway    string `json:"gateway"`               // e.g. "10.0.0.1"
	ExternalIP string `json:"external_ip,omitempty"` // outbound masquerade address
	// FirewallRules are this network's stateless ACLs (the AWS Network ACL /
	// GCP firewall equivalent), evaluated before any per-VM rule.
	FirewallRules []SecurityRule `json:"firewall_rules,omitempty"`
	DNSServers    []string       `json:"dns_servers,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// SecurityRule is one entry of a VM's security group. It maps onto an OVN ACL
// row; Allow=false is a deny rule, which OVN evaluates above allows.
type SecurityRule struct {
	// Direction is "ingress" or "egress" (from/to the VM's port).
	Direction string `json:"direction"`
	Protocol  string `json:"protocol,omitempty"` // "tcp", "udp", or any
	PortMin   int    `json:"port_min,omitempty"`
	PortMax   int    `json:"port_max,omitempty"`
	// Peer restricts the other endpoint, e.g. "10.0.0.0/24". Empty means any.
	Peer string `json:"peer,omitempty"`
	// Allow drops the traffic when false.
	Allow bool `json:"allow"`
	// Match is an explicit OVN match expression; when set it overrides the
	// fields above, for rules this model cannot express.
	Match string `json:"match,omitempty"`
	// Priority orders evaluation; a higher number wins. Zero means "derive".
	Priority int `json:"priority,omitempty"`
}

// NetworkACL is a stateless, switch-level rule: the AWS Network ACL / GCP
// firewall equivalent. It is evaluated for traffic on the VPC's logical switch,
// before any per-VM security-group rule.
type NetworkACL struct {
	// Applies when the traffic comes from or goes to a VM port on this switch.
	Rules []SecurityRule
}
