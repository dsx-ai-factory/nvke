#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# setup-ci-host.sh -- prepare a GitHub Actions runner to host nico-vms in
# native bridge mode for the kubeadm bring-up lane.
#
# WHY THIS EXISTS, rather than calling controller/nico-vms/scripts/setup-linux.sh
#
#   setup-linux.sh is a *workstation* installer. Alongside the networking it
#   also installs docker, tilt, kubectl, kind, helm, Go and ko, appends to
#   ~/.profile and ~/.bashrc, writes /etc/sudoers.d/nico-vms-setcap, edits the
#   swtpm AppArmor profile and rewrites virtlogd.conf. None of that belongs on
#   a single-use runner: Go arrives via actions/setup-go, nothing here uses
#   kind/tilt/helm, and the machine is destroyed when the job ends.
#
#   It is also unsafe here in one specific place. setup-linux.sh:398-404 adds
#   169.254.169.254/32 to the bridge so guest cloud-init can reach the
#   EC2-style metadata server. On these runners that address is the *host's
#   own* AWS EC2 Instance Metadata Service -- see "IMDS" in section 4 below.
#
#   So this script lifts only the parts nico-vms needs for native bridge mode:
#   the QEMU/libvirt packages, the bridge, dnsmasq, ip_forward, and the NAT and
#   FORWARD rules. Everything else from setup-linux.sh is deliberately absent.
#
# WHY NATIVE BRIDGE MODE, not libvirt-network mode
#
#   setup-libvirt-network.sh:8-12 exists for hosts where nico-vms must not
#   create host networking of its own -- a shared, long-lived, containerised
#   GitLab runner. A GitHub self-hosted runner is the opposite: an ephemeral,
#   single-tenant EC2 VM with passwordless sudo and no libvirt preinstalled.
#   Using libvirt-network mode there would mean installing libvirtd and
#   creating the `default` network first -- i.e. creating exactly the host
#   networking that mode exists to avoid, for strictly less capability.
#
#   Mode select is one variable: config.go:93 UseLibvirtNetwork() returns true
#   iff NICO_VMS_LIBVIRT_NETWORK is set. This script leaves it unset.
#
# Usage:
#   bash controller/test/kubeadm/scripts/setup-ci-host.sh
#
# Environment (all optional; defaults match nico-vms' own):
#   NICO_VMS_BRIDGE_NAME       bridge to create        (default cwebr0)
#   NICO_VMS_BRIDGE_CIDR       bridge subnet           (default 172.30.0.0/24)
#   NICO_VMS_DHCP_HOSTS_DIR    dnsmasq hostsdir        (default /var/lib/nico-vms/dhcp-hosts)
#   NICO_VMS_DHCP_LEASE_FILE   dnsmasq lease file      (default /var/run/nico-vms-dnsmasq.leases)
#   NICO_VMS_DHCP_RANGE_END    last DHCP address       (default <prefix>.239)
#   NKE_CI_HOST_FDE            "true" adds what the NKE FDE node image needs on
#                              top of the above: UEFI firmware, swtpm, debugfs,
#                              the carbide-pxe.forge record, and a route from
#                              the guests to the metadata server that does not
#                              claim the runner's own IMDS address. Sections 3,
#                              5, 6 and 6b. Default off; the kubeadm lane boots
#                              vanilla noble and needs none of it.
#
# Idempotent -- safe to re-run. Runs as the current user; sudo is used
# explicitly for every privileged operation. Undo with teardown-ci-host.sh.

set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

log() { printf '[setup-ci-host] %s\n' "$*"; }
warn() { printf '[setup-ci-host] WARNING: %s\n' "$*" >&2; }
fail() {
  printf '[setup-ci-host] ERROR: %s\n' "$*" >&2
  exit 1
}

# Defaults are nico-vms' own, so the engine and this script agree without the
# workflow having to set anything:
#   NICO_VMS_BRIDGE_NAME -> config.go:163, envOrDefault(..., "cwebr0")
#   NICO_VMS_BRIDGE_CIDR -> config.go:164 reads it with no default; the default
#                           is applied later during normalization, at
#                           config.go:419-421 ("172.30.0.0/24"), for every
#                           backend except darwin-qemu.
BRIDGE_NAME="${NICO_VMS_BRIDGE_NAME:-cwebr0}"
BRIDGE_CIDR="${NICO_VMS_BRIDGE_CIDR:-172.30.0.0/24}"
DHCP_HOSTS_DIR="${NICO_VMS_DHCP_HOSTS_DIR:-/var/lib/nico-vms/dhcp-hosts}"
DHCP_LEASE_FILE="${NICO_VMS_DHCP_LEASE_FILE:-/var/run/nico-vms-dnsmasq.leases}"
DNSMASQ_PID_FILE="/var/run/nico-vms-dnsmasq.pid"

# Derive the gateway and DHCP pool the same way run-dnsmasq.sh:22-26 does, and
# reject anything that is not a network-aligned /24. netutil.GatewayIP
# (netutil.go:27-30) computes network+1 properly; this string form only agrees
# with it when the CIDR ends in `.0`, so refuse the cases where it would not.
case "${BRIDGE_CIDR}" in
  *.0/24) ;;
  *) fail "NICO_VMS_BRIDGE_CIDR must be a network-aligned /24 (got '${BRIDGE_CIDR}')" ;;
