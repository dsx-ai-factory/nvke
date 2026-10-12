#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build one nke-kubeadm-distro-<x.y.z>_<rev>_amd64.deb from a bundle tree
# written by bundle.sh, using nfpm and packaging/nfpm.yaml.
#
# Usage:
#   build-deb.sh <k8s-version> <bundle-dir> <out-dir>
#
# bundle-dir and out-dir may be relative to the caller's cwd and are
# canonicalised below, because nfpm runs with PACKAGING_DIR as its cwd and a
# relative path would resolve against the wrong directory.
#
# Environment:
#   DEB_REVISION   Debian packaging revision (default 1).
#   PEER_VERSIONS  Other Kubernetes versions in this build, space- or
#                  comma-separated. They become Conflicts entries so apt cannot
#                  install two matrix rows at once.
#   NFPM_BIN       Path to the nfpm binary (default: nfpm on PATH).
#   ARCH           amd64 or arm64 (default amd64); must match the bundle dir.
set -euo pipefail

ARCH="${ARCH:-amd64}"
case "${ARCH}" in
  amd64|arm64) ;;
  *) echo "ARCH=${ARCH}: expected amd64 or arm64" >&2; exit 2 ;;
esac
PACKAGE_PREFIX=nke-kubeadm-distro
MATRIX_SUBDIR=k8s-matrix
NFPM_BIN="${NFPM_BIN:-nfpm}"

usage() {
  echo "usage: $(basename "$0") <k8s-version> <bundle-dir> <out-dir>" >&2
  exit 2
}

[[ $# -eq 3 ]] || usage
K8S_VER="$1"
SRC="$(cd "$2" 2>/dev/null && pwd)" || { echo "build-deb.sh: no such bundle directory: $2" >&2; exit 1; }
mkdir -p "$3"
OUT_DIR="$(cd "$3" && pwd)"
DEB_REVISION="${DEB_REVISION:-1}"

command -v "${NFPM_BIN}" >/dev/null 2>&1 || {
  echo "build-deb.sh: nfpm not found (${NFPM_BIN}); run 'make nfpm' or set NFPM_BIN" >&2
  exit 1
}

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ "${K8S_VER}" == v* ]] || K8S_VER="v${K8S_VER}"
K8S_PATCH="${K8S_VER#v}"
PACKAGE_NAME="${PACKAGE_PREFIX}-${K8S_PATCH}"

# The container images are what makes a node come up without a registry, so a
# missing tarball means `make bundle` never ran to completion.
BASE_TGZ="${SRC}/${ARCH}/k8s-base-container-images_${K8S_PATCH}_linux_${ARCH}.tgz"
if [[ ! -f "${BASE_TGZ}" || ! -f "${BASE_TGZ}.sha256" ]]; then
  echo "build-deb.sh: ${BASE_TGZ} and its .sha256 sidecar are missing." >&2
  echo "  The .deb would install without the control-plane images and could not bring up a node offline." >&2
  echo "  Run 'make bundle' first." >&2
  exit 1
fi

# The FHS-staged runtime tree bundle.sh writes: everything nfpm.yaml pulls in
# beyond the bare kubeadm/kubectl/kubelet/runc binaries and the image tarball
# above. Checked here so a partial `make bundle` fails with a message that
# says what to do about it, rather than with nfpm's missing-file error.
STAGE="${SRC}/stage"
for required in \
  "${STAGE}/usr/local/bin/containerd" \
  "${STAGE}/usr/local/bin/containerd-shim-runc-v2" \
  "${STAGE}/usr/local/bin/ctr" \
  "${STAGE}/usr/bin/crictl" \
  "${STAGE}/usr/bin/helm" \
  "${STAGE}/etc/containerd/config.toml" \
  "${STAGE}/etc/crictl.yaml"; do
  if [[ ! -f "${required}" ]]; then
    echo "build-deb.sh: missing staged file ${required}; run 'make bundle' first" >&2
    exit 1
  fi
done
if [[ -z "$(find "${STAGE}/opt/cni/bin" -mindepth 1 -maxdepth 1 2>/dev/null)" ]]; then
  echo "build-deb.sh: ${STAGE}/opt/cni/bin is empty or missing; run 'make bundle' first" >&2
  exit 1
