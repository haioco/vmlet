// Package ovn talks to the OVN Northbound database (OVSDB over JSON-RPC) and
// reconciles the desired logical topology derived from vmlet's VM state.
//
// The client uses the pure-Go libovsdb library; when it is not available the
// same operations are expressed as ovn-nbctl commands (see nbctl.go).
package ovn

import (
	"github.com/ovn-org/libovsdb/model"
)

// NorthboundDatabase is the OVSDB database that stores the logical topology.
const NorthboundDatabase = "OVN_Northbound"

// LogicalSwitch is a row of the OVN_Northbound Logical_Switch table. Only the
// columns vmlet needs are modelled; everything else stays untouched.
type LogicalSwitch struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"`
	DNSRecords  []string          `ovsdb:"dns_records"`
	ACLs        []string          `ovsdb:"acls"`
	OtherConfig map[string]string `ovsdb:"other_config"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// LogicalSwitchPort is a row of the Logical_Switch_Port table.
type LogicalSwitchPort struct {
	UUID          string            `ovsdb:"_uuid"`
	Name          string            `ovsdb:"name"`
	Type          *string           `ovsdb:"type"`
	Addresses     []string          `ovsdb:"addresses"`
	Options       map[string]string `ovsdb:"options"`
	PortSecurity  []string          `ovsdb:"port_security"`
	ParentName    *string           `ovsdb:"parent_name"`
	Tag           *int              `ovsdb:"tag"`
	DHCPv4Options *string           `ovsdb:"dhcpv4_options"`
	OtherConfig   map[string]string `ovsdb:"other_config"`
	ExternalIDs   map[string]string `ovsdb:"external_ids"`
}

// LogicalRouter is a row of the Logical_Router table.
type LogicalRouter struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"`
	NAT         []string          `ovsdb:"nat"`
	OtherConfig map[string]string `ovsdb:"other_config"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// LogicalRouterPort is a row of the Logical_Router_Port table.
type LogicalRouterPort struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Networks    []string          `ovsdb:"networks"`
	MAC         *string           `ovsdb:"mac"`
	Peer        *string           `ovsdb:"peer"`
	OtherConfig map[string]string `ovsdb:"other_config"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// NewClientDBModel builds the libovsdb model of the tables vmlet manages.
func NewClientDBModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel(NorthboundDatabase, map[string]model.Model{
		"Logical_Switch":      &LogicalSwitch{},
		"Logical_Switch_Port": &LogicalSwitchPort{},
		"Logical_Router":      &LogicalRouter{},
		"Logical_Router_Port": &LogicalRouterPort{},
		"DHCP_Options":        &DHCPOptions{},
		"NAT":                 &NAT{},
		"DNS":                 &DNS{},
		"ACL":                 &ACL{},
	})
}
