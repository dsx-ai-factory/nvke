#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

OUTPUT_DIR="${1:?usage: prepare-calico-assets.sh OUTPUT_DIR}"
CALICO_VERSION="3.30.1"
CALICO_MANIFESTS_PACKAGE="nvke-cni-calico-manifests-${CALICO_VERSION}"
ASSET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/assets" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

command -v dpkg-deb >/dev/null 2>&1 || {
  printf 'dpkg-deb is required to build the Calico manifests package\n' >&2
  exit 1
}

download_verified() {
  local url=$1
  local sha256=$2
  local destination=$3

  curl \
    --fail \
    --silent \
    --show-error \
    --location \
    --connect-timeout 10 \
    --max-time 30 \
    --retry 8 \
    --retry-all-errors \
    --retry-max-time 240 \
    --retry-connrefused \
    "${url}" \
    -o "${destination}"
  printf '%s  %s\n' "${sha256}" "${destination}" | sha256sum -c -
}

mkdir -p "${OUTPUT_DIR}"

calico_bundle_name="calico_v${CALICO_VERSION}_na"
calico_bundle="${TMP_DIR}/${calico_bundle_name}"
mkdir -p "${calico_bundle}/manifests"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/operator-crds.yaml" \
  "5969b2393d9cab1212480ad08dd989023106373c23163441474e65ceb2bb734f" \
  "${calico_bundle}/manifests/operator-crds.yaml"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/tigera-operator.yaml" \
  "65fc29459f236af44b31bd409640534db20ff940192f6b9023c972a69041977f" \
  "${calico_bundle}/manifests/tigera-operator.yaml"
download_verified \
  "https://raw.githubusercontent.com/projectcalico/calico/v${CALICO_VERSION}/manifests/custom-resources.yaml" \
  "f7cd142773609fc7f830b96d8f29bd1b5f05cdc5293324992d8524b54c09c717" \
  "${calico_bundle}/manifests/custom-resources.yaml"
install -m 0755 "${ASSET_DIR}/calico-install.py" "${calico_bundle}/calico-install.py"
tar -czf "${OUTPUT_DIR}/${calico_bundle_name}.tgz" -C "${TMP_DIR}" "${calico_bundle_name}"

# InstallCNI consumes a Debian package that dpkg can identify and enumerate.
calico_deb_root="${TMP_DIR}/${CALICO_MANIFESTS_PACKAGE}-deb"
calico_deb_install_root="${calico_deb_root}/usr/share/nvke-cni/calico"
calico_deb="${OUTPUT_DIR}/${CALICO_MANIFESTS_PACKAGE}_${CALICO_VERSION}_all.deb"
install -d -m 0755 "${calico_deb_root}/DEBIAN" "${calico_deb_install_root}/manifests"
install -m 0644 \
  "${calico_bundle}/manifests/operator-crds.yaml" \
  "${calico_bundle}/manifests/tigera-operator.yaml" \
  "${calico_bundle}/manifests/custom-resources.yaml" \
  "${calico_deb_install_root}/manifests/"
install -m 0755 "${ASSET_DIR}/calico-install.py" "${calico_deb_install_root}/apply-manifests.sh"
cat >"${calico_deb_root}/DEBIAN/control" <<EOF
Package: ${CALICO_MANIFESTS_PACKAGE}
Version: ${CALICO_VERSION}
Architecture: all
Maintainer: NVIDIA Kubernetes Engine
Description: Calico manifests for NVKE cluster bootstrap
EOF
chmod 0644 "${calico_deb_root}/DEBIAN/control"
dpkg-deb --root-owner-group --build "${calico_deb_root}" "${calico_deb}"

[[ "$(dpkg-deb --field "${calico_deb}" Package)" == "${CALICO_MANIFESTS_PACKAGE}" ]]
[[ "$(dpkg-deb --field "${calico_deb}" Version)" == "${CALICO_VERSION}" ]]
calico_deb_contents="${TMP_DIR}/calico-deb-contents.txt"
dpkg-deb --contents "${calico_deb}" >"${calico_deb_contents}"
grep -Fq './usr/share/nvke-cni/calico/apply-manifests.sh' "${calico_deb_contents}"
grep -Fq './usr/share/nvke-cni/calico/manifests/tigera-operator.yaml' "${calico_deb_contents}"

cat >"${OUTPUT_DIR}/index.json" <<EOF
{
  "calico": {
    "entries": [
      {
        "arch": "na",
        "path": "${calico_bundle_name}.tgz",
        "version": "${CALICO_VERSION}"
      }
    ]
  }
}
EOF
