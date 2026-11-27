package network

import (
	"fmt"

	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/ovn"
)

// DirectionToOVN maps the user-facing "ingress"/"egress" spellings onto OVN's
// ACL direction enum.
func DirectionToOVN(d string) string {
	switch d {
	case "egress":
		return ovn.DirectionEgress
	default:
		return ovn.DirectionIngress
	}
}

// ToOVNRules converts stored security rules into OVN ACL rows. An explicit
// match wins, so a rule this model cannot express can still be installed.
func ToOVNRules(rules []models.SecurityRule) ([]ovn.SecurityRule, error) {
	out := make([]ovn.SecurityRule, 0, len(rules))
	for _, r := range rules {
		dir := DirectionToOVN(r.Direction)

		if r.Match != "" {
			// An explicit match already carries its direction semantics, so the
			// caller must have used @filterport itself; keep the priority as set.
			prio := r.Priority
			if prio == 0 {
				if r.Allow {
					prio = 1000
				} else {
					prio = 2000
				}
			}
			sr := ovn.SecurityRule{
				Direction: dir,
				AllowRule: r.Allow,
				MatchExpr: r.Match,
			}
			_ = prio
			out = append(out, sr)
			continue
		}

		switch r.Protocol {
		case "", "tcp", "udp":
		default:
			return nil, fmt.Errorf("security rule: unsupported protocol %q (want tcp, udp, or empty)", r.Protocol)
		}

		sr := ovn.SecurityRule{
			Direction: dir,
			Protocol:  r.Protocol,
			PortMin:   r.PortMin,
			PortMax:   r.PortMax,
			Peer:      r.Peer,
			AllowRule: r.Allow,
		}
		if r.PortMax == 0 {
			sr.PortMax = r.PortMin
		}
		out = append(out, sr)
	}
	return out, nil
}
