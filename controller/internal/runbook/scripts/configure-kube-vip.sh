#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#
# Purpose: Configure kube-vip on control plane nodes for high availability during cluster initialization
# Prerequisites: Control plane node
#

set -euo pipefail

# Required environment variables with defaults

# VIP address
VIP_ADDRESS=${VIP_ADDRESS:-""}
# Unique identifier for the node chosen from the ASN range
BGP_AS=${BGP_AS:-""}
# IP address of the node used for BGP routing ID
NODE_IP=${NODE_IP:-""}

# Default kube-vip image pin. These two are a pair — DEFAULT_KUBE_VIP_DIGEST is
# the multi-arch index digest of DEFAULT_KUBE_VIP_VERSION. Bump them together.
DEFAULT_KUBE_VIP_VERSION="v1.2.0"
DEFAULT_KUBE_VIP_DIGEST="sha256:fe8c7b6641ba7633a12b337821e9e83a6456e8edc3434f97577058edd4eecaf6"

# Optional environment variables with defaults

# BGP peer address (defaults to node's default gateway, i.e. the Forge subnet gateway IP)
BGP_PEER_ADDRESS=${BGP_PEER_ADDRESS:-""}
# Constant BGP peer AS as specified by Forge
BGP_PEER_AS="${BGP_PEER_AS:-"4294967000"}"
# Full Forge Metadata Service URL used when BGP_PEER_AS is FROM_FMDS. The
# historical port remains the default for controllers that do not send it.
FMDS_ENDPOINT="${FMDS_ENDPOINT:-"http://169.254.169.254:7777/latest/meta-data/asn"}"
# Comma-separated multi-peer BGP list. Each entry is address:AS:password:multihop.
# When set, the single-peer path (BGP_PEER_ADDRESS / BGP_PEER_AS / FROM_FMDS /
# default-gateway fallback) is skipped and kube-vip's bgp_peers env is rendered
# instead. Required for spine-leaf fabrics where each control-plane node peers
# with multiple ToR switches.
BGP_PEERS=${BGP_PEERS:-""}
# Kube-vip version. Must be >= v1.2.0 for the control-plane BGP health check
# (CP_HEALTH_CHECK_* below); that feature was introduced in kube-vip v1.2.0.
KUBE_VIP_VERSION="${KUBE_VIP_VERSION:-"$DEFAULT_KUBE_VIP_VERSION"}"
# Image digest for the default version above. The rendered image reference is
# tag@digest, so the tag stays readable while the pull resolves by digest and
# cannot be moved under us by a re-push of the same tag.
#
# Set to "" to pull by mutable tag instead. If you override KUBE_VIP_VERSION you
# must also set (or clear) KUBE_VIP_DIGEST; validate_image_pin below refuses the
# mismatched combination rather than silently pulling the digest and ignoring
# the tag you asked for.
KUBE_VIP_DIGEST="${KUBE_VIP_DIGEST-"$DEFAULT_KUBE_VIP_DIGEST"}"
# Explicit interface override. When set, kube-vip uses this interface for both
# the BGP source (bgp_sourceif) and the VIP (vip_interface) instead of probing
# the interface that currently owns NODE_IP. Needed when NODE_IP will live on
# an OVS bridge that doesn't exist yet at static-pod render time.
VIP_INTERFACE="${VIP_INTERFACE:-""}"

