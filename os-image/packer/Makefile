# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PACKER      ?= packer
SSH_DIR     := $(CURDIR)/.ssh
ARTIFACTS   := $(CURDIR)/artifacts
# Deliberately separate from ARTIFACTS: the FDE/non-FDE "file" provisioner
# uploads ARTIFACTS/ to the guest wholesale (it's meant to hold only
# fetch-artifacts.sh's nke-agent/spire-agent downloads). The installer ISO
# (GBs, cached across builds) must never land in that directory, or it gets
# uploaded too -- this is exactly what caused an FDE build to try uploading
# 5.8GB instead of ~120MB and fail with a misleading "Disk quota exceeded".
ISO_CACHE   := $(CURDIR)/iso-cache
# FDE/non-FDE only: where the guest's serial console (boot + autoinstall
# output) gets captured, for CI to upload as an artifact on failure -- no
# VNC/monitor access there to inspect a stuck build interactively.
BUILD_LOGS  := $(CURDIR)/build-logs
PKR_FILE_FDE := $(CURDIR)/nke-node-fde.pkr.hcl
PKR_FILE_NON_FDE := $(CURDIR)/nke-node-non-fde.pkr.hcl

LUKS_PASSPHRASE ?= changeit
# Label applied to the root filesystem. Cloud-image importers can use this
# stable identifier instead of depending on a partition or device path.
ROOT_FS_LABEL ?= cloudimg-rootfs

# amd64 or arm64. Drives ISO_URL's default below and, through it,
# local.arch in nke-node-fde.pkr.hcl / nke-node-non-fde.pkr.hcl (parsed back
# out of the ISO filename) -- which is what actually picks qemu_binary,
# machine_type, the AAVMF/OVMF firmware pair, and the NVIDIA kernel flavor.
# There is deliberately no separate `-var arch=...`: iso_url is the single
# source of truth, so it can't drift out of sync with what's actually built.
ARCH ?= amd64

# FDE/non-FDE only. Overridable together; prepare-installer-iso.sh
# downloads ISO_URL, verifies it against ISO_CHECKSUM, and produces a
# GRUB-timeout-patched sibling before packer build ever runs, so packer's
# own iso_url/iso_checksum vars just point at the local copy (see the
# locals block in nke-node-fde.pkr.hcl).
#
# releases.ubuntu.com only mirrors the amd64 Server ISO -- confirmed
# directly: its SHA256SUMS has no arm64 entry at all, which is why
# prepare-installer-iso.sh fails with an empty EXPECTED_SHA256 for an
# ARCH=arm64 build pointed at it. arm64 (and every other non-amd64 arch)
# is published under cdimage.ubuntu.com/releases/<version>/release/
# instead, with its own separate SHA256SUMS there.
ifeq ($(ARCH),arm64)
  ISO_URL      ?= https://cdimage.ubuntu.com/releases/26.04/release/ubuntu-26.04-live-server-arm64.iso
  ISO_CHECKSUM ?= file:https://cdimage.ubuntu.com/releases/26.04/release/SHA256SUMS
else
  ISO_URL      ?= https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-$(ARCH).iso
  ISO_CHECKSUM ?= file:https://releases.ubuntu.com/26.04/SHA256SUMS
endif

# uname -m spelling of ARCH, to compare against the host below. KVM only
# virtualizes the host's own architecture -- an x86_64 host cannot
# KVM-accelerate an arm64 guest (or the reverse), regardless of whether
# /dev/kvm exists and is writable. Building arm64 on an x86_64 host (or
# vice versa) always falls back to tcg software emulation, which the CI
# workflow's own comments clock at roughly 10x slower than KVM for a
# same-arch build; cross-arch tcg is not expected to be faster.
ifeq ($(ARCH),amd64)
  ARCH_UNAME := x86_64
else ifeq ($(ARCH),arm64)
  ARCH_UNAME := aarch64
else
  $(error unsupported ARCH=$(ARCH); expected amd64 or arm64)
endif
HOST_ARCH := $(shell uname -m)

# Hardware acceleration where the host has it for this arch, software
# emulation otherwise. Probed rather than declared: the CI runners are
# containers with no /dev/kvm, so a fixed accelerator = kvm cannot build
# there at all, and a fixed tcg would emulate on developer machines that
# have the device.
#
# -w as well as -c: a present but unwritable /dev/kvm fails inside QEMU, later
# and less legibly than falling back here.
ACCELERATOR ?= $(shell if [ "$(HOST_ARCH)" = "$(ARCH_UNAME)" ] && [ -c /dev/kvm ] && [ -w /dev/kvm ]; then echo kvm; else echo tcg; fi)

