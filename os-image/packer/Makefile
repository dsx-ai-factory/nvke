# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PACKER      ?= packer
PYTHON      ?= python3
ifneq ($(origin MAKEFILE_LIST),file)
  $(error MAKEFILE_LIST is internal and cannot be overridden)
endif
override PACKER_MAKEFILE := $(abspath $(lastword $(MAKEFILE_LIST)))
override PACKER_ROOT := $(abspath $(dir $(PACKER_MAKEFILE)))
SSH_DIR     := $(CURDIR)/.ssh
# Packer's HCL consumes this mutable scratch path. Keep it Make-owned because
# prepare-artifacts replaces it and clean removes it. Externally staged inputs
# need a separate read-only path rather than an override of this directory.
override ARTIFACTS := $(PACKER_ROOT)/artifacts
# Deliberately separate from ARTIFACTS: the FDE/non-FDE "file" provisioner
# uploads ARTIFACTS/ to the guest wholesale (it's meant to hold only
# fetch-artifacts.sh's nke-agent/spire-agent downloads). The installer ISO
# (GBs, cached across builds) must never land in that directory, or it gets
# uploaded too -- this is exactly what caused an FDE build to try uploading
# 5.8GB instead of ~120MB and fail with a misleading "Disk quota exceeded".
ISO_CACHE   := $(CURDIR)/iso-cache
# Keep Packer's build/download cache in the existing ignored build cache
# instead of its default ./.packer_cache, which would make the checkout dirty
# and fail the source-cleanliness gates. Callers may still supply their own
# cache directory.
PACKER_CACHE_DIR ?= $(ISO_CACHE)/packer-cache
ifeq ($(strip $(PACKER_CACHE_DIR)),)
  override PACKER_CACHE_DIR := $(ISO_CACHE)/packer-cache
endif
export PACKER_CACHE_DIR
# FDE/non-FDE only: where the guest's serial console (boot + autoinstall
# output) gets captured, for CI to upload as an artifact on failure -- no
# VNC/monitor access there to inspect a stuck build interactively.
BUILD_LOGS  := $(CURDIR)/build-logs
PKR_FILE_FDE := $(CURDIR)/nke-node-fde.pkr.hcl
PKR_FILE_NON_FDE := $(CURDIR)/nke-node-non-fde.pkr.hcl
SOURCE_ROOT ?= $(abspath ../..)
QCOW2_DIST_DIR ?=
EXPECTED_SOURCE_COMMIT ?=
export SOURCE_ROOT EXPECTED_SOURCE_COMMIT PYTHON

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
#
# Use 26.04.1 to avoid the GA installer's intermittent overlayfs crash
# during curtin extraction. Reproduction details: PR #737.
ifeq ($(ARCH),arm64)
  ISO_URL      ?= https://cdimage.ubuntu.com/releases/26.04/release/ubuntu-26.04.1-live-server-arm64.iso
  ISO_CHECKSUM ?= file:https://cdimage.ubuntu.com/releases/26.04/release/SHA256SUMS
else
  ISO_URL      ?= https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-$(ARCH).iso
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

# "host" needs KVM to be meaningful; "max" is the safe choice under TCG. On an
# arm64 host "max" also turns on pointer authentication, which tcg emulates so
# slowly that the live installer's userspace stalls (2 h of wall time bought
# 20 min of guest uptime); pauth-impdef=on keeps the feature and makes it cheap.
comma := ,
CPU_MODEL ?= $(if $(filter kvm,$(ACCELERATOR)),host,$(if $(filter arm64,$(ARCH)),max$(comma)pauth-impdef=on,max))

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
# originally set from. 120m wasn't enough for same-arch tcg on the arm64 pool
# either: with pauth-impdef=on the install runs at roughly 10x the KVM pace
# (14 min under KVM, so ~140 min), and run 34771011057 was six packages into
# postinstall when it hit that wall.
SSH_TIMEOUT ?= $(if $(filter kvm,$(ACCELERATOR)),30m,180m)

