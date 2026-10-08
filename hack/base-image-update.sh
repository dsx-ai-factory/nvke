#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Re-resolves the digest of every base image this repository names through an
# `ARG <NAME>=<registry>/<image>:<tag>@sha256:<digest>` default, and rewrites
# the line when the tag has moved. The tag is never changed, only the digest it
# resolves to.
#
# Why this exists
#
# hack/base-image-check.sh requires the release-relevant Dockerfiles to name
# their bases by digest, and its header records the cost: Dependabot's `docker`
# ecosystem reads an image named on a `FROM` and stops there. It does not
# resolve an `ARG ..._IMAGE=` default, so the golang and alpine bases of the
# two published images -- controller/ and cluster-api-provider-provisioned/ --
# are watched by nothing. Pinned and unwatched is the worst of both: the digest
# that ships is immutable, and it also never moves, so an upstream base rebuild
# that fixes a CVE never reaches a release until somebody notices by hand.
#
# This script is the automation that closes that half of the gap, driven weekly
# by .github/workflows/base-image-digests.yml, which opens a pull request when
# anything here changes. Reviewing that pull request is the same act as
# reviewing a Dependabot one.
#
# What it deliberately does NOT do
#
# It does not move tags. `alpine:3.24.1` -> `3.24.2` and `golang:1.27.1` ->
# `1.27.2` are version bumps with their own policy -- the `ignore` block in
# .github/dependabot.yml holds the Go one -- and this script has no way to
# apply that policy. It follows a tag that has been republished, nothing more.
# A tag bump stays a human edit, as it was before.
#
# Scope is implicit rather than listed, which is on purpose: the set of lines
# this touches is exactly the set that is already digest-pinned through an ARG.
# A development-only Dockerfile that follows a mutable tag (the EXEMPT list in
# base-image-check.sh) has no such line and is therefore never rewritten, and
# the two lists cannot drift apart because there is only one.
#
# Usage:
#   hack/base-image-update.sh            rewrite the stale digests in place
#   hack/base-image-update.sh --check    report drift, change nothing, exit 1
#
# Needs crane on PATH. make/tool-versions.mk owns the pin; in CI,
# .github/actions/setup-crane installs it.

set -euo pipefail

cd "$(dirname "$0")/.."

check_only=0
case "${1:-}" in
	--check) check_only=1 ;;
	"") ;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
esac

if ! command -v crane >/dev/null 2>&1; then
	echo "ERROR: crane is not on PATH; see .github/actions/setup-crane" >&2
	exit 2
fi

# A single HEAD against the registry, but anonymous Docker Hub answers 429
# often enough that one unlucky weekly run would otherwise read as a real
# failure. Three attempts with a widening wait.
crane_digest() {
	local ref="$1" attempt=1 out
	while :; do
		if out="$(crane digest "${ref}" 2>&1)"; then
			printf '%s\n' "${out}"
			return 0
		fi
		if (( attempt >= 3 )); then
			printf '%s\n' "${out}" >&2
			return 1
		fi
		sleep $(( attempt * 5 ))
		attempt=$(( attempt + 1 ))
	done
}

fail=0
changed=0

# No `mapfile`, and no associative array for a digest cache: both are bash 4
# features, and this has to run on the macOS bash 3.2 a developer invokes it
# with. The handful of registry calls a repeat costs is not worth the risk of
# an empty array silently reporting success -- the trap base-image-check.sh
# records in its own comment.
dockerfiles=()
while IFS= read -r path; do
	dockerfiles+=("${path}")
done < <(git ls-files -- '*Dockerfile' '*Dockerfile.*' | grep -v '^vendor/' | sort)
[[ ${#dockerfiles[@]} -gt 0 ]] || { echo "ERROR: no Dockerfiles found" >&2; exit 2; }

pinned=0
for f in "${dockerfiles[@]}"; do
	# `ARG[[:space:]]+` rather than a single space: base-image-check.sh resolves
	# an ARG default with that same rule, so anything it accepts must be scanned
	# here too or a stale digest is reported as current.
	lines=()
	while IFS= read -r l; do
		lines+=("${l}")
	done < <(grep -nE '^ARG[[:space:]]+[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]+@sha256:[0-9a-f]{64}$' "${f}" || true)

	for entry in "${lines[@]}"; do
		lineno="${entry%%:*}"
		decl="${entry#*:}"          # ARG NAME=ref@sha256:...
		name="${decl%%=*}"             # ARG<whitespace>NAME
		name="${name#ARG}"
		name="${name//[[:space:]]/}"   # an ARG name cannot contain whitespace
		ref="${decl#*=}"
		tagged="${ref%@sha256:*}"
		old="${ref##*@}"

		# `ARG X=alpine@sha256:...` with no tag is pinned to one digest with
		# nothing to re-resolve it from. base-image-check.sh accepts it; this
		# script cannot maintain it, so say so rather than skipping silently.
		if [[ "${tagged##*/}" != *:* ]]; then
			echo "ERROR: ${f}:${lineno}: ${name} is digest-only (${tagged}); add the tag so the digest can be re-resolved" >&2
			fail=1
			continue
		fi

		pinned=$(( pinned + 1 ))

		if ! new="$(crane_digest "${tagged}")"; then
			echo "ERROR: ${f}:${lineno}: could not resolve ${tagged}" >&2
			fail=1
			continue
		fi

		if [[ "${new}" == "${old}" ]]; then
			continue
		fi

		changed=$(( changed + 1 ))
		echo "${f}:${lineno} ${name} ${tagged}"
		echo "    ${old} -> ${new}"

		if (( check_only )); then
			continue
		fi

		tmp="$(mktemp)"
		awk -v n="${lineno}" -v repl="ARG ${name}=${tagged}@${new}" \
			'NR==n { print repl; next } { print }' "${f}" >"${tmp}"
		# Preserve the file's mode; mktemp creates 0600.
		cat "${tmp}" >"${f}"
		rm -f "${tmp}"
	done
done

if (( fail )); then
	exit 1
fi

if (( changed == 0 )); then
	echo "Base images: ${pinned} ARG-pinned digests, all current."
	exit 0
fi

if (( check_only )); then
	echo "Base images: ${changed} of ${pinned} ARG-pinned digests are stale. Run 'make base-image-update'." >&2
	exit 1
fi

echo "Base images: updated ${changed} of ${pinned} ARG-pinned digests."
