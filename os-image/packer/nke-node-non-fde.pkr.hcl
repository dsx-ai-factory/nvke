# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Non-FDE variant: the same installer+autoinstall+LVM flow as
# nke-node-fde.pkr.hcl, minus LUKS/Clevis/TPM entirely. It keeps layout and
# first-boot storage-growth parity with the encrypted image.

packer {
  required_version = ">= 1.10.0"
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "~> 1.1"
    }
  }
}

variable "iso_url" {
  type        = string
  default     = "https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-amd64.iso"
  description = "Ubuntu 26.04 Server installer ISO (autoinstall needs the installer, not the cloud image)."
}

variable "iso_checksum" {
  type        = string
  default     = "file:https://releases.ubuntu.com/26.04/SHA256SUMS"
  description = "Checksum file or hash for iso_url."
}

variable "disk_size" {
  type        = string
  default     = "12G"
  description = "Build-time disk size, matching the FDE variant for comparability (see its comment about sizing-policy). The real deploy disk is grown to full size on first boot."
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

# See the same variable in nke-node-fde.pkr.hcl.
variable "accelerator" {
  type        = string
  default     = "kvm"
  description = "QEMU accelerator: kvm (hardware) or tcg (software emulation)."
}

variable "cpu_model" {
  type        = string
  default     = "host"
  description = "QEMU CPU model. Use 'host' with KVM, 'max' with TCG emulation."
}

variable "ssh_timeout" {
  type        = string
  default     = "60m"
  description = "How long Packer waits for SSH after the autoinstall reboot (installer + LVM setup is much slower than booting a prebuilt cloud image)."
}

# See the same variable in nke-node-fde.pkr.hcl.
variable "boot_wait" {
  type        = string
  default     = "5s"
  description = "Delay before boot_command starts typing into GRUB. Comfortably under 300s (the patched GRUB timeout) but past however long BIOS+GRUB take to reach the menu; raise under tcg."
}

locals {
  artifacts = "${path.root}/artifacts"
  ssh_dir   = "${path.root}/.ssh"
  # See the same locals block in nke-node-fde.pkr.hcl: this must stay out of
  # local.artifacts, which the "file" provisioner uploads to the guest
  # wholesale — a multi-gigabyte ISO doesn't belong in that upload.
  iso_cache = "${path.root}/iso-cache"
  iso_path  = "${local.iso_cache}/${basename(var.iso_url)}.patched.iso"
  # See the same local in nke-node-fde.pkr.hcl.
  build_logs = "${path.root}/build-logs"
  # See the same locals in nke-node-fde.pkr.hcl: derived from iso_url so the
  # output filename can't drift out of sync with the ISO actually being built.
  iso_name_parts = regex("ubuntu-([0-9.]+)-live-server-([a-z0-9_]+)\\.iso$", var.iso_url)
  ubuntu_version = local.iso_name_parts[0]
  arch           = local.iso_name_parts[1]
  # Ubuntu's NVIDIA-tuned kernel flavor: linux-nvidia-64k on arm64 (64k page
  # size, matching node-images), linux-nvidia everywhere else.
  nvidia_kernel_package = local.arch == "arm64" ? "linux-nvidia-64k" : "linux-nvidia"

  # QEMU on this host only ever runs the guest arch matching its own binary
  # (qemu-system-x86_64 vs qemu-system-aarch64) -- there's no single binary
  # that does both. "virt" is QEMU's generic ARM platform (q35/i440fx are
  # x86 chipsets, meaningless on arm64). AAVMF is UEFI firmware built for
  # aarch64, the same role OVMF plays for x86 -- they are not interchangeable
  # binaries despite both being "the UEFI firmware files".
  qemu_binary       = local.arch == "arm64" ? "qemu-system-aarch64" : "qemu-system-x86_64"
  machine_type      = local.arch == "arm64" ? "virt" : "q35"
  efi_firmware_code = local.arch == "arm64" ? "/usr/share/AAVMF/AAVMF_CODE.fd" : "/usr/share/OVMF/OVMF_CODE_4M.fd"
  efi_firmware_vars = local.arch == "arm64" ? "/usr/share/AAVMF/AAVMF_VARS.fd" : "/usr/share/OVMF/OVMF_VARS_4M.fd"

  # arm64's virt machine has no BIOS-era fixed (cd0) device alias the way
  # x86 does -- search --file finds the CD-ROM by content instead of
  # assuming a device name, which node-images' own arm64 LVM variant already
  # validated works. It also has no ttyS0 (x86's legacy 16550 UART); its
  # serial console is ttyAMA0 (PL011).
  boot_command = local.arch == "arm64" ? [
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
}

source "qemu" "nke-non-fde" {
  iso_url = local.iso_path
  # See the comment on this in nke-node-fde.pkr.hcl.
  iso_checksum = "none"

  # See the comment on this block in nke-node-fde.pkr.hcl: machine_type is
  # the QEMU chipset, not a CPU setting. q35 avoids the legacy "pc" (i440fx)
  # + OVMF/UEFI pairing that produced intermittent guest-kernel crashes
  # while developing these variants. "virt" is its arm64 equivalent -- see
  # local.machine_type's comment.
  qemu_binary    = local.qemu_binary
  machine_type   = local.machine_type
  cpu_model      = var.cpu_model
  disk_size      = var.disk_size
  disk_interface = "virtio"
  format         = "qcow2"
  accelerator    = var.accelerator
  # See the same setting in nke-node-fde.pkr.hcl: required for provision.sh's
  # fstrim to actually shrink the output qcow2.
  disk_discard = "unmap"

  efi_firmware_code = local.efi_firmware_code
  efi_firmware_vars = local.efi_firmware_vars
  efi_boot          = true

  http_content = {
    "/user-data" = templatefile("${path.root}/cidata/autoinstall-lvm.pkrtpl", {
      ssh_public_key        = file("${local.ssh_dir}/id_ed25519.pub")
      luks_enabled          = false
      luks_passphrase       = ""
      nvidia_kernel_package = local.nvidia_kernel_package
      serial_console        = local.arch == "arm64" ? "ttyAMA0" : "ttyS0"
    })
    "/meta-data" = "instance-id: nke-packer-non-fde\nlocal-hostname: nke-packer-non-fde\n"
  }

  boot_wait    = var.boot_wait
  boot_command = local.boot_command

  # See the same block in nke-node-fde.pkr.hcl.
  # Additive (see the -serial comment below). On arm64:
  #   -vga none -device virtio-gpu-pci: overrides the builder's default
  #     -vga, a legacy PCI graphics card that doesn't exist as a concept on
  #     -machine virt.
  #   -device qemu-xhci -device usb-kbd: -machine virt has no default
  #     keyboard controller the way x86's pc/q35 has a built-in PS/2 one.
  #     Without an explicit guest-side keyboard device, VNC keystrokes have
  #     nowhere to land -- confirmed by reproducing it twice, with and
  #     without the GPU device alone: boot_command's typed boot line landed
  #     in the QEMU monitor instead ("unknown command: linux"), not GRUB, so
  #     autoinstall never started either time. node-images' own arm64 LVM
  #     variant (ubuntu-lvm-arm64-ansible.pkr.hcl) hit and fixed the same
  #     issue the same way.
  #   -boot strict=off: drops the qemu builder's own default -boot once=d,
  #     which arm64 doesn't support.
  qemuargs = concat(
    local.arch == "arm64" ? [
      ["-vga", "none"],
      ["-device", "virtio-gpu-pci"],
      ["-device", "qemu-xhci"],
      ["-device", "usb-kbd"],
      ["-boot", "strict=off"],
    ] : [],
    [
      # Captures the live installer's boot/autoinstall console output, which
      # the console= kernel param in local.boot_command routes here instead
      # of the VGA framebuffer. Read on a stuck/failed build; harmless noise
      # otherwise.
      ["-serial", "file:${local.build_logs}/non-fde-install-console.log"],
    ]
  )

  ssh_username           = "ubuntu"
  ssh_private_key_file   = "${local.ssh_dir}/id_ed25519"
  ssh_timeout            = var.ssh_timeout
  ssh_handshake_attempts = 1000

  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node-non-fde-ubuntu-${local.ubuntu_version}-${local.arch}.qcow2"
  output_directory = "${path.root}/output-non-fde"
  headless         = true
  memory           = 8192
  cpus             = 4
  net_device       = "virtio-net"
}

build {
  name    = "nke-node-non-fde"
  sources = ["source.qemu.nke-non-fde"]

  # Same artifact fetch + install as the base image.
  provisioner "shell-local" {
    environment_vars = [
      "ARTIFACTS_DIR=${local.artifacts}",
      "ARCH=${local.arch}",
    ]
    script = "${path.root}/scripts/fetch-artifacts.sh"
  }

  provisioner "shell" {
    inline = ["mkdir -p /tmp/nke-image/lvm"]
  }

  provisioner "file" {
    source      = "${local.artifacts}/"
    destination = "/tmp/nke-image/"
  }

  provisioner "file" {
    source      = "${path.root}/../files/lvm/"
    destination = "/tmp/nke-image/lvm/"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/configure-lvm-growth.sh"
  }

  provisioner "shell" {
    environment_vars = ["ROOT_FS_LABEL=${var.root_fs_label}"]
    script           = "${path.root}/scripts/provision.sh"
  }

  # See scripts/compress-qcow2.sh's header: the qemu builder's own
  # disk_compression option is broken on the pinned plugin version, so
  # compression happens here instead, after the builder has already written
  # the uncompressed qcow2 to output_directory.
  post-processor "shell-local" {
    inline = ["${path.root}/scripts/compress-qcow2.sh ${path.root}/output-non-fde/nke-node-non-fde-ubuntu-${local.ubuntu_version}-${local.arch}.qcow2"]
  }
}
