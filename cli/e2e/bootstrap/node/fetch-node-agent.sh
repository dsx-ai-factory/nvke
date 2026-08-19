#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Fetch a published nke-agent binary from this repo's own GHCR artifact feed
# (see .github/workflows/agent-artifact.yml) into e2e/bootstrap/node/nke-agent.
#
# Unlike the GitLab-side nkectl spike this was ported from, this needs no
# separate personal-access-token flow: nke-agent is already published to
# ghcr.io/nvidia/nke/agent as a multi-arch OCI artifact, pulled with `oras`.
#
# Inputs (env):
#   NODE_AGENT_TAG   Image tag to pull, e.g. "edge" (tracks main) or a
#                     released "vX.Y.Z-rcN" (see `git tag -l 'agent/v*'`).
#                     Defaults to "edge".
#   NODE_AGENT_ARCH   Defaults to the host's Go-style arch (amd64/arm64).
#
# Auth: `oras` must already be logged in to ghcr.io. This repository is
# GitHub-internal, so an anonymous pull will 401 even though the artifact
# exists. In CI, log in with the workflow's own GITHUB_TOKEN (packages: read
# is enough — see nkectl-bootstrap-e2e.yml). Locally, `echo $(gh auth token) |
# oras login ghcr.io -u <your-gh-username> --password-stdin` before running
# this script.
#
# Idempotent against node/nke-agent.version.

set -euo pipefail

log() { printf '[fetch-node-agent] %s\n' "$*"; }
err() { printf '[fetch-node-agent] ERROR: %s\n' "$*" >&2; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TAG="${NODE_AGENT_TAG:-edge}"
ARCH="${NODE_AGENT_ARCH:-$(go env GOARCH 2>/dev/null || uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
REPO="ghcr.io/nvidia/nke/agent"

BIN_PATH="${SCRIPT_DIR}/nke-agent"
SIDE_CAR="${SCRIPT_DIR}/nke-agent.version"
CURRENT_TAG="${TAG}-${ARCH}"

if [[ -x "${BIN_PATH}" && -f "${SIDE_CAR}" ]]; then
  if [[ "$(cat "${SIDE_CAR}" 2>/dev/null || true)" == "${CURRENT_TAG}" ]]; then
    log "nke-agent ${CURRENT_TAG} already installed at ${BIN_PATH} — skipping."
    exit 0
  fi
fi

if ! command -v oras >/dev/null 2>&1; then
  err "oras is required to fetch nke-agent from ${REPO}. See https://oras.land/docs/installation"
  exit 1
fi

log "pulling ${REPO}:${TAG} (platform linux/${ARCH})"
if ! oras pull "${REPO}:${TAG}" --platform "linux/${ARCH}" -o "${SCRIPT_DIR}"; then
  err "pull failed. If this is a 401/403, log in first:"
  err "  echo \$(gh auth token) | oras login ghcr.io -u <your-gh-username> --password-stdin"
  exit 1
fi

if [[ ! -f "${BIN_PATH}" ]]; then
  err "oras pull succeeded but ${BIN_PATH} was not written — check the artifact's layer title"
  exit 1
fi

chmod +x "${BIN_PATH}"
printf '%s\n' "${CURRENT_TAG}" > "${SIDE_CAR}"
log "installed nke-agent ${CURRENT_TAG} -> ${BIN_PATH}"
