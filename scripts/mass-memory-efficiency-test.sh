#!/usr/bin/env bash
set -euo pipefail

# mass-memory-efficiency-test.sh
# Create N alpine microVMs with small memory, wait until running, then gather stats and compute efficiency.

API_BASE="http://localhost:8080"
COUNT="${COUNT:-50}"          # number of VMs
MEM_MB="${MEM_MB:-128}"        # configured memory per VM
IMAGE_ID="${IMAGE_ID:-alpine-3.18}" # image to use
PARALLEL="${PARALLEL:-10}"     # parallel creation batch size
REPORT="${REPORT:-mass-efficiency-report.json}"

echo "[INFO] Mass memory efficiency test starting: COUNT=$COUNT MEM_MB=$MEM_MB IMAGE=$IMAGE_ID"

fail() { echo "[ERROR] $*" >&2; exit 1; }

check_deps() {
  command -v curl >/dev/null || fail "curl not available"
  command -v jq >/dev/null || fail "jq not available"
}
check_deps

json_valid() {
  jq -e . >/dev/null 2>&1
}

fetch_stats() {
  local vm="$1"; local attempts=0; local max_attempts=5
  local out status
  while (( attempts < max_attempts )); do
    out=$(curl -s "$API_BASE/vms/$vm/stats") || true
    if [[ -n "$out" ]] && echo "$out" | json_valid; then
      echo "$out"
      return 0
    fi
    attempts=$((attempts+1))
    sleep 1
  done
  echo "" # indicate failure
  return 1
}

create_vm() {
  local name="$1"
  curl -s -X POST "$API_BASE/vms" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"vcpus\":1,\"memory_mb\":$MEM_MB,\"storage_mb\":256,\"image_id\":\"$IMAGE_ID\"}" | jq -r '.id' || return 1
}

echo "[INFO] Creating $COUNT VMs (parallel=$PARALLEL)"
created=()
batch=()
for i in $(seq 1 $COUNT); do
  name="eff-vm-$i"
  batch+=("$name")
  if (( ${#batch[@]} >= PARALLEL )); then
    for n in "${batch[@]}"; do create_vm "$n" & done
    wait
    created+=("${batch[@]}")
    batch=()
  fi
done
if (( ${#batch[@]} > 0 )); then
  for n in "${batch[@]}"; do create_vm "$n" & done
  wait
  created+=("${batch[@]}")
fi

echo "[INFO] Created ${#created[@]} VMs"

echo "[INFO] Waiting for all VMs to reach running state (auto-recover)"
deadline=$(( $(date +%s) + 300 )) # 5 min timeout
remaining=("${created[@]}")
recover_attempts=0
while (( ${#remaining[@]} > 0 )); do
  now=$(date +%s)
  (( now > deadline )) && { echo "[WARN] Timeout reached, proceeding with partial set"; break; }
  next=()
  for vm in "${remaining[@]}"; do
    status=$(curl -s "$API_BASE/vms/$vm" | jq -r '.status // empty')
    if [[ "$status" == "running" ]]; then
      continue
    fi
    # Attempt recovery/start if stuck in creating or stopped
    if [[ "$status" == "creating" || "$status" == "stopped" ]]; then
      curl -s -X POST "$API_BASE/vms/$vm/start" >/dev/null || true
    fi
    next+=("$vm")
  done
  remaining=("${next[@]}")
  echo "[INFO] Waiting: ${#remaining[@]} not running yet" >&2
  (( ${#remaining[@]} > 0 )) && { sleep 2; recover_attempts=$((recover_attempts+1)); }
done
running_count=$(curl -s "$API_BASE/vms" | jq '[.[] | select(.status=="running")] | length')
echo "[INFO] Running VMs: $running_count / ${#created[@]}"
if (( running_count == 0 )); then fail "No VMs running after recovery attempts"; fi

echo "[INFO] Collecting stats"
total_config_mb=0
total_rss_mb=0
balloon_enabled_count=0
declare -A vm_rss
declare -A vm_balloon

for vm in "${created[@]}"; do
  status=$(curl -s "$API_BASE/vms/$vm" | jq -r '.status // empty')
  [[ "$status" != "running" ]] && { echo "[WARN] Skipping $vm (status=$status)"; continue; }
  stats=$(fetch_stats "$vm")
  if [[ -z "$stats" ]]; then
    echo "[WARN] Skipping $vm (invalid stats)"
    continue
  fi
  cfg_mem=$(echo "$stats" | jq -r '.configured.memory_mb')
  rss=$(echo "$stats" | jq -r '.stats.memory_rss_mb')
  balloon_enabled=$(echo "$stats" | jq -r '.stats.balloon_enabled')
  total_config_mb=$(( total_config_mb + cfg_mem ))
  # Use awk for float addition
  total_rss_mb=$(awk -v a="$total_rss_mb" -v b="$rss" 'BEGIN{printf "%.2f", a+b}')
  [[ "$balloon_enabled" == "true" ]] && balloon_enabled_count=$((balloon_enabled_count + 1))
  vm_rss[$vm]="$rss"
  vm_balloon[$vm]="$balloon_enabled"
done

efficiency=$(awk -v rss="$total_rss_mb" -v cfg="$total_config_mb" 'BEGIN{if(cfg>0) printf "%.2f", (rss/cfg)*100; else print 0}')
avg_rss=$(awk -v rss="$total_rss_mb" -v n="${#created[@]}" 'BEGIN{if(n>0) printf "%.2f", rss/n; else print 0}')

echo "[INFO] Total configured MB: $total_config_mb"
echo "[INFO] Total RSS MB: $total_rss_mb"
echo "[INFO] Efficiency (RSS/Configured * 100): $efficiency%"
echo "[INFO] Balloon enabled: $balloon_enabled_count/${#created[@]}"

host_stats=$(curl -s "$API_BASE/resources/host")
host_total=$(echo "$host_stats" | jq -r '.total_memory_mb')
host_used=$(echo "$host_stats" | jq -r '.used_memory_mb')
host_avail=$(echo "$host_stats" | jq -r '.available_memory_mb')

echo "[INFO] Writing report to $REPORT"
{
  echo '{'
  echo '  "summary": {'
  echo "    \"vm_count\": ${#created[@]},"
  echo "    \"configured_total_mb\": $total_config_mb,"
  echo "    \"rss_total_mb\": $total_rss_mb,"
  echo "    \"efficiency_percent\": $efficiency,"
  echo "    \"avg_rss_mb\": $avg_rss,"
  echo "    \"balloon_enabled_count\": $balloon_enabled_count,"
  echo "    \"host_total_memory_mb\": $host_total,"
  echo "    \"host_used_memory_mb\": $host_used,"
  echo "    \"host_available_memory_mb\": $host_avail"
  echo '  },'
  echo '  "vms": ['
  first=1
  for vm in "${created[@]}"; do
    [[ $first -eq 0 ]] && echo ','
    first=0
    printf '    {"id":"%s","rss_mb":%s,"balloon_enabled":%s}' "$vm" "${vm_rss[$vm]}" "${vm_balloon[$vm]}"
  done
  echo ''
  echo '  ]'
  echo '}'
} > "$REPORT"

echo "[INFO] Done. Report: $REPORT"
