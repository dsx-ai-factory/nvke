# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# This module embeds the Helm charts beside it so nkectl carries them. It is
# its own module, like controller/api, so importing it does not pull the
# controller's dependency graph into the importer.

GO ?= go

GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# This module inherits controller/.golangci.yml; keep workspace mode on to
# match the other module builds.
GOLANGCI_LINT_GOWORK :=
include ../../../make/golangci.mk
include ../../../make/coverage.mk

GO_TEST_FLAGS ?=

.PHONY: help fmt lint test

##@ Help

help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

##@ Build and test

fmt: ## Format the source with golangci-lint's formatters.
	@$(GOLANGCI_LINT_RUN) fmt ./...

lint: ## Run golangci-lint over this module. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m

test: ## Run this module's tests.
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...
