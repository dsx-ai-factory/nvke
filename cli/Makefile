# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The licence header template lives once, at the repository root.
##@ Help

# Same awk as the sibling modules: sections come from ##@ lines and entries from
# the ## comment on each target, so the list is read from this file and cannot go
# stale.
.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

REPO_ROOT ?= $(shell git rev-parse --show-toplevel)

# Module-level targets, matching the other workspace members. The root Makefile
# builds nvkectl into the repository's bin/ as part of `make build`; this file is
# for working inside cli/ alone, and it is what the Lint workflow
# invokes.
#
# Every dependency here is public, so these targets can run with GOWORK=off.

GO ?= go
BINARY := bin/nvkectl

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches
# agent/Makefile and controller/nico-sim/Makefile.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# golangci-lint pin and bootstrap, shared with every other module.
include ../make/golangci.mk
include ../make/coverage.mk
include ../make/tool-versions.mk

# Version stamping, mirroring controller/Makefile.
#
# --match is not optional. This is a monorepo whose tags are per-component
# (agent/v*, controller/v*), so a bare `git describe --tags` returns whichever
# family happens to be nearest and would stamp nvkectl with, say, a controller
# version. That is not hypothetical: hack/capd-kindest-node/build-image.sh
# derives an agent version with an unmatched `git describe` and already
# produces the wrong string.
#
# nvkectl versions apart from the controller, so it gets its own tag family. Go
# requires a submodule's tags to carry its directory, so this module is tagged
# cli/vX.Y.Z. The prefix is stripped, so the binary reports v1.2.3. No cli/v*
# tag exists yet, hence the fallbacks: a plain v* tag, the commit, then v0.0.0.
# The commit says which source a build came from, leaving v0.0.0 to mean a build
# outside a git checkout. --dirty marks a modified tree.
_NVKECTL_VERSION_RAW := $(shell git describe --tags --dirty --match 'cli/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || git describe --always --dirty 2>/dev/null || echo "v0.0.0")
NVKECTL_VERSION  ?= $(patsubst cli/%,%,$(_NVKECTL_VERSION_RAW))
NVKECTL_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
NVKECTL_TREE     ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo clean || echo dirty)
# The commit timestamp, not the wall clock: building the same commit twice
# should produce the same binary.
NVKECTL_DATE     ?= $(shell git log -1 --format=%cI 2>/dev/null || echo "unknown")

# "main", not the full import path. The variables live in package main, and
# for that package the linker matches on the package NAME. Given the import
# path it finds no symbol and silently changes nothing -- no error, no warning,
# just a binary that still reports "dev". Hence the version assertion in
# `make build`.
NVKECTL_PKG := main

RELEASE_LDFLAGS ?= -s -w -buildid= -extldflags -static

NVKECTL_LDFLAGS := \
	-X '$(NVKECTL_PKG).version=$(NVKECTL_VERSION)' \
	-X '$(NVKECTL_PKG).gitCommit=$(NVKECTL_COMMIT)' \
	-X '$(NVKECTL_PKG).gitTreeState=$(NVKECTL_TREE)' \
	-X '$(NVKECTL_PKG).buildDate=$(NVKECTL_DATE)' \
	-X 'github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy.capiVersion=$(CAPI_VERSION)' \
	$(RELEASE_LDFLAGS)

.PHONY: build build-cross test test-e2e-bootstrap test-e2e-bootstrap-nico fmt lint license file-license-check check-no-docker \
	ensure-capd-image ensure-provisioned-provider-image images \
	nodes-up nodes-down nodes-status nodes-hosts nodes-attested

# The platforms shipped on a release. nvkectl is the one binary here a human
# downloads, so it is cross-built; the others are deployed, not fetched.
NVKECTL_PLATFORMS ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DIST ?= dist
# The binary was called nkectl until cli/v0.1.x. For one release build-cross
# also writes each binary under that name, so published download links and
# scripts that know it keep working. Set it empty to stop.
LEGACY_BINARY_NAME ?= nkectl

