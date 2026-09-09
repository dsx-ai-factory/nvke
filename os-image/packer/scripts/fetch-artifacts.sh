#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nke-agent binary from this checkout's
# source (agent/) and fetches SPIRE agent from GitHub Releases into
# ARTIFACTS_DIR.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"

# Pinned: same SPIRE release CAPD e2e already vendors (controller/hack/ci).
SPIRE_VERSION="${SPIRE_VERSION:-1.14.1}"
case "${TARGET_ARCH}" in
  amd64)
    SPIRE_SHA256_DEFAULT="0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba"
    TPM_PLUGIN_SHA256_DEFAULT="64c9ed83857e802571f9e4c8377994bfb9d220708bcefe6b50674b2eee4bfbd2"
    TPM_PUBHASH_SHA256_DEFAULT="e70d183f48f6941c41525e690649f7003df69235d4eae6ab1a668dbb00948a47"
    ;;
  arm64)
    SPIRE_SHA256_DEFAULT="94ec569b697323b067db05565ff098eb3d8fa217dad40f367c961e239d54858d"
    TPM_PLUGIN_SHA256_DEFAULT="b85502354358cd919691be022eb0f5678f5fdb9e84d86a2d1819362e18e796ae"
    TPM_PUBHASH_SHA256_DEFAULT="c0145069cb87d9b57563de66004c2aa0d10d9e87869046caf0faefe677efa1a8"
    ;;
  *)
    echo "unsupported TARGET_ARCH: ${TARGET_ARCH}" >&2
    exit 1
    ;;
esac
SPIRE_TARBALL="spire-${SPIRE_VERSION}-linux-${TARGET_ARCH}-musl.tar.gz"
SPIRE_URL="https://github.com/spiffe/spire/releases/download/v${SPIRE_VERSION}/${SPIRE_TARBALL}"
SPIRE_SHA256="${SPIRE_SHA256:-${SPIRE_SHA256_DEFAULT}}"
TPM_PLUGIN_VERSION="${TPM_PLUGIN_VERSION:-1.9.0}"
TPM_PLUGIN_TARBALL="spire_tpm_plugin_tpm_attestor_agent_linux_${TARGET_ARCH}_v${TPM_PLUGIN_VERSION}.tar.gz"
TPM_PLUGIN_URL="https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/${TPM_PLUGIN_TARBALL}"
TPM_PLUGIN_SHA256="${TPM_PLUGIN_SHA256:-${TPM_PLUGIN_SHA256_DEFAULT}}"
TPM_PUBHASH_TARBALL="spire_tpm_plugin_get_tpm_pubhash_linux_${TARGET_ARCH}_v${TPM_PLUGIN_VERSION}.tar.gz"
TPM_PUBHASH_URL="https://github.com/spiffe/spire-tpm-plugin/releases/download/v${TPM_PLUGIN_VERSION}/${TPM_PUBHASH_TARBALL}"
TPM_PUBHASH_SHA256="${TPM_PUBHASH_SHA256:-${TPM_PUBHASH_SHA256_DEFAULT}}"

rm -rf "${ARTIFACTS_DIR}"
mkdir -p "${ARTIFACTS_DIR}"
DOWNLOAD_DIR="$(mktemp -d)"
trap 'rm -rf "${DOWNLOAD_DIR}"' EXIT

if ! command -v go >/dev/null 2>&1; then
  echo "go is required to build nke-agent from source (agent/)" >&2
  echo "https://go.dev/dl/" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# The architecture-specific target cross-compiles (CGO off) when needed and stamps
# main.VERSION from `git describe --match 'agent/v*'`, same as a release build.
echo "building nke-agent from source (${repo_root}/agent)"
make -C "${repo_root}/agent" "build-${TARGET_ARCH}"
install -m 0755 "${repo_root}/agent/bin/linux_${TARGET_ARCH}/nke-agent" "${ARTIFACTS_DIR}/nke-agent"