esac
BRIDGE_PREFIX="${BRIDGE_CIDR%.*}"
BRIDGE_GW="${BRIDGE_PREFIX}.1"
# .10 as the pool start is not arbitrary: nico-vms' IPAM hands out static
# addresses from network+2 upwards (ipam.go:35-38), so .2-.9 stay reserved for
# the statically addressed VMs this lane creates and the DHCP pool cannot
# collide with them.
DHCP_RANGE_START="${BRIDGE_PREFIX}.10"
DHCP_RANGE_END="${NICO_VMS_DHCP_RANGE_END:-${BRIDGE_PREFIX}.239}"

# ── 1. Pre-flight ───────────────────────────────────────────────────────────

# Runners are ephemeral single-tenant EC2 VMs with passwordless sudo, not
# containers, so -n (never prompt) must succeed. Fail loudly if it does not
# rather than hanging on a password prompt no one can answer.
sudo -n true 2>/dev/null || fail "passwordless sudo is required to create the bridge and iptables rules"

# Lifted from setup-linux.sh:66-82. The lane is amd64 (design D-arm64: no arm64
# label in the fleet has KVM, so there is nothing to grow into), but resolving
# the package by architecture turns an obscure apt failure into a clear one.
DEB_ARCH="$(dpkg --print-architecture)"
case "${DEB_ARCH}" in
  amd64) QEMU_SYSTEM_PKG="qemu-system-x86" ;;
  arm64) QEMU_SYSTEM_PKG="qemu-system-arm" ;;
  *) fail "unsupported architecture: ${DEB_ARCH} (supported: amd64, arm64)" ;;
esac

# ── 2. Report the accelerator ───────────────────────────────────────────────
# Adapted from .github/workflows/os-image-artifact.yml:134-143, which reads:
#
#     - name: Report the accelerator
#       run: |
#         set -euo pipefail
#         if [[ -c /dev/kvm ]]; then
#           # Membership in group kvm does not apply until a new login.
#           sudo chmod 666 /dev/kvm
#           echo "::notice::/dev/kvm present; building with hardware acceleration"
#         else
#           echo "::warning::/dev/kvm is missing on this runner; building under QEMU software emulation (much slower)"
#         fi
#
# Not a gate, for the same reason it is not one there. It matters more here:
# nico-vms picks its accelerator silently -- virsh.go:248-252 detectDomainType()
# is os.Stat("/dev/kvm") -> "kvm" else "qemu", with no log line either way. This
# is the only place a run states which one it got.

if [[ -c /dev/kvm ]]; then
  # Membership in group kvm does not apply until a new login.
  sudo chmod 666 /dev/kvm
  log "/dev/kvm present; VMs will run with hardware acceleration (domain type kvm)"
  echo "::notice::/dev/kvm present; nico-vms VMs will run with hardware acceleration"
else
  log "/dev/kvm is MISSING; VMs will run under QEMU software emulation (domain type qemu, ~10x slower)"
  echo "::warning::/dev/kvm is missing on this runner; nico-vms VMs run under QEMU software emulation (much slower)"
fi

# ── 3. Packages ─────────────────────────────────────────────────────────────
# Trimmed from setup-linux.sh:157-176. Kept, with what needs each one:
#   ${QEMU_SYSTEM_PKG}      the emulator libvirt launches
#   qemu-utils              qemu-img, default of NICO_VMS_QEMU_IMG_PATH (config.go:168)
#   libvirt-daemon-system   libvirtd/virtlogd; virtlogd writes the *.ttyS[01].log
#                           serial logs declared in domain_xml.go:105-116
#   libvirt-clients         virsh, the only libvirt interface nico-vms uses
#                           (virsh.go:210); the default URI is qemu:///session
#                           (config.go:160)
#   cloud-image-utils       cloud-localds, default of NICO_VMS_CLOUD_LOCALDS_PATH
#                           (config.go:169), builds the NoCloud seed ISO
#   genisoimage             the seed-ISO fallback when cloud-localds is missing
#                           or produces nothing (artifacts.go:192-201)
#   dnsmasq-base            section 5
#   iptables                sections 5 and 6
#
# Dropped from setup-linux.sh, each for a reason:
#   ovmf, qemu-efi-aarch64  the lane boots vanilla Ubuntu noble on BIOS; no UEFI
#   swtpm, swtpm-tools      no LUKS/FDE image, so no vTPM. virsh.go:276-278
#                           returns ("", false, nil) -- TPM skipped, no error --
#                           when exec.LookPath("swtpm_setup") fails
#   e2fsprogs               debugfs is only used to patch FDE/iPXE images
#   virtinst                virt-install is never invoked; virsh is
#   bridge-utils            section 4 uses iproute2 (`ip link add ... type
#                           bridge`), which is already present
#   net-tools               the ARP fallback reads /proc/net/arp directly
#                           (metadataserver.go:271-273), it does not shell out
#   docker, tilt, kubectl,  workstation tooling; D5 dropped CNI and the
#   kind, helm, go, ko      kubectl-based verification that would have needed it

