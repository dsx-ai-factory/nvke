# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# strcase (vendored via teleport/api) asserts its Unicode tables match the
# stdlib's, so a toolchain newer than go.work's `go` directive panics at init.
# Pin every go invocation to it; an explicit GOTOOLCHAIN, as CI sets, still wins.
GOTOOLCHAIN ?= go$(shell awk '/^go /{print $$2; exit}' $(CURDIR)/../go.work)
export GOTOOLCHAIN

WORKER_BIN_NAME			?= controller
TEMPORAL_CLI_DOWNLOAD_DIR	?= $(CURDIR)/.go/temporal-cli
CWE_GOFAIL_E2E_WORKER_BINARY	?= $(CURDIR)/.go/bin/cwe-worker-gofail
GOFAIL				?= $(shell go env GOPATH)/bin/gofail
SETUP_ENVTEST			?= $(shell go env GOPATH)/bin/setup-envtest
# Explicit store, so CI can cache a known path instead of guessing setup-envtest's
# platform-dependent default.
ENVTEST_BIN_DIR			?= $(CURDIR)/.go/envtest
CWE_GOFAIL_E2E_CLUSTER_CRDS	?= $(shell go env GOMODCACHE)/sigs.k8s.io/cluster-api@$(shell go list -m -f '{{.Version}}' sigs.k8s.io/cluster-api)/config/crd/bases
CWE_GOFAIL_E2E_CAPNICO_CRDS	?= $(shell go env GOMODCACHE)/github.com/dsx-ai-factory/cluster-api-provider-nico@$(shell go list -m -f '{{.Version}}' github.com/dsx-ai-factory/cluster-api-provider-nico)/config/crd/bases

# The licence header template lives once, at the repository root.
REPO_ROOT ?= $(shell git rev-parse --show-toplevel)

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches agent/Makefile.
GOBIN ?= $(shell go env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell go env GOPATH)/bin
endif

# golangci-lint pin and bootstrap, shared with every other module.
#
# Keep the workspace on so lint resolves the same module graph as builds.
GOLANGCI_LINT_GOWORK :=
include ../make/tool-versions.mk
include ../make/golangci.mk

