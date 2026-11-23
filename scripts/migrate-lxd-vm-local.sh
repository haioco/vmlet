#!/usr/bin/env bash
set -euo pipefail

# Local same-node LXD VM -> Vmlet MicroVM migration helper
# Scope:
# - Stops source LXD VM first
# - Clones ZFS volume locally (snapshot -> clone -> promote -> destroy snapshot)
# - Removes LXD agent files from cloned rootfs
# - Calls local Vmlet API to create MicroVM from external rootfs
#
# Usage:
#   ./scripts/migrate-lxd-vm-local.sh region-a [target-vm-id] [--execute]
#
# Default mode is DRY-RUN. Add --execute to perform changes.
# Add --skip-backup or --no-backup to skip LXD export

SRC_VM="${1:-}"
TARGET_VM_ID="${2:-}"
EXECUTE_FLAG="${3:-}"
FOURTH_ARG="${4:-}"

if [[ -z "$SRC_VM" ]]; then
  echo "Usage: $0 <lxd-vm-name> [target-vm-id] [--execute] [--skip-backup|--no-backup]"
  exit 1
fi

DRY_RUN=true
SKIP_BACKUP=false

# Parse all arguments for flags
for arg in "$@"; do
  case "$arg" in
    --execute)
      DRY_RUN=false
      ;;
    --skip-backup|--no-backup)
      SKIP_BACKUP=true
      ;;
  esac
done

# Clean TARGET_VM_ID if it's a flag
if [[ "$TARGET_VM_ID" == "--execute" ]] || [[ "$TARGET_VM_ID" == "--skip-backup" ]] || [[ "$TARGET_VM_ID" == "--no-backup" ]]; then
  TARGET_VM_ID=""
fi

API_URL="${MICROVM_API_URL:-http://localhost:8080}"
TARGET_DATASET_BASE="${MICROVM_ZFS_DATASET:-default/vmlet-microvms}"
TARGET_IMAGE_ID="${MIGRATE_IMAGE_ID:-ubuntu-22.04}"
TARGET_NETWORK_MODE="${MIGRATE_NETWORK_MODE:-dhcp}"
TARGET_BRIDGE="${MIGRATE_BRIDGE:-lxdbr0}"
SUDO_BIN="${MIGRATE_SUDO_BIN:-sudo}"

PRESERVE_IP="${MIGRATE_PRESERVE_IP:-true}"
PRESERVE_MAC="${MIGRATE_PRESERVE_MAC:-true}"
OVERRIDE_IP="${MIGRATE_OVERRIDE_IP:-}"
OVERRIDE_MAC="${MIGRATE_OVERRIDE_MAC:-}"
DELETE_SOURCE="${MIGRATE_DELETE_SOURCE:-false}"
OVERWRITE_TARGET="${MIGRATE_OVERWRITE_TARGET:-false}"
EXPORT_BACKUP="${MIGRATE_EXPORT_BACKUP:-true}"
EXPORT_DIR="${MIGRATE_EXPORT_DIR:-/root/vmlet-lxd-exports}"

# Override EXPORT_BACKUP if --skip-backup flag was passed
if [[ "$SKIP_BACKUP" == "true" ]]; then
  EXPORT_BACKUP="false"
fi

log() { echo "[migrate] $*"; }
run_cmd() {
  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] $*"
  else
    eval "$@"
  fi
}

run_root() {
  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] $SUDO_BIN $*"
  else
    "$SUDO_BIN" bash -lc "$*"
  fi
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "Missing command: $1"; exit 1; }
}

trim() { awk '{$1=$1;print}' <<<"$*"; }

to_mb_from_lxd_memory() {
  local raw
  raw="$(trim "$1")"
  if [[ -z "$raw" ]]; then
    echo 1024
    return
  fi

  # Bytes only
  if [[ "$raw" =~ ^[0-9]+$ ]]; then
    echo $(( raw / 1024 / 1024 ))
    return
  fi

  # Common LXD units: MiB, GiB, MB, GB
  if [[ "$raw" =~ ^([0-9]+)([mMgG][iI]?[bB])$ ]]; then
    local num="${BASH_REMATCH[1]}"
    local unit="${BASH_REMATCH[2],,}"
    case "$unit" in
      mib|mb) echo "$num" ;;
      gib|gb) echo $(( num * 1024 )) ;;
      *) echo 1024 ;;
    esac
    return
  fi

  echo 1024
}

