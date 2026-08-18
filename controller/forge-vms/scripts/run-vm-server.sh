#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

go_bin="${GO:-go}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

expand_user_path() {
	local p="$1"
	if [[ "$p" == "~" ]]; then
		printf '%s\n' "$HOME"
		return
	fi
	if [[ ${#p} -ge 2 && "${p:0:1}" == "~" && "${p:1:1}" == "/" ]]; then
		printf '%s/%s\n' "$HOME" "${p:2}"
		return
	fi
	printf '%s\n' "$p"
}

if [[ -z "${FORGE_VMS_VM_BACKEND:-}" ]]; then
	case "$(uname -s)" in
	Darwin)
		FORGE_VMS_VM_BACKEND="darwin-qemu"
		;;
	Linux)
		FORGE_VMS_VM_BACKEND="libvirt"
		;;
	*)
		echo "Unsupported OS for auto backend selection: $(uname -s)"
		echo "Set FORGE_VMS_VM_BACKEND explicitly (mock|libvirt|darwin-qemu)."
		exit 1
		;;
	esac
fi
export FORGE_VMS_VM_BACKEND

if [[ "${FORGE_VMS_VM_BACKEND}" != "mock" && -z "${FORGE_VMS_BASE_IMAGE:-}" ]]; then
	echo "FORGE_VMS_BASE_IMAGE is required for backend ${FORGE_VMS_VM_BACKEND}."
	echo "Example:"
	echo "  FORGE_VMS_BASE_IMAGE=~/Downloads/noble-server-cloudimg-arm64.img make run-vm"
	exit 1
fi
if [[ "${FORGE_VMS_VM_BACKEND}" != "mock" ]]; then
	FORGE_VMS_BASE_IMAGE="$(expand_user_path "${FORGE_VMS_BASE_IMAGE}")"
	if [[ ! -f "${FORGE_VMS_BASE_IMAGE}" ]]; then
		echo "FORGE_VMS_BASE_IMAGE does not exist or is not a file: ${FORGE_VMS_BASE_IMAGE}"
		exit 1
	fi
fi
export FORGE_VMS_BASE_IMAGE

if [[ "${FORGE_VMS_VM_BACKEND}" == "darwin-qemu" ]]; then
	if [[ -z "${FORGE_VMS_QEMU_SYSTEM_PATH:-}" ]]; then
		if command -v qemu-system-aarch64 >/dev/null 2>&1; then
			FORGE_VMS_QEMU_SYSTEM_PATH="$(command -v qemu-system-aarch64)"
		elif command -v qemu-system-x86_64 >/dev/null 2>&1; then
			FORGE_VMS_QEMU_SYSTEM_PATH="$(command -v qemu-system-x86_64)"
		else
			echo "No qemu-system binary found. Install dependencies first:"
			echo "  make install-deps-homebrew"
			exit 1
		fi
	fi
	export FORGE_VMS_QEMU_SYSTEM_PATH

	if [[ -z "${FORGE_VMS_CLOUD_LOCALDS_PATH:-}" ]]; then
		if command -v cloud-localds >/dev/null 2>&1; then
			FORGE_VMS_CLOUD_LOCALDS_PATH="$(command -v cloud-localds)"
		elif [[ -x "${repo_root}/scripts/bin/cloud-localds" ]]; then
			FORGE_VMS_CLOUD_LOCALDS_PATH="${repo_root}/scripts/bin/cloud-localds"
		else
			echo "cloud-localds not found. Install dependencies first:"
			echo "  make install-deps-homebrew"
			exit 1
		fi
	fi
	export FORGE_VMS_CLOUD_LOCALDS_PATH
fi

echo "Starting forge-vms"
echo "  backend: ${FORGE_VMS_VM_BACKEND}"
if [[ -n "${FORGE_VMS_LISTEN_ADDR:-}" ]]; then
	echo "  listen: ${FORGE_VMS_LISTEN_ADDR}"
fi
if [[ -n "${FORGE_VMS_BASE_IMAGE:-}" ]]; then
	echo "  base image: ${FORGE_VMS_BASE_IMAGE}"
fi

cd "${repo_root}"
make build-privileged
exec ./bin/forge-vms
