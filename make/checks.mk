# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Local entry points for merge-gate checks. Every tool comes from PATH, and a
# missing one reports which tool and two ways to get it.

CHART ?= controller/deploy/charts/controller

define require_command
command -v $(1) >/dev/null 2>&1 || { \
	echo "ERROR: $(1) is missing."; \
	echo "Use 'flox activate', or install $(1) with your OS package manager."; \
	exit 1; }
endef

define require_tool
./hack/check-tool-version.sh "$(1)" "$(2)"
endef

##@ Checks

lint-workflows: ## Lint the GitHub Actions workflows. (run by the merge gate)
	@$(call require_tool,actionlint,$(ACTIONLINT_VERSION))
	@actionlint -color
	@echo "  workflows: no findings"

# --severity=error matches shellcheck.yml. The tree is clean at error and is not
# clean at warning, so raising it here would fail on a backlog the gate does not
# block on. xargs -0, never mapfile: see hack/shell-scripts.sh.
lint-shell: ## Lint every tracked shell script. (run by the merge gate)
	@$(call require_tool,shellcheck,$(SHELLCHECK_VERSION))
	@n=$$(./hack/shell-scripts.sh | tr -dc '\0' | wc -c | tr -d ' '); \
	 echo "  checking $$n shell scripts at severity=error"; \
	 ./hack/shell-scripts.sh | xargs -0 shellcheck --severity=error --format=tty
	@echo "  shell scripts: no error-level findings"

# Facts that live in the tree and were restated in prose: the module count, the
# binary names, the merge-gate member count, and every relative link. Each is
# read back from go.work, the Makefile and merge-gate.yml rather than from a
# second copy. Needs nothing beyond bash, git, awk and grep, so there is no
# version to pin and no require_tool line.
docs-check: ## Check the documented facts against the repository. (run by the merge gate)
	@./hack/docs-check.sh

# helm lint and helm template both pass on a manifest Kubernetes would reject, so
# kubeconform parses what was rendered. A chart that renders nothing also passes
# helm template silently, hence the count.
lint-chart: ## Lint and render the Helm chart, then validate it. (run by the merge gate)
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_tool,kubeconform,$(KUBECONFORM_VERSION))
	@helm lint $(CHART)
	@# A directory, not `mktemp -t`: kubeconform selects files by extension, and
	@# macOS mktemp appends random characters after the template, so a file named
	@# nke-chart-XXXXXX.qlKjrjG4RL is skipped and the summary reads
	@# "0 resource found in 0 file" while the target reports success.
	@set -eu; \
	 dir=$$(mktemp -d); \
	 trap 'rm -rf "$$dir"' EXIT HUP INT TERM; \
	 out="$$dir/rendered.yaml"; \
	 helm template cwe $(CHART) > "$$out"; \
	 count=$$(grep -c '^kind:' "$$out" || true); \
	 if [ "$$count" -eq 0 ]; then \
		echo "ERROR: the chart rendered no resources."; exit 1; \
	 fi; \
	 echo "  rendered $$count resources"; \
	 kubeconform -strict -summary -ignore-missing-schemas \
		-schema-location default \
		-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
		"$$out"
	@./controller/hack/chart-auth-assertions.sh

chart-rbac-check: ## Assert that tenant RBAC grants every permission the controller uses.
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_command,python3)
	@python3 -c 'import yaml' >/dev/null 2>&1 || { \
		echo "ERROR: Python module PyYAML is required for chart RBAC assertions."; \
		echo "  install: python3 -m pip install PyYAML"; \
		exit 1; \
	 }
	@python3 ./controller/hack/chart-rbac-assertions.py --chart "$(CHART)"

chart-package-check: ## Assert that chart packaging replaces placeholder versions.
	@$(call require_tool,helm,$(HELM_VERSION))
	@CHART="$(CHART)" ./controller/hack/chart-package-assertions.sh

# The chart package is named controller while everything it renders stays
# cloud-workflow-engine, held there by nameOverride. helm lint and kubeconform
# both pass if that pin is dropped, so this is the only check that would catch it.
chart-identity-check: ## Assert the chart still renders the deployed identities it must preserve.
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_command,python3)
	@python3 -c 'import yaml' >/dev/null 2>&1 || { \
		echo "ERROR: Python module PyYAML is required for chart identity assertions."; \
		echo "  install: python3 -m pip install PyYAML"; \
		exit 1; \
	 }
	@python3 ./controller/hack/chart-identity-assertions.py --chart "$(CHART)"

