#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the Linux-only image-build steps (package builds, installer ISO patching)
# in a container, for hosts that cannot run them natively: an Apple-silicon Mac
# without Nix. Packer and QEMU stay on the host, under HVF.
#
#   builder-container.sh check            podman answers; image built if missing
#   builder-container.sh run CMD [ARG...]  run CMD in the container, here
#
# The checkout is mounted at its own absolute path and the command runs in the
# host's current directory, so every path Make records (the artifacts staging
# directory, the ISO cache, the material manifest's ISO link) means the same
# thing on both sides. Reads ARCH (amd64|arm64) and SOURCE_ROOT (default: this
# checkout).
#
# podman only: cni-ovn builds its image from source, and inside the container
# it does so through the podman machine's own service (OCI_BIN=podman with
# CONTAINER_HOST), with the static podman-remote client the image carries.

set -euo pipefail

die() { echo "builder-container: $*" >&2; exit 1; }

arch=${ARCH:?ARCH is required}
case "$arch" in amd64|arm64) ;; *) die "ARCH must be amd64 or arm64, not $arch" ;; esac

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
source_root=${SOURCE_ROOT:-$(cd "$script_dir/../../.." && pwd -P)}
builder_dir="$source_root/os-image/packer/builder"
containerfile="$builder_dir/Containerfile"
[[ -f "$containerfile" ]] || die "no Containerfile at $containerfile"

command -v podman >/dev/null || die "podman is required: install it and start its machine (see os-image/README.md)"

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
  podman info >/dev/null 2>&1 || die "podman is not answering; start its VM with: podman machine start"
  if ! podman image exists "$image" >/dev/null 2>&1; then
    echo "building $image ($platform) from $containerfile"
    podman build --platform "$platform" -t "$image" -f "$containerfile" "$builder_dir"
  fi
}

run_in_container() {
  (($# > 0)) || die "run needs a command"
  local cache="${XDG_CACHE_HOME:-$HOME/.cache}/nvke/os-image-builder/$arch"
  mkdir -p "$cache"
  local cwd socket
  cwd=$(pwd -P)
  # The service socket as the machine sees it, where the container runs.
  socket=$(podman info --format '{{.Host.RemoteSocket.Path}}') || die "podman info did not report its service socket"
  socket=${socket#unix://}
  [[ -n "$socket" ]] || die "podman reported no service socket"
  local args=(run --rm --platform "$platform"
    -v "$source_root:$source_root"
    -v "$cache:/var/cache/nvke-builder"
    -w "$cwd"
    -e "GOMODCACHE=/var/cache/nvke-builder/gomod"
    -e "GOCACHE=/var/cache/nvke-builder/gobuild"
    -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e "GIT_CONFIG_VALUE_0=*"
    # The machine's SELinux policy would deny the container its runtime socket.
    --security-opt label=disable
    -v "$socket:/run/podman/podman.sock"
    -e CONTAINER_HOST=unix:///run/podman/podman.sock
    -e OCI_BIN=podman)
  # A linked worktree keeps its git metadata in the main checkout; the agent's
  # version stamp (git describe) needs it inside the container too.
  local common
  if common=$(git -C "$source_root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) &&
     [[ "$common" != "$source_root" && "$common" != "$source_root"/* ]]; then
    args+=(-v "$common:$common")
  fi
  # Package scratch (each package's dist/) goes on a Linux-native volume, one
  # per architecture, package and checkout. On a mount backed by the macOS
  # filesystem, container root is the host user and gets no override for a
  # mode-0 file, which is how GNU tar stages a symlink whose target contains
  # `..` (cni-ovn's source archive has several). Outputs go to OUT_DIR, which
  # stays on the shared mount.
  local checkout_id package
  checkout_id=$(printf '%s' "$source_root" | { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi; })
  checkout_id=${checkout_id:0:12}
  for package in "$source_root"/os-image/packages/*/; do
    [[ -f "${package}Makefile" ]] || continue
    package=${package%/}
    args+=(-v "nvke-os-image-builder-$arch-${package##*/}-$checkout_id:$package/dist")
  done
  local name
  for name in ARCH NKE_TARGET_UBUNTU_SERIES ARTIFACTS_DIR SOURCE_ROOT NKE_BUILD_IN_PLACE \
    K8S_VERSIONS CALICO_VERSIONS OVN_STREAMS SPIRE_VERSION TPM_PLUGIN_VERSION DOCA_VERSION \
    CNI_DOCKERHUB_MIRROR ISO_URL ISO_CHECKSUM OUT_DIR; do
    if [[ -n "${!name+x}" ]]; then
      args+=(-e "$name=${!name}")
    fi
  done
  podman "${args[@]}" "$image" "$@"
}

case "${1:-}" in
  check) ensure_image ;;
  run) shift; ensure_image; run_in_container "$@" ;;
  *) die "usage: builder-container.sh check | run CMD [ARG...]" ;;
esac
