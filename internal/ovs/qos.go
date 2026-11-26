package ovs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// QoSConfig is the per-port bandwidth limit vmlet applies on the OVS backend.
type QoSConfig struct {
	// Ingress is the rate in bits/second reaching the VM, e.g. "100mbit".
	Ingress string
	// Egress is the rate in bits/second leaving the VM.
	Egress string
}

// Applied reports whether anything would change. An empty QoSConfig clears
// the port's QoS configuration.
func (q QoSConfig) Applied() bool {
	return strings.TrimSpace(q.Ingress) != "" || strings.TrimSpace(q.Egress) != ""
}

// ParseBitRate parses the formats the tc implementation accepts ("100mbit",
// "1gbit", plain bits) into bits per second.
func ParseBitRate(rate string) (uint64, error) {
	cleaned := strings.ToLower(strings.TrimSpace(rate))
	if cleaned == "" {
		return 0, fmt.Errorf("ovs: empty rate")
	}

	// Order matters: check the longest suffixes first, otherwise "100mbit"
	// would match "bit" and leave the "m" behind.
	multiplier := uint64(1)
	for _, suffix := range []string{"gbit", "mbit", "kbit", "bit"} {
		if strings.HasSuffix(cleaned, suffix) {
			cleaned = strings.TrimSuffix(cleaned, suffix)
			multiplier = rateMultiplier(suffix)
			break
		}
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(cleaned), 64)
	if err != nil {
		return 0, fmt.Errorf("ovs: cannot parse rate %q: %w", rate, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("ovs: rate %q must be positive", rate)
	}
	return uint64(value * float64(multiplier)), nil
}

// rateMultiplier maps a rate suffix to its multiplier in bits per second.
func rateMultiplier(suffix string) uint64 {
	switch suffix {
	case "gbit":
		return 1_000_000_000
	case "mbit":
		return 1_000_000
	case "kbit":
		return 1_000
	default:
		return 1
	}
}

// BurstForRate mirrors the tc implementation's burst heuristic, so a limit set
// through OVS behaves the same as one set through tc (the same burst keeps
// short bursts smooth at low rates and bloated at high ones).
func BurstForRate(rateBits uint64) string {
	mbit := rateBits / 1_000_000
	switch {
	case mbit <= 20:
		return "32k"
	case mbit <= 50:
		return "64k"
	case mbit <= 100:
		return "128k"
	case mbit <= 200:
		return "256k"
	default:
		return "384k"
	}
}

// SetPortQoS applies a bandwidth limit to a port using an OVS QoS "fifo"
// queue, which is the OVS equivalent of tc's tbf token-bucket shaping.
//
// The queue and the rate column live on the interface row, so a re-run with the
// same limit rewrites the same column (idempotent), and an empty limit removes
// both. OVS applies the QoS table to egress; ingress policing is set by
// ovs-vsctl as an ingress-policing-rate on the interface.
func (c *Client) SetPortQoS(ctx context.Context, port string, qos QoSConfig) error {
	if port == "" {
		return fmt.Errorf("ovs: port name must not be empty")
	}

	if !qos.Applied() {
		return c.ClearPortQoS(ctx, port)
	}

	if err := c.setInterfaceColumn(ctx, port, "ingress_policing_rate", "0"); err != nil {
		return err
	}

	if egress := strings.TrimSpace(qos.Egress); egress != "" {
		rateBits, err := ParseBitRate(egress)
		if err != nil {
			return err
		}
		if err := c.setInterfaceColumn(ctx, port, "ingress_policing_burst", "0"); err != nil {
			return err
		}
		// Queue 0 carries the shaped egress rate; OVS reports the rate in
		// bytes/second on the QoS row, matching ovs-vsctl's own convention.
		if err := c.setQueueColumn(ctx, port, "0", rateBits); err != nil {
			return err
		}
		return nil
	}

	// Egress unset: drop any leftover shaping so a cleared limit is not left
	// behind on the interface.
	return c.clearQueues(ctx, port)
}

// SetPortIngressPolicing applies inbound rate limiting. OVS supports ingress
// policing natively on the interface, which is how the tc police filter is
// expressed on the OVS datapath.
func (c *Client) SetPortIngressPolicing(ctx context.Context, port string, ingress string) error {
	if port == "" {
		return fmt.Errorf("ovs: port name must not be empty")
	}

	rate := strings.TrimSpace(ingress)
	if rate == "" {
		return c.setInterfaceColumn(ctx, port, "ingress_policing_rate", "0")
	}

	rateBits, err := ParseBitRate(rate)
	if err != nil {
		return err
	}
	// OVS wants ingress_policing_rate in bytes/second.
	if err := c.setInterfaceColumn(ctx, port, "ingress_policing_rate", strconv.FormatUint(rateBits/8, 10)); err != nil {
		return err
	}
	return c.setInterfaceColumn(ctx, port, "ingress_policing_burst", BurstForRate(rateBits))
}

// ClearPortQoS removes all bandwidth limits from a port.
func (c *Client) ClearPortQoS(ctx context.Context, port string) error {
	if port == "" {
		return fmt.Errorf("ovs: port name must not be empty")
	}
	if err := c.setInterfaceColumn(ctx, port, "ingress_policing_rate", "0"); err != nil {
		return err
	}
	if err := c.setInterfaceColumn(ctx, port, "ingress_policing_burst", "0"); err != nil {
		return err
	}
	// Detaching the QoS row requires a `clear`, not an empty assignment.
	return c.clearQueues(ctx, port)
}

// setInterfaceColumn writes a column on an interface row. An empty value clears
// the column, which is how OVS removes a QoS or policing setting.
func (c *Client) setInterfaceColumn(ctx context.Context, port, column, value string) error {
	_, err := c.run(ctx, "set", "interface", port, columnAssignment(column, value))
	if err != nil {
		return fmt.Errorf("ovs: setting interface %s %s: %w", port, column, err)
	}
	return nil
}

// setQueueColumn creates the QoS row for a port and its queue.
func (c *Client) setQueueColumn(ctx context.Context, port, queueID string, rateBits uint64) error {
	// ovs-vsctl expresses a shaped queue as
	//   set port <port> qos=@newqos -- --id=@newqos create QoS type=fifo \
	//     other_config:max-rate=<bytes/s> queues:0=@newqueue \
	//     -- --id=@newqueue create Queue other_config:min-rate=<bytes/s>
	rateBytes := rateBits / 8
	_, err := c.run(ctx,
		"set", "port", port, "qos=@newqos",
		"--", "--id=@newqos", "create", "QoS", "type=fifo",
		fmt.Sprintf("other_config:max-rate=%d", rateBytes),
		"queues:0=@newqueue",
		"--", "--id=@newqueue", "create", "Queue",
		fmt.Sprintf("other_config:min-rate=%d", rateBytes),
	)
	if err != nil {
		return fmt.Errorf("ovs: creating QoS queue on port %s: %w", port, err)
	}
	return nil
}

// clearQueues detaches any QoS row from the port.
func (c *Client) clearQueues(ctx context.Context, port string) error {
	if _, err := c.run(ctx, "clear", "port", port, "qos"); err != nil {
		return fmt.Errorf("ovs: clearing QoS on port %s: %w", port, err)
	}
	return nil
}

// columnAssignment renders "column=value" or a bare column for clearing.
func columnAssignment(column, value string) string {
	if value == "" {
		return column
	}
	return column + "=" + value
}
