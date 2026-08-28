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
#      change that silently skips the deploy is caught here.
#   3. The Kind cluster is confirmed gone afterwards. It is torn down by this
#      script rather than by the command, because stage 4 needs it kept alive.
#   4. TPM node attestation, on the docker provider only. The fake nodes boot
#      with a software TPM and a spire-agent already looking for the SPIRE
#      server. `bootstrap site` is given their EK thumbprints, in the
#      inventory, and deploys SPIRE with them allowlisted; the check is that
#      the agents got in. The nodes are Docker's whichever provider is under
#      test, so under nerdctl there is no network they and the kind cluster can
#      share -- see the stage itself for why that is not addressable.
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
NKECTL="${CLI_DIR}/bin/nkectl"
INVENTORY="${SCRIPT_DIR}/inventory.ci.yaml"
BOOTSTRAP_KIND_CLUSTER="${NKECTL_BOOTSTRAP_CLUSTER_NAME:-nkectl-bootstrap}"
# Inventory generation records the exact journal boundary and expected result
# for every node here. The later check reads the same file after bootstrap.
export NKECTL_E2E_ATTESTATION_BASELINE="${CLI_DIR}/.local/attestation-baseline.json"
# Where the node images' spire-agents were built to look for the SPIRE server.
SPIRE_VIP="${NKECTL_E2E_SPIRE_SERVER_ADDRESS:-172.26.0.2}"
export NKECTL_BOOTSTRAP_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER:-docker}"
: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already pulled locally (see comment above)}"
export NKECTL_CWE_IMAGE

# OS profiles, without which CWE fails its schema migration. Built from
# controller/test/fixtures rather than pulled: the fleet image lives in
# nke-site-deploy, which this suite does not depend on.
export NKECTL_CWE_PROFILES_IMAGE="${NKECTL_CWE_PROFILES_IMAGE:-cwe-profiles:local}"

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

# KUBECONFIG alone is not enough: helm caches API discovery under
# ~/.kube/cache and creates ~/.kube mode 0750 to do it, so under `sudo -E`
# that directory is root-owned and the normal user cannot write the lock file
# next to it either. `kubectl --raw` does not do this; helm does, on every
# install. KUBECACHEDIR moves that cache into the same scratch directory.
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"

log() { printf '[nkectl-bootstrap-e2e] %s\n' "$*"; }

nodes_up=false
bootstrap_cluster_cleanup_pending=false
cleanup() {
  # --keep-bootstrap-cluster means the command no longer cleans up after
  # itself, so a failure between it and the explicit teardown below would
  # otherwise leak a cluster that the next run's stage 3 then trips over.
  if [ "${bootstrap_cluster_cleanup_pending}" = true ]; then
    log "tearing down the kept bootstrap kind cluster"
    KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" \
      kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
  fi
  if [ "${nodes_up}" = true ]; then
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

  rm -rf "${KUBECONFIG_DIR}"
}
trap cleanup EXIT

# ── Stage 1: fake nodes ──────────────────────────────────────────────────

log "starting fake node containers"
# Set before the call, not after: `nodes-up` can fail after creating some
# containers (e.g. node3 fails to start while node1/node2 succeeded). Under
# `set -e` that failure exits the script immediately, so cleanup must already
# know to tear down whatever got created rather than skipping it because the
# call never "finished". `nodes-down` is safe to run against a partial or
# nonexistent fixture.
nodes_up=true
make -C "${CLI_DIR}" nodes-up

log "PASS: all fake nodes are up and node-agent is reachable"

# ── Stages 2-4: bootstrap, then TPM attestation ─────────────────────────
#
# The fake nodes booted with a software TPM and a spire-agent already pointed at
# SPIRE_VIP:30081 — the address `bootstrap site` puts its SPIRE server on. They
# have been retrying since. So this stage does not configure or start anything
# on them: it records their EK thumbprints in the inventory, runs the command,
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

# ensure-profiles-image always builds through `docker` (see cli/Makefile):
# `nerdctl build` needs a running buildkitd, which this suite does not stand
# up (a second daemon alongside the containerd-nerdctl.service the nerdctl
# CI leg already installs -- see nkectl-bootstrap-e2e.yml). Under the
# nerdctl provider, ensureImageAvailable in cmd/nkectl/bootstrap.go inspects
# and loads NKECTL_CWE_PROFILES_IMAGE through nerdctl, and a purely local
# image docker built is invisible to nerdctl's own, separate containerd
# instance -- so bridge it across once here, the same way `docker save` /
# `nerdctl load` move any other locally built image between two runtimes.
if [ "${NKECTL_BOOTSTRAP_PROVIDER}" = "nerdctl" ]; then
  log "loading ${NKECTL_CWE_PROFILES_IMAGE} into nerdctl's containerd (built via docker above)"
  docker save "${NKECTL_CWE_PROFILES_IMAGE}" | nerdctl load
fi

