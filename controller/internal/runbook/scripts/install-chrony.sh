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

# Purpose: Installs the chrony NTP service if not already installed
# Prerequisites: System must use apt package manager (Debian/Ubuntu)

set -euo pipefail

error() {
    echo "Error: $1" >&2
    exit 1
}

is_chrony_installed() {
    dpkg -l chrony &> /dev/null
}

install_chrony() {
    if is_chrony_installed; then
        echo "chrony is already installed"
        return 0
    fi

    echo "Installing chrony..."
    if ! apt-get update &> /dev/null; then
        error "Failed to update package lists"
    fi

    if ! apt-get install -y chrony &> /dev/null; then
        error "Failed to install chrony"
    fi

    echo "chrony installed successfully"
}

main() {
    install_chrony
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
