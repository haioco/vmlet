package models

import "time"

// Network (VPC) is a logical network: an OVN switch with a subnet. VMs
// reference NetworkID to pick which VPC they live in. All VMs in one network
// share the same subnet and DHCP.
type Network struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CIDR       string    `json:"cidr"`    // e.g. "10.0.0.0/24"
	Gateway    string    `json:"gateway"` // e.g. "10.0.0.1"
	DNSServers []string  `json:"dns_servers,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