# The thumbprints and the bootstrap CA reach the command the way an operator's
# would: named in the inventory. Neither can be committed to the fixture — a
# TPM's endorsement key is created along with the container, and a private key
# does not belong in a repository — so both are filled in here against whatever
# this run generated.
log "recording each node's TPM EK thumbprint and the bootstrap CA in the inventory"
RENDERED_INVENTORY="${SCRIPT_DIR}/.inventory.rendered.yaml"
NKECTL_E2E_INVENTORY_IN="${INVENTORY}" NKECTL_E2E_INVENTORY_OUT="${RENDERED_INVENTORY}" \
  make -C "${CLI_DIR}" --no-print-directory nodes-inventory >/dev/null
grep -q tpmEkThumbprint "${RENDERED_INVENTORY}" ||
  { log "FAIL: no TPM EK thumbprints were recorded in ${RENDERED_INVENTORY}"; exit 1; }
grep -q caCertFile "${RENDERED_INVENTORY}" ||
  { log "FAIL: no bootstrap CA was recorded in ${RENDERED_INVENTORY}"; exit 1; }
log "allowlisting $(grep -c tpmEkThumbprint "${RENDERED_INVENTORY}") EK thumbprint(s)"

# No chart is named here. What gets installed is pinned in
# cli/internal/cwedeploy, which is the thing under test: the SPIFFE project's
# public hardened charts, needing no registry credential of their own.
log "running: nkectl bootstrap site -i ${RENDERED_INVENTORY} --keep-bootstrap-cluster" \
  "(NKECTL_BOOTSTRAP_PROVIDER=${NKECTL_BOOTSTRAP_PROVIDER}, NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}," \
  "NKECTL_CWE_PROFILES_IMAGE=${NKECTL_CWE_PROFILES_IMAGE})"
# Set before the call, not after: the command can fail after creating the
# cluster it was told to keep, and cleanup has to already know to remove it.
bootstrap_cluster_cleanup_pending=true
set +e
output="$("${NKECTL}" bootstrap site -i "${RENDERED_INVENTORY}" --keep-bootstrap-cluster 2>&1)"
status=$?
set -e
printf '%s\n' "${output}"

# Same contract as stage 2 — the command still stops at the dispatch stub.
if [ "${status}" -eq 0 ]; then
  log "FAIL: expected the bootstrap-dispatch stub to error, but the command exited 0"
  exit 1
fi
if ! grep -q "cwe deployment healthy" <<<"${output}"; then
  log "FAIL: CWE was never confirmed healthy on /readyz"
  exit 1
fi

# TPM attestation needs the agents and the SPIRE server to share a network, and
# that is only arrangeable when one runtime owns both ends.
#
# The fake hosts are Docker's, unconditionally: the substrate shells out to
# `docker` for every container, network and exec it does (see substrate.go), and
# the node image is built through docker on both legs for the reasons the
# profiles-image step above already explains. Under the nerdctl provider,
# though, `bootstrap site` builds its kind cluster in nerdctl's own containerd,
# which keeps its own networks. `docker network connect` cannot reach into that
# -- it fails with "No such container" -- and no amount of addressing fixes it,
# because the two ends are on different network stacks rather than different
# networks.
#
# So this stage is docker-only, and says so rather than pretending to have run.
# The nerdctl leg exists to prove bootstrapengine and cwedeploy drive that
# provider correctly, and it still does all of that above: kind cluster, image
# side-load, Postgres, Temporal, SPIRE, CWE healthy. Attestation is a property
# of the SPIRE deployment, not of the container runtime that hosted it, so
# proving it once on the docker leg is proving it.
if [ "${NKECTL_BOOTSTRAP_PROVIDER}" != "nerdctl" ]; then
  # The hosts and the bootstrap cluster are on different docker networks: the
  # nodes come up before the cluster exists, so there is nothing for them to
  # join. Attaching the cluster at the fixed address the agents were built to
  # look for stands in for the VIP a real site controller has.
  log "attaching the bootstrap cluster to the node network at ${SPIRE_VIP}"
  docker network connect --ip "${SPIRE_VIP}" \
    "${NKECTL_E2E_NETWORK:-nkectl-e2e-nodes}" "${BOOTSTRAP_KIND_CLUSTER}-control-plane"

  log "waiting for every node's spire-agent to attest"
  make -C "${CLI_DIR}" nodes-attested

  log "PASS: bootstrap site deployed SPIRE and every node attested to it over TPM"
else
  log "SKIP: TPM attestation is docker-only. The fake hosts are Docker containers"
  log "  and this leg's kind cluster lives in nerdctl's containerd, so there is no"
  log "  network both ends can share. Everything up to and including CWE reporting"
  log "  healthy has been checked above; the docker leg covers attestation."
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
# TestRunBootstrapSiteValidReachesStub, which can assert on the kind call
# itself rather than inferring it from what is left behind.
#
# kind resolves a cluster through its provider, so ask the same one bootstrap
# site used. Bare, the nerdctl leg asks Docker, sees none of nerdctl's
# clusters, and passes without checking anything. kind accepts "docker" here
# as well as its own default.
if KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters 2>/dev/null |
  grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"; then
  log "FAIL: kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after this suite deleted it"
  KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters || true
  exit 1
fi
log "PASS: this suite's teardown left no ${BOOTSTRAP_KIND_CLUSTER} cluster behind"
