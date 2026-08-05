#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Create a sample NKE cluster on the local forge-vms backend (real KVM/libvirt VMs).
#
# Defaults to 1 control-plane node + 1 worker node, using the forge-vms seed
# resources (see forge-vms/internal/seed/default-seed.yaml) and native VPC-prefix
# networking. Every value is overridable via the environment variables below.
#
# Notes:
#   - Defaults to the cpu4-mem6gb seed instance type. The kubelet reservations are
#     fixed (kubeReserved 2Gi via kube-reserved-resources.sh + systemReserved below),
#     so ~3Gi is carved off regardless of node size. On a 4Gi node that leaves only
#     ~700Mi allocatable -> the control plane exceeds it -> kubelet reports
#     MemoryPressure and tags the node NoSchedule (which, among other things, blocks
#     kubeadm's upgrade-health-check Job). 6Gi leaves ~3Gi allocatable, enough headroom.
#   - systemReservedMemory defaults to 1Gi. The CWE API/DB default is 16Gi, which alone
#     exceeds these small seed instance types, so it must be set explicitly here.
#   - The worker pool is always included; set WORKER_DESIRED_COUNT=0 to create a
#     control-plane-only cluster.

set -euo pipefail

CWE_HTTP_PORT="${CWE_HTTP_PORT:-8080}"
API_URL="${API_URL:-http://localhost:${CWE_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

CLUSTER_NAME="${CLUSTER_NAME:-forge-vms}"

# forge-vms seed resources (default-seed.yaml).
ORG_ID="${ORG_ID:-novxwn552ssc}"
SITE_ID="${SITE_ID:-a1b2c3d4-0001-4000-8000-000000000001}"
VPC_ID="${VPC_ID:-a1b2c3d4-0002-4000-8000-000000000001}"
VPC_PREFIX_ID="${VPC_PREFIX_ID:-a1b2c3d4-0004-4000-8000-000000000001}"
INSTANCE_TYPE_ID="${INSTANCE_TYPE_ID:-a1b2c3d4-0005-4000-8000-000000000003}"  # cpu4-mem6gb-disk60gb

KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-1}"

OS_PROFILE="${OS_PROFILE:-nke-260602-24.04-x86-fde}"
CP_CONFIG_PROFILE="${CP_CONFIG_PROFILE:-nvcf-cp-v3}"
WORKER_CONFIG_PROFILE="${WORKER_CONFIG_PROFILE:-nvcf-monitoring-v3}"
SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-1Gi}"
ETCD_ENCRYPTED="${ETCD_ENCRYPTED:-false}"

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "orgID": "${ORG_ID}",
  "siteID": "${SITE_ID}",
  "vpcID": "${VPC_ID}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "calico",
  "etcdEncrypted": ${ETCD_ENCRYPTED},
  "teleportEnabled": false,
  "nodePools": [
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "vpcPrefixIDs": ["${VPC_PREFIX_ID}"],
      "instanceTypeID": "${INSTANCE_TYPE_ID}",
      "osProfile": "${OS_PROFILE}",
      "configProfile": "${CP_CONFIG_PROFILE}",
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
      "vpcPrefixIDs": ["${VPC_PREFIX_ID}"],
      "instanceTypeID": "${INSTANCE_TYPE_ID}",
      "osProfile": "${OS_PROFILE}",
      "configProfile": "${WORKER_CONFIG_PROFILE}",
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

echo "Creating forge-vms cluster '${CLUSTER_NAME}' (cp=${CONTROL_PLANE_DESIRED_COUNT}, worker=${WORKER_DESIRED_COUNT})" >&2

curl -fsS --max-time "${CURL_MAX_TIME:-30}" "${CLUSTERS_API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
