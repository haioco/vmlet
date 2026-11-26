package ovn

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// DefaultNBEndpoint is the conventional OVN Northbound DB socket.
const DefaultNBEndpoint = "unix:/var/run/ovn/ovnnb_db.sock"

// Config configures an OVN Northbound client.
type Config struct {
	// Endpoint in OVSDB connection format, e.g.
	// "unix:/var/run/ovn/ovnnb_db.sock" or "tcp:127.0.0.1:6641".
	Endpoint string
	// When > 0 the client reconnects automatically using an exponential
	// backoff. Left at 0 by default so vmlet fails loudly rather than
	// silently dropping topology changes.
	ReconnectTimeout time.Duration
	Logger           *logr.Logger
}

// Client is a connected OVN Northbound client.
type Client struct {
	cfg    Config
	api    client.Client
	schema ovsdb.DatabaseSchema
}

// SelectNBEndpoint resolves the endpoint to use, falling back to the standard
// OVN Northbound socket when the operator did not configure one.
func SelectNBEndpoint(endpoint string) string {
	if strings.TrimSpace(endpoint) == "" {
		return DefaultNBEndpoint
	}
	return endpoint
}

// New connects to the OVN Northbound DB. Endpoint defaults to
// DefaultNBEndpoint when empty.
func New(ctx context.Context, cfg Config) (*Client, error) {
	cfg.Endpoint = SelectNBEndpoint(cfg.Endpoint)

	dbModel, err := NewClientDBModel()
	if err != nil {
		return nil, fmt.Errorf("ovn: cannot build db model: %w", err)
	}

	opts := []client.Option{client.WithEndpoint(cfg.Endpoint)}
	if cfg.ReconnectTimeout > 0 {
		opts = append(opts, client.WithReconnect(cfg.ReconnectTimeout, backoff.NewExponentialBackOff()))
	}
	if cfg.Logger != nil {
		opts = append(opts, client.WithLogger(cfg.Logger))
	}

	api, err := client.NewOVSDBClient(dbModel, opts...)
	if err != nil {
		return nil, fmt.Errorf("ovn: cannot create client for %s: %w", cfg.Endpoint, err)
	}
	if err := api.Connect(ctx); err != nil {
		return nil, fmt.Errorf("ovn: cannot connect to %s: %w", cfg.Endpoint, err)
	}
	// The client API reads from its cache, so populate it before any reconcile.
	if _, err := api.MonitorAll(ctx); err != nil {
		api.Close()
		return nil, fmt.Errorf("ovn: monitoring %s failed: %w", NorthboundDatabase, err)
	}

	return &Client{cfg: cfg, api: api, schema: api.Schema()}, nil
}

// Close disconnects the client.
func (c *Client) Close() {
	if c.api == nil {
		return
	}
	c.api.Close()
}

// Schema returns the schema as announced by the Northbound DB. Useful to check
// that the connected database is really OVN_Northbound.
func (c *Client) Schema() ovsdb.DatabaseSchema { return c.schema }

// ListSwitches returns the logical switches currently in the database.
func (c *Client) ListSwitches(ctx context.Context) ([]LogicalSwitch, error) {
	switches := []LogicalSwitch{}
	if err := c.api.List(ctx, &switches); err != nil {
		return nil, fmt.Errorf("ovn: listing logical switches: %w", err)
	}
	return switches, nil
}

// ListSwitchPorts returns the logical switch ports currently in the database.
func (c *Client) ListSwitchPorts(ctx context.Context) ([]LogicalSwitchPort, error) {
	ports := []LogicalSwitchPort{}
	if err := c.api.List(ctx, &ports); err != nil {
		return nil, fmt.Errorf("ovn: listing logical switch ports: %w", err)
	}
	return ports, nil
}

// ListRouters returns the logical routers currently in the database.
func (c *Client) ListRouters(ctx context.Context) ([]LogicalRouter, error) {
	routers := []LogicalRouter{}
	if err := c.api.List(ctx, &routers); err != nil {
		return nil, fmt.Errorf("ovn: listing logical routers: %w", err)
	}
	return routers, nil
}

