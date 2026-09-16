#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stages the upstream Calico manifests and images for the
# nke-cni-calico-<stream> debs.
#
# Usage: bundle.sh --version v3.30.1 --dist DIR --lock-dir DIR --crane PATH
#                  [--arch amd64] [--write-lock]
#
# The lock and image handling is ../cni-common/bundle-lib.sh.

set -euo pipefail

CNI=cni-calico
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../cni-common/bundle-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../cni-common/bundle-lib.sh"

parse_bundle_args "$@"
[[ "$VERSION" == v* ]] || die "--version must be leading-v, got ${VERSION}"

MANIFEST_BASE="https://raw.githubusercontent.com/projectcalico/calico/${VERSION}/manifests"

# The manifests the agent applies. calicoInstallRootFromFiles in
# agent/internal/agent/cni.go finds the install root by looking for
# manifests/tigera-operator.yaml. runCalicoKubectlApplyFromDir establishes the
# CRDs before it applies the operator and custom resources.
mkdir -p "${STAGE}/manifests"
for name in operator-crds.yaml tigera-operator.yaml custom-resources.yaml; do
  fetch "${MANIFEST_BASE}/${name}" "${STAGE}/manifests/${name}"
  verify_artifact "$name" "${STAGE}/manifests/${name}" "${MANIFEST_BASE}/${name}"
done

# The image list is the lock's, seeded by hand: the manifests name only the
# operator, which resolves the rest at runtime.
stage_images
finish_bundle
