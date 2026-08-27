# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go

GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

GOLANGCI_LINT_GOWORK :=
include ../../../make/golangci.mk

GO_TEST_FLAGS ?=

.PHONY: fmt lint test

fmt: ## Format Go files.
	@$(GOLANGCI_LINT_RUN) fmt ./...

lint: ## Run golangci-lint. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

test: ## Run tests.
	$(GO) test $(GO_TEST_FLAGS) ./...
