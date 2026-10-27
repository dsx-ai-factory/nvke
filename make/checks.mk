# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Local entry points for merge-gate checks. Tools come from PATH unless a
# target explicitly installs its pinned tool (credential-alerts-check).

CHART ?= controller/deploy/charts/controller
NKTP_CHART ?= nktp/deploy/charts/nktp

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

# The repository guard compares the id, never the slug. A renamed or
# transferred repository keeps its id, while a slug guard that has gone false
# skips every job without a failure. See .github/workflows/README.md,
# "Repository guards". `github.repository_id` and `github.repository_owner`
# do not match the pattern, and neither does `x == github.repository`.
lint-workflows: ## Lint the GitHub Actions workflows. (run by the merge gate)
	@$(call require_tool,actionlint,$(ACTIONLINT_VERSION))
	@actionlint -color
	@if git grep -n -E 'github\.repository[[:space:]]*[!=]=' -- .github >/dev/null; then \
		echo "ERROR: a workflow compares github.repository to a string. Guard on github.repository_id instead:"; \
		git grep -n -E 'github\.repository[[:space:]]*[!=]=' -- .github; \
		exit 1; \
	 fi
	@$(call require_command,python3)
	@$(call require_command,yq)
	@python3 -B ./hack/test_e2e_path_filter.py
	@python3 -B ./hack/e2e-path-filter.py check
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

# Identifiers of infrastructure only NVIDIA can see. Not credentials, which
# is why they read as safe and are not: this repository is the staging ground
# for a public one, and an org, site, VPC or subnet id is a map of a real
# account. Matched by digest so this check does not publish what it forbids.
internal-config-check: ## Check the tree names no internal infrastructure. (run by the merge gate)
	@python3 -B ./hack/test_internal_config_check.py
	@./hack/internal-config-check.sh

# A banned name, found as tokens rather than lines so that an English word or
# a camel-case identifier does not hide the real token beside it. Fail mode
# lists every hit outside hack/name-check-allowlist.txt and exits 1. Report
# mode summarises and exits 0; the gate ran that way until 2026-10-11, while
# the tree still carried the name. A dead allowlist entry fails in either mode.
NAME_CHECK_MODE ?= fail
name-check: ## Fail on tokens that carry a banned name outside the allowlist. (run by the merge gate)
	@$(call require_command,python3)
	@python3 -B ./hack/test_name_check.py
	@python3 -B ./hack/name-check.py --mode $(NAME_CHECK_MODE)

CHART_CRDS_KUSTOMIZE := controller/deploy/release-crds
CHART_CRDS_FILE := $(CHART)/files/crds.yaml

.PHONY: chart-crds chart-crds-check
chart-crds: ## Regenerate the release CRDs the controller chart installs.
	@# Pinned kubectl: the rendered bytes are compared, so the renderer has to
	@# be the same one everywhere or the check below fails on version drift.
	@$(call require_tool,kubectl,$(KUBECTL_VERSION))
	@mkdir -p "$(dir $(CHART_CRDS_FILE))"
	@{ \
		printf '%s\n' \
			'# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.' \
			'# SPDX-License-Identifier: Apache-2.0' \
			'#' \
			'# GENERATED FILE -- do not edit. Regenerate with `make chart-crds`.' \
			'#' \
			'# `kubectl kustomize $(CHART_CRDS_KUSTOMIZE)`: the release CRDs from' \
			'# management-engine/config/crd/bases plus the annotations only the chart needs.' \
			'# Edit the Go types and regenerate there, or the overlay, then regenerate this.'; \
		printf '%s\n' '---'; \
		kubectl kustomize --load-restrictor=LoadRestrictionsNone $(CHART_CRDS_KUSTOMIZE); \
	} > "$(CHART_CRDS_FILE)"
	@echo "  wrote $(CHART_CRDS_FILE)"

