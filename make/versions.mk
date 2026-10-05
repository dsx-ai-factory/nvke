# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tool-version interface for root targets and workflows. Canonical values live
# in tool-versions.mk so module Makefiles can include them without root targets.
#
# Each of these was written only inside a workflow file, so a developer had no
# way to find out which version CI uses, and no way to match it. `make
# show-versions` prints the set, and the workflows read it back with
# `make print-<name>` rather than repeating the number.
#
# The values are plain make variables, not a YAML file parsed with yq. A sibling
# repository does the yq form, and it costs a parse-time dependency: every
# target, `make help` included, fails when yq is absent. Tools here are resolved
# when a recipe runs, never to load the Makefile.
#
# When bumping one of these, change make/tool-versions.mk, then run
# `make show-versions` to see the result.

include make/tool-versions.mk

# Read one value from a workflow step, so a workflow never repeats a number:
#
#     V=$(make --no-print-directory print-KIND_VERSION)
#
# The pattern matches any variable, so it needs no upkeep as this file grows.
print-%:
	@echo "$($*)"

##@ Tool versions

show-versions: ## Print every pinned tool version.
	@echo "Tool versions (make/versions.mk):"
	@printf '  %-32s %s\n' \
		"GOVULNCHECK_GO_VERSION" "$(GOVULNCHECK_GO_VERSION)" \
		"GOVULNCHECK_VERSION"    "$(GOVULNCHECK_VERSION)" \
		"GOVULNCHECK_WAIVED" "$(GOVULNCHECK_WAIVED)" \
		"ACTIONLINT_VERSION"     "$(ACTIONLINT_VERSION)" \
		"SHELLCHECK_VERSION"     "$(SHELLCHECK_VERSION)" \
		"GITLEAKS_VERSION"       "$(GITLEAKS_VERSION)" \
		"GH_CLI_VERSION"         "$(GH_CLI_VERSION)" \
		"TELEPORT_VERSION"       "$(TELEPORT_VERSION)" \
		"KUBECONFORM_VERSION"    "$(KUBECONFORM_VERSION)" \
		"HELM_VERSION"           "$(HELM_VERSION)" \
		"KIND_VERSION"           "$(KIND_VERSION)" \
		"KUBECTL_VERSION"        "$(KUBECTL_VERSION)" \
		"CAPI_VERSION"           "$(CAPI_VERSION)" \
		"CERT_MANAGER_VERSION"   "$(CERT_MANAGER_VERSION)" \
		"ORAS_VERSION"           "$(ORAS_VERSION)" \
		"PACKER_VERSION"         "$(PACKER_VERSION)" \
		"S5CMD_VERSION"          "$(S5CMD_VERSION)" \
		"GO_LICENSES_VERSION"    "$(GO_LICENSES_VERSION)" \
		"VACUUM_VERSION"         "$(VACUUM_VERSION)" \
		"REDOCLY_VERSION"        "$(REDOCLY_VERSION)" \
		"FERN_VERSION"           "$(FERN_VERSION)"
	@echo
	@echo "Pinned in .flox/env/manifest.toml, and printed here from the live"
	@echo "environment rather than from a second copy of the number:"
	@printf '  %-24s %s\n' \
		"go"            "$$(command -v go >/dev/null && go version | awk '{print $$3}' || echo 'not on PATH')" \
		"golangci-lint" "$$(command -v golangci-lint >/dev/null && golangci-lint --version | awk '{print $$4}' || echo 'not on PATH')" \
		"make"          "$(MAKE_VERSION)"
