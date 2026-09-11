#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Mirror the third-party container images the `nkectl bootstrap site` Helm
# releases pull -- Postgres, Temporal, and the SPIRE stack -- into a local or
# private registry, so a bootstrap can pull them offline. Point the bootstrap at
# the mirror by setting registry.imageRegistry (REGISTRY_IMAGE_REGISTRY) to the
# same registry; each image keeps its upstream path under it, which is what that
# override rewrites references to.
#
# Usage:
#   hack/mirror-bootstrap-images.sh [TARGET_REGISTRY]
#
#   TARGET_REGISTRY  registry host, with an optional path prefix, to mirror into.
#                    Defaults to localhost:5000. Examples: localhost:5000,
#                    10.0.0.1:5000, nvcr.io/myorg.
#
# Env:
#   INSECURE=1       copy over plain HTTP / skip TLS verification. Defaults on
#                    for localhost / 127.0.0.1 targets, off otherwise.
#   CRANE=<path>     crane binary to use. Defaults to `crane` on PATH.
#
# Requires `crane` (github.com/google/go-containerregistry/cmd/crane):
#   go install github.com/google/go-containerregistry/cmd/crane@latest
#
# crane is used rather than `docker pull` + `tag` + `push` on purpose: it copies
# the manifest bytes as-is and preserves digests. The SPIRE chart pins
# chainguard/bash by digest, and a Docker round-trip re-serializes the manifest
# and changes that digest, so the pod's digest-pinned pull then fails with
# "not found".
#
# Image versions track the chart pins in cli/internal/cwedeploy/cwedeploy.go
# (postgresql 18.5.15, temporal 0.74.0, spire 0.30.1 / spire-crds 0.6.1) and the
# tags those charts resolve. After a chart bump, refresh this list from
# `helm template <chart> --version <pin> | grep 'image:'`.

set -Eeuo pipefail

TARGET="${1:-localhost:5000}"
CRANE="${CRANE:-crane}"

command -v "$CRANE" >/dev/null 2>&1 || {
  echo "error: crane not found on PATH (set CRANE=/path/to/crane, or" >&2
  echo "       go install github.com/google/go-containerregistry/cmd/crane@latest)" >&2
  exit 1
}

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

# upstream_reference | path under TARGET (repository[:tag]); each keeps its
# upstream org path so registry.imageRegistry resolves it as TARGET/<path>.
MAPPINGS=(
  # Postgres (Bitnami chart 18.5.15)
  "registry-1.docker.io/bitnami/postgresql:latest|bitnami/postgresql:latest"

  # Temporal (chart 0.74.0)
  "docker.io/temporalio/server:1.30.3|temporalio/server:1.30.3"
  "docker.io/temporalio/admin-tools:1.30.3|temporalio/admin-tools:1.30.3"
  "docker.io/temporalio/ui:2.48.1|temporalio/ui:2.48.1"

  # SPIRE (hardened chart 0.30.1)
  "ghcr.io/spiffe/spire-server:1.15.3|spiffe/spire-server:1.15.3"
  "ghcr.io/spiffe/spire-agent:1.15.3|spiffe/spire-agent:1.15.3"
  "ghcr.io/spiffe/spire-controller-manager:0.7.0|spiffe/spire-controller-manager:0.7.0"
  "ghcr.io/spiffe/spiffe-csi-driver:0.2.7|spiffe/spiffe-csi-driver:0.2.7"
  "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0|sig-storage/csi-node-driver-registrar:v2.15.0"
  "docker.io/busybox:1.37.0-uclibc|busybox:1.37.0-uclibc"
  # Digest-pinned by the chart -- crane preserves the digest.
  "cgr.dev/chainguard/bash@sha256:90041f375e30f41aa7e0390075d8a69dc61900771d52fa98d63ee5d03d866a58|chainguard/bash:latest"
  # Only pulled when TPM node attestation is enabled (the site spec lists a
  # tpmEkThumbprint); harmless to mirror unconditionally.
  "ghcr.io/spiffe/spire-tpm-plugin-tpm-attestor-server:v1.9.0|spiffe/spire-tpm-plugin-tpm-attestor-server:v1.9.0"
)

echo "Mirroring ${#MAPPINGS[@]} bootstrap images to ${TARGET} (insecure=${INSECURE})"
failed=0
for entry in "${MAPPINGS[@]}"; do
  src="${entry%%|*}"
  dst="${TARGET}/${entry##*|}"
  echo ">>> ${src}"
  echo "    -> ${dst}"
  if ! "$CRANE" copy "${crane_flags[@]}" "${src}" "${dst}"; then
    echo "    FAILED" >&2
    failed=1
  fi
done

if [ "${failed}" -ne 0 ]; then
  echo "one or more images failed to mirror" >&2
  exit 1
fi
echo "done: ${#MAPPINGS[@]} images mirrored to ${TARGET}"