# Image URL to use all building/pushing image targets
IMG_NAME ?= nvcr.io/j7sbcjl3qgta/nke-controller
IMG_TAG ?= latest
IMG ?= ${IMG_NAME}:${IMG_TAG}
# Falls back to the commit before v0.0.0. A CI checkout is shallow and carries
# no tags, so both describes find nothing there and every binary CI built
# reported v0.0.0. Same steps as cmd_version in the root Makefile.
_BUILD_VERSION_RAW := $(shell git describe --tags --dirty --match 'controller/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || git describe --always --dirty 2>/dev/null || echo "v0.0.0")
BUILD_VERSION ?= $(patsubst controller/%,%,$(_BUILD_VERSION_RAW))

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
lint: ## Lint source files. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 20m --concurrency 4 --max-same-issues 0 --max-issues-per-linter 0 ./...

##@ MachineImage and releases

# OS profiles are being replaced by MachineImage, ClusterRelease and
# NodePoolRelease custom resources, generated from an OS profile tree (Argo CD
# applies the fleet's, see nke-site-deploy/cwe-profiles/releases). The generator
# validates each manifest against the CRDs before it writes anything.

.PHONY: fixture-releases
fixture-releases: ## Regenerate test/fixtures/releases from test/fixtures/profiles.
	go run ./hack/osprofiles-to-crs --profiles test/fixtures/profiles --output test/fixtures/releases --license Apache-2.0 --gitops=false

.PHONY: license
license: ## Ensure license headers at code.
	go run github.com/google/addlicense@$(ADDLICENSE_VERSION) -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" -ignore '.local/**' -ignore 'hack/**' -ignore 'test/fixtures/profiles/**' -ignore 'test/fixtures/capnico-crds/**' -ignore '**/testUserData.yaml' -ignore 'internal/kubeadmconfig/testdata/**' -ignore 'internal/controllers/tenantaccess/testdata/**' -ignore 'nico-sim/ui/vendor/**' -ignore 'nico-sim/test/integration/testutil/testdata/**' -ignore 'nico-sim/.dev/**' -ignore 'deploy/charts/**' .

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
generate: ## Run every generator in this module and in api/.
	go generate ./...
	$(MAKE) generate-api

# Split out from `generate` so it can run without the rest of the module.
# Every //go:generate directive in the controller currently lives in
# api/generate.go, but `go generate ./...` above still has to load this
# module's package graph -- which may need public module downloads. The api
# module has neither problem: it is self-contained, so this target is the half
# that CI can run.
# GOWORK=off, so the self-contained api module is resolved on its own, and
# GOTOOLCHAIN pinned to the Go version api/go.mod declares, which is the version
# generate-check.yml installs. The embedded copy of openapi.yaml in server.gen.go
# is gzip-compressed and compress/flate output differs between Go releases, so a
# newer toolchain (go.work asks for one) regenerates a different blob and the
# gate rejects it.
.PHONY: generate-api
generate-api: ## Regenerate the OpenAPI client, server and models.
	cd api && GOWORK=off GOTOOLCHAIN=go$$(awk '/^go /{print $$2; exit}' go.mod) go generate ./...

# Verify the committed oapi-codegen output is reproducible with the pinned
# generator (api/oapi-codegen.mod) and in sync with api/openapi.yaml. Catches an
# un-pinned/wrong generator or an openapi.yaml edit that was not regenerated.
# Expected to be run on a clean working directory.
.PHONY: generate-check
generate-check: generate-api ## Verify generated API code is up to date. (run by the merge gate)
	@if ! git diff --exit-code -- api/models.gen.go api/server.gen.go api/client.gen.go > /dev/null 2>&1; then \
		echo "Generated API code is out of date. Please run 'make generate' locally and commit the changes."; \
		echo "Out-of-date files:"; \
		git diff --name-only -- api/models.gen.go api/server.gen.go api/client.gen.go; \
		exit 1; \
	fi
	@# git diff says nothing about a path git is not tracking, so a newly
	@# generated file would otherwise pass while never being committed. Same
	@# guard as Makefile's licences-check.
	@untracked=$$(git ls-files --others --exclude-standard -- api/models.gen.go api/server.gen.go api/client.gen.go); \
	 [ -z "$$untracked" ] \
		|| { echo "ERROR: generated API files are not tracked. Run 'make generate' and commit them:"; printf '%s\n' "$$untracked" | sed 's/^/    /'; exit 1; }

# The layer above generate-check. That target proves the committed Go matches
# api/openapi.yaml; it says nothing about whether the specification change is
# safe for an existing client. A removed field or a newly required property
# regenerates perfectly and breaks every one of them.
#
# The baseline is the newest published controller/api version tag, not the merge
# base: the tag is what a consumer can pin, so it is the only ref against which
# "backward compatible" means anything to them. Override it with
# API_COMPAT_BASELINE=<ref> to compare against something else.
#
# It needs tags. A shallow CI checkout has none, and the target fails rather
# than silently comparing against nothing -- see hack/openapi-compat/README.md.
API_COMPAT_BASELINE ?=
API_COMPAT_REPORT   ?=

.PHONY: api-compat-check
api-compat-check: ## Verify api/openapi.yaml is backward compatible with the newest published controller/api tag. (run by the merge gate)
	go run ./hack/openapi-compat \
		--differ-version $(OPENAPI_CHANGES_VERSION) \
		$(if $(API_COMPAT_BASELINE),--baseline $(API_COMPAT_BASELINE),) \
		$(if $(API_COMPAT_REPORT),--report-out $(API_COMPAT_REPORT),)

RELEASE_LDFLAGS ?= -s -w -buildid= -extldflags -static

# Read one variable, so a caller that needs the same value this Makefile computes
# does not recompute it. `make print-BUILD_VERSION` is the one the E2E uses.
.PHONY: print-%
print-%:
	@echo '$($*)'

.PHONY: build
# -buildvcs=false because the version comes from the -X ldflag below.
build: ## Build controller into bin/.
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-X github.com/dsx-ai-factory/nke/controller/internal/version.Version=${BUILD_VERSION} $(RELEASE_LDFLAGS)" -o ./bin/$(WORKER_BIN_NAME) ./cmd/worker/

.PHONY: fmt
fmt: ## Format code (gofumpt + goimports; matches .golangci.yml formatters).
	go run mvdan.cc/gofumpt@$(GOFUMPT_VERSION) -w -extra .
	go run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -w -local github.com/dsx-ai-factory/nke/controller .

# GO_TEST_FLAGS lets CI add -race without the workflow having to restate the
# test command, so `make test` stays the one definition of what "the tests" are.
GO_TEST_FLAGS ?=

# atomic, not count. Go rejects `-covermode count` outright when -race is on:
# "-covermode must be \"atomic\", not \"count\", when -race is enabled". atomic
# is valid with or without the race detector, so one setting covers both and
# there is no conditional to get wrong.
GO_COVER_MODE ?= atomic

.PHONY: test
test: ## Execute unit tests (excludes integration tests). (run by the merge gate)
	# coverage.txt is kept: `go tool cover` reads it, and producing it is free.
	# The cobertura.xml conversion that used to follow is gone. It existed for
	# GitLab's coverage visualisation, and that pipeline is gone -- no
	# .gitlab-ci.yml, no workflow reading either file, and both are gitignored.
	# It cost about 9 seconds of download and compile per run on a cold cache,
	# and pulled golang.org/x/tools, 15 MB, to convert one file to XML.
	# nico-sim already produces a coverprofile without it.
	#
	# integration-test/ is behind the `integration_test` build tag, so
	# `go list ./...` no longer needs filtering here. The old grep matched
	# by accident anyway: `.` is a regex wildcard, not a path prefix.
	go test $(GO_TEST_FLAGS) ./... -coverprofile=coverage.txt -covermode $(GO_COVER_MODE)

# The `integration` build tag covers two packages here, and neither needs a
# service to be running. internal/agent binds a loopback socket and mints its
# own certificates; internal/teleport skips every tagged test unless
# TELEPORT_ENABLED is set. Kept out of `test` above so a developer with
# TELEPORT_ENABLED exported does not reach a live Teleport from a unit run.
.PHONY: test-integration-local
test-integration-local: ## Execute the `integration`-tagged tests that need no external service. (run by the merge gate)
	go test $(GO_TEST_FLAGS) -tags=integration -count=1 ./internal/agent/... ./internal/teleport/...

.PHONY: test-integration
test-integration: ## Execute tests against a running CWE deployment.
	@echo "Running integration tests..."
	cd integration-test && go test -tags integration_test -timeout=3h

.PHONY: test-health
test-health: ## Run the health-labeled subset of the test/e2e suite against the cluster in KUBECONFIG.
	$(MAKE) test-e2e E2E_TIMEOUT=10m ARGS='--label-filter=health'

# The authentication specs. Both need deploy/k8s/nke-auth-dev.yaml applied and
# an enforcing NKE; the chart binds the JWKS read permission.
#
# The SPIRE spec carries both labels, so test-authn runs it too; it skips itself
# with a message naming deploy/k8s/nke-spire-client.yaml when that scaffolding
# is absent. test-authn-spire is the way to run only that one.
#
# The permissive specs have no target: they need an NKE deployed with
# PERMISSIVE_MODE=true, which is a different deployment. Run them against one
# with `make test-e2e ARGS='--label-filter=authn-permissive'`.
.PHONY: test-authn
test-authn: ## Run the authn-labeled subset of the test/e2e suite against the cluster in KUBECONFIG.
	$(MAKE) test-e2e E2E_TIMEOUT=20m ARGS='--label-filter=authn'

.PHONY: test-authn-spire
test-authn-spire: ## Run the SPIRE JWT-SVID authentication spec against the cluster in KUBECONFIG.
	$(MAKE) test-e2e E2E_TIMEOUT=20m ARGS='--label-filter=authn-spire'

.PHONY: test-integration-teleport
test-integration-teleport: ## Execute Teleport client integration tests (requires valid credentials).
	@bash -c '[ -f .env.local ] && set -a && source .env.local && set +a; \
		go test -tags=integration -timeout=2m ./internal/teleport/...'

E2E_DIR := test/e2e
E2E_TIMEOUT ?= 2h
# Ginkgo CLI flags, e.g. ARGS='--label-filter="nico-vms && happy-path"'
ARGS ?=

.PHONY: test-e2e
test-e2e: ## Run all NKE e2e tests; optional ARGS forwards ginkgo flags Example: make test-e2e ARGS='--label-filter="nico-vms && happy-path"'
	cd $(E2E_DIR) && go run github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION) run -v --timeout=$(E2E_TIMEOUT) $(ARGS) .

