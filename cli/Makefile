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
# builds nkectl into the repository's bin/ as part of `make build`; this file is
# for working inside cli/ alone, and it is what the Lint workflow
# invokes.
#
# Every dependency here is public, so these targets need neither the workspace
# nor the committed vendor/ tree, and CI runs them with GOWORK=off.

GO ?= go
BINARY := bin/nkectl

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches
# agent/Makefile and controller/nico-sim/Makefile.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# golangci-lint pin and bootstrap, shared with every other module.
include ../make/golangci.mk
include ../make/tool-versions.mk

# Version stamping, mirroring controller/Makefile.
#
# --match is not optional. This is a monorepo whose tags are per-component
# (agent/v*, controller/v*), so a bare `git describe --tags` returns whichever
# family happens to be nearest and would stamp nkectl with, say, a controller
# version. That is not hypothetical: hack/capd-kindest-node/build-image.sh
# derives an agent version with an unmatched `git describe` and already
# produces the wrong string.
#
# nkectl versions apart from the controller, so it gets its own tag family. Go
# requires a submodule's tags to carry its directory, so this module is tagged
# cli/vX.Y.Z. The prefix is stripped, so the binary reports v1.2.3. No cli/v*
# tag exists yet, hence the fallbacks: a plain v* tag, the commit, then v0.0.0.
# The commit says which source a build came from, leaving v0.0.0 to mean a build
# outside a git checkout. --dirty marks a modified tree.
_NKECTL_VERSION_RAW := $(shell git describe --tags --dirty --match 'cli/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || git describe --always --dirty 2>/dev/null || echo "v0.0.0")
NKECTL_VERSION  ?= $(patsubst cli/%,%,$(_NKECTL_VERSION_RAW))
NKECTL_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
NKECTL_TREE     ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo clean || echo dirty)
# The commit timestamp, not the wall clock: building the same commit twice
# should produce the same binary.
NKECTL_DATE     ?= $(shell git log -1 --format=%cI 2>/dev/null || echo "unknown")

# "main", not the full import path. The variables live in package main, and
# for that package the linker matches on the package NAME. Given the import
# path it finds no symbol and silently changes nothing -- no error, no warning,
# just a binary that still reports "dev". Hence the version assertion in
# `make build`.
NKECTL_PKG := main

RELEASE_LDFLAGS ?= -s -w -buildid= -extldflags -static

NKECTL_LDFLAGS := \
	-X '$(NKECTL_PKG).version=$(NKECTL_VERSION)' \
	-X '$(NKECTL_PKG).gitCommit=$(NKECTL_COMMIT)' \
	-X '$(NKECTL_PKG).gitTreeState=$(NKECTL_TREE)' \
	-X '$(NKECTL_PKG).buildDate=$(NKECTL_DATE)' \
	-X 'github.com/NVIDIA/nke/cli/internal/cwedeploy.capiVersion=$(CAPI_VERSION)' \
	$(RELEASE_LDFLAGS)

.PHONY: build build-cross test test-e2e-bootstrap fmt lint license file-license-check check-no-docker \
	ensure-capd-image ensure-provisioned-provider-image \
	nodes-up nodes-down nodes-status nodes-hosts nodes-attested

# The platforms shipped on a release. nkectl is the one binary here a human
# downloads, so it is cross-built; the others are deployed, not fetched.
NKECTL_PLATFORMS ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DIST ?= dist

build-cross: ## Build nkectl for every NKECTL_PLATFORMS entry into DIST.
	@mkdir -p $(DIST)
	@# CGO off so each output is a static binary that runs on a bare image, and
	@# so cross-compiling needs no per-target toolchain.
	@for p in $(NKECTL_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  building $(DIST)/nkectl-$$os-$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath -ldflags "$(NKECTL_LDFLAGS)" \
	    -o $(DIST)/nkectl-$$os-$$arch ./cmd/nkectl || exit 1; \
	done
	@# `make build` proves the -X symbols by running the binary. That cannot be
	@# done for a foreign GOOS, so the host-matching output is executed instead:
	@# every platform shares one NKECTL_LDFLAGS, so if one is stamped they all
	@# are. Skipped when no built platform matches the host.
	@host="$$($(GO) env GOOS)-$$($(GO) env GOARCH)"; \
	if [ -x "$(DIST)/nkectl-$$host" ]; then \
	  out="$$($(DIST)/nkectl-$$host version)"; \
	  case "$$out" in \
	    *" dev "*|*"unknown"*|*"v0.0.0"*) \
	      echo "ERROR: cross build produced an unstamped binary: $$out"; exit 1;; \
	    *) echo "  verified $$host: $$out";; \
	  esac; \
	else \
	  echo "  no host-matching platform in NKECTL_PLATFORMS; version assertion skipped"; \
	fi

# -buildvcs=false because nkectl's version comes from the -X symbols in
# NKECTL_LDFLAGS, asserted just below. See the note on work-check-offline in the
# root Makefile.
build: ## Build nkectl into bin/, and assert the version stamp took.
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(NKECTL_LDFLAGS)" -o $(BINARY) ./cmd/nkectl
	@# A wrong -X symbol is silently ignored by the linker, so the only way to
	@# know the stamping worked is to ask the binary. Cheap, and it fails loudly.
	@out="$$($(BINARY) version)"; \
	case "$$out" in \
	  *" dev "*|*"unknown"*|*"v0.0.0"*) \
	    echo "ERROR: nkectl was built without version information: $$out"; \
	    echo "       The -X symbols in NKECTL_LDFLAGS no longer match the variables in cmd/nkectl."; \
	    exit 1;; \
	  *) echo "  $$out";; \
	esac

