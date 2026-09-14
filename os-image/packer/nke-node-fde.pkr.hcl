# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# FDE (full-disk-encryption) build: boots the Ubuntu Server *installer* ISO
# with an autoinstall LVM-on-LUKS storage layout, provisions nke-agent and
# spire-agent, and seals the LUKS key to the node's TPM 2.0 on first boot.
# See os-image/docs/full-disk-encryption.md.
#
# See nke-node-non-fde.pkr.hcl for the same installer+autoinstall+LVM flow with
# LUKS turned off — useful for telling FDE-specific issues apart from
# installer-flow issues. Both variants share cidata/autoinstall-lvm.pkrtpl,
# gated by that template's luks_enabled flag.

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
  default     = "https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso"
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
  description = "Build-time disk size. Headroom for LVM+LUKS overhead over the base image's 10G; the real deploy disk is grown to full size on first boot. (autoinstall-lvm.pkrtpl's storage layout must actually claim the whole disk for this to matter — sizing-policy: all, not sizing_policy, which Subiquity silently ignores in favor of its own much smaller 'scaled' default.)"
}

variable "luks_passphrase" {
  type        = string
  default     = "changeit"
  sensitive   = true
  description = "Build-time-only LUKS passphrase (must match the autoinstall storage password). Replaced by a TPM binding on first boot; see docs/full-disk-encryption.md."
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

# "kvm" when the host exposes /dev/kvm, "tcg" for software emulation.
# Probed rather than declared: not every build host (or CI runner) exposes
# /dev/kvm, and a fixed value would either fail to build at all where it's
# missing or emulate needlessly where it's present.
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

variable "cpus" {
  type        = number
  default     = 4
  description = "Guest vCPUs. packer/Makefile raises it under TCG emulation."
}

variable "ssh_timeout" {
  type        = string
  default     = "60m"
  description = "How long Packer waits for SSH after the autoinstall reboot (installer + LVM/LUKS setup is much slower than booting a prebuilt cloud image)."
}

# Under tcg, BIOS POST + GRUB load from the installer ISO is much slower
# than under kvm, and unpredictably so. boot_command blind-types into
# GRUB's command line a fixed delay after boot_wait; if that lands before
# GRUB's own menu timeout expires and auto-boots the non-autoinstall
# default entry, the keystrokes go nowhere. packer/scripts/prepare-installer-iso.sh
# patches the ISO's GRUB timeout from the stock 30s to 300s before packer
# build runs (see the Makefile), so boot_wait only needs to be "comfortably
# longer than BIOS POST takes", not "inside a narrow, unknowable window" —
# it doesn't have to be exact, just not longer than 300s.
#
# (Direct QEMU -kernel/-initrd boot would remove the race entirely instead
# of just widening it, but it re-executes the same kernel/initrd on every
# guest-triggered reboot for the life of the QEMU process — confirmed by
# testing it: autoinstall completed and rebooted correctly, but landed back
# in the live installer instead of the newly-installed system, looping
# forever instead of ever reaching it. Packer runs one QEMU process for the
# whole build, with no way to switch to disk boot after the first boot, so
# that approach doesn't work here.)
variable "boot_wait" {
  type        = string
  default     = "5s"
  description = "Delay before boot_command starts typing into GRUB. Comfortably under 300s (the patched GRUB timeout) but past however long BIOS+GRUB take to reach the menu; raise under tcg."
}

locals {
  artifacts = "${path.root}/artifacts"
  ssh_dir   = "${path.root}/.ssh"
  # Deliberately NOT under local.artifacts: the "file" provisioner below
  # uploads local.artifacts/ to the guest wholesale, and this is a
  # multi-gigabyte ISO, not one of fetch-artifacts.sh's nke-agent/spire-agent
  # downloads. packer/scripts/prepare-installer-iso.sh downloads iso_url into
  # this directory and produces a GRUB-timeout-patched sibling next to it
  # (see the Makefile) before packer build runs; iso_url below points at
  # that patched copy.
  iso_cache = "${path.root}/iso-cache"
  iso_path  = "${local.iso_cache}/${basename(var.iso_url)}.patched.iso"
  # Derived from iso_url rather than a separate variable, so the output
  # filename can't drift out of sync with the ISO actually being built.
  iso_name_parts = regex("ubuntu-([0-9.]+)-live-server-([a-z0-9_]+)\\.iso$", var.iso_url)
  ubuntu_version = local.iso_name_parts[0]
  arch           = local.iso_name_parts[1]
  # Ubuntu's NVIDIA-tuned kernel flavor: linux-nvidia-64k on arm64 (64k page
  # size, matching node-images), linux-nvidia everywhere else.
  nvidia_kernel_package = local.arch == "arm64" ? "linux-nvidia-64k" : "linux-nvidia"

  # See the same locals in nke-node-non-fde.pkr.hcl: QEMU on this host only
  # ever runs the guest arch matching its own binary, "virt" is ARM's
  # equivalent of q35/i440fx, and AAVMF is arm64's UEFI firmware, not an
  # interchangeable drop-in for OVMF.
  qemu_binary       = local.arch == "arm64" ? "qemu-system-aarch64" : "qemu-system-x86_64"
  machine_type      = local.arch == "arm64" ? "virt" : "q35"
  efi_firmware_code = local.arch == "arm64" ? "/usr/share/AAVMF/AAVMF_CODE.fd" : "/usr/share/OVMF/OVMF_CODE_4M.fd"
  efi_firmware_vars = local.arch == "arm64" ? "/usr/share/AAVMF/AAVMF_VARS.fd" : "/usr/share/OVMF/OVMF_VARS_4M.fd"
  # Also not under local.artifacts, for the same reason. CI has no way to
  # inspect a stuck build interactively (no VNC/monitor access), so this
  # captures the guest's own boot/install console output to a file the
  # workflow can upload as an artifact on failure -- see the console=
  # kernel param in local.boot_command below, which is what routes it here.
  build_logs = "${path.root}/build-logs"

  # See the same local in nke-node-non-fde.pkr.hcl: arm64's virt machine has
  # no BIOS-era fixed (cd0) device alias (search --file finds the CD-ROM by
  # content instead) and no ttyS0 (its serial console is ttyAMA0, the PL011).
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

source "qemu" "nke-fde" {
  iso_url = local.iso_path
  # "none", not var.iso_checksum: local.iso_path is prepare-installer-iso.sh's
  # GRUB-patched copy, which legitimately doesn't match the upstream
  # SHA256SUMS entry for the original filename. That script already verifies
  # the original download against var.iso_checksum before patching it.
  iso_checksum = "none"

  # Use q35 for amd64 UEFI and virt for arm64; cpu_model selects the CPU.
  qemu_binary    = local.qemu_binary
  machine_type   = local.machine_type
  cpu_model      = var.cpu_model
  disk_size      = var.disk_size
  disk_interface = "virtio"
  format         = "qcow2"
  accelerator    = var.accelerator
  # Lets provision.sh's fstrim actually shrink the output qcow2: without
  # this, QEMU silently drops the guest's TRIM/discard commands, so freed
  # blocks still look "used" to Packer's post-build compaction step.
  disk_discard = "unmap"
  # See the same setting in nke-node-non-fde.pkr.hcl.
  disk_cache = "unsafe"

  efi_firmware_code = local.efi_firmware_code
  efi_firmware_vars = local.efi_firmware_vars
  efi_boot          = true

  http_content = {
    "/user-data" = templatefile("${path.root}/cidata/autoinstall-lvm.pkrtpl", {
      ssh_public_key        = file("${local.ssh_dir}/id_ed25519.pub")
      luks_enabled          = true
      luks_passphrase       = var.luks_passphrase
      nvidia_kernel_package = local.nvidia_kernel_package
      serial_console        = local.arch == "arm64" ? "ttyAMA0" : "ttyS0"
    })
    "/meta-data" = "instance-id: nke-packer-fde\nlocal-hostname: nke-packer-fde\n"
  }

  boot_wait    = var.boot_wait
  boot_command = local.boot_command

  # Additive, not a full override (qemuargs only replaces genuinely
  # conflicting switches). See the same block in nke-node-non-fde.pkr.hcl
  # for why arm64 needs -vga/-device overrides: the builder's default -vga
  # doesn't exist as a concept on -machine virt, and without an explicit
  # guest-side keyboard device (no default the way x86's pc/q35 has a
  # built-in PS/2 one), boot_command's VNC keystrokes land in the QEMU
  # monitor instead of GRUB -- confirmed by reproducing it twice, with and
  # without the GPU device alone. -boot strict=off drops the builder's own
  # default -boot once=d, which arm64 doesn't support.
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
      ["-serial", "file:${local.build_logs}/fde-install-console.log"],
    ]
  )

  ssh_username           = "ubuntu"
  ssh_private_key_file   = "${local.ssh_dir}/id_ed25519"
  ssh_timeout            = var.ssh_timeout
  # See the same setting in nke-node-non-fde.pkr.hcl.
  ssh_handshake_attempts = 10000

  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node-fde-ubuntu-${local.ubuntu_version}-${local.arch}.qcow2"
  output_directory = "${path.root}/output-fde"
  headless         = true
  memory           = 8192
  cpus             = var.cpus
  net_device       = "virtio-net"
}

