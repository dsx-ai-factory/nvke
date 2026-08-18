#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
state_dir="${LOCAL_CAPD_E2E_STATE_DIR:-${repo_root}/.local/capd-e2e}"
tmp_dir="$(mktemp -d)"
kind_cluster_name="${KIND_CLUSTER_NAME:-cwe-capi}"
runner_name="cwe-capd-local-e2e-${$}"
runner_image="${LOCAL_CAPD_E2E_RUNNER_IMAGE:-alpine:3.22}"
tilt_config="${repo_root}/tilt_config.json"
tilt_config_backup="${tmp_dir}/tilt_config.json"
host_kubeconfig="${tmp_dir}/management-host.kubeconfig"
internal_kubeconfig="${tmp_dir}/management-internal.kubeconfig"
e2e_config="${tmp_dir}/e2e-config.json"
e2e_binary="${tmp_dir}/cwe-e2e.test"
had_tilt_config=false
tilt_config_staged=false
management_ready=false

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

for command in curl docker go jq kind kubectl make tilt; do
  require_command "${command}"
done

[[ "$(uname -s)" == Darwin ]] || fail "this entrypoint currently supports macOS with Colima"

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

: "${COLIMA_PROFILE:=default}"
: "${COLIMA_CPUS:=10}"
: "${COLIMA_MEMORY_GIB:=24}"
: "${COLIMA_DISK_GIB:=150}"
: "${COLIMA_DOCKER_HOST:=unix://${HOME}/.colima/${COLIMA_PROFILE}/docker.sock}"
: "${DOCKER_HOST:=${COLIMA_DOCKER_HOST}}"
: "${NODE_AGENT_REPO_PATH:=${repo_root}/../agent}"

export COLIMA_PROFILE COLIMA_CPUS COLIMA_MEMORY_GIB COLIMA_DISK_GIB
export COLIMA_DOCKER_HOST DOCKER_HOST NODE_AGENT_REPO_PATH
export KIND_CLUSTER_NAME="${kind_cluster_name}"
export CAPD_NODE_AGENT_LOCAL_BUILD="${CAPD_NODE_AGENT_LOCAL_BUILD:-true}"
export CAPD_SPIRE_ATTESTATION_METHOD="${CAPD_SPIRE_ATTESTATION_METHOD:-tpm}"

[[ -f "${NODE_AGENT_REPO_PATH}/Makefile" ]] || \
  fail "NODE_AGENT_REPO_PATH=${NODE_AGENT_REPO_PATH} does not look like an agent checkout"

mkdir -p "${state_dir}"

if curl --fail --silent --show-error --connect-timeout 1 --max-time 1 \
  http://localhost:10350/api/view >/dev/null 2>&1; then
  fail "another Tilt session is running on port 10350; stop it before running this script"
fi

log "starting Colima with ${COLIMA_CPUS} CPUs, ${COLIMA_MEMORY_GIB} GiB memory, and ${COLIMA_DISK_GIB} GiB disk"
make -C "${repo_root}" colima-up

log "ensuring the Kind management cluster exists"
make -C "${repo_root}" kind-up
kind get kubeconfig --name "${kind_cluster_name}" >"${host_kubeconfig}"
export KUBECONFIG="${host_kubeconfig}"
management_ready=true

log "building the local agent CAPD image and installing CAPI/CAPD"
make -C "${repo_root}" capd-up

if [[ -f "${tilt_config}" ]]; then
  cp "${tilt_config}" "${tilt_config_backup}"
  had_tilt_config=true
  jq '. + {providers: ["capi_core", "bgp-containers", "capd"]}' \
    "${tilt_config_backup}" >"${tilt_config}"
else
  jq -n '{providers: ["capi_core", "bgp-containers", "capd"]}' >"${tilt_config}"
fi
tilt_config_staged=true

log "deploying local CWE through Tilt"
TILT_DISABLE_ANALYTICS=1 tilt ci \
  --port=0 \
  --timeout=40m \
  --output-snapshot-on-exit="${state_dir}/tilt-snapshot.json" \
  >"${state_dir}/tilt.log" 2>&1

case "$(uname -m)" in
  arm64 | aarch64) e2e_arch=arm64 ;;
  x86_64) e2e_arch=amd64 ;;
  *) fail "unsupported local architecture $(uname -m)" ;;
esac

log "compiling the E2E suite for linux/${e2e_arch}"
(
  cd "${repo_root}/test/e2e"
  GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${e2e_arch}" \
    go test -c -o "${e2e_binary}" .
)

kind get kubeconfig --internal --name "${kind_cluster_name}" >"${internal_kubeconfig}"
jq -n \
  --arg kubeconfig /state/management.kubeconfig \
  --arg context "kind-${kind_cluster_name}" \
  '{
    kube: {kubeconfig: $kubeconfig, context: $context},
    cluster: {capdNodeImage: "cwe-capd-kindest-node:local"},
    lifecycle: {preserveOnFailure: true}
  }' >"${e2e_config}"

log "starting the E2E client on the Kind Docker network"
docker pull "${runner_image}" >/dev/null
docker create --name "${runner_name}" --network kind "${runner_image}" sleep infinity >/dev/null
docker start "${runner_name}" >/dev/null
docker exec "${runner_name}" mkdir -p /state
docker cp "${e2e_binary}" "${runner_name}:/state/cwe-e2e.test"
docker cp "${internal_kubeconfig}" "${runner_name}:/state/management.kubeconfig"
docker cp "${e2e_config}" "${runner_name}:/state/e2e-config.json"

label_filter="${LOCAL_CAPD_E2E_LABEL_FILTER:-capd}"
log "running E2E specs matching ${label_filter}"
set +e
docker exec "${runner_name}" \
  /state/cwe-e2e.test \
    -test.v \
    -test.timeout=105m \
    -e2e.config=/state/e2e-config.json \
    -ginkgo.v \
    -ginkgo.no-color \
    -ginkgo.timeout=100m \
    "-ginkgo.label-filter=${label_filter}" \
    -ginkgo.junit-report=/state/junit.xml \
  2>&1 | tee "${state_dir}/e2e.log"
e2e_rc=${PIPESTATUS[0]}
set -e

docker cp "${runner_name}:/state/junit.xml" "${state_dir}/junit.xml" >/dev/null 2>&1 || true
((e2e_rc == 0)) || exit "${e2e_rc}"

log "E2E specs matching ${label_filter} passed"
