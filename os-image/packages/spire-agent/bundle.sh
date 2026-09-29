#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Download, verify and stage the upstream binaries the spire-agent .deb ships:
# SPIRE's agent, the TPM node attestor plugin's agent-side binary and its
# EK-reading helper, and the DSX IMDS node attestor plugin.
#
# Usage:
#   bundle.sh [--write-lock] --spire-version X.Y.Z --tpm-version X.Y.Z \
#             --arch amd64 --dist DIR --lock-dir DIR
#
# Default mode stages DIR/<spire>+tpm<tpm>/<arch>/ and enforces the lock.
# --write-lock resolves every arch from upstream and rewrites the lock
# instead; it never stages a tree.
set -euo pipefail

# URL bases, overridable so the tests can point every fetch at a local server.
GITHUB_URL="${BUNDLE_GITHUB_URL:-https://github.com}"
ASSETS_URL="${BUNDLE_ASSETS_URL:-https://nke-prod-assets.s3.us-west-2.amazonaws.com}"

# 256 MiB: well above the largest artifact here, which is ~40 MiB.
MAX_ARTIFACT_BYTES=268435456

ARCHES=(amd64 arm64)
COMPONENTS=(spire-agent tpm_attestor_agent get_tpm_pubhash dsx-imds-node-attestor-agent)

# die prints an error message prefixed with the script name and exits nonzero.
die() { echo "bundle.sh: $*" >&2; exit 1; }

WRITE_LOCK=0
SPIRE_VERSION=""
TPM_VERSION=""
ARCH=amd64
DIST=""
LOCK_DIR=""
while (($#)); do
  case "$1" in
    --write-lock) WRITE_LOCK=1; shift ;;
    --spire-version) SPIRE_VERSION="${2#v}"; shift 2 ;;
    --tpm-version) TPM_VERSION="${2#v}"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --dist) DIST="$2"; shift 2 ;;
    --lock-dir) LOCK_DIR="$2"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ -n "$SPIRE_VERSION" ]] || die "--spire-version is required"
[[ -n "$TPM_VERSION" ]] || die "--tpm-version is required"
[[ -n "$DIST" ]] || die "--dist is required"
[[ -n "$LOCK_DIR" ]] || die "--lock-dir is required"
case "$ARCH" in amd64 | arm64) ;; *) die "unsupported --arch ${ARCH} (expected amd64 or arm64)" ;; esac

BUNDLE_VERSION="${SPIRE_VERSION}+tpm${TPM_VERSION}"
LOCK_FILE="${LOCK_DIR}/${BUNDLE_VERSION}.lock"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# component_url prints the release tarball URL for component $1 on arch $2.
component_url() {
  case "$1" in
    spire-agent) echo "${GITHUB_URL}/spiffe/spire/releases/download/v${SPIRE_VERSION}/spire-${SPIRE_VERSION}-linux-${2}-musl.tar.gz" ;;
    tpm_attestor_agent | get_tpm_pubhash) echo "${GITHUB_URL}/spiffe/spire-tpm-plugin/releases/download/v${TPM_VERSION}/spire_tpm_plugin_${1}_linux_${2}_v${TPM_VERSION}.tar.gz" ;;
    # Unversioned on purpose: the publisher replaces these objects in place.
    # The lock digest is what stops a replaced object from entering a build.
    dsx-imds-node-attestor-agent) echo "${ASSETS_URL}/dsx-imds-node-attestor-agent/linux-${2}/dsx-imds-node-attestor-agent" ;;
    *) die "no URL for component $1" ;;
  esac
}

# download_path prints where fetch stores component $1 for arch $2. The IMDS
# plugin is published as a raw binary; every other component is a tarball.
download_path() {
  case "$1" in
    dsx-imds-node-attestor-agent) echo "${WORK}/$1-$2" ;;
    *) echo "${WORK}/$1-$2.tar.gz" ;;
  esac
}

# member_path prints the path inside the tarball that holds the binary. SPIRE
# ships a versioned tree; the plugin tarballs are a single flat file.
member_path() {
  case "$1" in
    spire-agent) echo "spire-${SPIRE_VERSION}/bin/spire-agent" ;;
    *) echo "$1" ;;
  esac
}

# No --retry-delay: it replaces curl's exponential backoff with a flat wait.
fetch() {
  curl -fsSL --retry 8 --max-time 300 --max-filesize "$MAX_ARTIFACT_BYTES" -o "$2" "$1" ||
    die "download $1 failed"
  # --max-filesize is advisory: a chunked response has no Content-Length for
  # curl to check it against, so re-check the ceiling on what landed.
  local size
  size="$(stat -c %s "$2")"
  ((size <= MAX_ARTIFACT_BYTES)) || { rm -f "$2"; die "$1 is ${size} bytes, over the ${MAX_ARTIFACT_BYTES} byte ceiling"; }
}

file_sha() { sha256sum "$1" | cut -d' ' -f1; }

