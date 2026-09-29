#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Mirror the container images `nkectl bootstrap site` runs into a local or
# private registry. The list comes from `nkectl get bootstrap-manifest`, so it
# is the list this nkectl was built with. Each image keeps its upstream path
# under TARGET_REGISTRY: docker.io/temporalio/server becomes
# TARGET_REGISTRY/temporalio/server.
#
# That layout is what NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR expects, so the
# bootstrap pulls the Docker Hub images from the mirror when that variable
# names TARGET_REGISTRY. The bootstrap has no such redirect for the other
# registries (ghcr.io, registry.k8s.io, cgr.dev) yet; their images are mirrored
# too, for a registry that will serve them later.
#
# Images with no registry host (the two this repository builds) are skipped.
# The bootstrap side-loads those from the local image store.
#
# Usage:
#   hack/mirror-bootstrap-images.sh [TARGET_REGISTRY]
#
#   TARGET_REGISTRY  registry host, with an optional path prefix, to mirror into.
#                    Defaults to localhost:5000. Examples: localhost:5000,
#                    10.0.0.1:5000, nvcr.io/myorg.
#
# Env:
#   NKECTL=<path>    nkectl binary to read the manifest from. Defaults to
#                    `nkectl` on PATH, then `go run` from this checkout.
#   MANIFEST=<file>  read the manifest from a file instead of nkectl.
#   DRY_RUN=1        print the copies without running crane.
#   INSECURE=1       copy over plain HTTP / skip TLS verification. Defaults on
#                    for localhost / 127.0.0.1 targets, off otherwise.
#   CRANE=<path>     crane binary to use. Defaults to `crane` on PATH.
#
# Requires `crane` (github.com/google/go-containerregistry/cmd/crane):
#   go install github.com/google/go-containerregistry/cmd/crane@latest
#
# crane copies the manifest bytes as-is and preserves digests. A Docker
# pull/tag/push round-trip re-serializes the manifest and changes the digest,
# and the SPIRE chart pins one image by digest, so that pull would then fail.

set -Eeuo pipefail

TARGET="${1:-localhost:5000}"
CRANE="${CRANE:-crane}"
DRY_RUN="${DRY_RUN:-0}"
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
  elif [ -n "${NKECTL:-}" ]; then
    "${NKECTL}" get bootstrap-manifest
  elif command -v nkectl >/dev/null 2>&1; then
    nkectl get bootstrap-manifest
  else
    (cd "${REPO_ROOT}/cli" && go run ./cmd/nkectl get bootstrap-manifest)
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
# first path component contains a dot or a colon, or is localhost.
has_host() {
  case "$1" in
    */*) first="${1%%/*}"; case "$first" in *.*|*:*|localhost) return 0 ;; esac ;;
  esac
  return 1
}

mapfile -t entries < <(read_manifest | list_images)
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
  if "$CRANE" copy "${crane_flags[@]}" "${src}" "${dst}"; then
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
echo "done: ${copied} images mirrored to ${TARGET}"
