# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_ROOT ?= $(shell git rev-parse --show-toplevel)
LOCALBIN ?= $(CURDIR)/bin

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches the other
# modules so the shared golangci.mk macro installs and finds the same binary.
# Module-local tools (controller-gen) use LOCALBIN directly instead, via an
# explicit GOBIN= prefix on their own `go install`.
GOBIN ?= $(shell go env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell go env GOPATH)/bin
endif

# The CRDs generated from v1 are the canonical files of the nvke-crds chart.
CRD_DIR ?= $(REPO_ROOT)/controller/deploy/charts/nvke-crds/bases
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
# CONTROLLER_TOOLS_VERSION comes from make/tool-versions.mk, which owns it for
# every module that runs controller-gen.
include ../../make/tool-versions.mk

include ../../make/golangci.mk
include ../../make/coverage.mk

.DEFAULT_GOAL := help
.PHONY: help fmt test test-chart-crds lint controller-gen generate manifests generate-check

help: ## Show the available targets.
	@echo "Usage: make <target>"
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

fmt: ## Format the source with golangci-lint's formatters.
	@$(GOLANGCI_LINT_RUN) fmt ./...

test: ## Run unit tests.
	@$(COVERAGE_MKDIR)
	go test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...

test-chart-crds: ## Render the nvke-crds chart and compare it with the generated CRDs (requires helm).
	@command -v helm >/dev/null || { echo "helm is required: the render test skips without it"; exit 1; }
	go test $(GO_TEST_FLAGS) ./v1 -run TestNVKECRDsRendering -count=1

lint: ## Run golangci-lint.
	@$(GOLANGCI_LINT_RUN) run --timeout 10m ./...

controller-gen: ## Install the pinned controller-gen binary.
	@if [ "$$($(CONTROLLER_GEN) --version 2>/dev/null)" != "Version: $(CONTROLLER_TOOLS_VERSION)" ]; then \
		mkdir -p "$(LOCALBIN)"; \
		GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Generate DeepCopy implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./v1/..."

manifests: controller-gen ## Generate the CRDs into the shared nvke-crds chart.
	"$(CONTROLLER_GEN)" \
		crd:crdVersions=v1,headerFile="hack/boilerplate.yaml.txt" \
		paths="./v1/..." \
		output:crd:artifacts:config="$(CRD_DIR)"

generate-check: generate manifests ## Verify generated code and CRDs are current.
	@git diff --exit-code -- v1/zz_generated.deepcopy.go "$(CRD_DIR)"
	@# git diff says nothing about a path git is not tracking, so a newly
	@# generated file would otherwise pass while never being committed.
	@untracked=$$(git ls-files --others --exclude-standard -- v1/zz_generated.deepcopy.go "$(CRD_DIR)"); \
	if [ -n "$$untracked" ]; then \
		echo "untracked generated files; commit them:"; echo "$$untracked"; exit 1; \
	fi
