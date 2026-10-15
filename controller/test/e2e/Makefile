# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go

GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

include ../../../make/golangci.mk
include ../../../make/coverage.mk

GO_TEST_FLAGS ?=

.PHONY: fmt lint test

fmt: ## Format Go files.
	@$(GOLANGCI_LINT_RUN) fmt ./...

lint: ## Run golangci-lint. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

test: ## Run tests. (run by the merge gate)
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) -skip '^TestE2E$$' . ./internal/...

# What the site lane (site_controller_test.go) stages onto its node. The image
# archives are written outside the clone so they can't be committed.
REPO_ROOT := $(abspath ../../..)
SITE_ARCH ?= amd64
SITE_INPUTS ?= $(HOME)/.cache/nke-e2e/site
SITE_CONTROLLER_VERSION ?=
SITE_PROVISIONED_VERSION ?= v0.1.0-rc.1
# Where the published images are pulled from: GHCR at ghcr.io/dsx-ai-factory/nvke,
# NVCR at nvcr.io/<NGC org>. NVCR needs a docker login with an NGC key, and the
# username is the literal $oauthtoken.
#
# The controller is nvke-controller on NVCR and nke-controller on GHCR
# (image-nvcr.yml publishes both), so the repository follows the registry.
# capi-provisioned carries the same name on both.
#
# The controller name also has to match what qualify-e2e.yml writes into the
# e2e config as site.images.controller.repository. The node's containerd knows
# the image only by the name inside controller.tar, so a disagreement leaves
# the Helm values asking for an image that is not on the node.
SITE_IMAGE_REGISTRY ?= ghcr.io/dsx-ai-factory/nvke
SITE_CONTROLLER_REPOSITORY ?= $(if $(findstring ghcr.io,$(SITE_IMAGE_REGISTRY)),nke-controller,nvke-controller)
SITE_CONTROLLER_IMAGE ?= $(SITE_IMAGE_REGISTRY)/$(SITE_CONTROLLER_REPOSITORY):$(SITE_CONTROLLER_VERSION)
SITE_PROVISIONED_IMAGE ?= $(SITE_IMAGE_REGISTRY)/cluster-api-provider-provisioned:$(SITE_PROVISIONED_VERSION)
SITE_PROFILES_IMAGE ?= nke-e2e-profiles:local

# Build the controller from this checkout instead of pulling SITE_CONTROLLER_IMAGE.
#
# nvkectl is built here by default (see SITE_CLI_BUILD), and it is a client of
# the controller it bootstraps, so the two have to agree on the cluster API. Built
# from one commit they do; a published pair has to be chosen to:
# GHCR carries release tags only and its newest, v0.1.1-rc.8, is 43 commits
# before #850, which removed infra.spireAttestationMethod from the cluster
# API. That pairing fails at "creating the site cluster" with
#   400 Bad Request: infra.spireAttestationMethod is required
# after forty minutes of setup -- see deploy/nico/controller/
# overlay.yaml, which hit the same wall from the other direction.
#
# Set to 0 to qualify a published tag, once one is cut past #850.
SITE_CONTROLLER_BUILD ?= 1

# Build nvkectl from this checkout. Set to 0 to stage the published
# SITE_CLI_REF instead: hack/fetch-nvkectl.sh pulls nvkectl-linux-$(SITE_ARCH)
# from the artifact it names, which needs oras and the registry credentials
# docker login wrote. Either way site.nvkectl in e2e-config.yaml points at
# SITE_NVKECTL.
SITE_CLI_BUILD ?= 1
# The version the staged binary has to report, which is what catches the right
# artifact carrying the wrong build. It does not address the artifact: a run
# that described a commit resolves a version no tag carries.
SITE_CLI_VERSION ?=
# Read by the recipe from the environment, never expanded into it: a value
# carrying a quote would otherwise end the argument and run as shell.
export SITE_CLI_VERSION
# The artifact SITE_CLI_BUILD=0 stages, whole. nvkectl is published per alias
# and platform, so the caller is what knows whether the alias is a version or a
# commit.
SITE_CLI_REF ?=
export SITE_CLI_REF
ifeq ($(SITE_CLI_BUILD),1)
SITE_NVKECTL = $(REPO_ROOT)/cli/dist/nvkectl-linux-$(SITE_ARCH)
else
SITE_NVKECTL = $(SITE_INPUTS)/nvkectl/nvkectl-linux-$(SITE_ARCH)
endif

# Passed through as GIT_VERSION, which the controller Makefile takes from
# CI_COMMIT_SHA whenever CI is set. That is GitLab's variable: under GitHub
# Actions CI is set and CI_COMMIT_SHA is not, so GIT_VERSION comes out empty
# and the build dies on `invalid tag "nvcr.io/j7sbcjl3qgta/nvke-controller:"`.
# image-nvcr.yml passes GITHUB_SHA for the same reason.
#
# BUILD_VERSION is a different thing: GIT_VERSION only names the image, while
# BUILD_VERSION is what the ldflags bake into version.Version, so it is what
# the running controller reports and what the suite compares against
# -e2e.versions=controller= with. It is SITE_CONTROLLER_VERSION, the same
# value the image is tagged with and the suite is told to expect, so the three
# cannot disagree. Left to itself the controller Makefile derives it
# from `git describe --always`, which abbreviates to seven characters on a
# checkout with no controller/v* tag in reach -- so run 36045936723 deployed
# the stack successfully and then failed on
#   CWE reports version "cd95099", want "cd9509990c8f"
# the same twelve-character commit, written two ways.
SITE_CONTROLLER_COMMIT ?= $(shell git -C "$(REPO_ROOT)" rev-parse --short=12 HEAD)

.PHONY: site-inputs
site-inputs: ## Build the nvkectl and image archives the site lane stages. Needs SITE_CONTROLLER_VERSION and a docker login to SITE_IMAGE_REGISTRY.
	@test -n "$(SITE_CONTROLLER_VERSION)" || { echo "SITE_CONTROLLER_VERSION is required, e.g. v0.1.1-rc.8" >&2; exit 1; }
	@mkdir -p "$(SITE_INPUTS)"
ifeq ($(SITE_CLI_BUILD),1)
	$(MAKE) -C "$(REPO_ROOT)/cli" build-cross NVKECTL_PLATFORMS=linux/$(SITE_ARCH)
else
	@test -n "$${SITE_CLI_REF}" || { echo "SITE_CLI_BUILD=0 stages a published nvkectl and needs SITE_CLI_REF, e.g. nvcr.io/<org>/nvkectl:v0.1.0-linux-amd64" >&2; exit 1; }
	"$(CURDIR)/hack/fetch-nvkectl.sh" "$${SITE_CLI_VERSION}" "$(SITE_ARCH)" "$(dir $(SITE_NVKECTL))"
endif
ifeq ($(SITE_CONTROLLER_BUILD),1)
	@test "$(SITE_ARCH)" = amd64 || { echo "SITE_CONTROLLER_BUILD=1 builds linux/amd64 only (docker build --load takes one platform); set SITE_CONTROLLER_BUILD=0 for $(SITE_ARCH)" >&2; exit 1; }
	$(MAKE) -C "$(REPO_ROOT)/controller" docker-build-local IMG=$(SITE_CONTROLLER_IMAGE) GIT_VERSION=$(SITE_CONTROLLER_COMMIT) BUILD_VERSION=$(SITE_CONTROLLER_VERSION)
else
	docker pull --platform linux/$(SITE_ARCH) $(SITE_CONTROLLER_IMAGE)
endif
	docker pull --platform linux/$(SITE_ARCH) $(SITE_PROVISIONED_IMAGE)
	docker buildx build --platform linux/$(SITE_ARCH) --load -t $(SITE_PROFILES_IMAGE) ../fixtures
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/controller.tar" $(SITE_CONTROLLER_IMAGE)
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/capi-provisioned.tar" $(SITE_PROVISIONED_IMAGE)
	docker save --platform linux/$(SITE_ARCH) -o "$(SITE_INPUTS)/profiles.tar" $(SITE_PROFILES_IMAGE)
	@echo "site inputs in $(SITE_INPUTS); see the site: block in e2e-config-example.yaml"
