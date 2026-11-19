#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the Linux-only image-build steps (package builds, installer ISO patching)
# in a container, for hosts that cannot run them natively: an Apple-silicon Mac
# without Nix. Packer and QEMU stay on the host, under HVF.
#
#   builder-container.sh check            runtime answers; image built if missing
#   builder-container.sh run CMD [ARG...]  run CMD in the container, here
#
# The checkout is mounted at its own absolute path and the command runs in the
# host's current directory, so every path Make records (the artifacts staging
# directory, the ISO cache, the material manifest's ISO link) means the same
# thing on both sides. Reads ARCH (amd64|arm64), CONTAINER_RUNTIME (podman or
# docker; default podman, then docker) and SOURCE_ROOT (default: this checkout).

set -euo pipefail

die() { echo "builder-container: $*" >&2; exit 1; }

arch=${ARCH:?ARCH is required}
case "$arch" in amd64|arm64) ;; *) die "ARCH must be amd64 or arm64, not $arch" ;; esac

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
source_root=${SOURCE_ROOT:-$(cd "$script_dir/../../.." && pwd -P)}
builder_dir="$source_root/os-image/packer/builder"
containerfile="$builder_dir/Containerfile"
[[ -f "$containerfile" ]] || die "no Containerfile at $containerfile"

if [[ -n "${CONTAINER_RUNTIME:-}" ]]; then
  case "$CONTAINER_RUNTIME" in
    podman|docker) runtime=$CONTAINER_RUNTIME ;;
    *) die "CONTAINER_RUNTIME must be podman or docker, not $CONTAINER_RUNTIME" ;;
  esac
  command -v "$runtime" >/dev/null || die "CONTAINER_RUNTIME=$runtime is not on PATH"
elif command -v podman >/dev/null; then
  runtime=podman
elif command -v docker >/dev/null; then
  runtime=docker
else
  die "a container runtime is required: install podman or docker (see os-image/README.md)"
fi

sha256_of() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}
# The tag follows the Containerfile's bytes, so an edited recipe builds a new
# image instead of reusing a stale one, and the architecture, so an arm64
# image is never run for an amd64 build.
digest=$(sha256_of "$containerfile")
image="localhost/nvke-os-image-builder:${digest:0:12}-$arch"
platform="linux/$arch"

ensure_image() {
  if ! "$runtime" info >/dev/null 2>&1; then
    if [[ "$runtime" == podman ]]; then
      die "podman is not answering; start its VM with: podman machine start"
    fi
    die "docker is not answering; start Docker Desktop or the docker daemon"
  fi
  local exists=(image inspect "$image")
  [[ "$runtime" == podman ]] && exists=(image exists "$image")
  if ! "$runtime" "${exists[@]}" >/dev/null 2>&1; then
    echo "building $image ($platform) from $containerfile"
    "$runtime" build --platform "$platform" -t "$image" -f "$containerfile" "$builder_dir"
  fi
}

run_in_container() {
  (($# > 0)) || die "run needs a command"
  local cache="${XDG_CACHE_HOME:-$HOME/.cache}/nvke/os-image-builder/$arch"
  mkdir -p "$cache"
  local cwd
  cwd=$(pwd -P)
  local args=(run --rm --platform "$platform"
    -v "$source_root:$source_root"
    -v "$cache:/var/cache/nvke-builder"
    -w "$cwd"
    -e "GOMODCACHE=/var/cache/nvke-builder/gomod"
    -e "GOCACHE=/var/cache/nvke-builder/gobuild"
    -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e "GIT_CONFIG_VALUE_0=*")
  # A linked worktree keeps its git metadata in the main checkout; the agent's
  # version stamp (git describe) needs it inside the container too.
  local common
  if common=$(git -C "$source_root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) &&
     [[ "$common" != "$source_root" && "$common" != "$source_root"/* ]]; then
    args+=(-v "$common:$common")
  fi
  local name
  for name in ARCH NKE_TARGET_UBUNTU_SERIES ARTIFACTS_DIR SOURCE_ROOT NKE_BUILD_IN_PLACE \
    K8S_VERSIONS CALICO_VERSIONS OVN_STREAMS SPIRE_VERSION TPM_PLUGIN_VERSION DOCA_VERSION \
    CNI_DOCKERHUB_MIRROR ISO_URL ISO_CHECKSUM OUT_DIR; do
    if [[ -n "${!name+x}" ]]; then
      args+=(-e "$name=${!name}")
    fi
  done
  "$runtime" "${args[@]}" "$image" "$@"
}

case "${1:-}" in
  check) ensure_image ;;
  run) shift; ensure_image; run_in_container "$@" ;;
  *) die "usage: builder-container.sh check | run CMD [ARG...]" ;;
esac
