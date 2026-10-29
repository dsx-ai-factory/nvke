#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# `nvkectl bootstrap site` e2e for `infra.provider: nico`: the control-plane
# machines are allocated by CAPNICo from a NICo site instead of taken from a
# hardcoded list. The sibling of run.sh, which covers `infra.provider:
# provisioned`; same host requirements (see run.sh), plus a Go toolchain and a C
# compiler (gcc) to build nico-sim, and network access to ghcr.io for the CAPNICo
# image.
#
# Stages:
#   1. Images: the CAPD node image nico-sim creates its nodes from, and a
#      controller profiles image built from the in-repo fixture tree.
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
# Four inputs the bootstrap reads and this script prepares:
#   - NVKECTL_NICO_CREDENTIALS: the token file for nico-sim's API.
#   - NVKECTL_NODE_USER, NVKECTL_NODE_PASSWORD: the login the controller puts on
#     each node, with throwaway values. `bootstrap site` creates the Secret that
#     carries it.
#   - NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT: nico-sim serves http, which the
#     bootstrap refuses for a NICo it sends the token to unless told.
#   - infra.nico.releases: the MachineImage and ClusterRelease the controller
#     resolves infra.nico.controlPlane.release from, copied from the in-repo
#     fixtures. `bootstrap site` applies them before it creates the cluster.
# One thing it does not supply, which this script hands it through an env hook:
#   - NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE: an OCI image reference whose /profiles
#     tree supplies the config profile a cluster request names. Without it the
#     controller deploys with an empty profiles directory.
#
# Beyond building the cluster the lane checks what the bootstrap promises around
# it: the binary refuses a bad token file, an http endpoint without the opt-in
# and a removed spec field before it touches anything; the site spec hands off to
# the stack render; the etcd key it saves is the nodes' key; no credential reaches
# the logs; a second bootstrap is refused while the first site answers; and the
# documented release-first teardown releases every instance. That teardown
# destroys the site, so it runs last.
#
# Everything created is registered for teardown as it is created, and the EXIT
# trap runs those registrations in reverse, whatever the exit.

set -Eeuo pipefail

# ── Configuration ──────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
REPO_ROOT="$(dirname "${CLI_DIR}")"
NVKECTL="${NVKECTL:-${CLI_DIR}/bin/nvkectl}"
NICO_SIM_DIR="${REPO_ROOT}/controller/nico-sim"
PROFILES_FIXTURE_DIR="${REPO_ROOT}/controller/test/fixtures"
RELEASES_FIXTURE_DIR="${PROFILES_FIXTURE_DIR}/releases"

E2E_CLUSTER_NAME="nvke-e2e-nico"
BOOTSTRAP_KIND_CLUSTER="${NVKECTL_BOOTSTRAP_CLUSTER_NAME:-nvkectl-bootstrap}"
BOOTSTRAP_CONTROL_PLANE="${BOOTSTRAP_KIND_CLUSTER}-control-plane"

# How many control-plane nodes CAPNICo is asked for. Odd, like the hosts list it
# stands in for.
NODE_COUNT="${NVKECTL_E2E_NICO_NODES:-1}"
NODE_NETWORK="${NVKECTL_E2E_NICO_NETWORK:-nvkectl-e2e-nico-nodes}"
NODE_SUBNET="${NVKECTL_E2E_NICO_SUBNET:-172.28.0.0/24}"

# A nico site always has a control-plane VIP, even with one node, so the lane
# always runs one: outside the node subnet, reached through the BGP-learned /32
# on the host. See run.sh and bgp-lib.sh.
CONTROL_PLANE_VIP="${NVKECTL_E2E_CONTROL_PLANE_VIP:-172.26.1.1}"
BGP_CONTAINER="nvkectl-e2e-nico-bgp"
FRR_IMAGE="${NVKECTL_E2E_FRR_IMAGE:-quay.io/frrouting/frr:9.1.0}"
BGP_FILES_DIR="${REPO_ROOT}/controller/hack/bgp-containers"
BGP_RENDER_DIR="${SCRIPT_DIR}/.bgp-nico"

SIM_PORT="${NVKECTL_E2E_NICO_SIM_PORT:-18080}"
SIM_BIN="${CLI_DIR}/.local/nico-sim"
SIM_STATE_DIR="$(mktemp -d)"
SIM_LOG="${SIM_STATE_DIR}/nico-sim.log"
NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE:-cwe-capd-kindest-node:local}"
PROFILES_IMAGE="${NVKECTL_E2E_PROFILES_IMAGE:-cwe-profiles:nvkectl-e2e}"
# nico-sim's managed-container label, which is how teardown finds what it made.
SIM_LABEL="nvidia.com/nico-containers.managed=true"

# What nico-sim seeds for its container site. The IDs are the simulator's own;
# see controller/hack/create-local-nico-cluster.sh, which uses the same ones.
NICO_ORG_ID="${NVKECTL_E2E_NICO_ORG_ID:-novxwn552ssc}"
NICO_SITE_ID="00000000-0000-0000-0000-000000000001"
NICO_VPC_ID="00000000-0000-0000-0000-000000000002"
NICO_SUBNET_ID="00000000-0000-0000-0000-000000000003"
NICO_SSH_KEY_GROUP_ID="00000000-0000-0000-0000-000000000004"
NICO_INSTANCE_TYPE_ID="00000000-0000-0000-0000-000000000005"
# The OS profile is a ClusterRelease from the releases fixture, so it names a
# file pair there; the config profile comes from the profiles image.
OS_PROFILE="${NVKECTL_E2E_NICO_OS_PROFILE:-nvke-local-dev-kindest-arm64}"
CONFIG_PROFILE="${NVKECTL_E2E_NICO_CONFIG_PROFILE:-dev-only-empty}"
# The controller's API port on the bootstrap host, and the isolated nvkectl
# config the teardown check writes its site entry to.
CWE_PORT=30080