// ListRouterPorts returns the logical router ports currently in the database.
func (c *Client) ListRouterPorts(ctx context.Context) ([]LogicalRouterPort, error) {
	ports := []LogicalRouterPort{}
	if err := c.api.List(ctx, &ports); err != nil {
		return nil, fmt.Errorf("ovn: listing logical router ports: %w", err)
	}
	return ports, nil
}

// ListDHCPOptions returns the DHCP_Options rows currently in the database.
func (c *Client) ListDHCPOptions(ctx context.Context) ([]DHCPOptions, error) {
	rows := []DHCPOptions{}
	if err := c.api.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: listing DHCP_Options: %w", err)
	}
	return rows, nil
}

// ListDNSRecords returns the DNS rows currently in the database.
func (c *Client) ListDNSRecords(ctx context.Context) ([]DNS, error) {
	rows := []DNS{}
	if err := c.api.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: listing DNS records: %w", err)
	}
	return rows, nil
}

// EnsureDHCPOptions creates or updates the DHCP_Options row for a subnet and
// returns its UUID (or the named-UUID for a row created in the same
// transaction). Idempotent.
func (c *Client) EnsureDHCPOptions(ctx context.Context, spec DHCPSpec, network string) (string, error) {
	options, err := dhcpOptions(spec)
	if err != nil {
		return "", err
	}
	externalIDs := dhcpExternalIDs(network)
	cidr := spec.CIDR

	current, err := c.dhcpByCIDR(ctx, cidr)
	if err != nil {
		return "", err
	}

	if len(current) == 0 {
		named := namedRef("dhcp", cidr)
		row := &DHCPOptions{
			UUID:        named,
			CIDR:        cidr,
			Options:     options,
			ExternalIDs: externalIDs,
		}
		ops, err := c.api.Create(row)
		if err != nil {
			return "", fmt.Errorf("ovn: cannot build create op for DHCP_Options %q: %w", cidr, err)
		}
		if err := c.transact(ctx, ops, fmt.Sprintf("create DHCP_Options %q", cidr)); err != nil {
			return "", err
		}
		// A named-UUID only resolves inside the transaction that created it, so
		// a caller that binds this row in a *later* transaction needs the real
		// UUID. Read it back now that the row is committed and cached.
		refreshed, err := c.dhcpByCIDR(ctx, cidr)
		if err != nil {
			return "", err
		}
		if len(refreshed) == 0 {
			return "", fmt.Errorf("ovn: DHCP_Options %q vanished after creation", cidr)
		}
		return refreshed[0].UUID, nil
	}

	existing := current[0]
	if mapsEqual(existing.Options, options) && mapsEqual(existing.ExternalIDs, externalIDs) {
		return existing.UUID, nil
	}

	row := &DHCPOptions{
		UUID:        existing.UUID,
		CIDR:        existing.CIDR,
		Options:     options,
		ExternalIDs: externalIDs,
	}
	ops, err := c.api.Where(existing).Update(row, &row.Options, &row.ExternalIDs)
	if err != nil {
		return "", fmt.Errorf("ovn: cannot build update op for DHCP_Options %q: %w", cidr, err)
	}
	return existing.UUID, c.transact(ctx, ops, fmt.Sprintf("update DHCP_Options %q", cidr))
}

// EnsureDHCPReservation makes a logical switch port use the given
// DHCP_Options row and pins its static address, so a VM keeps its IP across
// guest and host restarts. Idempotent.
func (c *Client) EnsureDHCPReservation(ctx context.Context, portName, dhcpOptionsUUID string) error {
	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return fmt.Errorf("ovn: logical switch port %q does not exist", portName)
	}

	existing := ports[0]
	if existing.DHCPv4Options != nil && *existing.DHCPv4Options == dhcpOptionsUUID {
		return nil
	}

	row := &LogicalSwitchPort{
		UUID:          existing.UUID,
		DHCPv4Options: &dhcpOptionsUUID,
	}
	ops, err := c.api.Where(&existing).Update(row, &row.DHCPv4Options)
	if err != nil {
		return fmt.Errorf("ovn: cannot build update op for port %q dhcpv4_options: %w", portName, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("bind DHCP_Options to port %q", portName))
}

