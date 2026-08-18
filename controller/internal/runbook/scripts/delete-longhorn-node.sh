#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


set -e

export KUBECONFIG="${KUBECONFIG:-"/etc/kubernetes/admin.conf"}"

main() {
  local node_name=$1

  if [[ -z "$node_name" ]]; then
    echo "Error: node_name argument is required" >&2
    exit 1
  fi

  # Check if the Longhorn node CRD exists in the cluster.
  if ! kubectl api-resources --api-group=longhorn.io 2>/dev/null | grep -q nodes; then
    echo "Longhorn is not installed in this cluster, skipping."
    exit 0
  fi

  # Check if the Longhorn node object exists.
  if ! kubectl get nodes.longhorn.io -n longhorn-system "$node_name" &>/dev/null; then
    echo "Longhorn node $node_name does not exist, skipping."
    exit 0
  fi

  # Disable scheduling first — Longhorn's admission webhook rejects deletion
  # of nodes that still have allowScheduling=true.
  echo "Disabling scheduling on Longhorn node $node_name..."
  if ! kubectl patch nodes.longhorn.io -n longhorn-system "$node_name" \
    --type merge -p '{"spec":{"allowScheduling":false}}'; then
    echo "Failed to disable scheduling on Longhorn node $node_name" >&2
    exit 1
  fi

  # Delete any replicas still pinned to this node. When the Kubernetes node is
  # already gone Longhorn cannot migrate them automatically, and its admission
  # webhook rejects node deletion while replicas remain.
  local replicas
  replicas=$(kubectl get replicas.longhorn.io -n longhorn-system \
    -o jsonpath="{range .items[?(@.spec.nodeID=='$node_name')]}{.metadata.name}{'\n'}{end}" 2>/dev/null || true)

  if [[ -n "$replicas" ]]; then
    echo "Deleting Longhorn replicas on node $node_name: $replicas"
    for replica in $replicas; do
      kubectl delete replicas.longhorn.io -n longhorn-system "$replica" --ignore-not-found --timeout=60s || true
    done

    # Wait briefly for Longhorn to reconcile replica removal.
    sleep 5
  fi

  echo "Deleting Longhorn node $node_name..."
  kubectl delete nodes.longhorn.io -n longhorn-system "$node_name" --ignore-not-found --timeout=120s || {
    echo "Failed to delete Longhorn node $node_name" >&2
    exit 1
  }

  echo "Longhorn node $node_name has been deleted."
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
