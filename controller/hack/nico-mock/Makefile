# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The licence header template lives once, at the repository root.
REPO_ROOT ?= $(shell git rev-parse --show-toplevel)

# Module-level targets, matching the other workspace members. Until this file
# existed, nico-mock was the only tested module with no Makefile: its
# controller-test.yml matrix entry ran a bare `go test ./...` while every other
# entry ran `make test`, and no lint job covered it at all.
#
# Every dependency is public, so these targets need neither the workspace nor
# the committed vendor/ tree, and CI runs them with GOWORK=off.

GO ?= go

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches the
# sibling modules.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

GOLANGCI_LINT_REPO    ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

# Lets CI add -race without the workflow restating the test command, so
# `make test` stays the one definition of what the tests are.
GO_TEST_FLAGS ?=

.PHONY: build test fmt lint license file-license-check

build:
	$(GO) build ./...

test:
	$(GO) test $(GO_TEST_FLAGS) ./...

# golangci-lint applies the formatters enabled in .golangci.yml (gci, gofumpt),
# so there is no separate gofmt step.
fmt:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint fmt ./...

lint:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@v1.1.1 -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" .

# The target name is load-bearing beyond this file: license.yml builds its
# header-check matrix from modules whose Makefile defines `file-license-check:`,
# so a module without one is silently absent rather than reported as unchecked.
# Expects a clean working directory.
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi
