#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Purpose: Installs GPU-specific apt packages from the Forge/nke apt repository.
# Prerequisites: apt sources configured (nke-apt.list from OS profile cloud-init)

set -euo pipefail

GPU_APT_PACKAGES=${GPU_APT_PACKAGES:-"displaymode-selector nvflash"}
DISPLAYMODESELECTOR_PATH=${DISPLAYMODESELECTOR_PATH:-/usr/local/bin/displaymodeselector}
NVFLASH_PATH=${NVFLASH_PATH:-/usr/local/bin/nvflash}

error() {
    echo "Error: $1" >&2
    exit 1
}

package_installed() {
    local spec="$1"
    local name="${spec%%=*}"

    if [[ "$spec" == *"="* ]]; then
        local version="${spec#*=}"
        local installed
        installed=$(dpkg-query -W -f='${Version}' "$name" 2>/dev/null) || return 1
        [[ "$installed" == "$version" ]]
        return
    fi

    dpkg -s "$name" &>/dev/null
}

install_packages() {
    local to_install=()
    local pkg

    for pkg in $GPU_APT_PACKAGES; do
        if package_installed "$pkg"; then
            echo "$pkg already installed"
        else
            to_install+=("$pkg")
        fi
    done

    if ((${#to_install[@]} == 0)); then
        echo "All GPU packages already installed"
        return 0
    fi

    echo "Installing GPU packages: ${to_install[*]}"
    if ! apt-get update -qq; then
        error "Failed to update package lists"
    fi

    if ! DEBIAN_FRONTEND=noninteractive apt-get install -y "${to_install[@]}"; then
        error "Failed to install GPU packages: ${to_install[*]}"
    fi

    echo "GPU packages installed successfully"
}

verify_binaries() {
    local pkg

    for pkg in $GPU_APT_PACKAGES; do
        case "${pkg%%=*}" in
            displaymode-selector)
                if [[ ! -x "$DISPLAYMODESELECTOR_PATH" ]]; then
                    error "displaymode-selector installed but $DISPLAYMODESELECTOR_PATH is missing or not executable"
                fi
                echo "Verified $DISPLAYMODESELECTOR_PATH"
                ;;
            nvflash)
                if [[ ! -x "$NVFLASH_PATH" ]]; then
                    error "nvflash installed but $NVFLASH_PATH is missing or not executable"
                fi
                echo "Verified $NVFLASH_PATH"
                ;;
            *)
                echo "No binary verification configured for package ${pkg%%=*}"
                ;;
        esac
    done
}

main() {
    if [[ -z "${GPU_APT_PACKAGES// }" ]]; then
        error "GPU_APT_PACKAGES is empty"
    fi

    install_packages
    verify_binaries
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
