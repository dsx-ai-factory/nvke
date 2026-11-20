#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build nvke-node-sysconfig_<version>_all.deb with nfpm and packaging/nfpm.yaml.
#
# Usage:
#   build-deb.sh <out-dir>
#
# Environment:
#   DEB_VERSION  Debian package version (required).
#   NFPM_BIN     Path to the nfpm binary (default: nfpm on PATH).
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $(basename "$0") <out-dir>" >&2; exit 2; }
mkdir -p "$1"
OUT_DIR="$(cd "$1" && pwd)"

: "${DEB_VERSION:?build-deb.sh: DEB_VERSION is required}"
NFPM_BIN="${NFPM_BIN:-nfpm}"
command -v "${NFPM_BIN}" >/dev/null 2>&1 || {
  echo "build-deb.sh: nfpm not found (${NFPM_BIN}); run 'make nfpm' or set NFPM_BIN" >&2
  exit 1
}

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export DEB_VERSION

# cd into PACKAGING_DIR: nfpm.yaml's ../../../files and script entries are
# given as paths relative to it.
(cd "${PACKAGING_DIR}" && "${NFPM_BIN}" package -f nfpm.yaml -p deb -t "${OUT_DIR}")
echo "wrote ${OUT_DIR}/nvke-node-sysconfig_${DEB_VERSION}_all.deb"
