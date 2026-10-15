#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# CWE_HTTP_PORT is a deprecated alias for NKE_CONTROLLER_HTTP_PORT.
NKE_CONTROLLER_HTTP_PORT="${NKE_CONTROLLER_HTTP_PORT:-${CWE_HTTP_PORT:-8080}}"
API_URL="${API_URL:-http://localhost:${NKE_CONTROLLER_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

# /api/* is authenticated. hack/nvke-token.sh prints nothing when NVKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nvke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

CLUSTER_NAME="${CLUSTER_NAME:-forge}"
ORG_ID="${ORG_ID:-local-dev-org}"
MOCK_SITE_ID="${MOCK_SITE_ID:-00000000-0000-0000-0000-000000000001}"
MOCK_VPC_ID="${MOCK_VPC_ID:-00000000-0000-0000-0000-000000000002}"
MOCK_SUBNET_ID="${MOCK_SUBNET_ID:-00000000-0000-0000-0000-000000000003}"
MOCK_SSH_KEY_GROUP_ID="${MOCK_SSH_KEY_GROUP_ID:-00000000-0000-0000-0000-000000000004}"
MOCK_INSTANCE_TYPE_ID="${MOCK_INSTANCE_TYPE_ID:-00000000-0000-0000-0000-000000000005}"

KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
# CNI defaults to ovn-kubernetes. Set CNI_PLUGIN=calico to use Calico instead.
CNI_PLUGIN="${CNI_PLUGIN:-ovn-kubernetes}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-0}"
RELEASE="${RELEASE:-nvke-local-dev-kindest-arm64}"
CONFIG_PROFILE="${CONFIG_PROFILE:-dev-only-empty}"
SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-1Gi}"

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "nico": {
    "orgID": "${ORG_ID}",
    "siteID": "${MOCK_SITE_ID}",
    "vpcID": "${MOCK_VPC_ID}"
  },
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "${CNI_PLUGIN}",
  "etcdEncrypted": false,
  "teleportEnabled": false,
  "nodePools": [
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "nico": {
        "subnetIDs": ["${MOCK_SUBNET_ID}"],
        "instanceTypeID": "${MOCK_INSTANCE_TYPE_ID}",
        "sshKeyGroupIDs": ["${MOCK_SSH_KEY_GROUP_ID}"],
        "release": "${RELEASE}",
        "configProfile": "${CONFIG_PROFILE}"
      },
      "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
      "controlPlane": true,
      "desiredNodeCount": ${CONTROL_PLANE_DESIRED_COUNT},
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "teleportEnabled": false
    },
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "nico": {
        "subnetIDs": ["${MOCK_SUBNET_ID}"],
        "instanceTypeID": "${MOCK_INSTANCE_TYPE_ID}",
        "sshKeyGroupIDs": ["${MOCK_SSH_KEY_GROUP_ID}"],
        "release": "${RELEASE}",
        "configProfile": "${CONFIG_PROFILE}"
      },
      "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
      "controlPlane": false,
      "desiredNodeCount": ${WORKER_DESIRED_COUNT},
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "teleportEnabled": false
    }
  ]
}
EOF
)

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -sS "${CLUSTERS_API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
