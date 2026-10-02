#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Generates /run/sysctl.d/10-nke-conntrack.conf at every boot, sized from this
# node's RAM, for systemd-sysctl.service to apply. Run by
# conntrack-tune.service, which is ordered before systemd-sysctl.service. See
# os-image/SPEC.md.
#
# The kernel caps its default table at 262144 entries on any host with more
# than 4 GiB of RAM. Under externalTrafficPolicy: Cluster, OVN-Kubernetes
# creates three or more entries per TCP connection, so services with many
# short-lived connections exhaust that default quickly.
#
# Target: min(max(MemTotal_KiB / 16, 1024), 600000). That is RAM_bytes / 16384,
# or 64 entries per MiB, capped at 600000, which any node with 9375 MiB of RAM
# or more reaches. The hash table (nf_conntrack_buckets) is sized 1:1 with the
# limit, the kernel's own ratio.
#
# The file goes under /run, not /etc: /run is emptied at every boot and is
# never part of the image, so the value always comes from the RAM of the node
# that is booting, never from the VM that built the image. A key is written
# only when the target is above what the kernel already has, so this never
# lowers the kernel's value; when nothing needs raising, no file is written.
#
# systemd-sysctl applies sysctl.d files in file-name order, and the last value
# set for a key wins. The 10- prefix sorts this file before the ones operators
# normally add, such as 60-conntrack.conf or 99-local.conf, so an operator's
# value for the same key wins, higher or lower. A same-named file in
# /etc/sysctl.d replaces this one outright.
#
# systemd-sysctl, and any later `sysctl --system`, re-applies this file. On a
# Calico node with 19 or more CPUs, kube-proxy raises nf_conntrack_max above
# 600000 when it starts; a later re-application lowers it back to 600000 until
# kube-proxy next restarts.
#
# --print-target prints the target computed from this node's RAM and exits, so
# checks do not need to restate the formula.

set -euo pipefail

# Overridable for tests; production always reads /proc and writes /run/sysctl.d.
MEMINFO="${CONNTRACK_MEMINFO:-/proc/meminfo}"
NETFILTER_DIR="${CONNTRACK_NETFILTER_DIR:-/proc/sys/net/netfilter}"
SYSCTL_DIR="${CONNTRACK_SYSCTL_DIR:-/run/sysctl.d}"

readonly MAX_ENTRIES=600000
readonly MIN_ENTRIES=1024
readonly DROPIN="${SYSCTL_DIR}/10-nke-conntrack.conf"

mem_kib="$(awk '/^MemTotal:/ { print $2; exit }' "${MEMINFO}")"
if [[ ! "${mem_kib}" =~ ^[0-9]+$ ]]; then
  echo "conntrack-tune: cannot read MemTotal from ${MEMINFO}" >&2
  exit 1
fi

# RAM_bytes / 16384, i.e. 64 entries per MiB.
target=$(( mem_kib / 16 ))
if (( target > MAX_ENTRIES )); then
  target=${MAX_ENTRIES}
elif (( target < MIN_ENTRIES )); then
  target=${MIN_ENTRIES}
fi

if [[ "${1:-}" == "--print-target" ]]; then
  echo "${target}"
  exit 0
fi

# /etc/modules-load.d/nf_conntrack.conf loads the module before this runs, so
# a missing key means it failed to load: there is nothing to compare against.
if [[ ! -r "${NETFILTER_DIR}/nf_conntrack_max" ]]; then
  echo "conntrack-tune: ${NETFILTER_DIR}/nf_conntrack_max is missing; is nf_conntrack loaded?" >&2
  exit 1
fi
have_max="$(cat "${NETFILTER_DIR}/nf_conntrack_max")"
have_buckets="$(cat "${NETFILTER_DIR}/nf_conntrack_buckets")"
if [[ ! "${have_max}" =~ ^[0-9]+$ || ! "${have_buckets}" =~ ^[0-9]+$ ]]; then
  echo "conntrack-tune: unexpected kernel values: max=${have_max} buckets=${have_buckets}" >&2
  exit 1
fi

# max and buckets are checked separately because they don't always start
# equal. If nf_conntrack is loaded with a hashsize option (for example
# "options nf_conntrack hashsize=131072" in /etc/modprobe.d), the kernel sets
# buckets to 131072 and max to 8 times that, 1048576. max is then already
# above the target and is left alone, while buckets is raised.
body=""
if (( target > have_max )); then
  body+="net.netfilter.nf_conntrack_max = ${target}"$'\n'
fi
if (( target > have_buckets )); then
  body+="net.netfilter.nf_conntrack_buckets = ${target}"$'\n'
fi

if [[ -z "${body}" ]]; then
  echo "conntrack-tune: nothing to raise: target ${target}, kernel has max ${have_max} and buckets ${have_buckets}; no file written"
  exit 0
fi

content="# Generated at boot by conntrack-tune.service (/usr/lib/nke/configure-conntrack.sh).
# MemTotal ${mem_kib} kB -> target ${target}. The kernel had nf_conntrack_max ${have_max}
# and nf_conntrack_buckets ${have_buckets}; only values above those are written.
# Regenerated every boot, so do not edit it. To override, add a same-named file in
# /etc/sysctl.d, or a later-sorting one such as 99-local.conf.
${body}"

# Write atomically: a temporary file in the same directory, then a rename.
install -d -m 0755 "${SYSCTL_DIR}"
tmp="$(mktemp "${SYSCTL_DIR}/.10-nke-conntrack.conf.XXXXXX")"
trap 'rm -f "${tmp}"' EXIT
printf '%s' "${content}" >"${tmp}"
if cmp -s "${tmp}" "${DROPIN}"; then
  echo "conntrack-tune: ${DROPIN} is unchanged"
  exit 0
fi
chmod 0644 "${tmp}"
mv -f "${tmp}" "${DROPIN}"
trap - EXIT
echo "conntrack-tune: wrote ${DROPIN} (MemTotal ${mem_kib} kB, target ${target}):"
printf '%s' "${body}"
