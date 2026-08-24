#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# teardown-linux.sh — Tear down the nico-vms host networking and local state.
#
# Reverses the networking side of scripts/setup-linux.sh: stops the nico-vms
# dnsmasq, removes the bridge iptables rules, deletes the cwebr0 bridge (which
# drops its addresses and the BGP blackhole route), and wipes the nico-vms
# SQLite DB so it re-seeds from the current bridge CIDR on next start.
#
# It does NOT uninstall packages, remove group memberships, or touch the
# sudoers/qemu/AppArmor config from setup-linux.sh — those are harmless to keep
# and are reused on the next `setup-linux.sh` run.
#
# Usage:
#   bash scripts/teardown-linux.sh
#
# Reset / migrate to new network ranges:
#   tilt down                      # (or stop the nico-vms Tilt resource)
#   bash scripts/teardown-linux.sh
#   bash scripts/setup-linux.sh
#   tilt up
#
# Safe to run multiple times. Runs as the current user; sudo is used only for
# privileged operations.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

info() { echo -e "${GREEN}[INFO]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

BRIDGE_NAME="${NICO_VMS_BRIDGE_NAME:-cwebr0}"
# Current bridge CIDR (matches setup-linux.sh default; override to match yours).
BRIDGE_CIDR="${NICO_VMS_BRIDGE_CIDR:-172.30.0.0/24}"
# Legacy CIDRs whose leftover rules we also clean, so a one-time migration from
# an older default removes its stale iptables rules too.
LEGACY_CIDRS="${NICO_VMS_LEGACY_CIDRS:-192.168.200.0/24}"
# VIP ranges whose speaker-installed blackhole routes we remove. These are
# device-less routes in the main table (netlink Type=blackhole, no dev), so
# deleting the bridge does NOT remove them, and the BGP speaker leaves them in
# place on shutdown by design (VMs may still be running). Includes the legacy
# 192.168.250 default so a one-time migration cleans up its stale blackhole too.
VIP_CIDRS="${NICO_VMS_VIP_CIDR:-172.31.0.0/24} ${NICO_VMS_LEGACY_VIP_CIDRS:-192.168.250.0/24}"

DB_PATH="${NICO_VMS_DB_PATH:-${REPO_ROOT}/.dev/nico-vms.db}"
DHCP_HOSTS_DIR="${NICO_VMS_DHCP_HOSTS_DIR:-/var/lib/nico-vms/dhcp-hosts}"
DNSMASQ_PID="/var/run/nico-vms-dnsmasq.pid"
DNSMASQ_LEASE="${NICO_VMS_DHCP_LEASE_FILE:-/var/run/nico-vms-dnsmasq.leases}"

echo ""
printf "${BOLD}nico-vms teardown${NC}\n"
echo "------------------"
echo ""

if ! sudo -v; then
    echo -e "${RED}[FAIL]${NC} sudo access is required to remove the bridge and iptables rules"
    exit 1
fi

# Warn (don't block) if nico-vms still looks like it's running: it holds the
# bridge (metadata bind) and the DB open, so a clean teardown wants it stopped.
if pgrep -f "${REPO_ROOT}/bin/nico-vms" >/dev/null 2>&1; then
    warn "nico-vms appears to be running. Stop it first (tilt down, or stop the"
    warn "nico-vms Tilt resource) so the bridge/DB aren't recreated underneath you."
fi

# gateway_of <cidr> -> first host address of a /24 (e.g. 172.30.0.0/24 -> 172.30.0.1)
gateway_of() {
    local net="${1%/*}"
    echo "${net%.*}.1"
}

# remove_cidr_rules <cidr> — delete every setup-linux.sh iptables rule for a CIDR.
# Loops per rule to clear accidental duplicates; missing rules are ignored.
remove_cidr_rules() {
    local cidr="$1"
    local gw
    gw="$(gateway_of "$cidr")"

    while sudo iptables -t nat -D POSTROUTING -s "$cidr" ! -d "$cidr" -j MASQUERADE 2>/dev/null; do :; done
    while sudo iptables -D FORWARD -i "$BRIDGE_NAME" -s "$cidr" -j ACCEPT 2>/dev/null; do :; done
    while sudo iptables -D FORWARD -o "$BRIDGE_NAME" -d "$cidr" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; do :; done
    # Remove TCP DNS reject rules installed by MR !808.
    while sudo iptables -D INPUT -i "$BRIDGE_NAME" -d "$gw" -p tcp --dport 53 -j REJECT --reject-with tcp-reset 2>/dev/null; do :; done
    while sudo iptables -t nat -D PREROUTING -i "$BRIDGE_NAME" -d "$gw" -p tcp --dport 80 -j REDIRECT --to-port 7777 2>/dev/null; do :; done
    while sudo iptables -t nat -D PREROUTING -i "$BRIDGE_NAME" -d "$gw" -p tcp --dport 179 -j REDIRECT --to-port 10179 2>/dev/null; do :; done
}

# ── 1. Stop dnsmasq ─────────────────────────────────────────────────────────
if [ -f "$DNSMASQ_PID" ]; then
    sudo kill "$(cat "$DNSMASQ_PID")" 2>/dev/null || true
    sudo rm -f "$DNSMASQ_PID"
    info "Stopped nico-vms dnsmasq"
fi
sudo pkill -f "dnsmasq.*--interface=${BRIDGE_NAME}" 2>/dev/null || true
sudo rm -f "$DNSMASQ_LEASE"
sudo rm -f "${DHCP_HOSTS_DIR}"/* 2>/dev/null || true

# ── 2. Remove iptables rules (current + legacy CIDRs) ───────────────────────
for cidr in $BRIDGE_CIDR $LEGACY_CIDRS; do
    remove_cidr_rules "$cidr"
    info "Removed iptables rules for $cidr"
done

# ── 3. Remove speaker-installed blackhole VIP routes ────────────────────────
# The BGP speaker installs these as device-less routes in the main table, and
# neither the bridge deletion below nor the speaker's own shutdown removes them,
# so clean them up explicitly (leftovers would keep dropping VIP traffic).
for cidr in $VIP_CIDRS; do
    while sudo ip route del blackhole "$cidr" 2>/dev/null; do :; done
done
info "Removed speaker blackhole routes for: $VIP_CIDRS"

# ── 4. Delete the bridge (removes the interface, its addresses, and routes
# whose device is the bridge — e.g. the connected subnet and /32 ECMP nexthops).
if ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then
    sudo ip link del "$BRIDGE_NAME"
    info "Deleted bridge $BRIDGE_NAME"
else
    info "Bridge $BRIDGE_NAME not present"
fi

# ── 5. Wipe the nico-vms SQLite DB (re-seeds from bridge CIDR on next start) ─
if [ -f "$DB_PATH" ]; then
    rm -f "$DB_PATH" "${DB_PATH}-wal" "${DB_PATH}-shm"
    info "Wiped nico-vms DB $DB_PATH"
else
    info "nico-vms DB $DB_PATH not present"
fi

echo ""
info "Teardown complete. Re-run scripts/setup-linux.sh (then tilt up) to bring"
info "nico-vms back up on the current bridge CIDR ($BRIDGE_CIDR)."
echo ""
