# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PACKER      ?= packer
PYTHON      ?= python3
ifneq ($(origin MAKEFILE_LIST),file)
  $(error MAKEFILE_LIST is internal and cannot be overridden)
endif
override PACKER_MAKEFILE := $(abspath $(lastword $(MAKEFILE_LIST)))
override PACKER_ROOT := $(abspath $(dir $(PACKER_MAKEFILE)))
override PACKER_PROJECT := $(PACKER_ROOT)
# UBUNTU_ISO_SHA256_* comes from make/tool-versions.mk, which owns every
# committed digest in the repository. Included after PACKER_MAKEFILE is taken
# from $(lastword $(MAKEFILE_LIST)) above, which an earlier include would
# have pointed at tool-versions.mk instead.
include $(PACKER_ROOT)/../../make/tool-versions.mk
SSH_DIR     := $(CURDIR)/.ssh
SSH_PRIVATE_KEY_FILE ?=
SSH_PUBLIC_KEY_FILE ?=
override PACKER_SSH_PRIVATE_KEY_FILE := $(if $(strip $(SSH_PRIVATE_KEY_FILE)),$(SSH_PRIVATE_KEY_FILE),$(SSH_DIR)/id_ed25519)
override PACKER_SSH_PUBLIC_KEY_FILE := $(if $(strip $(SSH_PUBLIC_KEY_FILE)),$(SSH_PUBLIC_KEY_FILE),$(SSH_DIR)/id_ed25519.pub)
# Make stages source-built packages here before adapting them into Packer's
# explicit material layout. Keep the mutable scratch path Make-owned because
# prepare-artifacts replaces it and clean removes it.
override ARTIFACTS := $(PACKER_ROOT)/artifacts
override PACKER_INPUT_ROOT := $(PACKER_ROOT)/.packer-inputs
override MATERIAL_MANIFEST := $(PACKER_INPUT_ROOT)/material-manifest.json
# Keep the variants in separate scratch roots. Direct Packer callers already
# get variant subdirectories under their output_directory; Make additionally
# isolates its two entry points so one completed build cannot remove the other
# build's active output during cleanup.
override PACKER_OUTPUT_DIRECTORY_FDE := $(PACKER_ROOT)/.packer-output-fde
override PACKER_OUTPUT_DIRECTORY_NON_FDE := $(PACKER_ROOT)/.packer-output-non-fde
# Deliberately separate from ARTIFACTS: prepare-packer-inputs archives that
# directory for guest provisioning. The multi-gigabyte installer ISO must stay
# outside the archive and is staged as its own Packer material.
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
# VNC/monitor access there to inspect a stuck build interactively. Packer's
# debug log goes there too (PACKER_LOG): the only trace of a QEMU start failure.
BUILD_LOGS  := $(CURDIR)/build-logs
PACKER_BUILD_FDE := nvke-node-fde.qemu.nvke-fde
PACKER_BUILD_NON_FDE := nvke-node-non-fde.qemu.nvke-non-fde
SOURCE_ROOT ?= $(abspath ../..)
QCOW2_DIST_DIR ?=
EXPECTED_SOURCE_COMMIT ?=
export SOURCE_ROOT EXPECTED_SOURCE_COMMIT PYTHON

LUKS_PASSPHRASE ?= changeit
REQUIRE_ACCELERATION ?= 0
# Attempts check-acceleration makes before giving up on an unusable /dev/kvm; the bake sets 6, laptops keep 1.
KVM_SETTLE_RETRIES ?= 1
export NKE_PACKAGE_BUILDER ?= auto
# Label applied to the root filesystem. Cloud-image importers can use this
# stable identifier instead of depending on a partition or device path.
ROOT_FS_LABEL ?= cloudimg-rootfs

# Serial console the INSTALLED system uses. Empty keeps the architecture default
# (ttyS0 on amd64, ttyAMA0 on arm64), which is what Forge and tenant nodes
# expect; CSC control-plane hardware puts SOL on the second UART, so it wants
# SERIAL_CONSOLE=ttyS1. The installer's own console stays on the first UART
# either way, because the build VM captures its log there.
SERIAL_CONSOLE ?=

