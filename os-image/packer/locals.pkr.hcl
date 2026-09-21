# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

locals {
  project_root = abspath(path.root)

  # The caller authenticates the manifest and stages these fixed NKE-owned
  # bindings beside it. Packer consumes local bytes and performs no acquisition.
  material_description = jsondecode(file(var.material_manifest))
  material_root        = dirname(var.material_manifest)
  installer_records = [for material in local.material_description.materials : material
    if material.binding == "installer-iso" && material.path == "materials/installer-iso"
  ]
  image_input_records = [for material in local.material_description.materials : material
    if material.binding == "image-inputs" && material.path == "materials/image-inputs"
  ]
  installer_record    = local.installer_records[0]
  image_input_record  = local.image_input_records[0]
  architecture        = local.installer_record.architecture
  installer_iso       = "${local.material_root}/${local.installer_record.path}"
  installer_checksum  = local.installer_record.digest
  image_inputs        = "${local.material_root}/${local.image_input_record.path}"
  image_inputs_digest = local.image_input_record.digest
  staged_image_inputs = {
    fde     = "${var.output_directory}/fde/.image-inputs"
    non_fde = "${var.output_directory}/non-fde/.image-inputs"
  }
  ssh_public_key      = trimspace(file(var.ssh_public_key_file))
  build_log_directory = var.build_log_directory == "" ? var.output_directory : var.build_log_directory

  # Architecture comes from the authenticated installer material instead of a
  # second caller-controlled variable, so the ISO, QEMU binary, machine and
  # firmware cannot drift apart. ARM uses virt, AAVMF, the PL011 serial
  # console, and the NVIDIA 64K-page kernel. AMD64 uses q35, OVMF, ttyS0, and
  # linux-nvidia.
  nvidia_kernel_package = local.architecture == "arm64" ? "linux-nvidia-64k" : "linux-nvidia"
  serial_console        = local.architecture == "arm64" ? "ttyAMA0" : "ttyS0"
  qemu_binary           = local.architecture == "arm64" ? "qemu-system-aarch64" : "qemu-system-x86_64"
  machine_type          = local.architecture == "arm64" ? "virt" : "q35"
  efi_firmware_code     = local.architecture == "arm64" ? "/usr/share/AAVMF/AAVMF_CODE.fd" : "/usr/share/OVMF/OVMF_CODE_4M.fd"
  efi_firmware_vars     = local.architecture == "arm64" ? "/usr/share/AAVMF/AAVMF_VARS.fd" : "/usr/share/OVMF/OVMF_VARS_4M.fd"

  # ARM has no fixed cd0 alias, so GRUB locates the installer by content.
  # Under TCG, BIOS and GRUB timing varies widely. The source-owned ISO
  # preparation widens GRUB's timeout to 300 seconds so boot_wait only has to
  # land after firmware startup, rather than race the stock menu timeout.
  boot_command = local.architecture == "arm64" ? [
    "<wait5>",
    "c<wait>",
    "search --set=root --file /casper/vmlinuz<enter><wait>",
    "linux /casper/vmlinuz autoinstall console=ttyAMA0 ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ---<enter><wait>",
    "initrd /casper/initrd<enter><wait>",
    "boot<enter>"
    ] : [
    "<wait3>",
    "c<wait>",
    "set root=(cd0)<enter><wait>",
    "linux /casper/vmlinuz autoinstall console=ttyS0,115200 ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ---<enter><wait>",
    "initrd /casper/initrd<enter><wait>",
    "boot<enter>"
  ]

  # QEMU's ARM virt machine has no legacy VGA or default keyboard controller.
  # These devices let Packer's VNC boot command reach GRUB; strict=off removes
  # the qemu builder's x86-only `-boot once=d` default.
  architecture_qemuargs = local.architecture == "arm64" ? [
    ["-vga", "none"],
    ["-device", "virtio-gpu-pci"],
    ["-device", "qemu-xhci"],
    ["-device", "usb-kbd"],
    ["-boot", "strict=off"],
  ] : []
}
