#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "$*" >&2; exit 1; }
: "${DEV_NAME:=nke}"
[[ "$DEV_NAME" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ && ${#DEV_NAME} -le 32 ]] || fail "DEV_NAME must be a lowercase DNS label of at most 32 characters."
dev_dir="$PWD/.local/dev/$DEV_NAME"
export KUBECONFIG="$dev_dir/kubeconfig"
export HOST_CONTEXT="kind-$DEV_NAME-management" STATE_CONTEXT="kind-$DEV_NAME-state"
export HOST_NAMESPACE=management-engine-system STATE_NAMESPACE=management-state-system
host=(kubectl --context "$HOST_CONTEXT" --request-timeout=30s)
state=(kubectl --context "$STATE_CONTEXT" --request-timeout=30s)
clusters=("$DEV_NAME-management" "$DEV_NAME-state")
umask 077

require_cluster() {
  local cluster=$1 nodes
  nodes=$(kind get nodes --name "$cluster")
  [[ -n "$nodes" ]] || fail "$cluster is missing. Complete setup with make dev-up DEV_NAME=$DEV_NAME before running this command."
  [[ "$nodes" == "$cluster-control-plane" ]] || fail "$cluster must have exactly one control-plane node."
}

connect() {
  local cluster host_uid state_uid
  for cluster in "${clusters[@]}"; do require_cluster "$cluster"; done
  mkdir -p "$dev_dir"
  for cluster in "${clusters[@]}"; do
    kind export kubeconfig --name "$cluster" --kubeconfig "$KUBECONFIG"
  done
  host_uid=$("${host[@]}" get namespace kube-system -o jsonpath='{.metadata.uid}')
  state_uid=$("${state[@]}" get namespace kube-system -o jsonpath='{.metadata.uid}')
  [[ -n "$host_uid" && -n "$state_uid" && "$host_uid" != "$state_uid" ]] || fail "Host and state must resolve to distinct clusters."
}

sync_state() {
  make install-state STATE_CONTEXT="$STATE_CONTEXT"
  "${state[@]}" wait --for=condition=Established --timeout=60s -f config/crd/bases
}

refresh_credentials() (
  local node address ca token_response token expiration scratch
  "${host[@]}" create namespace "$HOST_NAMESPACE" --dry-run=client -o yaml | "${host[@]}" apply -f -
  # Pods use the state node's Docker IP and verify TLS against its certificate hostname.
  node="$DEV_NAME-state-control-plane"
  address=$(docker inspect --format '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$node")
  [[ -n "$address" ]] || fail "State node has no IPv4 address on Docker's kind network."
  ca=$("${state[@]}" config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  [[ -n "$ca" ]] || fail "State kubeconfig must supply a certificate authority."
  scratch=$(mktemp -d "$dev_dir/credentials.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  config=(kubectl --kubeconfig "$scratch/kubeconfig" config)
  "${config[@]}" set-cluster management-state --server="https://$address:6443" --tls-server-name="$node" >/dev/null
  "${config[@]}" set clusters.management-state.certificate-authority-data "$ca" >/dev/null
  # client-go rereads this file when the mounted Secret is refreshed.
  "${config[@]}" set users.management-engine.tokenFile /etc/management-api/token >/dev/null
  "${config[@]}" set-context management-state --cluster=management-state --user=management-engine >/dev/null
  "${config[@]}" use-context management-state >/dev/null
  token_response=$("${state[@]}" -n "$STATE_NAMESPACE" create token management-engine-controller-manager \
    --duration=24h -o jsonpath='{.status.token}{"\n"}{.status.expirationTimestamp}')
  token=${token_response%%$'\n'*}
  expiration=${token_response#*$'\n'}
  [[ -n "$token" && -n "$expiration" && "$token" != "$expiration" ]] || fail "Token response is missing a token or expiration."
  printf '%s' "$token" >"$scratch/token"
  "${host[@]}" -n "$HOST_NAMESPACE" create secret generic management-state-client \
    --from-file=kubeconfig="$scratch/kubeconfig" --from-file=token="$scratch/token" \
    --dry-run=client -o yaml | "${host[@]}" apply --server-side --field-manager=management-engine-dev -f -
  echo "Datastore credentials expire at $expiration; renew with make dev-refresh DEV_NAME=$DEV_NAME."
)

case "${1:-}" in
  up)
    : "${KIND_NODE_IMAGE:?Run make dev-up to use the pinned node image}"
    existing=$(kind get clusters)
    # Check all existing clusters before creating or changing either cluster.
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then
        require_cluster "$cluster"
        actual=$(docker inspect --format '{{.Image}}' "$cluster-control-plane")
        expected=$(docker image inspect --format '{{.Id}}' "$KIND_NODE_IMAGE")
        [[ "$actual" == "$expected" ]] || fail "$cluster uses a different node image; run make dev-down DEV_NAME=$DEV_NAME before recreating it."
      fi
    done
    mkdir -p "$dev_dir"
    for cluster in "${clusters[@]}"; do
      if ! grep -Fxq -- "$cluster" <<<"$existing"; then
        kind create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG" --wait 120s
      fi
    done
    connect
    make chart
    sync_state
    refresh_credentials
    echo "Ready: make dev DEV_NAME=$DEV_NAME"
    ;;
  sync) connect; sync_state ;;
  refresh) connect; refresh_credentials ;;
  tilt)
    connect
    exec tilt up --context "$HOST_CONTEXT" --port "${TILT_PORT:-10351}"
    ;;
  test)
    connect
    exec bash test/install.sh
    ;;
  down)
    existing=$(kind get clusters)
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then require_cluster "$cluster"; fi
    done
    for cluster in "${clusters[@]}"; do
      kind delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG"
    done
    rm -f "$KUBECONFIG"
    if [[ -d "$dev_dir" ]]; then rmdir "$dev_dir"; fi
    ;;
  *) fail "Usage: $0 {up|sync|refresh|tilt|test|down}" ;;
esac
