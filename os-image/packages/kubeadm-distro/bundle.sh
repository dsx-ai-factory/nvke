#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Download, verify and stage the upstream Kubernetes node stack for one
# release, so packaging/build-deb.sh can turn it into an
# nvke-kubeadm-distro-<x.y.z> .deb.
#
# Usage:
#   bundle.sh [--write-lock] --version vX.Y.Z --dist DIR --lock-dir DIR \
#             --crane PATH [--arch amd64]
#
# Default mode builds DIR/vX.Y.Z/ and enforces lock-dir/<arch>/vX.Y.Z.lock.
# --write-lock resolves everything from upstream and rewrites that lock
# instead; it never stages a tree.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# URL bases, overridable so the tests can point every fetch at a local server.
DL_K8S_URL="${BUNDLE_DL_K8S_URL:-https://dl.k8s.io}"
GITHUB_URL="${BUNDLE_GITHUB_URL:-https://github.com}"
HELM_URL="${BUNDLE_HELM_URL:-https://get.helm.sh}"
RUNC_KEYRING="${BUNDLE_RUNC_KEYRING:-${SCRIPT_DIR}/packaging/keyrings/runc.keyring}"

# 2 GiB: well above the largest artifact here, which is a few hundred MiB.
MAX_ARTIFACT_BYTES=2147483648

# One pin for every row: the CNI plugins do not skew against the Kubernetes minor.
CNI_PLUGINS_VERSION=v1.6.0

COMPONENTS=(cni-plugins containerd crictl helm kubeadm kubectl kubelet runc)

CLEARSIGN_MARKER='-----BEGIN PGP SIGNED MESSAGE-----'

die() { echo "bundle.sh: $*" >&2; exit 1; }

# pins_for sets CONTAINERD/RUNC/HELM for a Kubernetes minor.
#
# containerd is the newest patch on the line its Kubernetes support matrix
# lists: https://github.com/containerd/containerd/blob/main/RELEASES.md#kubernetes-support
# runc is not in that matrix, so these are pragmatic pairings with each
# containerd line, from https://github.com/opencontainers/runc/releases.
# Helm is the newest patch on each line supported by
# https://helm.sh/docs/v3/topics/version_skew/. Kubernetes 1.36 and 1.37 use Helm v4.x
# because the published skew table has not caught up.
pins_for() {
  case "$1" in
    37) CONTAINERD=v2.3.7;  RUNC=v1.5.2; HELM=v4.3.1  ;;
    36) CONTAINERD=v2.3.0;  RUNC=v1.4.2; HELM=v4.1.4  ;;
    35) CONTAINERD=v2.2.3;  RUNC=v1.3.5; HELM=v3.20.2 ;;
    34) CONTAINERD=v2.1.7;  RUNC=v1.3.5; HELM=v3.20.2 ;;
    33) CONTAINERD=v2.1.7;  RUNC=v1.2.9; HELM=v3.20.2 ;;
    32) CONTAINERD=v2.1.7;  RUNC=v1.2.9; HELM=v3.20.2 ;;
    31) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.19.5 ;;
    30) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.18.6 ;;
    29) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.17.4 ;;
    *) die "no containerd, runc and helm pins for Kubernetes 1.$1: add a row to pins_for in bundle.sh, choosing containerd from its RELEASES.md Kubernetes support matrix and helm from its version skew policy" ;;
  esac
}

component_url() {
  case "$1" in
    kubeadm|kubectl|kubelet) echo "${DL_K8S_URL}/release/${VERSION}/bin/linux/${ARCH}/$1" ;;
    helm) echo "${HELM_URL}/helm-${HELM}-linux-${ARCH}.tar.gz" ;;
    cni-plugins) echo "${GITHUB_URL}/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${ARCH}-${CNI_PLUGINS_VERSION}.tgz" ;;
    crictl) echo "${GITHUB_URL}/kubernetes-sigs/cri-tools/releases/download/${CRI_TOOLS}/crictl-${CRI_TOOLS}-linux-${ARCH}.tar.gz" ;;
    containerd) echo "${GITHUB_URL}/containerd/containerd/releases/download/${CONTAINERD}/containerd-${CONTAINERD#v}-linux-${ARCH}.tar.gz" ;;
    runc) echo "${GITHUB_URL}/opencontainers/runc/releases/download/${RUNC}/runc.${ARCH}" ;;
    *) die "no URL for component $1" ;;
  esac
}

