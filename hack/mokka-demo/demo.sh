#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Give an NVKE workload cluster simulated GPU nodes and check that the GPU stack
# works on them: a node pool whose nodeLabels select Mokka (nvml-mock), Mokka on
# that pool only, GPU Operator, and a pod that requests a GPU. Prints PASS or FAIL
# per check and exits non-zero on any failure. README.md has the details.
set -euo pipefail

repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
# Shared with the e2e suite's CAPD Mokka entry, which embeds it.
values="${repo}/controller/test/e2e/testdata/mokka-gpu-operator-values.yaml"

CLUSTER="${CLUSTER:?set CLUSTER to the NVKE workload cluster name}"
POOL="${POOL:-mokka-gpu}"
POOL_PREFIX="${POOL_PREFIX:-mg}"
NODE_COUNT="${NODE_COUNT:-1}"
BASE_POOL="${BASE_POOL:-}"
GPU_PROFILE="${GPU_PROFILE:-gb300}"
LABEL_KEY="${LABEL_KEY:-mokka.example/simulated-gpu}"
MOKKA_RELEASE="${MOKKA_RELEASE:-nvml-mock}"
MOKKA_VERSION="${MOKKA_VERSION:-0.6.0}"
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-v26.3.3}"
TIMEOUT="${TIMEOUT:-20m}"