# amd64 or arm64. Drives the ISO default, source package preparation, and the
# explicit Packer architecture variable that selects QEMU, firmware, console,
# and NVIDIA kernel behavior.
ARCH ?= amd64

# FDE/non-FDE only. Overridable together; prepare-installer-iso.sh
# downloads ISO_URL, verifies it against ISO_CHECKSUM, and produces a
# GRUB-timeout-patched sibling before Packer starts. prepare-packer-inputs then
# stages that local copy and its checksum in the explicit material layout.
#
# releases.ubuntu.com only mirrors the amd64 Server ISO -- confirmed
# directly: its SHA256SUMS has no arm64 entry at all, which is why
# prepare-installer-iso.sh fails with an empty EXPECTED_SHA256 for an
# ARCH=arm64 build pointed at it. arm64 (and every other non-amd64 arch)
# is published under cdimage.ubuntu.com/ubuntu/releases/<version>/release/
# instead, with its own separate SHA256SUMS there.
#
# Use 26.04.1 to avoid the GA installer's intermittent overlayfs crash
# during curtin extraction. Reproduction details: PR #737.
#
# UBUNTU_RELEASE picks the point release, and with it the image flavor. 24.04 is
# published alongside 26.04 because the DSX CSC DPU stack installs Ubuntu 24.04
# host packages onto the node and no ubuntu2604 DOCA host package exists.
# Nothing else is per-release: linux-nvidia(-64k) and the split linux-firmware-*
# packages are in noble-updates too, and the FDE variant's initramfs-tools
# install is a no-op on 24.04.
#
# The archive directory is this value's major.minor. An explicit ISO_URL still
# wins and still names the output: UBUNTU_VERSION is read back out of the URL.
UBUNTU_RELEASE ?= 26.04.1
override UBUNTU_SERIES := $(word 1,$(subst ., ,$(UBUNTU_RELEASE))).$(word 2,$(subst ., ,$(UBUNTU_RELEASE)))
ifeq ($(ARCH),arm64)
  ISO_URL      ?= https://cdimage.ubuntu.com/ubuntu/releases/$(UBUNTU_SERIES)/release/ubuntu-$(UBUNTU_RELEASE)-live-server-arm64.iso
else
  ISO_URL      ?= https://releases.ubuntu.com/$(UBUNTU_SERIES)/ubuntu-$(UBUNTU_RELEASE)-live-server-$(ARCH).iso
endif

# The committed digest for this UBUNTU_RELEASE and ARCH, from
# make/tool-versions.mk -- see the comment there for why it is committed rather
# than fetched, and how to add a line. Empty when the combination has no line,
# which prepare-installer-iso turns into a named failure rather than a build
# against an unverified ISO.
override UBUNTU_ISO_SHA256 := $(UBUNTU_ISO_SHA256_$(subst .,_,$(UBUNTU_RELEASE))_$(if $(filter arm64,$(ARCH)),ARM64,AMD64))

# Either form is accepted, matching Packer's own iso_checksum spelling:
#   sha256:<hex>  a digest, which is what the committed pin above produces
#   file:<url>    a SHA256SUMS-style manifest to look ISO_URL's basename up in
# Override it together with ISO_URL when building from an ISO that is not a
# Canonical release; file: is the escape hatch for a locally served manifest.
ISO_CHECKSUM ?= sha256:$(UBUNTU_ISO_SHA256)
override UBUNTU_VERSION := $(patsubst ubuntu-%-live-server-$(ARCH).iso,%,$(notdir $(ISO_URL)))

# uname -m spelling of ARCH, to compare against the host below. KVM only
# virtualizes the host's own architecture -- an x86_64 host cannot
# KVM-accelerate an arm64 guest (or the reverse), regardless of whether
# /dev/kvm exists and is writable. Building arm64 on an x86_64 host (or
# vice versa) always falls back to tcg software emulation, which the CI
# workflow's own comments clock at roughly 10x slower than KVM for a
# same-arch build; cross-arch tcg is not expected to be faster.
# macOS spells it arm64, never aarch64, so it never matches here -- which costs
# nothing: this probe also requires /dev/kvm, which macOS does not have.
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
GUEST_MEMORY_MIB ?= 8192

