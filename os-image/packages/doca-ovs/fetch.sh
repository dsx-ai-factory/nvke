#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Download and verify NVIDIA's DOCA Open vSwitch debs, which the node image
# installs in place of Ubuntu's openvswitch-switch. They are NVIDIA binaries
# with no published source, so they are fetched as-is rather than rebuilt.
#
# Usage:
#   fetch.sh --doca-version X.Y.Z --lock-dir DIR --write-lock
#   fetch.sh --doca-version X.Y.Z --lock-dir DIR --series 26.04 --arch amd64 \
#            --dist DIR --out DIR
#
# --write-lock reads the repository's signed index for every series and arch
# and rewrites the lock. The default mode copies one series and arch into
# --out, enforcing the lock; --dist caches downloads across runs.
set -euo pipefail

REPO_URL="${DOCA_REPO_URL:-https://linux.mellanox.com/public/repo/doca}"
# The "NVIDIA DOCA Host" key (70454E6BF5D9C2D3B78709E8DC726C5E41B9CC50), from
# public_keys/nvidia-doca-debian-gpg-public-key.gpg in the repository. The older
# GPG-KEY-Mellanox.pub there does not sign DOCA 3.x.
KEY_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/nvidia-doca-host.asc"

SERIESES=(24.04 26.04)
ARCHES=(amd64 arm64)
# doca-openvswitch-switch and every dependency the DOCA repository serves
# itself, for DOCA 3.5.0. The rest of its dependencies come from the Ubuntu
# archive. Re-derive this list when bumping DOCA: it changed between 3.2.3 and
# 3.5.0.
PACKAGES=(
  doca-openvswitch-switch
  doca-openvswitch-common
  python3-doca-openvswitch
  collectx-clxapi
  libibverbs1
  ibverbs-providers
  libibmad5
  libibumad3
)

die() { echo "fetch.sh: $*" >&2; exit 1; }

WRITE_LOCK=0
DOCA_VERSION=""
LOCK_DIR=""
SERIES=""
ARCH=""
DIST=""
OUT=""
while (($#)); do
  case "$1" in
    --write-lock) WRITE_LOCK=1; shift ;;
    --doca-version) DOCA_VERSION="$2"; shift 2 ;;
    --lock-dir) LOCK_DIR="$2"; shift 2 ;;
    --series) SERIES="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --dist) DIST="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ -n "$DOCA_VERSION" ]] || die "--doca-version is required"
[[ -n "$LOCK_DIR" ]] || die "--lock-dir is required"
LOCK_FILE="${LOCK_DIR}/${DOCA_VERSION}.lock"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fetch() { curl -fsSL --retry 8 --max-time 300 -o "$2" "$1" || die "download $1 failed"; }
file_sha() { sha256sum "$1" | cut -d' ' -f1; }

# The repository's directory names for an Ubuntu series and a Debian arch.
repo_base() {
  local arch
  case "$2" in amd64) arch=x86_64 ;; arm64) arch=arm64-sbsa ;; esac
  echo "${REPO_URL}/${DOCA_VERSION}/ubuntu${1}/${arch}"
}

# lock_rows prints one lock row per package for series $1 and arch $2, from
# the repository's signed index. InRelease is checked against the committed key, Packages
# against the digest InRelease gives it.
lock_rows() {
  local series="$1" arch="$2" base want got package row
  base="$(repo_base "$series" "$arch")"
  fetch "${base}/InRelease" "${WORK}/InRelease"
  gpgv --keyring "${WORK}/keyring.gpg" --output "${WORK}/Release" "${WORK}/InRelease" 2>"${WORK}/gpgv.log" ||
    { cat "${WORK}/gpgv.log" >&2; die "${base}/InRelease is not signed by ${KEY_FILE}"; }
  want="$(awk '/^SHA256:/ { s = 1; next } /^[^ ]/ { s = 0 } s && $3 == "Packages" { print $1 }' "${WORK}/Release")"
  fetch "${base}/Packages" "${WORK}/Packages"
  got="$(file_sha "${WORK}/Packages")"
  [[ -n "$want" && "$got" == "$want" ]] || die "${base}/Packages is ${got}, but InRelease lists ${want:-nothing}"
  for package in "${PACKAGES[@]}"; do
    # One stanza per package: a second version would make the pin ambiguous.
    row="$(awk -v p="$package" '
      /^Package: / { in_pkg = ($2 == p) }
      in_pkg && /^Filename: / { file = $2 }
      in_pkg && /^SHA256: / { sha = $2 }
      /^$/ && in_pkg { n++; out = sha " " file; in_pkg = 0 }
      END { if (in_pkg) { n++; out = sha " " file } if (n == 1) print out; exit n != 1 }
    ' "${WORK}/Packages")" || die "${base}/Packages does not have exactly one ${package}"
    echo "artifact ${package} ${series} ${arch} ${row%% *} ${base}/${row#* }"
  done
}

if ((WRITE_LOCK)); then
  gpg --dearmor <"$KEY_FILE" >"${WORK}/keyring.gpg" || die "cannot read ${KEY_FILE}"
  mkdir -p "$LOCK_DIR"
  {
    echo "# doca-ovs lock for DOCA ${DOCA_VERSION}."
    echo "# Regenerate with \`make lock\`; review the diff before committing."
    for series in "${SERIESES[@]}"; do
      for arch in "${ARCHES[@]}"; do
        # Standalone, so a die in the subshell fails the whole write.
        rows="$(lock_rows "$series" "$arch")"
        echo "$rows"
      done
    done
  } >"${WORK}/lock.new"
  mv "${WORK}/lock.new" "$LOCK_FILE"
  echo "wrote ${LOCK_FILE}"
  exit 0
fi

[[ " ${SERIESES[*]} " == *" ${SERIES} "* ]] || die "unsupported --series '${SERIES}' (expected ${SERIESES[*]})"
[[ " ${ARCHES[*]} " == *" ${ARCH} "* ]] || die "unsupported --arch '${ARCH}' (expected ${ARCHES[*]})"
[[ -n "$DIST" ]] || die "--dist is required"
[[ -n "$OUT" ]] || die "--out is required"
[[ -f "$LOCK_FILE" ]] || die "no lock at ${LOCK_FILE}: run 'make lock' and review the diff"

cache="${DIST}/${DOCA_VERSION}/${SERIES}/${ARCH}"
mkdir -p "$cache" "$OUT"
for package in "${PACKAGES[@]}"; do
  row="$(awk -v p="$package" -v s="$SERIES" -v a="$ARCH" \
    '$1 == "artifact" && $2 == p && $3 == s && $4 == a { print $5, $6 }' "$LOCK_FILE")"
  sha="${row%% *}"
  url="${row#* }"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] ||
    die "${package} ${SERIES}/${ARCH} is not in ${LOCK_FILE}: run 'make lock' and review the diff"
  deb="${cache}/${url##*/}"
  if [[ ! -f "$deb" || "$(file_sha "$deb")" != "$sha" ]]; then
    fetch "$url" "${deb}.part"
    got="$(file_sha "${deb}.part")"
    [[ "$got" == "$sha" ]] || die "${url}: downloaded ${got} but ${LOCK_FILE} has ${sha}"
    mv "${deb}.part" "$deb"
  fi
  # OUT is shared with the other node packages: drop any other version of
  # this one so the index never carries two.
  rm -f "${OUT}/${package}_"*"_${ARCH}.deb"
  install -m 0644 "$deb" "${OUT}/"
done
echo "copied ${#PACKAGES[@]} DOCA ${DOCA_VERSION} debs for Ubuntu ${SERIES} ${ARCH} into ${OUT}"
