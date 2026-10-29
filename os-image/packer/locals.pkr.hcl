# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

locals {
  project_root = abspath(path.root)

  # The caller authenticates the manifest and stages these fixed NVKE-owned
  # bindings beside it. Packer consumes local bytes and performs no acquisition.
  material_description = jsondecode(file(var.material_manifest))
  material_root        = dirname(var.material_manifest)
  installer_records = [for material in local.material_description.materials : material
    if material.binding == "installer-iso" && material.path == "materials/installer-iso"
  ]
  image_input_records = [for material in local.material_description.materials : material
    if material.binding == "image-inputs" && material.path == "materials/image-inputs"
  ]
  installer_record   = local.installer_records[0]
  image_input_record = local.image_input_records[0]
  material_bound_architectures = {
    for candidate in ["amd64", "arm64"] : candidate => candidate
    if local.installer_record.architecture == candidate && local.image_input_record.architecture == candidate
  }
  # Indexing the material-bound set makes Packer validation fail closed when the
  # requested architecture does not match both authenticated material records.
  architecture        = local.material_bound_architectures[var.architecture]
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

  # The requested architecture is validated against the authenticated material
  # manifest before it selects QEMU, firmware, console, and guest packages.
  # ARM uses virt, AAVMF, the PL011 serial console, and the NVIDIA 64K-page
  # kernel. AMD64 uses q35, OVMF, ttyS0, and linux-nvidia. var.kernel_package
  # overrides the kernel, e.g. linux-generic for Apple-silicon HVF guests.
  nvidia_kernel_package = var.kernel_package != "" ? var.kernel_package : (local.architecture == "arm64" ? "linux-nvidia-64k" : "linux-nvidia")
  # The INSTALLED system's console only. boot_command below keeps the installer
  # on the first UART, because the build VM maps `-serial file:` there and
  # moving it would blank the log used to diagnose a stuck bake. Overridable
  # because SOL is not on the same UART on every board.
  default_serial_console = local.architecture == "arm64" ? "ttyAMA0" : "ttyS0"
  serial_console         = var.serial_console != "" ? var.serial_console : local.default_serial_console
  qemu_binary            = local.architecture == "arm64" ? "qemu-system-aarch64" : "qemu-system-x86_64"
  machine_type           = local.architecture == "arm64" ? "virt" : "q35"

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
