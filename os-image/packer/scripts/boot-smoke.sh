#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test: boot the qcow2 nke-node build produced and confirm it comes up
# clean (cloud-init completes, SSH answers, nke-agent and spire-agent units
# are installed and enabled). Does not exercise SPIRE/nke-agent against a real
# server -- just proves the golden image boots on its own.
#
# Set OVMF_CODE (and optionally OVMF_VARS_TEMPLATE) to boot a UEFI-installed
# image, e.g. the nke-node-non-fde.pkr.hcl / nke-node-fde.pkr.hcl variants:
#   QCOW=../output-non-fde/nke-node-non-fde OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./boot-smoke.sh
# Unset (the default), this boots BIOS-style, matching the base nke-node.pkr.hcl
# image. The FDE variant needs its own passphrase/TPM handling instead of
# this script -- see ../../demo-fde.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-${ROOT}/output/nke-node}"
SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
OVMF_CODE="${OVMF_CODE:-}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

# Same probe as packer/Makefile, and for the same reason: the CI runners have no
# /dev/kvm, so -enable-kvm makes QEMU exit instead of booting. Emulated boot is
# minutes rather than seconds, so the timeout moves with the accelerator.
if [[ -c /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-enable-kvm -cpu host)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"
else
  echo "boot-smoke: no usable /dev/kvm, booting under software emulation (slower)" >&2
  ACCEL_ARGS=(-accel tcg)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-1200}"
fi

if [[ ! -f "${QCOW}" ]]; then
  echo "boot-smoke: no image at ${QCOW}, run 'make os-image' first" >&2
  exit 1
fi
if [[ ! -f "${SSH_KEY}" ]]; then
  echo "boot-smoke: no ssh key at ${SSH_KEY}, run 'make ssh-key' first" >&2
  exit 1
fi

WORKDIR="$(mktemp -d)"
DISK="${WORKDIR}/nke-node-smoke.qcow2"
SEED="${WORKDIR}/seed.iso"
PIDFILE="${WORKDIR}/qemu.pid"

cleanup() {
  if [[ -f "${PIDFILE}" ]] && kill -0 "$(cat "${PIDFILE}")" 2>/dev/null; then
    kill "$(cat "${PIDFILE}")" 2>/dev/null || true
    wait "$(cat "${PIDFILE}")" 2>/dev/null || true
  fi
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

# Copy-on-write overlay so the smoke test never mutates the built artifact.
qemu-img create -f qcow2 -F qcow2 -b "${QCOW}" "${DISK}" >/dev/null

cat >"${WORKDIR}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
echo "instance-id: nke-boot-smoke
local-hostname: nke-boot-smoke" >"${WORKDIR}/meta-data"

cloud-localds "${SEED}" "${WORKDIR}/user-data" "${WORKDIR}/meta-data"

EFI_ARGS=()
if [[ -n "${OVMF_CODE}" ]]; then
  EFIVARS="${WORKDIR}/OVMF_VARS.fd"
  cp "${OVMF_VARS_TEMPLATE}" "${EFIVARS}"
  EFI_ARGS=(
    -drive if=pflash,format=raw,readonly=on,file="${OVMF_CODE}"
    -drive if=pflash,format=raw,file="${EFIVARS}"
  )
fi

echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT})"
qemu-system-x86_64 \
  -m 2048 -smp 2 \
  "${ACCEL_ARGS[@]}" \
  -nographic \
  "${EFI_ARGS[@]}" \
  -drive if=virtio,file="${DISK}",format=qcow2 \
  -drive if=virtio,file="${SEED}",format=raw \
  -netdev user,id=net0,hostfwd=tcp::"${SSH_PORT}"-:22 \
  -device virtio-net,netdev=net0 \
  -pidfile "${PIDFILE}" \
  &

deadline=$((SECONDS + BOOT_TIMEOUT))
# timeout wraps the ssh call itself: ConnectTimeout only bounds the SSH
# handshake, not 'cloud-init status --wait' actually returning on the guest.
# Without this, a guest that connects but never finishes cloud-init blocks
# forever inside the `until` condition -- the deadline check below never
# runs, since it's in the loop body, not the condition.
until timeout 30 ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o BatchMode=yes \
    ubuntu@127.0.0.1 'cloud-init status --wait' 2>/dev/null; do
  if (( SECONDS > deadline )); then
    echo "boot-smoke: VM did not become SSH-reachable within ${BOOT_TIMEOUT}s" >&2
    exit 1
  fi
  sleep 5
done

echo "boot-smoke: VM booted and completed cloud-init OK"

timeout 30 ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o BatchMode=yes \
    ubuntu@127.0.0.1 bash -s <<'EOF'
set -euo pipefail
systemctl is-enabled nke-agent.service
systemctl is-enabled spire-agent.service
systemctl is-enabled nke-spire-configure.service
test -x /usr/local/bin/nke-agent || test -x /usr/bin/nke-agent
test -x /usr/local/bin/spire-agent
test -x /usr/lib/nke/spire-configure.sh
test -x /usr/lib/nke/wait-spiffe-socket.sh
EOF

echo "boot-smoke: nke-agent and spire-agent are installed and enabled"
