# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go
GO_TEST_FLAGS ?=
LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION := v0.20.1
GOBIN ?= $(shell $(GO) env GOPATH)/bin
GOLANGCI_LINT_GOWORK :=
include ../make/golangci.mk

.DEFAULT_GOAL := help
.PHONY: help build test lint fmt controller-gen generate manifests generate-check

help: ## List the API development targets.
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Compile the internal API package.
	$(GO) build ./...

test: ## Test resource serialization and Kubernetes schema admission.
	$(GO) test $(GO_TEST_FLAGS) ./...

lint: ## Run the repository-pinned Go linter.
	@$(GOLANGCI_LINT_RUN) run --timeout 10m ./...

fmt: ## Format the handwritten API sources and tests.
	gofmt -w api/v1alpha1/*.go

controller-gen: ## Install the pinned CRD and deepcopy generator.
	@if [ "$$($(CONTROLLER_GEN) --version 2>/dev/null)" != "Version: $(CONTROLLER_TOOLS_VERSION)" ]; then \
		mkdir -p "$(LOCALBIN)"; \
		GOBIN="$(LOCALBIN)" $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Generate deepcopy code and CRD manifests from Go types.
	"$(CONTROLLER_GEN)" object:headerFile=api/boilerplate.go.txt paths=./api/v1alpha1
	"$(CONTROLLER_GEN)" crd:crdVersions=v1,headerFile=api/boilerplate.yaml.txt paths=./api/v1alpha1 output:crd:artifacts:config=api/crds

manifests: generate ## Generate the installable internal CRDs.

generate-check: ## Verify generation without relying on Git tracking new files.
	@set -eu; snapshot=$$(mktemp -d); trap 'rm -rf "$$snapshot"' EXIT; \
		cp -R api "$$snapshot/api"; \
		$(MAKE) generate; \
		diff -ru "$$snapshot/api" api
