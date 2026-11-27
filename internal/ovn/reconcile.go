package ovn

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/ovsdb"
)

// LogicalPortSpec describes the desired state of a logical switch port.
type LogicalPortSpec struct {
	Name         string
	Type         string
	Addresses    []string
	Options      map[string]string
	PortSecurity []string
	ExternalIDs  map[string]string
}

func (p LogicalPortSpec) toModel() *LogicalSwitchPort {
	m := &LogicalSwitchPort{
		Name:         p.Name,
		Addresses:    p.Addresses,
		Options:      p.Options,
		PortSecurity: p.PortSecurity,
		ExternalIDs:  p.ExternalIDs,
	}
	if p.Type != "" {
		m.Type = p.Type
	}
	return m
}

func (p LogicalPortSpec) equalModel(existing *LogicalSwitchPort) bool {
	if existing.Name != p.Name {
		return false
	}
	if existing.Type != p.Type {
		return false
	}
	if !strSlicesEqual(existing.Addresses, p.Addresses) {
		return false
	}
	if !strSlicesEqual(existing.PortSecurity, p.PortSecurity) {
		return false
	}
	if !mapsEqual(existing.Options, p.Options) {
		return false
	}
	return mapsEqual(existing.ExternalIDs, p.ExternalIDs)
}

// LogicalSwitchSpec describes the desired state of a logical switch and the
// ports that must live on it.
type LogicalSwitchSpec struct {
	Name        string
	Ports       []LogicalPortSpec
	OtherConfig map[string]string
	ExternalIDs map[string]string
}

// LogicalRouterSpec describes the desired state of a logical router. The real
// schema has no other_config on Logical_Router, so free-form settings are
// expressed through options.
type LogicalRouterSpec struct {
	Name        string
	Options     map[string]string
	ExternalIDs map[string]string
}

// Topology is the desired state of the OVN logical network, derived from vmlet's
// VM state.
type Topology struct {
	Switches []LogicalSwitchSpec
	Routers  []LogicalRouterSpec

	// Prune deletes logical switches and routers that are not in the desired
	// set. It is opt-in and defaults to false because the desired set is only
	// as wide as the networks this process knows about: a host that serves one
	// VPC must not delete another VPC's logical switch and router, which is
	// what took the whole subnet - and every VM attached to it - down the last
	// time the set was narrower than the database.
	Prune bool
}

// ReconcileResult reports what a single reconcile created or deleted, so
// callers (and tests) can assert idempotency.
type ReconcileResult struct {
	Created int
	Deleted int
}

