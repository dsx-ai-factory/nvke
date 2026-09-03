#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nke-agent binary from this checkout's
# source (agent/) and fetches SPIRE agent from GitHub Releases into
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
    SPIRE_ARCH=amd64
    # Pinned: same SPIRE release CAPD e2e already vendors (controller/hack/ci).
    SPIRE_SHA256_DEFAULT=0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba
    ;;
  arm64)
    AGENT_BUILD_TARGET=build-arm64
    AGENT_BIN_DIR=linux_arm64
    SPIRE_ARCH=arm64
    # Pinned from spire-1.14.1-linux-arm64-musl_sha256sum.txt on the v1.14.1
    # GitHub release, the same way the amd64 checksum above was sourced.
    SPIRE_SHA256_DEFAULT=94ec569b697323b067db05565ff098eb3d8fa217dad40f367c961e239d54858d
    ;;
  *)
    echo "unsupported ARCH=${ARCH} (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

SPIRE_VERSION="${SPIRE_VERSION:-1.14.1}"
SPIRE_TARBALL="spire-${SPIRE_VERSION}-linux-${SPIRE_ARCH}-musl.tar.gz"
SPIRE_URL="https://github.com/spiffe/spire/releases/download/v${SPIRE_VERSION}/${SPIRE_TARBALL}"
SPIRE_SHA256="${SPIRE_SHA256:-${SPIRE_SHA256_DEFAULT}}"

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
install -m 0644 "${files_dir}/spire-agent.service" "${ARTIFACTS_DIR}/spire-agent.service"
install -m 0644 "${files_dir}/nke-spire-configure.service" "${ARTIFACTS_DIR}/nke-spire-configure.service"
install -m 0644 "${files_dir}/spire-agent.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent.conf.tmpl"
install -m 0755 "${files_dir}/spire-configure.sh" "${ARTIFACTS_DIR}/spire-configure.sh"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