// EnsureDNSRecord creates the "ip hostname" DNS pair for a reservation.
// Idempotent; records are keyed by external_ids so re-runs converge.
func (c *Client) EnsureDNSRecord(ctx context.Context, ip, hostname string) error {
	if hostname == "" || ip == "" {
		return nil
	}

	current, err := c.dnsByIP(ctx, ip)
	if err != nil {
		return err
	}
	want := map[string]string{hostname: ip}

	if len(current) > 0 {
		existing := current[0]
		if mapsEqual(existing.Records, want) {
			return nil
		}
		row := &DNS{UUID: existing.UUID, Records: want}
		ops, err := c.api.Where(&existing).Update(row, &row.Records)
		if err != nil {
			return fmt.Errorf("ovn: cannot build update op for DNS %s: %w", ip, err)
		}
		return c.transact(ctx, ops, fmt.Sprintf("update DNS %s", ip))
	}

	row := &DNS{
		UUID:    namedRef("dns", ip+"-"+hostname),
		Records: want,
		ExternalIDs: map[string]string{
			"vmlet-managed":  "true",
			dnsKeyExternalID: ip,
		},
	}
	ops, err := c.api.Create(row)
	if err != nil {
		return fmt.Errorf("ovn: cannot build create op for DNS %s: %w", ip, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("create DNS %s", ip))
}

// DropDHCPReservation unlinks a port from its DHCP_Options row and removes its
// DNS record. Idempotent; used when a VM is deleted.
func (c *Client) DropDHCPReservation(ctx context.Context, portName string) error {
	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}

	existing := ports[0]
	if existing.DHCPv4Options == nil {
		// Nothing bound: dropping is a no-op.
		return nil
	}

	// To clear a weak reference the row must be updated with a nil value; an
	// empty string is treated as "no change" by the OVSDB mapper.
	row := &LogicalSwitchPort{UUID: existing.UUID}
	ops, err := c.api.Where(&existing).Update(row, &row.DHCPv4Options)
	if err != nil {
		return fmt.Errorf("ovn: cannot build update op to clear port %q: %w", portName, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("drop DHCP reservation for port %q", portName))
}

// PruneDNSRecords deletes every DNS row whose reservation external id is no
// longer desired. desired maps IP -> hostname.
func (c *Client) PruneDNSRecords(ctx context.Context, desired map[string]string) error {
	records, err := c.ListDNSRecords(ctx)
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	for i := range records {
		row := records[i]
		ip := row.ExternalIDs[dnsKeyExternalID]
		if ip == "" || row.ExternalIDs["vmlet-managed"] != "true" {
			continue
		}
		if _, keep := desired[ip]; keep {
			continue
		}
		deleteOps, err := c.api.Where(&row).Delete()
		if err != nil {
			return fmt.Errorf("ovn: cannot build delete op for DNS %s: %w", ip, err)
		}
		ops = append(ops, deleteOps...)
	}
	return c.transact(ctx, ops, "prune DNS records")
}

// EnsurePortSecurity pins a VM's MAC/IP on its logical switch port, which is
// what makes OVN drop spoofed traffic at the logical port. Idempotent.
func (c *Client) EnsurePortSecurity(ctx context.Context, portName string, security PortSecurity) error {
	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return fmt.Errorf("ovn: logical switch port %q does not exist", portName)
	}

	existing := ports[0]
	if strSlicesEqual(existing.PortSecurity, security.Values()) {
		return nil
	}

	row := &LogicalSwitchPort{
		UUID:         existing.UUID,
		PortSecurity: security.Values(),
	}
	ops, err := c.api.Where(&existing).Update(row, &row.PortSecurity)
	if err != nil {
		return fmt.Errorf("ovn: cannot build update op for port %q port_security: %w", portName, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("set port security on %q", portName))
}

// EnsureACL creates or drift-corrects one ACL row on a logical switch.
// Rules are matched by (switch, priority, direction, match) so re-applying the
// same desired state never duplicates rows. Idempotent.
func (c *Client) EnsureACL(ctx context.Context, switchName string, rule SecurityRule, vmID string) error {
	switches, err := c.switchByName(ctx, switchName)
	if err != nil {
		return err
	}
	if len(switches) == 0 {
		return fmt.Errorf("ovn: logical switch %q does not exist", switchName)
	}

	action := rule.Action()
	priority := rule.Priority()
	direction := rule.Direction
	match := rule.Match()
	externalIDs := aclExternalIDs("", vmID, switchName)

	current, err := c.aclBy(ctx, switchName, priority, direction, match)
	if err != nil {
		return err
	}

	if len(current) == 0 {
		row := &ACL{
			UUID:        namedRef("acl", fmt.Sprintf("%s-%d-%s", switchName, priority, direction)),
			Action:      action,
			Direction:   direction,
			Priority:    priority,
			Match:       match,
			ExternalIDs: externalIDs,
		}
		if rule.Loggable() {
			row.Log = true
		}
		ops, err := c.api.Create(row)
		if err != nil {
			return fmt.Errorf("ovn: cannot build create op for ACL on %q: %w", switchName, err)
		}
		// The switch's acls column is a strong reference: a row that is not
		// listed there is orphaned and OVN never installs it. Bind it in the
		// same transaction that creates it.
		aclRef := row.UUID
		refOps, err := mutateSwitchACL(c.api, switches[0].UUID, ovsdb.MutateOperationInsert, aclRef)
		if err != nil {
			return err
		}
		ops = append(ops, refOps...)
		return c.transact(ctx, ops, fmt.Sprintf("create ACL on %q", switchName))
	}

	existing := current[0]
	if mapsEqual(existing.ExternalIDs, externalIDs) {
		return nil
	}
	row := &ACL{
		UUID:        existing.UUID,
		Priority:    existing.Priority,
		Match:       existing.Match,
		ExternalIDs: externalIDs,
	}
	row.Action = action
	row.Direction = direction
	ops, err := c.api.Where(&existing).Update(row, &row.ExternalIDs)
	if err != nil {
		return fmt.Errorf("ovn: cannot build update op for ACL on %q: %w", switchName, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("update ACL on %q", switchName))
}

// DropVMACLs removes every ACL row derived from a VM. Idempotent.
func (c *Client) DropVMACLs(ctx context.Context, vmID string) error {
	rows, err := c.ListACLs(ctx)
	if err != nil {
		return err
	}

	switches, err := c.switchByName(ctx, switchForACL(rows))
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	for i := range rows {
		row := rows[i]
		if row.ExternalIDs["vmlet-vm"] != vmID || row.ExternalIDs["vmlet-managed"] != "true" {
			continue
		}
		for _, ls := range switches {
			if containsString(ls.ACLs, row.UUID) {
				refOps, err := mutateSwitchACL(c.api, ls.UUID, ovsdb.MutateOperationDelete, row.UUID)
				if err != nil {
					return err
				}
				ops = append(ops, refOps...)
			}
		}
		deleteOps, err := c.api.Where(&row).Delete()
		if err != nil {
			return fmt.Errorf("ovn: cannot build delete op for ACL of %s: %w", vmID, err)
		}
		ops = append(ops, deleteOps...)
	}
	return c.transact(ctx, ops, fmt.Sprintf("drop ACLs of %s", vmID))
}

// PruneACLs deletes vmlet-managed ACLs whose (switch, vm, priority, direction,
// match) is no longer desired. desired is keyed by switch name.
func (c *Client) PruneACLs(ctx context.Context, desired map[string][]SecurityRule) error {
	rows, err := c.ListACLs(ctx)
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	for i := range rows {
		row := rows[i]
		if row.ExternalIDs["vmlet-managed"] != "true" {
			continue
		}
		switchName := row.ExternalIDs["vmlet-switch"]
		if keepACL(desired[switchName], row) {
			continue
		}
		if switchRow, lerr := c.switchByName(ctx, switchName); lerr == nil && len(switchRow) > 0 {
			if containsString(switchRow[0].ACLs, row.UUID) {
				refOps, merr := mutateSwitchACL(c.api, switchRow[0].UUID, ovsdb.MutateOperationDelete, row.UUID)
				if merr != nil {
					return merr
				}
				ops = append(ops, refOps...)
			}
		}
		deleteOps, err := c.api.Where(&row).Delete()
		if err != nil {
			return fmt.Errorf("ovn: cannot build delete op for ACL %s: %w", row.UUID, err)
		}
		ops = append(ops, deleteOps...)
	}
	return c.transact(ctx, ops, "prune ACLs")
}

// ListACLs returns the ACL rows currently in the database.
func (c *Client) ListACLs(ctx context.Context) ([]ACL, error) {
	rows := []ACL{}
	if err := c.api.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: listing ACLs: %w", err)
	}
	return rows, nil
}

func (c *Client) aclBy(ctx context.Context, switchName string, priority int, direction, match string) ([]ACL, error) {
	rows := []ACL{}
	if err := c.api.WhereCache(func(r *ACL) bool {
		return r.Priority == priority &&
			r.Match == match &&
			r.Direction == direction &&
			r.ExternalIDs["vmlet-switch"] == switchName
	}).List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: lookup of ACL on %q: %w", switchName, err)
	}
	return rows, nil
}

