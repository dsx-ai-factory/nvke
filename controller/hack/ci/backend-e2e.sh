#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROLLER_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=controller/hack/backend-e2e-lib.sh
source "${CONTROLLER_ROOT}/hack/backend-e2e-lib.sh"
# shellcheck source=controller/hack/ci/nkectl-smoke.sh
source "${SCRIPT_DIR}/nkectl-smoke.sh"
WORKSPACE_ROOT="$(cd "${CONTROLLER_ROOT}/.." && pwd)"
cd "${CONTROLLER_ROOT}"

RUN_ID="${NKE_E2E_RUN_ID:-${GITHUB_RUN_ID:-local-$$}}"
RUN_ID="${RUN_ID//[^a-zA-Z0-9_.-]/-}"
RUN_ID="${RUN_ID:0:64}"
STATE_DIR="${NKE_E2E_STATE_DIR:-${CONTROLLER_ROOT}/.local/backend-e2e/${RUN_ID}}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cwe-capi}"
DOCKERHUB_MIRROR="${NKE_E2E_DOCKERHUB_MIRROR:-}"
# Names the backend in this script's own output. Both lanes share the script, so
# without it a NICo run reports itself as CAPD.
BACKEND="${NKE_E2E_BACKEND:-capd}"
RUNNER_NAME="capd-e2e-runner-${RUN_ID}"
RUNNER_IMAGE="${NKE_E2E_RUNNER_IMAGE:-docker:29.1.3-cli@sha256:4fa0ee1f3a7e4354c4ea34558b6d4ee32859baf4973d4c8ccc8e7fe3dd730c04}"
STATE_VOLUME="capd-e2e-state-${RUN_ID}"
BUNDLE_MIRROR_NAME="capd-e2e-bundle-mirror-${RUN_ID}"
BUNDLE_LOADER_NAME="capd-e2e-bundle-loader-${RUN_ID}"
BUNDLE_VOLUME="capd-e2e-bundles-${RUN_ID}"
CAPD_NODE_IMAGE="cwe-capd-kindest-node:run-${RUN_ID}"
BGP_CONTAINER_NAME="bgp-containers-${RUN_ID}"
DIAGNOSTICS_MARKER="${STATE_DIR}/SAFE_TO_UPLOAD"
# Sequential two-mode run: the cp-controller and node-pool provisioning modes run
# back to back on one kind cluster. The cp-controller leg runs first, so CWE is
# brought up flag-on here and the node-pool leg flips the deployment-wide flag
# off between the legs. A caller that drives the flag itself leaves this unset
# and takes the single-mode path below.
SINGLE_RUNNER_CONTROL_PLANE_MODES="${NKE_E2E_SINGLE_RUNNER_CONTROL_PLANE_MODES:-${NKE_E2E_SINGLE_RUNNER_CAPD_CONTROL_PLANE_MODES:-false}}"
# The ginkgo label for this backend's rows. BACKEND names the lane; the label is
# what tenant_test.go tags with, and for NICo the two differ.
case "${BACKEND}" in
  nico) BACKEND_LABEL=nico-containers ;;
  *) BACKEND_LABEL="${BACKEND}" ;;
esac
# Run the nkectl checks in hack/ci/nkectl-smoke.sh once the suite passes.
NKECTL_SMOKE="${NKE_E2E_NKECTL_SMOKE:-false}"
if [[ "${SINGLE_RUNNER_CONTROL_PLANE_MODES}" == "true" ]]; then
  PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true
fi
TMP_DIR="$(mktemp -d)"
MANAGEMENT_KUBECONFIG="${TMP_DIR}/management.kubeconfig"
CLUSTERCTL_BIN="${TMP_DIR}/clusterctl"
TILT_BIN="${TMP_DIR}/tilt"
BUNDLE_DIR="${TMP_DIR}/bundles"
SOURCE_ARCHIVE="${TMP_DIR}/cwe-source.tgz"
# Where the CAPNICo checkout lands inside the runner, when a NICo lane stages one.
CAPNICO_SOURCE_CONTAINER_DIR=/workspace/cluster-api-provider-nico
E2E_BIN="${TMP_DIR}/cwe-e2e.test"
NICO_SIM_BIN="${TMP_DIR}/nico-sim"
NKECTL_BIN="${TMP_DIR}/nkectl"
E2E_CONFIG="${TMP_DIR}/e2e-config.json"
RUNNER_DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}"
RUNNER_DOCKER_ARGS=()
TARGETS_VALIDATED=false
SKIP_DIAGNOSTICS=false
KIND_CLUSTER_OWNED=false
BGP_CONTAINER_OWNED=false
RUNNER_OWNED=false
BUNDLE_MIRROR_OWNED=false
BUNDLE_LOADER_OWNED=false
STATE_VOLUME_OWNED=false
BUNDLE_VOLUME_OWNED=false
CAPD_NODE_IMAGE_OWNED=false
STATE_MONITOR_PID=""

if [[ "${RUNNER_DOCKER_HOST}" == "unix:///var/run/docker.sock" ]]; then
  [[ -S /var/run/docker.sock ]] || {
    printf '[%s-e2e] /var/run/docker.sock is required for a local run\n' "${BACKEND}" >&2
    exit 1
  }
  RUNNER_DOCKER_ARGS=(-v /var/run/docker.sock:/var/run/docker.sock)
fi

# Mirrors local-backend-e2e.sh's detection: this runner's own architecture,
# used to build the CWE image, the E2E binary, and the CAPD node image for
# the platform actually running them, instead of the amd64 this script used
# to hardcode everywhere.
case "$(uname -m)" in
  arm64 | aarch64) host_arch=arm64 ;;
  x86_64) host_arch=amd64 ;;
  *)
    printf '[%s-e2e] unsupported architecture %s\n' "${BACKEND}" "$(uname -m)" >&2
    exit 1
    ;;
esac

log() {
  printf '[%s-e2e] %s\n' "${BACKEND}" "$*"
}

# The provider list controls provisioning backends and optional management-plane
# integrations. Parse it before image builds so integrations can stage images.
: "${NKE_E2E_PROVIDERS:=${E2E_CAPD_PROVIDERS}}"
if ! jq -e 'type == "array" and length > 0' <<<"${NKE_E2E_PROVIDERS}" >/dev/null 2>&1; then
  log "NKE_E2E_PROVIDERS must be a non-empty JSON array, got: ${NKE_E2E_PROVIDERS}"
  exit 1
fi
if jq -e 'index("dsx-identity-exchange") != null' <<<"${NKE_E2E_PROVIDERS}" >/dev/null; then
  DSX_IDENTITY_EXCHANGE_ENABLED=true
else
  DSX_IDENTITY_EXCHANGE_ENABLED=false
fi
if jq -e 'index("nico") != null' <<<"${NKE_E2E_PROVIDERS}" >/dev/null; then
  NICO_PROVIDER_ENABLED=true
else
  NICO_PROVIDER_ENABLED=false
fi

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    log "$1 is required" >&2
    exit 1
  }
}

