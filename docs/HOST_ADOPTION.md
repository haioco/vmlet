# Adopting a haioflash node

This is the runbook for replacing haioflash with vmlet on a node that is already
running production VMs. Written against a real audit of `node62`
(`root@94.182.94.8`, Ubuntu 22.04.5, 56 CPU / 251 GB RAM, ZFS pool `default`).

The rule the whole runbook rests on: **the running VMs are never touched.** The
adopter only reads — the ZFS dataset list, the work directory, each VM's
`config.json`, and each VM's systemd unit. It never starts, stops, restarts or
reconfigures a VM, and never re-creates the `fc-*` devices the VMs are already
bridged onto. haioflash can keep running in parallel on its own port until you
are satisfied.

## What the node looks like today

Measured with the adopter itself, 28 Nov 2026 (`vmlet-adopt scan`, read-only):

| | |
|---|---|
| VM datasets found | 84 (1.84 TiB) |
| Work dirs found | 49, 49/49 configs parse |
| Unit files found | 1 static (the rest are template instances) |
| Adoptable VMs | **48** (41 running, 7 failed units) |
| Already registered | 0 |
| Skipped unhealthy | 37 (36 zvols with no work dir; 1 config pointing at a vanished device) |
| Guest IPs resolved | 46/48 from the host neighbour table — all inside 94.182.94.0/24 or 94.182.172.64/26 |
| Boot-enabled | 0 |

The 48 live VMs run as `firecracker-vm@<id>.service` template instances, which
report `UnitFileState=disabled` while running, so health is decided by the unit
being *present*, never by it being enabled.

Measured on node62, November 2026:

| Thing | Value |
|---|---|
| Pool | `default`, 1.09 T, 368 G free, ONLINE |
| VM datasets | `default/haioflash-microvms`, 624 G, ~85 live VMs |
| Migration staging | `default/haioflash-mig` → `/opt/haioflash/mig`, 334 G avail |
| VM root disks | ZFS zvols: `/dev/zvol/default/haioflash-microvms/vm-<id>` |
| VM config | `/opt/haioflash/work/microvms/<id>/config.json` |
| VM units | static `firecracker-vm-<id>.service` in `/etc/systemd/system`, many enabled |
| Kernel | `/opt/haioflash/kernels/vmlinux` |
| Firecracker | `/opt/haioflash/bin/firecracker` for most VMs, **`/usr/local/bin/firecracker` v1.13.1 for some** |
| Public bridge | `br0`, carrying `94.182.94.8/24` **and** `94.182.172.64/26` (64 routable addresses) |
| VM NICs | ~50 `fc-<hex>` devices, all with `master: br0` |
| NAT bridge | `br-microvm` at `10.100.0.1/24`, **DOWN** |
| NAT rules | `iptables -t nat` = 11 rules; filter table 177 — mostly bridged, very little NAT |
| haioflash API | `http-server` on `:8080` |
| VM catalog | **none.** MySQL holds only a managed-SQL product DB; all VM state is on the filesystem |

Consequences worth internalising before you touch anything:

- **Public IPs are already per-VM on the interface.** Each VM's `fc-*` device is
  a persistent TAP (`tun type tap … master br0`) bridged onto `br0`, which
  carries routable public space. That is the "public IP without OVN" model
  working today; nothing needs to be built for it.
- **The VM's own systemd unit creates that TAP**, in its ExecStartPre:
  `ip link show fc-<hex> || (ip tuntap add dev fc-<hex> mode tap && brctl addif br0 fc-<hex>)`.
  The platform never creates it. Adoption must therefore never re-create a VM's
  NIC — which is why adopted records carry `NetworkModePreWired`, a mode for
  which vmlet's network layer is a deliberate no-op.
- **There is no database to migrate.** The adopter's job is to *create* the
  registry, from the filesystem.
- **Two firecracker binaries are in play.** Each VM's unit pins one, and 9 of the
  42 running processes do not match their unit's pin (both v1.13.1, so harmless
  today, but a blanket upgrade would break the ones on the older path). The
  adopter records the pin; leave them pinned.
- **Port 8080 is taken.** vmlet must run on another port while haioflash is up.
- **Deleting an adopted VM reaches the host's disk.** Adopted records point at
  the host's zvol through the `external_rootfs_path` label (the same label
  already used for migrated VMs), so `DELETE /vms/{id}` on one would touch a
  dataset under `default/haioflash-microvms`. Lifecycle control of adopted VMs
  is deliberately **not** part of this change; decide which dataset owns each
  zvol before enabling it. Keep the port unreachable from anything but a trusted
  network: this repo puts no auth on `/admin/*`.

