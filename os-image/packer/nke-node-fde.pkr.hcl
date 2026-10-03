# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Full-disk-encryption image variant. Shared input, architecture, and QEMU
# configuration is declared once in variables.pkr.hcl and locals.pkr.hcl.

source "qemu" "nke-fde" {
  iso_url = local.installer_iso
  # Recheck the staged ISO against the digest in the authenticated manifest.
  iso_checksum = local.installer_checksum

  qemu_binary    = local.qemu_binary
  machine_type   = local.machine_type
  cpu_model      = var.cpu_model
  disk_size      = var.disk_size
  disk_interface = "virtio"
  format         = "qcow2"
  accelerator    = var.accelerator
  # Propagate guest TRIM so provision.sh's fstrim can release unused QCOW2
  # clusters; without unmap the guest reports success but the image stays large.
  disk_discard = "unmap"
  # This is a throwaway build VM that Packer shuts down cleanly. Avoid paying
  # the runner-storage cost of every package-install fsync.
  disk_cache = "unsafe"

  efi_firmware_code = var.efi_firmware_code
  efi_firmware_vars = var.efi_firmware_vars
  # The per-build variable store is execution state, not a published artifact.
  efi_drop_efivars = true
  efi_boot         = true

  # The source-owned compressor below is the only final QCOW2 conversion.
  skip_compaction  = true
  disk_compression = false

  http_content = {
    "/user-data" = templatefile("${local.project_root}/cidata/autoinstall-lvm.pkrtpl", {
      ssh_public_key        = local.ssh_public_key
      luks_enabled          = true
      luks_passphrase       = var.luks_passphrase
      nvidia_kernel_package = local.nvidia_kernel_package
      serial_console        = local.serial_console
    })
    "/meta-data" = "instance-id: nke-packer-fde\nlocal-hostname: nke-packer-fde\n"
  }

  boot_wait    = var.boot_wait
  boot_command = local.boot_command
  qemuargs = concat(local.architecture_qemuargs, [
    ["-serial", "file:${local.build_log_directory}/fde-install-console.log"],
  ])

  ssh_username         = "ubuntu"
  ssh_private_key_file = var.ssh_private_key_file
  ssh_timeout          = var.ssh_timeout
  # The live installer may accept and drop many probes before its final reboot;
  # do not let those expected attempts exhaust Packer's handshake counter.
  ssh_handshake_attempts = 10000

  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node.qcow2"
  output_directory = "${var.output_directory}/fde"
  headless         = true
  memory           = var.guest_memory_mib
  cpus             = var.cpus
  net_device       = "virtio-net"
}

build {
  name    = "nke-node-fde"
  sources = ["source.qemu.nke-fde"]

  provisioner "shell-local" {
    environment_vars = [
      "MODE=stage",
      "MATERIAL_PATH=${local.image_inputs}",
      "EXPECTED_DIGEST=${local.image_inputs_digest}",
      "STAGING_DIRECTORY=${local.staged_image_inputs.fde}",
    ]
    script = "${local.project_root}/scripts/prepare-image-inputs.sh"
  }

  error-cleanup-provisioner "shell-local" {
    environment_vars = [
      "MODE=clean",
      "STAGING_DIRECTORY=${local.staged_image_inputs.fde}",
    ]
    script = "${local.project_root}/scripts/prepare-image-inputs.sh"
  }

  provisioner "shell" {
    inline = ["mkdir -p /var/tmp/nke-image/luks"]
  }

  provisioner "file" {
    source      = "${local.staged_image_inputs.fde}/"
    destination = "/var/tmp/nke-image/"
    generated   = true
  }

  provisioner "shell-local" {
    environment_vars = [
      "MODE=clean",
      "STAGING_DIRECTORY=${local.staged_image_inputs.fde}",
    ]
    script = "${local.project_root}/scripts/prepare-image-inputs.sh"
  }

  provisioner "file" {
    source      = "${local.project_root}/../files/luks/"
    destination = "/var/tmp/nke-image/luks/"
  }

  provisioner "shell" {
    # Run before provision.sh removes Packer's authorized SSH key.
    environment_vars = ["LUKS_PASSPHRASE=${var.luks_passphrase}"]
    script           = "${local.project_root}/scripts/configure-fde.sh"
  }

  provisioner "shell" {
    environment_vars = ["ROOT_FS_LABEL=${var.root_fs_label}"]
    script           = "${local.project_root}/scripts/provision.sh"
  }

  post-processor "shell-local" {
    # Keep one source-owned final conversion for Make and direct callers.
    inline = ["${local.project_root}/scripts/compress-qcow2.sh ${var.output_directory}/fde/nke-node.qcow2"]
  }
}
