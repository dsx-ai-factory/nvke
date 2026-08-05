#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION & AFFILIATES is
# strictly prohibited.

set -Eeuo pipefail

STATE_DIR="${CAPD_E2E_STATE_DIR:-.capd-e2e}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cwe-capi}"
RUNNER_NAME="capd-e2e-runner-${CI_JOB_ID:-local}"
RUNNER_IMAGE="${CAPD_E2E_RUNNER_IMAGE:?CAPD_E2E_RUNNER_IMAGE is required}"
STATE_VOLUME="capd-e2e-state-${CI_JOB_ID:-local}"
BUNDLE_MIRROR_NAME="capd-e2e-bundle-mirror-${CI_JOB_ID:-local}"
BUNDLE_LOADER_NAME="capd-e2e-bundle-loader-${CI_JOB_ID:-local}"
BUNDLE_VOLUME="capd-e2e-bundles-${CI_JOB_ID:-local}"
TMP_DIR="$(mktemp -d)"
MANAGEMENT_KUBECONFIG="${TMP_DIR}/management.kubeconfig"
CLUSTERCTL_BIN="${TMP_DIR}/clusterctl"
TILT_BIN="${TMP_DIR}/tilt"
BUNDLE_DIR="${TMP_DIR}/bundles"
SOURCE_ARCHIVE="${TMP_DIR}/cwe-source.tgz"
E2E_BIN="${TMP_DIR}/cwe-e2e.test"
E2E_CONFIG="${TMP_DIR}/e2e-config.json"

log() {
  printf '[capd-e2e] %s\n' "$*"
}

curl_with_retry() {
  local description=$1
  shift

  if ! curl \
    --fail \
    --silent \
    --show-error \
    --location \
    --connect-timeout 10 \
    --max-time 30 \
    --retry 3 \
    --retry-delay 5 \
    --retry-connrefused \
    "$@"; then
    log "curl failed after retries: ${description}" >&2
    return 1
  fi
}

runner_exec() {
  docker exec \
    -e KUBECONFIG=/state/management.kubeconfig \
    -e DOCKER_HOST="${DOCKER_HOST:-}" \
    "${RUNNER_NAME}" "$@"
}

collect_diagnostics() {
  local rc=$?
  set +e

  mkdir -p "${STATE_DIR}"
  docker info >"${STATE_DIR}/docker-info.txt" 2>&1
  docker system df -v >"${STATE_DIR}/docker-system-df.txt" 2>&1
  docker ps -a --no-trunc >"${STATE_DIR}/docker-ps.txt" 2>&1
  docker network ls >"${STATE_DIR}/docker-networks.txt" 2>&1
  docker logs "${KIND_CLUSTER_NAME}-control-plane" >"${STATE_DIR}/kind-control-plane.log" 2>&1
  docker logs "${BUNDLE_MIRROR_NAME}" >"${STATE_DIR}/bundle-mirror.log" 2>&1
  docker logs capd-tor >"${STATE_DIR}/capd-bgp.log" 2>&1
  docker exec capd-tor vtysh -c 'show bgp summary' >"${STATE_DIR}/capd-bgp-summary.txt" 2>&1
  docker exec capd-tor vtysh -c 'show bgp ipv4 unicast' >"${STATE_DIR}/capd-bgp-routes.txt" 2>&1

  mkdir -p "${STATE_DIR}/nodes"
  while IFS= read -r node_name; do
    [[ -n "${node_name}" ]] || continue
    docker logs "${node_name}" >"${STATE_DIR}/nodes/${node_name}-console.log" 2>&1
    docker exec "${node_name}" systemctl status nke-agent spire-agent swtpm-local --no-pager \
      >"${STATE_DIR}/nodes/${node_name}-systemd.txt" 2>&1
    docker exec "${node_name}" journalctl --no-pager -u nke-agent -u spire-agent -u swtpm-local \
      >"${STATE_DIR}/nodes/${node_name}-journal.log" 2>&1
  done < <(docker ps -a --filter ancestor=cwe-capd-kindest-node:ci --format '{{.Names}}')

  if docker inspect "${RUNNER_NAME}" >/dev/null 2>&1; then
    runner_exec kubectl get nodes -o wide >"${STATE_DIR}/management-nodes.txt" 2>&1
    runner_exec kubectl get pods -A -o wide >"${STATE_DIR}/management-pods.txt" 2>&1
    runner_exec kubectl get events -A --sort-by=.lastTimestamp >"${STATE_DIR}/management-events.txt" 2>&1
    runner_exec kubectl -n capd-system logs deployment/capd-controller-manager --all-containers --tail=-1 \
      >"${STATE_DIR}/capd-controller.log" 2>&1
    runner_exec kubectl -n capd-reboot-system logs deployment/capd-reboot-controller --all-containers --tail=-1 \
      >"${STATE_DIR}/capd-reboot-controller.log" 2>&1
    runner_exec kubectl -n cloud-workflow-engine logs deployment/cloud-workflow-engine --all-containers --tail=-1 \
      >"${STATE_DIR}/cwe.log" 2>&1
    runner_exec kubectl get clusters,machines,kubeadmcontrolplanes,dockermachines -A -o yaml \
      >"${STATE_DIR}/capi-resources.yaml" 2>&1
    for artifact in tilt.log tilt-snapshot.json e2e.log junit.xml; do
      docker cp "${RUNNER_NAME}:/state/${artifact}" "${STATE_DIR}/${artifact}" >/dev/null 2>&1 || true
    done
  fi

  docker rm -f "${RUNNER_NAME}" >/dev/null 2>&1
  docker stop --time 10 capd-tor >/dev/null 2>&1
  docker rm -f capd-tor >/dev/null 2>&1
  docker rm -f "${BUNDLE_MIRROR_NAME}" "${BUNDLE_LOADER_NAME}" >/dev/null 2>&1
  kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
  docker volume rm "${STATE_VOLUME}" >/dev/null 2>&1
  docker volume rm "${BUNDLE_VOLUME}" >/dev/null 2>&1
  rm -rf "${TMP_DIR}"
  return "${rc}"
}
trap collect_diagnostics EXIT

