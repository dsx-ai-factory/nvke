#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#
# Push tags from a local OCI layout to a registry repository with oras. The
# artifact-*.yml workflows build each artifact once into an OCI layout and push
# that same layout to every registry, so GHCR and NVCR serve the same digests.
#
# `oras cp` copies an image, an index with its manifests, or an OCI artifact
# alike, and preserves digests. Each tag is checked on both sides of the push:
#
#   - A target tag that already serves the layout's digest is left alone, so
#     re-running a publish job is a no-op.
#   - A target tag that serves a different digest is an error. Version tags are
#     immutable; a rebuild never overwrites a published release.
#   - After the push the target must serve the layout's digest.
#
# Requires oras on PATH and a login to the target registry.
#
# Usage: push-oci-layout.sh <layout directory> <target repository> <tag>...

set -euo pipefail

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <layout directory> <target repository> <tag>..." >&2
  exit 1
fi

layout="$1"
target="$2"
shift 2

for tag in "$@"; do
  want="$(oras resolve --oci-layout "${layout}:${tag}")"

  if have="$(oras resolve "${target}:${tag}" 2>/dev/null)"; then
    if [ "${have}" != "${want}" ]; then
      echo "error: ${target}:${tag} already serves ${have}, not ${want} from this build" >&2
      exit 1
    fi
    echo "${target}:${tag} already serves ${want}"
    continue
  fi

  oras cp --from-oci-layout "${layout}:${tag}" "${target}:${tag}"

  got="$(oras resolve "${target}:${tag}")"
  if [ "${got}" != "${want}" ]; then
    echo "error: ${target}:${tag} serves ${got} after the push, expected ${want}" >&2
    exit 1
  fi
  echo "${target}:${tag} is ${want}"
done
