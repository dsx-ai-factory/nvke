#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
local_cloud_localds="${repo_root}/scripts/bin/cloud-localds"

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "This script is macOS-only."
	exit 1
fi

if ! command -v brew >/dev/null 2>&1; then
	echo "Homebrew is required. Install from https://brew.sh first."
	exit 1
fi

# ── Core dependencies ─────────────────────────────────────────────────────────

for dep in qemu cdrtools; do
	if brew list --formula "${dep}" >/dev/null 2>&1; then
		echo "${dep} already installed"
	else
		echo "Installing ${dep}"
		brew install "${dep}"
	fi
done

# ── vmnet-helper (unprivileged vmnet networking for QEMU) ─────────────────────
# See: https://github.com/nirs/vmnet-helper

if ! brew tap | grep -q nirs/vmnet-helper; then
	echo "Tapping nirs/vmnet-helper"
	brew tap nirs/vmnet-helper
fi

if brew list --formula vmnet-helper >/dev/null 2>&1; then
	echo "vmnet-helper already installed"
else
	echo "Installing vmnet-helper"
	brew install vmnet-helper
fi

# vmnet-client has /opt/vmnet-helper/bin/vmnet-helper hardcoded.
# Create symlinks so it can find the helper binary.
if [[ ! -f /opt/vmnet-helper/bin/vmnet-helper ]]; then
	echo ""
	echo "Creating /opt/vmnet-helper/bin symlinks (requires sudo once)..."
	sudo mkdir -p /opt/vmnet-helper/bin
	sudo ln -sf /opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper /opt/vmnet-helper/bin/vmnet-helper
	sudo ln -sf /opt/homebrew/opt/vmnet-helper/libexec/vmnet-client /opt/vmnet-helper/bin/vmnet-client
	echo "Symlinks created."
fi

# ── cloud-localds ─────────────────────────────────────────────────────────────

if ! command -v cloud-localds >/dev/null 2>&1; then
	echo ""
	echo "Installing cloud-localds fallback script (patched to use mkisofs)..."
	mkdir -p "$(dirname "${local_cloud_localds}")"
	tmp_path="${local_cloud_localds}.tmp"
	curl -fsSL "https://raw.githubusercontent.com/canonical/cloud-utils/main/bin/cloud-localds" -o "${tmp_path}"
	sed 's/genisoimage/mkisofs/g' "${tmp_path}" > "${local_cloud_localds}"
	chmod +x "${local_cloud_localds}"
	rm -f "${tmp_path}"
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Dependency check:"
for bin in qemu-img mkisofs; do
	if command -v "${bin}" >/dev/null 2>&1; then
		echo "  ${bin} -> $(command -v "${bin}")"
	else
		echo "  ${bin} -> MISSING"
	fi
done

# vmnet-helper
vmnet_client=""
for p in /opt/vmnet-helper/bin/vmnet-client /opt/homebrew/opt/vmnet-helper/libexec/vmnet-client; do
	if [[ -x "${p}" ]]; then
		vmnet_client="${p}"
		break
	fi
done
if [[ -n "${vmnet_client}" ]]; then
	echo "  vmnet-client -> ${vmnet_client}"
else
	echo "  vmnet-client -> MISSING"
fi

if command -v cloud-localds >/dev/null 2>&1; then
	echo "  cloud-localds -> $(command -v cloud-localds)"
elif [[ -x "${local_cloud_localds}" ]]; then
	echo "  cloud-localds (fallback script) -> ${local_cloud_localds}"
	echo ""
	echo "Set this when running VM tests:"
	echo "  export FORGE_VMS_CLOUD_LOCALDS_PATH=${local_cloud_localds}"
else
	echo "  cloud-localds -> MISSING"
	exit 1
fi

if command -v qemu-system-aarch64 >/dev/null 2>&1; then
	echo "  qemu-system-aarch64 -> $(command -v qemu-system-aarch64)"
elif command -v qemu-system-x86_64 >/dev/null 2>&1; then
	echo "  qemu-system-x86_64 -> $(command -v qemu-system-x86_64)"
else
	echo "  qemu-system-* -> MISSING"
fi