.PHONY: test-e2e-core-capd test-e2e-core-nico-containers test-e2e-happy-path
test-e2e-core-capd: ## Run the bounded CAPD create, readiness, and delete lifecycle.
	make test-e2e ARGS='--fail-on-empty --label-filter="core-docker"'

test-e2e-core-nico-containers: ## Run the bounded NICo containers create, readiness, and delete lifecycle.
	make test-e2e ARGS='--fail-on-empty --label-filter="core-nico"'

test-e2e-happy-path: ## Run CAPD and NICo VMs happy-path e2e tests
	make test-e2e ARGS='--label-filter="happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: test-e2e-capd
test-e2e-capd: ## Run CAPD happy-path e2e tests
	make test-e2e ARGS='--label-filter="capd && happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: test-e2e-capd-local
test-e2e-capd-local: ## Run all CAPD e2e tests with local CWE and agent source. macOS bootstraps Colima; Linux uses the host Docker daemon.
	./hack/local-backend-e2e.sh

.PHONY: test-e2e-nico-vms
test-e2e-nico-vms: ## Run NICo VMs happy-path e2e tests
	make test-e2e ARGS='--label-filter="nico-vms && happy-path && (lifecycle || readiness) && !scale && !upgrade && !reboot"'

.PHONY: test-e2e-nico-vms-os-profiles
test-e2e-nico-vms-os-profiles: ## Run the opt-in NICo VMs OS profile provisioning matrix
	$(MAKE) test-e2e E2E_TIMEOUT=6h ARGS='--label-filter="nico-vms && os-profiles"'

