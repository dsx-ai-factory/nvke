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
#      that change actually reaching it.
#   2. Inventory load + validation only. `bootstrap site` stops at its
#      not-yet-implemented dispatch stub, which this script asserts by name
#      so a change that silently starts dispatching (or stops validating) is
#      caught here rather than downstream. Nothing dials the nodes yet.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
NKECTL="${CLI_DIR}/bin/nkectl"
INVENTORY="${SCRIPT_DIR}/inventory.ci.yaml"
# Matches inventory.ci.yaml's nodeAgentPort override — see the note there on
# why this isn't inventory.DefaultNodeAgentPort ("50051").
NODE_AGENT_PORT=15005
NODE_IPS=(172.26.0.10 172.26.0.11 172.26.0.12)

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
make -C "${CLI_DIR}" nodes-up
nodes_up=true

log "waiting for node-agent to answer on ${NODE_IPS[*]}:${NODE_AGENT_PORT}"
deadline=$((SECONDS + 120))
for ip in "${NODE_IPS[@]}"; do
  until (exec 3<>"/dev/tcp/${ip}/${NODE_AGENT_PORT}") 2>/dev/null; do
    exec 3>&- 2>/dev/null || true
    if [ "${SECONDS}" -ge "${deadline}" ]; then
      log "FAIL: node-agent never answered on ${ip}:${NODE_AGENT_PORT}"
      exit 1
    fi
    sleep 2
  done
  exec 3>&- 2>/dev/null || true
  log "node-agent reachable on ${ip}:${NODE_AGENT_PORT}"
done

log "PASS: all fake nodes are up and node-agent is reachable"

# ── Stage 2: bootstrap-dispatch stub ─────────────────────────────────────

log "running: nkectl bootstrap site -i ${INVENTORY}"
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

log "PASS: inventory loaded and validated; reached the bootstrap-dispatch stub"
