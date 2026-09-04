#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=controller/hack/backend-e2e-lib.sh
source "${repo_root}/hack/backend-e2e-lib.sh"

# `tilt ci` reads the Tiltfile from the working directory, so the script only
# worked when run from here. Do not make the caller know that.
cd "${repo_root}"
state_dir="${LOCAL_NKE_E2E_STATE_DIR:-${repo_root}/.local/backend-e2e}"
tmp_dir="$(mktemp -d)"
kind_cluster_name="${KIND_CLUSTER_NAME:-cwe-capi}"
runner_name="cwe-capd-local-e2e-${$}"
runner_image="${LOCAL_NKE_E2E_RUNNER_IMAGE:-docker:29.1.3-cli@sha256:4fa0ee1f3a7e4354c4ea34558b6d4ee32859baf4973d4c8ccc8e7fe3dd730c04}"
tilt_config="${repo_root}/tilt_config.json"
tilt_config_backup="${tmp_dir}/tilt_config.json"
host_kubeconfig="${tmp_dir}/management-host.kubeconfig"
internal_kubeconfig="${tmp_dir}/management-internal.kubeconfig"
e2e_config="${tmp_dir}/e2e-config.json"
e2e_binary="${tmp_dir}/cwe-e2e.test"
had_tilt_config=false
tilt_config_staged=false
management_ready=false
# Unique per run so two checkouts on one host do not fight over the container.
bundle_mirror_name="capd-e2e-bundle-mirror-${$}"
bundle_mirror_volume="capd-e2e-bundle-${$}"

log() {
  printf '[local-capd-e2e] %s\n' "$*"
}

fail() {
  log "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

collect_diagnostics() {
  local rc=$?
  set +e

  mkdir -p "${state_dir}"
  docker ps -a --no-trunc >"${state_dir}/docker-ps.txt" 2>&1
  docker system df >"${state_dir}/docker-system-df.txt" 2>&1
  e2e_collect_node_diagnostics "${state_dir}/nodes" \
    "${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}"
  docker logs bgp-containers >"${state_dir}/bgp-containers.log" 2>&1
  docker exec bgp-containers vtysh -c 'show bgp summary' >"${state_dir}/bgp-containers-summary.txt" 2>&1
  docker exec bgp-containers vtysh -c 'show bgp ipv4 unicast' >"${state_dir}/bgp-containers-routes.txt" 2>&1
  if [[ "${management_ready}" == true ]]; then
    kubectl get nodes -o wide >"${state_dir}/management-nodes.txt" 2>&1
    kubectl get pods -A -o wide >"${state_dir}/management-pods.txt" 2>&1
    kubectl get events -A --sort-by=.lastTimestamp >"${state_dir}/management-events.txt" 2>&1
    kubectl get clusters,machines,kubeadmcontrolplanes,dockermachines -A -o yaml \
      >"${state_dir}/capi-resources.yaml" 2>&1
    kubectl -n cloud-workflow-engine logs deployment/cloud-workflow-engine --all-containers --tail=-1 \
      >"${state_dir}/cwe.log" 2>&1
    kubectl -n capd-system logs deployment/capd-controller-manager --all-containers --tail=-1 \
      >"${state_dir}/capd-controller.log" 2>&1
  fi

  if docker inspect "${runner_name}" >/dev/null 2>&1; then
    docker cp "${runner_name}:/state/junit.xml" "${state_dir}/junit.xml" >/dev/null 2>&1 || true
    docker rm -f "${runner_name}" >/dev/null 2>&1 || true
  fi

  e2e_stop_bundle_mirror "${bundle_mirror_name}" "${bundle_mirror_volume}"

  if [[ "${tilt_config_staged}" == true ]]; then
    if [[ "${had_tilt_config}" == true ]]; then
      cp "${tilt_config_backup}" "${tilt_config}"
    else
      rm -f "${tilt_config}"
    fi
  fi
  rm -rf "${tmp_dir}"

  if ((rc == 0)); then
    log "CAPD E2E artifacts: ${state_dir}"
  else
    log "CAPD E2E failed; diagnostics: ${state_dir}" >&2
  fi
  return "${rc}"
}
trap collect_diagnostics EXIT

for command in curl docker dpkg-deb go jq kind kubectl make tilt; do
  require_command "${command}"
done

case "$(uname -s)" in
  Darwin) host_os=darwin ;;
  Linux) host_os=linux ;;
  *) fail "unsupported host $(uname -s): this entrypoint supports macOS and Linux" ;;
