#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stages the upstream ovn-kubernetes chart and image for the nke-cni-ovn-<stream>
# debs.
#
# Usage: bundle.sh --version 1.4.0 --dist DIR --lock-dir DIR --crane PATH
#                  [--arch amd64] [--write-lock]
#
# The lock and image handling is ../cni-common/bundle-lib.sh.

set -euo pipefail

CNI=cni-ovn
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../cni-common/bundle-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../cni-common/bundle-lib.sh"

parse_bundle_args "$@"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.0$ ]] ||
  die "--version must be a bare chart version with patch 0, such as 1.4.0, got ${VERSION}"

REPO="${OVN_KUBERNETES_REPO:-ovn-kubernetes/ovn-kubernetes}"
CHART_TAG="v${VERSION}"

# values-nke.yaml must name the ref stage_images imports, so read it from the
# lock stage_images reads: amd64's when locking another arch.
image_lock="$LOCK"
if [[ "$WRITE_LOCK" == true && "$ARCH" != amd64 ]]; then
  image_lock="${LOCK_DIR}/amd64/${VERSION}.lock"
fi
image_ref="$(awk '$1 == "image" { print $2; exit }' "$image_lock" 2>/dev/null || true)"
[[ "${image_ref##*:}" == "release-${VERSION%.0}" ]] ||
  die "${image_lock} must list 'image <repo>:release-${VERSION%.0}', got '${image_ref}'"

# Upstream publishes no chart repository; the chart is a directory in the source tree.
SRC_TGZ="ovn-kubernetes-src-${CHART_TAG}.tar.gz"
SRC_URL="https://github.com/${REPO}/archive/refs/tags/${CHART_TAG}.tar.gz"
fetch "$SRC_URL" "${WORK}/${SRC_TGZ}"
verify_artifact "$SRC_TGZ" "${WORK}/${SRC_TGZ}" "$SRC_URL"
tar_paths_safe "${WORK}/${SRC_TGZ}"

mkdir -p "${WORK}/src"
tar -xzf "${WORK}/${SRC_TGZ}" --no-same-owner -C "${WORK}/src"
# The archive's top-level directory name is GitHub's, not part of the contract.
chart_src="$(find "${WORK}/src" -mindepth 2 -maxdepth 4 -type d -path '*/helm/ovn-kubernetes' -print -quit)"
[[ -n "$chart_src" && -f "${chart_src}/Chart.yaml" ]] ||
  die "${SRC_TGZ} has no helm/ovn-kubernetes/Chart.yaml"
grep -qx "version: ${VERSION}" "${chart_src}/Chart.yaml" ||
  die "${SRC_TGZ} is not chart version ${VERSION}"

mkdir -p "${STAGE}/charts"
cp -R "$chart_src" "${STAGE}/charts/ovn-kubernetes"

# NVKE's overrides, not upstream's; ovnHelmChartStaged in agent/internal/agent/cni.go
# keys on this file. OVS runs on the host (checkHostOpenVSwitchReady), so the
# ovs-node subchart is tagged off.
cat >"${STAGE}/charts/ovn-kubernetes/values-nke.yaml" <<EOF
tags:
  ovs-node: false
global:
  image:
    repository: ${image_ref%:*}
    tag: ${image_ref##*:}
EOF

# nfpm's `type: tree` copies modes and mtimes from the source files.
find "${STAGE}/charts" -type d -exec chmod 0755 {} +
find "${STAGE}/charts" -type f -exec chmod 0644 {} +
find "${STAGE}/charts" -exec touch -d '2026-01-01 00:00:00Z' {} +

stage_images
finish_bundle
