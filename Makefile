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

# go.work owns the Go version. strcase (via teleport/api) asserts its
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
ADDLICENSE       ?= $(GO) run github.com/google/addlicense@$(ADDLICENSE_VERSION)

# The files that carry a licence header.
#
# Dockerfiles are absent deliberately: addlicense keys off the file extension
# and would give Dockerfile.proto C-style `//` comments, which Docker cannot
# parse.
#
# The exclusions cover vendored and generated trees, testdata, fixtures, build
# tooling, and the chart templates the chart ships. Files under hack/ are
# deliberately included: they are source and must carry the repository licence.
LICENSE_TRACKED_FILES = git ls-files -- '*.go' '*.sh' '*.py' '*.js' '*.css' '*.html' '*.yaml' '*.yml'
LICENSE_SOURCE_FILTER = awk '!/(^|\/)vendor\// && !/(^|\/)testdata\// && !/^generated\// \
	&& !/\.pb\.go$$/ && !/^controller\/deploy\/charts\// && !/^controller\/test\/fixtures\//'

# Modules in go.work — every module in the repository. All dependencies are
# public; the Go module cache supplies them after the first download.
WORK_MODULES := agent \
                auth \
                cluster-api-provider-provisioned \
                controller \
                controller/api \
                controller/deploy/charts \
                controller/nico-sim \
                controller/hack/capd-reboot-controller \
                cli \
                controller/test/e2e \
                management-engine \
                os-image \
                release

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
# management-engine also delegates to its Makefile to name cmd/manager correctly.
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
# NVKE_CONTROLLER_BIN_NAME overrides it. An empty value counts as unset.
# NKE_CONTROLLER_BIN_NAME and CWE_BIN_NAME are deprecated aliases for NVKE_CONTROLLER_BIN_NAME; #1399 removes them.
controller_bin_name = $(or $(NVKE_CONTROLLER_BIN_NAME),$(NKE_CONTROLLER_BIN_NAME),$(CWE_BIN_NAME),controller)

# No module is excluded from the workspace any more; kept (empty) because
# work-check accounts for every go.mod in the tree against these two lists.
PRIVATE_MODULES :=

# Every place an artefact ships LICENSE and NOTICE from.
#
# `helm package` includes only files inside the chart directory, so the root
# files are copied into the chart. The controller image does not need a copy:
# it builds with the repository root as its context and COPYs them directly.
#
# The chart copies existed before this and nothing maintained them -- they were
# committed by hand and happened to match. `licenses-check` now enforces it.
LICENSE_DESTS := controller/deploy/charts/controller

# Chart directories that ship the root LICENSE but keep a NOTICE of their own,
# which `make licenses` must not overwrite.
LICENSE_ONLY_DESTS := nktp/deploy/charts/nktp

# Redistributed binaries with generated third-party attribution.
NOTICE_MODULES := agent controller cli os-image

# Tool versions that CI pins, in one place. Included rather than repeated here so
# `make show-versions` and the workflows read the same definitions.
include make/versions.mk

# The merge-gate checks that had no target, plus root fmt, lint and gate.
include make/checks.mk

