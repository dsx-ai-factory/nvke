#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# `nkectl bootstrap site` e2e entrypoint, invoked by `make test-e2e-bootstrap`
# and the "nkectl bootstrap e2e" CI workflow.
#
# Stages:
#   1. Fake node containers come up and nke-agent answers on each.
#   2. `bootstrap site`, run in the background. The moment its ephemeral Kind
#      cluster is ready this suite bridges the two container networks and
#      proves packets cross both ways. The command then deploys Postgres,
#      Temporal, SPIRE and CWE, builds a cluster over the pre-provisioned
#      hosts, writes its kubeconfig and exits 0.
#   3. Every fake node's spire-agent attested to that SPIRE server over TPM.
#      The EK thumbprints were an input to the command, so a bootstrap that
#      deployed SPIRE without the TPM attestor, or dropped the allowlist,
#      leaves every agent locked out.
#   4. Every site spec node reports Ready through the kubeconfig the command
#      wrote -- the cluster an operator gets, not the bootstrap cluster.
#   5. Traefik serves a whoami Deployment on that cluster: hosts that already
#      existed now serve traffic like any other cluster.
#
# Everything created is registered for teardown as it is created, and the EXIT
# trap runs those registrations in reverse, whatever the exit. A failing exit
# first collects diagnostics into NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR.
#
# The suite runs entirely on nerdctl/containerd, which `make -C cli
# check-no-docker` keeps true. It needs on the host: nerdctl and a containerd
# for kind to drive (nkectl sets KIND_EXPERIMENTAL_PROVIDER=nerdctl
# unconditionally; kind states 1.7.0 as its minimum), buildkitd for `nerdctl
# build` to reach, and root or sudo to install two iptables FORWARD rules and
# the SPIRE VIP. `cli/hack/bootstrap-e2e-host.sh install-runtime` sets up the
# first two the way CI does.
#
# NKECTL_CWE_IMAGE must already be in containerd's image store: nkectl saves it
# from there and loads it into the kind cluster rather than pulling from inside
# the cluster, so no registry credentials are needed past that point.

set -Eeuo pipefail

# ── Configuration ──────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
NKECTL="${NKECTL:-${CLI_DIR}/bin/nkectl}"
# The site spec is generated per run by `nkectl generate sitespec`, so this lane
# tests the command an operator uses rather than a fixture kept in step with it
# by hand. Only the cluster name is fixed here.
E2E_CLUSTER_NAME="nke-e2e"
BOOTSTRAP_KIND_CLUSTER="${NKECTL_BOOTSTRAP_CLUSTER_NAME:-nkectl-bootstrap}"
BOOTSTRAP_CONTROL_PLANE="${BOOTSTRAP_KIND_CLUSTER}-control-plane"
# The fake-node fixture, as the substrate package names it: N containers on one
# private network, named by prefix and index. The defaults here mirror
# substrate.DefaultConfig; override the same variables to change both.
NODE_COUNT="${NKECTL_E2E_NODES:-3}"
NODE_PREFIX="${NKECTL_E2E_NAME_PREFIX:-node}"
NODE_NETWORK="${NKECTL_E2E_NETWORK:-nkectl-e2e-nodes}"
NODE1="nkectl-e2e-${NODE_PREFIX}1"
# The port nke-agent listens on, as the site spec's nodeAgentPort defaults it.
NODE_AGENT_PORT=15005
# SiteSpec generation records the exact journal boundary and expected result
# for every node here. The later check reads the same file after bootstrap.
export NKECTL_E2E_ATTESTATION_BASELINE="${CLI_DIR}/.local/attestation-baseline.json"
# Where the node images' spire-agents were built to look for the SPIRE server.
SPIRE_VIP="${NKECTL_E2E_SPIRE_SERVER_ADDRESS:-172.26.0.2}"
# The init node, which is also where the finished cluster's API server ends up:
# no controlPlaneVIP is set, so kubeadm uses the first control-plane node's IP.
# It matches the first row of the generated node CSV, and it is the address
# stages 4 and 5 use.
INIT_NODE_IP="${NKECTL_E2E_INIT_NODE_IP:-172.26.0.10}"
# Where Traefik is published on the new cluster, and where stage 5 asks for the
# page. In the NodePort range, and clear of the SPIRE server's 30081.
TRAEFIK_NODE_PORT="${NKECTL_E2E_TRAEFIK_NODE_PORT:-30082}"
# Where `bootstrap site` writes the finished cluster's kubeconfig.
TENANT_KUBECONFIG="${SCRIPT_DIR}/.tenant.kubeconfig"
BOOTSTRAP_LOG="${SCRIPT_DIR}/.bootstrap.log"
: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already present in containerd (see comment above)}"
export NKECTL_CWE_IMAGE

