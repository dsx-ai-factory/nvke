# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Root Makefile: manages the Go workspace and builds the commands in it.
# The per-module Makefiles under agent/ and controller/ are unchanged and still
# work on their own. This one only covers what spans modules.
#
# See docs/go-workspace.md.

# GNU Make 4.0+ is required. macOS ships 3.81, which fails on cmd_version below
# with "unterminated call to function `shell'" and never says why. Fail here
# instead, and say what to install.
# filter-out, not `4.%`: this stays correct at Make 5.
ifeq ($(filter-out 0 1 2 3,$(firstword $(subst ., ,$(MAKE_VERSION)))),)
$(error GNU Make 4.0+ is required, but this is GNU Make $(MAKE_VERSION). On macOS: `brew install make`, then use `gmake` in place of `make`. See CONTRIBUTING.md)
endif

# The docs teach `GOWORK=off` for the modules outside the workspace. Inherited
# here it would make every workspace target fail with "no go.work file found",
# so it is dropped from the environment the recipes pass on.
unexport GOWORK

# go.work owns the Go version. strcase (vendored via teleport/api) asserts its
# Unicode tables match the stdlib's, so a toolchain newer than the `go`
# directive panics at init -- controller/Makefile:7 has pinned against that
# for a while. Exporting it here extends the same pin to every module this
# Makefile delegates to. An explicit GOTOOLCHAIN, as CI sets, still wins.
GOTOOLCHAIN ?= go$(shell awk '/^go /{print $$2; exit}' $(CURDIR)/go.work)
export GOTOOLCHAIN

GO      ?= go
BIN_DIR ?= $(CURDIR)/bin

# One template for the whole repository. Every module's `make license` reads
# this file, so the header text has a single definition and cannot drift
# between modules -- which is how four of the five modules ended up stamping a
# proprietary identifier into a repository that ships Apache-2.0.
LICENSE_HOLDER   ?= NVIDIA CORPORATION & AFFILIATES
LICENSE_TEMPLATE ?= $(CURDIR)/hack/license-header.tmpl
ADDLICENSE       ?= $(GO) run github.com/google/addlicense@v1.1.1

# The files that carry a licence header.
#
# Dockerfiles are absent deliberately: addlicense keys off the file extension
# and would give Dockerfile.proto C-style `//` comments, which Docker cannot
# parse.
#
# The exclusions cover vendored and generated trees, testdata, fixtures, build
# tooling, and the chart templates the chart ships. Files under hack/ are
# deliberately included: they are source and must carry the repository licence.
LICENSE_FILES = git ls-files -- '*.go' '*.sh' '*.py' '*.js' '*.css' '*.html' '*.yaml' '*.yml' \
	| grep -vE '(^|/)vendor/' \
	| grep -vE '(^|/)testdata/' \
	| grep -vE '^generated/' \
	| grep -vE '\.pb\.go$$' \
	| grep -vE '^controller/deploy/charts/' \
	| grep -vE '^controller/test/fixtures/'

# Modules in go.work — every module in the repository. Building needs no
# NVIDIA credentials: the one still-INTERNAL dependency
# (github.com/dsx-ai-factory/cluster-api-provider-nico) is carried in the committed
# vendor/ tree, so `build` gives an outside contributor the same result as CI.
# Re-vendoring (inside `work-sync`) is the only step that needs access to it.
WORK_MODULES := agent \
                cluster-api-provider-provisioned \
                controller \
                controller/api \
                controller/deploy/charts \
                controller/nico-sim \
                controller/hack/capd-reboot-controller \
                cli \
                controller/test/e2e \
                management-engine \
                os-image

# Modules whose commands `build` can compile with `go build -o bin/ ./cmd/...`,
# which names each binary after its package directory. controller/api and
# controller/test/e2e hold library and test code with no ./cmd. The agent's
# samples/ and sample-clients/ are package main but are examples, not artefacts.
#
# controller does not belong here: its command is ./cmd/worker, but the binary
# it ships is named controller, so this loop would produce the wrong
# name. `build` delegates to controller/Makefile for it. controller/nico-sim
# ships through its own Makefile and the container image.
#
# cli is absent because its Makefile stamps four symbols, not one,
# and asserts the stamp took. `build` delegates to cli/Makefile.
CMD_MODULES := agent \
               controller/hack/capd-reboot-controller \
               os-image

# The version stamped into a CMD_MODULES binary. First hit wins: the module's own
# tag series, a repo-wide one, the commit, then v0.0.0 outside a git checkout.
# The hack tools have no tags, so they get the commit.
#
# --match is required, on EVERY describe including the --always one. This repo
# has several tag series, and a describe without --match returns the nearest one
# from any of them.
#
# --always only degrades to a bare commit when no tag describes HEAD, and in
# this repository some tag always does. An unmatched --always therefore returns
# another component's tag. ${v##*/} (from #216) strips whichever prefix came
# back, so a hack tool reported v0.2.1-rc.2-6-g… — an agent version, just
# without the agent/ prefix — and version-check could not catch it. Scoping
# --always with --match makes that step a commit when the module has no tags
# of its own.
cmd_version = $(shell \
    v=$$(git describe --tags --dirty --match '$(notdir $(1))/v[0-9]*' 2>/dev/null \
      || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null \
      || git describe --always --dirty --match '$(notdir $(1))/v[0-9]*' 2>/dev/null \
      || echo v0.0.0); \
    printf '%s' "$${v##*/}")

