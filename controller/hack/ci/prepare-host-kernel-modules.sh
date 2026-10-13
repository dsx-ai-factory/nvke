#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Ubuntu's uname release omits the upload number and distribution backport.
# Use its documented full build identity instead. This extraction follows
# Ubuntu's own debian/tests/ubuntu-regression-suite, including multi-part flavours.
# https://ubuntu.com/kernel#identifying-a-kernel
host_kernel_package_version() {
  local signature="$1" release="$2" vendor build upstream extra flavour version
  read -r vendor build upstream extra <<< "${signature}"
  if [[ "${vendor}" != Ubuntu || -z "${upstream}" || -n "${extra}" ]]; then
    echo "expected an official Ubuntu /proc/version_signature" >&2
    return 1
  fi
  flavour="${build#*-*-}"
  version="${build%-${flavour}}"
  if [[ "${release}" != *-"${flavour}" || "${version}" != "${release%-${flavour}}."* ]]; then
    echo "Ubuntu kernel signature does not match uname release ${release}" >&2
    return 1
  fi
  printf '%s\n' "${version}"
}

host_kernel_archive_suite() {
  local version="$1" runner_suite="$2"
  case "${version}" in
    *~22.04.*) printf 'jammy\n' ;;
    *~24.04.*) printf 'noble\n' ;;
    *~*) echo "unsupported Ubuntu backport build ${version}" >&2; return 1 ;;
    *) printf '%s\n' "${runner_suite}" ;;
  esac
}

prepare_host_kernel_modules() (
  set -euo pipefail
  local destination="$1" release signature version suite archive task_tmp deb
  local ID VERSION_CODENAME
  if [[ "${destination}" != /* || -e "${destination}" ]]; then
    echo "module staging directory must be a new absolute path" >&2
    exit 1
  fi
  release="$(uname -r)"
  signature="$(cat /proc/version_signature)"
  version="$(host_kernel_package_version "${signature}" "${release}")"
  dpkg --validate-version "${version}"
  # shellcheck source=/dev/null
  source /etc/os-release
  [[ "${ID}" == ubuntu && -n "${VERSION_CODENAME}" ]]
  suite="$(host_kernel_archive_suite "${version}" "${VERSION_CODENAME}")"
  case "$(dpkg --print-architecture)" in
    arm64) archive=http://ports.ubuntu.com/ubuntu-ports ;;
    amd64) archive=http://archive.ubuntu.com/ubuntu ;;
    *) echo "unsupported runner architecture" >&2; exit 1 ;;
  esac
  [[ -r /usr/share/keyrings/ubuntu-archive-keyring.gpg ]]
  printf 'host kernel signature: %s\nmodule package version: %s (%s)\n' "${signature}" "${version}" "${suite}"

  # Separate signed indexes avoid mixing the runner image's distribution with
  # the host's kernel. APT verifies InRelease signatures and package hashes.
  task_tmp="$(mktemp -d)"
  trap 'rm -rf "${task_tmp}"' EXIT
  chmod 0755 "${task_tmp}"
  mkdir -p "${task_tmp}/lists/partial" "${task_tmp}/debs"
  chown _apt "${task_tmp}/debs"
  for pocket in "${suite}" "${suite}-updates" "${suite}-security"; do
    printf 'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] %s %s main\n' "${archive}" "${pocket}"
  done > "${task_tmp}/sources.list"
  local apt_options=(
    -o "Dir::Etc::sourcelist=${task_tmp}/sources.list"
    -o Dir::Etc::sourceparts=-
    -o "Dir::State::lists=${task_tmp}/lists"
    -o Acquire::http::Timeout=30
    -o Acquire::Retries=2
  )
  apt-get "${apt_options[@]}" -o APT::Update::Error-Mode=any update -qq
  cd "${task_tmp}/debs"
  apt-get "${apt_options[@]}" download \
    "linux-modules-${release}=${version}" "linux-modules-extra-${release}=${version}"
  mkdir -p "${destination}"
  for deb in ./*.deb; do
    dpkg-deb -f "${deb}" Package Version Architecture
    dpkg --compare-versions "$(dpkg-deb -f "${deb}" Version)" eq "${version}"
    dpkg-deb -x "${deb}" "${destination}"
  done
  # Newer Ubuntu packages use /usr/lib. Keep the staging root usable by kmod
  # and the shared Docker preflight without writing the runner's module tree.
  if [[ ! -e "${destination}/lib" && -d "${destination}/usr/lib" ]]; then
    ln -s usr/lib "${destination}/lib"
  fi
  [[ -d "${destination}/lib/modules/${release}" ]]
  depmod -b "${destination}" "${release}"
  modinfo -b "${destination}" -k "${release}" openvswitch
)

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  [[ $# == 1 ]] || { echo "usage: $0 STAGING_DIRECTORY" >&2; exit 1; }
  prepare_host_kernel_modules "$1"
fi
