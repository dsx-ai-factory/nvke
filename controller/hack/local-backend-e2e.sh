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
mkdir -p "${state_dir}"
# Keep Docker bind-mounted build output under the repository. Colima does not
# share macOS's private /var/folders tree, so a default mktemp directory makes
# packages created by the Linux OVN builder invisible to the host.
tmp_dir="$(mktemp -d "${state_dir}/tmp.XXXXXX")"
tmp_dir="$(cd "${tmp_dir}" && pwd)"
kind_cluster_name="${KIND_CLUSTER_NAME:-controller-capi}"
dockerhub_mirror="${NKE_E2E_DOCKERHUB_MIRROR:-}"
runner_name="cwe-capd-local-e2e-${$}"
runner_image="${LOCAL_NKE_E2E_RUNNER_IMAGE:-docker:29.1.3-cli@sha256:4fa0ee1f3a7e4354c4ea34558b6d4ee32859baf4973d4c8ccc8e7fe3dd730c04}"
tilt_config="${repo_root}/tilt_config.json"
tilt_config_backup="${tmp_dir}/tilt_config.json"
host_kubeconfig="${LOCAL_NKE_E2E_KUBECONFIG:-${tmp_dir}/management-host.kubeconfig}"
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
  docker logs "${BGP_CONTAINER_NAME:-bgp-containers}" >"${state_dir}/bgp-containers.log" 2>&1
  docker exec "${BGP_CONTAINER_NAME:-bgp-containers}" vtysh -c 'show bgp summary' >"${state_dir}/bgp-containers-summary.txt" 2>&1
  docker exec "${BGP_CONTAINER_NAME:-bgp-containers}" vtysh -c 'show bgp ipv4 unicast' >"${state_dir}/bgp-containers-routes.txt" 2>&1
  if [[ "${management_ready}" == true ]]; then
    kubectl get nodes -o wide >"${state_dir}/management-nodes.txt" 2>&1
    kubectl get pods -A -o wide >"${state_dir}/management-pods.txt" 2>&1
    kubectl get events -A --sort-by=.lastTimestamp >"${state_dir}/management-events.txt" 2>&1
    kubectl get clusters,machines,kubeadmcontrolplanes,dockermachines -A -o yaml \
      >"${state_dir}/capi-resources.yaml" 2>&1
    kubectl -n nvke-controller logs deployment/nvke-controller --all-containers --tail=-1 \
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
if [[ -n "${dockerhub_mirror}" && ! "${dockerhub_mirror}" =~ ^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  fail "NKE_E2E_DOCKERHUB_MIRROR must be a registry host with an optional port"
fi

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
export NODE_AGENT_SPIRE_ATTESTATION_METHOD="${CAPD_SPIRE_ATTESTATION_METHOD}"
log "SPIRE node attestation: ${CAPD_SPIRE_ATTESTATION_METHOD}"

[[ -f "${NODE_AGENT_REPO_PATH}/Makefile" ]] || \
  fail "NODE_AGENT_REPO_PATH=${NODE_AGENT_REPO_PATH} does not look like an agent checkout"

if curl --fail --silent --show-error --connect-timeout 1 --max-time 1 \
  http://localhost:10350/api/view >/dev/null 2>&1; then
  fail "another Tilt session is running on port 10350; stop it before running this script"
fi

if [[ "${LOCAL_NVKE_E2E_REUSE_DOCKER_HOST:-false}" == true ]]; then
  docker info >/dev/null 2>&1 || fail "the existing Docker daemon is unavailable"
elif [[ "${host_os}" == darwin ]]; then
  log "starting Colima with ${COLIMA_CPUS} CPUs, ${COLIMA_MEMORY_GIB} GiB memory, and ${COLIMA_DISK_GIB} GiB disk"
  make -C "${repo_root}" host-up
else
  log "using the host Docker daemon"
  # `make host-up` loads /dev/vtpmx for a developer. This lane still falls back
  # to join-token attestation when the device is absent, which is the GitHub
  # runner case.
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

# Before the cluster and the builds, for the reason given in backend-e2e-lib.sh:
# the answer is a property of this host's kernel, so nothing below changes it,
# and a host that cannot load the module has already lost the suite.
log "checking this host can load the openvswitch module"
ovs_probe_image="$(e2e_capd_base_image "${repo_root}")" \
  || fail "could not resolve the CAPD base image to probe with"
e2e_host_loads_openvswitch "${ovs_probe_image}" ||
  fail "this host cannot load the openvswitch module (#1515)"

log "ensuring the Kind management cluster exists"
kind_config_for_mirror "${repo_root}/hack/controller-capi-kind.yaml" "${tmp_dir}/kind-config.yaml"
[[ "${host_kubeconfig}" == /* ]] || fail "LOCAL_NKE_E2E_KUBECONFIG must be an absolute path"
mkdir -p "$(dirname "${host_kubeconfig}")"
existing_kind_clusters="$(kind get clusters)"
if grep -Fx "${kind_cluster_name}" <<<"${existing_kind_clusters}" >/dev/null; then
  kind get kubeconfig --name "${kind_cluster_name}" >"${host_kubeconfig}"
fi
export KUBECONFIG="${host_kubeconfig}"
make -C "${repo_root}" kind-up KIND_CLUSTER_CONFIG="${tmp_dir}/kind-config.yaml"
configure_kind_dockerhub_mirror "${kind_cluster_name}"
kind get kubeconfig --name "${kind_cluster_name}" >"${host_kubeconfig}"
management_ready=true

log "building the CAPD bundle from public sources"
# Not under state_dir: that directory is what CI uploads on failure, and these
# tarballs are 22 MB of the 31 MB it collected, all of it re-downloadable.
bundle_dir="${tmp_dir}/bundles"
rm -rf "${bundle_dir}"
CNI_PLUGIN=ovn-kubernetes BUNDLE_ARCH="${host_arch}" bash "${repo_root}/hack/ci/prepare-capd-bundles.sh" "${bundle_dir}"
if [[ "${LOCAL_NVKE_E2E_NODE_AGENT_UPGRADE:-false}" == true ]]; then
  bash "${repo_root}/hack/prepare-node-agent-e2e.sh" "${bundle_dir}" "${host_arch}"
fi

export SPIRE_AGENT_BUNDLE_PATH="${bundle_dir}/spire-agent.tgz"
CAPD_CNI_CHARTS_DEB_PATH="${bundle_dir}/nvke-cni-ovn-charts-2.4.2_2.4.2_${host_arch}.deb"
export CAPD_CNI_CHARTS_DEB_PATH
export INSTALL_CNI_AFTER_INIT="${INSTALL_CNI_AFTER_INIT:-false}"
export CAPD_CNI_IMAGES_DEB_PATH="${bundle_dir}/nvke-cni-ovn-images-2.4.2_2.4.2_${host_arch}.deb"
export CAPD_HELM_DEB_PATH="${bundle_dir}/nke-e2e-helm.deb"
[[ -f "${CAPD_CNI_CHARTS_DEB_PATH}" ]] \
  || fail "prepare-capd-bundles.sh produced no OVN charts package"

# With INSTALL_CNI_AFTER_INIT the agent reads CNI from the OVN charts deb
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
  # Prove it serves before building an image that bakes the URL in. Poll rather
  # than probe once: nginx has only just been started, and a single wget raced
  # its first socket bind and failed a run with "Connection refused".
  docker run --rm --network kind "$(dockerhub_image alpine:3.22)" sh -c \
    'for _ in $(seq 30); do wget -q -O /dev/null "$1" && exit 0; sleep 1; done; exit 1' \
    _ "${INDEX_FILE_URL}" \
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
elif [[ "${LOCAL_NKE_E2E_REUSE_CAPD_NODE_IMAGE:-false}" == true ]]; then
  node_image="${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}"
  docker image inspect "${node_image}" >/dev/null || fail "CAPD node image ${node_image} does not exist"
  export CAPD_KINDEST_NODE_IMAGE_PREBUILT=true
  make -C "${repo_root}" clusterctl-install
  make -C "${repo_root}" registry-mirror REGISTRY_MIRROR_IMAGE="$(dockerhub_image registry:2)"
  capi_core_version="$(make -C "${repo_root}" -s --no-print-directory print-CAPI_CORE_VERSION)"
  capd_provider_version="$(make -C "${repo_root}" -s --no-print-directory print-CAPD_PROVIDER_VERSION)"
  PATH="${repo_root}/.local/bin:${PATH}" CLUSTER_TOPOLOGY=true clusterctl init \
    --core "cluster-api:${capi_core_version}" \
    --bootstrap "kubeadm:${capi_core_version}" \
    --control-plane "kubeadm:${capi_core_version}" \
    --infrastructure "docker:${capd_provider_version}"
  kubectl rollout status deployment/capi-controller-manager -n capi-system --timeout=300s
  kubectl rollout status deployment/capd-controller-manager -n capd-system --timeout=300s
  make -C "${repo_root}" capi-spiffe-exec-auth
  log "installed CAPD with the cached node image ${node_image}"
else
  make -C "${repo_root}" capd-up REGISTRY_MIRROR_IMAGE="$(dockerhub_image registry:2)"
fi

if [[ "${LOCAL_NVKE_E2E_NODE_AGENT_UPGRADE:-false}" == true ]]; then
  qualification_image_dir="${tmp_dir}/agent-qualification-image"
  mkdir -p "${qualification_image_dir}"
  cat >"${qualification_image_dir}/Dockerfile" <<'DOCKERFILE'
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY kubeadm-record-version /usr/local/sbin/kubeadm-record-version
RUN if [ ! -e /usr/local/sbin/kubeadm-container-original ]; then \
      mv /usr/local/sbin/kubeadm-systemverification-shim /usr/local/sbin/kubeadm-container-original; \
    fi && \
    chmod 0755 /usr/local/sbin/kubeadm-record-version && \
    ln -sf /usr/local/sbin/kubeadm-record-version /usr/local/sbin/kubeadm-systemverification-shim
DOCKERFILE
  cat >"${qualification_image_dir}/kubeadm-record-version" <<'WRAPPER'
#!/bin/sh
set -eu
case "$1" in
  init|join)
    mkdir -p /var/lib/nvke
    printf '%s %s\n' "$1" "$(/usr/local/bin/nvke-agent --version)" >>/var/lib/nvke/e2e-kubeadm-agent-versions.log
    ;;
esac
exec /usr/local/sbin/kubeadm-container-original "$@"
WRAPPER
  node_image="${CAPD_NODE_IMAGE:-cwe-capd-kindest-node:local}"
  qualification_image="nvke-capd-agent-qualification:$(basename "${tmp_dir}")"
  docker build --build-arg "BASE_IMAGE=${node_image}" -t "${qualification_image}" "${qualification_image_dir}"
  export CAPD_NODE_IMAGE="${qualification_image}"
fi

# The committed fixture, not the provider checkout: this script installs the
# CRDs but never builds the provider, so requiring a checkout would gate the
# whole entrypoint on fetching one. (It was also private when this was written,
# which made that gate an internal-access gate.) Same call the gofail suite
# already made -- see test/fixtures/capnico-crds/README.md.
# CAPNICO_SOURCE_DIR still wins when it is set.
if [[ -n "${CAPNICO_SOURCE_DIR:-}" ]]; then
  capnico_crd_dir="${CAPNICO_SOURCE_DIR}/config/crd/bases"
else
  capnico_crd_dir="${repo_root}/test/fixtures/capnico-crds"
fi
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
  agent_args="$(docker run --rm --entrypoint cat "${node_image}" /etc/default/nvke-agent)" \
    || fail "could not read /etc/default/nvke-agent from ${node_image}"
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

# `|` makes go fall back to a direct fetch on any proxy error; the default
# `,` only does so on 404/410. Set before `tilt ci`: its cwe-compile resource
# compiles the controller. See the same block in hack/ci/backend-e2e.sh.
export GOPROXY="${GOPROXY:-https://proxy.golang.org|direct}"

log "deploying local controller through Tilt"
TILT_DISABLE_ANALYTICS=1 tilt ci \
  --port=0 \
  --timeout=40m \
  --output-snapshot-on-exit="${state_dir}/tilt-snapshot.json" \
  >"${state_dir}/tilt.log" 2>&1
if [[ "${LOCAL_NKE_E2E_BUILD_TAGS:-}" =~ (^|[[:space:],])oidc_acceptance($|[[:space:],]) ]]; then
  log "checking the SPIRE exec-auth plugin in CAPI and CAPD controllers"
  if [[ "${LOCAL_NKE_E2E_REUSE_CAPD_SETUP:-false}" == true ]]; then
    make -C "${repo_root}" capi-spiffe-exec-auth
  fi
  for controller in \
    "capi-system capi-controller-manager" \
    "capi-kubeadm-control-plane-system capi-kubeadm-control-plane-controller-manager" \
    "capi-kubeadm-bootstrap-system capi-kubeadm-bootstrap-controller-manager" \
    "capd-system capd-controller-manager"; do
    read -r namespace deployment <<<"${controller}"
    kubectl rollout status "deployment/${deployment}" -n "${namespace}" --timeout=5m
    kubectl exec "deployment/${deployment}" -n "${namespace}" -- \
      /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth --help >/dev/null 2>&1 || \
      fail "SPIRE exec-auth plugin is unavailable in ${namespace}/deployment/${deployment}"
  done
fi

log "compiling the E2E suite for linux/${host_arch}"
e2e_built=false
e2e_build_flags=()
if [[ -n "${LOCAL_NKE_E2E_BUILD_TAGS:-}" ]]; then
  e2e_build_flags=(-tags="${LOCAL_NKE_E2E_BUILD_TAGS}")
fi
for attempt in 1 2 3; do
  if (
    cd "${repo_root}/test/e2e"
    GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${host_arch}" \
      go test -c ${e2e_build_flags[@]+"${e2e_build_flags[@]}"} -o "${e2e_binary}" .
  ); then
    e2e_built=true
    break
  fi
  log "E2E compile attempt ${attempt} failed"
  sleep $((attempt * 20))
done
[[ "${e2e_built}" == true ]] || fail "compiling the E2E suite failed three times"

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
  pull_with_retry "${runner_image}" >/dev/null || fail "could not pull the E2E runner image ${runner_image}"
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
oidc_issuer_host=""
if [[ "${LOCAL_NKE_E2E_BUILD_TAGS:-}" =~ (^|[[:space:],])oidc_acceptance($|[[:space:],]) ]]; then
  oidc_issuer_host="$(docker inspect --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "${runner_name}")"
  [[ -n "${oidc_issuer_host}" ]] || fail "could not read the E2E runner IP on the Kind network"
fi

# Tilt builds the controller with `make build`, which bakes BUILD_VERSION in, so ask the
# Makefile for the same value rather than deriving it again here. Without it the
# health suite's version spec skips itself and the run still reports success.
cwe_expected_version="$(make -C "${repo_root}" -s --no-print-directory print-BUILD_VERSION)"

# Exec adapter for the shared Ginkgo runner: locally the suite runs in a container
# on the Kind network, reached with `docker exec`.
local_e2e_exec() {
  local -a oidc_env=()
  local e2e_bundle_server_url="${BUNDLE_SERVER_URL:-}"
  if [[ "${INSTALL_CNI_AFTER_INIT:-false}" == "true" ]]; then
    e2e_bundle_server_url=""
  fi
  local exchange_enabled=false
  if jq -e 'index("dsx-identity-exchange") != null' <<<"${local_e2e_providers}" >/dev/null; then
    exchange_enabled=true
  fi
  if [[ -n "${oidc_issuer_host}" ]]; then
    oidc_env=(-e "NKE_E2E_OIDC_ISSUER_HOST=${oidc_issuer_host}")
  fi
  docker exec \
    -e "NVKE_CONTROLLER_EXPECTED_VERSION=${cwe_expected_version}" \
    -e "NKE_E2E_DSX_IDENTITY_EXCHANGE=${exchange_enabled}" \
    -e "NKE_E2E_BUNDLE_SERVER_URL=${e2e_bundle_server_url}" \
    -e "NVKE_E2E_NODE_AGENT_BUNDLE_VOLUME=${bundle_mirror_volume}" \
    -e "NVKE_E2E_NODE_AGENT_UPGRADE=${LOCAL_NVKE_E2E_NODE_AGENT_UPGRADE:-false}" \
    -e "E2E_BGP_CONTAINER_NAME=${BGP_CONTAINER_NAME:-bgp-containers}" \
    ${oidc_env[@]+"${oidc_env[@]}"} \
    "${runner_name}" "$@"
}

run_local_e2e() {
  local label_filter="$1" rc=0
  # The nvkectl row drives the CLI, so it needs nvkectl and kubectl inside the
  # runner. ci/backend-e2e.sh builds and stages both; this script stages only
  # the test binary, the kubeconfig and the config, and its runner image
  # carries neither. Without this the row would skip, and the skip audit in
  # e2e_run_ginkgo fails the run on a skip reported from an It.
  #
  # Excluded here rather than in the shared filters, so CI -- where both
  # binaries are present -- still runs it. Stage nvkectl and a linux kubectl
  # into the runner to drop this.
  label_filter="(${label_filter}) && !nvkectl"
  e2e_run_ginkgo local_e2e_exec log "${state_dir}" "${label_filter}" || rc=$?
  docker cp "${runner_name}:/state/junit.xml" "${state_dir}/junit.xml" >/dev/null 2>&1 || true
  return "${rc}"
}

# The full suite by default, as CI's full runs do. Set LOCAL_NKE_E2E_LABEL_FILTER
# to run a narrower filter.
label_filter="${LOCAL_NKE_E2E_LABEL_FILTER:-${E2E_CAPD_SUITE_FILTER}}"
run_local_e2e "${label_filter}" || exit "$?"
log "E2E specs matching ${label_filter} passed"
