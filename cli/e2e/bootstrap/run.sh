#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# nkectl bootstrap-site e2e entrypoint, invoked by `make test-e2e-bootstrap`
# and the "nkectl bootstrap e2e" CI workflow. Scoped to `bootstrap site`
# specifically — cli/e2e/ is the home for e2e suites covering other nkectl
# commands too, each in its own subdirectory alongside this one.
#
# Grown incrementally, one stage per change, alongside `nkectl bootstrap
# site`'s real implementation — each stage below is added only once the code
# it exercises exists, so this script and the command it drives are never out
# of sync.
#
# Current stages:
#   1. Fake node containers come up and nke-agent answers on each — proves
#      the fixture a later change's real dispatch will depend on, ahead of
#      that change actually reaching it. The wait for readiness lives in the
#      substrate package (cli/e2e/bootstrap/substrate), not here: `nodes-up`
#      does not return until every node's agent is confirmed active.
#   2. `bootstrap site` stands up the ephemeral bootstrap Kind cluster
#      (cli/internal/bootstrapengine), deploys Postgres, Temporal, SPIRE and CWE
#      onto it (cli/internal/cwedeploy) and waits for CWE to report healthy on
#      /readyz, then stops at its not-yet-implemented workflow-dispatch stub.
#      Both the CWE-healthy line and the stub error are asserted by name, so a
#      change that silently skips the deploy is caught here. The stub assertion
#      does not require the command to fail -- see the stage itself.
#   3. The Kind cluster is confirmed gone afterwards. It is torn down by this
#      script rather than by the command, because stage 4 needs it kept alive.
#   4. TPM node attestation. The fake nodes boot
#      with a software TPM and a spire-agent already looking for the SPIRE
#      server. `bootstrap site` is given their EK thumbprints, in the
#      site spec, and deploys SPIRE with them allowlisted; the check is that
#      the agents got in.
#   5. The cluster itself. `bootstrap site` posts the site spec to CWE as a
#      Cluster API cluster whose infrastructure is the pre-provisioned hosts
#      (a ProvisionedCluster and a ProvisionedMachineTemplate), waits for it to
#      finish, and writes out its kubeconfig. The command now exits 0.
#   6. Every node of that cluster reports Ready through that kubeconfig.
#   7. Hello world. Traefik is installed on the new cluster and a whoami
#      Deployment is served through it, and this suite asks for the page. That
#      is the end-to-end statement the earlier stages only imply: a cluster
#      built out of hosts that already existed serves traffic like any other.
#
# This suite runs entirely on nerdctl/containerd. It needs, on the host:
#
#   - nerdctl and a containerd for it to talk to. The ephemeral cluster runs
#     there, because nkectl drives kind through
#     KIND_EXPERIMENTAL_PROVIDER=nerdctl unconditionally (see
#     cli/internal/bootstrapengine), and so do the fake-node containers. kind
#     states 1.7.0 as the minimum for its nerdctl provider; 2.3.5 is the
#     version CI exercises. See the CI workflow
#     (.github/workflows/nkectl-bootstrap-e2e.yml) for the standalone install
#     this repo has verified.
#   - buildkitd, for `nerdctl build` to reach. The three fixture images are
#     built through it (see the ensure-*-image targets in cli/Makefile);
#     nerdctl is a BuildKit client, not a builder, and fails outright without
#     one. The CI workflow starts it on the same containerd.
#   - root, or sudo: attaching the bootstrap cluster to the node network
#     installs two iptables FORWARD rules and assigns the SPIRE VIP to a
#     bridge.
#     Without it the run fails at "the bootstrap cluster was never attached
#     to the node network", after the images have already been built.
#
# Docker is not used anywhere here, and `make -C cli check-no-docker` is what
# keeps that true.
#
# The fake nodes emulate a TPM with swtpm, which asks the kernel for a device
# through /dev/vtpmx. This script loads tpm_vtpm_proxy if it is not already
# there; a host that cannot load it cannot run this suite.
#
# NKECTL_CWE_IMAGE must already be present in containerd's image store --
# built there, or pulled with `nerdctl pull` -- before this script runs. That
# is the store nkectl loads from: it saves the image and loads it into the
# kind cluster rather than pulling from inside the cluster (see
# cmd/nkectl/bootstrap.go's ensureImageAvailable), so no registry credentials
# are needed past that point. CI builds the image locally rather than pulling
# it (see the workflow's "Build CWE image" step).

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
NKECTL="${NKECTL:-${CLI_DIR}/bin/nkectl}"
# The site spec is generated per run by `nkectl generate sitespec`, so this lane
# tests the command an operator uses rather than a fixture kept in step with it
# by hand. Only the cluster name is fixed here.
E2E_CLUSTER_NAME="nke-e2e"
BOOTSTRAP_KIND_CLUSTER="${NKECTL_BOOTSTRAP_CLUSTER_NAME:-nkectl-bootstrap}"
# SiteSpec generation records the exact journal boundary and expected result
# for every node here. The later check reads the same file after bootstrap.
export NKECTL_E2E_ATTESTATION_BASELINE="${CLI_DIR}/.local/attestation-baseline.json"
# Where the node images' spire-agents were built to look for the SPIRE server.
SPIRE_VIP="${NKECTL_E2E_SPIRE_SERVER_ADDRESS:-172.26.0.2}"
# The init node, which is also where the finished cluster's API server ends up:
# no controlPlaneVIP is set, so kubeadm uses the first control-plane node's IP.
# It matches the first row of the generated node CSV, and it is the address
# stages 6 and 7 use.
INIT_NODE_IP="${NKECTL_E2E_INIT_NODE_IP:-172.26.0.10}"
# Where Traefik is published on the new cluster, and where stage 7 asks for the
# page. In the NodePort range, and clear of the SPIRE server's 30081.
TRAEFIK_NODE_PORT="${NKECTL_E2E_TRAEFIK_NODE_PORT:-30082}"
# Where `bootstrap site` writes the finished cluster's kubeconfig.
TENANT_KUBECONFIG="${SCRIPT_DIR}/.tenant.kubeconfig"
: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already pulled locally (see comment above)}"
export NKECTL_CWE_IMAGE