mkdir -p "${STATE_DIR}"

: "${CI_API_V4_URL:?CI_API_V4_URL is required}"
: "${CI_JOB_TOKEN:?CI_JOB_TOKEN is required}"
: "${CI_SERVER_HOST:?CI_SERVER_HOST is required}"
: "${CAPI_CORE_VERSION:?CAPI_CORE_VERSION is required}"
: "${CAPD_PROVIDER_VERSION:?CAPD_PROVIDER_VERSION is required}"
: "${CLUSTERCTL_SHA256:?CLUSTERCTL_SHA256 is required}"
: "${TILT_VERSION:?TILT_VERSION is required}"
: "${TILT_SHA256:?TILT_SHA256 is required}"

CWE_IMAGE="${CWE_IMAGE:-cwe-mr:${CI_COMMIT_SHORT_SHA:-local}}"
CWE_BUILD_VERSION="${CWE_BUILD_VERSION:-mr-${CI_COMMIT_SHORT_SHA:-local}}"
GITCREDS_FILE="${TMP_DIR}/gitcreds"

log "building MR image ${CWE_IMAGE} for the AMD64 CAPD runner"
printf 'machine %s\nlogin gitlab-ci-token\npassword %s\n' \
  "${CI_SERVER_HOST}" "${CI_JOB_TOKEN}" >"${GITCREDS_FILE}"
DOCKER_BUILDKIT=1 docker build \
  --platform linux/amd64 \
  --secret "id=gitcreds,src=${GITCREDS_FILE}" \
  --build-arg "BUILD_VERSION=${CWE_BUILD_VERSION}" \
  --build-arg "GIT_VERSION=${CI_COMMIT_SHA:-}" \
  --tag "${CWE_IMAGE}" \
  .
rm -f "${GITCREDS_FILE}"
printf '%s\n' "${CWE_IMAGE}" >"${STATE_DIR}/cwe-image-ref.txt"
docker image inspect "${CWE_IMAGE}" >"${STATE_DIR}/cwe-image.json"

log "resolving latest released node-agent package"
packages_json="$(curl_with_retry "fetching node-agent package list" \
  --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
  "${CI_API_V4_URL}/projects/120765/packages?package_type=generic&package_name=nke-agent&order_by=created_at&sort=desc&per_page=100")"