.PHONY: e2e-gofail
e2e-gofail: ## Execute gofail e2e tests against a local Temporal dev server.
	@bash -euo pipefail -c '\
		start=$$SECONDS; last=$$start; \
		phase() { \
			now=$$SECONDS; \
			printf "[gofail] %-22s %4ds (total %4ds)\n" "$$1" "$$((now - last))" "$$((now - start))"; \
			last=$$now; \
		}; \
		mkdir -p "$(TEMPORAL_CLI_DOWNLOAD_DIR)" "$$(dirname "$(CWE_GOFAIL_E2E_WORKER_BINARY)")"; \
		go install go.etcd.io/gofail@$(GOFAIL_VERSION); \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION); \
		phase "install-tools"; \
		KUBEBUILDER_ASSETS="$$("$(SETUP_ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(ENVTEST_BIN_DIR)" -p path)"; \
		export KUBEBUILDER_ASSETS; \
		phase "envtest-assets"; \
		"$(GOFAIL)" enable internal/temporal/activities; \
		cleanup() { \
			"$(GOFAIL)" disable internal/temporal/activities; \
		}; \
		trap cleanup EXIT; \
		phase "gofail-enable"; \
		go build -tags gofail -o "$(CWE_GOFAIL_E2E_WORKER_BINARY)" ./cmd/worker; \
		phase "build-worker"; \
		TEMPORAL_CLI_DOWNLOAD_DIR="$(TEMPORAL_CLI_DOWNLOAD_DIR)" \
			CWE_GOFAIL_E2E_WORKER_BINARY="$(CWE_GOFAIL_E2E_WORKER_BINARY)" \
			CWE_GOFAIL_E2E_CLUSTER_CRDS="$(CWE_GOFAIL_E2E_CLUSTER_CRDS)" \
			CWE_GOFAIL_E2E_CAPNICO_CRDS="$(CWE_GOFAIL_E2E_CAPNICO_CRDS)" \
			go test -v -tags gofail ./e2e/gofail ./e2e/tracing -timeout 30m -count=1; \
		phase "run-tests"; \
		cleanup; \
		trap - EXIT; \
		test -z "$$(find . -name "*.fail.go" -print)" \
	'


