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
  bridged onto `br0`, which carries routable public space. That is the
  "public IP without OVN" model working today; nothing needs to be built for it.
- **There is no database to migrate.** The adopter's job is to *create* the
  registry, from the filesystem.
- **Two firecracker binaries are in play.** Each VM's unit pins one. A blanket
  upgrade of the binary breaks the VMs on the other one. The adopter records
  which binary each VM pins; leave them pinned.
- **Port 8080 is taken.** vmlet must run on another port while haioflash is up.

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
firecracker processes and their `fc-*` devices are untouched. After this, the
VMs appear in `GET /vms` and can be managed normally (status, resize, delete,
public-IP add-ons).

### 3. Stop haioflash, once you are satisfied

```bash
systemctl disable --now haioflash
```

Keep the binary and `/opt/haioflash` in place for a while — the root disks and
configs are the source of truth until vmlet has run a full VM lifecycle on them.

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