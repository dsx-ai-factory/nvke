# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Non-FDE variant: the same installer+autoinstall+LVM flow as
# nke-node-fde.pkr.hcl, minus LUKS/Clevis/TPM entirely. No encryption is
# configured or implied by this variant — it exists to isolate FDE-specific
# issues from installer-flow issues by giving an otherwise-identical build to
# compare against, and as a base for anyone who wants the installer-based
# LVM partitioning (e.g. for its own first-boot storage growth) without
# opting into encryption. Shares cidata/autoinstall-lvm.pkrtpl with the FDE
# variant, with luks_enabled = false.

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
  description = "Build-time disk size, matching the FDE variant for comparability (see its comment about sizing-policy). The real deploy disk is grown to full size on first boot."
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
}

source "qemu" "nke-non-fde" {
  iso_url = local.iso_path
  # See the comment on this in nke-node-fde.pkr.hcl.
  iso_checksum = "none"

  # See the comment on this block in nke-node-fde.pkr.hcl: machine_type is
  # the QEMU chipset, not a CPU setting. q35 avoids the legacy "pc" (i440fx)
  # + OVMF/UEFI pairing that produced intermittent guest-kernel crashes
  # while developing these variants.
  machine_type   = "q35"
  cpu_model      = var.cpu_model
  disk_size      = var.disk_size
  disk_interface = "virtio"
  format         = "qcow2"
  accelerator    = var.accelerator
  # See the same setting in nke-node-fde.pkr.hcl: required for provision.sh's
  # fstrim to actually shrink the output qcow2.
  disk_discard = "unmap"

  efi_firmware_code = "/usr/share/OVMF/OVMF_CODE_4M.fd"
  efi_firmware_vars = "/usr/share/OVMF/OVMF_VARS_4M.fd"
  efi_boot          = true

  http_content = {
    "/user-data" = templatefile("${path.root}/cidata/autoinstall-lvm.pkrtpl", {
      ssh_public_key  = file("${local.ssh_dir}/id_ed25519.pub")
      luks_enabled    = false
      luks_passphrase = ""
    })
    "/meta-data" = "instance-id: nke-packer-non-fde\nlocal-hostname: nke-packer-non-fde\n"
  }

  boot_wait = var.boot_wait
  boot_command = [
    "<wait3>",
    "c<wait>",
    "set root=(cd0)<enter><wait>",
    "linux /casper/vmlinuz autoinstall console=ttyS0,115200 ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ---<enter><wait>",
    "initrd /casper/initrd<enter><wait>",
    "boot<enter>"
  ]

  # See the same block in nke-node-fde.pkr.hcl.
  qemuargs = [
    ["-serial", "file:${local.build_logs}/non-fde-install-console.log"]
  ]

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
      "NKE_AGENT_VERSION=${var.nke_agent_version}",
      "NKE_AGENT_REGISTRY=${var.nke_agent_registry}",
      "NKE_AGENT_DEB_REGISTRY=${var.nke_agent_deb_registry}",
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

  # configure-lvm-growth.sh runs before provision.sh for the same reason as
  # the FDE variant: provision.sh's final cleanup (cloud-init clean,
  # machine-id truncate, removing the Packer SSH key) must be the last step.
  provisioner "shell" {
    script = "${path.root}/scripts/configure-lvm-growth.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/provision.sh"
  }
}