chart-check: lint-chart chart-rbac-check chart-package-check chart-identity-check ## Run every chart check enforced by CI.

# Use the same tool, Go version, offline scan, waiver, and stale-waiver guard as
# CI. The helper returns 3 for an unwaived finding and preserves tool failures.
vulncheck: ## Report vulnerabilities reachable from each module. (run by the merge gate)
	@$(call require_tool,govulncheck,$(GOVULNCHECK_VERSION))
	@echo "- govulncheck under Go $(GOVULNCHECK_GO_VERSION), the version CI uses"
	@# The scan runs with GOPROXY=off, and a toolchain switch fetches through the
	@# proxy. Download the toolchain first, or a machine that does not already
	@# have it fails inside the scan with an error naming neither GOPROXY nor
	@# this pin. CI is unaffected: setup-go leaves the toolchain in place.
	@GOTOOLCHAIN=go$(GOVULNCHECK_GO_VERSION) go version >/dev/null 2>&1 || { \
		echo "ERROR: could not obtain Go $(GOVULNCHECK_GO_VERSION), which this check pins to match CI." >&2; \
		echo "       It downloads once through the module proxy, so this step needs network access." >&2; \
		exit 1; \
	 }
	@fail=0; \
	 for m in $(WORK_MODULES); do \
		echo "- $$m"; \
		GOTOOLCHAIN=go$(GOVULNCHECK_GO_VERSION) \
		GOVULNCHECK_WAIVED_MODULES="$(GOVULNCHECK_WAIVED_MODULES)" \
			./hack/govulncheck-gate.sh "$$m"; \
		rc=$$?; \
		[ $$rc -eq 0 ] || fail=1; \
	 done; \
	 [ $$fail -eq 0 ] || exit 1
	@echo "  govulncheck gate passed"

tools-check: ## Check the versions of tools used by local merge-gate targets.
	@fail=0; \
	 ./hack/check-tool-version.sh actionlint "$(ACTIONLINT_VERSION)" || fail=1; \
	 ./hack/check-tool-version.sh shellcheck "$(SHELLCHECK_VERSION)" || fail=1; \
	 ./hack/check-tool-version.sh helm "$(HELM_VERSION)" || fail=1; \
	 ./hack/check-tool-version.sh kubeconform "$(KUBECONFORM_VERSION)" || fail=1; \
	 ./hack/check-tool-version.sh govulncheck "$(GOVULNCHECK_VERSION)" || fail=1; \
	 [ $$fail -eq 0 ]

##@ Everything

# Every module already has fmt and lint. There was no way to run them across the
# repository: `make lint` at the root failed with "No rule to make target". CI
# still calls the module targets directly, so its seven lint jobs stay parallel.
fmt: ## Format every module.
	@for m in $(FORMAT_MODULES); do \
		echo "- fmt $$m"; \
		$(MAKE) --no-print-directory -C $$m fmt || exit 1; \
	 done

lint: ## Lint every module. (run by the merge gate)
	@for m in $(LINT_MODULES); do \
		echo "- lint $$m"; \
		$(MAKE) --no-print-directory -C $$m lint || exit 1; \
	 done

# Two merge-gate jobs need Docker or live services. They are listed below.
gate: ## Run the merge-gate checks that work on a laptop.
	@$(MAKE) --no-print-directory tools-check
	@$(MAKE) --no-print-directory version-check
	@CGO_ENABLED=1 $(MAKE) --no-print-directory GO_TEST_FLAGS=-race test
	@$(MAKE) --no-print-directory test-compile
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory work-check
	@$(MAKE) --no-print-directory license-deps-check
	@$(MAKE) --no-print-directory licenses-check
	@$(MAKE) --no-print-directory license-header-check
	@$(MAKE) --no-print-directory lint-workflows
	@$(MAKE) --no-print-directory lint-shell
	@$(MAKE) --no-print-directory docs-check
	@$(MAKE) --no-print-directory chart-check
	@$(MAKE) --no-print-directory -C controller generate-check
	@$(MAKE) --no-print-directory vulncheck
	@echo
	@echo "Merge-gate jobs not run locally:"
	@echo "  nkectl bootstrap e2e   make -C cli test-e2e-bootstrap   Docker and kind"
	@echo "  gofail e2e             make -C controller e2e-gofail    PostgreSQL, envtest and Temporal"
	@echo "Workspace re-derivation needs access to the internal provider; work-check reports if it skips."
	@echo "CAPD backend e2e is advisory, not part of the merge gate: make test-e2e-backend-ci"
