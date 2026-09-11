# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared targets for the CNI deb packages. A package Makefile sets CNI, PRODUCT,
# VERSIONS, CONTENT_SPEC, CONTENT_KIND (and BUNDLE_DEPS), then includes this.

COMMON_DIR := $(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

GO ?= go

# CRANE_VERSION and NFPM_VERSION come from make/tool-versions.mk.
include $(COMMON_DIR)/../../../make/tool-versions.mk

# Where the .deb files are written. Required by `debs`.
OUT_DIR ?=

# Debian packaging revision. Bump when the package content changes but the
# upstream version does not.
DEB_REVISION ?= 1

DIST_DIR ?= $(CURDIR)/dist
BIN_DIR ?= $(CURDIR)/bin
LOCK_DIR ?= $(CURDIR)/lock
BUNDLE_DEPS ?=

##@ Build

.PHONY: debs
debs: check-host check-deb-tools bundle ## Build the content and images debs into OUT_DIR, and reindex it.
	@# A .deb left from a different VERSIONS would be indexed too.
	rm -f "$(OUT_DIR)"/nke-$(CNI)-*.deb
	@set -eu; \
	for v in $(VERSIONS); do \
		CNI="$(CNI)" PRODUCT="$(PRODUCT)" CONTENT_SPEC="$(CONTENT_SPEC)" CONTENT_KIND="$(CONTENT_KIND)" \
		DEB_REVISION="$(DEB_REVISION)" NFPM_BIN="$(BIN_DIR)/nfpm" \
			$(COMMON_DIR)/build-deb.sh "$$v" "$(DIST_DIR)/$$v" "$(OUT_DIR)"; \
	done
	@# Index everything in OUT_DIR: the CNI packages share it.
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"

.PHONY: bundle
bundle: check-host $(BUNDLE_DEPS) $(BIN_DIR)/crane ## Download, verify and stage the upstream artifacts into DIST_DIR.
	@set -eu; \
	for v in $(VERSIONS); do \
		./bundle.sh --version "$$v" --dist "$(DIST_DIR)" --lock-dir "$(LOCK_DIR)" --crane "$(BIN_DIR)/crane"; \
	done

.PHONY: lock
lock: check-host $(BUNDLE_DEPS) $(BIN_DIR)/crane ## Regenerate lock/<version>.lock from upstream. Review the diff, then commit it.
	@set -eu; \
	for v in $(VERSIONS); do \
		./bundle.sh --write-lock --version "$$v" --dist "$(DIST_DIR)" --lock-dir "$(LOCK_DIR)" --crane "$(BIN_DIR)/crane"; \
	done
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK_DIR)"

.PHONY: repro-check
repro-check: ## Build `debs` twice into throwaway OUT_DIRs and diff every .deb byte-for-byte.
	@set -eu; \
	a="$$(mktemp -d)"; b="$$(mktemp -d)"; \
	trap 'rm -rf "$$a" "$$b"' EXIT; \
	$(MAKE) debs OUT_DIR="$$a" VERSIONS="$(VERSIONS)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)"; \
	$(MAKE) debs OUT_DIR="$$b" VERSIONS="$(VERSIONS)" DEB_REVISION="$(DEB_REVISION)" DIST_DIR="$(DIST_DIR)"; \
	diffs=""; \
	for f in "$$a"/*.deb; do \
		name="$$(basename "$$f")"; \
		if [ ! -f "$$b/$$name" ] || ! cmp -s "$$f" "$$b/$$name"; then diffs="$$diffs $$name"; fi; \
	done; \
	if [ -n "$$diffs" ]; then \
		echo "$(CNI): not reproducible, these differ between builds:$$diffs" >&2; exit 1; \
	fi; \
	echo "$(CNI): reproducible: every .deb matched byte-for-byte"

$(BIN_DIR)/crane: ## Download the pinned crane (go-containerregistry) release into ./bin.
	@set -eu; \
	mkdir -p "$(BIN_DIR)"; \
	arch="$$(uname -m)"; \
	case "$$arch" in aarch64) arch=arm64 ;; esac; \
	archive="go-containerregistry_$$(uname -s)_$${arch}.tar.gz"; \
	base="https://github.com/google/go-containerregistry/releases/download/v$(CRANE_VERSION)"; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	curl -fsSL -o "$$tmp/$$archive" "$$base/$$archive"; \
	curl -fsSL -o "$$tmp/checksums.txt" "$$base/checksums.txt"; \
	(cd "$$tmp" && grep " $$archive\$$" checksums.txt | sha256sum -c); \
	tar -xzf "$$tmp/$$archive" -C "$$tmp" crane; \
	chmod +x "$$tmp/crane"; \
	mv "$$tmp/crane" "$(BIN_DIR)/crane"

.PHONY: nfpm
nfpm: $(BIN_DIR)/nfpm ## Build the pinned nfpm (goreleaser) into ./bin.

$(BIN_DIR)/nfpm:
	@GOBIN=$(BIN_DIR) $(GO) install github.com/goreleaser/nfpm/v2/cmd/nfpm@v$(NFPM_VERSION)

##@ Checks

.PHONY: check-host
check-host: ## Fail early when this host cannot run the bundle step.
	@# The repack pins owner, mode and mtime with GNU tar semantics.
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "$(CNI): the bundle step needs a Linux host; this is $$(uname -s)." >&2; \
		exit 1; \
	fi
	@missing=""; \
	for tool in curl gzip sha256sum tar; do \
		command -v "$$tool" >/dev/null 2>&1 || missing="$$missing $$tool"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "$(CNI): missing tools needed to bundle the upstream artifacts:$$missing" >&2; \
		exit 1; \
	fi

.PHONY: check-deb-tools
check-deb-tools: $(BIN_DIR)/nfpm ## Fail early when the Debian packaging tools are missing.
	@set -eu; \
	if [ "$$(uname -s)" != "Linux" ]; then \
		echo "$(CNI): packages are built on a Linux host only; this is $$(uname -s)." >&2; \
		exit 1; \
	fi; \
	if ! command -v dpkg-scanpackages >/dev/null 2>&1; then \
		echo "$(CNI): dpkg-scanpackages is required to index OUT_DIR." >&2; \
		echo "  install it with: apt install dpkg-dev" >&2; \
		exit 1; \
	fi
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "$(CNI): OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "$(CNI): OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac

.PHONY: clean
clean: ## Remove the built tools and the downloaded artifact tree.
	rm -rf $(BIN_DIR) $(DIST_DIR)
