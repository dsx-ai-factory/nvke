#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

MIRROR_HOST="${CAPD_E2E_DOCKERHUB_MIRROR:-dc.local.gha-runners.nvidia.com}"
POSTGRES_CHART_VERSION="${CAPD_E2E_POSTGRES_CHART_VERSION:-18.5.15}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-capd-registry-probe}"
BUILDER_NAME="capd-registry-probe-${GITHUB_RUN_ID:-$$}"
BUILDER_NAME="${BUILDER_NAME//[^a-zA-Z0-9_.-]/-}"
TMP_DIR="$(mktemp -d)"
KIND_CLUSTER_OWNED=false
BUILDER_OWNED=false
declare -a PROBE_NAMES=()
declare -a PROBE_RESULTS=()
declare -a PROBE_DETAILS=()

cleanup() {
  local rc=$?

  trap - EXIT
  set +e
  if [[ "${BUILDER_OWNED}" == true ]]; then
    docker buildx rm "${BUILDER_NAME}" >/dev/null 2>&1
  fi
  if [[ "${KIND_CLUSTER_OWNED}" == true ]]; then
    kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1
  fi
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

log() {
  printf '[capd-registry-probe] %s\n' "$*"
}

record() {
  PROBE_NAMES+=("$1")
  PROBE_RESULTS+=("$2")
  PROBE_DETAILS+=("$3")
  log "$1: $2 ($3)"
}

run_probe() {
  local name=$1
  local detail=$2
  shift 2
  local log_file="${TMP_DIR}/${name//[^a-zA-Z0-9_.-]/-}.log"
  local rc

  set +e
  "$@" >"${log_file}" 2>&1
  rc=$?
  set -e
  if ((rc == 0)); then
    record "${name}" PASS "${detail}"
  else
    record "${name}" FAIL "${detail}; exit ${rc}"
  fi
  sed -n '1,80p' "${log_file}"
  if (("$(wc -l <"${log_file}")" > 80)); then
    log "${name}: output truncated to 80 lines"
  fi
  return "${rc}"
}

probe_mirror_api() {
  local status

  getent hosts "${MIRROR_HOST}" >/dev/null
  status="$(curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --connect-timeout 10 \
    --max-time 30 \
    "https://${MIRROR_HOST}/v2/")"
  printf 'mirror /v2/ HTTP status: %s\n' "${status}"
  [[ "${status}" == 200 || "${status}" == 401 ]]
}

probe_buildkit_pull() {
  printf '%s\n' \
    'FROM docker.io/library/alpine:3.22' \
    'RUN true' | \
    docker buildx build \
      --pull \
      --no-cache \
      --progress=plain \
      --output=type=cacheonly \
      --file=- \
      .
}

create_buildkit_builder() {
  docker buildx create \
    --name "${BUILDER_NAME}" \
    --driver docker-container \
    --buildkitd-config /etc/buildkit/buildkitd.toml \
    --use
  docker buildx inspect "${BUILDER_NAME}" --bootstrap
}

probe_helm_direct() {
  helm show chart \
    oci://registry-1.docker.io/bitnamicharts/postgresql \
    --version "${POSTGRES_CHART_VERSION}" >/dev/null
}

probe_helm_mirror() {
  helm show chart \
    "oci://${MIRROR_HOST}/bitnamicharts/postgresql" \
    --version "${POSTGRES_CHART_VERSION}" >/dev/null
}

probe_kind_mirror() {
  local node_name="${KIND_CLUSTER_NAME}-control-plane"

  docker exec "${node_name}" mkdir -p /etc/containerd/certs.d/docker.io
  printf '%s\n' \
    "server = \"https://${MIRROR_HOST}\"" \
    '' \
    "[host.\"https://${MIRROR_HOST}\"]" \
    '  capabilities = ["pull", "resolve"]' | \
    docker exec -i "${node_name}" cp /dev/stdin /etc/containerd/certs.d/docker.io/hosts.toml
  docker exec "${node_name}" crictl pull docker.io/library/alpine:3.22
}

probe_kind_legacy_mirror() {
  local node_name="${KIND_CLUSTER_NAME}-control-plane"

  docker exec "${node_name}" crictl pull docker.io/library/busybox:1.37.0
}

create_kind_probe_cluster() {
  printf '%s\n' \
    'kind: Cluster' \
    'apiVersion: kind.x-k8s.io/v1alpha4' \
    'containerdConfigPatches:' \
    '  - |-' \
    '    [plugins."io.containerd.grpc.v1.cri".registry.mirrors."docker.io"]' \
    "      endpoint = [\"https://${MIRROR_HOST}\"]" \
    'nodes:' \
    '  - role: control-plane' | \
    kind create cluster \
      --name "${KIND_CLUSTER_NAME}" \
      --config=- \
      --wait 5m
}

for command in curl docker getent helm kind sed; do
  command -v "${command}" >/dev/null || {
    log "${command} is required"
    exit 1
  }
done

if kind get clusters | grep -Fxq "${KIND_CLUSTER_NAME}"; then
  log "Kind cluster ${KIND_CLUSTER_NAME} already exists; use a clean target name"
  exit 1
fi
if docker buildx inspect "${BUILDER_NAME}" >/dev/null 2>&1; then
  log "Buildx builder ${BUILDER_NAME} already exists; use a clean target name"
  exit 1
fi

log "checking the runner-local Docker Hub mirror"
mirror_api_ok=false
if run_probe mirror-api "${MIRROR_HOST} resolves and serves the registry API" probe_mirror_api; then
  mirror_api_ok=true
fi

log "checking the host Docker daemon mirror configuration"
daemon_mirrors="$(docker info --format '{{range .RegistryConfig.Mirrors}}{{println .}}{{end}}')"
daemon_mirror_ok=false
if [[ -n "${daemon_mirrors}" ]]; then
  daemon_mirror_count="$(wc -l <<<"${daemon_mirrors}")"
  if grep -Eq "${MIRROR_HOST}|dockerhub(-backup)?\.nvidia\.com" <<<"${daemon_mirrors}"; then
    record docker-daemon-mirror PASS "${daemon_mirror_count} configured; includes an NVIDIA Docker Hub mirror"
    daemon_mirror_ok=true
  else
    record docker-daemon-mirror INFO "${daemon_mirror_count} configured; no expected NVIDIA mirror found"
  fi
else
  record docker-daemon-mirror INFO "no daemon registry mirror is configured"
fi

log "checking the selected BuildKit builder"
buildkit_config_ok=false
buildkit_builder_ok=false
buildkit_ok=false
if [[ -r /etc/buildkit/buildkitd.toml ]] && \
   grep -Fq "${MIRROR_HOST}" /etc/buildkit/buildkitd.toml; then
  record buildkit-runner-config PASS "runner config contains ${MIRROR_HOST}"
  buildkit_config_ok=true
  BUILDER_OWNED=true
  if run_probe buildkit-builder "created a builder from the runner config" create_buildkit_builder; then
    buildkit_builder_ok=true
  fi
elif [[ -r /etc/buildkit/buildkitd.toml ]]; then
  record buildkit-runner-config INFO "runner config is readable but does not contain ${MIRROR_HOST}"
else
  record buildkit-runner-config FAIL "/etc/buildkit/buildkitd.toml is absent or unreadable"
fi
if [[ "${buildkit_builder_ok}" == true ]]; then
  if run_probe buildkit-pull "BuildKit pulled a Docker Hub base image" probe_buildkit_pull; then
    buildkit_ok=true
  fi
fi

log "checking Helm's OCI paths for PostgreSQL ${POSTGRES_CHART_VERSION}"
if run_probe helm-docker-hub "direct Docker Hub OCI chart lookup" probe_helm_direct; then
  :
fi
helm_mirror_ok=false
if run_probe helm-runner-mirror "OCI chart lookup through ${MIRROR_HOST}" probe_helm_mirror; then
  helm_mirror_ok=true
fi

log "checking the pasted Kind mirror patch against Kind $(kind version)"
KIND_CLUSTER_OWNED=true
kind_mirror_ok=false
kind_legacy_ok=false
if run_probe kind-cluster "created a disposable cluster with the pasted patch" create_kind_probe_cluster; then
  node_name="${KIND_CLUSTER_NAME}-control-plane"
  if docker exec "${node_name}" grep -Fq "${MIRROR_HOST}" /etc/containerd/config.toml; then
    record kind-legacy-config-rendered PASS "the pasted mirror stanza was written to config.toml"
  else
    record kind-legacy-config-rendered FAIL "the pasted mirror stanza was not written to config.toml"
  fi

  docker exec "${node_name}" sh -c \
    'printf "127.0.0.1 registry-1.docker.io\n" >> /etc/hosts'
  if run_probe kind-legacy-mirror \
    "containerd pulled busybox with direct Docker Hub blocked and only the pasted patch configured" \
    probe_kind_legacy_mirror; then
    kind_legacy_ok=true
  fi

  if run_probe kind-hosts-mirror "containerd pulled alpine with the mirror as its only docker.io host" probe_kind_mirror; then
    kind_mirror_ok=true
  fi
fi

printf '\n%-30s %-6s %s\n' PROBE RESULT DETAIL
printf '%-30s %-6s %s\n' ------------------------------ ------ ------------------------------
for index in "${!PROBE_NAMES[@]}"; do
  printf '%-30s %-6s %s\n' \
    "${PROBE_NAMES[index]}" \
    "${PROBE_RESULTS[index]}" \
    "${PROBE_DETAILS[index]}"
done

if [[ "${mirror_api_ok}" == true && \
      "${daemon_mirror_ok}" == true && \
      "${buildkit_config_ok}" == true && \
      "${buildkit_ok}" == true && \
      "${helm_mirror_ok}" == true && \
      "${kind_legacy_ok}" == true && \
      "${kind_mirror_ok}" == true ]]; then
  log "all runner-mirror paths needed by the CAPD E2E are usable"
  exit 0
fi

log "one or more runner-mirror paths needed by the CAPD E2E are not usable"
exit 1
