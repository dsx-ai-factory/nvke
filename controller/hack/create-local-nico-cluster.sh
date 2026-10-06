#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# CWE_HTTP_PORT is a deprecated alias for NKE_CONTROLLER_HTTP_PORT.
NKE_CONTROLLER_HTTP_PORT="${NKE_CONTROLLER_HTTP_PORT:-${CWE_HTTP_PORT:-8080}}"
API_URL="${API_URL:-http://localhost:${NKE_CONTROLLER_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

CLUSTER_NAME="${CLUSTER_NAME:-nico-local}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-0}"

DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
NICO_SITE_ID="${NICO_SITE_ID:-00000000-0000-0000-0000-000000000001}"
NICO_VPC_ID="${NICO_VPC_ID:-00000000-0000-0000-0000-000000000002}"
NICO_SUBNET_ID="${NICO_SUBNET_ID:-00000000-0000-0000-0000-000000000003}"
NICO_SSH_KEY_GROUP_ID="${NICO_SSH_KEY_GROUP_ID:-00000000-0000-0000-0000-000000000004}"
NICO_INSTANCE_TYPE_ID="${NICO_INSTANCE_TYPE_ID:-00000000-0000-0000-0000-000000000005}"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "calico",
  "etcdEncrypted": false,
  "infra": {
    "cluster": {
      "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
      "kind": "NicoCluster",
      "metadata": {
        "name": "${CLUSTER_NAME}"
      },
      "spec": {
        "siteID": "${NICO_SITE_ID}"
      }
    }
  },
  "controlPlaneConfig": {
    "desiredCount": ${CONTROL_PLANE_DESIRED_COUNT},
    "systemReservedMemory": "1Gi",
    "infra": {
        "machineTemplate": {
          "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
          "kind": "NicoMachineTemplate",
          "metadata": {
            "name": "${CLUSTER_NAME}-control-plane-template"
          },
          "spec": {
            "template": {
              "spec": {
                "vpcID": "${NICO_VPC_ID}",
                "instanceTypeID": "${NICO_INSTANCE_TYPE_ID}",
                "cloudInitInjectHostname": true,
                "ipxeScript": "#!ipxe\\necho local-nico-control-plane\\n",
                "sshKeyGroupIDs": [
                  "${NICO_SSH_KEY_GROUP_ID}"
                ],
                "interfaces": [
                  {
                    "subnetID": "${NICO_SUBNET_ID}"
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
          "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
          "kind": "NicoMachineTemplate",
          "metadata": {
            "name": "${CLUSTER_NAME}-worker-template"
          },
          "spec": {
            "template": {
              "spec": {
                "vpcID": "${NICO_VPC_ID}",
                "instanceTypeID": "${NICO_INSTANCE_TYPE_ID}",
                "cloudInitInjectHostname": true,
                "ipxeScript": "#!ipxe\\necho local-nico-worker\\n",
                "sshKeyGroupIDs": [
                  "${NICO_SSH_KEY_GROUP_ID}"
                ],
                "interfaces": [
                  {
                    "subnetID": "${NICO_SUBNET_ID}"
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

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -sS "${CLUSTERS_API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