esac

case "$(uname -m)" in
  arm64 | aarch64) host_arch=arm64 ;;
  x86_64) host_arch=amd64 ;;
  *) fail "unsupported local architecture $(uname -m)" ;;
esac

# The Tiltfile refuses to render without these (no defaults). Check here too so
# the failure is visible on the console instead of buried in tilt.log. Values
# may come from the shell environment or .env.tilt.
if [[ ! -f "${repo_root}/.env.tilt" ]]; then
  for var in DGXC_TENANT_USER DGXC_TENANT_PASSWORD CNPG_SUPERUSER_PASSWORD \
    CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
    [[ -n "${!var:-}" ]] || \
      fail "${var} is required: export it or create .env.tilt from .env.tilt.example"
  done
fi

# macOS reaches Docker through a Colima VM, which this script starts. Linux runs
# Docker on the host, so there is no VM to size and DOCKER_HOST is whatever the
# daemon already uses.
if [[ "${host_os}" == darwin ]]; then
  : "${COLIMA_PROFILE:=default}"
  : "${COLIMA_CPUS:=10}"
  : "${COLIMA_MEMORY_GIB:=24}"
  : "${COLIMA_DISK_GIB:=150}"
  : "${COLIMA_DOCKER_HOST:=unix://${HOME}/.colima/${COLIMA_PROFILE}/docker.sock}"
  : "${DOCKER_HOST:=${COLIMA_DOCKER_HOST}}"
  export COLIMA_PROFILE COLIMA_CPUS COLIMA_MEMORY_GIB COLIMA_DISK_GIB
  export COLIMA_DOCKER_HOST DOCKER_HOST
fi
: "${NODE_AGENT_REPO_PATH:=${repo_root}/../agent}"
export NODE_AGENT_REPO_PATH
export KIND_CLUSTER_NAME="${kind_cluster_name}"
export CAPD_NODE_AGENT_LOCAL_BUILD="${CAPD_NODE_AGENT_LOCAL_BUILD:-true}"
export CAPD_SPIRE_ATTESTATION_METHOD="${CAPD_SPIRE_ATTESTATION_METHOD:-$(e2e_attestation_method)}"
log "SPIRE node attestation: ${CAPD_SPIRE_ATTESTATION_METHOD}"

[[ -f "${NODE_AGENT_REPO_PATH}/Makefile" ]] || \
  fail "NODE_AGENT_REPO_PATH=${NODE_AGENT_REPO_PATH} does not look like an agent checkout"

mkdir -p "${state_dir}"

if curl --fail --silent --show-error --connect-timeout 1 --max-time 1 \
  http://localhost:10350/api/view >/dev/null 2>&1; then
  fail "another Tilt session is running on port 10350; stop it before running this script"
fi

if [[ "${host_os}" == darwin ]]; then
  log "starting Colima with ${COLIMA_CPUS} CPUs, ${COLIMA_MEMORY_GIB} GiB memory, and ${COLIMA_DISK_GIB} GiB disk"
  make -C "${repo_root}" colima-up
else
  log "using the host Docker daemon"
  docker info >/dev/null 2>&1 || fail "cannot reach the Docker daemon; is it running and is your user in the docker group?"
fi

# A failed run can leave CAPD tenant nodes behind, and kube-vip can keep
# advertising their control-plane VIP. Those containers may belong to another
# worktree or investigation, so preserve them unless cleanup is explicitly
# requested.
if [[ "${LOCAL_NKE_E2E_CLEAN_STALE_NODES:-false}" == true ]]; then
  stale_nodes="$(docker ps -aq --filter 'name=e2e-capd-' | wc -l | tr -d ' ')"
  if [[ "${stale_nodes}" != 0 ]]; then
    log "removing ${stale_nodes} tenant node containers left by an earlier run"
    docker ps -aq --filter 'name=e2e-capd-' | xargs -r docker rm -f >/dev/null 2>&1 || true
  fi
