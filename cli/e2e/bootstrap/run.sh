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
# NKECTL_BOOTSTRAP_PROVIDER selects the container runtime front end the
# ephemeral cluster runs on (see cli/internal/bootstrapengine).
# Defaults to "docker" here for a bare local run, since that is what a
# checkout has without any extra setup. The CI workflow
# (.github/workflows/nkectl-bootstrap-e2e.yml) now runs this script once per
# provider in a matrix and exports NKECTL_BOOTSTRAP_PROVIDER explicitly for
# each leg, exercising both docker and nerdctl end to end on the
# ubuntu-latest runner rather than relying on this default. See that
# workflow's own comments for how it stands up a standalone containerd for
# the nerdctl leg, and why that leg is advisory (continue-on-error) rather
# than required while it earns a track record. Override this var to try the
# nerdctl provider locally too, once nerdctl/containerd are set up on the
# host (see the workflow for the exact install this repo has verified).
#
# The fake nodes emulate a TPM with swtpm, which asks the kernel for a device
# through /dev/vtpmx. This script loads tpm_vtpm_proxy if it is not already
# there; a host that cannot load it cannot run this suite.
#
# NKECTL_CWE_IMAGE must already be pulled locally, through whichever runtime
# NKECTL_BOOTSTRAP_PROVIDER names (`docker pull`/`nerdctl pull`), before this
# script runs -- nkectl loads it into the kind cluster via `kind load
# docker-image` rather than pulling from inside the cluster (see
# cmd/nkectl/bootstrap.go's ensureImageAvailable), so it does not itself need
# registry credentials. The CI workflow logs in to GHCR and pulls the image
# through the matching CLI for each leg before invoking this script; a local
# run needs the same login + pull done by hand first.

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
export NKECTL_BOOTSTRAP_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER:-docker}"
: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already pulled locally (see comment above)}"
export NKECTL_CWE_IMAGE

# OS profiles, without which CWE fails its schema migration. Built from
# controller/test/fixtures rather than pulled: the fleet image lives in
# nke-site-deploy, which this suite does not depend on.
export NKECTL_CWE_PROFILES_IMAGE="${NKECTL_CWE_PROFILES_IMAGE:-cwe-profiles:local}"

# The Cluster API infrastructure provider for hosts that already exist, built
# below from this checkout. Without it the CAPI objects CWE writes have no
# controller and the cluster never leaves provisioning. nkectl loads it into the
# bootstrap cluster the same way it loads the CWE and profiles images, and finds
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
# nerdctl leg's CI job, which runs this whole script under `sudo -E`: without
# this, `kind create cluster` running as root would leave ~/.kube/config
# root-owned, and a later, unrelated cleanup step running as the normal user
# (helm/kind-action's own post-job step) would fail with "permission denied"
# trying to update it. Harmless for the docker leg too, so it is not
# provider-conditional.
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
# site used. Bare, the nerdctl leg asks Docker, sees none of nerdctl's clusters
# and answers "no" without checking anything. kind accepts "docker" here as
# well as its own default.
kind_lists_bootstrap_cluster() {
  KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters 2>/dev/null |
    grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"
}

nodes_up=false
bootstrap_cluster_cleanup_pending=false
spire_vip_device=""
nerdctl_bridge=""
nerdctl_node_access=false

