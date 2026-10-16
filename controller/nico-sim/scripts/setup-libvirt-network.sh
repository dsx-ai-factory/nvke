#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# setup-libvirt-network.sh — prepare a host to run nico-vms in libvirt-network
# mode, where nico-vms attaches VMs to a pre-existing libvirt NAT network
# (e.g. 'default'/virbr0) instead of creating its own bridge (controllerbr0) + dnsmasq
# + iptables. This is the counterpart to setup-linux.sh for environments where
# nico-vms must not create host networking of its own — most notably a
# hostNetwork Kubernetes pod (e.g. a shared GitLab libvirt runner), where a
# leaked bridge/iptables would pollute the node. Pair with
# NICO_VMS_LIBVIRT_NETWORK=<network> and NICO_VMS_LIBVIRT_URI=qemu:///system.
#
# Usage:
#   setup-libvirt-network.sh [--network NAME] [--uri URI] [--id ID]            # set up
#   setup-libvirt-network.sh [--network NAME] [--uri URI] [--id ID] --cleanup  # tear down
#
#   --id scopes the rule + state to a job (default: $CI_JOB_ID, else "local") so
#   concurrent jobs on a shared host don't remove each other's rule.
#
# What set-up does:
#   1. Ensures the libvirt network NAME is defined and active.
#   2. Exempts intra-subnet (VM<->VM) traffic from MASQUERADE by inserting a
#      RETURN at the top of the nat POSTROUTING chain. Some environments SNAT
#      inter-VM traffic to the node's IP (e.g. AWS EKS via the VPC CNI
#      AWS-SNAT-CHAIN, which masquerades anything not bound for the VPC CIDR).
#      That rewritten source IP breaks IP-bound peer TLS — etcd control-plane
#      joins fail with 'X does not match any of DNSNames'. The RETURN keeps the
#      real VM source IPs for VM<->VM traffic while leaving VM->internet NAT
#      intact. The subnet is derived from the network's own XML (not hardcoded).
#
# The rule carries a per-job comment tag, is idempotent, applied on every
# available iptables backend, and removed by --cleanup (call that from a CI
# after_script so it always runs) — cleanup removes only this job's tagged rule.
set -euo pipefail

NETWORK="default"
URI="qemu:///system"
CLEANUP=0
# Scope the iptables rule + its state file to this job so concurrent jobs on a
# shared host can't remove each other's no-masquerade rule. CI_JOB_ID is stable
# across the setup (before_script) and cleanup (after_script) invocations of one
# CI job; other callers can override with --id.
JOB_ID="${CI_JOB_ID:-local}"

while [ $# -gt 0 ]; do
  case "$1" in
    --network) NETWORK="$2"; shift 2 ;;
    --uri)     URI="$2"; shift 2 ;;
    --id)      JOB_ID="$2"; shift 2 ;;
    --cleanup) CLEANUP=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "setup-libvirt-network.sh: unknown arg: $1" >&2; exit 2 ;;
  esac
done

# Unique tag for this job's rule (an iptables comment) and its CIDR state file.
TAG="nico-vms-netmode-${JOB_ID}"
STATE_FILE="/tmp/${TAG}.cidr"

# mask2prefix 255.255.255.0 -> 24
mask2prefix() {
  local p=0 o a b c d
  IFS=. read -r a b c d <<EOF
$1
EOF
  for o in "$a" "$b" "$c" "$d"; do
    case "$o" in
      255) p=$((p + 8)) ;; 254) p=$((p + 7)) ;; 252) p=$((p + 6)) ;;
      248) p=$((p + 5)) ;; 240) p=$((p + 4)) ;; 224) p=$((p + 3)) ;;
      192) p=$((p + 2)) ;; 128) p=$((p + 1)) ;; 0) ;;
      *) echo "setup-libvirt-network.sh: bad netmask octet '$o'" >&2; return 1 ;;
    esac
  done
  echo "$p"
}

