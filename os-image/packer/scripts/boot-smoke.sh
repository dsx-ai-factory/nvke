#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test: boot the qcow2 nke-node build produced and confirm it comes up
# clean (cloud-init completes, SSH answers, nke-agent and spire-agent units
# are installed and enabled). Does not exercise SPIRE/nke-agent against a real
# server -- just proves the golden image boots on its own.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-${ROOT}/output/nke-node}"
SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"

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

echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT})"
qemu-system-x86_64 \
  -m 2048 -smp 2 \
  "${ACCEL_ARGS[@]}" \
  -nographic \
  -drive if=virtio,file="${DISK}",format=qcow2 \
  -drive if=virtio,file="${SEED}",format=raw \
  -netdev user,id=net0,hostfwd=tcp::"${SSH_PORT}"-:22 \
  -device virtio-net,netdev=net0 \
  -pidfile "${PIDFILE}" \
  &

deadline=$((SECONDS + BOOT_TIMEOUT))
until ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
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

ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
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
