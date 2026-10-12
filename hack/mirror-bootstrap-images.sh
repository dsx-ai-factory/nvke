#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Mirror the images `nvkectl bootstrap site` runs into a registry. The list is
# `nvkectl get bootstrap-manifest`. Each image keeps its upstream path under
# TARGET_REGISTRY, the layout NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR expects; that
# variable takes a TLS host[:port] with no path. Images from other registries
# are mirrored too, for a registry that will serve them later. Images with no
# registry host (the two this repository builds) are skipped: the bootstrap
# side-loads them.
#
# Usage: hack/mirror-bootstrap-images.sh [TARGET_REGISTRY]
#   TARGET_REGISTRY  host, with an optional path prefix. Default localhost:5000.
#
# Env:
#   NVKECTL=<path>    nvkectl to read the manifest from; default `nvkectl` on PATH,
#                    then `go run` from this checkout.
#   MANIFEST=<file>  read the manifest from a file instead.
#   DRY_RUN=1        print the copies without running crane (also true, yes).
#   INSECURE=1       plain HTTP / no TLS verification; default on for loopback.
#   CRANE=<path>     crane binary; default `crane` on PATH.
#
# Requires crane (go install github.com/google/go-containerregistry/cmd/crane@latest).
# crane preserves digests; a Docker pull/tag/push round-trip changes them, and
# the SPIRE chart pins one image by digest.

set -Eeuo pipefail

TARGET="${1:-localhost:5000}"
CRANE="${CRANE:-crane}"
case "${DRY_RUN:-0}" in 1|true|yes) DRY_RUN=1 ;; *) DRY_RUN=0 ;; esac
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "${DRY_RUN}" != "1" ]; then
  command -v "$CRANE" >/dev/null 2>&1 || {
    echo "error: crane not found on PATH (set CRANE=/path/to/crane, or" >&2
    echo "       go install github.com/google/go-containerregistry/cmd/crane@latest)" >&2
    exit 1
  }
fi

# Default to insecure for loopback targets, where a throwaway registry is
# usually plain HTTP; a real mirror over HTTPS leaves INSECURE unset.
if [ -z "${INSECURE:-}" ]; then
  case "$TARGET" in
    localhost:*|localhost/*|127.0.0.1:*|127.0.0.1/*) INSECURE=1 ;;
    *) INSECURE=0 ;;
  esac
fi
crane_flags=()
[ "${INSECURE}" = "1" ] && crane_flags+=(--insecure)

# read_manifest prints the bootstrap manifest.
read_manifest() {
  if [ -n "${MANIFEST:-}" ]; then
    cat "${MANIFEST}"
  elif [ -n "${NVKECTL:-}" ]; then
    "${NVKECTL}" get bootstrap-manifest
  elif command -v nvkectl >/dev/null 2>&1; then
    nvkectl get bootstrap-manifest
  else
    (cd "${REPO_ROOT}/cli" && go run ./cmd/nvkectl get bootstrap-manifest)
  fi
}

# Print "ref digest" per image. An image with no digest prints "ref -".
# Charts carry no `ref:` line in the embedded manifest.
list_images() {
  awk '
    function flush() { if (ref != "") { print ref, (digest == "" ? "-" : digest); ref = ""; digest = "" } }
    /^[[:space:]]*ref:[[:space:]]*/    { flush(); ref = $2; next }
    /^[[:space:]]*digest:[[:space:]]*/ { digest = $2; next }
    END { flush() }
  '
}

# has_host reports whether an image reference names a registry host: the
# first path component contains a dot or a colon, is localhost, or has an
# upper-case letter (repository names are lower case). Same rule as nvkectl.
has_host() {
  case "$1" in
    */*) first="${1%%/*}"; case "$first" in *.*|*:*|localhost|*[A-Z]*) return 0 ;; esac ;;
  esac
  return 1
}

manifest="$(read_manifest)" || { echo "error: reading the bootstrap manifest failed" >&2; exit 1; }
entries=()
while IFS= read -r line; do entries+=("$line"); done < <(list_images <<<"$manifest")
[ "${#entries[@]}" -gt 0 ] || { echo "error: the manifest lists no images" >&2; exit 1; }

echo "Mirroring bootstrap images to ${TARGET} (insecure=${INSECURE}, dry_run=${DRY_RUN})"
copied=0
failed=0
for entry in "${entries[@]}"; do
  ref="${entry% *}"
  digest="${entry##* }"
  if ! has_host "$ref"; then
    echo "--- ${ref}: no registry host; the bootstrap side-loads it. Skipped."
    continue
  fi
  src="$ref"
  [ "$digest" != "-" ] && src="${ref}@${digest}"
  dst="${TARGET}/${ref#*/}"
  echo ">>> ${src}"
  echo "    -> ${dst}"
  if [ "${DRY_RUN}" = "1" ]; then
    copied=$((copied + 1))
    continue
  fi
  if "$CRANE" copy ${crane_flags[@]+"${crane_flags[@]}"} "${src}" "${dst}"; then
    copied=$((copied + 1))
  else
    echo "    FAILED" >&2
    failed=1
  fi
done

if [ "${failed}" -ne 0 ]; then
  echo "one or more images failed to mirror" >&2
  exit 1
fi
if [ "${DRY_RUN}" = "1" ]; then
  echo "done: ${copied} copies planned to ${TARGET}"
else
  echo "done: ${copied} images mirrored to ${TARGET}"
fi
