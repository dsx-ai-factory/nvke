#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Compare controller/api/openapi.yaml against the last published version of
# itself, and fail when the change breaks an existing client.
#
# WHY THIS EXISTS ALONGSIDE generate-check
#
# `make -C controller generate-check` answers one question: do the committed
# *.gen.go files match openapi.yaml? That question has nothing to say about
# compatibility. Remove a response field, narrow a type, or add a required
# request property, and generate-check passes -- the generated code matches the
# new spec perfectly -- while every client built against the old one breaks.
#
# Regeneration correctness and compatibility are different questions. This
# script asks the second one.
#
# THE BASELINE
#
# controller/api is a Go submodule, and .github/workflows/controller-api-tag.yml
# gives every change to it an immutable controller/api/vX.Y.Z tag on main. That
# tag is therefore the last spec a consumer could have pinned, which makes it
# the baseline: not "the merge base", not "main", but a version somebody can
# actually be depending on.
#
# THE POLICY
#
# See controller/api/COMPATIBILITY.md. In short: anything oasdiff reports under
# `breaking` fails, removals must go through deprecation plus x-sunset, and an
# intentional break is landed by naming its check id in OPENAPI_COMPAT_WAIVED.
#
# FAIL-CLOSED
#
# Every way this check can fail to reach a verdict exits 2, never 0: no
# baseline tag, a baseline that has no spec in it, a spec neither side can
# parse, a tool that will not build, or output in a shape this script does not
# recognise. "Could not tell" must never read as "compatible", which is the
# failure mode a compatibility gate is most likely to have and least likely to
# notice.
#
# Exit codes: 0 compatible, 1 breaking change, 2 could not reach a verdict.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

spec_path="controller/api/openapi.yaml"
tool_modfile="oasdiff.mod"

# The baseline git ref. Empty means "resolve the newest published tag", which
# is what CI and a laptop both want. Set it to compare against something else:
#
#     OPENAPI_COMPAT_BASELINE=controller/api/v0.0.12 hack/openapi-compat-check.sh
baseline_ref="${OPENAPI_COMPAT_BASELINE:-}"

# Space-separated oasdiff check ids that are allowed to fail, e.g.
# "api-path-removed-without-deprecation". Scoped to the check, not to the
# endpoint, which is deliberately blunt: a waiver this broad is uncomfortable
# to leave in place, and it is meant to be.
waived="${OPENAPI_COMPAT_WAIVED:-}"
waiver_owner="${OPENAPI_COMPAT_WAIVER_OWNER:-}"
waiver_review="${OPENAPI_COMPAT_WAIVER_REVIEW:-}"

# How much notice a removal owes a client, in days. Reached through make, which
# owns the number; see make/tool-versions.mk.
deprecation_days="${OPENAPI_DEPRECATION_DAYS:-180}"

GO="${GO:-go}"

# GitHub renders `::error::` as an annotation. Locally that is noise, so print
# the plain form there. Same shape as hack/govulncheck-gate.sh.
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
trap 'rm -rf "${work}"' EXIT HUP INT TERM

# ---------------------------------------------------------------------------
# The baseline
# ---------------------------------------------------------------------------

# The same prefix and the same stable-version shape controller-api-tag.yml
# allocates, so the two cannot disagree about which tags count. A pre-release
# or a hand-made tag like controller/api/v1.2 is not a published version and is
# skipped rather than silently becoming the baseline.
tag_prefix='controller/api/v'
version_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

newest_published_tag() {
	local candidate version
	while IFS= read -r candidate; do
		version="${candidate#"${tag_prefix}"}"
		if [[ "${version}" =~ ${version_re} ]]; then
			printf '%s\n' "${candidate}"
			return 0
		fi
	done < <(git tag -l "${tag_prefix}[0-9]*.[0-9]*.[0-9]*" --sort=-version:refname)
	return 1
}

if [[ -z "${baseline_ref}" ]]; then
	if ! baseline_ref="$(newest_published_tag)"; then
		error "no published ${tag_prefix}X.Y.Z tag is present, so there is nothing to compare against"
		echo "       A shallow or tagless clone is the usual cause. Fetch the tags:" >&2
		echo "           git fetch --tags" >&2
		echo "       In a workflow, check out with fetch-depth: 0." >&2
		echo "       To compare against something else, set OPENAPI_COMPAT_BASELINE." >&2
		exit 2
	fi
fi

if ! git rev-parse --verify --quiet "${baseline_ref}^{commit}" >/dev/null; then
	error "baseline ref ${baseline_ref} does not resolve to a commit"
	exit 2
fi

# Named for the ref, because oasdiff prints the base file path in every
# finding and "baseline.yaml" in a temp directory tells a reader nothing.
baseline="${work}/${baseline_ref//\//-}.yaml"
if ! git show "${baseline_ref}:${spec_path}" >"${baseline}" 2>/dev/null; then
	error "${baseline_ref} contains no ${spec_path}"
	exit 2
fi
# A zero-byte baseline parses as an empty spec, and every endpoint then reads
# as removed. That is a broken baseline reported as one breaking change per
# operation in the API, so catch it here where the message can say so.
[[ -s "${baseline}" ]] || { error "${spec_path} is empty at ${baseline_ref}"; exit 2; }