.PHONY: e2e-tracing
e2e-tracing: ## Execute the tracing e2e test against a local Temporal dev server and PostgreSQL (initdb and postgres on PATH, or set CWE_GOFAIL_E2E_DB_CONNECTION_STRING).
	mkdir -p "$(TEMPORAL_CLI_DOWNLOAD_DIR)"
	TEMPORAL_CLI_DOWNLOAD_DIR="$(TEMPORAL_CLI_DOWNLOAD_DIR)" go test -v -tags gofail ./e2e/tracing -timeout 10m -count=1


.PHONY: docker-build-local
docker-build-local: ## Build the controller image for amd64 and load it into the local daemon
	@# The build context is the repository root: the controller is a member of
	@# the root Go workspace, and the image build needs go.work and sibling
	@# modules. Go downloads public dependencies into the builder cache.
	@# amd64 only because --load does not support multi-platform images.
	@# Use docker-build-multiarch to build both architectures.
	DOCKER_BUILDKIT=1 docker build -f Dockerfile --platform linux/amd64 \
	  --progress=plain \
	  --tag $(IMG_NAME):$(GIT_VERSION) \
	  --tag $(IMG_NAME):$(BUILD_VERSION) \
	  --tag $(IMG) \
	  --build-arg BUILD_VERSION=${BUILD_VERSION} \
	  --build-arg GIT_VERSION=${GIT_VERSION} \
	  ..

.PHONY: docker-build-multiarch
docker-build-multiarch: ## Build the controller image for linux/amd64 and linux/arm64 (requires --push or --output)
	@# Builds both architectures but does not load them into the local daemon.
	@# To push: add --push and set IMG. To export: add --output type=oci,dest=cwe.tar.
	docker buildx build -f Dockerfile \
	  --platform linux/amd64,linux/arm64 \
	  --progress=plain \
	  --tag $(IMG_NAME):$(GIT_VERSION) \
	  --tag $(IMG_NAME):$(BUILD_VERSION) \
	  --tag $(IMG) \
	  --build-arg BUILD_VERSION=${BUILD_VERSION} \
	  --build-arg GIT_VERSION=${GIT_VERSION} \
	  ..

# Publish targets for pushing to an explicit registry (used by CI's
# image-nvcr.yml). These deliberately do NOT use IMG_NAME/IMG: this OSS repo
# must never default-push to the internal production image
# (nvcr.io/j7sbcjl3qgta/nke-controller, IMG_NAME above). The destination
# has no default and must be passed explicitly via IMG_REPO, guarded by
# check-img-repo below, so a bare invocation cannot clobber it.
#
# Local usage (push to a scratch repo you own):
#   docker login nvcr.io
#   make docker-push-arch IMG_REPO=nvcr.io/<org>/<team>/scratch PUSH_TAGS=dev-me ARCH=amd64
IMG_REPO ?=
PUSH_TAGS ?=
ARCH ?= amd64
BUILDX_EXTRA_ARGS ?=

