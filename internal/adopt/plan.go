package adopt

import "sort"

// ActionKind is what the adopter intends to do with one discovered VM.
type ActionKind string

const (
	// ActionAdopt: new to vmlet, healthy — take it over.
	ActionAdopt ActionKind = "adopt"
	// ActionSkipExisting: already registered in vmlet; leave the existing
	// record alone.
	ActionSkipExisting ActionKind = "skip-existing"
	// ActionSkipUnhealthy: no config.json, no root disk, or no systemd unit —
	// adopting it would register a VM vmlet cannot manage.
	ActionSkipUnhealthy ActionKind = "skip-unhealthy"
)

// Action is one line of an adoption plan: what to do with a VM, and why.
type Action struct {
	Kind      ActionKind `json:"kind"`
	VMID      string     `json:"vm_id"`
	Reason    string     `json:"reason,omitempty"`
	Discovery *Discovery `json:"discovery,omitempty"`
}

// Plan diffs what was found on disk against what vmlet already knows.
//
// existing is the list of VM ids vmlet already has records for. The result is
// deterministic (input sorted by VM id) so a dry run and the apply that follows
// it always agree. A VM that is both known and unhealthy is reported as
// skip-existing: vmlet's record wins, the adopter does not touch it.
func Plan(discoveries []Discovery, existing []string) []Action {
	known := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		known[id] = struct{}{}
	}

	sorted := make([]Discovery, len(discoveries))
	copy(sorted, discoveries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	actions := make([]Action, 0, len(sorted))
	for _, d := range sorted {
		if _, ok := known[d.ID]; ok {
			actions = append(actions, Action{
				Kind:      ActionSkipExisting,
				VMID:      d.ID,
				Reason:    "already registered in vmlet",
				Discovery: &d,
			})
			continue
		}
		if reasons := unhealthyReasons(d); len(reasons) > 0 {
			actions = append(actions, Action{
				Kind:      ActionSkipUnhealthy,
				VMID:      d.ID,
				Reason:    joinReasons(reasons),
				Discovery: &d,
			})
			continue
		}
		actions = append(actions, Action{
			Kind:      ActionAdopt,
			VMID:      d.ID,
			Reason:    "healthy VM on disk, not yet registered",
			Discovery: &d,
		})
	}
	return actions
}

// Counts tallies the actions by kind — the numbers an operator reads first.
func Counts(actions []Action) map[ActionKind]int {
	counts := make(map[ActionKind]int, 3)
	for _, a := range actions {
		counts[a.Kind]++
	}
	return counts
}

// Adoptable returns the discoveries the plan would adopt, in order.
func Adoptable(actions []Action) []Discovery {
	var out []Discovery
	for _, a := range actions {
		if a.Kind == ActionAdopt {
			out = append(out, *a.Discovery)
		}
	}
	return out
}

// unhealthyReasons lists everything that makes a discovery unmanageable.
func unhealthyReasons(d Discovery) []string {
	var reasons []string
	if !d.ConfigReadable {
		reasons = append(reasons, "config.json is missing or unparseable")
	}
	if !d.RootDiskExists {
		reasons = append(reasons, "root disk does not exist")
	}
	if !d.UnitPresent {
		reasons = append(reasons, "no systemd unit")
	}
	return reasons
}

// joinReasons renders the reasons as one human-readable string.
func joinReasons(reasons []string) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}