redact_diagnostics() {
  local file_list="${TMP_DIR}/diagnostic-files"
  local file
  local -a files=()
  local grep_rc

  if ! find "${STATE_DIR}" -type f -print0 >"${file_list}"; then
    return 1
  fi
  while IFS= read -r -d '' file; do
    files+=("${file}")
    # -i.bak, not bare -i: BSD sed reads the next argument as the backup suffix,
    # so a bare -i with several -e leaves the file unredacted and exits 1.
    sed -E -i.bak \
      -e 's/[a-z0-9]{6}\.[a-z0-9]{16}/REDACTED-KUBEADM-TOKEN/g' \
      -e 's/(--certificate-key[=[:space:]]+)[0-9a-fA-F]{64}/\1REDACTED/g' \
      -e 's/(discovery-token-ca-cert-hash[=[:space:]]+sha256:)[0-9a-fA-F]{64}/\1REDACTED/g' \
      -e 's/((apiEncryptionKey|api_encryption_key|certificateKey|certificate_key|client-key-data)[" ]*[:=][" ]*)[^",[:space:]]+/\1REDACTED/g' \
      -e 's/((spireJoinToken|spire_join_token|joinToken|join_token)[" ]*[:=][" ]*)[^",[:space:]]+/\1REDACTED/g' \
      -e 's/(-joinToken[=[:space:]]+)[^[:space:]]+/\1REDACTED/g' \
      "${file}" || return 1
    rm -f "${file}.bak"
  done <"${file_list}"

  if ((${#files[@]} == 0)); then
    return 0
  fi

  if grep -EIl \
    -e '[a-z0-9]{6}\.[a-z0-9]{16}' \
    -e 'certificate-key[=[:space:]]+[0-9a-fA-F]{64}' \
    -e 'discovery-token-ca-cert-hash[=[:space:]]+sha256:[0-9a-fA-F]{64}' \
    -e '(apiEncryptionKey|api_encryption_key|certificateKey|certificate_key|client-key-data)[" ]*[:=][" ]*[A-Za-z0-9+/=]{20,}' \
    -e '(spireJoinToken|spire_join_token|joinToken|join_token)[" ]*[:=][" ]*[^",[:space:]]{12,}' \
    -e '-joinToken[=[:space:]]+[^[:space:]]{12,}' \
    -- "${files[@]}" >/dev/null; then
    grep_rc=0
  else
    grep_rc=$?
  fi
  case "${grep_rc}" in
    0) return 1 ;;
    1) return 0 ;;
    *) return 1 ;;
  esac
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
    -e DOCKER_HOST="${RUNNER_DOCKER_HOST}" \
    -e NKE_E2E_DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR}" \
    -e NKE_E2E_REQUIRE_AUTHN="${NKE_E2E_REQUIRE_AUTHN:-}" \
    "${RUNNER_NAME}" "$@"
}

runner_exec_detached() {
  docker exec -d \
    -e KUBECONFIG=/state/management.kubeconfig \
    -e DOCKER_HOST="${RUNNER_DOCKER_HOST}" \
    -e NKE_E2E_DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR}" \
    -e NKE_E2E_REQUIRE_AUTHN="${NKE_E2E_REQUIRE_AUTHN:-}" \
    "${RUNNER_NAME}" "$@"
}

