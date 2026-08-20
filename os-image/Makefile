# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The licence header template lives once, at the repository root.
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

GOLANGCI_LINT_REPO    ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

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

.PHONY: build compile test fmt lint license file-license-check clean boot-smoke

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

test:
	$(GO) test $(GO_TEST_FLAGS) ./...

fmt:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint fmt ./...

lint:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

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

clean:
	rm -rf bin
