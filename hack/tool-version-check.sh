#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# make/tool-versions.mk owns every tool pin. This fails when something else in
# the tree declares one of the same names, which is the only way the two can
# disagree.
#
# Two copies of controller-tools v0.20.1 (cluster-api-provider-provisioned and
# management-engine) and two of the TPM plugin 1.9.0 (the spire-agent package
# and the CAPD bundle script) sat in the tree agreeing with each other, held
# there by a comment reading "move them together". Agreement maintained by
# whoever remembers is agreement until someone does not: the failure is a green
# local run and a red CI run, or the reverse, with the version nobody looked at
# as the cause.
#
# Mirroring a pin into a shell script or a workflow is fine -- read it from
# here, the way controller/hack/ci/prepare-capd-bundles.sh does. Assigning it a
# second time is what this rejects.

set -euo pipefail

cd "$(dirname "$0")/.."

OWNER="make/tool-versions.mk"
[[ -f "${OWNER}" ]] || { echo "ERROR: ${OWNER} is missing" >&2; exit 2; }

# golangci-lint has a different owner: make/golangci.mk carries the pin beside
# the bootstrap logic that consumes it, and is included by every module for
# exactly that. It is not a hole in this check -- GOLANGCI_LINT_VERSION is not
# declared in ${OWNER}, so the check never looks for it at all. This exemption
# exists so that moving the pin here later does not immediately fail on the
# file that legitimately still defines it.
EXEMPT_FILES="^make/golangci\.mk$"

fail=0

names=""
while IFS= read -r name; do
	names="${names} ${name}"
done < <(sed -nE 's/^([A-Z][A-Z0-9_]*(_VERSION|_SHA256[A-Z0-9_]*))[[:space:]]*\??=.*/\1/p' "${OWNER}" | sort -u)

[[ -n "${names}" ]] || { echo "ERROR: no pins found in ${OWNER}" >&2; exit 2; }

owned_value() {
	sed -nE "s/^$1[[:space:]]*\??=[[:space:]]*([^[:space:]#]*).*/\1/p" "${OWNER}" | tail -1
}

for name in ${names}; do
	want="$(owned_value "${name}")"
	while IFS= read -r hit; do
		file="${hit%%:*}"
		rest="${hit#*:}"
		line="${rest%%:*}"
		text="${rest#*:}"
		[[ "${file}" == "${OWNER}" ]] && continue
		[[ "${file}" =~ ${EXEMPT_FILES} ]] && continue
		got="$(printf '%s' "${text}" \
			| sed -E "s/^[[:space:]]*(export[[:space:]]+)?${name}[[:space:]]*([:+?]?=|:)[[:space:]]*//" \
			| tr -d "\"'" | awk '{print $1}')"
		# A derivation is the wanted shape, not a second declaration: a `$`
		# on the right-hand side means the value is being read from somewhere
		# (make, another variable, this file) rather than restated. Only a
		# literal can disagree with the owner, so only a literal is an error.
		# A bare `#` is the rest of a comment, not a value.
		case "${got}" in
		'' | '#'* | *'$'*) continue ;;
		esac
		echo "ERROR: ${file}:${line} declares ${name}=${got}; ${OWNER} declares ${want}" >&2
		fail=1
	done < <({
		# `+=` is in here with the rest: appending to a pin the owner already
		# set produces a two-word value that no consumer can use, and it is a
		# second declaration by any reading.
		git grep -nE "^[[:space:]]*(export[[:space:]]+)?${name}[[:space:]]*[:+?]?=" \
			-- ':(exclude)vendor' ':(exclude)*.md' || true
		# A workflow or composite action declares its pins as `NAME: value`,
		# which is the same second declaration in another syntax. Restricted to
		# YAML, so a makefile target sharing a pin's name is not read as one.
		# Only catches a value on the same line as the key; a pin folded onto
		# the next line is out of reach and is why the comment in ${OWNER}
		# names the places nothing can check.
		git grep -nE "^[[:space:]]*${name}:[[:space:]]*[^[:space:]]" \
			-- '*.yml' '*.yaml' ':(exclude)vendor' || true
	})
done

if (( fail )); then
	cat >&2 <<-MSG

	${OWNER} is the single owner of these pins. Delete the second declaration
	and include the file:

	    include ../make/tool-versions.mk

	A shell script or workflow that cannot include a makefile should read the
	value out of ${OWNER}; see controller/hack/ci/prepare-capd-bundles.sh.
	MSG
	exit 1
fi

n="$(printf '%s' "${names}" | wc -w | tr -d ' ')"
echo "Tool versions: ${n} pins, each declared once in ${OWNER}."
