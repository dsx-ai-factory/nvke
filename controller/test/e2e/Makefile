# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go

GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

include ../../../make/golangci.mk

GO_TEST_FLAGS ?=

.PHONY: fmt lint test

fmt: ## Format Go files.
	@$(GOLANGCI_LINT_RUN) fmt ./...

lint: ## Run golangci-lint. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

test: ## Run tests. (run by the merge gate)
	$(GO) test $(GO_TEST_FLAGS) -skip '^TestE2E$$' . ./internal/...

# What the site lane (site_controller_test.go) stages onto its node. The image
# archives are written outside the clone so they can't be committed.
REPO_ROOT := $(abspath ../../..)
SITE_ARCH ?= amd64
SITE_INPUTS ?= $(HOME)/.cache/nke-e2e/site
SITE_CONTROLLER_VERSION ?=
SITE_CONTROLLER_IMAGE ?= ghcr.io/dsx-ai-factory/nke/nke-controller:$(SITE_CONTROLLER_VERSION)
SITE_PROVISIONED_IMAGE ?= ghcr.io/dsx-ai-factory/nke/cluster-api-provider-provisioned:v0.1.0-rc.1
SITE_PROFILES_IMAGE ?= nke-e2e-profiles:local
SITE_DEPLOY_PATH_FETCH ?= refs/pull/812/head

.PHONY: site-inputs
site-inputs: ## Build the nkectl and image archives the site lane stages. Needs SITE_CONTROLLER_VERSION and a docker login to ghcr.io.
	@test -n "$(SITE_CONTROLLER_VERSION)" || { echo "SITE_CONTROLLER_VERSION is required, e.g. v0.1.1-rc.8" >&2; exit 1; }
	@mkdir -p "$(SITE_INPUTS)"
	$(MAKE) -C "$(REPO_ROOT)/cli" build-cross NKECTL_PLATFORMS=linux/$(SITE_ARCH)
	git -C "$(REPO_ROOT)" fetch --no-tags origin $(SITE_DEPLOY_PATH_FETCH)
	docker pull --platform linux/$(SITE_ARCH) $(SITE_CONTROLLER_IMAGE)
	docker pull --platform linux/$(SITE_ARCH) $(SITE_PROVISIONED_IMAGE)
	docker buildx build --platform linux/$(SITE_ARCH) --load -t $(SITE_PROFILES_IMAGE) ../fixtures
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/controller.tar" $(SITE_CONTROLLER_IMAGE)
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/capi-provisioned.tar" $(SITE_PROVISIONED_IMAGE)
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/profiles.tar" $(SITE_PROFILES_IMAGE)
	@echo "site inputs in $(SITE_INPUTS); see the site: block in e2e-config-example.yaml"