# Guest vCPUs. More is not faster under tcg: every broadcast TLB invalidate
# has to synchronise all vCPU threads, and run 34791670173 with 8 was still
# copying the squashfs at 2 h 15 m where run 34771011057 with 4 had reached
# the postinstall packages at 2 h.
CPUS ?= 4

# FDE/non-FDE only: how long boot_command waits before typing into GRUB.
# prepare-installer-iso.sh patches the ISO's own GRUB timeout to 300s, so
# this just needs to comfortably exceed BIOS+GRUB boot time under tcg — it
# no longer has to land inside a narrow window. See boot_wait's comment in
# nke-node-fde.pkr.hcl.
BOOT_WAIT ?= $(if $(filter kvm,$(ACCELERATOR)),10s,30s)

.PHONY: os-image-fde os-image-non-fde preflight init-fde init-non-fde ssh-key prepare-installer-iso prepare-artifacts clean check-deps check-source collect-qcow2-fde collect-qcow2-non-fde

ssh-key: preflight $(SSH_DIR)/id_ed25519

$(SSH_DIR)/id_ed25519: | preflight
	mkdir -p $(SSH_DIR)
	ssh-keygen -t ed25519 -N '' -f $(SSH_DIR)/id_ed25519

check-source:
	@set -eu; \
	actual_commit="$$(git -C "$$SOURCE_ROOT" rev-parse --verify HEAD)"; \
	git -C "$$SOURCE_ROOT" rev-parse --verify 'HEAD^{tree}' >/dev/null; \
	if [ -n "$$EXPECTED_SOURCE_COMMIT" ] && [ "$$actual_commit" != "$$EXPECTED_SOURCE_COMMIT" ]; then \
		echo "source commit $$actual_commit does not match expected $$EXPECTED_SOURCE_COMMIT" >&2; \
		exit 1; \
	fi; \
	status="$$(git -C "$$SOURCE_ROOT" status --porcelain=v1 --untracked-files=normal)"; \
	if [ -n "$$status" ]; then \
		echo "source checkout is dirty; build and collection require an exact clean Git tree" >&2; \
		printf '%s\n' "$$status" >&2; \
		exit 1; \
	fi

preflight: check-source check-deps

init-fde: preflight
	$(PACKER) init $(PKR_FILE_FDE)

init-non-fde: preflight
	$(PACKER) init $(PKR_FILE_NON_FDE)

# FDE/non-FDE only. See prepare-installer-iso.sh's header for why this
# exists: widening the installer ISO's own GRUB timeout to 300s means
# boot_command's blind VNC keystrokes just need to land sometime in a wide
# window, not a narrow, unpredictable one, under slow/tcg boots.
prepare-installer-iso: preflight
	mkdir -p $(ISO_CACHE)
	ISO_URL=$(ISO_URL) ISO_CHECKSUM=$(ISO_CHECKSUM) OUT_DIR=$(ISO_CACHE) ./scripts/prepare-installer-iso.sh

