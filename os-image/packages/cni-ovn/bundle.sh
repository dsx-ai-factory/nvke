#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stages the ovn-kubernetes chart and images for the nke-cni-ovn-<stream> debs.
#
# Usage: bundle.sh --version 1.4.0 --dist DIR --lock-dir DIR --crane PATH
#                  [--arch amd64] [--write-lock]
#
# NGC_API_KEY must be set: the chart and the images live in NGC's nv-ngn org.
# The lock and image handling is ../cni-common/bundle-lib.sh.

set -euo pipefail

CNI=cni-ovn
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../cni-common/bundle-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../cni-common/bundle-lib.sh"

parse_bundle_args "$@"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "--version must be a bare chart version such as 1.4.0, got ${VERSION}"
[[ -n "${NGC_API_KEY:-}" ]] || die "NGC_API_KEY is required (an NGC key with access to the nv-ngn org)"

# Overridable so a test can point both at a local server and a fake crane.
HELM_BASE="${NGC_HELM_URL:-https://helm.ngc.nvidia.com/nv-ngn/sdn/charts}"
CHART=ovn-kubernetes
CHART_TGZ="${CHART}-${VERSION}.tgz"

# The key never goes on a command line: curl reads it from a config on stdin,
# crane from a throwaway DOCKER_CONFIG that is removed on exit.
CURL_CONFIG="user = \"\$oauthtoken:${NGC_API_KEY}\""
AUTH_DIR="$(mktemp -d)"
trap 'rm -rf "$AUTH_DIR"' EXIT
export DOCKER_CONFIG="$AUTH_DIR"
"$CRANE" auth login "${NGC_REGISTRY:-nvcr.io}" --username '$oauthtoken' --password-stdin <<<"$NGC_API_KEY" >/dev/null ||
  die "crane auth login failed"

mkdir -p "${STAGE}/charts"
fetch "${HELM_BASE}/${CHART_TGZ}" "${WORK}/${CHART_TGZ}"
verify_artifact "$CHART_TGZ" "${WORK}/${CHART_TGZ}" "${HELM_BASE}/${CHART_TGZ}"
tar_paths_safe "${WORK}/${CHART_TGZ}"
tar -xzf "${WORK}/${CHART_TGZ}" --no-same-owner -C "${STAGE}/charts"
[[ -f "${STAGE}/charts/${CHART}/Chart.yaml" ]] || die "${CHART_TGZ} did not unpack to ${CHART}/Chart.yaml"
grep -qx "version: ${VERSION}" "${STAGE}/charts/${CHART}/Chart.yaml" ||
  die "${CHART_TGZ} is not chart version ${VERSION}"
# ovnHelmChartStaged in agent/internal/agent/cni.go keys on this file.
[[ -f "${STAGE}/charts/${CHART}/values-nke.yaml" ]] || die "${CHART_TGZ} has no values-nke.yaml"
# nfpm's `type: tree` copies modes and mtimes from the source files.
find "${STAGE}/charts" -type d -exec chmod 0755 {} +
find "${STAGE}/charts" -type f -exec chmod 0644 {} +
find "${STAGE}/charts" -exec touch -d '2026-01-01 00:00:00Z' {} +

# With values-nke.yaml the chart runs only ovnkube-control-plane and
# ovnkube-single-node-zone, so ovn-c and ovnkube-c are all the lock lists.
stage_images
finish_bundle
