#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Show NVSentinel detect a simulated GPU fault on an NVKE node, cordon and drain
# it, and uncordon it once the fault clears, with no GPUs: ../demo.sh gives the
# workload cluster two Mokka nodes, then this heats one GPU past its slowdown
# threshold and cools it again. Prints PASS or FAIL per check and exits non-zero
# on any failure. README.md has the details.
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER="${CLUSTER:?set CLUSTER to the NVKE workload cluster name}"
POOL="${POOL:-mokka-sentinel}"
POOL_PREFIX="${POOL_PREFIX:-ms}"
LABEL_KEY="${LABEL_KEY:-mokka.example/nvsentinel-gpu}"
GPU_PROFILE="${GPU_PROFILE:-h100}"
MOKKA_RELEASE="${MOKKA_RELEASE:-nvml-mock-sentinel}"
GPU_OPERATOR_VERSION="${GPU_OPERATOR_VERSION:-v26.3.3}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.19.1}"
LOCAL_PATH_VERSION="${LOCAL_PATH_VERSION:-v0.0.31}"
NVSENTINEL_VERSION="${NVSENTINEL_VERSION:-v1.21.0}"
TARGET_GPU="${TARGET_GPU:-0}"
# Strictly above every Mokka profile's slowdown threshold (h100 87 C, gb300 90 C):
# at the threshold the margin is 0 and the watch stays healthy.
HOT_TEMP_C="${HOT_TEMP_C:-142}"
TIMEOUT="${TIMEOUT:-20m}"

failures=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
die() { printf 'nvsentinel: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
# wait_for SECONDS COMMAND...: poll COMMAND every 5 seconds until it succeeds.
wait_for() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    ((SECONDS < deadline)) || return 1
    sleep 5
  done
}

if [[ -z "${TENANT_KUBECONFIG:-}" ]]; then
  TENANT_KUBECONFIG="$(mktemp)"
  trap 'rm -f "$TENANT_KUBECONFIG"' EXIT
  nvkectl get kubeconfig "$CLUSTER" --output-file "$TENANT_KUBECONFIG" >/dev/null
fi
export TENANT_KUBECONFIG KUBECONFIG="$TENANT_KUBECONFIG"

step "two Mokka nodes in pool ${POOL}"
CLUSTER="$CLUSTER" POOL="$POOL" POOL_PREFIX="$POOL_PREFIX" NODE_COUNT=2 LABEL_KEY="$LABEL_KEY" GPU_PROFILE="$GPU_PROFILE" \
  MOKKA_RELEASE="$MOKKA_RELEASE" GPU_OPERATOR_VERSION="$GPU_OPERATOR_VERSION" TIMEOUT="$TIMEOUT" "${here}/../demo.sh" ||
  die "demo.sh failed; NVSentinel needs its GPU nodes first"