SPIRE_TRUST_DOMAIN="${NVKECTL_E2E_SPIRE_TRUST_DOMAIN:-nvkectl-e2e.example.test}"
# The controller's namespace in the bootstrap cluster, and the Secret
# `bootstrap site` creates there from the node login.
CONTROLLER_NAMESPACE=nvke-controller
TENANT_SECRET=dgxc-tenant-credentials

TENANT_KUBECONFIG="${SCRIPT_DIR}/.tenant-nico.kubeconfig"
RENDERED_SITESPEC="${SCRIPT_DIR}/.sitespec.nico.rendered.yaml"
BOOTSTRAP_LOG="${SCRIPT_DIR}/.bootstrap-nico.log"
# What the Go test that runs the command prints about the run itself. nvkectl's
# own output is not in here: it goes to BOOTSTRAP_LOG, so there is one copy of
# it and the credential scan covers that copy.
BOOTSTRAP_GO_TEST_LOG="${SCRIPT_DIR}/.bootstrap-nico-go-test.log"
# The namespaces the NVKE stack installs on the site cluster, which
# TestE2E_BootstrapSite checks once the bootstrap succeeds. It leaves out
# spire-server when NVKECTL_E2E_DISABLE_MTLS is set, since no SPIRE is installed.
SITE_STACK_NAMESPACES="local-path-storage,postgres,temporal,spire-server,capi-system,capi-kubeadm-bootstrap-system"
SITE_STACK_NAMESPACES+=",capi-kubeadm-control-plane-system,capnico-system,${CONTROLLER_NAMESPACE}"
GO="${GO:-go}"
# The lane runs `bootstrap site` through the Go tests in bootstraptest. Those
# tests carry two marks: the e2e_bootstrap build tag, which keeps them out of
# `make test` by leaving them unbuilt, and the TestE2E_ name prefix, which is
# what -run selects once the tag is on.
DIAGNOSTICS_DIR="${CLI_DIR}/cluster-diagnostics"
export NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR="${DIAGNOSTICS_DIR}"
# What this lane adds to the dump nvkectl writes there: the node journals and the
# nico-sim log, which the bootstrap cannot see.
DIAGNOSTICS_FILE="${DIAGNOSTICS_DIR}/nico-lane.txt"

: "${NVKECTL_NVKE_CONTROLLER_IMAGE:?NVKECTL_NVKE_CONTROLLER_IMAGE must name the controller image to deploy, already present in containerd (see run.sh)}"
export NVKECTL_NVKE_CONTROLLER_IMAGE

# `kind create cluster` merges into $KUBECONFIG; use a scratch path so this run
# never takes ownership of the caller's. See run.sh for the sudo -E reasoning.
KUBECONFIG_DIR="$(mktemp -d)"
export KUBECONFIG="${KUBECONFIG_DIR}/config"
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"
CNI_BUNDLE_DIR="$(mktemp -d)"
# What the site spec's infra.nico and infra.nico.releases name.
TOKEN_FILE="${SIM_STATE_DIR}/bootstrap-nico.token"
# The command refuses to overwrite a key file, so each run writes its own, which
# SIM_STATE_DIR's cleanup removes.
ETCD_KEY_FILE="${SIM_STATE_DIR}/etcd-encryption-key.json"
RELEASES_DIR="${SIM_STATE_DIR}/releases"

# ── Helpers ────────────────────────────────────────────────────────────────

# Throwaway credentials, distinct and unguessable, so finding one in a log means
# it leaked and not that it matched a common word. nico-sim accepts any token.
random_hex() { od -An -N8 -tx1 /dev/urandom | tr -d ' \n'; }
NICO_TOKEN_VALUE="e2e-nico-token-$(random_hex)"
NODE_USER_VALUE="nvke-e2e"
NODE_PASSWORD_VALUE="e2e-node-password-$(random_hex)"
DATABASE_PASSWORD_VALUE="e2e-db-password-$(random_hex)"

log() { printf '[nvkectl-bootstrap-nico-e2e] %s\n' "$*"; }

fail() {
  log "FAIL: $1"
  shift
  local line
  for line in "$@"; do log "  ${line}"; done
  exit 1
}

# NETCONFPATH outranks the cni_netconfpath bootstrap-e2e-host.sh writes, and
# this one would put the suite's nerdctl networks where nvkectl's kind commands
# no longer look and its preflight refuses to start.
netconfpath="${NETCONFPATH:-}"
if [ "${netconfpath%/}" = /etc/cni/net.d ]; then
  fail "NETCONFPATH names /etc/cni/net.d, the directory nvkectl keeps nerdctl's networks out of" \
    "unset it, or point it at /etc/containerd-nerdctl/net.d"
fi

# defer registers a command for the EXIT trap, which runs registrations in
# reverse order. Register the teardown *before* the operation it undoes.
DEFERRED=()
# Each argument is quoted as it is registered: cleanup() runs the entry through
# eval, and a path with a space in it would otherwise split into several
# arguments, which for `rm -rf` as root means deleting something else.
defer() {
  local cmd
  printf -v cmd '%q ' "$@"
  DEFERRED+=("${cmd}")
}

# shellcheck source=bgp-lib.sh
source "${SCRIPT_DIR}/bgp-lib.sh"
# shellcheck source=etcd-lib.sh
source "${SCRIPT_DIR}/etcd-lib.sh"

