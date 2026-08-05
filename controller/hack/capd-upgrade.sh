#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Upgrade a local CAPD workload cluster to a new Kubernetes version.
#
# CAPD DockerMachine templates embed CAPD_KINDEST_NODE_IMAGE (default
# cwe-capd-kindest-node:local). Bumping controlPlaneKubernetesVersion alone does
# not change that image name, so this script rebuilds :local from the target
# kindest/node tag before PATCH-ing the cluster.
#
# Prerequisites:
#   - CWE API reachable (Tilt / port-forward on :8080 by default)
#   - Cluster already created (make capd-create-cluster / ./hack/create-capd-cluster.sh)
#   - For CP recycling upgrades, kube-vip/BGP should already be enabled on create
#
# Usage:
#   ./hack/capd-upgrade.sh                         # upgrade cluster "capd" -> 1.35.0
#   CLUSTER_NAME=capd TARGET_VERSION=1.35.0 ./hack/capd-upgrade.sh
#   CLUSTER_ID=<uuid> TARGET_VERSION=1.35.0 ./hack/capd-upgrade.sh
#   SKIP_IMAGE=true ./hack/capd-upgrade.sh         # PATCH only (image already rebuilt)
#   DRY_RUN=true ./hack/capd-upgrade.sh            # print actions, do not change anything
#   WAIT=true ./hack/capd-upgrade.sh               # wait until controlPlaneUpgrading clears

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [[ -n "${API_URL:-}" ]]; then
  API_BASE_URL="${API_BASE_URL:-${API_URL%/}}"
  API_BASE_URL="${API_BASE_URL%/clusters}"
else
  API_BASE_URL="${API_BASE_URL:-http://localhost:8080/api}"
fi
CLUSTERS_API_URL="${API_BASE_URL%/}/clusters"

CLUSTER_NAME="${CLUSTER_NAME:-capd}"
CLUSTER_ID="${CLUSTER_ID:-}"
TARGET_VERSION="${TARGET_VERSION:-1.35.0}"
MAX_SURGE="${MAX_SURGE:-1}"
ENABLE_CP_NODE_RECYCLING="${ENABLE_CP_NODE_RECYCLING:-true}"
SKIP_IMAGE="${SKIP_IMAGE:-false}"
DRY_RUN="${DRY_RUN:-false}"
WAIT="${WAIT:-false}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-3600}"
# How long to wait for the controller to pick the upgrade up before giving up.
UPGRADE_START_TIMEOUT_SECONDS="${UPGRADE_START_TIMEOUT_SECONDS:-300}"
CURL_MAX_TIME="${CURL_MAX_TIME:-30}"

CAPD_KINDEST_NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE:-cwe-capd-kindest-node:local}"
# kindest/node tags are v-prefixed (v1.35.0). Allow override.
if [[ -z "${CAPD_KINDEST_NODE_TAG:-}" ]]; then
  if [[ "${TARGET_VERSION}" == v* ]]; then
    CAPD_KINDEST_NODE_TAG="${TARGET_VERSION}"
  else
    CAPD_KINDEST_NODE_TAG="v${TARGET_VERSION}"
  fi
fi

