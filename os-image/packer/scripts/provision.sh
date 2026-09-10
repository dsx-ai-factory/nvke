#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Guest provisioner. Artifacts are already in /tmp/nke-image.

set -euo pipefail

SRC=/tmp/nke-image
ROOT_FS_LABEL="${ROOT_FS_LABEL:?ROOT_FS_LABEL is required}"

sudo install -d -m 0755 /usr/lib/nke /etc/nke /var/lib/nke /usr/local/bin
sudo install -m 0755 "${SRC}/spire-agent" /usr/local/bin/spire-agent
sudo install -d -m 0755 /usr/local/lib/spire-agent
sudo install -m 0755 "${SRC}/tpm_attestor_agent" /usr/local/lib/spire-agent/tpm_attestor_agent
sudo install -m 0755 "${SRC}/get_tpm_pubhash" /usr/local/bin/get_tpm_pubhash
sudo install -m 0755 "${SRC}/spire-configure.sh" /usr/lib/nke/spire-configure.sh
sudo install -m 0755 "${SRC}/wait-spiffe-socket.sh" /usr/lib/nke/wait-spiffe-socket.sh
sudo install -m 0644 "${SRC}/spire-agent.conf.tmpl" /usr/lib/nke/spire-agent.conf.tmpl
sudo install -m 0644 "${SRC}/spire-agent.service" /etc/systemd/system/spire-agent.service
sudo install -m 0644 "${SRC}/nke-spire-configure.service" /etc/systemd/system/nke-spire-configure.service

sudo install -m 0755 "${SRC}/nke-agent" /usr/local/bin/nke-agent
sudo install -m 0644 "${SRC}/nke-agent.service" /etc/systemd/system/nke-agent.service

if [[ ! -f /etc/default/nke-agent ]]; then
  echo 'AGENT_ARGS=""' | sudo tee /etc/default/nke-agent >/dev/null
fi

if ! id -u spire >/dev/null 2>&1; then
  sudo useradd -r -s /bin/false -d /var/lib/spire-agent spire
fi
# The TPM node attestor opens /dev/tpmrm0, which udev owns tss:tss. Without this
# spire-agent crash-loops on "permission denied" and nke-agent never starts,
# because it waits on a workload socket that never opens.
if getent group tss >/dev/null 2>&1; then
  sudo usermod -aG tss spire
fi
sudo install -d -m 0750 -o spire -g spire /var/lib/spire-agent

sudo systemctl daemon-reload
sudo systemctl enable nke-spire-configure.service
sudo systemctl enable spire-agent.service
sudo systemctl enable nke-agent.service

# Stage the CNI debs as a local flat apt repo, so cluster create's `apt-get
# install nke-cni-...` resolves offline with apt's dependency handling intact.
# trusted=yes: the content is already on this root fs. amd64 artifacts only.
if [[ -d "${SRC}/debs" ]]; then
  sudo install -d -m 0755 /usr/lib/nke/debs
  sudo install -m 0644 "${SRC}"/debs/*.deb "${SRC}/debs/Packages" /usr/lib/nke/debs/
  echo 'deb [trusted=yes] file:/usr/lib/nke/debs ./' | sudo tee /etc/apt/sources.list.d/nke-local.list >/dev/null
fi

# Install the Calico manifest package. The content-only ARM64 build writes an
# `_all.deb` package, while the normal AMD64 build writes an `_amd64.deb`
# package.
calico_manifest_deb="$(find "${SRC}/debs" -maxdepth 1 -type f -name 'nke-cni-calico-manifests-*.deb' -print -quit 2>/dev/null || true)"
if [[ -n "${calico_manifest_deb}" ]]; then
  sudo dpkg -i "${calico_manifest_deb}"
fi

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

# Golden image: next boot must look like a new instance, without the Packer
# SSH key or a machine-id from this build VM.
sudo cloud-init clean --logs --seed || true
if [[ -f /etc/machine-id ]]; then
  sudo truncate -s 0 /etc/machine-id
fi
sudo rm -f /var/lib/dbus/machine-id
sudo rm -f /home/ubuntu/.ssh/authorized_keys

# Smaller qcow2: apt's package cache and now-unreferenced blocks (deleted
# files, old package versions) don't shrink the disk image on their own --
# fstrim tells the guest kernel to discard them, which the disk_discard =
# "unmap" builder setting (nke-node-fde.pkr.hcl / nke-node-non-fde.pkr.hcl)
# passes through to the backing qcow2 so Packer's own post-build compaction
# ("Converting hard drive...") actually has free space to reclaim. Must run
# last: any write after this leaves blocks un-trimmed again.
echo "Cleaning up package cache and trimming filesystem..."
sudo apt-get -y autoremove
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
sudo rm -rf /tmp/nke-image

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
