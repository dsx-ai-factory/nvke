#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Checks that the host can run the local dev flow on nerdctl and containerd
# (CONTAINER_CLI=nerdctl; see lib.sh). It reports every problem it finds, not
# just the first, and exits non-zero if there is one. Nothing to check, and
# silent, for docker: Tilt's own preflight looks for the docker binary.
#
# Run by `make host-up` and by Tilt's check-management-cluster resource.

set -uo pipefail

# shellcheck source=lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
ce_init || exit 1
[[ "$(ce_cli)" == nerdctl ]] || exit 0

# Where nerdctl looks for CNI plugins, in its order (pkg/defaults), and the
# plugins its default bridge network and kind's need.
CNI_DEFAULT_DIRS=(/opt/cni/bin /usr/local/libexec/cni /usr/local/lib/cni
  /home/linuxbrew/.linuxbrew/opt/cni-plugins/bin /usr/libexec/cni /usr/lib/cni)
CNI_PLUGINS=(bridge portmap firewall tuning host-local loopback)
# kind's own floor for its nerdctl provider.
MIN_NERDCTL_VERSION=1.7.0

failures=0
ok() { echo "[container-engine] ok: $*"; }
warn() { echo "[container-engine] WARN: $*" >&2; }
fail() {
  echo "[container-engine] FAIL: $*" >&2
  failures=$((failures + 1))
}

version_at_least() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]; }

if (( EUID != 0 )) && ! sudo -n true 2>/dev/null; then
  fail "passwordless sudo is required: containerd is a rootful service, so every nerdctl call runs through 'sudo -n'," \
    "and a prompt would hang the tools that call it. Grant it (a NOPASSWD sudoers rule for this user), then run this again."
  # Nothing below can run as root without it.
  exit 1
fi

version_out="$(nerdctl --version 2>&1)" || {
  fail "nerdctl did not run: ${version_out}"
  exit 1
}
version="$(sed -n 's/^nerdctl version \([0-9][0-9.]*\).*/\1/p' <<<"${version_out}")"
if [[ -z "${version}" ]]; then
  warn "could not read the nerdctl version from '${version_out}'"
elif version_at_least "${version}" "${MIN_NERDCTL_VERSION}"; then
  ok "nerdctl ${version}"
else
  fail "nerdctl ${version} is older than ${MIN_NERDCTL_VERSION}, which kind's nerdctl provider needs"
fi

if info_out="$(nerdctl info --format '{{.ServerVersion}}' 2>&1)"; then
  ok "containerd ${info_out} answers at ${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}"
else
  fail "nerdctl cannot reach containerd at ${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}: ${info_out}" \
    "Is containerd running (systemctl status containerd)? Docker's own containerd, on that socket, must not be stopped by stopping Docker."
fi

# nerdctl uses the first of these that exists, not the first that holds plugins.
cni_dirs=()
if [[ -n "${CNI_PATH:-}" ]]; then
  IFS=: read -r -a cni_dirs <<<"${CNI_PATH}"
