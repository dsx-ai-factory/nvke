#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# First-boot storage growth for the non-FDE (no encryption) build variant.
# Grows the partition/PV/LV/filesystem to use the target disk's full size —
# the image ships at build-time disk_size, same as the FDE variant, but with
# no LUKS container in the way. This is the LUKS-less half of
# luks-tpm-firstboot.sh's storage-growth logic, kept as its own script so
# this variant carries no clevis/tpm2-tools/TPM-binding code at all.

set -euo pipefail
exec &> >(tee -a /var/log/lvm-growth-firstboot.log)

echo "$(date): Starting first-boot storage growth..."

ROOT_VG="ubuntu-vg"
ROOT_LV="ubuntu-lv"

PV_DEVICE=$(pvs --noheadings -o pv_name -S vg_name=${ROOT_VG} 2>/dev/null | tr -d ' ')
echo "PV device: ${PV_DEVICE}"

if [[ -n "${PV_DEVICE}" ]]; then
    if [[ "${PV_DEVICE}" =~ ^/dev/nvme ]]; then
        DISK=$(echo "${PV_DEVICE}" | sed 's/p[0-9]*$//')
        PART_NUM=$(echo "${PV_DEVICE}" | grep -oE 'p[0-9]+$' | tr -d 'p')
    else
        DISK=$(echo "${PV_DEVICE}" | sed 's/[0-9]*$//')
        PART_NUM=$(echo "${PV_DEVICE}" | grep -oE '[0-9]+$')
    fi

    echo "Growing partition ${DISK} partition ${PART_NUM}..."
    udevadm settle 2>/dev/null || true
    # sgdisk -e moves the backup GPT header to the end of the disk first —
    # required when the disk was expanded after the image was written.
    sgdisk -e "${DISK}" 2>/dev/null || true
    if parted -s "${DISK}" resizepart "${PART_NUM}" 100% 2>/dev/null; then
        echo "Partition grown successfully."
        partx -u "${DISK}" 2>/dev/null || true
        sleep 1
    else
        echo "Partition already at maximum size or parted failed."
    fi

    echo "Resizing PV ${PV_DEVICE}..."
    if pvresize "${PV_DEVICE}" 2>/dev/null; then
        echo "PV resized successfully."
    else
        echo "PV resize failed or already at maximum size."
    fi

    echo "Extending LV /dev/${ROOT_VG}/${ROOT_LV}..."
    if lvextend -l +100%FREE "/dev/${ROOT_VG}/${ROOT_LV}" 2>/dev/null; then
        echo "LV extended successfully."
    else
        echo "LV already at maximum size or no free space in VG."
    fi

    echo "Resizing filesystem..."
    if resize2fs "/dev/${ROOT_VG}/${ROOT_LV}" 2>/dev/null; then
        echo "Filesystem resized successfully."
    else
        echo "Filesystem resize failed."
    fi
else
    echo "Warning: Could not find PV for VG ${ROOT_VG}"
fi

echo ""
echo "Current disk usage:"
df -h /

date -Iseconds > /root/.lvm-growth-firstboot-done
chmod 600 /root/.lvm-growth-firstboot-done
systemctl disable lvm-growth-firstboot.service
echo "$(date): Storage growth complete."
