#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test: boot a qcow2 the non-FDE build produced and confirm it comes up
# clean (cloud-init completes, SSH answers, nke-agent and spire-agent units
# are installed and enabled). Does not exercise SPIRE/nke-agent against a real
# server -- just proves the image boots on its own.
#
# QCOW defaults to the single *.qcow2 under output-non-fde/ (its filename
# encodes the Ubuntu version/arch, e.g. nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
# -- see vm_name in nke-node-non-fde.pkr.hcl). This variant is UEFI-installed,
# firmware is discovered automatically; override both paths together when necessary:
#   OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd \
#     OVMF_VARS_TEMPLATE=/usr/share/OVMF/OVMF_VARS_4M.fd ./boot-smoke.sh
# The FDE variant needs its own passphrase/TPM handling instead of this
# script -- see ../../demo-fde.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=qemu-arch.sh
source "${ROOT}/scripts/qemu-arch.sh"
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
nke_qemu_env "$(nke_arch_from_qcow "${QCOW}")"
OVMF_CODE="${OVMF_CODE:-${EFI_CODE_DEFAULT}}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-${EFI_VARS_DEFAULT}}"
ROOT_FS_LABEL="${ROOT_FS_LABEL:-cloudimg-rootfs}"
TARGET_DISK_SIZE="${TARGET_DISK_SIZE:-20G}"
if [[ "${NKE_ACCELERATED}" -eq 1 ]]; then
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"
else
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-2400}"
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

# Load the artifact into an independent target and make that target larger
# than the build disk. This exercises the same size change as a deployment to
# a larger physical disk and never mutates the source artifact.
qemu-img convert -f qcow2 -O qcow2 "${QCOW}" "${DISK}"
qemu-img resize "${DISK}" "${TARGET_DISK_SIZE}" >/dev/null
target_disk_bytes="$(qemu-img info --output=json "${DISK}" | awk '/"virtual-size"/ {gsub(/[^0-9]/, "", $0); print; exit}')"
if [[ -z "${target_disk_bytes}" ]]; then
  echo "boot-smoke: could not determine target disk size" >&2
  exit 1
fi

cat >"${WORKDIR}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
echo "instance-id: nke-boot-smoke
local-hostname: nke-boot-smoke" >"${WORKDIR}/meta-data"

cloud-localds "${SEED}" "${WORKDIR}/user-data" "${WORKDIR}/meta-data"

EFIVARS="${WORKDIR}/OVMF_VARS.fd"
install -m 0600 "${OVMF_VARS_TEMPLATE}" "${EFIVARS}"

echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT})"
"${QEMU_BIN}" \
  -m 2048 -smp 2 \
  -machine "type=${QEMU_MACHINE}" \
  -cpu "${QEMU_CPU}" \
  "${ACCEL_ARGS[@]}" \
  -nographic \
  -drive "if=pflash,format=raw,readonly=on,file=${OVMF_CODE}" \
  -drive "if=pflash,format=raw,file=${EFIVARS}" \
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
    ubuntu@127.0.0.1 bash -s -- "${ROOT_FS_LABEL}" "${target_disk_bytes}" <<'EOF'
set -euo pipefail
root_fs_label=$1
target_disk_bytes=$2
test "$(findmnt -n -o LABEL /)" = "${root_fs_label}"
systemctl is-active lvm-growth-firstboot.service
root_bytes=$(df -B1 --output=size / | tail -n 1 | tr -d ' ')
minimum_root_bytes=$((target_disk_bytes * 80 / 100))
test "${root_bytes}" -ge "${minimum_root_bytes}"
systemctl is-enabled nke-agent.service
systemctl is-enabled spire-agent.service
systemctl is-enabled nke-spire-configure.service
test -x /usr/local/bin/nke-agent
test -x /usr/local/bin/nke-agent-updater
# The mode bits say nothing about whether the binary runs: -version exits 0
# only if it matches the image's architecture and resolves its libraries.
/usr/local/bin/nke-agent -version
/usr/local/bin/nke-agent-updater -version
dpkg-query -W -f='${Status}\n' nke-agent | grep -Fxq 'install ok installed'
for path in /usr/local/bin/nke-agent /usr/local/bin/nke-agent-updater \
  /etc/systemd/system/nke-agent.service /etc/systemd/system/nke-agent-updater@.service; do
  test "$(dpkg-query -S "${path}" | cut -d: -f1)" = nke-agent
done
systemctl cat nke-agent.service | grep -Fx 'ExecStartPre=/usr/lib/nke/wait-spiffe-socket.sh'
test -x /usr/local/bin/spire-agent
test -x /usr/local/lib/spire-agent/tpm_attestor_agent
test -x /usr/local/bin/get_tpm_pubhash
test -x /usr/lib/nke/spire-configure.sh
test -x /usr/lib/nke/wait-spiffe-socket.sh
dpkg-query -W -f='${Status}\n' 'nke-cni-calico-manifests-*' | grep -Fxq 'install ok installed'
# The paths above would also pass against the raw-binary install this package
# replaced, so assert dpkg actually owns them.
dpkg-query -W -f='${Status}\n' spire-agent | grep -Fxq 'install ok installed'
test "$(dpkg-query -S /usr/local/bin/spire-agent | cut -d: -f1)" = spire-agent
# A unit left in /etc would mask the vendor unit the package ships in /lib.
test ! -e /etc/systemd/system/spire-agent.service
id -u spire >/dev/null
# A profile's agent.conf carries plugin_checksum = "${TPM_PLUGIN_CHECKSUM}"
# verbatim. Without both halves of this, SPIRE compares the plugin against that
# literal string, refuses to load it, and the node never attests.
test "$(. /etc/spire-agent/spire-agent.env; echo "${TPM_PLUGIN_CHECKSUM}")" \
    = "$(sha256sum /usr/local/lib/spire-agent/tpm_attestor_agent | cut -d' ' -f1)"
grep -Fq -- -expandEnv /lib/systemd/system/spire-agent.service
# Persistent upload scratch must not remain in the golden image.
test ! -e /var/tmp/nke-image

# The local apt repo has to offer what a node installs at cluster create.
# These are staged rather than installed, so nothing above would notice them
# missing: the arm64 image shipped with no kubeadm at all, and the failure
# only showed up inside a tenant guest, mid-provision.
# Not `| grep -q`: grep exits at the first match and SIGPIPEs apt-cache, which
# set -o pipefail above then reports as a failure.
have_in_local_repo() {
    [[ -n "$(apt-cache search --names-only "^$1-[0-9]")" ]] ||
        { echo "the image's local apt repo offers no $1-* package" >&2; return 1; }
}
have_in_local_repo nke-kubeadm-distro
have_in_local_repo nke-cni-calico-images

EOF

echo "boot-smoke: image contract and root-filesystem growth are satisfied"