# Control-plane apiserver health check (kube-vip >= v1.2.0). When the address is
# non-empty, kube-vip only announces the VIP via BGP while this endpoint returns
# 200, and withdraws it after consecutive failures (re-announcing on recovery).
# This stops a control-plane node whose apiserver is down or not-yet-ready --
# e.g. a freshly added CP, or one mid kubeadm upgrade -- from black-holing its
# share of ECMP'd API traffic. Set to "" to restore the legacy
# announce-on-startup behavior.
#
# Probe the node's own apiserver by NODE_IP, not localhost: the kubeadm apiserver
# serving cert covers the node IP and VIP but NOT localhost/127.0.0.1, so an
# https://localhost probe fails TLS verification and the route is never announced.
#
# Default endpoint is /readyz (not /livez or /healthz): we want "ready to serve
# client traffic", not merely "process alive". readyz stays false during apiserver
# startup (informer cache warmup) and after SIGTERM (graceful shutdown) while livez
# would still pass -- so livez would keep this node in ECMP during those windows
# and black-hole requests. Tradeoff: readyz also checks dependency health (etcd,
# etc.), so transient dependency blips can flip readyz and cause ECMP route churn;
# mitigate with period/failure_threshold below, or narrow the probe (e.g.
# /readyz?exclude=etcd). Upstream kube-vip docs recommend /livez for a narrower
# "process alive" signal; we prefer readyz for the not-yet-ready case above.
CP_HEALTH_CHECK_ADDRESS="${CP_HEALTH_CHECK_ADDRESS-"https://${NODE_IP}:6443/readyz"}"
# CA cert kube-vip uses to verify the HTTPS health endpoint. kubeadm clusters use
# a private CA, so this points at it and is mounted into the kube-vip static pod.
# Set to "" only if the endpoint is plain HTTP or uses a publicly-trusted cert.
CP_HEALTH_CHECK_CA_PATH="${CP_HEALTH_CHECK_CA_PATH-"/etc/kubernetes/pki/ca.crt"}"
# Health check cadence: seconds between probes, per-request timeout, and
# consecutive failures before route withdrawal. Matches kube-vip's own defaults.
CP_HEALTH_CHECK_PERIOD_SECONDS="${CP_HEALTH_CHECK_PERIOD_SECONDS:-"5"}"
# Per-probe timeout. 10s (not kube-vip's 3s default): readyz can block on etcd
# linearizable reads; etcd server-side timeout is 5s + 2× election timeout (~7s).
# A shorter timeout false-fails healthy nodes under load. Matches OpenShift apiserver
# readiness probe (cluster-kube-apiserver-operator#1517).
CP_HEALTH_CHECK_TIMEOUT_SECONDS="${CP_HEALTH_CHECK_TIMEOUT_SECONDS:-"10"}"
# Consecutive failures before route withdrawal. Matches kube-vip's own defaults.
CP_HEALTH_CHECK_FAILURE_THRESHOLD="${CP_HEALTH_CHECK_FAILURE_THRESHOLD:-"3"}"

# Error handling
error() {
    echo "Error: $1" >&2
    exit 1
}

