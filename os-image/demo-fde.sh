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
# covered by this script.
#
# Usage: ./demo-fde.sh [--help]
#
# Env overrides:
#   QCOW                 path to the FDE qcow2 (default: packer/output-fde/nke-node-fde)
#   SSH_KEY              ssh key used for the build (default: packer/.ssh/id_ed25519)
#   SSH_PORT             host port to forward to guest :22 (default: 2222)
#   BOOT_TIMEOUT         seconds to wait for SSH per boot (default: 180 kvm / 1200 tcg)
#   LUKS_BUILD_PASSPHRASE  build-time LUKS passphrase (default: changeit) — the shipped
#                        image only has a passphrase slot until TPM binding completes,
#                        so the very first real boot needs this (normally supplied via
#                        an iPXE luks.key= kernel parameter in production; here we type
#                        it at the console, same as a human would on first bring-up).

set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,23p' "${BASH_SOURCE[0]}"
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QCOW="${QCOW:-${ROOT}/packer/output-fde/nke-node-fde}"
SSH_KEY="${SSH_KEY:-${ROOT}/packer/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"

if [[ -c /dev/kvm && -w /dev/kvm ]]; then
  ACCEL_ARGS=(-enable-kvm -cpu host)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}"
else
  echo "demo: no usable /dev/kvm, booting under software emulation (slower)" >&2
  ACCEL_ARGS=(-accel tcg)
  BOOT_TIMEOUT="${BOOT_TIMEOUT:-1200}"
fi

for tool in qemu-system-x86_64 qemu-img swtpm cloud-localds ssh expect; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "demo: required tool '${tool}' not found" >&2; exit 3; }
done
LUKS_BUILD_PASSPHRASE="${LUKS_BUILD_PASSPHRASE:-changeit}"
if [[ ! -f "${QCOW}" ]]; then
  echo "demo: no image at ${QCOW}; run 'make -C packer os-image-fde' first" >&2
  exit 1
fi
if [[ ! -f "${SSH_KEY}" ]]; then
  echo "demo: no ssh key at ${SSH_KEY}; run 'make -C packer ssh-key' first" >&2
  exit 1
fi

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
DISK="${WORKDIR}/nke-node-fde-demo.qcow2"
trap 'rm -rf "${WORKDIR}"' EXIT

qemu-img create -f qcow2 -F qcow2 -b "${QCOW}" "${DISK}" >/dev/null

RESULTS=()
report() {
  local scenario="$1" verdict="$2" detail="${3:-}"
  RESULTS+=("${verdict}: ${scenario}${detail:+ — ${detail}}")
  echo "[${verdict}] ${scenario}${detail:+ — ${detail}}"
}

ssh_guest() {
  ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o BatchMode=yes \
    ubuntu@127.0.0.1 "$@"
}

