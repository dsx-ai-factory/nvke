#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

output_dir="${1:?usage: prepare-ovn-assets.sh OUTPUT_DIR}"
workspace_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
version=1.4.0
lock="${workspace_root}/os-image/packages/cni-ovn/lock/${BUNDLE_ARCH:-amd64}/${version}.lock"
checksum="$(awk '$1 == "artifact" {print $3; exit}' "${lock}")"
url="$(awk '$1 == "artifact" {print $4; exit}' "${lock}")"
image="$(awk '$1 == "image" {print $2; exit}' "${lock}")"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

curl -fsSL --retry 5 --connect-timeout 10 --max-time 120 "${url}" -o "${scratch}/ovn.tgz"
printf '%s  %s\n' "${checksum}" "${scratch}/ovn.tgz" | sha256sum -c -
mkdir -p "${scratch}/src" "${scratch}/deb/DEBIAN" "${scratch}/deb/etc/dgx" "${output_dir}"
tar -xzf "${scratch}/ovn.tgz" --strip-components=1 -C "${scratch}/src"
cp -R "${scratch}/src/helm/ovn-kubernetes" "${scratch}/deb/etc/dgx/ovn-kubernetes"
# KinD's node image has kubeadm but no Helm. Use the reference OS image pin.
helm_lock="${workspace_root}/os-image/packages/kubeadm-distro/lock/${BUNDLE_ARCH:-amd64}/v1.34.2.lock"
helm_checksum="$(awk '$2 == "helm" {print $3; exit}' "${helm_lock}")"
helm_url="$(awk '$2 == "helm" {print $4; exit}' "${helm_lock}")"
curl -fsSL --retry 5 --connect-timeout 10 --max-time 120 "${helm_url}" -o "${scratch}/helm.tgz"
printf '%s  %s\n' "${helm_checksum}" "${scratch}/helm.tgz" | sha256sum -c -
tar -xzf "${scratch}/helm.tgz" -C "${scratch}"
mkdir -p "${scratch}/deb/usr/bin"
cp "${scratch}/linux-${BUNDLE_ARCH:-amd64}/helm" "${scratch}/deb/usr/bin/helm"
cat >"${scratch}/deb/etc/dgx/ovn-kubernetes/values-nke.yaml" <<EOF
tags:
  ovs-node: false
global:
  image:
    repository: ${image%:*}
    tag: ${image##*:}
EOF
cat >"${scratch}/deb/DEBIAN/control" <<EOF
Package: nke-cni-ovn-charts-${version}
Version: ${version}
Architecture: ${BUNDLE_ARCH:-amd64}
Maintainer: NVIDIA Kubernetes Engine
Description: OVN-Kubernetes Helm chart for container E2E bootstrap
EOF
dpkg-deb --root-owner-group --build "${scratch}/deb" \
  "${output_dir}/nke-cni-ovn-charts-${version}_${version}_${BUNDLE_ARCH:-amd64}.deb"
# The default lane uses InstallCNI after init and pulls the public OVN image.
# Keep an index for the mirror's readiness probe; no legacy CNI bundle is used.
printf '{}\n' >"${output_dir}/index.json"
