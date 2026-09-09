#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Boots any qcow2 this repo produces in QEMU, with an SSH key injected via
# cloud-init and the guest's :22 forwarded to the host, and leaves it running
# in the foreground so you can `ssh` in and poke around.
#
# To quit: Ctrl-A then X (release Ctrl-A, then press X) -- NOT plain Ctrl-C.
# QEMU runs with -nographic, so the host terminal is the guest's serial
# console; plain Ctrl-C is sent to the guest as literal input (e.g. garbling
# a login prompt) instead of interrupting QEMU. Ctrl-A C instead drops into
# the QEMU monitor (type `quit` there). Either way triggers this script's
# cleanup trap (kills swtpm if attached, removes the scratch workdir).
#
# Unlike boot-smoke.sh / demo-fde.sh, this runs no checks and never tears
# itself down on its own — it's for manual, interactive use.
#
# Usage (QCOW filenames encode the Ubuntu version/arch -- see vm_name in
# nke-node-fde.pkr.hcl / nke-node-non-fde.pkr.hcl -- check output-fde/ or
# output-non-fde/ for the exact name):
#   QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04-amd64.qcow2 \
#     OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./packer/scripts/run-vm.sh
#   QCOW=packer/output-fde/nke-node-fde-ubuntu-26.04-amd64.qcow2 \
#     OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd TPM=1 ./packer/scripts/run-vm.sh
#
# Env:
#   QCOW          path to the qcow2 to boot (required)
#   SSH_KEY       ssh key to inject (default: packer/.ssh/id_ed25519)
#   SSH_PORT      host port to forward to guest :22 (default: 2222)
#   ARCH          guest architecture (amd64 or arm64); inferred from the qcow2
#                 filename when it ends in -arm64.qcow2
#   QEMU_BINARY   QEMU system emulator; defaults from ARCH
#   OVMF_CODE     needed for the FDE/non-FDE variants (both UEFI-installed);
#                 unset boots BIOS-style, for whatever other qcow2 you point
#                 this at
#   OVMF_VARS_TEMPLATE  default: /usr/share/OVMF/OVMF_VARS_4M.fd
#   TPM           set to 1 to attach a fresh swtpm (the FDE image's first
#                 real boot needs one to complete its TPM bind; the shipped
#                 image's build-time luks.key= GRUB parameter unlocks it
#                 unattended even without sudo/console access — see
#                 os-image/docs/full-disk-encryption.md)
#   MEMORY, CPUS  default: 4096 (MB), 2

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-}"
if [[ -z "${QCOW}" ]]; then
  echo "run-vm: QCOW is required, e.g. QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04-amd64.qcow2 $0" >&2
  exit 1
fi
if [[ ! -f "${QCOW}" ]]; then
  echo "run-vm: no image at ${QCOW}" >&2
  exit 1
fi
# Must be absolute: qemu-img resolves a relative -b/--backing-file against
# the *new* image's own directory (the scratch workdir below), not the
# caller's cwd — a relative QCOW would silently point at the wrong file.
QCOW="$(cd "$(dirname "${QCOW}")" && pwd)/$(basename "${QCOW}")"

SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
if [[ ! -f "${SSH_KEY}" ]]; then
  echo "run-vm: no ssh key at ${SSH_KEY}; run 'make ssh-key' first" >&2
  exit 1
fi
SSH_PORT="${SSH_PORT:-2222}"
MEMORY="${MEMORY:-4096}"
CPUS="${CPUS:-2}"
ARCH="${ARCH:-}"
if [[ -z "${ARCH}" ]]; then
  case "$(basename "${QCOW}")" in
    *-arm64.qcow2) ARCH=arm64 ;;
    *) ARCH=amd64 ;;
  esac
fi
case "${ARCH}" in
  amd64)
    QEMU_BINARY="${QEMU_BINARY:-qemu-system-x86_64}"
    MACHINE_TYPE=q35
    ;;
  arm64)
    QEMU_BINARY="${QEMU_BINARY:-qemu-system-aarch64}"
    MACHINE_TYPE=virt
    ;;
  *)
    echo "run-vm: unsupported ARCH: ${ARCH}" >&2
    exit 1
    ;;
