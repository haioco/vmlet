package ovs

import (
	"context"
	"strings"
	"testing"
)

func TestParseBitRate(t *testing.T) {
	cases := map[string]uint64{
		"100mbit":   100_000_000,
		"1gbit":     1_000_000_000,
		"10kbit":    10_000,
		"500bit":    500,
		" 250Mbit ": 250_000_000,
	}
	for in, want := range cases {
		got, err := ParseBitRate(in)
		if err != nil {
			t.Errorf("ParseBitRate(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBitRate(%q) = %d, want %d", in, got, want)
		}
	}

	for _, bad := range []string{"", "  ", "fast", "mbit", "-5mbit", "0mbit"} {
		if _, err := ParseBitRate(bad); err == nil {
			t.Errorf("ParseBitRate(%q) accepted an invalid rate", bad)
		}
	}
}

func TestBurstForRate_ParityWithTC(t *testing.T) {
	// These are the exact thresholds the tc implementation uses, so a limit
	// set through OVS produces the same burst and therefore the same feel.
	cases := []struct {
		rate string
		want string
	}{
		{"10mbit", "32k"},
		{"20mbit", "32k"},
		{"21mbit", "64k"},
		{"50mbit", "64k"},
		{"51mbit", "128k"},
		{"100mbit", "128k"},
		{"101mbit", "256k"},
		{"200mbit", "256k"},
		{"201mbit", "384k"},
		{"1gbit", "384k"},
	}
	for _, tc := range cases {
		bits, err := ParseBitRate(tc.rate)
		if err != nil {
			t.Fatalf("ParseBitRate(%q): %v", tc.rate, err)
		}
		if got := BurstForRate(bits); got != tc.want {
			t.Errorf("BurstForRate(%q) = %q, want %q", tc.rate, got, tc.want)
		}
	}
}

func TestQoSConfigApplied(t *testing.T) {
	if (QoSConfig{}).Applied() {
		t.Error("empty QoSConfig should report nothing to apply")
	}
	if (QoSConfig{Ingress: "  "}).Applied() {
		t.Error("whitespace-only values should report nothing to apply")
	}
	if !(QoSConfig{Egress: "100mbit"}).Applied() {
		t.Error("egress alone should be applied")
	}
	if !(QoSConfig{Ingress: "100mbit"}).Applied() {
		t.Error("ingress alone should be applied")
	}
}

func TestSetPortQoS_AppliesEgressShaping(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.SetPortQoS(ctx, "fc-tap-ab12", QoSConfig{Egress: "100mbit"}); err != nil {
		t.Fatalf("SetPortQoS: %v", err)
	}

	commands := strings.Join(f.commands, "\n")
	// 100mbit -> 12_500_000 bytes/s, burst 128k.
	if !strings.Contains(commands, "other_config:max-rate=12500000") {
		t.Errorf("max-rate not set for 100mbit:\n%s", commands)
	}
	if !strings.Contains(commands, "type=fifo") {
		t.Errorf("QoS type fifo missing:\n%s", commands)
	}
	if !strings.Contains(commands, "queues:0=@newqueue") {
		t.Errorf("queue binding missing:\n%s", commands)
	}
	if f.countContains("set port fc-tap-ab12 qos=") != 1 {
		t.Errorf("expected one QoS attach, got %d", f.countContains("set port fc-tap-ab12 qos="))
	}
}

func TestSetPortQOS_Idempotent(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	qos := QoSConfig{Egress: "100mbit"}
	if err := c.SetPortQoS(ctx, "fc-tap-ab12", qos); err != nil {
		t.Fatalf("SetPortQoS (first): %v", err)
	}

	// OVS columns are set with "=" so a repeat is a same-value rewrite, not an
	// accumulation; the observable effect is that the queue row is recreated
	// rather than duplicated alongside the old one.
	if err := c.SetPortQoS(ctx, "fc-tap-ab12", qos); err != nil {
		t.Fatalf("SetPortQoS (repeat): %v", err)
	}
	if got := f.countContains("create QoS"); got != 2 {
		t.Errorf("create QoS ran %d times, want 2 (both rewrites are equivalent)", got)
	}
}

func TestSetPortQoS_ClearsOnEmptyLimit(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.SetPortQoS(ctx, "fc-tap-ab12", QoSConfig{Egress: "100mbit"}); err != nil {
		t.Fatalf("SetPortQoS: %v", err)
	}
	if err := c.SetPortQoS(ctx, "fc-tap-ab12", QoSConfig{}); err != nil {
		t.Fatalf("SetPortQoS(clear): %v", err)
	}

	commands := strings.Join(f.commands, "\n")
	if !strings.Contains(commands, "clear port fc-tap-ab12 qos") {
		t.Errorf("clearing the limit did not remove the QoS row:\n%s", commands)
	}
}

func TestSetPortIngressPolicing(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.SetPortIngressPolicing(ctx, "fc-tap-ab12", "100mbit"); err != nil {
		t.Fatalf("SetPortIngressPolicing: %v", err)
	}

	commands := strings.Join(f.commands, "\n")
	// 100mbit -> 12_500_000 bytes/s, burst 128k.
	if !strings.Contains(commands, "ingress_policing_rate=12500000") {
		t.Errorf("policing rate not set:\n%s", commands)
	}
	if !strings.Contains(commands, "ingress_policing_burst=128k") {
		t.Errorf("policing burst not set:\n%s", commands)
	}

	// Clearing is a no-op rate of 0.
	if err := c.SetPortIngressPolicing(ctx, "fc-tap-ab12", ""); err != nil {
		t.Fatalf("SetPortIngressPolicing(clear): %v", err)
	}
	commands = strings.Join(f.commands, "\n")
	if !strings.Contains(commands, "ingress_policing_rate=0") {
		t.Errorf("clearing ingress policing did not zero the rate:\n%s", commands)
	}
}

func TestSetPortQoS_RejectsBadRates(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	if err := c.SetPortQoS(ctx, "fc-tap-ab12", QoSConfig{Egress: "not-a-rate"}); err == nil {
		t.Fatal("SetPortQoS accepted an invalid egress rate")
	}
	if err := c.SetPortIngressPolicing(ctx, "fc-tap-ab12", "nope"); err == nil {
		t.Fatal("SetPortIngressPolicing accepted an invalid ingress rate")
	}
	if err := c.SetPortQoS(ctx, "", QoSConfig{Egress: "100mbit"}); err == nil {
		t.Fatal("SetPortQoS accepted an empty port name")
	}
}

func TestSetPortQoS_PropagatesErrors(t *testing.T) {
	ctx := context.Background()
	f := newFakeOVS()
	c := fakeClient(f)

	f.failNext("set interface", 1)
	if err := c.SetPortQoS(ctx, "fc-tap-ab12", QoSConfig{Egress: "100mbit"}); err == nil {
		t.Fatal("SetPortQoS ignored a failing command")
	}
}

func TestColumnAssignment(t *testing.T) {
	if got := columnAssignment("qos", "@newqos"); got != "qos=@newqos" {
		t.Errorf("columnAssignment(set) = %q", got)
	}
	if got := columnAssignment("qos", ""); got != "qos" {
		t.Errorf("columnAssignment(clear) = %q, want the bare column", got)
	}
}
