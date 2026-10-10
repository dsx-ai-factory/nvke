#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Create a sample NKE cluster on the local nico-vms backend (real KVM/libvirt VMs).
#
# Defaults to 1 control-plane node + 1 worker node, using the nico-vms seed
# resources (see nico-vms/internal/seed/default-seed.yaml) and native VPC-prefix
# networking. Every value is overridable via the environment variables below.
#
# Notes:
#   - Defaults to the cpu4-mem6gb seed instance type. The kubelet reservations are
#     fixed by the controller's generated kubeadm patch (kubeReserved 2Gi plus the
#     systemReservedMemory below),
#     so ~3Gi is carved off regardless of node size. On a 4Gi node that leaves only
#     ~700Mi allocatable -> the control plane exceeds it -> kubelet reports
#     MemoryPressure and tags the node NoSchedule (which, among other things, blocks
#     kubeadm's upgrade-health-check Job). 6Gi leaves ~3Gi allocatable, enough headroom.
#   - systemReservedMemory defaults to 1Gi. The controller API/DB default is 16Gi, which alone
#     exceeds these small seed instance types, so it must be set explicitly here.
#   - The control plane is cluster-level controlPlaneConfig, not a node pool.
#     The controller rejects control-plane node pools when control-plane controller
#     provisioning is enabled, which is the default.
#   - The worker pool is always included; set WORKER_DESIRED_COUNT=0 to create a
#     control-plane-only cluster.
#   - CNI defaults to ovn-kubernetes. Set CNI_PLUGIN=calico to use Calico instead.
#     CNI_VERSION is optional; when empty the controller API applies the plugin default
#     (1.4.0 for OVN, 3.30.1 for Calico).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The helper path is resolved from this script, independent of the caller's working directory.
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/control-plane-vip.sh"

# CWE_HTTP_PORT is a deprecated alias for NKE_CONTROLLER_HTTP_PORT.
NKE_CONTROLLER_HTTP_PORT="${NKE_CONTROLLER_HTTP_PORT:-${CWE_HTTP_PORT:-8080}}"
API_URL="${API_URL:-http://localhost:${NKE_CONTROLLER_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

CLUSTER_NAME="${CLUSTER_NAME:-nico-vms}"

# nico-vms seed resources (default-seed.yaml).
ORG_ID="${ORG_ID:-novxwn552ssc}"
SITE_ID="${SITE_ID:-a1b2c3d4-0001-4000-8000-000000000001}"
VPC_ID="${VPC_ID:-a1b2c3d4-0002-4000-8000-000000000001}"
VPC_PREFIX_ID="${VPC_PREFIX_ID:-a1b2c3d4-0004-4000-8000-000000000001}"
INSTANCE_TYPE_ID="${INSTANCE_TYPE_ID:-a1b2c3d4-0005-4000-8000-000000000003}"  # cpu4-mem6gb-disk60gb

KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-1}"
CONTROL_PLANE_VIP="${CONTROL_PLANE_VIP:-}"
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.0.0/24}"
FIRST_CONTROL_PLANE_VIP="${FIRST_CONTROL_PLANE_VIP:-172.31.0.10}"
CNI_PLUGIN="${CNI_PLUGIN:-ovn-kubernetes}"
CNI_VERSION="${CNI_VERSION:-}"

case "$(uname -m)" in
  x86_64) PROFILE_ARCH=amd64 ;;
  aarch64 | arm64) PROFILE_ARCH=arm64 ;;
  *)
    echo "Unsupported host architecture for nico-vms: $(uname -m)" >&2
    exit 1
    ;;
esac
OS_PROFILE="${OS_PROFILE:-nke-nico-vms-24.04-${PROFILE_ARCH}-fde}"
CP_CONFIG_PROFILE="${CP_CONFIG_PROFILE:-nico-vms-cp}"
WORKER_CONFIG_PROFILE="${WORKER_CONFIG_PROFILE:-nico-vms-worker}"
SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-1Gi}"
ETCD_ENCRYPTED="${ETCD_ENCRYPTED:-false}"

CNI_VERSION_JSON=""
if [[ -n "${CNI_VERSION}" ]]; then
  CNI_VERSION_JSON=",
  \"cniVersion\": \"${CNI_VERSION}\""
fi

if [[ -z "${CONTROL_PLANE_VIP}" ]]; then
  CLUSTERS="$(curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -fsS --max-time "${CURL_MAX_TIME:-30}" "${CLUSTERS_API_URL}")"
  CONTROL_PLANE_VIP="$(
    first_available_control_plane_vip "${DEFAULT_VIP_RANGE}" "${CLUSTERS}" "${FIRST_CONTROL_PLANE_VIP}"
  )"
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "nico": {
    "orgID": "${ORG_ID}",
    "siteID": "${SITE_ID}",
    "vpcID": "${VPC_ID}"
  },
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "controlPlaneVIP": "${CONTROL_PLANE_VIP}",
  "cniPlugin": "${CNI_PLUGIN}"${CNI_VERSION_JSON},
  "etcdEncrypted": ${ETCD_ENCRYPTED},
  "teleportEnabled": false,
  "controlPlaneConfig": {
    "desiredCount": ${CONTROL_PLANE_DESIRED_COUNT},
    "systemReservedMemory": "${SYSTEM_RESERVED_MEMORY}",
    "teleportEnabled": false,
    "nico": {
      "instanceTypePreference": ["${INSTANCE_TYPE_ID}"],
      "vpcPrefixIDs": ["${VPC_PREFIX_ID}"],
      "osProfile": "${OS_PROFILE}",
      "configProfile": "${CP_CONFIG_PROFILE}"
    }
  },
  "nodePools": [
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "nico": {
        "vpcPrefixIDs": ["${VPC_PREFIX_ID}"],
        "instanceTypeID": "${INSTANCE_TYPE_ID}",
        "osProfile": "${OS_PROFILE}",
        "configProfile": "${WORKER_CONFIG_PROFILE}"
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

echo "Creating nico-vms cluster '${CLUSTER_NAME}' (cp=${CONTROL_PLANE_DESIRED_COUNT}, worker=${WORKER_DESIRED_COUNT}, cni=${CNI_PLUGIN}${CNI_VERSION:+/${CNI_VERSION}}, controlPlaneVIP=${CONTROL_PLANE_VIP})" >&2

# curl without --fail exits 0 on HTTP 400 and 401, and --fail hides the body.
# Keep the body visible, then fail the script from the status.
create_response="$(
  curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -sS --max-time "${CURL_MAX_TIME:-30}" \
    -w '\n%{http_code}' \
    "${CLUSTERS_API_URL}" \
    -H "Content-Type: application/json" \
    --data-raw "${BODY}"
)"
create_status="${create_response##*$'\n'}"
create_body="${create_response%$'\n'*}"
if ! printf '%s\n' "${create_body}" | jq; then
  printf '%s\n' "${create_body}"
fi
if [[ ! "${create_status}" =~ ^2[0-9][0-9]$ ]]; then
  echo "create-nico-vms-cluster: create failed with HTTP ${create_status}" >&2
  exit 1
fi
