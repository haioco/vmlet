package network

import (
	"testing"

	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/ovn"
)

func TestDirectionToOVN(t *testing.T) {
	if got := DirectionToOVN("ingress"); got != ovn.DirectionIngress {
		t.Errorf("ingress -> %q, want %q", got, ovn.DirectionIngress)
	}
	if got := DirectionToOVN("egress"); got != ovn.DirectionEgress {
		t.Errorf("egress -> %q, want %q", got, ovn.DirectionEgress)
	}
	// Unknown directions should not silently pass as egress.
	if got := DirectionToOVN("sideways"); got != ovn.DirectionIngress {
		t.Errorf("unknown -> %q, want %q (restrictive default)", got, ovn.DirectionIngress)
	}
}

func TestToOVNRulesRejectsBadProtocol(t *testing.T) {
	_, err := ToOVNRules([]models.SecurityRule{{Direction: "ingress", Protocol: "sctp", Allow: true}})
	if err == nil {
		t.Error("ToOVNRules accepted protocol \"sctp\"")
	}
}

func TestToOVNRulesDenySitsAboveAllow(t *testing.T) {
	allow := ovn.SecurityRule{Direction: ovn.DirectionIngress, AllowRule: true}
	deny := ovn.SecurityRule{Direction: ovn.DirectionIngress, AllowRule: false}
	if allow.Priority() >= deny.Priority() {
		t.Errorf("allow priority %d >= deny %d; an explicit drop must be evaluated first", allow.Priority(), deny.Priority())
	}
}

func TestToOVNRulesMatchOverrideWins(t *testing.T) {
	rules, err := ToOVNRules([]models.SecurityRule{{
		Direction: "ingress",
		Allow:     false,
		Match:     "inport == @filterport && ip4.dst == 10.0.0.9 && tcp.dst == 22",
	}})
	if err != nil {
		t.Fatalf("ToOVNRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if rules[0].Match() != "inport == @filterport && ip4.dst == 10.0.0.9 && tcp.dst == 22" {
		t.Errorf("match override lost: %q", rules[0].Match())
	}
	if rules[0].Action() != "drop" {
		t.Errorf("action = %q, want drop", rules[0].Action())
	}
}

func TestPeeringLinkIsStableAndDistinct(t *testing.T) {
	a1, b1 := peeringLinkForTest("lr-a", "lr-b")
	a2, b2 := peeringLinkForTest("lr-a", "lr-b")
	if a1 != a2 || b1 != b2 {
		t.Error("link addresses are not stable across calls")
	}
	if a1 == b1 {
		t.Error("both sides got the same link address")
	}
	other1, _ := peeringLinkForTest("lr-c", "lr-d")
	if other1 == a1 {
		t.Error("distinct peer pairs collided on a link subnet")
	}
}

func peeringLinkForTest(a, b string) (string, string) {
	return ovn.PeeringLink(a, b)
}
