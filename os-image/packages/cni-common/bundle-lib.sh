#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared by the CNI packages' bundle.sh: arguments, the lock file, and the
# images (pulled by digest, repacked deterministically). A caller sets CNI,
# sources this, then runs parse_bundle_args "$@", fetch and verify_artifact for
# its own files, stage_images, finish_bundle. Builds fetch what the committed
# lock names; --write-lock resolves upstream afresh and is a reviewed change,
# never a build.

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
# No --retry-delay: it replaces curl's exponential backoff with a flat wait.
fetch() {
  local url="$1" out="$2"
  curl --fail --silent --show-error --location \
    --connect-timeout 10 --max-time 300 \
    --retry 8 --retry-all-errors --retry-connrefused \
    --config - -o "$out" "$url" <<<"${CURL_CONFIG:-}" ||
    die "fetch ${url} failed"
}

# try_retry <what> <cmd...> retries a command but lets the caller choose a
# fallback after the final failure.
try_retry() {
  local what="$1"; shift
  local attempt
  for attempt in 1 2 3; do
    if "$@"; then
      return 0
    fi
    if [[ "$attempt" -lt 3 ]]; then
      echo "${CNI}: ${what} failed, attempt ${attempt} of 3; retrying in $((attempt * 5))s" >&2
      sleep $((attempt * 5))
    fi
  done
  return 1
}

# retry <what> <cmd...>. Commands without their own retry use three attempts.
# A reset mid-transfer used to fail the whole OS-image build after package
# preparation had already completed.
retry() {
  local what="$1"; shift
  try_retry "$what" "$@" || die "${what} failed after 3 attempts"
}

sha_of() {
  sha256sum "$1" | cut -d' ' -f1
}

# pull_images <out> <ref...>. One attempt, starting from a clean output file.
pull_images() {
  local out="$1"; shift
  rm -f "$out"
  "$CRANE" pull --platform "linux/${ARCH}" "$@" "$out"
}

# mirror_has_digest <mirror_ref> <digest> reports whether the mirror serves the
# exact manifest the lock names. One probe, no retry: this only chooses a
# transport, and the canonical reference is always there to fall back to.
mirror_has_digest() {
  local mirror_ref="$1" digest="$2"
  "$CRANE" manifest "${mirror_ref%:*}@${digest}" >/dev/null 2>&1
}

# warn_if_tag_moved <ref> <locked_digest> reports a tag that no longer resolves
# to what the lock pins. Informational by design: the build fetches the locked
# digest either way. Upstream branch tags such as ovn-kubernetes' release-X.Y
# move on every merge, which is a reason to review a lock refresh, not a reason
# to fail a build that names a digest.
# It reads the tag through the configured mirror: a build that cannot reach
# Docker Hub directly should not be making this call to docker.io. A stale
# pull-through cache can then report a tag that has not moved, or miss one that
# has, which is a cost this can carry -- the digest it would name is never
# fetched from here.
warn_if_tag_moved() {
  local ref="$1" want="$2" probe now
  probe="$(image_transport_ref "$ref")"
  now="$("$CRANE" digest --platform "linux/${ARCH}" "$probe" 2>/dev/null || true)"
  [[ -n "$now" && "$now" != "$want" ]] || return 0
  echo "${CNI}: note: ${ref} now resolves to ${now}, the lock pins ${want}; 'make lock' adopts it" >&2
}

# image_transport_ref keeps the locked and installed image identity unchanged
# while allowing CI or another caller to route Docker Hub reads through a
# pull-through registry mirror. Other registries are never rewritten.
image_transport_ref() {
  local ref="$1"
  if [[ -n "${CNI_DOCKERHUB_MIRROR:-}" && "$ref" == docker.io/* ]]; then
    printf '%s/%s\n' "${CNI_DOCKERHUB_MIRROR%/}" "${ref#docker.io/}"
    return
  fi
  printf '%s\n' "$ref"
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
  local -a refs pinned=() fallback_pinned=() transport_refs=()
  local ref mirror_ref transport_ref digest repo placeholder manifest leftover ref_src i
  local using_mirror=false
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
    mirror_ref="$(image_transport_ref "$ref")"
    transport_ref="$ref"
    digest=""
    if [[ "$WRITE_LOCK" == false ]]; then
      # A build fetches the digest the lock names, and never asks what the tag
      # points at now. Resolving the tag here instead would make every build
      # depend on today's upstream head: ovn-kubernetes' release-X.Y moves with
      # every merge to the branch, and each move broke every build in the repo
      # until someone landed a lock refresh.
      digest="$(lock_value image "$ref")"
      [[ -n "$digest" ]] || die "${LOCK} has no digest for ${ref}; run 'make lock' first"
      [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
        die "${LOCK} has a malformed digest for ${ref}: ${digest}"
      if [[ "$mirror_ref" != "$ref" ]] && mirror_has_digest "$mirror_ref" "$digest"; then
        transport_ref="$mirror_ref"
        using_mirror=true
      fi
      warn_if_tag_moved "$ref" "$digest"
    else
      # --write-lock is the one place a digest enters the repository, so it is
      # the one place the tag is resolved. Reviewing that diff is the gate.
      if [[ "$mirror_ref" != "$ref" ]]; then
        if digest="$(try_retry "crane digest ${mirror_ref} for ${ref}" "$CRANE" digest --platform "linux/${ARCH}" "$mirror_ref")" &&
          [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
          transport_ref="$mirror_ref"
          using_mirror=true
        else
          echo "${CNI}: mirror ${mirror_ref} unavailable for ${ref}; falling back to ${ref}" >&2
          digest=""
        fi
      fi
      if [[ -z "$digest" ]]; then
        digest="$(retry "crane digest ${ref}" "$CRANE" digest --platform "linux/${ARCH}" "$ref")"
      fi
      [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
        die "crane digest ${transport_ref} for ${ref} returned ${digest}"
    fi
    transport_refs+=("$transport_ref")
    IMAGE_LINES+=("image ${ref} ${digest}")
    # Pull by digest so the archive cannot drift from what was locked.
    pinned+=("${transport_ref%:*}@${digest}")
    fallback_pinned+=("${ref%:*}@${digest}")
  done

  # A partial tarball from a failed attempt would otherwise be re-read as a
  # complete one, so each attempt starts from nothing.
  if [[ "$using_mirror" == true ]]; then
    if ! try_retry "crane pull through configured mirror" pull_images "${WORK}/images.tar" "${pinned[@]}"; then
      echo "${CNI}: mirrored image transfer failed; falling back to canonical registry references" >&2
      retry "crane pull from canonical registry references" pull_images "${WORK}/images.tar" "${fallback_pinned[@]}"
      transport_refs=("${refs[@]}")
    fi
  else
    retry "crane pull" pull_images "${WORK}/images.tar" "${pinned[@]}"
  fi
  tar_paths_safe "${WORK}/images.tar"
  mkdir -p "${WORK}/unpacked"
  tar -xf "${WORK}/images.tar" --no-same-owner -C "${WORK}/unpacked"

  # crane names a digest-pulled image "<repo>:i-was-a-digest"; rewrite it to
  # the tag the pods ask containerd for.
  manifest="${WORK}/unpacked/manifest.json"
  [[ -f "$manifest" ]] || die "crane archive has no manifest.json"
  for i in "${!refs[@]}"; do
    ref="${refs[$i]}"
    transport_ref="${transport_refs[$i]}"
    repo="${transport_ref%:*}"
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
