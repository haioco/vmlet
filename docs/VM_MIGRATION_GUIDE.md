# VM Migration Guide (ZFS, Node-to-Node)

This guide covers migrating a Firecracker microVM between vmlet nodes using ZFS send/receive.

## Architecture Notes

- Both nodes use ZFS with pool `default` and dataset `default/vmlet-microvms`
- Each VM disk is a ZFS zvol at `/dev/zvol/default/vmlet-microvms/<vm-id>`
- vmlet `CreateDisk()` skips creation if the zvol already exists — this is the key to zero-overwirte migration
- Network (MAC + IP) is re-assigned on destination from the node's `dhcpd.conf`

## Policy

> **Source VM MUST remain stopped for the entire migration.**
> Do NOT restart the source VM after taking the snapshot.
> The source stays stopped until the destination VM is confirmed running.
> Only restart source as a rollback if destination fails.

---

## Step-by-Step Migration

### Step 0 — Inventory

```bash
VM_ID="vm-xxxxxxxx"
SRC="node2.example.com:9443"
SRC_PASS="CHANGE_ME_MIGRATION_TOKEN"
SRC_IP="45.252.182.18"

DST="node1.example.com:9443"
DST_PASS="CHANGE_ME_MIGRATION_TOKEN"
DST_IP="45.252.182.66"

# Get VM details from source
curl -k -s -u "vmlet:$SRC_PASS" https://$SRC/vms/$VM_ID | jq \
  '{vcpus: .resources.vcpus, memory_mb: .resources.memory_mb,
    storage_mb: .resources.storage_mb, image_id: .image_name,
    hostname: .metadata.cloudinit_hostname,
    password: .metadata.cloudinit_password,
    bandwidth_in: .network.bandwidth_ingress,
    bandwidth_out: .network.bandwidth_egress}'
```

### Step 1 — Pick Free IP/MAC on Destination

```bash
# List all IPs already in use on destination
curl -k -s -u "vmlet:$DST_PASS" https://$DST/vms \
  | jq -r '.[].network.ip_address' | sort -t. -k4 -n

# Browse dhcpd.conf on destination — find a host entry whose IP is NOT in the list above
ssh root@$DST_IP 'grep -A2 "host vm" /etc/dhcp/dhcpd.conf | grep -E "hardware|fixed"'

# Pick: NEW_MAC and NEW_IP from a free slot
NEW_MAC="xx:xx:xx:xx:xx:xx"
NEW_IP="45.252.182.xxx"
```

### Step 2 — Stop Source VM (Downtime Starts)

```bash
curl -k -s -u "vmlet:$SRC_PASS" -X POST https://$SRC/vms/$VM_ID/stop

# Wait for stopped
until curl -k -s -u "vmlet:$SRC_PASS" https://$SRC/vms/$VM_ID | grep -q '"stopped"'; do
  echo "Waiting for stop..."; sleep 2
done
echo "VM stopped."
```

### Step 3 — ZFS Snapshot on Source

```bash
ssh root@$SRC_IP "zfs snapshot default/vmlet-microvms/$VM_ID@migrate && echo OK"
```

### Step 4 — Transfer ZFS Snapshot Source → Destination (Direct)

```bash
# Direct node-to-node transfer (faster; avoids routing through local machine)
ssh root@$SRC_IP \
  "zfs send -c default/vmlet-microvms/$VM_ID@migrate | \
   ssh -o StrictHostKeyChecking=no root@$DST_IP \
   'zfs receive default/vmlet-microvms/$VM_ID'" \
  && echo "TRANSFER COMPLETE"
```

### Step 5 — Verify Zvol on Destination

```bash
ssh root@$DST_IP "zfs list default/vmlet-microvms/$VM_ID && \
  ls -la /dev/zvol/default/vmlet-microvms/$VM_ID && echo ZVOL_OK"
```

If this fails, **do not proceed** — check `zfs receive` errors and re-run Step 4.

### Step 6 — Register VM on Destination

> `storage_mb` must be the GB value of the zvol (e.g., 25600 MB disk = pass `25`).
> vmlet detects the existing zvol and skips recreation.

