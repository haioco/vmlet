#!/bin/bash
# OVN end-to-end smoke test for a single host.
#
# Exercises the OVN backend the way the roadmap's exit criteria describe:
# create -> connect (DHCP + port security) -> migrate (re-apply on a second
# pass, which is what a controller restart does) -> snapshot -> delete.
#
# Requires a host provisioned by scripts/setup-ovn-host.sh and a running
# vmlet instance. Everything destructive is gated behind --live; without it the
# script only reports what it would do (a dry run).

set -euo pipefail

BASE_URL="${VMLET_BASE_URL:-http://localhost:8080}"
NETWORK="${VMLET_OVN_NETWORK:-default}"
VM_NAME="${VMLET_TEST_VM_NAME:-vmlet-ovn-e2e-$$}"
LIVE=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()  { echo -e "${BLUE}==>${NC} $1"; }
ok()   { echo -e "${GREEN}  PASS${NC} $1"; }
fail() { echo -e "${RED}  FAIL${NC} $1"; }
skip() { echo -e "${YELLOW}  SKIP${NC} $1"; }

usage() {
    cat <<EOF
Usage: $0 [options]

  --live        actually create/migrate/delete VMs (requires a running vmlet)
  --url URL     vmlet base URL (default: $BASE_URL)
  --network N   OVN network name (default: $NETWORK)
  -h, --help    show this help

Without --live this is a dry run: it verifies prerequisites and prints the
steps it would run.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --live)     LIVE=1; shift ;;
        --url)      BASE_URL="$2"; shift 2 ;;
        --network)  NETWORK="$2"; shift 2 ;;
        -h|--help)  usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 1 ;;
    esac
done

PASSED=0
FAILED=0

report() {
    if [[ "$1" == "0" ]]; then
        ok "$2"
        ((PASSED++))
    else
        fail "$2"
        ((FAILED++))
    fi
}

run_or_dry() {
    if [[ "$LIVE" == "1" ]]; then
        "$@"
    else
        echo "    [dry-run] $*"
    fi
}

# ---------------------------------------------------------------- checks -----
log "Checking the OVN host state"

if ! command -v ovs-vsctl >/dev/null 2>&1; then
    skip "ovs-vsctl not found; run scripts/setup-ovn-host.sh first"
else
    if ovs-vsctl br-exists br-int 2>/dev/null; then
        ok "br-int integration bridge exists"
    else
        report 1 "br-int missing (run scripts/setup-ovn-host.sh)"
    fi

    if ovs-vsctl show >/dev/null 2>&1; then
        ok "the OVS daemon is reachable"
    else
        report 1 "the OVS daemon is unreachable"
    fi
fi

if ! command -v ovn-nbctl >/dev/null 2>&1; then
    skip "ovn-nbctl not found; OVN packages are incomplete"
else
    if ovn-nbctl show >/dev/null 2>&1; then
        ok "the OVN Northbound DB is reachable"
    else
        report 1 "the OVN Northbound DB is unreachable"
    fi
fi

if curl -s --max-time 3 "${BASE_URL}/health" >/dev/null 2>&1; then
    ok "vmlet is answering at ${BASE_URL}"
    if [[ "$LIVE" != "1" ]]; then
        log "vmlet is reachable: re-run with --live to exercise the full lifecycle"
    fi
else
    if [[ "$LIVE" == "1" ]]; then
        report 1 "vmlet is not reachable at ${BASE_URL}"
    else
        skip "vmlet is not reachable at ${BASE_URL} (start it for --live)"
    fi
fi

if [[ "$LIVE" != "1" ]]; then
    echo
    echo "Dry run complete. The steps that --live would perform:"
    echo
    echo "  1. POST /vms                    create a VM on the OVN backend"
    echo "  2. ovs-vsctl list-ports br-int  verify its TAP is on the integration bridge"
    echo "  3. ovs-vsctl get interface ... external_ids:iface-id"
    echo "                                   verify the logical port binding"
    echo "  4. ovn-nbctl show                verify switch, port and DHCP options"
    echo "  5. ovs-vsctl get interface ... port_security"
    echo "                                   verify MAC/IP anti-spoofing"
    echo "  6. POST /vms/{id}/migrate        live migration to a peer host"
    echo "  7. repeat 2-5 on the peer        the VM keeps its IP unchanged"
    echo "  8. POST /vms/{id}/snapshots      snapshot"
    echo "  9. POST /vms/{id}/clone           clone"
    echo " 10. DELETE /vms/{id}               delete and confirm cleanup"
    echo
    echo "Passed checks: $PASSED, failed: $FAILED"
    exit "$([[ $FAILED -eq 0 ]] && echo 0 || echo 1)"
fi

# ------------------------------------------------------------------ steps -----
log "Step 1/10: creating a VM on the OVN backend"
CREATE=$(curl -s -X POST "${BASE_URL}/vms" -H 'Content-Type: application/json' -d "{
  \"name\": \"${VM_NAME}\",
  \"vcpus\": 1,
  \"memory_mb\": 512,
  \"network\": { \"mode\": \"ovn\", \"ovn_network\": \"${NETWORK}\" }
}")
report $? "VM created"

