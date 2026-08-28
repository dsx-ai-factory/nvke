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
#      (cli/internal/bootstrapengine), deploys Postgres, Temporal and CWE onto
#      it (cli/internal/cwedeploy) and waits for CWE to report healthy on
#      /readyz, then stops at its not-yet-implemented workflow-dispatch stub.
#      This script asserts both the CWE-healthy line and the stub error by
#      name, so a change that silently skips the deploy (or stops validating
#      it) is caught here rather than downstream. Nothing dials the fake
#      nodes yet -- that is Track E, wiring `bootstrap site` to actually
#      dispatch a workflow against the now-healthy CWE.
#   3. The ephemeral Kind cluster is confirmed gone after the command exits —
#      `bootstrap site` tears it down unconditionally on every exit path,
#      including the dispatch-stub error path stage 2 asserts on.
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
cleanup() {
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

# ── Stage 2: CWE deploys and reports healthy, then the dispatch stub ────

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

log "running: nkectl bootstrap site -i ${INVENTORY}" \
  "(NKECTL_BOOTSTRAP_PROVIDER=${NKECTL_BOOTSTRAP_PROVIDER}, NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE}," \
  "NKECTL_CWE_PROFILES_IMAGE=${NKECTL_CWE_PROFILES_IMAGE})"
set +e
output="$("${NKECTL}" bootstrap site -i "${INVENTORY}" 2>&1)"
status=$?
set -e
printf '%s\n' "${output}"

if [ "${status}" -eq 0 ]; then
  log "FAIL: expected the bootstrap-dispatch stub to error, but the command exited 0"
  exit 1
fi
if ! grep -q "not yet implemented" <<<"${output}"; then
  log "FAIL: command failed, but not with the expected dispatch stub error"
  exit 1
fi
if ! grep -q "bootstrap kind cluster ready" <<<"${output}"; then
  log "FAIL: command reached the dispatch stub, but never reported the bootstrap kind cluster ready"
  exit 1
fi
if ! grep -q "cwe deployment healthy" <<<"${output}"; then
  log "FAIL: command reached the dispatch stub, but CWE was never confirmed healthy on /readyz"
  exit 1
fi

log "PASS: inventory loaded and validated; ephemeral kind cluster came up; CWE deployed and reported healthy; reached the bootstrap-dispatch stub"

# ── Stage 3: ephemeral kind cluster is gone after ───────────────────────

log "confirming the ephemeral bootstrap kind cluster (${BOOTSTRAP_KIND_CLUSTER}) was torn down"
if ! grep -q "tearing down bootstrap kind cluster" <<<"${output}"; then
  log "FAIL: command never logged tearing down the bootstrap kind cluster"
  exit 1
fi
# kind resolves a cluster through its provider, so ask the same one bootstrap
# site used. Bare, the nerdctl leg asks Docker, sees none of nerdctl's
# clusters, and passes without checking anything. kind accepts "docker" here
# as well as its own default.
if KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters 2>/dev/null |
  grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"; then
  log "FAIL: kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after bootstrap site exited"
  KIND_EXPERIMENTAL_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER}" kind get clusters || true
  exit 1
fi

log "PASS: ephemeral kind cluster ${BOOTSTRAP_KIND_CLUSTER} is gone after the command exited"