pick_root_device() {
  local zvol="$1"
  local part

  # If the zvol has partitions, find the one with LABEL="rootfs" or TYPE="ext4"
  # Skip UEFI/EFI partitions (vfat, ~100MB) that are commonly partition 1 on GPT disks
  
  # First try: partition labeled "rootfs"
  part="$(lsblk -lnpo NAME,LABEL "$zvol" 2>/dev/null | awk '$2=="rootfs"{print $1; exit}')"
  if [[ -n "$part" ]]; then
    echo "$part"
    return
  fi

  # Second try: partition with ext4 filesystem (largest one if multiple)
  part="$(lsblk -lnpo NAME,FSTYPE,SIZE,TYPE "$zvol" 2>/dev/null | awk '$2=="ext4" && $4=="part"{print $1}' | tail -1)"
  if [[ -n "$part" ]]; then
    echo "$part"
    return
  fi

  # Third try: partition with xfs filesystem
  part="$(lsblk -lnpo NAME,FSTYPE,SIZE,TYPE "$zvol" 2>/dev/null | awk '$2=="xfs" && $4=="part"{print $1}' | tail -1)"
  if [[ -n "$part" ]]; then
    echo "$part"
    return
  fi

  # Fallback: if no partitions found, use whole zvol (raw ext4 disk)
  echo "$zvol"
}

ensure_prereqs() {
  require_cmd lxc
  require_cmd zfs
  require_cmd jq
  require_cmd curl
  require_cmd lsblk
  require_cmd mount
  require_cmd umount
}

ensure_vm_exists() {
  lxc info "$SRC_VM" >/dev/null 2>&1 || {
    echo "LXD VM not found: $SRC_VM"
    exit 1
  }
}

get_vm_json() {
  lxc query "/1.0/instances/${SRC_VM}?recursion=1"
}

resolve_source_dataset() {
  if [[ -n "${MIGRATE_SOURCE_DATASET:-}" ]]; then
    echo "$MIGRATE_SOURCE_DATASET"
    return
  fi

  local candidate
  # LXD VMs on ZFS typically store the boot disk in a ".block" zvol dataset.
  # Prefer that to ensure we can boot directly via /dev/zvol/... in Firecracker.
  candidate="$(zfs list -H -o name | grep -E "/virtual-machines/${SRC_VM}\.block$" | head -n 1 || true)"
  if [[ -z "$candidate" ]]; then
    candidate="$(zfs list -H -o name | grep -E "/virtual-machines/${SRC_VM}$" | head -n 1 || true)"
  fi
  if [[ -z "$candidate" ]]; then
    echo ""
    return
  fi
  echo "$candidate"
}

