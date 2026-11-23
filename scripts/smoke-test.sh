#!/usr/bin/env bash
set -euo pipefail

API_URL=${API_URL:-http://localhost:8080}
IMAGE_ID=${IMAGE_ID:-ubuntu-22.04}
USERNAME=${USERNAME:-demo}
PASSWORD=${PASSWORD:-DemoPass123!}
TIMEOUT=${TIMEOUT:-30}

echo "[smoke] Checking health kernel endpoint..."
curl -fsS "$API_URL/health/kernel" | jq . || { echo "[smoke] health endpoint failed"; exit 1; }
KPATH=$(curl -fsS "$API_URL/health/kernel" | jq -r .kernel_path)
[[ -n "$KPATH" ]] || { echo "[smoke] empty kernel_path"; exit 1; }
echo "[smoke] Kernel path: $KPATH"

echo "[smoke] Creating test VM..."
VM_ID=$(curl -fsS -X POST "$API_URL/vms" -H 'Content-Type: application/json' \
  -d '{"vcpus":1,"memory_mb":512,"storage_mb":1024,"image_id":"'$IMAGE_ID'","cloudinit":{"hostname":"smoketest","username":"'$USERNAME'","password":"'$PASSWORD'"}}' | jq -r .id)
[[ "$VM_ID" == vm-* ]] || { echo "[smoke] invalid VM id: $VM_ID"; exit 1; }
echo "[smoke] VM: $VM_ID"

CFG="microvm-service/firecracker-work/microvms/$VM_ID/config.json"
for i in {1..20}; do [[ -f "$CFG" ]] && break; sleep 0.5; done
[[ -f "$CFG" ]] || { echo "[smoke] config.json not found"; exit 1; }

echo "[smoke] Validating kernel path in config.json..."
CFG_KERNEL=$(jq -r '."boot-source".kernel_image_path' "$CFG")
[[ "$CFG_KERNEL" == "$KPATH" ]] || { echo "[smoke] kernel path mismatch: $CFG_KERNEL vs $KPATH"; exit 1; }

echo "[smoke] Checking boot args include nomodules..."
BOOT_ARGS=$(jq -r '."boot-source".boot_args' "$CFG")
grep -q 'nomodules' <<<"$BOOT_ARGS" || { echo "[smoke] nomodules missing in boot args"; exit 1; }

echo "[smoke] Waiting for firecracker log..."
LOG="microvm-service/firecracker-work/microvms/$VM_ID/firecracker.log"
for i in {1..40}; do [[ -f "$LOG" ]] && grep -q 'Linux version' "$LOG" && break; sleep 0.5; done
grep -q 'Linux version' "$LOG" || { echo "[smoke] kernel boot line not found"; exit 1; }

echo "[smoke] Extracting VM IP via API..."
VM_IP=$(curl -fsS "$API_URL/vms/$VM_ID" | jq -r .network.ip_address)
[[ "$VM_IP" =~ ^192\.168\.100\.[0-9]+$ ]] || { echo "[smoke] unexpected IP: $VM_IP"; exit 1; }
echo "[smoke] VM IP: $VM_IP"

echo "[smoke] Probing SSH port 22 (timeout ${TIMEOUT}s)..."
SECONDS=0
REACH=0
while [[ $SECONDS -lt $TIMEOUT ]]; do
  if nc -z -w1 "$VM_IP" 22 2>/dev/null; then REACH=1; break; fi
  sleep 1
done
if [[ $REACH -ne 1 ]]; then echo "[smoke] SSH not reachable within ${TIMEOUT}s"; exit 1; fi
echo "[smoke] SSH reachable in ${SECONDS}s"
if [[ $SECONDS -gt 5 ]]; then echo "[smoke] WARNING: SSH took >5s"; fi

echo "[smoke] Checking log for apt noise (should be empty)..."
if grep -i -E 'apt-get|apt ' "$LOG" | head -n1 | grep -vq '^$'; then
  echo "[smoke] Unexpected apt activity detected"; exit 1
fi

echo "[smoke] SUCCESS"