# Both spellings: agent declares VERSION, the hack tools declare version. The
# linker ignores an -X for a symbol the binary lacks, so passing both is safe.
# It ignores it silently, which is why version-check exists.
cmd_ldflags = -X 'main.version=$(call cmd_version,$(1))' \
              -X 'main.VERSION=$(call cmd_version,$(1))' \
              $(RELEASE_LDFLAGS)

RELEASE_LDFLAGS ?= -s -w -buildid= -extldflags -static

# Must match WORKER_BIN_NAME in controller/Makefile. `build` fails if it does not.
CWE_BIN_NAME ?= controller

# No module is excluded from the workspace any more; kept (empty) because
# work-check accounts for every go.mod in the tree against these two lists.
PRIVATE_MODULES :=

# Every place an artefact ships LICENSE and NOTICE from.
#
# The root files cannot be COPYed into the controller image directly: that image
# builds with `controller/` as its context, and Docker cannot reach outside a
# build context. So the files are copied to a location inside it. The Helm chart
# has the same constraint and the same answer.
#
# The chart copies existed before this and nothing maintained them -- they were
# committed by hand and happened to match. `licenses-check` now enforces it.
LICENSE_DESTS := controller/licenses \
                 controller/deploy/charts/controller

# Redistributed binaries with generated third-party attribution.
NOTICE_MODULES := agent controller cli os-image

# Tool versions that CI pins, in one place. Included rather than repeated here so
# `make show-versions` and the workflows read the same definitions.
include make/versions.mk

# The merge-gate checks that had no target, plus root fmt, lint and gate.
include make/checks.mk

.DEFAULT_GOAL := help
.PHONY: help show-versions tools-check lint-workflows lint-shell docs-check internal-config-check lint-chart chart-rbac-check chart-package-check chart-check vulncheck fmt lint gate test-compile test-e2e-backend-ci work work-sync work-check work-check-offline build test preflight version-check clean licenses licenses-check license-deps-check notices notices-check license-header-check

##@ Help

# %-22s, not %-12s, because five target names are longer than twelve characters
# and the descriptions had stopped lining up. license-header-check is the longest
# at twenty.
#
# The ##@ line starts a section, and agent/Makefile and controller/Makefile
# already use the same convention. Grouping this way is read from the file, so a
# renamed or deleted target cannot leave a stale entry behind. Writing the groups
# out as echo lines instead, the way some sibling repositories do, duplicates
# every name and description and nothing checks the copy.
help: ## Show the available targets.
	@echo "Usage: make <target>"
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo
	@# Resolved by make at the moment you ask, so it cannot disagree with what
	@# the build will do. `make show-versions` prints the rest.
	@echo "Toolchain in use:"
	@printf '  %s, %s, golangci-lint %s\n' \
		"$$(command -v go >/dev/null && go version | awk '{print $$3}' || echo 'go not on PATH')" \
		"GNU Make $(MAKE_VERSION)" \
		"$$(command -v golangci-lint >/dev/null && golangci-lint --version | awk '{print $$4}' || echo 'not on PATH')"
	@echo

##@ Workspace

work: ## Reset go.work to exactly the public modules, creating it if absent.
	@test -f go.work || $(GO) work init
	@# `go work use` only ever adds, so on its own this could not drop a module
	@# that was removed from WORK_MODULES.
	@for d in $$($(GO) work edit -json | grep -o '"DiskPath": "[^"]*"' | cut -d'"' -f4); do \
		case " $(addprefix ./,$(WORK_MODULES)) " in \
			*" $$d "*) ;; \
			*) $(GO) work edit -dropuse $$d ;; \
		esac; \
	done
	@for o in $$($(GO) work edit -json | grep -A1 '"Old"' | grep -o '"Path": "[^"]*"' | cut -d'"' -f4); do \
		$(GO) work edit -dropreplace $$o; \
	done
	@$(GO) work use $(addprefix ./,$(WORK_MODULES))
	@echo "go.work lists $$($(GO) work edit -json | grep -c '"DiskPath"') modules."