# FDE/non-FDE only: how long boot_command waits before typing into GRUB.
# prepare-installer-iso.sh patches the ISO's own GRUB timeout to 300s, so
# this just needs to comfortably exceed BIOS+GRUB boot time under tcg — it
# no longer has to land inside a narrow window. See boot_wait's comment in
# nvke-node-fde.pkr.hcl.
BOOT_WAIT ?= $(if $(filter kvm,$(ACCELERATOR)),10s,30s)

# Firmware and acceleration checks share the boot harness selection, including
# QEMU's firmware directory on hosts without /usr/share/OVMF or AAVMF. The
# wrapper receives the whole `packer build ... TEMPLATE` command, inserts the
# firmware -var flags in front of the template, which Packer requires last, and
# retries once when a neighbour pod's qemu postinst closed /dev/kvm under QEMU.
PACKER_BUILD = ARCH=$(ARCH) ACCELERATOR=$(ACCELERATOR) REQUIRE_ACCELERATION=$(REQUIRE_ACCELERATION) \
	./scripts/run-packer-build.sh $(PACKER) build

.PHONY: os-image-fde os-image-non-fde _os-image-fde _os-image-non-fde preflight init-packer init-fde init-non-fde validate-packer ssh-key prepare-ssh-key prepare-image-build prepare-installer-iso prepare-artifacts prepare-packer-inputs clean clean-packer-scratch clean-fde-output clean-non-fde-output check-deps check-source collect-qcow2-fde collect-qcow2-non-fde check-acceleration

