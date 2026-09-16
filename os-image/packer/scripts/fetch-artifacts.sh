#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nke-agent binary from this checkout's
# source (agent/) and builds the node .deb packages (os-image/packages/,
# staged into the image by provision.sh as a local apt repo) into
# ARTIFACTS_DIR.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
# Set by the shell-local provisioner from local.arch (nke-node-fde.pkr.hcl /
# nke-node-non-fde.pkr.hcl), which is itself derived from the ISO filename --
# so this always matches the guest actually being built, not the host.
ARCH="${ARCH:-amd64}"

case "${ARCH}" in
  amd64)
    AGENT_BUILD_TARGET=build-amd64
    AGENT_BIN_DIR=linux_amd64
    ;;
  arm64)
    AGENT_BUILD_TARGET=build-arm64
    AGENT_BIN_DIR=linux_arm64
    ;;
  *)
    echo "unsupported ARCH=${ARCH} (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

mkdir -p "${ARTIFACTS_DIR}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required to build nke-agent from source (agent/)" >&2
  echo "https://go.dev/dl/" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# build-amd64/build-arm64 cross-compile (CGO off) regardless of host arch and
# stamp main.VERSION from `git describe --match 'agent/v*'`, same as a
# release build.
echo "building nke-agent from source (${repo_root}/agent, ${ARCH})"
make -C "${repo_root}/agent" "${AGENT_BUILD_TARGET}"
install -m 0755 "${repo_root}/agent/bin/${AGENT_BIN_DIR}/nke-agent" "${ARTIFACTS_DIR}/nke-agent"

# The debs a node installs at cluster create, plus a flat apt index that
# provision.sh stages as a local repo. Without the kubeadm one a node built
# here cannot obtain kubeadm at all: there is no bundle server to fall back to.
# Both architectures have a lock. bundle.sh runs the downloaded kubeadm, so the
# Packer host has to be the target architecture.
echo "building nke-kubeadm-distro debs (${repo_root}/os-image/packages/kubeadm-distro)"
kubeadm_distro_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${K8S_VERSIONS:-}" ]]; then
  kubeadm_distro_args+=(K8S_VERSIONS="${K8S_VERSIONS}")
fi
make -C "${repo_root}/os-image/packages/kubeadm-distro" debs "${kubeadm_distro_args[@]}"

echo "building nke-cni-calico debs (${repo_root}/os-image/packages/cni-calico)"
cni_calico_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
if [[ -n "${CALICO_VERSIONS:-}" ]]; then
  cni_calico_args+=(CALICO_VERSIONS="${CALICO_VERSIONS}")
fi
make -C "${repo_root}/os-image/packages/cni-calico" debs "${cni_calico_args[@]}"

# OVN's chart and images need an NGC key for nv-ngn; CI has none, so opt-in.
# amd64 only: nothing has locked the arm64 image set, which differs in
# membership rather than just in digests.
if [[ "${ARCH}" == amd64 && -n "${NGC_API_KEY:-}" ]]; then
  echo "building nke-cni-ovn debs (${repo_root}/os-image/packages/cni-ovn)"
  cni_ovn_args=(OUT_DIR="${ARTIFACTS_DIR}/debs" ARCH="${ARCH}")
  if [[ -n "${OVN_STREAMS:-}" ]]; then
    cni_ovn_args+=(OVN_STREAMS="${OVN_STREAMS}")
  fi
  make -C "${repo_root}/os-image/packages/cni-ovn" debs "${cni_ovn_args[@]}"
elif [[ "${ARCH}" == amd64 ]]; then
  echo "NGC_API_KEY is unset; skipping the nke-cni-ovn debs (this image cannot install OVN offline)"
else
  echo "skipping the nke-cni-ovn debs: no arm64 lock (this image cannot install OVN offline)"
fi

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

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nke-agent.service" "${ARTIFACTS_DIR}/nke-agent.service"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