work-sync: work ## Refresh go.work.sum and the modules' go.mod and go.sum.
	@# A workspace builds every member against one unified version of each
	@# dependency, whether or not that version is written down. `go work sync`
	@# writes it down: each go.mod then states the version its module is really
	@# built with, so Dependabot, THIRD_PARTY_NOTICES.md and a reader all see the
	@# same thing. Leaving it out does not avoid the upgrade, it only hides it.
	@$(GO) work sync
	@# go work sync updates go.mod but not go.sum, which leaves each module
	@# unbuildable on its own -- and the modules ARE built on their own, by their
	@# own Makefiles, by the image builds and by every per-module CI job. Tidy
	@# each one with the workspace off to complete it.
	@for m in $(WORK_MODULES); do \
		(cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; \
	done
	@# Deleted first: `go mod download all` only adds hashes, so without this
	@# go.work.sum accumulates entries for versions nothing references any more
	@# and stops being a function of the modules it describes.
	@rm -f go.work.sum
	@$(GO) mod download all
	@# The vendor tree is a function of the go.mod files above it, so it has to
	@# be regenerated here rather than by hand. A root vendor/ puts the whole
	@# workspace in -mod=vendor: leave it behind and every build stops with
	@# "is marked as explicit in vendor/modules.txt, but not explicitly
	@# required in a go.mod". That is how main broke on 2026-08-13.
	@$(GO) work vendor
	@echo "Workspace refreshed. Commit go.work, go.work.sum, vendor/ and every"
	@echo "go.mod and go.sum this changed, together."

work-check: ## Verify go.work and go.work.sum are complete and current. (run by the merge gate)
	@# git diff reports nothing for an untracked path, so an uncommitted
	@# workspace file would otherwise pass this check in silence.
	@git ls-files --error-unmatch go.work go.work.sum >/dev/null 2>&1 \
		|| { echo "ERROR: go.work and go.work.sum are not both tracked. Run 'make work-sync' and commit them."; exit 1; }
	@echo "- Checking go.work lists the expected modules..."
	@listed=$$($(GO) work edit -json | grep -o '"DiskPath": "[^"]*"' | cut -d'"' -f4 | sed 's|^\./||' | sort); \
	 wanted=$$(printf '%s\n' $(WORK_MODULES) | sort); \
	 if [ "$$listed" != "$$wanted" ]; then \
	   echo "ERROR: go.work does not list the expected modules."; \
	   echo "  in go.work:"; printf '%s\n' "$$listed" | sed 's/^/    /'; \
	   echo "  expected:";   printf '%s\n' "$$wanted" | sed 's/^/    /'; \
	   echo "Run 'make work' and commit go.work."; exit 1; \
	 fi
	@echo "- Checking go.work carries no replace directive..."
	@# A replace in go.work silently repoints a dependency for every module in
	@# the workspace while the module list above still looks correct. Module
	@# replaces belong in the individual go.mod, where gomoddirectives audits
	@# them.
	@[ "$$($(GO) work edit -json | grep -c '"Old"')" = "0" ] \
		|| { echo "ERROR: go.work carries a replace directive. Module replaces belong in the individual go.mod. Undo it with 'git checkout -- go.work'."; exit 1; }
	@echo "- Checking every module in the tree is accounted for..."
	@dup=$$(printf '%s\n' $(WORK_MODULES) $(PRIVATE_MODULES) | sort | uniq -d); \
	 [ -z "$$dup" ] || { echo "ERROR: listed in both WORK_MODULES and PRIVATE_MODULES:"; printf '%s\n' "$$dup" | sed 's/^/    /'; exit 1; }
	@# A module added to the repository and to neither list is the failure this
	@# catches: it would never be built, linted or synced, and nothing else
	@# would report it.
	@found=$$(find . -name go.mod -not -path './vendor/*' -not -path '*/vendor/*' -not -path '*/testdata/*' | sed 's|^\./||; s|/go\.mod$$||' | sort); \
	 known=$$(printf '%s\n' $(WORK_MODULES) $(PRIVATE_MODULES) | sort); \
	 added=$$(printf '%s\n' "$$found" "$$known" "$$known" | sort | uniq -u); \
	 gone=$$(printf '%s\n' "$$known" "$$found" "$$found" | sort | uniq -u); \
	 if [ -n "$$added" ]; then \
	   echo "ERROR: these modules exist in the tree but are in neither WORK_MODULES nor PRIVATE_MODULES:"; \
	   printf '%s\n' "$$added" | sed 's/^/    /'; \
	   echo "Add each to the right list in the Makefile, then run 'make work-sync'."; exit 1; \
	 fi; \
	 if [ -n "$$gone" ]; then \
	   echo "ERROR: these modules are listed in the Makefile but no longer exist:"; \
	   printf '%s\n' "$$gone" | sed 's/^/    /'; exit 1; \
	 fi
	@# Re-deriving workspace freshness needs the module graph over the network,
	@# which includes github.com/dsx-ai-factory/cluster-api-provider-nico — INTERNAL
	@# visibility until it is open-sourced, so unreachable from public runners
	@# and outside contributors. Builds are unaffected (the dependency is in the
	@# committed vendor/ tree); only this freshness re-derivation needs access,
	@# so it is skipped — as one shell block, because every recipe line runs in
	@# its own shell and an exit in one does not stop the next. Drop the guard
	@# when the repo goes public.
	@#
	@# Offline first: this needs no network, so it runs everywhere -- including on
	@# a public runner and for an outside contributor's fork, where the guarded
	@# block below cannot run at all.
	@$(MAKE) --no-print-directory work-check-offline
	@# Inside the guarded block: go work sync writes the workspace's selected
	@# versions back into every go.mod; each module is tidied with the workspace
	@# off to complete its go.sum; go.work.sum is rebuilt from scratch so it
	@# cannot accumulate stale hashes; vendor/ is regenerated because it is
	@# derived from the go.mod files, and a root vendor tree puts the workspace
	@# in -mod=vendor, so a stale one does not degrade the build, it stops it
	@# (main broke this way on 2026-08-13). The git diff then catches a bump
	@# landing in one module without the workspace being refreshed, and the
	@# untracked check catches a package that vendoring ADDS, which git diff
	@# alone would miss.
	@if (cd controller && GOWORK=off $(GO) mod download github.com/dsx-ai-factory/cluster-api-provider-nico >/dev/null 2>&1); then \
		echo "- Checking the workspace, go.work.sum, vendor/ and every go.mod agree..."; \
		$(GO) work sync || exit 1; \
		for m in $(WORK_MODULES); do \
			(cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; \
		done; \
		rm -f go.work.sum; \
		$(GO) mod download all || exit 1; \
		$(GO) work vendor || exit 1; \
		git diff --exit-code -- go.work go.work.sum '*/go.mod' '*/go.sum' vendor \
			|| { echo "ERROR: the workspace is stale. Run 'make work-sync' and commit every file it changes."; exit 1; }; \
		untracked=$$(git ls-files --others --exclude-standard -- vendor); \
		[ -z "$$untracked" ] \
			|| { echo "ERROR: vendoring added files that are not tracked. Run 'make work-sync' and commit every file it changes:"; \
			     printf '%s\n' "$$untracked" | sed 's/^/    /'; exit 1; }; \
		echo "go.work, go.work.sum and vendor/ are up to date."; \
	else \
		echo "NOTE: skipped the workspace freshness re-derivation:"; \
		echo "  github.com/dsx-ai-factory/cluster-api-provider-nico is not fetchable from here"; \
		echo "  (INTERNAL until open-sourced). The static checks above all passed."; \
	fi

# The assertions that need no network.
#
# Without this, a public runner reaches the guard below, finds
# cluster-api-provider-nico unfetchable, prints the skip notice and reports
# success -- having verified nothing about the vendor tree. That is the whole
# check for every outside contributor and every fork pull request, and it stays
# that way until the module is public.
#
# Go performs this comparison itself whenever it builds against a vendor tree:
# it reads vendor/modules.txt, compares it with every module's go.mod, and
# refuses to proceed on a mismatch. Asking `go list` for it makes the assertion
# explicit, and GOPROXY=off guarantees no network is consulted -- so a pass here
# means the tree really is self-consistent, not that a fetch quietly succeeded.
#
# This is the check that would have caught the 2026-08-13 breakage: main carried
# a go.mod requiring k8s.io/api v0.36.3 while vendor/modules.txt still said
# v0.35.3, and every build stopped.
#
# -mod=vendor is passed as a flag rather than through GOFLAGS, because setting
# GOFLAGS here would discard whatever the caller already had in it. -C has to be
# the first flag on the command line.
#
# -buildvcs=false because this lists rather than builds, so nothing is stamped
# with a commit. Reading the VCS can also fail outright: Go searches upwards from
# the module, and in a git worktree the search passes the worktree root, so a
# broken repository above it exits 128 and Go reports only "error obtaining VCS
# status".
#
# This target used to blame the vendor tree for that, and for any other `go list`
# failure. It printed "vendor/ is inconsistent" and sent the reader to
# `make work-sync`, which rewrites go.mod, go.sum and vendor/ across ten
# modules. The case below reports what actually failed.
work-check-offline: ## Verify vendor/ agrees with every go.mod. Needs no network.
	@echo "- Checking vendor/ agrees with every go.mod (offline)..."
	@for m in $(WORK_MODULES); do \
		err=$$(GOPROXY=off $(GO) list -C $$m -mod=vendor -buildvcs=false ./... 2>&1 >/dev/null) || { \
			printf '%s\n' "$$err" | sed 's/^/    /'; \
			case "$$err" in \
			  *vendor*|*"missing go.sum entry"*|*"inconsistent"*) \
			    echo "ERROR: vendor/ is inconsistent with the workspace -- Go's own message is above."; \
			    echo "  Detected from $$m; a root vendor tree is workspace-wide, so the module named"; \
			    echo "  there is where it was noticed, not necessarily where it originates."; \
			    echo "  Run 'make work-sync' and commit every file it changes." ;; \
			  *) \
			    echo "ERROR: 'go list' failed in $$m for a reason unrelated to vendoring."; \
			    echo "  Go's own message is above. vendor/ was NOT assessed, so do not run"; \
			    echo "  'make work-sync' on the strength of this failure -- fix the cause first." ;; \
			esac; \
			exit 1; }; \
	done
	@echo "  vendor/ is consistent with every module."

