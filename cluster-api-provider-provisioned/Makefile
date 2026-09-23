# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_ROOT ?= $(shell git rev-parse --show-toplevel)
LOCALBIN ?= $(CURDIR)/bin

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches
# agent/Makefile and cli/Makefile so the shared golangci.mk macro installs and
# finds the same binary. Module-local tools (controller-gen) use LOCALBIN
# directly instead, via an explicit GOBIN= prefix on their own `go install`.
GOBIN ?= $(shell go env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell go env GOPATH)/bin
endif

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.20.1
IMG ?= docker.io/library/cluster-api-provider-provisioned:latest
PLATFORMS ?= linux/amd64,linux/arm64
OCI_ARCHIVE ?= $(CURDIR)/out/cluster-api-provider-provisioned.oci.tar
BUILDX_OUTPUT ?= type=oci,dest=$(OCI_ARCHIVE)

include ../make/golangci.mk

BUILD_VERSION ?= $(shell \
	v=$$(git describe --tags --dirty --match 'cluster-api-provider-provisioned/v[0-9]*' 2>/dev/null \
		|| git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null \
		|| git describe --always --dirty --match 'cluster-api-provider-provisioned/v[0-9]*' 2>/dev/null \
		|| echo v0.0.0); \
	printf '%s' "$${v##*/}")

.DEFAULT_GOAL := help
.PHONY: help test lint controller-gen generate manifests generate-check docker-build \
	check-img-repo docker-push-arch docker-manifest

help: ## Show the available targets.
	@echo "Usage: make <target>"
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

test: ## Run unit tests.
	go test $(GO_TEST_FLAGS) ./...

lint: ## Run golangci-lint.
	@$(GOLANGCI_LINT_RUN) run --timeout 10m ./...

controller-gen: ## Install the pinned controller-gen binary.
	@if [ "$$($(CONTROLLER_GEN) --version 2>/dev/null)" != "Version: $(CONTROLLER_TOOLS_VERSION)" ]; then \
		mkdir -p "$(LOCALBIN)"; \
		GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Generate DeepCopy implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

manifests: controller-gen ## Generate CRDs and controller RBAC.
	"$(CONTROLLER_GEN)" \
		crd:crdVersions=v1,headerFile="hack/boilerplate.yaml.txt" \
		paths="./api/..." \
		output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" \
		rbac:roleName=provisioned-controller-manager,fileName=role.yaml,headerFile="hack/boilerplate.yaml.txt" \
		paths="./controllers/..." \
		output:rbac:artifacts:config=config/rbac

generate-check: generate manifests ## Verify generated code, CRDs, and RBAC are current.
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd config/rbac/role.yaml
	@# git diff says nothing about a path git is not tracking, so a newly
	@# generated file would otherwise pass while never being committed. Same
	@# guard as Makefile's licences-check.
	@untracked=$$(git ls-files --others --exclude-standard -- api/v1alpha1/zz_generated.deepcopy.go config/crd config/rbac/role.yaml); \
	 [ -z "$$untracked" ] \
		|| { echo "ERROR: generated files are not tracked. Run 'make generate manifests' and commit them:"; printf '%s\n' "$$untracked" | sed 's/^/    /'; exit 1; }

docker-build: ## Build AMD64 and ARM64 images into a local OCI archive.
	@mkdir -p "$(dir $(OCI_ARCHIVE))"
	docker buildx build \
		--file "$(CURDIR)/Dockerfile" \
		--platform "$(PLATFORMS)" \
		--tag "$(IMG)" \
		--build-arg "BUILD_VERSION=$(BUILD_VERSION)" \
		--output "$(BUILDX_OUTPUT)" \
		--progress plain \
		"$(REPO_ROOT)"
	@echo "Built $(PLATFORMS) with output $(BUILDX_OUTPUT)"

# Publish targets for pushing to an explicit registry, used by CI's
# image-provisioned-nvcr.yml. docker-build above stays what it was: a local
# multi-arch OCI archive, which is what a release artifact wants and not
# something a registry can be pushed from.
#
# The destination has no default. IMG above names a docker.io/library
# placeholder that exists only so a local build has a tag, and pushing there
# would be meaningless at best; check-img-repo makes a bare invocation fail
# instead. This mirrors controller/Makefile, where the same guard keeps an OSS
# build from ever defaulting onto the internal production image.
#
# Local usage (push to a scratch repo you own):
#   docker login nvcr.io
#   make docker-push-arch IMG_REPO=nvcr.io/<org>/<team>/scratch PUSH_TAGS=dev-me ARCH=amd64
IMG_REPO ?=
PUSH_TAGS ?=
ARCH ?= amd64
BUILDX_EXTRA_ARGS ?=

# Every stage after the push used to re-resolve a mutable tag. These files
# carry the digest forward instead. Under bin/, which is already gitignored.
DIGEST_FILE       ?= $(CURDIR)/bin/image-digest-$(ARCH).txt
INDEX_DIGEST_FILE ?= $(CURDIR)/bin/image-digest-index.txt

# The per-arch sources docker-manifest stitches. Empty means the mutable
# per-arch tags, which is what a local run wants. CI passes the digests
# docker-push-arch recorded, so the index holds exactly what this run pushed.
ARCH_DIGESTS ?=

check-img-repo:
	@test -n "$(IMG_REPO)" || { echo "ERROR: IMG_REPO must be set explicitly"; exit 1; }
	@test -n "$(PUSH_TAGS)" || { echo "ERROR: PUSH_TAGS must be set"; exit 1; }

docker-push-arch: check-img-repo ## Build+push one arch to IMG_REPO, tagged <t>-$(ARCH).
	@mkdir -p $(dir $(DIGEST_FILE))
	@# --provenance=false: the default provenance turns a single-arch push into
	@# an OCI index, which breaks docker-manifest's imagetools create below and
	@# renders wrongly in the NGC UI.
	@#
	@# Per-arch tags rather than one multi-arch push, so the two architectures
	@# build on their own native runners and docker-manifest stitches the result.
	@# The build context is the repository root for the reason docker-build
	@# gives: the Dockerfile compiles from the workspace vendor tree.
	docker buildx build \
		--file "$(CURDIR)/Dockerfile" \
		--platform "linux/$(ARCH)" \
		--push \
		--provenance=false \
		$(foreach t,$(PUSH_TAGS),--tag "$(IMG_REPO):$(t)-$(ARCH)") \
		--build-arg "BUILD_VERSION=$(BUILD_VERSION)" \
		$(BUILDX_EXTRA_ARGS) \
		--metadata-file "$(DIGEST_FILE).json" \
		--progress plain \
		"$(REPO_ROOT)"
	@# containerimage.digest is the manifest buildx just pushed. Read with sed
	@# rather than jq, which the self-hosted runners do not all carry.
	@sed -n 's/.*"containerimage\.digest": *"\([^"]*\)".*/\1/p' "$(DIGEST_FILE).json" > "$(DIGEST_FILE)"
	@test -s "$(DIGEST_FILE)" || { echo "ERROR: no containerimage.digest in $(DIGEST_FILE).json"; exit 1; }
	@echo "pushed $(IMG_REPO)@$$(cat "$(DIGEST_FILE)")"

docker-manifest: check-img-repo ## Stitch <t>-amd64 and <t>-arm64 into <t> under IMG_REPO.
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
		--format '{{.Manifest.Digest}}' > "$(INDEX_DIGEST_FILE)"
	@echo "index $(IMG_REPO)@$$(cat "$(INDEX_DIGEST_FILE)")"