check_version="$(git -C "${repo_root}" describe --tags --dirty --match 'os-image/v[0-9]*' 2>/dev/null || git -C "${repo_root}" rev-parse --short HEAD)"
echo "building nke-os-image-check from source (${repo_root}/os-image)"
(
  cd "${repo_root}"
  CGO_ENABLED=0 GOOS=linux GOARCH="${TARGET_ARCH}" go build \
    -ldflags "-X main.version=${check_version#os-image/}" \
    -o "${ARTIFACTS_DIR}/nke-os-image-check" \
    ./os-image/cmd/nke-os-image-check
)

spire_tar="${DOWNLOAD_DIR}/${SPIRE_TARBALL}"
echo "fetching ${SPIRE_URL}"
curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 60 \
  --retry 3 --retry-delay 5 --retry-connrefused \
  -o "${spire_tar}" "${SPIRE_URL}"
printf '%s  %s\n' "${SPIRE_SHA256}" "${spire_tar}" | sha256sum -c -

extract="${DOWNLOAD_DIR}/spire-extract"
mkdir -p "${extract}"
tar -xzf "${spire_tar}" -C "${extract}"
spire_bin="$(find "${extract}" -type f -name spire-agent -print -quit)"
if [[ -z "${spire_bin}" ]]; then
  echo "spire tarball did not contain spire-agent" >&2
  exit 1
fi
install -m 0755 "${spire_bin}" "${ARTIFACTS_DIR}/spire-agent"

fetch_single_binary() {
  local name=$1
  local url=$2
  local sha256=$3
  local tarball=$4
  local archive="${DOWNLOAD_DIR}/${tarball}"
  local output="${DOWNLOAD_DIR}/${name}-extract"

  echo "fetching ${url}"
  curl --fail --silent --show-error --location \
    --connect-timeout 10 --max-time 60 \
    --retry 3 --retry-delay 5 --retry-connrefused \
    -o "${archive}" "${url}"
  printf '%s  %s\n' "${sha256}" "${archive}" | sha256sum -c -
  rm -rf "${output}"
  mkdir -p "${output}"
  tar -xzf "${archive}" -C "${output}"
  local binary
  binary="$(find "${output}" -type f -name "${name}" -print -quit)"
  if [[ -z "${binary}" ]]; then
    echo "${tarball} did not contain ${name}" >&2
    exit 1
  fi
  install -m 0755 "${binary}" "${ARTIFACTS_DIR}/${name}"
}

fetch_single_binary tpm_attestor_agent "${TPM_PLUGIN_URL}" "${TPM_PLUGIN_SHA256}" "${TPM_PLUGIN_TARBALL}"
fetch_single_binary get_tpm_pubhash "${TPM_PUBHASH_URL}" "${TPM_PUBHASH_SHA256}" "${TPM_PUBHASH_TARBALL}"

calico_dir="${DOWNLOAD_DIR}/calico"
bash "${repo_root}/controller/hack/ci/prepare-calico-assets.sh" "${calico_dir}"
calico_deb="$(find "${calico_dir}" -maxdepth 1 -type f -name 'nke-cni-calico-manifests-*_all.deb' -print -quit)"
if [[ -z "${calico_deb}" ]]; then
  echo "Calico asset builder produced no manifests package" >&2
  exit 1
fi
install -m 0644 "${calico_deb}" "${ARTIFACTS_DIR}/calico-manifests.deb"

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nke-agent.service" "${ARTIFACTS_DIR}/nke-agent.service"
install -m 0644 "${files_dir}/spire-agent.service" "${ARTIFACTS_DIR}/spire-agent.service"
install -m 0644 "${files_dir}/nke-spire-configure.service" "${ARTIFACTS_DIR}/nke-spire-configure.service"
install -m 0644 "${files_dir}/spire-agent.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent.conf.tmpl"
install -m 0644 "${files_dir}/spire-agent-tpm.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent-tpm.conf.tmpl"
install -m 0755 "${files_dir}/spire-configure.sh" "${ARTIFACTS_DIR}/spire-configure.sh"
install -m 0755 "${files_dir}/configure-nke-agent-plaintext.sh" "${ARTIFACTS_DIR}/configure-nke-agent-plaintext.sh"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