##@ Build and test

test: ## Run each module's own test suite.
	@# The root builds every module but used to test none, which invites the
	@# assumption that `make test` here does what `make test` in a module does.
	@# Each module keeps its own exclusions, so defer to its Makefile when it
	@# has one and fall back to `go test ./...` when it does not.
	@#
	@# The fallback keeps the live Ginkgo TestE2E entry point out of local tests.
	@for m in $(WORK_MODULES); do \
		echo "- testing $$m"; \
		if [ -f $$m/Makefile ] && $(MAKE) -C $$m -n test >/dev/null 2>&1; then \
			$(MAKE) -C $$m test || exit 1; \
			if $(MAKE) -C $$m -n test-integration-local >/dev/null 2>&1; then \
				$(MAKE) -C $$m test-integration-local || exit 1; \
			fi; \
		else \
			$(GO) -C $$m test $(GO_TEST_FLAGS) -skip '^TestE2E$$' ./... || exit 1; \
		fi; \
	done

test-compile: ## Type-check test variants that need external services to run.
	@echo "- compile controller/test/e2e"
	@cd controller/test/e2e && CGO_ENABLED=1 $(GO) vet ./...
	@echo "- compile controller/e2e with gofail"
	@cd controller && CGO_ENABLED=1 $(GO) vet -tags gofail ./e2e/...
	@echo "- compile nico-sim integration tests"
	@cd controller/nico-sim && CGO_ENABLED=1 $(GO) vet -tags integration ./test/integration/...
	@echo "- compile controller integration tests"
	@cd controller && CGO_ENABLED=1 $(GO) vet -tags integration_test ./integration-test/...

