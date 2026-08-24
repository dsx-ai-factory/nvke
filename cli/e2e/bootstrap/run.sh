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
# ephemeral cluster runs on (see cli/internal/bootstrapengine and NKX-14787).
# Defaults to "docker" here specifically: that is the path this suite has
# actually verified end to end (including on the ubuntu-latest CI runner);
# nerdctl is the package's default everywhere else and is exercised by the
# package's own unit tests, but rootless/cgroup-v2 provisioning for a real
# nerdctl+containerd runtime on the CI runner is tracked as a NKX-14787
# follow-up, not yet validated here. Override to try it locally.
#
# NKECTL_CWE_IMAGE must already be pulled locally (`docker pull`) before this
# script runs -- nkectl loads it into the kind cluster via `kind load
# docker-image` rather than pulling from inside the cluster (see
# cmd/nkectl/bootstrap.go's ensureImageAvailable), so it does not itself need
# registry credentials. The CI workflow logs in to nvcr.io and pulls the
# image published by .github/workflows/image-nvcr.yml before invoking this
# script; a local run needs the same `docker login nvcr.io` + `docker pull`
# done by hand first.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
NKECTL="${CLI_DIR}/bin/nkectl"
INVENTORY="${SCRIPT_DIR}/inventory.ci.yaml"
BOOTSTRAP_KIND_CLUSTER="${NKECTL_BOOTSTRAP_CLUSTER_NAME:-nkectl-bootstrap}"
export NKECTL_BOOTSTRAP_PROVIDER="${NKECTL_BOOTSTRAP_PROVIDER:-docker}"
: "${NKECTL_CWE_IMAGE:?NKECTL_CWE_IMAGE must name the CWE image to deploy, already pulled locally (see comment above)}"
export NKECTL_CWE_IMAGE

log() { printf '[nkectl-bootstrap-e2e] %s\n' "$*"; }

nodes_up=false
cleanup() {
  if [ "${nodes_up}" = true ]; then
    log "tearing down fake node containers"
    make -C "${CLI_DIR}" nodes-down || true
  fi
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

log "running: nkectl bootstrap site -i ${INVENTORY}" \
  "(NKECTL_BOOTSTRAP_PROVIDER=${NKECTL_BOOTSTRAP_PROVIDER}, NKECTL_CWE_IMAGE=${NKECTL_CWE_IMAGE})"
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
if kind get clusters 2>/dev/null | grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"; then
  log "FAIL: kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after bootstrap site exited"
  kind get clusters || true
  exit 1
fi

log "PASS: ephemeral kind cluster ${BOOTSTRAP_KIND_CLUSTER} is gone after the command exited"