collect_diagnostics() {
  log "collecting diagnostics"
  # DIAGNOSTICS_DIR is ignored by git, handed back to the invoking user on exit
  # and uploaded by CI, which a file beside the script is not.
  mkdir -p "${DIAGNOSTICS_DIR}"
  {
    printf '### nerdctl ps -a\n'; nerdctl ps -a || true
    printf '\n### nico-sim log (tail)\n'; tail -n 80 "${SIM_LOG}" 2>/dev/null || true
    local c
    for c in $(nerdctl ps -aq --filter "label=${SIM_LABEL}" 2>/dev/null); do
      printf '\n### node container %s: journal (tail)\n' "${c}"
      nerdctl exec "${c}" journalctl --no-pager -n 60 2>&1 || true
    done
  } >"${DIAGNOSTICS_FILE}" 2>&1 || true
  log "wrote ${DIAGNOSTICS_FILE}"
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
  rm -rf "${KUBECONFIG_DIR}" "${CNI_BUNDLE_DIR}" "${SIM_STATE_DIR}" "${BGP_RENDER_DIR}"
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
for tool in nerdctl kind kubectl helm clusterctl go gcc make iptables ip ss; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done
[ -x "${NVKECTL}" ] || fail "${NVKECTL} is not built (run make -C cli build)"
if (( NODE_COUNT < 1 || NODE_COUNT % 2 == 0 )); then
  fail "NVKECTL_E2E_NICO_NODES must be odd and at least 1, got ${NODE_COUNT}"
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

log "building the OVN packages for post-init CNI"
BUNDLE_ARCH="$(go env GOARCH)"
export BUNDLE_ARCH
bash "${REPO_ROOT}/controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}"
CAPD_CNI_CHARTS_DEB_PATH="${CNI_BUNDLE_DIR}/nvke-cni-ovn-charts-2.4.2_2.4.2_${BUNDLE_ARCH}.deb"
CAPD_CNI_IMAGES_DEB_PATH="${CNI_BUNDLE_DIR}/nvke-cni-ovn-images-2.4.2_2.4.2_${BUNDLE_ARCH}.deb"
CAPD_HELM_DEB_PATH="${CNI_BUNDLE_DIR}/nke-e2e-helm.deb"
export CAPD_CNI_CHARTS_DEB_PATH CAPD_CNI_IMAGES_DEB_PATH CAPD_HELM_DEB_PATH
if [ ! -f "${CAPD_CNI_CHARTS_DEB_PATH}" ] || [ ! -f "${CAPD_CNI_IMAGES_DEB_PATH}" ] || [ ! -f "${CAPD_HELM_DEB_PATH}" ]; then
  fail "OVN packages were not built"
fi

# nico-sim creates its nodes from this image, which already carries nvke-agent,
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
# nvkectl loads it into the Kind cluster from here, so it must be a tag other
# than latest: the chart sets no pull policy on it.
export NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE="${PROFILES_IMAGE}"
log "PASS: the node and profiles images are in containerd"

# The bootstrap also installs the NVKE stack on the site cluster, whose nodes
# pull its images themselves, so nvkectl requires names with a registry. The
# images are imported into each site node instead (stage 3), so nothing is
# pulled from this registry. This is a workaround for nico-sim's container
# nodes having no registry they can pull from; a local registry they can reach
# would replace both the tag and the import.
SITE_IMAGE_REGISTRY=nvke-e2e.invalid
untag_site_images() { nerdctl rmi "$@" >/dev/null 2>&1 || true; }
site_images=()
for image in "${NVKECTL_NVKE_CONTROLLER_IMAGE}" "${NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE}"; do
  tagged="${SITE_IMAGE_REGISTRY}/${image##*/}"
  nerdctl tag "${image}" "${tagged}" || fail "could not tag ${image} as ${tagged}"
  defer untag_site_images "${tagged}"
  site_images+=("${tagged}")
done
export NVKECTL_NVKE_CONTROLLER_IMAGE="${site_images[0]}" NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE="${site_images[1]}"

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
ANCHOR="nvkectl-e2e-nico-anchor"
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
log "starting nico-sim on ${GATEWAY}:${SIM_PORT}"
defer remove_sim_nodes
defer stop_sim
(
  cd "${NICO_SIM_DIR}"
  mkdir -p "${SIM_STATE_DIR}/files"
  # Only the node network's gateway, which is where CAPNICo and the nodes reach
  # it. nico-sim accepts any bearer token and runs a request's commands as root
  # in privileged containers, so it must not listen on every interface of a host
  # that may share a network with others.
  export NICO_SIM_LISTEN_ADDR="${GATEWAY}:${SIM_PORT}"
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
  (exec 3<>"/dev/tcp/${GATEWAY}/${SIM_PORT}") 2>/dev/null
}
wait_until 120 1 sim_up || fail "nico-sim did not listen on ${GATEWAY}:${SIM_PORT} within 120s" "$(tail -n 20 "${SIM_LOG}")"
# Every listening socket on the port is the gateway's: none on a wildcard or
# loopback address, which anything that can reach the host could dial.
sim_listeners="$(ss -Hltn "sport = :${SIM_PORT}" | awk '{print $4}' | sort -u)"
[ "${sim_listeners}" = "${GATEWAY}:${SIM_PORT}" ] ||
  fail "nico-sim listens on $(echo "${sim_listeners}" | tr '\n' ' '), expected only ${GATEWAY}:${SIM_PORT}"
log "PASS: nico-sim is up on ${GATEWAY}:${SIM_PORT} and listens nowhere else"

# ── Stage 3: bootstrap site ────────────────────────────────────────────────

# nico-sim accepts any non-empty bearer token. The throwaway files are named by
# the site spec, so no credential is on a command line.
umask 077
printf '%s' "${NICO_TOKEN_VALUE}" >"${TOKEN_FILE}"
umask 022
export NVKECTL_NICO_CREDENTIALS="${TOKEN_FILE}"
export NVKECTL_NODE_USER="${NODE_USER_VALUE}"
export NVKECTL_NODE_PASSWORD="${NODE_PASSWORD_VALUE}"
export NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT=1

# Only the release pair the OS profile names, not every fixture release.
mkdir -p "${RELEASES_DIR}"
for kind in machineimage clusterrelease; do
  cp "${RELEASES_FIXTURE_DIR}/${OS_PROFILE}-${kind}.yaml" "${RELEASES_DIR}/" ||
    fail "no ${OS_PROFILE} ${kind} fixture in ${RELEASES_FIXTURE_DIR}"
done

vip_line=""
if [ -n "${CONTROL_PLANE_VIP}" ]; then
  vip_line="  controlPlaneVIP: ${CONTROL_PLANE_VIP}"
fi

cat >"${RENDERED_SITESPEC}" <<EOF
clusterName: ${E2E_CLUSTER_NAME}
infra:
  provider: nico
  nico:
    orgID: ${NICO_ORG_ID}
    remote:
      endpoint: http://${GATEWAY}:${SIM_PORT}
      siteID: ${NICO_SITE_ID}
      vpcID: ${NICO_VPC_ID}
    controlPlane:
      count: ${NODE_COUNT}
      instanceTypePreference:
        - ${NICO_INSTANCE_TYPE_ID}
      subnetIDs:
        - ${NICO_SUBNET_ID}
      sshKeyGroupIDs:
        - ${NICO_SSH_KEY_GROUP_ID}
      release: ${OS_PROFILE}
      configProfile: ${CONFIG_PROFILE}
    releases: ${RELEASES_DIR}
cluster:
${vip_line}
  k8sVersion: "1.34.8"
  cniName: ovn-kubernetes
  cniVersion: "2.4.2"
  podCidr: "192.168.0.0/16"
  serviceCidr: "10.96.0.0/12"
  kubeletConfig:
    systemReservedMemory: "1Gi"
spire:
  trustDomain: ${SPIRE_TRUST_DOMAIN}
  externalAddress: ${GATEWAY}
EOF

log "validating the site spec: nvkectl bootstrap site --dry-run"
"${NVKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" --dry-run >/dev/null ||
  fail "the nico site spec did not validate"

# ── Stage 3a: what the binary refuses before it touches anything ───────────
#
# Each case breaks one input of the spec that just passed and runs the same
# dry run (or `validate sitespec --bootstrap-only`), which stands nothing up. A
# dry run that stopped refusing would otherwise surface as a 25 minute bootstrap
# that fails late, or worse, one that sends a token over http.

# expect_refusal WHAT NEEDLE CMD...: CMD exits non-zero and its output holds NEEDLE.
expect_refusal() {
  local what="$1" needle="$2" out rc=0
  shift 2
  out="$("$@" 2>&1)" || rc=$?
  [ "${rc}" -ne 0 ] || fail "${what}: the command accepted it, expected a refusal" "${out}"
  grep -qF -- "${needle}" <<<"${out}" || fail "${what}: the refusal does not say ${needle}" "${out}"
}
dry_run() { "${NVKECTL}" bootstrap site --dry-run -f "$@"; }

http_endpoint_refusal="NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT"
expect_refusal "an http NICo endpoint without the opt-in" "${http_endpoint_refusal}" \
  env -u NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"
expect_refusal "an http NICo endpoint without the opt-in, validate sitespec" "${http_endpoint_refusal}" \
  env -u NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT "${NVKECTL}" validate sitespec --bootstrap-only -f "${RENDERED_SITESPEC}"

expect_refusal "no NICo token file named" "NVKECTL_NICO_CREDENTIALS must name" \
  env NVKECTL_NICO_CREDENTIALS= "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"
expect_refusal "a NICo token file that does not exist" "NVKECTL_NICO_CREDENTIALS:" \
  env NVKECTL_NICO_CREDENTIALS="${SIM_STATE_DIR}/no-such.token" "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"
: >"${SIM_STATE_DIR}/empty.token"
expect_refusal "an empty NICo token file" "is empty" \
  env NVKECTL_NICO_CREDENTIALS="${SIM_STATE_DIR}/empty.token" "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"
printf 'clientID: example\nclientSecret: example\n' >"${SIM_STATE_DIR}/client-credentials.token"
expect_refusal "a client-credentials NICo token file" "client-credentials form is not supported" \
  env NVKECTL_NICO_CREDENTIALS="${SIM_STATE_DIR}/client-credentials.token" "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"

expect_refusal "no node login password" "NVKECTL_NODE_PASSWORD is not set" \
  env -u NVKECTL_NODE_PASSWORD "${NVKECTL}" bootstrap site --dry-run -f "${RENDERED_SITESPEC}"

# A node login in the site spec is a credential in a file meant to be committed.
sed 's#^    releases: #    tenant:\n      user: example\n    releases: #' "${RENDERED_SITESPEC}" >"${SIM_STATE_DIR}/spec-tenant.yaml"
grep -q '^    tenant:' "${SIM_STATE_DIR}/spec-tenant.yaml" || fail "the test spec did not gain an infra.nico.tenant block"
expect_refusal "infra.nico.tenant in the site spec" "infra.nico.tenant is no longer accepted" dry_run "${SIM_STATE_DIR}/spec-tenant.yaml"

sed "s#^    releases: .*#    releases: ${SIM_STATE_DIR}/no-such-releases#" "${RENDERED_SITESPEC}" >"${SIM_STATE_DIR}/spec-releases.yaml"
expect_refusal "an infra.nico.releases path that does not exist" "infra.nico.releases" dry_run "${SIM_STATE_DIR}/spec-releases.yaml"

# spire.externalAddress is what nodes dial to reach SPIRE, so it is required
# unless --disable-mtls, which deploys no SPIRE and reads nothing from it.
grep -v '^  externalAddress:' "${RENDERED_SITESPEC}" >"${SIM_STATE_DIR}/spec-no-address.yaml"
expect_refusal "no spire.externalAddress with mTLS" "spire.externalAddress" dry_run "${SIM_STATE_DIR}/spec-no-address.yaml"
"${NVKECTL}" bootstrap site --dry-run --disable-mtls -f "${SIM_STATE_DIR}/spec-no-address.yaml" >/dev/null ||
  fail "bootstrap site --dry-run --disable-mtls refused a nico spec without spire.externalAddress"
log "PASS: the binary refuses a bad token file, an http endpoint without the opt-in, a spec-borne node login and bad paths"

# ── Stage 3b: the site spec hands off to the stack install ─────────────────
#
# The controller stack install reads the same site spec the bootstrap did, plus
# a stack config and the credentials from the environment. Render it, without
# installing, and check what the render carried across: the site's name, VIP
# and trust domain, and the credentials the install creates its Secrets from.
STACK_CONFIG_FILE="${SIM_STATE_DIR}/stack-config.json"
cat >"${STACK_CONFIG_FILE}" <<EOF
{
  "nico": {"endpoint": "http://${GATEWAY}:${SIM_PORT}", "orgID": "${NICO_ORG_ID}", "apiName": "forge"},
  "images": {"controller": "registry.example.com/nvke/controller:e2e", "profiles": "registry.example.com/nvke/profiles:e2e"},
  "mirror": ""
}
EOF
stack_env=(NVKECTL_NICO_TOKEN="${NICO_TOKEN_VALUE}" NVKECTL_NODE_USER="${NODE_USER_VALUE}"
  NVKECTL_NODE_PASSWORD="${NODE_PASSWORD_VALUE}" NVKECTL_DATABASE_PASSWORD="${DATABASE_PASSWORD_VALUE}")
stack_org="$(env "${stack_env[@]}" "${NVKECTL}" inspect stack-config "${STACK_CONFIG_FILE}" | sed -n "s/^STACK_NICO_ORG_ID='\(.*\)'$/\1/p")"
[ "${stack_org}" = "${NICO_ORG_ID}" ] || fail "inspect stack-config read org ${stack_org:-<none>}, expected ${NICO_ORG_ID}"
expect_refusal "a stack config without the credentials in the environment" "NVKECTL_DATABASE_PASSWORD" \
  env -u NVKECTL_DATABASE_PASSWORD "${NVKECTL}" inspect stack-config "${STACK_CONFIG_FILE}"

STACK_RENDER_DIR="${CLI_DIR}/.local/nico-stack-render"
rm -rf "${STACK_RENDER_DIR}"
defer rm -rf "${STACK_RENDER_DIR}"
env "${stack_env[@]}" "${NVKECTL}" render deploy-path --deploy-path "${REPO_ROOT}/deploy/nico" \
  --sitespec "${RENDERED_SITESPEC}" --stack-config "${STACK_CONFIG_FILE}" --arch "linux/$(go env GOARCH)" \
  --out "${STACK_RENDER_DIR}" >/dev/null 2>&1 || fail "render deploy-path did not accept the nico site spec"
for want in "SITE_NAME=${E2E_CLUSTER_NAME}" "SITE_VIP=${CONTROL_PLANE_VIP}" "SITE_TRUST_DOMAIN=${SPIRE_TRUST_DOMAIN}"; do
  grep -qxF -- "${want}" "${STACK_RENDER_DIR}/site.env" ||
    fail "the stack render did not carry ${want} across from the site spec" "$(cat "${STACK_RENDER_DIR}/site.env")"
done
grep -rqF -- "${NODE_PASSWORD_VALUE}" "${STACK_RENDER_DIR}/nvke-base-config" ||
  fail "the stack render did not put the node login password into its tenant credentials"
grep -rqF -- "${NICO_TOKEN_VALUE}" "${STACK_RENDER_DIR}/nvke-base-config" ||
  fail "the stack render did not put the NICo token into its NICo credentials"
log "PASS: the nico site spec renders for the stack install, carrying its site name, VIP, trust domain and credentials"

# See run.sh for why this runs in the background: the networks have to be
# bridged *during* the run, once the Kind cluster exists.
# Before bootstrap, so the nodes can reach the VIP as they join. The nodes' default
# gateway is this host on the node network, which is where kube-vip peers.
if [ -n "${CONTROL_PLANE_VIP}" ]; then
  command -v jq >/dev/null 2>&1 || fail "jq is required for the control-plane VIP checks"
  log "starting the FRR BGP speaker for control-plane VIP ${CONTROL_PLANE_VIP}"
  start_bgp_speaker || exit 1
fi

log "running: nvkectl bootstrap site -f ${RENDERED_SITESPEC} --keep-bootstrap-cluster --timeout 25m" \
  "via the e2e_bootstrap TestE2E_ tests" \
  "(NVKECTL_NVKE_CONTROLLER_IMAGE=${NVKECTL_NVKE_CONTROLLER_IMAGE}, NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE=${NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE})"
defer teardown_bootstrap_cluster
# The command is run by TestE2E_BootstrapSite rather than invoked here, so that
# starting it, giving it a process group and reaping that group are stated once
# in Go instead of twice in shell. It writes nvkectl's output to BOOTSTRAP_LOG,
# which is what the loop below and every assertion after it read, so this lane
# is unchanged on either side of this line. The test asserts nothing: it reports
# what the command exited with and no more. See run.sh.
#
# setsid puts the test in a session of its own, so nothing else ends it when
# this script exits early (a cancelled job, a signal during the bridging loop).
# It is stopped before the cluster is torn down, after the diagnostics are
# collected: deferred entries run in reverse, so this one is registered after
# the teardown. Signalling it stops nvkectl and the kind, nerdctl and helm it
# drives, which do not share the test's group.
bootstrap_pid=""
stop_bootstrap() {
  [ -n "${bootstrap_pid}" ] || return 0
  kill -- "-${bootstrap_pid}" 2>/dev/null || kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  bootstrap_pid=""
}
defer stop_bootstrap
# A -run that matches nothing exits 0, so a renamed or mistyped test would look
# like a bootstrap that ran and passed. Assert the selector picks something up
# before relying on it. This compiles the package too, so the run below starts
# without a build in front of it.
#
# Captured rather than piped into grep: under `set -o pipefail`, a `grep -q`
# that exits on the first match leaves `go test` writing to a closed pipe, and
# the pipeline then fails for a reason that has nothing to do with the result.
listed_tests="$("${GO}" test -C "${CLI_DIR}" -tags e2e_bootstrap -list '^TestE2E_' \
  ./e2e/bootstrap/bootstraptest/)" ||
  fail "could not list the bootstrap tests"
case "${listed_tests}" in
  *TestE2E_*) ;;
  *) fail "no TestE2E_ test in the bootstrap test package: the lane would report success without bootstrapping anything" ;;