# The modules that own lint and test targets.
#
# Deepest first, so the first match wins. controller/nico-sim/x.go sits inside
# both controller/nico-sim and controller, and only the first is right.
#
# controller/api has no fmt target because its Go is generated.
PREFLIGHT_MODULES := controller/hack/capd-reboot-controller \
                     controller/test/e2e \
                     controller/nico-sim \
                     controller/api \
                     controller/deploy/charts \
                     agent \
                     cli \
                     controller \
                     management-engine \
                     os-image

# controller/api ships no tests of its own, so linting and testing it alone
# compiles nothing that uses it. These are the modules that import it, and a
# change to the API has to build them to mean anything.
API_DEPENDENTS := controller cli controller/test/e2e

# The modules that import controller/deploy/charts, so a chart change builds
# what ships it. nkectl is the consumer; until it imports the module, this only
# costs the cli suite a run it cannot fail.
CHARTS_DEPENDENTS := cli

# The modules CI tests with -race. agent-test.yml races agent; the unit-test
# matrix in controller-test.yml sets GO_TEST_FLAGS: -race for the whole job, so
# every module in that matrix is raced, not just controller.
#
# Preflight runs these with CGO_ENABLED=1, which is what controller-test.yml
# and `make gate` already do. Recent Go no longer needs cgo for the race
# detector everywhere, so this is not always load-bearing -- but a host that
# exports CGO_ENABLED=0 should not get a different answer from the gate, and
# matching it costs nothing.
RACE_MODULES := agent \
                cli \
                cluster-api-provider-provisioned \
                controller \
                controller/nico-sim \
                controller/test/e2e \
                os-image

# Root lint covers every module CI lints. Root fmt excludes generated API code,
# whose formatting belongs to controller's generator.
LINT_MODULES := $(PREFLIGHT_MODULES)
FORMAT_MODULES := $(filter-out controller/api,$(PREFLIGHT_MODULES))

# What "changed" is measured against.
PREFLIGHT_BASE ?= origin/main

