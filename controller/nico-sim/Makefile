# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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

GO ?= go

REPO_ROOT := $(shell pwd)
BINARY := $(REPO_ROOT)/bin/nico-sim

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches agent/Makefile.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

# ADDLICENSE_VERSION comes from make/tool-versions.mk, which owns it.
include ../../make/tool-versions.mk
# golangci-lint pin and bootstrap, shared with every other module.
include ../../make/golangci.mk
include ../../make/coverage.mk
ifeq ($(shell uname -s),Darwin)
SETCAP := true
else
SETCAP := sudo setcap cap_net_admin+ep
endif

.PHONY: \
	build build-static build-privileged run run-vm test \
	test-integration-all test-integration-mock test-integration-libvirt test-integration-qemu \
	install-deps-homebrew vms vm-delete vms-clean vm-stop vm-start vms-stop vms-start \
	lima-libvirt-list lima-libvirt-clean setup teardown fmt lint \
	nico-vms-up nico-vms-test nico-vms-test-cluster

REAL_VM_TEST_TIMEOUT ?= 5m
LIMA_VM_NAME ?= nico-vms
RUN_TESTS ?=
GO_TEST_RUN := $(if $(RUN_TESTS),-run "$(RUN_TESTS)")

NICO_VMS_BASE_IMAGE ?= images/ubuntu-cloud.qcow2

build: ## Build this module's binaries.
	$(GO) build -o $(BINARY) ./cmd/nico-sim
	$(GO) build -o ./bin/nico-simctl ./cmd/nico-simctl

build-static: ## Build a static nico-sim binary for minimal Linux containers.
	CGO_ENABLED=1 $(GO) build -tags "netgo osusergo" \
		-ldflags "-linkmode external -extldflags -static" \
		-o $(BINARY) ./cmd/nico-sim

# Build + apply CAP_NET_ADMIN (requires sudoers rule — see setup script)
build-privileged: build
	$(SETCAP) $(BINARY)

run:
	NICO_SIM_LOG_FORMAT=text NICO_VMS_BASE_IMAGE="$(NICO_VMS_BASE_IMAGE)" GO="$(GO)" bash ./scripts/run-vm-server.sh

# Build, set capabilities, and run — the primary dev loop on Linux
run-vm: build-privileged
	NICO_SIM_LOG_FORMAT=text NICO_VMS_BASE_IMAGE="$(NICO_VMS_BASE_IMAGE)" $(BINARY)

# GO_TEST_FLAGS lets CI add -race without the workflow having to restate the
# test command, so `make test` stays the one definition of what "the tests" are.
GO_TEST_FLAGS ?=

test: ## Run this module's tests. (run by the merge gate)
	@$(COVERAGE_MKDIR)
	$(GO) test $(GO_TEST_FLAGS) $(GO_COVER_FLAGS) ./...

# All backends (mock + real VMs) in a single invocation.
#Linux runs libvirt; macOS runs qemu + libvirt-via-lima.
test-integration-all:
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		$(GO) test -tags=integration -v -count=1 -timeout "$(REAL_VM_TEST_TIMEOUT)" $(GO_TEST_RUN) \
			./test/integration/mock ./test/integration/qemu ./test/integration/libvirt; \
	else \
		$(GO) test -tags=integration -v -count=1 -timeout "$(REAL_VM_TEST_TIMEOUT)" $(GO_TEST_RUN) \
			./test/integration/mock ./test/integration/libvirt; \
	fi

# Mock-backend integration tests (fast, no VMs).
test-integration-mock:
	$(GO) test ./test/integration/mock -v $(GO_TEST_RUN)

# Linux local libvirt path. On darwin routes through Lima.
test-integration-libvirt:
	$(GO) test ./test/integration/libvirt -tags=integration -v -count=1 -timeout "$(REAL_VM_TEST_TIMEOUT)" $(GO_TEST_RUN)

# macOS direct qemu path.
test-integration-qemu:
	@if [ "$$(uname -s)" != "Darwin" ]; then \
		echo "test-integration-vm-qemu is darwin-only"; \
		exit 1; \
	fi
	$(GO) test ./test/integration/qemu -tags=integration -v -count=1 -timeout "$(REAL_VM_TEST_TIMEOUT)" $(GO_TEST_RUN)

# macOS libvirt resource helpers inside the Lima VM used by integration tests.
lima-libvirt-list:
	LIMA_VM_NAME="$(LIMA_VM_NAME)" bash ./scripts/lima-libvirt-machines.sh list

lima-libvirt-clean:
	LIMA_VM_NAME="$(LIMA_VM_NAME)" bash ./scripts/lima-libvirt-machines.sh clean

install-deps-homebrew:
	bash ./scripts/install-deps-homebrew.sh

##@ VM management (macOS and Linux)

vms: ## List VMs: nico-sim's view joined with the host's, orphans flagged.
	bash ./scripts/vms.sh list

vm-delete: ## Delete VMs by name or ID: make vm-delete VM="a b".
	bash ./scripts/vms.sh delete $(VM)

vms-clean: ## Delete orphaned VMs that nico-sim no longer knows about.
	bash ./scripts/vms.sh clean

vm-stop: ## Stop VMs, keeping their disks: make vm-stop VM="a b".
	bash ./scripts/vms.sh stop $(VM)

vm-start: ## Start stopped VMs: make vm-start VM="a b".
	bash ./scripts/vms.sh start $(VM)

vms-stop: ## Stop every VM.
	bash ./scripts/vms.sh stop --all

vms-start: ## Start every stopped VM.
	bash ./scripts/vms.sh start --all

##@ nico-sim (macOS)

nico-vms-up: ## Run the Mac VM simulator against IMAGE (foreground).
	ARCH="$(ARCH)" IMAGE="$(IMAGE)" bash ./scripts/local-mac.sh up

nico-vms-test: ## Test IMAGE with isolated Mac HVF VMs.
	ARCH="$(ARCH)" IMAGE="$(IMAGE)" bash ./scripts/local-mac.sh test

nico-vms-test-cluster: ## Boot IMAGE in two Mac VMs and test its staged Kubernetes/CNI packages.
	ARCH="$(ARCH)" IMAGE="$(IMAGE)" bash ./scripts/local-mac.sh test-cluster

# Linux host setup: install deps + configure bridge/dnsmasq/iptables (then tilt up).
setup:
	bash ./scripts/setup-linux.sh

# Linux host teardown: stop dnsmasq, remove the bridge + its iptables rules, and
# wipe the nico-sim DB. Use to reset, or to migrate to new bridge/VIP ranges:
#   make teardown && make setup && tilt up
teardown:
	bash ./scripts/teardown-linux.sh

fmt: ## Format the source with go fmt.
	$(GO) fmt ./...

lint: ## Run golangci-lint over this module. (run by the merge gate)
	@$(GOLANGCI_LINT_RUN) run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@$(ADDLICENSE_VERSION) -f $(REPO_ROOT)/hack/license-header.tmpl -c "NVIDIA CORPORATION & AFFILIATES" \
		-ignore 'ui/vendor/**' \
		-ignore 'test/integration/testutil/testdata/**' \
		-ignore '.dev/**' \
		.

# The target name is load-bearing beyond this file. license.yml builds its
# header-check matrix by selecting modules whose Makefile defines
# `file-license-check:`, so a module without one is silently absent from that
# matrix rather than reported as unchecked. Headers here were already correct,
# enforced incidentally because controller's addlicense walks the filesystem and
# does not know about module boundaries -- this makes the coverage explicit and
# survives someone adding an ignore glob upstream. Expects a clean tree.
.PHONY: license file-license-check
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi
