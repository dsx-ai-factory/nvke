#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# `nkectl bootstrap site` e2e for `infrastructure: nico`: the control-plane
# machines are allocated by CAPNICo from a NICo site instead of taken from a
# hardcoded list. The sibling of run.sh, which covers `infrastructure:
# provisioned`; same host requirements (see run.sh), plus a Go toolchain to
# build nico-sim, and network access to ghcr.io for the CAPNICo chart.
#
# Stages:
#   1. Images: the CAPD node image nico-sim creates its nodes from, and a CWE
#      profiles image built from the in-repo fixture tree.
#   2. nico-sim, run as a host process with the container backend pointed at
#      nerdctl, on a node network this script creates.
#   3. `bootstrap site` with a nico site spec, run in the background. As soon as
#      its ephemeral Kind cluster is ready this script permits forwarding between
#      the Kind bridge and the node bridge, which the CNI firewall otherwise
#      drops. The command then deploys everything, including CAPNICo, which asks
#      nico-sim for the machines.
#   4. The cluster the command built: its nodes are Ready, each node's spire-agent
#      attested to the bootstrap SPIRE server over TPM, and the allowlist that
#      admitted it was filled in by CWE from the NicoMachine, not by the site spec.
#
# Two things nkectl does not supply and CWE needs to provision a node through the
# NICo path, which this script hands it through env hooks:
#   - NKECTL_CWE_PROFILES_IMAGE: the OS and config profiles a cluster request
#     names. nkectl deploys CWE with an empty profiles directory.
#   - NKECTL_CWE_TENANT_CREDENTIALS_SECRET: the login CWE puts on each node. The
#     Secret is created here, with throwaway values, as soon as the bootstrap
#     cluster exists.
#
# Everything created is registered for teardown as it is created, and the EXIT
# trap runs those registrations in reverse, whatever the exit.

set -Eeuo pipefail

# ── Configuration ──────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
REPO_ROOT="$(dirname "${CLI_DIR}")"
NKECTL="${NKECTL:-${CLI_DIR}/bin/nkectl}"
NICO_SIM_DIR="${REPO_ROOT}/controller/nico-sim"
PROFILES_FIXTURE_DIR="${REPO_ROOT}/controller/test/fixtures"

E2E_CLUSTER_NAME="nke-e2e-nico"
BOOTSTRAP_KIND_CLUSTER="${NKECTL_BOOTSTRAP_CLUSTER_NAME:-nkectl-bootstrap}"
BOOTSTRAP_CONTROL_PLANE="${BOOTSTRAP_KIND_CLUSTER}-control-plane"

# How many control-plane nodes CAPNICo is asked for. Odd, like the hosts list it
# stands in for.
NODE_COUNT="${NKECTL_E2E_NICO_NODES:-1}"
NODE_NETWORK="${NKECTL_E2E_NICO_NETWORK:-nkectl-e2e-nico-nodes}"
NODE_SUBNET="${NKECTL_E2E_NICO_SUBNET:-172.28.0.0/24}"

SIM_PORT="${NKECTL_E2E_NICO_SIM_PORT:-18080}"
SIM_BIN="${CLI_DIR}/.local/nico-sim"
SIM_STATE_DIR="$(mktemp -d)"
SIM_LOG="${SIM_STATE_DIR}/nico-sim.log"
NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE:-cwe-capd-kindest-node:local}"
PROFILES_IMAGE="${NKECTL_E2E_PROFILES_IMAGE:-cwe-profiles:nkectl-e2e}"
# nico-sim's managed-container label, which is how teardown finds what it made.
SIM_LABEL="nvidia.com/nico-containers.managed=true"

