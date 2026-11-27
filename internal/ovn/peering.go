package ovn

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/ovsdb"
)

// PeerRouters connects two logical routers so their subnets exchange routes: a
// pair of peered router ports carrying a /31 link subnet, plus a static route on
// each router for the other's whole CIDR. This is how OVN expresses inter-VPC
// routing (the AWS VPC peering equivalent) - the routers are linked, not merged,
// so each VPC keeps its own broadcast domain.
//
// Idempotent: every row is named deterministically, so re-applying converges
// instead of duplicating.
func (c *Client) PeerRouters(ctx context.Context, routerA, routerB, cidrA, cidrB string) (*PeeringResult, error) {
	if routerA == "" || routerB == "" || routerA == routerB {
		return nil, fmt.Errorf("ovn: peering needs two distinct router names")
	}

	routers, err := c.ListRouters(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*LogicalRouter{}
	for i := range routers {
		byName[routers[i].Name] = &routers[i]
	}
	ra, ok := byName[routerA]
	if !ok {
		return nil, fmt.Errorf("ovn: logical router %q does not exist", routerA)
	}
	rb, ok := byName[routerB]
	if !ok {
		return nil, fmt.Errorf("ovn: logical router %q does not exist", routerB)
	}

	result := &PeeringResult{}
	var ops []ovsdb.Operation

	linkA, linkB := peeringLink(routerA, routerB)

	// One port per router, each the peer of the other.
	for _, side := range []struct {
		router *LogicalRouter
		name   string
		link   string
		peer   string
	}{
		{ra, PeeringPortName(routerA, routerB), linkA, PeeringPortName(routerB, routerA)},
		{rb, PeeringPortName(routerB, routerA), linkB, PeeringPortName(routerA, routerB)},
	} {
		if existing, _ := c.routerPortByName(ctx, side.name); existing != nil {
			continue
		}
		lrp := &LogicalRouterPort{
			UUID:        namedRef("lrp", side.name),
			Name:        side.name,
			Networks:    []string{side.link + "/31"},
			MAC:         deterministicMAC(side.name),
			Peer:        &side.peer,
			ExternalIDs: aclExternalIDs("peer", "", side.router.Name),
		}
		lrpOps, err := c.api.Create(lrp)
		if err != nil {
			return nil, fmt.Errorf("ovn: cannot build create op for peering port %q: %w", side.name, err)
		}
		ops = append(ops, lrpOps...)
		refOps, err := mutateRouterPortsAdd(c.api, side.router.UUID, lrp.UUID)
		if err != nil {
			return nil, err
		}
		ops = append(ops, refOps...)
		result.RouterPorts++
	}

	// Each router learns the other's whole subnet through the link.
	if cidrA != "" {
		if routeOps, added, err := c.peeringRouteOps(ctx, rb, cidrA, linkB, routerA, routerB); err != nil {
			return nil, err
		} else if added {
			ops = append(ops, routeOps...)
			result.Routes++
		}
	}
	if cidrB != "" {
		if routeOps, added, err := c.peeringRouteOps(ctx, ra, cidrB, linkA, routerB, routerA); err != nil {
			return nil, err
		} else if added {
			ops = append(ops, routeOps...)
			result.Routes++
		}
	}

	if len(ops) == 0 {
		return result, nil
	}
	if err := c.transact(ctx, ops, "peer OVN routers"); err != nil {
		return nil, err
	}
	return result, nil
}

// peeringRouteOps builds the static-route ops for one direction, if that route
// is not already installed.
func (c *Client) peeringRouteOps(ctx context.Context, router *LogicalRouter, prefix, nextHop, from, to string) ([]ovsdb.Operation, bool, error) {
	existing, err := c.ListStaticRoutes(ctx)
	if err != nil {
		return nil, false, err
	}
	want := namedRef("route", fmt.Sprintf("%s-via-%s", prefix, to))
	for _, r := range existing {
		if r.UUID == want {
			return nil, false, nil
		}
	}
	route := &StaticRoute{
		UUID:     want,
		IPPrefix: prefix,
		Nexthop:  nextHop,
		ExternalIDs: map[string]string{
			"vmlet-managed": "true",
			"vmlet-peer":    fmt.Sprintf("%s<->%s", from, to),
			"vmlet-network": from,
		},
	}
	createOps, err := c.api.Create(route)
	if err != nil {
		return nil, false, fmt.Errorf("ovn: cannot build create op for static route %s: %w", prefix, err)
	}
	ops := append([]ovsdb.Operation{}, createOps...)
	refOps, err := mutateRouterStaticRoutesAdd(c.api, router.UUID, route.UUID)
	if err != nil {
		return nil, false, err
	}
	ops = append(ops, refOps...)
	return ops, true, nil
}

// PeeringResult reports what a peering reconcile changed.
type PeeringResult struct {
	RouterPorts int
	Routes      int
}

// PeeringPortName is the deterministic router-port name for one side of a peer
// link.
func PeeringPortName(a, b string) string {
	return sanitizeUUIDName("pr-" + a + "-" + b)
}

// peeringLink picks a stable /31 pair out of 169.254.0.0/16 (link-local), so it
// never collides with a VPC's routable subnet.
func peeringLink(a, b string) (string, string) {
	h := 0
	for _, r := range a + "|" + b {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	base := uint32(169)<<24 | uint32(254)<<16 | uint32(h%0x3FFF)<<2
	ipA := base + 1
	ipB := base + 2
	return ipString(ipA), ipString(ipB)
}

// deterministicMAC derives a stable locally-administered MAC from the port name,
// so a re-applied peering link keeps the same hardware address.
func deterministicMAC(name string) string {
	h := 0
	for _, r := range name {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return fmt.Sprintf("02:pr:%02x:%02x:%02x:%02x",
		(h>>16)&0xff, (h>>8)&0xff, h&0xff, (h>>24)&0xff)
}

func ipString(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", v>>24&0xff, v>>16&0xff, v>>8&0xff, v&0xff)
}

// routerPortByName finds an existing router port by name.
func (c *Client) routerPortByName(ctx context.Context, name string) (*LogicalRouterPort, error) {
	rows := []LogicalRouterPort{}
	if err := c.api.WhereCache(func(r *LogicalRouterPort) bool { return r.Name == name }).List(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListStaticRoutes returns the static routes currently in the database.
func (c *Client) ListStaticRoutes(ctx context.Context) ([]StaticRoute, error) {
	rows := []StaticRoute{}
	if err := c.api.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("ovn: listing static routes: %w", err)
	}
	return rows, nil
}

// PeeringLink exposes the link-address derivation for tests.
func PeeringLink(a, b string) (string, string) { return peeringLink(a, b) }