chart-crds-check: ## Verify the release CRDs in the controller chart are up to date.
	@# Tracking first: git diff says nothing about an untracked path, so a
	@# never-committed file would pass silently.
	@git ls-files --error-unmatch "$(CHART_CRDS_FILE)" > /dev/null 2>&1 \
		|| { echo "$(CHART_CRDS_FILE) is not tracked by git. Run 'make chart-crds' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory chart-crds
	@git diff --exit-code -- "$(CHART_CRDS_FILE)" \
		|| { echo "ERROR: $(CHART_CRDS_FILE) is stale. Run 'make chart-crds' and commit it."; exit 1; }
	@echo "  $(CHART_CRDS_FILE) is up to date."

# helm lint and helm template both pass on a manifest Kubernetes would reject, so
# kubeconform parses what was rendered. A chart that renders nothing also passes
# helm template silently, hence the count.
lint-chart: ## Lint and render the Helm chart, then validate it. (run by the merge gate)
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_tool,kubeconform,$(KUBECONFORM_VERSION))
	@helm lint $(CHART)
	@# A directory, not `mktemp -t`: kubeconform selects files by extension, and
	@# macOS mktemp appends random characters after the template, so a file named
	@# nvke-chart-XXXXXX.qlKjrjG4RL is skipped and the summary reads
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
	@./controller/hack/chart-node-agent-auth-assertions.sh
	@./controller/hack/chart-node-agent-external-mtls-assertions.sh
	@./controller/hack/chart-provider-assertions.sh
	@./controller/hack/chart-spire-attestation-assertions.sh

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

# The chart package is named nke-controller while everything it renders stays
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

# helm lint and kubeconform cannot tell whether nvke-controller-config agrees
# with the controller's environment variables; this checks each key against them.
chart-controller-config-check: ## Assert nvke-controller-config loads and matches the controller's environment.
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_command,go)
	@$(call require_command,yq)
	@CHART="$(CHART)" ./controller/hack/chart-controller-config-assertions.sh

.PHONY: credential-alerts-check
credential-alerts-check: ## Validate credential alert rules, fixtures and the enabled PrometheusRule render.
	@$(call require_tool,helm,$(HELM_VERSION))
	@$(call require_tool,kubeconform,$(KUBECONFORM_VERSION))
	@CHART="$(CHART)" ./controller/hack/chart-credential-alerts-assertions.sh

chart-check: lint-chart chart-rbac-check chart-package-check chart-identity-check chart-controller-config-check credential-alerts-check ## Run every chart check enforced by CI.

.PHONY: nktp-chart-deps nktp-chart-lint nktp-chart-check

# The dependency archives are downloaded, never committed; the chart's own
# .gitignore keeps them out. `helm dependency build` refuses to run until every
# repository in Chart.yaml has been added, so they are added here, read from
# Chart.yaml rather than listed a second time. They go into a throwaway Helm
# config, so your `helm repo list` is left alone. The build also fails when
# Chart.lock no longer matches Chart.yaml.
nktp-chart-deps: ## Download the NKTP chart's dependencies into its charts/ directory.
	@$(call require_tool,helm,$(HELM_VERSION))
	@set -eu; \
	 dir=$$(mktemp -d); \
	 trap 'rm -rf "$$dir"' EXIT HUP INT TERM; \
	 export HELM_REPOSITORY_CONFIG="$$dir/repositories.yaml" HELM_REPOSITORY_CACHE="$$dir/cache"; \
	 n=0; \
	 for url in $$(awk '$$1 == "repository:" { print $$2 }' $(NKTP_CHART)/Chart.yaml | sort -u); do \
		n=$$((n + 1)); \
		helm repo add "nktp-dep-$$n" "$$url" >/dev/null; \
	 done; \
	 helm dependency build $(NKTP_CHART) --skip-refresh

# Custom resources are validated against the CRDs this chart installs; see
# nktp/hack/chart-validate.sh.
nktp-chart-lint: nktp-chart-deps ## Lint and render the NKTP chart, then validate it.
	@$(call require_tool,kubeconform,$(KUBECONFORM_VERSION))
	@$(call require_command,yq)
	@helm lint --strict $(NKTP_CHART)
	@NKTP_CHART="$(NKTP_CHART)" CERT_MANAGER_VERSION="$(CERT_MANAGER_VERSION)" ./nktp/hack/chart-validate.sh

nktp-chart-check: nktp-chart-lint ## Run every NKTP chart check enforced by CI. (run by the merge gate)

# Use the same tool, Go version, waiver, and stale-waiver guard as
# CI. The helper returns 3 for an unwaived finding and preserves tool failures.
vulncheck: ## Report vulnerabilities reachable from each module. (run by the merge gate)
	@$(call require_tool,govulncheck,$(GOVULNCHECK_VERSION))
	@echo "- govulncheck under Go $(GOVULNCHECK_GO_VERSION), the version CI uses"
	@# Download the toolchain first, or a machine that does not already
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
		GOVULNCHECK_WAIVED="$(GOVULNCHECK_WAIVED)" \
		GOVULNCHECK_WAIVER_OWNER="$(GOVULNCHECK_WAIVER_OWNER)" \
		GOVULNCHECK_WAIVER_REVIEW="$(GOVULNCHECK_WAIVER_REVIEW)" \
			./hack/govulncheck-gate.sh "$$m"; \
		rc=$$?; \
		[ $$rc -eq 0 ] || fail=1; \
	 done; \
	 [ $$fail -eq 0 ] || exit 1
	@echo "  govulncheck gate passed"

