#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#
# Upload flat release files to S3 and write a checksum next to each one, so
# the upload is independently verifiable without trusting this script's own
# exit code. Generic on purpose -- unlike agent/scripts/upload-s3-apt-intake.sh,
# this does not know about .deb control fields or apt-publisher's index.json;
# it just copies files to a prefix. Used by components that need a plain,
# unauthenticated CloudFront URL (nSpect's "Registering a binary archive"
# accepts this as a source) but are not apt packages.
#
# Layout produced:
#   <S3_URI>/<S3_PREFIX>/<file basename>
#   <S3_URI>/<S3_PREFIX>/<file basename>.sha256
#
# Required env:
#   S3_URI      e.g. s3://nke-prod-assets/nvke-packages
#   S3_PREFIX   e.g. nvkectl/releases/v0.1.0-rc.3/linux/amd64
# Args: one or more file paths to upload.

set -euo pipefail

: "${S3_URI:?S3_URI must be set (e.g. s3://nke-prod-assets/nvke-packages)}"
: "${S3_PREFIX:?S3_PREFIX must be set (e.g. nvkectl/releases/v0.1.0-rc.3/linux/amd64)}"

if [ "$#" -eq 0 ]; then
  echo "error: no files given to upload" >&2
  exit 1
fi

base="${S3_URI%/}/${S3_PREFIX%/}"

for f in "$@"; do
  if [ ! -f "$f" ]; then
    echo "error: ${f} does not exist" >&2
    exit 1
  fi
  bn="$(basename "$f")"
  dest="${base}/${bn}"
  sha="$(sha256sum "$f" | awk '{print $1}')"

  echo "uploading: ${bn} -> ${dest}"
  aws s3 cp "$f" "$dest" --only-show-errors
  printf '%s' "$sha" | aws s3 cp - "${dest}.sha256" --only-show-errors
  echo "  sha256: ${sha}"
done

echo "S3 upload complete: ${base}"
