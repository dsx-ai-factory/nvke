# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go
GO_TEST_FLAGS ?=
GOBIN ?= $(shell $(GO) env GOPATH)/bin
# GOLANGCI_LINT_GOWORK is left at its default of `off`. This module has no
# internal dependency and needs nothing from the root vendor/ tree, so it
# lints against its own go.mod and never reaches cluster-api-provider-nico.
include ../make/golangci.mk
include ../make/coverage.mk

.DEFAULT_GOAL := help
.PHONY: help build test lint fmt

help: ## List the release tooling targets.
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Compile the qualify command.
	$(GO) build ./...

test: ## Run this module's tests. (run by the merge gate)
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...

lint: ## Run the repository-pinned Go linter. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

fmt: ## Format this module's sources.
	$(GO) fmt ./...
