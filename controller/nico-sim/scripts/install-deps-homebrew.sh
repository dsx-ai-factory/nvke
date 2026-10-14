#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "This script is macOS-only."
	exit 1
fi

if ! command -v brew >/dev/null 2>&1; then
	echo "Homebrew is required. Install from https://brew.sh first."
	exit 1
fi

# ── Core dependencies ─────────────────────────────────────────────────────────

for dep in qemu cdrtools swtpm; do
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

# Trust only this reviewed formula, rather than every present and future entry
# in the tap. Current Homebrew requires explicit trust for third-party formulae.
brew trust --formula nirs/vmnet-helper/vmnet-helper

if brew list --formula vmnet-helper >/dev/null 2>&1; then
	echo "vmnet-helper already installed"
else
	echo "Installing vmnet-helper"
	brew install nirs/vmnet-helper/vmnet-helper
fi

vmnet_prefix="$(brew --prefix vmnet-helper)"

# vmnet-client has /opt/vmnet-helper/bin/vmnet-helper hardcoded.
# Create symlinks so it can find the helper binary.
if [[ -x "${vmnet_prefix}/libexec/vmnet-client" && ! -f /opt/vmnet-helper/bin/vmnet-helper ]]; then
	echo ""
	echo "Creating /opt/vmnet-helper/bin symlinks (requires sudo once)..."
	sudo mkdir -p /opt/vmnet-helper/bin
	sudo ln -sf "${vmnet_prefix}/libexec/vmnet-helper" /opt/vmnet-helper/bin/vmnet-helper
	sudo ln -sf "${vmnet_prefix}/libexec/vmnet-client" /opt/vmnet-helper/bin/vmnet-client
	echo "Symlinks created."
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Dependency check:"
for bin in qemu-img mkisofs swtpm swtpm_setup swtpm_localca; do
	if command -v "${bin}" >/dev/null 2>&1; then
		echo "  ${bin} -> $(command -v "${bin}")"
	else
		echo "  ${bin} -> MISSING"
		exit 1
	fi
done

# vmnet-helper
vmnet_client=""
for p in "${vmnet_prefix}/libexec/vmnet-run" /opt/vmnet-helper/bin/vmnet-run \
	"${vmnet_prefix}/libexec/vmnet-client" /opt/vmnet-helper/bin/vmnet-client; do
	if [[ -x "${p}" ]]; then
		vmnet_client="${p}"
		break
	fi
done
if [[ -n "${vmnet_client}" ]]; then
	echo "  vmnet launcher -> ${vmnet_client}"
else
	echo "  vmnet-run / vmnet-client -> MISSING"
	exit 1
fi

if command -v cloud-localds >/dev/null 2>&1; then
	echo "  cloud-localds -> $(command -v cloud-localds)"
else
	echo "  cloud-localds not installed; the existing seed builder uses mkisofs directly"
fi

if command -v qemu-system-aarch64 >/dev/null 2>&1; then
	echo "  qemu-system-aarch64 -> $(command -v qemu-system-aarch64)"
elif command -v qemu-system-x86_64 >/dev/null 2>&1; then
	echo "  qemu-system-x86_64 -> $(command -v qemu-system-x86_64)"
else
	echo "  qemu-system-* -> MISSING"
	exit 1
fi