collect_node_diagnostics() {
  local node_count="${NKECTL_E2E_NODES:-3}"
  local name_prefix="${NKECTL_E2E_NAME_PREFIX:-node}"
  local i node output

  mkdir -p "${DIAGNOSTICS_DIR}" || return 0
  log "collecting kubelet and containerd diagnostics from ${node_count} fake node(s)"
  for ((i = 1; i <= node_count; i++)); do
    node="nkectl-e2e-${name_prefix}${i}"
    output="${DIAGNOSTICS_DIR}/host-diagnostics-${name_prefix}${i}.txt"
    {
      printf '### journalctl: kubelet and containerd\n'
      docker exec "${node}" journalctl -u kubelet -u containerd \
        --boot --no-pager --output=short-iso-precise --lines=10000 || true

      printf '\n### systemctl status\n'
      docker exec "${node}" systemctl status kubelet containerd --no-pager || true

      printf '\n### crictl ps -a\n'
      docker exec "${node}" crictl ps -a || true

      printf '\n### crictl pods\n'
      docker exec "${node}" crictl pods || true

      printf '\n### crictl info\n'
      docker exec "${node}" crictl info || true

      printf '\n### ip addr\n'
      docker exec "${node}" ip addr || true

      printf '\n### ip route\n'
      docker exec "${node}" ip route || true

      printf '\n### CNI files\n'
      docker exec "${node}" ls -la /etc/cni/net.d /opt/cni/bin || true
    } >"${output}" 2>&1
  done
}