log "Installing QEMU/libvirt packages for ${DEB_ARCH}..."
sudo apt-get update -qq
sudo apt-get install -y -qq -o Dpkg::Options::="--force-confold" \
  "${QEMU_SYSTEM_PKG}" \
  qemu-utils \
  libvirt-daemon-system \
  libvirt-clients \
  cloud-image-utils \
  genisoimage \
  dnsmasq-base \
  iptables

# The FDE node image, opt-in. It boots UEFI (ovmf), binds LUKS to a TPM on its
# first boot (swtpm; nico-vms provisions a vTPM only when swtpm_setup is on
# PATH -- virsh.go:276-282, silently skipped otherwise), and nico-vms rewrites
# its grub.cfg through debugfs (e2fsprogs) before the first boot. The same
# four packages setup-linux.sh:157-176 installs for that image.
FDE="${NKE_CI_HOST_FDE:-false}"
if [[ "${FDE}" == true ]]; then
  log "NKE_CI_HOST_FDE=true: installing UEFI firmware, swtpm, e2fsprogs and sshpass"
  # sshpass: the E2E lane reads each guest's journals through nico-vms'
  # password-login diagnostic user when a node never registers.
  sudo apt-get install -y -qq -o Dpkg::Options::="--force-confold" \
    ovmf \
    swtpm \
    swtpm-tools \
    e2fsprogs \
    sshpass \
    ethtool

  # GRO merges inbound segments into super-frames the tap cannot take, so guest
  # downloads crawl on retransmits: 1.6-5 MB/s, against 62 with it off.
  uplink="$(ip -4 route show default | awk '/default/ {print $5; exit}')"
  if [[ -n "${uplink}" ]]; then
    sudo ethtool -K "${uplink}" gro off lro off 2>/dev/null || warn "could not change receive offloads on ${uplink}"
    log "offloads on ${uplink}: $(ethtool -k "${uplink}" 2>/dev/null | grep -E '^(generic-receive-offload|large-receive-offload):' | tr -s ' ' | tr '\n' ' ')"
  fi
fi

# libvirtd is only needed for the system daemon's helpers; nico-vms itself talks
# to qemu:///session. Swallow the failure like setup-linux.sh:360-362 does --
# a container-ish environment without systemd as PID 1 must not stop the run
# here, it should fail later with a message about the thing that actually broke.
sudo systemctl enable --now libvirtd >/dev/null 2>&1 || warn "could not start libvirtd via systemd; continuing"

# ── 4. Bridge ───────────────────────────────────────────────────────────────

# have_addr <dev> <cidr> -- exact match on an assigned address, so that
# 172.30.0.1/24 is not mistaken for 172.30.0.10/24.
have_addr() {
  ip -4 -o addr show dev "$1" 2>/dev/null |
    awk -v want="$2" '$4 == want { found = 1 } END { exit !found }'
}

log "Creating bridge ${BRIDGE_NAME} (${BRIDGE_CIDR}, gateway ${BRIDGE_GW})..."
if ! ip link show "${BRIDGE_NAME}" >/dev/null 2>&1; then
  sudo ip link add name "${BRIDGE_NAME}" type bridge
fi
if ! have_addr "${BRIDGE_NAME}" "${BRIDGE_GW}/24"; then
  sudo ip addr add "${BRIDGE_GW}/24" dev "${BRIDGE_NAME}"
fi
sudo ip link set "${BRIDGE_NAME}" up

# IMDS -- the one line from setup-linux.sh that is deliberately NOT lifted.
#
# setup-linux.sh:398-404 adds 169.254.169.254/32 to the bridge so guests can
# reach nico-vms' EC2-style cloud-init metadata server. 169.254.169.254 is also
# the AWS EC2 Instance Metadata Service, and these runners are EC2 instances.
# Assigning it locally installs a `local` route in the host's main table, which
# hijacks the *runner's own* IMDS: credential refresh and the runner agent's
# housekeeping start answering from our metadata server, and fail in a way that
# looks nothing like a nico-vms bug.
#
# nico-vms tolerates the omission. network.go:122-128 MetadataAddrs() returns
# two addresses -- 169.254.169.254:7777 and {bridge_gw}:7777 -- and
# app.go:220-232 loops over them logging "Could not start metadata listener" and
# `continue`-ing per address. Only if *every* address fails does it error out
# (app.go:250-254, `len(addrs) > 0 && bound == 0`). The bridge-gateway listener
# still binds, so bound == 1 and startup succeeds with one warning.
#
# Belt and braces: D7 boots vanilla Ubuntu noble from a local NoCloud seed ISO,
# and static-mode network-config bakes the address in directly
# (artifacts.go:75-89), so no guest needs a metadata server to get onto the net.
#
# CAVEAT for the test driver, not for this script: the phone_home block in
# testdata/user-data.yml.tmpl points at http://169.254.169.254:7777/, which
# will now never answer. Readiness must therefore be gated on SSH rather than
# phone_home -- i.e. NodeSpec.PhoneHomeEnabled=false (multi.go:21) plus
# cluster.WaitForAgent, which init.go:180-183 documents as exactly that path.

