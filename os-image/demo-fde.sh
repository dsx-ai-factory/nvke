#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Runs the host-FDE (full disk encryption) acceptance scenarios against a
# built FDE image, under QEMU with swtpm as the virtual TPM. Prints
# pass/fail per scenario and exits non-zero if any scenario fails.
#
# Scope: os-image/ Linux host disk encryption only. Kubernetes Secrets/etcd
# encryption-at-rest is a separate controller/agent concern and is not
# covered by this script. The first boot also runs
# packer/scripts/check-conntrack.sh, since the FDE image gets the same
# conntrack tuning as the non-FDE one.
#
# Usage: ./demo-fde.sh [--help]
#
# Env overrides:
#   QCOW          path to the FDE qcow2 (default: the single *.qcow2 under
#                 packer/output-fde/, e.g. nvke-node-fde-ubuntu-26.04.1-amd64.qcow2)
#   ARCH          amd64 or arm64; must match the qcow2 (default: amd64)
#   SSH_KEY       ssh key used for the build (default: packer/.ssh/id_ed25519)
#   SSH_PORT      host port to forward to guest :22 (default: 2222)
#   BOOT_TIMEOUT  seconds to wait for SSH per boot (default: 360 kvm / 1800 tcg)
#   LOCKED_GRACE  seconds the wrong-TPM boot must sit at the LUKS prompt to
#                 count as locked (default: 60 kvm / 300 tcg)
#   CPU_MODEL     QEMU -cpu model (default: host kvm / max tcg)
#
# No interactive/blind console typing is needed: autoinstall-lvm.pkrtpl bakes
# a build-time luks.key=<passphrase> GRUB parameter, so even the very first
# real boot of a freshly built image unlocks itself via
# files/luks/luks-cmdline-script — the same mechanism production uses via an
# iPXE luks.key= kernel parameter. luks-tpm-firstboot.sh strips luks.key=
# from GRUB once the TPM bind succeeds, so a later boot against a different
# TPM (scenario 2) has no fallback and correctly fails to unlock.