failures=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
die() { printf 'mokka-demo: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

for tool in nvkectl kubectl helm jq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
done

step "cluster ${CLUSTER}"
cluster_id="$(nvkectl get clusters "$CLUSTER" -o json | jq -r 'if type == "array" then .[0] else . end | .id')"
[[ -n "$cluster_id" && "$cluster_id" != null ]] || die "cluster ${CLUSTER} not found"
# The workload cluster's kubeconfig, never an inherited KUBECONFIG, which on an
# operator's shell usually points at the management cluster.
if [[ -n "${TENANT_KUBECONFIG:-}" ]]; then
  KUBECONFIG="$TENANT_KUBECONFIG"
else
  KUBECONFIG="$(mktemp)"
  trap 'rm -f "$KUBECONFIG"' EXIT
  nvkectl get kubeconfig "$CLUSTER" --output-file "$KUBECONFIG" >/dev/null
fi
export KUBECONFIG
pass "cluster ${CLUSTER} (${cluster_id}) is reachable"

step "node pool ${POOL} with nodeLabels ${LABEL_KEY}=true"
pools="$(nvkectl get nodepools --cluster "$CLUSTER" -o json)"
if ! jq -e --arg p "$POOL" '.[] | select(.name == $p)' <<<"$pools" >/dev/null; then
  # Copy the infrastructure settings (instance type, release, network) of an
  # existing worker pool, so the demo needs no provider-specific input.
  base="$(jq --arg b "$BASE_POOL" 'map(select(.controlPlane | not)) | map(select($b == "" or .name == $b)) | first' <<<"$pools")"
  [[ "$base" != null ]] || die "no worker pool to copy settings from; set BASE_POOL"
  jq --arg c "$cluster_id" --arg n "$POOL" --arg p "$POOL_PREFIX" --arg k "$LABEL_KEY" --argjson count "$NODE_COUNT" '
    {clusterID: $c, name: $n, nodeNamePrefix: $p, desiredNodeCount: $count,
     nodeLabels: {($k): "true"}}
    + (if .systemReservedMemory then {systemReservedMemory} else {} end)
    + (if .nico then {nico} elif .launchLayer then {launchLayer} elif .infra then {infra} else {} end)' \
    <<<"$base" | nvkectl create nodepool -f - >/dev/null
fi
deadline=$((SECONDS + 1200))
until [[ "$(kubectl get nodes -l "${LABEL_KEY}=true" --no-headers 2>/dev/null | grep -c ' Ready ')" -ge "$NODE_COUNT" ]]; do
  ((SECONDS < deadline)) || break
  sleep 15
done
nodes="$(kubectl get nodes -l "${LABEL_KEY}=true" -o jsonpath='{.items[*].metadata.name}')"
ready="$(kubectl get nodes -l "${LABEL_KEY}=true" --no-headers 2>/dev/null | grep -c ' Ready ' || true)"
if ((ready >= NODE_COUNT)); then
  pass "${ready} Ready node(s) carry ${LABEL_KEY}=true from the pool: ${nodes}"
else
  fail "${ready} of ${NODE_COUNT} labeled node(s) Ready"
  exit 1
fi

step "Mokka ${MOKKA_VERSION} (profile ${GPU_PROFILE}) on the labeled nodes"
helm upgrade --install "$MOKKA_RELEASE" oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock --version "$MOKKA_VERSION" \
  --namespace mokka --create-namespace --set "gpu.profile=${GPU_PROFILE}" \
  --set-string "nodeSelector.${LABEL_KEY//./\\.}=true" --wait --timeout "$TIMEOUT" >/dev/null
for node in $nodes; do
  pod="$(kubectl -n mokka get pods -o name --field-selector "spec.nodeName=${node}" | head -1)"
  gpus="$(kubectl -n mokka exec "$pod" -- nvidia-smi -L 2>/dev/null | grep -c '^GPU ' || true)"
  if ((gpus > 0)); then pass "${node}: nvidia-smi lists ${gpus} GPU(s)"; else fail "${node}: nvidia-smi lists no GPUs"; fi
done

step "GPU Operator ${GPU_OPERATOR_VERSION}"
helm upgrade --install gpu-operator gpu-operator --repo https://helm.ngc.nvidia.com/nvidia --version "$GPU_OPERATOR_VERSION" \
  --namespace gpu-operator --create-namespace -f "$values" --wait --timeout "$TIMEOUT" >/dev/null
if kubectl wait clusterpolicy/cluster-policy --for=jsonpath='{.status.state}'=ready --timeout="$TIMEOUT" >/dev/null; then
  pass "ClusterPolicy is ready"
else
  fail "ClusterPolicy did not become ready"
fi
for node in $nodes; do
  deadline=$((SECONDS + 600))
  until [[ "$(kubectl get node "$node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')" -gt 0 ]] 2>/dev/null; do
    ((SECONDS < deadline)) || break
    sleep 10
  done
  alloc="$(kubectl get node "$node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')"
  product="$(kubectl get node "$node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')"
  if [[ "${alloc:-0}" -gt 0 ]]; then pass "${node}: allocatable nvidia.com/gpu=${alloc} (${product})"; else fail "${node}: no allocatable nvidia.com/gpu"; fi
  # The validator's steps run in turn after the GPUs show up; wait for the last.
  deadline=$((SECONDS + 600))
  while :; do
    validator="$(kubectl -n gpu-operator get pods -l app=nvidia-operator-validator -o name --field-selector "spec.nodeName=${node}" | head -1)"
    steps="$(kubectl -n gpu-operator get "$validator" -o jsonpath='{range .status.initContainerStatuses[*]}{.name}={.state.terminated.exitCode}{" "}{end}' 2>/dev/null || true)"
    [[ -n "$steps" && "$steps" != *"= "* ]] && break
    ((SECONDS < deadline)) || break
    sleep 10
  done
  # Every step must have exited 0: a reason such as ContainerStatusUnknown is not success.
  failed="$(tr ' ' '\n' <<<"$steps" | grep . | grep -v '=0$' || true)"
  if [[ -n "$steps" && -z "$failed" ]]; then pass "${node}: validator steps exited 0: ${steps//=0/}"; else fail "${node}: validator not complete: ${steps:-none}"; fi
done

step "a pod requesting one GPU"
node="${nodes%% *}"
kubectl delete pod mokka-demo-smoke --ignore-not-found >/dev/null
kubectl apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: mokka-demo-smoke, namespace: default}
spec:
  restartPolicy: Never
  nodeSelector: {kubernetes.io/hostname: "${node}"}
  containers:
    - name: smi
      image: ubuntu:24.04
      command: ["nvidia-smi", "-L"]
      resources: {limits: {nvidia.com/gpu: 1}}
POD
if kubectl wait pod/mokka-demo-smoke --for=jsonpath='{.status.phase}'=Succeeded --timeout=5m >/dev/null &&
  [[ "$(kubectl logs mokka-demo-smoke | grep -c '^GPU ')" -eq 1 ]]; then
  pass "pod on ${node} saw exactly one GPU: $(kubectl logs mokka-demo-smoke)"
else
  fail "pod on ${node} did not see exactly one GPU"
fi
kubectl delete pod mokka-demo-smoke --ignore-not-found >/dev/null

printf '\n'
if ((failures)); then
  printf 'mokka-demo: %d check(s) failed\n' "$failures"
  exit 1
fi
printf 'mokka-demo: all checks passed\n'
