# Changelog

All notable changes to vmlet are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](docs/VERSIONING.md).

## [Unreleased]

### Added
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
