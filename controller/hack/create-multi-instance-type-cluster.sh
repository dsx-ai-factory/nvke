#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Local-only helper to create a control-plane cluster spanning two real instance
# types to exercise instance-type selection + in-place rotation.
# Creates a Forge CAPI cluster whose control-plane preference is
# [constrained, available]: a scale-0 control-plane node pool contributes the
# highest-preference type and a second control-plane pool (count 1) contributes
# the fallback type. Both types are registered with the local nico-mock up front
# under distinct display names so they can be told apart in the API/UI:
#   - instance type 1 ("mock-instance-type-1", preference[0]): registered with
#     zero capacity (unusedUsable=0), so capnico's preflight reports
#     InstanceTypeUnavailable.
#   - instance type 2 ("mock-instance-type-2", preference[1]): registered with
#     capacity, so CWE can rotate the NicoMachine here and provision.
# The single type-less CP node seeds preference[0], capnico reports Unavailable,
# and CWE rotates the NicoMachine in place to preference[1] and provisions there.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CWE_HTTP_PORT="${CWE_HTTP_PORT:-8080}"
API_URL="${API_URL:-http://localhost:${CWE_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

# nico-mock admin endpoint used to register the two instance types below.
# Tilt already port-forwards the in-cluster mock Service to localhost:18080
# (see hack/nico-provider/Tiltfile, port_forwards=["18080:8080"]), so the
# default works as long as Tilt is up. If you run without Tilt, port-forward it
# yourself: kubectl -n nico-mock port-forward svc/mock 18080:8080
NICO_MOCK_URL="${NICO_MOCK_URL:-http://localhost:18080}"

CLUSTER_NAME="${CLUSTER_NAME:-rot}"
ORG_ID="${ORG_ID:-local-dev-org}"
MOCK_SITE_ID="${MOCK_SITE_ID:-00000000-0000-0000-0000-000000000001}"
MOCK_VPC_ID="${MOCK_VPC_ID:-00000000-0000-0000-0000-000000000002}"
MOCK_SUBNET_ID="${MOCK_SUBNET_ID:-00000000-0000-0000-0000-000000000003}"
MOCK_SSH_KEY_GROUP_ID="${MOCK_SSH_KEY_GROUP_ID:-00000000-0000-0000-0000-000000000004}"
# instance type 1 / preference[0]: a real, registered instance type with zero
# capacity. capnico's preflight (GetInstanceTypeWithAllocationStats) sees
# unusedUsable==0 and marks the NicoMachine Ready=False with reason
# InstanceTypeUnavailable.
UNAVAILABLE_INSTANCE_TYPE_ID="${UNAVAILABLE_INSTANCE_TYPE_ID:-00000000-0000-0000-0000-0000000000aa}"
# instance type 2 / preference[1]: a real, registered instance type with
# capacity, so CWE can rotate the NicoMachine here and provision.
AVAILABLE_INSTANCE_TYPE_ID="${AVAILABLE_INSTANCE_TYPE_ID:-00000000-0000-0000-0000-0000000000bb}"

# Distinct display names for the two types so they are distinguishable in the
# nico-mock API/UI. instance type 1 is the constrained preference[0]; instance
# type 2 is the available preference[1]. Both are env-overridable.
INSTANCE_TYPE_1_NAME="${INSTANCE_TYPE_1_NAME:-mock-instance-type-1}"
INSTANCE_TYPE_2_NAME="${INSTANCE_TYPE_2_NAME:-mock-instance-type-2}"

KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
OS_PROFILE="${OS_PROFILE:-nke-local-dev-kindest-arm64}"
CONFIG_PROFILE="${CONFIG_PROFILE:-dev-only-empty}"
SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-1Gi}"

# Register the two control-plane instance types with the local nico-mock before
# creating the cluster. preference[0] starts at zero capacity
# (InstanceTypeUnavailable); preference[1] starts with capacity. Set
# BOTH_UNAVAILABLE=1 to also start preference[1] at zero for the convergence
# demo documented at the bottom of this script.
UNAVAILABLE_UNUSED_USABLE=0
AVAILABLE_UNUSED_USABLE=1
if [[ "${BOTH_UNAVAILABLE:-0}" == "1" ]]; then
  AVAILABLE_UNUSED_USABLE=0