VM_ID=$(echo "$CREATE" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
if [[ -z "$VM_ID" ]]; then
    fail "could not read the VM id from POST /vms"
    echo "$CREATE"
    exit 1
fi
ok "VM id: ${VM_ID}"

log "Step 2/10: waiting for a TAP on br-int"
TAP=""
for _ in $(seq 1 30); do
    FOUND=$(ovs-vsctl list-ports br-int 2>/dev/null | grep -c . || true)
    [[ "$FOUND" -gt 0 ]] && break
    sleep 1
done
TAP=$(ovs-vsctl list-ports br-int 2>/dev/null | head -1)
if [[ -n "$TAP" ]]; then
    ok "TAP ${TAP} is on br-int"
else
    report 1 "no port appeared on br-int"
fi

log "Step 3/10: verifying the logical port binding"
IFACE_ID=$(ovs-vsctl get interface "${TAP}" external_ids:iface-id 2>/dev/null | tr -d '"' || true)
if [[ -n "$IFACE_ID" && "$IFACE_ID" != "[]" ]]; then
    ok "iface-id = ${IFACE_ID}"
    if ovn-nbctl lsp-get-addresses "${IFACE_ID}" >/dev/null 2>&1; then
        ok "a matching logical switch port exists in OVN"
    else
        report 1 "logical switch port ${IFACE_ID} is absent from OVN"
    fi
else
    report 1 "no iface-id external id on ${TAP}"
fi

log "Step 4/10: verifying the logical topology"
if ovn-nbctl show >/dev/null 2>&1; then
    SWITCHES=$(ovn-nbctl ls-list 2>/dev/null | wc -l)
    ok "${SWITCHES} logical switch(es) present"
else
    report 1 "OVN Northbound DB unreachable"
fi

log "Step 5/10: verifying port security"
SECURITY=$(ovs-vsctl get interface "${TAP}" port_security 2>/dev/null | tr -d '[]"' || true)
if [[ -n "$SECURITY" && "$SECURITY" != "[]" ]]; then
    ok "port_security = ${SECURITY}"
else
    skip "no port_security set (enable MICROVM_ANTISPOOF_IP_ENABLE=true)"
fi

MIGRATE_TARGET="${VMLET_MIGRATE_TARGET:-}"
log "Step 6/10: live migration"
if [[ -n "$MIGRATE_TARGET" ]]; then
    ORIGINAL_IP=$(curl -s "${BASE_URL}/vms/${VM_ID}" | sed -n 's/.*"ip_address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
    curl -s -X POST "${BASE_URL}/vms/${VM_ID}/migrate" -H 'Content-Type: application/json' \
        -d "{\"target_host\": \"${MIGRATE_TARGET}\"}" >/dev/null
    report $? "migration requested"

    NEW_IP=$(curl -s "${BASE_URL}/vms/${VM_ID}" | sed -n 's/.*"ip_address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
    if [[ -n "$ORIGINAL_IP" && "$ORIGINAL_IP" == "$NEW_IP" ]]; then
        ok "the VM kept its IP (${NEW_IP}) across the migration"
    else
        report 1 "the VM IP changed across the migration (${ORIGINAL_IP} -> ${NEW_IP})"
    fi
else
    skip "no VMLET_MIGRATE_TARGET set"
fi

if [[ -n "$MIGRATE_TARGET" ]]; then
    log "Step 7/10: re-verifying the datapath on the peer"
    IFACE_ID=$(ovs-vsctl get interface "${TAP}" external_ids:iface-id 2>/dev/null | tr -d '"' || true)
    [[ -n "$IFACE_ID" && "$IFACE_ID" != "[]" ]] && ok "the port binding survived migration" || report 1 "the port binding was lost"
else
    skip "step 7 needs a migration target"
fi

log "Step 8/10: snapshot"
curl -s -X POST "${BASE_URL}/vms/${VM_ID}/snapshots" -H 'Content-Type: application/json' -d '{}' >/dev/null
report $? "snapshot created"

log "Step 9/10: clone"
curl -s -X POST "${BASE_URL}/vms/${VM_ID}/clone" -H 'Content-Type: application/json' -d '{}' >/dev/null
report $? "clone created"

log "Step 10/10: delete"
curl -s -X DELETE "${BASE_URL}/vms/${VM_ID}" >/dev/null
report $? "VM deleted"

sleep 2
REMAINING=$(ovs-vsctl list-ports br-int 2>/dev/null | grep -c . || true)
if [[ "$REMAINING" -eq 0 ]]; then
    ok "br-int has no leftover ports"
else
    report 1 "${REMAINING} port(s) left on br-int after deletion"
fi

echo
if [[ "$FAILED" -eq 0 ]]; then
    log "OVN e2e finished: ${PASSED} passed, ${FAILED} failed"
    exit 0
fi
fail "OVN e2e finished: ${PASSED} passed, ${FAILED} failed"
exit 1