# network_cidr -> e.g. 192.168.122.0/24, derived from the network's IPv4 <ip>.
network_cidr() {
  local xml ipline addr mask a b c d ma mb mc md
  xml="$(virsh -c "$URI" net-dumpxml "$NETWORK" 2>/dev/null)" || return 1
  # The IPv4 <ip> element is the one carrying a netmask (IPv6 uses a prefix).
  ipline="$(printf '%s' "$xml" | tr '>' '\n' | grep "netmask=" | head -1)"
  addr="$(printf '%s' "$ipline" | sed -nE "s/.*address=['\"]([0-9.]+)['\"].*/\1/p" | head -1)"
  mask="$(printf '%s' "$ipline" | sed -nE "s/.*netmask=['\"]([0-9.]+)['\"].*/\1/p" | head -1)"
  [ -n "$addr" ] && [ -n "$mask" ] || return 1
  IFS=. read -r a b c d <<EOF
$addr
EOF
  IFS=. read -r ma mb mc md <<EOF
$mask
EOF
  echo "$((a & ma)).$((b & mb)).$((c & mc)).$((d & md))/$(mask2prefix "$mask")"
}

# remove_no_masq CIDR — delete only THIS job's rule (matched by its unique
# comment tag) from all iptables backends, leaving other jobs' rules intact.
remove_no_masq() {
  local cidr="$1" ipt
  for ipt in iptables iptables-nft iptables-legacy; do
    command -v "$ipt" >/dev/null 2>&1 || continue
    while "$ipt" -t nat -D POSTROUTING -s "$cidr" -d "$cidr" -m comment --comment "$TAG" -j RETURN 2>/dev/null; do :; done
  done
}

if [ "$CLEANUP" = 1 ]; then
  cidr="$(cat "$STATE_FILE" 2>/dev/null || true)"
  [ -n "$cidr" ] || cidr="$(network_cidr || true)"
  if [ -n "$cidr" ]; then
    remove_no_masq "$cidr"
    echo "setup-libvirt-network: removed no-masquerade rule(s) for $cidr"
  else
    echo "setup-libvirt-network: no CIDR to clean up (network '$NETWORK' gone?)"
  fi
  rm -f "$STATE_FILE"
  exit 0
fi

# ── Set up ──────────────────────────────────────────────────────────────────
virsh -c "$URI" net-info "$NETWORK" >/dev/null 2>&1 || {
  echo "setup-libvirt-network: network '$NETWORK' is not defined on $URI" >&2
  exit 1
}
if start_out="$(virsh -c "$URI" net-start "$NETWORK" 2>&1)"; then
  echo "setup-libvirt-network: started network '$NETWORK'"
elif printf '%s\n' "$start_out" | grep -qi "already active"; then
  # net-start exits non-zero with "...network is already active" when the network
  # is already up — the normal case on a shared runner where virbr0/'default' is
  # always active. Detect that from virsh's own (stable) message rather than
  # parsing net-info output. Any OTHER net-start failure (permission, socket,
  # URI, undefined) still falls through to the hard error below.
  echo "setup-libvirt-network: network '$NETWORK' already active"
else
  echo "setup-libvirt-network: failed to start network '$NETWORK': ${start_out}" >&2
  exit 1
fi

CIDR="$(network_cidr)" || {
  echo "setup-libvirt-network: could not derive an IPv4 subnet for '$NETWORK'" >&2
  exit 1
}
echo "setup-libvirt-network: network '$NETWORK' subnet is $CIDR"

# Exempt intra-subnet traffic from MASQUERADE (idempotent, all backends).
remove_no_masq "$CIDR"
applied=0
for ipt in iptables iptables-nft iptables-legacy; do
  command -v "$ipt" >/dev/null 2>&1 || continue
  if "$ipt" -t nat -I POSTROUTING 1 -s "$CIDR" -d "$CIDR" -m comment --comment "$TAG" -j RETURN 2>/dev/null; then
    echo "setup-libvirt-network: $ipt inserted no-masquerade RETURN for $CIDR ($TAG)"
    applied=1
  fi
done
[ "$applied" = 1 ] || { echo "setup-libvirt-network: failed to insert the no-masquerade rule on any iptables backend" >&2; exit 1; }
echo "$CIDR" > "$STATE_FILE"

echo "setup-libvirt-network: nat POSTROUTING (top):"
iptables -t nat -S POSTROUTING 2>/dev/null | head -8 || true
