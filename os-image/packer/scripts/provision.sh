#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Guest provisioner. Artifacts are already in /var/tmp/nke-image.

set -euo pipefail

SRC=/var/tmp/nke-image
ROOT_FS_LABEL="${ROOT_FS_LABEL:?ROOT_FS_LABEL is required}"
UBUNTU_SERIES="${UBUNTU_SERIES:?UBUNTU_SERIES is required}"

# autoinstall chose its release-specific packages from the caller's
# ubuntu_series; refuse an image whose installed release disagrees.
# shellcheck disable=SC1090 # Tests point OS_RELEASE at a fixture.
installed_series=$(. "${OS_RELEASE:-/etc/os-release}" && echo "${VERSION_ID}")
if [[ "${installed_series}" != "${UBUNTU_SERIES}" ]]; then
  echo "ubuntu_series is ${UBUNTU_SERIES}, but the installed system is Ubuntu ${installed_series}" >&2
  exit 1
fi

sudo install -d -m 0755 /usr/lib/nvke /etc/nke /var/lib/nke /usr/local/bin
sudo install -m 0755 "${SRC}/wait-spiffe-socket.sh" /usr/lib/nvke/wait-spiffe-socket.sh
# Conntrack limits. conntrack-tune.service generates its sysctl.d file at
# every boot from that node's RAM; nothing here computes it, so the build VM's
# RAM never reaches the image. Only the RAM-independent TIME_WAIT file is
# baked in.
sudo install -m 0755 "${SRC}/configure-conntrack.sh" /usr/lib/nvke/configure-conntrack.sh
sudo install -m 0644 "${SRC}/conntrack-tune.service" /etc/systemd/system/conntrack-tune.service
sudo install -m 0644 "${SRC}/10-nke-conntrack-timeouts.conf" /etc/sysctl.d/10-nke-conntrack-timeouts.conf
# Load nf_conntrack at boot, before systemd-sysctl.service, so its keys exist
# when conntrack-tune.service compares against them and when systemd-sysctl
# applies both conntrack files. Otherwise it loads on demand (a netfilter
# rule, or the openvswitch module), after the boot pass. nvke-kubeadm-distro's
# modules-load.d/k8s.conf lists it too, but only from cluster create on.
echo 'nf_conntrack' | sudo tee /etc/modules-load.d/nf_conntrack.conf >/dev/null
sudo systemctl enable conntrack-tune.service

# Kata's erofs snapshotter: containerd converts image layers to erofs (with
# dm-verity metadata) and hands them to the guest as block devices. Load both
# modules at boot rather than relying on on-demand loading. 26.04 only,
# matching erofs-utils in cidata/autoinstall-lvm.pkrtpl.
if [[ "${UBUNTU_SERIES}" == 26.04 ]]; then
  printf '%s\n' erofs dm-verity | sudo tee /etc/modules-load.d/nvke-kata.conf >/dev/null
fi

# Own the etcd data directory as etcd:etcd (CIS 1.1.12). kubeadm creates
# /var/lib/etcd as root:root during `kubeadm init` and exposes no knob for its
# ownership, so the only way to influence it is to have the directory already
# exist: kubeadm reuses one it finds rather than creating its own, and leaves
# the ownership alone. Doing it at bake time also means it is in place before
# any control-plane node is ever provisioned.
#
# There is no etcd user on a node otherwise -- etcd runs from
# registry.k8s.io/etcd as a static pod, as root, mounting this directory as a
# hostPath. Root bypasses directory permissions, so an etcd-owned 0700 data
# directory stays writable by it; this changes what the node reports, not what
# etcd can do. Mirrors how the SPIRE agent's own user and state directory are
# created (files/spire-configure.sh).
id -u etcd >/dev/null 2>&1 || sudo useradd -r -s /bin/false -d /var/lib/etcd etcd
sudo install -d -m 0700 -o etcd -g etcd /var/lib/etcd