func keepACL(rules []SecurityRule, row ACL) bool {
	for _, rule := range rules {
		if rule.Priority() == row.Priority &&
			rule.Match() == row.Match &&
			row.Direction == rule.Direction {
			return true
		}
	}
	return false
}

func (c *Client) dhcpByCIDR(ctx context.Context, cidr string) ([]DHCPOptions, error) {
	rows := []DHCPOptions{}
	if err := c.api.WhereCache(func(r *DHCPOptions) bool { return r.CIDR == cidr }).List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: lookup of DHCP_Options %q: %w", cidr, err)
	}
	return rows, nil
}

func (c *Client) dnsByIP(ctx context.Context, ip string) ([]DNS, error) {
	rows := []DNS{}
	if err := c.api.WhereCache(func(r *DNS) bool {
		return r.ExternalIDs[dnsKeyExternalID] == ip
	}).List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: lookup of DNS records for %s: %w", ip, err)
	}
	return rows, nil
}

// EnsureLogicalSwitch creates or updates a logical switch. Idempotent.
func (c *Client) EnsureLogicalSwitch(ctx context.Context, name string, otherConfig, externalIDs map[string]string) error {
	switches, err := c.switchByName(ctx, name)
	if err != nil {
		return err
	}

	if len(switches) == 0 {
		ops, err := c.api.Create(&LogicalSwitch{
			Name:        name,
			OtherConfig: otherConfig,
			ExternalIDs: externalIDs,
		})
		if err != nil {
			return fmt.Errorf("ovn: cannot build create op for logical switch %q: %w", name, err)
		}
		return c.transact(ctx, ops, fmt.Sprintf("create logical switch %q", name))
	}

	existing := switches[0]
	if mapsEqual(existing.OtherConfig, otherConfig) && mapsEqual(existing.ExternalIDs, externalIDs) {
		return nil
	}
	// The update row and the field pointers must be the same model instance.
	target := &LogicalSwitch{
		UUID:        existing.UUID,
		OtherConfig: otherConfig,
		ExternalIDs: externalIDs,
	}
	ops, err := c.api.Where(existing).Update(target, &target.OtherConfig, &target.ExternalIDs)
	if err != nil {
		return fmt.Errorf("ovn: cannot build update op for logical switch %q: %w", name, err)
	}
	return c.transact(ctx, ops, fmt.Sprintf("update logical switch %q", name))
}