NODE_AGENT_VERSION="${NODE_AGENT_VERSION:-$(
  jq -r '.[] | .version | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' <<<"${packages_json}" \
    | sort -V \
    | tail -1
)}"
if [[ -z "${NODE_AGENT_VERSION}" ]]; then
  log "no released node-agent semantic version was returned by the package API"
  exit 1
fi
printf '%s\n' "${NODE_AGENT_VERSION}" >"${STATE_DIR}/node-agent-version.txt"
log "using node-agent ${NODE_AGENT_VERSION}"

node_agent_package_id="$(jq -r --arg version "${NODE_AGENT_VERSION}" \
  'first(.[] | select(.version == $version) | .id) // empty' <<<"${packages_json}")"
if [[ -z "${node_agent_package_id}" ]]; then
  log "package metadata was not returned for node-agent ${NODE_AGENT_VERSION}"
  exit 1
fi
node_agent_files="$(curl_with_retry "fetching node-agent package files" \
  --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
  "${CI_API_V4_URL}/projects/120765/packages/${node_agent_package_id}/package_files")"
node_agent_filename="agent_${NODE_AGENT_VERSION}_amd64.tgz"
node_agent_sha256="$(jq -r --arg name "${node_agent_filename}" \
  'first(.[] | select(.file_name == $name) | .file_sha256) // empty' <<<"${node_agent_files}")"
if [[ ! "${node_agent_sha256}" =~ ^[0-9a-f]{64}$ ]]; then
  log "package checksum was not returned for ${node_agent_filename}"
  exit 1
fi
node_agent_bundle="${TMP_DIR}/${node_agent_filename}"
curl_with_retry "downloading ${node_agent_filename}" \
  --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
  "${CI_API_V4_URL}/projects/120765/packages/generic/nke-agent/${NODE_AGENT_VERSION}/${node_agent_filename}" \
  -o "${node_agent_bundle}"
printf '%s  %s\n' "${node_agent_sha256}" "${node_agent_bundle}" | sha256sum -c -

log "preparing verified SPIRE and local Calico bundles"
bash hack/ci/prepare-capd-bundles.sh "${BUNDLE_DIR}"

log "installing clusterctl ${CAPD_PROVIDER_VERSION}"
arch="$(uname -m)"
case "${arch}" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) log "unsupported runner architecture ${arch}"; exit 1 ;;
esac
if [[ "${arch}" != amd64 || ! "${CLUSTERCTL_SHA256}" =~ ^[0-9a-f]{64}$ ]]; then
  log "clusterctl checksum is not configured for linux/${arch}"
  exit 1
fi
clusterctl_url="https://github.com/kubernetes-sigs/cluster-api/releases/download/${CAPD_PROVIDER_VERSION}/clusterctl-linux-${arch}"
curl_with_retry "downloading clusterctl ${CAPD_PROVIDER_VERSION}" \
  "${clusterctl_url}" \
  -o "${CLUSTERCTL_BIN}"
printf '%s  %s\n' "${CLUSTERCTL_SHA256}" "${CLUSTERCTL_BIN}" | sha256sum -c -
chmod +x "${CLUSTERCTL_BIN}"

log "installing Tilt ${TILT_VERSION}"
tilt_asset_name="tilt.${TILT_VERSION#v}.linux.x86_64.tar.gz"
if [[ ! "${TILT_SHA256}" =~ ^[0-9a-f]{64}$ ]]; then
  log "Tilt checksum is invalid for ${TILT_VERSION}"
  exit 1
fi
tilt_url="https://github.com/tilt-dev/tilt/releases/download/${TILT_VERSION}/${tilt_asset_name}"
tilt_archive="${TMP_DIR}/${tilt_asset_name}"
curl_with_retry "downloading Tilt ${TILT_VERSION}" \
  "${tilt_url}" \
  -o "${tilt_archive}"
printf '%s  %s\n' "${TILT_SHA256}" "${tilt_archive}" | sha256sum -c -
tar -xzf "${tilt_archive}" -C "${TMP_DIR}" tilt
chmod +x "${TILT_BIN}"

