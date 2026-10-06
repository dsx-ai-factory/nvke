#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# teardown-ci-host.sh -- collect diagnostics, then undo setup-ci-host.sh.
#
# WHY THIS EXISTS
#
#   Two jobs, in this order:
#
#   1. Salvage. This lane is a probe: it is expected to fail on its first runs,
#      and the answer is usually inside a guest, not in the Go test output. The
#      serial-console logs libvirt writes per VM (domain_xml.go:105-116, one
#      *.ttyS0.log and one *.ttyS1.log next to each disk overlay) are the only
#      record of what cloud-init and kubeadm did in the VM, and they live under
#      a work dir that is destroyed with the runner. This script copies them,
#      plus host networking state, into one directory for actions/upload-artifact.
#
#   2. Undo. Strictly speaking a single-use runner needs no cleanup at all --
#      but a teardown that runs on every attempt keeps a local run, a re-run on
#      the same machine, and a future long-lived runner all behaving the same.
#
#   Everything here is best effort: `set -uo pipefail` with NO `-e`, and the
#   script always exits 0. It runs from an `if: always()` step, after a step
#   that has usually already failed; a teardown that fails the job a second time
#   would replace the real error in the run summary with its own.
#
# Not undone, deliberately, mirroring teardown-linux.sh:12-14: installed
# packages and net.ipv4.ip_forward. Both are harmless to leave and are reused
# by the next setup-ci-host.sh run.
#
# Not needed here, unlike teardown-linux.sh:115-122: the BGP speaker's blackhole
# VIP routes. launch.go:178 runs nico-vms with NICO_VMS_BGP_ENABLED=false, so
# the speaker never starts and installs nothing.
#
# Usage:
#   bash controller/test/kubeadm/scripts/teardown-ci-host.sh
#
# Environment (all optional; defaults match setup-ci-host.sh):
#   NICO_VMS_BRIDGE_NAME       bridge to remove          (default cwebr0)
#   NICO_VMS_BRIDGE_CIDR       bridge subnet             (default 172.30.0.0/24)
#   NICO_VMS_LIBVIRT_URI       libvirt connection        (default qemu:///session)
#   NICO_VMS_DHCP_HOSTS_DIR    dnsmasq hostsdir          (default /var/lib/nico-vms/dhcp-hosts)
#   NICO_VMS_DHCP_LEASE_FILE   dnsmasq lease file        (default /var/run/nico-vms-dnsmasq.leases)
#   NICO_SIM_DB_PATH           nico-vms SQLite path; its directory is where the
#                              console logs are searched for first, because
#                              config.go:310-312 puts the per-VM work dirs at
#                              <dirname(NICO_SIM_DB_PATH)>/instances/<id>/
#   NKE_BRINGUP_ARTIFACT_DIR   where to write the bundle
#                              (default <workspace>/.ci-artifacts/kubeadm-bringup)

set -uo pipefail

log() { printf '[teardown-ci-host] %s\n' "$*"; }

BRIDGE_NAME="${NICO_VMS_BRIDGE_NAME:-cwebr0}"
BRIDGE_CIDR="${NICO_VMS_BRIDGE_CIDR:-172.30.0.0/24}"
DHCP_HOSTS_DIR="${NICO_VMS_DHCP_HOSTS_DIR:-/var/lib/nico-vms/dhcp-hosts}"
DHCP_LEASE_FILE="${NICO_VMS_DHCP_LEASE_FILE:-/var/run/nico-vms-dnsmasq.leases}"
DNSMASQ_PID_FILE="/var/run/nico-vms-dnsmasq.pid"
LIBVIRT_URI="${NICO_VMS_LIBVIRT_URI:-qemu:///session}"
# Same derivation as setup-ci-host.sh, which only accepts a network-aligned /24.
BRIDGE_GW="${BRIDGE_CIDR%.*}.1"