[[ -s "${spec_path}" ]] || { error "${spec_path} is missing or empty in the working tree"; exit 2; }

echo "- baseline ${baseline_ref}, revision ${spec_path} as it stands in the working tree"

if git diff --quiet "${baseline_ref}" -- "${spec_path}" 2>/dev/null; then
	echo "  the specification is unchanged since ${baseline_ref}"
	exit 0
fi

# ---------------------------------------------------------------------------
# The comparison
# ---------------------------------------------------------------------------

# Pinned in its own module file, for the reason controller/api/generate.go
# records about oapi-codegen: the version that runs must not depend on what
# happens to be on PATH. GOWORK=off because Go refuses -modfile in workspace
# mode ("go: -modfile cannot be used in workspace mode").
#
# Built rather than `go run`, because `go run` reports every non-zero program
# exit as 1 after printing "exit status N" to stderr. oasdiff uses 1 for
# "findings" and 102 for "could not load a spec", and this script has to tell
# those apart -- a parse failure must not be read as a clean diff.
oasdiff="${work}/oasdiff"
echo "- building the pinned oasdiff from controller/api/${tool_modfile}"
if ! (cd controller/api && GOWORK=off "${GO}" build -modfile="${tool_modfile}" \
	-o "${oasdiff}" github.com/oasdiff/oasdiff); then
	error "could not build the pinned oasdiff; this check needs the module proxy"
	exit 2
fi

# --deprecation-days-stable and not --deprecation-days-beta: oasdiff reads a
# stability level from an x-stability-level extension, this spec carries none,
# so every operation is treated as stable and the one number governs.
#
# --flatten-allof and --flatten-params make the comparison see what a client
# sees. A schema split into allOf subschemas, or a parameter declared once at
# path level, is the same contract either way, and without these a pure
# refactor of the spec reports as a change.
oasdiff_args=(
	breaking "${baseline}" "${spec_path}"
	--deprecation-days-stable "${deprecation_days}"
	--flatten-allof
	--flatten-params
)

# json first, because it carries the check ids the waiver list is written in.
# The text run below is for the human; it re-reads the same two files and
# cannot disagree with this one.
findings_json="${work}/breaking.json"
set +e
"${oasdiff}" "${oasdiff_args[@]}" --format json >"${findings_json}" 2>"${work}/stderr"
rc=$?
set -e

if [[ ${rc} -ne 0 ]]; then
	cat "${work}/stderr" >&2
	error "oasdiff could not compare the specifications (exit ${rc}); treating that as a failure, not as compatible"
	exit 2
fi

# The shape this script knows how to read: one JSON array. Anything else means
# oasdiff changed its output and the id extraction below is now reading
# nothing -- which would pass. Fail instead.
if [[ ! "$(tr -d ' \n\t' <"${findings_json}")" =~ ^\[.*\]$ ]]; then
	cat "${findings_json}" >&2
	error "oasdiff --format json did not produce a JSON array; this check cannot read its output"
	exit 2
fi

echo
"${oasdiff}" "${oasdiff_args[@]}" --format text --color never || true
echo

# oasdiff check ids are lower-case kebab words, so the field never needs
# unescaping and a grep is enough. The envelope check above is what makes that
# safe: an unrecognised shape has already failed.
found="$(grep -oE '"id":"[a-z0-9-]+"' "${findings_json}" |
	sed 's/^"id":"//; s/"$//' | sort -u || true)"

allowed="${waived//,/ }"

fail=0
for id in ${found}; do
	case " ${allowed} " in
	*" ${id} "*) ;;
	*)
		error "${id} is a breaking change and is not waived"
		fail=1
		;;
	esac
done

# A waiver that no longer describes anything is the thing that makes waiver
# lists accumulate forever. Removing it is someone's job, so make it the job
# of whoever fixed the break.
for id in ${allowed}; do
	case " ${found} " in
	*" ${id} "*) ;;
	*)
		error "${id} is waived but no longer reported; remove it from OPENAPI_COMPAT_WAIVED"
		fail=1
		;;
	esac
done

if ((fail)); then
	echo >&2
	echo "A breaking change to ${spec_path} is a decision, not an accident." >&2
	echo "controller/api/COMPATIBILITY.md records the three ways to proceed:" >&2
	echo "  1. make the change additive," >&2
	echo "  2. deprecate first, with x-sunset at least ${deprecation_days} days out, or" >&2
	echo "  3. waive it in OPENAPI_COMPAT_WAIVED, with an owner and a review date." >&2
	[[ -n "${waiver_owner}" ]] && echo "Waiver owner: ${waiver_owner}" >&2
	exit 1
fi

if [[ -n "${found}" ]]; then
	# A date that fails the build would get the whole mechanism deleted the
	# first time it fired on a Monday. Warn loudly instead; the stale-waiver
	# check above is what actually stops rot. Same reasoning as
	# hack/govulncheck-gate.sh.
	if [[ -n "${waiver_review}" ]] && [[ "$(date -u +%Y-%m-%d)" > "${waiver_review}" ]]; then
		warn "the OpenAPI compatibility waiver passed its review date ${waiver_review} (owner: ${waiver_owner:-unassigned})"
	fi
	warn "waived against ${baseline_ref}: $(echo "${found}" | tr '\n' ' ')"
	exit 0
fi

echo "  no breaking changes against ${baseline_ref}"
