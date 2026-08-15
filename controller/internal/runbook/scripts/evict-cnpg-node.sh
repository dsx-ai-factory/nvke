#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# Evict CNPG (CloudNativePG) pods from a node before drain.
#
# With Longhorn dataLocality=strict-local, CNPG PVCs are bound to specific
# nodes. A standard drain will hang because the PDB blocks eviction and the
# pods cannot reschedule to another node. This runbook:
#   1. Cordons the target node so no new pods are scheduled on it
#   2. Recovers unhealthy clusters caused by stuck join jobs (e.g. from a
#      prior cordon that left Longhorn unable to provision volumes)
#   3. Verifies each CNPG cluster is healthy before making further changes
#   4. If all instances are on the target node, force-deletes one non-primary
#      replica and its PVCs so CNPG recreates it on another node
#   5. Switches the CNPG primary away from the target node
#   6. Force-deletes remaining pods and their PVCs on the target node
#   7. Waits for the cluster to return to healthy
#
# All CNPG namespaces are evicted in parallel to minimize total time.
#
# On a small cluster (e.g. 2 control-plane nodes), it is common for all or
# most CNPG instances to land on the same node due to storage provisioning.
# The force-delete-one-replica step handles this case gracefully.
#
# When a cluster is unhealthy due to stuck join jobs (incomplete jobs whose
# pods are Pending or stuck in Init), the runbook first switches the primary
# off the target node to break the operator deadlock, then cleans up the
# stale jobs and PVCs so the operator can recreate instances on available
# nodes.
#
# The reclaim-local-pvs-cnpg runbook handles PV recovery when the node
# rejoins the cluster.
#
# FORCE_UNREACHABLE=true (exported by CWE when the operator force-deletes an
# unreachable node) switches the runbook to best-effort. Every hard failure
# below is a wait on the target node: its pods will not terminate because the
# kubelet is gone, so the PVC-protection finalizer never clears, the primary
# cannot be switched over gracefully, and the cluster never reports the ready
# instance the dead node used to hold. Blocking teardown on those waits is
# what the force-delete path exists to break out of, so in force mode they
# degrade to warnings and pods are deleted with --force --grace-period=0.
# CWE's own force-drain removes whatever this runbook leaves behind.

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-"/etc/kubernetes/admin.conf"}"

CNPG_NAMESPACES=("cnpg-cloud" "cnpg-temporal" "cnpg-site")
SWITCHOVER_TIMEOUT="${SWITCHOVER_TIMEOUT:-120}"
POD_DELETE_TIMEOUT="${POD_DELETE_TIMEOUT:-120}"
HEALTHY_TIMEOUT="${HEALTHY_TIMEOUT:-300}"
FORCE_UNREACHABLE="${FORCE_UNREACHABLE:-false}"

# abort_unless_forced fails the eviction with the given message, unless the
# operator declared the node unreachable — then it warns and returns non-zero
# so the caller can skip the rest of the namespace. Callers must guard the
# call (`abort_unless_forced "..." || return 0`) because of `set -e`.
abort_unless_forced() {
  local msg=$1

  if [[ "$FORCE_UNREACHABLE" == "true" ]]; then
    echo "Warning: $msg; continuing due to FORCE_UNREACHABLE." >&2
    return 1
  fi

  echo "Error: $msg" >&2
  exit 1
}

