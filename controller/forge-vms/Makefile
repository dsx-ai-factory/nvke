GO ?= go

REPO_ROOT := $(shell pwd)
BINARY := $(REPO_ROOT)/bin/forge-vms

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches agent/Makefile.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

GOLANGCI_LINT_REPO    ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
SETCAP := sudo setcap cap_net_admin+ep

.PHONY: \
	build build-privileged run run-vm test \
	test-integration-all test-integration-mock test-integration-libvirt test-integration-qemu \
	install-deps-homebrew darwin-qemu-list darwin-qemu-clean \
	lima-libvirt-list lima-libvirt-clean setup teardown fmt lint

REAL_VM_TEST_TIMEOUT ?= 5m
LIMA_VM_NAME ?= forge-vms
RUN_TESTS ?=
GO_TEST_RUN := $(if $(RUN_TESTS),-run "$(RUN_TESTS)")

FORGE_VMS_BASE_IMAGE ?= images/ubuntu-cloud.qcow2

build:
	$(GO) build -o $(BINARY) ./cmd/forge-vms
	$(GO) build -o ./bin/forge-vmsctl ./cmd/forge-vmsctl

# Build + apply CAP_NET_ADMIN (requires sudoers rule — see setup script)
build-privileged: build
	$(SETCAP) $(BINARY)

run:
	FORGE_VMS_LOG_FORMAT=text FORGE_VMS_BASE_IMAGE="$(FORGE_VMS_BASE_IMAGE)" GO="$(GO)" bash ./scripts/run-vm-server.sh

# Build, set capabilities, and run — the primary dev loop on Linux
run-vm: build-privileged
	FORGE_VMS_LOG_FORMAT=text FORGE_VMS_BASE_IMAGE="$(FORGE_VMS_BASE_IMAGE)" $(BINARY)

test:
	$(GO) test ./... -coverprofile=coverage.txt -covermode=count

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

# macOS darwin-qemu direct resource helpers (no API calls).
darwin-qemu-list:
	bash ./scripts/darwin-qemu-machines.sh list

darwin-qemu-clean:
	bash ./scripts/darwin-qemu-machines.sh clean

# macOS libvirt resource helpers inside the Lima VM used by integration tests.
lima-libvirt-list:
	LIMA_VM_NAME="$(LIMA_VM_NAME)" bash ./scripts/lima-libvirt-machines.sh list

lima-libvirt-clean:
	LIMA_VM_NAME="$(LIMA_VM_NAME)" bash ./scripts/lima-libvirt-machines.sh clean

install-deps-homebrew:
	bash ./scripts/install-deps-homebrew.sh

# Linux host setup: install deps + configure bridge/dnsmasq/iptables (then tilt up).
setup:
	bash ./scripts/setup-linux.sh

# Linux host teardown: stop dnsmasq, remove the bridge + its iptables rules, and
# wipe the forge-vms DB. Use to reset, or to migrate to new bridge/VIP ranges:
#   make teardown && make setup && tilt up
teardown:
	bash ./scripts/teardown-linux.sh

fmt:
	$(GO) fmt ./...

lint:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...