# Boots $DISK against the swtpm state dir at $1, waits up to BOOT_TIMEOUT for
# SSH. Prints "up" or "timeout" to stdout; callers decide what that means.
#
# needs_passphrase=1 types LUKS_BUILD_PASSPHRASE at the console if a "Please
# unlock disk" prompt appears. Only the very first real boot of a freshly
# built image needs this: the shipped image ships with only the passphrase
# slot (the build-time keyfile was already removed by configure-fde.sh), and
# luks-tpm-firstboot.service — which performs the TPM bind — only runs AFTER
# that first unlock succeeds and the root filesystem is mounted. Production
# supplies this passphrase via an iPXE luks.key= kernel parameter; here we
# type it at the console the same way a human would on first bring-up. A
# boot against the wrong TPM (scenario 2) must NOT get this treatment, or it
# would unlock via passphrase and mask the "is this actually TPM-sealed"
# check — every other boot in this script relies on the TPM alone.
boot_and_wait() {
  local tpmdir="$1" needs_passphrase="${2:-0}" runwork
  runwork="$(mktemp -d)"
  local efivars="${runwork}/OVMF_VARS.fd"
  cp "${OVMF_VARS_TEMPLATE}" "${efivars}"

  cat >"${runwork}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
  echo "instance-id: nke-fde-demo-$$-${RANDOM}
local-hostname: nke-fde-demo" >"${runwork}/meta-data"
  cloud-localds "${runwork}/seed.iso" "${runwork}/user-data" "${runwork}/meta-data"

  rm -f "${tpmdir}/swtpm-sock" "${tpmdir}/.lock"
  swtpm socket --tpmstate "dir=${tpmdir}" \
    --ctrl "type=unixio,path=${tpmdir}/swtpm-sock" \
    --tpm2 --daemon --pid "file=${tpmdir}/swtpm.pid"

  local qemu_args=(
    -m 4096 -smp 2
    "${ACCEL_ARGS[@]}"
    -nographic
    -drive if=pflash,format=raw,readonly=on,file="${OVMF_CODE}"
    -drive if=pflash,format=raw,file="${efivars}"
    -drive if=virtio,file="${DISK}",format=qcow2
    -drive if=virtio,file="${runwork}/seed.iso",format=raw
    -chardev "socket,id=chrtpm,path=${tpmdir}/swtpm-sock"
    -tpmdev emulator,id=tpm0,chardev=chrtpm
    -device tpm-tis,tpmdev=tpm0
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22"
    -device virtio-net,netdev=net0
  )

  local qpid
  if [[ "${needs_passphrase}" == "1" ]]; then
    # expect owns qemu's pty (via -nographic, serial is multiplexed onto
    # stdio) so it can type the passphrase the moment the prompt appears,
    # then just relays everything else through to the log until exit/timeout.
    # Plain (unquoted) interpolation is deliberate: expect/Tcl does its own
    # whitespace word-splitting on the spawn line, not shell-style quote
    # parsing, so bash quoting (e.g. ${var@Q}) would pass literal quote
    # characters through to qemu's argv instead of being stripped. This is
    # only safe because none of these values (temp-dir paths, -netdev
    # strings, etc.) contain internal whitespace — same assumption the rest
    # of this script already makes about its paths.
    cat >"${runwork}/unlock.exp" <<EOF
set timeout ${BOOT_TIMEOUT}
log_file -a "${runwork}/qemu.log"
spawn qemu-system-x86_64 ${qemu_args[@]}
expect {
  "Please unlock disk" { send "${LUKS_BUILD_PASSPHRASE}\r"; exp_continue }
  timeout { exit 0 }
  eof { exit 0 }
}
EOF
    expect "${runwork}/unlock.exp" >/dev/null 2>&1 &
    local expect_pid=$!
    echo "${expect_pid}" > "${runwork}/expect.pid"
  else
    qemu-system-x86_64 "${qemu_args[@]}" >"${runwork}/qemu.log" 2>&1 &
    qpid=$!
    echo "${qpid}" > "${runwork}/qemu.pid"
  fi

  local deadline=$((SECONDS + BOOT_TIMEOUT)) up=0
  until ssh_guest true 2>/dev/null; do
    if (( SECONDS > deadline )); then
      up=1
      break
    fi
    sleep 5
  done

  # pkill by disk path rather than tracking expect's qemu child pid — expect
  # forks qemu as a grandchild, so the recorded expect_pid alone won't reach it.
  pkill -f "qemu-system-x86_64.*${DISK}" 2>/dev/null || true
  if [[ -f "${tpmdir}/swtpm.pid" ]] && kill -0 "$(cat "${tpmdir}/swtpm.pid")" 2>/dev/null; then
    kill "$(cat "${tpmdir}/swtpm.pid")" 2>/dev/null || true
  fi
  rm -rf "${runwork}"

  [[ "${up}" -eq 0 ]] && echo "up" || echo "timeout"
}

echo "=== Scenario: host volumes are LUKS2, sealed to the node TPM ==="
TPM_A="${WORKDIR}/tpm-a"
mkdir -p "${TPM_A}"
STATE="$(boot_and_wait "${TPM_A}" 1)"
if [[ "${STATE}" != "up" ]]; then
  report "host-volumes-luks2-tpm-sealed" "FAIL" "VM did not become SSH-reachable within ${BOOT_TIMEOUT}s (first-boot TPM bind may have failed)"
else
  if ssh_guest 'test -f /root/.luks-tpm-firstboot-done' 2>/dev/null; then
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
fi

echo "=== Scenario: an encrypted volume cannot be read off the node ==="
TPM_B="${WORKDIR}/tpm-b"
mkdir -p "${TPM_B}"
STATE="$(boot_and_wait "${TPM_B}")"
if [[ "${STATE}" == "timeout" ]]; then
  report "moved-to-different-tpm-fails" "PASS" "boot against a different TPM did not unlock within ${BOOT_TIMEOUT}s, as expected"
else
  report "moved-to-different-tpm-fails" "FAIL" "boot against a different TPM unexpectedly reached SSH — the LUKS key is not actually TPM-bound"
fi

echo "=== Scenario: encryption survives node replacement (fresh boot, same TPM, still unlocks) ==="
STATE="$(boot_and_wait "${TPM_A}")"
if [[ "${STATE}" == "up" ]]; then
  report "replacement-node-fresh-seal" "PASS" "a later boot against the original TPM still auto-unlocks (stand-in for a replacement node sealing its own key)"
else
  report "replacement-node-fresh-seal" "FAIL" "a later boot against the original TPM failed to auto-unlock within ${BOOT_TIMEOUT}s"
fi

echo
echo "=== Summary ==="
FAILED=0
for r in "${RESULTS[@]}"; do
  echo "${r}"
  [[ "${r}" == FAIL:* ]] && FAILED=1
done

exit "${FAILED}"