# Every stage after the push used to re-resolve a mutable tag: the manifest
# stitched <t>-amd64 and <t>-arm64, the SBOM re-pulled sha-<sha>-<arch>, and the
# promotion resolved the version tag again. Each could read a newer push than
# the one that run made. These files carry the digest forward instead. Under
# bin/, which is already gitignored.
DIGEST_FILE       ?= $(CURDIR)/bin/image-digest-$(ARCH).txt
INDEX_DIGEST_FILE ?= $(CURDIR)/bin/image-digest-index.txt

# The per-arch sources docker-manifest stitches. Empty means the mutable
# per-arch tags, which is what a local run wants. CI passes the digests
# docker-push-arch recorded, so the index holds exactly what this run pushed.
ARCH_DIGESTS ?=

.PHONY: check-img-repo
check-img-repo:
	@test -n "$(IMG_REPO)" || { echo "ERROR: IMG_REPO must be set explicitly"; exit 1; }
	@test -n "$(PUSH_TAGS)" || { echo "ERROR: PUSH_TAGS must be set"; exit 1; }

.PHONY: docker-push-arch
docker-push-arch: check-img-repo ## Build+push one arch to IMG_REPO, tags <t>-$(ARCH)
	@mkdir -p $(dir $(DIGEST_FILE))
	@# --provenance=false: the default provenance turns a single-arch push into an
	@# OCI index, which breaks docker-manifest's imagetools create and the NGC UI.
	docker buildx build -f Dockerfile \
	  --platform linux/$(ARCH) --push --provenance=false --progress=plain \
	  $(foreach t,$(PUSH_TAGS),--tag $(IMG_REPO):$(t)-$(ARCH)) \
	  --build-arg BUILD_VERSION=${BUILD_VERSION} \
	  --build-arg GIT_VERSION=${GIT_VERSION} \
	  $(BUILDX_EXTRA_ARGS) \
	  --metadata-file $(DIGEST_FILE).json \
	  ..
	@# containerimage.digest is the manifest buildx just pushed. Read with sed
	@# rather than jq, which the self-hosted runners do not all carry.
	@sed -n 's/.*"containerimage\.digest": *"\([^"]*\)".*/\1/p' $(DIGEST_FILE).json > $(DIGEST_FILE)
	@test -s $(DIGEST_FILE) || { echo "ERROR: no containerimage.digest in $(DIGEST_FILE).json"; exit 1; }
	@echo "pushed $(IMG_REPO)@$$(cat $(DIGEST_FILE))"

.PHONY: docker-manifest
docker-manifest: check-img-repo ## Stitch <t>-amd64 + <t>-arm64 into <t> under IMG_REPO
	@mkdir -p $(dir $(INDEX_DIGEST_FILE))
	@set -e; for t in $(PUSH_TAGS); do \
	  if [ -n "$(ARCH_DIGESTS)" ]; then \
	    srcs=""; for d in $(ARCH_DIGESTS); do srcs="$$srcs $(IMG_REPO)@$$d"; done; \
	  else \
	    srcs="$(IMG_REPO):$$t-amd64 $(IMG_REPO):$$t-arm64"; \
	  fi; \
	  docker buildx imagetools create -t "$(IMG_REPO):$$t" $$srcs; \
	done
	@# One inspect, not one per stage: every tag above points at the same index,
	@# and everything downstream reads this digest rather than a tag.
	@docker buildx imagetools inspect "$(IMG_REPO):$(firstword $(PUSH_TAGS))" \
	  --format '{{.Manifest.Digest}}' > $(INDEX_DIGEST_FILE)
	@echo "index $(IMG_REPO)@$$(cat $(INDEX_DIGEST_FILE))"

##@ Run

.PHONY: run
run: ## Runs the worker.
	go run cmd/worker/main.go

.PHONY: debug
debug: ## Runs the worker with the delve debugger (attach on :12345).
	dlv debug --headless --listen=:12345 ./cmd/worker/main.go

# Installed into ./bin rather than the shared GOBIN so a stale global binary
# cannot leak in — see the toolchain note below for why that matters.
GO_LICENSES         ?= $(CURDIR)/bin/go-licenses