esac

setsid env \
  NVKECTL="${NVKECTL}" \
  NVKECTL_E2E_SITESPEC="${RENDERED_SITESPEC}" \
  NVKECTL_E2E_BOOTSTRAP_LOG="${BOOTSTRAP_LOG}" \
  NVKECTL_E2E_KUBECONFIG_OUT="${TENANT_KUBECONFIG}" \
  NVKECTL_E2E_ENCRYPTION_KEY_OUT="${ETCD_KEY_FILE}" \
  NVKECTL_E2E_BOOTSTRAP_TIMEOUT=25m \
  NVKECTL_E2E_KEEP_BOOTSTRAP_CLUSTER=1 \
  NVKECTL_E2E_SITE_STACK_NAMESPACES="${SITE_STACK_NAMESPACES}" \
  "${GO}" test -C "${CLI_DIR}" -tags e2e_bootstrap -timeout 0 -count=1 -v \
  ./e2e/bootstrap/bootstraptest/ -run '^TestE2E_' \
  >"${BOOTSTRAP_GO_TEST_LOG}" 2>&1 &
bootstrap_pid=$!

abort_bootstrap() {
  stop_bootstrap
  cat "${BOOTSTRAP_LOG}" || true
  cat "${BOOTSTRAP_GO_TEST_LOG}" || true
  exit 1
}

