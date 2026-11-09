#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run demo.sh in CI against the CAPD stack that controller/hack/local-backend-e2e.sh
# leaves up with LOCAL_NKE_E2E_DEPLOY_ONLY=true: forward the NVKE API, point
# nvkectl at it, create a CAPD workload cluster with one worker pool, then run the
# demo, which adds the Mokka pool. MANAGEMENT_KUBECONFIG is that run's
# LOCAL_NKE_E2E_KUBECONFIG. .github/workflows/mokka-demo-e2e.yml is the caller.
set -euo pipefail

repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

MANAGEMENT_KUBECONFIG="${MANAGEMENT_KUBECONFIG:?set MANAGEMENT_KUBECONFIG to the management cluster kubeconfig}"
CLUSTER="${CLUSTER:-mokka-ci}"
API_PORT="${API_PORT:-18080}"
STATE_DIR="${STATE_DIR:-$(mktemp -d)}"
PROVISION_TIMEOUT="${PROVISION_TIMEOUT:-1800}"

log() { printf '[mokka-ci] %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }

mkdir -p "${STATE_DIR}/bin"
# Keep nvkectl's config and token cache out of the runner user's home.
export XDG_CONFIG_HOME="${STATE_DIR}/xdg-config" XDG_CACHE_HOME="${STATE_DIR}/xdg-cache"
export PATH="${STATE_DIR}/bin:${PATH}"

# Whether the runner reaches what GPU Operator and Mokka pull from. Informational:
# the demo fails on its own if one is unreachable, and this says which.
for url in https://nvcr.io/v2/ https://helm.ngc.nvidia.com/nvidia/index.yaml https://ghcr.io/v2/; do
  log "reachability ${url}: HTTP $(curl -s -o /dev/null -m 20 -w '%{http_code}' "${url}" || echo unreachable)"
done

log "building nvkectl"
(cd "${repo}/cli" && CGO_ENABLED=0 go build -o "${STATE_DIR}/bin/nvkectl" ./cmd/nvkectl)

# tilt ci's port-forwards end with it, so forward the API for this run.
log "forwarding the NVKE API to 127.0.0.1:${API_PORT}"
KUBECONFIG="${MANAGEMENT_KUBECONFIG}" kubectl -n nvke-controller port-forward svc/nvke-controller "${API_PORT}:8080" \
  >"${STATE_DIR}/port-forward.log" 2>&1 &
forward=$!
# The cache holds a minted API token; it must not reach the diagnostics artifact.
trap 'kill "${forward}" 2>/dev/null || true; rm -rf "${XDG_CACHE_HOME}"' EXIT

# nvkectl mints its API token from this kubeconfig and reads tenant kubeconfigs with it.
nvkectl config set-site --name ci --url "http://127.0.0.1:${API_PORT}" --kubeconfig "${MANAGEMENT_KUBECONFIG}" >/dev/null
deadline=$((SECONDS + 120))
until nvkectl get clusters >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    cat "${STATE_DIR}/port-forward.log" >&2
    die "the NVKE API did not answer within 2 minutes"
  fi
  sleep 5
done

log "creating CAPD workload cluster ${CLUSTER}"
sed "s/my-cluster/${CLUSTER}/g" "${repo}/cli/examples/capd-cluster.yaml" >"${STATE_DIR}/cluster.yaml"
nvkectl create cluster -f "${STATE_DIR}/cluster.yaml" >/dev/null

provisioned() {
  nvkectl get clusters "${CLUSTER}" -o json 2>/dev/null |
    jq -e 'if type == "array" then .[0] else . end | .conditions.provisioned.status == "True"' >/dev/null
}
deadline=$((SECONDS + PROVISION_TIMEOUT))
until provisioned; do
  if ((SECONDS >= deadline)); then
    nvkectl get clusters "${CLUSTER}" -o json >"${STATE_DIR}/cluster.json" 2>&1 || true
    die "cluster ${CLUSTER} was not Provisioned within ${PROVISION_TIMEOUT}s; see ${STATE_DIR}/cluster.json"
  fi
  sleep 20
done
log "cluster ${CLUSTER} is Provisioned"

# The demo's own failures are its PASS/FAIL lines; on a failure, keep what the
# workload cluster looked like for the diagnostics artifact.
rc=0
CLUSTER="${CLUSTER}" TIMEOUT="${TIMEOUT:-20m}" "${repo}/hack/mokka-demo/demo.sh" || rc=$?
if ((rc != 0)); then
  log "demo failed; collecting the workload cluster's state in ${STATE_DIR}"
  kubeconfig="${STATE_DIR}/workload.kubeconfig"
  if nvkectl get kubeconfig "${CLUSTER}" --output-file "${kubeconfig}" >/dev/null 2>&1; then
    KUBECONFIG="${kubeconfig}" kubectl get nodes -o wide >"${STATE_DIR}/workload-nodes.txt" 2>&1 || true
    KUBECONFIG="${kubeconfig}" kubectl get pods -A -o wide >"${STATE_DIR}/workload-pods.txt" 2>&1 || true
    KUBECONFIG="${kubeconfig}" kubectl get events -A --sort-by=.lastTimestamp >"${STATE_DIR}/workload-events.txt" 2>&1 || true
    rm -f "${kubeconfig}"
  fi
  nvkectl get nodepools --cluster "${CLUSTER}" -o json >"${STATE_DIR}/nodepools.json" 2>&1 || true
fi
exit "${rc}"
