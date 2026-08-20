#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Pulls the nke-agent .deb from GHCR (OCI) when
# published, otherwise the nke-agent binary artifact, and SPIRE agent from
# GitHub Releases into ARTIFACTS_DIR.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
NKE_AGENT_VERSION="${NKE_AGENT_VERSION:-edge}"
NKE_AGENT_REGISTRY="${NKE_AGENT_REGISTRY:-ghcr.io/nvidia/nke/agent}"
NKE_AGENT_DEB_REGISTRY="${NKE_AGENT_DEB_REGISTRY:-ghcr.io/nvidia/nke/agent-deb}"
NKE_AGENT_PLATFORM="${NKE_AGENT_PLATFORM:-linux/amd64}"

# Pinned: same SPIRE release CAPD e2e already vendors (controller/hack/ci).
SPIRE_VERSION="${SPIRE_VERSION:-1.14.1}"
SPIRE_TARBALL="spire-${SPIRE_VERSION}-linux-amd64-musl.tar.gz"
SPIRE_URL="https://github.com/spiffe/spire/releases/download/v${SPIRE_VERSION}/${SPIRE_TARBALL}"
SPIRE_SHA256="${SPIRE_SHA256:-0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba}"

mkdir -p "${ARTIFACTS_DIR}"

if ! command -v oras >/dev/null 2>&1; then
  echo "oras is required to pull ${NKE_AGENT_DEB_REGISTRY} (or ${NKE_AGENT_REGISTRY})" >&2
  echo "https://oras.land/docs/installation" >&2
  exit 1
fi

pull_dir="$(mktemp -d)"
trap 'rm -rf "${pull_dir}"' EXIT

echo "pulling ${NKE_AGENT_DEB_REGISTRY}:${NKE_AGENT_VERSION} (${NKE_AGENT_PLATFORM})"
if oras pull "${NKE_AGENT_DEB_REGISTRY}:${NKE_AGENT_VERSION}" \
  --platform "${NKE_AGENT_PLATFORM}" \
  -o "${pull_dir}"; then
  deb="$(find "${pull_dir}" -maxdepth 1 -type f -name '*.deb' -print -quit)"
  if [[ -z "${deb}" ]]; then
    echo "oras pull of ${NKE_AGENT_DEB_REGISTRY}:${NKE_AGENT_VERSION} wrote no .deb" >&2
    ls -la "${pull_dir}" >&2
    exit 1
  fi
  install -m 0644 "${deb}" "${ARTIFACTS_DIR}/nke-agent.deb"
  echo "using Debian package $(basename "${deb}")"
else
  echo "no package at ${NKE_AGENT_DEB_REGISTRY}:${NKE_AGENT_VERSION}; falling back to OCI binary ${NKE_AGENT_REGISTRY}:${NKE_AGENT_VERSION}" >&2
  echo ".debs are published to GHCR on agent/v* tags (see agent-deb-publish.yml)." >&2
  oras pull "${NKE_AGENT_REGISTRY}:${NKE_AGENT_VERSION}" \
    --platform "${NKE_AGENT_PLATFORM}" \
    -o "${pull_dir}"
  if [[ ! -f "${pull_dir}/nke-agent" ]]; then
    echo "oras pull did not produce nke-agent" >&2
    ls -la "${pull_dir}" >&2
    exit 1
  fi
  install -m 0755 "${pull_dir}/nke-agent" "${ARTIFACTS_DIR}/nke-agent"
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

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nke-agent.service" "${ARTIFACTS_DIR}/nke-agent.service"
install -m 0644 "${files_dir}/nke-agent.service.d/spiffe.conf" "${ARTIFACTS_DIR}/nke-agent-spiffe.conf"
install -m 0644 "${files_dir}/spire-agent.service" "${ARTIFACTS_DIR}/spire-agent.service"
install -m 0644 "${files_dir}/nke-spire-configure.service" "${ARTIFACTS_DIR}/nke-spire-configure.service"
install -m 0644 "${files_dir}/spire-agent.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent.conf.tmpl"
install -m 0755 "${files_dir}/spire-configure.sh" "${ARTIFACTS_DIR}/spire-configure.sh"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