else
  log "preserving tenant node containers from other local E2E runs"
fi

log "ensuring the Kind management cluster exists"
make -C "${repo_root}" kind-up
kind get kubeconfig --name "${kind_cluster_name}" >"${host_kubeconfig}"
export KUBECONFIG="${host_kubeconfig}"
management_ready=true

log "building the CAPD bundle from public sources"
# Not under state_dir: that directory is what CI uploads on failure, and these
# tarballs are 22 MB of the 31 MB it collected, all of it re-downloadable.
bundle_dir="${tmp_dir}/bundles"
rm -rf "${bundle_dir}"
BUNDLE_ARCH="${host_arch}" bash "${repo_root}/hack/ci/prepare-capd-bundles.sh" "${bundle_dir}"

export SPIRE_AGENT_BUNDLE_PATH="${bundle_dir}/spire-agent.tgz"
CAPD_CNI_MANIFESTS_DEB_PATH="$(echo "${bundle_dir}"/nke-cni-calico-manifests-*_all.deb)"
export CAPD_CNI_MANIFESTS_DEB_PATH
[[ -f "${CAPD_CNI_MANIFESTS_DEB_PATH}" ]] \
  || fail "prepare-capd-bundles.sh produced no Calico manifests package"

# With INSTALL_CNI_AFTER_INIT the agent reads CNI from the Calico manifests deb
# baked into the node image, so no bundle mirror is started and the node image
# bakes in no bundle flags. That is the whole point: a green run this way proves
# a node with no bundle source can still get CNI.
if [[ "${INSTALL_CNI_AFTER_INIT:-false}" == "true" ]]; then
  log "INSTALL_CNI_AFTER_INIT=true: no bundle mirror, no bundle flags on the node image"
else
  e2e_stop_bundle_mirror "${bundle_mirror_name}" "${bundle_mirror_volume}"
  mirror_ip="$(e2e_start_bundle_mirror \
    "${bundle_mirror_name}" "${bundle_mirror_volume}" "${bundle_dir}")" \
    || fail "could not start the bundle mirror"
  export BUNDLE_SERVER_URL="http://${mirror_ip}/"
  export INDEX_FILE_URL="http://${mirror_ip}/index.json"
  # Prove it serves before building an image that bakes the URL in.
  docker run --rm --network kind alpine:3.22 \
    wget -q -O /dev/null "${INDEX_FILE_URL}" \
    || fail "bundle mirror is not serving ${INDEX_FILE_URL}"
  log "bundle mirror at ${BUNDLE_SERVER_URL}"
fi

log "building the local agent CAPD image and installing CAPI/CAPD"
if [[ "${LOCAL_NKE_E2E_REUSE_CAPD_SETUP:-false}" == true ]]; then
  node_image="${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}"
  docker image inspect "${node_image}" >/dev/null || fail "CAPD node image ${node_image} does not exist"
  kubectl rollout status deployment/capi-controller-manager -n capi-system --timeout=120s
  kubectl rollout status deployment/capd-controller-manager -n capd-system --timeout=120s
  log "reusing the existing CAPD providers and node image ${node_image}"
else
  make -C "${repo_root}" capd-up
fi

capnico_crd_dir="${NICO_PROVIDER_REPO_DIR:-${repo_root}/../../cluster-api-provider-nico}/config/crd/bases"
[[ -d "${capnico_crd_dir}" ]] ||
  fail "CAPNICo CRD directory not found: ${capnico_crd_dir}"
log "installing CAPNICo CRDs"
kubectl apply -f "${capnico_crd_dir}"
kubectl wait --for=condition=Established crd \
  nicoclusters.infrastructure.cluster.x-k8s.io \
  nicoclustertemplates.infrastructure.cluster.x-k8s.io \
  nicomachines.infrastructure.cluster.x-k8s.io \
  nicomachinetemplates.infrastructure.cluster.x-k8s.io \
  --timeout=2m

