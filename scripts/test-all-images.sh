#!/usr/bin/env bash
set -euo pipefail

# Change to the microvm-service directory
cd "$(dirname "$0")/.." || exit 1

API=${API:-http://localhost:8080}
USERNAME='demo'
PASSWORD='vmlet-demo-pw'
IMAGES=(ubuntu-24.04 ubuntu-22.04 debian-12 alpine-3.18)
TIMEOUT=${TIMEOUT:-30}
WORK=tmp-image-tests
mkdir -p "$WORK"

printf "%-16s %-8s %-8s %-14s %-8s\n" IMAGE BOOT_S SSH_S CLOUDINIT LOGIN
printf "%s\n" "------------------------------------------------------------"

json_field() { jq -r "$1" <<<"$2" 2>/dev/null || echo ""; }

for img in "${IMAGES[@]}"; do
  REQ_START=$(date +%s)
  RESP=$(curl -s -X POST "$API/vms" -H 'Content-Type: application/json' \
    -d '{"vcpus":1,"memory_mb":512,"storage_mb":1024,"image_id":"'"$img"'","cloudinit":{"hostname":"test-'"$img"'","username":"'"$USERNAME"'","password":"'"$PASSWORD"'"}}') || { echo "$img: create failed" >&2; continue; }
  VM_ID=$(json_field .id "$RESP")
  if [[ -z "$VM_ID" || "$VM_ID" == "null" ]]; then echo "$img: bad vm id"; continue; fi
  VM_DIR="firecracker-work/microvms/$VM_ID"
  LOG="$VM_DIR/firecracker.log"
  
  # Wait for boot (Linux version line in log)
  BOOT_TIME="timeout"
  for i in {1..60}; do
    if [[ -f "$LOG" ]] && grep -q 'Linux version' "$LOG" 2>/dev/null; then
      BOOT_ELAPSED=$(($(date +%s) - REQ_START))
      BOOT_TIME="${BOOT_ELAPSED}s"
      break
    fi
    sleep 0.5
  done
  
  # Get IP
  sleep 1
  VM_JSON=$(curl -s "$API/vms/$VM_ID" 2>/dev/null || echo "{}")
  IP=$(json_field .network.ip_address "$VM_JSON")
  [[ -z "$IP" || "$IP" == "null" ]] && IP="none"
  
  # SSH probe
  SSH_TIME="timeout"
  if [[ "$IP" != "none" ]]; then
    START_SSH=$(date +%s)
    for i in $(seq 1 $TIMEOUT); do
      if nc -z -w1 "$IP" 22 2>/dev/null; then
        SSH_ELAPSED=$(($(date +%s) - START_SSH))
        SSH_TIME="${SSH_ELAPSED}s"
        break
      fi
      sleep 1
    done
  fi
  
  # Check cloud-init markers
  CLOUDINIT_OK="no"
  if [[ -f "$LOG" ]]; then
    if grep -Eqi 'Cloud-init.*running.*init|Cloud-init v\. .* running' "$LOG"; then
      CLOUDINIT_OK="yes"
    fi
  fi
  
  # Attempt password SSH login
  LOGIN_OK="no"
  if [[ "$IP" != "none" && "$SSH_TIME" != "timeout" ]]; then
    if command -v sshpass >/dev/null 2>&1; then
      if sshpass -p "$PASSWORD" ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
         -o ConnectTimeout=5 -o BatchMode=no "$USERNAME@$IP" 'echo ok' 2>/dev/null | grep -q ok; then
        LOGIN_OK="yes"
      fi
    fi
  fi
  
  printf "%-16s %-8s %-8s %-14s %-8s\n" "$img" "$BOOT_TIME" "$SSH_TIME" "$CLOUDINIT_OK" "$LOGIN_OK"
done | tee "$WORK/results.txt"

echo
echo "Results saved to $WORK/results.txt"
