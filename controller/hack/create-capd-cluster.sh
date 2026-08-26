#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -n "${API_URL:-}" ]]; then
  CLUSTERS_API_URL="${API_URL%/}"
  API_BASE_URL="${API_BASE_URL:-${CLUSTERS_API_URL%/clusters}}"
else
  API_BASE_URL="${API_BASE_URL:-http://localhost:8080/api}"
  CLUSTERS_API_URL="${API_BASE_URL%/}/clusters"
fi
API_URL="${CLUSTERS_API_URL}"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi
CLUSTER_NAME="${CLUSTER_NAME:-capd}"
DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
# Distinct from nico-vms NICO_VMS_VIP_CIDR (172.31.0.0/24).
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.1.0/24}"
CONTROL_PLANE_VIP="${CONTROL_PLANE_VIP:-172.31.1.1}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-3}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-1}"
ETCD_ENCRYPTED="${ETCD_ENCRYPTED:-false}"
# DockerMachine base image (build with: make capd-image). Same default as CAPD_KINDEST_NODE_IMAGE in Makefile.dev.
CAPD_KINDEST_NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE:-cwe-capd-kindest-node:local}"
BGP_CONTAINERS_CONTAINER_NAME="${BGP_CONTAINERS_CONTAINER_NAME:-bgp-containers}"

die() { echo "[create-capd-cluster] ERROR: $*" >&2; exit 1; }

detect_bgp_containers() {
  if command -v docker >/dev/null 2>&1 && \
      docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "${BGP_CONTAINERS_CONTAINER_NAME}"; then
    echo true
  else
    echo false
  fi
}

case "${CAPD_ENABLE_KUBE_VIP:-}" in
  "") CAPD_ENABLE_KUBE_VIP="$(detect_bgp_containers)" ;;
  true|false) ;;
  *) die "CAPD_ENABLE_KUBE_VIP must be true or false" ;;
esac

if [[ "${CAPD_ENABLE_KUBE_VIP}" == "true" ]]; then
  CONTROL_PLANE_VIP_FIELD=$(printf ',\n  "controlPlaneVIP": "%s"' "${CONTROL_PLANE_VIP}")
else
  CONTROL_PLANE_VIP_FIELD=""
fi

if [[ "${REGISTER_DEFAULT_SITE:-true}" == "true" ]]; then
  API_URL="${API_BASE_URL}" \
    DEFAULT_SITE_ID="${DEFAULT_SITE_ID}" \
    DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE}" \
    "${SCRIPT_DIR}/register-default-site.sh" >/dev/null
fi

if [[ "${CAPD_ENABLE_KUBE_VIP}" == "true" ]]; then
  echo "Creating CAPD cluster with kube-vip enabled: vipRange=${DEFAULT_VIP_RANGE}, controlPlaneVIP=${CONTROL_PLANE_VIP}" >&2
else
  echo "Creating CAPD cluster with kube-vip disabled; start bgp-containers first or set CAPD_ENABLE_KUBE_VIP=true to enable it." >&2
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "siteID": "${DEFAULT_SITE_ID}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "calico",
  "etcdEncrypted": ${ETCD_ENCRYPTED}${CONTROL_PLANE_VIP_FIELD},
  "nodePools": [
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "systemReservedMemory": "1Gi",
      "desiredNodeCount": ${CONTROL_PLANE_DESIRED_COUNT},
      "controlPlane": true,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "capiInfraMachineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
        "kind": "DockerMachineTemplate",
        "spec": {
          "template": {
            "spec": {
              "customImage": "${CAPD_KINDEST_NODE_IMAGE}",
              "extraMounts": [
                {
                  "containerPath": "/var/run/docker.sock",
                  "hostPath": "/var/run/docker.sock"
                }
              ]
            }
          }
        }
      },
      "capiSpireAttestationMethod": "joinToken",
      "capiPreKubeadmCommands": [
        "mkdir -p /run/cluster-api",
        "echo success > /run/cluster-api/bootstrap-success.complete"
      ]
    },
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "systemReservedMemory": "1Gi",
      "desiredNodeCount": ${WORKER_DESIRED_COUNT},
      "controlPlane": false,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "capiInfraMachineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
        "kind": "DockerMachineTemplate",
        "spec": {
          "template": {
            "spec": {
              "customImage": "${CAPD_KINDEST_NODE_IMAGE}",
              "extraMounts": [
                {
                  "containerPath": "/var/run/docker.sock",
                  "hostPath": "/var/run/docker.sock"
                }
              ]
            }
          }
        }
      },
      "capiSpireAttestationMethod": "joinToken",
      "capiPreKubeadmCommands": [
        "mkdir -p /run/cluster-api",
        "echo success > /run/cluster-api/bootstrap-success.complete"
      ]
    }
  ],
  "capiInfraCluster": {
    "apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
    "kind": "DockerCluster",
    "metadata": {
      "name": "${CLUSTER_NAME}"
    },
    "spec": {}
  }
}
EOF
)

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -fsS --max-time "${CURL_MAX_TIME:-30}" "${API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