# fetch writes url to a scratch path; the caller moves it into place only once
# it verifies, so a partial body never appears under the artifact's name.
# --retry-all-errors is left off so a pin typo fails on the first 404.
# No --retry-delay: it replaces curl's exponential backoff with a flat wait.
fetch() {
  local attempt curl_status
  for attempt in 1 2 3; do
    if curl -fsSL --retry 8 --max-time 600 --max-filesize "$MAX_ARTIFACT_BYTES" -o "$2" "$1"; then
      break
    else
      curl_status=$?
    fi
    # curl's normal retries omit receive errors such as a peer reset (56).
    # Retry those twice, while HTTP errors such as 404 still fail at once.
    if [[ "$curl_status" != 56 || "$attempt" -eq 3 ]]; then
      die "download $1 failed (curl exit ${curl_status})"
    fi
    echo "bundle.sh: receive error downloading $1; retrying (${attempt}/2)" >&2
    sleep "$attempt"
  done
  # --max-filesize is advisory: a chunked response has no Content-Length for
  # curl to check it against, so re-check the ceiling on what landed.
  local size; size="$(stat -c %s "$2")"
  ((size <= MAX_ARTIFACT_BYTES)) || { rm -f "$2"; die "$1 is ${size} bytes, over the ${MAX_ARTIFACT_BYTES} byte ceiling"; }
}

# image_digest resolves one tag to its linux/<arch> manifest digest. A registry
# blip, such as a connect timeout to registry.k8s.io, gets two more attempts.
image_digest() {
  local attempt digest
  for attempt in 1 2 3; do
    if digest="$("$CRANE" digest --platform "linux/${ARCH}" "$1")"; then
      printf '%s\n' "$digest"
      return 0
    fi
    [[ "$attempt" -eq 3 ]] && return 1
    echo "bundle.sh: crane digest $1 failed; retrying (${attempt}/2)" >&2
    sleep "$attempt"
  done
}

file_sha() { sha256sum "$1" | cut -d' ' -f1; }

# checksum_from_doc reads the publisher's digest for one basename out of a
# checksum document: a GNU `sha256sum` list, or a single bare hex digest. A
# document that names any file is read only by name, since falling back to a
# bare digest there would verify our artifact against somebody else's bytes.
# Anything ambiguous prints nothing, and the caller fails.
checksum_from_doc() {
  awk -v want="$2" '
    $1 !~ /^[0-9a-fA-F]{64}$/ { next }
    NF == 1 { bare_count++; bare = tolower($1); next }
    {
      named_lines++
      name = $2
      sub(/^\*/, "", name)
      tail = "/" want
      if (name == want || (length(name) > length(tail) - 1 && \
          substr(name, length(name) - length(tail) + 1) == tail)) {
        h = tolower($1)
        # `exit` here would still run END, which would print and exit 0, so
        # the conflict has to be carried into END as a flag.
        if (matched != "" && matched != h) conflict = 1
        matched = h
      }
    }
    END {
      if (conflict) exit 1
      if (named_lines > 0) { if (matched == "") exit 1; print matched; exit 0 }
      if (bare_count == 1) { print bare; exit 0 }
      exit 1
    }
  ' "$1"
}

# verify_runc_doc checks runc's clearsigned checksum list against the bundled
# upstream keyring and writes its clear text to $2. gpg runs in a scratch
# GNUPGHOME: a homedir with use-keyboxd set ignores --keyring outright, so
# importing into a throwaway home is the only way to be sure which keys were
# consulted.
verify_runc_doc() {
  [[ -f "$RUNC_KEYRING" ]] || die "runc keyring missing at ${RUNC_KEYRING}"
  GPG_HOME="$(mktemp -d)"
  chmod 0700 "$GPG_HOME"
  gpg --homedir "$GPG_HOME" --no-options --batch --no-tty --quiet --import "$RUNC_KEYRING" || die "importing ${RUNC_KEYRING} failed"
  local status="${WORK}/gpg-status" rc=0 rejected
  gpg --homedir "$GPG_HOME" --no-options --batch --no-tty --quiet --status-fd 3 --verify "$1" 3>"$status" >/dev/null 2>&1 || rc=$?
  rejected="$(grep -E '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|EXPSIG|BADSIG|NO_PUBKEY|ERRSIG)' "$status" || true)"
  [[ -z "$rejected" ]] || die "runc checksum signature rejected: ${rejected}"
  grep -q '^\[GNUPG:\] GOODSIG ' "$status" || die "runc checksum document has no GOODSIG; refusing to trust it"
  grep -q '^\[GNUPG:\] VALIDSIG ' "$status" || die "runc checksum document has no VALIDSIG; refusing to trust it"
  # A GOODSIG on one signature says nothing about a second gpg rejected, so
  # a non-zero exit is fatal even with both lines present.
  ((rc == 0)) || die "gpg --verify exited ${rc} on the runc checksum document; refusing to trust it"
  gpg --homedir "$GPG_HOME" --no-options --batch --no-tty --quiet --yes --decrypt --output "$2" "$1" 2>/dev/null || die "extracting the signed text of $1 failed"
}

