#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Configures deferred (first-boot) LUKS TPM2 binding on top of the LVM+LUKS
# layout autoinstall-fde.pkrtpl already created. See
# os-image/docs/full-disk-encryption.md.
#
# Runs after provision.sh, on a VM that autoinstall partitioned with an
# LVM-on-LUKS layout and an /etc/luks-build.key keyfile slot (added so the
# unattended post-autoinstall reboot can unlock the disk without a human
# typing the passphrase). This script:
#   1. installs clevis/tpm2-tools and the initramfs/kernel-hook plumbing
#   2. removes the build keyfile slot (the passphrase slot set by
#      storage.layout.password remains as the interim unlock method)
#   3. installs and enables luks-tpm-firstboot.service, which performs the
#      actual TPM bind (and passphrase-slot removal) on the target hardware's
#      own TPM at first real boot — TPM binding is machine-specific and
#      cannot happen at build time.

set -euo pipefail

SRC=/tmp/nke-image/luks

echo "Detecting LUKS devices..."
LUKS_DEVICES=$(sudo lsblk -ln -o NAME,FSTYPE | grep crypto_LUKS | awk '{print "/dev/" $1}' || true)
if [[ -z "${LUKS_DEVICES}" ]]; then
    echo "No LUKS devices found; skipping FDE configuration."
    echo "(This script must run on a VM built from the FDE autoinstall layout.)"
    exit 1
fi
echo "Detected LUKS devices: ${LUKS_DEVICES}"

echo "Installing clevis + tpm2-tools..."
sudo DEBIAN_FRONTEND=noninteractive apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
    clevis clevis-luks clevis-tpm2 clevis-initramfs tpm2-tools

sudo install -m 0755 "${SRC}/clevis-luks-regen-hook" /etc/kernel/postinst.d/zz-clevis-regen

echo "Cleaning up stale cryptsetup-initramfs cache..."
sudo rm -rf /var/cache/cryptsetup-initramfs /cryptroot

BUILD_KEYFILE=/etc/luks-build.key
if [[ -f "${BUILD_KEYFILE}" ]]; then
    echo "Removing build-time keyfile slot (passphrase slot remains for TPM binding)..."
    for dev in ${LUKS_DEVICES}; do
        sudo cryptsetup luksRemoveKey "${dev}" "${BUILD_KEYFILE}" 2>/dev/null || true
    done
    sudo rm -f "${BUILD_KEYFILE}"
fi

echo "Regenerating crypttab with luks-<UUID> naming..."
LUKS_UUID=$(sudo blkid -t TYPE=crypto_LUKS -s UUID -o value | head -1)
if [[ -n "${LUKS_UUID}" ]]; then
    printf 'luks-%s UUID=%s none luks,initramfs\n' "${LUKS_UUID}" "${LUKS_UUID}" | sudo tee /etc/crypttab >/dev/null
fi

echo "Disabling cryptsetup-initramfs automatic keyfile generation..."
sudo install -d -m 0755 /etc/cryptsetup-initramfs
if grep -q '^#\?KEYFILE_PATTERN=' /etc/cryptsetup-initramfs/conf-hook 2>/dev/null; then
    sudo sed -i 's/^#\?KEYFILE_PATTERN=.*/KEYFILE_PATTERN=/' /etc/cryptsetup-initramfs/conf-hook
else
    echo 'KEYFILE_PATTERN=' | sudo tee -a /etc/cryptsetup-initramfs/conf-hook >/dev/null
fi

echo "Adding TPM kernel modules to initramfs..."
for mod in tpm tpm_tis_core tpm_tis; do
    grep -qxF "${mod}" /etc/initramfs-tools/modules 2>/dev/null || echo "${mod}" | sudo tee -a /etc/initramfs-tools/modules >/dev/null
done

echo "Installing initramfs scripts..."
sudo install -m 0755 "${SRC}/00-wait-for-crypttab-devices" /etc/initramfs-tools/scripts/local-top/00-wait-for-crypttab-devices
sudo install -m 0755 "${SRC}/luks-cmdline-script" /etc/initramfs-tools/scripts/local-top/luks-cmdline
sudo install -m 0755 "${SRC}/copy-crypttab-hook" /etc/initramfs-tools/hooks/copy-crypttab

echo "Regenerating initramfs from scratch..."
sudo update-initramfs -c -k all

echo "Installing first-boot binding script + service..."
sudo install -m 0700 "${SRC}/luks-tpm-firstboot.sh" /usr/local/bin/luks-tpm-firstboot.sh
sudo install -m 0755 "${SRC}/luks-tpm-verify" /usr/local/bin/luks-tpm-verify
sudo install -m 0644 "${SRC}/luks-tpm-firstboot.service" /etc/systemd/system/luks-tpm-firstboot.service
sudo systemctl daemon-reload
sudo systemctl enable luks-tpm-firstboot.service

echo "FDE configuration complete. TPM binding deferred to first boot on target hardware."