## Adopting a node

### 1. Dry run first

```bash
# Read-only: reports what vmlet can see on disk. Changes nothing.
curl -s -X POST http://localhost:8081/admin/adopt/scan | jq
```

Expect roughly 85 discoveries, each with its zvol, config path, vcpu/memory,
guest MAC, `host_dev_name`, pinned firecracker binary, and whether it is
currently running. Inspect the `skip-unhealthy` reasons: a VM with no
`config.json` or no disk is usually one that was deleted by hand.

### 2. Apply

```bash
curl -s -X POST http://localhost:8081/admin/adopt/apply | jq
```

This writes records into vmlet's own store. Nothing is restarted; the running
firecracker processes and their `fc-*` devices are untouched. After this, the VMs
appear in `GET /vms` with their real vcpu, memory, MAC, tap device and guest
address.

**What you gain is visibility, not control.** Reading is safe. Start, stop,
resize and delete over an adopted VM are deliberately *not* wired up yet, because
each of them has an unanswered question about who owns the zvol — `delete` in
particular would reach `default/haioflash-microvms`, not a vmlet dataset.
Treat adopted VMs as read-only inventory until that decision is made, and keep
managing them the way you do today (their own units).

### 3. Stop haioflash, once you are satisfied

```bash
systemctl disable --now haioflash
```

Keep the binary and `/opt/haioflash` in place for a while — the root disks and
configs are still the source of truth, since vmlet has not taken over any VM's
lifecycle yet.

## vmlet configuration for an adopted host

```ini
# /etc/systemd/system/vmlet.service
[Service]
Environment=MICROVM_WORK_DIR=/opt/vmlet/data
Environment=MICROVM_KERNEL_PATH=/opt/haioflash/kernels/vmlinux
Environment=MICROVM_NETWORK_BACKEND=bridge
Environment=MICROVM_BRIDGE_ADOPT=true
Environment=MICROVM_NETWORK_BRIDGE=br0
Environment=MICROVM_MANAGE_NAT=false
Environment=MICROVM_OVN_NB_ENDPOINT=unix:/var/run/ovn/ovnnb_db.sock
Environment=PORT=8081
User=root
```

The two lines that matter most:

- **`MICROVM_BRIDGE_ADOPT=true`** — `br0` belongs to the host. vmlet verifies it
  exists, never addresses it, never runs DHCP on it, never writes NAT rules for
  it, and never deletes it. Without this, vmlet would add a MASQUERADE rule to a
  bridge carrying 64 public addresses.
- **`PORT=8081`** — haioflash's `http-server` owns 8080 until you stop it.

## Adding OVN to an adopted node (optional)

node62 has no OVS or OVN installed (all four services inactive) and `br-int` is
free. Because vmlet probes for the overlay at startup and falls back cleanly,
the two can coexist once you install the packages:

```bash
apt install openvswitch-switch ovn-central ovn-host
```

Keep the subnets disjoint — the adopted bridge carries real public ranges, so
the overlay must not overlap them:

```ini
Environment=MICROVM_OVN_NETWORK_CIDR=10.250.0.0/24
Environment=MICROVM_OVN_NETWORK_IP=10.250.0.1
```

Then `GET /network-modes` reports `ovn` alongside the bridged modes, and
`{"network": {"mode": "ovn"}}` VMs get the overlay while bridged VMs keep their
public IPs on `br0`. **Do this after** the adoption is proven, not before: the
overlay's failure modes would otherwise be indistinguishable from adoption
breakage.

## Multi-node caveat

`br-microvm` on node62 is `10.100.0.1/24` and **down**. If node57 runs a NAT
bridge on the same `10.100.0.0/24`, NAT-mode VMs on the two nodes will collide
addresses once any cross-node traffic exists. Check node57's bridge ranges
before enabling NAT mode anywhere, and give each node its own subnet.
## Implementation: the adopter

The adopter is `internal/adopt` (+ `cmd/vmlet-adopt`, the same thing as a CLI):

