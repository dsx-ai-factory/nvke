#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.


# Check if swap is enabled
SWAP_TOTAL=$(free | awk '/^Swap:/ { print $2 }')

if [ "$SWAP_TOTAL" -gt 0 ]; then
    echo "Disabling swap now..."
    # Disable swap now
    swapoff -a
else
    echo "Swap is already disabled."
fi

# Path to the fstab file
FSTAB_FILE="/etc/fstab"
BACKUP_FILE="/etc/fstab.bak.$(date +%F_%T)"

# Create a backup of /etc/fstab
echo "Creating a backup of /etc/fstab at $BACKUP_FILE"
cp "$FSTAB_FILE" "$BACKUP_FILE"

# Comment out swap entries in /etc/fstab to disable swap after reboot
echo "Disabling swap on reboot by commenting out swap entries in /etc/fstab..."
sed -i '/^\s*\S\+\s\+\S\+\s\+swap\s\+/ s/^/#&/' "$FSTAB_FILE"

echo "Swap has been disabled and will remain disabled after reboot."
