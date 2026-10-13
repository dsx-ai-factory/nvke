#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Checks a booted node's conntrack tuning. boot-smoke.sh and demo-fde.sh pipe
# it to the guest over ssh (`ssh ... bash -s < check-conntrack.sh`), so the
# FDE and non-FDE images get the same checks. Needs passwordless sudo. Reads
# /proc/sys directly rather than calling sysctl, which lives in /usr/sbin and
# may not be on a non-login session's PATH.
#
# The target comes from configure-conntrack.sh --print-target, so this check
# cannot drift from the formula. The unit tests in os-image/files/test cover
# the 1024 floor, the 600000 cap and never-lower; a test VM only exercises
# whichever range its RAM falls in.

set -euo pipefail

NF=/proc/sys/net/netfilter
DROPIN=/run/sysctl.d/10-nke-conntrack.conf
TIMEOUTS=/etc/sysctl.d/10-nke-conntrack-timeouts.conf

fail() {
  echo "check-conntrack: FAIL: $*" >&2
  exit 1
}

read_values() {
  echo "$(<"${NF}/nf_conntrack_max") $(<"${NF}/nf_conntrack_buckets") $(<"${NF}/nf_conntrack_tcp_timeout_time_wait")"
}

systemctl is-active --quiet conntrack-tune.service || fail "conntrack-tune.service is not active"
[[ -f "${TIMEOUTS}" ]] || fail "${TIMEOUTS} is missing"

want="$(/usr/lib/nvke/configure-conntrack.sh --print-target)"
read -r max buckets time_wait <<<"$(read_values)"

# The generator writes nf_conntrack_max only when the target is above the
# kernel's default, so with that line the live value must be exactly the
# target; without it, the default was already at least the target.
if [[ -f "${DROPIN}" ]] && grep -q '^net\.netfilter\.nf_conntrack_max = ' "${DROPIN}"; then
  grep -qx "net.netfilter.nf_conntrack_max = ${want}" "${DROPIN}" ||
    fail "${DROPIN} does not set nf_conntrack_max to the target ${want}"
  (( max == want )) || fail "nf_conntrack_max is ${max}, but ${DROPIN} sets ${want}"
else
  (( max >= want )) || fail "nf_conntrack_max ${max} is below the target ${want}, and nothing raised it"
fi
# The kernel rounds the bucket count up, so it can only end at or above the target.
(( buckets >= want )) || fail "nf_conntrack_buckets ${buckets} is below the target ${want}"
(( time_wait == 60 )) || fail "nf_conntrack_tcp_timeout_time_wait is ${time_wait}, want 60"

# Re-applying sysctl.d, as systemd-sysctl and `sysctl --system` do, must leave
# the values exactly where they are.
before="$(read_values)"
sudo systemctl restart systemd-sysctl.service
after="$(read_values)"
[[ "${before}" == "${after}" ]] ||
  fail "values changed when systemd-sysctl re-applied sysctl.d: ${before} -> ${after}"

echo "check-conntrack: nf_conntrack_max=${max} (target ${want}), buckets=${buckets}, time_wait=${time_wait}; unchanged by a systemd-sysctl restart"
