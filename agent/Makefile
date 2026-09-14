# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The licence header template lives once, at the repository root.
REPO_ROOT ?= $(shell git rev-parse --show-toplevel)

# Makefile for multi-arch agent

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
# ?= means: only set if not already defined (preserves env var from CI)
GOBIN ?= $(shell go env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell go env GOPATH)/bin
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Component versioning follows the Kubernetes/Go multi-module monorepo pattern:
# release tags are namespaced as agent/vX.Y.Z (not a bare vX.Y.Z).
# Override with VERSION=... when building outside of a tagged checkout.
VERSION_RAW ?= $(shell git describe --match 'agent/v*' --tags --always --dirty 2>/dev/null || git rev-parse --short HEAD)
VERSION ?= $(shell echo '$(VERSION_RAW)' | sed 's|^agent/||')
# dpkg refuses a version that does not start with a digit:
#
#   dpkg-deb: error: 'Version' field value 'f0578cc8':
#             version number does not start with digit
#
# `git describe --always` falls back to a bare commit sha when no agent/v* tag
# is reachable, and this repository has none -- so `make deb-all` cannot build
# at all on main today. Prefixing 0.0.0- keeps an untagged build working and
# sorts below every real release, so a development package can never be
# mistaken for, or upgrade over, a tagged one.
# The test is "did this come from a tag", NOT "does it start with a digit".
# Tags are agent/vX.Y.Z, so VERSION starts with `v` exactly when describe found
# one; anything else is the --always sha fallback.
#
# Testing the first character instead is wrong and looks right: roughly ten of
# the sixteen possible leading hex digits ARE digits, so a sha like 587f4bbd
# passes and becomes the package version verbatim -- and 587f4bbd sorts ABOVE
# every real release, so a development build would upgrade over a tagged one.
# The bug reproduces on about 62% of commits, which is the worst kind.
#
# Pure Make, no $(shell): Make counts parentheses to find the end of
# $(shell ...), so an unbalanced `)` in a shell pattern like [0-9]*) closes it
# early and the rest of the statement leaks into the value.
DEB_VERSION := $(if $(filter v%,$(VERSION)),$(patsubst v%,%,$(VERSION)),0.0.0-$(VERSION))
$(info $$VERSION is [${VERSION}])
$(info $$DEB_VERSION is [${DEB_VERSION}])

RELEASE_LDFLAGS ?= -s -w -buildid=

# -trimpath removes the build machine's filesystem paths from the binary. These
# binaries are published to ghcr.io/dsx-ai-factory/nke/agent, so without it every
# release carries the layout of whatever machine built it, and two builds of the
# same commit from different directories differ.
LDFLAGS += -trimpath -ldflags="-X main.VERSION=$(VERSION) $(RELEASE_LDFLAGS)"

ARCHES := amd64 arm64

# golangci-lint pin and bootstrap, shared with every other module.
include ../make/tool-versions.mk
include ../make/golangci.mk

BUF_VERSION := v1.65.0

GIT_HOOKS_DIR := $(shell git rev-parse --git-path hooks 2>/dev/null || echo .git/hooks)

BUILD_DIR=build/deb

##@ Help
.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Git hooks
.PHONY: setup-hooks
setup-hooks: ## Install or update Git pre-commit hook (monorepo-aware)
	@set -e; \
	echo "🔧 Ensuring Git pre-commit hook is up to date..."; \
	mkdir -p "$(GIT_HOOKS_DIR)"; \
	src="internal-scripts/hooks/pre-commit"; \
	dst="$(GIT_HOOKS_DIR)/pre-commit"; \
	if [ ! -f "$$dst" ] || ! cmp -s "$$src" "$$dst"; then \
		cp "$$src" "$$dst"; \
		chmod +x "$$dst"; \
		echo "✅ Installed/updated Git pre-commit hook at $$dst"; \
	else \
		echo "✅ Git pre-commit hook already up to date."; \
	fi

##@ Docker images
.PHONY: build-builder-image push-builder-image setup-buildx

BUILDER_IMAGE := $(or $(BUILDER_IMAGE),$(if $(NKE_CONTAINER_REGISTRY),$(NKE_CONTAINER_REGISTRY)/$(or $(NKE_BUILDER_IMAGE_NAME),nke-builder),ghcr.io/example/nke/nke-builder))
BUILDER_TAG := $(or $(BUILDER_TAG),$(or $(NKE_BUILDER_IMAGE_TAG),latest))
BUILDX_PLATFORMS := linux/amd64,linux/arm64
BUILDX_BUILDER := nke-multiarch-builder

