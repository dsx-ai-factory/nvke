#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test: boot a qcow2 the non-FDE build produced and confirm it comes up
# clean (cloud-init completes, SSH answers, the Forge root-filesystem label is
# present, and nke-agent and spire-agent units are installed and enabled). Does
# not exercise SPIRE/nke-agent against a real server -- just proves the image
# boots on its own.
#
# QCOW defaults to the single *.qcow2 under output-non-fde/ (its filename
# encodes the Ubuntu version/arch, e.g. nke-node-non-fde-ubuntu-26.04-amd64.qcow2
# -- see vm_name in nke-node-non-fde.pkr.hcl). This variant is UEFI-installed,
# so OVMF_CODE (and optionally OVMF_VARS_TEMPLATE) is required:
#   OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./boot-smoke.sh
# ARCH is inferred from the qcow2 filename. Set ARCH=arm64 for an image whose
# filename does not end in -arm64.qcow2.
# The FDE variant needs its own passphrase/TPM handling instead of this
# script -- see ../../demo-fde.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-}"
if [[ -z "${QCOW}" ]]; then
  shopt -s nullglob
  non_fde_qcows=("${ROOT}/output-non-fde"/*.qcow2)
  shopt -u nullglob
  if [[ ${#non_fde_qcows[@]} -eq 1 ]]; then
    QCOW="${non_fde_qcows[0]}"
  else
    echo "boot-smoke: QCOW not set and found ${#non_fde_qcows[@]} qcow2 files under output-non-fde/ (need exactly 1); run 'make os-image-non-fde' or set QCOW explicitly" >&2
    exit 1
  fi
fi
SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
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
    MACHINE_ARGS=()
    ;;
  arm64)
    QEMU_BINARY="${QEMU_BINARY:-qemu-system-aarch64}"
    MACHINE_ARGS=(-machine virt)
    ;;
  *)
    echo "boot-smoke: unsupported ARCH: ${ARCH}" >&2
    exit 1
    ;;
esac
OVMF_CODE="${OVMF_CODE:-}"
if [[ "${ARCH}" == arm64 ]]; then
  OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/AAVMF/AAVMF_VARS.fd}"
else
  OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
fi

# Same probe as packer/Makefile, and for the same reason: the CI runners have no
# /dev/kvm, so KVM acceleration would make QEMU exit instead of booting. KVM
# also requires matching host and guest architectures. Emulated boot is minutes
# rather than seconds, so the timeout moves with the accelerator.
host_arch="$(uname -m)"
if [[ ( "${host_arch}" == x86_64 && "${ARCH}" == amd64 ) || ( "${host_arch}" == aarch64 && "${ARCH}" == arm64 ) ]] \
    && [[ -c /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-accel kvm -cpu host)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"
else
  echo "boot-smoke: no usable /dev/kvm, booting under software emulation (slower)" >&2
  ACCEL_ARGS=(-accel tcg -cpu max)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-1200}"
fi

if [[ ! -f "${QCOW}" ]]; then
  echo "boot-smoke: no image at ${QCOW}, run 'make os-image-non-fde' first" >&2
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
    -drive "if=pflash,format=raw,readonly=on,file=${OVMF_CODE}"
    -drive "if=pflash,format=raw,file=${EFIVARS}"
  )
fi

echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT})"
"${QEMU_BINARY}" \
  -m 2048 -smp 2 \
  "${MACHINE_ARGS[@]}" \
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
test "$(findmnt -n -o LABEL /)" = cloudimg-rootfs
systemctl is-enabled nke-agent.service
systemctl is-enabled spire-agent.service
systemctl is-enabled nke-spire-configure.service
systemd-analyze verify \
  /etc/systemd/system/nke-spire-configure.service \
  /etc/systemd/system/spire-agent.service \
  /etc/systemd/system/nke-agent.service
test ! -e /etc/nke/spire.env
! systemctl is-active --quiet spire-agent.service
! systemctl is-active --quiet nke-agent.service
test -x /usr/local/bin/nke-agent || test -x /usr/bin/nke-agent
test -x /usr/local/bin/nke-os-image-check
test -x /usr/local/bin/spire-agent
test -x /usr/local/lib/spire-agent/tpm_attestor_agent
test -x /usr/local/bin/get_tpm_pubhash
test -x /usr/lib/nke/spire-configure.sh
test -x /usr/lib/nke/configure-nke-agent-plaintext.sh
test -x /usr/lib/nke/wait-spiffe-socket.sh
test -f /usr/lib/nke/spire-agent-tpm.conf.tmpl
dpkg-query -W -f='${Status}\n' nke-cni-calico-manifests-3.30.1 | grep -Fxq 'install ok installed'

# Exercise the no-identity first-boot transition. The helper must remove the
# SPIRE-only unit gates, start nke-agent, and serve a real plaintext gRPC call.
sudo /usr/lib/nke/configure-nke-agent-plaintext.sh
for attempt in $(seq 1 12); do
  if /usr/local/bin/nke-os-image-check \
      --plaintext --endpoint 127.0.0.1:15005 --timeout 5s; then
    break
  fi
  if [[ "${attempt}" == 12 ]]; then
    sudo systemctl --no-pager --full status nke-agent.service || true
    sudo journalctl --no-pager -u nke-agent.service -n 50 || true
    exit 1
  fi
  sleep 5
done
systemctl is-active --quiet nke-agent.service
EOF

echo "boot-smoke: agents, TPM tools, Calico manifests, and plaintext mode are operational"
