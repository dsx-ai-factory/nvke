# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

variable "architecture" {
  type        = string
  description = "Target guest architecture selected by the external build plan."

  validation {
    condition     = contains(["amd64", "arm64"], var.architecture)
    error_message = "The architecture value must be amd64 or arm64."
  }
}

variable "material_manifest" {
  type        = string
  description = "Absolute path to the caller-authenticated material manifest. Materials are staged in its sibling materials directory."

  validation {
    condition     = startswith(var.material_manifest, "/") && can(file(var.material_manifest))
    error_message = "The material_manifest value must name an existing absolute file."
  }

  validation {
    condition = try(
      jsondecode(file(var.material_manifest)).schemaVersion == "nke.dev/os-image-materials/v1",
      false,
    )
    error_message = "The material manifest must use schemaVersion nke.dev/os-image-materials/v1."
  }

  validation {
    condition = try(
      length([
        for material in jsondecode(file(var.material_manifest)).materials : material
        if material.binding == "installer-iso" && material.path == "materials/installer-iso"
      ]) == 1 &&
      length([
        for material in jsondecode(file(var.material_manifest)).materials : material
        if material.binding == "image-inputs" && material.path == "materials/image-inputs"
      ]) == 1 &&
      length([
        for material in jsondecode(file(var.material_manifest)).materials : material
        if material.binding == "installer-iso" && material.kind == "disk-image" && material.path == "materials/installer-iso" && contains(["amd64", "arm64"], material.architecture) && can(regex("^sha256:[0-9a-f]{64}$", material.digest))
      ]) == 1 &&
      length([
        for material in jsondecode(file(var.material_manifest)).materials : material
        if material.binding == "image-inputs" && material.kind == "archive" && material.path == "materials/image-inputs" && contains(["amd64", "arm64"], material.architecture) && can(regex("^sha256:[0-9a-f]{64}$", material.digest))
      ]) == 1 &&
      [
        for material in jsondecode(file(var.material_manifest)).materials : material.architecture
        if material.binding == "installer-iso" && material.path == "materials/installer-iso"
        ][0] == [
        for material in jsondecode(file(var.material_manifest)).materials : material.architecture
        if material.binding == "image-inputs" && material.path == "materials/image-inputs"
      ][0],
      false,
    )
    error_message = "The material manifest must contain one installer-iso and one image-inputs record with matching amd64 or arm64 architecture and lowercase sha256 digests."
  }
}

variable "output_directory" {
  type        = string
  description = "Absolute caller-owned parent directory for image outputs and build logs."

  validation {
    condition     = startswith(var.output_directory, "/") && abspath(var.output_directory) == var.output_directory
    error_message = "The output_directory value must be a clean absolute path without dot segments."
  }
}

variable "build_log_directory" {
  type        = string
  default     = ""
  description = "Optional absolute directory for serial-console logs. Defaults to output_directory."

  validation {
    condition     = var.build_log_directory == "" || startswith(var.build_log_directory, "/")
    error_message = "The build_log_directory value must be empty or absolute."
  }
}

variable "ssh_private_key_file" {
  type        = string
  description = "Absolute path to the caller-owned private key used for Packer SSH."

  validation {
    condition     = startswith(var.ssh_private_key_file, "/") && fileexists(var.ssh_private_key_file)
    error_message = "The ssh_private_key_file value must name an existing absolute file."
  }
}

variable "ssh_public_key_file" {
  type        = string
  description = "Absolute path to the caller-owned public key injected by autoinstall for Packer SSH."

  validation {
    condition     = startswith(var.ssh_public_key_file, "/") && fileexists(var.ssh_public_key_file)
    error_message = "The ssh_public_key_file value must name an existing absolute file."
  }
}

variable "disk_size" {
  type        = string
  default     = "12G"
  description = "Build-time disk size shared by both variants, with headroom for the installer and FDE overhead. The deployed disk grows on first boot."
}

variable "serial_console" {
  type        = string
  default     = ""
  description = "Serial console the INSTALLED system directs kernel output to. Empty selects the architecture default (ttyS0 on amd64, ttyAMA0 on arm64)."

  # A boundary, not a typo check: this is interpolated into a shell command in
  # autoinstall-lvm.pkrtpl's late-commands, so anything accepted runs in the
  # target. A bare tty name rejects a quoted or chained payload before QEMU or
  # the installer is involved.
  validation {
    condition     = var.serial_console == "" || can(regex("^tty[A-Za-z0-9]+$", var.serial_console))
    error_message = "The serial_console value must be empty or a bare tty device name such as ttyS0, ttyS1 or ttyAMA0."
  }
}

variable "root_fs_label" {
  type        = string
  default     = "cloudimg-rootfs"
  description = "Filesystem label applied to the root volume for cloud-image discovery."

  validation {
    condition     = length(var.root_fs_label) > 0 && length(var.root_fs_label) <= 16
    error_message = "The root_fs_label value must contain 1 to 16 characters for ext4."
  }
}

variable "accelerator" {
  type        = string
  default     = "kvm"
  description = "QEMU accelerator: kvm for hardware virtualization or tcg for software emulation."

  validation {
    condition     = contains(["kvm", "tcg"], var.accelerator)
    error_message = "The accelerator value must be kvm or tcg."
  }
}

variable "cpu_model" {
  type        = string
  default     = "host"
  description = "QEMU CPU model. Use host with KVM and max with TCG emulation."
}

variable "guest_memory_mib" {
  type        = number
  default     = 8192
  description = "Guest memory in MiB. 1024 is a schema floor, not a supported installer sizing recommendation; the default is 8192."

  validation {
    condition     = var.guest_memory_mib >= 1024 && floor(var.guest_memory_mib) == var.guest_memory_mib
    error_message = "The guest_memory_mib value must be an integer of at least 1024."
  }
}

variable "cpus" {
  type        = number
  default     = 4
  description = "Guest vCPU count."

  validation {
    condition     = var.cpus >= 1
    error_message = "The cpus value must be at least 1."
  }
}

variable "ssh_timeout" {
  type        = string
  default     = "60m"
  description = "Maximum time Packer waits for the complete installer run and reboot before SSH becomes available."
}

variable "boot_wait" {
  type        = string
  default     = "5s"
  description = "Delay before Packer types the autoinstall boot command into the patched ISO's extended GRUB window."
}

variable "luks_passphrase" {
  type        = string
  default     = "changeit"
  sensitive   = true
  description = "FDE-only bootstrap passphrase used during encrypted image construction; ignored by the non-FDE build."
}

# Make supplies the discovered pair. Direct Packer callers provide both paths.
variable "efi_firmware_code" {
  type        = string
  description = "UEFI firmware code blob."

  validation {
    condition     = length(var.efi_firmware_code) > 0
    error_message = "The firmware code path must not be empty."
  }
}

variable "efi_firmware_vars" {
  type        = string
  description = "Matching UEFI firmware variables template."

  validation {
    condition     = length(var.efi_firmware_vars) > 0
    error_message = "The firmware variables path must not be empty."
  }
}