# Answers "will my pull request be green?" without running the whole gate.
#
# The gate runs every member on every pull request whatever you touched,
# because a called workflow ignores its own paths filter. See
# .github/workflows/README.md, invariant 3. So this cannot narrow what runs. It
# narrows what can newly fail: when main is green and you only changed cli/, only
# the checks that read cli/ and the workspace-wide ones can go red.
#
# Whole-workspace and environment-dependent members stay opt-in. The target
# prints that list before it runs.
#
# The module match reads ("$$m"/*) because the case sits inside a $$( )
# substitution. Without the leading parenthesis, /bin/sh reads the pattern's
# closing ) as the end of the substitution and fails with "syntax error near
# unexpected token ;;".
preflight: ## Lint and test the modules you changed. MODULES=... to choose them yourself.
	@set -e; \
	base=$$(git merge-base $(PREFLIGHT_BASE) HEAD 2>/dev/null || true); \
	if [ -z "$$base" ]; then \
		echo "NOTE: no merge base with $(PREFLIGHT_BASE); using uncommitted changes only."; \
		echo "      Fetch it, or pass PREFLIGHT_BASE=<ref>."; \
	fi; \
	changed=$$( { [ -n "$$base" ] && git diff --name-only "$$base"...HEAD; \
	              git diff --cached --name-only; \
	              git diff --name-only; \
	              git ls-files --others --exclude-standard; } | sort -u ); \
	build_system=no; workflows=no; shell=no; chart=no; nktp=no; workspace=no; licences=no; provider_manifest=no; \
	printf '%s\n' "$$changed" | grep -qE '^Makefile$$|^make/[^/]+\.mk$$' && build_system=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^\.github/workflows/.*\.ya?ml$$' && workflows=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^controller/deploy/charts/|^controller/hack/chart-.*-assertions\.(py|sh)$$|^\.github/workflows/chart\.yml$$' && chart=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^nktp/deploy/charts/|^nktp/hack/chart-.*\.sh$$|^\.github/workflows/nktp-chart\.yml$$' && nktp=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^cluster-api-provider-provisioned/config/|^cli/internal/cwedeploy/manifests/provisioned-provider\.yaml$$' && provider_manifest=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '(^|/)(go\.mod|go\.sum)$$|^go\.work|^vendor/' && workspace=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '\.(go|sh|py|js|css|html|ya?ml)$$' && licences=yes || true; \
	for f in $$changed; do \
		case "$$f" in \
			*.sh) shell=yes ;; \
			*) [ -f "$$f" ] && head -n 1 "$$f" | grep -qE '^#!.*(bash|dash|ksh|/sh| sh)([[:space:]]|$$)' && shell=yes || true ;; \
		esac; \
	done; \
	if [ "$$build_system" = yes ]; then \
		workflows=yes; shell=yes; chart=yes; nktp=yes; workspace=yes; licences=yes; provider_manifest=yes; \
	fi; \
	if [ -n "$(MODULES)" ]; then \
		mods="$(MODULES)"; \
		echo "Modules (from MODULES=): $$mods"; \
	elif [ "$$build_system" = yes ]; then \
		mods="$(PREFLIGHT_MODULES)"; \
	else \
		mods=$$(for f in $$changed; do \
			for m in $(PREFLIGHT_MODULES); do \
				case "$$f" in ("$$m"/*) echo "$$m"; break ;; esac; \
			done; \
		done | sort -u); \
		mods=$$(for m in $$mods; do \
			echo "$$m"; \
			case "$$m" in \
				(controller/api) printf '%s\n' $(API_DEPENDENTS) ;; \
				(controller/deploy/charts) printf '%s\n' $(CHARTS_DEPENDENTS) ;; \
			esac; \
		done | sort -u); \
	fi; \
	echo "Will run:"; \
	if [ -n "$$mods" ]; then \
		for m in $$mods; do echo "  lint + test   $$m"; done; \
	else \
		echo "  (nothing: no changed module has its own lint/test)"; \
	fi; \
	[ "$$workspace" = yes ] && echo "  work-check-offline   (the workspace moved)" || true; \
	[ "$$workspace" = yes ] && echo "  license-deps-check   (dependencies changed)" || true; \
	[ "$$licences" = yes ] && echo "  license-header-check (source files changed)" || true; \
	[ "$$workflows" = yes ] && echo "  lint-workflows       (workflow or build-system changes)" || true; \
	[ "$$shell" = yes ] && echo "  lint-shell           (shell or build-system changes)" || true; \
	[ "$$chart" = yes ] && echo "  chart-check          (chart or build-system changes)" || true; \
	[ "$$nktp" = yes ] && echo "  nktp-chart-check     (NKTP chart or build-system changes)" || true; \
	[ "$$provider_manifest" = yes ] && echo "  provider-manifest-check (the provisioned provider's manifests changed)" || true; \
	echo; \
	echo "Will NOT run, and is not covered:"; \
	echo "  - the e2e lanes (kind, Docker; CAPD additionally needs Linux amd64)"; \
	echo "  - the gofail suite  -- make -C controller e2e-gofail"; \
	echo "  - govulncheck       -- make vulncheck (whole workspace and slow)"; \
	echo "  - generate-check, version-check, test-compile, licenses-check"; \
	echo "                      -- make gate runs all four"; \
	echo; \
	failed=""; \
	for m in $$mods; do \
		echo "==> $$m lint"; \
		$(MAKE) --no-print-directory -C $$m lint || failed="$$failed $$m:lint"; \
		echo "==> $$m test"; \
		race=no; \
		case " $(RACE_MODULES) " in (*" $$m "*) race=yes ;; esac; \
		if [ "$$race" = yes ]; then \
			CGO_ENABLED=1 $(MAKE) --no-print-directory -C $$m test GO_TEST_FLAGS=-race \
				|| failed="$$failed $$m:test"; \
		else \
			$(MAKE) --no-print-directory -C $$m test || failed="$$failed $$m:test"; \
		fi; \
	done; \
	if [ "$$workspace" = yes ]; then \
		echo "==> work-check-offline"; \
		$(MAKE) --no-print-directory work-check-offline || failed="$$failed workspace"; \
		echo "==> license-deps-check"; \
		$(MAKE) --no-print-directory license-deps-check || failed="$$failed dependency-licenses"; \
	fi; \
	if [ "$$licences" = yes ]; then \
		echo "==> license-header-check"; \
		$(MAKE) --no-print-directory license-header-check || failed="$$failed licences"; \
	fi; \
	if [ "$$workflows" = yes ]; then \
		echo "==> lint-workflows"; \
		$(MAKE) --no-print-directory lint-workflows || failed="$$failed workflows"; \
	fi; \
	if [ "$$shell" = yes ]; then \
		echo "==> lint-shell"; \
		$(MAKE) --no-print-directory lint-shell || failed="$$failed shell"; \
	fi; \
	if [ "$$chart" = yes ]; then \
		echo "==> chart-check"; \
		$(MAKE) --no-print-directory chart-check || failed="$$failed chart"; \
	fi; \
	if [ "$$nktp" = yes ]; then \
		echo "==> nktp-chart-check"; \
		$(MAKE) --no-print-directory nktp-chart-check || failed="$$failed nktp"; \
	fi; \
	if [ "$$provider_manifest" = yes ]; then \
		echo "==> provider-manifest-check"; \
		$(MAKE) --no-print-directory -C cli provider-manifest-check || failed="$$failed provider-manifest"; \
	fi; \
	echo; \
	if [ -n "$$failed" ]; then \
		echo "preflight FAILED:$$failed"; \
		exit 1; \
	fi; \
	echo "preflight passed."

build: ## Build every workspace package, and the released commands into bin/.
	@# Compile everything first. Building only ./cmd/... left controller/api and
	@# controller/test/e2e with no compile coverage anywhere in CI, and this
	@# change re-versioned both of them.
	@# -buildvcs=false because every binary here takes its version from the -X
	@# ldflags below, not from Go's VCS stamp. See the note on work-check-offline
	@# for how reading the VCS can fail in a git worktree.
	@for m in $(WORK_MODULES); do \
		echo "- checking $$m"; \
		$(GO) build -C $$m -buildvcs=false ./... || exit 1; \
	done
	@mkdir -p $(BIN_DIR)
	@# foreach, not a shell loop: $(call) cannot read a shell variable.
	@$(foreach m,$(CMD_MODULES), \
		echo "- building $(m) $(call cmd_version,$(m))"; \
		CGO_ENABLED=0 $(GO) build -C $(m) -buildvcs=false -trimpath -ldflags "$(call cmd_ldflags,$(m))" -o $(BIN_DIR)/ ./cmd/... || exit 1; \
	)
	@# controller/Makefile owns this binary's name and its version ldflag, so
	@# delegate rather than repeat either here. See CMD_MODULES above.
	@echo "- building controller"
	@$(MAKE) --no-print-directory -C controller build >/dev/null || exit 1
	@cp controller/bin/$(CWE_BIN_NAME) $(BIN_DIR)/ \
		|| { echo "ERROR: controller/Makefile no longer produces bin/$(CWE_BIN_NAME). Update CWE_BIN_NAME here to match its WORKER_BIN_NAME."; exit 1; }
	@# Same reason as controller: cli/Makefile owns nkectl's
	@# version ldflags, and it asserts the stamp took.
	@echo "- building cli"
	@$(MAKE) --no-print-directory -C cli build >/dev/null || exit 1
	@cp cli/bin/nkectl $(BIN_DIR)/ \
		|| { echo "ERROR: cli/Makefile no longer produces bin/nkectl."; exit 1; }
	@echo "Built into $(BIN_DIR):"
	@ls -1 $(BIN_DIR) | sed 's/^/    /'

# Every binary `build` produces, and how each reports its version. nkectl takes a
# subcommand, the rest take a flag. Both answer the same question.
#
# name:argument pairs, because the check below reads them at make level. A shell
# loop cannot: make expands $(VERSION_CMD_$$b) before the shell sets b.
VERSION_CMDS := agent:--version \
                capd-reboot-controller:--version \
                controller:--version \
                nke-os-image-check:--version \
                nkectl:version \
                updater:--version

version-check: build ## Verify every built binary reports a real version. (run by the merge gate)
	@# Two tests. The name list catches a placeholder version. The emptiness test
	@# catches a variable with no default, which prints a label and nothing else.
	@# A shell function, because a `case` pattern inside $(foreach) would end it
	@# at the pattern's own `)`.
	@#
	@# The declared/built comparison sorts LC_ALL=C, because the two sides are
	@# sorted by different tools: make's $(sort) is byte order, while the shell's
	@# sort follows the locale, which folds punctuation and puts nkectl before
	@# nke-os-image-check where bytes put nke-os-image-check first. Every name that
	@# existed before sorted the same both ways, so this surfaced only when
	@# nke-os-image-check was added -- as a mismatch listing the identical six names.
	@fail=0; \
	check() { \
		out=$$("$(BIN_DIR)/$$1" $$2 2>&1) || { \
			echo "  FAIL $$1 $$2 -- exited non-zero:"; \
			echo "$$out" | sed 's/^/         /'; \
			return 1; \
		}; \
		case "$$out" in \
			*dev*|*unknown*|*v0.0.0*|"") \
				echo "  FAIL $$1 -- reports an unstamped version: $$out"; \
				return 1 ;; \
		esac; \
		if [ -z "$$(printf '%s' "$$out" | tr -d '[:space:]:' | tail -c 1)" ]; then \
			echo "  FAIL $$1 -- reports a label with no version: '$$out'"; \
			return 1; \
		fi; \
		echo "  ok   $$1 -- $$out"; \
	}; \
	$(foreach p,$(VERSION_CMDS),check $(word 1,$(subst :, ,$(p))) $(word 2,$(subst :, ,$(p))) || fail=1;) \
	declared="$(sort $(foreach p,$(VERSION_CMDS),$(word 1,$(subst :, ,$(p)))))"; \
	built=$$(ls -1 $(BIN_DIR) | LC_ALL=C sort | tr '\n' ' '); \
	if [ "$$(echo $$declared)" != "$$(echo $$built)" ]; then \
		echo "  FAIL VERSION_CMDS lists [$$declared] but build produced [$$built]"; \
		echo "       Add the new binary to VERSION_CMDS so its version is checked too."; \
		fail=1; \
	fi; \
	[ $$fail -eq 0 ] || { \
		echo "ERROR: a binary does not report a build version. See the -X notes on cmd_ldflags."; \
		exit 1; }

##@ Licences

license-deps-check: ## Check dependency licenses in every workspace module. (run by the merge gate)
	@$(MAKE) --no-print-directory -C controller go-licenses
	@modules="$(MODULE)"; \
	 if [ -z "$$modules" ]; then modules="$$(./hack/workspace-modules.sh)"; fi; \
	 for module in $$modules; do \
		module="$${module#./}"; \
		echo "- dependency licences: $$module"; \
		GOTOOLCHAIN=local ./controller/bin/go-licenses check "./$$module/..." \
			--disallowed_types=restricted,reciprocal,forbidden,unknown \
			--ignore github.com/dsx-ai-factory/nke \
			--ignore github.com/dsx-ai-factory/cluster-api-provider-nico || exit 1; \
	 done

notices: ## Regenerate third-party notices for redistributed binaries.
	@for module in $(NOTICE_MODULES); do \
		echo "- notices: $$module"; \
		$(MAKE) --no-print-directory -C "$$module" notices || exit 1; \
	 done

notices-check: ## Verify all redistributed binaries have current notices.
	@for module in $(NOTICE_MODULES); do \
		echo "- notices-check: $$module"; \
		$(MAKE) --no-print-directory -C "$$module" notices-check || exit 1; \
	 done

licenses: ## Copy LICENSE and NOTICE everywhere an artefact ships them from.
	@for d in $(LICENSE_DESTS); do \
		mkdir -p $$d && cp LICENSE NOTICE $$d/ || exit 1; \
	done
	@echo "LICENSE and NOTICE copied to: $(LICENSE_DESTS)"

licenses-check: licenses ## Verify the shipped LICENSE and NOTICE copies match the root files. (run by the merge gate)
	@# The image and the chart redistribute this work, so they carry its terms.
	@# Both read from a copy inside their own build context, and a copy is a
	@# thing that goes stale -- this is the assertion that it has not.
	@git diff --exit-code -- $(LICENSE_DESTS) \
		|| { echo "ERROR: a shipped LICENSE/NOTICE copy is stale. Run 'make licenses' and commit the change."; exit 1; }
	@# git diff says nothing about a path git is not tracking, so a newly added
	@# destination would otherwise pass while shipping nothing.
	@untracked=$$(git ls-files --others --exclude-standard -- $(LICENSE_DESTS)); \
	 [ -z "$$untracked" ] \
		|| { echo "ERROR: these shipped licence files are not tracked:"; printf '%s\n' "$$untracked" | sed 's/^/    /'; exit 1; }
	@echo "Shipped LICENSE and NOTICE copies are current."

license-header-check: ## Verify every source file carries the Apache-2.0 SPDX header. (run by the merge gate)
	@# Two assertions, because either one alone passes while the tree is wrong.
	@#
	@# addlicense only ADDS a header to a file that has none. It never rewrites
	@# one that is already there, so a per-module `make license` reports success
	@# on a file stamped with a different licence -- which is how 830 files here
	@# carried LicenseRef-NvidiaProprietary for months with every gate green,
	@# including six under agent/, whose template has said Apache-2.0 all along.
	@#
	@# So the first assertion catches a MISSING header, and the second catches a
	@# header naming the WRONG licence. The second is the one this repository
	@# lacked.
	@#
	@# stderr is deliberately NOT captured. addlicense prints the offending paths
	@# to stdout, while `go run` writes "go: downloading ..." there on a cold
	@# module cache and "exit status 1" on any failure -- to stderr. Folding the
	@# two together reports a module download as a file with no licence header,
	@# which is exactly how the first CI run of this check failed while passing
	@# on a developer machine whose cache was already warm.
	@missing=$$($(LICENSE_FILES) | xargs $(ADDLICENSE) -check -c "$(LICENSE_HOLDER)" -f $(LICENSE_TEMPLATE) | grep -v '^$$' || true); \
	 if [ -n "$$missing" ]; then \
		echo "ERROR: these files carry no licence header. Run 'make license':"; \
		printf '%s\n' "$$missing" | sed 's/^/    /'; \
		exit 1; \
	 fi
	@wrong=$$($(LICENSE_FILES) | while read -r f; do \
		grep -qE '^.{0,3} ?Code generated .* DO NOT EDIT\.$$' "$$f" && continue; \
		head -20 "$$f" | grep -q 'SPDX-License-Identifier: Apache-2.0' || echo "$$f"; \
	 done); \
	 if [ -n "$$wrong" ]; then \
		echo "ERROR: these files carry a header that is not Apache-2.0:"; \
		printf '%s\n' "$$wrong" | sed 's/^/    /'; \
		echo "The repository ships Apache-2.0. Fix the header rather than the check."; \
		exit 1; \
	 fi
	@echo "Licence headers: every tracked source file declares Apache-2.0."

##@ Housekeeping

clean: ## Remove the built binaries.
	@rm -rf $(BIN_DIR)

##@ End-to-end

test-e2e-backend-ci: ## Run a backend E2E suite (CAPD by default) with local controller and agent source.
	./controller/hack/ci/backend-e2e.sh

.PHONY: test-e2e-backend-local
test-e2e-backend-local: ## Run the CAPD E2E suite the way a developer does. macOS uses Colima, Linux uses the host Docker daemon.
	./controller/hack/local-backend-e2e.sh

##@ Security

.PHONY: secrets-check
secrets-check: ## Scan the commits on this branch for credentials, as the secrets workflow does.
	@command -v gitleaks >/dev/null || { \
	  echo "gitleaks not found. Install v$(GITLEAKS_VERSION): https://github.com/gitleaks/gitleaks/releases"; \
	  exit 1; \
	 }
	@echo "Scanning $(shell git rev-list --count origin/main..HEAD 2>/dev/null || echo 0) commit(s) on this branch..."
	@gitleaks git . --no-banner --redact \
	  --log-opts="origin/main..HEAD --diff-merges=first-parent"
	@echo "gitleaks: no credential found in the commits on this branch."
