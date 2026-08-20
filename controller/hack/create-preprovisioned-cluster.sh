#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Create a cluster on hosts that already exist, with CWE in generic-CAPI mode.
#
# The hosts are on, the operating system is installed, the network has already
# addressed them, and each one runs the node agent. Nothing here creates
# infrastructure: the inventory travels inside the capiInfraCluster manifest,
# and the preprovisioned provider gives one host to each machine.
#
# Install the provider CRDs before you run this:
#   kubectl apply -f controller/preprovisioned/config/crd/

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

CLUSTER_NAME="${CLUSTER_NAME:-bootstrap}"
# A generic-CAPI cluster uses the nil UUID as its site. The site row must still
# exist, because the provisioning controllers iterate registered sites.
DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.1.0/24}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
ETCD_ENCRYPTED="${ETCD_ENCRYPTED:-false}"
CNI_PLUGIN="${CNI_PLUGIN:-calico}"

# Inventory: "name=address,name=address,...". Give the addresses the network
# already assigned. An entry may carry a role: "name=address=control-plane".
PREPROVISIONED_HOSTS="${PREPROVISIONED_HOSTS:-}"

# Nodes to ask for. Keep the total at or below the number of hosts.
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-0}"

# A preprovisioned host booted before CWE existed, so no cloud-init installed
# SPIRE material and nothing reads the bootstrap data secret. "none" turns
# SPIFFE off for these nodes and reaches the node agent unauthenticated. Never
# use it for a tenant cluster.
SPIRE_ATTESTATION_METHOD="${SPIRE_ATTESTATION_METHOD:-none}"

# Optional. CWE configures kube-vip on the control plane nodes when it is set.
CONTROL_PLANE_VIP="${CONTROL_PLANE_VIP:-}"

die() { echo "[create-preprovisioned-cluster] ERROR: $*" >&2; exit 1; }

[[ -n "${PREPROVISIONED_HOSTS}" ]] || die \
  'set PREPROVISIONED_HOSTS, for example "host-01=10.0.0.11,host-02=10.0.0.12"'

command -v jq >/dev/null 2>&1 || die "jq is required"

hosts_json() {
  local entries=() entry name address role
  IFS=',' read -ra entries <<< "${PREPROVISIONED_HOSTS}"
  for entry in "${entries[@]}"; do
    IFS='=' read -r name address role <<< "${entry}"
    [[ -n "${name}" && -n "${address}" ]] || die "bad host entry: ${entry}"
    if [[ -n "${role:-}" ]]; then
      jq -nc --arg n "${name}" --arg a "${address}" --arg r "${role}" \
        '{name:$n, role:$r, addresses:[{type:"InternalIP", address:$a}]}'
    else
      jq -nc --arg n "${name}" --arg a "${address}" \
        '{name:$n, addresses:[{type:"InternalIP", address:$a}]}'
    fi
  done | jq -sc .
}

HOSTS_JSON="$(hosts_json)"
HOST_COUNT="$(jq 'length' <<< "${HOSTS_JSON}")"
REQUESTED_COUNT=$(( CONTROL_PLANE_DESIRED_COUNT + WORKER_DESIRED_COUNT ))
if (( REQUESTED_COUNT > HOST_COUNT )); then
  die "asked for ${REQUESTED_COUNT} nodes but the inventory holds ${HOST_COUNT} hosts"
fi

if [[ "${REGISTER_DEFAULT_SITE:-true}" == "true" ]]; then
  API_URL="${API_BASE_URL}" \
    DEFAULT_SITE_ID="${DEFAULT_SITE_ID}" \
    DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE}" \
    "${SCRIPT_DIR}/register-default-site.sh" >/dev/null
fi

machine_template() {
  jq -nc '{
    apiVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
    kind: "PreprovisionedMachineTemplate",
    spec: { template: { spec: {} } }
  }'
}

node_pool() {
  local name="$1" prefix="$2" count="$3" control_plane="$4"
  jq -nc \
    --arg name "${name}" \
    --arg prefix "${prefix}" \
    --argjson count "${count}" \
    --argjson controlPlane "${control_plane}" \
    --arg attestation "${SPIRE_ATTESTATION_METHOD}" \
    --argjson template "$(machine_template)" \
    '{
      name: $name,
      nodeNamePrefix: $prefix,
      desiredNodeCount: $count,
      controlPlane: $controlPlane,
      systemReservedMemory: "1Gi",
      autoProvisionDisabled: false,
      autoModeDisabled: false,
      recycleBudget: 0,
      capiSpireAttestationMethod: $attestation,
      capiInfraMachineTemplate: $template
    }'
}

NODE_POOLS="$(node_pool "control-plane" "cp" "${CONTROL_PLANE_DESIRED_COUNT}" true)"
if (( WORKER_DESIRED_COUNT > 0 )); then
  NODE_POOLS="${NODE_POOLS}"$'\n'"$(node_pool "worker" "w" "${WORKER_DESIRED_COUNT}" false)"
fi
NODE_POOLS="$(jq -sc . <<< "${NODE_POOLS}")"

BODY="$(jq -nc \
  --arg name "${CLUSTER_NAME}" \
  --arg siteID "${DEFAULT_SITE_ID}" \
  --arg version "${KUBERNETES_VERSION}" \
  --arg cni "${CNI_PLUGIN}" \
  --argjson etcdEncrypted "${ETCD_ENCRYPTED}" \
  --argjson hosts "${HOSTS_JSON}" \
  --argjson nodePools "${NODE_POOLS}" \
  --arg vip "${CONTROL_PLANE_VIP}" \
  '{
    name: $name,
    siteID: $siteID,
    controlPlaneKubernetesVersion: $version,
    cniPlugin: $cni,
    etcdEncrypted: $etcdEncrypted,
    nodePools: $nodePools,
    capiInfraCluster: {
      apiVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
      kind: "PreprovisionedCluster",
      metadata: { name: $name },
      spec: { hosts: $hosts }
    }
  }
  + (if $vip == "" then {} else { controlPlaneVIP: $vip } end)')"

echo "Creating preprovisioned cluster ${CLUSTER_NAME}: ${HOST_COUNT} hosts, ${REQUESTED_COUNT} nodes, attestation=${SPIRE_ATTESTATION_METHOD}" >&2

curl -fsS --max-time "${CURL_MAX_TIME:-30}" "${API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
