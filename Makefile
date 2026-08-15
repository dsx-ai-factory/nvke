# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Root Makefile: manages the Go workspace and builds the commands in it.
# The per-module Makefiles under agent/ and controller/ are unchanged and still
# work on their own. This one only covers what spans modules.
#
# See docs/go-workspace.md.

# The docs teach `GOWORK=off` for the modules outside the workspace. Inherited
# here it would make every workspace target fail with "no go.work file found",
# so it is dropped from the environment the recipes pass on.
unexport GOWORK

GO      ?= go
BIN_DIR ?= $(CURDIR)/bin

# Modules in go.work — every module in the repository. Building needs no
# NVIDIA credentials: the one still-INTERNAL dependency
# (github.com/NVIDIA/cluster-api-provider-nico) is carried in the committed
# vendor/ tree, so `build` gives an outside contributor the same result as CI.
# Re-vendoring (inside `work-sync`) is the only step that needs access to it.
WORK_MODULES := agent \
                controller \
                controller/api \
                controller/forge-vms \
                controller/hack/capd-reboot-controller \
                controller/hack/nico-mock \
                controller/nkectl \
                controller/test/e2e

# Modules whose commands `build` can compile with `go build -o bin/ ./cmd/...`,
# which names each binary after its package directory. controller/api and
# controller/test/e2e hold library and test code with no ./cmd. The agent's
# samples/ and sample-clients/ are package main but are examples, not artefacts.
#
# controller does not belong here: its command is ./cmd/worker, but the binary
# it ships is named cloud-workflow-engine and carries a version ldflag, so this
# loop would produce the wrong name and no version. `build` delegates to
# controller/Makefile for it. controller/forge-vms ships through its own
# Makefile and the container image.
CMD_MODULES := agent \
               controller/hack/capd-reboot-controller \
               controller/hack/nico-mock \
               controller/nkectl

# Must match WORKER_BIN_NAME in controller/Makefile. `build` fails if it does not.
CWE_BIN_NAME ?= cloud-workflow-engine

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
                 controller/deploy/charts/cloud-workflow-engine

.DEFAULT_GOAL := help
.PHONY: help work work-sync work-check work-check-offline build clean licenses licenses-check

help: ## Show the available targets.
	@echo "Usage: make <target>"
	@echo
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z-]*:.*## / { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

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

work-check: ## Verify go.work and go.work.sum are complete and current.
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
	@# which includes github.com/NVIDIA/cluster-api-provider-nico — INTERNAL
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
	@if (cd controller && GOWORK=off $(GO) mod download github.com/NVIDIA/cluster-api-provider-nico >/dev/null 2>&1); then \
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
		echo "  github.com/NVIDIA/cluster-api-provider-nico is not fetchable from here"; \
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
work-check-offline: ## Verify vendor/ agrees with every go.mod. Needs no network.
	@echo "- Checking vendor/ agrees with every go.mod (offline)..."
	@for m in $(WORK_MODULES); do \
		GOPROXY=off $(GO) list -C $$m -mod=vendor ./... >/dev/null \
			|| { echo "ERROR: vendor/ is inconsistent with the workspace -- Go's own message is above."; \
			     echo "  Detected from $$m; a root vendor tree is workspace-wide, so the module named"; \
			     echo "  there is where it was noticed, not necessarily where it originates."; \
			     echo "  Run 'make work-sync' and commit every file it changes."; exit 1; }; \
	done
	@echo "  vendor/ is consistent with every module."

build: ## Build every workspace package, and the released commands into bin/.
	@# Compile everything first. Building only ./cmd/... left controller/api and
	@# controller/test/e2e with no compile coverage anywhere in CI, and this
	@# change re-versioned both of them.
	@for m in $(WORK_MODULES); do \
		echo "- checking $$m"; \
		$(GO) build -C $$m ./... || exit 1; \
	done
	@mkdir -p $(BIN_DIR)
	@for m in $(CMD_MODULES); do \
		echo "- building $$m"; \
		$(GO) build -C $$m -o $(BIN_DIR)/ ./cmd/... || exit 1; \
	done
	@# controller/Makefile owns this binary's name and its version ldflag, so
	@# delegate rather than repeat either here. See CMD_MODULES above.
	@echo "- building controller"
	@$(MAKE) --no-print-directory -C controller build >/dev/null || exit 1
	@cp controller/bin/$(CWE_BIN_NAME) $(BIN_DIR)/ \
		|| { echo "ERROR: controller/Makefile no longer produces bin/$(CWE_BIN_NAME). Update CWE_BIN_NAME here to match its WORKER_BIN_NAME."; exit 1; }
	@echo "Built into $(BIN_DIR):"
	@ls -1 $(BIN_DIR) | sed 's/^/    /'

licenses: ## Copy LICENSE and NOTICE everywhere an artefact ships them from.
	@for d in $(LICENSE_DESTS); do \
		mkdir -p $$d && cp LICENSE NOTICE $$d/ || exit 1; \
	done
	@echo "LICENSE and NOTICE copied to: $(LICENSE_DESTS)"

licenses-check: licenses ## Verify the shipped LICENSE and NOTICE copies match the root files.
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

clean: ## Remove the built binaries.
	@rm -rf $(BIN_DIR)