// ReconcileTopology drives the live OVN topology to the desired state in one
// transaction. It is safe to retry after a controller restart because the
// current state is always read from the database.
func (c *Client) ReconcileTopology(ctx context.Context, desired Topology) (*ReconcileResult, error) {
	currentSwitches, err := c.ListSwitches(ctx)
	if err != nil {
		return nil, err
	}
	currentPorts, err := c.ListSwitchPorts(ctx)
	if err != nil {
		return nil, err
	}
	currentRouters, err := c.ListRouters(ctx)
	if err != nil {
		return nil, err
	}

	switchByPort := indexSwitchPorts(currentPorts)
	portsBySwitch := indexPortsBySwitch(currentSwitches, currentPorts)

	result := &ReconcileResult{}
	var ops []ovsdb.Operation

	// --- desired switches and their ports ---
	for _, want := range desired.Switches {
		var exists *LogicalSwitch
		for i := range currentSwitches {
			if currentSwitches[i].Name == want.Name {
				exists = &currentSwitches[i]
				break
			}
		}

		switchRef := ""
		if exists == nil {
			switchRef = namedRef("sw", want.Name)
			newSwitch := &LogicalSwitch{
				UUID:        switchRef,
				Name:        want.Name,
				Ports:       []string{},
				OtherConfig: want.OtherConfig,
				ExternalIDs: want.ExternalIDs,
			}
			createOps, err := c.api.Create(newSwitch)
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build create op for logical switch %q: %w", want.Name, err)
			}
			ops = append(ops, createOps...)
			result.Created++
		} else {
			switchRef = exists.UUID
			if !mapsEqual(exists.OtherConfig, want.OtherConfig) || !mapsEqual(exists.ExternalIDs, want.ExternalIDs) {
				// The update row and the field pointers must be the same model.
				switchTarget := &LogicalSwitch{
					UUID:        exists.UUID,
					OtherConfig: want.OtherConfig,
					ExternalIDs: want.ExternalIDs,
				}
				updateOps, err := c.api.Where(exists).Update(switchTarget,
					&switchTarget.OtherConfig, &switchTarget.ExternalIDs)
				if err != nil {
					return nil, fmt.Errorf("ovn: cannot build update op for logical switch %q: %w", want.Name, err)
				}
				ops = append(ops, updateOps...)
			}
		}

		wantPorts := map[string]LogicalPortSpec{}
		for _, port := range want.Ports {
			wantPorts[port.Name] = port
		}

		for _, portName := range sortedPortNames(wantPorts) {
			port := wantPorts[portName]
			if portExists := switchByPort[portName]; portExists != nil {
				if port.equalModel(portExists) {
					continue
				}
				target := port.toModel()
				target.UUID = portExists.UUID
				updateOps, err := c.api.Where(portExists).Update(
					target,
					&target.Type, &target.Addresses, &target.Options,
					&target.PortSecurity, &target.ExternalIDs,
				)
				if err != nil {
					return nil, fmt.Errorf("ovn: cannot build update op for port %q: %w", portName, err)
				}
				ops = append(ops, updateOps...)
				continue
			}

			newPort := port.toModel()
			newPort.UUID = namedRef("port", portName)
			createOps, err := c.api.Create(newPort)
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build create op for port %q: %w", portName, err)
			}
			ops = append(ops, createOps...)
			refOps, err := mutatePortReference(c.api, switchRef, ovsdb.MutateOperationInsert, newPort.UUID)
			if err != nil {
				return nil, err
			}
			ops = append(ops, refOps...)
			result.Created++
		}

		// Ports currently on this switch that are no longer desired. Gated too:
		// this host's view of a switch can be narrower than the database's when
		// a VM lives on another node sharing the same logical switch.
		for _, portName := range sortedNames(portsBySwitch[want.Name]) {
			if !desired.Prune {
				break
			}
			if _, keep := wantPorts[portName]; !keep {
				stale := switchByPort[portName]
				if stale == nil {
					continue
				}
				deleteOps, err := c.api.Where(stale).Delete()
				if err != nil {
					return nil, fmt.Errorf("ovn: cannot build delete op for port %q: %w", portName, err)
				}
				ops = append(ops, deleteOps...)
				refOps, err := mutatePortReference(c.api, switchRef, ovsdb.MutateOperationDelete, stale.UUID)
				if err != nil {
					return nil, err
				}
				ops = append(ops, refOps...)
				result.Deleted++
			}
		}
	}

	// --- switches that are no longer desired ---
	// Only when the caller opts in: see the Prune field.
	for i := range currentSwitches {
		if !desired.Prune {
			break
		}
		if containsSwitch(desired.Switches, currentSwitches[i].Name) {
			continue
		}
		stale := &currentSwitches[i]
		for _, portName := range sortedNames(portsBySwitch[stale.Name]) {
			port := switchByPort[portName]
			if port == nil {
				continue
			}
			deleteOps, err := c.api.Where(port).Delete()
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build delete op for port %q: %w", portName, err)
			}
			ops = append(ops, deleteOps...)
			result.Deleted++
		}
		deleteOps, err := c.api.Where(stale).Delete()
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build delete op for logical switch %q: %w", stale.Name, err)
		}
		ops = append(ops, deleteOps...)
		result.Deleted++
	}

	// --- desired routers ---
	for _, want := range desired.Routers {
		var exists *LogicalRouter
		for i := range currentRouters {
			if currentRouters[i].Name == want.Name {
				exists = &currentRouters[i]
				break
			}
		}
		if exists == nil {
			createOps, err := c.api.Create(&LogicalRouter{
				Name:        want.Name,
				Ports:       []string{},
				Options:     want.Options,
				ExternalIDs: want.ExternalIDs,
			})
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build create op for logical router %q: %w", want.Name, err)
			}
			ops = append(ops, createOps...)
			result.Created++
			continue
		}
		if !mapsEqual(exists.Options, want.Options) || !mapsEqual(exists.ExternalIDs, want.ExternalIDs) {
			// The update row and the field pointers must be the same model.
			routerTarget := &LogicalRouter{
				UUID:        exists.UUID,
				Options:     want.Options,
				ExternalIDs: want.ExternalIDs,
			}
			updateOps, err := c.api.Where(exists).Update(routerTarget,
				&routerTarget.Options, &routerTarget.ExternalIDs)
			if err != nil {
				return nil, fmt.Errorf("ovn: cannot build update op for logical router %q: %w", want.Name, err)
			}
			ops = append(ops, updateOps...)
		}
	}

	// --- routers that are no longer desired ---
	// Gated for the same reason as the switch prune.
	for i := range currentRouters {
		if !desired.Prune {
			break
		}
		if containsRouter(desired.Routers, currentRouters[i].Name) {
			continue
		}
		deleteOps, err := c.api.Where(&currentRouters[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build delete op for logical router %q: %w", currentRouters[i].Name, err)
		}
		ops = append(ops, deleteOps...)
		result.Deleted++
	}

	if err := c.transact(ctx, ops, "reconcile OVN topology"); err != nil {
		return nil, err
	}
	return result, nil
}