# What nico-sim seeds for its container site. The IDs are the simulator's own;
# see controller/hack/create-local-nico-cluster.sh, which uses the same ones.
NICO_ORG_ID="${NKECTL_E2E_NICO_ORG_ID:-novxwn552ssc}"
NICO_SITE_ID="00000000-0000-0000-0000-000000000001"
NICO_VPC_ID="00000000-0000-0000-0000-000000000002"
NICO_SUBNET_ID="00000000-0000-0000-0000-000000000003"
NICO_SSH_KEY_GROUP_ID="00000000-0000-0000-0000-000000000004"
NICO_INSTANCE_TYPE_ID="00000000-0000-0000-0000-000000000005"
# Profiles from the fixture tree the profiles image is built from.
OS_PROFILE="${NKECTL_E2E_NICO_OS_PROFILE:-nke-local-dev-kindest-arm64}"
CONFIG_PROFILE="${NKECTL_E2E_NICO_CONFIG_PROFILE:-dev-only-empty}"

SPIRE_TRUST_DOMAIN="${NKECTL_E2E_SPIRE_TRUST_DOMAIN:-nkectl-e2e.example.test}"
# CWE's namespace in the bootstrap cluster, and the Secret that holds the login
# it gives the nodes it provisions.
CWE_NAMESPACE=nke-controller
TENANT_SECRET="nke-e2e-tenant-credentials"

TENANT_KUBECONFIG="${SCRIPT_DIR}/.tenant-nico.kubeconfig"
RENDERED_SITESPEC="${SCRIPT_DIR}/.sitespec.nico.rendered.yaml"
BOOTSTRAP_LOG="${SCRIPT_DIR}/.bootstrap-nico.log"
DIAGNOSTICS_DIR="${CLI_DIR}/cluster-diagnostics"
export NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR="${DIAGNOSTICS_DIR}"

: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already present in containerd (see run.sh)}"
export NKECTL_CWE_IMAGE

# `kind create cluster` merges into $KUBECONFIG; use a scratch path so this run
# never takes ownership of the caller's. See run.sh for the sudo -E reasoning.
KUBECONFIG_DIR="$(mktemp -d)"
export KUBECONFIG="${KUBECONFIG_DIR}/config"
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"
CNI_BUNDLE_DIR="$(mktemp -d)"
CREDENTIALS_FILE="${SIM_STATE_DIR}/nico-credentials.env"

# ── Helpers ────────────────────────────────────────────────────────────────

log() { printf '[nkectl-bootstrap-nico-e2e] %s\n' "$*"; }

fail() {
  log "FAIL: $1"
  shift
  local line
  for line in "$@"; do log "  ${line}"; done
  exit 1
}

# defer registers a command for the EXIT trap, which runs registrations in
# reverse order. Register the teardown *before* the operation it undoes.
DEFERRED=()
defer() { DEFERRED+=("$*"); }

collect_diagnostics() {
  log "collecting diagnostics"
  {
    printf '### nerdctl ps -a\n'; nerdctl ps -a || true
    printf '\n### nico-sim log (tail)\n'; tail -n 80 "${SIM_LOG}" 2>/dev/null || true
    local c
    for c in $(nerdctl ps -aq --filter "label=${SIM_LABEL}" 2>/dev/null); do
      printf '\n### node container %s: journal (tail)\n' "${c}"
      nerdctl exec "${c}" journalctl --no-pager -n 60 2>&1 || true
    done
  } >"${SCRIPT_DIR}/.nico-diagnostics.txt" 2>&1 || true
  log "wrote ${SCRIPT_DIR}/.nico-diagnostics.txt"
}

