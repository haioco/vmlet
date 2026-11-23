# Versioning Policy

vmlet follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)
(SemVer). This document defines what our version numbers mean, how releases
are cut, and how the road to **v1.0.0** is structured.

## Public API

SemVer requires a declared public API. For vmlet it is:

- the REST API under `/api/v1` (routes, request/response shapes),
- the `vmlet-http` CLI flags and `MICROVM_*` environment variables,
- the JSON configuration file schema (`configs/*.json`),
- the on-disk state format (`vms-state.db`) and image layout.

Internal Go packages (`internal/…`) are **not** public API and may change at
any time, even in patch releases.

## Re-baseline: why we start at 0.1.0

The code previously carried an internal service version (`2.0.0`) from its
proprietary ancestry. That number was never a public SemVer contract — no
public releases, tags or compatibility promises existed. Open-sourcing resets
the public version line:

- **v0.1.0** — first public release: single-node Firecracker host (current
  `main`).
- Per SemVer, `0.y.z` means *initial development*: anything may change, the
  public API is not yet stable. This is the honest signal while the
  architecture evolves toward clustering.

## Version plan

| Version | Theme | Content |
|---|---|---|
| **v0.1.0** | Single node | Current open-source release: REST API, console, bridge networking, ZFS/local storage, snapshots, manual live migration, balloon + vertical autoscaling |
| **v0.2.0** | OVN networking | Phase 1 of the [roadmap](../ROADMAP.md): OVN primary backend, native DHCP/DNS, ACL security groups, OVS QoS, floating IPs |
| **v0.3.0** | Multi-node cluster | Phase 2: membership, replicated state, controller/agent split, scheduler, cluster API + mTLS, drain/maintenance |
| **v0.4.0** | DRS | Phase 3: demand model, imbalance detection, migration planner, automated rebalancing, dry-run, rules |
| **v1.0.0** | **GA** | OVN + cluster + DRS all generally available; public API frozen under SemVer guarantees |

During `0.x`, each phase ships as a **minor** bump; breaking changes are
allowed (and documented in the changelog) without a major bump — that is what
`0.x` is for.

### v1.0.0 exit criteria

v1.0.0 is tagged only when **all** of the following hold:

1. OVN is the default network backend; bridge mode remains supported.
2. A 3-node cluster passes the e2e suite: placement, drain, leader failover,
   mid-migration node loss.
3. DRS rebalances a loaded cluster within configured limits, with dry-run
   parity and a full audit trail.
4. A VM live-migrated between nodes keeps IP, MAC, security groups and
   connections.
5. The REST API is documented (OpenAPI), versioned (`/api/v1`), and covered by
   compatibility tests.
6. Authentication/authorization is enabled by default for cluster endpoints.
7. Upgrade path from v0.1 single-node installs is documented and tested.

### Pre-releases

Unstable builds use SemVer pre-release identifiers:

```
v0.2.0-alpha.1   early integration, may break
v0.2.0-beta.1    feature-complete, hardening
v0.2.0-rc.1      release candidate, no known blockers
v0.2.0           stable phase release
```

Pre-releases are published as GitHub Releases marked *pre-release* and are
never recommended for production.

## After 1.0.0

Standard SemVer rules apply:

- **MAJOR** — incompatible public API changes (e.g. `/api/v2` replacing v1
  semantics, config schema breaks).
- **MINOR** — backward-compatible functionality; also used to *deprecate*
  functionality (deprecation must ship in at least one minor release before
  removal in the next major).
- **PATCH** — backward-compatible bug fixes and security fixes.

## Releases, tags and branches

**Tags are the source of truth for versions — not branches.**

- Every release is an annotated git tag `vX.Y.Z` on `main` (or on a release
  branch for patches), published as a GitHub Release with notes.
- `CHANGELOG.md` follows [Keep a Changelog](https://keepachangelog.com/);
  entries are grouped Added / Changed / Deprecated / Removed / Fixed /
  Security.
- Released tags are immutable: never re-tag or rewrite a published version.

### Branching model (trunk-based)

- `main` is always releasable; CI must pass.
- Work happens on **short-lived feature branches** (`feat/ovn-backend`,
  `fix/balloon-race`) merged via PR.
- `release-0.2` style branches exist **only** to stabilize a release and carry
  its patch versions; they are cut from `main` at feature freeze and deleted
  after the release line is superseded.
- **There is no long-lived "v1 branch".** Developing v1.0 on a permanently
  diverging branch causes merge drift and double maintenance. Instead, phase
  work lands on `main` behind toggles (e.g. `MICROVM_NETWORK_BACKEND=bridge|ovn`)
  and defaults that keep single-node users safe; the *version* is expressed by
  milestones and tags, not by a branch.

### Go module compatibility

The module path is `github.com/haioco/vmlet`. Go's module rules require a
`/vN` path suffix only for major versions ≥ 2. Staying on the `0.x → 1.x`
line keeps the import path stable; if a breaking v2 ever happens, the module
becomes `github.com/haioco/vmlet/v2` per Go convention.

## Build stamping

`ServiceVersion` in `cmd/http-server/main.go` defaults to the development
version and is stamped at build time:

```bash
go build -ldflags "-X main.ServiceVersion=$(git describe --tags --always)" \
  -o vmlet-http ./cmd/http-server
```

The stamped version is reported by `GET /health` and the startup banner.

## Commit conventions

[Conventional Commits](https://www.conventionalcommits.org/) are recommended
(`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`) so changelogs and future
automated versioning can be derived from history.

## Support policy

- The latest minor release line receives bug fixes; the previous line receives
  security fixes only.
- Security issues: see `SECURITY.md` (private reporting), never public issues.