# publisher_sha returns the digest the component's publisher advertises for
# file $2. runc's list is OpenPGP clearsigned; an unsigned one is a downgrade.
publisher_sha() {
  local component="$1" file="$2" url doc="${WORK}/checksum-doc"
  url="$(component_url "$component")"
  if [[ "$component" == runc ]]; then
    fetch "${url%/*}/runc.sha256sum" "${doc}.asc"
    grep -q -- "$CLEARSIGN_MARKER" "${doc}.asc" || die "runc checksum document is not OpenPGP clearsigned"
    verify_runc_doc "${doc}.asc" "$doc"
  else
    case "$component" in
      containerd) fetch "${url}.sha256sum" "$doc" ;;
      *) fetch "${url}.sha256" "$doc" ;;
    esac
    # Only runc signs its list. Reading a signed document from any other
    # publisher as plain text would skip the signature.
    if grep -q -- "$CLEARSIGN_MARKER" "$doc"; then
      die "unexpected clearsigned checksum for ${component}"
    fi
  fi
  local sum
  sum="$(checksum_from_doc "$doc" "$file")" || sum=""
  [[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "no unambiguous SHA-256 digest for ${file} in the checksum document for ${component}"
  echo "$sum"
}

# lock_field prints field $2 of the "artifact <component> <sha256> <url>"
# record for component $1.
lock_field() { awk -v c="$1" -v f="$2" '$1 == "artifact" && $2 == c { print $f }' "$LOCK_FILE"; }

# download_component fetches one artifact unless the cache already holds the
# bytes the lock pins, and verifies whatever it ends up with against the
# publisher's checksum and, in build mode, against the lock.
download_component() {
  local component="$1" url file dest tmp locked="" got published
  url="$(component_url "$component")"
  file="${url##*/}"
  dest="${ARCH_DIR}/${file}"
  if ((!WRITE_LOCK)); then
    # Presence only, for the early message: the artifact diff after the
    # downloads compares the URL and the digest.
    locked="$(lock_field "$component" 3)"
    [[ -n "$locked" ]] || die "${component} is not in ${LOCK_FILE}: run make lock and review the diff"
    if [[ -f "$dest" && "$(file_sha "$dest")" == "$locked" ]]; then
      # The mode is not part of the digest, and nfpm reads these straight
      # from here: a 0644 kubeadm cannot run.
      make_executable "$component" "$dest"
      ARTIFACT_SHA="$locked"
      return
    fi
  fi
  tmp="${WORK}/${file}"
  fetch "$url" "$tmp"
  got="$(file_sha "$tmp")"
  published="$(publisher_sha "$component" "$file")"
  if [[ "$got" != "$published" ]]; then
    rm -f "$tmp"
    die "${component}: ${file} hashes to ${got}, but the publisher's checksum for ${url} is ${published}"
  fi
  if ((!WRITE_LOCK)) && [[ "$got" != "$locked" ]]; then
    rm -f "$tmp"
    die "${component}: downloaded ${got} but ${LOCK_FILE} has ${locked}; upstream changed the bytes behind ${url}"
  fi
  mv "$tmp" "$dest"
  make_executable "$component" "$dest"
  ARTIFACT_SHA="$got"
}

# make_executable sets 0755 on the components shipped as bare binaries; an
# HTTP download lands at 0644 and would not run from where the .deb puts it.
make_executable() {
  case "$1" in kubeadm|kubectl|kubelet|runc) chmod 0755 "$2" ;; esac
}