fi

# Every matrix row owns /usr/bin/kubeadm, so each package conflicts with its
# peers. One comma-separated string; nfpm.yaml explains why.
PEER_CONFLICTS="${PACKAGE_PREFIX}"
for peer in ${PEER_VERSIONS//,/ }; do
  peer="${peer#v}"
  [[ "${peer}" == "${K8S_PATCH}" ]] && continue
  PEER_CONFLICTS="${PEER_CONFLICTS}, ${PACKAGE_PREFIX}-${peer}"
done

# TMPDIR_NO_SLASH: macOS sets TMPDIR with a trailing slash, which doubles the
# "/" before the mktemp suffix below; nfpm's globbing (used to resolve every
# `expand: true` content src) then fails to match that path at all.
TMPDIR_NO_SLASH="${TMPDIR:-/tmp}"
SCRATCH="$(mktemp -d "${TMPDIR_NO_SLASH%/}/nke-kubeadm-distro-XXXXXX")"
# SCRIPTS_DIR is assigned later, but initialise it here so the trap does not
# fail under `set -u` and skip the SCRATCH cleanup with it.
SCRIPTS_DIR=""
trap 'rm -rf "${SCRATCH}" "${SCRIPTS_DIR}"' EXIT

printf '%s\n' "Bundled Kubernetes ${K8S_VER} node stack (NVKE)." >"${SCRATCH}/README.Debian"

# nfpm.yaml's top-level `mtime` does not reach files pulled in through the one
# `type: tree` content entry (/opt/cni/bin), so that directory gets a touched
# copy here instead of being read from STAGE. Without it, two builds of the
# same bundle staged at different times would not be byte-identical. Plain
# `src:` entries pick up the top-level mtime on their own.
mkdir -p "${SCRATCH}/stage/opt/cni/bin"
cp -a "${STAGE}/opt/cni/bin/." "${SCRATCH}/stage/opt/cni/bin/"
(export TZ=UTC && find "${SCRATCH}/stage" -exec touch -t 202601010000 {} +)

# {{PLACEHOLDER}} substitution with sed, so packaging needs no python3.
render() {
  sed \
    -e "s|{{PACKAGE_NAME}}|${PACKAGE_NAME}|g" \
    -e "s|{{ARCH}}|${ARCH}|g" \
    -e "s|{{K8S_VER}}|${K8S_VER}|g" \
    -e "s|{{K8S_PATCH}}|${K8S_PATCH}|g" \
    -e "s|{{MATRIX_SUBDIR}}|${MATRIX_SUBDIR}|g" \
    "$1" >"$2"
}
# nfpm's `scripts:` fields get no env-var expansion, so nfpm.yaml points at
# this fixed, package-relative path instead of SCRATCH. Overwriting it every
# run is safe: the Makefile's `debs` loop is sequential.
SCRIPTS_DIR="${PACKAGING_DIR}/../bin/.nfpm-scripts"
rm -rf "${SCRIPTS_DIR}"
mkdir -p "${SCRIPTS_DIR}"
for f in postinst prerm postrm; do
  render "${PACKAGING_DIR}/${f}" "${SCRIPTS_DIR}/${f}"
done
chmod 0755 "${SCRIPTS_DIR}/postinst" "${SCRIPTS_DIR}/prerm" "${SCRIPTS_DIR}/postrm"

mkdir -p "${OUT_DIR}"
export PACKAGE_NAME DEB_REVISION ARCH K8S_VER K8S_PATCH MATRIX_SUBDIR PEER_CONFLICTS SRC SCRATCH

# cd into PACKAGING_DIR: nfpm.yaml's systemd/*.conf, kubelet.default and
# scripts entries are given as paths relative to it.
(cd "${PACKAGING_DIR}" && "${NFPM_BIN}" package -f nfpm.yaml -p deb -t "${OUT_DIR}")
echo "wrote ${OUT_DIR}/${PACKAGE_NAME}_${DEB_REVISION}_${ARCH}.deb"
