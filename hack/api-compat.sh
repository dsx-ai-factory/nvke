#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Gate the modules nke supports as importable Go contracts on API compatibility.
#
# WHAT IT COMPARES, AND WHY THAT BASELINE
#
# controller-api-tag.yml mints controller/api/vX.Y.(Z+1) on every push to main
# that touches controller/api. It always increments the patch, and it does not
# look at what changed. So a removed method, a renamed field or a widened
# parameter list ships to importers as a patch release: `go get -u` picks it up
# and the build breaks at the consumer, with nothing in this repository having
# said so. This script is the thing that says so.
#
# The baseline is therefore the newest stable tag the tagger has already
# published, because that is exactly the version the next patch bump claims to
# be compatible with. docs/go-api-compatibility.md records which modules are in
# the supported set and what the promise is.
#
# WHY A LOCAL GIT WORKTREE AND NOT gorelease
#
# gorelease is the obvious tool and it cannot be used here: it resolves the base
# version through the module proxy, and this repository is private, so the
# download fails with a sum.golang.org 404 for every run. Checking the tag out
# into a temporary worktree needs no proxy access to nke itself, which also
# keeps the check working for a fork pull request, where no secret is available.
#
# WHY THE EXIT CODE OF apidiff IS NOT THE ANSWER
#
# apidiff exits 0 whether or not it found an incompatible change -- it reports,
# it does not judge. A gate that trusted its status would pass on every breaking
# change ever made. So the report is captured and the verdict is taken from its
# content, and a non-zero status is treated as the tool failing rather than as a
# finding. Empty export data is checked for the same reason: a package that
# failed to load yields an empty API, and two empty APIs compare equal.
#
# No `mapfile`: it is a bash 4.4 builtin, and on macOS bash 3.2 it leaves the
# array empty and exits 0, so the caller checks nothing and passes.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

# The supported set, from docs/go-api-compatibility.md. Adding a module here
# without adding it there leaves the promise undocumented, which is the gap
# this check was written to close.
SUPPORTED_MODULES=("controller/api")

# Pinned by the caller so a local run and CI compare with the same tool. A
# floating @latest would let a tool upgrade change the verdict on an unchanged
# tree.
apidiff_pin="${APIDIFF_VERSION:-}"
[[ -n "${apidiff_pin}" ]] || {
	echo "ERROR: APIDIFF_VERSION is unset. Run this through 'make api-compat', or" >&2
	echo "       export the pin from make/tool-versions.mk." >&2
	exit 2
}

# <module>@<base-version>, space separated. Version-bounded on purpose: the
# waiver names the baseline it was reviewed against, so the next tag retires it.
waived_spec="${API_COMPAT_WAIVED:-}"
waiver_owner="${API_COMPAT_WAIVER_OWNER:-}"

error() {
	if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
		echo "::error::$*" >&2
	else
		echo "ERROR: $*" >&2
	fi
}

warn() {
	if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
		echo "::warning::$*" >&2
	else
		echo "WARNING: $*" >&2
	fi
}

work="$(mktemp -d)"
cleanup() {
	# The worktree has to be unregistered, not just deleted, or it is left in
	# `git worktree list` as prunable and the next run of this script on the
	# same tag fails with "already exists".
	for stale in "${work}"/base-*; do
		[[ -d "${stale}" ]] || continue
		git worktree remove --force "${stale}" >/dev/null 2>&1 || true
	done
	rm -rf "${work}"
}
trap cleanup EXIT HUP INT TERM

apidiff() {
	go run "golang.org/x/exp/cmd/apidiff@${apidiff_pin}" "$@"
}