# resolve_images lists the control-plane images with the verified kubeadm and
# pins each to its linux/<arch> manifest digest, as sorted
# "image <ref> <digest>" records.
resolve_images() {
  local ref digest dupes
  "${ARCH_DIR}/kubeadm" config images list --kubernetes-version "$VERSION" |
    awk '$0 !~ /^[[:space:]]*#/ && NF == 1 && ($0 ~ /:/ || $0 ~ /@/) { print }' >"${WORK}/refs"
  [[ -s "${WORK}/refs" ]] || die "kubeadm config images list returned no images"
  : >"${WORK}/resolved"
  while read -r ref; do
    # Everything downstream splits a reference on its last colon to get the
    # repository, so anything but a plain host[:port]/path:tag stops here
    # rather than being mangled into the wrong pull.
    [[ "$ref" =~ ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9._-]+)*:[A-Za-z0-9._-]+$ ]] || die "kubeadm listed an image reference this script cannot handle: ${ref}"
    digest="$(image_digest "$ref")" || die "crane digest ${ref} failed after 3 attempts"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "crane digest ${ref} returned ${digest}"
    echo "image ${ref} ${digest}" >>"${WORK}/resolved"
  done <"${WORK}/refs"
  LC_ALL=C sort "${WORK}/resolved" >"${WORK}/images"
  # Two tags of one repository rewrite to the same placeholder below, and
  # the second rewrite would silently win.
  dupes="$(awk '{ print $2 }' "${WORK}/images" | sed 's|:[^:/]*$||' | LC_ALL=C sort | uniq -d)"
  [[ -z "$dupes" ]] || die "kubeadm asks for two tags of one repository (${dupes}); they cannot be named apart in one archive"
}

# write_images_tarball pulls every pinned image into the gzipped
# docker-archive postinst feeds to `ctr images import`, plus the sidecar
# postinst runs `sha256sum -c` on.
write_images_tarball() {
  local tgz="${ARCH_DIR}/k8s-base-container-images_${VERSION#v}_linux_${ARCH}.tgz"
  local base="${tgz##*/}" digests="${tgz}.lock-digests" refs=() ref digest leftover unsafe strays
  # Reuse needs all three: the archive is there, it was built from exactly
  # the images the lock now pins, and its bytes still match the sidecar.
  if [[ -f "$tgz" && -f "${tgz}.sha256" ]] && cmp -s "$digests" "${WORK}/images" &&
    (cd "${ARCH_DIR}" && sha256sum -c --quiet "${base}.sha256" >/dev/null 2>&1); then return; fi
  rm -f "$tgz" "${tgz}.sha256" "$digests"
  while read -r _ ref digest; do
    refs+=("${ref%:*}@${digest}")
  done <"${WORK}/images"
  "$CRANE" pull --platform "linux/${ARCH}" "${refs[@]}" "${WORK}/images.tar" || die "crane pull failed"
  # Checked on what crane wrote, before anything is unpacked from it: after
  # the extract the names come from `find .` and can never be unsafe.
  unsafe="$(tar -tf "${WORK}/images.tar" | grep -E '^/|(^|/)\.\.(/|$)' || true)"
  [[ -z "$unsafe" ]] || die "the pulled archive has unsafe member name(s): ${unsafe}"
  mkdir -p "${WORK}/unpacked" && tar -xf "${WORK}/images.tar" --no-same-owner -C "${WORK}/unpacked"
  # The repack enumerates with `find -type f`, which would silently drop a
  # symlink or a device node rather than carry it into the .deb.
  strays="$(find "${WORK}/unpacked" ! -type f ! -type d)"
  [[ -z "$strays" ]] || die "the pulled archive has non-regular member(s): ${strays}"
  # crane pulled by digest, so it names each image "<repo>:i-was-a-digest" in
  # the archive's manifest.json. kubelet looks images up by the tag kubeadm
  # asked for, so put the real tags back before `ctr images import` sees them,
  # and fail rather than ship a package a crane change mis-names.
  while read -r _ ref _; do
    grep -qF "\"${ref%:*}:i-was-a-digest\"" "${WORK}/unpacked/manifest.json" ||
      die "crane did not name ${ref} as expected; it would import under the wrong name"
    sed -i "s|\"${ref%:*}:i-was-a-digest\"|\"${ref}\"|g" "${WORK}/unpacked/manifest.json"
  done <"${WORK}/images"
  # crane could have written the placeholder to a file the loop above does
  # not read. Top level only: a layer blob's contents are the image's.
  leftover="$(find "${WORK}/unpacked" -maxdepth 1 -type f -exec grep -lF 'i-was-a-digest' {} + || true)"
  [[ -z "$leftover" ]] || die "crane's digest placeholder survived in ${leftover}"

  # crane writes the archive's members in Go map order, so a second pull of
  # the same images is not byte-identical. Repack the extracted tree with
  # every field a local umask, locale or filesystem could leak into pinned:
  # format, member order (the -T list is sorted), mtime, owner, mode.
  (cd "${WORK}/unpacked" && find . -type f -print | sed 's|^\./||' | LC_ALL=C sort >"${WORK}/members")
  [[ -s "${WORK}/members" ]] || die "the pulled archive contains no files"
  (cd "${WORK}/unpacked" && LC_ALL=C tar --format=ustar \
    --mtime='2026-01-01 00:00Z' --owner=0 --group=0 --numeric-owner \
    --mode='u+rw,go+r-w,a-s' -cf "${WORK}/repacked.tar" -T "${WORK}/members")
  gzip -n -9 -c "${WORK}/repacked.tar" >"${WORK}/images.tgz"
  mv "${WORK}/images.tgz" "$tgz"
  (cd "${ARCH_DIR}" && sha256sum "$base" >"${base}.sha256")
  cp "${WORK}/images" "$digests"
}