build {
  name    = "nke-node-fde"
  sources = ["source.qemu.nke-fde"]

  # Same artifact fetch + install as the base image.
  provisioner "shell-local" {
    environment_vars = [
      "ARTIFACTS_DIR=${local.artifacts}",
      "ARCH=${local.arch}",
    ]
    script = "${path.root}/scripts/fetch-artifacts.sh"
  }

  provisioner "shell" {
    inline = ["mkdir -p /tmp/nke-image/luks"]
  }

  provisioner "file" {
    source      = "${local.artifacts}/"
    destination = "/tmp/nke-image/"
  }

  provisioner "file" {
    source      = "${path.root}/../files/luks/"
    destination = "/tmp/nke-image/luks/"
  }

  # configure-fde.sh runs before provision.sh: provision.sh's final cleanup
  # (cloud-init clean, machine-id truncate, removing the Packer SSH key from
  # /home/ubuntu/.ssh/authorized_keys) must be the last step, or Packer loses
  # its SSH connection before configure-fde.sh can run.
  provisioner "shell" {
    environment_vars = ["LUKS_PASSPHRASE=${var.luks_passphrase}"]
    script           = "${path.root}/scripts/configure-fde.sh"
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
    inline = ["${path.root}/scripts/compress-qcow2.sh ${path.root}/output-fde/nke-node-fde-ubuntu-${local.ubuntu_version}-${local.arch}.qcow2"]
  }
}
