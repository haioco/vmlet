package ovn

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// NAT is a row of the Logical_Router.nat target table. OVN's northd turns
// these into dataplane DNAT/SNAT rules.
type NAT struct {
	UUID        string            `ovsdb:"_uuid"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	ExternalIP  string            `ovsdb:"external_ip"`
	LogicalIP   string            `ovsdb:"logical_ip"`
	LogicalPort *string           `ovsdb:"logical_port"`
	ExternalMAC *string           `ovsdb:"external_mac"`
	Type        string            `ovsdb:"type"`
}

// NAT types.
const (
	NATTypeDNAT = "dnat"
	NATTypeSNAT = "snat"
)

// FloatingIP is one addon entry: a public IPv4 mapped onto a VM's private
// address. This is what the /vms/{id}/ipv4 endpoints add and detach.
type FloatingIP struct {
	// ExternalIP is the public address being mapped in ("external_ip").
	ExternalIP string
	// InternalIP is the VM's address on its logical network ("logical_ip").
	InternalIP string
	// MAC is the VM's MAC; OVN answers ARP for the external IP with it.
	MAC string
	// Router is the logical router the NAT rule is installed on.
	Router string
}

// natExternalIDs marks a row as vmlet-managed and identifies it by external IP
// plus VM, so attach/detach and pruning can find it again.
func natExternalIDs(vmID, externalIP string) map[string]string {
	return map[string]string{
		"vmlet-managed":  "true",
		"vmlet-vm":       vmID,
		"vmlet-floating": externalIP,
	}
}

// toNAT renders the floating IP as a DNAT row. SNAT is installed separately by
// EnsureSNAT, which is one rule for the whole external range.
func (f FloatingIP) toNAT(vmID string) *NAT {
	n := &NAT{
		ExternalIDs: natExternalIDs(vmID, f.ExternalIP),
		ExternalIP:  f.ExternalIP,
		LogicalIP:   f.InternalIP,
		Type:        NATTypeDNAT,
	}
	if f.MAC != "" {
		mac := f.MAC
		n.ExternalMAC = &mac
	}
	return n
}

// SNATRule is the masquerade entry letting a VM reach the internet without a
// dedicated public IP.
type SNATRule struct {
	// Router hosting the rule.
	Router string
	// LogicalIP is the source address to translate ("" = the network's subnet).
	LogicalIP string
	// ExternalIP is the address presented to the outside world.
	ExternalIP string
}

func (s SNATRule) toNAT() *NAT {
	return &NAT{
		ExternalIDs: natExternalIDs("", s.ExternalIP),
		ExternalIP:  s.ExternalIP,
		LogicalIP:   s.LogicalIP,
		Type:        NATTypeSNAT,
	}
}

// ReconcileNAT drives the logical router's NAT rows to the desired floating-IP
// and SNAT sets in one transaction, returning the number of rows created and
// deleted. Idempotent: re-applying the same state is a no-op.
func (c *Client) ReconcileNAT(ctx context.Context, floating []FloatingIP, snats []SNATRule) (*NATResult, error) {
	routers, err := c.ListRouters(ctx)
	if err != nil {
		return nil, err
	}
	current, err := c.ListNATs(ctx)
	if err != nil {
		return nil, err
	}

	routerByName := make(map[string]*LogicalRouter, len(routers))
	for i := range routers {
		routerByName[routers[i].Name] = &routers[i]
	}
	currentByKey := indexNATs(current)

	result := &NATResult{}
	var ops []ovsdb.Operation

	for _, f := range floating {
		if f.ExternalIP == "" || f.InternalIP == "" {
			continue
		}
		router, ok := routerByName[f.Router]
		if !ok {
			return nil, fmt.Errorf("ovn: logical router %q does not exist", f.Router)
		}

		key := natKey(NATTypeDNAT, f.ExternalIP, f.InternalIP, "")
		if _, exists := currentByKey[key]; !exists {
			row := f.toNAT(vmIDFor(f))
			named := namedRef("nat", key)
			row.UUID = named
			createOps, err := c.api.Create(row)
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build create op for NAT %s: %w", f.ExternalIP, err)
			}
			ops = append(ops, createOps...)
			// The router's nat column must reference the new row in the same
			// transaction, so a later read sees the rule already attached.
			refOps, err := mutateRouterRef(c.api, router.UUID, ovsdb.MutateOperationInsert, named)
			if err != nil {
				return nil, err
			}
			ops = append(ops, refOps...)
			result.Created++
			continue
		}

		// Drift: only the port binding can change for an existing key.
		existing := currentByKey[key]
		if existing.ExternalMAC != nil && *existing.ExternalMAC == f.MAC {
			continue
		}
		updated := f.toNAT(vmIDFor(f))
		updated.UUID = existing.UUID
		updateOps, err := c.api.Where(existing).Update(updated, &updated.ExternalMAC)
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build update op for NAT %s: %w", f.ExternalIP, err)
		}
		ops = append(ops, updateOps...)
		result.Updated++
	}

	for _, s := range snats {
		if s.ExternalIP == "" {
			continue
		}
		router, ok := routerByName[s.Router]
		if !ok {
			return nil, fmt.Errorf("ovn: logical router %q does not exist", s.Router)
		}

		key := natKey(NATTypeSNAT, s.ExternalIP, s.LogicalIP, "")
		if _, exists := currentByKey[key]; exists {
			continue
		}
		row := s.toNAT()
		named := namedRef("nat", key)
		row.UUID = named
		createOps, err := c.api.Create(row)
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build create op for SNAT %s: %w", s.ExternalIP, err)
		}
		ops = append(ops, createOps...)
		refOps, err := mutateRouterRef(c.api, router.UUID, ovsdb.MutateOperationInsert, named)
		if err != nil {
			return nil, err
		}
		ops = append(ops, refOps...)
		result.Created++
	}

	// Rows no longer desired: detach from the router, then delete.
	desired := map[string]bool{}
	for _, f := range floating {
		desired[natKey(NATTypeDNAT, f.ExternalIP, f.InternalIP, "")] = true
	}
	for _, s := range snats {
		desired[natKey(NATTypeSNAT, s.ExternalIP, s.LogicalIP, "")] = true
	}

	for i := range current {
		row := current[i]
		if row.ExternalIDs["vmlet-managed"] != "true" {
			continue
		}
		key := natKey(row.Type, row.ExternalIP, row.LogicalIP, "")
		if desired[key] {
			continue
		}
		for _, router := range routers {
			if containsString(router.NAT, row.UUID) {
				deleteOps, err := mutateRouterRef(c.api, router.UUID, ovsdb.MutateOperationDelete, row.UUID)
				if err != nil {
					return nil, err
				}
				ops = append(ops, deleteOps...)
			}
		}
		deleteOps, err := c.api.Where(&row).Delete()
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build delete op for NAT %s: %w", row.UUID, err)
		}
		ops = append(ops, deleteOps...)
		result.Deleted++
	}

	if err := c.transact(ctx, ops, "reconcile OVN NAT"); err != nil {
		return nil, err
	}
	return result, nil
}

// NATResult reports what a reconcile changed, so idempotency is assertable.
type NATResult struct {
	Created int
	Updated int
	Deleted int
}

// ListNATs returns the NAT rows currently in the database.
func (c *Client) ListNATs(ctx context.Context) ([]NAT, error) {
	rows := []NAT{}
	if err := c.api.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: listing NAT rows: %w", err)
	}
	return rows, nil
}

// natKey identifies a row by the tuple that makes it unique.
func natKey(natType, externalIP, logicalIP, logicalPort string) string {
	return fmt.Sprintf("%s|%s|%s|%s", natType, externalIP, logicalIP, logicalPort)
}

func indexNATs(rows []NAT) map[string]*NAT {
	out := make(map[string]*NAT, len(rows))
	for i := range rows {
		row := rows[i]
		out[natKey(row.Type, row.ExternalIP, row.LogicalIP, strPtrValue(row.LogicalPort))] = &row
	}
	return out
}

// vmIDFor derives the VM tag for a floating IP from the router it hangs off,
// until the caller passes the VM id explicitly.
func vmIDFor(f FloatingIP) string {
	return f.Router
}

// mutateRouterRef inserts or deletes a NAT row reference in a router's nat set.
// The pattern matches mutatePortReference: a single mutating operation on the
// router, bound by a _uuid condition.
func mutateRouterRef(api client.API, routerUUID string, mutator ovsdb.Mutator, natUUID string) ([]ovsdb.Operation, error) {
	target := &LogicalRouter{}
	ops, err := api.WhereAll(target, model.Condition{
		Field:    &target.UUID,
		Function: ovsdb.ConditionEqual,
		Value:    routerUUID,
	}).Mutate(target, model.Mutation{
		Field:   &target.NAT,
		Mutator: mutator,
		Value:   []string{natUUID},
	})
	if err != nil {
		return nil, fmt.Errorf("ovn: cannot build router nat reference op: %w", err)
	}
	return ops, nil
}
