#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Inject the SPIRE exec-auth plugin into CAPI/CAPD controller Deployments.
# Applied after clusterctl init/upgrade. Missing Deployments are skipped so
# capi-core-up (no CAPD) and capd-up (may omit kubeadm CP until topology is on)
# can share this script.
#
# The overlay mounts csi.spiffe.io. On a fresh Kind cluster that CSI driver
# does not exist until Tilt installs SPIRE, so rollout wait is skipped until
# the driver is registered. Re-run after `tilt up` (or on capd-upgrade) to
# wait for Ready.
#
# Usage: ./hack/apply-capi-spiffe-exec-auth.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVERLAY="${ROOT}/deploy/k8s/capi-spiffe-exec-auth"

wait_for_csi=false
if kubectl get csidriver csi.spiffe.io >/dev/null 2>&1; then
  wait_for_csi=true
else
  echo "SPIRE CSI driver not registered yet; patched pods become Ready after tilt up"
fi

patch_if_present() {
  local ns="$1"
  local name="$2"
  local patch="$3"
  if ! kubectl get "deployment/${name}" -n "${ns}" >/dev/null 2>&1; then
    echo "skip ${ns}/${name} (not installed)"
    return 0
  fi
  echo "patching ${ns}/${name}"
  kubectl patch "deployment/${name}" -n "${ns}" --type strategic --patch-file "${patch}"
  if [[ "${wait_for_csi}" == "true" ]]; then
    kubectl rollout status "deployment/${name}" -n "${ns}" --timeout=180s
  fi
}

patch_if_present capi-system capi-controller-manager \
  "${OVERLAY}/capi-controller-manager.yaml"
patch_if_present capi-kubeadm-control-plane-system capi-kubeadm-control-plane-controller-manager \
  "${OVERLAY}/capi-kubeadm-control-plane-controller-manager.yaml"
patch_if_present capi-kubeadm-bootstrap-system capi-kubeadm-bootstrap-controller-manager \
  "${OVERLAY}/capi-kubeadm-bootstrap-controller-manager.yaml"
patch_if_present capd-system capd-controller-manager \
  "${OVERLAY}/capd-controller-manager.yaml"