# Keep image-specific identity ordering in a drop-in so agent upgrades retain it.
sudo install -d -m 0755 /etc/systemd/system/nvke-agent.service.d
sudo install -m 0644 "${SRC}/nvke-agent-spire.conf" /etc/systemd/system/nvke-agent.service.d/10-spire.conf
# The package enables the agent and creates the default environment file, but
# the golden image has no instance identity yet: start only on first boot.
sudo env NKE_AGENT_SKIP_START=1 dpkg -i "${SRC}/nvke-agent.deb"

# Stage the CNI debs as a local flat apt repo, so cluster create's `apt-get
# install nvke-cni-...` resolves offline with apt's dependency handling intact.
# trusted=yes: the content is already on this root fs.
if [[ -d "${SRC}/debs" ]]; then
  sudo install -d -m 0755 /usr/lib/nvke/debs
  sudo install -m 0644 "${SRC}"/debs/*.deb "${SRC}/debs/Packages" /usr/lib/nvke/debs/
  echo 'deb [trusted=yes] file:/usr/lib/nvke/debs ./' | sudo tee /etc/apt/sources.list.d/nke-local.list >/dev/null
fi

# Install the Calico manifest package for this image architecture.
calico_manifest_deb="$(find "${SRC}/debs" -maxdepth 1 -type f -name 'nvke-cni-calico-manifests-*.deb' -print -quit 2>/dev/null || true)"
if [[ -n "${calico_manifest_deb}" ]]; then
  sudo dpkg -i "${calico_manifest_deb}"
fi

# Install the spire-agent package. Unlike the CNI and kubeadm-distro debs --
# which nvke-agent installs by name at cluster create, choosing a variant and a
# version then -- SPIRE is how a node gets an identity at all, so it is baked
# in rather than staged for later. Its postinst creates the spire user, adds it
# to tss and enables both units, so nothing here repeats that.
spire_agent_deb="$(find "${SRC}/debs" -maxdepth 1 -type f -name 'spire-agent_*.deb' -print -quit 2>/dev/null || true)"
if [[ -z "${spire_agent_deb}" ]]; then
  echo "no spire-agent deb in ${SRC}/debs; the image would have no SPIRE agent and could not join a cluster" >&2
  exit 1
fi
sudo dpkg -i "${spire_agent_deb}"

# NVIDIA's DOCA Open vSwitch in kernel mode, in place of Ubuntu's
# openvswitch-switch (which it Provides, with the same units). ovn-kubernetes
# runs against the host's OVS (values-nke.yaml: ovs-node false). It is baked in
# because installing OVS on a live node disrupts host networking. The DOCA debs
# come from the local repo staged above; their remaining dependencies come from
# the Ubuntu archive.
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y doca-openvswitch-switch
# DOCA adds ovs-exporter.service, which its postinst enables, wanted by
# ovs-vswitchd: a Prometheus endpoint on TCP 6103 on every interface, with no
# authentication. Stock Open vSwitch has no such listener; keep the node's open
# ports what they were.
sudo systemctl mask ovs-exporter.service
# Ubuntu's package enables openvswitch-switch.service; DOCA's postinst does not.
sudo systemctl enable openvswitch-switch.service

# Give cloud-image importers a stable root-filesystem identifier. Do not rely
# on a device path because it can change between the build VM and a target.
root_device="$(findmnt -n -o SOURCE /)"
if [[ ! -b "${root_device}" ]]; then
  echo "Root filesystem source is not a block device: ${root_device}" >&2
  exit 1
fi
sudo e2label "${root_device}" "${ROOT_FS_LABEL}"
if [[ "$(sudo e2label "${root_device}")" != "${ROOT_FS_LABEL}" ]]; then
  echo "Failed to label the root filesystem ${ROOT_FS_LABEL}" >&2
  exit 1
fi

# containerd's erofs snapshotter enables fs-verity on
# /var/lib/containerd/io.containerd.snapshotter.v1.erofs at startup, and with
# the ext4 feature off every plugin that depends on it fails to load, CRI
# included ("enable fsverity failed: operation not supported"), so the node
# goes NotReady as soon as Kata configures that snapshotter. The feature can
# be set on the mounted filesystem.
if [[ "${UBUNTU_SERIES}" == 26.04 ]]; then
  sudo tune2fs -O verity "${root_device}"
fi

# Keep multipathd off the node. Ubuntu Server's installer pulls in
# multipath-tools and leaves multipathd enabled, and with no blacklist it
# claims Longhorn's iSCSI-exported volumes -- they present as
# IET,VIRTUAL-DISK, so multipathd wraps each one in a device-mapper map.
# device-mapper then holds the block device open, mkfs' O_EXCL open returns
# EBUSY, and every newly provisioned Longhorn PVC fails to format with
# "is apparently in use by the system; will not make a filesystem here".
# Observed in the field blocking a Longhorn engine upgrade and a CloudNativePG
# replica rebuild.
#
# Bake it in rather than leaving it to a provisioning step: on nodes whose
# cloud-init came up with no datasource, the equivalent post-provision step
# never ran and multipathd stayed active from first boot, undetected for
# months. An image that ships it off cannot regress that way.
#
# No NVKE node boots from a multipath-backed disk -- the root filesystem is LVM
# (optionally over LUKS) on a single local NVMe device, per
# cidata/autoinstall-lvm.pkrtpl -- so nothing here depends on multipathd.
#
# Both units, and mask rather than disable: multipathd.socket socket-activates
# the service, so disabling the service alone leaves it reachable (systemctl
# says as much: "can still be activated by multipathd.socket"). Masking also
# holds if multipath-tools is installed or reinstalled later, and works even
# though the units may not exist on this image yet.
sudo systemctl disable --now multipathd.socket multipathd.service >/dev/null 2>&1 || true
sudo systemctl mask multipathd.socket multipathd.service

# No background apt on a node. Ubuntu's apt-daily timers run a few minutes
# after first boot and, via needrestart and dpkg triggers, restart journald,
# networkd, udevd and friends. kubeadm's kubelet-start phase runs
# `systemctl daemon-reload` right then on a fresh node and gets exit 1, so the
# kubelet never starts and init or join fails for good. unattended-upgrades
# also holds the dpkg lock against cloud-init's package step. Nodes get updates
# through a new image, not in place.
sudo systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null 2>&1 || true
sudo systemctl mask apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service

# Golden image: next boot must look like a new instance, without the Packer
# SSH key or a machine-id from this build VM.
sudo cloud-init clean --logs --seed || true
if [[ -f /etc/machine-id ]]; then
  sudo truncate -s 0 /etc/machine-id
fi
sudo rm -f /var/lib/dbus/machine-id
sudo rm -f /home/ubuntu/.ssh/authorized_keys

sudo install -d -m 0755 /etc/cloud/cloud.cfg.d /etc/netplan
sudo install -m 0644 "${SRC}/99-disable-network-config.cfg" /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg
sudo install -m 0600 "${SRC}/00-dhcp-all.yaml" /etc/netplan/00-dhcp-all.yaml
# Remove the installer config so DHCP cannot also run on a bridged physical NIC.
sudo rm -f /etc/netplan/50-cloud-init.yaml

# cidata/autoinstall-lvm.pkrtpl sets kernel.package to the NVIDIA-tuned
# flavor specifically so, per its own comment, "there's no generic kernel to
# reboot into or purge afterward" -- but curtin installs it alongside the
# live ISO's default linux-generic rather than replacing it, leaving a
# second, never-booted kernel (image + modules + headers, ~450MB) on the
# golden image. Guard on the running kernel so this can never remove what's
# actually booted, in case that assumption ever stops holding.
running_kernel="$(uname -r)"
if [[ "${running_kernel}" == *generic* ]]; then
  echo "refusing to purge the generic kernel: the running kernel (${running_kernel}) looks like a generic flavor" >&2
  exit 1
fi

for pkg in linux-generic linux-headers-generic linux-image-generic; do
  if dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q "^install ok installed$"; then
    sudo apt-get -y purge "${pkg}"
  fi
done

# Purging the meta-packages above does not cascade to their versioned
# dependents: `apt-get autoremove` (below) deliberately never fully removes
# a kernel family -- /etc/apt/apt.conf.d/01autoremove's
# VersionedKernelPackages groups every "linux-.*" package and keeps at least
# one full kernel per flavor installed as a safety net against an
# unbootable system, regardless of its auto-install mark. Confirmed on a
# real build: autoremove left linux-image-*-generic, linux-modules-*-generic,
# linux-headers-*-generic/*-generic's arch-independent linux-headers-<ver>,
# linux-tools-*-generic and its zfs modules all installed. Purge that whole
# family by exact name instead, the same way an explicit `apt-get purge
# <version>` from the command line bypasses the same protection.
# dpkg-query -W lists a package regardless of its current state, so an
# already-purged package still known to dpkg (e.g. the old
# linux-image-unsigned-7.0.0-14-generic rc-state leftover the "Cleaning up"
# autoremove below sweeps up) can match this glob and win `head -1` over the
# actually-installed kernel -- filter to installed packages explicitly.
# Querying with no pattern (filtering in awk instead) also avoids dpkg-query
# exiting 1 -- which set -euo pipefail would treat as fatal here -- on a
# system where dpkg has no record at all matching the glob.
# Consume all input: exiting awk early can give dpkg-query SIGPIPE under pipefail.
generic_image_pkg="$(dpkg-query -W -f='${Package}\t${Status}\n' 2>/dev/null |
  awk -F'\t' '!found && $1 ~ /^linux-image-.*-generic$/ && $2 == "install ok installed" { print $1; found=1 }')"
if [[ -n "${generic_image_pkg}" ]]; then
  ver="${generic_image_pkg#linux-image-}"      # e.g. 7.0.0-31-generic
  ver_noflavor="${ver%-generic}"                # e.g. 7.0.0-31
  generic_kernel_candidates=(
    "linux-image-${ver}"
    "linux-modules-${ver}"
    "linux-modules-extra-${ver}"
    "linux-headers-${ver}"
    "linux-headers-${ver_noflavor}"
    "linux-tools-${ver}"
    "linux-tools-${ver_noflavor}"
    "linux-main-modules-zfs-${ver}"
  )
  generic_kernel_pkgs=()
  for pkg in "${generic_kernel_candidates[@]}"; do
    if dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q "^install ok installed$"; then
      generic_kernel_pkgs+=("${pkg}")
    fi
  done
  if [[ "${#generic_kernel_pkgs[@]}" -gt 0 ]]; then
    sudo apt-get -y purge "${generic_kernel_pkgs[@]}"
  fi
fi

# Ubuntu splits linux-firmware into one binary package per hardware vendor,
# but the linux-firmware meta-package hard-Depends on all of them, and
# linux-image-nvidia (the kernel this image actually boots) hard-Depends on
# that meta -- `apt-get purge` on an unwanted vendor split would therefore
# either be refused or cascade into removing the booted kernel's own
# package. There is no clean `apt purge` here, so delete the files each
# unwanted package owns directly and leave dpkg's database (and every
# Depends chain through it) untouched.
#
# This image only ever runs on bare-metal NVIDIA GPU nodes with Mellanox
# ConnectX NICs -- ConnectX firmware lives on-card (flashed via MFT), not in
# this package, and linux-firmware-mellanox-spectrum is for Mellanox
# Spectrum *switch* ASICs, not NICs. None of the phone/laptop wifi+BT,
# integrated graphics, or switch-ASIC vendors below apply. Kept:
# linux-firmware-misc (carries the intel-ucode CPU microcode, vendor-
# independent), linux-firmware-nvidia-graphics, and linux-firmware-intel-misc
# (unclear which Intel NIC/adapter firmware it bundles; small enough to
# leave alone rather than guess).
unwanted_firmware_pkgs=(
  linux-firmware-qualcomm-misc
  linux-firmware-qualcomm-wireless
  linux-firmware-qualcomm-graphics
  linux-firmware-intel-wireless
  linux-firmware-intel-graphics
  linux-firmware-mellanox-spectrum
  linux-firmware-marvell-prestera
  linux-firmware-marvell-wireless
  linux-firmware-amd-graphics
  linux-firmware-amd-misc
  linux-firmware-mediatek
  linux-firmware-broadcom-wireless
  linux-firmware-realtek
  linux-firmware-qlogic
  linux-firmware-netronome
)
for pkg in "${unwanted_firmware_pkgs[@]}"; do
  if dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q "^install ok installed$"; then
    dpkg -L "${pkg}" | while IFS= read -r f; do
      if [[ -f "${f}" ]]; then
        sudo rm -f "${f}"
      fi
    done
  fi
done

# Smaller qcow2: apt's package cache and now-unreferenced blocks (deleted
# files, old package versions) don't shrink the disk image on their own --
# fstrim tells the guest kernel to discard them, which the disk_discard =
# "unmap" builder setting (nvke-node-fde.pkr.hcl / nvke-node-non-fde.pkr.hcl)
# passes through to the backing qcow2 so Packer's own post-build compaction
# ("Converting hard drive...") actually has free space to reclaim. Must run
# last: any write after this leaves blocks un-trimmed again.
echo "Cleaning up package cache and trimming filesystem..."
sudo apt-get -y --purge autoremove
sudo apt-get clean

# apt's downloaded package indexes and installed man/doc/translation files
# are dead weight on a golden image that never runs `apt-get update`/install
# again after this: nothing on the node reads them. `en` is kept under
# /usr/share/locale so English program strings still resolve; compiled
# system locales (/usr/lib/locale, used by glibc for LC_* generation) are
# untouched -- this only drops the separate per-package translation catalogs.
sudo rm -rf /var/lib/apt/lists/*
sudo rm -rf /usr/share/doc/* /usr/share/man/*
find /usr/share/locale -mindepth 1 -maxdepth 1 ! -name en -exec sudo rm -rf {} +

# Index only the local repo, after the cleanup above, so cluster create needs
# no apt-get update of its own and no Ubuntu archive lists get baked in.
if [[ -f /etc/apt/sources.list.d/nke-local.list ]]; then
  sudo apt-get update \
    -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/nke-local.list \
    -o Dir::Etc::sourceparts=- \
    -o APT::Get::List-Cleanup=0
fi

# The uploaded artifacts (gigabytes of debs) must not stay in the qcow2.
sudo rm -rf /var/tmp/nke-image

sudo fstrim -av || true

# Belt-and-suspenders for fstrim/disk_discard=unmap: TRIM passthrough
# through virtio isn't guaranteed, especially under the tcg (software
# emulation) accelerator this module's Makefile falls back to when the
# builder has no usable /dev/kvm. Any block fstrim failed to discard still
# looks "used" to qemu-img's sparse detection. Zero-filling the rest of the
# free space guarantees those blocks compress away regardless. Skipped on
# LUKS: zeros written through dm-crypt come out as ciphertext, not zeros, so
# this does nothing there -- the FDE variant relies on the discard chain
# (disk_discard=unmap + crypttab's discard option) alone.
if ! sudo lsblk -ln -o FSTYPE | grep -q crypto_LUKS; then
  echo "Zeroing free space for better compression..."
  AVAILABLE_MB=$(df -m / | tail -1 | awk '{print $4}')
  RESERVE_MB=100
  FILL_MB=$((AVAILABLE_MB - RESERVE_MB))
  if [[ ${FILL_MB} -gt 0 ]]; then
    sudo dd if=/dev/zero of=/EMPTY bs=1M count="${FILL_MB}" || true
    sudo rm -f /EMPTY
    sync
  fi
fi