// EnsureLogicalSwitchPort creates or updates a port on an existing logical
// switch and makes sure the switch references it. Idempotent.
func (c *Client) EnsureLogicalSwitchPort(ctx context.Context, switchName string, port LogicalPortSpec) error {
	switches, err := c.switchByName(ctx, switchName)
	if err != nil {
		return err
	}
	if len(switches) == 0 {
		return fmt.Errorf("ovn: logical switch %q does not exist", switchName)
	}
	switchUUID := switches[0].UUID

	ports, err := c.switchPortByName(ctx, port.Name)
	if err != nil {
		return err
	}

	if len(ports) == 0 {
		// The port must carry a named-UUID so the switch can reference it in
		// the same transaction: Logical_Switch.ports is a strong reference, so
		// the server rejects a mutation pointing at an empty (or unknown) UUID.
		newPort := port.toModel()
		newPort.UUID = namedRef("port", port.Name)
		createOps, err := c.api.Create(newPort)
		if err != nil {
			return fmt.Errorf("ovn: cannot build create op for logical switch port %q: %w", port.Name, err)
		}
		ops := append([]ovsdb.Operation{}, createOps...)
		if !containsString(switches[0].Ports, newPort.UUID) {
			ops, err = appendPortReference(c.api, ops, switchUUID, newPort.UUID)
			if err != nil {
				return err
			}
		}
		return c.transact(ctx, ops, fmt.Sprintf("create logical switch port %q", port.Name))
	}

	existing := ports[0]
	referenced := containsString(switches[0].Ports, existing.UUID)
	if port.equalModel(&existing) && referenced {
		return nil
	}

	var ops []ovsdb.Operation
	if !port.equalModel(&existing) {
		// The update row and the field pointers must be the same model instance.
		target := port.toModel()
		target.UUID = existing.UUID
		updateOps, err := c.api.Where(&existing).Update(
			target,
			&target.Type, &target.Addresses, &target.Options,
			&target.PortSecurity, &target.ExternalIDs,
		)
		if err != nil {
			return fmt.Errorf("ovn: cannot build update op for logical switch port %q: %w", port.Name, err)
		}
		ops = append(ops, updateOps...)
	}
	if !referenced {
		ops, err = appendPortReference(c.api, ops, switchUUID, existing.UUID)
		if err != nil {
			return err
		}
	}
	return c.transact(ctx, ops, fmt.Sprintf("update logical switch port %q", port.Name))
}