set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  # The header comment, however long it grows.
  awk 'NR > 1 && !/^#/ { exit } NR > 1' "${BASH_SOURCE[0]}"
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QCOW="${QCOW:-}"
if [[ -z "${QCOW}" ]]; then
  # The output filename encodes the Ubuntu version/arch (see vm_name in
  # nvke-node-fde.pkr.hcl), so glob for it instead of hardcoding it here.
  shopt -s nullglob
  fde_qcows=("${ROOT}/packer/output-fde"/*.qcow2)
  shopt -u nullglob
  if [[ ${#fde_qcows[@]} -eq 1 ]]; then
    QCOW="${fde_qcows[0]}"
  else
    echo "demo-fde: QCOW not set and found ${#fde_qcows[@]} qcow2 files under packer/output-fde/ (need exactly 1); run 'make os-image-fde' or set QCOW explicitly" >&2
    exit 1
  fi
fi
SSH_KEY="${SSH_KEY:-${ROOT}/packer/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
# amd64 or arm64. Must match the qcow2 under test -- see the same
# ARCH-vs-host-arch reasoning in packer/Makefile's ACCELERATOR probe.
ARCH="${ARCH:-amd64}"

# machine_type/cpu_model matter here for the same reason they do in
# nvke-node-fde.pkr.hcl: this image boots via UEFI, and on amd64 QEMU's default
# "pc" (i440fx) machine type is a known-fragile pairing with OVMF that
# produced intermittent guest-kernel instability while developing this
# variant. q35 + an explicit cpu_model avoids it. aarch64 has no default
# machine at all and must be told "virt".
#
# The TPM device differs too: "tpm-tis" is the x86 ISA device, arm64's virt
# needs the MMIO one, "tpm-tis-device".
case "${ARCH}" in
  amd64)
    QEMU_BIN=qemu-system-x86_64
    ARCH_UNAME=x86_64
    MACHINE_ARGS=(-machine type=q35)
    TPM_DEVICE=tpm-tis
    DEFAULT_OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
    DEFAULT_OVMF_VARS=/usr/share/OVMF/OVMF_VARS_4M.fd
    ;;
  arm64)
    QEMU_BIN=qemu-system-aarch64
    ARCH_UNAME=aarch64
    MACHINE_ARGS=(-machine virt)
    TPM_DEVICE=tpm-tis-device
    # AAVMF is arm64's UEFI firmware, the equivalent of amd64's OVMF.
    DEFAULT_OVMF_CODE=/usr/share/AAVMF/AAVMF_CODE.fd
    DEFAULT_OVMF_VARS=/usr/share/AAVMF/AAVMF_VARS.fd
    ;;
  *)
    echo "demo: unsupported ARCH=${ARCH} (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

# Host arch too, not just /dev/kvm: KVM only accelerates a same-arch guest.
if [[ "$(uname -m)" == "${ARCH_UNAME}" && -c /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-enable-kvm)
  CPU_MODEL="${CPU_MODEL:-host}"
  # The first real boot does partition/LUKS/PV/LV growth, a TPM bind+verify,
  # and two full initramfs regens before it ever reaches cloud-init/sshd —
  # comfortably past 180s in practice, so the default here is generous
  # rather than tuned to the fast-path boots.
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-360}"
  LOCKED_GRACE="${LOCKED_GRACE:-60}"
else
  echo "demo: no usable same-arch /dev/kvm, booting under software emulation (slower)" >&2
  ACCEL_ARGS=(-accel tcg)
  # Same knob as packer/Makefile: on arm64 a bare "max" turns on pointer auth
  # with QARMA5, which tcg emulates far too slowly; impdef keeps it cheap.
  if [[ "${ARCH}" == "arm64" ]]; then
    CPU_MODEL="${CPU_MODEL:-max,pauth-impdef=on}"
  else
    CPU_MODEL="${CPU_MODEL:-max}"
  fi
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-1800}"
  LOCKED_GRACE="${LOCKED_GRACE:-300}"
fi

for tool in "${QEMU_BIN}" qemu-img swtpm cloud-localds ssh; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "demo: required tool '${tool}' not found" >&2; exit 3; }
done
if [[ ! -f "${QCOW}" ]]; then
  echo "demo: no image at ${QCOW}; run 'make -C packer os-image-fde' first" >&2
  exit 1
fi
if [[ ! -f "${SSH_KEY}" ]]; then
  echo "demo: no ssh key at ${SSH_KEY}; run 'make -C packer ssh-key' first" >&2
  exit 1
fi

OVMF_CODE="${OVMF_CODE:-${DEFAULT_OVMF_CODE}}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-${DEFAULT_OVMF_VARS}}"

WORKDIR="$(mktemp -d)"
DISK="${WORKDIR}/nvke-node-fde-demo.qcow2"
trap 'rm -rf "${WORKDIR}"' EXIT

qemu-img create -f qcow2 -F qcow2 -b "${QCOW}" "${DISK}" >/dev/null

RESULTS=()
report() {
  local scenario="$1" verdict="$2" detail="${3:-}"
  RESULTS+=("${verdict}: ${scenario}${detail:+ — ${detail}}")
  echo "[${verdict}] ${scenario}${detail:+ — ${detail}}"
}

ssh_guest() {
  # timeout, not just ConnectTimeout: ConnectTimeout only bounds the SSH
  # handshake, not the remote command actually returning. Several callers
  # use this inside `until ssh_guest ...; do ...; done` retry loops with
  # their own deadline check in the loop body -- a remote command that
  # connects but never returns would block forever in the loop condition,
  # where that deadline check never runs.
  timeout 30 ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o BatchMode=yes \
    ubuntu@127.0.0.1 "$@"
}

# Boots $DISK against the swtpm state dir at $1 and leaves it running. Sets
# VM_RUNWORK/VM_TPMDIR/VM_QPID for wait_ssh/stop_vm to use — deliberately
# does NOT tear the VM down itself, so a caller that needs to run more than
# one ssh_guest command after boot (scenario 1's firstboot-marker and
# luks-tpm-verify checks) has a still-live VM to run them against. Call
# stop_vm once done with a given boot.
VM_RUNWORK=""
VM_TPMDIR=""
VM_QPID=""
start_vm() {
  local tpmdir="$1"
  VM_TPMDIR="${tpmdir}"
  VM_RUNWORK="$(mktemp -d)"
  local runwork="${VM_RUNWORK}"
  local efivars="${runwork}/OVMF_VARS.fd"
  cp "${OVMF_VARS_TEMPLATE}" "${efivars}"

  cat >"${runwork}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
  echo "instance-id: nvke-fde-demo-$$-${RANDOM}
local-hostname: nvke-fde-demo" >"${runwork}/meta-data"
  cloud-localds "${runwork}/seed.iso" "${runwork}/user-data" "${runwork}/meta-data"

  rm -f "${tpmdir}/swtpm-sock" "${tpmdir}/.lock"
  swtpm socket --tpmstate "dir=${tpmdir}" \
    --ctrl "type=unixio,path=${tpmdir}/swtpm-sock" \
    --tpm2 --daemon --pid "file=${tpmdir}/swtpm.pid"

  local qemu_args=(
    "${MACHINE_ARGS[@]}"
    -cpu "${CPU_MODEL}"
    -m 4096 -smp 2
    "${ACCEL_ARGS[@]}"
    -nographic
    -drive if=pflash,format=raw,readonly=on,file="${OVMF_CODE}"
    -drive if=pflash,format=raw,file="${efivars}"
    -drive if=virtio,file="${DISK}",format=qcow2
    -drive if=virtio,file="${runwork}/seed.iso",format=raw
    -chardev "socket,id=chrtpm,path=${tpmdir}/swtpm-sock"
    -tpmdev emulator,id=tpm0,chardev=chrtpm
    -device "${TPM_DEVICE},tpmdev=tpm0"
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22"
    -device virtio-net,netdev=net0
  )

  "${QEMU_BIN}" "${qemu_args[@]}" >"${runwork}/qemu.log" 2>&1 &
  VM_QPID=$!
  echo "${VM_QPID}" > "${runwork}/qemu.pid"
}

# Polls for SSH up to BOOT_TIMEOUT. Prints "up" or "timeout" to stdout.
wait_ssh() {
  local deadline=$((SECONDS + BOOT_TIMEOUT)) up=0
  until ssh_guest true 2>/dev/null; do
    if (( SECONDS > deadline )); then
      up=1
      break
    fi
    sleep 5
  done
  [[ "${up}" -eq 0 ]] && echo "up" || echo "timeout"
}

# For a boot that should NOT unlock: watches the serial console instead of
# waiting out BOOT_TIMEOUT for SSH that never comes. cryptroot prints its
# passphrase prompt even while clevis is still unlocking in the background,
# so the prompt alone proves nothing; LOCKED_GRACE seconds must pass after it
# without the guest reaching systemd in the real root, answering SSH, or
# panicking or dropping to the initramfs shell. Prints "locked", "unlocked", "died" (qemu
# exited), "panic" (kernel panic on the console), "initramfs" (dropped to the
# initramfs emergency shell), or "timeout" (prompt never seen and no SSH).
# Only "locked" is a pass; the rest prove nothing.
wait_locked() {
  local log="${VM_RUNWORK}/qemu.log"
  local deadline=$((SECONDS + BOOT_TIMEOUT)) prompt_at=""
  while (( SECONDS <= deadline )); do
    # systemd's banner in the real root. It colors the OS name, so an escape
    # sequence sits between "Welcome to " and "Ubuntu". A bare "Welcome to"
    # also matches GRUB's, before the disk is even opened.
    if grep -aq 'Welcome to .*Ubuntu' "${log}" || ssh_guest true 2>/dev/null; then
      echo "unlocked"
      return
    fi
    if ! kill -0 "${VM_QPID}" 2>/dev/null; then
      echo "died"
      return
    fi
    if grep -aq 'Kernel panic' "${log}"; then
      echo "panic"
      return
    fi
    # initramfs-tools prints "ALERT! ... Dropping to a shell!" when it gives
    # up, which would otherwise sit quietly past LOCKED_GRACE like a prompt.
    if grep -aq 'ALERT!' "${log}"; then
      echo "initramfs"
      return
    fi
    if [[ -z "${prompt_at}" ]] && grep -aq 'Please unlock disk' "${log}"; then
      prompt_at="${SECONDS}"
      grep -a -m1 'Please unlock disk' "${log}" >&2
    fi
    if [[ -n "${prompt_at}" ]] && (( SECONDS - prompt_at >= LOCKED_GRACE )); then
      echo "locked"
      return
    fi
    sleep 5
  done
  echo "timeout"
}

# Tears down the VM started by the most recent start_vm.
stop_vm() {
  kill "${VM_QPID}" 2>/dev/null || true
  # Reap it, so the next boot's hostfwd can bind SSH_PORT. A qemu stuck on
  # shutdown gets 30s before SIGKILL, rather than hanging until the job timeout.
  local i
  for (( i = 0; i < 30; i++ )); do
    kill -0 "${VM_QPID}" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "${VM_QPID}" 2>/dev/null; then
    kill -9 "${VM_QPID}" 2>/dev/null || true
  fi
  wait "${VM_QPID}" 2>/dev/null || true
  # Fallback by disk path in case $VM_QPID somehow isn't qemu's own pid (e.g.
  # a future refactor backgrounds it indirectly again).
  pkill -f "${QEMU_BIN}.*${DISK}" 2>/dev/null || true
  local swtpm_pid
  swtpm_pid="$(cat "${VM_TPMDIR}/swtpm.pid" 2>/dev/null)" || true
  if [[ -n "${swtpm_pid}" ]] && kill -0 "${swtpm_pid}" 2>/dev/null; then
    kill "${swtpm_pid}" 2>/dev/null || true
  fi
  rm -rf "${VM_RUNWORK}"
}

echo "=== Scenario: host volumes are LUKS2, sealed to the node TPM ==="
TPM_A="${WORKDIR}/tpm-a"
mkdir -p "${TPM_A}"
start_vm "${TPM_A}"
STATE="$(wait_ssh)"
if [[ "${STATE}" != "up" ]]; then
  report "host-volumes-luks2-tpm-sealed" "FAIL" "VM did not become SSH-reachable within ${BOOT_TIMEOUT}s (first-boot TPM bind may have failed)"
else
  # VM is still running here (stop_vm hasn't been called yet) — these
  # checks need a live guest, unlike the pass/fail-only scenarios below.
  #
  # sshd/cloud-init's SSH-key injection isn't ordered against
  # luks-tpm-firstboot.service, so SSH can (and does, in practice) become
  # reachable a bit before that service — still mid partition-grow / dual
  # initramfs-regen — finishes. Poll for the marker rather than assuming
  # SSH-up means firstboot-done.
  FIRSTBOOT_DEADLINE=$((SECONDS + 180))
  until ssh_guest 'sudo test -f /root/.luks-tpm-firstboot-done' 2>/dev/null; do
    if (( SECONDS > FIRSTBOOT_DEADLINE )); then
      break
    fi
    sleep 5
  done
  if ssh_guest 'sudo test -f /root/.luks-tpm-firstboot-done' 2>/dev/null; then
    if ssh_guest 'sudo /usr/local/bin/luks-tpm-verify' 2>/dev/null; then
      SLOTS="$(ssh_guest "LUKS_DEV=\$(lsblk -ln -o NAME,FSTYPE | grep crypto_LUKS | awk '{print \"/dev/\" \$1}' | head -1); sudo cryptsetup luksDump \"\${LUKS_DEV}\" | grep -cE '^[[:space:]]+[0-9]+: luks2'" 2>/dev/null || echo "?")"
      if [[ "${SLOTS}" == "1" ]]; then
        report "host-volumes-luks2-tpm-sealed" "PASS" "TPM-bound, unsealable now, passphrase slot removed (1 active key slot)"
      else
        report "host-volumes-luks2-tpm-sealed" "FAIL" "expected exactly 1 key slot (TPM-only) after bind, got: ${SLOTS}"
      fi
    else
      report "host-volumes-luks2-tpm-sealed" "FAIL" "luks-tpm-verify reported an unsealable or missing TPM binding"
    fi
  else
    report "host-volumes-luks2-tpm-sealed" "FAIL" "/root/.luks-tpm-firstboot-done missing — first-boot binding never completed"
  fi

  # provision.sh gives the FDE image the same conntrack tuning, so check it on
  # this boot, with the same script boot-smoke.sh runs on the non-FDE image.
  if CONNTRACK="$(ssh_guest bash -s <"${ROOT}/packer/scripts/check-conntrack.sh" 2>&1)"; then
    report "conntrack-limits-sized-to-node" "PASS" "${CONNTRACK##*check-conntrack: }"
  else
    report "conntrack-limits-sized-to-node" "FAIL" "${CONNTRACK##*check-conntrack: }"
  fi
fi
stop_vm

echo "=== Scenario: an encrypted volume cannot be read off the node ==="
TPM_B="${WORKDIR}/tpm-b"
mkdir -p "${TPM_B}"
start_vm "${TPM_B}"
STATE="$(wait_locked)"
stop_vm
case "${STATE}" in
  locked) report "moved-to-different-tpm-fails" "PASS" "boot against a different TPM stayed at the LUKS passphrase prompt for ${LOCKED_GRACE}s, as expected" ;;
  unlocked) report "moved-to-different-tpm-fails" "FAIL" "boot against a different TPM unexpectedly unlocked the root — the LUKS key is not actually TPM-bound" ;;
  *) report "moved-to-different-tpm-fails" "FAIL" "inconclusive: boot against a different TPM ended in state '${STATE}' without reaching the LUKS prompt and staying locked" ;;
esac

echo "=== Scenario: encryption survives node replacement (fresh boot, same TPM, still unlocks) ==="
# Also the control for the scenario above, whose early pass assumes a real
# unlock lands within LOCKED_GRACE of the prompt. Watching this boot the same
# way checks that assumption instead of trusting it.
start_vm "${TPM_A}"
STATE="$(wait_locked)"
[[ "${STATE}" == "unlocked" ]] && STATE="$(wait_ssh)"
stop_vm
case "${STATE}" in
  up) report "replacement-node-fresh-seal" "PASS" "a later boot against the original TPM still auto-unlocks (stand-in for a replacement node sealing its own key)" ;;
  locked) report "replacement-node-fresh-seal" "FAIL" "a boot against the original TPM sat at the passphrase prompt for ${LOCKED_GRACE}s; either the binding is broken or LOCKED_GRACE is too short to tell a wrong TPM from a slow unlock, so moved-to-different-tpm-fails proves nothing" ;;
  *) report "replacement-node-fresh-seal" "FAIL" "a later boot against the original TPM failed to auto-unlock (state: ${STATE}, BOOT_TIMEOUT ${BOOT_TIMEOUT}s)" ;;
esac

echo
echo "=== Summary ==="
FAILED=0
for r in "${RESULTS[@]}"; do
  echo "${r}"
  [[ "${r}" == FAIL:* ]] && FAILED=1
done

exit "${FAILED}"