# go-licenses embeds golang.org/x/tools/go/packages and must be built with the
# SAME toolchain that `go list` runs inside this module. A module's `go`
# directive can make Go switch toolchains (GOTOOLCHAIN=auto), and on a mismatch
# go-licenses reports every stdlib package as "does not have module info" and
# produces nothing at all — a failure that reads like a broken generator rather
# than a stale binary. `go install pkg@version` deliberately ignores the current
# module, so the toolchain has to be pinned explicitly rather than inherited.
GO_TOOLCHAIN := $(shell go version | awk '{print $$3}')

$(GO_LICENSES):
	@echo "Installing go-licenses $(GO_LICENSES_VERSION) (toolchain $(GO_TOOLCHAIN))..."
	@GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(CURDIR)/bin \
		go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)

# A named alias for the rule above, so another module can ask for the binary by
# name. GO_LICENSES is an absolute path, which means the real rule's target is
# also absolute and `make -C .. bin/go-licenses` does not match it -- it fails
# with "No rule to make target 'bin/go-licenses'". nkectl's notices target hit
# exactly that, and it was masked in CI because the controller step runs first
# and leaves the binary behind.
.PHONY: go-licenses
go-licenses: $(GO_LICENSES) ## Build the pinned go-licenses into ./bin.

# Permissive licenses approved for redistribution. Matches the allowlist used by
# NVIDIA/aicr. A dependency introducing anything else - copyleft in particular -
# fails the check and needs a conscious decision rather than silent acceptance.
#
# This is a local pre-flight, not the gate of record. CI enforces dependency
# licenses in the security stage via the shared go-license-checks job
# (nke/gitlab-ci-cd, templates/security/go-license-checks.gitlab-ci.yml), which
# runs over ./... rather than just the shipped binary. The two differ in shape:
# CI denies by license *type* (restricted, reciprocal, forbidden, unknown) while
# this allows by license *name*, which is the stricter test on the packages it
# covers. Both pass on the current dependency set. Run this before pushing to
# catch a bad dependency without waiting for the pipeline.
ALLOWED_LICENSES ?= MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,Zlib

.PHONY: license-policy-check
license-policy-check: $(GO_LICENSES) ## Verify every third-party dependency uses an approved license.
	@echo "Checking dependency licenses against $(ALLOWED_LICENSES)..."
	@# GOWORK=off and the quotes both matter. Under the workspace, `go list -m`
	@# prints all eight member modules, so unquoted it word-splits into one
	@# --ignore= plus seven stray positional arguments that go-licenses reads as
	@# package patterns. GOWORK=off narrows it to this module, and the siblings
	@# that genuinely need ignoring are named explicitly.
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO_LICENSES) check ./cmd/worker \
		--allowed_licenses=$(ALLOWED_LICENSES) \
		--ignore="$$(GOWORK=off go list -m),github.com/dsx-ai-factory/nke/agent,github.com/dsx-ai-factory/cluster-api-provider-nico"

.PHONY: notices
notices: $(GO_LICENSES) ## Regenerate THIRD_PARTY_NOTICES.md for the released worker binary.
	@GO_LICENSES="$(GO_LICENSES)" ./hack/generate-notices

# Not declared as depending on `notices`: the tracked-file gate below is cheap
# and must run before spending minutes regenerating.
.PHONY: notices-check
notices-check: ## Verify THIRD_PARTY_NOTICES.md is up to date. (advisory; not in the merge gate)
	@# git diff reports nothing for an untracked path, so without this an
	@# uncommitted or accidentally-removed notices file would pass silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md > /dev/null 2>&1 \
		|| { echo "THIRD_PARTY_NOTICES.md is not tracked by git. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory notices
	@if ! git diff --exit-code -- THIRD_PARTY_NOTICES.md > /dev/null 2>&1; then \
		echo "THIRD_PARTY_NOTICES.md is out of date. Please run 'make notices' locally and commit the changes."; \
		git diff --name-only -- THIRD_PARTY_NOTICES.md; \
		exit 1; \
	fi