read -r -a nodes <<<"$(kubectl get nodes -l "${LABEL_KEY}=true" -o jsonpath='{.items[*].metadata.name}')"
((${#nodes[@]} >= 2)) || die "need two nodes labeled ${LABEL_KEY}=true, found ${#nodes[@]}"

step "GPU Operator standalone DCGM, which NVSentinel's GPU Health Monitor polls"
helm upgrade gpu-operator gpu-operator --repo https://helm.ngc.nvidia.com/nvidia --version "$GPU_OPERATOR_VERSION" \
  --namespace gpu-operator --reuse-values --set dcgm.enabled=true --wait --timeout "$TIMEOUT" >/dev/null
if wait_for 600 kubectl -n gpu-operator get service nvidia-dcgm >/dev/null 2>&1; then
  pass "service gpu-operator/nvidia-dcgm exists"
else
  fail "no service gpu-operator/nvidia-dcgm"
fi

step "storage and cert-manager for NVSentinel's MongoDB"
default_sc="$(kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{end}')"
if [[ -z "$default_sc" ]]; then
  # The workload cluster has no default StorageClass; MongoDB needs one volume.
  kubectl apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_VERSION}/deploy/local-path-storage.yaml" >/dev/null
  kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite >/dev/null
  kubectl -n local-path-storage rollout status deploy/local-path-provisioner --timeout="$TIMEOUT" >/dev/null
  default_sc=local-path
fi
pass "default StorageClass ${default_sc}"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
kubectl -n cert-manager wait --for=condition=Available deploy --all --timeout="$TIMEOUT" >/dev/null
pass "cert-manager ${CERT_MANAGER_VERSION} is available"

step "NVSentinel ${NVSENTINEL_VERSION}"
# No --wait: the datastore comes up in stages (Percona operator, replica set,
# certificates, the collection-setup Job), and the pods that use it stay unready
# until it is done.
helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel --version "$NVSENTINEL_VERSION" \
  --namespace nvsentinel --create-namespace -f "${here}/nvsentinel-values.yaml" --timeout "$TIMEOUT" >/dev/null
kubectl -n nvsentinel wait --for=condition=complete job -l app.kubernetes.io/name=create-mongodb-database --timeout=15m >/dev/null ||
  fail "the MongoDB collection-setup Job did not complete"
all_ready() {
  ! kubectl -n nvsentinel get pods --field-selector=status.phase!=Succeeded \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -qv ' True$'
}
if wait_for 900 all_ready; then
  pass "NVSentinel pods are Ready"
else
  fail "NVSentinel pods not Ready: $(kubectl -n nvsentinel get pods --no-headers | awk '$2 !~ /^([0-9]+)\/\1$/ && $3 != "Completed" {print $1}' | tr '\n' ' ')"
fi

step "a GPU workload for the drainer to move"
kubectl apply -f - >/dev/null <<WORKLOAD
apiVersion: apps/v1
kind: Deployment
metadata: {name: nvsentinel-sample, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: nvsentinel-sample}}
  template:
    metadata: {labels: {app: nvsentinel-sample}}
    spec:
      nodeSelector: {${LABEL_KEY}: "true"}
      containers:
        - name: sleeper
          image: registry.k8s.io/pause:3.10
          resources: {limits: {nvidia.com/gpu: 1}}
WORKLOAD
kubectl rollout status deploy/nvsentinel-sample --timeout=5m >/dev/null
workload_node() { kubectl get pods -l app=nvsentinel-sample --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null; }
target="$(workload_node)"
[[ -n "$target" ]] || die "the sample workload is not running"
pass "sample workload runs on ${target}"
mock="$(kubectl -n mokka get pods -o name --field-selector "spec.nodeName=${target}" | head -1)"

cordoned() { [[ "$(kubectl get node "$target" -o jsonpath='{.spec.unschedulable}')" == true ]]; }
uncordoned() { ! cordoned; }
moved() { local n; n="$(workload_node)"; [[ -n "$n" && "$n" != "$target" ]]; }

step "fault: GPU ${TARGET_GPU} on ${target} at ${HOT_TEMP_C} C"
kubectl -n mokka exec "$mock" -- nvml-mock-ctl temp --gpu "$TARGET_GPU" "$HOT_TEMP_C" >/dev/null
if wait_for 600 cordoned; then
  pass "NVSentinel cordoned ${target}: $(kubectl get node "$target" -o json |
    jq -r '[.status.conditions[] | select(.type | test("Thermal")) | select(.status == "True") | .message] | first // "no thermal condition"')"
else
  fail "${target} was not cordoned"
fi
if wait_for 600 moved; then
  pass "the workload was drained to $(workload_node)"
else
  fail "the workload did not move off ${target}"
fi

step "recovery: GPU ${TARGET_GPU} on ${target} back to normal"
kubectl -n mokka exec "$mock" -- nvml-mock-ctl reset --gpu "$TARGET_GPU" >/dev/null
if wait_for 600 uncordoned; then
  pass "NVSentinel uncordoned ${target}"
else
  fail "${target} is still cordoned"
fi
kubectl delete deploy nvsentinel-sample --ignore-not-found >/dev/null

printf '\n'
if ((failures)); then
  printf 'nvsentinel: %d check(s) failed\n' "$failures"
  exit 1
fi
printf 'nvsentinel: all checks passed\n'
