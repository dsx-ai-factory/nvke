# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

GO ?= go

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# CRANE_VERSION and NFPM_VERSION come from make/tool-versions.mk.
include ../../../make/tool-versions.mk

# The newest patch of each of the three newest Kubernetes minors. Bump them
# here; see README.md. v1.34.2 stays as a fourth until the CI and dev
# defaults move off it.
K8S_VERSIONS ?= v1.37.2 v1.36.3 v1.35.3 v1.34.2

# The bundle directories and download URLs are all leading-v, so accept
# 1.36.3 as well as v1.36.3. Package names drop the v again.
K8S_VERSIONS_V := $(foreach v,$(K8S_VERSIONS),v$(patsubst v%,%,$(v)))

# Where the .deb files and the apt index are written. Required by `debs`.
OUT_DIR ?=

# Target architecture, amd64 or arm64. bundle.sh runs the downloaded kubeadm
# to list its images, so this has to match the host: there is no cross-build.
ARCH ?= amd64

# Debian packaging revision. Bump when the package content changes but the
# Kubernetes patch does not.
DEB_REVISION ?= 1

# Downloaded upstream artifacts, reused across runs when they still verify.
DIST_DIR ?= $(CURDIR)/dist
BIN_DIR ?= $(CURDIR)/bin
NFPM_BIN := $(BIN_DIR)/nfpm-v$(NFPM_VERSION)/nfpm

# The committed lock files, one per Kubernetes version per architecture. The
# build refuses to run without them.
LOCK_DIR ?= $(CURDIR)/lock

##@ Build

.PHONY: debs
debs: check-host check-deb-tools bundle ## Build one .deb per K8S_VERSIONS into OUT_DIR, plus a flat apt index.
	@# Clear the previous run first: a .deb left from a different K8S_VERSIONS
	@# would be indexed here without appearing in any package's Conflicts.
	rm -f "$(OUT_DIR)"/nvke-kubeadm-distro-*.deb "$(OUT_DIR)/Packages"
	@set -eu; \
	for v in $(K8S_VERSIONS_V); do \
		PEER_VERSIONS="$(K8S_VERSIONS_V)" DEB_REVISION="$(DEB_REVISION)" NFPM_BIN="$(NFPM_BIN)" ARCH="$(ARCH)" \
			./packaging/build-deb.sh "$$v" "$(DIST_DIR)/$$v" "$(OUT_DIR)"; \
	done
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"

.PHONY: bundle
bundle: check-host $(BIN_DIR)/crane ## Download, verify and stage the upstream artifacts into DIST_DIR.
	@set -eu; \
	for v in $(K8S_VERSIONS_V); do \
		./bundle.sh --version "$$v" --dist "$(DIST_DIR)" --lock-dir "$(LOCK_DIR)" --crane "$(BIN_DIR)/crane" --arch "$(ARCH)"; \
	done