if [[ "${INSTALL_CNI_AFTER_INIT:-false}" == "true" ]]; then
  node_image="${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}"
  agent_args="$(docker run --rm --entrypoint cat "${node_image}" /etc/default/nke-agent)" \
    || fail "could not read /etc/default/nke-agent from ${node_image}"
  if grep -qE -- '--bundle-server-url|--index-file-url' <<<"${agent_args}"; then
    fail "${node_image} bakes in a bundle source, so this run would not prove the post-init CNI path: ${agent_args}"
  fi
  log "node image carries no bundle source: ${agent_args}"
fi

# CAPD tenant nodes are sibling containers on the `kind` network, so they reach
# the management cluster's SPIRE NodePort at its container IP. Without this the
# Tiltfile falls back to the kind bridge gateway, where nothing is bound on
# Linux, and every node's spire-agent gets connection refused. Same value the CI
# entrypoint computes.
management_vip="$(e2e_management_vip "${kind_cluster_name}")" ||
  fail "could not read the Kind control-plane IP"

if [[ -f "${tilt_config}" ]]; then
  cp "${tilt_config}" "${tilt_config_backup}"
  had_tilt_config=true
fi
local_e2e_providers="${LOCAL_NKE_E2E_PROVIDERS:-${E2E_CAPD_PROVIDERS}}"
e2e_write_tilt_config "${tilt_config}" \
  "${local_e2e_providers}" \
  "${LOCAL_CWE_IMAGE:-}" "${management_vip}" "${BGP_CONTAINER_NAME:-bgp-containers}"
tilt_config_staged=true

# Sequential two-mode CAPD run (mirrors controller/hack/ci/backend-e2e.sh): a
# default local run validates both control-plane paths on one cluster -- the
# cp-controller leg (flag-on) first, then a light CWE redeploy to flag-off for
# the node-pool leg. Set LOCAL_NKE_E2E_LABEL_FILTER to run a single explicit
# filter instead, or LOCAL_NKE_E2E_SINGLE_RUNNER_CAPD_CONTROL_PLANE_MODES=false.
sequential_modes="${LOCAL_NKE_E2E_SINGLE_RUNNER_CAPD_CONTROL_PLANE_MODES:-true}"
if [[ -n "${LOCAL_NKE_E2E_LABEL_FILTER:-}" ]]; then
  sequential_modes=false
fi
# The cp-controller leg needs a flag-on CWE, so deploy flag-on when sequential.
if [[ "${sequential_modes}" == "true" ]]; then
  export PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true
fi

log "deploying local CWE through Tilt"
TILT_DISABLE_ANALYTICS=1 tilt ci \
  --port=0 \
  --timeout=40m \
  --output-snapshot-on-exit="${state_dir}/tilt-snapshot.json" \
  >"${state_dir}/tilt.log" 2>&1

log "compiling the E2E suite for linux/${host_arch}"
(
  cd "${repo_root}/test/e2e"
  GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${host_arch}" \
    go test -c -o "${e2e_binary}" .
)

kind get kubeconfig --internal --name "${kind_cluster_name}" >"${internal_kubeconfig}"
jq -n \
  --arg kubeconfig /state/management.kubeconfig \
  --arg context "kind-${kind_cluster_name}" \
  --arg capd_node_image "${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}" \
  '{
    kube: {kubeconfig: $kubeconfig, context: $context},
    cluster: {capdNodeImage: $capd_node_image},
    lifecycle: {preserveOnFailure: true}
  }' >"${e2e_config}"

log "starting the E2E client on the Kind Docker network"
if ! docker image inspect "${runner_image}" >/dev/null 2>&1; then
  docker pull "${runner_image}" >/dev/null
fi
docker create \
  --name "${runner_name}" \
  --network kind \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  "${runner_image}" sleep infinity >/dev/null
