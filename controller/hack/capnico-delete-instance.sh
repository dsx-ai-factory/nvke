#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Delete an instance created by hack/capnico-create-instance.sh.
#
# Deleting the Machine triggers the NicoMachine finalizer, which calls nico-vms
# DeleteInstance (VM + qemu/swtpm torn down in ~4s); deleting the Cluster
# cascades to the NicoCluster.
#
# Notes:
#   - The nico-vms instance ID is captured from Machine.spec.providerID BEFORE
#     anything is deleted -- afterwards there is nothing left to read it from.
#   - Deletion is verified with a by-ID GET expecting 404. A list-based check is
#     invalid: nico-vms ignores the `?name=` filter and returns all instances.
#   - Several instances share one Cluster: the create script re-applies the same
#     NAMESPACE/CLUSTER_NAME for every INSTANCE_NAME. CAPI cascades a Cluster
#     deletion to every Machine labelled with that cluster, so deleting the
#     Cluster while siblings exist destroys their VMs too. The Cluster is
#     therefore only removed once no labelled Machine is left -- see
#     DELETE_CLUSTER.
#   - Instances of *different* Clusters can also share one namespace, and a
#     namespace deletion cascades to all of them regardless of the label
#     selector. The namespace is therefore only removed once no Cluster or
#     Machine at all is left in it.

set -euo pipefail

INSTANCE_BACKEND="${INSTANCE_BACKEND:-vm}"
case "${INSTANCE_BACKEND}" in
  VM | vm)
    DEFAULT_CLUSTER_NAME="nico-vms-bare"
    DEFAULT_INSTANCE_NAME="capnico-vm-1"
    ;;
  Container | container)
    DEFAULT_CLUSTER_NAME="nico-containers-bare"
    DEFAULT_INSTANCE_NAME="capnico-container-1"
    ;;
  *)
    echo "Unsupported INSTANCE_BACKEND '${INSTANCE_BACKEND}' (expected VM or Container)" >&2
    exit 1
    ;;
esac

NAMESPACE="${NAMESPACE:-nico-test}"
CLUSTER_NAME="${CLUSTER_NAME:-${DEFAULT_CLUSTER_NAME}}"
INSTANCE_NAME="${INSTANCE_NAME:-${DEFAULT_INSTANCE_NAME}}"
BOOTSTRAP_SECRET_NAME="${BOOTSTRAP_SECRET_NAME:-${INSTANCE_NAME}-bootstrap}"
DELETE_NAMESPACE="${DELETE_NAMESPACE:-true}"

# auto  -- delete the Cluster only when no other Machine still uses it (default)
# always -- delete the Cluster unconditionally (destroys sibling instances' VMs)
# never  -- keep the Cluster (and the namespace) whatever happens
DELETE_CLUSTER="${DELETE_CLUSTER:-auto}"

ORG_ID="${ORG_ID:-novxwn552ssc}"
NICO_SIM_URL="${NICO_SIM_URL:-http://localhost:18080}"
NICO_SIM_TOKEN="${NICO_SIM_TOKEN:-nico-simctl-token}"

DELETE_TIMEOUT="${DELETE_TIMEOUT:-5m}"

case "${DELETE_CLUSTER}" in
  auto | always | never) ;;
  *)
    echo "Unsupported DELETE_CLUSTER '${DELETE_CLUSTER}' (expected auto, always or never)" >&2
    exit 1
    ;;
esac

# `|| true`: a missing Machine (already deleted, or a create run that never got
# that far) must not abort the script under `set -e` -- the rest of the cleanup
# still has to run. Empty is handled explicitly further down.
INSTANCE_ID="$(kubectl get machine "${INSTANCE_NAME}" -n "${NAMESPACE}" \
  -o jsonpath='{.spec.providerID}' 2>/dev/null || true)"
INSTANCE_ID="${INSTANCE_ID#nico://}"

