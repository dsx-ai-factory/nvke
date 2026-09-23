#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=qemu-arch.sh
source "${ROOT}/qemu-arch.sh"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
mkdir -p "${workdir}/firmware"
touch "${workdir}/firmware/edk2-aarch64-code.fd" "${workdir}/firmware/edk2-arm-vars.fd" \
  "${workdir}/firmware/edk2-x86_64-code.fd" "${workdir}/firmware/edk2-i386-vars.fd"
cat >"${workdir}/qemu-system-aarch64" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == -L && "\$2" == help ]]; then printf '%s\\n' '${workdir}/firmware'; fi
if [[ "\$1" == -accel && "\$2" == help ]]; then printf '%s\\n' hvf tcg; fi
if [[ "\$1" == -device && "\$2" == tpm-tis-device,help && -z "\${NO_PPI:-}" ]]; then printf '%s\\n' 'ppi=<bool>'; fi
EOF
chmod +x "${workdir}/qemu-system-aarch64"
cp "${workdir}/qemu-system-aarch64" "${workdir}/qemu-system-x86_64"
# nke_qemu_env picks its QEMU binary by name, so the fixtures shadow any real
# QEMU on PATH under both names it can choose.
PATH="${workdir}:${PATH}"

QEMU_FAKE="${workdir}/qemu-system-aarch64"
# Exercise firmware search with controlled candidates, independent of OVMF/AAVMF
# installed on the host. A complete distro pair must beat QEMU's fallback.
touch "${workdir}/distro-code.fd" "${workdir}/distro-vars.fd"
nke_find_firmware_pair "$QEMU_FAKE" edk2-aarch64-code.fd edk2-arm-vars.fd \
  "${workdir}/distro-code.fd" "${workdir}/distro-vars.fd"
[[ "${EFI_CODE_DEFAULT}" == "${workdir}/distro-code.fd" ]]
[[ "${EFI_VARS_DEFAULT}" == "${workdir}/distro-vars.fd" ]]
nke_find_firmware_pair "$QEMU_FAKE" edk2-aarch64-code.fd edk2-arm-vars.fd \
  "${workdir}/missing-code.fd" "${workdir}/missing-vars.fd"
[[ "${EFI_CODE_DEFAULT}" == "${workdir}/firmware/edk2-aarch64-code.fd" ]]
[[ "${EFI_VARS_DEFAULT}" == "${workdir}/firmware/edk2-arm-vars.fd" ]]
# Half a pair is a broken host, not a licence to borrow the other half from a
# different firmware build.
if nke_find_firmware_pair "$QEMU_FAKE" edk2-aarch64-code.fd edk2-arm-vars.fd \
  "${workdir}/distro-code.fd" "${workdir}/missing-vars.fd" 2>/dev/null; then
  echo 'half a firmware pair unexpectedly accepted' >&2
  exit 1
fi
# No candidate at all is not an error: OVMF_CODE/OVMF_VARS_TEMPLATE can supply it.
nke_find_firmware_pair "$QEMU_FAKE" missing-code.fd missing-vars.fd \
  "${workdir}/missing-code.fd" "${workdir}/missing-vars.fd"
[[ -z "${EFI_CODE_DEFAULT}" && -z "${EFI_VARS_DEFAULT}" ]]

unset OVMF_CODE OVMF_VARS_TEMPLATE
uname() { case "$1" in -m) printf '%s\n' arm64 ;; -s) printf '%s\n' Darwin ;; esac; }
sysctl() { printf '%s\n' 1; }
nke_qemu_env arm64
# Architecture selection may use the host's distro firmware or our QEMU fixture.
[[ -f "${EFI_CODE_DEFAULT}" ]]
[[ -f "${EFI_VARS_DEFAULT}" ]]
[[ "${TPM_DEVICE}" == tpm-tis-device ]]
[[ "${ACCEL_ARGS[*]}" == '-accel hvf' ]]
[[ "${TPM_DEVICE_SPEC}" == 'tpm-tis-device,tpmdev=tpm0,ppi=off' ]]

# A custom firmware half must never be combined with a discovered counterpart.
for override in OVMF_CODE OVMF_VARS_TEMPLATE; do
  (
    export "${override}=${workdir}/custom.fd"
    if nke_qemu_env arm64 >"${workdir}/partial-override.log" 2>&1; then
      echo "partial firmware override ${override} unexpectedly accepted" >&2
      exit 1
    fi
    grep -Fq 'OVMF_CODE and OVMF_VARS_TEMPLATE must be set together' "${workdir}/partial-override.log"
  )
