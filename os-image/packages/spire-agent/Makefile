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

# NFPM_VERSION comes from make/tool-versions.mk.
include ../../../make/tool-versions.mk

# The upstream releases this package wraps. Bump them here; see README.md.
# TPM_PLUGIN_VERSION also pins the *server*-side attestor elsewhere in the
# repo (controller/Tiltfile, controller/hack/ci/prepare-capd-bundles.sh,
# cli/internal/bootstrap/manifest/bootstrap-manifest.yaml, and the tpm-direct
# checksums in cli/internal/bootstrap/cwedeploy/cwedeploy.go) -- move them together.
SPIRE_VERSION      ?= 1.14.1
TPM_PLUGIN_VERSION ?= 1.9.0

# Both architectures: unlike the CNI and kubeadm-distro packages, every
# artifact here is published for arm64 too and the lock pins both.
ARCH ?= amd64

# Where the .deb files are written. Required by `debs`.
OUT_DIR ?=

# Debian packaging revision. Bump when the package content changes but neither
# upstream version does. Revision 2 adds the DSX IMDS node attestor plugin.
DEB_REVISION ?= 2

BUNDLE_VERSION := $(SPIRE_VERSION)+tpm$(TPM_PLUGIN_VERSION)

# Downloaded upstream artifacts, reused across runs when they still verify.
DIST_DIR ?= $(CURDIR)/dist
BIN_DIR ?= $(CURDIR)/bin
NFPM_BIN := $(BIN_DIR)/nfpm-v$(NFPM_VERSION)/nfpm

# The committed lock. The build refuses to run without it.
LOCK_DIR ?= $(CURDIR)/lock

BUNDLE_ARGS = --spire-version $(SPIRE_VERSION) --tpm-version $(TPM_PLUGIN_VERSION) \
	--dist "$(DIST_DIR)" --lock-dir "$(LOCK_DIR)"

##@ Build

.PHONY: debs
debs: check-host check-deb-tools bundle ## Build spire-agent_<version>_$(ARCH).deb into OUT_DIR, plus a flat apt index.
	@# Clear the previous run first: a .deb left from a different version pair
	@# would be indexed here alongside the current one.
	rm -f "$(OUT_DIR)"/spire-agent_*_$(ARCH).deb
	SPIRE_VERSION="$(SPIRE_VERSION)" TPM_PLUGIN_VERSION="$(TPM_PLUGIN_VERSION)" \
	DEB_REVISION="$(DEB_REVISION)" NFPM_BIN="$(NFPM_BIN)" \
		./packaging/build-deb.sh "$(ARCH)" "$(DIST_DIR)/$(BUNDLE_VERSION)/$(ARCH)" "$(OUT_DIR)"
	@# Index everything in OUT_DIR: the other node packages share it.
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"

.PHONY: bundle
bundle: check-host ## Download, verify and stage the upstream binaries into DIST_DIR.
	./bundle.sh $(BUNDLE_ARGS) --arch "$(ARCH)"

.PHONY: lock
lock: check-host ## Regenerate lock/<version>.lock from upstream, for every arch. Review the diff, then commit it.
	./bundle.sh --write-lock $(BUNDLE_ARGS)
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK_DIR)"

.PHONY: repro-check
repro-check: ## Build `debs` twice into throwaway OUT_DIRs and diff every output byte-for-byte.
	@# DIST_DIR is shared by both nested runs, so the download happens at most
	@# once and the second build measures build determinism rather than
	@# artifact-fetch flakiness.
	@set -eu; \
	a="$$(mktemp -d)"; b="$$(mktemp -d)"; \
	trap 'rm -rf "$$a" "$$b"' EXIT; \
	echo "spire-agent: first build -> $$a"; \
	$(MAKE) debs OUT_DIR="$$a" ARCH="$(ARCH)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)"; \
	echo "spire-agent: second build -> $$b"; \
	$(MAKE) debs OUT_DIR="$$b" ARCH="$(ARCH)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)"; \
	diffs=""; \
	for f in "$$a"/*.deb; do \
		name="$$(basename "$$f")"; \
		if [ ! -f "$$b/$$name" ] || ! cmp -s "$$f" "$$b/$$name"; then diffs="$$diffs $$name"; fi; \
	done; \
	if [ -n "$$diffs" ]; then \
		echo "spire-agent: not reproducible, these differ between builds:$$diffs" >&2; exit 1; \
	fi; \
	echo "spire-agent: reproducible: every .deb matched byte-for-byte"

.PHONY: nfpm
nfpm: $(NFPM_BIN) ## Build the pinned nfpm (goreleaser) into ./bin.

$(NFPM_BIN):
	@GOBIN=$(dir $(NFPM_BIN)) $(GO) install github.com/goreleaser/nfpm/v2/cmd/nfpm@v$(NFPM_VERSION)

##@ Checks

.PHONY: test
test: ## Exercise cold bundle builds for both architectures using local archives.
	python3 test_bundle.py

.PHONY: check-host
check-host: ## Fail early when this host cannot run the bundle step.
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "spire-agent: the bundle step needs a Linux host; this is $$(uname -s)." >&2; \
		exit 1; \
	fi
	@missing=""; \
	for tool in curl sha256sum tar; do \
		command -v "$$tool" >/dev/null 2>&1 || missing="$$missing $$tool"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "spire-agent: missing tools needed to bundle the upstream artifacts:$$missing" >&2; \
		exit 1; \
	fi

.PHONY: check-deb-tools
check-deb-tools: $(NFPM_BIN) ## Fail early when the Debian packaging tools are missing.
	@set -eu; \
	if [ "$$(uname -s)" != "Linux" ]; then \
		echo "spire-agent: packages are built on a Linux host only; this is $$(uname -s)." >&2; \
		exit 1; \
	fi; \
	if ! command -v dpkg-scanpackages >/dev/null 2>&1; then \
		echo "spire-agent: dpkg-scanpackages is required to index OUT_DIR." >&2; \
		echo "  install it with: apt install dpkg-dev" >&2; \
		exit 1; \
	fi
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "spire-agent: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "spire-agent: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac

.PHONY: clean
clean: ## Remove the built tools and the downloaded artifact tree.
	rm -rf $(BIN_DIR) $(DIST_DIR)
