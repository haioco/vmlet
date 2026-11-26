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