done
(
  OVMF_CODE="${workdir}/distro-code.fd"
  OVMF_VARS_TEMPLATE="${workdir}/distro-vars.fd"
  nke_qemu_env arm64
  [[ "${EFI_CODE_DEFAULT}" == "$OVMF_CODE" ]]
  [[ "${EFI_VARS_DEFAULT}" == "$OVMF_VARS_TEMPLATE" ]]
)

(
  OVMF_CODE=''
  OVMF_VARS_TEMPLATE=''
  nke_qemu_env arm64
  [[ -f "${EFI_CODE_DEFAULT}" && -f "${EFI_VARS_DEFAULT}" ]]
)

ACCELERATOR=tcg
nke_qemu_env arm64
[[ "${NKE_ACCELERATOR}" == tcg ]]
[[ "${NKE_ACCELERATED}" == 0 ]]
[[ "${QEMU_CPU}" == max,pauth-impdef=on ]]
REQUIRE_ACCELERATION=1
if nke_qemu_env arm64 >/dev/null 2>&1; then
  echo 'strict acceleration unexpectedly accepted explicit TCG' >&2
  exit 1
fi
unset ACCELERATOR REQUIRE_ACCELERATION

export NO_PPI=1
nke_qemu_env arm64
[[ "${TPM_DEVICE_SPEC}" == 'tpm-tis-device,tpmdev=tpm0' ]]
unset NO_PPI

# Every call re-derives its outputs from the architecture argument: nothing may
# leak from a previous call in the same shell.
nke_qemu_env arm64
[[ "${QEMU_BIN}" == qemu-system-aarch64 ]]
nke_qemu_env amd64
[[ "${QEMU_BIN}" == qemu-system-x86_64 ]]
[[ "${QEMU_MACHINE}" == q35 ]]
[[ "${QEMU_CPU}" == max ]]
[[ "${TPM_DEVICE}" == tpm-tis ]]

uname() { case "$1" in -m) printf '%s\n' x86_64 ;; -s) printf '%s\n' Linux ;; esac; }
OVMF_CODE="${workdir}/firmware/edk2-aarch64-code.fd" OVMF_VARS_TEMPLATE="${workdir}/firmware/edk2-arm-vars.fd"
nke_qemu_env arm64
[[ "${ACCEL_ARGS[*]}" == '-accel tcg' ]]
[[ "${NKE_ACCELERATED}" == 0 ]]
[[ "${TPM_DEVICE_SPEC}" == 'tpm-tis-device,tpmdev=tpm0' ]]

REQUIRE_ACCELERATION=1
if nke_qemu_env arm64 >/dev/null 2>&1; then
  echo 'strict acceleration unexpectedly accepted TCG' >&2
  exit 1
fi
unset REQUIRE_ACCELERATION

if nke_qemu_env ppc64le >/dev/null 2>&1; then
  echo 'invalid architecture unexpectedly accepted' >&2
  exit 1
fi
# Empty overrides still require discovery to find a complete firmware pair.
(
  OVMF_CODE=''
  OVMF_VARS_TEMPLATE=''
  # Isolate missing firmware from any distro firmware installed on the host.
  nke_find_firmware_pair() { EFI_CODE_DEFAULT=''; EFI_VARS_DEFAULT=''; }
  if nke_qemu_env arm64 >"${workdir}/missing-firmware.log" 2>&1; then
    echo 'empty overrides unexpectedly accepted missing firmware' >&2
    exit 1
  fi
  grep -Fq 'no UEFI firmware found' "${workdir}/missing-firmware.log"
)
# Explicit overrides must name files that exist.
(
  OVMF_CODE=/nonexistent/code.fd
  if nke_qemu_env arm64 >"${workdir}/missing-override.log" 2>&1; then
    echo 'missing firmware override unexpectedly accepted' >&2
    exit 1
  fi
  grep -Fq 'no UEFI firmware found' "${workdir}/missing-override.log"
)

uname() { case "$1" in -m) printf '%s\n' ppc64le ;; -s) printf '%s\n' Linux ;; esac; }
if nke_qemu_env amd64 >/dev/null 2>&1; then
  echo 'invalid host architecture unexpectedly accepted' >&2
  exit 1
fi
if nke_arch_from_qcow /tmp/image.qcow2 >/dev/null 2>&1; then
  echo 'architecture-less qcow unexpectedly accepted' >&2
  exit 1
fi
