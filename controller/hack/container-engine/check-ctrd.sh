#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Checks that the host can run the local dev flow on containerd alone
# (CONTAINER_CLI=ctrd; see lib.sh and ctrd/). It reports every problem it finds,
# not just the first, and exits non-zero if there is one. check.sh runs it.

set -uo pipefail

# shellcheck source=lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
ce_init || exit 1

CNI_DEFAULT_DIRS=(/opt/cni/bin /usr/local/libexec/cni /usr/local/lib/cni /usr/libexec/cni /usr/lib/cni)
# The plugins the networks ctrd writes use. kind's IPv6 subnet is configured on
# the bridge, so the host's own interfaces are not a problem to check for: ctrd
# answers an overlap the way Docker does and kind picks another subnet.
CNI_PLUGINS=(bridge portmap firewall host-local loopback)
namespace="${CTRD_NAMESPACE:-ctrd}"

failures=0
ok() { echo "[container-engine] ok: $*"; }
warn() { echo "[container-engine] WARN: $*" >&2; }
fail() {
  echo "[container-engine] FAIL: $*" >&2
  failures=$((failures + 1))
}

if (( EUID != 0 )) && ! sudo -n true 2>/dev/null; then
  fail "passwordless sudo is required: containerd is a rootful service, and ctrd makes network namespaces and CNI interfaces, so every ctrd call runs through 'sudo -n'," \
    "and a prompt would hang the tools that call it. Grant it (a NOPASSWD sudoers rule for this user), then run this again."
  # Nothing below can run as root without it.
  exit 1
fi

real="${CTRD_REAL:-${CE_DIR}/../../bin/ctrd}"
if [[ ! -x "${real}" ]]; then
  fail "${real} is not built. Run: make -C controller ctrd"
elif version="$(ctrd version --format '{{.Server.Version}}' 2>&1)"; then
  ok "containerd ${version} answers at ${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}, through ctr"
else
  fail "ctrd cannot reach containerd at ${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}: ${version}" \
    "ctr must be on root's PATH and containerd running (systemctl status containerd)."
fi

if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
  ok "cgroup v2"
else
  fail "cgroup v2 is not mounted at /sys/fs/cgroup: the node containers and ctrd's console supervisors need it"
fi

cni_dirs=()
if [[ -n "${CNI_PATH:-}" ]]; then
  IFS=: read -r -a cni_dirs <<<"${CNI_PATH}"
else
  cni_dirs=("${CNI_DEFAULT_DIRS[@]}")
fi
missing=()
for plugin in "${CNI_PLUGINS[@]}"; do
  found=false
  for dir in "${cni_dirs[@]}"; do
    if [[ -x "${dir}/${plugin}" ]]; then
      found=true
      break
    fi
  done
  [[ "${found}" == true ]] || missing+=("${plugin}")
done
if (( ${#missing[@]} > 0 )); then
  fail "CNI plugins missing from ${cni_dirs[*]}: ${missing[*]}. Install the containernetworking plugins, or set CNI_PATH."
else
  ok "CNI plugins in ${cni_dirs[*]}"
fi

# ctrd build drives buildkitd through buildctl, both looked up as root.
if ! ce_as_root sh -c 'command -v buildctl >/dev/null'; then
  fail "buildctl is not on root's PATH, which ctrd build needs. Install buildkit (buildctl and buildkitd) into /usr/local/bin."
else
  buildkit_host="${BUILDKIT_HOST:-unix:///run/buildkit/buildkitd.sock}"
  if ! workers="$(ce_as_root buildctl --addr "${buildkit_host}" debug workers --format '{{json .}}' 2>/dev/null)"; then
    fail "buildkitd does not answer at ${buildkit_host}. Start it, or set BUILDKIT_HOST."
  elif [[ "${workers}" == *'"org.mobyproject.buildkit.worker.executor":"containerd"'* &&
    "${workers}" == *"\"org.mobyproject.buildkit.worker.containerd.namespace\":\"${namespace}\""* ]]; then
    ok "buildkitd answers at ${buildkit_host}, with a containerd worker in the '${namespace}' namespace"
  else
    # An image another worker builds is not in the namespace ctrd runs containers from.
    fail "buildkitd at ${buildkit_host} has no containerd worker in the '${namespace}' namespace, so the images ctrd build makes would not be ctrd's." \
      "Run it with --oci-worker=false --containerd-worker=true --containerd-worker-namespace ${namespace}, or set CTRD_NAMESPACE to its namespace."
  fi
fi

for tool in ip mount umount cp sh; do
  ce_as_root sh -c "command -v ${tool} >/dev/null" || fail "${tool} is not on root's PATH, which ctrd needs"
done
command -v iptables >/dev/null 2>&1 || fail "iptables is required to allow forwarding on the kind bridge"

# The nodes' /var, and so their containerd, live in ctrd's state directory, where
# the node's own containerd needs a filesystem it can mount overlay on.
root="${CTRD_ROOT:-/var/lib/ctrd}"
probe="${root}"
while [[ ! -e "${probe}" && "${probe}" != / ]]; do probe="$(dirname "${probe}")"; done
fstype="$(stat -f -c %T "${probe}" 2>/dev/null || true)"
if [[ "${fstype}" == overlay* ]]; then
  fail "ctrd's state directory ${root} is on ${fstype}: the nodes' containerd cannot mount overlay there. Set CTRD_ROOT to a path on a disk filesystem."
else
  ok "ctrd state directory ${root} is on ${fstype:-an unknown filesystem}"
fi

# Containers get the host's real upstream resolvers, not a loopback stub.
if [[ -n "${CTRD_DNS:-}" ]]; then
  ok "containers use the resolvers in CTRD_DNS: ${CTRD_DNS}"
else
  upstream=""
  for file in /run/systemd/resolve/resolv.conf /etc/resolv.conf; do
    upstream="$(awk '$1 == "nameserver" && $2 ~ /^[0-9]+(\.[0-9]+){3}$/ && $2 !~ /^127\./ { print $2 }' "${file}" 2>/dev/null | tr '\n' ' ')"
    [[ -z "${upstream}" ]] || break
  done
  if [[ -n "${upstream}" ]]; then
    ok "containers use the host's upstream resolvers: ${upstream}"
  else
    fail "the host has no upstream IPv4 resolver outside the loopback stub; set CTRD_DNS to the servers containers should use"
  fi
fi

# GPUs reach the nodes through CDI, only when nico-sim is asked for them.
if [[ -n "${NICO_CONTAINERS_NODE_GPUS:-}" ]]; then
  if ! command -v nvidia-ctk >/dev/null 2>&1; then
    fail "NICO_CONTAINERS_NODE_GPUS is set and nvidia-ctk is not installed. Install the NVIDIA container toolkit."
  elif ! nvidia-ctk cdi list 2>/dev/null | grep -q '^nvidia.com/gpu='; then
    fail "NICO_CONTAINERS_NODE_GPUS is set and the CDI specification names no GPU. Generate it: sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml"
  else
    ok "CDI specification names the GPUs"
  fi
fi

if (( failures > 0 )); then
  echo "[container-engine] ${failures} problem(s); see controller/README.md, \"Running the local flow on containerd alone\"." >&2
  exit 1
fi