WORKSPACE="${GITHUB_WORKSPACE:-${PWD}}"
ARTIFACT_DIR="${NKE_BRINGUP_ARTIFACT_DIR:-${WORKSPACE}/.ci-artifacts/kubeadm-bringup}"
CONSOLE_LOG_DIR="${ARTIFACT_DIR}/console-logs"
DIAG_FILE="${ARTIFACT_DIR}/host-diagnostics.txt"

mkdir -p "${CONSOLE_LOG_DIR}"

# sudo may be absent or password-gated on a developer laptop. Degrade to
# unprivileged commands rather than prompting or aborting -- most of the
# diagnostics below still produce something useful without root.
#
# A function rather than a `SUDO=(sudo)` array prefix, because expanding an
# empty array is an unbound-variable error under `set -u` on bash before 4.4,
# which is still what /bin/bash is on macOS.
HAVE_SUDO=false
if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  HAVE_SUDO=true
else
  log "passwordless sudo unavailable; running unprivileged (output will be partial)"
fi

run_priv() {
  if [[ "${HAVE_SUDO}" == true ]]; then
    sudo "$@"
  else
    "$@"
  fi
}

# ── 1. Diagnostics, before anything is removed ──────────────────────────────

dump() {
  local title="$1"
  shift
  printf '\n===== %s =====\n' "${title}"
  "$@" 2>&1 || printf '(command failed: %s)\n' "$*"
}

# ps + grep in a function so `dump` can invoke it like any other command. The
# [q] bracket keeps the grep from matching its own command line.
qemu_processes() {
  ps -eo pid,ppid,etime,args | grep -E '[q]emu-system' || echo "(no qemu-system processes)"
}

# `nico-sim` must be followed by whitespace or end-of-line, so that paths that
# merely contain the name -- nico-sim.db, controller/nico-sim/... -- do not
# match. Without it this reports the shell that invoked the script, because
# NICO_SIM_DB_PATH appears on its command line.
nicovms_processes() {
  ps -eo pid,ppid,etime,args | grep -E '[n]ico-sim([[:space:]]|$)|[d]nsmasq' || echo "(no nico-sim or dnsmasq processes)"
}

dhcp_leases() {
  if [[ -f "${DHCP_LEASE_FILE}" ]]; then
    cat "${DHCP_LEASE_FILE}"
  else
    echo "(no lease file at ${DHCP_LEASE_FILE})"
  fi
}

{
  printf 'teardown-ci-host diagnostics -- %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf 'bridge=%s cidr=%s\n' "${BRIDGE_NAME}" "${BRIDGE_CIDR}"
  if [[ -c /dev/kvm ]]; then
    printf 'accelerator=kvm (/dev/kvm present)\n'
  else
    printf 'accelerator=tcg (/dev/kvm absent -- QEMU software emulation)\n'
  fi

  dump "ip addr" ip addr
  dump "ip -4 route" ip -4 route show
  dump "iptables -t nat -S" run_priv iptables -t nat -S
  dump "iptables -S" run_priv iptables -S
  dump "running QEMU processes" qemu_processes
  dump "running nico-sim / dnsmasq processes" nicovms_processes
  # Tells a /dev/kvm mode reset from a libvirt fault after a failed define.
  dump "/dev/kvm" ls -l /dev/kvm
  dump "virsh domcapabilities --virttype kvm (${LIBVIRT_URI})" virsh -c "${LIBVIRT_URI}" domcapabilities --virttype kvm
  dump "virsh list --all (${LIBVIRT_URI})" virsh -c "${LIBVIRT_URI}" list --all
  dump "dnsmasq leases" dhcp_leases
} 2>&1 | tee "${DIAG_FILE}"

log "Wrote host diagnostics to ${DIAG_FILE}"

# ── 2. Console logs ─────────────────────────────────────────────────────────
# libvirt names them <overlay>.ttyS0.log / <overlay>.ttyS1.log, so with
# artifacts.go:46 building the overlay as <workdir>/disk.qcow2 the files are
# .../instances/<instance-id>/disk.qcow2.ttyS[01].log. The instance ID is the
# parent directory, so prefix each copy with it to keep names unique once
# flattened into one directory.