# The docker-container driver runs BuildKit in its own container, so it does not
# inherit the Docker daemon's registry-mirror configuration. CI runners ship a
# prepopulated /etc/buildkit/buildkitd.toml pointing at the local mirror; pass it
# through when present so builds are cached and are not subject to Docker Hub
# rate limits. Absent (developer laptops), the builder is created without it and
# pulls directly, which is the current behaviour.
BUILDKITD_CONFIG ?= /etc/buildkit/buildkitd.toml
BUILDX_CONFIG_ARG = $(if $(wildcard $(BUILDKITD_CONFIG)),--config $(BUILDKITD_CONFIG),)

setup-buildx: ## Create buildx builder for multi-arch builds (if not exists)
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container \
			--driver-opt env.GOGC=100 \
			--buildkitd-flags '--allow-insecure-entitlement network.host' \
			$(BUILDX_CONFIG_ARG) \
			--bootstrap
	docker buildx use $(BUILDX_BUILDER)

build-builder-image: setup-buildx ## Build the CI/CD builder image for amd64 and arm64
	docker buildx build \
		--platform $(BUILDX_PLATFORMS) \
		-t $(BUILDER_IMAGE):$(BUILDER_TAG) \
		-f Dockerfile.nke-builder \
		--shm-size=2g \
		--ulimit nofile=1048576:1048576 \
		--progress=plain \
		.

push-builder-image: setup-buildx ## Build and push the CI/CD builder image for amd64 and arm64
	docker buildx build \
		--platform $(BUILDX_PLATFORMS) \
		-t $(BUILDER_IMAGE):$(BUILDER_TAG) \
		-f Dockerfile.nke-builder \
		--shm-size=2g \
		--ulimit nofile=1048576:1048576 \
		--progress=plain \
		--push \
		.

##@ Proto generation

.PHONY: buf
buf: ## Install buf if not present or wrong version
	@$(GOBIN)/buf version 2>/dev/null | grep -q $(BUF_VERSION) || \
		go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: protos
protos: buf clean-proto ## Generate proto files using buf
	$(GOBIN)/buf generate

.PHONY: proto-fmt
proto-fmt: buf ## Format proto files using buf
	$(GOBIN)/buf format -w

.PHONY: check-protos
check-protos: buf ## Check if proto files need formatting or regeneration
	@$(GOBIN)/buf format --diff --exit-code \
 || \
		(echo "❌ Proto files need formatting. Run 'make proto-fmt' to fix." && false)

.PHONY: clean-proto
clean-proto: ## Remove generated proto files
	rm -f pkg/agent/proto/*.pb.go

##@ Code quality
.PHONY: fmt vet lint test
fmt: ## Run go fmt
	go fmt ./...

vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Lint source files. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --show-stats ./...

# GO_TEST_FLAGS lets CI add -race without the workflow having to restate the
# test command, so `make test` stays the one definition of what "the tests" are.
GO_TEST_FLAGS ?=

# macOS puts TMPDIR under /var/folders, and /var there is a symlink to
# /private/var. internal/agent refuses a destination whose parent path contains a
# symlink, so 23 tests failed on a Mac with "invalid destination ...: parent path
# /var is a symlink". The agent was right, and the tests were only reading the
# developer's TMPDIR. /private/tmp is the same directory with the symlink already
# resolved. Linux and CI are unaffected.
ifeq ($(shell uname -s),Darwin)
export TMPDIR := /private/tmp
endif

test: ## Run tests (run by the merge gate)
	go test $(GO_TEST_FLAGS) ./...

# Sources in this module carry the repository's Apache-2.0 identifier, per the
# "License headers" section of the root CONTRIBUTING.md. Ignored paths are
# generated output (*.pb.go, rewritten by `make protos`), build tooling under
# hack/, build artifacts, and samples/kubeadm-deploy/calico.yaml, which is an
# upstream Calico manifest and not ours to stamp. Dockerfiles are ignored too:
# addlicense keys off the file extension, so Dockerfile.proto gets C-style `//`
# comments, which Docker cannot parse.
ADDLICENSE_IGNORES := \
	-ignore '**/Dockerfile*' \
	-ignore 'bin/**' \
	-ignore 'build/**' \
	-ignore 'release/**' \
	-ignore 'vendor/**' \
	-ignore 'hack/**' \
	-ignore '**/*.pb.go' \
	-ignore 'samples/kubeadm-deploy/calico.yaml'

.PHONY: license
license: ## Ensure license headers at code.
	go run github.com/google/addlicense@v1.1.1 -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" $(ADDLICENSE_IGNORES) .