# Both are nerdctl bridges, and nerdctl's firewall plugin drops forwards between
# them. The controller -> nvke-agent and node -> SPIRE (published on the Kind node's
# NodePort) both cross, so permit exactly this pair, both directions, at
# position 1 so it precedes the isolation chain. See run.sh.
bridge_of() {
  local id
  id="$(nerdctl network inspect --format '{{.Id}}' "$1")" || return 1
  printf 'br-%s' "${id:0:12}"
}
remove_vip_forward_rule() {
  iptables -D FORWARD -i "$1" -o "$1" -j ACCEPT 2>/dev/null || true
}
permit_cross_bridge_forwarding() {
  local node_bridge kind_bridge
  node_bridge="$(bridge_of "${NODE_NETWORK}")" || return 1
  kind_bridge="$(bridge_of kind)" || return 1
  ip link show "${node_bridge}" >/dev/null || return 1
  ip link show "${kind_bridge}" >/dev/null || return 1
  defer remove_forward_rules "${kind_bridge}" "${node_bridge}"
  iptables -I FORWARD 1 -i "${kind_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  iptables -I FORWARD 1 -i "${node_bridge}" -o "${kind_bridge}" -j ACCEPT || return 1
  # VIP traffic enters and leaves the same node bridge.
  if [ -n "${CONTROL_PLANE_VIP}" ]; then
    defer remove_vip_forward_rule "${node_bridge}"
    iptables -I FORWARD 1 -i "${node_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  fi
}

