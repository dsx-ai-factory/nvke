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

# Install Lustre client modules and utils
# FIXME: The original Ansible uses a locally stored .deb file for installation.
# We will need to download the .deb files from the Lustre repository and store them in the assets directory.

set -e

# Default version if not provided
LUSTRE_VERSION="${LUSTRE_VERSION:-2.15.4-1~jammy}"
ASSETS_DIR="/home/nke/assets"
KERNEL_LOCK="/etc/apt/preferences.d/nvidia-kernel-lock"
TMP_KERNEL_LOCK="$ASSETS_DIR/nvidia-kernel-lock"

error() {
    echo "Error: $@"
    exit 1
}

restore_kernel() {
    echo "Restoring kernel lock file"
    mv $TMP_KERNEL_LOCK $KERNEL_LOCK 2>/dev/null || true
}

trap restore_kernel EXIT

# Create assets directory
mkdir -p "$ASSETS_DIR" && chmod 755 "$ASSETS_DIR"

MODULES_DEB_URL="${MODULES_DEB_URL:-"https://nke-dev.s3.us-west-2.amazonaws.com/lustre-client/lustre-client-modules-dkms_${LUSTRE_VERSION}_amd64.deb"}"
UTILS_DEB_URL="${UTILS_DEB_URL:-"https://nke-dev.s3.us-west-2.amazonaws.com/lustre-client/lustre-client-utils_${LUSTRE_VERSION}_amd64.deb"}"

ASSETS_DIR="/home/nke/assets"
MODULES_DEB="$ASSETS_DIR/lustre-client-modules.deb"
UTILS_DEB="$ASSETS_DIR/lustre-client-utils.deb"

mkdir -p "$ASSETS_DIR" && chmod 755 "$ASSETS_DIR"
wget -q $MODULES_DEB_URL -O $MODULES_DEB
wget -q $UTILS_DEB_URL -O $UTILS_DEB

# Move kernel lock file if it exists
if [ -f "$KERNEL_LOCK" ]; then
    echo "Moving kernel lock file..."
    mv "$KERNEL_LOCK" "$TMP_KERNEL_LOCK"
fi

# Install Lustre client modules
echo "Installing Lustre client modules..."
if ! dpkg -i "$MODULES_DEB" 1>/dev/null; then
    echo "Attempting to fix dependencies..."
    apt install --fix-broken -y 1>/dev/null
    if ! dpkg -i "$MODULES_DEB"; then
        error "failed to install Lustre client modules"
    fi
fi

# Install Lustre client utils
echo "Installing Lustre client utils..."
if ! dpkg -i "$UTILS_DEB" 1>/dev/null; then
    echo "Attempting to fix dependencies..."
    apt install --fix-broken -y 1>/dev/null
    if ! dpkg -i "$UTILS_DEB"; then
        error "failed to install Lustre client utils"
    fi
fi

echo "Lustre installation completed successfully"