| File | What it does |
|---|---|
| `internal/adopt/adopt.go` | The read-only command allowlist every shell call goes through, and the size parsing |
| `internal/adopt/discovery.go` | `Scan`: zfs datasets + work dirs + static unit files → per-VM `Discovery` |
| `internal/adopt/plan.go` | `Plan`: diff discoveries against the ids vmlet already has → `adopt` / `skip-existing` / `skip-unhealthy` |
| `internal/adopt/apply.go` | `Apply`: healthy discoveries → `models.MicroVMInstance` records, saved through a `VMStore` |
| `internal/adopt/adopt_test.go` | Fixture-driven tests, including "Scan never issues a mutating command" |
| `cmd/http-server/adopt.go` | `POST /admin/adopt/scan`, `POST /admin/adopt/apply`, and the opt-in startup report |
| `cmd/vmlet-adopt/main.go` | `vmlet-adopt scan|plan|apply` for the same job from a shell |

Knobs (all optional, all off by default):

| Setting | Default | Meaning |
|---|---|---|
| `-adopt-scan` / `MICROVM_ADOPT_SCAN` | `false` | Startup report only (read-only). Discovery never runs implicitly |
| `MICROVM_ADOPT_WORK_DIR` | `/opt/haioflash/work/microvms` | Where the per-VM `config.json` files live |
| `MICROVM_ADOPT_ZFS_DATASET` | `default/haioflash-microvms` | Dataset holding the root disks (a path is resolved by mountpoint) |
| `MICROVM_ADOPT_UNIT_DIR` | `/etc/systemd/system` | Static unit files |
| `MICROVM_ADOPT_BRIDGE` | `br0` | Bridge recorded on the adopted records |
| `MICROVM_ADOPT_RESOLVE_IPS` | `true` | Read the guest IP from the host neighbour table |
| `MICROVM_ADOPT_CUSTOMER_ID` | `haioflash-adopted` | Customer stamp for records with no billing owner |

Adopted records carry `external_rootfs_path` (the zvol they already run on),
`Network.AssignmentMethod = macvlan`, the guest MAC, and the existing `fc-<hex>`
device as `Network.TAPDevice` — so vmlet reuses the disk and the device instead
of creating either. `auto_start` is **false**: an adopted VM is already running
its own unit, and vmlet must not decide on its own when it should come back
after a host reboot. Set it per VM once you have taken the units over.

## Measured on node62 (Nov 2026, after this branch)

```
$ vmlet-adopt scan
Read-only scan of /opt/haioflash/work/microvms (dataset default/haioflash-microvms)
  sources: zfs datasets=84 work dirs=49 unit files=1
  found 85 VMs; 0 already registered; plan: adopt=48 skip-existing=0 skip-unhealthy=37
  skip vm-0e7547f1: config.json is missing or unparseable; root disk does not exist
  ...
```

| Number | Value |
|---|---|
| VM datasets under `default/haioflash-microvms` | 84 volumes (1.84 TiB of root disks) |
| Per-VM dirs with a `config.json` | 49 — all parse, all have exactly 1 NIC |
| Static unit files in `/etc/systemd/system` | 1 (`firecracker-vm-46d812e1.service`, enabled, no work dir) |
| Discoveries / adopt / skip-unhealthy | 85 / 48 / 37 |
| Adoptable VMs running today | 41 (the other 7 are `failed` units) |
| Guest IP resolved from the neighbour table | 46 of 48 (the 2 unresolved are stopped VMs) |
| Pinned firecracker binary | 48/48 pin `/opt/haioflash/bin/firecracker` via `firecracker-vm@<id>.service` |
| `enabled` units among the adoptable | 0 — none of them are boot-enabled |

The 37 unhealthy ones are almost all leftovers: 36 zvols with no work dir at all
(deleted by hand, disk still around), plus `vm-VM91247697` whose config points at
`/dev/zd800p2`, a partition that no longer exists.

Two things a reviewer should know before trusting the output:

1. **The unit's pinned binary is not the binary of the live process.** 9 of the
   42 running VMs execute `/usr/local/bin/firecracker` while their unit pins
   `/opt/haioflash/bin/firecracker` (both are v1.13.1). The adopter records the
   pin; fixing those processes is a separate decision.
2. **This host no longer matches the units the runbook above describes.** There
   is one static unit left; the 48 live VMs are template instances
   (`firecracker-vm@<id>.service`). The adopter handles both shapes, and warns
   on the static one because vmlet polls `firecracker-vm@<id>.service` for
   liveness (`firecracker.Manager.IsVMProcessRunning`) — a static unit will read
   as "stopped" to vmlet while the VM runs.