docker start "${runner_name}" >/dev/null
docker exec "${runner_name}" mkdir -p /state
docker cp "${e2e_binary}" "${runner_name}:/state/cwe-e2e.test"
docker cp "${internal_kubeconfig}" "${runner_name}:/state/management.kubeconfig"
docker cp "${e2e_config}" "${runner_name}:/state/e2e-config.json"

# Run one Ginkgo leg against the already-deployed CWE. suffix keeps each leg's
# log and JUnit report distinct (empty for a single-mode run).
# Tilt builds CWE with `make build`, which bakes BUILD_VERSION in, so ask the
# Makefile for the same value rather than deriving it again here. Without it the
# health suite's version spec skips itself and the leg still reports success.
cwe_expected_version="$(make -C "${repo_root}" -s --no-print-directory print-BUILD_VERSION)"

# Exec adapter for the shared leg runner: locally the suite runs in a container
# on the Kind network, reached with `docker exec`.
local_e2e_exec() {
  local cp_flag="$1"; shift
  local exchange_enabled=false
  if jq -e 'index("dsx-identity-exchange") != null' <<<"${local_e2e_providers}" >/dev/null; then
    exchange_enabled=true
  fi
  docker exec \
    -e "CWE_EXPECTED_VERSION=${cwe_expected_version}" \
    -e "NKE_E2E_DSX_IDENTITY_EXCHANGE=${exchange_enabled}" \
    -e "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${cp_flag}" \
    -e E2E_BGP_CONTAINER_NAME=bgp-containers \
    "${runner_name}" "$@"
}

run_local_leg() {
  local label_filter="$1" cp_flag="$2" suffix="$3" rc=0
  e2e_run_ginkgo_leg local_e2e_exec log "${state_dir}" \
    "${label_filter}" "${cp_flag}" "${suffix}" || rc=$?
  docker cp "${runner_name}:/state/junit${suffix}.xml" "${state_dir}/junit${suffix}.xml" >/dev/null 2>&1 || true
  return "${rc}"
}

# Cleanup gate between legs: the cp-controller leg's delete spec should have
# removed every tenant cluster before the node-pool leg starts. Count CAPI
# Cluster objects directly (not the CWE API, which enforces auth); zero of them
# is a faithful, auth-free "cluster is clean" signal.
local_kubectl() { kubectl "$@"; }

wait_for_zero_tenant_clusters() {
  e2e_wait_for_zero_tenant_clusters local_kubectl log ||
    fail "cleanup gate: tenant clusters still present"
}

# Flip the deployment-wide control-plane-controller flag with a light redeploy
# (no re-run of Tilt), wait for the rollout, and confirm the new value stuck.
redeploy_cwe() {
  local value="$1" got
  log "redeploying CWE with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${value}"
  kubectl -n cloud-workflow-engine set env deployment/cloud-workflow-engine \
    "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${value}"
  kubectl -n cloud-workflow-engine rollout status deployment/cloud-workflow-engine --timeout=5m
  got="$(kubectl -n cloud-workflow-engine get deployment/cloud-workflow-engine \
    -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER\")].value}")"
  [[ "${got}" == "${value}" ]] || \
    fail "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER must be ${value} after redeploy, got ${got:-<empty>}"
}

if [[ "${sequential_modes}" == "true" ]]; then
  log "sequential CAPD control-plane modes: cp-controller (flag-on) then node-pool (flag-off)"
  run_local_leg "${E2E_CAPD_CP_CONTROLLER_FILTER}" "true" "-cp-controller" || \
    fail "cp-controller leg failed with exit code $?; not running the node-pool leg"
  log "cp-controller leg passed; confirming the cluster is clean before the node-pool leg"
  wait_for_zero_tenant_clusters
  redeploy_cwe false
  run_local_leg "${E2E_CAPD_NODE_POOL_FILTER}" "false" "-nodepool" || \
    fail "node-pool leg failed with exit code $?"
  log "sequential CAPD control-plane E2E passed (cp-controller then node-pool)"
else
  label_filter="${LOCAL_NKE_E2E_LABEL_FILTER:-capd}"
  run_local_leg "${label_filter}" "${PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER:-}" "" || exit "$?"
  log "E2E specs matching ${label_filter} passed"
fi