# The newest tag of the form <module>/vX.Y.Z, pre-releases excluded. A
# pre-release is not a baseline anyone can have imported by accident, and
# --sort=version:refname would rank it above the stable tag it precedes.
latest_stable_tag() {
	local module="$1" candidate version
	while IFS= read -r candidate; do
		version="${candidate#"${module}/v"}"
		if [[ "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
			printf '%s\n' "${candidate}"
			return 0
		fi
	done < <(git tag -l "${module}/v[0-9]*.[0-9]*.[0-9]*" --sort=-version:refname)
	return 1
}

# apidiff loads a module from the directory holding its go.mod, so each side is
# written from inside its own tree. GOWORK=off because the base worktree has no
# go.work member list that matches its own checkout, and because the workspace
# would resolve the api module's dependencies against the other modules' pins
# rather than against its own go.mod -- which is the thing being compared.
write_module_api() {
	local dir="$1" module_path="$2" out="$3"
	(
		cd "${dir}"
		GOWORK=off apidiff -m -w "${out}" "${module_path}"
	)
}

fail=0
matched_waivers=""

for module in "${SUPPORTED_MODULES[@]}"; do
	echo "- ${module}"

	[[ -f "${module}/go.mod" ]] || {
		error "${module} is named in SUPPORTED_MODULES but has no go.mod"
		fail=1
		continue
	}

	module_path="$(awk '$1 == "module" { print $2; exit }' "${module}/go.mod")"
	[[ -n "${module_path}" ]] || {
		error "${module}/go.mod has no module directive"
		fail=1
		continue
	}

	# A missing baseline fails. It is tempting to pass -- there is nothing to
	# compare against -- but the two ways to get here are a module that has
	# never been tagged, which means the promise in the documentation is not
	# backed by anything, and a shallow clone or one fetched without tags,
	# which means the check silently stopped running. Neither should be green.
	if ! base_tag="$(latest_stable_tag "${module}")"; then
		error "no ${module}/vX.Y.Z tag exists locally, so there is no baseline to compare against."
		error "  If this is a clone without tags: git fetch --tags"
		error "  If ${module} has genuinely never been released: tag it, or remove it from"
		error "  SUPPORTED_MODULES in this script and from docs/go-api-compatibility.md."
		fail=1
		continue
	fi
	base_version="${base_tag#"${module}/"}"
	echo "    baseline: ${base_tag}"

	base_tree="${work}/base-${module//\//-}"
	git worktree add --detach --quiet "${base_tree}" "${base_tag}"

	# Built before the diff so that a module which does not compile says so,
	# naming the compiler error, instead of surfacing as empty export data.
	for side in "${base_tree}/${module}" "${repo_root}/${module}"; do
		(cd "${side}" && GOWORK=off go build ./...) || {
			error "${module} does not build at ${side}; cannot compare APIs"
			fail=1
			continue 2
		}
	done

	write_module_api "${base_tree}/${module}" "${module_path}" "${work}/base.api"
	write_module_api "${repo_root}/${module}" "${module_path}" "${work}/head.api"

	for side in base head; do
		[[ -s "${work}/${side}.api" ]] || {
			error "apidiff wrote no export data for the ${side} side of ${module}"
			fail=1
			continue 2
		}
	done

	# Both reports are produced: the full one is what a reviewer reads, the
	# filtered one is what the verdict is taken from.
	all="$(apidiff -m "${work}/base.api" "${work}/head.api")"
	incompatible="$(apidiff -m -incompatible "${work}/base.api" "${work}/head.api")"

	if [[ -z "${all}" ]]; then
		echo "    no exported API change since ${base_version}"
	else
		echo "    exported API changes since ${base_version}:"
		printf '%s\n' "${all}" | sed 's/^/      /'
	fi

	waiver="${module}@${base_version}"
	is_waived=false
	case " ${waived_spec} " in
	*" ${waiver} "*) is_waived=true ;;
	esac

	if [[ -z "${incompatible}" ]]; then
		if [[ "${is_waived}" == true ]]; then
			matched_waivers="${matched_waivers} ${waiver}"
			error "${waiver} is waived in API_COMPAT_WAIVED, but ${module} has no incompatible change against ${base_version}. Remove the waiver."
			fail=1
		fi
		continue
	fi

	if [[ "${is_waived}" == true ]]; then
		matched_waivers="${matched_waivers} ${waiver}"
		warn "${module} breaks compatibility with ${base_version}, waived by API_COMPAT_WAIVED${waiver_owner:+ (owner: ${waiver_owner})}."
		warn "  The waiver names ${base_version} and expires when the next ${module} tag is published."
		continue
	fi

	error "${module} breaks compatibility with ${base_version}:"
	printf '%s\n' "${incompatible}" | sed 's/^/      /' >&2
	error "  controller-api-tag.yml will publish this as a patch bump, so importers"
	error "  will take it on 'go get -u' and break. Either keep the change backwards"
	error "  compatible, or follow the breaking-change procedure in"
	error "  docs/go-api-compatibility.md."
	fail=1
done

# A waiver naming a baseline that has moved on, or a module that is not in the
# supported set, is dead text. Left alone it reads as cover for a break it can
# no longer authorise, so it fails here rather than being ignored.
for entry in ${waived_spec}; do
	case " ${matched_waivers} " in
	*" ${entry} "*) continue ;;
	esac
	error "API_COMPAT_WAIVED names ${entry}, which matched no module and baseline checked above. Remove it."
	fail=1
done

exit "${fail}"
