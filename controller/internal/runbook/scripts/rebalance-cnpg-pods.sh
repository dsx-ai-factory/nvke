#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Rebalance CNPG (CloudNativePG) pods across nodes.
#
# After a new node joins the cluster, CNPG pods may be unevenly distributed
# (e.g. all on one node because the other was previously removed). This
# runbook redistributes them by deleting a non-primary replica and its PVCs
# from an overloaded node so CNPG recreates it on a node with capacity.
#
# For each CNPG namespace, the runbook:
#   1. Verifies the cluster is healthy
#   2. Counts pods per node to find imbalance
#   3. If any node has 2+ pods, picks a non-primary replica from the most
#      loaded node, deletes it and its PVCs simultaneously
#   4. Waits for the cluster to return to healthy
#   5. Repeats until balanced (no node has 2+ pods) or no further moves
#      are possible
#
# All CNPG namespaces are rebalanced in parallel.

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-"/etc/kubernetes/admin.conf"}"

CNPG_NAMESPACES=("cnpg-cloud" "cnpg-temporal" "cnpg-site")
HEALTHY_TIMEOUT="${HEALTHY_TIMEOUT:-300}"
POD_DELETE_TIMEOUT="${POD_DELETE_TIMEOUT:-120}"

main() {
  local new_node=${1:-$(hostname)}

  if [[ -z "$new_node" ]]; then
    echo "Error: could not determine new node name from argument or hostname" >&2
    exit 1
  fi

  if ! kubectl get node "$new_node" &>/dev/null; then
    echo "Node $new_node does not exist, nothing to rebalance."
    exit 0
  fi

  # Verify the new node is schedulable.
  local node_json
  node_json=$(kubectl get node "$new_node" -o json 2>/dev/null)

  local is_unschedulable
  is_unschedulable=$(echo "$node_json" | jq -r '.spec.unschedulable // false')
  if [[ "$is_unschedulable" == "true" ]]; then
    echo "Error: node $new_node is cordoned (SchedulingDisabled), cannot rebalance onto it" >&2
    exit 1
  fi

  local no_schedule_taints
  no_schedule_taints=$(echo "$node_json" | jq -r '[.spec.taints // [] | .[] | select(.effect == "NoSchedule")] | length')
  if [[ "$no_schedule_taints" -gt 0 ]]; then
    echo "Error: node $new_node has NoSchedule taints, cannot rebalance onto it" >&2
    echo "Taints:" >&2
    echo "$node_json" | jq -r '.spec.taints[] | select(.effect == "NoSchedule") | "  \(.key)=\(.value):\(.effect)"' >&2
    exit 1
  fi

  # Check if the CNPG Cluster CRD is installed.
  local api_output
  api_output=$(kubectl api-resources --api-group=postgresql.cnpg.io 2>/dev/null || true)
  if ! echo "$api_output" | grep -q clusters; then
    echo "CNPG is not installed in this cluster, skipping."
    exit 0
  fi

  # Rebalance all namespaces in parallel.
  local log_dir
  log_dir=$(mktemp -d)
  trap "rm -rf '$log_dir'" EXIT

  local pids=()
  for ns in "${CNPG_NAMESPACES[@]}"; do
    rebalance_namespace "$ns" > "$log_dir/$ns.log" 2>&1 &
    pids+=($!)
  done

  local failed=""
  for i in "${!CNPG_NAMESPACES[@]}"; do
    local ns="${CNPG_NAMESPACES[$i]}"
    local pid="${pids[$i]}"
    local ec=0
    wait "$pid" || ec=$?

    echo "=== $ns ==="
    cat "$log_dir/$ns.log"
    echo ""

    if [[ $ec -ne 0 ]]; then
      failed="$failed $ns"
    fi
  done

  if [[ -n "$failed" ]]; then
    echo "Error: CNPG rebalance failed for:$failed" >&2
    exit 1
  fi

  echo "CNPG rebalance complete."
}

# collect_pvcs outputs the PVC names for a given pod.
collect_pvcs() {
  local ns=$1
  local pod=$2
  kubectl get pod "$pod" -n "$ns" \
    -o jsonpath='{.spec.volumes[?(@.persistentVolumeClaim)].persistentVolumeClaim.claimName}' \
    2>/dev/null || echo ""
}