.DEFAULT_GOAL := help
.PHONY: local-site-setup local-site-status local-site-down help show-versions tools-check lint-workflows lint-shell docs-check internal-config-check lint-chart chart-rbac-check chart-package-check chart-controller-config-check chart-check vulncheck fmt lint gate test-compile test-e2e-backend-ci work work-sync work-check build test coverage coverage-report coverage-check preflight version-check clean licenses licenses-check license-deps-check notices notices-check license-header-check

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
	@echo "Workspace refreshed. Commit go.work, go.work.sum and every"
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
	@echo "- Checking the workspace, go.work.sum and every go.mod agree..."
	@# This refreshes the manifests in place, then compares a hash of the diff
	@# before and after, so it asserts that the refresh changed nothing rather
	@# than that the tree is clean. A tree already dirty in these paths passes,
	@# and a refresh that creates a previously absent go.sum is untracked, so it
	@# shows no diff and passes too -- nothing here catches that.
	@before=$$(git diff --binary -- go.work go.work.sum '*/go.mod' '*/go.sum' | shasum); \
		$(GO) work sync || exit 1; \
		for m in $(WORK_MODULES); do \
			(cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; \
		done; \
		rm -f go.work.sum; \
		$(GO) mod download all || exit 1; \
		after=$$(git diff --binary -- go.work go.work.sum '*/go.mod' '*/go.sum' | shasum); \
		[ "$$before" = "$$after" ] \
			|| { echo "ERROR: the workspace is stale. Run 'make work-sync' and commit every file it changes."; exit 1; }
	@echo "go.work, go.work.sum and module manifests are up to date."

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

coverage: ## Run the tests of every collecting module and report the coverage.
	@# Not folded into `test` above. `test` runs every workspace module, this
	@# runs the nine that collect, and a developer wanting one does not always
	@# want the other. The profiles land in coverage/ at the root; the report
	@# fails on a missing, malformed or empty one, and on a test package that
	@# nothing ran. It does not fail on a percentage -- see
	@# docs/coverage-policy.md for why not.
	@MAKE='$(MAKE)' ./hack/coverage.sh run

coverage-report: ## Validate and summarise the profiles already in coverage/.
	@./hack/coverage.sh report

coverage-check: ## Check the coverage registry against the tree. Runs no tests.
	@./hack/coverage.sh check

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
                     auth \
                     cli \
                     controller \
                     management-engine \
                     os-image \
                     release

# controller/api ships no tests of its own, so linting and testing it alone
# compiles nothing that uses it. These are the modules that import it, and a
# change to the API has to build them to mean anything.
API_DEPENDENTS := controller cli controller/test/e2e

# The modules that import auth, so an authentication change builds what uses it.
AUTH_DEPENDENTS := controller

# The modules that import controller/deploy/charts, so a chart change builds
# what ships it. nvkectl is the consumer; until it imports the module, this only
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
                auth \
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
	build_system=no; workflows=no; shell=no; chart=no; nktp=no; workspace=no; licences=no; provider_manifest=no; cli_docs=no; \
	printf '%s\n' "$$changed" | grep -qE '^Makefile$$|^make/[^/]+\.mk$$' && build_system=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^\.github/workflows/.*\.ya?ml$$' && workflows=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^controller/deploy/charts/|^controller/deploy/monitoring/|^controller/hack/chart-.*-assertions\.(py|sh)$$|^controller/internal/(config|controllerconfig)/|^\.github/workflows/chart\.yml$$' && chart=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^nktp/deploy/charts/|^nktp/hack/chart-.*\.sh$$|^\.github/workflows/nktp-chart\.yml$$' && nktp=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^cluster-api-provider-provisioned/config/|^cli/internal/bootstrap/controllerdeploy/manifests/(provisioned-provider|capnico)\.yaml$$|^cli/internal/stackinstall/stack/manifests/capnico\.yaml\.tmpl$$|^deploy/nico/capnico/capnico\.yaml$$' && provider_manifest=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '^cli/' && cli_docs=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '(^|/)(go\.mod|go\.sum)$$|^go\.work|^vendor/' && workspace=yes || true; \
	printf '%s\n' "$$changed" | grep -qE '\.(go|sh|py|js|css|html|ya?ml)$$' && licences=yes || true; \
	for f in $$changed; do \
		case "$$f" in \
			*.sh) shell=yes ;; \
			*) [ -f "$$f" ] && head -n 1 "$$f" | grep -qE '^#!.*(bash|dash|ksh|/sh| sh)([[:space:]]|$$)' && shell=yes || true ;; \
		esac; \
	done; \
	if [ "$$build_system" = yes ]; then \
		workflows=yes; shell=yes; chart=yes; nktp=yes; workspace=yes; licences=yes; provider_manifest=yes; cli_docs=yes; \
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
				(auth) printf '%s\n' $(AUTH_DEPENDENTS) ;; \
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
	[ "$$workspace" = yes ] && echo "  work-check           (the workspace moved)" || true; \
	[ "$$workspace" = yes ] && echo "  license-deps-check   (dependencies changed)" || true; \
	[ "$$licences" = yes ] && echo "  license-header-check (source files changed)" || true; \
	[ "$$workflows" = yes ] && echo "  lint-workflows       (workflow or build-system changes)" || true; \
	[ "$$shell" = yes ] && echo "  lint-shell           (shell or build-system changes)" || true; \
	[ "$$chart" = yes ] && echo "  chart-check          (chart or build-system changes)" || true; \
	[ "$$nktp" = yes ] && echo "  nktp-chart-check     (NKTP chart or build-system changes)" || true; \
	[ "$$provider_manifest" = yes ] && echo "  provider-manifest-check (the provisioned provider's or CAPNICo's manifests changed)" || true; \
	[ "$$cli_docs" = yes ] && echo "  cli docs-check       (the CLI changed, so its reference may be stale)" || true; \
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
		echo "==> work-check"; \
		$(MAKE) --no-print-directory work-check || failed="$$failed workspace"; \
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
	if [ "$$cli_docs" = yes ]; then \
		echo "==> cli docs-check"; \
		$(MAKE) --no-print-directory -C cli docs-check || failed="$$failed cli-docs"; \
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
	@# ldflags below, not from Go's VCS stamp. VCS status can fail in worktrees
	@# whose parent repository is unavailable.
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
	@cp controller/bin/$(controller_bin_name) $(BIN_DIR)/ \
		|| { echo "ERROR: controller/Makefile no longer produces bin/$(controller_bin_name). Update controller_bin_name here to match its WORKER_BIN_NAME."; exit 1; }
	@# Same reason as controller: cli/Makefile owns nvkectl's
	@# version ldflags, and it asserts the stamp took.
	@echo "- building cli"
	@$(MAKE) --no-print-directory -C cli build >/dev/null || exit 1
	@cp cli/bin/nvkectl $(BIN_DIR)/ \
		|| { echo "ERROR: cli/Makefile no longer produces bin/nvkectl."; exit 1; }
	@echo "- building management-engine"
	@$(MAKE) --no-print-directory -C management-engine build-bin BIN_DIR=$(BIN_DIR) VERSION=$(call cmd_version,management-engine) >/dev/null || exit 1
	@echo "Built into $(BIN_DIR):"
	@ls -1 $(BIN_DIR) | sed 's/^/    /'