```bash
# storage_gb = original storage_mb / 1024 (round down)
# Example: 25600 MB → 25, 15360 MB → 15, 51200 MB → 50

curl -k -s -u "vmlet:$DST_PASS" \
  -X POST https://$DST/vms \
  -H 'Content-Type: application/json' \
  -d "{
    \"id\": \"$VM_ID\",
    \"name\": \"$VM_ID\",
    \"vcpus\": <vcpus>,
    \"memory_mb\": <memory_mb>,
    \"storage_mb\": <storage_GB>,
    \"image_id\": \"<image_id>\",
    \"network\": {
      \"assignment_method\": \"dhcp\",
      \"mac_address\": \"$NEW_MAC\",
      \"ip_address\": \"$NEW_IP\",
      \"bandwidth_ingress\": \"<ingress>\",
      \"bandwidth_egress\": \"<egress>\"
    },
    \"cloudinit\": {
      \"hostname\": \"<hostname>\",
      \"username\": \"root\",
      \"password\": \"<password>\"
    },
    \"auto_start\": true
  }"
```

### Step 7 — Verify Destination VM Running

```bash
sleep 20

# Check status
curl -k -s -u "vmlet:$DST_PASS" https://$DST/vms/$VM_ID \
  | jq '{status, ip: .network.ip_address}'

# Test network from destination node
ssh root@$DST_IP "ping -c3 $NEW_IP && nc -w3 -zv $NEW_IP 22 && echo SSH_OK"
```

### Step 8 — Cleanup Migration Snapshot on Source

```bash
ssh root@$SRC_IP "zfs destroy default/vmlet-microvms/$VM_ID@migrate && echo SNAPSHOT_CLEANED"
```

### Step 9 — Update Billing Panel

1. Change VM's node from source → destination node
2. Update IP from old IP → `$NEW_IP`
3. Run smoke tests from billing panel
4. **Source VM stays stopped** until billing confirms destination is stable
5. When confirmed — delete source VM:
```bash
curl -k -s -u "vmlet:$SRC_PASS" -X DELETE https://$SRC/vms/$VM_ID
# Then optionally clean up source zvol (vmlet DeleteVM does this automatically)
```

---

## Rollback

If destination fails at **any step** after Step 2:

```bash
# 1. Start source VM back — customer is live again
curl -k -s -u "vmlet:$SRC_PASS" -X POST https://$SRC/vms/$VM_ID/start

# 2. If VM was registered on destination, remove it
curl -k -s -u "vmlet:$DST_PASS" -X DELETE https://$DST/vms/$VM_ID

# 3. Destroy destination zvol if present
ssh root@$DST_IP "zfs destroy -r default/vmlet-microvms/$VM_ID"

# 4. Clean migration snapshot on source
ssh root@$SRC_IP "zfs destroy default/vmlet-microvms/$VM_ID@migrate 2>/dev/null"
```

---

## Real Migration Example (Feb 27, 2026)

**VM:** `vm-a2f17485` — ubuntu-24.04, 2 vCPU / 2048 MB / 25 GB, hostname `hf-nl-ams-2-flash-core-01`

| | Source (node47) | Destination (node46) |
|---|---|---|
| API | node2.example.com:9443 | node1.example.com:9443 |
| IP | 45.252.182.219 | 45.252.182.67 |
| MAC | 00:0c:6a:1c:65:29 | 00:59:63:c9:18:f9 |
| ZFS dataset used | 1.29 GB (compressed) | same |
| Transfer time | ~15 seconds node-to-node (same DC) | |
| Total downtime | ~30 seconds | |

**What happened:**
1. Source VM stopped
2. `zfs snapshot default/vmlet-microvms/vm-a2f17485@migrate`
3. `zfs send -c ... | ssh root@45.252.182.66 'zfs receive ...'` — 1.29 GB in ~15s same-DC
4. `POST /vms` on node46 — vmlet detected existing zvol, skipped creation, started VM
5. VM running on node46 at `45.252.182.67` with customer data intact
6. Snapshot destroyed on node47
7. Source VM left stopped (not deleted)

---

## Important Notes

- **`storage_mb` is in GB** when passed to `POST /vms` (vmlet quirk). `25600 MB → 25`.
- If wrong value used, the metadata shows wrong disk size but actual data is fine — update billing panel.
- `zfs send -c` uses compressed transfer — actual bytes on wire is much less than zvol size.
- **Direct node-to-node send** is always faster than routing through developer machine.
- After migration, old SSH known_hosts entry for the new IP should be cleared: `ssh-keygen -R <new-ip>`.
- vmlet `auto_start: true` causes VM to auto-restart after service restart — set `false` if you want manual control during testing.
