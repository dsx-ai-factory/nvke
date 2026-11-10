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

is_wsl() {
  grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null
}

WSL_KERNEL_REPO=https://github.com/microsoft/WSL2-Linux-Kernel.git
WSL_BUILD_ROOT=/var/cache/nvke-wsl-vtpm

# The WSL2 kernel ships without CONFIG_TCG_VTPM_PROXY and no package provides it.
# Builds the module out of tree from the Microsoft tag that matches the running
# kernel, using that kernel's own /proc/config.gz so the vermagic and symbol CRCs match.
build_wsl_vtpm_module() {
  local release tag work src ext
  release="$(uname -r)"
  [[ "${release}" =~ ^([0-9]+(\.[0-9]+)+)-microsoft-standard-WSL2$ ]] || die "unrecognized WSL2 kernel release ${release}"
  tag="linux-msft-wsl-${BASH_REMATCH[1]}"
  work="${WSL_BUILD_ROOT}/${release}"
  src="${work}/src"
  ext="${work}/ext"

  if [[ ! -f "${ext}/${VTPM_MODULE}.ko" ]]; then
    command -v apt-get >/dev/null 2>&1 || die "apt-get is unavailable; install build-essential flex bison bc libssl-dev libelf-dev dwarves git to build ${VTPM_MODULE}"
    info "installing kernel build dependencies"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential git flex bison bc libssl-dev libelf-dev dwarves

    info "building ${VTPM_MODULE} from ${tag}; the first build takes about 10 minutes"
    rm -rf "${work}"
    install -d "${ext}"
    git clone --quiet --depth 1 --branch "${tag}" "${WSL_KERNEL_REPO}" "${src}"
    (
      cd "${src}"
      zcat /proc/config.gz >.config
      scripts/config --module TCG_VTPM_PROXY
      make LOCALVERSION= olddefconfig
      make -j"$(nproc)" LOCALVERSION= vmlinux
      cp vmlinux.symvers Module.symvers
      make LOCALVERSION= modules_prepare
    )
    cp "${src}/drivers/char/tpm/${VTPM_MODULE}.c" "${src}"/drivers/char/tpm/*.h "${ext}/"
    echo "obj-m := ${VTPM_MODULE}.o" >"${ext}/Makefile"
    make -C "${src}" LOCALVERSION= M="${ext}" modules
  fi

  install -D -m 0644 "${ext}/${VTPM_MODULE}.ko" "/lib/modules/${release}/extra/${VTPM_MODULE}.ko"
  depmod -a "${release}"
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

  if is_wsl && ! modinfo "${VTPM_MODULE}" >/dev/null 2>&1; then
    build_wsl_vtpm_module
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
    # Same default as colima-start.sh. ssh without --profile checks "default"
    # even when the VM was prepared under another COLIMA_PROFILE.
    colima_profile="${COLIMA_PROFILE:-default}"
    if colima ssh --profile "${colima_profile}" -- test -c "${VTPMX_DEVICE}" \
      && colima ssh --profile "${colima_profile}" -- test -r "${VTPMX_SYSFS}"; then
      info "Colima VM has ${VTPMX_DEVICE}"
      exit 0
    fi
    die "Colima VM is missing ${VTPMX_DEVICE}.$(prepare_hint)"
    ;;
  *)
    die "unsupported OS $(uname -s)"
    ;;
esac
