#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the two nke-<cni>-<stream> debs from a bundle.sh tree: the content deb
# (manifests or charts) from the package's spec, the images deb from
# nfpm-images.yaml here.
#
# Usage: build-deb.sh <version> <bundle-dir> <out-dir>
# Env, from deb.mk: CNI, PRODUCT, CONTENT_SPEC, CONTENT_KIND (manifests or
# charts), DEB_REVISION (default 1), NFPM_BIN (default: nfpm on PATH).

set -euo pipefail

[[ $# -eq 3 ]] || { echo "usage: $(basename "$0") <version> <bundle-dir> <out-dir>" >&2; exit 2; }
: "${CNI:?}" "${PRODUCT:?}" "${CONTENT_SPEC:?}" "${CONTENT_KIND:?}"

ARCH="${ARCH:-amd64}"
CONTENT_ONLY="${CONTENT_ONLY:-false}"
# Package names carry the stream without a leading v, matching what
# agent/internal/agent/cni.go asks apt for.
STREAM="${1#v}"
SRC="$(cd "$2" && pwd)"
mkdir -p "$3"
OUT_DIR="$(cd "$3" && pwd)"
DEB_REVISION="${DEB_REVISION:-1}"
NFPM_BIN="${NFPM_BIN:-nfpm}"
COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ -f "$CONTENT_SPEC" ]] || { echo "build-deb.sh: no such spec: ${CONTENT_SPEC}" >&2; exit 1; }
command -v "$NFPM_BIN" >/dev/null 2>&1 || {
  echo "build-deb.sh: nfpm not found (${NFPM_BIN}); run 'make nfpm' or set NFPM_BIN" >&2
  exit 1
}

CONTENT_PACKAGE="nke-${CNI}-${CONTENT_KIND}-${STREAM}"
IMAGES_PACKAGE="nke-${CNI}-images-${STREAM}"
IMAGE_DIR="/usr/lib/nke/${CNI}/${STREAM}"
# The installed name bundle-lib.sh writes into the sha256 sidecar.
IMG_TGZ="${IMAGE_DIR}/${CNI#cni-}-images_${STREAM}_linux_${ARCH}.tgz"

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

# build_one <spec> <package> <cwd>: nfpm expands ${VAR} in the spec but resolves
# `scripts:` against its cwd, so the images build runs beside its postinst.
build_one() {
  local spec="$1" package="$2" cwd="$3"
  echo "${CNI}: building ${package}_${DEB_REVISION}_${ARCH}.deb"
  (cd "$cwd" &&
    PACKAGE_NAME="$package" STREAM="$STREAM" DEB_REVISION="$DEB_REVISION" ARCH="$ARCH" \
    SRC="$SRC" CNI="$CNI" PRODUCT="$PRODUCT" IMAGE_DIR="$IMAGE_DIR" IMG_TGZ="$IMG_TGZ" \
    CONTENT_PACKAGE="$CONTENT_PACKAGE" IMAGES_PACKAGE="$IMAGES_PACKAGE" \
    "$NFPM_BIN" package -f "$spec" -p deb -t "$OUT_DIR")
  echo "wrote ${OUT_DIR}/${package}_${DEB_REVISION}_${ARCH}.deb"
}

if [[ "$CONTENT_ONLY" == true ]]; then
  ARCH=all
fi
build_one "$CONTENT_SPEC" "$CONTENT_PACKAGE" "$(dirname "$CONTENT_SPEC")"

if [[ "$CONTENT_ONLY" == true ]]; then
  exit 0
fi

sed -e "s|@PACKAGE_NAME@|${IMAGES_PACKAGE}|g" -e "s|@IMG_TGZ@|${IMG_TGZ}|g" \
  -e "s|@PRODUCT@|${PRODUCT}|g" -e "s|@CNI@|${CNI}|g" \
  "${COMMON_DIR}/postinst" >"${SCRATCH}/postinst.rendered"
chmod 0755 "${SCRATCH}/postinst.rendered"
cp "${COMMON_DIR}/nfpm-images.yaml" "${SCRATCH}/nfpm-images.yaml"
build_one "${SCRATCH}/nfpm-images.yaml" "$IMAGES_PACKAGE" "$SCRATCH"