# "host" needs KVM to be meaningful; "max" is the safe choice under TCG.
CPU_MODEL ?= $(if $(filter kvm,$(ACCELERATOR)),host,max)

# Emulated boot is minutes, not seconds. Overridable, so a slow runner can
# raise it without editing this file. 60m wasn't enough under tcg: a real CI
# run got through partitioning, curthooks, and most package installs and was
# still legitimately working (mid unattended-upgrades) when it hit the 60m
# wall — SSH only becomes reachable after the *entire* autoinstall completes
# and reboots, so the whole install has to fit inside this one window. 15m
# wasn't enough under kvm either: a real CI run on a GPU-labeled runner with
# genuine /dev/kvm was still mid-autoinstall/second-boot (confirmed clean via
# the serial console log, no crash) when it hit that wall at 15m48s — FDE's
# LUKS/LVM/package-install work is heavier than the boot times this value was
# originally set from.
SSH_TIMEOUT ?= $(if $(filter kvm,$(ACCELERATOR)),30m,120m)

# FDE/non-FDE only: how long boot_command waits before typing into GRUB.
# prepare-installer-iso.sh patches the ISO's own GRUB timeout to 300s, so
# this just needs to comfortably exceed BIOS+GRUB boot time under tcg — it
# no longer has to land inside a narrow window. See boot_wait's comment in
# nke-node-fde.pkr.hcl.
BOOT_WAIT ?= $(if $(filter kvm,$(ACCELERATOR)),10s,30s)

.PHONY: os-image-fde os-image-non-fde init-fde init-non-fde ssh-key prepare-installer-iso clean check-deps

ssh-key: $(SSH_DIR)/id_ed25519

$(SSH_DIR)/id_ed25519:
	mkdir -p $(SSH_DIR)
	ssh-keygen -t ed25519 -N '' -f $(SSH_DIR)/id_ed25519

init-fde:
	$(PACKER) init $(PKR_FILE_FDE)

init-non-fde:
	$(PACKER) init $(PKR_FILE_NON_FDE)

# FDE/non-FDE only. See prepare-installer-iso.sh's header for why this
# exists: widening the installer ISO's own GRUB timeout to 300s means
# boot_command's blind VNC keystrokes just need to land sometime in a wide
# window, not a narrow, unpredictable one, under slow/tcg boots.
prepare-installer-iso:
	mkdir -p $(ISO_CACHE)
	ISO_URL=$(ISO_URL) ISO_CHECKSUM=$(ISO_CHECKSUM) OUT_DIR=$(ISO_CACHE) ./scripts/prepare-installer-iso.sh

check-deps:
	@command -v go >/dev/null 2>&1 || { \
		echo "go is required to build nke-agent from source (agent/)" >&2; \
		echo "https://go.dev/dl/" >&2; \
		exit 1; \
	}

# Full-disk-encryption variant: boots the Ubuntu Server installer to lay down
# an LVM-on-LUKS layout via autoinstall, then provisions nke-agent/spire-agent,
# plus TPM2 binding scaffolding deferred to first boot. See
# ../docs/full-disk-encryption.md.
os-image-fde: check-deps ssh-key init-fde prepare-installer-iso
	mkdir -p $(ARTIFACTS) $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	PKR_VAR_luks_passphrase=$(LUKS_PASSPHRASE) \
	$(PACKER) build \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		-var root_fs_label=$(ROOT_FS_LABEL) \
		$(PKR_FILE_FDE)

# Non-FDE variant: LVM-backed ext4 without LUKS. See
# nke-node-non-fde.pkr.hcl.
os-image-non-fde: check-deps ssh-key init-non-fde prepare-installer-iso
	mkdir -p $(ARTIFACTS) $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	$(PACKER) build \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		-var root_fs_label=$(ROOT_FS_LABEL) \
		$(PKR_FILE_NON_FDE)

clean:
	rm -rf $(CURDIR)/output-fde $(CURDIR)/output-non-fde $(ARTIFACTS) $(ISO_CACHE) $(BUILD_LOGS)
