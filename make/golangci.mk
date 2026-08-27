# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Which golangci-lint the modules run, and how they get it.
#
# The pin was repeated in six module Makefiles. Dependabot does not read a
# `go install` argument, so a bump meant six hand edits.
#
# Each module keeps its own lint and fmt recipes and its own flags. Only the
# choice of binary is shared. Include after GOBIN is set.
GO                    ?= go
GOLANGCI_LINT_REPO    ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT_EXE     ?= golangci-lint

# Where golangci-lint keeps its issue cache.
#
# The default is one user-wide directory, so a second checkout of this
# repository replays the first one's cached issues, printed with the paths they
# had there. `make lint` then fails naming files that do not exist in the
# checkout you are standing in, and no message connects that to a cache. It
# survives the other checkout being deleted.
#
# Key the cache to this checkout instead. Derived from this file's own location,
# because each module includes it from a different depth.
GOLANGCI_MK_DIR       := $(dir $(lastword $(MAKEFILE_LIST)))
GOLANGCI_LINT_CACHE   ?= $(abspath $(GOLANGCI_MK_DIR)../.local/golangci-lint)
export GOLANGCI_LINT_CACHE

# Whether the workspace stays on while linting.
#
# lint.yml:47 sets GOWORK=off for the whole workflow and turns it back on for
# controller (:92) and controller/api (:198), which lint against the root
# vendor/ tree. Every other module lints against its own go.mod.
#
# With the workspace on, golangci-lint resolves the whole workspace, reaches the
# INTERNAL cluster-api-provider-nico, and fails with typecheck errors naming an
# unrelated package. It passes wherever that module is already cached, so it
# only showed up on a fresh box. controller/Makefile sets this empty.
GOLANGCI_LINT_GOWORK ?= off

# golangci-lint prints its version without the leading v.
GOLANGCI_LINT_WANT := $(patsubst v%,%,$(GOLANGCI_LINT_VERSION))

# Run the pinned golangci-lint, and install it only when it is absent.
#
# lint and fmt used to run `go install` every time. That cost 136s of a 141s
# `make -C cli lint`, and it overwrote whatever golangci-lint the developer kept
# in GOBIN. PATH is checked first, so a binary from Flox, from Homebrew or from
# an earlier install is used as it stands. Flox is not required.
#
# The block ends in the resolved binary so a recipe appends its own arguments:
#
#     lint:
#             @$(GOLANGCI_LINT_RUN) run --timeout 10m ./...
define GOLANGCI_LINT_RUN
set -e; \
want='$(GOLANGCI_LINT_WANT)'; \
bin=''; \
if [ "$$($(GOLANGCI_LINT_EXE) --version 2>/dev/null | awk '{print $$4}')" = "$$want" ]; then \
  bin="$$(command -v $(GOLANGCI_LINT_EXE))"; \
elif [ "$$('$(GOBIN)/golangci-lint' --version 2>/dev/null | awk '{print $$4}')" = "$$want" ]; then \
  bin='$(GOBIN)/golangci-lint'; \
else \
  echo "  golangci-lint $(GOLANGCI_LINT_VERSION) not found, installing into $(GOBIN)"; \
  $(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION); \
  bin='$(GOBIN)/golangci-lint'; \
fi; \
GOWORK='$(GOLANGCI_LINT_GOWORK)' "$$bin"
endef