main() {
  local node_name=$1

  if [[ -z "$node_name" ]]; then
    echo "Error: node_name argument is required" >&2
    exit 1
  fi

  if ! kubectl get node "$node_name" &>/dev/null; then
    echo "Node $node_name does not exist, nothing to evict."
    exit 0
  fi

  # Cordon the node so CNPG does not schedule replacement pods back onto it.
  echo "Cordoning node $node_name..."
  kubectl cordon "$node_name"

  # Check if the CNPG Cluster CRD is installed at all.
  local api_output
  api_output=$(kubectl api-resources --api-group=postgresql.cnpg.io 2>/dev/null || true)
  if ! echo "$api_output" | grep -q clusters; then
    echo "CNPG is not installed in this cluster, skipping."
    exit 0
  fi

  # Evict all namespaces in parallel. Each runs in a subshell with output
  # captured to a log file so the results are cleanly separated.
  local log_dir
  log_dir=$(mktemp -d)
  trap "rm -rf '$log_dir'" EXIT

  local pids=()
  for ns in "${CNPG_NAMESPACES[@]}"; do
    evict_namespace "$ns" "$node_name" > "$log_dir/$ns.log" 2>&1 &
    pids+=($!)
  done

  # Wait for all parallel evictions and collect results.
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
    echo "Error: CNPG eviction failed for:$failed" >&2
    exit 1
  fi

  echo "CNPG eviction complete for node $node_name."
}

# count_schedulable_nodes returns the number of nodes that match a CNPG
# cluster's nodeSelector, are Ready, are not cordoned, and are not the target
# node. CNPG anti-affinity allows at most one instance per node, so this is
# the upper bound on ready instances reachable while the target is being
# evicted. Returns non-zero if the node list cannot be enumerated; callers
# must treat empty output as an error rather than as zero schedulable nodes.
count_schedulable_nodes() {
  local ns=$1
  local cluster_name=$2
  local target_node=$3

  # Build a label selector from spec.affinity.nodeSelector. Empty values
  # become a key-existence check (e.g. "node-role.kubernetes.io/control-plane").
  local label_selector
  label_selector=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o go-template='{{range $k, $v := .spec.affinity.nodeSelector}}{{if $v}}{{$k}}={{$v}},{{else}}{{$k}},{{end}}{{end}}' \
    2>/dev/null | sed 's/,$//')

  # Use `|| echo ""` so an apiserver blip surfaces as empty (handled below)
  # rather than being silently swallowed into a 0-node count by `2>/dev/null`.
  local jsonpath='{range .items[*]}{.metadata.name}={.spec.unschedulable}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
  local nodes_raw
  if [[ -n "$label_selector" ]]; then
    nodes_raw=$(kubectl get nodes -l "$label_selector" -o jsonpath="$jsonpath" || echo "")
  else
    nodes_raw=$(kubectl get nodes -o jsonpath="$jsonpath" || echo "")
  fi

  if [[ -z "$nodes_raw" ]]; then
    echo "Error: failed to enumerate nodes for $cluster_name in $ns; cannot compute schedulable nodes." >&2
    return 1
  fi

  local count=0
  while IFS='=' read -r name unschedulable ready; do
    [[ -z "$name" ]] && continue
    [[ "$name" == "$target_node" ]] && continue
    [[ "$unschedulable" == "true" ]] && continue
    [[ "$ready" != "True" ]] && continue
    count=$((count + 1))
  done <<< "$nodes_raw"

  echo "$count"
}

# effective_expected_instances returns min(spec.instances, schedulable_nodes).
# When schedulable nodes cannot satisfy spec.instances under anti-affinity,
# waiting for the full spec is geometrically impossible and would time out.
# Soften the target to what is achievable so the eviction can complete.
effective_expected_instances() {
  local ns=$1
  local cluster_name=$2
  local spec_instances=$3
  local target_node=$4

  local schedulable
  if ! schedulable=$(count_schedulable_nodes "$ns" "$cluster_name" "$target_node"); then
    return 1
  fi

  if [[ "$schedulable" -lt "$spec_instances" ]]; then
    echo "Warning: $cluster_name in $ns has spec.instances=$spec_instances but only" \
      "$schedulable schedulable node(s) (excluding $target_node);" \
      "softening health target to $schedulable to avoid impossible wait." >&2
    echo "$schedulable"
  else
    echo "$spec_instances"
  fi
}

# get_pods_on_node outputs space-separated pod names for a CNPG cluster on a
# given node.
get_pods_on_node() {
  local ns=$1
  local cluster_name=$2
  local node_name=$3
  kubectl get pods -n "$ns" \
    -l "cnpg.io/cluster=$cluster_name,cnpg.io/podRole=instance" \
    --field-selector "spec.nodeName=$node_name" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo ""
}

