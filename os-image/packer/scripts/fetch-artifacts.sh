#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nke-agent binary from this checkout's
# source (agent/), builds the CNI debs (os-image/packages/, staged into the
# image by provision.sh as a local apt repo) and fetches SPIRE agent from
# GitHub Releases into ARTIFACTS_DIR.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
# Set by the shell-local provisioner from local.arch (nke-node-fde.pkr.hcl /
# nke-node-non-fde.pkr.hcl), which is itself derived from the ISO filename --
# so this always matches the guest actually being built, not the host.
ARCH="${ARCH:-amd64}"

# Keep each pinned digest architecture-explicit. This prevents an update for
# one release artifact from looking like an architecture-neutral default.
readonly SPIRE_SHA256_AMD64=0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba
readonly SPIRE_SHA256_ARM64=94ec569b697323b067db05565ff098eb3d8fa217dad40f367c961e239d54858d
readonly TPM_PLUGIN_SHA256_AMD64=64c9ed83857e802571f9e4c8377994bfb9d220708bcefe6b50674b2eee4bfbd2
readonly TPM_PLUGIN_SHA256_ARM64=b85502354358cd919691be022eb0f5678f5fdb9e84d86a2d1819362e18e796ae
readonly TPM_PUBHASH_SHA256_AMD64=e70d183f48f6941c41525e690649f7003df69235d4eae6ab1a668dbb00948a47
readonly TPM_PUBHASH_SHA256_ARM64=c0145069cb87d9b57563de66004c2aa0d10d9e87869046caf0faefe677efa1a8

case "${ARCH}" in
  amd64)
    AGENT_BUILD_TARGET=build-amd64
    AGENT_BIN_DIR=linux_amd64
    SPIRE_ARCH=amd64
    SPIRE_SHA256_PINNED="${SPIRE_SHA256_AMD64}"
    TPM_PLUGIN_SHA256_PINNED="${TPM_PLUGIN_SHA256_AMD64}"
    TPM_PUBHASH_SHA256_PINNED="${TPM_PUBHASH_SHA256_AMD64}"
    ;;
  arm64)
    AGENT_BUILD_TARGET=build-arm64
    AGENT_BIN_DIR=linux_arm64
    SPIRE_ARCH=arm64
    SPIRE_SHA256_PINNED="${SPIRE_SHA256_ARM64}"
    TPM_PLUGIN_SHA256_PINNED="${TPM_PLUGIN_SHA256_ARM64}"
    TPM_PUBHASH_SHA256_PINNED="${TPM_PUBHASH_SHA256_ARM64}"
    ;;
  *)
    echo "unsupported ARCH=${ARCH} (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

SPIRE_VERSION="${SPIRE_VERSION:-1.14.1}"
SPIRE_TARBALL="spire-${SPIRE_VERSION}-linux-${SPIRE_ARCH}-musl.tar.gz"
SPIRE_URL="https://github.com/spiffe/spire/releases/download/v${SPIRE_VERSION}/${SPIRE_TARBALL}"
SPIRE_SHA256="${SPIRE_SHA256:-${SPIRE_SHA256_PINNED}}"

# The TPM node attestor, pinned to the same release controller/hack/ci vendors
# for the CAPD e2e. A site cluster attests its nodes by TPM, so the image needs
# this plugin to join one; without it the agent can only use a join token, which
# the site cluster never mints.
TPM_PLUGIN_VERSION="${TPM_PLUGIN_VERSION:-1.9.0}"
TPM_PLUGIN_TARBALL="spire_tpm_plugin_tpm_attestor_agent_linux_${SPIRE_ARCH}_v${TPM_PLUGIN_VERSION}.tar.gz"
TPM_PLUGIN_URL="https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/${TPM_PLUGIN_TARBALL}"
TPM_PLUGIN_SHA256="${TPM_PLUGIN_SHA256:-${TPM_PLUGIN_SHA256_PINNED}}"
TPM_PUBHASH_TARBALL="spire_tpm_plugin_get_tpm_pubhash_linux_${SPIRE_ARCH}_v${TPM_PLUGIN_VERSION}.tar.gz"
TPM_PUBHASH_URL="https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/${TPM_PUBHASH_TARBALL}"
TPM_PUBHASH_SHA256="${TPM_PUBHASH_SHA256:-${TPM_PUBHASH_SHA256_PINNED}}"

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

