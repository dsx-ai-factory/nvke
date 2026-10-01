#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Raises the nf_conntrack limits at boot, run by conntrack-tune.service. See
# os-image/SPEC.md.
#
# The kernel caps its default table at 262144 entries on any host with more
# than 4 GiB of RAM. Under externalTrafficPolicy: Cluster, OVN-Kubernetes
# creates three or more entries per TCP connection, so services with many
# short-lived connections exhaust that default quickly.
#
# Target: min(max(RAM_MiB * 64, 1024), 600000), which is 600000 on any node
# with 9375 MiB of RAM or more; the RAM term only matters on small VMs. The
# hash table (nf_conntrack_buckets) is sized 1:1 with the limit, the kernel's
# own default ratio.
#
# Both are only ever raised. On clusters that run kube-proxy (Calico),
# kube-proxy re-sets nf_conntrack_max from the CPU count every time it starts;
# re-running this script must not undo a larger value it wrote.
#
# TIME_WAIT drops from the kernel's 120s to 60s, so entries for
# Connection: close traffic expire sooner. The UDP stream timeout drops from
# 180s to 120s; kernels since 5.0 already default to 120s, so there it pins
# the value. Timeouts are per network namespace: this sets the host's, which
# is where OVS and kube-proxy track connections.

set -euo pipefail

readonly MAX_ENTRIES=600000
readonly TIME_WAIT_SECONDS=60
readonly UDP_STREAM_SECONDS=120

mem_kib="$(awk '/^MemTotal:/ { print $2; exit }' /proc/meminfo)"
if [[ ! "${mem_kib}" =~ ^[0-9]+$ ]]; then
  echo "conntrack-tune: cannot read MemTotal from /proc/meminfo" >&2
  exit 1
fi

# RAM_bytes / 16384, i.e. 64 entries per MiB.
target=$(( mem_kib / 16 ))
if (( target > MAX_ENTRIES )); then
  target=${MAX_ENTRIES}
elif (( target < 1024 )); then
  target=1024
fi

# raise KEY VALUE sets KEY to VALUE unless it is already at least VALUE.
raise() {
  local key="$1" want="$2" have
  have="$(sysctl -n "${key}")"
  if (( have >= want )); then
    echo "conntrack-tune: leaving ${key} at ${have} (target ${want})"
    return
  fi
  sysctl -w "${key}=${want}"
}

raise net.netfilter.nf_conntrack_max "${target}"
raise net.netfilter.nf_conntrack_buckets "${target}"
sysctl -w net.netfilter.nf_conntrack_tcp_timeout_time_wait="${TIME_WAIT_SECONDS}"
sysctl -w net.netfilter.nf_conntrack_udp_timeout_stream="${UDP_STREAM_SECONDS}"