started=false
cleanup() {
  local status=$? i
  if [ "${status}" -ne 0 ] && [ "${started}" = true ]; then
    collect_diagnostics
  fi
  for ((i = ${#DEFERRED[@]} - 1; i >= 0; i--)); do
    eval "${DEFERRED[$i]}" || true
  done
  if [ -n "${SUDO_UID:-}" ] && [ -d "${DIAGNOSTICS_DIR}" ]; then
    chown -R "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${DIAGNOSTICS_DIR}" || true
  fi
  rm -f "${TENANT_KUBECONFIG}" "${RENDERED_SITESPEC}" "${BOOTSTRAP_LOG}"
  rm -rf "${KUBECONFIG_DIR}" "${CNI_BUNDLE_DIR}" "${SIM_STATE_DIR}"
}
trap cleanup EXIT

# wait_until retries CMD every INTERVAL seconds until it succeeds or DEADLINE
# seconds have passed.
wait_until() {
  local deadline=$((SECONDS + $1)) interval="$2"
  shift 2
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if "$@"; then return 0; fi
    sleep "${interval}"
  done
  return 1
}

# gcc: nico-sim stores its state in SQLite through cgo. Go builds without cgo,
# silently, when it finds no C compiler, and the binary then fails at startup.
for tool in nerdctl kind kubectl helm clusterctl go gcc make iptables ip; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done
[ -x "${NKECTL}" ] || fail "${NKECTL} is not built (run make -C cli build)"
if (( NODE_COUNT < 1 || NODE_COUNT % 2 == 0 )); then
  fail "NKECTL_E2E_NICO_NODES must be odd and at least 1, got ${NODE_COUNT}"
fi

# ── Teardowns, one per thing created ───────────────────────────────────────

stop_sim() {
  log "stopping nico-sim"
  if [ -s "${SIM_STATE_DIR}/nico-sim.pid" ]; then
    kill "$(cat "${SIM_STATE_DIR}/nico-sim.pid")" 2>/dev/null || true
  fi
}
remove_sim_nodes() {
  local ids
  ids="$(nerdctl ps -aq --filter "label=${SIM_LABEL}" 2>/dev/null || true)"
  if [ -n "${ids}" ]; then
    log "removing the node containers nico-sim created"
    # shellcheck disable=SC2086
    nerdctl rm -f ${ids} >/dev/null 2>&1 || true
  fi
}
remove_node_network() {
  log "removing the node network ${NODE_NETWORK}"
  nerdctl network rm "${NODE_NETWORK}" >/dev/null 2>&1 || true
}
teardown_bootstrap_cluster() {
  log "tearing down the kept bootstrap kind cluster"
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
}
remove_forward_rules() {
  log "removing the cross-bridge forwarding rules"
  iptables -D FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$2" -o "$1" -j ACCEPT 2>/dev/null || true
}

# ── Stage 1: images ────────────────────────────────────────────────────────

started=true

log "building the Calico manifests package for post-init CNI"
BUNDLE_ARCH="$(go env GOARCH)" \
  bash "${REPO_ROOT}/controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}"
CAPD_CNI_MANIFESTS_DEB_PATH="${CNI_BUNDLE_DIR}/nke-cni-calico-manifests-3.30.1_3.30.1_all.deb"
export CAPD_CNI_MANIFESTS_DEB_PATH
[ -f "${CAPD_CNI_MANIFESTS_DEB_PATH}" ] || fail "no Calico manifests package was built"

# nico-sim creates its nodes from this image, which already carries nke-agent,
# swtpm and the SPIRE plumbing. A no-op when it exists.
log "ensuring the node image ${NODE_IMAGE}"
make -C "${CLI_DIR}" ensure-capd-image CAPD_KINDEST_NODE_IMAGE="${NODE_IMAGE}"
nerdctl image inspect "${NODE_IMAGE}" >/dev/null 2>&1 || fail "node image ${NODE_IMAGE} is not in containerd"

# shellcheck disable=SC1091
source "${REPO_ROOT}/controller/hack/backend-e2e-lib.sh"
mapfile -t mirror_args < <(dockerhub_build_args "${PROFILES_FIXTURE_DIR}/Dockerfile")
log "building the CWE profiles image ${PROFILES_IMAGE} from ${PROFILES_FIXTURE_DIR}"
nerdctl build --file "${PROFILES_FIXTURE_DIR}/Dockerfile" --tag "${PROFILES_IMAGE}" \
  "${mirror_args[@]}" "${PROFILES_FIXTURE_DIR}" >/dev/null
# nkectl loads it into the Kind cluster from here, so it must be a tag other
# than latest: the chart sets no pull policy on it.
export NKECTL_CWE_PROFILES_IMAGE="${PROFILES_IMAGE}"
export NKECTL_CWE_TENANT_CREDENTIALS_SECRET="${TENANT_SECRET}"
log "PASS: the node and profiles images are in containerd"

# ── Stage 2: nico-sim ──────────────────────────────────────────────────────

log "building nico-sim"
mkdir -p "$(dirname "${SIM_BIN}")"
(cd "${NICO_SIM_DIR}" && CGO_ENABLED=1 go build -o "${SIM_BIN}" ./cmd/nico-sim)

# nerdctl cannot be told a subnet's gateway, but it assigns the first address.
log "creating the node network ${NODE_NETWORK} (${NODE_SUBNET})"
nerdctl network rm "${NODE_NETWORK}" >/dev/null 2>&1 || true
defer remove_node_network
nerdctl network create --driver bridge --subnet "${NODE_SUBNET}" "${NODE_NETWORK}" >/dev/null
GATEWAY="$(nerdctl network inspect "${NODE_NETWORK}" \
  --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' |
  awk '{for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}$/) {print $i; exit}}')"
[ -n "${GATEWAY}" ] || fail "could not read the ${NODE_NETWORK} gateway"

# A nerdctl network is only configuration until a container attaches: the bridge,
# and the host's gateway address on it, do not exist yet. Both are needed before
# nico-sim creates its first node. CAPNICo is pointed at the gateway address, and
# the forwarding rules below name the bridge. This container holds the bridge up
# for the length of the run.
ANCHOR="nkectl-e2e-nico-anchor"
remove_anchor() { nerdctl rm -f "${ANCHOR}" >/dev/null 2>&1 || true; }
defer remove_anchor
nerdctl rm -f "${ANCHOR}" >/dev/null 2>&1 || true
nerdctl run -d --name "${ANCHOR}" --network "${NODE_NETWORK}" --entrypoint sleep "${NODE_IMAGE}" infinity >/dev/null ||
  fail "could not start the container that holds the node bridge up"
gateway_up() { ip -4 -o addr show | grep -q "inet ${GATEWAY}/"; }
wait_until 30 1 gateway_up || fail "the host never got ${GATEWAY} on the node bridge"
log "the host is ${GATEWAY} on the node network: nico-sim's endpoint and the SPIRE address nodes use"

# The environment mirrors controller/hack/nico-provider/run-nico-sim.sh, with
# the container CLI swapped for nerdctl and the node network named explicitly.
log "starting nico-sim on :${SIM_PORT}"
defer remove_sim_nodes
defer stop_sim
(
  cd "${NICO_SIM_DIR}"
  mkdir -p "${SIM_STATE_DIR}/files"
  export NICO_SIM_LISTEN_ADDR=":${SIM_PORT}"
  export NICO_SIM_DB_PATH="${SIM_STATE_DIR}/nico-sim.db"
  export NICO_SIM_FILES_DIR="${SIM_STATE_DIR}/files"
  export NICO_VMS_VM_BACKEND=mock
  export NICO_VMS_BGP_ENABLED=false
  export NICO_VMS_BRIDGE_CIDR=172.30.0.0/24
  export NICO_VMS_BGP_VIP_CIDRS=172.31.0.0/24
  export NICO_VMS_VM_DISK_GIB=40
  export NICO_CONTAINERS_ENABLED=true
  export NICO_CONTAINERS_NODE_IMAGE="${NODE_IMAGE}"
  export NICO_CONTAINERS_NODE_NETWORK="${NODE_NETWORK}"
  export NICO_CONTAINERS_DOCKER_BINARY=nerdctl
  export NICO_CONTAINERS_REQUIRE_HEALTHY=true
  setsid "${SIM_BIN}" >"${SIM_LOG}" 2>&1 &
  echo $! >"${SIM_STATE_DIR}/nico-sim.pid"
)
sim_up() {
  kill -0 "$(cat "${SIM_STATE_DIR}/nico-sim.pid")" 2>/dev/null || fail "nico-sim exited" "$(tail -n 20 "${SIM_LOG}")"
  (exec 3<>"/dev/tcp/127.0.0.1/${SIM_PORT}") 2>/dev/null
}
wait_until 120 1 sim_up || fail "nico-sim did not listen on :${SIM_PORT} within 120s" "$(tail -n 20 "${SIM_LOG}")"
log "PASS: nico-sim is up on :${SIM_PORT}"

# ── Stage 3: bootstrap site ────────────────────────────────────────────────

# nico-sim accepts any non-empty bearer token. The keys are CAPNICo's own.
umask 077
cat >"${CREDENTIALS_FILE}" <<EOF
endpoint=http://${GATEWAY}:${SIM_PORT}
orgID=${NICO_ORG_ID}
token=nico-simctl-token
apiName=forge
EOF
umask 022

cat >"${RENDERED_SITESPEC}" <<EOF
clusterName: ${E2E_CLUSTER_NAME}
infrastructure: nico
cluster:
  nico:
    nodeCount: ${NODE_COUNT}
    orgID: ${NICO_ORG_ID}
    siteID: ${NICO_SITE_ID}
    vpcID: ${NICO_VPC_ID}
    instanceTypePreference:
      - ${NICO_INSTANCE_TYPE_ID}
    subnetIDs:
      - ${NICO_SUBNET_ID}
    sshKeyGroupIDs:
      - ${NICO_SSH_KEY_GROUP_ID}
    osProfile: ${OS_PROFILE}
    configProfile: ${CONFIG_PROFILE}
  k8sVersion: "1.34.8"
  cniName: calico
  podCidr: "192.168.0.0/16"
  serviceCidr: "10.96.0.0/12"
  etcdEncryption: false
  kubeletConfig:
    systemReservedMemory: "1Gi"
spire:
  trustDomain: ${SPIRE_TRUST_DOMAIN}
  externalAddress: ${GATEWAY}
registry:
  bootstrapRepo: "nvcr.io/org"
  bootstrapChart: "controller-chart"
EOF

log "validating the site spec: nkectl bootstrap site --dry-run"
"${NKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" --dry-run >/dev/null || fail "the nico site spec did not validate"

# See run.sh for why this runs in the background: the networks have to be
# bridged *during* the run, once the Kind cluster exists.
log "running: nkectl bootstrap site -f ${RENDERED_SITESPEC} --nico-credentials-file <file> --keep-bootstrap-cluster --timeout 25m" \
  "(NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}, NKECTL_CWE_PROFILES_IMAGE=${NKECTL_CWE_PROFILES_IMAGE})"
defer teardown_bootstrap_cluster
setsid "${NKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" \
  --nico-credentials-file "${CREDENTIALS_FILE}" --keep-bootstrap-cluster \
  --timeout 25m --kubeconfig-out "${TENANT_KUBECONFIG}" >"${BOOTSTRAP_LOG}" 2>&1 &
bootstrap_pid=$!

abort_bootstrap() {
  kill -- "-${bootstrap_pid}" 2>/dev/null || kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  cat "${BOOTSTRAP_LOG}" || true
  exit 1
}

# Both are nerdctl bridges, and nerdctl's firewall plugin drops forwards between
# them. CWE -> nke-agent and node -> SPIRE (published on the Kind node's
# NodePort) both cross, so permit exactly this pair, both directions, at
# position 1 so it precedes the isolation chain. See run.sh.
bridge_of() {
  local id
  id="$(nerdctl network inspect --format '{{.Id}}' "$1")" || return 1
  printf 'br-%s' "${id:0:12}"
}
permit_cross_bridge_forwarding() {
  local node_bridge kind_bridge
  node_bridge="$(bridge_of "${NODE_NETWORK}")" || return 1
  kind_bridge="$(bridge_of kind)" || return 1
  ip link show "${node_bridge}" >/dev/null || return 1
  ip link show "${kind_bridge}" >/dev/null || return 1
  defer "remove_forward_rules ${kind_bridge} ${node_bridge}"
  iptables -I FORWARD 1 -i "${kind_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  iptables -I FORWARD 1 -i "${node_bridge}" -o "${kind_bridge}" -j ACCEPT || return 1
}

# CWE reads a login user and password for every node it provisions through the
# NICo path, and retries a node indefinitely without them. The values are
# throwaway, for a cluster that lives minutes, and go in through files so they
# never appear in a process listing. The namespace is created ahead of the CWE
# release, which tolerates it existing.
create_tenant_credentials() {
  local cfg dir
  cfg="$(mktemp)"
  dir="$(mktemp -d)"
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name "${BOOTSTRAP_KIND_CLUSTER}" >"${cfg}" || return 1
  printf 'nke-e2e' >"${dir}/user"
  head -c 18 /dev/urandom | base64 | tr -d '/+=\n' >"${dir}/password"
  kubectl --kubeconfig "${cfg}" create namespace "${CWE_NAMESPACE}" >/dev/null 2>&1 || true
  kubectl --kubeconfig "${cfg}" -n "${CWE_NAMESPACE}" create secret generic "${TENANT_SECRET}" \
    --from-file=user="${dir}/user" --from-file=password="${dir}/password" >/dev/null
  local rc=$?
  rm -rf "${cfg}" "${dir}"
  return "${rc}"
}

log "waiting for the bootstrap cluster to become ready, to bridge it to the node network"
bridged=false
bridge_deadline=$((SECONDS + ${NKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${bridge_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    permit_cross_bridge_forwarding || { log "WARN: could not bridge the Kind and node networks"; break; }
    log "permitted forwarding between the Kind and node bridges"
    create_tenant_credentials || { log "WARN: could not create the tenant credentials Secret"; break; }
    log "created the tenant credentials Secret ${CWE_NAMESPACE}/${TENANT_SECRET}"
    bridged=true
    break
  fi
  sleep 2
done
if [ "${bridged}" != true ]; then
  log "FAIL: the Kind and node networks were never bridged, or the tenant credentials"
  log "  Secret was never created, so CWE cannot provision or reach the nodes. Not"
  log "  waiting for a bootstrap that cannot finish."
  abort_bootstrap
fi

status=0
wait "${bootstrap_pid}" || status=$?
cat "${BOOTSTRAP_LOG}"
output="$(cat "${BOOTSTRAP_LOG}")"

[ "${status}" -eq 0 ] || fail "nkectl bootstrap site exited ${status}"
grep -q "installing CAPNICo" <<<"${output}" || fail "the bootstrap never installed CAPNICo"
grep -q "cluster provisioned" <<<"${output}" || fail "the command exited 0 without reporting the cluster provisioned"
[ -s "${TENANT_KUBECONFIG}" ] || fail "no kubeconfig was written to ${TENANT_KUBECONFIG}"
log "PASS: bootstrap site created the site cluster, allocating its machines through CAPNICo"

# ── Stage 4: what was built ────────────────────────────────────────────────

bootstrap_kubeconfig="$(mktemp)"
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name "${BOOTSTRAP_KIND_CLUSTER}" \
  >"${bootstrap_kubeconfig}" || fail "could not read the bootstrap cluster's kubeconfig"
defer "rm -f ${bootstrap_kubeconfig}"
bk() { kubectl --kubeconfig "${bootstrap_kubeconfig}" "$@"; }

sim_nodes="$(nerdctl ps -q --filter "label=${SIM_LABEL}" | wc -l | tr -d ' ')"
[ "${sim_nodes}" -eq "${NODE_COUNT}" ] ||
  fail "nico-sim is running ${sim_nodes} node container(s), expected ${NODE_COUNT}"
log "PASS: nico-sim created ${sim_nodes} node container(s) for the ${NODE_COUNT} control-plane node(s) asked for"

# nkectl returns once CWE reports every node provisioned, which can be seconds
# before the last one's CNI settles and the node reports Ready, so wait for it.
ready_nodes=0
nodes_ready() {
  ready_nodes="$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes --no-headers 2>/dev/null |
    awk '$2 == "Ready" {n++} END {print n+0}')"
  [ "${ready_nodes}" -eq "${NODE_COUNT}" ]
}
if ! wait_until "${NKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-600}" 5 nodes_ready; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide || true
  fail "${ready_nodes} of ${NODE_COUNT} node(s) became Ready in the cluster nkectl built"
fi
log "PASS: all ${NODE_COUNT} node(s) are Ready in the cluster nkectl built"

# The address CWE wrote into each node's userdata is the one nodes can reach
# before any cluster exists: the host on the node network, on the NodePort the
# bootstrap publishes SPIRE on, not the in-cluster name.
for node in $(nerdctl ps -q --filter "label=${SIM_LABEL}"); do
  agent_conf="$(nerdctl exec "${node}" cat /etc/spire-agent/agent.conf 2>/dev/null)" ||
    fail "could not read /etc/spire-agent/agent.conf on node ${node}"
  grep -q "${GATEWAY}" <<<"${agent_conf}" ||
    fail "node ${node}'s spire-agent is not configured for ${GATEWAY} (spire.externalAddress)" "${agent_conf}"
  grep -q "30081" <<<"${agent_conf}" ||
    fail "node ${node}'s spire-agent is not configured for the published SPIRE NodePort 30081" "${agent_conf}"
done
log "PASS: each node's spire-agent points at spire.externalAddress ${GATEWAY}, on NodePort 30081"

# The allowlist is the point of the dynamic path: nothing in the site spec
# lists a thumbprint, so every hash SPIRE admits came from a NicoMachine.
hashes="$(bk get nicomachines.infrastructure.cluster.x-k8s.io -A \
  -o 'jsonpath={range .items[*]}{.status.tpmEkPubHash}{"\n"}{end}' | sed '/^$/d' | sort -u)"
hash_count="$(printf '%s\n' "${hashes}" | sed '/^$/d' | wc -l | tr -d ' ')"
[ "${hash_count}" -eq "${NODE_COUNT}" ] ||
  fail "${hash_count} NicoMachine(s) report a TPM EK hash, expected ${NODE_COUNT}"
allowlist="$(bk -n spire-server get configmap tpm-pubhashes -o 'jsonpath={.data}' 2>/dev/null)"
while read -r h; do
  [ -z "${h}" ] && continue
  grep -q "${h}" <<<"${allowlist}" || fail "the tpm-pubhashes allowlist is missing ${h}, which a NicoMachine reported"
done <<<"${hashes}"
log "PASS: CWE filled the SPIRE allowlist with the ${hash_count} EK hash(es) the NicoMachine(s) reported"

agents="$(bk -n spire-server exec spire-server-0 -c spire-server -- \
  /opt/spire/bin/spire-server agent list 2>/dev/null)"
while read -r h; do
  [ -z "${h}" ] && continue
  grep -q "spiffe://${SPIRE_TRUST_DOMAIN}/spire/agent/tpm/${h}" <<<"${agents}" ||
    fail "no SPIRE agent attested by TPM with hash ${h}" "${agents}"
done <<<"${hashes}"
log "PASS: every node's spire-agent attested to the bootstrap SPIRE server over TPM"

log "PASS: the nico lane built a site cluster from machines CAPNICo allocated"