else
  for dir in "${CNI_DEFAULT_DIRS[@]}"; do
    if [[ -e "${dir}" ]]; then
      cni_dirs=("${dir}")
      break
    fi
  done
  [[ ${#cni_dirs[@]} -gt 0 ]] || cni_dirs=("${CNI_DEFAULT_DIRS[0]}")
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

# nerdctl build drives buildkitd through buildctl, both looked up as root.
if ! ce_as_root sh -c 'command -v buildctl >/dev/null'; then
  fail "buildctl is not on root's PATH, which nerdctl build needs. Install buildkit (buildctl and buildkitd) into /usr/local/bin."
else
  workers=""
  buildkit_hosts=("${BUILDKIT_HOST:-}")
  if [[ -z "${BUILDKIT_HOST:-}" ]]; then
    buildkit_hosts=(unix:///run/buildkit-default/buildkitd.sock unix:///run/buildkit/buildkitd.sock)
  fi
  for host in "${buildkit_hosts[@]}"; do
    if workers="$(ce_as_root buildctl --addr "${host}" debug workers --format '{{json .}}' 2>/dev/null)"; then
      buildkit_host="${host}"
      break
    fi
    workers=""
  done
  if [[ -z "${workers}" ]]; then
    fail "buildkitd does not answer at ${buildkit_hosts[*]}. Start it, or set BUILDKIT_HOST."
  else
    ok "buildkitd answers at ${buildkit_host}"
    # With any other worker nerdctl build still works, but through an archive,
    # and the base images it pulls are not the ones containerd holds.
    namespace="${CONTAINERD_NAMESPACE:-default}"
    if [[ "${workers}" != *'"org.mobyproject.buildkit.worker.executor":"containerd"'* ||
      "${workers}" != *"\"org.mobyproject.buildkit.worker.containerd.namespace\":\"${namespace}\""* ]]; then
      warn "buildkitd is not running a containerd worker in the '${namespace}' namespace" \
        "(--oci-worker=false --containerd-worker=true --containerd-worker-namespace ${namespace}); builds will be slower and will not share base images with containerd"
    fi
  fi
fi

# The kind node image declares VOLUME /var/lib/containerd, and nico-sim's nodes
# mount a volume at /var: both land in nerdctl's data root, where the node's own
# containerd needs a filesystem it can mount overlay on.
data_root=/var/lib/nerdctl
toml="${NERDCTL_TOML:-/etc/nerdctl/nerdctl.toml}"
if [[ -r "${toml}" ]]; then
  configured="$(sed -n 's/^data_root *= *"\(.*\)".*/\1/p' "${toml}" | head -n1)"
  [[ -z "${configured}" ]] || data_root="${configured}"
fi
probe="${data_root}"
while [[ ! -e "${probe}" && "${probe}" != / ]]; do probe="$(dirname "${probe}")"; done
fstype="$(stat -f -c %T "${probe}" 2>/dev/null || true)"
if [[ "${fstype}" == overlay* ]]; then
  fail "nerdctl's data root ${data_root} is on ${fstype}: the nodes' containerd cannot mount overlay there. Set data_root in ${toml} to a path on a disk filesystem."
else
  ok "nerdctl data root ${data_root} is on ${fstype:-an unknown filesystem}"
fi

for tool in iptables ip; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required to allow forwarding on the kind bridge"
done
# kind derives the IPv6 subnet of its network from the network's name, so for
# "kind" it is always this one, and nerdctl refuses a subnet that a host
# interface already holds. Docker's own `kind` network leaves that behind: its
# bridge, down once Docker is stopped, keeps the address. Report it when kind has
# yet to create its network; once the nerdctl network exists, its bridge holds
# the address legitimately.
KIND_IPV6_PREFIX="fc00:f853:ccd:e793:"
if command -v ip >/dev/null 2>&1 && ! nerdctl network inspect kind >/dev/null 2>&1; then
  leftover="$(ip -o -6 addr show 2>/dev/null | awk -v prefix="${KIND_IPV6_PREFIX}" 'index($4, prefix) == 1 { print $2 }' | sort -u)"
  if [[ -n "${leftover}" ]]; then
    for iface in ${leftover}; do
      fail "interface ${iface} holds ${KIND_IPV6_PREFIX}:/64, the subnet kind creates its network with, and nerdctl rejects it as overlapping." \
        "This is what stopping Docker leaves behind: the bridge of Docker's own kind network. If nothing needs it, remove it: sudo ip link delete ${iface}"
    done
  else
    ok "no interface holds kind's IPv6 subnet ${KIND_IPV6_PREFIX}:/64"
  fi
fi

# nerdctl cp, which the FRR container's config goes through, runs the host's tar.
command -v tar >/dev/null 2>&1 || fail "tar is required by nerdctl cp"

if (( failures > 0 )); then
  echo "[container-engine] ${failures} problem(s); see controller/README.md, \"Run the local flow on containerd with nerdctl\"." >&2
  exit 1
fi
