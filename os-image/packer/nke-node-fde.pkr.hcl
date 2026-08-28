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
  description = "Build-time disk size. Headroom for LVM+LUKS overhead over the base image's 10G; the real deploy disk is grown to full size on first boot. (autoinstall-lvm.pkrtpl's storage layout must actually claim the whole disk for this to matter — sizing-policy: all, not sizing_policy, which Subiquity silently ignores in favor of its own much smaller 'scaled' default.)"
}

variable "luks_passphrase" {
  type        = string
  default     = "changeit"
  sensitive   = true
  description = "Build-time-only LUKS passphrase (must match the autoinstall storage password). Replaced by a TPM binding on first boot; see docs/full-disk-encryption.md."
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
  # Also not under local.artifacts, for the same reason. CI has no way to
  # inspect a stuck build interactively (no VNC/monitor access), so this
  # captures the guest's own boot/install console output to a file the
  # workflow can upload as an artifact on failure -- see console=ttyS0 on
  # the boot_command line below, which is what routes it here.
  build_logs = "${path.root}/build-logs"
}

source "qemu" "nke-fde" {
  iso_url = local.iso_path
  # "none", not var.iso_checksum: local.iso_path is prepare-installer-iso.sh's
  # GRUB-patched copy, which legitimately doesn't match the upstream
  # SHA256SUMS entry for the original filename. That script already verifies
  # the original download against var.iso_checksum before patching it.
  iso_checksum = "none"

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
  # Lets provision.sh's fstrim actually shrink the output qcow2: without
  # this, QEMU silently drops the guest's TRIM/discard commands, so freed
  # blocks still look "used" to Packer's post-build compaction step.
  disk_discard = "unmap"

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
    "<wait3>",
    "c<wait>",
    "set root=(cd0)<enter><wait>",
    "linux /casper/vmlinuz autoinstall console=ttyS0,115200 ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ---<enter><wait>",
    "initrd /casper/initrd<enter><wait>",
    "boot<enter>"
  ]

  # Additive, not a full override (qemuargs only replaces genuinely
  # conflicting switches) -- captures the live installer's boot/autoinstall
  # console output, which the console=ttyS0 kernel param above routes here
  # instead of the VGA framebuffer. Read on a stuck/failed build; harmless
  # noise otherwise.
  qemuargs = [
    ["-serial", "file:${local.build_logs}/fde-install-console.log"]
  ]

  ssh_username           = "ubuntu"
  ssh_private_key_file   = "${local.ssh_dir}/id_ed25519"
  ssh_timeout            = var.ssh_timeout
  ssh_handshake_attempts = 1000

  shutdown_command = "sudo shutdown -P now"
  vm_name          = "nke-node-fde-ubuntu-${local.ubuntu_version}-${local.arch}.qcow2"
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
    environment_vars = ["LUKS_PASSPHRASE=${var.luks_passphrase}"]
    script           = "${path.root}/scripts/configure-fde.sh"
  }

  provisioner "shell" {
    script = "${path.root}/scripts/provision.sh"
  }
}
