# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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
  default     = "https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-amd64.img"
  description = "Ubuntu 26.04 cloud image (qcow2)."
}

variable "iso_checksum" {
  type        = string
  default     = "file:https://cloud-images.ubuntu.com/releases/26.04/release/SHA256SUMS"
  description = "Checksum file or hash for iso_url."
}

variable "disk_size" {
  type    = string
  default = "10G"
}

# "kvm" when the host exposes /dev/kvm, "tcg" for software emulation. The
# Makefile picks one by probing the device; a container-based CI runner has no
# /dev/kvm and a hard kvm requirement fails there before Packer starts.
# forge-vms already treats KVM and TCG as the two supported Linux modes
# (controller/forge-vms/docs/e2e-testing.md).
variable "accelerator" {
  type        = string
  default     = "kvm"
  description = "QEMU accelerator: kvm (hardware) or tcg (software emulation)."
}

# Boot to SSH takes minutes under TCG where it takes seconds under KVM, so the
# caller raises this alongside the accelerator rather than carrying one timeout
# that is either too tight for emulation or too slack to be a useful signal.
variable "ssh_timeout" {
  type        = string
  default     = "15m"
  description = "How long Packer waits for SSH after boot."
}

locals {
  artifacts = "${path.root}/artifacts"
  ssh_dir   = "${path.root}/.ssh"
}

source "qemu" "nke" {
  iso_url              = var.iso_url
  iso_checksum         = var.iso_checksum
  disk_image           = true
  format               = "qcow2"
  disk_size            = var.disk_size
  accelerator          = var.accelerator
  ssh_username         = "ubuntu"
  ssh_private_key_file = "${local.ssh_dir}/id_ed25519"
  ssh_timeout          = var.ssh_timeout
  cd_label             = "cidata"
  cd_content = {
    "user-data" = templatefile("${path.root}/cidata/user-data.pkrtpl", {
      ssh_public_key = file("${local.ssh_dir}/id_ed25519.pub")
    })
    "meta-data" = "instance-id: nke-packer\nlocal-hostname: nke-packer\n"
  }
  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node"
  output_directory = "${path.root}/output"
  headless         = true
  memory           = 2048
  cpus             = 2
  net_device       = "virtio-net"
  disk_interface   = "virtio"
}

build {
  name    = "nke-node"
  sources = ["source.qemu.nke"]

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
    inline = ["mkdir -p /tmp/nke-image"]
  }

  provisioner "file" {
    source      = "${local.artifacts}/"
    destination = "/tmp/nke-image/"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/provision.sh"
  }
}
