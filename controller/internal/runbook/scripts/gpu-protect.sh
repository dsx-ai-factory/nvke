#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
NVFLASH=${NVFLASH:-/usr/local/bin/nvflash}

error() {
    echo "Error: $1" >&2
    exit 1
}

# This script ensures that the L40 GPU can be cleaned up via a secondary bus
# reset. It has two components: 1) check to make sure that the EEPROM of the GPU
# is properly write protected. 2) ubinding of the pciehp driver from the GPU's
# upstream port in order to make sure that the SBR does not cause undesirable
# side effects.
#
# In order to be effective, this script must be run upon boot, before
# the kubelet service starts. If the script exits with a non-zero code, it must
# be treated as node failure and the kubelet must be prevented from starting.

check_gpus_present() {
  if lspci -d 10de: | grep -E 'VGA|3D controller'; then
    echo "Nvidia GPUs are present"
    return 0
  fi

  echo "No Nvidia GPU found, skip further checks."
  exit 0
}

check_nvflash() {
  if [ ! -x "$NVFLASH" ] ; then
    error "nvflash binary not found or not executable"
  fi
}

check_gpu_write_protection() {
  echo "Checking GPU EEPROM write protection.."
  NUM_NV_GPUS=$(lspci -d 10de: | grep -E 'VGA|3D controller' | wc -l)
  for (( i=0 ; i<$NUM_NV_GPUS ; i++ )); do
    if $NVFLASH --index=$i --protectinfo | grep "Lower 7/8" | grep YES; then
      echo "GPU $i is protected"
    else
      echo "error: GPU $i is not protected"
      exit 1
    fi
  done
}

unbind_us_pciehp() {
  echo "Unbinding gpu upstream ports from pciehp driver.."
  for dev in $(lspci -d 10de: -Dmm | grep -E 'VGA|3D controller' | awk '{print $1}'); do
    dev_count=$(find /sys/devices -type d -name "$dev" -print | wc -l)
    if [ "$dev_count" -ne 1 ]; then
      echo "error: expected single device for $dev, found $dev_count"
      exit 1
    fi

    dev_path=$(find /sys/devices -type d -name "$dev" -print | head -1)
    upstream=$(dirname "$dev_path")
    us_bdf=$(basename "$upstream")
    us_id=$(ls "$upstream" | grep "$us_bdf")
    if ls /sys/bus/pci_express/drivers/pciehp | grep "$us_id"; then
      echo "unbinding $us_id from pciehp for device $dev_path"
      echo "$us_id" > /sys/bus/pci_express/drivers/pciehp/unbind
    fi
  done
}

check_us_pciehp_unbound() {
  echo "Verifying that gpu upstream ports are unbound from pciehp driver.."
  for dev in $(lspci -d 10de: -Dmm | grep -E 'VGA|3D controller' | awk '{print $1}'); do
    dev_path=$(find /sys/devices -type d -name "$dev" -print | head -1)
    upstream=$(dirname "$dev_path")
    us_bdf=$(basename "$upstream")
    us_id=$(ls "$upstream" | grep "$us_bdf")
    if ls /sys/bus/pci_express/drivers/pciehp | grep "$us_id"; then
      echo "error: $us_id for device $dev_path still bound to pciehp"
      exit 1
    fi
  done

  echo "No gpu upstream ports are bound to pciehp"
}

main() {
  check_gpus_present
  check_nvflash
  check_gpu_write_protection
  unbind_us_pciehp
  sleep 1
  check_us_pciehp_unbound
}

main "$@"
