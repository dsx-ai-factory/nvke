# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash
# Shared by deploy/check-site.sh: failure exit, cluster reads, the pin
# lookup, check reporting and failure diagnostics. Sourced, not run.

die() { printf '%s: %s\n' "${0##*/}" "$*" >&2; exit 1; }

# kq is kubectl for reading: it never fails the caller. A read whose failure
# must be a finding goes through check-site.sh's kread instead.
kq() { kubectl ${KUBECONFIG_PATH:+--kubeconfig "${KUBECONFIG_PATH}"} "$@" 2>/dev/null || true; }

# mk_var prints a pin from make/tool-versions.mk, or nothing when the file is
# not there; the check reports a missing pin as a finding.
mk_var() {
  local file="${REPO_ROOT}/make/tool-versions.mk"
  [[ -f "${file}" ]] || return 0
  sed -n "s/^$1[[:space:]]*?=[[:space:]]*//p" "${file}" | head -n 1
}

# The check prints one line per finding and counts the failures.
CHECK_FAILURES=0
check_pass() { printf 'PASS  %s\n' "$*"; }
check_warn() { printf 'WARN  %s\n' "$*"; }
check_skip() { printf 'SKIP  %s\n' "$*"; }
check_fail() {
  printf 'FAIL  %s\n' "$*"
  CHECK_FAILURES=$((CHECK_FAILURES + 1))
}

# diagnostics writes what a failed step leaves behind in the cluster into
# the state directory: nodes, pods, events, helm releases, controller logs.
diagnostics() {
  local ns deploy name
  local dir="${STATE_DIR}/diagnostics"
  [[ -n "${KUBECONFIG_PATH}" ]] || return 0
  mkdir -p "${dir}" 2>/dev/null || return 0
  kq get nodes -o wide >"${dir}/nodes.txt"
  kq get pods -A -o wide >"${dir}/pods.txt"
  kq get events -A --sort-by=.lastTimestamp | tail -200 >"${dir}/events.txt"
  helm --kubeconfig "${KUBECONFIG_PATH}" list -A >"${dir}/helm-list.txt" 2>&1 || true
  for ns in "${CONTROLLER_NAMESPACE}" capnico-system; do
    for deploy in $(kq -n "${ns}" get deployments -o name); do
      name="${deploy#deployment.apps/}"
      kq -n "${ns}" logs "${deploy}" --all-containers --tail=200 >"${dir}/${ns}-${name}.log"
    done
  done
  printf '%s: diagnostics written to %s\n' "${0##*/}" "${dir}" >&2
}
