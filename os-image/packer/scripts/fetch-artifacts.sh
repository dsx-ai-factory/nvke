#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nvke-agent Debian package from this checkout's
# source (agent/) and builds the node .deb packages (os-image/packages/,
# staged into the image by provision.sh as a local apt repo) into
# ARTIFACTS_DIR.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
# Set explicitly by the Packer Makefile from the requested image architecture,
# so this always matches the guest being built rather than the host.
ARCH="${ARCH:?ARCH is required}"
SOURCE_ROOT="${SOURCE_ROOT:?SOURCE_ROOT is required}"
# The image's Ubuntu release, which selects the DOCA OVS build.
UBUNTU_SERIES="${NKE_TARGET_UBUNTU_SERIES:?NKE_TARGET_UBUNTU_SERIES is required}"

case "${ARCH}" in
  amd64|arm64) ;;
  *)
    echo "unsupported ARCH=${ARCH} (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

script_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
if [[ ! "${SOURCE_ROOT}" -ef "${script_repo_root}" ]]; then
  echo "SOURCE_ROOT ${SOURCE_ROOT} does not own ${BASH_SOURCE[0]}" >&2
  exit 1
fi
repo_root="${script_repo_root}"

if [[ "${NKE_BUILD_IN_PLACE:-0}" != 1 ]]; then
  exec bash "$(dirname "${BASH_SOURCE[0]}")/package-builder.sh"
fi

mkdir -p "${ARTIFACTS_DIR}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required to build nvke-agent from source (agent/)" >&2
  echo "https://go.dev/dl/" >&2
  exit 1
fi

# Use the package target so dpkg owns the agent, updater, and their units.
# An exact output path avoids selecting a stale version from agent/bin.
echo "building nvke-agent deb from source (${repo_root}/agent, ${ARCH})"
make -C "${repo_root}/agent" "deb-${ARCH}" DEB_OUTPUT="${ARTIFACTS_DIR}/nvke-agent.deb"

# The debs a node installs at cluster create, plus a flat apt index that
# provision.sh stages as a local repo. Without the kubeadm one a node built
# here cannot obtain kubeadm at all: there is no bundle server to fall back to.
# Both architectures have a lock. bundle.sh runs the downloaded kubeadm, so the
# Packer host has to be the target architecture.
echo "building nvke-kubeadm-distro debs (${repo_root}/os-image/packages/kubeadm-distro)"
kubeadm_distro_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${K8S_VERSIONS:-}" ]]; then
  kubeadm_distro_args+=(K8S_VERSIONS="${K8S_VERSIONS}")
fi
make -C "${repo_root}/os-image/packages/kubeadm-distro" debs "${kubeadm_distro_args[@]}"

echo "building nvke-cni-calico debs (${repo_root}/os-image/packages/cni-calico)"
cni_calico_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${CALICO_VERSIONS:-}" ]]; then
  cni_calico_args+=(CALICO_VERSIONS="${CALICO_VERSIONS}")
fi
make -C "${repo_root}/os-image/packages/cni-calico" debs "${cni_calico_args[@]}"

echo "building nvke-cni-ovn debs (${repo_root}/os-image/packages/cni-ovn)"
cni_ovn_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${OVN_STREAMS:-}" ]]; then
  cni_ovn_args+=(OVN_STREAMS="${OVN_STREAMS}")
fi
make -C "${repo_root}/os-image/packages/cni-ovn" debs "${cni_ovn_args[@]}"

# The spire-agent deb: the upstream SPIRE agent and the TPM node attestor
# plugin, plus the units that run them. Its lock pins both architectures.
# A site cluster attests its nodes by TPM, so the image
# needs the plugin to join one; without it the agent can only use a join token,
# which the site cluster never mints.
echo "building spire-agent deb (${repo_root}/os-image/packages/spire-agent)"
spire_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${SPIRE_VERSION:-}" ]]; then
  spire_args+=(SPIRE_VERSION="${SPIRE_VERSION}")
fi
if [[ -n "${TPM_PLUGIN_VERSION:-}" ]]; then
  spire_args+=(TPM_PLUGIN_VERSION="${TPM_PLUGIN_VERSION}")
fi
make -C "${repo_root}/os-image/packages/spire-agent" debs "${spire_args[@]}"

# NVIDIA's DOCA Open vSwitch, which replaces Ubuntu's openvswitch-switch.
# provision.sh installs it at bake time. DOCA builds each Ubuntu release
# separately, so this is the one package set that depends on the target series.
echo "fetching DOCA OVS debs (${repo_root}/os-image/packages/doca-ovs, Ubuntu ${UBUNTU_SERIES})"
doca_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}" UBUNTU_SERIES="${UBUNTU_SERIES}")
if [[ -n "${DOCA_VERSION:-}" ]]; then
  doca_args+=(DOCA_VERSION="${DOCA_VERSION}")
fi
make -C "${repo_root}/os-image/packages/doca-ovs" debs "${doca_args[@]}"

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nvke-agent-spire.conf" "${ARTIFACTS_DIR}/nvke-agent-spire.conf"
install -m 0644 "${files_dir}/00-dhcp-all.yaml" "${ARTIFACTS_DIR}/00-dhcp-all.yaml"
install -m 0644 "${files_dir}/99-disable-network-config.cfg" "${ARTIFACTS_DIR}/99-disable-network-config.cfg"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
install -m 0644 "${files_dir}/conntrack-tune.service" "${ARTIFACTS_DIR}/conntrack-tune.service"
install -m 0755 "${files_dir}/configure-conntrack.sh" "${ARTIFACTS_DIR}/configure-conntrack.sh"
install -m 0644 "${files_dir}/10-nke-conntrack-timeouts.conf" "${ARTIFACTS_DIR}/10-nke-conntrack-timeouts.conf"
