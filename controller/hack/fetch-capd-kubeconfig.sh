#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# CAPD workload kubeconfig for local Mac/Colima: CWE uses namespace cluster-<name>
# (internal/capi/naming.go). Pulls admin.conf from a CAPD control-plane container, or
# Secret <name>-kubeconfig when present. Rewrites apiserver to 127.0.0.1:<docker-published 6443>
# (<name>-lb first, else the same control-plane container).
#
# Usage: ./hack/fetch-capd-kubeconfig.sh [CLUSTER_NAME] [OUTPUT_FILE]
#   Defaults: CLUSTER_NAME=capd, OUTPUT_FILE=workload-<CLUSTER_NAME>.kubeconfig
#   SKIP_LB_PATCH=1 — do not rewrite server URL.

set -euo pipefail

CLUSTER_NAME="${1:-capd}"
OUTPUT_FILE="${2:-workload-${CLUSTER_NAME}.kubeconfig}"
CLUSTER_NAMESPACE="cluster-${CLUSTER_NAME}"
TEMP_FILE="$(mktemp)"
trap 'rm -f "${TEMP_FILE}"' EXIT

CONTROL_PLANE_CONTAINER_ID="$(docker ps -qf "label=io.x-k8s.kind.cluster=${CLUSTER_NAME}" -f "label=io.x-k8s.kind.role=control-plane" | head -1)"
[[ -n "${CONTROL_PLANE_CONTAINER_ID}" ]] || {
  echo "No CAPD control-plane container with io.x-k8s.kind.cluster=${CLUSTER_NAME}" >&2
  exit 1
}

if kubectl get secret "${CLUSTER_NAME}-kubeconfig" -n "${CLUSTER_NAMESPACE}" -o jsonpath='{.data.value}' 2>/dev/null | base64 -d >"${TEMP_FILE}" \
  && grep -q '^apiVersion:' "${TEMP_FILE}"; then
  :
else
  docker exec "${CONTROL_PLANE_CONTAINER_ID}" cat /etc/kubernetes/admin.conf >"${TEMP_FILE}"
fi

if [[ "${SKIP_LB_PATCH:-}" == "1" ]]; then
  cp "${TEMP_FILE}" "${OUTPUT_FILE}"
  echo "Wrote ${OUTPUT_FILE}"
  exit 0
fi

APISERVER_HOST_PORT="$(docker port "${CLUSTER_NAME}-lb" 6443/tcp 2>/dev/null | head -1 | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p')"
[[ -n "${APISERVER_HOST_PORT}" ]] || APISERVER_HOST_PORT="$(docker port "${CONTROL_PLANE_CONTAINER_ID}" 6443/tcp 2>/dev/null | head -1 | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p')"
if [[ -z "${APISERVER_HOST_PORT}" ]]; then
  cp "${TEMP_FILE}" "${OUTPUT_FILE}"
  echo "Wrote ${OUTPUT_FILE} (could not read docker port 6443; apiserver URL unchanged)" >&2
  exit 0
fi

KUBECONFIG_CLUSTER_NAME="$(kubectl --kubeconfig="${TEMP_FILE}" config view -o jsonpath='{.clusters[0].name}')"
SERVER_URL="$(kubectl --kubeconfig="${TEMP_FILE}" config view -o jsonpath='{.clusters[0].cluster.server}')"
[[ "${SERVER_URL}" == https://* ]] || {
  echo "Unexpected server in kubeconfig: ${SERVER_URL}" >&2
  exit 1
}
TLS_SERVER_NAME="${SERVER_URL#https://}"
TLS_SERVER_NAME="${TLS_SERVER_NAME%%/*}"
TLS_SERVER_NAME="${TLS_SERVER_NAME%%:*}"

kubectl --kubeconfig="${TEMP_FILE}" config set-cluster "${KUBECONFIG_CLUSTER_NAME}" --server="https://127.0.0.1:${APISERVER_HOST_PORT}" >/dev/null
kubectl --kubeconfig="${TEMP_FILE}" config set-cluster "${KUBECONFIG_CLUSTER_NAME}" --tls-server-name="${TLS_SERVER_NAME}" >/dev/null
cp "${TEMP_FILE}" "${OUTPUT_FILE}"
echo "Wrote ${OUTPUT_FILE} — kubectl --kubeconfig=\"${OUTPUT_FILE}\" get nodes"
