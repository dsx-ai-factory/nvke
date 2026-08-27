#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Start dnsmasq for nico-vms DHCP/DNS on the VM bridge.
# Runs in the foreground — intended to be managed by Tilt via SSH.
# Kills any existing nico-vms dnsmasq before starting (takes ownership).
#
# Env vars (matching nico-vms config defaults):
#   NICO_VMS_BRIDGE_NAME      (default: cwebr0)
#   NICO_VMS_BRIDGE_CIDR      (default: 172.30.0.0/24)
#   NICO_VMS_DHCP_HOSTS_DIR   (default: /var/lib/nico-vms/dhcp-hosts)
#   NICO_VMS_DHCP_LEASE_FILE  (default: /var/run/nico-vms-dnsmasq.leases)
set -euo pipefail

BRIDGE="${NICO_VMS_BRIDGE_NAME:-cwebr0}"
CIDR="${NICO_VMS_BRIDGE_CIDR:-172.30.0.0/24}"
HOSTS_DIR="${NICO_VMS_DHCP_HOSTS_DIR:-/var/lib/nico-vms/dhcp-hosts}"
LEASE_FILE="${NICO_VMS_DHCP_LEASE_FILE:-/var/run/nico-vms-dnsmasq.leases}"
PID_FILE="/var/run/nico-vms-dnsmasq.pid"

# Derive IPs from CIDR (assumes /24).
PREFIX="${CIDR%.*}"
GATEWAY="${PREFIX}.1"
RANGE_START="${PREFIX}.10"
RANGE_END="${NICO_VMS_DHCP_RANGE_END:-${PREFIX}.239}"

# Kill any existing nico-vms dnsmasq to take ownership.
# First try PID file, then kill any dnsmasq on our bridge (catches instances
# started by setup scripts or previous runs with different PID files).
if [[ -f "$PID_FILE" ]]; then
    pid=$(cat "$PID_FILE" 2>/dev/null || true)
    if [[ -n "$pid" ]] && sudo kill -0 "$pid" 2>/dev/null; then
        echo "Stopping existing dnsmasq (PID $pid)"
        sudo kill "$pid" || true
    fi
    sudo rm -f "$PID_FILE"
fi
sudo pkill -f "dnsmasq.*${BRIDGE}" 2>/dev/null || true
sleep 1

sudo mkdir -p "$HOSTS_DIR"

# Host firewalls commonly default INPUT to DROP. Allow only the services that
# nico-vms exposes to guests on its dedicated bridge.
ensure_bridge_input_rule() {
    if ! sudo iptables -C INPUT -i "$BRIDGE" "$@" -j ACCEPT 2>/dev/null; then
        sudo iptables -I INPUT -i "$BRIDGE" "$@" -j ACCEPT
    fi
}
ensure_bridge_input_rule -p udp --dport 67
ensure_bridge_input_rule -p udp --dport 53
ensure_bridge_input_rule -p tcp --dport 53
ensure_bridge_input_rule -p tcp --dport 7777
ensure_bridge_input_rule -p tcp --dport 10179

# Detect the host's upstream DNS servers. Prefer resolvectl (systemd-resolved),
# fall back to /etc/resolv.conf non-loopback entries. Avoids hardcoding 8.8.8.8,
# which is blocked on networks that enforce internal DNS (e.g. corporate environments).
UPSTREAM_DNS_ARGS=()
if command -v resolvectl &>/dev/null; then
    while IFS= read -r dns_ip; do
        UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
    done < <(resolvectl status 2>/dev/null | awk '/DNS Servers:/{for(i=3;i<=NF;i++) print $i}' | grep -v '^$' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
    while IFS= read -r dns_ip; do
        UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
    done < <(grep '^nameserver' /etc/resolv.conf 2>/dev/null | awk '{print $2}' | grep -v '^127\.' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
    # Last resort: systemd-resolved stub handles forwarding
    UPSTREAM_DNS_ARGS=("--server=127.0.0.53")
fi

echo "Starting dnsmasq on ${BRIDGE} (${CIDR}), upstream DNS: ${UPSTREAM_DNS_ARGS[*]}"

# --keep-in-foreground keeps normal TCP worker forking while Tilt owns the foreground process.
# --log-facility=- sends runtime logs to stderr so Tilt can capture them.
exec sudo dnsmasq \
    --keep-in-foreground \
    --log-facility=- \
    --listen-address="$GATEWAY" \
    --bind-interfaces \
    --dhcp-range="interface:${BRIDGE},${RANGE_START},${RANGE_END},255.255.255.0,30m" \
    --dhcp-hostsdir="$HOSTS_DIR" \
    --dhcp-option=option:router,"$GATEWAY" \
    --dhcp-option=option:dns-server,"$GATEWAY" \
    --except-interface=lo \
    --no-resolv \
    --no-hosts \
    "${UPSTREAM_DNS_ARGS[@]}" \
    --address=/carbide-pxe.forge/"$GATEWAY" \
    --dhcp-leasefile="$LEASE_FILE" \
    --pid-file="$PID_FILE" \
    --log-dhcp \
    --dhcp-ignore-clid