main() {
  ensure_prereqs
  ensure_vm_exists

  log "mode: $([[ "$DRY_RUN" == "true" ]] && echo DRY-RUN || echo EXECUTE)"
  log "source VM: $SRC_VM"

  local vm_json
  vm_json="$(get_vm_json)"

  local status arch cpu_raw mem_raw discovered_mac discovered_ip
  status="$(jq -r '.status // empty' <<<"$vm_json")"
  arch="$(jq -r '.architecture // empty' <<<"$vm_json")"
  cpu_raw="$(jq -r '.config["limits.cpu"] // "1"' <<<"$vm_json")"
  mem_raw="$(jq -r '.config["limits.memory"] // "1024MiB"' <<<"$vm_json")"
  
  # Try multiple MAC address fields (devices.eth0.hwaddr is most reliable)
  discovered_mac="$(jq -r '.devices.eth0.hwaddr // .config["volatile.eth0.hwaddr"] // .config["volatile.enp5s0.hwaddr"] // .expanded_config["volatile.eth0.hwaddr"] // empty' <<<"$vm_json")"
  
  # Try to get IP from runtime state first, then parse static config
  discovered_ip="$(jq -r '.state.network.eth0.addresses[]? | select(.family=="inet") | .address' <<<"$vm_json" | head -n1 || true)"
  if [[ -z "$discovered_ip" ]]; then
    # VM stopped - try to parse static IP from user.network-config (handles escaped \n)
    discovered_ip="$(jq -r '.config["user.network-config"] // empty' <<<"$vm_json" | sed 's/\\n/\n/g' | grep -oP 'addresses:\s*\n\s*-\s*\K[0-9.]+(/[0-9]+)?' | sed 's|/.*||' | head -n1 || true)"
  fi

  local vcpus memory_mb
  vcpus="${cpu_raw%%,*}"
  if [[ ! "$vcpus" =~ ^[0-9]+$ ]]; then
    vcpus=1
  fi
  memory_mb="$(to_mb_from_lxd_memory "$mem_raw")"
  if [[ -z "$memory_mb" || "$memory_mb" -le 0 ]]; then
    memory_mb=1024
  fi

  local source_dataset
  source_dataset="$(resolve_source_dataset)"
  if [[ -z "$source_dataset" ]]; then
    echo "Could not resolve source ZFS dataset for $SRC_VM. Set MIGRATE_SOURCE_DATASET explicitly."
    exit 1
  fi

  if [[ -z "$TARGET_VM_ID" ]]; then
    TARGET_VM_ID="vm-${SRC_VM}"
    TARGET_VM_ID="${TARGET_VM_ID//_/-}"
  fi

  local target_dataset target_zvol snapshot
  target_dataset="${TARGET_DATASET_BASE}/${TARGET_VM_ID}"
  target_zvol="/dev/zvol/${target_dataset}"
  snapshot="${source_dataset}@vmlet-migrate-$(date +%Y%m%d%H%M%S)"

  local ip_to_use mac_to_use
  ip_to_use=""
  mac_to_use=""

  if [[ -n "$OVERRIDE_IP" ]]; then
    ip_to_use="$OVERRIDE_IP"
  elif [[ "$PRESERVE_IP" == "true" ]]; then
    ip_to_use="$discovered_ip"
  fi

  if [[ -n "$OVERRIDE_MAC" ]]; then
    mac_to_use="$OVERRIDE_MAC"
  elif [[ "$PRESERVE_MAC" == "true" ]]; then
    mac_to_use="$discovered_mac"
  fi

  log "LXD status: ${status:-unknown}, arch: ${arch:-unknown}, vcpus: $vcpus, memory_mb: $memory_mb"
  log "source dataset: $source_dataset"
  log "target dataset: $target_dataset"
  log "network preserve ip/mac => ip='${ip_to_use:-}' mac='${mac_to_use:-}'"

  if [[ "$arch" != "x86_64" && -n "$arch" ]]; then
    echo "Unsupported architecture for Firecracker migration: $arch"
    exit 1
  fi

  # 1) Stop source VM first
  if [[ "$status" == "Running" || "$status" == "running" ]]; then
    run_cmd "lxc stop '$SRC_VM' --force"
  else
    log "source VM already stopped"
  fi

  # 1.5) Create a full LXD backup export before migration continues
  if [[ "$EXPORT_BACKUP" == "true" ]]; then
    local export_file
    export_file="${EXPORT_DIR}/${SRC_VM}-$(date +%Y%m%d%H%M%S).tar.gz"
    log "creating LXD export backup: $export_file"
    run_cmd "mkdir -p '$EXPORT_DIR'"
    run_cmd "lxc export '$SRC_VM' '$export_file'"
  else
    log "skipping LXD export backup (MIGRATE_EXPORT_BACKUP=false)"
  fi

  # 2) ZFS clone flow
  if [[ "$DRY_RUN" == "false" ]]; then
    if "$SUDO_BIN" zfs list "$target_dataset" >/dev/null 2>&1; then
      if [[ "$OVERWRITE_TARGET" == "true" ]]; then
        log "target dataset already exists, deleting due to MIGRATE_OVERWRITE_TARGET=true"
        
        # Check if zvol device exists and is in use
        local target_zvol_dev="/dev/zvol/${target_dataset}"
        if [[ -e "$target_zvol_dev" ]]; then
          log "checking if zvol device is busy: $target_zvol_dev"
          
          # Check for mounted filesystems
          local mount_points
          mount_points=$("$SUDO_BIN" mount | grep "$target_zvol_dev" | awk '{print $3}' || true)
          if [[ -n "$mount_points" ]]; then
            log "unmounting busy mount points..."
            while IFS= read -r mp; do
              [[ -z "$mp" ]] && continue
              "$SUDO_BIN" umount "$mp" 2>/dev/null || "$SUDO_BIN" umount -l "$mp" 2>/dev/null || true
            done <<< "$mount_points"
          fi
          
          # Check for partition devices (zd*p1, zd*p2, etc.)
          local partition_devs
          partition_devs=$(find "${target_zvol_dev}"* -type b 2>/dev/null || true)
          if [[ -n "$partition_devs" ]]; then
            log "checking partition devices for mounts..."
            while IFS= read -r partdev; do
              [[ -z "$partdev" ]] && continue
              local part_mounts
              part_mounts=$("$SUDO_BIN" mount | grep "$partdev" | awk '{print $3}' || true)
              if [[ -n "$part_mounts" ]]; then
                while IFS= read -r pm; do
                  [[ -z "$pm" ]] && continue
                  "$SUDO_BIN" umount "$pm" 2>/dev/null || "$SUDO_BIN" umount -l "$pm" 2>/dev/null || true
                done <<< "$part_mounts"
              fi
            done <<< "$partition_devs"
          fi
          
          # Give the system a moment to release the device
          sleep 1
        fi
        
        # Release any ZFS holds that block destruction (left by interrupted zfs recv etc.)
        local holds_output
        holds_output=$("$SUDO_BIN" zfs holds -r "$target_dataset" 2>/dev/null || true)
        if [[ -n "$holds_output" ]] && echo "$holds_output" | grep -qv "^NAME"; then
          log "releasing ZFS holds on target dataset..."
          echo "$holds_output" | tail -n +2 | while IFS=$'\t' read -r ds snap tag _rest; do
            [[ -z "$tag" ]] && continue
            "$SUDO_BIN" zfs release "$tag" "${ds}@${snap}" 2>/dev/null || \
              "$SUDO_BIN" zfs release -r "$tag" "$target_dataset" 2>/dev/null || true
          done
        fi

        # Destroy all child snapshots first (most common cause of "busy")
        local child_snaps
        child_snaps=$("$SUDO_BIN" zfs list -H -o name -t snapshot -r "$target_dataset" 2>/dev/null || true)
        if [[ -n "$child_snaps" ]]; then
          log "destroying child snapshots first..."
          while IFS= read -r snap_name; do
            [[ -z "$snap_name" ]] && continue
            "$SUDO_BIN" zfs destroy "$snap_name" 2>/dev/null || true
          done <<< "$child_snaps"
        fi

        # Destroy dependent clones (datasets that depend on this one)
        local dependent_clones
        dependent_clones=$("$SUDO_BIN" zfs list -H -o name -t all -r "$target_dataset" 2>/dev/null | grep -v "^${target_dataset}$" || true)
        if [[ -n "$dependent_clones" ]]; then
          log "destroying dependent clones..."
          while IFS= read -r clone; do
            [[ -z "$clone" ]] && continue
            "$SUDO_BIN" zfs destroy -r "$clone" 2>/dev/null || true
          done <<< "$dependent_clones"
        fi
        
        # Now destroy the main dataset (try escalating flags)
        "$SUDO_BIN" zfs destroy -r "$target_dataset" 2>/dev/null || \
        "$SUDO_BIN" zfs destroy -R "$target_dataset" 2>/dev/null || \
        "$SUDO_BIN" zfs destroy -Rf "$target_dataset" 2>/dev/null || {
          # Last resort: rename out of the way so migration can proceed
          local ts_suffix
          ts_suffix=$(date +%s)
          log "WARNING: cannot destroy dataset, renaming to ${target_dataset}-orphan-${ts_suffix}"
          "$SUDO_BIN" zfs rename "$target_dataset" "${target_dataset}-orphan-${ts_suffix}" 2>/dev/null || {
            log "ERROR: dataset is still busy, trying to identify what's using it..."
            "$SUDO_BIN" lsof "$target_zvol_dev"* 2>/dev/null || true
            "$SUDO_BIN" fuser -v "$target_zvol_dev"* 2>/dev/null || true
            echo "Failed to destroy or rename busy dataset: $target_dataset"
            echo "Try: sudo zfs destroy -Rf $target_dataset  or  reboot the node"
            exit 1
          }
        }
      else
        echo "Target dataset already exists: $target_dataset"
        echo "Set MIGRATE_OVERWRITE_TARGET=true to replace it."
        exit 1
      fi
    fi
  fi

  # Use zfs send/recv to create a FULLY INDEPENDENT copy of the source dataset.
  # IMPORTANT: We previously used snapshot->clone->promote, but "zfs promote"
  # reverses the parent-child relationship, making the SOURCE depend on the
  # TARGET. If the target is later destroyed, the source LXD VM is corrupted
  # (its .block zvol becomes invalid). zfs send/recv avoids this entirely.
  run_root "zfs snapshot '$snapshot'"

  if [[ "$DRY_RUN" == "false" ]]; then
    log "copying dataset via zfs send/recv (independent copy, safe for source)..."
    "$SUDO_BIN" bash -c "zfs send '$snapshot' | zfs recv -F '$target_dataset'" || {
      echo "Failed to zfs send/recv from $snapshot to $target_dataset"
      "$SUDO_BIN" zfs destroy "$snapshot" 2>/dev/null || true
      exit 1
    }
  else
    echo "[dry-run] $SUDO_BIN zfs send '$snapshot' | zfs recv -F '$target_dataset'"
  fi

  # Clean up the source snapshot (safe - target is fully independent now)
  if [[ "$DRY_RUN" == "false" ]]; then
    "$SUDO_BIN" zfs destroy "$snapshot" 2>/dev/null || true
    # Also clean up the snapshot copy that zfs recv creates on the target
    local target_snap_name="${snapshot##*@}"
    "$SUDO_BIN" zfs destroy "${target_dataset}@${target_snap_name}" 2>/dev/null || true
  else
    echo "[dry-run] $SUDO_BIN zfs destroy '$snapshot' || true"
  fi

  if [[ "$DRY_RUN" == "false" && ! -e "$target_zvol" ]]; then
    # zvol device nodes can take a moment to appear after zfs recv
    log "waiting for zvol device to appear: $target_zvol"
    for _i in $(seq 1 10); do
      [[ -e "$target_zvol" ]] && break
      sleep 1
    done
    if [[ ! -e "$target_zvol" ]]; then
      echo "Cloned zvol device not found: $target_zvol"
      exit 1
    fi
  fi

  # Ensure kernel has scanned partition table so /dev/zd*p1 etc. appear
  if [[ "$DRY_RUN" == "false" ]]; then
    "$SUDO_BIN" udevadm settle --timeout=10 2>/dev/null || true
    "$SUDO_BIN" partprobe "$target_zvol" 2>/dev/null || true
    sleep 1
  fi

  # 3) Guest prep: remove LXD agent files from cloned rootfs
  local mount_dir root_device rootfs_for_firecracker boot_root_device
  mount_dir="/tmp/vmlet-migrate-${TARGET_VM_ID}-$$"
  root_device="$(pick_root_device "$target_zvol")"
  rootfs_for_firecracker="$root_device"

  # When Firecracker gets a single partition (e.g. /dev/zd16p2) as path_on_host,
  # it presents it as /dev/vda to the guest. So root=/dev/vda is correct.
  # When Firecracker gets the WHOLE disk (e.g. /dev/zd16), the guest sees the
  # full GPT table, so root=/dev/vda2 would be needed.
  if [[ "$root_device" != "$target_zvol" ]]; then
    # Passing a partition -> Firecracker sees it as whole /dev/vda
    boot_root_device="/dev/vda"
    log "using partition $root_device as rootfs (Firecracker sees it as /dev/vda)"
  else
    # Passing whole disk -> guest needs to find the right partition
    boot_root_device="/dev/vda"
    log "using whole disk $root_device as rootfs"
  fi

  run_cmd "mkdir -p '$mount_dir'"
  run_root "mount '$root_device' '$mount_dir'"

  # Full LXD agent deletion (best-effort)
  run_root "rm -f '$mount_dir/usr/bin/lxd-agent' '$mount_dir/usr/sbin/lxd-agent'"
  run_root "rm -f '$mount_dir/etc/systemd/system/lxd-agent.service' '$mount_dir/etc/systemd/system/lxd-agent-9p.service' '$mount_dir/etc/systemd/system/lxd-agent-virtiofs.service'"
  run_root "rm -f '$mount_dir/lib/systemd/system/lxd-agent.service' '$mount_dir/lib/systemd/system/lxd-agent-9p.service' '$mount_dir/lib/systemd/system/lxd-agent-virtiofs.service'"
  run_root "rm -f '$mount_dir/etc/systemd/system/multi-user.target.wants/lxd-agent.service' '$mount_dir/etc/systemd/system/multi-user.target.wants/lxd-agent-9p.service' '$mount_dir/etc/systemd/system/multi-user.target.wants/lxd-agent-virtiofs.service'"
  run_root "rm -f '$mount_dir/lib/systemd/system-generators/lxd-agent-9p-mount'"
  run_root "rm -f '$mount_dir/lib/udev/rules.d/99-lxd-agent.rules' '$mount_dir/etc/udev/rules.d/99-lxd-agent.rules'"
  run_root "rm -rf '$mount_dir/run/lxd_config' '$mount_dir/run/lxd_agent' '$mount_dir/var/lib/lxd-agent' '$mount_dir/dev/.lxd-mounts'"
  run_root "rm -f '$mount_dir/var/log/lxd-agent.log'"

  # Remove LXD mounts from fstab
  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] sed -i '/lxd_config\\|lxd_agent\\|virtiofs\\| 9p /d' '$mount_dir/etc/fstab'"
  else
    if [[ -f "$mount_dir/etc/fstab" ]]; then
      "$SUDO_BIN" sed -i '/lxd_config\|lxd_agent\|virtiofs\| 9p /d' "$mount_dir/etc/fstab"

      # If rootfs is a partition clone (common for LXD GPT disks), disable
      # BOOT/UEFI extra mounts that no longer exist in Firecracker.
      if [[ "$root_device" != "$target_zvol" ]]; then
        "$SUDO_BIN" sed -i 's|^LABEL=BOOT|# LABEL=BOOT|' "$mount_dir/etc/fstab"
        "$SUDO_BIN" sed -i 's|^LABEL=UEFI|# LABEL=UEFI|' "$mount_dir/etc/fstab"
      fi
    fi
  fi

  # Ensure migrated guest uses Firecracker NIC naming and not LXD datasource.
  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] patch netplan interface names to eth0 when needed"
    echo "[dry-run] write cloud-init datasource override to NoCloud"
  else
    if [[ -f "$mount_dir/etc/netplan/50-cloud-init.yaml" ]]; then
      "$SUDO_BIN" sed -i 's/^[[:space:]]*enp[0-9][a-z0-9]*:/    eth0:/' "$mount_dir/etc/netplan/50-cloud-init.yaml"
    fi

    # Create cloud-init config directory if it doesn't exist
    "$SUDO_BIN" mkdir -p "$mount_dir/etc/cloud/cloud.cfg.d"
    
    "$SUDO_BIN" tee "$mount_dir/etc/cloud/cloud.cfg.d/99-disable-lxd.cfg" >/dev/null <<'EOF'