fi

register_instance_type() {
  local id="$1" unused_usable="$2" name="$3"
  # Use curl -G with --data-urlencode so the display name is safely URL-encoded
  # (the query string is appended to the URL; -X PUT keeps the PUT method).
  curl -sS -f -G -X PUT "${NICO_MOCK_URL%/}/admin/instance-type/${id}" \
    --data-urlencode "unusedUsable=${unused_usable}" \
    --data-urlencode "name=${name}" >/dev/null
  echo "registered instance type ${id} (unusedUsable=${unused_usable}, name=${name})" >&2
}

# instance type 1 (constrained, preference[0]) and instance type 2 (available,
# preference[1]), each registered under its own display name.
register_instance_type "${UNAVAILABLE_INSTANCE_TYPE_ID}" "${UNAVAILABLE_UNUSED_USABLE}" "${INSTANCE_TYPE_1_NAME}"
register_instance_type "${AVAILABLE_INSTANCE_TYPE_ID}" "${AVAILABLE_UNUSED_USABLE}" "${INSTANCE_TYPE_2_NAME}"

# Node pool -> instance type mapping:
#   - "cp-constrained" (scale 0) contributes instance type 1 (constrained,
#     preference[0], UNAVAILABLE_INSTANCE_TYPE_ID / INSTANCE_TYPE_1_NAME).
#   - "control-plane" (count 1) and "worker" use instance type 2 (available,
#     preference[1], AVAILABLE_INSTANCE_TYPE_ID / INSTANCE_TYPE_2_NAME).
BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "nico": {
    "orgID": "${ORG_ID}",
    "siteID": "${MOCK_SITE_ID}",
    "vpcID": "${MOCK_VPC_ID}"
  },
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "calico",
  "etcdEncrypted": false,
  "teleportEnabled": false,
  "nodePools": [
    {
      "name": "cp-constrained",
      "nodeNamePrefix": "cpc",
      "nico": {
        "subnetIDs": ["${MOCK_SUBNET_ID}"],
        "instanceTypeID": "${UNAVAILABLE_INSTANCE_TYPE_ID}",
        "sshKeyGroupIDs": ["${MOCK_SSH_KEY_GROUP_ID}"],
        "osProfile": "${OS_PROFILE}",
        "configProfile": "${CONFIG_PROFILE}"
      },
      "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
      "controlPlane": true,
      "desiredNodeCount": 0,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "teleportEnabled": false
    },
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "nico": {
        "subnetIDs": ["${MOCK_SUBNET_ID}"],
        "instanceTypeID": "${AVAILABLE_INSTANCE_TYPE_ID}",
        "sshKeyGroupIDs": ["${MOCK_SSH_KEY_GROUP_ID}"],
        "osProfile": "${OS_PROFILE}",
        "configProfile": "${CONFIG_PROFILE}"
      },
      "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
      "controlPlane": true,
      "desiredNodeCount": 1,
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
        "instanceTypeID": "${AVAILABLE_INSTANCE_TYPE_ID}",
        "sshKeyGroupIDs": ["${MOCK_SSH_KEY_GROUP_ID}"],
        "osProfile": "${OS_PROFILE}",
        "configProfile": "${CONFIG_PROFILE}"
      },
      "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
      "controlPlane": false,
      "desiredNodeCount": 0,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "teleportEnabled": false
    }
  ]
}
EOF
)

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" --fail-with-body -sS -X POST "${CLUSTERS_API_URL}" \
  -H 'Content-Type: application/json' \
  -d "${BODY}"

# Convergence demo (optional): to watch the NicoMachine converge back to the
# preferred type in place (no recycle), start with BOTH preferences unavailable
# so the CP node cannot provision and cycles Ready=False:
#
#   BOTH_UNAVAILABLE=1 ./hack/create-multi-instance-type-cluster.sh
#
# then bring the preferred type (preference[0]) back to capacity and watch the
# NicoMachine settle on it without a recycle:
#
#   curl -sS -X PUT "${NICO_MOCK_URL}/admin/instance-type/${UNAVAILABLE_INSTANCE_TYPE_ID}?unusedUsable=32"
#
# Observe the rotation via the NicoMachine Spec.InstanceTypeID + Ready condition
# and the CWE node's instance_type.