# require_flat_archive rejects a tarball that is anything other than a
# non-empty flat list of regular files. The CNI plugins land straight in
# /opt/cni/bin, so a nested path or a symlink there would be extracted
# somewhere this script never looked. Names and types are read in two passes
# because a symlink prints as "name -> target" under `tar -tvzf`, so the last
# field is not the name.
require_flat_archive() {
  local archive="$1" name types dirs roots=0 files=0
  while IFS= read -r name; do
    name="${name#./}"
    # The archive's own root entry ("./"), which has no name once the prefix
    # is stripped.
    if [[ -z "$name" || "$name" == / ]]; then
      roots=$((roots + 1))
      continue
    fi
    [[ "${name%/}" != */* ]] || die "${archive}: member ${name} is not a flat top-level file"
    files=$((files + 1))
  done < <(tar -tzf "$archive")
  ((files > 0)) || die "${archive}: contains no members"
  types="$(tar -tvzf "$archive" | cut -c1 | LC_ALL=C sort -u | tr -d 'd-')"
  [[ -z "$types" ]] || die "${archive}: contains member(s) that are not regular files (tar type '${types}')"
  # Any other directory is a nested tree this script never looks inside.
  dirs="$(tar -tvzf "$archive" | cut -c1 | grep -c d || true)"
  ((dirs == roots)) || die "${archive}: contains a directory member; expected a flat list of files"
}

# stage_tree lays out everything nfpm.yaml installs beyond the bare
# kubeadm/kubectl/kubelet/runc binaries and the image archive.
stage_tree() {
  local stage="${DIST}/${VERSION}/stage" pause cni
  # config.toml below is config version 3, which only containerd 2.x parses.
  [[ "$CONTAINERD" == v2.* ]] || die "containerd pin ${CONTAINERD} is below v2.0.0: config version 3 needs containerd 2.x"
  rm -rf "$stage"
  mkdir -p "${stage}/usr/local/bin" "${stage}/usr/bin" "${stage}/opt/cni/bin" "${stage}/etc/containerd"

  tar -xzf "${ARCH_DIR}/containerd-${CONTAINERD#v}-linux-${ARCH}.tar.gz" \
    -C "${stage}/usr/local/bin" --no-same-owner --strip-components=1 \
    bin/containerd bin/containerd-shim-runc-v2 bin/ctr
  cni="${ARCH_DIR}/cni-plugins-linux-${ARCH}-${CNI_PLUGINS_VERSION}.tgz"
  require_flat_archive "$cni"
  tar -xzf "$cni" --no-same-owner -C "${stage}/opt/cni/bin"
  tar -xzf "${ARCH_DIR}/crictl-${CRI_TOOLS}-linux-${ARCH}.tar.gz" --no-same-owner -C "${stage}/usr/bin" crictl
  tar -xzf "${ARCH_DIR}/helm-${HELM}-linux-${ARCH}.tar.gz" --no-same-owner -C "${WORK}" "linux-${ARCH}/helm"
  mv "${WORK}/linux-${ARCH}/helm" "${stage}/usr/bin/helm"
  chmod -R 0755 "${stage}/usr/local/bin" "${stage}/usr/bin" "${stage}/opt/cni/bin"

  # The pause image this release asks for, read back from the lock.
  pause="$(awk '$1 == "image" && $2 ~ /\/pause:/ { print $2 }' "$LOCK_FILE" | LC_ALL=C sort | head -n1)"
  [[ -n "$pause" ]] || die "no pause image in ${LOCK_FILE}; containerd has no sandbox image to pin"
  cat >"${stage}/etc/containerd/config.toml" <<EOF
# Managed by nvke-kubeadm-distro. Rendered at package build time from the
# locked image list, so the sandbox image cannot skew from the bundled images.
# Rewritten on every install: put local overrides in /etc/containerd/conf.d/
# rather than editing this file.
imports = ['/etc/containerd/conf.d/*.toml']
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.pinned_images]
      sandbox = '${pause}'
  [plugins.'io.containerd.cri.v1.runtime']
    [plugins.'io.containerd.cri.v1.runtime'.cni]
      bin_dir = '/opt/cni/bin'
      conf_dir = '/etc/cni/net.d'
    [plugins.'io.containerd.cri.v1.runtime'.containerd]
      default_runtime_name = 'runc'
      [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes]
        [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
          runtime_type = 'io.containerd.runc.v2'
          [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
            BinaryName = '/usr/sbin/runc'
            SystemdCgroup = true
EOF
  cat >"${stage}/etc/crictl.yaml" <<'EOF'
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
EOF
}

WRITE_LOCK=0
VERSION=""
DIST=""
LOCK_DIR=""
CRANE=""
ARCH=amd64
while (($#)); do
  case "$1" in
    --write-lock) WRITE_LOCK=1; shift ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --dist) DIST="${2:-}"; shift 2 ;;
    --lock-dir) LOCK_DIR="${2:-}"; shift 2 ;;
    --crane) CRANE="${2:-}"; shift 2 ;;
    --arch) ARCH="${2:-}"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must be vX.Y.Z, got '${VERSION}'"
[[ -n "$DIST" && -n "$LOCK_DIR" && -n "$CRANE" ]] || die "--dist, --lock-dir and --crane are required"
case "$ARCH" in
  amd64|arm64) ;;
  *) die "--arch ${ARCH}: expected amd64 or arm64" ;;
esac
[[ "$(uname -s)" == Linux ]] || die "the container images are listed by the downloaded linux kubeadm binary, so this build host ($(uname -s)) cannot produce a bundle; run on Linux"
# Both modes run the downloaded kubeadm (resolve_images), so the host has to be
# the target architecture. There is no cross-build.
HOST_ARCH="$(uname -m)"
case "${ARCH}:${HOST_ARCH}" in
  amd64:x86_64|arm64:aarch64) ;;
  *) die "--arch ${ARCH} runs the ${ARCH} kubeadm to list its images; this host is ${HOST_ARCH}" ;;
esac
command -v "$CRANE" >/dev/null 2>&1 || die "crane not found at ${CRANE}"

MINOR="${VERSION#v}"
[[ "${MINOR%%.*}" == 1 ]] || die "only Kubernetes 1.x is supported, got ${VERSION}"
MINOR="${MINOR#*.}"
MINOR="${MINOR%%.*}"
pins_for "$MINOR"
CRI_TOOLS="v1.${MINOR}.0"

ARCH_DIR="${DIST}/${VERSION}/${ARCH}"
# Per architecture: every artifact URL and every image digest in the lock is
# arch-specific, so one file per version could only ever hold one of them.
LOCK_FILE="${LOCK_DIR}/${ARCH}/${VERSION}.lock"

# Before anything is created on disk, so a bad lock leaves no dist tree.
if ((!WRITE_LOCK)); then
  [[ -f "$LOCK_FILE" ]] || die "no lock for ${VERSION}: run make lock and review the diff (expected ${LOCK_FILE})"
  # The comparisons below read only the rows they recognise, so an unknown
  # record is a lock this script cannot enforce.
  stray_records="$(awk '$1 !~ /^#/ && $1 != "kubernetes" && $1 != "artifact" && $1 != "image" && NF > 0 { print }' "$LOCK_FILE")"
  [[ -z "$stray_records" ]] || die "${LOCK_FILE} has unrecognised record(s): ${stray_records}"
  kubernetes_rows="$(grep -c '^kubernetes ' "$LOCK_FILE" || true)"
  ((kubernetes_rows == 1)) || die "${LOCK_FILE} has ${kubernetes_rows} kubernetes records, want exactly one"
  locked_version="$(awk '$1 == "kubernetes" { print $2; exit }' "$LOCK_FILE")"
  [[ "$locked_version" == "$VERSION" ]] || die "${LOCK_FILE} locks '${locked_version}', not ${VERSION}"
fi

mkdir -p "$ARCH_DIR" "$(dirname "$LOCK_FILE")"
WORK="$(mktemp -d)"
GPG_HOME=""
LOCK_TMP=""
cleanup() {
  [[ -z "$GPG_HOME" ]] || { gpgconf --homedir "$GPG_HOME" --kill all >/dev/null 2>&1 || true; }
  rm -rf "$WORK" "$GPG_HOME"
  [[ -z "$LOCK_TMP" ]] || rm -f "$LOCK_TMP"
}
trap cleanup EXIT

echo "bundle.sh: ${VERSION} (containerd ${CONTAINERD}, runc ${RUNC}, helm ${HELM}, cri-tools ${CRI_TOOLS})" >&2
: >"${WORK}/artifacts"
for component in "${COMPONENTS[@]}"; do
  ARTIFACT_SHA=""
  download_component "$component"
  echo "artifact ${component} ${ARTIFACT_SHA} $(component_url "$component")" >>"${WORK}/artifacts"
done

# Safe to execute now, and only now: kubeadm's bytes matched what was verified.
resolve_images

if ((WRITE_LOCK)); then
  # Written beside the file it replaces, so the lock is either the old one or
  # the new one, never half-written.
  LOCK_TMP="$(mktemp "${LOCK_FILE}.XXXXXX")"
  {
    echo "# nvke-kubeadm-distro lock for ${VERSION} (${ARCH}). Regenerate with \`make lock\`; review the diff before committing."
    echo "kubernetes ${VERSION}"
    LC_ALL=C sort "${WORK}/artifacts" "${WORK}/images"
  } >"$LOCK_TMP"
  if [[ -f "$LOCK_FILE" ]]; then
    # `diff | grep` cannot be tested for emptiness by its exit status:
    # diff exits 1 precisely when it has something to report.
    summary="$(diff -- "$LOCK_FILE" "$LOCK_TMP" | grep -E '^[<>]' || true)"
    if [[ -n "$summary" ]]; then
      printf '%s\n' "$summary"
    else
      echo "  (unchanged)"
    fi
  else
    echo "  new lock file"
  fi
  mv "$LOCK_TMP" "$LOCK_FILE"
  LOCK_TMP=""
  echo "bundle.sh: wrote ${LOCK_FILE}" >&2
  exit 0
fi

# Both directions: a row for something this build never downloaded, or two
# rows for one component, is not the file that was reviewed.
LC_ALL=C sort "${WORK}/artifacts" >"${WORK}/artifacts.sorted"
if ! diff -q <(grep '^artifact ' "$LOCK_FILE") "${WORK}/artifacts.sorted" >/dev/null; then
  diff -- <(grep '^artifact ' "$LOCK_FILE") "${WORK}/artifacts.sorted" >&2 || true
  die "the artifacts do not match ${LOCK_FILE}: run make lock and review the diff"
fi
if ! diff -q <(grep '^image ' "$LOCK_FILE") "${WORK}/images" >/dev/null; then
  diff -- <(grep '^image ' "$LOCK_FILE") "${WORK}/images" >&2 || true
  die "kubeadm's images do not match ${LOCK_FILE}: run make lock and review the diff"
fi
write_images_tarball
stage_tree
echo "bundle.sh: staged ${DIST}/${VERSION}" >&2
