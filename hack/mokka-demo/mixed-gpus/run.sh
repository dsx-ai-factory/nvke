#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Give one NVKE workload cluster two node pools that present different simulated
# GPU models, and check that each pool advertises its own model and count and that
# a workload selecting a model lands only on that model's pool. Runs ../demo.sh
# once per profile, then checks across both. README.md has the details.
set -euo pipefail

demo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER="${CLUSTER:?set CLUSTER to the NVKE workload cluster name}"
PROFILES="${PROFILES:-gb300 h100}"
TIMEOUT="${TIMEOUT:-20m}"

failures=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
step() { printf '\n== %s\n' "$*"; }

read -r -a profiles <<<"$PROFILES"
((${#profiles[@]} >= 2)) || { echo "mixed-gpus: set PROFILES to two or more Mokka GPU profiles" >&2; exit 1; }

# One pool, node label and Mokka release per profile.
pool() { printf 'mokka-%s' "$1"; }
label() { printf 'mokka.example/gpu-%s' "$1"; }
# The GPU name in nvidia-smi -L output, as GPU Feature Discovery labels it: spaces as dashes.
gpu_product() { sed -E 's/^GPU [0-9]+: (.*) \(UUID.*/\1/' | head -1 | tr ' ' '-'; }

if [[ -z "${TENANT_KUBECONFIG:-}" ]]; then
  TENANT_KUBECONFIG="$(mktemp)"
  trap 'rm -f "$TENANT_KUBECONFIG"' EXIT
  nvkectl get kubeconfig "$CLUSTER" --output-file "$TENANT_KUBECONFIG" >/dev/null
fi
export TENANT_KUBECONFIG KUBECONFIG="$TENANT_KUBECONFIG"

for profile in "${profiles[@]}"; do
  step "pool $(pool "$profile"): Mokka profile ${profile}"
  CLUSTER="$CLUSTER" POOL="$(pool "$profile")" POOL_PREFIX="m${profile}" LABEL_KEY="$(label "$profile")" \
    GPU_PROFILE="$profile" MOKKA_RELEASE="nvml-mock-${profile}" TIMEOUT="$TIMEOUT" "${demo_dir}/demo.sh" ||
    fail "demo.sh for profile ${profile}"
done

step "each pool advertises its own GPU model and count"
declare -A product_of
for profile in "${profiles[@]}"; do
  for node in $(kubectl get nodes -l "$(label "$profile")=true" -o jsonpath='{.items[*].metadata.name}'); do
    pod="$(kubectl -n mokka get pods -o name --field-selector "spec.nodeName=${node}" | head -1)"
    smi="$(kubectl -n mokka exec "$pod" -- nvidia-smi -L 2>/dev/null | grep '^GPU ' || true)"
    want_count="$(grep -c . <<<"$smi" || true)"
    want_product="$(gpu_product <<<"$smi")"
    product="$(kubectl get node "$node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')"
    count="$(kubectl get node "$node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')"
    if [[ -n "$product" && "$product" == "$want_product" && "$count" == "$want_count" ]]; then
      pass "${node} (${profile}): ${product} x${count}"
    else
      fail "${node} (${profile}): ${product:-no product} x${count:-0}, want ${want_product:-?} x${want_count}"
    fi
    product_of[$profile]="$product"
  done
done
if [[ "$(printf '%s\n' "${product_of[@]}" | sort -u | grep -c .)" -eq "${#profiles[@]}" ]]; then
  pass "the ${#profiles[@]} pools advertise ${#profiles[@]} different models"
else
  fail "the pools do not advertise distinct models: ${product_of[*]}"
fi

step "a workload that selects a model lands on that model's pool"
for profile in "${profiles[@]}"; do
  product="${product_of[$profile]:-}"
  [[ -n "$product" ]] || { fail "no product label for ${profile}"; continue; }
  name="mixed-gpus-${profile}"
  kubectl delete pod "$name" --ignore-not-found >/dev/null
  kubectl apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: ${name}}
spec:
  restartPolicy: Never
  nodeSelector: {nvidia.com/gpu.product: "${product}"}
  containers:
    - name: smi
      image: ubuntu:24.04
      command: ["nvidia-smi", "-L"]
      resources: {limits: {nvidia.com/gpu: 1}}
POD
  if kubectl wait "pod/${name}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=5m >/dev/null; then
    node="$(kubectl get pod "$name" -o jsonpath='{.spec.nodeName}')"
    key="$(label "$profile")"
    on_pool="$(kubectl get node "$node" -o jsonpath="{.metadata.labels.${key//./\\.}}" 2>/dev/null || true)"
    seen="$(kubectl logs "$name")"
    if [[ "$on_pool" == true && "$(gpu_product <<<"$seen")" == "$product" ]]; then
      pass "${product} pod ran on ${node} in pool $(pool "$profile") and saw: ${seen}"
    else
      fail "${product} pod ran on ${node}, outside pool $(pool "$profile"), and saw: ${seen}"
    fi
  else
    fail "${product} pod did not complete"
  fi
  kubectl delete pod "$name" --ignore-not-found >/dev/null
done

printf '\n'
if ((failures)); then
  printf 'mixed-gpus: %d check(s) failed\n' "$failures"
  exit 1
fi
printf 'mixed-gpus: all checks passed\n'