# ── 4b. qemu-bridge-helper (required by qemu:///session) ────────────────────
# Lifted from setup-linux.sh:364-381, and NOT optional: nico-vms defaults
# NICO_VMS_LIBVIRT_URI to qemu:///session (config.go:160) and launch.go never
# overrides it, so QEMU runs as the unprivileged runner user. Attaching a tap
# to an unmanaged host bridge from a session-mode domain goes through the
# setuid qemu-bridge-helper, which refuses any bridge missing from
# /etc/qemu/bridge.conf. Without both halves, domain start fails with an ACL
# error rather than anything that names the bridge.
#
# setup-linux.sh:373-375 hardcodes "allow cwebr0" even though the bridge name
# is configurable; this uses ${BRIDGE_NAME} so an overridden name still works.
# `|| true` is required, not defensive: under `set -e` plus `pipefail`, find
# exits non-zero when one of the two directories does not exist (and again on
# SIGPIPE from `head -1`), which would abort the script on the assignment.
# setup-linux.sh:369 has the same construct without the guard.
BRIDGE_HELPER="$(find /usr/lib/qemu /usr/libexec -name qemu-bridge-helper 2>/dev/null | head -1 || true)"
if [[ -n "${BRIDGE_HELPER}" ]]; then
  sudo mkdir -p /etc/qemu
  if ! grep -qx "allow ${BRIDGE_NAME}" /etc/qemu/bridge.conf 2>/dev/null; then
    echo "allow ${BRIDGE_NAME}" | sudo tee -a /etc/qemu/bridge.conf >/dev/null
    log "Added 'allow ${BRIDGE_NAME}' to /etc/qemu/bridge.conf"
  fi
  sudo chmod u+s "${BRIDGE_HELPER}"
  log "Set setuid on ${BRIDGE_HELPER}"
else
  warn "qemu-bridge-helper not found; qemu:///session bridge networking will fail"
fi

# ── 5. dnsmasq (DNS for the guests; DHCP for the pool) ──────────────────────
# Required even though this lane uses static IPs: artifacts.go:82-89 writes the
# static network-config with `nameservers: [{gateway}]`, so every guest resolves
# through dnsmasq on the bridge. Without it the VMs cannot resolve the apt repo
# and never install kubeadm.

sudo mkdir -p "${DHCP_HOSTS_DIR}"
sudo chmod 777 "${DHCP_HOSTS_DIR}"
# The lease file must be writable by the (non-root) nico-vms process so it can
# purge stale leases -- same reason as setup-linux.sh:418-420.
sudo touch "${DHCP_LEASE_FILE}"
sudo chmod 666 "${DHCP_LEASE_FILE}"

# Idempotency: take ownership of any dnsmasq left over from an earlier run,
# by pid file first and then by command line (run-dnsmasq.sh:28-39).
if [[ -f "${DNSMASQ_PID_FILE}" ]]; then
  sudo kill "$(cat "${DNSMASQ_PID_FILE}")" 2>/dev/null || true
  sudo rm -f "${DNSMASQ_PID_FILE}"
fi
sudo pkill -f "dnsmasq.*--interface=${BRIDGE_NAME}" 2>/dev/null || true

# Host firewalls commonly default INPUT to DROP. Allow only what nico-vms
# exposes to guests on its own bridge (setup-linux.sh:429-440), minus 10179:
# that is the BGP speaker's port, and launch.go:178 sets NICO_VMS_BGP_ENABLED=false
# because this lane has no kube-vip to peer with.
ensure_bridge_input_rule() {
  if ! sudo iptables -C INPUT -i "${BRIDGE_NAME}" "$@" -j ACCEPT 2>/dev/null; then
    sudo iptables -I INPUT -i "${BRIDGE_NAME}" "$@" -j ACCEPT
    log "INPUT rule added on ${BRIDGE_NAME}: $*"
  fi
}
ensure_bridge_input_rule -p udp --dport 67
ensure_bridge_input_rule -p udp --dport 53
ensure_bridge_input_rule -p tcp --dport 53
ensure_bridge_input_rule -p tcp --dport 7777

