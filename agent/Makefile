# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

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
DEB_VERSION := $(shell echo '$(VERSION)' | sed 's/^v//')
$(info $$VERSION is [${VERSION}])
$(info $$DEB_VERSION is [${DEB_VERSION}])

LDFLAGS += -ldflags="-X main.VERSION=$(VERSION)"

ARCHES := amd64 arm64

GOLANGCI_LINT_REPO      ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION   ?= v2.7.0

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

setup-buildx: ## Create buildx builder for multi-arch builds (if not exists)
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container \
			--driver-opt env.GOGC=100 \
			--buildkitd-flags '--allow-insecure-entitlement network.host' \
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
lint: ## Lint source files.
	go install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint run --timeout 10m --show-stats ./...

test: ## Run tests
	go test ./...

##@ Utility
.PHONY: clean
clean: ## Remove all build artifacts except protos
	rm -rf bin release

##@ Build
.PHONY: build-amd64 build-arm64 build-all build

build-amd64:
	GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/linux_amd64/nke-agent ./cmd/agent/main.go

build-arm64:
	GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o bin/linux_arm64/nke-agent ./cmd/agent/main.go

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

	sed -i "s/Architecture: .*/Architecture: $(1)/" $(ARCH_BUILD_DIR)/DEBIAN/control

	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/postinst
	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/prerm
	chmod 755 $(ARCH_BUILD_DIR)/DEBIAN/postrm
	fakeroot dpkg-deb --build $(ARCH_BUILD_DIR) bin/nke-agent_$(DEB_VERSION)_$(1).deb
endef

##@ Debian Packaging
.PHONY: deb-amd64 deb-arm64 deb-all debs

deb-amd64: build-amd64 ## Build the amd64 Debian package
	$(call build_deb,amd64)

deb-arm64: build-arm64 ## Build the arm64 Debian package
	$(call build_deb,arm64)

deb-all: deb-amd64 deb-arm64 ## Build all Debian packages
debs: deb-all

##@ Third-party notices

# go-licenses is pinned here rather than taken from PATH so CI and a laptop
# generate the same file. It installs into ./bin, not the shared GOBIN.
GO_LICENSES_VERSION ?= v2.0.1

# go-licenses embeds go/packages, and must be built with the *same* toolchain
# that `go list` actually runs inside this module. A module's `go` directive can
# make Go switch toolchains (GOTOOLCHAIN=auto), and on a mismatch go-licenses
# reports every stdlib package as having no module info and emits nothing at
# all. `go install pkg@version` deliberately ignores the current module, so the
# toolchain has to be pinned explicitly rather than inherited.
GO_TOOLCHAIN := $(shell go version | awk '{print $$3}')

bin/go-licenses:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(CURDIR)/bin \
		go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)

.PHONY: notices
notices: bin/go-licenses ## Regenerate THIRD_PARTY_NOTICES.md.
	@bash hack/generate-notices.sh

.PHONY: notices-check
notices-check: ## Verify THIRD_PARTY_NOTICES.md matches the dependency tree.
	@echo "- Checking THIRD_PARTY_NOTICES.md is up to date..."
	@# Cheap gate first, before spending minutes regenerating. git diff reports
	@# nothing for an untracked path, so an uncommitted notices file would
	@# otherwise pass silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md >/dev/null 2>&1 \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is not tracked. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) notices
	@git diff --exit-code -- THIRD_PARTY_NOTICES.md \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is stale. Run 'make notices' and commit the change."; exit 1; }
