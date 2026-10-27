#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# The control plane is cluster-level controlPlaneConfig, not a node pool.
# The controller rejects control-plane node pools when control-plane controller
# provisioning is enabled, which is the default. More than one control-plane
# node requires controlPlaneVIP unless the dev override is enabled in both this
# helper and the NVKE controller. The count defaults to 1 unless kube-vip is on.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The helper path is resolved from this script, independent of the caller's working directory.
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/control-plane-vip.sh"
if [[ -n "${API_URL:-}" ]]; then
  CLUSTERS_API_URL="${API_URL%/}"
  API_BASE_URL="${API_BASE_URL:-${CLUSTERS_API_URL%/clusters}}"
else
  API_BASE_URL="${API_BASE_URL:-http://localhost:8080/api}"
  CLUSTERS_API_URL="${API_BASE_URL%/}/clusters"
fi
API_URL="${CLUSTERS_API_URL}"

# /api/* is authenticated. hack/nvke-token.sh prints nothing when NVKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nvke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi
CLUSTER_NAME="${CLUSTER_NAME:-capd}"
DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
CONTROL_PLANE_VIP="${CONTROL_PLANE_VIP:-}"
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.1.0/24}"
FIRST_CONTROL_PLANE_VIP="${FIRST_CONTROL_PLANE_VIP:-172.31.1.1}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
# CNI defaults to ovn-kubernetes. Set CNI_PLUGIN=calico to use Calico instead.
CNI_PLUGIN="${CNI_PLUGIN:-ovn-kubernetes}"
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

ALLOW_CONTROL_PLANE_WITHOUT_VIP="${ALLOW_CONTROL_PLANE_WITHOUT_VIP:-false}"
case "${ALLOW_CONTROL_PLANE_WITHOUT_VIP}" in
  true|false) ;;
  *) die "ALLOW_CONTROL_PLANE_WITHOUT_VIP must be true or false" ;;
esac

if [[ "${CAPD_ENABLE_KUBE_VIP}" == "true" ]]; then
  CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-3}"
  if [[ -z "${CONTROL_PLANE_VIP}" ]]; then
    CLUSTERS="$(curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -fsS --max-time "${CURL_MAX_TIME:-30}" "${CLUSTERS_API_URL}")"
    CONTROL_PLANE_VIP="$(
      first_available_control_plane_vip "${DEFAULT_VIP_RANGE}" "${CLUSTERS}" "${FIRST_CONTROL_PLANE_VIP}"
    )"
  fi
  CONTROL_PLANE_VIP_FIELD=$(printf ',\n  "controlPlaneVIP": "%s"' "${CONTROL_PLANE_VIP}")
  echo "Creating CAPD cluster with kube-vip enabled: controlPlaneVIP=${CONTROL_PLANE_VIP} controlPlane=${CONTROL_PLANE_DESIRED_COUNT}" >&2
else
  CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
  if [[ "${CONTROL_PLANE_DESIRED_COUNT}" -gt 1 && "${ALLOW_CONTROL_PLANE_WITHOUT_VIP}" != "true" ]]; then
    die "a control plane of ${CONTROL_PLANE_DESIRED_COUNT} nodes requires controlPlaneVIP; start bgp-containers, set CAPD_ENABLE_KUBE_VIP=true, set CONTROL_PLANE_DESIRED_COUNT=1, or enable allowControlPlaneWithoutVIP in the controller and set ALLOW_CONTROL_PLANE_WITHOUT_VIP=true"
  fi
  CONTROL_PLANE_VIP_FIELD=""
  echo "Creating CAPD cluster with kube-vip disabled: controlPlane=${CONTROL_PLANE_DESIRED_COUNT}. Start bgp-containers or set CAPD_ENABLE_KUBE_VIP=true to add a VIP." >&2
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "${CNI_PLUGIN}",
  "etcdEncrypted": ${ETCD_ENCRYPTED}${CONTROL_PLANE_VIP_FIELD},
  "controlPlaneConfig": {
    "desiredCount": ${CONTROL_PLANE_DESIRED_COUNT},
    "systemReservedMemory": "1Gi",
    "infra": {
      "machineTemplate": {
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
      "preKubeadmCommands": [
        "mkdir -p /run/cluster-api",
        "echo success > /run/cluster-api/bootstrap-success.complete"
      ]
    }
  },
  "infra": {
    "cluster": {
      "apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
      "kind": "DockerCluster",
      "metadata": {
        "name": "${CLUSTER_NAME}"
      },
      "spec": {}
    }
  },
  "nodePools": [
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "systemReservedMemory": "1Gi",
      "desiredNodeCount": ${WORKER_DESIRED_COUNT},
      "controlPlane": false,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "infra": {
        "machineTemplate": {
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
        "preKubeadmCommands": [
          "mkdir -p /run/cluster-api",
          "echo success > /run/cluster-api/bootstrap-success.complete"
        ]
      }
    }
  ]
}
EOF
)

# curl without --fail exits 0 on HTTP 400, and --fail hides the body.
create_response="$(
  curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -sS --max-time "${CURL_MAX_TIME:-30}" \
    -w '\n%{http_code}' \
    "${API_URL}" \
    -H "Content-Type: application/json" \
    --data-raw "${BODY}"
)"
create_status="${create_response##*$'\n'}"
create_body="${create_response%$'\n'*}"
if ! printf '%s\n' "${create_body}" | jq; then
  printf '%s\n' "${create_body}"
fi
if [[ ! "${create_status}" =~ ^2[0-9][0-9]$ ]]; then
  die "create failed with HTTP ${create_status}"
fi
