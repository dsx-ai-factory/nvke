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

.PHONY: manifests-state manifests
manifests-state: controller-gen generate ## Generate datastore CRDs and RBAC.
	"$(CONTROLLER_GEN)" crd:crdVersions=v1,headerFile=hack/boilerplate.yaml.txt paths=./apis/... output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role,headerFile=hack/boilerplate.yaml.txt paths=./controllers/... output:rbac:artifacts:config=config/rbac

manifests: manifests-state kustomize kubebuilder ## Generate state, the runtime bundle, and Helm chart.
	mkdir -p dist
	"$(KUSTOMIZE)" build config/default > dist/install.yaml
	"$(KUBEBUILDER)" edit --plugins=helm/v2-alpha --manifests=./dist/install.yaml --output-dir=.
	# Kubebuilder v4.15.0 over-indents extra volumes beside existing Kustomize lists.
	# Keep this correction in generation until the Helm plugin fixes list indentation.
	sed -e '/toYaml .Values.manager.extraVolumeMounts/s/nindent 10/nindent 8/' \
		-e '/toYaml .Values.manager.extraVolumes/s/nindent 8/nindent 6/' \
		chart/templates/manager/manager.yaml > dist/manager-chart.yaml
	mv dist/manager-chart.yaml chart/templates/manager/manager.yaml
	rm -f .github/workflows/test-chart.yml
	@rmdir .github/workflows .github 2>/dev/null || true
	$(GO) run github.com/google/addlicense@v1.1.1 -f ../hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" chart

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

TEST_PROCS ?= 5

.PHONY: test
test: test-api test-controller ## Run API, client, startup, and controller tests.

.PHONY: test-unit
test-unit: fmt vet ## Run client and startup unit tests.
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) -race -coverprofile=$(call coverprofile,unit) -covermode=$(GO_COVER_MODE) ./internal/... ./cmd/...

.PHONY: test-api
# Separate test invocations need distinct profiles to avoid overwriting coverage.
# hack/coverage.sh treats every coverage/management-engine*.out as this
# module's, and the suffixes come from make/coverage.mk.
test-api: manifests generate fmt vet ## Run API tests.
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) -race -coverprofile=$(call coverprofile,apis) -covermode=$(GO_COVER_MODE) ./apis/...

.PHONY: test-controller
test-controller: test-unit manifests generate fmt vet setup-envtest ginkgo ## Run controller envtest fixtures.
	@$(COVERAGE_MKDIR)
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" "$(GINKGO)" --race --procs=$(TEST_PROCS) --cover --covermode=$(GO_COVER_MODE) --output-dir=$(COVERAGE_DIR) --coverprofile=$(call coverprofile_name,controllers) ./controllers/...

.PHONY: test-update
test-update: export TESTUTIL_UPDATE_EXPECTED=true
test-update: test-controller ## Run controller tests and update expected fixture goldens.

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

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
	$(CONTAINER_TOOL) build -f Dockerfile --build-arg VERSION=$(VERSION) -t ${IMG} ..

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment
HOST_CONTEXT ?=
STATE_CONTEXT ?=

.PHONY: require-host-context require-state-context
require-host-context:
	@test -n "$(HOST_CONTEXT)" || { echo 'Set HOST_CONTEXT to the services cluster'; exit 1; }
require-state-context:
	@test -n "$(STATE_CONTEXT)" || { echo 'Set STATE_CONTEXT to the datastore cluster'; exit 1; }

DEV_NAME ?= nvke
KIND_NODE_IMAGE ?= kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5
TILT_PORT ?= 10351
export DEV_NAME KIND_NODE_IMAGE TILT_PORT

.PHONY: dev-up dev dev-sync dev-refresh dev-down test-install
dev-up: ## Create or reuse the named development environment and prepare credentials.
	bash hack/dev.sh up

dev: ## Check Tilt, prepare or reuse the development clusters, then start Tilt.
	bash hack/dev.sh check
	$(MAKE) dev-up
	bash hack/dev.sh tilt

