#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run `packer build ... TEMPLATE` with the firmware -var flags inserted in front
# of the template, which Packer requires last. A neighbour pod's qemu postinst
# resets the shared /dev/kvm to 0660 for a few seconds (see check-acceleration
# in the Makefile); a build that starts QEMU inside that window dies at once
# although every check passed. Widen the device again and run the build once
# more, keeping the first attempt's log beside the second.
set -euo pipefail

# shellcheck source=qemu-arch.sh
source "$(dirname "${BASH_SOURCE[0]}")/qemu-arch.sh"
nvke_qemu_env "${ARCH:?}"

template="${!#}"
set -- "${@:1:$#-1}" \
  -var "efi_firmware_code=${OVMF_CODE:-$EFI_CODE_DEFAULT}" \
  -var "efi_firmware_vars=${OVMF_VARS_TEMPLATE:-$EFI_VARS_DEFAULT}" \
  "${template}"

status=0
"$@" || status=$?
if [[ "${status}" -eq 0 || -z "${PACKER_LOG_PATH:-}" ]] ||
  ! grep -q 'Could not access KVM kernel module' "${PACKER_LOG_PATH}"; then
  exit "${status}"
fi

echo "QEMU could not open /dev/kvm: a neighbour pod's qemu postinst resets its mode for a few seconds; widening it and retrying the build once" >&2
mv -- "${PACKER_LOG_PATH}" "${PACKER_LOG_PATH%.log}.kvm-denied.log"
sudo -n chmod 666 /dev/kvm 2>/dev/null || true
exec "$@"