LOG_ROOTS=()
if [[ -n "${NICO_SIM_DB_PATH:-}" ]]; then
  LOG_ROOTS+=("$(dirname "${NICO_SIM_DB_PATH}")")
fi
LOG_ROOTS+=("${WORKSPACE}")
if [[ -n "${RUNNER_TEMP:-}" ]]; then
  LOG_ROOTS+=("${RUNNER_TEMP}")
fi
LOG_ROOTS+=("${TMPDIR:-/tmp}")

# The find below -prunes the artifact dir, because it defaults to a path
# *inside* the workspace and the workspace is itself a search root: without the
# prune, a later root would re-copy what an earlier one just wrote, under a
# doubly-prefixed name. The copy goes through run_priv because a root-owned
# virtlogd may own the log, and upload-artifact silently skips what the runner
# user cannot read.
copied=0
for root in "${LOG_ROOTS[@]}"; do
  [[ -d "${root}" ]] || continue
  while IFS= read -r -d '' logfile; do
    dir="$(dirname "${logfile}")"
    dest="${CONSOLE_LOG_DIR}/$(basename "${dir}")-$(basename "${logfile}")"
    # The roots overlap by design -- NICO_SIM_DB_PATH normally points inside the
    # workspace -- so the same file is found more than once. The destination
    # name carries the instance ID, so an existing dest means "already copied".
    [[ -e "${dest}" ]] && continue
    if run_priv cp -f "${logfile}" "${dest}" 2>/dev/null; then
      copied=$((copied + 1))
    fi
  done < <(run_priv find "${root}" -path "${ARTIFACT_DIR}" -prune -o -type f -name '*ttyS[01].log' -print0 2>/dev/null)
done

if ((copied > 0)); then
  run_priv chown -R "$(id -u):$(id -g)" "${CONSOLE_LOG_DIR}" 2>/dev/null
  log "Copied ${copied} serial-console log(s) to ${CONSOLE_LOG_DIR}"
  # A guest that oopses reports only "Node data not available" upstack, and the
  # console log that says why is inside a 3 GB artifact. Print the trace header
  # so the job log names the cause. Seen on 7.0.0-1018-nvidia: a page fault in
  # execmem_free killed the kworker and froze boot for 888 seconds.
  oops="$(grep -ahoE '(BUG: unable to handle|Oops: |kernel BUG at |general protection fault|watchdog: BUG).*' \
    "${CONSOLE_LOG_DIR}"/*ttyS0.log 2>/dev/null | sort -u)"
  if [[ -n "${oops}" ]]; then
    log "GUEST KERNEL FAULT in serial console:"
    printf '%s\n' "${oops}"
  fi
else
  log "No *ttyS[01].log files found under: ${LOG_ROOTS[*]}"
fi

log "Artifact bundle ready at ${ARTIFACT_DIR}"

# ── 3. Remove the nico-vms domains ──────────────────────────────────────────
# The suite deletes its instances through the nico-sim API, which stops each
# VM but leaves the libvirt domain defined. A run that fails before its cleanup
# leaves them running as well. On a long-lived runner they accumulate, and this
# script's header promises such a runner behaves like a fresh one.
#
# Diagnostics above already ran, so nothing here destroys evidence. virsh runs
# unprivileged on purpose: a session URI is per-user, so sudo would address
# root's libvirt session rather than the one holding these domains.

domains=0
while IFS= read -r domain; do
  [[ -n "${domain}" ]] || continue
  virsh -c "${LIBVIRT_URI}" destroy "${domain}" >/dev/null 2>&1
  virsh -c "${LIBVIRT_URI}" undefine "${domain}" --nvram >/dev/null 2>&1 ||
    virsh -c "${LIBVIRT_URI}" undefine "${domain}" >/dev/null 2>&1
  domains=$((domains + 1))
