#!/usr/bin/env bash
# Build / package the nke node-agent from the monorepo.
# Usage:
#   ./build/node-agent.sh              # build all arches
#   ./build/node-agent.sh test         # run unit tests
#   ./build/node-agent.sh package      # build + package release tarballs
#   ./build/node-agent.sh protos       # regenerate protobuf stubs
#   REGISTRY=ghcr.io/nvidia/nke ./build/node-agent.sh builder-image
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENT_DIR="${ROOT}/node-agent"
TARGET="${1:-build}"

cd "${AGENT_DIR}"

case "${TARGET}" in
  build|build-all|build-amd64|build-arm64|test|fmt|vet|lint|package|package-all|protos|proto-fmt|check-protos|clean)
    exec make "${TARGET}"
    ;;
  builder-image)
    # Optional override: REGISTRY=ghcr.io/nvidia/nke TAG=latest
    export BUILDER_IMAGE="${BUILDER_IMAGE:-${REGISTRY:-ghcr.io/nvidia/nke}/nke-builder}"
    export BUILDER_TAG="${TAG:-latest}"
    exec make push-builder-image
    ;;
  *)
    echo "unknown target: ${TARGET}" >&2
    echo "usage: $0 [build|test|package|protos|builder-image|...]" >&2
    exit 2
    ;;
esac