# Expected to be run on clean working directory.
.PHONY: file-license-check
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi

##@ Utility
.PHONY: clean
clean: ## Remove all build artifacts except protos
	rm -rf bin release

##@ Build
.PHONY: build-amd64 build-arm64 build-all build

# cgo is left to Go on a native host and turned off when cross-compiling.
#
# On a native Linux runner this is a no-op: cgo stays on, the binary links
# against libc, and `Depends: libc6` in scripts/control is true. That is what
# deb.yml relies on by giving each architecture a runner of its own arch.
#
# Cross-compiling is where it matters. Go turns cgo off by itself when there is
# no C toolchain for the target, but an environment that exports CC -- the
# repository's Flox environment sets CC=clang -- makes it try anyway, and the
# build then fails against the host SDK:
#
#   /Library/Developer/CommandLineTools/SDKs/MacOSX.sdk/usr/include/unistd.h:609:6:
#   note: 'setreuid' declared here
#   make: *** [Makefile:230: build-amd64] Error 1
#
# So `make build` worked on a Mac outside Flox and failed inside it, for the same
# checkout. Deciding here makes the result the same either way.
GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
agent_cgo = $(if $(and $(filter linux,$(GOHOSTOS)),$(filter $(1),$(GOHOSTARCH))),,CGO_ENABLED=0)

build-amd64:
	$(call agent_cgo,amd64) GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/linux_amd64/nke-agent ./cmd/agent/main.go
	$(call agent_cgo,amd64) GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-X main.version=$(VERSION) $(RELEASE_LDFLAGS)" -o bin/linux_amd64/nke-agent-updater ./cmd/updater/main.go

build-arm64:
	$(call agent_cgo,arm64) GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o bin/linux_arm64/nke-agent ./cmd/agent/main.go
	$(call agent_cgo,arm64) GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-X main.version=$(VERSION) $(RELEASE_LDFLAGS)" -o bin/linux_arm64/nke-agent-updater ./cmd/updater/main.go

build-all: build-amd64 build-arm64
build: build-all ## Default build = all arches

##@ Package
.PHONY: package-amd64 package-arm64 package-all package

package-amd64: build-amd64
	rm -rf release/agent_${VERSION}_amd64
	mkdir -p release/agent_${VERSION}_amd64
	cp -r bin/linux_amd64/* release/agent_${VERSION}_amd64/
	cp -r scripts/* release/agent_${VERSION}_amd64/
	tar -czf release/agent_${VERSION}_amd64.tgz -C release agent_${VERSION}_amd64

package-arm64: build-arm64
	rm -rf release/agent_${VERSION}_arm64
	mkdir -p release/agent_${VERSION}_arm64
	cp -r bin/linux_arm64/* release/agent_${VERSION}_arm64/
	cp -r scripts/* release/agent_${VERSION}_arm64/
	tar -czf release/agent_${VERSION}_arm64.tgz -C release agent_${VERSION}_arm64

package-all: package-amd64 package-arm64

package: package-all ## Default package = all arches

# Helper to build the deb package for a specific arch
# Usage: $(call build_deb,amd64)
define build_deb
	@echo "Building Debian package for $(1)..."
	$(eval ARCH_BUILD_DIR := $(BUILD_DIR)_$(1))
	@echo "Building Debian package for $(1)..."
	rm -rf $(ARCH_BUILD_DIR)
	mkdir -p $(ARCH_BUILD_DIR)/DEBIAN
	mkdir -p $(ARCH_BUILD_DIR)/usr/local/bin
	mkdir -p $(ARCH_BUILD_DIR)/etc/systemd/system/

	cp -r bin/linux_$(1)/* $(ARCH_BUILD_DIR)/usr/local/bin/

	cp ./scripts/*.service $(ARCH_BUILD_DIR)/etc/systemd/system/

	for f in control postinst prerm postrm; do \
		[ -f ./scripts/$$f ] && sed "s/{{VERSION}}/$(DEB_VERSION)/g" ./scripts/$$f > $(ARCH_BUILD_DIR)/DEBIAN/$$f; \
	done

	for s in ./scripts/*.service; do \
		sed "s/{{VERSION}}/$(DEB_VERSION)/g" $$s > $(ARCH_BUILD_DIR)/etc/systemd/system/$$(basename $$s); \
	done

	@# Not `sed -i "s/..."`: BSD sed reads the script as the -i backup suffix and
	@# the filename as the script, so this failed on a stock Mac before
	@# `make deb-amd64` reached fakeroot.
	sed "s/Architecture: .*/Architecture: $(1)/" $(ARCH_BUILD_DIR)/DEBIAN/control > $(ARCH_BUILD_DIR)/DEBIAN/control.new
	mv $(ARCH_BUILD_DIR)/DEBIAN/control.new $(ARCH_BUILD_DIR)/DEBIAN/control

	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/postinst
	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/prerm
	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/postrm
	fakeroot dpkg-deb --build $(ARCH_BUILD_DIR) bin/nke-agent_$(DEB_VERSION)_$(1).deb