datasource_list: [NoCloud, None]
datasource:
  NoCloud:
    seedfrom: /var/lib/cloud/seed/nocloud/
EOF

    "$SUDO_BIN" rm -rf "$mount_dir/var/lib/cloud/instances"/* 2>/dev/null || true
  fi

  run_root "umount '$mount_dir'"
  run_cmd "rmdir '$mount_dir'"

  # 4) Create MicroVM from cloned zvol using external_rootfs_path
  local storage_mb volsize_bytes
  if [[ "$DRY_RUN" == "true" ]]; then
    storage_mb=10240
  else
    volsize_bytes="$("$SUDO_BIN" zfs get -Hp -o value volsize "$target_dataset")"
    storage_mb=$(( volsize_bytes / 1024 / 1024 ))
    if [[ "$storage_mb" -le 0 ]]; then
      storage_mb=10240
    fi
  fi

  local payload
  payload="$(jq -n \
    --arg name "$TARGET_VM_ID" \
    --arg image "$TARGET_IMAGE_ID" \
    --arg mode "$TARGET_NETWORK_MODE" \
    --arg bridge "$TARGET_BRIDGE" \
    --arg ip "$ip_to_use" \
    --arg mac "$mac_to_use" \
    --arg rootfs "$rootfs_for_firecracker" \
    --arg source "$SRC_VM" \
    --arg boot_root "$boot_root_device" \
    --argjson vcpus "$vcpus" \
    --argjson memory_mb "$memory_mb" \
    --argjson storage_mb "$storage_mb" \
    '{
      name: $name,
      vcpus: $vcpus,
      memory_mb: $memory_mb,
      storage_mb: $storage_mb,
      image_id: $image,
      network: {
        assignment_method: $mode,
        bridge_name: $bridge,
        ip_address: (if $ip == "" then null else $ip end),
        mac_address: (if $mac == "" then null else $mac end)
      },
      metadata: {
        external_rootfs_path: $rootfs,
        boot_root_device: $boot_root,
        skip_image_prepare: "true",
        migrated_from_lxd: $source,
        migrated_at: now|tostring,
        cloudinit_force_iso: "false"
      }
    }')"

  if [[ "$DRY_RUN" == "true" ]]; then
    log "would POST to ${API_URL}/vms with payload:"
    echo "$payload" | jq .
  else
    # If a VM with the same ID already exists (or stale process is still alive),
    # force-remove it first when overwrite is enabled. Otherwise, create can race
    # with old runtime state and report "running" while guest is unhealthy.
    local existing_vm_status
    existing_vm_status="$(curl -s "${API_URL}/vms/${TARGET_VM_ID}" | jq -r '.status // empty' 2>/dev/null || true)"
    if [[ -n "$existing_vm_status" ]]; then
      if [[ "$OVERWRITE_TARGET" != "true" ]]; then
        echo "Target VM already exists in API: ${TARGET_VM_ID} (status=${existing_vm_status})"
        echo "Set MIGRATE_OVERWRITE_TARGET=true to replace it."
        exit 1
      fi

      log "existing target VM found in API (status=${existing_vm_status}), deleting before recreate"
      curl -sS -X DELETE "${API_URL}/vms/${TARGET_VM_ID}" >/dev/null || true

      # Wait briefly for cleanup to complete (systemd unit + API state)
      local wait_i
      for wait_i in {1..20}; do
        local still_exists
        still_exists="$(curl -s "${API_URL}/vms/${TARGET_VM_ID}" | jq -r '.id // empty' 2>/dev/null || true)"
        if [[ -z "$still_exists" ]]; then
          break
        fi
        sleep 1
      done

      # Best-effort kill stale firecracker process if still lingering
      if pgrep -f "firecracker.*${TARGET_VM_ID}" >/dev/null 2>&1; then
        log "stale firecracker process still present, terminating it"
        pkill -f "firecracker.*${TARGET_VM_ID}" || true
        sleep 1
      fi
    fi

    log "creating microvm via API ${API_URL}/vms"
    curl -sS -X POST "${API_URL}/vms" \
      -H 'Content-Type: application/json' \
      -d "$payload" | jq .
  fi

  # 5) Optionally delete source LXD VM
  if [[ "$DELETE_SOURCE" == "true" ]]; then
    run_cmd "lxc delete '$SRC_VM' --force"
  fi

  log "done"
}

main "$@"