log "archiving the MR source for the inner runner"
git archive --format=tar.gz --output="${SOURCE_ARCHIVE}" HEAD

log "compiling the CAPD E2E suite"
e2e_built=false
for attempt in 1 2 3; do
  if (cd test/e2e && GOWORK=off go test -c -o "${E2E_BIN}" .); then
    e2e_built=true
    break
  fi
  log "E2E compile attempt ${attempt} failed"
  sleep 5
done
if [[ "${e2e_built}" != true ]]; then
  exit 1
fi

log "building CAPD node image with ${NODE_AGENT_VERSION}"
CAPD_NODE_AGENT_LOCAL_BUILD=false \
NODE_AGENT_VERSION="${NODE_AGENT_VERSION}" \
NODE_AGENT_BUNDLE_PATH="${node_agent_bundle}" \
SPIRE_AGENT_BUNDLE_PATH="${BUNDLE_DIR}/spire-agent.tgz" \
CAPD_SPIRE_ATTESTATION_METHOD=joinToken \
BUNDLE_SERVER_URL="http://${BUNDLE_MIRROR_NAME}/" \
INDEX_FILE_URL="http://${BUNDLE_MIRROR_NAME}/index.json" \
CAPD_KINDEST_NODE_IMAGE=cwe-capd-kindest-node:ci \
CAPD_KINDEST_NODE_PLATFORM="${CAPD_KINDEST_NODE_PLATFORM:-linux/amd64}" \
  bash hack/capd-kindest-node/build-image.sh
docker image inspect cwe-capd-kindest-node:ci >"${STATE_DIR}/capd-node-image.json"

log "creating Kind management cluster ${KIND_CLUSTER_NAME}"
kind create cluster \
  --name "${KIND_CLUSTER_NAME}" \
  --config hack/cwe-capi-kind.yaml \
  --wait 5m
kind load docker-image --name "${KIND_CLUSTER_NAME}" "${CWE_IMAGE}"
kind get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" >"${MANAGEMENT_KUBECONFIG}"

log "starting the CAPD bundle mirror on the Kind Docker network"
docker volume create "${BUNDLE_VOLUME}" >/dev/null
docker create --name "${BUNDLE_LOADER_NAME}" -v "${BUNDLE_VOLUME}:/bundle" alpine:3.22 true >/dev/null
docker cp "${BUNDLE_DIR}/." "${BUNDLE_LOADER_NAME}:/bundle/"
docker rm "${BUNDLE_LOADER_NAME}" >/dev/null
docker run -d \
  --name "${BUNDLE_MIRROR_NAME}" \
  --network kind \
  -v "${BUNDLE_VOLUME}:/usr/share/nginx/html:ro" \
  nginx:alpine >/dev/null

log "starting inner runner on the Kind Docker network"
docker pull "${RUNNER_IMAGE}"
docker create \
  --name "${RUNNER_NAME}" \
  --network kind \
  --add-host=docker:host-gateway \
  -e DOCKER_HOST="${DOCKER_HOST:-}" \
  -v "${STATE_VOLUME}:/state" \
  "${RUNNER_IMAGE}" sleep infinity >/dev/null
docker start "${RUNNER_NAME}" >/dev/null
docker cp "${MANAGEMENT_KUBECONFIG}" "${RUNNER_NAME}:/state/management.kubeconfig"
docker cp "${CLUSTERCTL_BIN}" "${RUNNER_NAME}:/usr/local/bin/clusterctl"
docker cp "${TILT_BIN}" "${RUNNER_NAME}:/usr/local/bin/tilt"
docker cp "${SOURCE_ARCHIVE}" "${RUNNER_NAME}:/state/cwe-source.tgz"
docker cp "${E2E_BIN}" "${RUNNER_NAME}:/state/cwe-e2e.test"

runner_exec mkdir -p /workspace/cloud-workflow-engine
runner_exec tar -xzf /state/cwe-source.tgz -C /workspace/cloud-workflow-engine

