#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build one spire-agent_<version>_<arch>.deb from a bundle tree written by
# bundle.sh, using nfpm and packaging/nfpm.yaml.
#
# Usage:
#   build-deb.sh <arch> <bundle-dir> <out-dir>
#
# bundle-dir and out-dir may be relative to the caller's cwd and are
# canonicalised below, because nfpm runs with PACKAGING_DIR as its cwd and a
# relative path would resolve against the wrong directory.
#
# Environment:
#   SPIRE_VERSION       SPIRE release, e.g. 1.14.1 (required).
#   TPM_PLUGIN_VERSION  spire-tpm-plugin release, e.g. 1.9.0 (required).
#   DEB_REVISION        Debian packaging revision (default 1).
#   NFPM_BIN            Path to the nfpm binary (default: nfpm on PATH).
set -euo pipefail

usage() {
  echo "usage: $(basename "$0") <arch> <bundle-dir> <out-dir>" >&2
  exit 2
}

[[ $# -eq 3 ]] || usage
ARCH="$1"
case "$ARCH" in amd64 | arm64) ;; *) echo "build-deb.sh: unsupported arch ${ARCH}" >&2; exit 1 ;; esac
SRC="$(cd "$2" 2>/dev/null && pwd)" || { echo "build-deb.sh: no such bundle directory: $2" >&2; exit 1; }
mkdir -p "$3"
OUT_DIR="$(cd "$3" && pwd)"

: "${SPIRE_VERSION:?build-deb.sh: SPIRE_VERSION is required}"
: "${TPM_PLUGIN_VERSION:?build-deb.sh: TPM_PLUGIN_VERSION is required}"
DEB_REVISION="${DEB_REVISION:-1}"
NFPM_BIN="${NFPM_BIN:-nfpm}"

command -v "${NFPM_BIN}" >/dev/null 2>&1 || {
  echo "build-deb.sh: nfpm not found (${NFPM_BIN}); run 'make nfpm' or set NFPM_BIN" >&2
  exit 1
}

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Checked here so a partial `make bundle` fails with a message that says what
# to do about it, rather than with nfpm's missing-file error.
for required in spire-agent tpm_attestor_agent get_tpm_pubhash; do
  [[ -f "${SRC}/${required}" ]] || {
    echo "build-deb.sh: missing staged binary ${SRC}/${required}; run 'make bundle' first" >&2
    exit 1
  }
done

# nfpm's `scripts:` fields get no env-var expansion, so nfpm.yaml points at
# this fixed, package-relative path. The maintainer scripts need no templating,
# so this is a plain copy.
SCRIPTS_DIR="${PACKAGING_DIR}/../bin/.nfpm-scripts"
rm -rf "${SCRIPTS_DIR}"
mkdir -p "${SCRIPTS_DIR}"
for f in postinst prerm postrm; do
  install -m 0755 "${PACKAGING_DIR}/${f}" "${SCRIPTS_DIR}/${f}"
done

DEB_VERSION="${SPIRE_VERSION}+tpm${TPM_PLUGIN_VERSION}-${DEB_REVISION}"
export ARCH SRC SPIRE_VERSION TPM_PLUGIN_VERSION DEB_REVISION DEB_VERSION

# cd into PACKAGING_DIR: nfpm.yaml's ../../../files and scripts entries are
# given as paths relative to it.
(cd "${PACKAGING_DIR}" && "${NFPM_BIN}" package -f nfpm.yaml -p deb -t "${OUT_DIR}")
echo "wrote ${OUT_DIR}/spire-agent_${DEB_VERSION}_${ARCH}.deb"