# GO_TEST_FLAGS lets CI add -race without the workflow restating the test
# command, so `make test` stays the one definition of what the tests are.
# controller-test.yml sets it; without this the matrix entry would run without
# the race detector and nothing would say so.
GO_TEST_FLAGS ?=

test: ## Run this module's tests. (run by the merge gate)
	$(GO) test $(GO_TEST_FLAGS) ./...

# e2e coverage for "nkectl bootstrap site" specifically -- e2e/bootstrap/ is
# one of what may become several e2e/<command>/ suites as nkectl grows more
# commands worth covering this way.
#
# Self-contained: no live cluster or external service required, unlike
# controller/test/e2e. Safe for a pull-request lane. See e2e/bootstrap/run.sh
# for what it currently exercises and why it grows in step with bootstrap
# site.
test-e2e-bootstrap: build
	./e2e/bootstrap/run.sh

# Fake-node fixture for the bootstrap e2e suite: nerdctl containers running
# nke-agent, standing in for pre-provisioned hardware. cli/e2e/bootstrap/substrate
# is a thin derivation of controller/hack/capd-kindest-node's node image
# (nke-agent, SPIRE/TPM plumbing, kubeadm/reboot shims and registry mirrors
# stay a single source of truth there) plus a small Go package that brings the
# containers up, waits for the agent, and tears them down — see
# e2e/bootstrap/substrate/substrate.go. `nodes-up`/`nodes-down` are also
# invoked by e2e/bootstrap/run.sh (via `make -C`).
CAPD_KINDEST_NODE_IMAGE ?= cwe-capd-kindest-node:local
CAPD_CNI_MANIFESTS_DEB_PATH ?=

# Builds nke-agent from this checkout's own agent/ module rather than
# downloading a published bundle. The image gets no bundle server. The E2E
# supplies a local Calico manifests package for CWE's post-init CNI path. A
# no-op once the image exists locally.
ensure-capd-image:
	@nerdctl image inspect $(CAPD_KINDEST_NODE_IMAGE) >/dev/null 2>&1 || \
	  CONTAINER_CLI=nerdctl \
	  CAPD_NODE_AGENT_LOCAL_BUILD=true NODE_AGENT_REPO_PATH=$(CURDIR)/../agent \
	  CAPD_CNI_MANIFESTS_DEB_PATH="$(CAPD_CNI_MANIFESTS_DEB_PATH)" \
	  CAPD_KINDEST_NODE_IMAGE=$(CAPD_KINDEST_NODE_IMAGE) \
	  bash ../controller/hack/capd-kindest-node/build-image.sh

# The Cluster API infrastructure provider for hosts that already exist, which is
# what makes a site cluster reconcile: `nkectl bootstrap site` installs it onto
# the ephemeral bootstrap cluster (cli/internal/cwedeploy) and loads this image
# into it. image-provisioned-nvcr.yml publishes the image, but this target
# builds it from the checkout regardless: the e2e lane has to exercise the
# provider the pull request changed, not the last one published.
#
# A plain `nerdctl build`, not `make -C ../cluster-api-provider-provisioned
# docker-build`: that target is a multi-arch buildx build into an OCI archive,
# which is what a release wants and not what a local `kind load` can use. The
# build context is the repository root, because the Dockerfile builds from the
# workspace vendor tree.
CAPI_PROVISIONED_IMAGE ?= cluster-api-provider-provisioned:local

ensure-provisioned-provider-image:
	@nerdctl image inspect $(CAPI_PROVISIONED_IMAGE) >/dev/null 2>&1 || \
	  nerdctl build -t $(CAPI_PROVISIONED_IMAGE) \
	    -f ../cluster-api-provider-provisioned/Dockerfile ..

nodes-up: ensure-capd-image
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes up

nodes-down:
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes down

nodes-status:
	$(GO) run ./e2e/bootstrap/substrate/cmd/nodes status

# TPM attestation stage of the bootstrap e2e. Split in two because
# the stage straddles a `nkectl bootstrap site` invocation: the thumbprints go
# into the site spec that command reads, and the check runs against the SPIRE
# server it left behind.
#
# `nodes-hosts` writes the node CSV that `nkectl generate sitespec` takes to
# NKECTL_E2E_HOSTS_CSV, with each running fixture host's TPM EK thumbprint, and
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
	$(GO) run github.com/google/addlicense@v1.1.1 -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" .

# nkectl is a redistributed binary, so it carries its own attribution. It reuses
# the controller's generator rather than copying it: the script discovers the
# vendor tree by walking up, and takes the module and its packages from the
# environment, so one implementation serves every module in the workspace.
.PHONY: notices notices-check
notices: ## Regenerate THIRD_PARTY_NOTICES.md for the nkectl binary.
	@# The tool and the script both live in controller/, which is a sibling now,
	@# not the parent. `go-licenses`, not `bin/go-licenses`: the real rule's
	@# target is an absolute path, so a relative name does not match it.
	@$(MAKE) --no-print-directory -C ../controller go-licenses
	@MODULE_ROOT="$(CURDIR)" \
		OUTPUT="$(CURDIR)/THIRD_PARTY_NOTICES.md" \
		PACKAGES="./cmd/nkectl" \
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