// DeleteLogicalSwitchPort removes a port and drops it from its switch.
// Idempotent.
func (c *Client) DeleteLogicalSwitchPort(ctx context.Context, portName string) error {
	ports, err := c.switchPortByName(ctx, portName)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}

	switches, err := c.ListSwitches(ctx)
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	existing := ports[0]
	for _, ls := range switches {
		if containsString(ls.Ports, existing.UUID) {
			mutateOps, err := mutatePortReference(c.api, ls.UUID, ovsdb.MutateOperationDelete, existing.UUID)
			if err != nil {
				return err
			}
			ops = append(ops, mutateOps...)
		}
	}
	deleteOps, err := c.api.Where(&existing).Delete()
	if err != nil {
		return fmt.Errorf("ovn: cannot build delete op for logical switch port %q: %w", portName, err)
	}
	ops = append(ops, deleteOps...)
	return c.transact(ctx, ops, fmt.Sprintf("delete logical switch port %q", portName))
}

func (c *Client) switchByName(ctx context.Context, name string) ([]LogicalSwitch, error) {
	switches := []LogicalSwitch{}
	if err := c.api.WhereCache(func(ls *LogicalSwitch) bool { return ls.Name == name }).List(ctx, &switches); err != nil {
		return nil, fmt.Errorf("ovn: lookup of logical switch %q: %w", name, err)
	}
	return switches, nil
}

func (c *Client) switchPortByName(ctx context.Context, name string) ([]LogicalSwitchPort, error) {
	ports := []LogicalSwitchPort{}
	if err := c.api.WhereCache(func(p *LogicalSwitchPort) bool { return p.Name == name }).List(ctx, &ports); err != nil {
		return nil, fmt.Errorf("ovn: lookup of logical switch port %q: %w", name, err)
	}
	return ports, nil
}