build-cross: ## Build nvkectl for every NVKECTL_PLATFORMS entry into DIST.
	@mkdir -p $(DIST)
	@# CGO off so each output is a static binary that runs on a bare image, and
	@# so cross-compiling needs no per-target toolchain.
	@for p in $(NVKECTL_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  building $(DIST)/nvkectl-$$os-$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath -ldflags "$(NVKECTL_LDFLAGS)" \
	    -o $(DIST)/nvkectl-$$os-$$arch ./cmd/nvkectl || exit 1; \
	done
	@if [ -n "$(LEGACY_BINARY_NAME)" ]; then \
	  for p in $(NVKECTL_PLATFORMS); do \
	    os=$${p%/*}; arch=$${p#*/}; \
	    cp "$(DIST)/nvkectl-$$os-$$arch" "$(DIST)/$(LEGACY_BINARY_NAME)-$$os-$$arch"; \
	  done; \
	  echo "  also written as $(LEGACY_BINARY_NAME)-<os>-<arch>, for one release"; \
	fi
	@# `make build` proves the -X symbols by running the binary. That cannot be
	@# done for a foreign GOOS, so the host-matching output is executed instead:
	@# every platform shares one NVKECTL_LDFLAGS, so if one is stamped they all
	@# are. Skipped when no built platform matches the host.
	@host="$$($(GO) env GOOS)-$$($(GO) env GOARCH)"; \
	if [ -x "$(DIST)/nvkectl-$$host" ]; then \
	  out="$$($(DIST)/nvkectl-$$host version)"; \
	  case "$$out" in \
	    *" dev "*|*"unknown"*|*"v0.0.0"*) \
	      echo "ERROR: cross build produced an unstamped binary: $$out"; exit 1;; \
	    *) echo "  verified $$host: $$out";; \
	  esac; \
	else \
	  echo "  no host-matching platform in NVKECTL_PLATFORMS; version assertion skipped"; \
	fi

# -buildvcs=false because nvkectl's version comes from the -X symbols in
# NVKECTL_LDFLAGS, asserted just below.
build: ## Build nvkectl into bin/, and assert the version stamp took.
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(NVKECTL_LDFLAGS)" -o $(BINARY) ./cmd/nvkectl
	@# A wrong -X symbol is silently ignored by the linker, so the only way to
	@# know the stamping worked is to ask the binary. Cheap, and it fails loudly.
	@out="$$($(BINARY) version)"; \
	case "$$out" in \
	  *" dev "*|*"unknown"*|*"v0.0.0"*) \
	    echo "ERROR: nvkectl was built without version information: $$out"; \
	    echo "       The -X symbols in NVKECTL_LDFLAGS no longer match the variables in cmd/nvkectl."; \
	    exit 1;; \
	  *) echo "  $$out";; \
	esac

# GO_TEST_FLAGS lets CI add -race without the workflow restating the test
# command, so `make test` stays the one definition of what the tests are.
# controller-test.yml sets it; without this the matrix entry would run without
# the race detector and nothing would say so.
GO_TEST_FLAGS ?=

test: ## Run this module's tests. (run by the merge gate)
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...

# e2e coverage for "nvkectl bootstrap site" specifically -- e2e/bootstrap/ is
# one of what may become several e2e/<command>/ suites as nvkectl grows more
# commands worth covering this way.
#
# Self-contained: no live cluster or external service required, unlike
# controller/test/e2e. Safe for a pull-request lane. See e2e/bootstrap/run.sh
# for what it currently exercises and why it grows in step with bootstrap
# site.
test-e2e-bootstrap: build
	./e2e/bootstrap/run.sh

# The same suite for `infra.provider: nico`: CAPNICo allocates the nodes from a
# nico-sim running on the host, instead of fake hosts being listed in the site
# spec. Needs everything run.sh does, plus a Go toolchain and gcc to build
# nico-sim, network access to ghcr.io, and NVKECTL_NVKE_CONTROLLER_IMAGE set to a
# controller image already in containerd. See e2e/bootstrap/run-nico.sh.
test-e2e-bootstrap-nico: build
	./e2e/bootstrap/run-nico.sh

