#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Materialise the upstream SPIRE chart and print its directory.
#
# Every other component a deploy path installs is either published to a helm
# repository or lives in this repository with no dependencies, so helm can
# install it from coordinates or from a directory. spire is neither:
#
#   * it is pinned to a COMMIT, not a release, for global.spire.jwtIssuer, which
#     has no upstream release yet
#   * oci://ghcr.io/dsx-ai-factory/nke/charts/spire:0.29.0 does not exist
#   * it carries file://../spire-lib sibling dependencies, so the whole
#     helm-charts-hardened repository must be on disk for `helm dependency
#     build` -- a git-plugin sparse checkout strips the siblings. Same reason
#     controller/Tiltfile clones it.
#
# So it has to be fetched and resolved before anything can install it. The
# directory is printed on stdout for
# `use-nke-sbom.sh --chart-override spire=...`; logging goes to stderr.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${REPO_ROOT}/.local/spire-chart"

# Pinned in controller/Tiltfile as SPIRE_CHART_REPO_COMMIT (spire chart 0.29.0),
# and in deploy/*/spire/release.yaml as the version that comment documents.
SPIRE_REPO_URL="https://github.com/spiffe/helm-charts-hardened.git"
SPIRE_REPO_COMMIT="585203462b6bc7aa06d67b3a153bfff7ff287173"

log() { printf '\n=== %s\n' "$*" >&2; }

clone="${WORK_DIR}/helm-charts-hardened"
if [[ ! -d "${clone}/.git" ]]; then
  log "cloning helm-charts-hardened"
  mkdir -p "${WORK_DIR}"
  git clone --quiet "${SPIRE_REPO_URL}" "${clone}"
fi
git -C "${clone}" fetch --quiet origin "${SPIRE_REPO_COMMIT}" 2>/dev/null || git -C "${clone}" fetch --quiet
git -C "${clone}" checkout --quiet "${SPIRE_REPO_COMMIT}"
helm dependency build "${clone}/charts/spire" >/dev/null
printf '%s\n' "${clone}/charts/spire"