# Every binary `build` produces, and how each reports its version. nvkectl takes a
# subcommand, the rest take a flag. Both answer the same question.
#
# name:argument pairs, because the check below reads them at make level. A shell
# loop cannot: make expands $(VERSION_CMD_$$b) before the shell sets b.
VERSION_CMDS := agent:--version \
                capd-reboot-controller:--version \
                controller:--version \
                management-engine:--version \
                nvke-os-image-check:--version \
                nvkectl:version \
                updater:--version

version-check: build ## Verify every built binary reports a real version. (run by the merge gate)
	@# Two tests. The name list catches a placeholder version. The emptiness test
	@# catches a variable with no default, which prints a label and nothing else.
	@# A shell function, because a `case` pattern inside $(foreach) would end it
	@# at the pattern's own `)`.
	@#
	@# The declared/built comparison sorts LC_ALL=C, because the two sides are
	@# sorted by different tools: make's $(sort) is byte order, while the shell's
	@# sort follows the locale, which folds punctuation and puts nvkectl before
	@# nvke-os-image-check where bytes put nvke-os-image-check first. Every name that
	@# existed before sorted the same both ways, so this surfaced only when
	@# nvke-os-image-check was added -- as a mismatch listing the identical six names.
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
			--ignore github.com/dsx-ai-factory/nvke \
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
	@for d in $(LICENSE_ONLY_DESTS); do \
		mkdir -p $$d && cp LICENSE $$d/ || exit 1; \
	done
	@echo "LICENSE and NOTICE copied to: $(LICENSE_DESTS)"
	@echo "LICENSE copied to: $(LICENSE_ONLY_DESTS)"

licenses-check: licenses ## Verify the shipped LICENSE and NOTICE copies match the root files. (run by the merge gate)
	@# The image and the chart redistribute this work, so they carry its terms.
	@# Both read from a copy inside their own build context, and a copy is a
	@# thing that goes stale -- this is the assertion that it has not.
	@git diff --exit-code -- $(LICENSE_DESTS) $(addsuffix /LICENSE,$(LICENSE_ONLY_DESTS)) \
		|| { echo "ERROR: a shipped LICENSE/NOTICE copy is stale. Run 'make licenses' and commit the change."; exit 1; }
	@# git diff says nothing about a path git is not tracking, so a newly added
	@# destination would otherwise pass while shipping nothing.
	@untracked=$$(git ls-files --others --exclude-standard -- $(LICENSE_DESTS) $(addsuffix /LICENSE,$(LICENSE_ONLY_DESTS))); \
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
	@tracked=$$($(LICENSE_TRACKED_FILES)); \
	 tracked_rc=$$?; \
	 if [ $$tracked_rc -ne 0 ]; then \
		echo "ERROR: could not enumerate source files for the licence-header check."; \
		exit 1; \
	 fi; \
	 files=$$(printf '%s\n' "$$tracked" | $(LICENSE_SOURCE_FILTER)); \
	 filter_rc=$$?; \
	 if [ $$filter_rc -ne 0 ]; then \
		echo "ERROR: could not filter source files for the licence-header check."; \
		exit 1; \
	 fi; \
	 if [ -z "$$files" ]; then \
		echo "ERROR: no source files found for the licence-header check."; \
		exit 1; \
	 fi; \
	 missing=$$(printf '%s\n' "$$files" | xargs $(ADDLICENSE) -check -c "$(LICENSE_HOLDER)" -f $(LICENSE_TEMPLATE)); \
	 rc=$$?; \
	 if [ $$rc -ne 0 ] && [ -z "$$missing" ]; then \
		echo "ERROR: addlicense could not check licence headers."; \
		exit 1; \
	 fi; \
	 missing=$$(printf '%s\n' "$$missing" | grep -v '^$$' || true); \
	 if [ -n "$$missing" ]; then \
		echo "ERROR: these files carry no licence header. Run 'make license':"; \
		printf '%s\n' "$$missing" | sed 's/^/    /'; \
		exit 1; \
	 fi; \
	 wrong=$$(printf '%s\n' "$$files" | while read -r f; do \
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

clean: ## Remove the built binaries and the coverage profiles.
	@rm -rf $(BIN_DIR) $(CURDIR)/coverage

##@ End-to-end

# The local site: everything the operator flow needs on one machine, so
# `nvkectl bootstrap site` and the deploy scripts can be typed as on real
# hardware. docs/get-started/local-site.md is the page.
local-site-setup: ## Prepare the local site: a container node with its hosts.csv and CA, the images, the inputs, nico-sim, and a workspace with env.sh.
	@./hack/local-site.sh setup

local-site-status: ## Say what of the local site is up.
	@./hack/local-site.sh status

local-site-down: ## Delete the tenant clusters, nico-sim, the node and the local site's workspace.
	@./hack/local-site.sh down

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