# get_pods_off_node outputs space-separated pod names for a CNPG cluster NOT
# on a given node.
get_pods_off_node() {
  local ns=$1
  local cluster_name=$2
  local node_name=$3
  kubectl get pods -n "$ns" \
    -l "cnpg.io/cluster=$cluster_name,cnpg.io/podRole=instance" \
    --field-selector "spec.nodeName!=$node_name" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo ""
}

# collect_pvcs outputs the PVC names for a given pod.
collect_pvcs() {
  local ns=$1
  local pod=$2
  kubectl get pod "$pod" -n "$ns" \
    -o jsonpath='{.spec.volumes[?(@.persistentVolumeClaim)].persistentVolumeClaim.claimName}' \
    2>/dev/null || echo ""
}

# cleanup_stuck_joins recovers a cluster that is unhealthy due to incomplete
# join jobs. With strict-local storage and a cordoned node, the CNPG operator
# can get stuck: it tries to switchover the primary off the unschedulable
# node but reports "something is already in progress" (the join), while the
# join can't complete because Longhorn can't provision a volume. Breaking
# this deadlock requires two steps in order:
#   1. Patch targetPrimary to move the primary off the target node, which
#      lets the operator complete the switchover without the join blocking it.
#   2. Delete the stale join jobs and their PVCs. With the switchover done,
#      the operator is free to create fresh instances on available nodes.
cleanup_stuck_joins() {
  local ns=$1
  local cluster_name=$2
  local node_name=$3

  # Find incomplete join jobs for this cluster. List all jobs and filter out
  # completed ones in bash, since kubectl JSONPath does not reliably support
  # the != operator across versions.
  local stuck_jobs
  stuck_jobs=$(kubectl get jobs -n "$ns" -l "cnpg.io/cluster=$cluster_name" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.succeeded}{"\n"}{end}' \
    2>/dev/null | grep -v '=1$' | cut -d= -f1 | xargs || echo "")
  stuck_jobs=$(echo "$stuck_jobs" | xargs)

  if [[ -z "$stuck_jobs" ]]; then
    return 1
  fi

  # Verify these are actually stuck: their pods must be Pending or in Init
  # (not actively running a healthy data sync).
  local confirmed_stuck=""
  for job in $stuck_jobs; do
    local pod_phase
    pod_phase=$(kubectl get pods -n "$ns" -l "job-name=$job" \
      -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "")
    local container_waiting
    container_waiting=$(kubectl get pods -n "$ns" -l "job-name=$job" \
      -o jsonpath='{.items[0].status.initContainerStatuses[0].state.waiting.reason}' \
      2>/dev/null || echo "")

    if [[ "$pod_phase" == "Pending" || -n "$container_waiting" ]]; then
      confirmed_stuck="$confirmed_stuck $job"
    fi
  done
  confirmed_stuck=$(echo "$confirmed_stuck" | xargs)

  if [[ -z "$confirmed_stuck" ]]; then
    return 1
  fi

  echo "Found stuck join jobs in $ns: $confirmed_stuck"

  # Step 1: Switchover the primary off the target node to break the operator
  # deadlock. The operator cannot create new instances while it is stuck
  # looping between switchover and join logic.
  local current_primary
  current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")
  local primary_node
  primary_node=$(kubectl get pod "$current_primary" -n "$ns" \
    -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

  if [[ "$primary_node" == "$node_name" ]]; then
    local off_node_pods
    off_node_pods=$(get_pods_off_node "$ns" "$cluster_name" "$node_name")
    if [[ -n "$off_node_pods" ]]; then
      echo "Primary $current_primary is on target node, switching over first..."
      switchover_primary "$ns" "$cluster_name" "$current_primary" "$node_name"
    else
      echo "Primary $current_primary is on target node but no replicas off-node; deferring switchover."
    fi
  fi

  # Step 2: Delete the stale join jobs (cascades to pods) and their PVCs.
  for job in $confirmed_stuck; do
    # Collect PVCs from the job's pod before deleting.
    local pod_name
    pod_name=$(kubectl get pods -n "$ns" -l "job-name=$job" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

    if [[ -n "$pod_name" ]]; then
      local pod_pvcs
      pod_pvcs=$(collect_pvcs "$ns" "$pod_name")
      for pvc in $pod_pvcs; do
        echo "Deleting PVC: $pvc"
        kubectl delete pvc "$pvc" -n "$ns" --ignore-not-found --wait=false 2>/dev/null || true
      done
    fi

    echo "Deleting stuck job: $job"
    kubectl delete job "$job" -n "$ns" --ignore-not-found 2>/dev/null || true
  done

  echo "Cleaned up stuck join jobs in $ns."
}

evict_namespace() {
  local ns=$1
  local node_name=$2

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

  local original_instances
  original_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.spec.instances}' 2>/dev/null || echo "")

  # Compute the effective health target up front. With anti-affinity, a
  # cluster cannot exceed one ready instance per schedulable node. If the
  # target node is the difference, waiting for spec.instances would time out
  # forever. Soften the target now so every wait_for_healthy below uses an
  # achievable value.
  local effective_instances=""
  if [[ -n "$original_instances" ]]; then
    if ! effective_instances=$(effective_expected_instances \
      "$ns" "$cluster_name" "$original_instances" "$node_name"); then
      echo "Error: failed to compute effective instances for $cluster_name in $ns" >&2
      exit 1
    fi
  fi

  local pods_on_node
  pods_on_node=$(get_pods_on_node "$ns" "$cluster_name" "$node_name")

  if [[ -z "$pods_on_node" ]]; then
    echo "No CNPG pods on $node_name in $ns."
    # Even with no pods to evict, verify the cluster is healthy. A previous
    # eviction attempt may have removed the pods but the cluster might still
    # be recovering (e.g. join pods stuck on a node with broken storage).
    # Failing here ensures CWE retries until recovery completes rather than
    # proceeding with the teardown while databases are degraded.
    if [[ -n "$effective_instances" ]]; then
      wait_for_healthy "$ns" "$cluster_name" "$effective_instances" || return 0
    fi
    return
  fi

  local pod_count
  pod_count=$(echo "$pods_on_node" | wc -w | tr -d ' ')
  echo "Found $pod_count CNPG pod(s) on $node_name in $ns: $pods_on_node"

  local current_primary ready_instances
  current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")
  ready_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "0")

  if [[ -z "$current_primary" || -z "$original_instances" ]]; then
    abort_unless_forced "could not read cluster status for $cluster_name in $ns" || return 0
  fi

  # Verify the cluster is healthy before making changes. A degraded cluster
  # could lose data or availability if we remove additional instances.
  #
  # The CNPG operator may react to the cordon by auto-initiating a switchover,
  # which temporarily drops readyInstances. Wait briefly for the cluster to
  # settle before checking health.
  #
  # If the cluster remains unhealthy due to stuck join jobs, attempt automatic
  # recovery: switchover the primary off the target node, then clean up the
  # stale jobs so the operator can recreate instances on available nodes.
  if (( ready_instances < effective_instances )); then
    echo "Cluster $cluster_name in $ns is not fully ready (ready=$ready_instances, expected=$effective_instances)."
    echo "Waiting for cluster to settle..."
    local settle_elapsed=0
    while [[ $settle_elapsed -lt 60 ]]; do
      sleep 5
      settle_elapsed=$((settle_elapsed + 5))
      ready_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
        -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "0")
      if (( ready_instances >= effective_instances )); then
        break
      fi
      echo "Cluster $cluster_name in $ns: $ready_instances/$effective_instances ready (settling ${settle_elapsed}s/60s)"
    done
    # Re-read primary in case the operator auto-switched over.
    current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
      -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")
  fi

  if (( ready_instances < effective_instances )); then
    echo "Cluster $cluster_name in $ns is not healthy (ready=$ready_instances, expected=$effective_instances)."
    echo "Checking for stuck join jobs..."

    if cleanup_stuck_joins "$ns" "$cluster_name" "$node_name"; then
      wait_for_healthy "$ns" "$cluster_name" "$effective_instances" || return 0

      # Re-read state after recovery.
      current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
        -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")
      ready_instances=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
        -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "0")
      pods_on_node=$(get_pods_on_node "$ns" "$cluster_name" "$node_name")
      pod_count=$(echo "$pods_on_node" | wc -w | tr -d ' ')
      echo "After recovery: $pod_count pod(s) on $node_name in $ns: $pods_on_node"
    else
      abort_unless_forced "$cluster_name in $ns is not healthy and no stuck join jobs found;\
 resolve the unhealthy cluster before removing this node" || return 0
    fi
  fi
  echo "Cluster $cluster_name in $ns is healthy ($ready_instances/$effective_instances ready)."

  # If all instances are on the target node, we need to move one replica to
  # another node before we can switchover the primary. Force-delete one
  # non-primary replica and its PVCs; CNPG recreates it on another node
  # since the target is cordoned.
  local pods_off_node
  pods_off_node=$(get_pods_off_node "$ns" "$cluster_name" "$node_name")

  if [[ -z "$pods_off_node" ]]; then
    echo "All instances are on $node_name in $ns, moving one replica off..."

    if [[ $original_instances -lt 2 ]]; then
      abort_unless_forced "cannot evict single-instance cluster $cluster_name in $ns;\
 all instances are on the target node and there are no replicas to migrate" || return 0
    fi

    # Pick a non-primary replica to move off the target node.
    local replica_to_move=""
    for pod in $pods_on_node; do
      if [[ "$pod" != "$current_primary" ]]; then
        replica_to_move="$pod"
        break
      fi
    done

    if [[ -z "$replica_to_move" ]]; then
      abort_unless_forced "could not find a non-primary replica to move in $ns" || return 0
    fi

    force_delete_pods "$ns" "$cluster_name" "$node_name" "$replica_to_move" || return 0
    wait_for_healthy "$ns" "$cluster_name" "$effective_instances" || return 0

    # Re-read state after migration.
    pods_on_node=$(get_pods_on_node "$ns" "$cluster_name" "$node_name")
    pod_count=$(echo "$pods_on_node" | wc -w | tr -d ' ')
    current_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
      -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")
    echo "After migration: $pod_count pod(s) remain on $node_name in $ns: $pods_on_node"
  fi

  if [[ -z "$pods_on_node" ]]; then
    echo "No CNPG pods remaining on $node_name in $ns after migration."
    return
  fi

  # Switchover primary if it is still on the target node.
  local primary_node
  primary_node=$(kubectl get pod "$current_primary" -n "$ns" \
    -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

  if [[ "$primary_node" == "$node_name" ]]; then
    echo "Primary $current_primary in $ns is on target node, initiating switchover..."
    # In force mode a failed switchover is not fatal: the primary's node is gone,
    # so CNPG's own failover is what will promote a replica.
    switchover_primary "$ns" "$cluster_name" "$current_primary" "$node_name" || true
  else
    echo "Primary $current_primary in $ns is on $primary_node (not target node)."
  fi

  # Re-read pods on node (switchover may have changed pod names).
  pods_on_node=$(get_pods_on_node "$ns" "$cluster_name" "$node_name")

  if [[ -z "$pods_on_node" ]]; then
    echo "No CNPG pods remaining on $node_name in $ns after switchover."
    return
  fi

  # Force-delete all remaining pods on the target and their PVCs. The node
  # is cordoned so CNPG will recreate them on other available nodes.
  force_delete_pods "$ns" "$cluster_name" "$node_name" "$pods_on_node" || return 0
  wait_for_healthy "$ns" "$cluster_name" "$effective_instances" || return 0

  echo "Eviction complete for $ns."
}

