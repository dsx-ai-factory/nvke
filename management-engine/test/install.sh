#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
cd "$(dirname "$0")/.."
: "${HOST_CONTEXT:?Select the management Kind context}"
: "${STATE_CONTEXT:?Select the state Kind context}"
: "${IMG:?Build the image with make docker-build first}"
: "${KUBECONFIG:?Run make test-install to select the development environment}"
: "${HOST_NAMESPACE:?Run make test-install}"
: "${STATE_NAMESPACE:?Run make test-install}"
host=(kubectl --context "$HOST_CONTEXT" --request-timeout=30s)
state=(kubectl --context "$STATE_CONTEXT" --request-timeout=30s)
host_namespace=$HOST_NAMESPACE
state_namespace=$STATE_NAMESPACE
existing=$("${host[@]}" -n "$host_namespace" get deployments -l app.kubernetes.io/name=management-engine -o name)
if [[ -n "$existing" ]]; then
  echo "Use a separate DEV_NAME for test-install, or stop Tilt and run tilt down first." >&2; exit 1;
fi
release="engine-smoke-$$"
org_namespace="$release-org"
umask 077
scratch=$(mktemp -d)
forward_pid=""
cleanup() {
  if [[ -n "$forward_pid" ]]; then kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; fi
  helm --kube-context "$HOST_CONTEXT" uninstall "$release" -n "$host_namespace" --ignore-not-found >/dev/null || true
  "${state[@]}" delete clusterrolebinding "$release-metrics" --ignore-not-found >/dev/null || true
  "${state[@]}" delete namespace "$org_namespace" --ignore-not-found --wait=false >/dev/null || true
  rm -rf "$scratch"
}
trap cleanup EXIT
kind load docker-image "$IMG" --name "${HOST_CONTEXT#kind-}"
"${state[@]}" create namespace "$org_namespace"
make helm-deploy HOST_CONTEXT="$HOST_CONTEXT" HELM_RELEASE="$release" IMG="$IMG" \
  HELM_EXTRA_ARGS='-f config/samples/runtime-values.yaml --set manager.image.pullPolicy=Never'
deployment=$("${host[@]}" -n "$host_namespace" get deployment -l "app.kubernetes.io/instance=$release" -o name)
[[ -n "$deployment" ]]
"${host[@]}" -n "$host_namespace" rollout status "$deployment" --timeout=60s

# Readiness alone does not prove the manager authenticated to the remote API.
elected=false
for ((attempt=0; attempt<30; attempt++)); do
  holder=$("${state[@]}" -n "$state_namespace" get lease management-engine.nke.nvidia.com --ignore-not-found -o jsonpath='{.spec.holderIdentity}')
  if [[ "$holder" == "$release-"* ]]; then elected=true; break; fi
  sleep 2
done
[[ "$elected" == true ]] || { echo "Test runtime did not acquire the remote election Lease." >&2; exit 1; }
host_lease=$("${host[@]}" -n "$host_namespace" get lease management-engine.nke.nvidia.com --ignore-not-found -o name)
host_crds=$("${host[@]}" get --ignore-not-found -f config/crd/bases -o name)
[[ -z "$host_lease" && -z "$host_crds" ]] || { echo "State resources were found on the management cluster." >&2; exit 1; }
identity="system:serviceaccount:$state_namespace:management-engine-controller-manager"
[[ $("${state[@]}" auth can-i get leases --as="$identity" -n "$state_namespace") == yes ]]
[[ $("${state[@]}" auth can-i get /apis/internal.nke.nvidia.com/v1 --as="$identity") == yes ]]
[[ $("${state[@]}" auth can-i get secrets --as="$identity" -n "$org_namespace") == no ]]
[[ $("${state[@]}" auth can-i create nkeclusters.internal.nke.nvidia.com --as="$identity" -n "$org_namespace") == no ]]
[[ $("${state[@]}" auth can-i update leases --as="$identity" -n "$org_namespace") == no ]]
"${state[@]}" -n "$org_namespace" apply \
  -f config/samples/internal_v1_nkecluster.yaml -f config/samples/internal_v1_nodepool.yaml

# Prove watches and status writes work against the remote API, not just election.
resources=$("${state[@]}" -n "$org_namespace" get -k config/samples -o name)
while IFS= read -r resource; do
  generation=$("${state[@]}" -n "$org_namespace" get "$resource" -o jsonpath='{.metadata.generation}')
  "${state[@]}" -n "$org_namespace" wait "$resource" \
    --for="jsonpath={.status.observedGeneration}=$generation" --timeout=60s
done <<<"$resources"

# Secure metrics authenticate against the datastore API. Bind a temporary scraper
# there; its namespace and cluster-scoped binding are removed by cleanup.
"${state[@]}" -n "$org_namespace" create serviceaccount metrics-reader
"${state[@]}" create clusterrolebinding "$release-metrics" \
  --clusterrole=management-engine-metrics-reader \
  --serviceaccount="$org_namespace:metrics-reader"
metrics_token=$("${state[@]}" -n "$org_namespace" create token metrics-reader --duration=10m)
printf 'Authorization: Bearer %s\n' "$metrics_token" >"$scratch/metrics-header"
unset metrics_token
service=$("${host[@]}" -n "$host_namespace" get service -l "app.kubernetes.io/instance=$release" -o name)
"${host[@]}" -n "$host_namespace" port-forward "$service" :8443 >"$scratch/forward.log" 2>&1 &
forward_pid=$!
# The disposable Kind runtime uses a self-signed serving certificate. TLS
# verification is skipped only for this localhost port-forwarded smoke test.
metrics_ready=false
for ((attempt=0; attempt<30; attempt++)); do
  port=$(sed -n 's/.*127.0.0.1:\([0-9]*\) ->.*/\1/p' "$scratch/forward.log")
  if [[ -n "$port" ]] && curl -kfsS --max-time 2 --header "@$scratch/metrics-header" "https://127.0.0.1:$port/metrics" >"$scratch/metrics"; then
    metrics_ready=true
    break
  fi
  sleep 1
done
[[ "$metrics_ready" == true ]] || { cat "$scratch/forward.log" >&2; echo "Metrics endpoint was not reachable." >&2; exit 1; }
grep -q '^go_goroutines ' "$scratch/metrics"
anonymous_status=$(curl -ksS --max-time 2 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$port/metrics")
[[ "$anonymous_status" == 401 || "$anonymous_status" == 403 ]] || {
  echo "Metrics endpoint did not reject an unauthenticated request." >&2; exit 1;
}
kill "$forward_pid"
wait "$forward_pid" 2>/dev/null || true
forward_pid=""

make helm-deploy HOST_CONTEXT="$HOST_CONTEXT" HELM_RELEASE="$release" IMG="$IMG" \
  HELM_EXTRA_ARGS='-f config/samples/runtime-values.yaml --set manager.image.pullPolicy=Never --set manager.replicas=2'
[[ $("${host[@]}" -n "$host_namespace" get "$deployment" -o jsonpath='{.status.readyReplicas}') == 2 ]]
helm --kube-context "$HOST_CONTEXT" uninstall "$release" -n "$host_namespace" --wait --timeout 90s
"${state[@]}" get -f config/crd/bases -o name
"${state[@]}" -n "$org_namespace" get \
  -f config/samples/internal_v1_nkecluster.yaml -f config/samples/internal_v1_nodepool.yaml -o name
echo "Remote reconciliation, leadership/RBAC, metrics, replica upgrade, and state retention passed."