# The Cluster API infrastructure provider for hosts that already exist, built
# below from this checkout. Without it the CAPI objects CWE writes have no
# controller and the cluster never leaves provisioning. nkectl finds the
# kustomize base by walking up to go.work, so only the image is named here.
export NKECTL_CAPI_PROVISIONED_IMAGE="${NKECTL_CAPI_PROVISIONED_IMAGE:-cluster-api-provider-provisioned:local}"

# Where a failed bootstrap writes its cluster dump. Set explicitly because
# nkectl's default is relative to the working directory, which would put the
# bundle where the CI artifact path does not match.
DIAGNOSTICS_DIR="${CLI_DIR}/cluster-diagnostics"
export NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR="${DIAGNOSTICS_DIR}"

# `kind create cluster` merges into $KUBECONFIG, defaulting to ~/.kube/config:
# use a scratch path so this run never takes ownership of the caller's. Under
# CI's `sudo -E` that file would otherwise end up root-owned, and a later step
# running as the normal user could not update it.
KUBECONFIG_DIR="$(mktemp -d)"
export KUBECONFIG="${KUBECONFIG_DIR}/config"
CNI_BUNDLE_DIR="$(mktemp -d)"

# KUBECONFIG alone is not enough: helm caches API discovery under ~/.kube/cache
# and creates ~/.kube mode 0750 to do it, root-owned under `sudo -E`.
# KUBECACHEDIR moves that cache into the same scratch directory.
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"

# ── Helpers ────────────────────────────────────────────────────────────────

log() { printf '[nkectl-bootstrap-e2e] %s\n' "$*"; }

# fail logs one or more lines as a failure and exits 1. The EXIT trap then
# collects diagnostics and tears down whatever was created.
fail() {
  log "FAIL: $1"
  shift
  local line
  for line in "$@"; do log "  ${line}"; done
  exit 1
}

# defer registers a command for the EXIT trap, which runs registrations in
# reverse order. Register the teardown *before* the operation it undoes: under
# `set -e` an operation can fail partway and exit on the spot, so the teardown
# has to already be there. Each one is best-effort, so a partial state never
# blocks the rest.
DEFERRED=()
defer() { DEFERRED+=("$*"); }