# force_delete_pods deletes the specified CNPG pods and their PVCs from the
# target node. PVCs are marked for deletion first (blocked by the
# pvc-protection finalizer), then pods are deleted. Once a pod terminates the
# finalizer clears and its PVCs are removed. CNPG then recreates the
# instance with a fresh PVC on an available node (target is cordoned).
force_delete_pods() {
  local ns=$1
  local cluster_name=$2
  local node_name=$3
  local pods_to_delete=$4

  # Collect PVCs while the pods still exist.
  local pvc_names=""
  for pod in $pods_to_delete; do
    local pod_pvcs
    pod_pvcs=$(collect_pvcs "$ns" "$pod")
    pvc_names="$pvc_names $pod_pvcs"
  done
  pvc_names=$(echo "$pvc_names" | xargs)

  # Delete PVCs and pods simultaneously. The PVC deletions are queued but
  # blocked by the pvc-protection finalizer until the owning pod terminates.
  # Once the pod is gone the finalizer clears, the PVC is removed, and CNPG
  # creates a fresh instance with a new PVC on an available node.
  if [[ -n "$pvc_names" ]]; then
    echo "Deleting PVCs: $pvc_names"
    for pvc in $pvc_names; do
      kubectl delete pvc "$pvc" -n "$ns" --ignore-not-found --wait=false 2>/dev/null || true
    done
  fi

  # A graceful delete waits for the kubelet to confirm termination. On an
  # unreachable node that confirmation never arrives: the pod object lingers in
  # Terminating and keeps the PVC-protection finalizer set, so the PVC wait below
  # can never be satisfied. --force removes the pod object from the API server
  # without waiting for the kubelet, which releases the finalizer.
  local -a delete_opts=(--grace-period=30 --wait=false)
  if [[ "$FORCE_UNREACHABLE" == "true" ]]; then
    delete_opts=(--force --grace-period=0 --wait=false)
  fi

  echo "Deleting pods: $pods_to_delete"
  for pod in $pods_to_delete; do
    kubectl delete pod "$pod" -n "$ns" "${delete_opts[@]}" 2>/dev/null || true
  done

  # Wait for PVCs to be fully removed (confirms pods terminated and
  # finalizers cleared).
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
        echo "All pods and PVCs removed in $ns."
        return
      fi

      echo "Waiting for PVCs to be deleted in $ns: $remaining"
      sleep 5
      elapsed=$((elapsed + 5))
    done

    abort_unless_forced "PVCs still exist after ${POD_DELETE_TIMEOUT}s in $ns: $remaining" || return 1
  fi
}

