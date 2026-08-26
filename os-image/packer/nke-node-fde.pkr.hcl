# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# FDE (full-disk-encryption) variant of nke-node.pkr.hcl. Opt-in build: same
# nke-agent/spire-agent provisioning as the base image, plus an LVM-on-LUKS
# storage layout sealed to the node's TPM 2.0 on first boot. See
# os-image/docs/full-disk-encryption.md.
#
# Structurally different from nke-node.pkr.hcl: the base image boots the
# prebuilt Ubuntu cloud .img directly, but LUKS/LVM partitioning requires the
# Ubuntu Server *installer* ISO with an autoinstall storage layout, so this
# variant has its own Packer source rather than a variable on the base one.
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

variable "nke_agent_version" {
  type        = string
  default     = "edge"
  description = "GHCR tag for the agent .deb (ghcr.io/nvidia/nke/agent-deb) or binary fallback (ghcr.io/nvidia/nke/agent)."
}

variable "nke_agent_registry" {
  type        = string
  default     = "ghcr.io/nvidia/nke/agent"
  description = "OCI repository for the nke-agent binary artifact (fallback when no .deb is published)."
}

variable "nke_agent_deb_registry" {
  type        = string
  default     = "ghcr.io/nvidia/nke/agent-deb"
  description = "OCI repository for the nke-agent Debian package."
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
  description = "Build-time disk size. Headroom for LVM+LUKS overhead over the base image's 10G; the real deploy disk is grown to full size on first boot."
}

variable "luks_passphrase" {
  type        = string
  default     = "changeit"
  sensitive   = true
  description = "Build-time-only LUKS passphrase (must match the autoinstall storage password). Replaced by a TPM binding on first boot; see docs/full-disk-encryption.md."
}

# "kvm" when the host exposes /dev/kvm, "tcg" for software emulation — see
# nke-node.pkr.hcl for why this is probed rather than declared.
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
  description = "How long Packer waits for SSH after the autoinstall reboot (installer + LVM/LUKS setup is much slower than booting a prebuilt cloud image)."
}

# Under tcg, BIOS POST + GRUB load from the installer ISO is itself much
# slower than under kvm. boot_command blind-types into GRUB's command line
# a few seconds after boot_wait elapses; if GRUB isn't at its menu yet, those
# keystrokes land nowhere, the autoinstall kernel params never get set, and
# the VM sits at an interactive installer prompt until ssh_timeout expires
# with no further log output — indistinguishable from "just slow" unless you
# know to look for it. See docs/full-disk-encryption.md.
variable "boot_wait" {
  type        = string
  default     = "5s"
  description = "Delay before boot_command starts typing into GRUB. Must exceed however long BIOS+GRUB take to reach the menu; raise well past 5s under tcg."
}

locals {
  artifacts = "${path.root}/artifacts"
  ssh_dir   = "${path.root}/.ssh"
}

source "qemu" "nke-fde" {
  iso_url      = var.iso_url
  iso_checksum = var.iso_checksum

  # machine_type is the QEMU virtual chipset, not a CPU setting — cpu_model
  # (below) is the actual CPU. This matters here because this variant boots
  # via OVMF/UEFI (efi_boot below), and the legacy "pc" (i440fx) machine type
  # QEMU falls back to when machine_type is left unset predates PCIe/UEFI as
  # first-class targets — a known-fragile pairing with OVMF. q35 (a modern
  # PCIe-based chipset) resolved intermittent guest-kernel crashes (overlayfs
  # page faults during curtin's install-time rsync) seen while developing
  # this variant on "pc".
  machine_type   = "q35"
  cpu_model      = var.cpu_model
  disk_size      = var.disk_size
  disk_interface = "virtio"
  format         = "qcow2"
  accelerator    = var.accelerator

  efi_firmware_code = "/usr/share/OVMF/OVMF_CODE_4M.fd"
  efi_firmware_vars = "/usr/share/OVMF/OVMF_VARS_4M.fd"
  efi_boot          = true

  http_content = {
    "/user-data" = templatefile("${path.root}/cidata/autoinstall-lvm.pkrtpl", {
      ssh_public_key  = file("${local.ssh_dir}/id_ed25519.pub")
      luks_enabled    = true
      luks_passphrase = var.luks_passphrase
    })
    "/meta-data" = "instance-id: nke-packer-fde\nlocal-hostname: nke-packer-fde\n"
  }

  boot_wait = var.boot_wait
  boot_command = [
    "<wait10>",
    "c<wait>",
    "set root=(cd0)<enter><wait>",
    "linux /casper/vmlinuz autoinstall ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ---<enter><wait>",
    "initrd /casper/initrd<enter><wait>",
    "boot<enter>"
  ]

  ssh_username           = "ubuntu"
  ssh_private_key_file   = "${local.ssh_dir}/id_ed25519"
  ssh_timeout            = var.ssh_timeout
  ssh_handshake_attempts = 1000

  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node-fde"
  output_directory = "${path.root}/output-fde"
  headless         = true
  memory           = 8192
  cpus             = 4
  net_device       = "virtio-net"
}

build {
  name    = "nke-node-fde"
  sources = ["source.qemu.nke-fde"]

  # Same artifact fetch + install as the base image.
  provisioner "shell-local" {
    environment_vars = [
      "ARTIFACTS_DIR=${local.artifacts}",
      "NKE_AGENT_VERSION=${var.nke_agent_version}",
      "NKE_AGENT_REGISTRY=${var.nke_agent_registry}",
      "NKE_AGENT_DEB_REGISTRY=${var.nke_agent_deb_registry}",
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
    script = "${path.root}/scripts/configure-fde.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/provision.sh"
  }
}
