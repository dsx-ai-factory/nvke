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

# NFPM_VERSION comes from make/tool-versions.mk, which owns it.
include ../../../make/tool-versions.mk

# Where the .deb is written. Required by `debs`.
OUT_DIR ?=

# The package version. Nothing is downloaded, so there is no upstream version to
# follow: bump this when the package's content changes.
# 0.1.0 holds the agent's SPIRE ordering drop-in and the socket wait script.
DEB_VERSION ?= 0.1.0

BIN_DIR ?= $(CURDIR)/bin
NFPM_BIN := $(BIN_DIR)/nfpm-v$(NFPM_VERSION)/nfpm

##@ Build

# The package is architecture-independent (shell, unit and config files), so
# there is no ARCH.
.PHONY: debs
debs: check-deb-tools ## Build nvke-node-identity_<version>_all.deb into OUT_DIR, plus a flat apt index.
	$(RM) "$(OUT_DIR)"/nvke-node-identity_*_all.deb
	DEB_VERSION="$(DEB_VERSION)" NFPM_BIN="$(NFPM_BIN)" ./packaging/build-deb.sh "$(OUT_DIR)"
	@# Index everything in OUT_DIR: the other node packages share it.
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"

.PHONY: nfpm
nfpm: $(NFPM_BIN) ## Build the pinned nfpm (goreleaser) into ./bin.

$(NFPM_BIN):
	@GOBIN=$(dir $(NFPM_BIN)) $(GO) install github.com/goreleaser/nfpm/v2/cmd/nfpm@v$(NFPM_VERSION)

##@ Checks

.PHONY: test
test: nfpm ## Build the package and inspect it. test/install.sh covers install, upgrade and remove on a systemd host.
	NFPM_BIN="$(NFPM_BIN)" $(GO) test ./test/...

.PHONY: check-deb-tools
check-deb-tools: $(NFPM_BIN) ## Fail early when the Debian packaging tools are missing.
	@set -eu; \
	if ! command -v dpkg-scanpackages >/dev/null 2>&1; then \
		echo "nvke-node-identity: dpkg-scanpackages is required to index OUT_DIR." >&2; \
		echo "  install it with: apt install dpkg-dev" >&2; \
		exit 1; \
	fi
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "nvke-node-identity: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "nvke-node-identity: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac
