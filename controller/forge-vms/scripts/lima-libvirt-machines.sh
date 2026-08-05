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

set -euo pipefail

usage() {
	echo "Usage: $0 <list|clean>"
	echo ""
	echo "  list   List forge-vms libvirt domains (forge-vm-*) inside a Lima VM."
	echo "  clean  Destroy/undefine matching domains and remove integration scratch paths."
}

if [[ $# -ne 1 ]]; then
	usage
	exit 1
fi

mode="$1"
case "${mode}" in
list | clean) ;;
*)
	usage
	exit 1
	;;
esac

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "lima libvirt helper targets are only supported on Darwin hosts."
	exit 1
fi

if ! command -v limactl >/dev/null 2>&1; then
	echo "limactl is required"
	exit 1
fi

vm_name="${LIMA_VM_NAME:-forge-vms}"

if [[ "${mode}" == "list" ]]; then
	limactl shell "${vm_name}" sudo bash -lc '\
set -euo pipefail; \
if ! command -v virsh >/dev/null 2>&1; then \
  echo "virsh is not available in the Lima VM"; \
  exit 1; \
fi; \
found=0; \
while IFS= read -r domain; do \
  [ -z "$domain" ] && continue; \
  case "$domain" in \
    forge-vm-*) \
      found=1; \
      state=$(virsh -c qemu:///system domstate "$domain" 2>/dev/null || echo unknown); \
      printf "%-48s %s\n" "$domain" "$state"; \
      ;; \
  esac; \
done < <(virsh -c qemu:///system list --all --name || true); \
if [ "$found" -eq 0 ]; then \
  echo "No forge-vm-* domains found."; \
fi'
	exit 0
fi

limactl shell "${vm_name}" sudo bash -lc '\
set -euo pipefail; \
if ! command -v virsh >/dev/null 2>&1; then \
  echo "virsh is not available in the Lima VM"; \
  exit 1; \
fi; \
cleaned=0; \
while IFS= read -r domain; do \
  [ -z "$domain" ] && continue; \
  case "$domain" in \
    forge-vm-*) \
      echo "Cleaning $domain"; \
      virsh -c qemu:///system destroy "$domain" >/dev/null 2>&1 || true; \
      virsh -c qemu:///system undefine "$domain" --nvram >/dev/null 2>&1 || \
        virsh -c qemu:///system undefine "$domain" >/dev/null 2>&1 || true; \
      cleaned=$((cleaned + 1)); \
      ;; \
  esac; \
done < <(virsh -c qemu:///system list --all --name || true); \
rm -rf /tmp/forge-vms-integration /tmp/forge-vms-realvm* || true; \
echo "Processed $cleaned domain(s)."'
