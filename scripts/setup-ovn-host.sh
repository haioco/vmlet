#!/bin/bash
# vmlet OVN host provisioning.
#
# Provisions a fresh Debian/Ubuntu/RHEL-family host as an OVN node:
#   * installs Open vSwitch and OVN packages
#   * creates the br-int integration bridge
#   * installs and enables the ovsdb-server / ovn-northd / ovn-controller units
#   * verifies the result
#
# The script is idempotent: re-running it converges the host to the same state.

set -euo pipefail

BRIDGE="${VMLET_OVN_BRIDGE:-br-int}"
NB_DB_SOCK="/var/run/ovn/ovnnb_db.sock"
OVN_DB_DIR="/var/lib/ovn"
SYSTEMD_DIR="${VMLET_SYSTEMD_DIR:-/etc/systemd/system}"
UNIT_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/systemd/ovn"
DRY_RUN=0
SKIP_PACKAGES=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()   { echo -e "${BLUE}==>${NC} $1"; }
ok()    { echo -e "${GREEN}  ✓${NC} $1"; }
warn()  { echo -e "${YELLOW}  !${NC} $1"; }
err()   { echo -e "${RED}  ✗${NC} $1" >&2; }

usage() {
    cat <<EOF
Usage: $0 [options]

  --bridge NAME      OVS integration bridge (default: $BRIDGE)
  --dry-run          print what would be done, change nothing
  --skip-packages    do not install packages (assume already installed)
  -h, --help         show this help
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --bridge)         BRIDGE="$2"; shift 2 ;;
        --dry-run)        DRY_RUN=1; shift ;;
        --skip-packages)  SKIP_PACKAGES=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        *) err "unknown option: $1"; usage; exit 1 ;;
    esac
done

run() {
    if [[ "$DRY_RUN" == "1" ]]; then
        echo "    [dry-run] $*"
    else
        "$@"
    fi
}

as_root() {
    if [[ "$(id -u)" == "0" ]]; then
        run "$@"
    else
        run sudo "$@"
    fi
}

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- detect OS ---
detect_os() {
    if [[ -r /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        OS_ID="${ID:-unknown}"
        OS_LIKE="${ID_LIKE:-}"
    else
        OS_ID="unknown"
        OS_LIKE=""
    fi

    case "$OS_ID" in
        debian|ubuntu|*debian*|linuxmint|pop)
            PKG=apt ;;
        rhel|centos|fedora|rocky|almalinux|anolis|openeuler)
            if have dnf; then PKG=dnf; else PKG=yum; fi ;;
        sles|opensuse*|suse)
            PKG=zypper ;;
        *)
            # Fall back on ID_LIKE for derivatives.
            case "$OS_LIKE" in
                *debian*|*ubuntu*) PKG=apt ;;
                *rhel*|*fedora*|*centos*) PKG=dnf ;;
                *) PKG="" ;;
            esac ;;
    esac

    if [[ -z "$PKG" ]]; then
        err "unsupported distribution: ${OS_ID} (like: ${OS_LIKE:-none})"
        exit 1
    fi
    log "detected distribution: ${OS_ID} (package manager: ${PKG})"
}

# ------------------------------------------------------------- prerequisites ---
require_tools() {
    local missing=()
    for tool in "$@"; do
        have "$tool" || missing+=("$tool")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "missing required tool(s): ${missing[*]}"
        if [[ "$SKIP_PACKAGES" == "1" ]]; then
            err "install them manually or re-run without --skip-packages"
            exit 1
        fi
        return 1
    fi
}

# ------------------------------------------------------------------ packages ---
install_packages() {
    if [[ "$SKIP_PACKAGES" == "1" ]]; then
        warn "skipping package installation (--skip-packages)"
        ok "OVS/OVN packages assumed present"
        return
    fi
    log "installing Open vSwitch and OVN packages"
    case "$PKG" in
        apt)
            as_root env DEBIAN_FRONTEND=noninteractive apt-get update -qq
            as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
                openvswitch-switch openvswitch-common ovn-central ovn-host ovn-common
            ;;
        dnf) as_root dnf install -y openvswitch ovn-central ovn-host ;;
        yum) as_root yum install -y openvswitch ovn-central ovn-host ;;
        zypper) as_root zypper install -y openvswitch ovn-central ovn-host ;;
    esac
    ok "OVS/OVN packages installed"
}

