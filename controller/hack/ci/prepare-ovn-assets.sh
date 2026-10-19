#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

output_dir="${1:?usage: prepare-ovn-assets.sh OUTPUT_DIR}"
workspace_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
version=2.4.2
arch="${BUNDLE_ARCH:-amd64}"
# The reference package builder needs Linux. Local macOS runs already have
# Docker/Colima; use a Linux builder there while CI builds directly on its host.
# The OVN image is built through the host daemon, whose socket is at this path
# inside the Docker VM.
if [[ "$(uname -s)" == Darwin ]]; then
  mkdir -p "${output_dir}"
  output_dir="$(cd "${output_dir}" && pwd)"
  go_version="$(awk '/^go / {print $2; exit}' "${workspace_root}/go.work")"
  exec docker run --rm \
    -v "${workspace_root}:/workspace:ro" -v "${output_dir}:/output" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e "BUNDLE_ARCH=${arch}" -e OVN_TOOLS_DIR=/tmp/ovn-tools \
    "golang:${go_version}-trixie" \
    bash -c '
      set -euo pipefail
      apt-get update -qq
      apt-get install -y -qq dpkg-dev docker-cli docker-buildx
      exec bash /workspace/controller/hack/ci/prepare-ovn-assets.sh /output
    '
fi
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
mkdir -p "${output_dir}"
output_dir="$(cd "${output_dir}" && pwd)"
# Reuse the reference OS packaging, including the committed per-arch image
# digest and its containerd-importing postinst. Keep crane and nfpm in the
# scratch directory: the package tree's bin/ is gitignored, and a checkout
# sync can remove it while crane is still downloading. The ARC runners' dind
# daemon has no Docker Hub mirror, so the image's ubuntu base comes through
# NKE_E2E_DOCKERHUB_MIRROR like the lanes' other Docker Hub pulls.
CNI_DOCKERHUB_MIRROR="${NKE_E2E_DOCKERHUB_MIRROR:-}" make -C "${workspace_root}/os-image/packages/cni-ovn" debs \
  ARCH="${arch}" OVN_STREAMS="${version}" DEB_REVISION="${version}" \
  OUT_DIR="${output_dir}" DIST_DIR="${scratch}/dist" \
  BIN_DIR="${OVN_TOOLS_DIR:-${scratch}/tools}"

# KinD's node image has kubeadm but no Helm. Use the reference OS image pin.
helm_lock="${workspace_root}/os-image/packages/kubeadm-distro/lock/${BUNDLE_ARCH:-amd64}/v1.34.2.lock"
helm_checksum="$(awk '$2 == "helm" {print $3; exit}' "${helm_lock}")"
helm_url="$(awk '$2 == "helm" {print $4; exit}' "${helm_lock}")"
curl -fsSL --retry 5 --connect-timeout 10 --max-time 120 "${helm_url}" -o "${scratch}/helm.tgz"
printf '%s  %s\n' "${helm_checksum}" "${scratch}/helm.tgz" | sha256sum -c -
tar -xzf "${scratch}/helm.tgz" -C "${scratch}"
mkdir -p "${scratch}/helm-deb/usr/bin"
cp "${scratch}/linux-${BUNDLE_ARCH:-amd64}/helm" "${scratch}/helm-deb/usr/bin/helm"
mkdir -p "${scratch}/helm-deb/DEBIAN"
cat >"${scratch}/helm-deb/DEBIAN/control" <<EOF
Package: nke-e2e-helm
Version: 3.20.2
Architecture: ${arch}
Maintainer: NVIDIA Kubernetes Engine
Description: Reference OS Helm binary for KIND E2E nodes
EOF
dpkg-deb --root-owner-group --build "${scratch}/helm-deb" "${output_dir}/nke-e2e-helm.deb"

# The legacy KubeadmInit path fetches a tarball and runs install.sh, then the
# agent invokes the same Helm chart as InstallCNI. Wrap the canonical debs.
case "${arch}" in
  amd64) bundle_arch=x86_64 ;;
  arm64) bundle_arch=aarch64 ;;
  *) printf 'unsupported BUNDLE_ARCH: %s\n' "${arch}" >&2; exit 1 ;;
esac
bundle_name="ovn_v${version}_${bundle_arch}"
mkdir -p "${scratch}/${bundle_name}"
cp "${output_dir}"/nvke-cni-ovn-*_${version}_${arch}.deb "${scratch}/${bundle_name}/"
install -m 0755 "${workspace_root}/controller/hack/ci/assets/ovn-install.sh" \
  "${scratch}/${bundle_name}/install.sh"
tar -czf "${output_dir}/${bundle_name}.tgz" -C "${scratch}" "${bundle_name}"
cat >"${output_dir}/index.json" <<EOF
{
  "ovn": {
    "entries": [{"arch": "${bundle_arch}", "path": "${bundle_name}.tgz", "version": "${version}"}]
  }
}
EOF
