# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOTOOLCHAIN ?= go$(shell awk '/^go /{print $$2; exit}' $(CURDIR)/../go.work)
export GOTOOLCHAIN
BIN_DIR ?= $(CURDIR)/bin
MANAGER_ARGS ?=
VERSION ?= $(shell git describe --always --dirty --match 'management-engine/v[0-9]*' 2>/dev/null || echo dev)
RELEASE_LDFLAGS ?= -s -w -buildid=
MANAGER_LDFLAGS := -X 'main.version=$(VERSION)' $(RELEASE_LDFLAGS)
CAPI_CRD_DIR ?=
GO_TEST_FLAGS ?=
LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION := v0.20.1
GOBIN ?= $(shell $(GO) env GOPATH)/bin
GOLANGCI_LINT_GOWORK :=
include ../make/golangci.mk

.DEFAULT_GOAL := help
.PHONY: help build run test test-integration lint fmt controller-gen generate manifests generate-check

help: ## List management engine development targets.
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the manager and API packages.
	$(GO) build ./...
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(MANAGER_LDFLAGS)" -o $(BIN_DIR)/management-engine ./cmd/manager

run: build ## Run against the management API selected by MANAGER_ARGS.
	$(BIN_DIR)/management-engine $(MANAGER_ARGS)

test: ## Run API and runtime unit tests.
	$(GO) test $(GO_TEST_FLAGS) ./...

test-integration: ## Test against a disposable API server; fetch pinned test assets if absent.
	@set -eu; capi_crd_dir="$(CAPI_CRD_DIR)"; \
		if [ -z "$$capi_crd_dir" ]; then \
			GOWORK=off $(GO) mod download sigs.k8s.io/cluster-api; \
			capi_crd_dir="$$(GOWORK=off $(GO) list -m -f '{{.Dir}}' sigs.k8s.io/cluster-api)/config/crd/bases"; \
		fi; \
		CAPI_CRD_DIR="$$capi_crd_dir" $(GO) test $(GO_TEST_FLAGS) -tags=integration ./pkg/manager/... -count=1

lint: ## Run the repository-pinned Go linter.
	@$(GOLANGCI_LINT_RUN) run --build-tags=integration --timeout 10m ./...

fmt: ## Format Go sources and tests.
	find apis cmd pkg -name '*.go' ! -name 'zz_generated.*' -exec gofmt -w {} +

controller-gen: ## Install the pinned CRD and deepcopy generator.
	@if [ "$$($(CONTROLLER_GEN) --version 2>/dev/null)" != "Version: $(CONTROLLER_TOOLS_VERSION)" ]; then \
		mkdir -p "$(LOCALBIN)"; \
		GOBIN="$(LOCALBIN)" $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Generate deepcopy code and CRD manifests from Go types.
	"$(CONTROLLER_GEN)" object:headerFile=apis/boilerplate.go.txt paths=./apis/v1
	"$(CONTROLLER_GEN)" crd:crdVersions=v1,headerFile=apis/boilerplate.yaml.txt paths=./apis/v1 output:crd:artifacts:config=apis/crds

manifests: generate ## Generate the installable internal CRDs.

generate-check: ## Verify generation without relying on Git tracking new files.
	@set -eu; snapshot=$$(mktemp -d); trap 'rm -rf "$$snapshot"' EXIT; \
		cp -R apis "$$snapshot/apis"; \
		$(MAKE) generate; \
		diff -ru "$$snapshot/apis" apis
