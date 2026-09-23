# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The licence header template lives once, at the repository root.
##@ Help

# Same awk as the sibling modules: sections come from ##@ lines and entries from
# the ## comment on each target, so the list is read from this file and cannot go
# stale.
.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

REPO_ROOT ?= $(shell git rev-parse --show-toplevel)

# Module-level targets, matching the other workspace members. The root Makefile
# builds nke-os-image-check into the repository's bin/ as part of `make build`;
# this file is for working inside os-image/ alone, and it is what the Lint
# workflow invokes.
#
# Every dependency here is public, so these targets need neither the workspace
# nor the committed vendor/ tree, and CI runs them with GOWORK=off.

GO ?= go
BINARY := bin/nke-os-image-check

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches the
# sibling modules.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# golangci-lint pin and bootstrap, shared with every other module.
include ../make/golangci.mk

# Version stamping, mirroring cli/Makefile.
#
# --match is not optional, and it is required on the --always fallback too. This
# is a monorepo whose tags are per-component (agent/v*, controller/v*), so any
# describe without --match returns whichever family happens to be nearest. No
# os-image/v* tag exists yet, so an unmatched fallback does not degrade to a
# commit the way it reads -- it stamps this binary with an agent version.
# Scoped, the chain is: this module's tag, a plain v* tag, the commit, then
# v0.0.0 for a build outside a git checkout.
_CHECK_VERSION_RAW := $(shell git describe --tags --dirty --match 'os-image/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || git describe --always --dirty --match 'os-image/v[0-9]*' 2>/dev/null || echo "v0.0.0")
CHECK_VERSION ?= $(patsubst os-image/%,%,$(_CHECK_VERSION_RAW))

# "main", not the full import path. The variable lives in package main, and for
# that package the linker matches on the package NAME. Given the import path it
# finds no symbol and silently changes nothing -- no error, no warning, just a
# binary that still reports "dev". Hence the version assertion in `make build`.
CHECK_LDFLAGS := -X 'main.version=$(CHECK_VERSION)'

GO_TEST_FLAGS ?=

.PHONY: build compile test fmt lint license file-license-check clean boot-smoke run-vm

build: ## Build nke-os-image-check into bin/.
	$(GO) build -ldflags "$(CHECK_LDFLAGS)" -o $(BINARY) ./cmd/nke-os-image-check
	@# A wrong -X symbol is silently ignored by the linker, so the only way to
	@# know the stamping worked is to ask the binary. Cheap, and it fails loudly.
	@out="$$($(BINARY) --version)"; \
	case "$$out" in \
	  *dev*|*unknown*|*v0.0.0*) \
	    echo "ERROR: nke-os-image-check was built without version information: $$out"; \
	    echo "       The -X symbol in CHECK_LDFLAGS no longer matches the variable in cmd/nke-os-image-check."; \
	    exit 1;; \
	  *) echo "  $$out";; \
	esac

compile: ## Type-check every package, including the ones with no binary.
	$(GO) build ./...

test: test-qemu ## Run this module's tests. (run by the merge gate)
	$(GO) test $(GO_TEST_FLAGS) ./...

fmt: ## Format the source with golangci-lint's formatters.
	@$(GOLANGCI_LINT_RUN) fmt ./...

lint: ## Run golangci-lint over this module. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@v1.1.1 -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" .

file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi

boot-smoke: ## Boot the packer/output qcow2 in QEMU and confirm cloud-init completes.
	./packer/scripts/boot-smoke.sh

run-vm: ## Boot a qcow2 and leave it running for interactive SSH. QCOW=path required; see the script header for OVMF_CODE/TPM.
	./packer/scripts/run-vm.sh

clean:
	rm -rf bin

.PHONY: notices
notices: ## Regenerate THIRD_PARTY_NOTICES.md for the nke-os-image-check binary.
	@# The tool and the script both live in controller/, a sibling of this
	@# module. `go-licenses`, not `bin/go-licenses`: the real rule's target is an
	@# absolute path, so a relative name does not match it.
	@#
	@# GOWORK is deliberately NOT set to off here, unlike the other targets in
	@# this file. os-image requires github.com/dsx-ai-factory/nke/agent, which resolves
	@# only through the workspace; with the workspace off, go-licenses cannot
	@# load the module and fails before it collects anything.
	@$(MAKE) --no-print-directory -C ../controller go-licenses
	@MODULE_ROOT="$(CURDIR)" \
		OUTPUT="$(CURDIR)/THIRD_PARTY_NOTICES.md" \
		PACKAGES="./cmd/nke-os-image-check" \
		GO_LICENSES="$(CURDIR)/../controller/bin/go-licenses" \
		bash ../controller/hack/generate-notices

.PHONY: notices-check
notices-check: ## Verify THIRD_PARTY_NOTICES.md is up to date. (advisory; not in the merge gate)
	@# git diff says nothing about an untracked path, so check tracking first --
	@# otherwise a deleted or never-committed notices file passes silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md > /dev/null 2>&1 \
		|| { echo "THIRD_PARTY_NOTICES.md is not tracked by git. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory notices
	@if ! git diff --exit-code -- THIRD_PARTY_NOTICES.md > /dev/null 2>&1; then \
		echo "THIRD_PARTY_NOTICES.md is out of date. Please run 'make notices' locally and commit the changes."; \
		git diff --name-only -- THIRD_PARTY_NOTICES.md; \
		exit 1; \
	fi

.PHONY: image image-native test-qemu
ARCH ?= amd64
VARIANT ?= non-fde

image: ## Bake with native or NixOS VM package builds (ARCH=amd64|arm64, VARIANT=fde|non-fde).
	@case "$(VARIANT)" in fde|non-fde) ;; *) echo "VARIANT must be fde or non-fde" >&2; exit 1 ;; esac
	$(MAKE) -C packer os-image-$(VARIANT) ARCH=$(ARCH)

image-native: ## Bake with native Linux KVM; fail if acceleration is unavailable.
	$(MAKE) image REQUIRE_ACCELERATION=1

test-qemu: ## Test QEMU architecture, firmware, and accelerator selection without a VM.
	bash ./packer/scripts/qemu-arch-test.sh
	python3 ./packer/scripts/test_build_entrypoint.py
