#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared QEMU architecture selection for the image boot harnesses. The image
# filename is the source of truth, so an arm64 image cannot accidentally be
# launched with x86 QEMU.

nke_arch_from_qcow() {
  case "$(basename "$1")" in
    *-amd64.qcow2) printf '%s\n' amd64 ;;
    *-arm64.qcow2) printf '%s\n' arm64 ;;
    *)
      echo "qemu-arch: cannot determine architecture from '$1' (expected *-amd64.qcow2 or *-arm64.qcow2)" >&2
      return 1
      ;;
  esac
}

# Sets EFI_CODE_DEFAULT and EFI_VARS_DEFAULT from the first complete firmware
# pair: the distro paths first, then the EDK2 files QEMU exposes through -L.
# The two halves are looked up as a pair on purpose. A host carrying
# OVMF_CODE_4M.fd but no OVMF_VARS_4M.fd would otherwise pair the distro's code
# blob with QEMU's own edk2-i386-vars.fd, a combination nobody ships or tests.
# Finding nothing at all is not an error here: OVMF_CODE and
# OVMF_VARS_TEMPLATE can still supply the firmware.
nke_find_firmware_pair() {
  local qemu="$1" code_name="$2" vars_name="$3" dir pair code vars
  local -a pairs=("$4|$5")
  while IFS= read -r dir; do
    [[ -n "$dir" ]] && pairs+=("$dir/$code_name|$dir/$vars_name")
  done < <("$qemu" -L help 2>/dev/null)

  for pair in "${pairs[@]}"; do
    code="${pair%%|*}"
    vars="${pair##*|}"
    if [[ -f "$code" && -f "$vars" ]]; then
      EFI_CODE_DEFAULT="$code"
      EFI_VARS_DEFAULT="$vars"
      return 0
    fi
    if [[ -f "$code" ]]; then
      echo "qemu-arch: $code has no matching $vars; install the firmware package's variables template" >&2
      return 1
    fi
    if [[ -f "$vars" ]]; then
      echo "qemu-arch: $vars has no matching $code; install the firmware package's code blob" >&2
      return 1
    fi
  done
  EFI_CODE_DEFAULT=""
  EFI_VARS_DEFAULT=""
  return 0
}

# shellcheck disable=SC2034 # Values are the public interface for callers that source this file.
# Sets QEMU_BIN, QEMU_MACHINE, QEMU_CPU, ACCEL_ARGS, TPM_DEVICE,
# EFI_CODE_DEFAULT, EFI_VARS_DEFAULT, NKE_ACCELERATED, NKE_ACCELERATOR and
# TPM_DEVICE_SPEC. TPM_DEVICE_SPEC includes the software TPM's tpmdev id.
nvke_qemu_env() {
  local arch="$1" host_arch host_os hvf_supported=0 kvm_supported=0
  local -a firmware
  # Every output is derived from $arch alone: the same shell calls this once
  # per architecture (arm64 packages then an amd64 image, say), so a value
  # carried over from the previous call would silently run the wrong QEMU.
  case "$arch" in
    amd64)
      QEMU_BIN="qemu-system-x86_64"
      QEMU_MACHINE=q35
      TPM_DEVICE=tpm-tis
      firmware=(edk2-x86_64-code.fd edk2-i386-vars.fd
        /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd)
      ;;
    arm64)
      QEMU_BIN="qemu-system-aarch64"
      QEMU_MACHINE=virt
      TPM_DEVICE=tpm-tis-device
      firmware=(edk2-aarch64-code.fd edk2-arm-vars.fd
        /usr/share/AAVMF/AAVMF_CODE.fd /usr/share/AAVMF/AAVMF_VARS.fd)
      ;;
    *)
      echo "qemu-arch: unsupported architecture '$arch': expected amd64 or arm64" >&2
      return 1
      ;;
  esac

  if ! command -v "$QEMU_BIN" >/dev/null 2>&1; then
    echo "qemu-arch: $QEMU_BIN is not on PATH; $arch images need it" >&2
    return 1
  fi

  if [[ -n "${OVMF_CODE:-}" || -n "${OVMF_VARS_TEMPLATE:-}" ]]; then
    if [[ -z "${OVMF_CODE:-}" || -z "${OVMF_VARS_TEMPLATE:-}" ]]; then
      echo "qemu-arch: OVMF_CODE and OVMF_VARS_TEMPLATE must be set together" >&2
      return 1
    fi
    # Fully overridden: there is nothing discovery could contribute.
    EFI_CODE_DEFAULT="$OVMF_CODE"
    EFI_VARS_DEFAULT="$OVMF_VARS_TEMPLATE"
  else
    nke_find_firmware_pair "$QEMU_BIN" "${firmware[@]}" || return 1
  fi

  if [[ ! -f "$EFI_CODE_DEFAULT" || ! -f "$EFI_VARS_DEFAULT" ]]; then
    echo "qemu-arch: no UEFI firmware found for $arch; set OVMF_CODE and OVMF_VARS_TEMPLATE to existing files" >&2
    return 1
  fi

  case "$(uname -m)" in
    x86_64|amd64) host_arch=amd64 ;;
    aarch64|arm64) host_arch=arm64 ;;
    *)
      echo "qemu-arch: unsupported host architecture '$(uname -m)'" >&2
      return 1
      ;;
  esac
  host_os="$(uname -s)"
  if "$QEMU_BIN" -accel help 2>/dev/null | grep -Fxq hvf; then
    hvf_supported=1
  fi
  if "$QEMU_BIN" -accel help 2>/dev/null | grep -Fxq kvm; then
    kvm_supported=1
  fi

  if [[ "$host_arch" != "$arch" ]]; then
    ACCEL_ARGS=(-accel tcg)
    NKE_ACCELERATED=0
    NKE_ACCELERATOR=tcg
  elif [[ "$host_os" == Darwin && "$(sysctl -n kern.hv_support 2>/dev/null)" == 1 && "$hvf_supported" == 1 ]]; then
    ACCEL_ARGS=(-accel hvf)
    NKE_ACCELERATED=1
    NKE_ACCELERATOR=hvf
  elif [[ "$host_os" == Linux && -c /dev/kvm && -w /dev/kvm && "$kvm_supported" == 1 ]]; then
    ACCEL_ARGS=(-enable-kvm)
    NKE_ACCELERATED=1
    NKE_ACCELERATOR=kvm
  else
    ACCEL_ARGS=(-accel tcg)
    NKE_ACCELERATED=0
    NKE_ACCELERATOR=tcg
  fi

  # TCG is an explicit development choice even where hardware acceleration is
  # available. Hardware accelerator requests must match the detected usable
  # capability; accepting a forced mismatch would fail only after a build has
  # started.
  case "${ACCELERATOR:-}" in
    ""|"${NKE_ACCELERATOR}") ;;
    tcg)
      ACCEL_ARGS=(-accel tcg)
      NKE_ACCELERATED=0
      NKE_ACCELERATOR=tcg
      ;;
    kvm|hvf)
      echo "qemu-arch: requested accelerator ${ACCELERATOR} is unavailable for ${arch}; detected ${NKE_ACCELERATOR}" >&2
      return 1
      ;;
    *)
      echo "qemu-arch: unsupported accelerator '${ACCELERATOR}': expected kvm, hvf, or tcg" >&2
      return 1
      ;;
  esac

  if [[ "$NKE_ACCELERATED" == 1 ]]; then
    QEMU_CPU=host
  elif [[ "$arch" == arm64 ]]; then
    QEMU_CPU=max,pauth-impdef=on
  else
    QEMU_CPU=max
  fi

  TPM_DEVICE_SPEC="${TPM_DEVICE},tpmdev=tpm0"
  # QEMU 11's ARM tpm-tis-device PPI RAM region cannot be mapped by HVF
  # (HV_BAD_ARGUMENT in hvf-all.c). Disabling PPI fixes that path. Only add
  # the property when QEMU advertises it: older ARM QEMU releases do not have
  # PPI and reject an unknown property instead.
  if [[ "$NKE_ACCELERATOR" == hvf && "$TPM_DEVICE" == tpm-tis-device ]] && \
    "$QEMU_BIN" -device "${TPM_DEVICE},help" 2>/dev/null | grep -Eq '(^|[[:space:]])ppi='; then
    TPM_DEVICE_SPEC+=",ppi=off"
  fi

  if [[ "${REQUIRE_ACCELERATION:-0}" == 1 && "$NKE_ACCELERATED" != 1 ]]; then
    echo "qemu-arch: ${arch} cannot be hardware accelerated on this ${host_arch} ${host_os} host" >&2
    return 1
  fi
}