# -------------------------------------------------------------------- bridge ---
ensure_bridge() {
    log "ensuring ${BRIDGE} integration bridge exists"
    if have ovs-vsctl && as_root ovs-vsctl br-exists "$BRIDGE" 2>/dev/null; then
        ok "${BRIDGE} already exists"
    else
        if ! have ovs-vsctl; then
            err "ovs-vsctl not found; install openvswitch first"
            exit 1
        fi
        as_root ovs-vsctl --may-exist add-br "$BRIDGE"
        ok "${BRIDGE} created"
    fi
}

# ----------------------------------------------------------------- units -------
install_units() {
    log "installing systemd units to ${SYSTEMD_DIR}"
    local units=(ovsdb-server.service ovn-northd.service ovn-controller.service)
    local missing=()
    for unit in "${units[@]}"; do
        [[ -f "${UNIT_SRC}/${unit}" ]] || missing+=("$unit")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "unit file(s) not found in ${UNIT_SRC}: ${missing[*]}"
        exit 1
    fi

    for unit in "${units[@]}"; do
        as_root install -D -m 0644 "${UNIT_SRC}/${unit}" "${SYSTEMD_DIR}/${unit}"
    done
    ok "unit files installed"
}

enable_services() {
    if ! have systemctl; then
        warn "systemctl not available; skipping service enablement (run units manually)"
        return 0
    fi
    log "enabling and starting OVN services"
    # Order matters: ovsdb-server before northd before controller.
    as_root systemctl daemon-reload
    as_root systemctl enable --now ovsdb-server ovn-northd ovn-controller
    for svc in openvswitch-switch ovsdb-server ovn-northd ovn-controller; do
        if as_root systemctl is-enabled "$svc" >/dev/null 2>&1; then
            ok "${svc} enabled"
        else
            warn "${svc} could not be enabled"
        fi
    done
}

# ------------------------------------------------------------------ verify -----
verify() {
    log "verifying the OVN datapath"
    local failed=0

    if have ovs-vsctl && as_root ovs-vsctl br-exists "$BRIDGE" 2>/dev/null; then
        ok "${BRIDGE} is configured in OVS"
    else
        err "${BRIDGE} is not a configured OVS bridge"
        failed=1
    fi

    if have ovs-vsctl && as_root ovs-vsctl show >/dev/null 2>&1; then
        ok "ovs-vsctl can talk to the OVS daemon"
    else
        warn "ovs-vsctl could not reach the OVS daemon"
    fi

    if have ovn-nbctl && as_root ovn-nbctl --db="unix:${NB_DB_SOCK}" show >/dev/null 2>&1; then
        ok "OVN Northbound DB is reachable at ${NB_DB_SOCK}"
    else
        warn "could not verify ${NB_DB_SOCK} (ovn-nbctl may need --db to point elsewhere)"
    fi

    if have ovn-controller && as_root systemctl is-active --quiet ovn-controller 2>/dev/null; then
        ok "ovn-controller is running"
    fi

    if [[ "$failed" != "0" ]]; then
        err "verification failed"
        return 1
    fi
    ok "host is ready for OVN networking"
}

# -------------------------------------------------------------------- main -----
main() {
    log "vmlet OVN host setup (bridge: ${BRIDGE})"
    detect_os

    if [[ "$DRY_RUN" == "1" ]]; then
        warn "dry-run mode: no changes will be made"
    fi

    install_packages

    require_tools ovs-vsctl || install_packages

    ensure_bridge

    if ! have ovn-nbctl; then
        warn "ovn-nbctl not found; OVN packages may be incomplete"
    fi

    install_units
    enable_services

    # Keep runtime state on a documented path.
    run mkdir -p "$OVN_DB_DIR"

    verify

    echo
    ok "done. Next: set MICROVM_NETWORK_BACKEND=ovn and start vmlet."
    echo "    NOTE: Firecracker guests need iptables NAT on the outside for external"
    echo "    connectivity until item 1.8 (distributed NAT) lands."
}

main "$@"