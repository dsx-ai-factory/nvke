# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Builds qcow-imager.efi, the EFI a node chainloads from iPXE to write an NVKE
# node qcow2 onto its disk. See README.md.
#
# This file holds no build logic. It checks out NVIDIA/infra-controller at a
# pinned commit and runs that repo's own cargo-make tasks, which are the
# upstream definition of this artifact. Every target runs the same here as in
# CI; the workflow only adds runner selection and artifact upload.

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

# Public, Apache-2.0. ncx-infra-controller-core redirects here.
IC_URL ?= https://github.com/NVIDIA/infra-controller.git

# Pinned by commit, never a branch: a moving tip would change what this builds
# with no commit here saying so. No published matrix says which EFI can image
# which qcow2, so bump this only after imaging a node and booting it.
#
# This is not the commit the shipped EFI is built from -- see "What this does
# not build" in README.md.
IC_COMMIT ?= 288755ffba508af7086af5463779f5ebf6e8ef8f

# Inside os-image/efi/ so `clean` can reach it. Holds mkosi's package cache,
# which is most of the disk.
IC_DIR ?= $(CURDIR)/.infra-controller
OUTPUT_DIR ?= $(CURDIR)/output
PATCHES := $(sort $(wildcard $(CURDIR)/patches/*.patch))
MKOSI_PATCHES := $(sort $(wildcard $(CURDIR)/patches/mkosi/*.patch))

# Upstream derives this from `git describe --tags`, and the depth-1 checkout
# below has no tags. Nothing in the qcow-imager profile reads the value.
PKG_VERSION ?= 0.0.0-nke-efi

CARGO ?= cargo

.PHONY: check-deps setup-host upstream test-patches build-efi-x86 build-efi-arm64 build-efi-all clean

setup-host: ## Install the host packages mkosi needs. Ubuntu/Debian; needs root or sudo.
	./scripts/setup-efi-build-host.sh

# Probes only what has been observed to fail, and points at setup-host for the
# rest: mkosi runs most of its tooling from the tools image it builds itself,
# so a longer list of command -v checks would give false confidence.
check-deps: ## Check this host can build the EFI, and say what is missing.
	@fail=0; \
	if [ "$$(uname -s)" != "Linux" ]; then \
		echo "mkosi needs Linux namespaces; this is $$(uname -s). Build on a Linux box." >&2; \
		exit 1; \
	fi; \
	command -v git > /dev/null 2>&1 || { echo "git is required" >&2; fail=1; }; \
	command -v $(CARGO) > /dev/null 2>&1 || { \
		echo "cargo is required (upstream's tasks are cargo-make tasks): https://rustup.rs" >&2; fail=1; }; \
	$(CARGO) make --version > /dev/null 2>&1 || { \
		echo "cargo-make is required: cargo install cargo-make --locked" >&2; fail=1; }; \
	command -v apt-get > /dev/null 2>&1 || { \
		echo "apt-get is required: mkosi bootstraps Debian/Ubuntu roots with apt" >&2; fail=1; }; \
	[ -f /usr/share/keyrings/debian-archive-keyring.gpg ] || { \
		echo "missing /usr/share/keyrings/debian-archive-keyring.gpg (package: debian-archive-keyring)" >&2; fail=1; }; \
	[ -f /usr/share/keyrings/ubuntu-archive-keyring.gpg ] || { \
		echo "missing /usr/share/keyrings/ubuntu-archive-keyring.gpg (package: ubuntu-keyring)" >&2; fail=1; }; \
	if [ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ] && \
	   [ "$$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns)" != "0" ]; then \
		echo "kernel.apparmor_restrict_unprivileged_userns is 1; mkosi cannot unshare namespaces" >&2; \
		fail=1; \
	fi; \
	if [ "$$fail" != "0" ]; then \
		echo "Run 'make -C os-image/efi setup-host'." >&2; exit 1; \
	fi; \
	echo "Host looks able to build the EFI."

# Fetch the pinned commit by SHA at depth 1 (GitHub allows want-by-SHA), then
# detach onto it -- the shape controller/Tiltfile uses for its pinned upstream
# chart. Works for a fresh directory and an existing checkout alike.
#
# --force on the submodule: upstream's own build edits a tracked file inside
# pxe/mkosi in place, so after one build a plain update refuses to touch it.
# Only pxe/mkosi is initialised; no qcow-imager task reads the iPXE submodule.
upstream: ## Check out NVIDIA/infra-controller at the pinned commit.
	@if [ ! -d "$(IC_DIR)/.git" ]; then \
		rm -rf "$(IC_DIR)"; mkdir -p "$(IC_DIR)"; \
		git -C "$(IC_DIR)" init -q; \
		git -C "$(IC_DIR)" remote add origin "$(IC_URL)"; \
	else \
		git -C "$(IC_DIR)" remote set-url origin "$(IC_URL)"; \
	fi
	git -C "$(IC_DIR)" fetch --depth=1 origin "$(IC_COMMIT)"
	git -C "$(IC_DIR)" checkout -q --detach "$(IC_COMMIT)"
	git -C "$(IC_DIR)" submodule update --init --force --depth=1 pxe/mkosi
	@for patch in $(PATCHES); do \
		if git -C "$(IC_DIR)" apply --unidiff-zero --reverse --check "$$patch" >/dev/null 2>&1; then \
			continue; \
		fi; \
		git -C "$(IC_DIR)" apply --unidiff-zero "$$patch"; \
	done
	@for patch in $(MKOSI_PATCHES); do \
		if git -C "$(IC_DIR)/pxe/mkosi" apply --reverse --check "$$patch" >/dev/null 2>&1; then \
			continue; \
		fi; \
		git -C "$(IC_DIR)/pxe/mkosi" apply "$$patch"; \
	done

test-patches: upstream ## Test the downstream disk-imaging fixes.
	bash "$(IC_DIR)/pxe/common_files/disk_imaging_test.sh"
	@if grep -Fq '?exact-name(grub-pc-bin)' \
		"$(IC_DIR)/pxe/mkosi/mkosi/resources/mkosi-tools/mkosi.conf.d/10-debian-kali-ubuntu/mkosi.conf"; then \
		echo "architecture-neutral mkosi tools still require grub-pc-bin" >&2; \
		exit 1; \
	fi
	@grep -Fq 'grub-pc-bin' \
		"$(IC_DIR)/pxe/mkosi/mkosi.conf.d/30-debian-kali-ubuntu/mkosi.conf.d/20-x86-64.conf"

# `--cwd pxe`, not a cd: upstream's copy step writes to a path relative to pxe.
# This is how infra-controller's own CI invokes it.
define build_efi
	$(MAKE) check-deps
	$(MAKE) test-patches
	mkdir -p "$(OUTPUT_DIR)/$(2)"
	cd "$(IC_DIR)" && PKG_VERSION="$(PKG_VERSION)" $(CARGO) make --cwd pxe $(1)
	cp "$(IC_DIR)/pxe/static/blobs/internal/$(2)/qcow-imager.efi" "$(OUTPUT_DIR)/$(2)/qcow-imager.efi"
	@file "$(OUTPUT_DIR)/$(2)/qcow-imager.efi"
endef

build-efi-x86: ## Build qcow-imager.efi for x86_64 into output/x86_64/.
	$(call build_efi,build-qcow-imager-x86,x86_64)

# On an x86_64 host this runs the whole apt/dpkg pass under qemu-user
# emulation, which works but roughly doubles the build.
build-efi-arm64: ## Build qcow-imager.efi for aarch64 into output/aarch64/.
	$(call build_efi,build-qcow-imager-aarch64,aarch64)

build-efi-all: build-efi-x86 build-efi-arm64 ## Build both architectures on this host.

# Removes the checkout too: mkosi's package cache lives under it.
clean: ## Remove the upstream checkout, its caches, and output/.
	rm -rf "$(IC_DIR)" "$(OUTPUT_DIR)"