runner_exec bash -lc '
  set -euo pipefail
  if ! command -v nc >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -qq -y --no-install-recommends netcat-openbsd
    rm -rf /var/lib/apt/lists/*
  fi
'
runner_exec bash -lc '
  set -euo pipefail
  for tool in docker helm kind kubectl nc tar tilt; do
    command -v "${tool}" >/dev/null || {
      echo "${tool} is required in the inner runner" >&2
      exit 1
    }
  done
'

runner_exec kubectl wait --for=condition=Ready node --all --timeout=2m
docker exec "${KIND_CLUSTER_NAME}-control-plane" test -S /var/run/docker.sock

log "installing CAPI core ${CAPI_CORE_VERSION} and CAPD ${CAPD_PROVIDER_VERSION}"
runner_exec env CLUSTER_TOPOLOGY=true clusterctl init \
  --core "cluster-api:${CAPI_CORE_VERSION}" \
  --infrastructure "docker:${CAPD_PROVIDER_VERSION}"
runner_exec kubectl rollout status deployment/capi-controller-manager -n capi-system --timeout=5m
runner_exec kubectl rollout status deployment/capd-controller-manager -n capd-system --timeout=5m

management_vip="$(docker inspect \
  --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}' \
  "${KIND_CLUSTER_NAME}-control-plane")"
if [[ -z "${management_vip}" ]]; then
  log "Kind control-plane IP was not found"
  exit 1
fi
jq -n \
  --arg cwe_image "${CWE_IMAGE}" \
  --arg site_controller_vip "${management_vip}" \
  '{
    providers: ["capi_core", "capd", "capd-reboot-controller"],
    cwe_image: $cwe_image,
    site_controller_vip: $site_controller_vip
  }' >"${TMP_DIR}/tilt_config.json"
docker cp "${TMP_DIR}/tilt_config.json" \
  "${RUNNER_NAME}:/workspace/cloud-workflow-engine/tilt_config.json"
jq -n \
  --arg kubeconfig /state/management.kubeconfig \
  --arg context "kind-${KIND_CLUSTER_NAME}" \
  '{
    kube: {
      kubeconfig: $kubeconfig,
      context: $context
    },
    cluster: {
      capdNodeImage: "cwe-capd-kindest-node:ci"
    },
    lifecycle: {
      preserveOnFailure: true
    }
  }' >"${E2E_CONFIG}"
docker cp "${E2E_CONFIG}" "${RUNNER_NAME}:/state/e2e-config.json"

log "deploying the management plane with Tilt"
runner_exec env \
  KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
  TENANT_ACCESS_ENABLED="${TENANT_ACCESS_ENABLED:-true}" \
  NKE_OIDC_AUTH_ENABLED="${NKE_OIDC_AUTH_ENABLED:-true}" \
  TILT_DISABLE_ANALYTICS=1 \
  bash -lc '
    set -euo pipefail
    cd /workspace/cloud-workflow-engine
    if ! tilt ci \
      --timeout=40m \
      --output-snapshot-on-exit=/state/tilt-snapshot.json \
      >/state/tilt.log 2>&1; then
      tail -n 250 /state/tilt.log >&2
      exit 1
    fi
  '

runner_exec kubectl -n cloud-workflow-engine rollout status deployment/cloud-workflow-engine --timeout=5m
for env_name in TENANT_ACCESS_ENABLED NKE_OIDC_AUTH_ENABLED; do
  env_value="$(runner_exec kubectl -n cloud-workflow-engine get deployment/cloud-workflow-engine \
    -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"${env_name}\")].value}")"
  if [[ "${env_value}" != "true" ]]; then
    log "${env_name} must be true in the CAPD E2E deployment, got ${env_value:-<empty>}"
    exit 1
  fi
done
runner_exec kubectl get --raw \
  /api/v1/namespaces/cloud-workflow-engine/services/cloud-workflow-engine:8080/proxy/api/version \
  >"${STATE_DIR}/cwe-version.json"

log "running all CAPD E2E scenarios"
runner_exec bash -lc '
  set -euo pipefail
  /state/cwe-e2e.test \
    -test.v \
    -test.timeout=105m \
    -e2e.config=/state/e2e-config.json \
    -ginkgo.v \
    -ginkgo.no-color \
    -ginkgo.timeout=100m \
    -ginkgo.label-filter=capd \
    -ginkgo.junit-report=/state/junit.xml \
    2>&1 | tee /state/e2e.log
'

log "CAPD management-plane E2E passed"