# The CNI debs nke-agent installs at cluster create, plus a flat apt index that
# provision.sh stages as a local repo. amd64 only: the locks pin amd64 digests.
if [[ "${ARCH}" == amd64 ]]; then
  echo "building nke-cni-calico debs (${repo_root}/os-image/packages/cni-calico)"
  cni_calico_args=(OUT_DIR="${ARTIFACTS_DIR}/debs")
  if [[ -n "${CALICO_VERSIONS:-}" ]]; then
    cni_calico_args+=(CALICO_VERSIONS="${CALICO_VERSIONS}")
  fi
  make -C "${repo_root}/os-image/packages/cni-calico" debs "${cni_calico_args[@]}"

  # The kubeadm/kubelet/containerd debs the agent installs at cluster create.
  # Without them a node built here cannot obtain kubeadm at all: there is no
  # bundle server to fall back to.
  echo "building nke-kubeadm-distro debs (${repo_root}/os-image/packages/kubeadm-distro)"
  kubeadm_distro_args=(OUT_DIR="${ARTIFACTS_DIR}/debs")
  if [[ -n "${K8S_VERSIONS:-}" ]]; then
    kubeadm_distro_args+=(K8S_VERSIONS="${K8S_VERSIONS}")
  fi
  make -C "${repo_root}/os-image/packages/kubeadm-distro" debs "${kubeadm_distro_args[@]}"

  # OVN's chart and images need an NGC key for nv-ngn; CI has none, so opt-in.
  if [[ -n "${NGC_API_KEY:-}" ]]; then
    echo "building nke-cni-ovn debs (${repo_root}/os-image/packages/cni-ovn)"
    cni_ovn_args=(OUT_DIR="${ARTIFACTS_DIR}/debs")
    if [[ -n "${OVN_STREAMS:-}" ]]; then
      cni_ovn_args+=(OVN_STREAMS="${OVN_STREAMS}")
    fi
    make -C "${repo_root}/os-image/packages/cni-ovn" debs "${cni_ovn_args[@]}"
  else
    echo "NGC_API_KEY is unset; skipping the nke-cni-ovn debs (this image cannot install OVN offline)"
  fi
else
  echo "building ARM64 nke-cni-calico debs (${repo_root}/os-image/packages/cni-calico)"
  bash "${repo_root}/os-image/packages/cni-calico/build-arm64.sh" "${ARTIFACTS_DIR}/debs"
fi

spire_tar="${ARTIFACTS_DIR}/${SPIRE_TARBALL}"
echo "fetching ${SPIRE_URL}"
curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 60 \
  --retry 3 --retry-delay 5 --retry-connrefused \
  -o "${spire_tar}" "${SPIRE_URL}"
printf '%s  %s\n' "${SPIRE_SHA256}" "${spire_tar}" | sha256sum -c -

extract="${ARTIFACTS_DIR}/spire-extract"
rm -rf "${extract}"
mkdir -p "${extract}"
tar -xzf "${spire_tar}" -C "${extract}"
spire_bin="$(find "${extract}" -type f -name spire-agent -print -quit)"
if [[ -z "${spire_bin}" ]]; then
  echo "spire tarball did not contain spire-agent" >&2
  exit 1
fi
install -m 0755 "${spire_bin}" "${ARTIFACTS_DIR}/spire-agent"

tpm_tar="${ARTIFACTS_DIR}/${TPM_PLUGIN_TARBALL}"
echo "fetching ${TPM_PLUGIN_URL}"
curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 60 \
  --retry 3 --retry-delay 5 --retry-connrefused \
  -o "${tpm_tar}" "${TPM_PLUGIN_URL}"
printf '%s  %s\n' "${TPM_PLUGIN_SHA256}" "${tpm_tar}" | sha256sum -c -

tpm_extract="${ARTIFACTS_DIR}/tpm-plugin-extract"
rm -rf "${tpm_extract}"
mkdir -p "${tpm_extract}"
tar -xzf "${tpm_tar}" -C "${tpm_extract}"
tpm_bin="$(find "${tpm_extract}" -type f -name tpm_attestor_agent -print -quit)"
if [[ -z "${tpm_bin}" ]]; then
  echo "tpm plugin tarball did not contain tpm_attestor_agent" >&2
  exit 1
fi
install -m 0755 "${tpm_bin}" "${ARTIFACTS_DIR}/tpm_attestor_agent"

tpm_pubhash_tar="${ARTIFACTS_DIR}/${TPM_PUBHASH_TARBALL}"
echo "fetching ${TPM_PUBHASH_URL}"
curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 60 \
  --retry 3 --retry-delay 5 --retry-connrefused \
  -o "${tpm_pubhash_tar}" "${TPM_PUBHASH_URL}"
printf '%s  %s\n' "${TPM_PUBHASH_SHA256}" "${tpm_pubhash_tar}" | sha256sum -c -

tpm_pubhash_extract="${ARTIFACTS_DIR}/tpm-pubhash-extract"
rm -rf "${tpm_pubhash_extract}"
mkdir -p "${tpm_pubhash_extract}"
tar -xzf "${tpm_pubhash_tar}" -C "${tpm_pubhash_extract}"
tpm_pubhash_bin="$(find "${tpm_pubhash_extract}" -type f -name get_tpm_pubhash -print -quit)"
if [[ -z "${tpm_pubhash_bin}" ]]; then
  echo "TPM public-hash tarball did not contain get_tpm_pubhash" >&2
  exit 1
fi
install -m 0755 "${tpm_pubhash_bin}" "${ARTIFACTS_DIR}/get_tpm_pubhash"

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nke-agent.service" "${ARTIFACTS_DIR}/nke-agent.service"
install -m 0644 "${files_dir}/spire-agent.service" "${ARTIFACTS_DIR}/spire-agent.service"
install -m 0644 "${files_dir}/nke-spire-configure.service" "${ARTIFACTS_DIR}/nke-spire-configure.service"
install -m 0644 "${files_dir}/spire-agent.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent.conf.tmpl"
install -m 0755 "${files_dir}/spire-configure.sh" "${ARTIFACTS_DIR}/spire-configure.sh"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