switchover_primary() {
  local ns=$1
  local cluster_name=$2
  local current_primary=$3
  local node_name=$4

  local target_instance
  target_instance=$(get_pods_off_node "$ns" "$cluster_name" "$node_name" | awk '{print $1}')

  if [[ -z "$target_instance" ]]; then
    abort_unless_forced "no candidate instance for switchover in $ns" || return 1
  fi

  echo "Switching primary from $current_primary to $target_instance in $ns..."
  kubectl patch clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
    --type merge -p "{\"status\":{\"targetPrimary\":\"$target_instance\"}}" \
    --subresource=status

  local elapsed=0
  while [[ $elapsed -lt $SWITCHOVER_TIMEOUT ]]; do
    local new_primary
    new_primary=$(kubectl get clusters.postgresql.cnpg.io "$cluster_name" -n "$ns" \
      -o jsonpath='{.status.currentPrimary}' 2>/dev/null || echo "")

    if [[ "$new_primary" == "$target_instance" ]]; then
      echo "Switchover complete: new primary is $new_primary in $ns."
      return
    fi

    sleep 5
    elapsed=$((elapsed + 5))
  done

  abort_unless_forced "switchover did not complete within ${SWITCHOVER_TIMEOUT}s for $ns" || return 1
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

  abort_unless_forced "$cluster_name in $ns did not reach healthy state within ${HEALTHY_TIMEOUT}s" || return 1
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