cleanup() {
  local status=$?
  if [ "${status}" -ne 0 ] && [ "${nodes_up}" = true ]; then
    collect_node_diagnostics
  fi

  # --keep-bootstrap-cluster means the command no longer cleans up after
  # itself, so a failure between it and the explicit teardown below would
  # otherwise leak a cluster that the next run's stage 3 then trips over.
  if [ "${bootstrap_cluster_cleanup_pending}" = true ]; then
    log "tearing down the kept bootstrap kind cluster"
    KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" \
      kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
  fi
  if [ "${nodes_up}" = true ]; then
    if [ "${nerdctl_node_access}" = true ]; then
      log "removing nerdctl access to the node network"
      iptables -t raw -D PREROUTING -i "${nerdctl_bridge}" \
        -d "${NKECTL_E2E_SUBNET:-172.26.0.0/24}" -j ACCEPT 2>/dev/null || true
    fi
    if [ -n "${spire_vip_device}" ]; then
      log "removing ${SPIRE_VIP} from ${spire_vip_device}"
      ip address del "${SPIRE_VIP}/32" dev "${spire_vip_device}" 2>/dev/null || true
    fi
    log "tearing down fake node containers"
    make -C "${CLI_DIR}" nodes-down || true
  fi

  # Same class of bug as the KUBECONFIG one above, and the same cause: under
  # `sudo -E` (the nerdctl leg) nkectl wrote this bundle as root. The CI step
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

log "building the profiles image (${NKECTL_CWE_PROFILES_IMAGE})"
make -C "${CLI_DIR}" ensure-profiles-image CWE_PROFILES_IMAGE="${NKECTL_CWE_PROFILES_IMAGE}"

log "building the provisioned CAPI provider image (${NKECTL_CAPI_PROVISIONED_IMAGE})"
make -C "${CLI_DIR}" ensure-provisioned-provider-image \
  CAPI_PROVISIONED_IMAGE="${NKECTL_CAPI_PROVISIONED_IMAGE}"

# The workflow and ensure-* targets build these images through `docker`:
# `nerdctl build` needs a running buildkitd, which this suite does not stand
# up (a second daemon alongside the containerd-nerdctl.service the nerdctl
# CI leg already installs -- see nkectl-bootstrap-e2e.yml). A local Docker
# image is invisible to nerdctl's separate containerd instance, so bridge all
# three images across once here.
if [ "${NKECTL_BOOTSTRAP_PROVIDER}" = "nerdctl" ]; then
  image_platform="$(go env GOOS)/$(go env GOARCH)"
  for image in "${NKECTL_CWE_IMAGE}" "${NKECTL_CWE_PROFILES_IMAGE}" "${NKECTL_CAPI_PROVISIONED_IMAGE}"; do
    log "loading ${image} for ${image_platform} into nerdctl's containerd (built via docker above)"
    docker save --platform="${image_platform}" "${image}" | nerdctl load
  done
fi

# The completed images are sufficient from this point. On GitHub's 14 GB
# runner, retaining their BuildKit intermediates competes with the container
# storage needed by the bootstrap cluster and three site nodes.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  log "removing Docker build cache before cluster provisioning"
  docker builder prune --all --force
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
  "(NKECTL_BOOTSTRAP_PROVIDER=${NKECTL_BOOTSTRAP_PROVIDER}, NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}," \
  "NKECTL_CWE_PROFILES_IMAGE=${NKECTL_CWE_PROFILES_IMAGE}," \
  "NKECTL_CAPI_PROVISIONED_IMAGE=${NKECTL_CAPI_PROVISIONED_IMAGE})"

# In the background, because the run has to be joined while it is still going.
#
# The command no longer stops after deploying CWE: it creates the cluster and
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
"${NKECTL}" bootstrap site -f "${RENDERED_SITESPEC}" --keep-bootstrap-cluster \
  --timeout 15m --kubeconfig-out "${TENANT_KUBECONFIG}" >"${BOOTSTRAP_LOG}" 2>&1 &
bootstrap_pid=$!

attach_bootstrap_to_node_network() {
  local node_network="${NKECTL_E2E_NETWORK:-nkectl-e2e-nodes}"
  if [ "${NKECTL_BOOTSTRAP_PROVIDER}" = "nerdctl" ]; then
    local network_id bridge nerdctl_network_id
    network_id="$(docker network inspect --format '{{.Id}}' "${node_network}")" || return 1
    bridge="$(docker network inspect --format '{{index .Options "com.docker.network.bridge.name"}}' "${node_network}")" || return 1
    if [ -z "${bridge}" ] || [ "${bridge}" = "<no value>" ]; then
      bridge="br-${network_id:0:12}"
    fi
    ip link show "${bridge}" >/dev/null || return 1
    nerdctl_network_id="$(nerdctl network inspect --format '{{.Id}}' kind)" || return 1
    nerdctl_bridge="br-${nerdctl_network_id:0:12}"
    ip link show "${nerdctl_bridge}" >/dev/null || return 1

    # Docker drops packets addressed directly to its containers when they
    # arrive from another bridge. Permit only this Kind bridge and node subnet,
    # so CWE can call the node agents while both runtimes remain isolated.
    iptables -t raw -I PREROUTING 1 -i "${nerdctl_bridge}" \
      -d "${NKECTL_E2E_SUBNET:-172.26.0.0/24}" -j ACCEPT || return 1
    nerdctl_node_access=true
    ip address replace "${SPIRE_VIP}/32" dev "${bridge}" || return 1
    spire_vip_device="${bridge}"
    return 0
  fi

  docker network connect --ip "${SPIRE_VIP}" \
    "${node_network}" "${BOOTSTRAP_KIND_CLUSTER}-control-plane"
}

# Attach as soon as Kind reports the bootstrap cluster ready. This is before CWE
# is deployed, so changing the container's networks cannot reset the CWE create
# request. It is also after Kind has finished configuring the control-plane
# container, so the additional network cannot affect cluster creation.
#
# Bounded separately from the command's own timeout, and abandoned if the
# command exits first. Docker can attach the control-plane container directly.
# nerdctl cannot connect a running container to another network, so its Kind
# host mapping receives the SPIRE VIP on the Docker node-network bridge.
log "waiting for the bootstrap cluster to become ready, to attach it to the node network at ${SPIRE_VIP}"
attached=false
attach_deadline=$((SECONDS + ${NKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${attach_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    if attach_bootstrap_to_node_network; then
      log "attached the bootstrap cluster at ${SPIRE_VIP}"
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
  kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  cat "${BOOTSTRAP_LOG}" || true
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
node_ready_deadline=$((SECONDS + ${NKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-600}))
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
  log "  suite cannot see the ${NKECTL_BOOTSTRAP_PROVIDER} provider and the check below proves nothing"
  exit 1
fi

log "tearing down the bootstrap kind cluster kept for attestation"
KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" \
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
  KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters || true
  exit 1
fi
log "PASS: this suite's teardown left no ${BOOTSTRAP_KIND_CLUSTER} cluster behind"