wait_for_runner_tilt() {
  local deadline=$((SECONDS + 40 * 60))
  local stall_limit=$((12 * 60))
  local state errors pending total last_state="" last_change=${SECONDS}

  while true; do
    if runner_exec test -s /state/tilt.pid && \
      ! runner_exec kill -0 "$(runner_exec cat /state/tilt.pid)"; then
      runner_exec tail -n 250 /state/tilt.log >&2 || true
      log "Tilt exited before the E2E suite started" >&2
      return 1
    fi
    if state="$(runner_exec tilt get uiresources -o json 2>/dev/null | jq -r '
        [.items[] | select(.status.disableStatus.state != "Disabled")] as $r
        | {
            errors: [$r[] | select(.status.updateStatus == "error" or .status.runtimeStatus == "error") | .metadata.name],
            pending: [$r[] | select((.status.updateStatus | IN("pending", "in_progress")) or (.status.runtimeStatus | IN("pending"))) | .metadata.name],
            total: ($r | length)
          }
        | "\(.errors | join(","))|\(.pending | join(","))|\(.total)"' 2>/dev/null)" && [[ -n "${state}" ]]; then
      IFS='|' read -r errors pending total <<<"${state}"
      if [[ "${state}" != "${last_state}" ]]; then
        last_state="${state}"
        last_change=${SECONDS}
        log "tilt: ${total} resources, pending: ${pending:-none}"
      fi
      if [[ -n "${errors}" ]]; then
        for name in ${errors//,/ }; do
          echo "::group::tilt logs ${name}"
          runner_exec tilt logs "${name}" 2>/dev/null | tail -n 150
          echo "::endgroup::"
        done
        log "Tilt resources failed: ${errors}" >&2
        return 1
      fi
      if [[ -z "${pending}" && "${total}" -gt 1 ]]; then
        log "Tilt is ready: ${total} resources"
        return 0
      fi
      if [[ -n "${pending}" ]] && ((SECONDS - last_change >= stall_limit)); then
        for name in ${pending//,/ }; do
          echo "::group::tilt logs ${name}"
          runner_exec tilt logs "${name}" 2>/dev/null | tail -n 150
          echo "::endgroup::"
        done
        log "no Tilt resource changed state for 12m; still pending: ${pending}" >&2
        return 1
      fi
    fi
    if ((SECONDS >= deadline)); then
      runner_exec tilt get uiresources 2>/dev/null || true
      log "Tilt did not become ready within 40m; still pending: ${pending:-unknown}" >&2
      return 1
    fi
    sleep 15
  done
}

configure_kind_dockerhub_mirror() {
  local node_name="${KIND_CLUSTER_NAME}-control-plane"
  local registry_host

  if [[ -z "${DOCKERHUB_MIRROR}" ]]; then
    return
  fi
  log "routing management-cluster Docker Hub pulls through ${DOCKERHUB_MIRROR}"
  for registry_host in docker.io registry-1.docker.io; do
    docker exec "${node_name}" mkdir -p "/etc/containerd/certs.d/${registry_host}"
    printf '%s\n' \
      "server = \"https://${DOCKERHUB_MIRROR}\"" \
      '' \
      "[host.\"https://${DOCKERHUB_MIRROR}\"]" \
      '  capabilities = ["pull", "resolve"]' | \
      docker exec -i "${node_name}" cp /dev/stdin "/etc/containerd/certs.d/${registry_host}/hosts.toml"
  done
}

start_state_monitor() {
  (
    while true; do
      captured_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      printf '=== %s CAPI conditions ===\n' "${captured_at}"
      if ! runner_exec kubectl get \
        clusters,machinedeployments,machinesets,machines,kubeadmcontrolplanes,kubeadmconfigs,dockerclusters,dockermachines \
        -A -o json | jq -c --arg captured_at "${captured_at}" '
          .items[] | {
            capturedAt: $captured_at,
            apiVersion: .apiVersion,
            kind: .kind,
            namespace: .metadata.namespace,
            name: .metadata.name,
            generation: .metadata.generation,
            observedGeneration: (.status.observedGeneration // null),
            phase: (.status.phase // null),
            ready: (.status.ready // null),
            failureReason: (.status.failureReason // null),
            failureMessage: (.status.failureMessage // null),
            conditions: (.status.conditions // [])
          }
        '; then
        printf '[%s-e2e] failed to capture CAPI conditions at %s\n' "${BACKEND}" "${captured_at}"
      fi
      printf '%s\n' "=== ${captured_at} CAPD node containers ==="
      docker ps -a \
        --filter "ancestor=${CAPD_NODE_IMAGE}" \
        --format '{{.Names}} {{.Status}}' || true
      sleep 30
    done
  ) >"${STATE_DIR}/capi-state-timeline.log" 2>&1 &
  STATE_MONITOR_PID=$!
}

stop_state_monitor() {
  if [[ -z "${STATE_MONITOR_PID}" ]]; then
    return
  fi
  kill "${STATE_MONITOR_PID}" >/dev/null 2>&1 || true
  wait "${STATE_MONITOR_PID}" >/dev/null 2>&1 || true
  STATE_MONITOR_PID=""
}

# Run one Ginkgo leg against the already-deployed management plane. The label
# filter and the control-plane-controller flag differ per leg; the flag is
# forwarded to the e2e binary the same way the single-mode run always has. The
# suffix keeps each leg's e2e log and JUnit report distinct (empty for the
# single-mode run, so its artifact names are unchanged). Returns the suite's
# exit code rather than aborting, so the caller decides what a failure means.
# Exec adapter for the shared leg runner: CI runs the suite inside its runner
# container, with two env vars the local path has no equivalent for.
# CWE_EXPECTED_VERSION turns the third health spec from a self-skip into a real
# assertion, so a stale image fails here rather than passing quietly.
ci_e2e_exec() {
  local cp_flag="$1"; shift
  runner_exec env \
    "CWE_EXPECTED_VERSION=${CWE_BUILD_VERSION}" \
    "CWE_PROFILES_DIR=${NKE_E2E_PROFILES_DIR}" \
    "NKE_E2E_DSX_IDENTITY_EXCHANGE=${DSX_IDENTITY_EXCHANGE_ENABLED}" \
    "E2E_BGP_CONTAINER_NAME=${BGP_CONTAINER_NAME}" \
    "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${cp_flag}" \
    "NKE_E2E_SPIRE_RELEASE=${NKE_E2E_SPIRE_RELEASE:-}" \
    "$@"
}

run_ginkgo_leg() {
  local label_filter="$1" cp_flag="$2" suffix="$3"
  local -a extra=()
  [[ -n "${NKE_E2E_FOCUS}" ]] && extra+=("-ginkgo.focus=${NKE_E2E_FOCUS}")
  e2e_run_ginkgo_leg ci_e2e_exec log "${STATE_DIR}" \
    "${label_filter}" "${cp_flag}" "${suffix}" -- "${extra[@]}"
}

# Cleanup gate between the two sequential legs: the cp-controller leg's delete
# spec should have removed every tenant cluster before the node-pool leg starts.
# Count CAPI Cluster objects directly with kubectl rather than the CWE API: the
# apiserver service proxy authenticates the caller to the apiserver but does not
# forward a bearer token to CWE, so a flag-on, auth-enforcing CWE 401s the raw
# /api/clusters probe. Every tenant owns a cluster.cluster.x-k8s.io object CWE
# deletes on teardown, so zero of them is a faithful, auth-free "cluster is clean"
# signal (no DB access needed).
ci_kubectl() { runner_exec kubectl "$@"; }

wait_for_zero_tenant_clusters() {
  e2e_wait_for_zero_tenant_clusters ci_kubectl log && return 0
  log "refusing the node-pool leg" >&2
  runner_exec kubectl get clusters.cluster.x-k8s.io -A >&2 2>/dev/null || true
  return 1
}

# Flip the deployment-wide control-plane-controller flag via a light redeploy
# (no re-run of Tilt), wait for the rollout, then confirm the deployment now
# carries the requested value. Mirrors the flag-on verification the Tilt deploy
# already does, so a forwarding regression fails here rather than mid-leg.
redeploy_cwe() {
  local value="$1"
  log "redeploying CWE with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${value}"
  runner_exec kubectl -n cloud-workflow-engine set env deployment/cloud-workflow-engine \
    "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=${value}"
  runner_exec kubectl -n cloud-workflow-engine rollout status deployment/cloud-workflow-engine --timeout=5m
  local got
  got="$(runner_exec kubectl -n cloud-workflow-engine get deployment/cloud-workflow-engine \
    -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER\")].value}")"
  if [[ "${got}" != "${value}" ]]; then
    log "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER must be ${value} after redeploy, got ${got:-<empty>}" >&2
    exit 1
  fi
}

handle_signal() {
  SKIP_DIAGNOSTICS=true
  exit 130
}

collect_diagnostics() {
  local rc=$?
  local cleanup_failed=false
  local node_names
  local bgp_stop_timeout=10

  trap - EXIT
  trap '' INT TERM
  set +e

  stop_state_monitor

  mkdir -p "${STATE_DIR}"
  if [[ "${SKIP_DIAGNOSTICS}" != true ]]; then
    docker info >"${STATE_DIR}/docker-info.txt" 2>&1
    docker system df -v >"${STATE_DIR}/docker-system-df.txt" 2>&1
    docker ps -a --no-trunc >"${STATE_DIR}/docker-ps.txt" 2>&1
    docker network ls >"${STATE_DIR}/docker-networks.txt" 2>&1
    if [[ "${TARGETS_VALIDATED}" == true ]]; then
      docker logs "${KIND_CLUSTER_NAME}-control-plane" >"${STATE_DIR}/kind-control-plane.log" 2>&1
      docker logs "${BUNDLE_MIRROR_NAME}" >"${STATE_DIR}/bundle-mirror.log" 2>&1
      docker logs "${BGP_CONTAINER_NAME}" >"${STATE_DIR}/bgp-containers.log" 2>&1
      docker exec "${BGP_CONTAINER_NAME}" vtysh -c 'show bgp summary' >"${STATE_DIR}/bgp-containers-summary.txt" 2>&1
      docker exec "${BGP_CONTAINER_NAME}" vtysh -c 'show bgp ipv4 unicast' >"${STATE_DIR}/bgp-containers-routes.txt" 2>&1

      mkdir -p "${STATE_DIR}/nodes"
      while IFS= read -r node_name; do
        [[ -n "${node_name}" ]] || continue
        docker logs "${node_name}" >"${STATE_DIR}/nodes/${node_name}-console.log" 2>&1
        docker exec "${node_name}" systemctl status \
          nke-agent spire-agent swtpm-local containerd kubelet --no-pager \
          >"${STATE_DIR}/nodes/${node_name}-systemd.txt" 2>&1
        docker exec "${node_name}" journalctl --no-pager \
          -u nke-agent -u spire-agent -u swtpm-local -u containerd -u kubelet \
          >"${STATE_DIR}/nodes/${node_name}-journal.log" 2>&1
        docker exec "${node_name}" crictl ps -a \
          >"${STATE_DIR}/nodes/${node_name}-crictl-ps.txt" 2>&1
        docker exec "${node_name}" crictl pods \
          >"${STATE_DIR}/nodes/${node_name}-crictl-pods.txt" 2>&1
        docker exec "${node_name}" crictl images \
          >"${STATE_DIR}/nodes/${node_name}-crictl-images.txt" 2>&1
        docker exec "${node_name}" crictl info \
          >"${STATE_DIR}/nodes/${node_name}-crictl-info.json" 2>&1
        docker exec "${node_name}" cat /etc/containerd/certs.d/docker.io/hosts.toml \
          >"${STATE_DIR}/nodes/${node_name}-dockerhub-hosts.toml" 2>&1
        docker exec "${node_name}" sh -c 'ip -brief address; ip route show; ip -6 route show' \
          >"${STATE_DIR}/nodes/${node_name}-network.txt" 2>&1
      done < <(docker ps -a --filter "ancestor=${CAPD_NODE_IMAGE}" --format '{{.Names}}')

      if docker inspect "${RUNNER_NAME}" >/dev/null 2>&1; then
        runner_exec tilt snapshot create /state/tilt-snapshot.json >/dev/null 2>&1 || true
        runner_exec kubectl get nodes -o wide >"${STATE_DIR}/management-nodes.txt" 2>&1
        runner_exec kubectl get pods -A -o wide >"${STATE_DIR}/management-pods.txt" 2>&1
        runner_exec kubectl get events -A --sort-by=.lastTimestamp >"${STATE_DIR}/management-events.txt" 2>&1
        runner_exec kubectl -n capd-system logs deployment/capd-controller-manager --all-containers --tail=-1 \
          >"${STATE_DIR}/capd-controller.log" 2>&1
        runner_exec kubectl -n capi-system logs deployment/capi-controller-manager --all-containers --tail=-1 \
          >"${STATE_DIR}/capi-controller.log" 2>&1
        runner_exec kubectl -n capi-kubeadm-bootstrap-system \
          logs deployment/capi-kubeadm-bootstrap-controller-manager --all-containers --tail=-1 \
          >"${STATE_DIR}/capi-kubeadm-bootstrap-controller.log" 2>&1
        runner_exec kubectl -n capi-kubeadm-control-plane-system \
          logs deployment/capi-kubeadm-control-plane-controller-manager --all-containers --tail=-1 \
          >"${STATE_DIR}/capi-kubeadm-control-plane-controller.log" 2>&1
        runner_exec kubectl -n capd-reboot-system logs deployment/capd-reboot-controller --all-containers --tail=-1 \
          >"${STATE_DIR}/capd-reboot-controller.log" 2>&1
        runner_exec kubectl -n cloud-workflow-engine logs deployment/cloud-workflow-engine --all-containers --tail=-1 \
          >"${STATE_DIR}/cwe.log" 2>&1
        if [[ "${DSX_IDENTITY_EXCHANGE_ENABLED}" == "true" ]]; then
          runner_exec kubectl -n spire-server logs deployment/dsx-identity-exchange-mock --all-containers --tail=-1 \
            >"${STATE_DIR}/dsx-identity-exchange.log" 2>&1
        fi
        runner_exec kubectl get \
          clusters,machinedeployments,machinesets,machines,kubeadmcontrolplanes,kubeadmconfigs,dockerclusters,dockermachines \
          -A -o yaml \
          >"${STATE_DIR}/capi-resources.yaml" 2>&1
        runner_exec kubectl describe \
          clusters,machinedeployments,machinesets,machines,kubeadmcontrolplanes,kubeadmconfigs,dockerclusters,dockermachines \
          -A \
          >"${STATE_DIR}/capi-describe.txt" 2>&1
        for artifact in tilt.log tilt-snapshot.json e2e.log junit.xml \
          junit-cp-controller.xml junit-nodepool.xml; do
          docker cp "${RUNNER_NAME}:/state/${artifact}" "${STATE_DIR}/${artifact}" >/dev/null 2>&1 || true
        done
      fi
    fi

    if ! redact_diagnostics; then
      log "diagnostic redaction failed; refusing a successful result" >&2
      rc=1
    elif ! touch "${DIAGNOSTICS_MARKER}"; then
      log "could not mark diagnostics as safe to upload" >&2
      rc=1
    fi
  else
    bgp_stop_timeout=1
  fi

  if [[ "${BGP_CONTAINER_OWNED}" == true ]]; then
    if ! docker stop --time "${bgp_stop_timeout}" "${BGP_CONTAINER_NAME}" >/dev/null 2>&1; then
      log "failed to stop owned BGP container ${BGP_CONTAINER_NAME}" >&2
      cleanup_failed=true
    fi
    if ! docker rm -f "${BGP_CONTAINER_NAME}" >/dev/null 2>&1; then
      log "failed to remove owned BGP container ${BGP_CONTAINER_NAME}" >&2
      cleanup_failed=true
    fi
  fi
  if [[ "${RUNNER_OWNED}" == true ]] && ! docker rm -f "${RUNNER_NAME}" >/dev/null 2>&1; then
    log "failed to remove owned inner runner ${RUNNER_NAME}" >&2
    cleanup_failed=true
  fi
  if [[ "${BUNDLE_MIRROR_OWNED}" == true ]] && ! docker rm -f "${BUNDLE_MIRROR_NAME}" >/dev/null 2>&1; then
    log "failed to remove owned bundle mirror ${BUNDLE_MIRROR_NAME}" >&2
    cleanup_failed=true
  fi
  if [[ "${BUNDLE_LOADER_OWNED}" == true ]] && ! docker rm -f "${BUNDLE_LOADER_NAME}" >/dev/null 2>&1; then
    log "failed to remove owned bundle loader ${BUNDLE_LOADER_NAME}" >&2
    cleanup_failed=true
  fi
  if [[ "${CAPD_NODE_IMAGE_OWNED}" == true ]]; then
    if node_names="$(docker ps -a --filter "ancestor=${CAPD_NODE_IMAGE}" --format '{{.Names}}')"; then
      while IFS= read -r node_name; do
        [[ -n "${node_name}" ]] || continue
        if ! docker rm -f "${node_name}" >/dev/null 2>&1; then
          log "failed to remove owned CAPD node ${node_name}" >&2
          cleanup_failed=true
        fi
      done <<<"${node_names}"
    else
      log "failed to list CAPD nodes using ${CAPD_NODE_IMAGE}" >&2
      cleanup_failed=true
    fi
  fi
  if [[ "${KIND_CLUSTER_OWNED}" == true ]] && ! kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1; then
    log "failed to delete owned Kind cluster ${KIND_CLUSTER_NAME}" >&2
    cleanup_failed=true
  fi
  if [[ "${STATE_VOLUME_OWNED}" == true ]] && ! docker volume rm "${STATE_VOLUME}" >/dev/null 2>&1; then
    log "failed to remove owned state volume ${STATE_VOLUME}" >&2
    cleanup_failed=true
  fi
  if [[ "${BUNDLE_VOLUME_OWNED}" == true ]] && ! docker volume rm "${BUNDLE_VOLUME}" >/dev/null 2>&1; then
    log "failed to remove owned bundle volume ${BUNDLE_VOLUME}" >&2
    cleanup_failed=true
  fi
  if [[ "${CAPD_NODE_IMAGE_OWNED}" == true ]] && ! docker image rm "${CAPD_NODE_IMAGE}" >/dev/null 2>&1; then
    log "failed to remove owned CAPD node image ${CAPD_NODE_IMAGE}" >&2
    cleanup_failed=true
  fi
  if ! rm -rf "${TMP_DIR}"; then
    log "failed to remove temporary directory ${TMP_DIR}" >&2
    cleanup_failed=true
  fi

  if [[ "${cleanup_failed}" == true && "${rc}" -eq 0 ]]; then
    rc=1
  fi
  exit "${rc}"
}
trap collect_diagnostics EXIT
trap handle_signal INT TERM

mkdir -p "${STATE_DIR}"
rm -f "${DIAGNOSTICS_MARKER}"

for command in curl docker find git go grep helm jq kind kubectl sed sha256sum tar; do
  require_command "${command}"
done
if [[ -n "${DOCKERHUB_MIRROR}" && ! "${DOCKERHUB_MIRROR}" =~ ^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  log "NKE_E2E_DOCKERHUB_MIRROR must be a registry host with an optional port" >&2
  exit 1
fi
docker info >/dev/null
if ! kind_clusters="$(kind get clusters 2>&1)"; then
  log "could not inspect existing Kind clusters: ${kind_clusters}" >&2
  exit 1
fi
if grep -Fxq "${KIND_CLUSTER_NAME}" <<<"${kind_clusters}"; then
  log "Kind cluster ${KIND_CLUSTER_NAME} already exists; start from a clean target name" >&2
  exit 1
fi
for container_name in \
  "${KIND_CLUSTER_NAME}-control-plane" \
  "${RUNNER_NAME}" \
  "${BUNDLE_MIRROR_NAME}" \
  "${BUNDLE_LOADER_NAME}" \
  "${BGP_CONTAINER_NAME}"; do
  if docker inspect "${container_name}" >/dev/null 2>&1; then
    log "Docker container ${container_name} already exists; choose a fresh run ID" >&2
    exit 1
  fi
done
if docker image inspect "${CAPD_NODE_IMAGE}" >/dev/null 2>&1; then
  log "Docker image ${CAPD_NODE_IMAGE} already exists; choose a fresh run ID" >&2
  exit 1
fi
for volume_name in "${STATE_VOLUME}" "${BUNDLE_VOLUME}"; do
  if docker volume inspect "${volume_name}" >/dev/null 2>&1; then
    log "Docker volume ${volume_name} already exists; choose a fresh run ID" >&2
    exit 1
  fi
done
if docker ps -a --filter "ancestor=${CAPD_NODE_IMAGE}" --format '{{.Names}}' | grep -q .; then
  log "CAPD nodes already use ${CAPD_NODE_IMAGE}; choose a fresh run ID" >&2
  exit 1
fi
TARGETS_VALIDATED=true

[[ "$(uname -s)" == Linux ]] || {
  log "this entrypoint requires a Linux Docker host" >&2
  exit 1
}
: "${CAPI_CORE_VERSION:=v1.12.3}"
: "${CAPD_PROVIDER_VERSION:=v1.12.3}"
: "${TILT_VERSION:=v0.37.5}"
# Per-arch pins. host_arch was already validated above, so this is exhaustive.
case "${host_arch}" in
  amd64)
    : "${CLUSTERCTL_SHA256:=9b75287b15656a62ece65c8ad7491412cebfd30c78e085a9d61396a50df17349}"
    : "${TILT_SHA256:=9efb56969c48ddea4adfb56a604b9ca5ed254924ddf140fc3e068a305cacf558}"
    ;;
  arm64)
    : "${CLUSTERCTL_SHA256:=974bdf47fc58864df3d87766f4bdc72164171655c54b759e73596efc98f5e3e1}"
    : "${TILT_SHA256:=1f9c62dcce0608aeb0c63533b14e260fc2e8c5797f81589b0914c398ba53d5b0}"
    ;;
esac

git_sha="$(git -C "${WORKSPACE_ROOT}" rev-parse HEAD)"
git_short_sha="$(git -C "${WORKSPACE_ROOT}" rev-parse --short=12 HEAD)"
CWE_IMAGE="${CWE_IMAGE:-cwe-e2e:${git_short_sha}}"
CWE_BUILD_VERSION="${CWE_BUILD_VERSION:-e2e-${git_short_sha}}"
CWE_PROFILES_TILT_IMAGE="docker.local/cwe-profiles:tilt"
# Matches the ref the manifest names; Tilt builds the same tag via custom_build.
CAPD_REBOOT_IMAGE="ko.local/capd-reboot-controller"
DSX_IDENTITY_EXCHANGE_IMAGE=""

log "building the sanitized profiles fixture image"
docker build \
  --platform "linux/${host_arch}" \
  --file test/fixtures/Dockerfile \
  --tag "${CWE_PROFILES_TILT_IMAGE}" \
  test/fixtures
printf '%s\n' "${CWE_PROFILES_TILT_IMAGE}" >"${STATE_DIR}/cwe-profiles-image-ref.txt"
docker image inspect "${CWE_PROFILES_TILT_IMAGE}" >"${STATE_DIR}/cwe-profiles-image.json"

log "building controller image ${CWE_IMAGE} for the ${host_arch} CAPD runner"
DOCKER_BUILDKIT=1 docker build \
  --platform "linux/${host_arch}" \
  --file "${CONTROLLER_ROOT}/Dockerfile" \
  --build-arg "BUILD_VERSION=${CWE_BUILD_VERSION}" \
  --build-arg "GIT_VERSION=${git_sha}" \
  --tag "${CWE_IMAGE}" \
  "${WORKSPACE_ROOT}"
printf '%s\n' "${CWE_IMAGE}" >"${STATE_DIR}/cwe-image-ref.txt"
docker image inspect "${CWE_IMAGE}" >"${STATE_DIR}/cwe-image.json"

if [[ "${DSX_IDENTITY_EXCHANGE_ENABLED}" == "true" ]]; then
  DSX_IDENTITY_EXCHANGE_IMAGE="dsx-identity-exchange-e2e:${git_short_sha}"
  log "building DSX identity exchange mock ${DSX_IDENTITY_EXCHANGE_IMAGE}"
  DOCKER_BUILDKIT=1 docker build \
    --platform "linux/${host_arch}" \
    --file "${CONTROLLER_ROOT}/hack/dsx-identity-exchange-mock/Dockerfile" \
    --tag "${DSX_IDENTITY_EXCHANGE_IMAGE}" \
    "${WORKSPACE_ROOT}"
  printf '%s\n' "${DSX_IDENTITY_EXCHANGE_IMAGE}" >"${STATE_DIR}/dsx-identity-exchange-image-ref.txt"
  docker image inspect "${DSX_IDENTITY_EXCHANGE_IMAGE}" >"${STATE_DIR}/dsx-identity-exchange-image.json"
fi

NODE_AGENT_VERSION="${NODE_AGENT_VERSION:-$(
  git -C "${WORKSPACE_ROOT}" describe --match 'agent/v*' --tags --always --dirty 2>/dev/null \
    || git -C "${WORKSPACE_ROOT}" rev-parse --short HEAD
)}"
NODE_AGENT_VERSION="${NODE_AGENT_VERSION#agent/}"
printf '%s\n' "${NODE_AGENT_VERSION}" >"${STATE_DIR}/agent-version.txt"
log "using agent source version ${NODE_AGENT_VERSION}"

log "preparing verified SPIRE and local Calico bundles"
BUNDLE_ARCH="${host_arch}" bash hack/ci/prepare-capd-bundles.sh "${BUNDLE_DIR}"
CALICO_MANIFESTS_DEB="${BUNDLE_DIR}/nke-cni-calico-manifests-3.30.1_3.30.1_all.deb"
[[ -f "${CALICO_MANIFESTS_DEB}" ]] || {
  log "Calico manifests package was not produced: ${CALICO_MANIFESTS_DEB}" >&2
  exit 1
}

log "installing clusterctl ${CAPD_PROVIDER_VERSION}"
if [[ ! "${CLUSTERCTL_SHA256}" =~ ^[0-9a-f]{64}$ ]]; then
  log "clusterctl checksum is not configured for linux/${host_arch}"
  exit 1
fi
clusterctl_url="https://github.com/kubernetes-sigs/cluster-api/releases/download/${CAPD_PROVIDER_VERSION}/clusterctl-linux-${host_arch}"
curl_with_retry "downloading clusterctl ${CAPD_PROVIDER_VERSION}" \
  "${clusterctl_url}" \
  -o "${CLUSTERCTL_BIN}"
printf '%s  %s\n' "${CLUSTERCTL_SHA256}" "${CLUSTERCTL_BIN}" | sha256sum -c -
chmod +x "${CLUSTERCTL_BIN}"

log "installing Tilt ${TILT_VERSION}"
# Tilt's own release asset names use x86_64, not amd64, for that one arch;
# arm64 matches host_arch directly.
tilt_release_arch="${host_arch}"
[[ "${host_arch}" == amd64 ]] && tilt_release_arch=x86_64
tilt_asset_name="tilt.${TILT_VERSION#v}.linux.${tilt_release_arch}.tar.gz"
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

log "archiving the controller working tree for the inner runner"
git -C "${WORKSPACE_ROOT}" ls-files --cached --others --exclude-standard -z controller | \
  while IFS= read -r -d '' source_path; do
    if [[ -e "${WORKSPACE_ROOT}/${source_path}" || -L "${WORKSPACE_ROOT}/${source_path}" ]]; then
      printf '%s\0' "${source_path}"
    fi
  done | \
  tar \
    --directory "${WORKSPACE_ROOT}" \
    --null \
    --files-from=- \
    --transform='s,^controller/,,' \
    --create \
    --gzip \
    --file "${SOURCE_ARCHIVE}"

log "compiling the ${BACKEND} E2E suite for linux/${host_arch}"
e2e_built=false
for attempt in 1 2 3; do
  if (cd test/e2e && CGO_ENABLED=0 GOOS=linux GOARCH="${host_arch}" go test -c -o "${E2E_BIN}" .); then
    e2e_built=true
    break
  fi
  log "E2E compile attempt ${attempt} failed"
  sleep 5
done
if [[ "${e2e_built}" != true ]]; then
  exit 1
fi

NICO_SIM_BINARY_PREBUILT=false
if [[ "${NICO_PROVIDER_ENABLED}" == true ]]; then
  if [[ "$(uname -s)" != Linux ]]; then
    log "the NICo backend requires a Linux host to build nico-sim with CGO"
    exit 1
  fi
  log "compiling nico-sim for linux/${host_arch}"
  mkdir -p nico-sim/bin
  GOOS=linux GOARCH="${host_arch}" \
    make -C nico-sim BINARY="${NICO_SIM_BIN}" build-static
  if readelf -l "${NICO_SIM_BIN}" | grep -q 'INTERP'; then
    echo "nico-sim binary is dynamically linked; the inner runner requires a static binary" >&2
    exit 1
  fi
  NICO_SIM_BINARY_PREBUILT=true
fi

if [[ "${NKECTL_SMOKE}" == "true" ]]; then
  nkectl_smoke_build "${host_arch}"
fi

log "building CAPD node image with agent source ${NODE_AGENT_VERSION}"
log "SPIRE node attestation: $(e2e_attestation_method)"
CAPD_NODE_AGENT_LOCAL_BUILD=true \
NODE_AGENT_VERSION="${NODE_AGENT_VERSION}" \
NODE_AGENT_REPO_PATH="${WORKSPACE_ROOT}/agent" \
SPIRE_AGENT_BUNDLE_PATH="${BUNDLE_DIR}/spire-agent.tgz" \
CAPD_CNI_MANIFESTS_DEB_PATH="${CALICO_MANIFESTS_DEB}" \
CAPD_SPIRE_ATTESTATION_METHOD="$(e2e_attestation_method)" \
BUNDLE_SERVER_URL="http://${BUNDLE_MIRROR_NAME}/" \
INDEX_FILE_URL="http://${BUNDLE_MIRROR_NAME}/index.json" \
CAPD_KINDEST_NODE_IMAGE="${CAPD_NODE_IMAGE}" \
CAPD_KINDEST_NODE_PLATFORM="${CAPD_KINDEST_NODE_PLATFORM:-linux/${host_arch}}" \
NKE_E2E_DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR}" \
  bash hack/capd-kindest-node/build-image.sh
CAPD_NODE_IMAGE_OWNED=true
docker image inspect "${CAPD_NODE_IMAGE}" >"${STATE_DIR}/capd-node-image.json"

log "creating Kind management cluster ${KIND_CLUSTER_NAME}"
KIND_CLUSTER_OWNED=true
kind create cluster \
  --name "${KIND_CLUSTER_NAME}" \
  --config hack/cwe-capi-kind.yaml \
  --wait 5m
configure_kind_dockerhub_mirror
kind load docker-image --name "${KIND_CLUSTER_NAME}" "${CWE_IMAGE}"
kind load docker-image --name "${KIND_CLUSTER_NAME}" "${CWE_PROFILES_TILT_IMAGE}"

# capd-reboot-controller is a CAPD test double, not management-plane software:
# CAPD "nodes" are containers, so a machine reboot has no analogue and this
# restarts the container instead. The Tilt path gets it from its provider
# module; the nke-stack path has no Tilt, so it is built here and applied by
# deploy-nke-stack.sh. It stays out of the nke-stack chart deliberately -- no
# operator deploys it, and its image is a local ko.local reference.
# The suite reads the SPIRE server's ConfigMap and lists its pods by label, and
# both names derive from the helm release: <release>-server and
# instance=<release>. Tilt installs SPIRE as its own release named "spire",
# which is what the suite defaults to; under the umbrella every subchart
# inherits the umbrella's release instead, so the defaults find nothing and
# every spec needing the management trust domain fails with
# `configmaps "spire-server" not found`. Must match STACK_RELEASE in
# deploy-nke-stack.sh.
NKE_E2E_SPIRE_RELEASE="${NKE_E2E_SPIRE_RELEASE:-}"
if [[ "${NKE_E2E_USE_STACK:-false}" == "true" ]]; then
  NKE_E2E_SPIRE_RELEASE="${NKE_E2E_SPIRE_RELEASE:-nke-stack}"
  log "spire release for the suite: ${NKE_E2E_SPIRE_RELEASE}"
fi

if [[ "${NKE_E2E_USE_STACK:-false}" == "true" ]]; then
  log "building capd-reboot-controller for the nke-stack path"
  docker build \
    -t "${CAPD_REBOOT_IMAGE}" \
    -f "${CONTROLLER_ROOT}/hack/capd-reboot-controller/Dockerfile" \
    "${CONTROLLER_ROOT}/hack/capd-reboot-controller"
  kind load docker-image --name "${KIND_CLUSTER_NAME}" "${CAPD_REBOOT_IMAGE}"
fi
if [[ -n "${DSX_IDENTITY_EXCHANGE_IMAGE}" ]]; then
  kind load docker-image --name "${KIND_CLUSTER_NAME}" "${DSX_IDENTITY_EXCHANGE_IMAGE}"
fi
kind get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" >"${MANAGEMENT_KUBECONFIG}"

log "starting the CAPD bundle mirror on the Kind Docker network"
docker volume create "${BUNDLE_VOLUME}" >/dev/null
BUNDLE_VOLUME_OWNED=true
BUNDLE_LOADER_OWNED=true
docker create --name "${BUNDLE_LOADER_NAME}" -v "${BUNDLE_VOLUME}:/bundle" alpine:3.22 true >/dev/null
docker cp "${BUNDLE_DIR}/." "${BUNDLE_LOADER_NAME}:/bundle/"
docker rm "${BUNDLE_LOADER_NAME}" >/dev/null
BUNDLE_LOADER_OWNED=false
BUNDLE_MIRROR_OWNED=true
docker run -d \
  --name "${BUNDLE_MIRROR_NAME}" \
  --network kind \
  -v "${BUNDLE_VOLUME}:/usr/share/nginx/html:ro" \
  nginx:alpine >/dev/null

log "starting inner runner on the Kind Docker network"
docker pull "${RUNNER_IMAGE}"
docker volume create "${STATE_VOLUME}" >/dev/null
STATE_VOLUME_OWNED=true
RUNNER_OWNED=true
docker create \
  --name "${RUNNER_NAME}" \
  --network kind \
  --add-host=docker:host-gateway \
  -e DOCKER_HOST="${RUNNER_DOCKER_HOST}" \
  -v "${STATE_VOLUME}:/state" \
  "${RUNNER_DOCKER_ARGS[@]}" \
  "${RUNNER_IMAGE}" sleep infinity >/dev/null
docker start "${RUNNER_NAME}" >/dev/null
runner_kind_ip="$(docker inspect "${RUNNER_NAME}" \
  --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}')"
if [[ ! "${runner_kind_ip}" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then
  log "inner runner has no valid IPv4 address on the kind network: ${runner_kind_ip}"
  exit 1
fi
docker cp "${MANAGEMENT_KUBECONFIG}" "${RUNNER_NAME}:/state/management.kubeconfig"
docker cp "${CLUSTERCTL_BIN}" "${RUNNER_NAME}:/usr/local/bin/clusterctl"
if [[ "${NKECTL_SMOKE}" == "true" ]]; then
  nkectl_smoke_stage
fi
docker cp "${TILT_BIN}" "${RUNNER_NAME}:/usr/local/bin/tilt"
docker cp "$(command -v helm)" "${RUNNER_NAME}:/usr/local/bin/helm-real"
docker cp hack/ci/helm-dockerhub-mirror.sh "${RUNNER_NAME}:/usr/local/bin/helm"
for tool in kind kubectl; do
  docker cp "$(command -v "${tool}")" "${RUNNER_NAME}:/usr/local/bin/${tool}"
done
docker cp "${SOURCE_ARCHIVE}" "${RUNNER_NAME}:/state/cwe-source.tgz"
docker cp "${E2E_BIN}" "${RUNNER_NAME}:/state/cwe-e2e.test"
if [[ "${NICO_SIM_BINARY_PREBUILT}" == "true" ]]; then
  docker cp "${NICO_SIM_BIN}" "${RUNNER_NAME}:/tmp/cwe-nico-sim"
fi

# The NICo backend builds CAPNICo from a checkout. The source archive is
# extracted with --transform s,^controller/,, so /workspace/cloud-workflow-engine
# holds the CONTROLLER directory, not the monorepo root. That makes the
# Tiltfile's relative default resolve outside /workspace, so the path is passed
# explicitly as CAPNICO_SOURCE_DIR at the tilt invocation below rather than
# inferred. Every other backend skips this entirely.
if [[ -n "${CAPNICO_SOURCE_SRC:-}" ]]; then
  if [[ ! -d "${CAPNICO_SOURCE_SRC}" ]]; then
    log "CAPNICO_SOURCE_SRC is set but not a directory: ${CAPNICO_SOURCE_SRC}"
    exit 1
  fi
  log "staging the CAPNICo provider from ${CAPNICO_SOURCE_SRC}"
  nico_archive="${TMP_DIR}/cluster-api-provider-nico.tgz"
  tar -C "$(dirname "${CAPNICO_SOURCE_SRC}")" \
    --exclude .git \
    -czf "${nico_archive}" "$(basename "${CAPNICO_SOURCE_SRC}")"
  docker cp "${nico_archive}" "${RUNNER_NAME}:/state/cluster-api-provider-nico.tgz"
  runner_exec sh -lc '
    set -eu
    mkdir -p /workspace
    tar -C /workspace -xzf /state/cluster-api-provider-nico.tgz
  '
fi

runner_exec sh -lc '
  set -eu
  if command -v apk >/dev/null; then
    apk add --no-cache \
      bash ca-certificates curl gcompat git jq libstdc++ netcat-openbsd openssl python3 \
      >/dev/null
  elif command -v apt-get >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -qq -y --no-install-recommends \
      bash ca-certificates curl git jq netcat-openbsd openssl python3
    rm -rf /var/lib/apt/lists/*
  fi
'

runner_exec mkdir -p /workspace/cloud-workflow-engine
runner_exec tar -xzf /state/cwe-source.tgz -C /workspace/cloud-workflow-engine

# The source archive carries only controller/ (tar --transform), so the umbrella
# chart's build scripts and metadata -- which live at the monorepo root -- are
# not in it. Stage them separately, preserving the build/ + generated/ layout the
# script derives REPO_ROOT from. Only when the stack path is selected.
if [[ "${NKE_E2E_USE_STACK:-false}" == "true" ]]; then
  runner_exec mkdir -p /workspace/nke-stack/generated/nke-stack
  docker cp "${WORKSPACE_ROOT}/build" "${RUNNER_NAME}:/workspace/nke-stack/build"
  # .helmignore included: without it the packaged chart differs from what a
  # normal build produces, and helm falls back to its built-in default.
  for f in Chart.yaml values.yaml .helmignore; do
    docker cp "${WORKSPACE_ROOT}/generated/nke-stack/${f}" \
      "${RUNNER_NAME}:/workspace/nke-stack/generated/nke-stack/${f}"
  done
  docker cp "${WORKSPACE_ROOT}/controller/hack/ci/deploy-nke-stack.sh" \
    "${RUNNER_NAME}:/usr/local/bin/deploy-nke-stack.sh"
  # build-local packages the CWE chart from the working tree, so the build tree
  # needs it at the path build/nke-stack.sh derives from REPO_ROOT.
  runner_exec mkdir -p /workspace/nke-stack/controller/deploy
  docker cp "${WORKSPACE_ROOT}/controller/deploy/charts" \
    "${RUNNER_NAME}:/workspace/nke-stack/controller/deploy/charts"
  # A chart in a private package needs credentials inside the runner too; helm
  # on the outer host already has them if anyone pushed or pulled it.
  if [[ -f "${HOME}/.config/helm/registry/config.json" ]]; then
    runner_exec mkdir -p /root/.config/helm/registry
    docker cp "${HOME}/.config/helm/registry/config.json" \
      "${RUNNER_NAME}:/root/.config/helm/registry/config.json"
  fi
fi

runner_exec bash -lc '
  set -euo pipefail
  for tool in curl docker git helm jq kind kubectl nc openssl python3 tar tilt; do
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
if [[ -n "${CAPNICO_SOURCE_SRC:-}" && "${NICO_PROVIDER_ENABLED}" != true ]]; then
  log "installing CAPNICo CRDs"
  runner_exec kubectl apply -f "${CAPNICO_SOURCE_CONTAINER_DIR}/config/crd/bases"
  runner_exec kubectl wait --for=condition=Established crd \
    nicoclusters.infrastructure.cluster.x-k8s.io \
    nicoclustertemplates.infrastructure.cluster.x-k8s.io \
    nicomachines.infrastructure.cluster.x-k8s.io \
    nicomachinetemplates.infrastructure.cluster.x-k8s.io \
    --timeout=2m
fi
log "injecting SPIRE exec-auth plugin into CAPI/CAPD controllers"
runner_exec bash /workspace/cloud-workflow-engine/hack/apply-capi-spiffe-exec-auth.sh

if ! management_vip="$(e2e_management_vip "${KIND_CLUSTER_NAME}")"; then
  log "Kind control-plane IP was not found"
  exit 1
fi
# The provider list is the main thing that varies by backend. The default is
# the CAPD set this script has always used; nico-e2e.yml passes the NICo one.
# Where the suite reads the OS profile index it builds the nico-vms matrix
# from. This has to be a path on the inner runner. The suite is compiled on the
# outer runner, so the fallback resolveOSProfileMetadataPath derives from
# runtime.Caller names a directory that does not exist where the binary runs,
# and without this the matrix soft-skips and 21 specs never register.
#
# fleet-profiles, not profiles. The latter is the synthetic index the CWE
# deployment serves below, and it omits the profiles the matrix documents as
# exclusions, so building the matrix from it panics the suite.
#
# A lane with a real nke-site-deploy checkout can point this at that instead.
: "${NKE_E2E_PROFILES_DIR:=/workspace/cloud-workflow-engine/test/fixtures/fleet-profiles}"
e2e_write_tilt_config "${TMP_DIR}/tilt_config.json" \
  "${NKE_E2E_PROVIDERS}" "${CWE_IMAGE}" "${management_vip}" "${BGP_CONTAINER_NAME}" \
  "${DSX_IDENTITY_EXCHANGE_IMAGE}"
if [[ "${NICO_PROVIDER_ENABLED}" == true ]]; then
  jq '.nico_vms_observability = false | .nico_sim_backends = ["container"]' "${TMP_DIR}/tilt_config.json" \
    >"${TMP_DIR}/tilt_config.json.tmp"
  mv "${TMP_DIR}/tilt_config.json.tmp" "${TMP_DIR}/tilt_config.json"
fi
docker cp "${TMP_DIR}/tilt_config.json" \
  "${RUNNER_NAME}:/workspace/cloud-workflow-engine/tilt_config.json"
jq -n \
  --arg kubeconfig /state/management.kubeconfig \
  --arg context "kind-${KIND_CLUSTER_NAME}" \
  --arg capd_node_image "${CAPD_NODE_IMAGE}" \
  '{
    kube: {
      kubeconfig: $kubeconfig,
      context: $context
    },
    cluster: {
      capdNodeImage: $capd_node_image
    },
    lifecycle: {
      preserveOnFailure: true
    }
  }' >"${E2E_CONFIG}"
docker cp "${E2E_CONFIG}" "${RUNNER_NAME}:/state/e2e-config.json"

# NKE_E2E_STACK_CHART deploys the six management-plane charts from the nke-stack
# umbrella instead of Tilt. Everything below -- rollout checks, CAPI probes, the
# Ginkgo legs -- is identical either way. Tilt remains the default: it also
# runs the inner-loop live_update the umbrella has no equivalent for. The pieces
# its providers deploy -- capd-reboot-controller, the CAPD RBAC for CWE and the
# BGP container -- are handled explicitly on this path instead.
if [[ "${NKE_E2E_USE_STACK:-false}" == "true" ]]; then
  log "deploying the management plane from the nke-stack umbrella chart"

  # FRR, so kube-vip on container-based tenant nodes can peer through the kind
  # gateway. Tilt's bgp-containers provider runs this; without Tilt nothing did,
  # and BGP_CONTAINER_OWNED was being set for a container that had never been
  # created -- which is what "failed to stop owned BGP container" was reporting
  # on every run. up.sh derives the gateway and subnet from the Docker network
  # itself, so it needs only the name.
  log "starting the BGP container ${BGP_CONTAINER_NAME}"
  CONTAINER_NAME="${BGP_CONTAINER_NAME}" bash "${CONTROLLER_ROOT}/hack/bgp-containers/up.sh"
  BGP_CONTAINER_OWNED=true
  # VERSION is passed in because the runner holds a tar of the working tree, not
  # a clone: `git describe` inside it finds no repository, so build-local cannot
  # derive one. It is NOT CWE_BUILD_VERSION -- that is an image tag
  # (e2e-<sha>), and a chart version has to be semver. The commit is carried in
  # the prerelease field instead. The controller image tag is set separately
  # from CWE_IMAGE, so the two need not match.
  runner_exec env \
    VERSION="0.0.0-e2e.${git_short_sha}" \
    MANAGEMENT_VIP="${management_vip}" \
    KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
    CAPD_REBOOT_SLEEP_DURATION="${CAPD_REBOOT_SLEEP_DURATION:-10s}" \
    DGXC_TENANT_USER=capd-e2e \
    DGXC_TENANT_PASSWORD=example-password \
    CNPG_SUPERUSER_PASSWORD=example-password \
    CNPG_CLOUD_PASSWORD=example-password \
    CNPG_TEMPORAL_PASSWORD=example-password \
    PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER="${PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER:-}" \
    SPIRE_TPM_ATTESTOR_SERVER_CHECKSUM="${SPIRE_TPM_ATTESTOR_SERVER_CHECKSUM:-}" \
    NKE_E2E_STACK_CHART="${NKE_E2E_STACK_CHART}" \
    NKE_E2E_STACK_VERSION="${NKE_E2E_STACK_VERSION:-}" \
    bash /usr/local/bin/deploy-nke-stack.sh "${CWE_IMAGE}"
else
log "deploying the management plane with Tilt"
BGP_CONTAINER_OWNED=true
# The inner runner has no Go toolchain, and this lane exercises
# container-backed sites only, so reuse the host-built nico-sim in mock VM mode.
tilt_exec=runner_exec
tilt_mode=ci
if [[ "${NICO_PROVIDER_ENABLED}" == true ]]; then
  # NICo's simulator is a Tilt local resource. Keep Tilt alive through both
  # E2E legs; `tilt ci` exits after deployment and stops the simulator.
  tilt_exec=runner_exec_detached
  tilt_mode=up
fi
"${tilt_exec}" env \
  KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
  CWE_PROFILES_PRELOADED=true \
  TENANT_ACCESS_ENABLED=true \
  NKE_OIDC_AUTH_ENABLED=true \
  DGXC_TENANT_USER=capd-e2e \
  DGXC_TENANT_PASSWORD=example-password \
  CNPG_SUPERUSER_PASSWORD=example-password \
  CNPG_CLOUD_PASSWORD=example-password \
  CNPG_TEMPORAL_PASSWORD=example-password \
  CWE_PROFILES_DIR=/workspace/cloud-workflow-engine/test/fixtures/profiles \
  CAPNICO_SOURCE_DIR="${CAPNICO_SOURCE_CONTAINER_DIR}" \
  CAPD_KINDEST_NODE_IMAGE="${CAPD_NODE_IMAGE}" \
  CAPD_KINDEST_NODE_IMAGE_PREBUILT=true \
  NICO_SIM_BINARY_PREBUILT="${NICO_SIM_BINARY_PREBUILT}" \
  NICO_SIM_ENDPOINT_HOST="${runner_kind_ip}" \
  NICO_VMS_VM_BACKEND=mock \
  NKE_E2E_TILT_MODE="${tilt_mode}" \
  INSTALL_CNI_AFTER_INIT="${INSTALL_CNI_AFTER_INIT:-false}" \
  PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER="${PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER:-}" \
  NODE_REBOOT_SOFT_REBOOT_ENABLED="${NODE_REBOOT_SOFT_REBOOT_ENABLED:-}" \
  TILT_DISABLE_ANALYTICS=1 \
  bash -lc '
    set -euo pipefail
    cd /workspace/cloud-workflow-engine
    if [[ "${NKE_E2E_TILT_MODE}" == up ]]; then
      echo $$ >/state/tilt.pid
      exec tilt up --stream=true >/state/tilt.log 2>&1
    fi
    if ! tilt ci \
      --timeout=40m \
      --output-snapshot-on-exit=/state/tilt-snapshot.json \
      >/state/tilt.log 2>&1; then
      tail -n 250 /state/tilt.log >&2
      exit 1
    fi
  '
if [[ "${tilt_mode}" == up ]]; then
  wait_for_runner_tilt
  runner_exec tilt snapshot create /state/tilt-snapshot.json >/dev/null
fi
fi

for controller in \
  "capi-system capi-controller-manager" \
  "capi-kubeadm-control-plane-system capi-kubeadm-control-plane-controller-manager" \
  "capd-system capd-controller-manager"; do
  read -r namespace deployment <<<"${controller}"
  runner_exec kubectl rollout status "deployment/${deployment}" -n "${namespace}" --timeout=5m
  runner_exec kubectl exec "deployment/${deployment}" -n "${namespace}" -- \
    /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth --help >/dev/null 2>&1
done
runner_exec kubectl -n cloud-workflow-engine rollout status deployment/cloud-workflow-engine --timeout=5m
for env_name in TENANT_ACCESS_ENABLED NKE_OIDC_AUTH_ENABLED; do
  env_value="$(runner_exec kubectl -n cloud-workflow-engine get deployment/cloud-workflow-engine \
    -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"${env_name}\")].value}")"
  if [[ "${env_value}" != "true" ]]; then
    log "${env_name} must be true in the CAPD E2E deployment, got ${env_value:-<empty>}"
    exit 1
  fi
done
# When the caller asked for control-plane-controller provisioning, confirm the
# flag actually reached the CWE deployment. This catches a Tiltfile forwarding
# regression before the cp-controller specs would mis-fail at cluster create.
if [[ "${PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER:-}" == "true" ]]; then
  cp_flag_value="$(runner_exec kubectl -n cloud-workflow-engine get deployment/cloud-workflow-engine \
    -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER\")].value}")"
  if [[ "${cp_flag_value}" != "true" ]]; then
    log "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER must be true in the CWE deployment, got ${cp_flag_value:-<empty>}"
    exit 1
  fi
fi
runner_exec kubectl get --raw \
  /api/v1/namespaces/cloud-workflow-engine/services/cloud-workflow-engine:8080/proxy/api/version \
  >"${STATE_DIR}/cwe-version.json"

NKE_E2E_FOCUS="${NKE_E2E_FOCUS:-}"

if [[ "${SINGLE_RUNNER_CONTROL_PLANE_MODES}" == "true" ]]; then
  # Two provisioning modes, one cluster, back to back. cp-controller runs first,
  # against the flag-on CWE the Tilt deploy just verified, so a regression in the
  # newer control-plane-controller path fails fast -- before the long node-pool
  # leg spends a runner on a feature the first leg already showed is broken.
  #
  # `health` rides along with the cp-controller leg so CWE's /healthz, /readyz and
  # version endpoints are probed against a flag-on deployment too.
  start_state_monitor
  leg_rc=0
  run_ginkgo_leg "$(e2e_cp_controller_filter "${BACKEND_LABEL}")" "true" "-cp-controller" || leg_rc=$?
  if ((leg_rc != 0)); then
    stop_state_monitor
    log "cp-controller leg failed with exit code ${leg_rc}; not running the node-pool leg" >&2
    exit "${leg_rc}"
  fi
  log "cp-controller leg passed; confirming the cluster is clean before the node-pool leg"
  if ! wait_for_zero_tenant_clusters; then
    stop_state_monitor
    exit 1
  fi
  redeploy_cwe false
  # Node-pool control plane (flag off): the long-standing CAPD lane. `!cp-controller`
  # keeps the flag-on spec out of this flag-off CWE. `health` and the authn specs
  # ride along here -- both need a deployed, enforcing NKE, which this leg has;
  # authn-spire is excluded (needs SPIRE pod attestation not yet wired in CAPD).
  leg_rc=0
  run_ginkgo_leg "$(e2e_node_pool_filter "${BACKEND_LABEL}")" "false" "-nodepool" || leg_rc=$?
  stop_state_monitor
  if ((leg_rc != 0)); then
    log "node-pool leg failed with exit code ${leg_rc}" >&2
    exit "${leg_rc}"
  fi
  if [[ "${NKECTL_SMOKE}" == "true" ]]; then
    nkectl_smoke
  fi
  log "${BACKEND} sequential management-plane E2E passed (cp-controller then node-pool)"
  exit 0
fi

# Single-mode (default): one label filter, one flag. Every other backend and any
# local run takes this path unchanged.
#
# The default is the CAPD lane's filter, for a local run with nothing set. A
# caller passes its own; the NICo lane passes nico-containers.
#
# `health` rides along with `capd`. Those specs need a cluster running CWE in
# namespace cloud-workflow-engine on port 8080, which this script has already
# built and waited on above. They are three HTTP probes through a service proxy,
# so they cost seconds rather than minutes.
NKE_E2E_LABEL_FILTER="${NKE_E2E_LABEL_FILTER:-capd || health}"
start_state_monitor
e2e_rc=0
run_ginkgo_leg "${NKE_E2E_LABEL_FILTER}" "${PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER:-}" "" || e2e_rc=$?
stop_state_monitor

if ((e2e_rc != 0)); then
  log "${BACKEND} E2E scenarios failed with exit code ${e2e_rc}" >&2
  exit "${e2e_rc}"
fi

# A focused pull-request run takes the single-mode path, so it does not pass
# through the cleanup gate between sequential legs above. Assert the same
# invariant before reporting success: a green core run must not leave a tenant
# behind on its management cluster.
log "E2E specs passed; confirming the cluster is clean"
if ! wait_for_zero_tenant_clusters; then
  exit 1
fi

if [[ "${NKECTL_SMOKE}" == "true" ]]; then
  nkectl_smoke
fi

log "${BACKEND} management-plane E2E passed"
