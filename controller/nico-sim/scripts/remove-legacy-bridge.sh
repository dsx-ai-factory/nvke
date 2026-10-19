#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# remove-legacy-bridge.sh — remove the cwebr0 bridge an older nico-vms left behind.
#
# nico-vms named its bridge cwebr0 before controllerbr0. Both use the same
# address range, so a host that still has cwebr0 holds the gateway address the
# new bridge needs. This removes cwebr0 together with its dnsmasq, the iptables
# rules that name it, and its line in /etc/qemu/bridge.conf. The MASQUERADE
# rule names only the address range, which the new bridge shares, so it stays.
#
# Usage:
#   bash scripts/remove-legacy-bridge.sh <configured-bridge>
#
# Does nothing when the configured bridge is cwebr0 or the host is not Linux.
# Safe to run multiple times. setup-linux.sh, teardown-linux.sh and
# test/kubeadm/scripts/{setup,teardown}-ci-host.sh call it.

set -euo pipefail

LEGACY_BRIDGE="cwebr0"
CONFIGURED_BRIDGE="${1:?usage: remove-legacy-bridge.sh <configured-bridge>}"

[[ "${CONFIGURED_BRIDGE}" != "${LEGACY_BRIDGE}" ]] || exit 0
[[ "$(uname -s)" == Linux ]] || exit 0

log() { printf '[remove-legacy-bridge] %s\n' "$*"; }

# dnsmasq. The setup scripts pass --interface=cwebr0 and run-dnsmasq.sh passes
# interface:cwebr0, so match those exact forms: the configured bridge's dnsmasq
# shares the pid and lease files and must keep running. [d] keeps the pattern
# from matching the sudo process that carries it on its own command line.
if sudo pkill -f "[d]nsmasq.*(--interface=${LEGACY_BRIDGE}( |$)|interface:${LEGACY_BRIDGE},)"; then
    log "Stopped the dnsmasq on ${LEGACY_BRIDGE}"
fi

# iptables. Delete every rule whose input or output interface is the legacy
# bridge, whichever script added it. iptables -S double-quotes arguments that
# contain spaces, such as comments; xargs splits on that quoting. A rule that
# still fails to delete is reported, not fatal, so setup can go on.
for table in filter nat; do
    while IFS= read -r rule; do
        if printf '%s\n' "-D ${rule#-A }" | xargs sudo iptables -t "${table}"; then
            log "Removed iptables rule (${table}): ${rule#-A }"
        else
            log "WARNING: could not remove iptables rule (${table}): ${rule#-A }"
        fi
    done < <(sudo iptables -t "${table}" -S 2>/dev/null | grep -E -- "^-A .*-[io] ${LEGACY_BRIDGE}( |$)" || true)
done

# The bridge. Deleting it also removes its addresses, its routes and its proxy
# neighbour entries.
if ip link show "${LEGACY_BRIDGE}" >/dev/null 2>&1; then
    sudo ip link del "${LEGACY_BRIDGE}"
    log "Deleted bridge ${LEGACY_BRIDGE}"
fi

if grep -qx "allow ${LEGACY_BRIDGE}" /etc/qemu/bridge.conf 2>/dev/null; then
    sudo sed -i "/^allow ${LEGACY_BRIDGE}\$/d" /etc/qemu/bridge.conf
    log "Removed 'allow ${LEGACY_BRIDGE}' from /etc/qemu/bridge.conf"
fi