# Fake-node fixture for the bootstrap e2e suite: nerdctl containers running
# nvke-agent, standing in for pre-provisioned hardware. cli/e2e/bootstrap/substrate
# is a thin derivation of controller/hack/capd-kindest-node's node image
# (nvke-agent, SPIRE/TPM plumbing, kubeadm/reboot shims and registry mirrors
# stay a single source of truth there) plus a small Go package that brings the
# containers up, waits for the agent, and tears them down — see
# e2e/bootstrap/substrate/substrate.go. `nodes-up`/`nodes-down` are also
# invoked by e2e/bootstrap/run.sh (via `make -C`).
CAPD_KINDEST_NODE_IMAGE ?= cwe-capd-kindest-node:local
CAPD_CNI_MANIFESTS_DEB_PATH ?=
CAPD_CNI_CHARTS_DEB_PATH ?=
CAPD_CNI_IMAGES_DEB_PATH ?=
CAPD_HELM_DEB_PATH ?=

# Builds nvke-agent from this checkout's own agent/ module rather than
# downloading a published bundle. The image gets no bundle server. The E2E
# supplies local OVN chart, image, and Helm packages for the controller's post-init CNI
# path. A no-op once the image exists locally.
ensure-capd-image:
	@nerdctl image inspect $(CAPD_KINDEST_NODE_IMAGE) >/dev/null 2>&1 || \
	  CONTAINER_CLI=nerdctl \
	  CAPD_NODE_AGENT_LOCAL_BUILD=true NODE_AGENT_REPO_PATH=$(CURDIR)/../agent \
	  CAPD_CNI_MANIFESTS_DEB_PATH="$(CAPD_CNI_MANIFESTS_DEB_PATH)" \
	  CAPD_CNI_CHARTS_DEB_PATH="$(CAPD_CNI_CHARTS_DEB_PATH)" \
	  CAPD_CNI_IMAGES_DEB_PATH="$(CAPD_CNI_IMAGES_DEB_PATH)" \
	  CAPD_HELM_DEB_PATH="$(CAPD_HELM_DEB_PATH)" \
	  CAPD_KINDEST_NODE_IMAGE=$(CAPD_KINDEST_NODE_IMAGE) \
	  bash ../controller/hack/capd-kindest-node/build-image.sh