done < <(virsh -c "${LIBVIRT_URI}" list --all --name 2>/dev/null | grep '^nico-vm-' || true)
if [[ ${domains} -gt 0 ]]; then
  log "Removed ${domains} nico-vms libvirt domain(s)"
fi

# ── 4. Stop dnsmasq ─────────────────────────────────────────────────────────

if [[ -f "${DNSMASQ_PID_FILE}" ]]; then
  run_priv kill "$(cat "${DNSMASQ_PID_FILE}")" 2>/dev/null
  run_priv rm -f "${DNSMASQ_PID_FILE}"
  log "Stopped nico-vms dnsmasq"
fi
run_priv pkill -f "dnsmasq.*--interface=${BRIDGE_NAME}" 2>/dev/null
run_priv rm -f "${DHCP_LEASE_FILE}"
run_priv rm -rf "${DHCP_HOSTS_DIR}"

# ── 5. Remove the iptables rules ────────────────────────────────────────────
# Looping per rule clears accidental duplicates from repeated setup runs;
# a missing rule just fails the first -D and ends the loop
# (teardown-linux.sh:85-97 uses the same idiom).

while run_priv iptables -t nat -D POSTROUTING -s "${BRIDGE_CIDR}" ! -d "${BRIDGE_CIDR}" -j MASQUERADE 2>/dev/null; do :; done
while run_priv iptables -D FORWARD -i "${BRIDGE_NAME}" -s "${BRIDGE_CIDR}" -j ACCEPT 2>/dev/null; do :; done
while run_priv iptables -D FORWARD -o "${BRIDGE_NAME}" -d "${BRIDGE_CIDR}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; do :; done
# Only present when setup found port 53 taken and moved guest DNS aside.
for proto in udp tcp; do
  while run_priv iptables -t nat -D PREROUTING -i "${BRIDGE_NAME}" -p "${proto}" --dport 53 -j REDIRECT --to-ports 5353 2>/dev/null; do :; done
done
# Only present when setup ran with NKE_CI_HOST_FDE=true.
while run_priv iptables -t nat -D PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 80 -j REDIRECT --to-port 7777 2>/dev/null; do :; done
while run_priv iptables -t nat -D PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 179 -j REDIRECT --to-port 10179 2>/dev/null; do :; done
while run_priv iptables -t nat -D PREROUTING -i "${BRIDGE_NAME}" -d 169.254.169.254 -p tcp --dport 7777 -j DNAT --to-destination "${BRIDGE_GW}:7777" 2>/dev/null; do :; done
run_priv ip neigh del proxy 169.254.169.254 dev "${BRIDGE_NAME}" 2>/dev/null
for spec in "-p udp --dport 67" "-p udp --dport 53" "-p tcp --dport 53" "-p tcp --dport 7777" "-p udp --dport 5353" "-p tcp --dport 5353" "-p tcp --dport 10179" "-p tcp --dport 18080" "-p tcp --dport 30440" "-p tcp --dport 30443"; do
  # Word splitting is intended: each spec is a list of iptables arguments.
  # shellcheck disable=SC2086
  while run_priv iptables -D INPUT -i "${BRIDGE_NAME}" ${spec} -j ACCEPT 2>/dev/null; do :; done
done
log "Removed iptables rules for ${BRIDGE_NAME} / ${BRIDGE_CIDR}"

# ── 6. Delete the bridge ────────────────────────────────────────────────────
# Deleting the interface takes its addresses and the connected-subnet route
# with it, so there is nothing else to unwind.

if ip link show "${BRIDGE_NAME}" >/dev/null 2>&1; then
  run_priv ip link del "${BRIDGE_NAME}" 2>/dev/null
  log "Deleted bridge ${BRIDGE_NAME}"
else
  log "Bridge ${BRIDGE_NAME} not present"
fi

log "Teardown complete (best effort; always exits 0)"
exit 0