endef

##@ Debian Packaging

# The .deb declares `Depends: libc6` (scripts/control), so the binary inside it
# must link against libc. That happens only when cgo is on, and Go turns cgo off
# by itself when cross-compiling, because there is no C toolchain for the target.
#
# Nothing in this file sets CGO_ENABLED, so what you get is decided by the host:
# native builds are dynamic and correct, cross builds are static and ship a
# Depends that is false for them. deb.yml:24-33 records the same thing and solves
# it by building each architecture on a runner of that architecture.
#
# The same build-amd64 and build-arm64 targets also feed the OCI artifact, which
# wants the opposite: agent-artifact.yml sets CGO_ENABLED=0 and asserts
# "statically linked". One pair of targets, two callers, opposite requirements,
# and the Makefile said nothing about either.
#
# It says something now. assert_dynamic runs the check CI runs, so a cross build
# fails here with the reason rather than producing a package whose Depends is
# wrong. Set AGENT_SKIP_LINKAGE_CHECK=1 to build a deb you know is not for
# release.
define assert_dynamic
	@if [ -z "$(AGENT_SKIP_LINKAGE_CHECK)" ] && command -v file >/dev/null 2>&1; then \
		linkage=$$(file bin/linux_$(1)/nke-agent); \
		case "$$linkage" in \
			*"dynamically linked"*) ;; \
			*) echo "ERROR: bin/linux_$(1)/nke-agent is not dynamically linked, so"; \
			   echo "  'Depends: libc6' in scripts/control is wrong for it."; \
			   echo "  Go disables cgo when cross-compiling, so build the $(1) package"; \
			   echo "  on a $(1) host, as deb.yml does. Override with"; \
			   echo "  AGENT_SKIP_LINKAGE_CHECK=1 for a package you will not release."; \
			   echo "  file said: $$linkage"; \
			   exit 1 ;; \
		esac; \
	fi
endef

.PHONY: deb-amd64 deb-arm64 deb-all debs

deb-amd64: build-amd64 ## Build the amd64 Debian package (advisory; not in the merge gate)
	$(call assert_dynamic,amd64)
	$(call build_deb,amd64)

deb-arm64: build-arm64 ## Build the arm64 Debian package (advisory; not in the merge gate)
	$(call assert_dynamic,arm64)
	$(call build_deb,arm64)

deb-all: deb-amd64 deb-arm64 ## Build all Debian packages
debs: deb-all

##@ Third-party notices

# go-licenses embeds go/packages, and must be built with the *same* toolchain
# that `go list` actually runs inside this module. A module's `go` directive can
# make Go switch toolchains (GOTOOLCHAIN=auto), and on a mismatch go-licenses
# reports every stdlib package as having no module info and emits nothing at
# all. `go install pkg@version` deliberately ignores the current module, so the
# toolchain has to be pinned explicitly rather than inherited.
# GOWORK=off: this must be the toolchain that builds THIS module. With a root
# go.work the bare command reports the workspace's Go version instead, and a
# go-licenses built with a different Go than runs it reports "no module info"
# and emits nothing -- the failure warned about just above.
GO_TOOLCHAIN := $(shell GOWORK=off go version | awk '{print $$3}')

bin/go-licenses:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(CURDIR)/bin \
		go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)

.PHONY: notices
notices: bin/go-licenses ## Regenerate THIRD_PARTY_NOTICES.md.
	@bash hack/generate-notices.sh

.PHONY: notices-check
notices-check: ## Verify THIRD_PARTY_NOTICES.md matches the dependency tree. (advisory; not in the merge gate)
	@echo "- Checking THIRD_PARTY_NOTICES.md is up to date..."
	@# Cheap gate first, before spending minutes regenerating. git diff reports
	@# nothing for an untracked path, so an uncommitted notices file would
	@# otherwise pass silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md >/dev/null 2>&1 \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is not tracked. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) notices
	@git diff --exit-code -- THIRD_PARTY_NOTICES.md \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is stale. Run 'make notices' and commit the change."; exit 1; }