rebalance_namespace() {
  local ns=$1

  if ! kubectl get namespace "$ns" &>/dev/null; then
    echo "Namespace $ns does not exist, skipping."
    return
  fi

  local cluster_name
  cluster_name=$(kubectl get clusters.postgresql.cnpg.io -n "$ns" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

  if [[ -z "$cluster_name" ]]; then
    echo "No CNPG cluster found in $ns, skipping."
    return
  fi

  local original_instances ready_instances
  original_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.spec.instances}' 2>/dev/null || echo "")
  ready_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "0")

  if [[ -z "$original_instances" ]]; then
    echo "Error: could not read cluster spec for $cluster_name in $ns" >&2
    exit 1
  fi

  if [[ "$ready_instances" != "$original_instances" ]]; then
    echo "Cluster $cluster_name in $ns is not yet healthy (ready=$ready_instances, expected=$original_instances), waiting..."
    wait_for_healthy "$ns" "$cluster_name" "$original_instances"
  else
    echo "Cluster $cluster_name in $ns is healthy ($ready_instances/$original_instances ready)."
  fi

  # Rebalance loop: move one pod at a time until no node has 2+ pods.
  local max_iterations=$original_instances
  local iteration=0
  while [[ $iteration -lt $max_iterations ]]; do
    iteration=$((iteration + 1))

    local overloaded_node pod_count_on_node
    overloaded_node=$(get_most_loaded_node "$ns" "$cluster_name" "$new_node")

    if [[ -z "$overloaded_node" ]]; then
      echo "No node has 2+ CNPG pods in $ns, already balanced."
      return
    fi

    pod_count_on_node=$(kubectl get pods -n "$ns" \
      -l "cnpg.io/cluster=$cluster_name" \
      --field-selector "spec.nodeName=$overloaded_node" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')

    echo "Node $overloaded_node has $pod_count_on_node pod(s) in $ns, rebalancing (iteration $iteration)..."

    local current_primary
    current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
      -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")

    # Pick a non-primary replica on the overloaded node.
    local replica_to_move=""
    local pods_on_node
    pods_on_node=$(kubectl get pods -n "$ns" \
      -l "cnpg.io/cluster=$cluster_name" \
      --field-selector "spec.nodeName=$overloaded_node" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

    for pod in $pods_on_node; do
      if [[ "$pod" != "$current_primary" ]]; then
        replica_to_move="$pod"
        break
      fi
    done

    if [[ -z "$replica_to_move" ]]; then
      echo "No non-primary replica to move from $overloaded_node in $ns, done."
      return
    fi

    echo "Moving replica $replica_to_move off $overloaded_node in $ns..."
    force_delete_pod "$ns" "$replica_to_move"
    wait_for_healthy "$ns" "$cluster_name" "$original_instances"
  done

  echo "Rebalance complete for $ns."
}

# get_most_loaded_node returns the node with the most CNPG pods if the
# cluster is imbalanced. Accounts for the new node which may have 0 pods
# and therefore won't appear in the pod distribution.
# Returns empty string if already as balanced as possible.
get_most_loaded_node() {
  local ns=$1
  local cluster_name=$2
  local new_node=$3

  local distribution
  distribution=$(kubectl get pods -n "$ns" \
    -l "cnpg.io/cluster=$cluster_name" \
    -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null \
    | sort | uniq -c | sort -rn)

  if [[ -z "$distribution" ]]; then
    return
  fi

  local max_count max_node min_count
  max_count=$(echo "$distribution" | head -1 | awk '{print $1}')
  max_node=$(echo "$distribution" | head -1 | awk '{print $2}')
  min_count=$(echo "$distribution" | tail -1 | awk '{print $1}')

  # If the new node has no pods in this namespace, treat min_count as 0.
  if ! echo "$distribution" | awk '{print $2}' | grep -qx "$new_node"; then
    min_count=0
  fi

  if [[ $((max_count - min_count)) -le 1 ]]; then
    return
  fi

  echo "$max_node"
}

# force_delete_pod deletes a pod and its PVCs simultaneously.
force_delete_pod() {
  local ns=$1
  local pod=$2

  local pvc_names
  pvc_names=$(collect_pvcs "$ns" "$pod")

  if [[ -n "$pvc_names" ]]; then
    echo "Deleting PVCs: $pvc_names"
    for pvc in $pvc_names; do
      kubectl delete pvc "$pvc" -n "$ns" --ignore-not-found --wait=false 2>/dev/null || true
    done
  fi

  echo "Deleting pod: $pod"
  kubectl delete pod "$pod" -n "$ns" --grace-period=30 --wait=false 2>/dev/null || true

  # Wait for PVCs to be fully removed.
  if [[ -n "$pvc_names" ]]; then
    local elapsed=0
    while [[ $elapsed -lt $POD_DELETE_TIMEOUT ]]; do
      local remaining=""
      for pvc in $pvc_names; do
        if kubectl get pvc "$pvc" -n "$ns" &>/dev/null; then
          remaining="$remaining $pvc"
        fi
      done
      remaining=$(echo "$remaining" | xargs)

      if [[ -z "$remaining" ]]; then
        echo "Pod and PVCs removed in $ns."
        return
      fi

      echo "Waiting for PVCs to be deleted in $ns: $remaining"
      sleep 5
      elapsed=$((elapsed + 5))
    done

    echo "Error: PVCs still exist after ${POD_DELETE_TIMEOUT}s in $ns" >&2
    exit 1
  fi
}

wait_for_healthy() {
  local ns=$1
  local cluster_name=$2
  local expected_instances=$3

  echo "Waiting for $cluster_name in $ns to reach $expected_instances ready instances..."
  local elapsed=0
  while [[ $elapsed -lt $HEALTHY_TIMEOUT ]]; do
    local ready
    ready=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
      -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "0")

    if (( ready >= expected_instances )); then
      echo "Cluster $cluster_name in $ns is healthy ($ready/$expected_instances ready)."
      return
    fi

    echo "Cluster $cluster_name in $ns: $ready/$expected_instances ready (${elapsed}s/${HEALTHY_TIMEOUT}s)"
    sleep 10
    elapsed=$((elapsed + 10))
  done

  echo "Error: $cluster_name in $ns did not reach healthy state within ${HEALTHY_TIMEOUT}s" >&2
  exit 1
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
