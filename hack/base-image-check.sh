#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# A tag is mutable. `alpine:3.24.1` can be repushed, and then the image that
# ships is not the image that was reviewed and scanned. Every Dockerfile whose
# output reaches a consumer therefore names its base images by digest.
#
# Which Dockerfiles those are was previously implied rather than stated: four
# were digest-pinned, six were not, and nothing said whether the six were a
# deliberate exemption or an oversight. This script is that statement. Each
# Dockerfile in the tree is either checked or listed in EXEMPT with a reason,
# and a new one that is neither fails here rather than shipping unpinned.
#
# Dependabot keeps only SOME of these current, which is worth knowing before
# relying on it. The `docker` ecosystem in .github/dependabot.yml covers
# "/**/*", but it reads an image named on a `FROM` and stops there: it does not
# resolve an `ARG ..._IMAGE=` default, and a `FROM ${VAR}` is invisible to it
# either way. So of the bases this script enforces:
#
#   controller/Dockerfile   SPIFFE jwt-exec-auth  direct FROM  Dependabot bumps it
#                           golang, distroless    via ARG      base-image-update.sh
#   cluster-api-provider-provisioned/Dockerfile
#                           golang, alpine        via ARG      base-image-update.sh
#
# cluster-api-provider-provisioned has no directly-named base at all -- all
# three of its FROM lines go through an ARG or a previous stage.
#
# #888 moved both files off `FROM ${BASE_REGISTRY}/...` and onto the ARG form
# on the understanding that an ARG default was the parseable one. It is not.
# The evidence is in this repository: #888 landed 2026-09-20, and the next
# weekly Dependabot run on 2026-09-21 bumped alpine 3.24.1 -> 3.24.2 in three
# Kubernetes manifests (#956) and a golang digest on a direct FROM (#916),
# while leaving the ARG-named bases in these two files alone.
#
# Closing that gap by naming the image on the FROM, so Dependabot can see it,
# gives up the `--build-arg GOLANG_IMAGE=...` override #888 added for internal
# mirrors -- which the e2e lanes use to route Docker Hub pulls through
# NKE_E2E_DOCKERHUB_MIRROR (controller/hack/backend-e2e-lib.sh). That is a
# maintainer decision, not one this script makes, and it is still open.
#
# So the ARG form stays, and the automation is local instead:
# hack/base-image-update.sh re-resolves these digests and
# .github/workflows/base-image-digests.yml runs it weekly and opens a pull
# request with whatever moved. That covers a republished tag, which is what
# Dependabot would have caught here. It does NOT move a tag -- 3.24.1 to
# 3.24.2 remains a human edit until the FROM question above is settled.

set -euo pipefail

cd "$(dirname "$0")/.."

# Dockerfiles that build nothing a consumer receives: local development
# (Tilt, Makefile.dev), test fixtures, and e2e substrate images built on the
# machine that runs the test and thrown away with it. A mutable tag there
# cannot reach a release, and following a tag is what makes them useful --
# `kindest/node` in particular is selected per Kubernetes version at test time.
#
# Moving a Dockerfile out of this list is the decision to publish it.
EXEMPT=(
	cli/e2e/bootstrap/substrate/node/Dockerfile        # e2e substrate, built and discarded per run
	controller/hack/Dockerfile.controller.tilt                # Tilt inner loop, local only
	controller/hack/capd-kindest-node/Dockerfile       # CAPD node image, tag selected per k8s version
	controller/hack/dsx-imds-attestation/Dockerfile.mock # Tilt-only IMDS fixture
	controller/hack/dsx-imds-attestation/Dockerfile.server # Tilt-only SPIRE plugin fixture
	controller/hack/dsx-identity-exchange-mock/Dockerfile # e2e mock identity service
	controller/profiles/example/Dockerfile             # documentation example, never built by CI
	controller/test/fixtures/Dockerfile                # unit/e2e test fixture
)

is_exempt() {
	local f="$1" e
	for e in "${EXEMPT[@]}"; do
		[[ "${e}" == "${f}" ]] && return 0
	done
	return 1
}

fail=0
report() {
	echo "ERROR: $1: $2" >&2
	fail=1
}

# A stale exemption is worse than none: it reads as a considered decision about
# a file that no longer exists.
for e in "${EXEMPT[@]}"; do
	[[ -f "${e}" ]] || { echo "ERROR: EXEMPT lists ${e}, which is not in the tree" >&2; fail=1; }
done

# No `mapfile`: it is a bash 4.4 builtin, and on macOS bash 3.2 it leaves the
# array empty and exits 0 -- this would then check nothing and report success.
dockerfiles=()
while IFS= read -r path; do
	dockerfiles+=("${path}")
done < <(git ls-files -- '*Dockerfile' '*Dockerfile.*' | grep -v '^vendor/' | sort)
[[ ${#dockerfiles[@]} -gt 0 ]] || { echo "ERROR: no Dockerfiles found" >&2; exit 2; }

checked=0
for f in "${dockerfiles[@]}"; do
	if is_exempt "${f}"; then
		continue
	fi
	checked=$((checked + 1))

	# `FROM [--platform=...] <ref> [AS <stage>]`, with line continuations and
	# comments already excluded by the FROM anchor.
	while read -r ref; do
		# A later stage may build FROM an earlier stage by name. That is not a
		# base image, so it needs no digest.
		if grep -qiE "^FROM[[:space:]].*[[:space:]]AS[[:space:]]+${ref}[[:space:]]*$" "${f}"; then
			continue
		fi

		resolved="${ref}"
		# `FROM ${GOLANG_IMAGE}` -- resolve through the ARG default in the same
		# file. This script resolves it; Dependabot does not, which is the gap
		# described in the header.
		if [[ "${resolved}" =~ ^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$ ]]; then
			var="${BASH_REMATCH[1]}"
			resolved="$(sed -nE "s/^ARG[[:space:]]+${var}=(.+)$/\1/p" "${f}" | tail -1)"
			if [[ -z "${resolved}" ]]; then
				report "${f}" "FROM \${${var}} has no ARG ${var}=<image> default to pin"
				continue
			fi
		fi

		if [[ "${resolved}" != *@sha256:* ]]; then
			report "${f}" "base image ${resolved} is tag-only; pin it by digest"
		fi
	done < <(sed -nE 's/^FROM[[:space:]]+(--[^[:space:]]+[[:space:]]+)*([^[:space:]]+).*/\2/p' "${f}")
done

if (( fail )); then
	cat >&2 <<-'MSG'

	Pin with the tag kept alongside the digest, so the version stays readable:

	    FROM docker.io/alpine:3.24.1@sha256:28bd5fe8...

	Read the digest with:  crane digest docker.io/alpine:3.24.1

	If the image is development-only, add it to EXEMPT in this script with a
	reason instead.
	MSG
	exit 1
fi

echo "Base images: ${checked} release-relevant Dockerfiles digest-pinned, ${#EXEMPT[@]} exempt."
