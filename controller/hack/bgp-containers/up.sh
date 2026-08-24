#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Start an FRR BGP speaker for kube-vip on local container-based nodes.
#
# FRR runs with --net=host so zebra installs BGP-learned routes on the colima VM
# (or Linux host) kernel routing table: the same routing table the kind bridge
# uses for forwarding decisions. CAPD and NICo CP containers' default gateway
# is the kind bridge gateway (172.18.0.1 by default), and kube-vip's stock
# configure-kube-vip runbook auto-detects that gateway as its BGP peer, so no
# static routes and no manifest patching are needed.
# This is the closest analogue to nico-vms' embedded gobgp speaker.
#
# Usage:
#   ./up.sh           # start host-mode FRR
#   ./up.sh --verify  # also print BGP peer summary after start

set -euo pipefail

VERIFY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --verify) VERIFY=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

CONTAINER_NAME="${CONTAINER_NAME:-bgp-containers}"
NETWORK_NAME="${NETWORK_NAME:-kind}"
FRR_IMAGE="${FRR_IMAGE:-quay.io/frrouting/frr:9.1.0}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP_DIR="$(mktemp -d)"
started=false

cleanup() {
  rm -rf "${TMP_DIR}"
  if [[ "${started}" != true ]]; then
    docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

info()  { echo "[bgp-containers:up] $*"; }
die()   { echo "[bgp-containers:up] ERROR: $*" >&2; exit 1; }

# Resolve the kind bridge gateway. In host mode this is also FRR's effective
# bind address (FRR binds 0.0.0.0:179, but the gateway is what kube-vip will
# auto-detect and dial).
if ! docker network inspect "${NETWORK_NAME}" &>/dev/null; then
  die "Docker network '${NETWORK_NAME}' not found. Start the kind management cluster first."
fi
# Match an IPv4 address rather than "anything without a colon". A newer Docker
# CLI renders an absent Gateway as the string "invalid IP" (Go's zero netip.Addr),
# and the kind network lists its IPv6 subnet, which has no gateway, first. The
# old test then picked the word "invalid".
KIND_GW="$(docker network inspect "${NETWORK_NAME}" -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}$/) {print $i; exit}}')"
KIND_SUBNET="$(docker network inspect "${NETWORK_NAME}" -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}\/[0-9]+$/) {print $i; exit}}')"
[[ -n "${KIND_GW}" ]] || die "Could not detect kind network gateway."
[[ -n "${KIND_SUBNET}" ]] || die "Could not detect kind network subnet."
info "kind network: subnet=${KIND_SUBNET} gateway=${KIND_GW}"

if docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  info "Replacing existing FRR container '${CONTAINER_NAME}'."
  CONTAINER_NAME="${CONTAINER_NAME}" "${HERE}/down.sh"
fi

# Render frr.conf with the right router-id and listen range. Copy the files via
# the Docker API so this works with both local Colima and a remote DinD daemon.
RENDERED_CONF="${TMP_DIR}/frr.conf"
sed -e "s|@ROUTER_ID@|${KIND_GW}|g" \
    -e "s|@LISTEN_RANGE@|${KIND_SUBNET}|g" \
    "${HERE}/frr.conf.tmpl" > "${RENDERED_CONF}"

info "Starting FRR in host network mode (routes install on colima/host kernel)..."
docker create \
  --name "${CONTAINER_NAME}" \
  --network host \
  --cap-add NET_ADMIN \
  --cap-add SYS_ADMIN \
  --cap-add NET_BIND_SERVICE \
  "${FRR_IMAGE}" >/dev/null
docker cp "${RENDERED_CONF}" "${CONTAINER_NAME}:/etc/frr/frr.conf"
docker cp "${HERE}/vtysh.conf" "${CONTAINER_NAME}:/etc/frr/vtysh.conf"
docker cp "${HERE}/daemons" "${CONTAINER_NAME}:/etc/frr/daemons"
docker start "${CONTAINER_NAME}" >/dev/null
info "kube-vip will auto-detect ${KIND_GW} as its BGP peer (default gateway)."
info "No manifest patching required; the stock configure-kube-vip runbook works as-is."

info "Waiting for bgpd to start..."
ready=0
for _ in $(seq 1 30); do
  if docker exec "${CONTAINER_NAME}" vtysh -c 'show version' &>/dev/null; then
    info "FRR is up."
    ready=1
    break
  fi
  sleep 1
done

(( ready == 1 )) || die "FRR did not become ready within 30s."
started=true

info "Useful commands:"
echo "  docker exec ${CONTAINER_NAME} vtysh -c 'show bgp summary'"
echo "  docker exec ${CONTAINER_NAME} vtysh -c 'show bgp ipv4 unicast'"
echo "  docker exec -it ${CONTAINER_NAME} vtysh"

if (( VERIFY == 1 )); then
  info "Sleeping 10s before peer summary..."
  sleep 10
  docker exec "${CONTAINER_NAME}" vtysh -c 'show bgp summary' || true
fi