// appendPortReference adds an operation making switchUUID reference portUUID.
func appendPortReference(api client.API, ops []ovsdb.Operation, switchUUID, portUUID string) ([]ovsdb.Operation, error) {
	mutated, err := mutatePortReference(api, switchUUID, ovsdb.MutateOperationInsert, portUUID)
	if err != nil {
		return nil, err
	}
	return append(ops, mutated...), nil
}

// mutatePortReference inserts or deletes a port UUID in a switch's ports set.
func mutatePortReference(api client.API, switchUUID string, mutator ovsdb.Mutator, portUUID string) ([]ovsdb.Operation, error) {
	target := &LogicalSwitch{}
	ops, err := api.WhereAll(target, model.Condition{
		Field:    &target.UUID,
		Function: ovsdb.ConditionEqual,
		Value:    switchUUID,
	}).Mutate(target, model.Mutation{
		Field:   &target.Ports,
		Mutator: mutator,
		Value:   []string{portUUID},
	})
	if err != nil {
		return nil, fmt.Errorf("ovn: cannot build port reference op: %w", err)
	}
	return ops, nil
}

// transact runs the given operations in a single JSON-RPC transaction and
// reports the first operation error found.
func (c *Client) transact(ctx context.Context, ops []ovsdb.Operation, what string) error {
	if len(ops) == 0 {
		return nil
	}
	results, err := c.api.Transact(ctx, ops...)
	if err != nil {
		return fmt.Errorf("ovn: transaction to %s failed: %w", what, err)
	}
	for i, result := range results {
		if result.Error != "" {
			// Include the server's detail: it is the only place a schema or
			// protocol mismatch is actually explained, and dropping it makes
			// those failures look like opaque "syntax error"s.
			if result.Details != "" {
				return fmt.Errorf("ovn: transaction to %s failed at op %d: %s: %s",
					what, i, result.Error, result.Details)
			}
			return fmt.Errorf("ovn: transaction to %s failed at op %d: %s", what, i, result.Error)
		}
	}
	return nil
}

// TransactSelect runs a raw server-side select on a table and returns the rows.
// It is the one read path that does not go through the client cache, which
// makes it the right tool for verifying that a write actually persisted.
func (c *Client) TransactSelect(ctx context.Context, table string) (ovsdb.OperationResult, error) {
	res, err := c.api.Transact(ctx, ovsdb.Operation{Op: ovsdb.OperationSelect, Table: table})
	if err != nil {
		return ovsdb.OperationResult{}, err
	}
	if len(res) == 0 {
		return ovsdb.OperationResult{}, fmt.Errorf("ovn: select on %s returned no results", table)
	}
	if res[0].Error != "" {
		return ovsdb.OperationResult{}, fmt.Errorf("ovn: select on %s: %s", table, res[0].Error)
	}
	return res[0], nil
}

// RawTransact exposes per-result error details, which the high-level error
// message discards. Used for diagnosing schema/protocol mismatches.
func (c *Client) RawTransact(ctx context.Context, table string) ([]ovsdb.OperationResult, error) {
	return c.api.Transact(ctx, ovsdb.Operation{Op: ovsdb.OperationSelect, Table: table})
}

// mutateSwitchACL inserts or deletes an ACL row reference in a switch's acls
// set. It mirrors mutatePortReference: without it an ACL row is orphaned and
// OVN never installs the rule.
func mutateSwitchACL(api client.API, switchUUID string, mutator ovsdb.Mutator, aclUUID string) ([]ovsdb.Operation, error) {
	target := &LogicalSwitch{}
	ops, err := api.WhereAll(target, model.Condition{
		Field:    &target.UUID,
		Function: ovsdb.ConditionEqual,
		Value:    switchUUID,
	}).Mutate(target, model.Mutation{
		Field:   &target.ACLs,
		Mutator: mutator,
		Value:   []string{aclUUID},
	})
	if err != nil {
		return nil, fmt.Errorf("ovn: cannot build ACL reference op: %w", err)
	}
	return ops, nil
}

// switchForACL returns the switch a set of ACL rows belongs to.
func switchForACL(rows []ACL) string {
	for _, row := range rows {
		if name := row.ExternalIDs["vmlet-switch"]; name != "" {
			return name
		}
	}
	return ""
}
