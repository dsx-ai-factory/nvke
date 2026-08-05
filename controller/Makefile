# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

WORKER_BIN_NAME			?= cloud-workflow-engine
TEMPORAL_CLI_DOWNLOAD_DIR	?= $(CURDIR)/.go/temporal-cli
CWE_GOFAIL_E2E_WORKER_BINARY	?= $(CURDIR)/.go/bin/cwe-worker-gofail
GOFAIL				?= $(shell go env GOPATH)/bin/gofail

# Image URL to use all building/pushing image targets
IMG_NAME ?= nvcr.io/j7sbcjl3qgta/cloud-workflow-engine
IMG_TAG ?= latest
IMG ?= ${IMG_NAME}:${IMG_TAG}
export NAME := cloud-workflow-engine
BUILD_VERSION ?= "$(shell git describe --tags --dirty --match 'v[0-9]*' || echo "v0.0.0")"

# Get the last git commit hash. If we are on CI depend on what it provides
# else use git describe.
ifdef CI
  GIT_VERSION := $(CI_COMMIT_SHA)
else
  GIT_VERSION ?= "$(shell git describe --always --dirty --match="" --abbrev=0)"
endif

# Include development environment targets
include Makefile.dev

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Build

.PHONY: lint
lint: ## Lint source files.
ifdef CI
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /usr/local/bin v2.7.2; \
	fi
	golangci-lint run --timeout 20m --max-same-issues 0 --max-issues-per-linter 0 ./...
else
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.7.2 run --timeout 5m --max-same-issues 0 --max-issues-per-linter 0 ./...
endif

.PHONY: license
license: ## Ensure license headers at code.
	go run github.com/google/addlicense@v1.1.1 -f ./hack/boilerplate.addlicense.txt -ignore '.local/**' -ignore 'hack/**' -ignore 'profiles/osProfiles/**/testUserData.yaml' -ignore '**/testUserData.yaml' -ignore 'internal/kubeadmconfig/testdata/**' -ignore 'internal/controllers/tenantaccess/testdata/**' -ignore 'forge-vms/ui/vendor/**' -ignore 'forge-vms/test/integration/testutil/testdata/**' -ignore 'forge-vms/.dev/**' -ignore 'deploy/charts/**' .

# Expected to be run on clean working directory.
.PHONY: file-license-check
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . ':!.go/' > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- . ':!.go/'; \
		exit 1; \
	fi

.PHONY: generate
generate:
	go generate ./...
	cd api && go generate ./...

# Verify the committed oapi-codegen output is reproducible with the pinned
# generator (api/oapi-codegen.mod) and in sync with api/openapi.yaml. Catches an
# un-pinned/wrong generator or an openapi.yaml edit that was not regenerated.
# Expected to be run on a clean working directory.
.PHONY: generate-check
generate-check: generate ## Verify generated API code is up to date.
	@if ! git diff --exit-code -- api/models.gen.go api/server.gen.go api/client.gen.go > /dev/null 2>&1; then \
		echo "Generated API code is out of date. Please run 'make generate' locally and commit the changes."; \
		echo "Out-of-date files:"; \
		git diff --name-only -- api/models.gen.go api/server.gen.go api/client.gen.go; \
		exit 1; \
	fi

.PHONY: build
build:
	go build -ldflags "-X github.com/NVIDIA/nke/controller/internal/version.Version=${BUILD_VERSION}" -o ./bin/$(WORKER_BIN_NAME) ./cmd/worker/

.PHONY: fmt
fmt: ## Format code (gofumpt + goimports; matches .golangci.yml formatters).
	go run mvdan.cc/gofumpt@v0.9.2 -w -extra .
	go run golang.org/x/tools/cmd/goimports@v0.36.0 -w -local github.com/NVIDIA/nke/controller,gitlab-master.nvidia.com .

.PHONY: test
test: ## Execute unit tests (excludes integration tests).
	# See: https://docs.gitlab.com/ee/ci/testing/test_coverage_visualization.html#go-example
	go test $(shell go list ./... | grep -v './integration-test') -coverprofile=coverage.txt -covermode count
	go run github.com/boumenot/gocover-cobertura@v1.2.0 < coverage.txt > cobertura.xml

.PHONY: test-integration
test-integration: ## Execute tests against a running CWE deployment.
	@echo "Running integration tests..."
	cd integration-test && go test -timeout=3h

.PHONY: test-health
test-health: ## Run the health-labeled subset of the test/e2e suite against the cluster in KUBECONFIG.
	$(MAKE) test-e2e E2E_TIMEOUT=10m ARGS='--label-filter=health'