check_dependencies() {
  local missing_deps=()
  # `command -v` is run directly, not wrapped in [ ]. Wrapping it passes the
  # words to the test builtin as operands instead of executing anything, which
  # is what this used to do.
  command -v jq >/dev/null 2>&1 || missing_deps+=("jq")
  command -v curl >/dev/null 2>&1 || missing_deps+=("curl")

  if [[ ${#missing_deps[@]} -gt 0 ]]; then
    error "Missing required tools: ${missing_deps[*]}"
  fi
}

# validate_image_pin refuses a KUBE_VIP_VERSION override that leaves the default
# digest in place. `repo:tag@digest` resolves by digest and ignores the tag, so
# that combination would silently run the default version while appearing to run
# the requested one.
validate_image_pin() {
    [[ -z "${KUBE_VIP_DIGEST:-}" ]] && return 0
    if [[ "$KUBE_VIP_VERSION" != "$DEFAULT_KUBE_VIP_VERSION" && \
          "$KUBE_VIP_DIGEST" == "$DEFAULT_KUBE_VIP_DIGEST" ]]; then
        error "KUBE_VIP_VERSION was overridden to $KUBE_VIP_VERSION but KUBE_VIP_DIGEST still holds the digest for $DEFAULT_KUBE_VIP_VERSION. Set KUBE_VIP_DIGEST to that version's digest, or KUBE_VIP_DIGEST=\"\" to pull by tag."
    fi
}

# Validate required environment variables
validate_environment() {
    local missing_vars=()

    [[ -z "$VIP_ADDRESS" ]] && missing_vars+=("VIP_ADDRESS")
    [[ -z "$BGP_AS" ]] && missing_vars+=("BGP_AS")
    [[ -z "$NODE_IP" ]] && missing_vars+=("NODE_IP")
    [[ -z "$KUBE_VIP_VERSION" ]] && missing_vars+=("KUBE_VIP_VERSION")
    [[ -z "$BGP_PEER_AS" ]] && missing_vars+=("BGP_PEER_AS")

    if [[ ${#missing_vars[@]} -gt 0 ]]; then
        error "Missing required environment variables: ${missing_vars[*]}"
    fi

    validate_image_pin

    # Multi-peer path replaces the single-peer validation wholesale.
    if [[ -n "${BGP_PEERS:-}" ]]; then
        validate_bgp_peers
        return
    fi

    # Validate BGP_PEER_AS format (numeric or FROM_FMDS constant)
    if [[ "$BGP_PEER_AS" != "FROM_FMDS" ]] && ! [[ "$BGP_PEER_AS" =~ ^[0-9]+$ ]]; then
        error "BGP_PEER_AS must be a numeric value or 'FROM_FMDS', got: $BGP_PEER_AS"
    fi
}

# validate_bgp_peers checks each comma-separated entry of BGP_PEERS conforms to
# kube-vip's address:AS:password:multihop format. The password may be empty;
# multihop must be "true" or "false".
validate_bgp_peers() {
    # `read -ra` silently drops a trailing empty field; check the raw
    # string first so leading/trailing/double commas don't slip past.
    [[ "$BGP_PEERS" == ,* || "$BGP_PEERS" == *,,* || "$BGP_PEERS" == *, ]] && \
        error "BGP_PEERS must not contain empty entries"

    local peers peer address as password multihop extra

    IFS=',' read -ra peers <<< "$BGP_PEERS"
    for peer in "${peers[@]}"; do
        # Trailing ':' produces a 5th empty field that read consumes as
        # `extra=""` — the `extra` non-empty check below would miss it.
        [[ "$peer" == *: ]] && error "BGP_PEERS entry has too many fields: $peer"
        IFS=':' read -r address as password multihop extra <<< "$peer"
        [[ -z "$address" ]] && error "BGP_PEERS entry is missing address: $peer"
        [[ -z "$as" ]] && error "BGP_PEERS entry is missing AS: $peer"
        [[ -n "${extra:-}" ]] && error "BGP_PEERS entry has too many fields: $peer"
        [[ "$as" =~ ^[0-9]+$ ]] || error "BGP_PEERS entry AS must be numeric: $peer"
        [[ "$multihop" == "true" || "$multihop" == "false" ]] || \
            error "BGP_PEERS entry multihop must be true or false: $peer"
    done
}

get_bgp_iface_name() {
  if [[ -n "${VIP_INTERFACE:-}" ]]; then
    echo "$VIP_INTERFACE"
    return 0
  fi

  local iface="$(ip -j addr | jq -r --arg nodeIp $NODE_IP '.[] | select(.addr_info[].local == $nodeIp) | .ifname')"
  if [[ -z $iface ]]; then
    error "Unable to find bgp interface"
  fi

  echo $iface
}

# yaml_quote emits a YAML-safe double-quoted scalar for arbitrary input by
# round-tripping through jq's JSON-string encoder (JSON strings are valid YAML
# 1.2 double-quoted scalars). Used for BGP_PEERS because per-peer passwords
# are operator-supplied and may contain quotes, backslashes, or newlines that
# would otherwise break the rendered manifest.
yaml_quote() {
    jq -Rn --arg v "$1" '$v'
}

# render_bgp_peer_env emits the per-peer env block for the kube-vip manifest.
# Multi-peer (BGP_PEERS) emits a single bgp_peers env entry; single-peer emits
# the historical bgp_peeraddress / bgp_peeras pair. bgp_as / bgp_routerid stay
# out of this helper because they are per-node, not per-peer.
render_bgp_peer_env() {
    if [[ -n "${BGP_PEERS:-}" ]]; then
        cat <<EOF
    - name: bgp_peers
      value: $(yaml_quote "$BGP_PEERS")
EOF
        return
    fi

    cat <<EOF
    - name: bgp_peeraddress
      value: "${BGP_PEER_ADDRESS:-}"
    - name: bgp_peeras
      value: "${BGP_PEER_AS:-}"
EOF
}

get_default_gateway() {
  local peer_address=$(ip -j route | jq -r 'map(select(.dst == "default"))[0].gateway')
  if [[ -z $peer_address ]]; then
    error "Unable to find default gateway to use as BGP peer address"
  fi

  echo "$peer_address"
}

# Fetch ASN from Forge Metadata Service (FMDS)
fetch_asn_from_fmds() {
  local fmds_url="$FMDS_ENDPOINT"
  local asn
  
  if ! asn=$(curl -s --connect-timeout 10 --max-time 30 "$fmds_url" 2>/dev/null); then
    error "Failed to fetch ASN from FMDS service at $fmds_url"
  fi
  
  if [[ -z "$asn" ]]; then
    error "FMDS service returned empty ASN"
  fi
  
  if ! [[ "$asn" =~ ^[0-9]+$ ]]; then
    error "FMDS service returned invalid ASN format: $asn"
  fi
  
  echo "$asn"
}

# cleanup_prev_interface drops $VIP_ADDRESS from interfaces holding a stale
# copy after a VIP_INTERFACE change. $1 names the desired interface, which
# is skipped so a no-op rerun doesn't flap the live VIP.
cleanup_prev_interface() {
  local desired_iface="$1"
  local ifaces
  ifaces=$(ip -j addr | jq -r --arg vip "$VIP_ADDRESS" '.[] | select(any(.addr_info[]?; .local == $vip)) | .ifname')
  if [[ -z "$ifaces" ]]; then
    return 0
  fi
  while IFS= read -r iface; do
    [[ -z "$iface" ]] && continue
    [[ "$iface" == "$desired_iface" ]] && continue
    if ip addr del "${VIP_ADDRESS}/32" dev "$iface" 2>/dev/null; then
      echo "Removed existing VIP from $iface"
    fi
  done <<< "$ifaces"
}

# Create list of peer IPs excluding this node
create_peer_list() {
    local peer_list=""
    local ips

    IFS=',' read -ra ips <<< "$CONTROL_PLANE_IPS"
    for ip in "${ips[@]}"; do
        if [[ "$ip" != "$NODE_IP" ]]; then
            if [[ -n "$peer_list" ]]; then
                peer_list="${peer_list},"
            fi
            peer_list="${peer_list}${ip}:${BGP_PEER_AS}::false"
        fi
    done

    echo "$peer_list"
}

# render_health_check_env emits the kube-vip control-plane health check env vars
# (kube-vip >= v1.2.0). An empty CP_HEALTH_CHECK_ADDRESS disables the feature and
# emits nothing, preserving the legacy announce-on-startup behavior.
render_health_check_env() {
    [[ -z "${CP_HEALTH_CHECK_ADDRESS:-}" ]] && return 0
    cat <<EOF
    - name: control_plane_health_check_address
      value: "${CP_HEALTH_CHECK_ADDRESS}"
    - name: control_plane_health_check_period_seconds
      value: "${CP_HEALTH_CHECK_PERIOD_SECONDS}"
    - name: control_plane_health_check_timeout_seconds
      value: "${CP_HEALTH_CHECK_TIMEOUT_SECONDS}"
    - name: control_plane_health_check_failure_threshold
      value: "${CP_HEALTH_CHECK_FAILURE_THRESHOLD}"
EOF
    [[ -z "${CP_HEALTH_CHECK_CA_PATH:-}" ]] && return 0
    cat <<EOF
    - name: control_plane_health_check_ca_path
      value: "${CP_HEALTH_CHECK_CA_PATH}"
EOF
}

# render_health_check_volume_mount / render_health_check_volume mount the CA cert
# used to verify the HTTPS health endpoint into the kube-vip static pod. Only
# emitted when both the health check and a CA path are set.
render_health_check_volume_mount() {
    [[ -z "${CP_HEALTH_CHECK_ADDRESS:-}" || -z "${CP_HEALTH_CHECK_CA_PATH:-}" ]] && return 0
    cat <<EOF
    - mountPath: ${CP_HEALTH_CHECK_CA_PATH}
      name: health-check-ca
      readOnly: true
EOF
}

render_health_check_volume() {
    [[ -z "${CP_HEALTH_CHECK_ADDRESS:-}" || -z "${CP_HEALTH_CHECK_CA_PATH:-}" ]] && return 0
    cat <<EOF
  - hostPath:
      path: ${CP_HEALTH_CHECK_CA_PATH}
      type: File
    name: health-check-ca
EOF
}

# Create kube-vip manifest
create_kube_vip_manifest() {
    local bgp_iface="$1"
    local vip_iface="${VIP_INTERFACE:-lo}"
    local manifest_path="/etc/kubernetes/manifests/kube-vip.yaml"
    local bgp_peer_env health_check_env health_check_vmount health_check_volume
    bgp_peer_env="$(render_bgp_peer_env)"
    health_check_env="$(render_health_check_env)"
    health_check_vmount="$(render_health_check_volume_mount)"
    health_check_volume="$(render_health_check_volume)"

    # Render the full desired manifest into a temp file first, then compare.
    # Reconciling on the entire desired state (not just VIP_ADDRESS) catches
    # changes to vip_interface / bgp_peers on reruns. Only swap in place when
    # the content differs so kubelet doesn't bounce kube-vip needlessly.
    mkdir -p /etc/kubernetes/manifests
    # Temp file lives in the manifests dir (same filesystem → mv is an atomic
    # rename) and starts with `.` so kubelet's static-pod globber ([^.]*)
    # ignores it during the brief render-then-compare window.
    local rendered
    rendered=$(mktemp "$(dirname "$manifest_path")/.kube-vip.tmp.XXXXXX")

    cat > "$rendered" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kube-vip
  namespace: kube-system
  annotations:
    scheduler.alpha.kubernetes.io/critical-pod: ""
  labels:
    component: kube-vip
    tier: control-plane
spec:
  containers:
  - args:
    - manager
    env:
    - name: vip_arp
      value: "false"
    - name: port
      value: "6443"
    - name: vip_nodename
      valueFrom:
        fieldRef:
          fieldPath: spec.nodeName
    - name: vip_interface
      value: "${vip_iface}"
    - name: vip_cidr
      value: "32"
    - name: dns_mode
      value: first
    - name: cp_enable
      value: "true"
    - name: cp_namespace
      value: kube-system
    - name: svc_enable
      value: "false"
    - name: svc_leasename
      value: plndr-svcs-lock
    - name: bgp_enable
      value: "true"
    - name: address
      value: "${VIP_ADDRESS}"
${bgp_peer_env}
    - name: bgp_as
      value: "${BGP_AS}"
    - name: bgp_routerid
      value: "${NODE_IP}"
    - name: bgp_sourceif
      value: "${bgp_iface}"
    - name: prometheus_server
      value: :2112
${health_check_env}
    image: ghcr.io/kube-vip/kube-vip:${KUBE_VIP_VERSION}${KUBE_VIP_DIGEST:+@${KUBE_VIP_DIGEST}}
    imagePullPolicy: IfNotPresent
    name: kube-vip
    resources:
      requests:
        cpu: 50m
        memory: 64Mi
      limits:
        memory: 128Mi
    securityContext:
      seccompProfile:
        type: RuntimeDefault
      capabilities:
        add:
        - NET_ADMIN
        - NET_RAW
    volumeMounts:
    - mountPath: /etc/kubernetes/admin.conf
      name: kubeconfig
      readOnly: true
${health_check_vmount}
  hostAliases:
  - hostnames:
    - kubernetes
    ip: 127.0.0.1
  hostNetwork: true
  volumes:
  - hostPath:
      path: "/etc/kubernetes/super-admin.conf"
    name: kubeconfig
${health_check_volume}
EOF

    if [[ -f "$manifest_path" ]] && cmp -s "$manifest_path" "$rendered"; then
        echo "kube-vip manifest already up to date"
        rm -f "$rendered"
        return 0
    fi

    echo "Writing kube-vip manifest at $manifest_path"
    mv "$rendered" "$manifest_path"
}

main() {
    check_dependencies
    validate_environment

    # Split: `local x=$(...)` masks failures because `local` returns 0
    # even when the substitution exits non-zero.
    local bgp_iface
    bgp_iface=$(get_bgp_iface_name)

    # Single-peer resolution only — multi-peer (BGP_PEERS) ships its own peer
    # list already and doesn't use BGP_PEER_AS / BGP_PEER_ADDRESS.
    if [[ -z "${BGP_PEERS:-}" ]]; then
        if [[ "$BGP_PEER_AS" == "FROM_FMDS" ]]; then
            BGP_PEER_AS=$(fetch_asn_from_fmds)
        fi
        if [[ -z "$BGP_PEER_ADDRESS" ]]; then
            BGP_PEER_ADDRESS=$(get_default_gateway)
            echo "Using default gateway as BGP peer address: $BGP_PEER_ADDRESS"
        fi
    fi

    cleanup_prev_interface "${VIP_INTERFACE:-lo}"
    create_kube_vip_manifest "$bgp_iface"
    echo "kube-vip configuration completed successfully"
}

if [[ "${BASH_SOURCE[0]:-}" == "${0}" ]]; then
    main "$@"
fi
