#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared by the CNI packages' bundle.sh: arguments, the lock file, and the
# images (pulled by digest, repacked deterministically). A caller sets CNI,
# sources this, then runs parse_bundle_args "$@", fetch and verify_artifact for
# its own files, stage_images, finish_bundle. Builds enforce the committed
# lock; --write-lock regenerates it and is a reviewed change, never a build.

# shellcheck disable=SC2034  # VERSION, STAGE, WORK are set here for the caller.

die() {
  echo "${CNI}: $*" >&2
  exit 1
}

parse_bundle_args() {
  VERSION="" DIST="" LOCK_DIR="" CRANE="" ARCH="amd64" WRITE_LOCK=false
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version) VERSION="${2:-}"; shift 2 ;;
      --dist) DIST="${2:-}"; shift 2 ;;
      --lock-dir) LOCK_DIR="${2:-}"; shift 2 ;;
      --crane) CRANE="${2:-}"; shift 2 ;;
      --arch) ARCH="${2:-}"; shift 2 ;;
      --write-lock) WRITE_LOCK=true; shift ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  [[ -n "$VERSION" ]] || die "--version is required"
  [[ -n "$DIST" ]] || die "--dist is required"
  [[ -n "$LOCK_DIR" ]] || die "--lock-dir is required"
  [[ -n "$CRANE" && -x "$CRANE" ]] || die "--crane must point at an executable crane"
  case "$ARCH" in
    amd64|arm64) ;;
    *) die "--arch ${ARCH}: expected amd64 or arm64" ;;
  esac

  # Per arch: the digests below are resolved for one platform.
  LOCK="${LOCK_DIR}/${ARCH}/${VERSION}.lock"
  STAGE="${DIST}/${VERSION}"
  WORK="${STAGE}/.work"
  ARTIFACT_LINES=()
  rm -rf "$STAGE"
  mkdir -p "$WORK"
}

# fetch <url> <out>. CURL_CONFIG, when set, is fed to curl as a config file, so
# a credential never appears on a command line.
fetch() {
  local url="$1" out="$2"
  curl --fail --silent --show-error --location \
    --connect-timeout 10 --max-time 300 \
    --retry 3 --retry-all-errors --retry-delay 5 --retry-connrefused \
    --config - -o "$out" "$url" <<<"${CURL_CONFIG:-}" ||
    die "fetch ${url} failed"
}

sha_of() {
  sha256sum "$1" | cut -d' ' -f1
}

# lock_value <kind> <key> prints the pinned value, or nothing when absent.
lock_value() {
  local kind="$1" key="$2"
  [[ -f "$LOCK" ]] || return 0
  awk -v kind="$kind" -v key="$key" '$1 == kind && $2 == key { print $3; exit }' "$LOCK"
}

# verify_artifact <name> <path> <url> checks the file's sha256 against the
# lock's artifact line, or records it for the lock being written.
verify_artifact() {
  local name="$1" path="$2" url="$3" got want
  got="$(sha_of "$path")"
  if [[ "$WRITE_LOCK" == false ]]; then
    want="$(lock_value artifact "$name")"
    [[ -n "$want" ]] || die "${LOCK} has no artifact line for ${name}; run 'make lock' first"
    [[ "$want" == "$got" ]] ||
      die "${name} sha256 mismatch: lock has ${want}, upstream served ${got}"
  fi
  ARTIFACT_LINES+=("artifact ${name} ${got} ${url}")
}

# tar_paths_safe <archive> refuses members that would escape the extraction dir.
tar_paths_safe() {
  local unsafe
  unsafe="$(tar -tf "$1" | grep -E '^/|(^|/)\.\.(/|$)' || true)"
  [[ -z "$unsafe" ]] || die "$(basename "$1") has an unsafe member path: ${unsafe}"
}