echo "Deleting instance '${INSTANCE_NAME}' (cluster ${CLUSTER_NAME}) in namespace ${NAMESPACE}" >&2

# --timeout makes this block until the Machine (and with it the NicoMachine
# finalizer's nico-vms DeleteInstance) is really gone, so the sibling count
# below no longer includes the instance being deleted.
kubectl delete machine "${INSTANCE_NAME}" -n "${NAMESPACE}" --ignore-not-found --timeout="${DELETE_TIMEOUT}"
# Normally a no-op: the Machine ownerRef cascades to the NicoMachine. It matters
# when the create script applied the NicoMachine but not the Machine, leaving an
# orphan no cascade would ever reap.
kubectl delete nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" --ignore-not-found --timeout="${DELETE_TIMEOUT}"
kubectl delete secret "${BOOTSTRAP_SECRET_NAME}" -n "${NAMESPACE}" --ignore-not-found

SIBLINGS="$(kubectl get machine -n "${NAMESPACE}" \
  -l "cluster.x-k8s.io/cluster-name=${CLUSTER_NAME}" \
  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
SIBLING_COUNT="$(printf '%s' "${SIBLINGS}" | wc -w | tr -d ' ')"

case "${DELETE_CLUSTER}" in
  always)
    echo "DELETE_CLUSTER=always -- deleting cluster ${CLUSTER_NAME} (${SIBLING_COUNT} other machine(s) will be cascaded away)" >&2
    REMOVE_CLUSTER=true
    ;;
  never)
    echo "DELETE_CLUSTER=never -- keeping cluster ${CLUSTER_NAME} and namespace ${NAMESPACE}." >&2
    REMOVE_CLUSTER=false
    ;;
  *)
    if [ "${SIBLING_COUNT}" -eq 0 ]; then
      REMOVE_CLUSTER=true
    else
      echo "${SIBLING_COUNT} sibling machine(s) still use cluster ${CLUSTER_NAME} (${SIBLINGS})." >&2
      echo "Deleting the Cluster would cascade to their VMs -- skipping Cluster and Namespace deletion." >&2
      REMOVE_CLUSTER=false
    fi
    ;;
esac

if [ "${REMOVE_CLUSTER}" = "true" ]; then
  kubectl delete cluster "${CLUSTER_NAME}" -n "${NAMESPACE}" --ignore-not-found --timeout="${DELETE_TIMEOUT}"

  if [ "${DELETE_NAMESPACE}" = "true" ]; then
    # The sibling count above is scoped to CLUSTER_NAME, but a namespace
    # deletion cascades to every Cluster/Machine in the namespace regardless of
    # label. Only remove it once nothing CAPI-owned is left.
    LEFTOVERS="$(kubectl get cluster,machine -n "${NAMESPACE}" -o name 2>/dev/null || true)"
    if [ -n "${LEFTOVERS}" ]; then
      echo "Namespace ${NAMESPACE} still holds CAPI objects of other clusters -- keeping it:" >&2
      printf '%s\n' "${LEFTOVERS}" >&2
    else
      kubectl delete namespace "${NAMESPACE}" --ignore-not-found --timeout="${DELETE_TIMEOUT}"
    fi
  fi
fi

if [ -z "${INSTANCE_ID}" ]; then
  echo "Machine had no providerID -- no nico-sim instance to verify." >&2
  exit 0
fi

echo "Verifying nico-sim instance ${INSTANCE_ID} is gone..." >&2
for _ in $(seq 1 30); do
  HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${NICO_SIM_TOKEN}" \
    "${NICO_SIM_URL}/v2/org/${ORG_ID}/forge/instance/${INSTANCE_ID}")"
  if [ "${HTTP_CODE}" = "404" ]; then
    echo "nico-sim instance ${INSTANCE_ID} is deleted (404)."
    exit 0
  fi
  sleep 2
done

echo "nico-sim still returns HTTP ${HTTP_CODE} for instance ${INSTANCE_ID} after 60s." >&2
exit 1
