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
GO_TEST_FLAGS ?=
.DEFAULT_GOAL := help

# Image URL to use all building/pushing image targets
IMG ?= management-engine:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: license
license: ## Add the repository's license header to source files.
	$(GO) run github.com/google/addlicense@v1.1.1 -f ../hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" -ignore 'bin/**' -ignore 'dist/**' .

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role,headerFile=hack/boilerplate.yaml.txt crd:headerFile=hack/boilerplate.yaml.txt webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

GOLANGCI_LINT_GOWORK :=
include ../make/golangci.mk

.PHONY: test test-integration lint generate-check
test: manifests generate ## Run unit tests and the controller envtest suite.
	$(GO) test $(GO_TEST_FLAGS) ./...

test-integration: manifests generate ## Test the runtime against a disposable API server.
	$(GO) test $(GO_TEST_FLAGS) -tags=integration ./cmd/... -count=1

lint: ## Run the repository-pinned Go linter.
	@$(GOLANGCI_LINT_RUN) run --build-tags=integration --timeout 10m ./...

generate-check: ## Verify deepcopy code, CRDs, and RBAC without requiring tracked output.
	@set -eu; snapshot=$$(mktemp -d); trap 'rm -rf "$$snapshot"' EXIT; \
		for path in apis config/crd/bases config/rbac; do \
			mkdir -p "$$snapshot/$$(dirname "$$path")"; cp -R "$$path" "$$snapshot/$$path"; \
		done; \
		$(MAKE) generate manifests; \
		for path in apis config/crd/bases config/rbac; do diff -ru "$$snapshot/$$path" "$$path"; done

##@ Build

.PHONY: build build-bin
build: manifests generate fmt vet build-bin ## Build manager binary.

# The generate steps in `build` exceed the Go Build job timeout.
build-bin: ## Compile the manager binary without regenerating.
	$(GO) build ./...
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(MANAGER_LDFLAGS)" -o $(BIN_DIR)/management-engine ./cmd

.PHONY: run
run: build ## Run against the management API selected by MANAGER_ARGS.
	$(BIN_DIR)/management-engine $(MANAGER_ARGS)

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: build-installer
build-installer: manifests generate kustomize ## Render separate runtime and datastore installation bundles.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/runtime.yaml
	"$(KUSTOMIZE)" build config/state > dist/state.yaml

##@ Deployment
HOST_CONTEXT ?=
STATE_CONTEXT ?=

.PHONY: require-host-context require-state-context
require-host-context:
	@test -n "$(HOST_CONTEXT)" || { echo 'Set HOST_CONTEXT to the services cluster'; exit 1; }
require-state-context:
	@test -n "$(STATE_CONTEXT)" || { echo 'Set STATE_CONTEXT to the datastore cluster'; exit 1; }

.PHONY: install install-state
install: install-state ## Install the datastore resources using STATE_CONTEXT.
install-state: require-state-context manifests kustomize ## Install CRDs and identity/RBAC on the datastore cluster.
	"$(KUSTOMIZE)" build config/state | "$(KUBECTL)" --context "$(STATE_CONTEXT)" apply --server-side --field-manager=management-engine-state -f -

.PHONY: deploy
deploy: require-host-context kustomize ## Deploy the runtime and its namespace to HOST_CONTEXT; startup requires its kubeconfig Secret.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" --context "$(HOST_CONTEXT)" apply -f -

.PHONY: undeploy
undeploy: require-host-context kustomize ## Remove runtime resources; preserve state and the host namespace.
	"$(KUSTOMIZE)" build config/base | "$(KUBECTL)" --context "$(HOST_CONTEXT)" delete --ignore-not-found=true -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
KUBEBUILDER_VERSION ?= v4.15.0
CONTROLLER_TOOLS_VERSION ?= v0.20.1

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: kubebuilder
kubebuilder: $(KUBEBUILDER) ## Download the pinned Kubebuilder CLI.
$(KUBEBUILDER): $(LOCALBIN)
	$(call go-install-tool,$(KUBEBUILDER),sigs.k8s.io/kubebuilder/v4,$(KUBEBUILDER_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

.PHONY: chart
chart: build-installer kubebuilder ## Generate a Helm chart from the rendered Kustomize manifests.
	"$(KUSTOMIZE)" build config/helm > dist/chart-input.yaml
	"$(KUBEBUILDER)" edit --plugins=helm/v2-alpha --manifests=./dist/chart-input.yaml --output-dir=.
	# This monorepo owns CI at its root; discard the module-local generated workflow.
	rm -f .github/workflows/test-chart.yml
	@rmdir .github/workflows .github 2>/dev/null || true
	$(MAKE) license

##@ Helm Deployment

## Helm binary to use for deploying the chart
HELM ?= helm
## Namespace to deploy the Helm release
HELM_NAMESPACE ?= management-engine-system
## Name of the Helm release
HELM_RELEASE ?= management-engine
## Path to the Helm chart directory
HELM_CHART_DIR ?= ./chart
## Additional arguments to pass to helm commands
HELM_EXTRA_ARGS ?=

.PHONY: helm-deploy
helm-deploy: require-host-context ## Deploy manager to the K8s cluster via Helm. Specify an image with IMG.
	@repository="$(IMG)"; tag=latest; \
	if [[ "$$repository" != *@* && "$${repository##*/}" == *:* ]]; then \
		tag="$${repository##*:}"; repository="$${repository%:*}"; \
	fi; \
	$(HELM) --kube-context "$(HOST_CONTEXT)" upgrade --install $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--namespace $(HELM_NAMESPACE) \
		--create-namespace \
		--set-string manager.image.repository="$$repository" \
		--set-string manager.image.tag="$$tag" \
		--wait \
		--timeout 5m \
		$(HELM_EXTRA_ARGS)

.PHONY: helm-uninstall
helm-uninstall: require-host-context ## Uninstall the Helm release from the K8s cluster.
	$(HELM) --kube-context "$(HOST_CONTEXT)" uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: chart-check
chart-check: kustomize ## Check generated chart and datastore/runtime separation without a cluster.
	helm lint chart -f config/samples/runtime-values.yaml
	KUSTOMIZE="$(KUSTOMIZE)" $(GO) test -tags=integration ./test -count=1
