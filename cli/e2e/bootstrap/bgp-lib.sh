#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The FRR BGP speaker kube-vip peers with, shared by run.sh and run-nico.sh.
# Source it; it defines functions only.
#
# The caller sets BGP_CONTAINER, FRR_IMAGE, BGP_FILES_DIR, BGP_RENDER_DIR,
# CONTROL_PLANE_VIP and NODE_NETWORK, and defines log, defer and wait_until.
# FRR runs on the host's network namespace, so it is the node network's default
# gateway, which is where kube-vip looks for its peer.

teardown_bgp_speaker() {
  # Let zebra withdraw its routes before removing the container.
  log "stopping the FRR BGP speaker ${BGP_CONTAINER}"
  nerdctl stop --time 10 "${BGP_CONTAINER}" >/dev/null 2>&1 || true
  nerdctl rm -f "${BGP_CONTAINER}" >/dev/null 2>&1 || true
  # Remove routes left behind if FRR could not withdraw them.
  ip route del "${CONTROL_PLANE_VIP}/32" 2>/dev/null || true
}

# bgp_vip_nexthops prints sorted, unique next-hop IPs for the VIP /32.
bgp_vip_nexthops() {
  local out
  out="$(nerdctl exec "${BGP_CONTAINER}" vtysh -c \
    "show bgp ipv4 unicast ${CONTROL_PLANE_VIP}/32 json" 2>/dev/null)" || return 0
  printf '%s' "${out}" | jq -r '.paths[]?.nexthops[]?.ip // empty' 2>/dev/null | sort -u || true
}

# start_bgp_speaker starts FRR before bootstrap so joining nodes can reach the VIP through BGP.
start_bgp_speaker() {
  local router_id listen_range

  # kube-vip peers with the default gateway. Read it and the node subnet from
  # the live network, skipping IPv6 entries that may have no gateway.
  router_id="$(nerdctl network inspect "${NODE_NETWORK}" \
    --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' \
    | awk '{for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}$/) {print $i; exit}}')"
  listen_range="$(nerdctl network inspect "${NODE_NETWORK}" \
    --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' \
    | awk '{for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}\/[0-9]+$/) {print $i; exit}}')"
  [ -n "${router_id}" ] || { log "FAIL: could not read the ${NODE_NETWORK} gateway for the FRR router-id"; return 1; }
  [ -n "${listen_range}" ] || { log "FAIL: could not read the ${NODE_NETWORK} subnet for the FRR listen range"; return 1; }
  log "FRR router-id=${router_id} listen-range=${listen_range} (node network ${NODE_NETWORK})"

  # Reuse the controller's FRR fixture with this network's addresses.
  rm -rf "${BGP_RENDER_DIR}"
  mkdir -p "${BGP_RENDER_DIR}"
  # Called as `start_bgp_speaker || exit 1`, so errexit is off for this body
  # and each step reports its own failure. FRR starts on an empty frr.conf.
  sed -e "s|@ROUTER_ID@|${router_id}|g" \
      -e "s|@LISTEN_RANGE@|${listen_range}|g" \
      "${BGP_FILES_DIR}/frr.conf.tmpl" > "${BGP_RENDER_DIR}/frr.conf" ||
    { log "FAIL: could not render frr.conf from ${BGP_FILES_DIR}/frr.conf.tmpl"; return 1; }
  [ -s "${BGP_RENDER_DIR}/frr.conf" ] ||
    { log "FAIL: rendered frr.conf is empty"; return 1; }
  cp "${BGP_FILES_DIR}/vtysh.conf" "${BGP_RENDER_DIR}/vtysh.conf" ||
    { log "FAIL: could not copy vtysh.conf from ${BGP_FILES_DIR}"; return 1; }
  cp "${BGP_FILES_DIR}/daemons" "${BGP_RENDER_DIR}/daemons" ||
    { log "FAIL: could not copy daemons from ${BGP_FILES_DIR}"; return 1; }

  # A leftover container from an aborted run would make `run` fail on the name.
  nerdctl rm -f "${BGP_CONTAINER}" >/dev/null 2>&1 || true

  # Host networking lets zebra install routes used by both nerdctl bridges.
  # Capabilities match controller/hack/bgp-containers/up.sh.
  defer teardown_bgp_speaker
  nerdctl run -d --name "${BGP_CONTAINER}" \
    --net host \
    --cap-add NET_ADMIN --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE \
    -v "${BGP_RENDER_DIR}/frr.conf:/etc/frr/frr.conf" \
    -v "${BGP_RENDER_DIR}/vtysh.conf:/etc/frr/vtysh.conf" \
    -v "${BGP_RENDER_DIR}/daemons:/etc/frr/daemons" \
    "${FRR_IMAGE}" >/dev/null || { log "FAIL: could not start the FRR BGP speaker"; return 1; }

  if ! wait_until 30 1 nerdctl exec "${BGP_CONTAINER}" vtysh -c 'show version' >/dev/null 2>&1; then
    log "FAIL: FRR (${BGP_CONTAINER}) did not become ready within 30s"
    nerdctl logs "${BGP_CONTAINER}" 2>&1 | sed 's/^/  /' || true
    return 1
  fi
  log "PASS: FRR BGP speaker ${BGP_CONTAINER} is up (image ${FRR_IMAGE}, pulled from quay.io)"
}
