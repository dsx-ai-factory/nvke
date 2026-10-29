#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# nico-vms E2E lane: the controller on kind, CAPNICo from source, nico-vms on the host,
# tenant nodes as KVM guests booted from the NVKE FDE image.
#
# Runs on the host, not in ci/backend-e2e.sh's container: nico-vms needs
# libvirt, the host bridge and CAP_NET_ADMIN. Tilt runs in the background for
# the whole suite; `tilt ci` would exit and kill nico-vms with it.
# Requires test/kubeadm/scripts/setup-ci-host.sh with NKE_CI_HOST_FDE=true.
#
# Environment:
#   CAPNICO_SOURCE_DIR       checkout of dsx-ai-factory/cluster-api-provider-nico (required)
#   NKE_E2E_LABEL_FILTER     Ginkgo label filter        (default: nico-vms && happy-path)
#   NKE_E2E_GINKGO_TIMEOUT   -ginkgo.timeout            (default: 240m)
#   NKE_E2E_TILT_TIMEOUT     wait for Tilt readiness    (default: 30m)
#   NKE_E2E_TILT_STALL       fail when no resource changes state for this long (default: 12m)
#   NKE_E2E_STATE_DIR        diagnostics directory      (default: controller/.local/nico-vms-e2e/<run>)
#   NKE_E2E_RUN_ID           names the state directory  (default: GITHUB_RUN_ID or local-<pid>)
#   NKE_E2E_NVKECTL_SMOKE     run hack/ci/nvkectl-smoke.sh after the suite (default: true)
#   KIND_CLUSTER_NAME        management cluster name    (default: cwe-nico-vms)
#   NKE_E2E_KEEP             non-empty keeps kind, Tilt and the VMs running on exit

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROLLER_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WORKSPACE_ROOT="$(cd "${CONTROLLER_ROOT}/.." && pwd)"
# shellcheck source=controller/hack/backend-e2e-lib.sh
source "${CONTROLLER_ROOT}/hack/backend-e2e-lib.sh"
# No runner container here, so the smoke runs nvkectl on this host. It reads
# NVKECTL_SMOKE_RUNNER being unset as the signal; see its header.
# shellcheck source=controller/hack/ci/nvkectl-smoke.sh
source "${SCRIPT_DIR}/nvkectl-smoke.sh"
cd "${CONTROLLER_ROOT}"

RUN_ID="${NKE_E2E_RUN_ID:-${GITHUB_RUN_ID:-local-$$}}"
RUN_ID="${RUN_ID//[^a-zA-Z0-9_.-]/-}"
STATE_DIR="${NKE_E2E_STATE_DIR:-${CONTROLLER_ROOT}/.local/nico-vms-e2e/${RUN_ID}}"
DIAGNOSTICS_MARKER="${STATE_DIR}/SAFE_TO_UPLOAD"
export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cwe-nico-vms}"
LABEL_FILTER="${NKE_E2E_LABEL_FILTER:-nico-vms && happy-path}"
GINKGO_TIMEOUT="${NKE_E2E_GINKGO_TIMEOUT:-240m}"
TILT_TIMEOUT="${NKE_E2E_TILT_TIMEOUT:-30m}"
KEEP="${NKE_E2E_KEEP:-}"
LIBVIRT_URI="${NICO_VMS_LIBVIRT_URI:-qemu:///session}"
# Not Tilt's default 10350, so a developer's own Tilt session is never mistaken
# for this one. Every tilt command below carries it.
TILT_PORT="${NKE_E2E_TILT_PORT:-10351}"
TILT_CONFIG="${CONTROLLER_ROOT}/tilt_config.json"
NICO_SIM_DIR="${CONTROLLER_ROOT}/nico-sim"
E2E_BIN="${STATE_DIR}/cwe-e2e.test"
E2E_CONFIG="${STATE_DIR}/e2e-config.json"
E2E_KUBECONFIG="${STATE_DIR}/kubeconfig"
# What the nvkectl smoke needs. BACKEND gates its write round trip to CAPD, so
# this lane runs the read paths, negatives and provider checks only.
# NVKECTL_SMOKE_RUNNER stays unset, which is how the smoke knows to run the CLI
# on this host.
NVKECTL_SMOKE="${NKE_E2E_NVKECTL_SMOKE:-true}"
NVKECTL_BIN="${STATE_DIR}/nvkectl"
NVKECTL_SMOKE_KUBECONFIG="${E2E_KUBECONFIG}"
TMP_DIR="${STATE_DIR}"
BACKEND=nico-vms
# The controller runs with OIDC enabled here, so the smoke's no-token control is
# valid: a call without a token must fail.
export NKE_E2E_REQUIRE_AUTHN=true
TILT_PGID=""
TILT_CONFIG_BACKUP=""
KIND_CLUSTER_OWNED=false
STATE_MONITOR_PID=""
PROGRESS_MONITOR_PID=""
NICO_VMS_ORG_ID=novxwn552ssc
NICO_VMS_TOKEN=nico-vmsctl-token