.PHONY: repro-check
repro-check: ## Build `debs` twice into throwaway OUT_DIRs and diff every output byte-for-byte.
	@# No check-host/check-deb-tools prereqs: they would demand an OUT_DIR
	@# this target never reads, since it builds its own. The nested `debs`
	@# calls carry those prereqs themselves.
	@#
	@# DIST_DIR is shared by both nested runs, so the download happens at
	@# most once and the second build measures build determinism rather than
	@# artifact-fetch flakiness.
	@set -eu; \
	a="$$(mktemp -d)"; b="$$(mktemp -d)"; \
	trap 'rm -rf "$$a" "$$b"' EXIT; \
	echo "kubeadm-distro: first build -> $$a"; \
	$(MAKE) debs OUT_DIR="$$a" K8S_VERSIONS="$(K8S_VERSIONS)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)" ARCH="$(ARCH)"; \
	echo "kubeadm-distro: second build -> $$b"; \
	$(MAKE) debs OUT_DIR="$$b" K8S_VERSIONS="$(K8S_VERSIONS)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)" ARCH="$(ARCH)"; \
	sort "$$a/Packages" >"$$a/Packages.sorted"; \
	sort "$$b/Packages" >"$$b/Packages.sorted"; \
	diffs=""; \
	for f in "$$a"/*.deb "$$a/Packages.sorted"; do \
		name="$$(basename "$$f")"; \
		if [ ! -f "$$b/$$name" ] || ! cmp -s "$$f" "$$b/$$name"; then \
			diffs="$$diffs $$name"; \
		fi; \
	done; \
	for f in "$$b"/*.deb; do \
		name="$$(basename "$$f")"; \
		if [ ! -f "$$a/$$name" ]; then \
			diffs="$$diffs $$name(only-in-second-build)"; \
		fi; \
	done; \
	if [ -n "$$diffs" ]; then \
		echo "kubeadm-distro: not reproducible, these files differ between the two builds:$$diffs" >&2; \
		exit 1; \
	fi; \
	echo "kubeadm-distro: reproducible: every .deb and the sorted Packages index matched byte-for-byte"

.PHONY: lock
lock: check-host $(BIN_DIR)/crane ## Regenerate lock/$(ARCH)/<version>.lock from upstream. Review the diff, then commit it.
	@# Needs Linux and network: it downloads every artifact, runs the verified
	@# kubeadm for its image list, and resolves each tag to a digest.
	@set -eu; \
	for v in $(K8S_VERSIONS_V); do \
		./bundle.sh --write-lock --version "$$v" --dist "$(DIST_DIR)" --lock-dir "$(LOCK_DIR)" --crane "$(BIN_DIR)/crane" --arch "$(ARCH)"; \
	done
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK_DIR)"

$(BIN_DIR)/crane: ## Download the pinned crane (go-containerregistry) release into ./bin.
	@set -eu; \
	mkdir -p "$(BIN_DIR)"; \
	arch="$(UNAME_M)"; \
	case "$$arch" in aarch64) arch=arm64 ;; esac; \
	archive="go-containerregistry_$(UNAME_S)_$${arch}.tar.gz"; \
	base="https://github.com/google/go-containerregistry/releases/download/v$(CRANE_VERSION)"; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	curl -fsSL --retry 8 --retry-all-errors --retry-max-time 240 \
		-o "$$tmp/$$archive" "$$base/$$archive"; \
	curl -fsSL --retry 8 --retry-all-errors --retry-max-time 240 \
		-o "$$tmp/checksums.txt" "$$base/checksums.txt"; \
	(cd "$$tmp" && grep " $$archive\$$" checksums.txt | sha256sum -c); \
	tar -xzf "$$tmp/$$archive" -C "$$tmp" crane; \
	chmod +x "$$tmp/crane"; \
	mv "$$tmp/crane" "$(BIN_DIR)/crane"

# Phony alias so ../../Makefile's `test` can ask for the binary by name rather
# than by its absolute, versioned path.
.PHONY: nfpm
nfpm: $(NFPM_BIN) ## Build the pinned nfpm (goreleaser) into ./bin.

$(NFPM_BIN):
	@GOBIN=$(dir $(NFPM_BIN)) $(GO) install github.com/goreleaser/nfpm/v2/cmd/nfpm@v$(NFPM_VERSION)

##@ Checks

.PHONY: check-host
check-host: ## Fail early when this host cannot run the bundle step.
	@# bundle.sh runs the downloaded linux kubeadm binary to list the
	@# control-plane images and stages the containerd/CNI/crictl/helm tarballs
	@# into an FHS tree, both of which need a Linux host.
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "kubeadm-distro: the bundle step runs the downloaded linux kubeadm binary, so it needs a Linux host; this is $$(uname -s)." >&2; \
		exit 1; \
	fi
	@# What bundle.sh downloads, verifies and unpacks with.
	@missing=""; \
	for tool in curl gpg gzip sha256sum tar; do \
		command -v "$$tool" >/dev/null 2>&1 || missing="$$missing $$tool"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "kubeadm-distro: missing tools needed to bundle the upstream artifacts:$$missing" >&2; \
		exit 1; \
	fi

.PHONY: check-deb-tools
check-deb-tools: $(NFPM_BIN) ## Fail early when the Debian packaging tools are missing.
	@set -eu; \
	if [ "$$(uname -s)" != "Linux" ]; then \
		echo "kubeadm-distro: packages are built on a Linux host only; this is $$(uname -s)." >&2; \
		exit 1; \
	fi; \
	missing=""; \
	command -v dpkg-scanpackages >/dev/null 2>&1 || missing="$$missing dpkg-scanpackages(dpkg-dev)"; \
	command -v gpg >/dev/null 2>&1 || missing="$$missing gpg(gnupg)"; \
	if [ -n "$$missing" ]; then \
		echo "kubeadm-distro: missing build tools:$$missing" >&2; \
		echo "  install them with: apt install dpkg-dev gnupg" >&2; \
		exit 1; \
	fi
	@# Checked here rather than against MAKECMDGOALS, which `make clean debs`
	@# would defeat.
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "kubeadm-distro: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "kubeadm-distro: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac

.PHONY: test
test: $(NFPM_BIN) ## Run this package's Go tests.
	NFPM_BIN="$(NFPM_BIN)" $(GO) test ./...

.PHONY: clean
clean: ## Remove the built tools and the downloaded artifact tree.
	rm -rf $(BIN_DIR) $(DIST_DIR)