# The Cluster API infrastructure provider for hosts that already exist, built
# below from this checkout. Without it the CAPI objects CWE writes have no
# controller and the cluster never leaves provisioning. nkectl loads it into the
# bootstrap cluster the same way it loads the CWE image, and finds
# the kustomize base to render by walking up to go.work — so only the image has
# to be named here.
export NKECTL_CAPI_PROVISIONED_IMAGE="${NKECTL_CAPI_PROVISIONED_IMAGE:-cluster-api-provider-provisioned:local}"

# Where a failed bootstrap writes its cluster dump. Set explicitly rather than
# left to nkectl's default, which is relative to the working directory: CI runs
# this from cli/, but a developer running it from the repo root would otherwise
# scatter the bundle somewhere the CI artifact path does not match.
DIAGNOSTICS_DIR="${CLI_DIR}/cluster-diagnostics"
export NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR="${DIAGNOSTICS_DIR}"

# `kind create cluster` merges into $KUBECONFIG, defaulting to ~/.kube/config
# if unset -- own scratch path instead, so this run never touches (or takes
# ownership of) the caller's default kubeconfig. This matters most for the
# CI job, which runs this whole script under `sudo -E`: without
# this, `kind create cluster` running as root would leave ~/.kube/config
# root-owned, and a later, unrelated cleanup step running as the normal user
# (helm/kind-action's own post-job step) would fail with "permission denied"
# trying to update it.
KUBECONFIG_DIR="$(mktemp -d)"
export KUBECONFIG="${KUBECONFIG_DIR}/config"
CNI_BUNDLE_DIR="$(mktemp -d)"

# KUBECONFIG alone is not enough: helm caches API discovery under
# ~/.kube/cache and creates ~/.kube mode 0750 to do it, so under `sudo -E`
# that directory is root-owned and the normal user cannot write the lock file
# next to it either. `kubectl --raw` does not do this; helm does, on every
# install. KUBECACHEDIR moves that cache into the same scratch directory.
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"