log() { printf '[nico-vms-e2e] %s\n' "$*"; }
fail() {
  log "ERROR: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

tilt_cmd() { tilt --port "${TILT_PORT}" "$@"; }

# ── Diagnostics and cleanup ─────────────────────────────────────────────────

# Same redaction as ci/backend-e2e.sh. Fails closed: a leftover match means no
# SAFE_TO_UPLOAD marker.
redact_diagnostics() {
  local file
  local -a files=()
  while IFS= read -r -d '' file; do
    files+=("${file}")
    sed -E -i.bak \
      -e 's/[a-z0-9]{6}\.[a-z0-9]{16}/REDACTED-KUBEADM-TOKEN/g' \
      -e 's/(--certificate-key[=[:space:]]+)[0-9a-fA-F]{64}/\1REDACTED/g' \
      -e 's/(discovery-token-ca-cert-hash[=[:space:]]+sha256:)[0-9a-fA-F]{64}/\1REDACTED/g' \
      -e 's/((apiEncryptionKey|api_encryption_key|certificateKey|certificate_key|client-key-data)[" ]*[:=][" ]*)[^",[:space:]]+/\1REDACTED/g' \
      -e 's/((spireJoinToken|spire_join_token|joinToken|join_token)[" ]*[:=][" ]*)[^",[:space:]]+/\1REDACTED/g' \
      -e 's/(-joinToken[=[:space:]]+)[^[:space:]]+/\1REDACTED/g' \
      "${file}" || return 1
    rm -f "${file}.bak"
  done < <(find "${STATE_DIR}" -type f -print0)
  ((${#files[@]} > 0)) || return 0
  if grep -EIl \
    -e '[a-z0-9]{6}\.[a-z0-9]{16}' \
    -e 'certificate-key[=[:space:]]+[0-9a-fA-F]{64}' \
    -e 'discovery-token-ca-cert-hash[=[:space:]]+sha256:[0-9a-fA-F]{64}' \
    -e '(apiEncryptionKey|api_encryption_key|certificateKey|certificate_key|client-key-data)[" ]*[:=][" ]*[A-Za-z0-9+/=]{20,}' \
    -e '(spireJoinToken|spire_join_token|joinToken|join_token)[" ]*[:=][" ]*[^",[:space:]]{12,}' \
    -e '-joinToken[=[:space:]]+[^[:space:]]{12,}' \
    -- "${files[@]}" >/dev/null; then
    return 1
  fi
  return 0
}

capture() {
  # capture <file> <command...>: best effort, one file per command.
  local out="${STATE_DIR}/$1"
  shift
  "$@" >"${out}" 2>&1 || true
}

# Every 30s: CAPI object conditions, nico-vms instances, libvirt domains. The
# create spec waits up to 90m, so a snapshot at the end says little.
start_state_monitor() {
  (
    while true; do
      captured_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      printf '=== %s CAPI ===\n' "${captured_at}"
      kubectl get clusters,machinedeployments,machines,kubeadmcontrolplanes,nicoclusters,nicomachines -A -o json 2>/dev/null |
        jq -c --arg captured_at "${captured_at}" '
          .items[] | {
            capturedAt: $captured_at,
            kind: .kind,
            namespace: .metadata.namespace,
            name: .metadata.name,
            phase: (.status.phase // null),
            ready: (.status.ready // null),
            providerID: (.spec.providerID // null),
            failureMessage: (.status.failureMessage // null),
            conditions: [(.status.conditions // [])[] | {type, status, reason, message}]
          }' 2>/dev/null || printf '(kubectl get failed)\n'
      printf '=== %s nico-vms instances ===\n' "${captured_at}"
      curl -fsS --max-time 5 -H "Authorization: Bearer ${NICO_VMS_TOKEN}" \
        "http://localhost:18080/v2/org/${NICO_VMS_ORG_ID}/forge/instance" 2>/dev/null |
        jq -c '.[]? | {id, name, status, ips: [(.interfaces // [])[] | .ipAddresses[]?]}' 2>/dev/null || printf '(nico-vms list failed)\n'
      printf '=== %s domains ===\n' "${captured_at}"
      virsh -c "${LIBVIRT_URI}" list --all 2>/dev/null | tail -n +3 | sed '/^$/d' || printf '(virsh failed)\n'
      sleep 30
    done
  ) >"${STATE_DIR}/state-timeline.log" 2>&1 &
  STATE_MONITOR_PID=$!
  # One line every 3 minutes on stdout too; a 90-minute spec is otherwise silent.
  (
    while true; do
      sleep 180
      machines="$(kubectl get machines -A --no-headers 2>/dev/null | awk '{print $4}' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')"
      instances="$(curl -fsS --max-time 5 -H "Authorization: Bearer ${NICO_VMS_TOKEN}" \
        "http://localhost:18080/v2/org/${NICO_VMS_ORG_ID}/forge/instance" 2>/dev/null | jq -r '[.[]?.status] | group_by(.) | map("\(.[0])=\(length)") | join(" ")' 2>/dev/null)"
      log "progress: machines[${machines:-none}] instances[${instances:-none}] domains=$(virsh -c "${LIBVIRT_URI}" list --name 2>/dev/null | grep -c . || true)"
    done
  ) &
  PROGRESS_MONITOR_PID=$!
}

stop_state_monitor() {
  local pid
  for pid in "${STATE_MONITOR_PID}" "${PROGRESS_MONITOR_PID}"; do
    [[ -n "${pid}" ]] || continue
    kill "${pid}" >/dev/null 2>&1 || true
    wait "${pid}" >/dev/null 2>&1 || true
  done
  STATE_MONITOR_PID=""
  PROGRESS_MONITOR_PID=""
}

# Guest-side facts over SSH, through the diag user nico-vms injects into every
# VM (NICO_VMS_DIAG_SSH_*). A node that never registers explains itself only here.
guest_diagnostics() {
  local user="${NICO_VMS_DIAG_SSH_USER:-nico-diag}" password="${NICO_VMS_DIAG_SSH_PASSWORD:-nico-diag}"
  local out_dir="${STATE_DIR}/guests" instances line name ip
  command -v sshpass >/dev/null 2>&1 || { log "sshpass is not installed; skipping guest diagnostics"; return 0; }
  instances="$(curl -fsS --max-time 5 -H "Authorization: Bearer ${NICO_VMS_TOKEN}" \
    "http://localhost:18080/v2/org/${NICO_VMS_ORG_ID}/forge/instance" 2>/dev/null |
    jq -r '.[]? | select(((.interfaces // [])[0].ipAddresses // []) | length > 0) | "\(.name) \(.interfaces[0].ipAddresses[0])"' 2>/dev/null)" || return 0
  [[ -n "${instances}" ]] || return 0
  mkdir -p "${out_dir}"
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    read -r name ip <<<"${line}"
    log "collecting guest diagnostics from ${name} (${ip})"
    # -n: ssh must not read the instance list from the loop's stdin.
    SSHPASS="${password}" timeout 180 sshpass -e ssh -n \
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
      -o ConnectTimeout=15 -o PreferredAuthentications=password -o PubkeyAuthentication=no \
      "${user}@${ip}" 'sudo bash -c '"'"'
        set +e
        crictl() { command crictl --runtime-endpoint unix:///run/containerd/containerd.sock "$@"; }
        echo "=== cloud-init ==="; cloud-init status --long 2>&1
        echo "=== cloud-init-output.log (last 200) ==="; tail -n 200 /var/log/cloud-init-output.log 2>&1
        echo "=== units ==="; systemctl --no-pager --no-legend list-units "nvke-agent*" "spire-agent*" kubelet containerd 2>&1
        echo "=== /etc/default/nvke-agent ==="; cat /etc/default/nvke-agent 2>&1
        echo "=== nvke-agent binary ==="; ls -l /usr/local/bin/nvke-agent 2>&1; /usr/local/bin/nvke-agent -version 2>&1
        echo "=== listening ==="; ss -ltnp 2>&1
        echo "=== network ==="; ip -brief address 2>&1; ip route show 2>&1; cat /etc/resolv.conf 2>&1
        echo "=== journal: nvke-agent spire-agent kubelet containerd (last 400) ==="; journalctl --no-pager -n 400 -u nvke-agent -u spire-agent -u kubelet -u containerd 2>&1
        echo "=== kubelet journal: pod adds, admission and ovn (last 60 matches) ==="; journalctl --no-pager -u kubelet 2>&1 | grep -E "SyncLoop ADD|ovnkube|ovn-kubernetes|Predicate|OutOf|admission|Failed to create pod sandbox" | tail -n 60
        echo "=== pods and containers on this node ==="; crictl pods 2>&1; crictl ps -a 2>&1
        echo "=== ovn-kubernetes containers (last 80 lines each) ==="
        for c in $(crictl ps -a --label io.kubernetes.pod.namespace=ovn-kubernetes -q 2>/dev/null); do
          echo "--- $(crictl inspect -o go-template --template "{{.status.metadata.name}}" "$c" 2>/dev/null) $c ---"; crictl logs --tail 80 "$c" 2>&1
        done
        if [ -f /etc/kubernetes/admin.conf ]; then
          export KUBECONFIG=/etc/kubernetes/admin.conf
          echo "=== tenant nodes ==="; kubectl get nodes -o wide --show-labels 2>&1
          echo "=== tenant pods ==="; kubectl get pods -A -o wide 2>&1
          echo "=== ovn-kubernetes workloads ==="; kubectl -n ovn-kubernetes get ds,deploy -o wide 2>&1; kubectl -n ovn-kubernetes describe ds 2>&1
          echo "=== tenant events (last 150) ==="; kubectl get events -A --sort-by=.lastTimestamp 2>&1 | tail -n 150
        fi
        echo "=== kubeadm ==="; ls -la /etc/kubernetes 2>&1; cat /etc/kubernetes/kubelet.conf >/dev/null 2>&1 && echo kubelet.conf present
        echo "=== disk ==="; df -h / 2>&1; lsblk -o NAME,TYPE,FSTYPE,SIZE,MOUNTPOINTS 2>&1
        echo "=== dmesg (last 60) ==="; dmesg 2>&1 | tail -n 60
      '"'"'' >"${out_dir}/${name}-${ip}.txt" 2>&1 || log "guest diagnostics from ${name} (${ip}) incomplete: exit $?"
  done <<<"${instances}"
}

# Serial consoles vanish with the instance; copy them first. ttyS1 is the FDE
# image's console.
save_console_logs() {
  local out_dir="${STATE_DIR}/console" f
  [[ -d "${NICO_SIM_DIR}/.dev/instances" ]] || return 0
  mkdir -p "${out_dir}"
  while IFS= read -r -d '' f; do
    cp -f "${f}" "${out_dir}/$(basename "$(dirname "${f}")")-$(basename "${f}")" 2>/dev/null
  done < <(find "${NICO_SIM_DIR}/.dev/instances" -type f -name '*ttyS*.log' -print0 2>/dev/null)
}

collect_diagnostics() {
  local rc=$?
  trap - EXIT
  set +e
  mkdir -p "${STATE_DIR}"
  stop_state_monitor
  # The suite runs from CONTROLLER_ROOT and writes the report beside its log.
  cp "${CONTROLLER_ROOT}/tenant-diagnostics.json" "${STATE_DIR}/" 2>/dev/null

  if kubectl cluster-info >/dev/null 2>&1; then
    capture management-nodes.txt kubectl get nodes -o wide
    capture management-pods.txt kubectl get pods -A -o wide
    capture management-events.txt kubectl get events -A --sort-by=.lastTimestamp
    capture cwe.log kubectl -n nvke-controller logs deployment/nvke-controller --all-containers --tail=-1
    capture capnico-controller.log kubectl -n capnico-system logs deployment/controller-manager --all-containers --tail=-1
    capture capi-controller.log kubectl -n capi-system logs deployment/capi-controller-manager --all-containers --tail=-1
    capture capi-kubeadm-bootstrap-controller.log kubectl -n capi-kubeadm-bootstrap-system logs deployment/capi-kubeadm-bootstrap-controller-manager --all-containers --tail=-1
    capture capi-kubeadm-control-plane-controller.log kubectl -n capi-kubeadm-control-plane-system logs deployment/capi-kubeadm-control-plane-controller-manager --all-containers --tail=-1
    capture capi-resources.yaml kubectl get clusters,machinedeployments,machinesets,machines,kubeadmcontrolplanes,kubeadmconfigs,nicoclusters,nicomachines,nicomachinetemplates -A -o yaml
    capture capi-describe.txt kubectl describe clusters,machines,kubeadmcontrolplanes,nicoclusters,nicomachines -A
  fi
  if [[ -n "${TILT_PGID}" ]]; then
    capture tilt-uiresources.json tilt_cmd get uiresources -o json
    capture tilt-engine.json tilt_cmd dump engine
    tilt_cmd snapshot create "${STATE_DIR}/tilt-snapshot.json" >/dev/null 2>&1 || true
    # Through the port-forward Tilt still holds open.
    if token="$(hack/nvke-token.sh 2>/dev/null)"; then
      curl -fsS --max-time 10 ${token:+-H "Authorization: Bearer ${token}"} \
        http://localhost:8080/api/clusters >"${STATE_DIR}/cwe-clusters.json" 2>&1 || true
    fi
  fi
  if [[ -d "${NICO_SIM_DIR}/.dev/logs" ]]; then
    mkdir -p "${STATE_DIR}/nico-vms"
    cp -f "${NICO_SIM_DIR}"/.dev/logs/*.log "${STATE_DIR}/nico-vms/" 2>/dev/null
  fi
  capture nico-vms-instances.json curl -fsS --max-time 5 -H "Authorization: Bearer ${NICO_VMS_TOKEN}" \
    "http://localhost:18080/v2/org/${NICO_VMS_ORG_ID}/forge/instance"
  guest_diagnostics
  save_console_logs
  capture virsh-list.txt virsh -c "${LIBVIRT_URI}" list --all
  capture host-routes.txt ip -4 route show

  if redact_diagnostics; then
    touch "${DIAGNOSTICS_MARKER}"
  else
    log "diagnostic redaction failed; refusing to mark the state directory safe to upload" >&2
    ((rc == 0)) && rc=1
  fi

  if [[ -n "${KEEP}" ]]; then
    log "NKE_E2E_KEEP set: leaving Tilt (port ${TILT_PORT}), kind cluster ${KIND_CLUSTER_NAME} and the VMs running"
    exit "${rc}"
  fi

  if [[ -n "${TILT_PGID}" ]]; then
    log "stopping Tilt and the processes it started (nico-vms, dnsmasq, port-forwards)"
    kill -TERM -- "-${TILT_PGID}" 2>/dev/null
    for _ in $(seq 1 30); do
      kill -0 -- "-${TILT_PGID}" 2>/dev/null || break
      sleep 1
    done
    kill -KILL -- "-${TILT_PGID}" 2>/dev/null
  fi
  # A failed run keeps its VMs (preserveOnFailure); logs were copied above.
  while IFS= read -r domain; do
    [[ -n "${domain}" ]] || continue
    virsh -c "${LIBVIRT_URI}" destroy "${domain}" >/dev/null 2>&1
    virsh -c "${LIBVIRT_URI}" undefine "${domain}" --nvram >/dev/null 2>&1 ||
      virsh -c "${LIBVIRT_URI}" undefine "${domain}" >/dev/null 2>&1
  done < <(virsh -c "${LIBVIRT_URI}" list --all --name 2>/dev/null | grep '^nico-vm-' || true)
  if [[ "${KIND_CLUSTER_OWNED}" == true ]]; then
    kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || log "kind delete cluster ${KIND_CLUSTER_NAME} failed" >&2
  fi
  if [[ -n "${TILT_CONFIG_BACKUP}" ]]; then
    if [[ -s "${TILT_CONFIG_BACKUP}" ]]; then
      cp -f "${TILT_CONFIG_BACKUP}" "${TILT_CONFIG}"
    else
      rm -f "${TILT_CONFIG}"
    fi
    rm -f "${TILT_CONFIG_BACKUP}"
  fi
  if ((rc == 0)); then
    log "PASSED; diagnostics in ${STATE_DIR}"
  else
    log "FAILED (exit ${rc}); diagnostics in ${STATE_DIR}" >&2
  fi
  exit "${rc}"
}
trap collect_diagnostics EXIT

# ── Preflight ───────────────────────────────────────────────────────────────

for command in curl docker go helm jq kind kubectl make setsid tilt virsh; do
  require_command "${command}"
done
[[ "$(uname -s)" == Linux ]] || fail "this entrypoint runs on a Linux host with KVM"
docker info >/dev/null 2>&1 || fail "cannot reach the Docker daemon"
docker buildx version >/dev/null 2>&1 || fail "docker buildx is required to build CAPNICo from source"

CAPNICO_SOURCE_DIR="${CAPNICO_SOURCE_DIR:-}"
[[ -n "${CAPNICO_SOURCE_DIR}" ]] || fail "CAPNICO_SOURCE_DIR must point at a checkout of dsx-ai-factory/cluster-api-provider-nico"
[[ -d "${CAPNICO_SOURCE_DIR}/config/default" ]] || fail "${CAPNICO_SOURCE_DIR} is not a cluster-api-provider-nico checkout (config/default is missing)"
export CAPNICO_SOURCE_DIR

BRIDGE_NAME="${NICO_VMS_BRIDGE_NAME:-controllerbr0}"
ip link show "${BRIDGE_NAME}" >/dev/null 2>&1 ||
  fail "bridge ${BRIDGE_NAME} does not exist; run test/kubeadm/scripts/setup-ci-host.sh with NKE_CI_HOST_FDE=true first"
if [[ -c /dev/kvm ]]; then
  log "/dev/kvm present; tenant nodes run with hardware acceleration"
else
  log "WARNING: /dev/kvm is missing; tenant nodes run under QEMU software emulation and the budgets below will not hold"
fi
[[ -s "${NICO_SIM_DIR}/images/ubuntu-cloud.qcow2" ]] ||
  fail "${NICO_SIM_DIR}/images/ubuntu-cloud.qcow2 is missing; setup-ci-host.sh fetches it (nico-vms requires it even on the iPXE path)"

if curl --fail --silent --connect-timeout 1 --max-time 1 "http://localhost:${TILT_PORT}/api/view" >/dev/null 2>&1; then
  fail "another Tilt session is running on port ${TILT_PORT}"
fi
if kind get clusters 2>/dev/null | grep -Fxq "${KIND_CLUSTER_NAME}"; then
  fail "kind cluster ${KIND_CLUSTER_NAME} already exists; pick another KIND_CLUSTER_NAME or delete it"
fi

mkdir -p "${STATE_DIR}"
rm -f "${DIAGNOSTICS_MARKER}"

# Placeholders the Tiltfile requires, same as the other lanes.
export DGXC_TENANT_USER="${DGXC_TENANT_USER:-nico-vms-e2e}"
export DGXC_TENANT_PASSWORD="${DGXC_TENANT_PASSWORD:-example-password}"
export CNPG_SUPERUSER_PASSWORD="${CNPG_SUPERUSER_PASSWORD:-example-password}"
export CNPG_CLOUD_PASSWORD="${CNPG_CLOUD_PASSWORD:-example-password}"
export CNPG_TEMPORAL_PASSWORD="${CNPG_TEMPORAL_PASSWORD:-example-password}"
export TENANT_ACCESS_ENABLED=true
export NKE_OIDC_AUTH_ENABLED=true
# Config profiles and runbooks only. OS images come from the releases below.
# NKE_CONTROLLER_PROFILES_DIR and CWE_PROFILES_DIR are the deprecated names for NVKE_CONTROLLER_PROFILES_DIR; #1399 removes them.
export NVKE_CONTROLLER_PROFILES_DIR="${NVKE_CONTROLLER_PROFILES_DIR:-${NKE_CONTROLLER_PROFILES_DIR:-${CWE_PROFILES_DIR:-${CONTROLLER_ROOT}/test/fixtures/profiles}}}"
# NKE_CONTROLLER_RELEASES_DIR is a deprecated name for NVKE_CONTROLLER_RELEASES_DIR; #1399 removes it.
RELEASES_SRC="${NVKE_CONTROLLER_RELEASES_DIR:-${NKE_CONTROLLER_RELEASES_DIR:-${CONTROLLER_ROOT}/test/fixtures/releases}}"
[[ -d "${RELEASES_SRC}" ]] || fail "releases not found at ${RELEASES_SRC}"
export NVKE_CONTROLLER_RELEASES_DIR="${STATE_DIR}/releases"
rm -rf "${NVKE_CONTROLLER_RELEASES_DIR}"
mkdir -p "${NVKE_CONTROLLER_RELEASES_DIR}"
cp -R "${RELEASES_SRC}/." "${NVKE_CONTROLLER_RELEASES_DIR}/"
# Inherited by Tilt's serve_cmd. -1 skips the console SSH listener; :2222 taken is fatal.
export NICO_VMS_CONSOLE_SSH_PORT="${NICO_VMS_CONSOLE_SSH_PORT:--1}"
export NICO_VMS_LIBVIRT_URI="${LIBVIRT_URI}"
export TILT_DISABLE_ANALYTICS=1
# Read by the Tiltfile. The agent then installs the CNI from the charts deb
# rather than fetching a bundle, as local-entrypoint-e2e already does.
export INSTALL_CNI_AFTER_INIT="${INSTALL_CNI_AFTER_INIT:-true}"

case "$(uname -m)" in
  x86_64) host_arch=amd64 ;;
  aarch64) host_arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

# ── The agent the tenant nodes run ──────────────────────────────────────────
# The release's userData apt-installs nvke-agent from the bundle server, then
# replaces it with http://carbide-pxe.forge:18080/files/node-agent/nvke-agent
# when that URL answers -- and silently keeps the packaged one otherwise. Build
# it from this checkout and stage it at the path the template names. The file
# root is set explicitly: the default sits beside the database in .dev/, which
# this script wipes.
export NICO_SIM_FILES_DIR="${NICO_SIM_DIR}/var/files"
# nico-sim boots guests of the host's architecture, so the specs use the
# matching release. Keep this name in step with nicoVMSRelease in
# test/e2e/tenant_test.go.
NODEPOOL_RELEASE="nvke-nico-vms-24.04-${host_arch}-fde"
AGENT_URL_PATH="$(grep -oE 'files/[a-z-]+/nvke-agent' "${NVKE_CONTROLLER_RELEASES_DIR}/${NODEPOOL_RELEASE}-nodepoolrelease.yaml" | head -1)"
[[ -n "${AGENT_URL_PATH}" ]] || fail "${NODEPOOL_RELEASE} no longer downloads nvke-agent from /files/"
log "building nvke-agent from ${WORKSPACE_ROOT}/agent"
make -C "${WORKSPACE_ROOT}/agent" "build-${host_arch}" >"${STATE_DIR}/agent-build.log" 2>&1 ||
  { tail -n 50 "${STATE_DIR}/agent-build.log" >&2; fail "building nvke-agent failed"; }
install -D -m 0755 "${WORKSPACE_ROOT}/agent/bin/linux_${host_arch}/nvke-agent" "${NICO_SIM_FILES_DIR}/${AGENT_URL_PATH#files/}"
log "staged nvke-agent for the tenant nodes: $(git -C "${WORKSPACE_ROOT}" describe --match 'agent/v*' --tags --always 2>/dev/null || echo unknown)"

# ── The node image the tenant VMs boot ──────────────────────────────────────
# Set NKE_E2E_OS_IMAGE_TAG to pin one image. Unset, the newest published one
# for this architecture is resolved below, because the profile's own image-url
# is CloudFront and the self-hosted fleet gets 403 from it.
#
# No server: nico-sim resolves image-url on the host, never in a guest, and
# copies a local path straight into its image cache. See localImagePath and
# copyLocal in nico-sim/internal/ipxe/imagecache.go, whose own comment gives
# the reason -- an image built in the same job is already on disk.
# The newest tag GHCR holds for this architecture, built from a commit on the
# branch under test: sha-<hex> from a tag or a dispatch, main-sha-<hex> from a
# run on main. Registry tag order is not chronological and the hex carries no
# ordering, so this reads updated_at from the packages API. PR builds publish
# here too, so a tag counts only if its commit is an ancestor of HEAD (the
# checkout has fetch-depth 0). Flavor tags (-ubuntu24.04-) are skipped. The
# -<arch> suffix comes off, because image_ref adds it back.
newest_os_image_tag() {
  local arch="$1" repo path org pkg tag sha
  # Keep this in step with image_repo below: both lanes booted an LVM image
  # before this, and -fde is a different boot path that has to unlock first.
  repo="${NKE_E2E_OS_IMAGE_REPO:-ghcr.io/dsx-ai-factory/nvke/os-image-non-fde}"
  path="${repo#ghcr.io/}"
  org="${path%%/*}"
  pkg="${path#*/}"
  while read -r tag; do
    sha="${tag#main-}"
    sha="${sha#sha-}"
    sha="${sha%%-*}"
    if ! git -C "${WORKSPACE_ROOT}" merge-base --is-ancestor "${sha}" HEAD 2>/dev/null; then
      log "skipping os-image ${tag}: commit ${sha:0:9} is not on this branch" >&2
      continue
    fi
    printf '%s\n' "${tag%-"${arch}"}"
    return 0
  done < <(gh api --paginate \
      "/orgs/${org}/packages/container/${pkg//\//%2F}/versions?per_page=100" \
      --jq '(if type == "array" then . else [] end)[] | {u: .updated_at, t: (.metadata.container.tags // [])[]}' 2>/dev/null |
    jq -rs --arg a "-${arch}" \
      'map(select(.t | (startswith("sha-") or startswith("main-sha-")) and endswith($a) and (contains("-ubuntu") | not)))
       | sort_by(.u) | reverse | .[].t')
  return 1
}

os_image_tag_pinned=no
[[ -n "${NKE_E2E_OS_IMAGE_TAG:-}" ]] && os_image_tag_pinned=yes

if [[ -z "${NKE_E2E_OS_IMAGE_TAG:-}" ]]; then
  NKE_E2E_OS_IMAGE_TAG="$(newest_os_image_tag "${host_arch}" || true)"
  # No fallback. The profile's image-url is CloudFront, and this lane does not
  # depend on it. Stopping here costs a minute; falling back spends the whole
  # run to arrive somewhere we have decided not to go.
  [[ -n "${NKE_E2E_OS_IMAGE_TAG}" ]] ||
    fail "no os-image built from this branch is published for ${host_arch}. Build one with os-image-artifact.yml, or set NKE_E2E_OS_IMAGE_TAG."
  log "newest published os-image for ${host_arch}: ${NKE_E2E_OS_IMAGE_TAG}"
fi

if [[ -n "${NKE_E2E_OS_IMAGE_TAG:-}" ]]; then
  command -v oras >/dev/null 2>&1 ||
    fail "NKE_E2E_OS_IMAGE_TAG is set but oras is not on PATH"
  image_repo="${NKE_E2E_OS_IMAGE_REPO:-ghcr.io/dsx-ai-factory/nvke/os-image-non-fde}"
  image_ref="${image_repo}:${NKE_E2E_OS_IMAGE_TAG}-${host_arch}"
  image_dir="${STATE_DIR}/os-image"
  rm -rf "${image_dir}"
  mkdir -p "${image_dir}"
  log "pulling ${image_ref}"
  # oras has no retry of its own, and the qcow2 layer is big enough that a
  # mid-stream registry error ends the lane before the test starts.
  for attempt in 1 2 3; do
    oras pull "${image_ref}" --platform "linux/${host_arch}" -o "${image_dir}" \
      >"${STATE_DIR}/os-image-pull.log" 2>&1 && break
    if ((attempt < 3)); then
      log "pulling ${image_ref} failed (attempt ${attempt}/3), retrying in $((attempt * 15))s"
      # A partial pull leaves files the qcow2 count below rejects.
      rm -rf "${image_dir}"
      mkdir -p "${image_dir}"
      sleep $((attempt * 15))
      continue
    fi
    tail -n 20 "${STATE_DIR}/os-image-pull.log" >&2
    # One tag input covers every leg, and each appends its own architecture.
    # Pinning a tag built for one architecture fails the others here.
    if [[ "${os_image_tag_pinned}" == yes ]]; then
      fail "pulling ${image_ref} failed. A pinned tag needs an image for every architecture the run covers; this leg wanted -${host_arch}. Build it, or leave the tag empty to resolve one per architecture."
    fi
    fail "pulling ${image_ref} failed"
  done

  shopt -s nullglob
  pulled=("${image_dir}"/*.qcow2)
  shopt -u nullglob
  (( ${#pulled[@]} == 1 )) ||
    fail "${image_ref} carried ${#pulled[@]} qcow2 files, expected exactly 1"
  qcow="${pulled[0]}"

  # Repoint every MachineImage that names a real image. Counted, because an
  # image that stopped carrying an image-url would otherwise leave the CloudFront
  # pin in place and the lane would quietly test the wrong thing. The script is a
  # YAML block scalar, so the line keeps its indentation.
  repointed=0
  for manifest in "${NVKE_CONTROLLER_RELEASES_DIR}"/*-machineimage.yaml; do
    grep -Eq '^ +set image-url ' "${manifest}" || continue
    # Not sed -i: it takes an argument on BSD and none on GNU, and this script
    # is run by hand on both.
    awk -v img="${qcow}" '
      /^ +set image-url / { match($0, /^ +/); print substr($0, 1, RLENGTH) "set image-url " img; next }
      { print }' "${manifest}" >"${manifest}.new" && mv "${manifest}.new" "${manifest}"
    repointed=$(( repointed + 1 ))
  done
  (( repointed > 0 )) ||
    fail "no MachineImage iPXE script carries a 'set image-url' line to repoint"
  # Every releases manifest, not just the ipxeScript. The bundle server sat in
  # userData.tmpl and this check used to walk straight past it, so the lane
  # reported "no CloudFront left" while still shipping a CloudFront URL to
  # every node it created.
  if grep -rl cloudfront.net "${NVKE_CONTROLLER_RELEASES_DIR}" 2>/dev/null; then
    fail "the manifest(s) listed above still name CloudFront after repointing"
  fi
  log "tenant nodes boot ${qcow} (${repointed} image(s) repointed, no CloudFront left)"
fi

# A previous run's database, instances and logs would confuse this one.
rm -rf "${NICO_SIM_DIR}/.dev"

# ── Images Tilt must not build ──────────────────────────────────────────────
# Tilt's own controller build watches bin/ and rewrites bin/cloud-workflow-engine, so
# it re-queues itself forever. Prebuild both images, as ci/backend-e2e.sh does.
git_sha="$(git -C "${WORKSPACE_ROOT}" rev-parse HEAD)"
git_short_sha="$(git -C "${WORKSPACE_ROOT}" rev-parse --short=12 HEAD)"
NVKE_CONTROLLER_IMAGE="cwe-e2e:${git_short_sha}"
NVKE_CONTROLLER_BUILD_VERSION="e2e-${git_short_sha}"
NVKE_CONTROLLER_PROFILES_TILT_IMAGE="docker.local/cwe-profiles:tilt"

# Both host builds pull their bases through the mirror; see dockerhub_build_args.
log "building the profiles fixture image ${NVKE_CONTROLLER_PROFILES_TILT_IMAGE}"
mapfile -t profiles_build_args < <(dockerhub_build_args test/fixtures/Dockerfile)
docker build --platform "linux/${host_arch}" "${profiles_build_args[@]}" \
  --file test/fixtures/Dockerfile --tag "${NVKE_CONTROLLER_PROFILES_TILT_IMAGE}" test/fixtures \
  >"${STATE_DIR}/profiles-image-build.log" 2>&1 ||
  { tail -n 40 "${STATE_DIR}/profiles-image-build.log" >&2; fail "building the profiles image failed"; }

log "building the controller image ${NVKE_CONTROLLER_IMAGE}"
mapfile -t cwe_build_args < <(dockerhub_build_args "${CONTROLLER_ROOT}/Dockerfile")
DOCKER_BUILDKIT=1 docker build --platform "linux/${host_arch}" "${cwe_build_args[@]}" \
  --file "${CONTROLLER_ROOT}/Dockerfile" \
  --build-arg "BUILD_VERSION=${NVKE_CONTROLLER_BUILD_VERSION}" \
  --build-arg "GIT_VERSION=${git_sha}" \
  --tag "${NVKE_CONTROLLER_IMAGE}" "${WORKSPACE_ROOT}" \
  >"${STATE_DIR}/cwe-image-build.log" 2>&1 ||
  { tail -n 60 "${STATE_DIR}/cwe-image-build.log" >&2; fail "building the controller image failed"; }
export NVKE_CONTROLLER_PROFILES_PRELOADED=true

# ── Management cluster: kind + CAPI core ────────────────────────────────────
log "creating the kind management cluster ${KIND_CLUSTER_NAME} with CAPI core"
KIND_CLUSTER_OWNED=true
# With a Docker Hub mirror set, the node image, the node's own containerd and
# the pull-through cache image all go through it, as in the backend lanes. The
# node first, so nothing capi-core-up starts pulls from Docker Hub directly.
kind_config_for_mirror hack/controller-capi-kind.yaml "${STATE_DIR}/kind-config.yaml"
make -C "${CONTROLLER_ROOT}" kind-up KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
  KIND_CLUSTER_CONFIG="${STATE_DIR}/kind-config.yaml"
configure_kind_dockerhub_mirror "${KIND_CLUSTER_NAME}"
make -C "${CONTROLLER_ROOT}" capi-core-up KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
  REGISTRY_MIRROR_IMAGE="$(dockerhub_image registry:2)"
kind get kubeconfig --name "${KIND_CLUSTER_NAME}" >"${E2E_KUBECONFIG}"
log "loading ${NVKE_CONTROLLER_IMAGE} and ${NVKE_CONTROLLER_PROFILES_TILT_IMAGE} into the kind cluster"
kind load docker-image --name "${KIND_CLUSTER_NAME}" "${NVKE_CONTROLLER_IMAGE}" "${NVKE_CONTROLLER_PROFILES_TILT_IMAGE}"

# ── Tilt: controller, SPIRE, nico-vms, CAPNICo ─────────────────────────────────────
# No site_controller_vip: the kind node's container address is not routable
# from the VM bridge (SPIRE dials time out). Without the override the Tiltfile
# binds the SPIRE and OIDC NodePorts on the kind gateway with port-forwards,
# and the VMs reach that host address.
if [[ -e "${TILT_CONFIG}" ]]; then
  TILT_CONFIG_BACKUP="$(mktemp)"
  cp -f "${TILT_CONFIG}" "${TILT_CONFIG_BACKUP}"
else
  TILT_CONFIG_BACKUP="$(mktemp)"
  : >"${TILT_CONFIG_BACKUP}"
fi
jq -n \
  --arg capnico "${CAPNICO_SOURCE_DIR}" \
  --arg nvke_controller_image "${NVKE_CONTROLLER_IMAGE}" \
  '{
    providers: ["capi_core", "nico"],
    nvke_controller_image: $nvke_controller_image,
    capnico_source_dir: $capnico,
    nico_sim_backends: ["vm"],
    nico_vms_observability: false
  }' >"${TILT_CONFIG}"

log "starting Tilt on port ${TILT_PORT} (log: ${STATE_DIR}/tilt.log)"
# setsid: one process group, so cleanup can stop nico-vms and the port-forwards too.
setsid tilt up --port "${TILT_PORT}" --stream=true >"${STATE_DIR}/tilt.log" 2>&1 &
tilt_pid=$!
TILT_PGID="$(ps -o pgid= -p "${tilt_pid}" | tr -d ' ')"
[[ -n "${TILT_PGID}" ]] || fail "could not read Tilt's process group"

# What `tilt ci` waits for: no enabled resource pending, none in error. Manual
# resources report "none" and never block. A healthy bring-up takes ~8m here,
# so a stall (no state change for NKE_E2E_TILT_STALL) fails before the cap.
wait_for_tilt() {
  local deadline=$((SECONDS + $(( $(echo "${TILT_TIMEOUT}" | sed 's/m$//') * 60 )) ))
  local stall_limit=$(( $(echo "${NKE_E2E_TILT_STALL:-12m}" | sed 's/m$//') * 60 ))
  local state errors pending total last_state="" last_change=${SECONDS}
  while true; do
    if ! kill -0 "${tilt_pid}" 2>/dev/null; then
      tail -n 100 "${STATE_DIR}/tilt.log" >&2
      fail "Tilt exited early"
    fi
    if state="$(tilt_cmd get uiresources -o json 2>/dev/null | jq -r '
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
          tilt_cmd logs "${name}" 2>/dev/null | tail -n 150
          echo "::endgroup::"
        done
        fail "Tilt resources failed: ${errors}"
      fi
      if [[ -z "${pending}" && "${total}" -gt 1 ]]; then
        log "Tilt is ready: ${total} resources"
        return 0
      fi
      if [[ -n "${pending}" ]] && ((SECONDS - last_change >= stall_limit)); then
        for name in ${pending//,/ }; do
          echo "::group::tilt logs ${name}"
          tilt_cmd logs "${name}" 2>/dev/null | tail -n 150
          echo "::endgroup::"
        done
        fail "no Tilt resource changed state for ${NKE_E2E_TILT_STALL:-12m}; still pending: ${pending}"
      fi
    fi
    if ((SECONDS >= deadline)); then
      tilt_cmd get uiresources 2>/dev/null || true
      fail "Tilt did not become ready within ${TILT_TIMEOUT}; still pending: ${pending:-unknown}"
    fi
    sleep 15
  done
}
wait_for_tilt

kubectl -n nvke-controller rollout status deployment/nvke-controller --timeout=5m
# Tilt passes both as chart values, which the chart renders into nvke-controller-config.
for setting in tenantAccess.enabled nvkeOIDCAuth.enabled; do
  value="$(helm -n nvke-controller get values nvke-controller --all -o json | jq -r ".${setting}")"
  [[ "${value}" == "true" ]] || fail "${setting} must be true in the nvke-controller release, got ${value:-<empty>}"
done
kubectl -n capnico-system rollout status deployment/controller-manager --timeout=5m
curl -fsS --max-time 5 http://localhost:18080/healthz >/dev/null || fail "nico-vms does not answer on localhost:18080"
curl -fsS --max-time 5 "http://localhost:18080/${AGENT_URL_PATH}" >/dev/null ||
  fail "nico-vms does not serve the branch-built nvke-agent at /${AGENT_URL_PATH}"
log "management plane is up: the controller, CAPNICo and nico-vms all answer"

# ── The suite ───────────────────────────────────────────────────────────────
# Through go.work: dependencies resolve from public modules and the Go cache.
log "compiling the E2E suite"
# `|` makes go fall back to a direct fetch on any proxy error; the default
# `,` only does so on 404/410. See the same block in backend-e2e.sh.
export GOPROXY="${GOPROXY:-https://proxy.golang.org|direct}"
e2e_built=false
for attempt in 1 2 3; do
  if (cd test/e2e && CGO_ENABLED=0 go test -c -o "${E2E_BIN}" .); then
    e2e_built=true
    break
  fi
  log "E2E compile attempt ${attempt} failed"
  sleep $((attempt * 20))
done
[[ "${e2e_built}" == true ]] || fail "compiling the E2E suite failed three times"

jq -n \
  --arg kubeconfig "${E2E_KUBECONFIG}" \
  --arg context "kind-${KIND_CLUSTER_NAME}" \
  '{
    kube: {kubeconfig: $kubeconfig, context: $context},
    lifecycle: {preserveOnFailure: true}
  }' >"${E2E_CONFIG}"

# -test.timeout above -ginkgo.timeout, so Ginkgo reports before Go kills it.
ginkgo_minutes="$(echo "${GINKGO_TIMEOUT}" | sed 's/m$//')"
test_timeout="$((ginkgo_minutes + 15))m"

# fail-fast: a failed entry keeps its 4-6 VMs; a second entry would not fit and
# would ask for the same control-plane VIP.
log "running E2E specs matching ${LABEL_FILTER} (ginkgo timeout ${GINKGO_TIMEOUT})"
start_state_monitor
set +e
env NVKE_CONTROLLER_RELEASES_DIR="${CONTROLLER_ROOT}/test/fixtures/fleet-releases" \
  NVKE_CONTROLLER_EXPECTED_VERSION="${NVKE_CONTROLLER_BUILD_VERSION}" \
  "${E2E_BIN}" \
  -test.v \
  -test.timeout="${test_timeout}" \
  -e2e.config="${E2E_CONFIG}" \
  -ginkgo.v \
  -ginkgo.no-color \
  -ginkgo.fail-on-empty \
  -ginkgo.fail-fast \
  -ginkgo.timeout="${GINKGO_TIMEOUT}" \
  "-ginkgo.label-filter=${LABEL_FILTER}" \
  "-ginkgo.junit-report=${STATE_DIR}/junit.xml" \
  2>&1 | tee "${STATE_DIR}/e2e.log"
e2e_rc=${PIPESTATUS[0]}
set -e
stop_state_monitor

if ((e2e_rc == 0)); then
  # Only the multi-pod-cidrs entry sets two pod CIDRs. OIDC is on here, so an
  # oidc_test.go skip is a regression, unlike in the CAPD lanes.
  # shellcheck disable=SC2034  # read by e2e_assert_no_new_skips
  E2E_EXPECTED_RUNTIME_SKIPS=(pod_cidr_test.go)
  e2e_assert_no_new_skips log "${STATE_DIR}/e2e.log" || e2e_rc=1
fi

((e2e_rc == 0)) || fail "E2E specs matching ${LABEL_FILTER} failed with exit code ${e2e_rc}"
log "nico-vms E2E passed: ${LABEL_FILTER}"

# After the suite, like the CAPD and NICo lanes: drive the real binary against
# the controller this lane deployed. Called bare so errexit and the EXIT trap turn a
# non-zero into a failed job with diagnostics intact.
if [[ "${NVKECTL_SMOKE}" == "true" ]]; then
  nvkectl_smoke_build "$(go env GOARCH)"
  nvkectl_smoke
fi
