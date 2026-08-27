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
PKR_FILE    := $(CURDIR)/nke-node.pkr.hcl
PKR_FILE_FDE := $(CURDIR)/nke-node-fde.pkr.hcl
PKR_FILE_NON_FDE := $(CURDIR)/nke-node-non-fde.pkr.hcl

LUKS_PASSPHRASE ?= changeit

# FDE/non-FDE only. Overridable together; prepare-installer-iso.sh
# downloads ISO_URL, verifies it against ISO_CHECKSUM, and produces a
# GRUB-timeout-patched sibling before packer build ever runs, so packer's
# own iso_url/iso_checksum vars just point at the local copy (see the
# locals block in nke-node-fde.pkr.hcl).
ISO_URL      ?= https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-amd64.iso
ISO_CHECKSUM ?= file:https://releases.ubuntu.com/26.04/SHA256SUMS

NKE_AGENT_VERSION      ?= edge
NKE_AGENT_REGISTRY     ?= ghcr.io/nvidia/nke/agent
NKE_AGENT_DEB_REGISTRY ?= ghcr.io/nvidia/nke/agent-deb

# Hardware acceleration where the host has it, software emulation where it does
# not. Probed rather than declared: the CI runners are containers with no
# /dev/kvm, so a fixed accelerator = kvm cannot build there at all, and a fixed
# tcg would emulate on developer machines that have the device.
#
# -w as well as -c: a present but unwritable /dev/kvm fails inside QEMU, later
# and less legibly than falling back here.
ACCELERATOR ?= $(shell if [ -c /dev/kvm ] && [ -w /dev/kvm ]; then echo kvm; else echo tcg; fi)

# Only used by the FDE/non-FDE variants (nke-node.pkr.hcl has no cpu_model var).
# "host" needs KVM to be meaningful; "max" is the safe choice under TCG.
CPU_MODEL ?= $(if $(filter kvm,$(ACCELERATOR)),host,max)

# Emulated boot is minutes, not seconds. Overridable, so a slow runner can raise
# it without editing this file.
SSH_TIMEOUT ?= $(if $(filter kvm,$(ACCELERATOR)),15m,60m)

# FDE/non-FDE only: how long boot_command waits before typing into GRUB.
# prepare-installer-iso.sh patches the ISO's own GRUB timeout to 300s, so
# this just needs to comfortably exceed BIOS+GRUB boot time under tcg — it
# no longer has to land inside a narrow window. See boot_wait's comment in
# nke-node-fde.pkr.hcl.
BOOT_WAIT ?= $(if $(filter kvm,$(ACCELERATOR)),10s,30s)

.PHONY: os-image os-image-fde os-image-non-fde image init init-fde init-non-fde ssh-key prepare-installer-iso clean check-deps

ssh-key: $(SSH_DIR)/id_ed25519

$(SSH_DIR)/id_ed25519:
	mkdir -p $(SSH_DIR)
	ssh-keygen -t ed25519 -N '' -f $(SSH_DIR)/id_ed25519

init:
	$(PACKER) init $(PKR_FILE)

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
	@command -v oras >/dev/null 2>&1 || { \
		echo "oras is required to pull $(NKE_AGENT_DEB_REGISTRY) (or $(NKE_AGENT_REGISTRY))" >&2; \
		echo "https://oras.land/docs/installation" >&2; \
		exit 1; \
	}

os-image: check-deps ssh-key init
	mkdir -p $(ARTIFACTS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	NKE_AGENT_VERSION=$(NKE_AGENT_VERSION) \
	NKE_AGENT_REGISTRY=$(NKE_AGENT_REGISTRY) \
	NKE_AGENT_DEB_REGISTRY=$(NKE_AGENT_DEB_REGISTRY) \
	$(PACKER) build \
		-var nke_agent_version=$(NKE_AGENT_VERSION) \
		-var nke_agent_registry=$(NKE_AGENT_REGISTRY) \
		-var nke_agent_deb_registry=$(NKE_AGENT_DEB_REGISTRY) \
		-var accelerator=$(ACCELERATOR) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		$(PKR_FILE)

# Alias kept so existing `make image` muscle memory still works.
image: os-image

# Full-disk-encryption variant: boots the Ubuntu Server installer (not the
# cloud image) to lay down an LVM-on-LUKS layout via autoinstall, then
# provisions the same nke-agent/spire-agent bits as `os-image`, plus TPM2
# binding scaffolding deferred to first boot. See ../docs/full-disk-encryption.md.
os-image-fde: check-deps ssh-key init-fde prepare-installer-iso
	mkdir -p $(ARTIFACTS) $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	NKE_AGENT_VERSION=$(NKE_AGENT_VERSION) \
	NKE_AGENT_REGISTRY=$(NKE_AGENT_REGISTRY) \
	NKE_AGENT_DEB_REGISTRY=$(NKE_AGENT_DEB_REGISTRY) \
	$(PACKER) build \
		-var nke_agent_version=$(NKE_AGENT_VERSION) \
		-var nke_agent_registry=$(NKE_AGENT_REGISTRY) \
		-var nke_agent_deb_registry=$(NKE_AGENT_DEB_REGISTRY) \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		-var luks_passphrase=$(LUKS_PASSPHRASE) \
		$(PKR_FILE_FDE)

# Non-FDE variant: same installer+autoinstall+LVM flow as os-image-fde,
# minus LUKS/Clevis/TPM entirely. See nke-node-non-fde.pkr.hcl.
os-image-non-fde: check-deps ssh-key init-non-fde prepare-installer-iso
	mkdir -p $(ARTIFACTS) $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	NKE_AGENT_VERSION=$(NKE_AGENT_VERSION) \
	NKE_AGENT_REGISTRY=$(NKE_AGENT_REGISTRY) \
	NKE_AGENT_DEB_REGISTRY=$(NKE_AGENT_DEB_REGISTRY) \
	$(PACKER) build \
		-var nke_agent_version=$(NKE_AGENT_VERSION) \
		-var nke_agent_registry=$(NKE_AGENT_REGISTRY) \
		-var nke_agent_deb_registry=$(NKE_AGENT_DEB_REGISTRY) \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		$(PKR_FILE_NON_FDE)

clean:
	rm -rf $(CURDIR)/output $(CURDIR)/output-fde $(CURDIR)/output-non-fde $(ARTIFACTS) $(ISO_CACHE) $(BUILD_LOGS)
