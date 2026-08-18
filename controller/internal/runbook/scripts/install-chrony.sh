#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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