# tools-check asks what is installed. This asks whether the repository still
# agrees with itself about what should be installed -- so it needs none of the
# tools present and runs anywhere.
.PHONY: tool-version-check
tool-version-check: ## Verify every tool pin is declared once, in make/tool-versions.mk.
	@./hack/tool-version-check.sh

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

# One merge-gate job needs live services, and is listed below alongside the
# advisory lanes that are not part of the gate at all.
# WORK_MODULES and PRIVATE_MODULES are passed in rather than repeated in the
# script: they are already the list the workspace and the sync are built from,
# and a second copy is the drift this check exists to catch.
.PHONY: go-version-check
go-version-check: ## Verify every hand-kept Go version matches go.work.
	@./hack/go-version-check.sh --modules "$(WORK_MODULES)" --private "$(PRIVATE_MODULES)"

# A tag is mutable, so an image built from one is not reproducible and not the
# thing that was scanned. The script also carries the list of development-only
# Dockerfiles that are deliberately exempt, so that stays a recorded decision
# rather than an unexplained gap.
.PHONY: base-image-check
base-image-check: ## Verify release-relevant Dockerfiles pin base images by digest.
	@./hack/base-image-check.sh

# The other half of the same policy. base-image-check keeps the digests
# immutable; this follows the tags they were taken from, because Dependabot
# cannot see an ARG-named base and a pin nothing bumps goes stale instead.
# Deliberately NOT in `gate`: a republished upstream base is news, not a
# reason to fail somebody's pull request. base-image-digests.yml runs it
# weekly and opens a pull request with whatever moved.
# Pass --check to the script directly to report without writing.
.PHONY: base-image-update
base-image-update: ## Re-resolve ARG-pinned base image digests in place (needs crane).
	@./hack/base-image-update.sh

# Not read-only. A Go bump moves go.work and then eleven `go` directives, and a
# fix you have to apply by hand eleven times is a fix that gets applied ten
# times.
.PHONY: go-version-sync
go-version-sync: ## Rewrite every go.mod `go` directive to match go.work.
	@./hack/go-version-check.sh --fix --modules "$(WORK_MODULES)" --private "$(PRIVATE_MODULES)"

# The scan that reads .grype.yaml needs a vulnerability database and a network.
# This reads the file only, so the three rules its preamble states are checked
# on a laptop and in the merge gate, not just in the weekly scan -- including
# the review date, which is the one that goes off while nobody is looking.
.PHONY: grype-ignore-check
grype-ignore-check: ## Verify every .grype.yaml exception is scoped, owned and in date.
	@./hack/grype-ignore-check.sh

.PHONY: deploy-lists-check
deploy-lists-check: ## Verify every nvke-deploy.yaml item has its directory.
	@./hack/deploy-lists-check.sh

gate: ## Run the merge-gate checks that work on a laptop.
	@$(MAKE) --no-print-directory tool-version-check
	@$(MAKE) --no-print-directory tools-check
	@$(MAKE) --no-print-directory version-check
	@$(MAKE) --no-print-directory go-version-check
	@$(MAKE) --no-print-directory base-image-check
	@$(MAKE) --no-print-directory grype-ignore-check
	@$(MAKE) --no-print-directory deploy-lists-check
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
	@$(MAKE) --no-print-directory internal-config-check
	@$(MAKE) --no-print-directory name-check
	@$(MAKE) --no-print-directory -C deploy test
	@$(MAKE) --no-print-directory chart-check
	@$(MAKE) --no-print-directory nktp-chart-check
	@$(MAKE) --no-print-directory -C controller generate-check
	@$(MAKE) --no-print-directory -C cli provider-manifest-check
	@$(MAKE) --no-print-directory -C cli docs-check
	@$(MAKE) --no-print-directory chart-crds-check
	@$(MAKE) --no-print-directory vulncheck
	@echo
	@echo "Merge-gate jobs not run locally:"
	@echo "  gofail e2e             make -C controller e2e-gofail    PostgreSQL, envtest and Temporal"
	@echo "Workspace re-derivation needs access to the internal provider; work-check reports if it skips."
	@echo "CAPD backend e2e is advisory, not part of the merge gate: make test-e2e-backend-ci"
	@echo "nvkectl bootstrap e2e is advisory too, not part of the merge gate:"
	@echo "  make -C cli test-e2e-bootstrap   (nerdctl, buildkitd, kind, and root)"