# Stage the source-built packages before Packer starts the VM. Keeping this in
# Make preserves the current workflow and leaves Packer responsible only for
# consuming already-prepared image inputs.
prepare-artifacts: preflight
	@set -eu; \
	artifacts="$(ARTIFACTS)"; \
	if [ -L "$$artifacts" ] || { [ -e "$$artifacts" ] && [ ! -d "$$artifacts" ]; }; then \
		echo "refusing unsafe artifact staging path: $$artifacts" >&2; \
		exit 1; \
	fi; \
	stage="$$(mktemp -d "$(PACKER_ROOT)/.artifacts.tmp.XXXXXX")"; \
	backup=""; \
	cleanup() { \
		status="$$?"; \
		trap - 0 1 2 15; \
		if [ -n "$$backup" ] && [ -d "$$backup" ]; then \
			if [ ! -e "$$artifacts" ] && [ ! -L "$$artifacts" ]; then \
				if mv -- "$$backup" "$$artifacts"; then \
					backup=""; \
				else \
					echo "failed to restore previous artifacts directory: $$backup" >&2; \
					status=1; \
				fi; \
			elif [ ! -e "$$stage" ] && [ -d "$$artifacts" ] && [ ! -L "$$artifacts" ]; then \
				rm -rf -- "$$backup"; \
				backup=""; \
			fi; \
		fi; \
		if [ -n "$$stage" ]; then \
			rm -rf -- "$$stage"; \
		fi; \
		exit "$$status"; \
	}; \
	trap cleanup 0; \
	trap 'exit 1' 1 2 15; \
	ARTIFACTS_DIR="$$stage" \
	ARCH="$(ARCH)" \
	"$(PACKER_ROOT)/scripts/fetch-artifacts.sh"; \
	if [ -L "$$artifacts" ] || { [ -e "$$artifacts" ] && [ ! -d "$$artifacts" ]; }; then \
		echo "refusing unsafe artifact staging path: $$artifacts" >&2; \
		exit 1; \
	fi; \
	if [ -d "$$artifacts" ]; then \
		backup="$$(mktemp -d "$(PACKER_ROOT)/.artifacts.backup.XXXXXX")"; \
		rmdir -- "$$backup"; \
		mv -- "$$artifacts" "$$backup"; \
	fi; \
	mv -- "$$stage" "$$artifacts"; \
	stage=""; \
	if [ -n "$$backup" ]; then \
		rm -rf -- "$$backup"; \
		backup=""; \
	fi; \
	trap - 0 1 2 15

check-deps:
	@command -v go >/dev/null 2>&1 || { \
		echo "go is required to build nke-agent and the pinned nfpm" >&2; \
		exit 1; \
	}
	@command -v $(PYTHON) >/dev/null 2>&1 || { \
		echo "Python 3.10 or newer is required to validate the compressed QCOW2" >&2; \
		exit 1; \
	}
	@$(PYTHON) -c 'import sys; raise SystemExit(sys.version_info < (3, 10))' || { \
		echo "Python 3.10 or newer is required to validate the compressed QCOW2" >&2; \
		exit 1; \
	}
	@command -v qemu-img >/dev/null 2>&1 || { \
		echo "qemu-img is required to build and compress the QCOW2" >&2; \
		exit 1; \
	}

.PHONY: test
test: ## Check artifact staging, QCOW2 collection, and workflow wiring.
	$(PYTHON) -B -m unittest discover -s scripts -p 'test_*.py'