log() { printf '[nkectl-bootstrap-e2e] %s\n' "$*"; }

# kind resolves a cluster through its provider, so ask the same one bootstrap
# site used. Bare, kind asks Docker, sees none of nerdctl's clusters, and
# answers "no" without checking anything.
kind_lists_bootstrap_cluster() {
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters 2>/dev/null |
    grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"
}

nodes_up=false
bootstrap_cluster_cleanup_pending=false
spire_vip_device=""
node_bridge=""
nerdctl_bridge=""
nerdctl_node_access=false

collect_node_diagnostics() {
  local node_count="${NKECTL_E2E_NODES:-3}"
  local name_prefix="${NKECTL_E2E_NAME_PREFIX:-node}"
  local i node output

  mkdir -p "${DIAGNOSTICS_DIR}" || return 0
  log "host forwarding and network state (for the cross-bridge path)"
  {
    echo "### iptables -S FORWARD"; iptables -S FORWARD
    echo "### iptables -S CNI-ISOLATION-STAGE-1"; iptables -S CNI-ISOLATION-STAGE-1
    echo "### iptables -S CNI-ISOLATION-STAGE-2"; iptables -S CNI-ISOLATION-STAGE-2
    echo "### iptables -t nat -S CNI-HOSTPORT-DNAT"; iptables -t nat -S CNI-HOSTPORT-DNAT
    echo "### nerdctl network inspect"; nerdctl network inspect kind "${NKECTL_E2E_NETWORK:-nkectl-e2e-nodes}"
  } > "${DIAGNOSTICS_DIR}/host-network.txt" 2>&1 || true
  log "collecting kubelet and containerd diagnostics from ${node_count} fake node(s)"
  for ((i = 1; i <= node_count; i++)); do
    node="nkectl-e2e-${name_prefix}${i}"
    output="${DIAGNOSTICS_DIR}/host-diagnostics-${name_prefix}${i}.txt"
    {
      printf '### journalctl: kubelet and containerd\n'
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

cleanup() {
  local status=$?
  if [ "${status}" -ne 0 ] && [ "${nodes_up}" = true ]; then
    collect_node_diagnostics
  fi

  # --keep-bootstrap-cluster means the command does not clean up after
  # itself, so a failure between it and the explicit teardown below would
  # otherwise leak a cluster that the next run's stage 3 then trips over.
  if [ "${bootstrap_cluster_cleanup_pending}" = true ]; then
    log "tearing down the kept bootstrap kind cluster"
    KIND_EXPERIMENTAL_PROVIDER=nerdctl \
      kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
  fi
  if [ "${nodes_up}" = true ]; then
    if [ "${nerdctl_node_access}" = true ]; then
      log "removing the cross-bridge forwarding rules"
      iptables -D FORWARD -i "${nerdctl_bridge}" -o "${node_bridge}" -j ACCEPT 2>/dev/null || true
      iptables -D FORWARD -i "${node_bridge}" -o "${nerdctl_bridge}" -j ACCEPT 2>/dev/null || true
    fi
    if [ -n "${spire_vip_device}" ]; then
      log "removing ${SPIRE_VIP} from ${spire_vip_device}"
      ip address del "${SPIRE_VIP}/32" dev "${spire_vip_device}" 2>/dev/null || true
    fi
    log "tearing down fake node containers"
    make -C "${CLI_DIR}" nodes-down || true
  fi

  # Same class of bug as the KUBECONFIG one above, and the same cause: under
  # `sudo -E` nkectl wrote this bundle as root. The CI step
  # that uploads it runs as the normal user, which cannot so much as traverse
  # into a root-owned 0750 directory -- so without this the diagnostics are
  # collected and then silently dropped, on exactly the failing runs they
  # exist for. Hand ownership back instead of loosening nkectl's file modes:
  # the bundle contains whatever the pod logs contained.
  if [ -n "${SUDO_UID:-}" ] && [ -d "${DIAGNOSTICS_DIR}" ]; then
    log "restoring ownership of ${DIAGNOSTICS_DIR} to uid ${SUDO_UID}"
    chown -R "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${DIAGNOSTICS_DIR}" || true
  fi

  rm -f "${TENANT_KUBECONFIG}" "${SCRIPT_DIR}/.bootstrap.log"
  rm -rf "${KUBECONFIG_DIR}" "${CNI_BUNDLE_DIR}"
}
trap cleanup EXIT

# ── Stage 1: fake nodes ──────────────────────────────────────────────────

log "building the Calico manifests package for post-init CNI"
BUNDLE_ARCH="$(go env GOARCH)" \
  bash "${CLI_DIR}/../controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}"
CAPD_CNI_MANIFESTS_DEB_PATH="${CNI_BUNDLE_DIR}/nke-cni-calico-manifests-3.30.1_3.30.1_all.deb"
export CAPD_CNI_MANIFESTS_DEB_PATH
if [ ! -f "${CAPD_CNI_MANIFESTS_DEB_PATH}" ]; then
  log "FAIL: no Calico manifests package was built"
  exit 1
fi

log "starting fake node containers"
# Set before the call, not after: `nodes-up` can fail after creating some
# containers (e.g. node3 fails to start while node1/node2 succeeded). Under
# `set -e` that failure exits the script immediately, so cleanup must already
# know to tear down whatever got created rather than skipping it because the
# call never "finished". `nodes-down` is safe to run against a partial or
# nonexistent fixture.
nodes_up=true
make -C "${CLI_DIR}" nodes-up

log "PASS: all fake nodes are up with TPM and SPIRE services ready"

# ── Stages 2-4: bootstrap, then TPM attestation ─────────────────────────
#
# The fake nodes booted with a software TPM and a spire-agent already pointed at
# SPIRE_VIP:30081 — the address `bootstrap site` puts its SPIRE server on. They
# have been retrying since. So this stage does not configure or start anything
# on them: it records their EK thumbprints in the site spec, runs the command,
# and checks the agents got in.
#
# That ordering is the point. The thumbprints are an *input* to `bootstrap
# site`, which seeds them into the SPIRE server it installs. A bootstrap that
# deployed SPIRE without wiring the TPM attestor, or dropped the allowlist, or
# put the server somewhere else, leaves every agent locked out and fails here.
#
# The command is given --keep-bootstrap-cluster, because the agents need the
# cluster to still be there when they reach it, and this stage tears it down
# itself afterwards. The "cluster is gone" assertion is kept, at the end,
# against that explicit teardown.
if [ ! -c /dev/vtpmx ]; then
  log "loading the vTPM proxy driver (/dev/vtpmx)"
  sudo modprobe tpm_vtpm_proxy 2>/dev/null || {
    log "not loadable as shipped; it usually lives in linux-modules-extra"
    sudo apt-get install -y "linux-modules-extra-$(uname -r)" >/dev/null 2>&1 || true
    sudo modprobe tpm_vtpm_proxy 2>/dev/null || true
  }
fi
if [ ! -c /dev/vtpmx ]; then
  log "FAIL: no /dev/vtpmx. The fake nodes emulate a TPM with swtpm, which asks"
  log "  the kernel to allocate one through that device, so this suite cannot run"
  log "  without it. There are two causes, and the kernel config separates them:"
  log ""
  log "    grep CONFIG_TCG_VTPM_PROXY /boot/config-$(uname -r)"
  log ""
  log "  =m       built, just not installed or loaded. Install"
  log "           linux-modules-extra-$(uname -r) and modprobe tpm_vtpm_proxy."
  log "           Persist it with:"
  log "             echo tpm_vtpm_proxy | sudo tee /etc/modules-load.d/tpm_vtpm_proxy.conf"
  log "  not set  this kernel never built it, and no package will supply it."
  log "           Ubuntu enables it on some flavours and versions and not others"
  log "           (6.8-azure yes; 6.11-azure and 6.17-azure no), so this is an"
  log "           ordinary outcome rather than a broken machine. Run on a host"
  log "           whose kernel has it -- the CI workflow pins its runner for"
  log "           exactly this reason."
  exit 1
fi

log "building the provisioned CAPI provider image (${NKECTL_CAPI_PROVISIONED_IMAGE})"
make -C "${CLI_DIR}" ensure-provisioned-provider-image \
  CAPI_PROVISIONED_IMAGE="${NKECTL_CAPI_PROVISIONED_IMAGE}"

# No cross-runtime image bridge here: every image above is built by `nerdctl
# build`, so it already lives in the containerd store nkectl loads from.

# The completed images are sufficient from this point. On GitHub's 14 GB
# runner, retaining their BuildKit intermediates competes with the container
# storage needed by the bootstrap cluster and three site nodes.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  log "removing the BuildKit cache before cluster provisioning"
  nerdctl builder prune --all --force || true
fi

# The site spec is generated, not committed, so this lane exercises the command
# an operator runs rather than a fixture kept in step with it by hand. The
# thumbprints and the bootstrap CA could not be committed anyway: a TPM's
# endorsement key is created along with the container, and a private key does
# not belong in a repository.
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

grep -qE '^[[:space:]]+tpmEkThumbprint:' "${RENDERED_SITESPEC}" ||
  { log "FAIL: no TPM EK thumbprints reached ${RENDERED_SITESPEC}"; exit 1; }
if [[ ${#spire_set[@]} -gt 0 ]]; then
  grep -qE '^[[:space:]]*caCertFile:' "${RENDERED_SITESPEC}" ||
    { log "FAIL: a bootstrap CA was supplied but did not reach ${RENDERED_SITESPEC}"; exit 1; }
fi
log "allowlisting $(grep -cE '^[[:space:]]+tpmEkThumbprint:' "${RENDERED_SITESPEC}") EK thumbprint(s)"

# No chart is named here. What gets installed is pinned in
# cli/internal/cwedeploy, which is the thing under test: the SPIFFE project's
# public hardened charts, needing no registry credential of their own.
log "running: nkectl bootstrap site -f ${RENDERED_SITESPEC} --keep-bootstrap-cluster --timeout 15m" \
  "(NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}," \
  "NKECTL_CAPI_PROVISIONED_IMAGE=${NKECTL_CAPI_PROVISIONED_IMAGE})"

# In the background, because the run has to be joined while it is still going.
#
# The command does not stop after deploying CWE: it creates the cluster and
# waits for CWE to build it on the fake nodes. CWE reaches those nodes, and
# their spire-agents reach SPIRE, over the node network — which the bootstrap
# cluster is not on, because the nodes came up before it existed. So the attach
# below has to happen *during* the run rather than after it, or nothing on
# either side of that route ever completes.
#
# Set before the call, not after: the command can fail after creating the
# cluster it was told to keep, and cleanup has to already know to remove it.
bootstrap_cluster_cleanup_pending=true
BOOTSTRAP_LOG="${SCRIPT_DIR}/.bootstrap.log"
# setsid makes nkectl a process-group leader, so stop_bootstrap can signal it
# and everything it spawns -- the kind, nerdctl and helm it drives -- as one
# group. nkectl does not forward signals to its children.
setsid "${NKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" --keep-bootstrap-cluster \
  --timeout 15m --kubeconfig-out "${TENANT_KUBECONFIG}" >"${BOOTSTRAP_LOG}" 2>&1 &
bootstrap_pid=$!

# stop_bootstrap ends the background `bootstrap site` before this script exits,
# children included: the whole process group is signalled, then reaped, and
# the log printed. Without it the EXIT trap would delete the cluster the
# command is still driving, and the command would outlive the script.
stop_bootstrap() {
  kill -- "-${bootstrap_pid}" 2>/dev/null || kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  cat "${BOOTSTRAP_LOG}" || true
}

# Both directions across the two bridges, checked the moment the cluster is
# attached. Unchecked, a break here surfaces fifteen minutes later as "did not
# report provisioned", with the cause buried in CWE's pod log; this names it
# in seconds.
#
# What is being tested is forwarding, not the services. nke-agent does not
# bind its port until the node has an SVID (agent/cmd/agent/main.go blocks on
# workloadapi.NewX509Source before net.Listen), and SPIRE is not deployed yet
# at this point, so a connection to 15005 cannot succeed here. A refused
# connection is the pass: the packet crossed the bridges and the node
# answered that nothing is listening. A timeout is the failure: nothing
# answered, which is what the CNI firewall's cross-bridge DROP looks like.
# bash's /dev/tcp gives that distinction for free (neither image ships curl):
# refused fails instantly, dropped hangs until `timeout` kills it with 124.
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

probe_cross_bridge_connectivity() {
  local cp="${BOOTSTRAP_KIND_CLUSTER}-control-plane"
  local node1="nkectl-e2e-${NKECTL_E2E_NAME_PREFIX:-node}1"
  local cp_ip ok=true
  # From the network, not the container: the control plane's own inspect
  # lists its pod bridge alongside the kind network, and ranging over both
  # concatenated the addresses. The network knows exactly one address for it.
  cp_ip="$(nerdctl network inspect kind --format \
    "{{range .Containers}}{{if eq .Name \"${cp}\"}}{{.IPv4Address}}{{end}}{{end}}")" || return 1
  cp_ip="${cp_ip%/*}"
  if [ -z "${cp_ip}" ]; then
    log "FAIL: could not find ${cp} on the kind network"
    return 1
  fi
  # kind -> node: the path CWE takes to nke-agent (the site spec's nodeAgentPort).
  cross_bridge_reachable "${cp}" "${INIT_NODE_IP}" 15005 "kind -> node" || ok=false
  # node -> kind: the path a node takes to the SPIRE VIP, against the control
  # plane's kubelet port, which is listening as soon as the cluster is Ready.
  cross_bridge_reachable "${node1}" "${cp_ip}" 10250 "node -> kind" || ok=false
  if [ "${ok}" != true ]; then
    log "host FORWARD chain:"
    iptables -S FORWARD | sed 's/^/  /'
    return 1
  fi
  log "cross-bridge connectivity confirmed in both directions"
}

attach_bootstrap_to_node_network() {
  local node_network="${NKECTL_E2E_NETWORK:-nkectl-e2e-nodes}"
  local network_id bridge nerdctl_network_id
  network_id="$(nerdctl network inspect --format '{{.Id}}' "${node_network}")" || return 1
  bridge="br-${network_id:0:12}"
  ip link show "${bridge}" >/dev/null || return 1
  nerdctl_network_id="$(nerdctl network inspect --format '{{.Id}}' kind)" || return 1
  nerdctl_bridge="br-${nerdctl_network_id:0:12}"
  ip link show "${nerdctl_bridge}" >/dev/null || return 1

  # Both networks are nerdctl bridges, and nerdctl's default network config
  # gives the CNI firewall plugin ingressPolicy "same-bridge"
  # (nerdctl pkg/netutil/cni_plugin_unix.go). The plugin implements that as
  # `iptables -I FORWARD -j CNI-ISOLATION-STAGE-1`, whose STAGE-2 DROPs any
  # forward from one CNI bridge out another
  # (containernetworking/plugins plugins/meta/firewall/ingresspolicy.go). So
  # CWE -> nke-agent and node -> SPIRE VIP are both dropped by default; CWE's
  # NodePreflight then times out dialing 172.26.0.x:15005, and no node ever
  # attests. Permit exactly this pair of bridges, both directions, inserted
  # at position 1 so it precedes STAGE-1. Nothing else on the host is
  # reachable through these rules.
  # Recorded before anything is installed, so cleanup removes whatever did
  # get installed: a failure between the two inserts, or after them, must
  # not leave a FORWARD ACCEPT behind on the host. The VIP's device likewise,
  # before the address goes onto it.
  node_bridge="${bridge}"
  nerdctl_node_access=true
  iptables -I FORWARD 1 -i "${nerdctl_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  iptables -I FORWARD 1 -i "${node_bridge}" -o "${nerdctl_bridge}" -j ACCEPT || return 1
  spire_vip_device="${node_bridge}"
  ip address replace "${SPIRE_VIP}/32" dev "${node_bridge}" || return 1
}

# Attach as soon as Kind reports the bootstrap cluster ready. This is before CWE
# is deployed, so changing the container's networks cannot reset the CWE create
# request. It is also after Kind has finished configuring the control-plane
# container, so the additional network cannot affect cluster creation.
#
# Bounded separately from the command's own timeout, and abandoned if the
# command exits first. nerdctl cannot connect a running container to another
# network, so the Kind host mapping receives the SPIRE VIP on the Docker
# node-network bridge instead.
log "waiting for the bootstrap cluster to become ready, to attach it to the node network at ${SPIRE_VIP}"
attached=false
attach_deadline=$((SECONDS + ${NKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${attach_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    if attach_bootstrap_to_node_network; then
      log "attached the bootstrap cluster at ${SPIRE_VIP}"
      if ! probe_cross_bridge_connectivity; then
        stop_bootstrap
        exit 1
      fi
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
  stop_bootstrap
  exit 1
fi
set +e
wait "${bootstrap_pid}"
status=$?
set -e
cat "${BOOTSTRAP_LOG}"
output="$(cat "${BOOTSTRAP_LOG}")"

# ── Stage 5: the command builds the cluster and exits 0 ──────────────────

if [ "${status}" -ne 0 ]; then
  log "FAIL: nkectl bootstrap site exited ${status}"
  exit 1
fi
if ! grep -q "cwe deployment healthy" <<<"${output}"; then
  log "FAIL: CWE was never confirmed healthy on /readyz"
  exit 1
fi
if ! grep -q "cluster provisioned" <<<"${output}"; then
  log "FAIL: the command exited 0 without reporting the cluster provisioned"
  exit 1
fi
if [ ! -s "${TENANT_KUBECONFIG}" ]; then
  log "FAIL: no kubeconfig was written to ${TENANT_KUBECONFIG}"
  exit 1
fi
log "PASS: bootstrap site created the site cluster through the CWE API"

log "waiting for every node's spire-agent to attest"
make -C "${CLI_DIR}" nodes-attested

log "PASS: bootstrap site deployed SPIRE and every node attested to it over TPM"

# ── Stage 6: every pre-provisioned host is a Ready node ──────────────────
#
# Through the kubeconfig the command wrote, not through the bootstrap cluster:
# what is being checked is that an operator can now use the thing they asked
# for. Every site spec node joins as a control-plane node, and CWE untaints
# them, so all three must be Ready and schedulable.

# One thumbprint per node, init node included -- the same count this suite
# already asserted is non-zero before running the command.
expected_nodes=$(grep -cE '^[[:space:]]+tpmEkThumbprint:' "${RENDERED_SITESPEC}")

log "waiting for ${expected_nodes} nodes to report Ready"
# `kubectl wait --all` only waits for the objects that exist when it starts.
# Wait for the expected count and readiness together so that the first Ready
# control-plane node cannot make this stage pass before CAPI creates the rest.
# CWE joins control-plane nodes one at a time, ~6 min apart, so the third
# kubelet starts ~13 min after the cluster is created; 600 s left no margin.
node_ready_deadline=$((SECONDS + ${NKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-1200}))
ready_nodes=0
while [ "${SECONDS}" -lt "${node_ready_deadline}" ]; do
  node_states=$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
    -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
    2>/dev/null || true)
  total_nodes=$(printf '%s\n' "${node_states}" | awk 'NF {count++} END {print count+0}')
  ready_nodes=$(printf '%s\n' "${node_states}" | awk '$2 == "True" {count++} END {print count+0}')
  if [ "${total_nodes}" -eq "${expected_nodes}" ] && [ "${ready_nodes}" -eq "${expected_nodes}" ]; then
    break
  fi
  sleep 5
done

if [ "${ready_nodes}" -ne "${expected_nodes}" ]; then
  log "FAIL: ${ready_nodes} of ${expected_nodes} site spec nodes became Ready"
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide || true
  exit 1
fi
log "PASS: all ${ready_nodes} pre-provisioned hosts are Ready nodes"

# ── Stage 7: hello world ────────────────────────────────────────────────
#
# The point of the whole suite. Traefik is published on a NodePort because that
# is a route the host has to these containers; the request carries the Ingress
# host name so it has to match a rule Traefik read from the Ingress, rather
# than falling through to a default backend.

# `--repo` rather than `helm repo add`, for the same reason cli/internal/cwedeploy
# uses it: this suite does not mutate the caller's global Helm repository list.
# service.spec.type, not service.type: the chart puts the Service spec's own
# fields under `spec`, and a value set at the wrong depth is silently ignored.
# Which registries the nodes will reach. A pull that falls through to upstream
# is the difference between a 30 second install and a helm timeout.
log "node registry routing (${NKECTL_E2E_NAME_PREFIX:-node}1):"
nerdctl exec "nkectl-e2e-${NKECTL_E2E_NAME_PREFIX:-node}1" \
  cat /etc/containerd/certs.d/docker.io/hosts.toml 2>&1 | sed 's/^/  /' || true
# The bootstrap Kind cluster is still up (--keep-bootstrap-cluster above); its
# node pulls the CWE stack's images, so show where those went too.
log "bootstrap cluster registry routing (${BOOTSTRAP_KIND_CLUSTER}-control-plane):"
nerdctl exec "${BOOTSTRAP_KIND_CLUSTER}-control-plane" \
  cat /etc/containerd/certs.d/docker.io/hosts.toml 2>&1 | sed 's/^/  /' || true

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
served=false
for _ in $(seq 1 60); do
  # whoami answers with the request it received, so "Hostname:" in the body is
  # the pod talking, not Traefik's own error page.
  if curl -fsS --max-time 5 -H "Host: hello.nke.local" \
    "http://${INIT_NODE_IP}:${TRAEFIK_NODE_PORT}/" | grep -q "Hostname:"; then
    served=true
    break
  fi
  sleep 5
done
if [ "${served}" != true ]; then
  log "FAIL: hello world was never served through Traefik"
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n hello-world get all,ingress || true
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n traefik logs deployment/traefik --tail=100 || true
  exit 1
fi
log "PASS: the cluster built from pre-provisioned hosts serves hello world through Traefik"

# Positive control for the "cluster is gone" check below. kind exits 0 and
# prints "No kind clusters found." even when its provider is unreachable, so an
# empty answer there means either "deleted" or "cannot see anything" -- and the
# check cannot tell them apart. Confirming the cluster IS listed first is what
# makes its later absence evidence.
if ! kind_lists_bootstrap_cluster; then
  log "FAIL: ${BOOTSTRAP_KIND_CLUSTER} is not registered before teardown, so this"
  log "  suite cannot see the nerdctl provider and the check below proves nothing"
  exit 1
fi

log "tearing down the bootstrap kind cluster kept for attestation"
KIND_EXPERIMENTAL_PROVIDER=nerdctl \
  kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}"
bootstrap_cluster_cleanup_pending=false

# This checks the delete above, not the command's own cleanup: `bootstrap site`
# was given --keep-bootstrap-cluster, so it deliberately left the cluster
# running and this suite owns tearing it down. What it earns is the next run --
# a cluster left registered here makes stage 1 trip over it. That `bootstrap
# site` removes its own cluster when it is not told to keep one is checked in
# TestRunBootstrapSiteCreatesTheCluster, which can assert on the kind call
# itself rather than inferring it from what is left behind.
#
if kind_lists_bootstrap_cluster; then
  log "FAIL: kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after this suite deleted it"
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters || true
  exit 1
fi
log "PASS: this suite's teardown left no ${BOOTSTRAP_KIND_CLUSTER} cluster behind"