# stage_images pulls the lock's `image` lines by digest into one gzipped
# docker-archive, ${STAGE}/images.tgz, repacked so two builds of the same lock
# are byte-identical, plus a sha256sum sidecar naming the installed file.
stage_images() {
  local -a refs pinned=()
  local ref digest want repo placeholder manifest leftover ref_src
  IMAGE_LINES=()
  # amd64's lock decides which images ship; other arches pin their own digests
  # for the same refs. Reading refs from amd64 keeps the sets in step.
  ref_src="$LOCK"
  if [[ "$WRITE_LOCK" == true && "$ARCH" != amd64 ]]; then
    ref_src="${LOCK_DIR}/amd64/${VERSION}.lock"
    [[ -f "$ref_src" ]] ||
      die "no amd64 lock at ${ref_src}; lock amd64 first, it is the source of truth for the image list"
  fi
  mapfile -t refs < <(awk '$1 == "image" { print $2 }' "$ref_src" 2>/dev/null || true)
  [[ ${#refs[@]} -gt 0 ]] || die "${ref_src} lists no images; seed it with 'image <ref>' lines"

  for ref in "${refs[@]}"; do
    digest="$("$CRANE" digest --platform "linux/${ARCH}" "$ref")" ||
      die "crane digest ${ref} failed"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
      die "crane digest ${ref} returned ${digest}"
    if [[ "$WRITE_LOCK" == false ]]; then
      want="$(lock_value image "$ref")"
      [[ -n "$want" ]] || die "${LOCK} has no digest for ${ref}; run 'make lock' first"
      [[ "$want" == "$digest" ]] ||
        die "${ref} digest mismatch: lock has ${want}, registry serves ${digest}"
    fi
    IMAGE_LINES+=("image ${ref} ${digest}")
    # Pull by digest so the archive cannot drift from what was verified.
    pinned+=("${ref%:*}@${digest}")
  done

  "$CRANE" pull --platform "linux/${ARCH}" "${pinned[@]}" "${WORK}/images.tar" ||
    die "crane pull failed"
  tar_paths_safe "${WORK}/images.tar"
  mkdir -p "${WORK}/unpacked"
  tar -xf "${WORK}/images.tar" --no-same-owner -C "${WORK}/unpacked"

  # crane names a digest-pulled image "<repo>:i-was-a-digest"; rewrite it to
  # the tag the pods ask containerd for.
  manifest="${WORK}/unpacked/manifest.json"
  [[ -f "$manifest" ]] || die "crane archive has no manifest.json"
  for ref in "${refs[@]}"; do
    repo="${ref%:*}"
    placeholder="${repo}:i-was-a-digest"
    grep -q -- "$placeholder" "$manifest" ||
      die "crane did not name ${ref} as expected; it would import under the wrong name"
    sed -i "s|${placeholder//./\\.}|${ref}|g" "$manifest"
  done
  leftover="$(grep -l 'i-was-a-digest' "${WORK}/unpacked"/* 2>/dev/null || true)"
  [[ -z "$leftover" ]] || die "crane's digest placeholder survived in ${leftover}"

  (cd "${WORK}/unpacked" && find . -type f | LC_ALL=C sort >"${WORK}/members")
  tar --format=gnu --numeric-owner --owner=0 --group=0 \
    --mtime='2026-01-01 00:00:00Z' --mode='u+rw,go+r-w,a-s' \
    -C "${WORK}/unpacked" -cf "${WORK}/repacked.tar" -T "${WORK}/members"
  gzip -n -9 -c "${WORK}/repacked.tar" >"${STAGE}/images.tgz"
  printf '%s  %s-images_%s_linux_%s.tgz\n' \
    "$(sha_of "${STAGE}/images.tgz")" "${CNI#cni-}" "${VERSION#v}" "${ARCH}" \
    >"${STAGE}/images.tgz.sha256"
}

# finish_bundle removes the work tree, then writes the lock for --write-lock or
# reports the verified stage.
finish_bundle() {
  rm -rf "$WORK"
  if [[ "$WRITE_LOCK" == true ]]; then
    mkdir -p "$(dirname "$LOCK")"
    {
      echo "# nke-${CNI} lock for ${VERSION} (${ARCH}). Regenerate with \`make lock\`; review the diff before committing."
      echo "${CNI#cni-} ${VERSION}"
      printf '%s\n' "${ARTIFACT_LINES[@]}" "${IMAGE_LINES[@]}"
    } >"${LOCK}.new"
    mv "${LOCK}.new" "$LOCK"
    echo "${CNI}: wrote ${LOCK}"
  else
    echo "${CNI}: staged ${VERSION} into ${STAGE} (verified against ${LOCK})"
  fi
}