prepare-ssh-key:
	@command -v ssh-keygen >/dev/null 2>&1 || { \
		echo "ssh-keygen is required to prepare the Packer SSH key pair" >&2; \
		exit 1; \
	}
	@set -eu; \
	private_supplied=$(if $(strip $(SSH_PRIVATE_KEY_FILE)),1,0); \
	public_supplied=$(if $(strip $(SSH_PUBLIC_KEY_FILE)),1,0); \
	if [ "$$private_supplied" -eq 1 ] && [ "$$public_supplied" -eq 0 ]; then \
		echo "SSH_PUBLIC_KEY_FILE is required when SSH_PRIVATE_KEY_FILE is set" >&2; \
		exit 1; \
	fi; \
	if [ "$$private_supplied" -eq 0 ] && [ "$$public_supplied" -eq 1 ]; then \
		echo "SSH_PRIVATE_KEY_FILE is required when SSH_PUBLIC_KEY_FILE is set" >&2; \
		exit 1; \
	fi; \
	private="$(PACKER_SSH_PRIVATE_KEY_FILE)"; \
	public="$(PACKER_SSH_PUBLIC_KEY_FILE)"; \
	case "$$private" in /*) ;; *) \
		echo "SSH private key path must be absolute: $$private" >&2; \
		exit 1; \
	esac; \
	case "$$public" in /*) ;; *) \
		echo "SSH public key path must be absolute: $$public" >&2; \
		exit 1; \
	esac; \
	if [ "$$private_supplied" -eq 0 ]; then \
		private_exists=0; \
		public_exists=0; \
		if [ -e "$$private" ] || [ -L "$$private" ]; then private_exists=1; fi; \
		if [ -e "$$public" ] || [ -L "$$public" ]; then public_exists=1; fi; \
		if [ "$$private_exists" -ne "$$public_exists" ]; then \
			echo "generated SSH key state is incomplete: $(SSH_DIR)" >&2; \
			exit 1; \
		fi; \
		if [ "$$private_exists" -eq 0 ]; then \
			mkdir -p "$(SSH_DIR)"; \
			ssh-keygen -t ed25519 -N '' -f "$$private"; \
		fi; \
	fi; \
	if [ ! -f "$$private" ] || [ -L "$$private" ]; then \
		echo "SSH private key is not a regular file: $$private" >&2; \
		exit 1; \
	fi; \
	if [ ! -f "$$public" ] || [ -L "$$public" ]; then \
		echo "SSH public key is not a regular file: $$public" >&2; \
		exit 1; \
	fi; \
	if ! derived="$$(ssh-keygen -y -f "$$private" </dev/null)"; then \
		echo "SSH private key must be readable and unencrypted: $$private" >&2; \
		exit 1; \
	fi; \
	derived="$$(printf '%s\n' "$$derived" | awk 'NF >= 2 { print $$1 " " $$2; exit }')"; \
	supplied="$$(awk 'NF >= 2 { print $$1 " " $$2; exit }' "$$public")"; \
	if [ -z "$$derived" ] || [ -z "$$supplied" ] || [ "$$derived" != "$$supplied" ]; then \
		echo "SSH public key does not match private key: $$public" >&2; \
		exit 1; \
	fi

# Preserve the existing direct target contract: explicit key preparation also
# checks the source and complete toolchain before it succeeds.
ssh-key: preflight prepare-ssh-key

# Keep SSH input validation ahead of every source, dependency, artifact, and
# Packer action, including under parallel Make. Once the key contract is valid,
# the nested Make may initialize Packer and prepare materials concurrently.
prepare-image-build: prepare-ssh-key
	@$(MAKE) --no-print-directory --file "$(PACKER_MAKEFILE)" init-packer prepare-packer-inputs

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

preflight: check-source check-deps check-acceleration

init-packer: preflight
	$(PACKER) init "$(PACKER_PROJECT)"

init-fde init-non-fde: init-packer

validate-packer:
	@command -v "$(PACKER)" >/dev/null 2>&1 || { echo "Packer is required to validate the directory project" >&2; exit 1; }
	@command -v "$(PYTHON)" >/dev/null 2>&1 || { echo "Python is required to validate the material contract" >&2; exit 1; }
	@command -v ssh-keygen >/dev/null 2>&1 || { echo "ssh-keygen is required to create validation state" >&2; exit 1; }
	@command -v tar >/dev/null 2>&1 || { echo "tar is required to create validation materials" >&2; exit 1; }
	PACKER="$(PACKER)" PYTHON="$(PYTHON)" ./scripts/validate-packer-project.sh

# FDE/non-FDE only. See prepare-installer-iso.sh's header for why this
# exists: widening the installer ISO's own GRUB timeout to 300s means
# boot_command's blind VNC keystrokes just need to land sometime in a wide
# window, not a narrow, unpredictable one, under slow/tcg boots.
prepare-installer-iso: preflight
	@case "$(notdir $(ISO_URL))" in \
		ubuntu-*-live-server-$(ARCH).iso) ;; \
		*) echo "ISO_URL does not identify an $(ARCH) Ubuntu installer: $(ISO_URL)" >&2; exit 1 ;; \
	esac
	@# Packer and the package builder take the series from UBUNTU_RELEASE, so an
	@# overridden ISO_URL must be the same series.
	@case "$(UBUNTU_VERSION)" in \
		$(UBUNTU_SERIES)|$(UBUNTU_SERIES).*) ;; \
		*) echo "ISO_URL is Ubuntu $(UBUNTU_VERSION), not UBUNTU_RELEASE's $(UBUNTU_SERIES) series: $(ISO_URL)" >&2; exit 1 ;; \
	esac
	@# Fail closed rather than build against an unverified ISO: the default
	@# ISO_CHECKSUM is sha256:$(UBUNTU_ISO_SHA256), so an unpinned release
	@# leaves nothing after the colon.
	@if [ "$(ISO_CHECKSUM)" = "sha256:" ]; then \
		echo "no committed installer digest for UBUNTU_RELEASE=$(UBUNTU_RELEASE) ARCH=$(ARCH)." >&2; \
		echo "Add UBUNTU_ISO_SHA256_$(subst .,_,$(UBUNTU_RELEASE))_$(if $(filter arm64,$(ARCH)),ARM64,AMD64) to make/tool-versions.mk; that file says how to obtain and verify it." >&2; \
		exit 1; \
	fi
	mkdir -p $(ISO_CACHE)
	ISO_URL=$(ISO_URL) ISO_CHECKSUM=$(ISO_CHECKSUM) OUT_DIR=$(ISO_CACHE) ./scripts/prepare-installer-iso.sh

# Image builds run the Ubuntu installer, so they need kvm or tcg. The shared
# boot harness also knows about hvf, but that is for run-vm/boot-smoke on
# macOS; check-deps picks the host or VM package builder separately.
# Make writes ARCH into the material manifest, so require the acquired ISO
# filename to agree with the architecture Packer will read from that manifest.
check-acceleration:
	@case "$(ACCELERATOR)" in \
		kvm|tcg) ;; \
		*) echo "ACCELERATOR=$(ACCELERATOR) is not supported for image builds: they run under kvm or tcg. hvf is for run-vm/boot-smoke only." >&2; exit 1 ;; \
	esac
	@ARCH=$(ARCH) ISO_URL=$(ISO_URL) bash -ec '\
		if [[ "$$ISO_URL" =~ ubuntu-[0-9.]+-live-server-(amd64|arm64)\.iso$$ ]]; then \
			iso_arch="$${BASH_REMATCH[1]}"; \
		else \
			iso_arch=unrecognised; \
		fi; \
		if [[ "$$iso_arch" != "$$ARCH" ]]; then \
			echo "ISO_URL architecture ($$iso_arch) does not match ARCH=$$ARCH: $$ISO_URL" >&2; \
			exit 1; \
		fi'
	@# A neighbour pod's qemu postinst resets the shared /dev/kvm to 0660 for a few seconds; widen it again and retry.
	@ARCH=$(ARCH) ACCELERATOR=$(ACCELERATOR) REQUIRE_ACCELERATION=$(REQUIRE_ACCELERATION) bash -ec '\
		. ./scripts/qemu-arch.sh; \
		for attempt in $$(seq 1 $(KVM_SETTLE_RETRIES)); do \
			nvke_qemu_env "$$ARCH" && exit 0; \
			[[ -c /dev/kvm && $$attempt -lt $(KVM_SETTLE_RETRIES) ]] || exit 1; \
			echo "KVM check attempt $$attempt failed with /dev/kvm mode $$(stat -c %a /dev/kvm); retrying in 10s" >&2; \
			sudo -n chmod 666 /dev/kvm 2>/dev/null || true; \
			sleep 10; \
		done'

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
	NKE_TARGET_UBUNTU_SERIES="$(UBUNTU_SERIES)" \
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

# Adapt Make's current networked preparation into the same material
# layout used by direct Packer callers. The manifest is only an input-root
# anchor in this compatibility path; external runners provide their own
# authenticated manifest and exact material bytes.
prepare-packer-inputs: prepare-installer-iso prepare-artifacts
	@set -eu; \
	input_root="$(PACKER_INPUT_ROOT)"; \
	stage="$$(mktemp -d "$(PACKER_ROOT)/.packer-inputs.tmp.XXXXXX")"; \
	cleanup() { rm -rf -- "$$stage"; }; \
	trap cleanup 0 1 2 15; \
	mkdir -p "$$stage/materials"; \
	ln -s "$(ISO_CACHE)/$(notdir $(ISO_URL)).patched.iso" "$$stage/materials/installer-iso"; \
	tar -C "$(ARTIFACTS)" -cf "$$stage/materials/image-inputs" .; \
	iso_sha="$$(sha256sum "$(ISO_CACHE)/$(notdir $(ISO_URL)).patched.iso")"; \
	iso_sha="$${iso_sha%% *}"; \
	inputs_sha="$$(sha256sum "$$stage/materials/image-inputs")"; \
	inputs_sha="$${inputs_sha%% *}"; \
	printf '%s' '{"schemaVersion":"nvke.nvidia.com/os-image-materials/v1","materials":[' > "$$stage/material-manifest.json"; \
	printf '{"binding":"image-inputs","component":"nvke-image-inputs","kind":"archive","architecture":"%s","digest":"sha256:%s","path":"materials/image-inputs"},' \
		"$(ARCH)" "$$inputs_sha" >> "$$stage/material-manifest.json"; \
	printf '{"binding":"installer-iso","component":"ubuntu-installer","kind":"disk-image","architecture":"%s","digest":"sha256:%s","path":"materials/installer-iso"}]}\n' \
		"$(ARCH)" "$$iso_sha" >> "$$stage/material-manifest.json"; \
	rm -rf -- "$$input_root"; \
	mv -- "$$stage" "$$input_root"; \
	stage=""; \
	trap - 0 1 2 15

check-deps:
	@command -v ssh-keygen >/dev/null 2>&1 || { \
		echo "ssh-keygen is required to prepare the Packer SSH key pair" >&2; \
		exit 1; \
	}
	@ARCH=$(ARCH) NKE_TARGET_UBUNTU_SERIES=$(UBUNTU_SERIES) bash ./scripts/package-builder.sh --check
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
	@command -v tar >/dev/null 2>&1 || { \
		echo "tar is required to package prepared image inputs" >&2; \
		exit 1; \
	}
	@command -v sha256sum >/dev/null 2>&1 || { \
		echo "sha256sum is required to describe prepared image inputs" >&2; \
		exit 1; \
	}

.PHONY: test
test: ## Check artifact staging, QCOW2 collection, and workflow wiring.
	$(PYTHON) -B -m unittest discover -s scripts -p 'test_*.py'

# Read one derived value, the same way make/versions.mk does for the repository
# root. The values that depend on UBUNTU_RELEASE and ARCH are computed here and
# nowhere else, so this is how a caller or a test sees what a given pair
# resolves to without starting a build.
print-%:
	@echo "$($*)"

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
	expected="nvke-node-$(1)-ubuntu-$$version-$(ARCH).qcow2"; \
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
# an LVM-on-LUKS layout via autoinstall, then provisions nvke-agent/spire-agent,
# plus TPM2 binding scaffolding deferred to first boot. See
# ../docs/full-disk-encryption.md.
os-image-fde:
	@if [ -e "$(CURDIR)/output-fde" ] || [ -L "$(CURDIR)/output-fde" ]; then \
		echo "output-fde already exists; run make clean-fde-output before rebuilding" >&2; \
		exit 1; \
	fi
	@if [ -e "$(PACKER_OUTPUT_DIRECTORY_FDE)" ] || [ -L "$(PACKER_OUTPUT_DIRECTORY_FDE)" ]; then \
		echo "Packer output scratch already exists; run make clean-fde-output before rebuilding" >&2; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory --file "$(PACKER_MAKEFILE)" _os-image-fde

_os-image-fde: prepare-image-build
	mkdir -p "$(BUILD_LOGS)" "$(PACKER_OUTPUT_DIRECTORY_FDE)"
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	PACKER_LOG=1 PACKER_LOG_PATH="$(BUILD_LOGS)/packer-fde.log" \
	PKR_VAR_luks_passphrase=$(LUKS_PASSPHRASE) \
	$(PACKER_BUILD) \
		-only=$(PACKER_BUILD_FDE) \
		-var "architecture=$(ARCH)" \
		-var "ubuntu_series=$(UBUNTU_SERIES)" \
		-var "material_manifest=$(MATERIAL_MANIFEST)" \
		-var "output_directory=$(PACKER_OUTPUT_DIRECTORY_FDE)" \
		-var "build_log_directory=$(BUILD_LOGS)" \
		-var "ssh_private_key_file=$(PACKER_SSH_PRIVATE_KEY_FILE)" \
		-var "ssh_public_key_file=$(PACKER_SSH_PUBLIC_KEY_FILE)" \
		-var "accelerator=$(ACCELERATOR)" \
		-var "cpu_model=$(CPU_MODEL)" \
		-var "cpus=$(CPUS)" \
		-var "guest_memory_mib=$(GUEST_MEMORY_MIB)" \
		-var "ssh_timeout=$(SSH_TIMEOUT)" \
		-var "boot_wait=$(BOOT_WAIT)" \
		-var "root_fs_label=$(ROOT_FS_LABEL)" \
		-var "serial_console=$(SERIAL_CONSOLE)" \
		"$(PACKER_PROJECT)"
	mkdir "$(CURDIR)/output-fde"
	mv "$(PACKER_OUTPUT_DIRECTORY_FDE)/fde/nvke-node.qcow2" \
		"$(CURDIR)/output-fde/nvke-node-fde-ubuntu-$(UBUNTU_VERSION)-$(ARCH).qcow2"
	rm -rf -- "$(PACKER_OUTPUT_DIRECTORY_FDE)"

# Non-FDE variant: LVM-backed ext4 without LUKS. See
# nvke-node-non-fde.pkr.hcl.
os-image-non-fde:
	@if [ -e "$(CURDIR)/output-non-fde" ] || [ -L "$(CURDIR)/output-non-fde" ]; then \
		echo "output-non-fde already exists; run make clean-non-fde-output before rebuilding" >&2; \
		exit 1; \
	fi
	@if [ -e "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)" ] || [ -L "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)" ]; then \
		echo "Packer output scratch already exists; run make clean-non-fde-output before rebuilding" >&2; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory --file "$(PACKER_MAKEFILE)" _os-image-non-fde

_os-image-non-fde: prepare-image-build
	mkdir -p "$(BUILD_LOGS)" "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)"
	@if [ "$(ACCELERATOR)" = "tcg" ]; then \
		echo "no usable /dev/kvm: building with software emulation, which is far slower"; \
	fi
	PACKER_LOG=1 PACKER_LOG_PATH="$(BUILD_LOGS)/packer-non-fde.log" \
	$(PACKER_BUILD) \
		-only=$(PACKER_BUILD_NON_FDE) \
		-var "architecture=$(ARCH)" \
		-var "ubuntu_series=$(UBUNTU_SERIES)" \
		-var "material_manifest=$(MATERIAL_MANIFEST)" \
		-var "output_directory=$(PACKER_OUTPUT_DIRECTORY_NON_FDE)" \
		-var "build_log_directory=$(BUILD_LOGS)" \
		-var "ssh_private_key_file=$(PACKER_SSH_PRIVATE_KEY_FILE)" \
		-var "ssh_public_key_file=$(PACKER_SSH_PUBLIC_KEY_FILE)" \
		-var "accelerator=$(ACCELERATOR)" \
		-var "cpu_model=$(CPU_MODEL)" \
		-var "cpus=$(CPUS)" \
		-var "guest_memory_mib=$(GUEST_MEMORY_MIB)" \
		-var "ssh_timeout=$(SSH_TIMEOUT)" \
		-var "boot_wait=$(BOOT_WAIT)" \
		-var "root_fs_label=$(ROOT_FS_LABEL)" \
		-var "serial_console=$(SERIAL_CONSOLE)" \
		"$(PACKER_PROJECT)"
	mkdir "$(CURDIR)/output-non-fde"
	mv "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)/non-fde/nvke-node.qcow2" \
		"$(CURDIR)/output-non-fde/nvke-node-non-fde-ubuntu-$(UBUNTU_VERSION)-$(ARCH).qcow2"
	rm -rf -- "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)"

clean-packer-scratch:
	rm -rf -- "$(PACKER_INPUT_ROOT)" "$(PACKER_ROOT)/.packer-output" "$(PACKER_OUTPUT_DIRECTORY_FDE)" "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)"

clean-fde-output:
	rm -rf -- "$(CURDIR)/output-fde" "$(PACKER_OUTPUT_DIRECTORY_FDE)"

clean-non-fde-output:
	rm -rf -- "$(CURDIR)/output-non-fde" "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)"

clean:
	rm -rf -- "$(CURDIR)/output-fde" "$(CURDIR)/output-non-fde" "$(ARTIFACTS)" "$(PACKER_INPUT_ROOT)" "$(PACKER_ROOT)/.packer-output" "$(PACKER_OUTPUT_DIRECTORY_FDE)" "$(PACKER_OUTPUT_DIRECTORY_NON_FDE)" "$(ISO_CACHE)" "$(BUILD_LOGS)"
