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
		newPort := port.toModel()
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
			return fmt.Errorf("ovn: transaction to %s failed at op %d: %s", what, i, result.Error)
		}
	}
	return nil
}