cleanup() {
  local status=$? i
  if [ "${status}" -ne 0 ] && [ "${nodes_started}" = true ]; then
    collect_diagnostics
  fi
  for ((i = ${#DEFERRED[@]} - 1; i >= 0; i--)); do
    eval "${DEFERRED[$i]}" || true
  done

  # Same cause as the KUBECONFIG scratch path above: under `sudo -E` nkectl
  # writes this bundle as root, and the CI step that uploads it runs as the
  # normal user, which cannot traverse a root-owned 0750 directory. Hand
  # ownership back rather than loosening nkectl's file modes.
  if [ -n "${SUDO_UID:-}" ] && [ -d "${DIAGNOSTICS_DIR}" ]; then
    log "restoring ownership of ${DIAGNOSTICS_DIR} to uid ${SUDO_UID}"
    chown -R "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${DIAGNOSTICS_DIR}" || true
  fi

  rm -f "${TENANT_KUBECONFIG}" "${BOOTSTRAP_LOG}"
  rm -rf "${KUBECONFIG_DIR}" "${CNI_BUNDLE_DIR}"
}
nodes_started=false
trap cleanup EXIT

# wait_until retries CMD every INTERVAL seconds until it succeeds or DEADLINE
# seconds have passed. Returns 0 once CMD succeeds, 1 if the deadline passes.
wait_until() {
  local deadline=$((SECONDS + $1)) interval="$2"
  shift 2
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if "$@"; then return 0; fi
    sleep "${interval}"
  done
  return 1
}

# The teardowns that defer registers, one per thing created.
teardown_nodes() {
  log "tearing down fake node containers"
  make -C "${CLI_DIR}" nodes-down || true
}
# Bare, kind asks Docker, sees none of nerdctl's clusters and answers "no"
# without checking, so ask the provider `bootstrap site` used.
kind_lists_bootstrap_cluster() {
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters 2>/dev/null |
    grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"
}

# `bootstrap site` is given --keep-bootstrap-cluster, so this suite owns the
# kept cluster's removal; one left behind would trip the next run's stage 2.
# The failure-path net; the happy path deletes at the end, where it can assert.
teardown_bootstrap_cluster() {
  log "tearing down the kept bootstrap kind cluster"
  KIND_EXPERIMENTAL_PROVIDER=nerdctl \
    kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
}
remove_forward_rules() {
  log "removing the cross-bridge forwarding rules"
  iptables -D FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$2" -o "$1" -j ACCEPT 2>/dev/null || true
}
remove_spire_vip() {
  log "removing ${SPIRE_VIP} from $1"
  ip address del "${SPIRE_VIP}/32" dev "$1" 2>/dev/null || true
}

# Which registry each side's pulls resolve to. A pull that falls through to
# upstream is the difference between a 30 second install and a helm timeout,
# so a slow run that still passes wants this too.
registry_routing() {
  echo "### ${NODE1}: /etc/containerd/certs.d/docker.io/hosts.toml"
  nerdctl exec "${NODE1}" cat /etc/containerd/certs.d/docker.io/hosts.toml
  echo "### ${BOOTSTRAP_CONTROL_PLANE}: /etc/containerd/certs.d/docker.io/hosts.toml"
  nerdctl exec "${BOOTSTRAP_CONTROL_PLANE}" cat /etc/containerd/certs.d/docker.io/hosts.toml
}

# collect_diagnostics writes what a failed run needs once the containers are
# gone: the host's forwarding and network state, each node's journals and
# runtime state, and where each side's image pulls were routed.
collect_diagnostics() {
  local i node output
  mkdir -p "${DIAGNOSTICS_DIR}" || return 0

  log "collecting host forwarding and network state (for the cross-bridge path)"
  {
    echo "### iptables -S FORWARD"; iptables -S FORWARD
    echo "### iptables -S CNI-ISOLATION-STAGE-1"; iptables -S CNI-ISOLATION-STAGE-1
    echo "### iptables -S CNI-ISOLATION-STAGE-2"; iptables -S CNI-ISOLATION-STAGE-2
    echo "### iptables -t nat -S CNI-HOSTPORT-DNAT"; iptables -t nat -S CNI-HOSTPORT-DNAT
    echo "### nerdctl network inspect"; nerdctl network inspect kind "${NODE_NETWORK}"
    registry_routing
  } > "${DIAGNOSTICS_DIR}/host-network.txt" 2>&1 || true

  log "collecting kubelet, containerd, nke-agent and spire-agent diagnostics from ${NODE_COUNT} fake node(s)"
  for ((i = 1; i <= NODE_COUNT; i++)); do
    node="nkectl-e2e-${NODE_PREFIX}${i}"
    output="${DIAGNOSTICS_DIR}/host-diagnostics-${NODE_PREFIX}${i}.txt"
    {
      printf '### journalctl: kubelet, containerd, nke-agent, spire-agent\n'
      nerdctl exec "${node}" journalctl -u kubelet -u containerd -u nke-agent -u spire-agent \
        --boot --no-pager --output=short-iso-precise --lines=10000 || true

      printf '\n### systemctl status\n'
      nerdctl exec "${node}" systemctl status kubelet containerd --no-pager || true

      printf '\n### crictl ps -a\n'
      nerdctl exec "${node}" crictl ps -a || true

      printf '\n### crictl pods\n'
      nerdctl exec "${node}" crictl pods || true

      printf '\n### crictl info\n'
      nerdctl exec "${node}" crictl info || true

      printf '\n### ip addr\n'
      nerdctl exec "${node}" ip addr || true

      printf '\n### ip route\n'
      nerdctl exec "${node}" ip route || true

      printf '\n### CNI files\n'
      nerdctl exec "${node}" ls -la /etc/cni/net.d /opt/cni/bin || true
    } >"${output}" 2>&1
  done
}

# ── Stage 1: fake nodes ──────────────────────────────────────────────────

log "building the Calico manifests package for post-init CNI"
BUNDLE_ARCH="$(go env GOARCH)" \
  bash "${CLI_DIR}/../controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}"
CAPD_CNI_MANIFESTS_DEB_PATH="${CNI_BUNDLE_DIR}/nke-cni-calico-manifests-3.30.1_3.30.1_all.deb"
export CAPD_CNI_MANIFESTS_DEB_PATH
if [ ! -f "${CAPD_CNI_MANIFESTS_DEB_PATH}" ]; then
  fail "no Calico manifests package was built"
fi

log "starting fake node containers"
# `nodes-up` can fail after creating some containers; `nodes-down` is safe
# against a partial or nonexistent fixture, and diagnostics are only worth
# collecting once there are nodes to ask.
nodes_started=true
defer teardown_nodes
make -C "${CLI_DIR}" nodes-up

log "PASS: all fake nodes are up with TPM and SPIRE services ready"

# ── Stage 2: bootstrap site ─────────────────────────────────────────────
#
# The fake nodes booted with a software TPM and a spire-agent already pointed
# at SPIRE_VIP:30081, the address `bootstrap site` puts its SPIRE server on,
# and have been retrying since. This stage only records their EK thumbprints in
# the site spec and runs the command; the next one checks the agents got in.

if [ ! -c /dev/vtpmx ]; then
  log "loading the vTPM proxy driver (/dev/vtpmx)"
  sudo modprobe tpm_vtpm_proxy 2>/dev/null || {
    log "not loadable as shipped; it usually lives in linux-modules-extra"
    sudo apt-get install -y "linux-modules-extra-$(uname -r)" >/dev/null 2>&1 || true
    sudo modprobe tpm_vtpm_proxy 2>/dev/null || true
  }
fi
if [ ! -c /dev/vtpmx ]; then
  fail "no /dev/vtpmx. The fake nodes emulate a TPM with swtpm, which asks" \
    "the kernel to allocate one through that device, so this suite cannot run" \
    "without it. There are two causes, and the kernel config separates them:" \
    "" \
    "  grep CONFIG_TCG_VTPM_PROXY /boot/config-$(uname -r)" \
    "" \
    "=m       built, just not installed or loaded. Install" \
    "         linux-modules-extra-$(uname -r) and modprobe tpm_vtpm_proxy." \
    "         Persist it with:" \
    "           echo tpm_vtpm_proxy | sudo tee /etc/modules-load.d/tpm_vtpm_proxy.conf" \
    "not set  this kernel never built it, and no package will supply it." \
    "         Ubuntu enables it on some flavours and versions and not others" \
    "         (6.8-azure yes; 6.11-azure and 6.17-azure no), so this is an" \
    "         ordinary outcome rather than a broken machine. Run on a host" \
    "         whose kernel has it -- the CI workflow pins its runner for" \
    "         exactly this reason."
fi

log "building the provisioned CAPI provider image (${NKECTL_CAPI_PROVISIONED_IMAGE})"
make -C "${CLI_DIR}" ensure-provisioned-provider-image \
  CAPI_PROVISIONED_IMAGE="${NKECTL_CAPI_PROVISIONED_IMAGE}"

# The completed images are sufficient from this point. On a CI runner,
# retaining their BuildKit intermediates competes with the container storage
# needed by the bootstrap cluster and three site nodes.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  log "removing the BuildKit cache before cluster provisioning"
  nerdctl builder prune --all --force || true
fi

# The site spec is generated, not committed: it exercises the command an
# operator runs, and a TPM's endorsement key is created with the container so
# the thumbprints could not be committed anyway.
log "collecting each node's TPM EK thumbprint and the bootstrap CA"
HOSTS_CSV="${SCRIPT_DIR}/.hosts.csv"
# shellcheck disable=SC2046 # the assignments are the documented output contract
export $(NKECTL_E2E_HOSTS_CSV="${HOSTS_CSV}" \
  make -C "${CLI_DIR}" --no-print-directory nodes-hosts | xargs)

# Both or neither. Without a CA, SPIRE self-signs — a supported configuration,
# so the lane does not require one.
spire_set=()
if [[ -n "${NKECTL_E2E_SPIRE_CA_CERT:-}" && -n "${NKECTL_E2E_SPIRE_CA_KEY:-}" ]]; then
  spire_set=(--set "spire.caCertFile=${NKECTL_E2E_SPIRE_CA_CERT}"
             --set "spire.caKeyFile=${NKECTL_E2E_SPIRE_CA_KEY}")
fi

log "generating the site spec: nkectl generate sitespec ${E2E_CLUSTER_NAME} --nodes ${NKECTL_E2E_HOSTS_CSV}"
RENDERED_SITESPEC="${SCRIPT_DIR}/.sitespec.rendered.yaml"
"${NKECTL}" generate sitespec "${E2E_CLUSTER_NAME}" \
  --nodes "${NKECTL_E2E_HOSTS_CSV}" \
  --set cluster.k8sVersion=1.34.8 \
  --set cluster.cniName=calico \
  --set cluster.cniVersion=3.30.1 \
  --set cluster.podCidr=192.168.0.0/16 \
  --set cluster.serviceCidr=10.96.0.0/12 \
  --set cluster.kubeletConfig.systemReservedMemory=512Mi \
  "${spire_set[@]}" \
  > "${RENDERED_SITESPEC}"

# One thumbprint per node, init node included; stage 4 expects that many nodes.
expected_nodes=$(grep -cE '^[[:space:]]+tpmEkThumbprint:' "${RENDERED_SITESPEC}" || true)
if [ "${expected_nodes}" -eq 0 ]; then
  fail "no TPM EK thumbprints reached ${RENDERED_SITESPEC}"
fi
if [[ ${#spire_set[@]} -gt 0 ]]; then
  grep -qE '^[[:space:]]*caCertFile:' "${RENDERED_SITESPEC}" ||
    fail "a bootstrap CA was supplied but did not reach ${RENDERED_SITESPEC}"
fi
log "allowlisting ${expected_nodes} EK thumbprint(s)"

# No chart is named here. What gets installed is pinned in
# cli/internal/cwedeploy, which is the thing under test: the SPIFFE project's
# public hardened charts, needing no registry credential of their own.
log "running: nkectl bootstrap site -f ${RENDERED_SITESPEC} --keep-bootstrap-cluster --timeout 15m" \
  "(NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}," \
  "NKECTL_CAPI_PROVISIONED_IMAGE=${NKECTL_CAPI_PROVISIONED_IMAGE})"

# In the background, because the attach below has to happen *during* the run:
# CWE and the spire-agents talk over the node network, which the bootstrap
# cluster is not on, because the nodes came up before it existed.
#
# --keep-bootstrap-cluster: this suite's EXIT trap removes the cluster, and the
# teardown registers here because the command can fail after creating it.
defer teardown_bootstrap_cluster
# setsid makes nkectl a process-group leader so abort_bootstrap can signal the
# kind, nerdctl and helm it drives as one group; nkectl does not forward
# signals to its children.
setsid "${NKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" --keep-bootstrap-cluster \
  --timeout 15m --kubeconfig-out "${TENANT_KUBECONFIG}" >"${BOOTSTRAP_LOG}" 2>&1 &
bootstrap_pid=$!

# abort_bootstrap stops the background `bootstrap site` and its children before
# this script exits -- nkectl runs as a process-group leader, so the whole
# group is signalled and reaped. Otherwise the EXIT trap would delete the
# cluster the command is still driving.
abort_bootstrap() {
  kill -- "-${bootstrap_pid}" 2>/dev/null || kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  cat "${BOOTSTRAP_LOG}" || true
  exit 1
}

# cross_bridge_reachable tests forwarding between the two networks, not the
# service on the far side. Refused is the pass: the packet crossed and the far
# side answered that nothing is listening. A timeout is the failure, and is
# what the CNI firewall's cross-bridge DROP looks like. bash's /dev/tcp gives
# that distinction for free, neither image shipping curl.
cross_bridge_reachable() {
  local from="$1" ip="$2" port="$3" what="$4"
  local out rc
  out="$(nerdctl exec "${from}" timeout 5 bash -c "exec 3<>/dev/tcp/${ip}/${port}" 2>&1)" && rc=0 || rc=$?
  case "${rc}" in
    0) log "  ${what}: ${ip}:${port} accepted the connection"; return 0 ;;
    124) log "FAIL: ${what}: ${ip}:${port} timed out -- packets are not crossing the bridges"; return 1 ;;
    *)
      if printf '%s' "${out}" | grep -qi "refused"; then
        log "  ${what}: ${ip}:${port} reachable (refused; nothing listening yet, as expected before SPIRE)"
        return 0
      fi
      log "FAIL: ${what}: ${ip}:${port} failed (exit ${rc}): ${out}"
      return 1
      ;;
  esac
}

# probe_cross_bridge_connectivity checks both directions the moment the cluster
# is attached; unchecked, a break here surfaces fifteen minutes later as "did
# not report provisioned". nke-agent does not bind its port until the node has
# an SVID and SPIRE is not deployed yet, so kind -> node is expected to be
# refused rather than accepted. node -> kind uses the control plane's kubelet
# port, listening as soon as the cluster is Ready.
probe_cross_bridge_connectivity() {
  local cp_ip ok=true
  # From the network, not the container: the container's own inspect lists its
  # pod bridge alongside the kind network, and ranging over both concatenates
  # the addresses. The network knows exactly one address for it.
  cp_ip="$(nerdctl network inspect kind --format \
    "{{range .Containers}}{{if eq .Name \"${BOOTSTRAP_CONTROL_PLANE}\"}}{{.IPv4Address}}{{end}}{{end}}")" || return 1
  cp_ip="${cp_ip%/*}"
  if [ -z "${cp_ip}" ]; then
    log "FAIL: could not find ${BOOTSTRAP_CONTROL_PLANE} on the kind network"
    return 1
  fi
  cross_bridge_reachable "${BOOTSTRAP_CONTROL_PLANE}" "${INIT_NODE_IP}" "${NODE_AGENT_PORT}" "kind -> node" || ok=false
  cross_bridge_reachable "${NODE1}" "${cp_ip}" 10250 "node -> kind" || ok=false
  if [ "${ok}" != true ]; then
    log "host FORWARD chain:"
    iptables -S FORWARD | sed 's/^/  /'
    return 1
  fi
  log "cross-bridge connectivity confirmed in both directions"
}

# attach_bootstrap_to_node_network bridges the two networks. Both are nerdctl
# bridges, and nerdctl's default CNI config sets the firewall plugin's
# ingressPolicy to "same-bridge", which DROPs any forward from one CNI bridge
# out another. So CWE -> nke-agent and node -> SPIRE VIP are dropped by
# default, CWE's NodePreflight times out, and no node attests. Permit exactly
# this pair of bridges, both directions, at position 1 so it precedes the
# isolation chain; nothing else on the host becomes reachable.
#
# nerdctl cannot connect a running container to another network, so the SPIRE
# VIP goes onto the node network's bridge instead.
attach_bootstrap_to_node_network() {
  local node_id kind_id node_bridge kind_bridge
  node_id="$(nerdctl network inspect --format '{{.Id}}' "${NODE_NETWORK}")" || return 1
  node_bridge="br-${node_id:0:12}"
  ip link show "${node_bridge}" >/dev/null || return 1
  kind_id="$(nerdctl network inspect --format '{{.Id}}' kind)" || return 1
  kind_bridge="br-${kind_id:0:12}"
  ip link show "${kind_bridge}" >/dev/null || return 1

  defer "remove_forward_rules ${kind_bridge} ${node_bridge}"
  iptables -I FORWARD 1 -i "${kind_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  iptables -I FORWARD 1 -i "${node_bridge}" -o "${kind_bridge}" -j ACCEPT || return 1
  defer "remove_spire_vip ${node_bridge}"
  ip address replace "${SPIRE_VIP}/32" dev "${node_bridge}" || return 1
}

# Attach as soon as Kind reports the cluster ready: after Kind has finished
# with the control-plane container, and before CWE is deployed, so the extra
# network disturbs neither. Bounded separately from the command's own timeout,
# and abandoned if the command exits first.
log "waiting for the bootstrap cluster to become ready, to attach it to the node network at ${SPIRE_VIP}"
attached=false
attach_deadline=$((SECONDS + ${NKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${attach_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    if attach_bootstrap_to_node_network; then
      log "attached the bootstrap cluster at ${SPIRE_VIP}"
      probe_cross_bridge_connectivity || abort_bootstrap
      attached=true
    else
      log "WARN: could not attach the bootstrap cluster to the node network"
    fi
    break
  fi
  sleep 2
done

if [ "${attached}" != true ]; then
  log "FAIL: the bootstrap cluster was never attached to the node network, so CWE"
  log "  cannot reach the nodes and their agents cannot reach SPIRE. Not waiting"
  log "  for a bootstrap that has no route to the hosts it is building on."
  abort_bootstrap
fi

status=0
wait "${bootstrap_pid}" || status=$?
cat "${BOOTSTRAP_LOG}"
output="$(cat "${BOOTSTRAP_LOG}")"

if [ "${status}" -ne 0 ]; then
  fail "nkectl bootstrap site exited ${status}"
fi
if ! grep -q "cwe deployment healthy" <<<"${output}"; then
  fail "CWE was never confirmed healthy on /readyz"
fi
if ! grep -q "cluster provisioned" <<<"${output}"; then
  fail "the command exited 0 without reporting the cluster provisioned"
fi
if [ ! -s "${TENANT_KUBECONFIG}" ]; then
  fail "no kubeconfig was written to ${TENANT_KUBECONFIG}"
fi
log "PASS: bootstrap site created the site cluster through the CWE API"

# ── Stage 3: every node attested to SPIRE over TPM ──────────────────────

log "waiting for every node's spire-agent to attest"
make -C "${CLI_DIR}" nodes-attested

log "PASS: bootstrap site deployed SPIRE and every node attested to it over TPM"

# all_nodes_ready is the stage 4 condition. `kubectl wait --all` only waits for
# the objects that exist when it starts, so count and readiness are checked
# together: the first Ready node must not pass the stage before CAPI creates
# the rest. Sets total_nodes/ready_nodes for the failure message.
all_nodes_ready() {
  local node_states
  node_states=$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
    -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
    2>/dev/null || true)
  total_nodes=$(printf '%s\n' "${node_states}" | awk 'NF {count++} END {print count+0}')
  ready_nodes=$(printf '%s\n' "${node_states}" | awk '$2 == "True" {count++} END {print count+0}')
  [ "${total_nodes}" -eq "${expected_nodes}" ] && [ "${ready_nodes}" -eq "${expected_nodes}" ]
}

# ── Stage 4: every pre-provisioned host is a Ready node ─────────────────
#
# Every site spec node joins as a control-plane node and CWE untaints them, so
# all must be Ready and schedulable. CWE joins them one at a time, ~6 min
# apart, so the third kubelet starts ~13 min after the cluster is created.

log "waiting for ${expected_nodes} nodes to report Ready"
total_nodes=0
ready_nodes=0
if ! wait_until "${NKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-1200}" 5 all_nodes_ready; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide || true
  fail "${ready_nodes} of ${expected_nodes} site spec nodes became Ready (${total_nodes} exist)"
fi
log "PASS: all ${ready_nodes} pre-provisioned hosts are Ready nodes"

# hello_world_served is the stage 5 condition. whoami answers with the request
# it received, so "Hostname:" in the body is the pod talking, not Traefik's own
# error page. The request carries the Ingress host name so it has to match a
# rule Traefik read from the Ingress rather than falling through to a default.
hello_world_served() {
  curl -fsS --max-time 5 -H "Host: hello.nke.local" \
    "http://${INIT_NODE_IP}:${TRAEFIK_NODE_PORT}/" | grep -q "Hostname:"
}

# ── Stage 5: hello world ────────────────────────────────────────────────
#
# Traefik is published on a NodePort because that is a route the host has to
# these containers. `--repo` rather than `helm repo add`, so this suite does
# not mutate the caller's global repository list. service.spec.type, not
# service.type: the chart puts the Service spec's fields under `spec`, and a
# value set at the wrong depth is silently ignored.

log "registry routing for this run:"
registry_routing 2>&1 | sed 's/^/  /' || true

log "installing traefik on the new cluster"
helm --kubeconfig "${TENANT_KUBECONFIG}" upgrade --install traefik traefik \
  --repo https://traefik.github.io/charts \
  --version "${NKECTL_E2E_TRAEFIK_CHART_VERSION:-41.4.0}" \
  --namespace traefik --create-namespace \
  --set service.spec.type=NodePort \
  --set "ports.web.nodePort=${TRAEFIK_NODE_PORT}" \
  --set 'tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'tolerations[0].operator=Exists' \
  --set 'tolerations[0].effect=NoSchedule' \
  --wait --timeout 5m

log "deploying the hello-world workload"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" apply -f "${SCRIPT_DIR}/manifests/hello-world.yaml"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n hello-world rollout status deployment/whoami --timeout=5m

log "asking for the page at http://${INIT_NODE_IP}:${TRAEFIK_NODE_PORT}/ (Host: hello.nke.local)"
if ! wait_until 300 5 hello_world_served; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n hello-world get all,ingress || true
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n traefik logs deployment/traefik --tail=100 || true
  fail "hello world was never served through Traefik"
fi
log "PASS: the cluster built from pre-provisioned hosts serves hello world through Traefik"

# ── This suite's own teardown ───────────────────────────────────────────
#
# Deleted here rather than left to the EXIT trap, which can only swallow a
# failed delete. A cluster left registered makes the next run stop on a name
# clash instead.
#
# Positive control first: kind exits 0 and prints "No kind clusters found."
# even when its provider is unreachable, so absence alone proves nothing.
if ! kind_lists_bootstrap_cluster; then
  fail "${BOOTSTRAP_KIND_CLUSTER} is not registered before teardown" \
    "so this suite cannot see the nerdctl provider and the check below proves nothing"
fi

log "tearing down the bootstrap kind cluster kept for attestation"
KIND_EXPERIMENTAL_PROVIDER=nerdctl \
  kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}"

if kind_lists_bootstrap_cluster; then
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters || true
  fail "kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after this suite deleted it"
fi
log "PASS: this suite's teardown left no ${BOOTSTRAP_KIND_CLUSTER} cluster behind"