# Detect the host's upstream DNS rather than hardcoding 8.8.8.8, which is
# blocked on networks that enforce internal resolvers (setup-linux.sh:448-463).
UPSTREAM_DNS_ARGS=()
if command -v resolvectl >/dev/null 2>&1; then
  while IFS= read -r dns_ip; do
    UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
  done < <(resolvectl status 2>/dev/null | awk '/DNS Servers:/{for(i=3;i<=NF;i++) print $i}' | grep -v '^$' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
  while IFS= read -r dns_ip; do
    UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
  done < <(grep '^nameserver' /etc/resolv.conf 2>/dev/null | awk '{print $2}' | grep -v '^127\.' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
  # Last resort: the systemd-resolved stub forwards for us.
  UPSTREAM_DNS_ARGS=("--server=127.0.0.53")
fi
log "dnsmasq upstream DNS: ${UPSTREAM_DNS_ARGS[*]}"

# Serving DNS on the gateway when something else already holds the port.
#
# --bind-interfaces binds ${BRIDGE_GW}:53 specifically, so any resolver already
# listening on every interface takes that address and dnsmasq exits with
# "failed to create listening socket ... Address already in use". A Bright
# Cluster head node running named is one such host, and this script's header
# promises a long-lived runner behaves like a fresh one.
#
# The guests cannot be pointed elsewhere: in static addressing mode nico-vms
# writes the gateway as the only nameserver (nico-vms artifacts.go, the
# network-config it bakes into the seed ISO), so a DHCP resolver option never
# reaches them. Run dnsmasq on an alternative port instead and redirect port 53
# arriving on the bridge to it. The other resolver keeps the port for the host,
# and only guest queries are diverted.
#
# Try the port and react to the result, rather than checking whether it is free
# first. The competing resolver binds the gateway when it notices the new
# interface, which can be after any such check and before dnsmasq starts.
ALT_DNS_PORT=5353

# The FDE image seeds cloud-init from http://carbide-pxe.forge/api/v0/cloud-init/
# (nico-vms/internal/lifecycle/network.go:20-24). Resolve that name to the
# gateway, where nico-vms' metadata server listens; section 6 redirects the
# port. setup-linux.sh:477 has the same record. Off by default: vanilla noble
# takes its seed from a local ISO and never asks.
DNSMASQ_EXTRA_ARGS=()
if [[ "${FDE}" == true ]]; then
  DNSMASQ_EXTRA_ARGS+=("--address=/carbide-pxe.forge/${BRIDGE_GW}")
fi

start_dnsmasq() {
  # ${arr[@]+"${arr[@]}"} expands to nothing for an empty array, which a bare
  # "${arr[@]}" does not do under set -u on every bash.
  sudo dnsmasq \
    --interface="${BRIDGE_NAME}" \
    --bind-interfaces \
    "$@" \
    --dhcp-range="${DHCP_RANGE_START},${DHCP_RANGE_END},255.255.255.0,30m" \
    --dhcp-hostsdir="${DHCP_HOSTS_DIR}" \
    --dhcp-option=option:router,"${BRIDGE_GW}" \
    --dhcp-option=option:dns-server,"${BRIDGE_GW}" \
    --except-interface=lo \
    --no-resolv \
    --no-hosts \
    ${DNSMASQ_EXTRA_ARGS[@]+"${DNSMASQ_EXTRA_ARGS[@]}"} \
    "${UPSTREAM_DNS_ARGS[@]}" \
    --dhcp-leasefile="${DHCP_LEASE_FILE}" \
    --pid-file="${DNSMASQ_PID_FILE}" \
    --log-dhcp \
    --dhcp-ignore-clid
}

log "Starting dnsmasq on ${BRIDGE_NAME} (${DHCP_RANGE_START}-${DHCP_RANGE_END})..."
DNSMASQ_ERR="$(mktemp)"
if ! start_dnsmasq 2>"${DNSMASQ_ERR}"; then
  if ! grep -q "Address already in use" "${DNSMASQ_ERR}"; then
    cat "${DNSMASQ_ERR}" >&2
    fail "dnsmasq did not start"
  fi
  # Purely diagnostic, and deliberately non-fatal. Two traps here:
  #
  #   - The common holder is a WILDCARD listener, which ss prints as 0.0.0.0:53
  #     or *:53, never as ${BRIDGE_GW}:53. Filtering on the gateway address
  #     matched nothing in exactly that case, and under `set -euo pipefail` a
  #     grep that matches nothing aborts the script -- inside the fallback whose
  #     whole job is to survive this. We already know port 53 is taken, because
  #     dnsmasq just said so, so report whoever holds it and do not filter.
  #   - `tr -d` takes a character SET, so it also ate the u/s/e/r/: characters
  #     out of the process name ("systemd-resolved" -> "ytmd-olvd"). sed
  #     extracts the quoted name instead.
  holder="$(sudo ss -lnupH "sport = :53" 2>/dev/null |
    sed -n 's/.*users:((\"\([^\"]*\)\".*/\1/p' | head -1 || true)"
  log "${BRIDGE_GW}:53 is unavailable; port 53 is held by ${holder:-another process}"
  log "serving guest DNS on port ${ALT_DNS_PORT} and redirecting ${BRIDGE_NAME} port 53 to it"
  for proto in udp tcp; do
    if ! sudo iptables -t nat -C PREROUTING -i "${BRIDGE_NAME}" -p "${proto}" --dport 53 \
      -j REDIRECT --to-ports "${ALT_DNS_PORT}" 2>/dev/null; then
      sudo iptables -t nat -A PREROUTING -i "${BRIDGE_NAME}" -p "${proto}" --dport 53 \
        -j REDIRECT --to-ports "${ALT_DNS_PORT}"
    fi
  done
  ensure_bridge_input_rule -p udp --dport "${ALT_DNS_PORT}"
  ensure_bridge_input_rule -p tcp --dport "${ALT_DNS_PORT}"
  start_dnsmasq --port="${ALT_DNS_PORT}" || fail "dnsmasq did not start on port ${ALT_DNS_PORT} either"
fi
rm -f "${DNSMASQ_ERR}"
log "dnsmasq started (pid=$(cat "${DNSMASQ_PID_FILE}"))"

# ── 6. Forwarding and NAT ───────────────────────────────────────────────────
# ip_forward only. setup-linux.sh:498 also sets
# net.ipv4.fib_multipath_hash_policy=1, which exists to spread connections
# across ECMP nexthops installed by the BGP speaker -- and the speaker is off
# here (launch.go:178), so there are no multipath routes to hash over.

sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
log "net.ipv4.ip_forward=1"

if ! sudo iptables -t nat -C POSTROUTING -s "${BRIDGE_CIDR}" ! -d "${BRIDGE_CIDR}" -j MASQUERADE 2>/dev/null; then
  sudo iptables -t nat -A POSTROUTING -s "${BRIDGE_CIDR}" ! -d "${BRIDGE_CIDR}" -j MASQUERADE
  log "NAT rule added for ${BRIDGE_CIDR}"
fi

# docker and libvirt both set the FORWARD policy to DROP, so VM traffic is
# silently dropped without these even though MASQUERADE is in place
# (setup-linux.sh:504-514).
if ! sudo iptables -C FORWARD -i "${BRIDGE_NAME}" -s "${BRIDGE_CIDR}" -j ACCEPT 2>/dev/null; then
  sudo iptables -I FORWARD -i "${BRIDGE_NAME}" -s "${BRIDGE_CIDR}" -j ACCEPT
  log "FORWARD rule added: allow outbound from ${BRIDGE_NAME}"
fi
if ! sudo iptables -C FORWARD -o "${BRIDGE_NAME}" -d "${BRIDGE_CIDR}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  sudo iptables -I FORWARD -o "${BRIDGE_NAME}" -d "${BRIDGE_CIDR}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  log "FORWARD rule added: allow return traffic to ${BRIDGE_NAME}"
fi

# The two ways an FDE guest reaches nico-vms' cloud-init metadata server. Both
# come from setup-linux.sh, reshaped for a host that must not own
# 169.254.169.254 (see IMDS in section 4).
if [[ "${FDE}" == true ]]; then
  # 1. http://carbide-pxe.forge/ -> ${BRIDGE_GW}:80 -> 7777 (setup-linux.sh:516-525).
  #    Scoped to the gateway address, so a guest still reaches real port-80
  #    servers on the internet.
  if ! sudo iptables -t nat -C PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 80 -j REDIRECT --to-port 7777 2>/dev/null; then
    sudo iptables -t nat -A PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 80 -j REDIRECT --to-port 7777
    log "REDIRECT added: ${BRIDGE_GW}:80 -> 7777 on ${BRIDGE_NAME}"
  fi
  # 2. http://169.254.169.254:7777/, the EC2-style fallback and the phone_home
  #    target in the fleet userData. setup-linux.sh:398-404 puts that address
  #    on the bridge, which here would hijack the runner's own IMDS. Instead
  #    the host answers the guests' ARP for it with a proxy neighbour entry,
  #    and DNATs the one port to the gateway listener. The kernel proxies the
  #    address only because the host routes it out of a different interface,
  #    and that route -- the runner's real IMDS -- is untouched.
  sudo ip neigh replace proxy 169.254.169.254 dev "${BRIDGE_NAME}"
  if ! sudo iptables -t nat -C PREROUTING -i "${BRIDGE_NAME}" -d 169.254.169.254 -p tcp --dport 7777 -j DNAT --to-destination "${BRIDGE_GW}:7777" 2>/dev/null; then
    sudo iptables -t nat -A PREROUTING -i "${BRIDGE_NAME}" -d 169.254.169.254 -p tcp --dport 7777 -j DNAT --to-destination "${BRIDGE_GW}:7777"
    log "DNAT added: 169.254.169.254:7777 -> ${BRIDGE_GW}:7777 on ${BRIDGE_NAME}"
  fi
  # 3. kube-vip on a tenant control plane peers with the gateway on BGP port
  #    179, hardcoded; nico-vms' speaker listens on 10179 (setup-linux.sh:527-533).
  #    The speaker installs one ECMP route per control-plane node, and the
  #    multipath hash policy spreads connections across them (setup-linux.sh:498).
  if ! sudo iptables -t nat -C PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 179 -j REDIRECT --to-port 10179 2>/dev/null; then
    sudo iptables -t nat -A PREROUTING -i "${BRIDGE_NAME}" -d "${BRIDGE_GW}" -p tcp --dport 179 -j REDIRECT --to-port 10179
    log "REDIRECT added: ${BRIDGE_GW}:179 -> 10179 on ${BRIDGE_NAME}"
  fi
  ensure_bridge_input_rule -p tcp --dport 10179
  sudo sysctl -w net.ipv4.fib_multipath_hash_policy=1 >/dev/null
  # 4. The nico-vms API and file server on 18080: the fleet userData downloads
  #    nke-agent from http://carbide-pxe.forge:18080/files/agent/nke-agent.
  ensure_bridge_input_rule -p tcp --dport 18080
  # 5. SPIRE and OIDC, which Tilt port-forwards onto the kind gateway address;
  #    the tenant nodes dial them from the bridge.
  ensure_bridge_input_rule -p tcp --dport 30440
  ensure_bridge_input_rule -p tcp --dport 30443
fi

# Held in reserve, NOT applied. If the runner sits behind the AWS VPC CNI, its
# AWS-SNAT-CHAIN jump sits earlier in POSTROUTING and rewrites VM<->VM source
# IPs, which breaks IP-bound peer TLS -- nico-vms/README.md:167 names the exact
# symptom: etcd control-plane joins failing with
#   "<node-ip>" does not match any of DNSNames [...]
# The fix is one rule, lifted from setup-libvirt-network.sh:149 -- note the
# `-I ... 1`, because setup-linux.sh appends its MASQUERADE and so would lose:
#
#   sudo iptables -t nat -I POSTROUTING 1 \
#     -s "${BRIDGE_CIDR}" -d "${BRIDGE_CIDR}" \
#     -m comment --comment nico-vms-ci -j RETURN
#
# Apply it only if that symptom actually appears; an unnecessary RETURN is
# harmless but hides which hypothesis was the real one.

# The 80->7777 and 179->10179 PREROUTING REDIRECTs from setup-linux.sh:516-533
# are omitted too, along with dnsmasq's --address=/carbide-pxe.forge/ record
# (setup-linux.sh:477). All three serve production FDE images, which reach
# http://carbide-pxe.forge/api/v0/cloud-init/ for their NoCloud seed and run
# kube-vip on a hardcoded BGP port 179. D7 boots vanilla noble from a local
# seed ISO and D5 drops CNI, so no guest here asks for any of them. With
# NKE_CI_HOST_FDE=true all three are added above, because a CWE-driven tenant
# runs kube-vip and its control plane reaches the metadata server.

# ── 6b. vTPM and console logs for the FDE image ─────────────────────────────

if [[ "${FDE}" == true ]]; then
  # swtpm_setup runs as the runner user and needs a writable localca statedir
  # (setup-linux.sh:304-317); without it every domain start fails with
  # "Need read/write rights on statedir".
  sudo mkdir -p /var/lib/swtpm-localca
  sudo chown -R "$(id -u):$(id -g)" /var/lib/swtpm-localca

  # swtpm's AppArmor profile allows `owner @{HOME}/**`, which is /home/* only.
  # A runner whose HOME or cache lives elsewhere gets "swtpm died"; add those
  # paths to the local override, as setup-linux.sh:319-356 does.
  SWTPM_LOCAL=/etc/apparmor.d/local/usr.bin.swtpm
  if [[ -f /etc/apparmor.d/usr.bin.swtpm ]]; then
    reload=false
    for dir in "${HOME}" "${XDG_CACHE_HOME:-${HOME}/.cache}"; do
      [[ "${dir}" == /home/* ]] && continue
      rule="owner ${dir}/** rwk,"
      if ! grep -qF "${rule}" "${SWTPM_LOCAL}" 2>/dev/null; then
        echo "${rule}" | sudo tee -a "${SWTPM_LOCAL}" >/dev/null
        reload=true
      fi
    done
    if [[ "${reload}" == true ]]; then
      sudo apparmor_parser -r /etc/apparmor.d/usr.bin.swtpm && log "AppArmor: swtpm may use ${HOME}"
    fi
  fi

  # virtlogd caps each console log at 2 MiB, and a production first boot writes
  # more than that. The ttyS1 log is the main evidence a failed run leaves, so
  # raise the cap for the session daemon nico-vms uses (setup-linux.sh:535-593).
  mkdir -p "${HOME}/.config/libvirt"
  printf 'max_size = 52428800\nmax_backups = 3\n' >"${HOME}/.config/libvirt/virtlogd.conf"
  log "virtlogd (session) max_size raised to 50 MiB"
fi

# ── 7. Base image ───────────────────────────────────────────────────────────
#
# nico-vms does NOT download this. It resolves NICO_VMS_BASE_IMAGE when an
# instance is created and fails that reconcile with "no bootable image
# available", which surfaces as an instance in state Error rather than as a
# startup failure -- so a missing image looks like a broken VM, not a missing
# file. Same URLs nico-vms' own integration tests use
# (controller/nico-vms/test/integration/testutil/image.go:24-26).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
BASE_IMAGE="${NICO_VMS_BASE_IMAGE:-${REPO_ROOT}/controller/nico-vms/images/ubuntu-cloud.qcow2}"
IMAGE_BASE_URL="${NKE_BRINGUP_IMAGE_BASE_URL:-https://cloud-images.ubuntu.com/releases/24.04/release}"
case "${DEB_ARCH}" in
  amd64) IMAGE_NAME="ubuntu-24.04-server-cloudimg-amd64.img" ;;
  arm64) IMAGE_NAME="ubuntu-24.04-server-cloudimg-arm64.img" ;;
  *) fail "unsupported architecture for the base image: ${DEB_ARCH}" ;;
esac
IMAGE_NAME="${NKE_BRINGUP_IMAGE_NAME:-${IMAGE_NAME}}"

if [[ -s "${BASE_IMAGE}" ]] && qemu-img info "${BASE_IMAGE}" >/dev/null 2>&1; then
  log "Base image already present at ${BASE_IMAGE}"
else
  log "Fetching ${IMAGE_NAME} from ${IMAGE_BASE_URL}"
  mkdir -p "$(dirname "${BASE_IMAGE}")"

  # Verify the download against the publisher's SHA256SUMS before booting it.
  # We boot this image as a VM, so an unverified fetch is an unverified
  # execution. Same pattern, and the same reasoning, as the actionlint install
  # in .github/workflows/actionlint.yml: fetch the checksum list, look up this
  # exact filename, compare, and fail closed on any mismatch or missing entry.
  IMAGE_TMP="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand IMAGE_TMP now, not at trap time
  trap "rm -rf '${IMAGE_TMP}'" EXIT

  if ! curl -fsSL --retry 3 --retry-delay 5 --max-time 120 \
       -o "${IMAGE_TMP}/SHA256SUMS" "${IMAGE_BASE_URL}/SHA256SUMS"; then
    fail "could not download SHA256SUMS from ${IMAGE_BASE_URL}"
  fi

  # Entries are "<sha256> *<filename>"; the star marks binary mode.
  expected="$(awk -v n="${IMAGE_NAME}" '$2 == n || $2 == "*" n {print $1}' "${IMAGE_TMP}/SHA256SUMS" | head -1)"
  [[ -n "${expected}" ]] || fail "no SHA256SUMS entry for ${IMAGE_NAME}"

  if ! curl -fsSL --retry 3 --retry-delay 5 --max-time 900 \
       -o "${IMAGE_TMP}/${IMAGE_NAME}" "${IMAGE_BASE_URL}/${IMAGE_NAME}"; then
    fail "could not download the base image from ${IMAGE_BASE_URL}/${IMAGE_NAME}"
  fi

  actual="$(sha256sum "${IMAGE_TMP}/${IMAGE_NAME}" | awk '{print $1}')"
  if [[ "${actual}" != "${expected}" ]]; then
    fail "checksum mismatch for ${IMAGE_NAME}: expected ${expected}, got ${actual}"
  fi
  log "Checksum verified: ${IMAGE_NAME} sha256=${actual}"

  qemu-img info "${IMAGE_TMP}/${IMAGE_NAME}" >/dev/null 2>&1 \
    || fail "the downloaded base image is not a usable disk image"

  # Move into place only after verification, so a failed run never leaves a
  # file the idempotency check above would accept next time.
  mv "${IMAGE_TMP}/${IMAGE_NAME}" "${BASE_IMAGE}"
  rm -rf "${IMAGE_TMP}"
  trap - EXIT
  log "Base image ready at ${BASE_IMAGE} ($(du -h "${BASE_IMAGE}" | cut -f1))"
fi

# ── 8. inotify capacity ─────────────────────────────────────────────────────
# kubelet starts cAdvisor, which takes an inotify instance per container. The
# stock 128 runs out once nico-vms, the seed cluster and the tenant nodes are
# all up on one host: kubelet stays "activating" with "inotify_init: too many
# open files", no static pod starts, and kubeadm init fails in
# wait-control-plane with a message that names none of this.
#
# nico-vms-e2e.yml raises the same two values for its runner. A host prepared
# by this script needs them just as much, and gets them here so it does not
# depend on the caller having done it.

raise_sysctl_minimum() { # raise_sysctl_minimum <key> <minimum>
  local key="$1" want="$2" have
  have="$(sysctl -n "${key}" 2>/dev/null || echo 0)"
  if [[ "${have}" -lt "${want}" ]]; then
    sysctl -qw "${key}=${want}" && log "${key}: ${have} -> ${want}"
  else
    log "${key}: ${have}, already at or above ${want}"
  fi
}

raise_sysctl_minimum fs.inotify.max_user_instances 1024
raise_sysctl_minimum fs.inotify.max_user_watches 1048576

# ── 9. Summary ──────────────────────────────────────────────────────────────

log "Host ready. nico-vms should be run with:"
log "  NICO_VMS_BRIDGE_NAME=${BRIDGE_NAME}"
log "  NICO_VMS_BRIDGE_CIDR=${BRIDGE_CIDR}"
log "  NICO_VMS_DHCP_HOSTS_DIR=${DHCP_HOSTS_DIR}"
log "  NICO_VMS_DHCP_LEASE_FILE=${DHCP_LEASE_FILE}"
log "  NICO_VMS_LIBVIRT_NETWORK unset (native bridge mode -- config.go:93)"
if [[ "${FDE}" == true ]]; then
  log "  FDE image support on: ovmf, swtpm, carbide-pxe.forge -> ${BRIDGE_GW}, 169.254.169.254:7777 -> ${BRIDGE_GW}:7777"
fi

# Hand the same values to the rest of the job so the workflow does not have to
# repeat the defaults and drift from them. Same mechanism shellcheck.yml:108
# uses to pass state between steps.
if [[ -n "${GITHUB_ENV:-}" ]]; then
  {
    echo "NICO_VMS_BRIDGE_NAME=${BRIDGE_NAME}"
    echo "NICO_VMS_BRIDGE_CIDR=${BRIDGE_CIDR}"
    echo "NICO_VMS_DHCP_HOSTS_DIR=${DHCP_HOSTS_DIR}"
    echo "NICO_VMS_DHCP_LEASE_FILE=${DHCP_LEASE_FILE}"
  } >>"${GITHUB_ENV}"
  log "Exported the NICO_VMS_* bridge settings to GITHUB_ENV"
fi
