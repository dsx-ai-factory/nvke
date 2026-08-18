#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

OUTPUT_DIR="${1:?usage: prepare-capd-bundles.sh OUTPUT_DIR}"
SPIRE_AGENT_VERSION="1.14.1"
TPM_PLUGIN_VERSION="1.9.0"
CALICO_VERSION="3.30.1"
ASSET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/assets" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

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
  "https://github.com/spiffe/spire/releases/download/v${SPIRE_AGENT_VERSION}/spire-${SPIRE_AGENT_VERSION}-linux-amd64-musl.tar.gz" \
  "0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba" \
  "${spire_archive}"
download_verified \
  "https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/spire_tpm_plugin_tpm_attestor_agent_linux_amd64_v${TPM_PLUGIN_VERSION}.tar.gz" \
  "64c9ed83857e802571f9e4c8377994bfb9d220708bcefe6b50674b2eee4bfbd2" \
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

calico_bundle_name="calico_v${CALICO_VERSION}_amd64"
calico_bundle="${TMP_DIR}/${calico_bundle_name}"
mkdir -p "${calico_bundle}/manifests"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/operator-crds.yaml" \
  "5969b2393d9cab1212480ad08dd989023106373c23163441474e65ceb2bb734f" \
  "${calico_bundle}/manifests/operator-crds.yaml"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/tigera-operator.yaml" \
  "65fc29459f236af44b31bd409640534db20ff940192f6b9023c972a69041977f" \
  "${calico_bundle}/manifests/tigera-operator.yaml"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/custom-resources.yaml" \
  "f7cd142773609fc7f830b96d8f29bd1b5f05cdc5293324992d8524b54c09c717" \
  "${calico_bundle}/manifests/custom-resources.yaml"
install -m 0755 "${ASSET_DIR}/calico-install.py" "${calico_bundle}/calico-install.py"
tar -czf "${OUTPUT_DIR}/${calico_bundle_name}.tgz" -C "${TMP_DIR}" "${calico_bundle_name}"

cat >"${OUTPUT_DIR}/index.json" <<EOF
{
  "calico": {
    "entries": [
      {
        "arch": "amd64",
        "path": "${calico_bundle_name}.tgz",
        "version": "${CALICO_VERSION}"
      }
    ]
  }
}
EOF