# Copy the one completed image without changing its bytes. Collection is a
# separate target so local builds and CI use the same source-owned boundary.
# Callers must supply QCOW2_DIST_DIR and capture the single basename printed on
# stdout (use make --silent when stdout is machine-consumed).
define collect_qcow2
	@set -eu; \
	if [ -z "$$QCOW2_DIST_DIR" ]; then \
		echo "QCOW2_DIST_DIR is required for $(1) QCOW2 collection" >&2; \
		exit 1; \
	fi; \
	source_dir="$(CURDIR)/output-$(1)"; \
	set -- "$$source_dir"/*.qcow2; \
	if [ "$$#" -eq 1 ] && [ ! -e "$$1" ] && [ ! -L "$$1" ]; then \
		set --; \
	fi; \
	if [ "$$#" -ne 1 ]; then \
		echo "expected exactly one QCOW2 in $$source_dir; found $$#" >&2; \
		exit 1; \
	fi; \
	source="$$1"; \
	if [ ! -f "$$source" ] || [ -L "$$source" ]; then \
		echo "QCOW2 source is not a regular file: $$source" >&2; \
		exit 1; \
	fi; \
	iso_name="$(notdir $(ISO_URL))"; \
	case "$$iso_name" in \
		ubuntu-*-live-server-"$(ARCH)".iso) ;; \
		*) echo "ISO_URL does not identify an $(ARCH) Ubuntu installer: $(ISO_URL)" >&2; exit 1 ;; \
	esac; \
	version="$${iso_name#ubuntu-}"; \
	version="$${version%-live-server-$(ARCH).iso}"; \
	expected="nke-node-$(1)-ubuntu-$$version-$(ARCH).qcow2"; \
	name="$${source##*/}"; \
	if [ "$$name" != "$$expected" ]; then \
		echo "expected $(1) $(ARCH) QCOW2 $$expected; found $$name" >&2; \
		exit 1; \
	fi; \
	mkdir -p "$$QCOW2_DIST_DIR"; \
	if [ ! -d "$$QCOW2_DIST_DIR" ] || [ -L "$$QCOW2_DIST_DIR" ]; then \
		echo "QCOW2_DIST_DIR is not a regular directory: $$QCOW2_DIST_DIR" >&2; \
		exit 1; \
	fi; \
	destination="$$QCOW2_DIST_DIR/$$name"; \
	if [ -L "$$destination" ]; then \
		echo "refusing to replace symlink destination: $$destination" >&2; \
		exit 1; \
	fi; \
	cp "$$source" "$$destination"; \
	if [ ! -f "$$destination" ] || [ -L "$$destination" ]; then \
		echo "collected QCOW2 is not a regular file: $$destination" >&2; \
		exit 1; \
	fi; \
	if ! cmp -s "$$source" "$$destination"; then \
		rm -f "$$destination"; \
		echo "collected QCOW2 differs from source: $$destination" >&2; \
		exit 1; \
	fi; \
	printf '%s\n' "$$name"
endef

collect-qcow2-fde collect-qcow2-non-fde: export QCOW2_DIST_DIR := $(QCOW2_DIST_DIR)

collect-qcow2-fde:
	@$(MAKE) --no-print-directory --silent --file "$(PACKER_MAKEFILE)" check-source
	$(call collect_qcow2,fde)

collect-qcow2-non-fde:
	@$(MAKE) --no-print-directory --silent --file "$(PACKER_MAKEFILE)" check-source
	$(call collect_qcow2,non-fde)

# Full-disk-encryption variant: boots the Ubuntu Server installer to lay down
# an LVM-on-LUKS layout via autoinstall, then provisions nke-agent/spire-agent,
# plus TPM2 binding scaffolding deferred to first boot. See
# ../docs/full-disk-encryption.md.
os-image-fde: ssh-key init-fde prepare-installer-iso prepare-artifacts
	mkdir -p $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	PKR_VAR_luks_passphrase=$(LUKS_PASSPHRASE) \
	$(PACKER) build \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var cpus=$(CPUS) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		-var root_fs_label=$(ROOT_FS_LABEL) \
		$(PKR_FILE_FDE)

# Non-FDE variant: LVM-backed ext4 without LUKS. See
# nke-node-non-fde.pkr.hcl.
os-image-non-fde: ssh-key init-non-fde prepare-installer-iso prepare-artifacts
	mkdir -p $(BUILD_LOGS)
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	$(PACKER) build \
		-var accelerator=$(ACCELERATOR) \
		-var cpu_model=$(CPU_MODEL) \
		-var cpus=$(CPUS) \
		-var ssh_timeout=$(SSH_TIMEOUT) \
		-var boot_wait=$(BOOT_WAIT) \
		-var iso_url=$(ISO_URL) \
		-var iso_checksum=$(ISO_CHECKSUM) \
		-var root_fs_label=$(ROOT_FS_LABEL) \
		$(PKR_FILE_NON_FDE)

clean:
	rm -rf $(CURDIR)/output-fde $(CURDIR)/output-non-fde $(ARTIFACTS) $(ISO_CACHE) $(BUILD_LOGS)
