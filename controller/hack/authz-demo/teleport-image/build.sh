#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build the demo's Teleport image, nvke-demo/teleport:<version>-agpl, from the
# AGPL source in a golang container. No web UI (WEBASSETS_SKIP_BUILD) and no
# desktop access (RDPCLIENT_SKIP_BUILD). The clone and Go cache live in docker
# volumes teleport-src and teleport-gocache, so a rebuild reuses them;
# docker volume rm them when done.
#
# Usage: hack/authz-demo/teleport-image/build.sh
# Environment: TAG (default v18.11.1), GO_IMAGE (default golang:1.25.14-bookworm,
# the go directive of that tag's go.mod).

set -euo pipefail

TAG="${TAG:-v18.11.1}"
GO_IMAGE="${GO_IMAGE:-golang:1.25.14-bookworm}"
IMAGE="${IMAGE:-nvke-demo/teleport:${TAG#v}-agpl}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$(uname -m)" in
  arm64 | aarch64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac

docker volume create teleport-src >/dev/null
docker volume create teleport-gocache >/dev/null

# Colima does not mount every host path, so the build uses volumes and docker cp.
docker run --rm --platform "linux/${ARCH}" \
  -v teleport-src:/src -v teleport-gocache:/root/.cache \
  -e TAG="${TAG}" -e ARCH="${ARCH}" -w /src "${GO_IMAGE}" bash -euo pipefail -c '
    if [ ! -d "teleport-${TAG}/.git" ]; then
      git clone -q --depth 1 --branch "${TAG}" https://github.com/gravitational/teleport "teleport-${TAG}"
    fi
    cd "teleport-${TAG}"
    export GOFLAGS=-buildvcs=false GOTOOLCHAIN=local
    make build/teleport build/tctl build/tsh WEBASSETS_SKIP_BUILD=1 RDPCLIENT_SKIP_BUILD=1 OS=linux ARCH="${ARCH}"
  '

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin"
cid="$(docker create -v teleport-src:/src alpine true)"
for b in teleport tctl tsh; do docker cp "${cid}:/src/teleport-${TAG}/build/${b}" "${work}/bin/${b}"; done
docker rm "${cid}" >/dev/null
cp "${HERE}/Dockerfile" "${work}/Dockerfile"
docker build --platform "linux/${ARCH}" -t "${IMAGE}" "${work}"
echo "${IMAGE}"