log() { printf '[capd-upgrade] %s\n' "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

normalize_version() {
  local v="$1"
  v="${v#v}"
  printf '%s\n' "${v}"
}

resolve_cluster_id() {
  if [[ -n "${CLUSTER_ID}" ]]; then
    local existing
    existing="$(curl -fsS --max-time "${CURL_MAX_TIME}" "${CLUSTERS_API_URL}/${CLUSTER_ID}" 2>/dev/null | jq -r '.id // empty')" \
      || die "CLUSTER_ID=${CLUSTER_ID} not found at ${CLUSTERS_API_URL}/${CLUSTER_ID}"
    [[ "${existing}" == "${CLUSTER_ID}" ]] || die "CLUSTER_ID=${CLUSTER_ID} not found"
    printf '%s\n' "${CLUSTER_ID}"
    return
  fi

  local resp
  resp="$(curl -fsS --max-time "${CURL_MAX_TIME}" "${CLUSTERS_API_URL}")" \
    || die "failed to list clusters from ${CLUSTERS_API_URL}"

  local resolved
  resolved="$(printf '%s\n' "${resp}" | jq -r --arg name "${CLUSTER_NAME}" '
    (.clusters // .)
    | if type == "array" then . else [] end
    | map(select(.name == $name))
    | if length == 0 then empty
      elif length > 1 then error("multiple clusters named \($name); set CLUSTER_ID")
      else .[0].id end
  ')" || die "failed to resolve cluster named ${CLUSTER_NAME}"

  [[ -n "${resolved}" && "${resolved}" != "null" ]] \
    || die "no cluster named ${CLUSTER_NAME} found; set CLUSTER_ID or create one first"

  printf '%s\n' "${resolved}"
}

build_target_image() {
  log "building CAPD node image ${CAPD_KINDEST_NODE_IMAGE} from kindest/node:${CAPD_KINDEST_NODE_TAG}"
  if [[ "${DRY_RUN}" == "true" ]]; then
    log "DRY_RUN: would run CAPD_KINDEST_NODE_TAG=${CAPD_KINDEST_NODE_TAG} make -C ${REPO_ROOT} capd-image"
    return
  fi
  require_cmd make
  (
    cd "${REPO_ROOT}"
    CAPD_KINDEST_NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE}" \
      CAPD_KINDEST_NODE_TAG="${CAPD_KINDEST_NODE_TAG}" \
      make capd-image
  )
}

patch_cluster() {
  local cluster_id="$1"
  local target
  target="$(normalize_version "${TARGET_VERSION}")"

  local body
  body="$(jq -nc \
    --arg version "${target}" \
    --argjson maxSurge "${MAX_SURGE}" \
    --argjson recycle "${ENABLE_CP_NODE_RECYCLING}" \
    '{
      controlPlaneKubernetesVersion: $version,
      enableCPNodeRecycling: $recycle,
      controlPlaneConfig: { maxSurge: $maxSurge }
    }')"

  log "PATCHing cluster ${cluster_id} -> ${target} (maxSurge=${MAX_SURGE}, recycle=${ENABLE_CP_NODE_RECYCLING})"
  if [[ "${DRY_RUN}" == "true" ]]; then
    log "DRY_RUN: would PATCH ${CLUSTERS_API_URL}/${cluster_id} with:"
    printf '%s\n' "${body}" | jq .
    return
  fi

  curl -fsS --max-time "${CURL_MAX_TIME}" -X PATCH "${CLUSTERS_API_URL}/${cluster_id}" \
    -H "Content-Type: application/json" \
    --data-raw "${body}" | jq '{
      id, name,
      controlPlaneKubernetesVersion,
      enableCPNodeRecycling,
      maxSurge: .controlPlaneConfig.maxSurge,
      controlPlaneUpgrading: .conditions.controlPlaneUpgrading
    }'
}

wait_for_upgrade() {
  local cluster_id="$1"
  local target
  target="$(normalize_version "${TARGET_VERSION}")"
  local deadline=$((SECONDS + WAIT_TIMEOUT_SECONDS))
  local start_deadline=$((SECONDS + UPGRADE_START_TIMEOUT_SECONDS))
  # PATCH sets controlPlaneKubernetesVersion immediately, so the version alone
  # never proves progress; wait for the controller to raise controlPlaneUpgrading
  # before treating a False status as completion.
  local started=false

  log "waiting for upgrade to ${target} (timeout ${WAIT_TIMEOUT_SECONDS}s)"
  while (( SECONDS < deadline )); do
    local resp status message version
    resp="$(curl -fsS --max-time "${CURL_MAX_TIME}" "${CLUSTERS_API_URL}/${cluster_id}")" \
      || die "failed to get cluster ${cluster_id}"

    status="$(printf '%s\n' "${resp}" | jq -r '.conditions.controlPlaneUpgrading.status // "False"')"
    message="$(printf '%s\n' "${resp}" | jq -r '.conditions.controlPlaneUpgrading.message // ""')"
    version="$(printf '%s\n' "${resp}" | jq -r '.controlPlaneKubernetesVersion // .kubernetesVersion // ""')"

    if [[ "${status}" == "True" ]]; then
      started=true
    fi

    if [[ "${started}" == "true" && "${status}" != "True" && "${version}" == "${target}" ]]; then
      log "upgrade complete: controlPlaneKubernetesVersion=${version}"
      return
    fi

    if [[ "${started}" != "true" ]] && (( SECONDS >= start_deadline )); then
      die "controlPlaneUpgrading never became True within ${UPGRADE_START_TIMEOUT_SECONDS}s; check CWE controller logs"
    fi

    if [[ "${started}" == "true" ]]; then
      log "still upgrading: version=${version} controlPlaneUpgrading=${status} msg=${message}"
    else
      log "waiting for upgrade to start: version=${version} controlPlaneUpgrading=${status} msg=${message}"
    fi
    sleep 15
  done
  die "timed out waiting for upgrade of cluster ${cluster_id}"
}

main() {
  require_cmd curl
  require_cmd jq

  local cluster_id
  cluster_id="$(resolve_cluster_id)"
  log "target cluster id=${cluster_id} name=${CLUSTER_NAME:-"(from CLUSTER_ID)"} -> ${TARGET_VERSION}"

  if [[ "${SKIP_IMAGE}" != "true" ]]; then
    build_target_image
  else
    log "SKIP_IMAGE=true; not rebuilding ${CAPD_KINDEST_NODE_IMAGE}"
  fi

  patch_cluster "${cluster_id}"

  if [[ "${WAIT}" == "true" ]]; then
    [[ "${DRY_RUN}" == "true" ]] && { log "DRY_RUN: skip WAIT"; return; }
    wait_for_upgrade "${cluster_id}"
  else
    log "upgrade requested. Watch progress in the CWE UI or re-run with WAIT=true"
  fi
}

main "$@"