dev-sync: ## Update datastore CRDs and RBAC without refreshing credentials.
	bash hack/dev.sh sync

dev-refresh: ## Renew datastore credentials and report their expiration.
	bash hack/dev.sh refresh

dev-down: ## Delete this environment's clusters and local credentials; stop Tilt first.
	bash hack/dev.sh down

test-install: ## Test the runtime in the prepared environment; run before Tilt.
	IMG="$(IMG)" bash hack/dev.sh test

.PHONY: dev-cwe dev-cwe-up dev-cwe-down test-cwe
dev-cwe dev-cwe-up dev-cwe-down test-cwe: DEV_NAME = nvke-cwe
dev-cwe dev-cwe-up dev-cwe-down test-cwe: TILT_PORT = 10353
dev-cwe: ## Start CWE and management-engine in one Tilt dashboard on dedicated Kind clusters.
	CAPI_VERSION="$(CAPI_VERSION)" bash hack/dev.sh cwe-up
	bash hack/dev.sh cwe-tilt

dev-cwe-up: ## Prepare the dedicated CWE integration environment without starting Tilt.
	CAPI_VERSION="$(CAPI_VERSION)" bash hack/dev.sh cwe-up

test-cwe: ## Run the Ginkgo integration suite against the running combined environment.
	bash hack/dev.sh cwe-test

dev-cwe-down: ## Remove the dedicated CWE environment and credentials; stop Tilt first.
	bash hack/dev.sh cwe-down

.PHONY: install install-state
install: install-state ## Install the datastore resources using STATE_CONTEXT.
install-state: require-state-context manifests-state kustomize ## Install CRDs and identity/RBAC on the datastore cluster.
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
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GINKGO ?= $(LOCALBIN)/ginkgo
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
KUBEBUILDER_VERSION ?= v4.15.0
GINKGO_VERSION ?= $(call gomodver,github.com/onsi/ginkgo/v2)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')
GOLANGCI_LINT_VERSION ?= v2.14.0

# CONTROLLER_TOOLS_VERSION comes from make/tool-versions.mk, which owns it for
# this module and for cluster-api-provider-provisioned/. Included after the
# pins above so the ones derived from go.mod keep their values.
include ../make/tool-versions.mk
include ../make/coverage.mk

.PHONY: kind
kind: $(LOCALBIN) ## Install the pinned Kind CLI for the development environment.
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

dev-up dev dev-sync dev-refresh dev-down test-install dev-cwe dev-cwe-up dev-cwe-down test-cwe: kind
dev-up dev dev-sync dev-refresh dev-down test-install dev-cwe dev-cwe-up dev-cwe-down test-cwe: export PATH := $(LOCALBIN):$(PATH)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Download the Ginkgo CLI locally if necessary.
$(GINKGO): $(LOCALBIN)
	$(call go-install-tool,$(GINKGO),github.com/onsi/ginkgo/v2/ginkgo,$(GINKGO_VERSION))

.PHONY: kubebuilder
kubebuilder: $(KUBEBUILDER) ## Download the pinned Kubebuilder CLI.
$(KUBEBUILDER): $(LOCALBIN)
	$(call go-install-tool,$(KUBEBUILDER),sigs.k8s.io/kubebuilder/v4,$(KUBEBUILDER_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN) .custom-gcl.yml
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

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

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

.PHONY: chart
chart: manifests ## Generate the Helm chart from the Kustomize source manifests.

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
chart-check: manifests ## Lint and render the Helm chart without a cluster.
	$(HELM) lint chart -f config/samples/runtime-values.yaml
	$(HELM) template management-engine chart --namespace management-engine-system -f config/samples/runtime-values.yaml > dist/chart-rendered.yaml
	$(HELM) lint chart -f config/samples/runtime-values.yaml -f config/samples/cwe-values.yaml
	$(HELM) template management-engine chart --namespace management-engine-system -f config/samples/runtime-values.yaml -f config/samples/cwe-values.yaml > dist/chart-cwe-rendered.yaml
