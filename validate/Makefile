# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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

GO ?= go
BINARY := bin/nvke-image-validate
VERDICT_BINARY := bin/nvke-verdict

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches the
# sibling modules.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# ADDLICENSE_VERSION comes from make/tool-versions.mk, which owns it.
include ../make/tool-versions.mk
# golangci-lint pin and bootstrap, shared with every other module.
include ../make/golangci.mk
include ../make/coverage.mk

# Version stamping, mirroring os-image/Makefile: tags are per-component in this
# monorepo, so every describe is scoped to validate/v* before falling back.
_VALIDATE_VERSION_RAW := $(shell git describe --tags --dirty --match 'validate/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || git describe --always --dirty --match 'validate/v[0-9]*' 2>/dev/null || echo "v0.0.0")
VALIDATE_VERSION ?= $(patsubst validate/%,%,$(_VALIDATE_VERSION_RAW))
VALIDATE_LDFLAGS := -X 'main.version=$(VALIDATE_VERSION)'

GO_TEST_FLAGS ?=

.PHONY: build compile test fmt vet lint license

##@ Build

build: ## Build nvke-image-validate and nvke-verdict into bin/.
	$(GO) build -ldflags "$(VALIDATE_LDFLAGS)" -o $(BINARY) ./cmd/nvke-image-validate
	$(GO) build -ldflags "$(VALIDATE_LDFLAGS)" -o $(VERDICT_BINARY) ./cmd/nvke-verdict

compile: ## Type-check every package, including the ones with no binary.
	$(GO) build ./...

##@ Verify

test: ## Run this module's tests. (run by the merge gate)
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...

fmt: ## Format the source with golangci-lint's formatters.
	@$(GOLANGCI_LINT_RUN) fmt ./...

vet: ## go vet all packages.
	$(GO) vet ./...

lint: ## Run golangci-lint over this module. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@$(ADDLICENSE_VERSION) -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" .
