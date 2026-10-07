#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Load tpm_vtpm_proxy on the Linux kernel that runs Docker node containers.
# swtpm --vtpm-proxy asks that kernel for a TPM through /dev/vtpmx.
#
#   sudo ./hack/ensure-vtpm-proxy.sh     # install the module if needed, then load it
#   ./hack/ensure-vtpm-proxy.sh --check  # verify only
#
# Developers run `make -C controller host-up`. On macOS that pipes this script
# into the Colima VM. On Linux it runs this script on the host. `--check`
# verifies the device and does not load the module.

set -euo pipefail

CHECK_ONLY=false
for arg in "$@"; do
  case "${arg}" in
    --check|-c) CHECK_ONLY=true ;;
    -h|--help)
      sed -n '5,13p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "ensure-vtpm-proxy: unknown option: ${arg}" >&2
      exit 2
      ;;
  esac
done

VTPM_MODULE=tpm_vtpm_proxy
VTPMX_DEVICE=/dev/vtpmx
VTPMX_SYSFS=/sys/class/misc/vtpmx/dev
MODULES_LOAD_CONF=/etc/modules-load.d/99-capi-vtpm.conf

die() {
  echo "ensure-vtpm-proxy: $*" >&2
  exit 1
}

info() {
  echo "ensure-vtpm-proxy: $*"
}

check_linux_vtpmx() {
  [[ -c "${VTPMX_DEVICE}" && -r "${VTPMX_SYSFS}" ]]
}

# controller_dir is this file's parent when the script is executed by path.
# A `bash -s` pipe has no path, and callers there already print their own hint.
controller_dir() {
  local src="${BASH_SOURCE[0]}"
  [[ "${src}" == */* ]] || return 1
  (cd "$(dirname "${src}")/.." && pwd)
}

prepare_hint() {
  local dir
  if dir="$(controller_dir)"; then
    printf ' Prepare the Docker host with: make -C %s host-up' "${dir}"
  fi
}

ensure_linux_vtpmx() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "must run as root to load ${VTPM_MODULE}.$(prepare_hint)"
  fi

  local release modules_extra_pkg
  release="$(uname -r)"
  modules_extra_pkg="linux-modules-extra-${release}"

  # Some kernels, including Colima's, ship tpm_vtpm_proxy in linux-modules-extra
  # rather than the base modules package.
  if ! modinfo "${VTPM_MODULE}" >/dev/null 2>&1; then
    if ! command -v apt-get >/dev/null 2>&1; then
      die "modinfo ${VTPM_MODULE} failed and apt-get is unavailable; install the module for kernel ${release}"
    fi
    info "installing ${modules_extra_pkg} (provides ${VTPM_MODULE})"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq || true
    apt-get install -y -qq "${modules_extra_pkg}" || true
  fi

  if ! modinfo "${VTPM_MODULE}" >/dev/null 2>&1; then
    die "${VTPM_MODULE} is unavailable for kernel ${release}.
Check: grep CONFIG_TCG_VTPM_PROXY /boot/config-${release}
=m means the module package is missing; an empty result means this kernel was built without it."
  fi

  install -d /etc/modules-load.d
  printf '%s\n' "${VTPM_MODULE}" >"${MODULES_LOAD_CONF}"
  modprobe "${VTPM_MODULE}"

  check_linux_vtpmx || die "loaded ${VTPM_MODULE} but ${VTPMX_DEVICE} is still missing"
  info "${VTPMX_DEVICE} ready"
  ls -l "${VTPMX_DEVICE}" "${VTPMX_SYSFS}"
}

case "$(uname -s)" in
  Linux)
    if [[ "${CHECK_ONLY}" == true ]]; then
      check_linux_vtpmx && info "${VTPMX_DEVICE} present" && exit 0
      die "${VTPMX_DEVICE} missing.$(prepare_hint)"
    fi
    if check_linux_vtpmx; then
      info "${VTPMX_DEVICE} already present"
      exit 0
    fi
    ensure_linux_vtpmx
    ;;
  Darwin)
    if [[ "${CHECK_ONLY}" != true ]]; then
      die "this script loads ${VTPM_MODULE} on the Linux kernel that runs Docker.$(prepare_hint)"
    fi
    command -v colima >/dev/null 2>&1 || die "colima is not installed.$(prepare_hint)"
    if colima ssh -- test -c "${VTPMX_DEVICE}" && colima ssh -- test -r "${VTPMX_SYSFS}"; then
      info "Colima VM has ${VTPMX_DEVICE}"
      exit 0
    fi
    die "Colima VM is missing ${VTPMX_DEVICE}.$(prepare_hint)"
    ;;
  *)
    die "unsupported OS $(uname -s)"
    ;;
esac