esac

host_arch="$(uname -m)"
if [[ ( "${host_arch}" == x86_64 && "${ARCH}" == amd64 ) || ( "${host_arch}" == aarch64 && "${ARCH}" == arm64 ) ]] \
    && [[ -c /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-accel kvm -cpu host)
else
  echo "run-vm: no usable /dev/kvm, booting under software emulation (slower)" >&2
  ACCEL_ARGS=(-accel tcg -cpu max)
fi

WORKDIR="$(mktemp -d)"
DISK="${WORKDIR}/vm.qcow2"
cleanup() {
  if [[ -n "${SWTPM_PID:-}" ]]; then
    kill "${SWTPM_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

# Copy-on-write overlay: this script never mutates the qcow2 you pointed it at.
qemu-img create -f qcow2 -F qcow2 -b "${QCOW}" "${DISK}" >/dev/null

cat >"${WORKDIR}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
echo "instance-id: nke-run-vm-$$
local-hostname: nke-run-vm" >"${WORKDIR}/meta-data"
cloud-localds "${WORKDIR}/seed.iso" "${WORKDIR}/user-data" "${WORKDIR}/meta-data"

QEMU_ARGS=(
  -m "${MEMORY}" -smp "${CPUS}"
  "${ACCEL_ARGS[@]}"
  -nographic
  -drive "if=virtio,file=${DISK},format=qcow2"
  -drive "if=virtio,file=${WORKDIR}/seed.iso,format=raw"
  -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22"
  -device "virtio-net,netdev=net0"
)

# UEFI/OVMF: needed for the FDE/non-FDE variants, and q35 + an explicit cpu
# model to avoid the legacy "pc" (i440fx) + OVMF pairing that produced
# intermittent guest-kernel instability while developing those variants —
# see the same comment in demo-fde.sh.
if [[ -n "${OVMF_CODE:-}" ]]; then
  if [[ "${ARCH}" == arm64 ]]; then
    OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/AAVMF/AAVMF_VARS.fd}"
  else
    OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
  fi
  cp "${OVMF_VARS_TEMPLATE}" "${WORKDIR}/OVMF_VARS.fd"
  QEMU_ARGS=(
    -machine "type=${MACHINE_TYPE}"
    -drive "if=pflash,format=raw,readonly=on,file=${OVMF_CODE}"
    -drive "if=pflash,format=raw,file=${WORKDIR}/OVMF_VARS.fd"
    "${QEMU_ARGS[@]}"
  )
fi

SWTPM_PID=""
if [[ "${TPM:-}" == "1" ]]; then
  command -v swtpm >/dev/null 2>&1 || { echo "run-vm: TPM=1 requires swtpm" >&2; exit 3; }
  mkdir -p "${WORKDIR}/tpm"
  swtpm socket --tpmstate "dir=${WORKDIR}/tpm" \
    --ctrl "type=unixio,path=${WORKDIR}/tpm/swtpm-sock" \
    --tpm2 --daemon --pid "file=${WORKDIR}/tpm/swtpm.pid"
  SWTPM_PID="$(cat "${WORKDIR}/tpm/swtpm.pid")"
  QEMU_ARGS+=(
    -chardev "socket,id=chrtpm,path=${WORKDIR}/tpm/swtpm-sock"
    -tpmdev "emulator,id=tpm0,chardev=chrtpm"
    -device "tpm-tis,tpmdev=tpm0"
  )
fi

echo "run-vm: booting ${QCOW}"
echo "run-vm: once cloud-init finishes, ssh in with:"
echo "run-vm:   ssh -i ${SSH_KEY} -p ${SSH_PORT} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@127.0.0.1"
echo "run-vm: Ctrl-A then X stops the VM and cleans up (plain Ctrl-C won't -- see header comment)."
echo

# Not `exec`: the EXIT trap (swtpm teardown, workdir cleanup) needs this
# shell to still be around when qemu exits, whether from Ctrl-C or a guest
# shutdown.
"${QEMU_BINARY}" "${QEMU_ARGS[@]}"
