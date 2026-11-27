# Changelog

All notable changes to vmlet are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](docs/VERSIONING.md).

- OVN operator guide ([docs/OVN.md](docs/OVN.md)): architecture diagram and
  the control-plane/data-plane split (which is the key to debugging), host
  prerequisites and `scripts/setup-ovn-host.sh` usage, the
  `MICROVM_NETWORK_BACKEND` selection and OVN config block, the derived naming
  scheme and `external_ids` conventions (including "never hand-edit
  vmlet-managed rows"), copy-pasteable verification commands, MTU/Geneve tuning
  with the path-MTU black-hole symptom spelled out, and a layer-by-layer
  troubleshooting section with `ovn-trace` / `ovs-ofctl` / `ovn-nbctl` and a
  symptom table.

- OVN Northbound protocol fixes found by running against a real ovsdb-server:
  named-UUIDs are sanitised to `[a-zA-Z0-9_]` because ovsdb-server rejects
  hyphens and dots in `uuid-name` (libovsdb's own server accepts them, so this
  was invisible until now); `EnsureDHCPOptions` returns the resolved UUID
  rather than a named-UUID, since a named-UUID only resolves inside the
  transaction that created it; and transaction errors now carry the server's
  `details`, which is the only place a schema or protocol mismatch is
  actually explained.

- VM creation accepts `network.mode: "ovn"`. The create handler's network-mode
  switch had cases for static/nat/dhcp/macvlan/ipv6-only and fell through to
  NAT for anything else, so a request for OVN was silently NAT-routed instead
  of failing.

- OVN-mode VMs are now actually wired: `CreateMicroVM` includes OVN in the
  modes that go through `ConfigureNetwork`, and the network manager holds the
  OVN backend's allocator so a VM's logical port, DHCP reservation and
  port security are created (instead of a placeholder address). Previously
  `mode: "ovn"` booted a VM with no logical port and no br-int attachment.

- OVN port names and address allocation fixed after live testing:
  `internal/firecracker`'s port name was truncated to the 8-char VM id while
  `internal/ovn` used the full one, so the OVS `iface-id` disagreed with the
  logical port and ovn-controller never bound them (only the TAP name needs to
  fit the 15-char IFNAMSIZ limit, and that truncation happens separately).
  `OVNBackend.subnet()` also produced a dotted netmask instead of CIDR, which
  `net.ParseCIDR` rejects, and `AllocateSubnetIP` started at the gateway
  address, handing the router's own IP to the first VM.

- OVN ACLs are now bound to their switch. `EnsureACL` wrote the `ACL` row but
  never listed it in `Logical_Switch.acls`, which is a strong reference: an
  unlisted row is orphaned and OVN never installs the rule, so security groups
  silently did nothing. `DropVMACLs` and `PruneACLs` now remove the reference as
  well as the row. This is the same defect class that affected logical ports and
  NAT rows.

- OVN guests no longer boot with a static address on the NAT-mode gateway.
  `generateEnhancedConfig` only excluded DHCP from its static-`ip=` branch, so
  an OVN VM (which has an allocated address) took it and got
  `ip=<ip>::192.168.100.1:...` — a hardcoded gateway that does not exist on an
  OVN network, leaving the guest with a default route straight off the overlay.
  OVN now boots with `ip=dhcp`, which is what the `DHCP_Options` row bound to
  its logical port actually serves.

- Floating IPs for OVN VMs: `AttachFloatingIP` / `DetachFloatingIP` /
  `ListFloatingIPs` on the OVN backend map a public address onto a VM's private
  address as a DNAT row carrying the VM's MAC, so OVN answers ARP for the
  external address. Detach is idempotent and reconciles from the database when
  the in-process cache has no entry, so a rule left by an earlier process is
  still withdrawn. Exposed as `POST`/`GET /vms/{id}/floating-ip` and
  `DELETE /vms/{id}/floating-ip/{address}`, which answer 400 on a non-OVN
  backend and 404 for an unknown VM.

### Fixed
- Three per-VM ownership bugs on OVN resources that are shared host-wide. Each
  resource (ACL rows, NAT rows) lives on a switch or router rather than on a
  port, so code that derived the desired state from a cache in the wrong order,
  or from a key that omitted the VM, let one VM's reconcile overwrite or free
  another's:
  - `AttachFloatingIP` recorded the new address *after* reconciling, so it always
    reconciled to the previous state - the DNAT row was never created, and any
    existing rows were pruned. It now records first.
  - `EnsureACL` keyed its row on `(switch, priority, direction, match)` with a
    deterministic UUID, so two VMs with the same rule converged onto one row: the
    second VM rewrote its external_ids, and releasing whichever VM held it last
    freed a rule its neighbours relied on. The VM is now part of both the row
    name and the lookup key.
  - `ReleaseVMNetwork` called `ReconcileNAT(nil, nil)`, flushing every NAT row on
    the router: releasing one VM silently removed every other VM's public address.
    It now reconciles the VMs that remain.
- `floatingSpecs` stamped one MAC onto every DNAT row; each row now carries the
  MAC of the VM it targets, or OVN answers ARP for the external address with the
  wrong guest.
- `vmEndpoint` no longer returns an address read from OVN without caching it, so
  a restart can still build a DNAT row (the MAC comes from that cache).

### Added
- Security-group ACLs are wired to the VM lifecycle: `ovn.DefaultVMRules` builds
  the baseline rule set and `AllocateVMNetwork` installs it, completing item
  1.6. The baseline is allow-only and scoped to the VM's own address - bridge
  mode installs no deny rule either, and restating its anti-spoof drop here would
  drop the VM's own DHCP discovery, whose source is `0.0.0.0` until it is offered
  an address. Drop rules need a security-group model the VM state does not have.
- `SecurityRule.MatchExpr` for rules scoped to a specific port address.

### Fixed
- `EnsureDHCPOptions` passed a value where libovsdb needs a pointer when
  building the update op for an existing `DHCP_Options` row, so `AllocateVMNetwork` failed on
  any host whose DHCP row already existed with different options - which is every
  host provisioned by `scripts/setup-ovn-host.sh`, since it writes Google DNS and
  vmlet derives Cloudflare's. Found by running the VM path against a live
  Northbound database. The same value form on the NAT drift path would have
  failed to correct a DNAT row's MAC.

### Fixed
- **`ReconcileTopology` deleted logical switches and routers outside its desired
  set.** The desired set is only as wide as the networks this process knows
  about, so a host serving one VPC deleted every other VPC's switch and router,
  taking that subnet and every VM attached to it down. Pruning is now opt-in via
  `MICROVM_OVN_PRUNE` (default off) and `Topology.Prune`; `TopologyFromNetworks`
  does not set it. The port-level prune on a switch we do own is gated the same
  way, because a VM on another node can share that logical switch. Verified live:
  re-running with a single-network desired set left the existing topology intact.

### Added
- Outbound NAT (SNAT) per VPC, so a VM without a public address still has a path
  to the internet - the AWS NAT Gateway / GCP Cloud NAT role. Masquerades the
  VPC's whole subnet to `Network.ExternalIP`, which defaults to the host's own
  address and can be pinned on the network. The desired set passed to
  `ReconcileNAT` is complete (floating IPs + SNAT) because it prunes anything not
  listed, and `ReleaseVMNetwork` re-derives the SNAT row so releasing a VM no
  longer drops the VPC's outbound path.
- Security groups with allow *and deny* rules: `models.SecurityRule`,
  `network.ToOVNRules`, and `PUT`/`GET /vms/{id}/security-rules`. Rules persist
  in a `vm_security_rules` table, are re-staged on every allocation so they
  survive a restart and a reboot, and sit above the allow-only baseline by
  priority. Validation rejects an unsupported protocol before storing, so a bad
  rule cannot half-apply.
- Network ACLs (stateless, per-VPC): `PUT`/`GET /networks/{id}/firewall`,
  persisted as JSON on the network row. These are the AWS Network ACL / GCP
  firewall equivalent and are evaluated for all traffic on the logical switch,
  before any per-VM rule.
- VPC peering: `POST /networks/{id}/peer` creates a pair of peered router ports
  on a stable /31 out of 169.254.0.0/16 plus a static route for each side's
  whole CIDR, so two VPCs exchange routes while keeping separate broadcast
  domains. Overlapping subnets are refused, since identical prefixes make the
  routes ambiguous. Requires `Logical_Router.static_routes`, now modelled.
- `ovn.ValidateSubnetIP`: a static address must be parseable, inside the subnet,
  not the gateway, and not already held by another VM - checked inside the
  allocator, not only at the API edge, so no path can bypass it.

### Fixed
- **`recoverFromNetworkInterfaceError` fabricated a substitute TAP and reported
  success.** It matched on `"tap"` or `"interface"` in the error text, which
  appears in nearly every network error, then deleted and recreated an unrelated
  interface named `tap<hash of vm id>` and returned `nil` — "success". That
  turned a loud, correct failure into a silent misconfiguration: the recovery
  produced a second, unbound TAP that the rest of creation then used, so a VM
  created with `network.mode: "ovn"` booted on the wrong bridge with the overlay
  absent from the packet path. It now reports the original cause unchanged and
  creates nothing.
- **TAP device names collided across VMs.** The name was `fc-tap-` plus the first
  8 characters of the VM id, so `ubuntu-ovn-1` and `ubuntu-ovn-2` both resolved
  to `fc-tap-ubuntu-o` and the second VM silently attached to the first VM's TAP.
  One call site also sliced `microvm.ID[:8]`, which panics on a short id.
  `ovnTAPName` now derives a unique, IFNAMSIZ-safe (15 character) name per VM
  with a stable hash suffix and is used by all four call sites.

### Added
- `internal/firecracker/tapname_test.go` covers both the collision and the
  stability of `ovnTAPName`, including ids that sanitise away entirely, 200
  characters long, and a single character.

## [Unreleased]

### Added
- Bridge → OVN migration (`internal/ovn/migrate.go`): a pure planner that turns
  current + desired per-VM state into an ordered step list — validate (a VM
  missing its tap/bridge/network/MAC is refused rather than half-migrated),
  detach from the old bridge, attach to `br-int` with the `iface-id` binding,
  reconcile the logical port/DHCP/security, then hand bridge-side rule cleanup
  to the bridge backend (the one TODO step). VMs already on OVN are only
  reconciled. `scripts/ovn-e2e-test.sh` exercises create → connect → migrate →
  snapshot → clone → delete against the OVN backend, asserting the VM keeps its
  IP across migration and that `br-int` is clean afterwards; a `--live` flag
  gates every destructive step so the script is a dry run by default.
- Floating IPs and distributed NAT (`internal/ovn`): `FloatingIP` / `SNATRule`
  map the `/vms/{id}/ipv4` addon lifecycle onto OVN `Logical_Router.nat` rows
  (DNAT with the VM's MAC so OVN answers ARP for the external IP, plus a
  masquerade SNAT for the subnet). `ReconcileNAT` attaches rows to the router
  in the same transaction that creates them, so a read after the write always
  sees an installed rule; detaching an addon removes both the row and the
  router reference. DNAT and SNAT for the same external IP coexist.
- Bandwidth QoS via OVS (`internal/ovs`): `SetPortQoS` / `SetPortIngressPolicing`
  map the existing ingress/egress limits to OVS QoS fifo queues (with
  `max-rate`/`min-rate` in bytes per second) and interface ingress policing,
  reproducing the tc implementation's rate parsing and burst heuristics
  (32k/64k/128k/256k/384k by rate band) so limits feel identical. Clearing a
  limit removes the QoS row and zeroes policing.
- Security groups via OVN ACLs (`internal/ovn`): declarative `SecurityGroup` /
  `SecurityRule` (ingress/egress, protocol, port range, peer) translated into
  `ACL` rows with OVN match expressions and direction-aware priorities (deny
  always above allow, deny rules logged), plus `PortSecurity` binding a VM's
  MAC/IP on its logical switch port so spoofed traffic is dropped at the
  logical port. The `MICROVM_ANTISPOOF_ENABLE` / `_IP_ENABLE` / `_STRICT`
  switches behave as they do on the ebtables backend, and ACLs are derived
  from VM state so they follow the VM and are pruned with it.
- OVN native DHCP and DNS (`internal/ovn`): `DHCP_Options` rows derived per
  subnet (cidr, netmask, gateway, DNS servers, lease time), MAC-keyed static
  reservations bound to logical switch ports via `dhcpv4_options`, and `DNS`
  records (`ip hostname`) that are updated in place and pruned when a VM
  disappears. Reservations are derived from desired state, so they survive a
  controller restart; clearing a weak reference requires a nil update value.
- Per-network logical topology (`internal/ovn`): `TopologyFromNetworks` /
  `TopologyFromVMState` derive one VM switch plus one gateway switch per network
  and a logical router per network, with deterministic OVN-safe names derived
  from the network name. VM switches only carry VM ports; router ports live on
  the gateway switch, so networks stay isolated and VMs cannot leak between them.
- OVS datapath package (`internal/ovs`): idempotent br-int bridge management,
  TAP attachment with `external_ids:iface-id` for ovn-controller binding, and a
  `VerifyPort` assertion; `scripts/setup-ovn-host.sh` + `systemd/ovn/*.service`
  provision a fresh OVN host in one command.
- OVN Northbound integration package (`internal/ovn`): libovsdb client to the
  `OVN_Northbound` DB plus an idempotent, single-transaction reconcile loop over
  logical switches, switch ports and routers, and an `ovn-nbctl` fallback.
- Pluggable network backend interface (`network.Backend`) with a registry and
  selection via `MICROVM_NETWORK_BACKEND` (default `bridge`); `noop` backend for
  tests/external networking, `ovn` reserved for roadmap item 1.2.
- Roadmap: OVN primary networking → multi-node cluster → DRS-style auto live
  migration (`ROADMAP.md`, GitHub issues #1–#30).
- Versioning policy (`docs/VERSIONING.md`); build-time version stamping via
  `-ldflags "-X main.ServiceVersion=…"`.

### Changed
- Public version line re-baselined to SemVer `0.x` (first public release =
  `v0.1.0`); `ServiceVersion` is now a build-stampable var.

### Removed
- Leftover Managed SQL / MySQL / PostgreSQL instructions from the deployment
  guide (vmlet state is embedded SQLite).

## [0.1.0] - 2026-11-23

First public open-source release.

### Added
- Single-node Firecracker microVM platform: REST API (`/api/v1`), web console
  with token-authenticated WebSocket access, creation queue workers.
- Networking: Linux bridge + TAP, dnsmasq DHCP (hosts files), optional MACVLAN,
  IPv6, anti-spoofing, `tc` bandwidth limits, SSH forwarding.
- Storage: local file, ZFS (snapshots/clones/send-recv), NBD; embedded SQLite
  state (`vms-state.db`).
- Migration: cold export/import and live migration over HTTP peer streams
  (disk / memory / vmstate) with preflight, abort and confirm.
- Memory: smart balloon, per-VM vertical autoscaling, memory guarantees and
  profiles.
- Images: multi-distro Docker-based builder (Alpine, Debian, Ubuntu, Fedora,
  AlmaLinux, Arch), vsock guest agent (password reset, SSH keys, config sync).
- Observability: Prometheus `/metrics`, health endpoints, structured logs.
- Docs: architecture, API, deployment, networking, storage, migration,
  monitoring, operations guides. MIT license.

[Unreleased]: https://github.com/haioco/vmlet/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/haioco/vmlet/releases/tag/v0.1.0