log "waiting for the bootstrap cluster to become ready, to bridge it to the node network"
bridged=false
bridge_deadline=$((SECONDS + ${NVKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${bridge_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    permit_cross_bridge_forwarding || { log "WARN: could not bridge the Kind and node networks"; break; }
    log "permitted forwarding between the Kind and node bridges"
    bridged=true
    break
  fi
  sleep 2
done
if [ "${bridged}" != true ]; then
  log "FAIL: the Kind and node networks were never bridged, so the controller cannot"
  log "  reach the nodes. Not waiting for a bootstrap that cannot finish."
  abort_bootstrap
fi

# Each site node gets the images as soon as its containerd answers. A pod that
# asked first backs off and finds the image on its next try. The archive is
# copied in rather than piped: nerdctl exec -i truncates a large stdin.
site_images_archive="${SIM_STATE_DIR}/site-images.tar"
nerdctl save --output "${site_images_archive}" "${site_images[@]}" || fail "could not save ${site_images[*]}"
declare -A imported=()
import_site_images() {
  local node
  for node in $(nerdctl ps -q --filter "label=${SIM_LABEL}"); do
    [ -z "${imported[${node}]:-}" ] || continue
    nerdctl exec "${node}" ctr -n k8s.io version >/dev/null 2>&1 || continue
    nerdctl cp "${site_images_archive}" "${node}:/site-images.tar" &&
      nerdctl exec "${node}" ctr -n k8s.io images import /site-images.tar >/dev/null || continue
    imported[${node}]=1
    log "imported the controller and profiles images into site node ${node}"
  done
}
while kill -0 "${bootstrap_pid}" 2>/dev/null; do
  import_site_images
  sleep 5
done

status=0
wait "${bootstrap_pid}" || status=$?
bootstrap_pid=""
cat "${BOOTSTRAP_LOG}"
output="$(cat "${BOOTSTRAP_LOG}")"

if [ "${status}" -ne 0 ]; then
  # The go test exit status says only that the run failed. What nvkectl exited
  # with, or that it could not be started at all, is in its log.
  cat "${BOOTSTRAP_GO_TEST_LOG}" || true
  fail "nvkectl bootstrap site failed; see ${BOOTSTRAP_GO_TEST_LOG}"
fi
grep -q "installing CAPNICo" <<<"${output}" || fail "the bootstrap never installed CAPNICo"
grep -q "cluster provisioned" <<<"${output}" || fail "the command exited 0 without reporting the cluster provisioned"
[ -s "${TENANT_KUBECONFIG}" ] || fail "no kubeconfig was written to ${TENANT_KUBECONFIG}"
log "PASS: bootstrap site created the site cluster, allocating its machines through CAPNICo"

# TestE2E_BootstrapSite checked the NVKE stack on the site (bootstraptest.VerifySiteStack);
# make sure it did, rather than skipped it for want of the namespaces.
grep -q "bootstraptest: the NVKE stack on the site cluster is rolled out and Ready" "${BOOTSTRAP_GO_TEST_LOG}" ||
  fail "TestE2E_BootstrapSite did not check the NVKE stack on the site cluster"
log "PASS: every NVKE stack workload on the site cluster is rolled out and Ready: ${SITE_STACK_NAMESPACES}"

# ── Stage 4: what was built ────────────────────────────────────────────────

bootstrap_kubeconfig="$(mktemp)"
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name "${BOOTSTRAP_KIND_CLUSTER}" \
  >"${bootstrap_kubeconfig}" || fail "could not read the bootstrap cluster's kubeconfig"
defer rm -f "${bootstrap_kubeconfig}"
bk() { kubectl --kubeconfig "${bootstrap_kubeconfig}" "$@"; }

# What the node login and infra.nico.releases asked for, which the
# controller needed to create the first node at all.
bk get clusterrelease "${OS_PROFILE}" >/dev/null 2>&1 ||
  fail "the ClusterRelease ${OS_PROFILE} infra.nico.releases names was not applied to the bootstrap cluster"
bk -n "${CONTROLLER_NAMESPACE}" get secret "${TENANT_SECRET}" >/dev/null 2>&1 ||
  fail "the tenant credentials Secret ${CONTROLLER_NAMESPACE}/${TENANT_SECRET} the node login asks for was not created"
log "PASS: bootstrap site applied ${OS_PROFILE} and created the tenant credentials Secret"

sim_nodes="$(nerdctl ps -q --filter "label=${SIM_LABEL}" | wc -l | tr -d ' ')"
[ "${sim_nodes}" -eq "${NODE_COUNT}" ] ||
  fail "nico-sim is running ${sim_nodes} node container(s), expected ${NODE_COUNT}"
log "PASS: nico-sim created ${sim_nodes} node container(s) for the ${NODE_COUNT} control-plane node(s) asked for"

# nvkectl returns once the controller reports every node provisioned, which can be seconds
# before the last one's CNI settles and the node reports Ready, so wait for it.
ready_nodes=0
nodes_ready() {
  ready_nodes="$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes --no-headers 2>/dev/null |
    awk '$2 == "Ready" {n++} END {print n+0}')"
  [ "${ready_nodes}" -eq "${NODE_COUNT}" ]
}
if ! wait_until "${NVKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-600}" 5 nodes_ready; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide || true
  fail "${ready_nodes} of ${NODE_COUNT} node(s) became Ready in the cluster nvkectl built"
fi
log "PASS: all ${NODE_COUNT} node(s) are Ready in the cluster nvkectl built"

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

if [ -n "${CONTROL_PLANE_VIP}" ]; then
  cp_node_ips="$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
    -o 'jsonpath={range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | sed '/^$/d' | sort -u)"
  server="$(grep -E '^[[:space:]]*server:' "${TENANT_KUBECONFIG}" | head -n1 | awk '{print $2}')"
  [ "${server}" = "https://${CONTROL_PLANE_VIP}:6443" ] ||
    fail "the written kubeconfig's API server is ${server}, expected https://${CONTROL_PLANE_VIP}:6443"
  log "PASS: the written kubeconfig's API server is ${server}"

  log "waiting for FRR to learn ${CONTROL_PLANE_VIP}/32 with next-hops: $(echo "${cp_node_ips}" | tr '\n' ' ')"
  learned_hops=""
  hops_deadline=$((SECONDS + ${NVKECTL_E2E_VIP_BGP_TIMEOUT_SECONDS:-180}))
  while [ "${SECONDS}" -lt "${hops_deadline}" ]; do
    learned_hops="$(bgp_vip_nexthops)"
    [ "${learned_hops}" = "${cp_node_ips}" ] && break
    sleep 3
  done
  if [ "${learned_hops}" != "${cp_node_ips}" ]; then
    nerdctl exec "${BGP_CONTAINER}" vtysh -c 'show bgp summary' 2>&1 | sed 's/^/  /' || true
    fail "FRR next-hops for ${CONTROL_PLANE_VIP}/32 did not converge to the node set" \
      "expected: $(echo "${cp_node_ips}" | tr '\n' ' ')" "learned:  $(echo "${learned_hops}" | tr '\n' ' ')"
  fi
  log "PASS: FRR learned ${CONTROL_PLANE_VIP}/32 with one next-hop per control-plane node"

  vip_readyz() {
    kubectl --kubeconfig "${TENANT_KUBECONFIG}" --server "https://${CONTROL_PLANE_VIP}:6443" get --raw /readyz >/dev/null 2>&1
  }
  wait_until "${NVKECTL_E2E_VIP_READYZ_TIMEOUT_SECONDS:-60}" 3 vip_readyz ||
    fail "the API server did not answer /readyz through the VIP https://${CONTROL_PLANE_VIP}:6443"
  log "PASS: the API server answered /readyz through the VIP https://${CONTROL_PLANE_VIP}:6443"
fi

log "PASS: the nico lane built a site cluster from machines CAPNICo allocated"

# ── Stage 5: etcd data is encrypted, and the saved key is the nodes' key ───

mapfile -t cp_containers < <(nerdctl ps -q --filter "label=${SIM_LABEL}")
check_etcd_encrypted_at_rest "${TENANT_KUBECONFIG}" "${ETCD_KEY_FILE}"
check_saved_key_matches_nodes "${ETCD_KEY_FILE}" "${cp_containers[@]}"

# ── Stage 6: the credentials went where they belong, and nowhere else ──────
#
# The Secrets hold what the environment held; and neither value appears in what
# nvkectl printed or in what the controller and CAPNICo logged, which is where a
# credential that took the wrong path would show.

secret_value() { bk -n "$1" get secret "$2" -o "jsonpath={.data.$3}" | base64 -d; }
[ "$(secret_value capnico-system nico-credentials token)" = "${NICO_TOKEN_VALUE}" ] ||
  fail "the capnico-system/nico-credentials Secret does not hold the token the environment named"
[ "$(secret_value "${CONTROLLER_NAMESPACE}" "${TENANT_SECRET}" user)" = "${NODE_USER_VALUE}" ] ||
  fail "the ${TENANT_SECRET} Secret does not hold the node user the environment named"
[ "$(secret_value "${CONTROLLER_NAMESPACE}" "${TENANT_SECRET}" password)" = "${NODE_PASSWORD_VALUE}" ] ||
  fail "the ${TENANT_SECRET} Secret does not hold the node password the environment named"
log "PASS: the NICo token and node login Secrets hold the values the environment named"

pod_logs="${SIM_STATE_DIR}/pod-logs.txt"
: >"${pod_logs}"
for ns in "${CONTROLLER_NAMESPACE}" capnico-system; do
  for pod in $(bk -n "${ns}" get pods -o name); do
    bk -n "${ns}" logs "${pod}" --all-containers --tail=-1 >>"${pod_logs}" 2>&1 || true
  done
done
[ -s "${pod_logs}" ] || fail "collected no controller or CAPNICo logs to check for credentials"
for leak_file in "${BOOTSTRAP_LOG}" "${pod_logs}"; do
  for secret in "${NICO_TOKEN_VALUE}" "${NODE_PASSWORD_VALUE}"; do
    if grep -qF -- "${secret}" "${leak_file}"; then
      fail "a credential reached $(basename "${leak_file}")"
    fi
  done
done
log "PASS: neither the NICo token nor the node password is in what nvkectl, the controller or CAPNICo logged"

# ── Stage 7: a second bootstrap is refused while the first site answers ────
#
# A re-run allocates another set of NICo machines and a second control plane
# advertising the same VIP, so the command refuses twice over: while the first
# bootstrap cluster is still there, and, under another name that would not
# collide, because the VIP already answers. Neither may stand anything up.

sim_node_count() { nerdctl ps -aq --filter "label=${SIM_LABEL}" | wc -l | tr -d ' '; }
nodes_before="$(sim_node_count)"
rerun_args=(bootstrap site -f "${RENDERED_SITESPEC}" --timeout 2m
  --kubeconfig-out "${SIM_STATE_DIR}/rerun.kubeconfig" --encryption-key-out "${SIM_STATE_DIR}/rerun-key.json")
expect_refusal "a re-run with the first bootstrap cluster still there" "do not re-run" "${NVKECTL}" "${rerun_args[@]}"
# A regression that stops refusing would create this cluster, and the assertion
# below would then fail the run before anything else removed it.
RERUN_KIND_CLUSTER=nvkectl-e2e-rerun
teardown_rerun_cluster() {
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name "${RERUN_KIND_CLUSTER}" >/dev/null 2>&1 || true
}
defer teardown_rerun_cluster
expect_refusal "a re-run over a site whose VIP answers" "another set of NICo machines" \
  env NVKECTL_BOOTSTRAP_CLUSTER_NAME="${RERUN_KIND_CLUSTER}" "${NVKECTL}" "${rerun_args[@]}"
if KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters 2>/dev/null | grep -qx "${RERUN_KIND_CLUSTER}"; then
  fail "the refused re-run created a bootstrap cluster"
fi
[ ! -e "${SIM_STATE_DIR}/rerun-key.json" ] && [ ! -e "${SIM_STATE_DIR}/rerun.kubeconfig" ] ||
  fail "the refused re-run wrote its outputs"
[ "$(sim_node_count)" = "${nodes_before}" ] ||
  fail "the refused re-run changed the number of NICo machines: ${nodes_before} before, $(sim_node_count) after"
log "PASS: a second bootstrap is refused, and allocates no machines"

# ── Stage 8: the documented release-first teardown releases every instance ─
#
# Deleting the bootstrap Kind cluster runs no finalizers and would leave the
# instances allocated. The recovery docs say to point nvkectl at the bootstrap
# cluster's controller and delete the site cluster through it first. Follow
# them, in an nvkectl config of this run's own, then check the machines are
# released and the site is gone. This destroys the site, so it is last.

export XDG_CONFIG_HOME="${SIM_STATE_DIR}/xdg"
"${NVKECTL}" config set-site --name "${BOOTSTRAP_KIND_CLUSTER}" --url "http://127.0.0.1:${CWE_PORT}" \
  --kubeconfig "${bootstrap_kubeconfig}" >/dev/null || fail "nvkectl config set-site did not accept the bootstrap controller"
site_listed() { "${NVKECTL}" --site "${BOOTSTRAP_KIND_CLUSTER}" get clusters 2>/dev/null | grep -qw -- "${E2E_CLUSTER_NAME}"; }
site_listed || fail "the bootstrap controller does not list ${E2E_CLUSTER_NAME} before it is deleted"
"${NVKECTL}" --site "${BOOTSTRAP_KIND_CLUSTER}" delete cluster "${E2E_CLUSTER_NAME}" >/dev/null ||
  fail "nvkectl delete cluster ${E2E_CLUSTER_NAME} failed against the bootstrap controller"
site_unlisted() { ! site_listed; }
wait_until "${NVKECTL_E2E_RELEASE_TIMEOUT_SECONDS:-900}" 5 site_unlisted ||
  fail "the bootstrap controller still lists ${E2E_CLUSTER_NAME} after delete cluster"
nodes_released() { [ "$(sim_node_count)" = 0 ]; }
wait_until "${NVKECTL_E2E_RELEASE_TIMEOUT_SECONDS:-900}" 5 nodes_released ||
  fail "delete cluster left $(sim_node_count) NICo machine(s) allocated"
log "PASS: the release-first teardown deleted the site and released every NICo machine"
site_unreachable() { ! vip_readyz; }
if [ -n "${CONTROL_PLANE_VIP}" ]; then
  wait_until 120 3 site_unreachable || fail "the API server still answers through the VIP after the site was deleted"
  log "PASS: the VIP no longer answers"
fi
