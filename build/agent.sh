#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Build / package the NVKE agent from the monorepo.
#
# Versioning (Kubernetes/Go monorepo style):
#   Release tags are namespaced: agent/vX.Y.Z
#   make derives VERSION via: git describe --match 'agent/v*'
#   Override with VERSION=vX.Y.Z when needed.
#
# Consumer config:
#   cp agent/config.env.example agent/config.env
#   Edit endpoints/registries, then run this script (config.env is sourced).
#
# Usage:
#   ./build/agent.sh              # build all arches
#   ./build/agent.sh test         # run unit tests
#   ./build/agent.sh package      # build + package release tarballs
#   ./build/agent.sh protos       # regenerate protobuf stubs
#   REGISTRY=ghcr.io/my-org/nvke ./build/agent.sh builder-image
#   VERSION=v0.1.30 ./build/agent.sh package
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENT_DIR="${ROOT}/agent"
TARGET="${1:-build}"

if [[ -f "${AGENT_DIR}/config.env" ]]; then
  # shellcheck disable=SC1091
  set -a
  # shellcheck disable=SC1091
  source "${AGENT_DIR}/config.env"
  set +a
fi

cd "${AGENT_DIR}"

case "${TARGET}" in
  build|build-all|build-amd64|build-arm64|test|fmt|vet|lint|package|package-all|protos|proto-fmt|check-protos|clean)
    exec make "${TARGET}"
    ;;
  builder-image)
    if [[ -n "${NKE_CONTAINER_REGISTRY:-}" ]]; then
      export BUILDER_IMAGE="${BUILDER_IMAGE:-${NKE_CONTAINER_REGISTRY}/${NKE_BUILDER_IMAGE_NAME:-nke-builder}}"
    else
      export BUILDER_IMAGE="${BUILDER_IMAGE:-${REGISTRY:-ghcr.io/example/nvke}/nke-builder}"
    fi
    export BUILDER_TAG="${BUILDER_TAG:-${TAG:-${NKE_BUILDER_IMAGE_TAG:-latest}}}"
    exec make push-builder-image
    ;;
  *)
    echo "unknown target: ${TARGET}" >&2
    echo "usage: $0 [build|test|package|protos|builder-image|...]" >&2
    exit 2
    ;;
esac