.PHONY: test-integration-teleport
test-integration-teleport: ## Execute Teleport client integration tests (requires valid credentials).
	@bash -c '[ -f .env.local ] && set -a && source .env.local && set +a; \
		go test -tags=integration -timeout=2m ./internal/teleport/...'

E2E_DIR := test/e2e
E2E_TIMEOUT ?= 2h
GINKGO_VERSION ?= v2.27.2
# Ginkgo CLI flags, e.g. ARGS='--label-filter="forge-vms && happy-path"'
ARGS ?=

.PHONY: test-e2e
test-e2e: ## Run all NKE e2e tests; optional ARGS forwards ginkgo flags Example: make test-e2e ARGS='--label-filter="forge-vms && happy-path"'
	cd $(E2E_DIR) && go run github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION) run -v --timeout=$(E2E_TIMEOUT) $(ARGS) .

.PHONY: test-e2e-happy-path
test-e2e-happy-path: ## Run CAPD and Forge VMs happy-path e2e tests
	make test-e2e ARGS='--label-filter="happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: test-e2e-capd
test-e2e-capd: ## Run CAPD happy-path e2e tests
	make test-e2e ARGS='--label-filter="capd && happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: test-e2e-capd-local
test-e2e-capd-local: ## Bootstrap Colima and run all CAPD e2e tests with local CWE and node-agent source
	./hack/local-capd-e2e.sh

.PHONY: test-e2e-forge-vms
test-e2e-forge-vms: ## Run Forge VMs happy-path e2e tests
	make test-e2e ARGS='--label-filter="forge-vms && happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: e2e-gofail
e2e-gofail: ## Execute gofail e2e tests against a local Temporal dev server.
	@bash -euo pipefail -c '\
		mkdir -p "$(TEMPORAL_CLI_DOWNLOAD_DIR)" "$$(dirname "$(CWE_GOFAIL_E2E_WORKER_BINARY)")"; \
		go install go.etcd.io/gofail@v0.2.0; \
		"$(GOFAIL)" enable internal/temporal/activities; \
		cleanup() { \
			"$(GOFAIL)" disable internal/temporal/activities; \
		}; \
		trap cleanup EXIT; \
		go build -tags gofail -o "$(CWE_GOFAIL_E2E_WORKER_BINARY)" ./cmd/worker; \
		TEMPORAL_CLI_DOWNLOAD_DIR="$(TEMPORAL_CLI_DOWNLOAD_DIR)" \
			CWE_GOFAIL_E2E_WORKER_BINARY="$(CWE_GOFAIL_E2E_WORKER_BINARY)" \
			go test -v -tags gofail ./e2e/gofail -run TestGofailRunbookWorkerCrashRetriesAfterHeartbeatTimeout -count=1; \
		cleanup; \
		trap - EXIT; \
		test -z "$$(find . -name "*.fail.go" -print)" \
	'


ifdef CI
  # To download go mods from our private gitlab repo we use a netrc file to store the git credentials
  # See: https://www.gnu.org/software/inetutils/manual/html_node/The-_002enetrc-file.html
  # In CI this file is created see recipe below.
  DOCKER_BUILD_SECRET := --secret id=gitcreds,src=../netrc.txt
else
  # For local docker build we pass the netrc value using an ENV variable.
  # So: export GIT_CREDENTIALS_NETRC="machine gitlab-master.nvidia.com login oathtoken password ${GITLAB_TOKEN}"
  # Where GITLAB_TOKEN="oauthtoken:my-personal-access-token"
  DOCKER_BUILD_SECRET := --secret id=gitcreds,env=GIT_CREDENTIALS_NETRC
endif

.PHONY: docker-build-local
docker-build-local: ## Build the docker image for Cloud Workflow Engine
	DOCKER_BUILDKIT=1 docker build -f Dockerfile --platform linux/amd64 \
	  --progress=plain \
	  $(DOCKER_BUILD_SECRET) \
	  --tag $(IMG_NAME):$(GIT_VERSION) \
	  --tag $(IMG_NAME):$(BUILD_VERSION) \
	  --tag $(IMG) \
	  --build-arg BUILD_VERSION=${BUILD_VERSION} \
	  --build-arg GIT_VERSION=${GIT_VERSION} \
	  --build-arg GIT_CREDENTIALS=${GIT_CREDENTIALS} \
	  .

##@ Run

.PHONY: run
run: ## Runs the worker.
	go run cmd/worker/main.go

.PHONY: debug
debug: ## Runs the worker with the delve debugger (attach on :12345).
	dlv debug --headless --listen=:12345 ./cmd/worker/main.go