# publisher_sha prints the digest SPIRE advertises for its own tarball, from
# the GNU-format `<name>_sha256sum.txt` sidecar beside the release asset.
# spire-tpm-plugin publishes no checksum document at all, and the DSX IMDS
# plugin is a raw binary with no checksum sidecar, so for those artifacts the
# committed lock is the only pin -- see README.md.
publisher_sha() {
  local url="$1" doc="${WORK}/checksum-doc" sum
  local checksum_url="${url%.tar.gz}_sha256sum.txt"
  fetch "$checksum_url" "$doc"
  sum="$(awk -v want="${url##*/}" '$1 ~ /^[0-9a-f]{64}$/ && $2 == want { print $1; found++ } END { exit found != 1 }' "$doc")" ||
    die "no unambiguous SHA-256 digest for ${url##*/} in ${checksum_url}"
  echo "$sum"
}

# lock_sha prints the digest the lock pins for component $1 on arch $2.
lock_sha() { awk -v c="$1" -v a="$2" '$1 == "artifact" && $2 == c && $3 == a { print $4 }' "$LOCK_FILE"; }

# resolve downloads one artifact, checks it against the publisher where there
# is one, and prints its digest.
resolve() {
  local component="$1" arch="$2" url got published artifact
  url="$(component_url "$component" "$arch")"
  artifact="$(download_path "$component" "$arch")"
  fetch "$url" "$artifact"
  got="$(file_sha "$artifact")"
  if [[ "$component" == spire-agent ]]; then
    published="$(publisher_sha "$url")"
    [[ "$got" == "$published" ]] ||
      die "${component}/${arch}: downloaded ${got}, but the publisher's checksum for ${url} is ${published}"
  fi
  echo "$got"
}

if ((WRITE_LOCK)); then
  mkdir -p "$LOCK_DIR"
  {
    echo "# spire-agent lock for SPIRE ${SPIRE_VERSION} with spire-tpm-plugin ${TPM_VERSION}."
    echo "# Regenerate with \`make lock\`; review the diff before committing."
    echo "# spire-tpm-plugin publishes no upstream checksum document, so the TPM"
    echo "# rows are the only pin those artifacts have. The DSX IMDS node attestor"
    echo "# is an unversioned raw binary with no checksum sidecar, so its rows are"
    echo "# the only pin that artifact has. See README.md."
    for component in "${COMPONENTS[@]}"; do
      for arch in "${ARCHES[@]}"; do
        # A standalone assignment, not inline in the echo: a `die` inside
        # $(...) exits only that subshell, and echo's own status would still
        # be 0 -- which would write a row with an empty digest and let
        # `make lock` report success over a failed download.
        sha="$(resolve "$component" "$arch")"
        echo "artifact ${component} ${arch} ${sha} $(component_url "$component" "$arch")"
      done
    done
  } >"${LOCK_FILE}.new"
  mv "${LOCK_FILE}.new" "$LOCK_FILE"
  echo "wrote ${LOCK_FILE}"
  exit 0
fi

[[ -f "$LOCK_FILE" ]] || die "no lock at ${LOCK_FILE}: run 'make lock' and review the diff"

OUT="${DIST}/${BUNDLE_VERSION}/${ARCH}"
mkdir -p "$OUT"
for component in "${COMPONENTS[@]}"; do
  locked="$(lock_sha "$component" "$ARCH")"
  [[ "$locked" =~ ^[0-9a-f]{64}$ ]] ||
    die "${component}/${ARCH} is not in ${LOCK_FILE}: run 'make lock' and review the diff"
  dest="${OUT}/${component}"
  # The staged binary's own digest is not the downloaded artifact's when that
  # artifact is a tarball, so reuse is keyed on a sidecar naming the download
  # digest it was staged from.
  if [[ -f "$dest" && "$(cat "${dest}.from" 2>/dev/null)" == "$locked" ]]; then
    continue
  fi
  got="$(resolve "$component" "$ARCH")"
  [[ "$got" == "$locked" ]] ||
    die "${component}/${ARCH}: downloaded ${got} but ${LOCK_FILE} has ${locked}; upstream changed the bytes behind $(component_url "$component" "$ARCH")"
  # An HTTP download and a tar extract both land at the archive's mode; the
  # .deb runs these, and SPIRE execs the plugin binary directly.
  if [[ "$component" == dsx-imds-node-attestor-agent ]]; then
    install -m 0755 "$(download_path "$component" "$ARCH")" "$dest"
  else
    member="$(member_path "$component")"
    tar -xzf "$(download_path "$component" "$ARCH")" --no-same-owner -C "$WORK" "$member" ||
      die "${component}/${ARCH}: its tarball has no ${member}"
    install -m 0755 "${WORK}/${member}" "$dest"
  fi
  printf '%s\n' "$locked" >"${dest}.from"
done
echo "staged ${COMPONENTS[*]} for ${ARCH} in ${OUT}"