# The Cluster API infrastructure provider for hosts that already exist, which is
# what makes a site cluster reconcile: `nvkectl bootstrap site` installs it onto
# the ephemeral bootstrap cluster (cli/internal/bootstrap/controllerdeploy) and loads this image
# into it. image-provisioned-nvcr.yml publishes the image, but this target
# builds it from the checkout regardless: the e2e lane has to exercise the
# provider the pull request changed, not the last one published.
#
# A plain `nerdctl build`, not `make -C ../cluster-api-provider-provisioned
# docker-build`: that target is a multi-arch buildx build into an OCI archive,
# which is what a release wants and not what a local `kind load` can use. The
# build context is the repository root, because the Dockerfile builds from the
# workspace vendor tree.
CAPI_PROVISIONED_IMAGE ?= cluster-api-provider-provisioned:latest
# The tags below are the defaults `bootstrap site` uses when neither the image
# flags nor NVKECTL_NVKE_CONTROLLER_IMAGE / NVKECTL_CAPI_PROVISIONED_IMAGE name an image; the
# bootstrap manifest pins them.
# CONTROLLER_IMAGE is the tag `images` gives the controller image. CWE_IMAGE
# is its deprecated name. When CONTROLLER_IMAGE is unset or empty and
# CWE_IMAGE is set, `images` uses CWE_IMAGE and make prints a warning. When
# both are set, CONTROLLER_IMAGE wins and there is no warning. `override` is
# needed because an empty CONTROLLER_IMAGE= on the command line otherwise
# blocks the assignment. #1399 removes CWE_IMAGE support.
ifneq ($(CWE_IMAGE),)
ifeq ($(CONTROLLER_IMAGE),)
$(warning CWE_IMAGE is deprecated and #1399 removes it; set CONTROLLER_IMAGE instead)
override CONTROLLER_IMAGE := $(CWE_IMAGE)
endif
endif
CONTROLLER_IMAGE ?= nke-controller:latest

# Set by cli/e2e/bootstrap/run.sh to point the images' Docker Hub bases at a
# mirror; see dockerhub_build_args in controller/hack/backend-e2e-lib.sh.
DOCKERHUB_MIRROR_BUILD_ARGS ?=

ensure-provisioned-provider-image:
	@nerdctl image inspect $(CAPI_PROVISIONED_IMAGE) >/dev/null 2>&1 || \
	  nerdctl build -t $(CAPI_PROVISIONED_IMAGE) $(DOCKERHUB_MIRROR_BUILD_ARGS) \
	    -f ../cluster-api-provider-provisioned/Dockerfile ..

# The controller image records the commit it was built from; the Dockerfile
# defaults both to v0.0.0 otherwise.
CONTROLLER_IMAGE_BUILD_ARGS = --build-arg BUILD_VERSION=$(NVKECTL_VERSION) --build-arg GIT_VERSION=$(NVKECTL_COMMIT)

# `images` always rebuilds. An `ensure-*` target keeps a stale image after a
# `git pull`; this target does not. BuildKit's cache keeps a no-op rebuild cheap.
images: ## Build the two images `bootstrap site` side-loads by default.
	nerdctl build -t $(CONTROLLER_IMAGE) $(CONTROLLER_IMAGE_BUILD_ARGS) $(DOCKERHUB_MIRROR_BUILD_ARGS) -f ../controller/Dockerfile ..
	nerdctl build -t $(CAPI_PROVISIONED_IMAGE) $(DOCKERHUB_MIRROR_BUILD_ARGS) -f ../cluster-api-provider-provisioned/Dockerfile ..

nodes-up: ensure-capd-image
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes up

nodes-down:
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes down

nodes-status:
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes status

# TPM attestation stage of the bootstrap e2e. Split in two because
# the stage straddles a `nvkectl bootstrap site` invocation: the thumbprints go
# into the site spec that command reads, and the check runs against the SPIRE
# server it left behind.
#
# `nodes-hosts` writes the node CSV that `nvkectl generate sitespec` takes to
# NVKECTL_E2E_HOSTS_CSV, with each running fixture host's TPM EK thumbprint, and
# prints the shell assignments run.sh sources: the CSV path and the bootstrap
# CA paths. Nothing else goes to stdout.
nodes-hosts:
	@$(GO) run ./e2e/bootstrap/substrate/cmd/nodes hosts

nodes-attested:
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes attested

# golangci-lint applies the formatters enabled in .golangci.yml (gci, gofumpt),
# so there is no separate gofmt step here.
fmt: ## Format the source with golangci-lint's formatters.
	@$(GOLANGCI_LINT_RUN) fmt ./...

# One recipe for CI and local, pinned by version, as in the sibling modules.
lint: check-no-docker ## Run golangci-lint over this module. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@$(ADDLICENSE_VERSION) -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" .

# nvkectl is a redistributed binary, so it carries its own attribution. It reuses
# the controller's generator rather than copying it: the script discovers the
# vendor tree by walking up, and takes the module and its packages from the
# environment, so one implementation serves every module in the workspace.
.PHONY: provider-manifest provider-manifest-check capnico-manifest capnico-manifest-check

.PHONY: notices notices-check
# The provisioned provider's kustomize base, rendered into the binary so nvkectl
# installs it without a checkout. The base is the source of truth and its CRDs
# and RBAC are controller-gen output, so this is regenerated from there rather
# than edited: change the provider, `make -C cluster-api-provider-provisioned
# manifests`, then this.
PROVIDER_KUSTOMIZE := ../cluster-api-provider-provisioned/config/default
PROVIDER_MANIFEST  := internal/bootstrap/controllerdeploy/manifests/provisioned-provider.yaml

provider-manifest: ## Regenerate the embedded provisioned-provider manifest.
	@# Pinned kubectl: the rendered bytes are compared, so the renderer has to
	@# be the same one everywhere or the check below fails on version drift.
	@../hack/check-tool-version.sh kubectl $(KUBECTL_VERSION)
	@{ \
		printf '%s\n' \
			'# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.' \
			'# SPDX-License-Identifier: Apache-2.0' \
			'#' \
			'# GENERATED FILE -- do not edit. Regenerate with `make -C cli provider-manifest`.' \
			'#' \
			'# `kubectl kustomize cluster-api-provider-provisioned/config/default`, rendered' \
			'# here so nvkectl installs the provider of the commit it was built from without' \
			'# a checkout. The source of truth is that kustomize base, whose CRDs and RBAC' \
			'# are themselves controller-gen output; edit the provider, regenerate there,' \
			'# then regenerate this.' \
			'#' \
			'# image.repository is the placeholder controllerdeploy rewrites to the image the' \
			'# bootstrap was told to run.'; \
		kubectl kustomize $(PROVIDER_KUSTOMIZE); \
	} > $(PROVIDER_MANIFEST)
	@echo "  wrote $(PROVIDER_MANIFEST)"

# The CAPNICo manifest the bootstrap installs when the site spec selects
# infra.provider nico. deploy/nico/capnico/capnico.yaml is the source of truth:
# it is the manifest the management cluster's own deploy path installs, with its one
# edit (the manager's args, which set the reboot/repair annotation prefixes the
# controller writes), and the
# bootstrap installs the same bytes so the two cannot pin different CAPNICos.
# The copy is embedded in nvkectl, because go:embed cannot reach outside this
# module and the bootstrap has to work from the binary alone, without a checkout.
CAPNICO_SOURCE   := ../deploy/nico/capnico/capnico.yaml
CAPNICO_MANIFEST := internal/bootstrap/controllerdeploy/manifests/capnico.yaml
# The stack directory's copy is a template: the image line takes the
# capnico-controller reference from the artifact file.
CAPNICO_TEMPLATE := internal/stackinstall/stack/manifests/capnico.yaml.tmpl
CAPNICO_TEMPLATE_SED := 's|^\( *image: \)ghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller[:@].*$$|\1{{ .CAPNICoImage }}|'
CAPNICO_PLACEHOLDERS = $$(grep -c '{{ .CAPNICoImage }}' $(1))
# The template drops the source's header, which describes the deploy path, and
# carries its own. The manifest body starts at the first line that is not a
# comment or blank.
define CAPNICO_TEMPLATE_RENDER
{ sed -n '1,2p' $(CAPNICO_SOURCE); \
  printf '%s\n' '#' \
    '# CAPNICo, its released infrastructure-components.yaml with the image taken' \
    '# from the artifact file. Generated from deploy/nico/capnico/capnico.yaml by' \
    '# make -C cli capnico-manifest. Do not edit.' ''; \
  awk 'f || (!/^#/ && !/^$$/) { f = 1; print }' $(CAPNICO_SOURCE) | sed $(CAPNICO_TEMPLATE_SED); }
endef

capnico-manifest: ## Copy the deploy path's CAPNICo manifest into the embedded manifests.
	@$(CAPNICO_TEMPLATE_RENDER) > $(CAPNICO_TEMPLATE).tmp || { rm -f $(CAPNICO_TEMPLATE).tmp; exit 1; }
	@test "$(call CAPNICO_PLACEHOLDERS,$(CAPNICO_TEMPLATE).tmp)" = 1 \
		|| { rm -f $(CAPNICO_TEMPLATE).tmp; echo "ERROR: expected exactly one CAPNICo controller image line in $(CAPNICO_SOURCE)"; exit 1; }
	@mv $(CAPNICO_TEMPLATE).tmp $(CAPNICO_TEMPLATE)
	@echo "  wrote $(CAPNICO_TEMPLATE)"
	@cp $(CAPNICO_SOURCE) $(CAPNICO_MANIFEST)
	@echo "  wrote $(CAPNICO_MANIFEST)"

capnico-manifest-check: ## Verify the embedded CAPNICo manifests are the deploy path's.
	@git ls-files --error-unmatch $(CAPNICO_MANIFEST) $(CAPNICO_TEMPLATE) > /dev/null 2>&1 \
		|| { echo "$(CAPNICO_MANIFEST) or $(CAPNICO_TEMPLATE) is not tracked by git. Run 'make -C cli capnico-manifest' and commit them."; exit 1; }
	@cmp -s $(CAPNICO_SOURCE) $(CAPNICO_MANIFEST) \
		|| { echo "ERROR: $(CAPNICO_MANIFEST) differs from deploy/nico/capnico/capnico.yaml. Run 'make -C cli capnico-manifest' and commit it."; exit 1; }
	@$(CAPNICO_TEMPLATE_RENDER) | cmp -s - $(CAPNICO_TEMPLATE) \
		|| { echo "ERROR: $(CAPNICO_TEMPLATE) differs from deploy/nico/capnico/capnico.yaml with the image templated. Run 'make -C cli capnico-manifest' and commit it."; exit 1; }
	@test "$(call CAPNICO_PLACEHOLDERS,$(CAPNICO_TEMPLATE))" = 1 \
		|| { echo "ERROR: $(CAPNICO_TEMPLATE) must template exactly one image line."; exit 1; }
	@echo "  $(CAPNICO_MANIFEST) and $(CAPNICO_TEMPLATE) are up to date."

provider-manifest-check: capnico-manifest-check ## Verify the embedded provider manifests are up to date.
	@# Tracking first, as notices-check does: git diff says nothing about an
	@# untracked path, so a never-committed manifest would pass silently.
	@git ls-files --error-unmatch $(PROVIDER_MANIFEST) > /dev/null 2>&1 \
		|| { echo "$(PROVIDER_MANIFEST) is not tracked by git. Run 'make -C cli provider-manifest' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory provider-manifest
	@git diff --exit-code -- $(PROVIDER_MANIFEST) \
		|| { echo "ERROR: $(PROVIDER_MANIFEST) is stale. Run 'make -C cli provider-manifest' and commit it."; exit 1; }
	@echo "  $(PROVIDER_MANIFEST) is up to date."

.PHONY: docs docs-check
# The nvkectl command reference: one page per public command, generated from the
# same cobra tree the binary runs. hack/gendocs builds that tree and reads its
# metadata -- it runs no command, so this needs no credential, kubeconfig,
# configured site or reachable cluster, and nothing beyond the module cache.
#
# Deterministic by construction: the generator clears the NVKECTL_ and NKECTL_
# variables before building the tree, because two of `bootstrap site`'s flag
# defaults are read from the environment when the command is constructed, and
# it switches off cobra's generated-on timestamp. See cli/internal/clidocs.
DOCS_DIR := docs/reference

docs: ## Regenerate the nvkectl command reference under docs/reference.
	@$(GO) run ./hack/gendocs -output $(DOCS_DIR)

docs-check: ## Verify the nvkectl command reference is up to date. (run by the merge gate)
	@# Tracking first, as provider-manifest-check does: git diff says nothing
	@# about an untracked path, so a reference that was never committed would
	@# otherwise pass this check in silence.
	@git ls-files --error-unmatch $(DOCS_DIR)/README.md > /dev/null 2>&1 \
		|| { echo "ERROR: $(DOCS_DIR) is not tracked by git. Run 'make -C cli docs' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory docs
	@# `docs` empties the directory before writing, so a page for a command
	@# that no longer exists is not rewritten and shows up here as a deletion.
	@# That is the obsolete-page case; a changed page is a modification.
	@git diff --exit-code --stat -- $(DOCS_DIR) \
		|| { echo "ERROR: the nvkectl reference is stale. Run 'make -C cli docs' and commit $(DOCS_DIR)."; exit 1; }
	@# And the case git diff cannot see: a page for a NEW command, generated
	@# just now and never added. Without this a new command reaches main
	@# undocumented, with every check green.
	@untracked=$$(git ls-files --others --exclude-standard -- $(DOCS_DIR)); \
	if [ -n "$$untracked" ]; then \
		echo "ERROR: the nvkectl reference has pages that are not tracked by git:"; \
		printf '%s\n' "$$untracked" | sed 's/^/    /'; \
		echo "Run 'make -C cli docs' and commit $(DOCS_DIR)."; \
		exit 1; \
	fi
	@echo "  the nvkectl reference matches the command tree."

notices: ## Regenerate THIRD_PARTY_NOTICES.md for the nvkectl binary.
	@# The tool and the script both live in controller/, which is a sibling now,
	@# not the parent. `go-licenses`, not `bin/go-licenses`: the real rule's
	@# target is an absolute path, so a relative name does not match it.
	@$(MAKE) --no-print-directory -C ../controller go-licenses
	@MODULE_ROOT="$(CURDIR)" \
		OUTPUT="$(CURDIR)/THIRD_PARTY_NOTICES.md" \
		PACKAGES="./cmd/nvkectl" \
		GO_LICENSES="$(CURDIR)/../controller/bin/go-licenses" \
		bash ../controller/hack/generate-notices

notices-check: ## Verify THIRD_PARTY_NOTICES.md is up to date. (advisory; not in the merge gate)
	@# git diff says nothing about an untracked path, so check tracking first --
	@# otherwise a deleted or never-committed notices file passes silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md > /dev/null 2>&1 \
		|| { echo "THIRD_PARTY_NOTICES.md is not tracked by git. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory notices
	@if ! git diff --exit-code -- THIRD_PARTY_NOTICES.md > /dev/null 2>&1; then \
		echo "THIRD_PARTY_NOTICES.md is out of date. Please run 'make notices' locally and commit the changes."; \
		git diff --name-only -- THIRD_PARTY_NOTICES.md; \
		exit 1; \
	fi

# The name matters beyond this file. license.yml builds its header-check matrix
# by selecting the modules whose Makefile defines a `file-license-check:`
# target, so a module without one is silently absent from that matrix rather
# than reported as unchecked. Expects a clean working directory.
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi

# The bootstrap e2e suite runs entirely on nerdctl/containerd. Docker is still
# installed on the CI runner, though, so a reintroduced `docker` call would
# work there and pass unnoticed -- the suite would quietly depend on Docker
# again without anyone choosing that. This is the check that keeps it honest.
#
# Invocations only: the word appears legitimately in Dockerfile paths, in
# docker.io registry references, and in comments explaining why Docker is not
# used here. A command position -- bare or by absolute path, followed by an
# argument, end of line, or a line continuation -- is what is matched, so any
# subcommand counts, including ones that did not exist when this was written.
#
# Shell and Makefile only. Go sources are covered by TestNoDockerInvocations
# in the substrate package, which understands Go string literals; a text
# grep over Go would match that test's own error message, among other prose.
# GNU grep applies --include to command-line file names as well as to files
# found while recursing, so the Makefile has to be included by name or the
# explicit argument is silently skipped.
#
# The second alternation is the assignment-then-expand form -- `CLI=docker`,
# `CLI:=docker`, `CLI='/usr/bin/docker'`, then `$CLI build` -- and the third
# is the parameter-expansion default, `${CLI:-docker}`; the first alternation
# sees neither. A grep is a heuristic either way; the runtime removal in the
# workflow is the backstop for whatever shape this does not anticipate.
#
# Also a dependency of `lint`, which the merge gate runs for this module: a
# reintroduction then blocks a merge rather than only reddening the advisory
# e2e lane.
check-no-docker: ## Fail if the bootstrap e2e suite invokes docker.
	@matches=$$(grep -rnE '(^|[;&|(]|[[:space:]])(sudo[[:space:]]+)?(/[A-Za-z0-9_./-]*/)?docker([[:space:]]|\\$$|$$)|(^|[[:space:]])[A-Za-z_][A-Za-z0-9_]*[:?+]?=['\''"]?(/[A-Za-z0-9_./-]*/)?docker(['\''"]|[[:space:];]|$$)|\$$\{[A-Za-z_][A-Za-z0-9_]*:?[-=+]['\''"]?(/[A-Za-z0-9_./-]*/)?docker['\''"]?\}' \
	    --include='*.sh' --include='Makefile' e2e/ Makefile 2>/dev/null | grep -v '^[^:]*:[0-9]*:[[:space:]]*#' || true); \
	if [ -n "$$matches" ]; then \
	  echo "The bootstrap e2e suite must not invoke docker; it runs on nerdctl/containerd."; \
	  echo "$$matches"; \
	  exit 1; \
	fi; \
	echo "bootstrap e2e suite: no Docker invocations."
