#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# Configure kata requirements: /dev/shm and IOMMU settings

set -e

# Environment variables with defaults
KATA_SHM_SIZE="${KATA_SHM_SIZE:-768G}"
KATA_GRUB_FILE="${KATA_GRUB_FILE:-/etc/default/grub}"

echo "Configuring kata requirements..."

# Configure /dev/shm
echo "Configuring /dev/shm..."
if ! grep -q "^none /dev/shm tmpfs defaults,size=$KATA_SHM_SIZE 0 0" /etc/fstab; then
    # Remove any existing /dev/shm entry
    sed -i '/^.*[[:space:]]\/dev\/shm[[:space:]].*$/d' /etc/fstab
    # Add new entry
    echo "none /dev/shm tmpfs defaults,size=$KATA_SHM_SIZE 0 0" >> /etc/fstab
fi

# Mount /dev/shm with new settings
mount -o remount /dev/shm

# Configure GRUB settings
echo "Configuring GRUB settings..."
GRUB_UPDATED=0

# Enable IOMMU
if ! grep -q "intel_iommu=on" "$KATA_GRUB_FILE"; then
    sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"$/GRUB_CMDLINE_LINUX_DEFAULT="\1 intel_iommu=on"/' "$KATA_GRUB_FILE"
    GRUB_UPDATED=1
fi

# Disable nouveau driver
if ! grep -q "modprobe.blacklist=nouveau" "$KATA_GRUB_FILE"; then
    sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"$/GRUB_CMDLINE_LINUX_DEFAULT="\1 modprobe.blacklist=nouveau"/' "$KATA_GRUB_FILE"
    GRUB_UPDATED=1
fi

# Update GRUB if changes were made
if [ "$GRUB_UPDATED" -eq 1 ]; then
    update-grub
fi

# Check IOMMU status
if [ ! -d "/sys/kernel/iommu_groups" ] || [ -z "$(ls -A /sys/kernel/iommu_groups)" ]; then
    echo "IOMMU is not properly enabled. A reboot is required."
    exit 1
fi

echo "Kata configuration completed successfully"
exit 0
