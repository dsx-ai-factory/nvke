#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

OUTPUT_DIR="${1:?usage: prepare-capd-bundles.sh OUTPUT_DIR}"
SPIRE_AGENT_VERSION="1.14.1"
TPM_PLUGIN_VERSION="1.9.0"
# SPIRE and the TPM plugin ship real binaries, so they follow the node image's
# architecture. amd64 keeps the CI runners on the bytes they already used.
BUNDLE_ARCH="${BUNDLE_ARCH:-amd64}"
case "${BUNDLE_ARCH}" in
  amd64)
    SPIRE_AGENT_SHA256="0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba"
    TPM_PLUGIN_SHA256="64c9ed83857e802571f9e4c8377994bfb9d220708bcefe6b50674b2eee4bfbd2"
    ;;
  arm64)
    SPIRE_AGENT_SHA256="94ec569b697323b067db05565ff098eb3d8fa217dad40f367c961e239d54858d"
    TPM_PLUGIN_SHA256="b85502354358cd919691be022eb0f5678f5fdb9e84d86a2d1819362e18e796ae"
    ;;
  *)
    printf 'unsupported BUNDLE_ARCH: %s (amd64 or arm64)\n' "${BUNDLE_ARCH}" >&2
    exit 1
    ;;
esac
ASSET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/assets" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

command -v dpkg-deb >/dev/null 2>&1 || {
  printf 'dpkg-deb is required to build the local Calico manifests package\n' >&2
  exit 1
}

download_verified() {
  local url=$1
  local sha256=$2
  local destination=$3

  curl \
    --fail \
    --silent \
    --show-error \
    --location \
    --connect-timeout 10 \
    --max-time 30 \
    --retry 3 \
    --retry-delay 5 \
    --retry-connrefused \
    "${url}" \
    -o "${destination}"
  printf '%s  %s\n' "${sha256}" "${destination}" | sha256sum -c -
}

mkdir -p "${OUTPUT_DIR}"

spire_archive="${TMP_DIR}/spire.tar.gz"
tpm_archive="${TMP_DIR}/tpm-attestor.tar.gz"
download_verified \
  "https://github.com/spiffe/spire/releases/download/v${SPIRE_AGENT_VERSION}/spire-${SPIRE_AGENT_VERSION}-linux-${BUNDLE_ARCH}-musl.tar.gz" \
  "${SPIRE_AGENT_SHA256}" \
  "${spire_archive}"
download_verified \
  "https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/spire_tpm_plugin_tpm_attestor_agent_linux_${BUNDLE_ARCH}_v${TPM_PLUGIN_VERSION}.tar.gz" \
  "${TPM_PLUGIN_SHA256}" \
  "${tpm_archive}"

spire_extract="${TMP_DIR}/spire-extract"
tpm_extract="${TMP_DIR}/tpm-extract"
spire_bundle_name="spire_agent_v${SPIRE_AGENT_VERSION}_tpm_attestor_v${TPM_PLUGIN_VERSION}"
spire_bundle="${TMP_DIR}/${spire_bundle_name}"
mkdir -p "${spire_extract}" "${tpm_extract}" "${spire_bundle}/bin"
tar -xzf "${spire_archive}" -C "${spire_extract}"
tar -xzf "${tpm_archive}" -C "${tpm_extract}"
install -m 0755 "$(find "${spire_extract}" -type f -name spire-agent -print -quit)" "${spire_bundle}/bin/spire-agent"
install -m 0755 "$(find "${tpm_extract}" -type f -name tpm_attestor_agent -print -quit)" "${spire_bundle}/bin/tpm_attestor_agent"
install -m 0755 "${ASSET_DIR}/spire-agent-install.sh" "${spire_bundle}/install.sh"
install -m 0644 "${ASSET_DIR}/spire-agent.service" "${spire_bundle}/spire-agent.service"
printf 'TPM_PLUGIN_CHECKSUM=%s\n' "$(sha256sum "${spire_bundle}/bin/tpm_attestor_agent" | cut -d' ' -f1)" \
  >"${spire_bundle}/spire-agent.env"
tar -czf "${OUTPUT_DIR}/spire-agent.tgz" -C "${TMP_DIR}" "${spire_bundle_name}"

"$(dirname "${BASH_SOURCE[0]}")/prepare-calico-assets.sh" "${OUTPUT_DIR}"
