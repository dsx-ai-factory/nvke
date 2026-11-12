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

if [[ -z "${NICO_VMS_VM_BACKEND:-}" ]]; then
	case "$(uname -s)" in
	Darwin)
		NICO_VMS_VM_BACKEND="darwin-qemu"
		;;
	Linux)
		NICO_VMS_VM_BACKEND="libvirt"
		;;
	*)
		echo "Unsupported OS for auto backend selection: $(uname -s)"
		echo "Set NICO_VMS_VM_BACKEND explicitly (mock|libvirt|darwin-qemu)."
		exit 1
		;;
	esac
fi
export NICO_VMS_VM_BACKEND

if [[ "${NICO_VMS_VM_BACKEND}" != "mock" && -z "${NICO_VMS_BASE_IMAGE:-}" ]]; then
	echo "NICO_VMS_BASE_IMAGE is required for backend ${NICO_VMS_VM_BACKEND}."
	echo "Example:"
	echo "  NICO_VMS_BASE_IMAGE=~/Downloads/noble-server-cloudimg-arm64.img make run-vm"
	exit 1
fi
if [[ "${NICO_VMS_VM_BACKEND}" != "mock" ]]; then
	NICO_VMS_BASE_IMAGE="$(expand_user_path "${NICO_VMS_BASE_IMAGE}")"
	if [[ ! -f "${NICO_VMS_BASE_IMAGE}" ]]; then
		echo "NICO_VMS_BASE_IMAGE does not exist or is not a file: ${NICO_VMS_BASE_IMAGE}"
		exit 1
	fi
fi
export NICO_VMS_BASE_IMAGE

if [[ "${NICO_VMS_VM_BACKEND}" == "darwin-qemu" ]]; then
	if [[ -z "${NICO_VMS_QEMU_SYSTEM_PATH:-}" ]]; then
		case "$(uname -m)" in
		arm64) qemu=qemu-system-aarch64 ;;
		x86_64) qemu=qemu-system-x86_64 ;;
		*) echo "Unsupported Mac architecture: $(uname -m)" >&2; exit 1 ;;
		esac
		NICO_VMS_QEMU_SYSTEM_PATH="$(command -v "$qemu")" || {
			echo "Missing $qemu; see README.md macOS setup" >&2
			exit 1
		}
	fi
	export NICO_VMS_QEMU_SYSTEM_PATH

	# The artifact builder can use mkisofs/genisoimage directly when
	# cloud-localds is absent. Do not require the Linux helper on macOS.

fi

echo "Starting nico-sim with the nico-vms backend"
echo "  backend: ${NICO_VMS_VM_BACKEND}"
if [[ -n "${NICO_SIM_LISTEN_ADDR:-}" ]]; then
	echo "  listen: ${NICO_SIM_LISTEN_ADDR}"
fi
if [[ -n "${NICO_VMS_BASE_IMAGE:-}" ]]; then
	echo "  base image: ${NICO_VMS_BASE_IMAGE}"
fi

cd "${repo_root}"
make build-privileged
exec ./bin/nico-sim
