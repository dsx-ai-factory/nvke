#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stage a published nkectl for the site lane: download nkectl-linux-<arch>
# from the cli/<version> GitHub Release, check it against that release's
# checksums.txt, and check it reports the version it was asked for.
#
#   fetch-nkectl.sh <version> <arch> <dir>
#
# Writes <dir>/nkectl-linux-<arch>. `make site-inputs SITE_CLI_BUILD=0` runs
# this; the Forge workflow is one caller of that.
#
# The repository is private, so the assets come through the API with a token
# rather than their browser URLs. GITHUB_TOKEN is required; GITHUB_REPOSITORY
# and GITHUB_API_URL default to this repository on github.com, and Actions
# sets all three.
set -Eeuo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <version> <arch> <dir>" >&2
	exit 2
fi
version="$1"
arch="$2"
dir="$3"

# The two shapes release.yml publishes a cli/<version> tag in.
if [[ ! "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
	echo "version '${version}' is neither vX.Y.Z nor vX.Y.Z-rc.N, so no cli/ release carries it" >&2
	exit 1
fi
case "${arch}" in
amd64 | arm64) ;;
*)
	echo "unsupported arch ${arch}; nkectl releases ship linux/amd64 and linux/arm64" >&2
	exit 1
	;;
esac
[ -n "${GITHUB_TOKEN:-}" ] || {
	echo "GITHUB_TOKEN is required to read the cli/${version} release assets" >&2
	exit 1
}
command -v jq >/dev/null || {
	echo "jq is not installed, so the release assets cannot be located" >&2
	exit 1
}

asset="nkectl-linux-${arch}"
tag="cli/${version}"
api="${GITHUB_API_URL:-https://api.github.com}/repos/${GITHUB_REPOSITORY:-dsx-ai-factory/nke}"
mkdir -p "${dir}"
# site.nkectl points at this path, so a binary that fails any check below
# must not be left there for a later suite run to pick up.
trap '[ "$?" -eq 0 ] || rm -f "${dir}/${asset}"' EXIT

if ! release="$(curl -fsSL --retry 3 -H "Authorization: Bearer ${GITHUB_TOKEN}" \
	-H "Accept: application/vnd.github+json" "${api}/releases/tags/${tag}")"; then
	echo "no published ${tag} release" >&2
	exit 1
fi
for name in "${asset}" checksums.txt; do
	id="$(jq -r --arg n "${name}" '.assets[] | select(.name == $n) | .id' <<<"${release}")"
	if [ -z "${id}" ]; then
		echo "the ${tag} release has no ${name}" >&2
		exit 1
	fi
	curl -fsSL --retry 3 -H "Authorization: Bearer ${GITHUB_TOKEN}" \
		-H "Accept: application/octet-stream" -o "${dir}/${name}" "${api}/releases/assets/${id}"
done
# checksums.txt is sha256sum's own output, one line per asset.
(cd "${dir}" && grep -E "^[0-9a-f]{64}  ${asset}\$" checksums.txt | sha256sum -c -) || {
	echo "${asset} does not match the ${tag} checksums.txt" >&2
	exit 1
}
chmod 0755 "${dir}/${asset}"

# Only a binary for this host can be run. For any other the checksum above is
# the whole check, and the bootstrap is the first thing to execute it.
host_arch="$(uname -m)"
case "${host_arch}" in
x86_64) host_arch=amd64 ;;
aarch64 | arm64) host_arch=arm64 ;;
esac
if [ "$(uname -s)" != Linux ] || [ "${host_arch}" != "${arch}" ]; then
	echo "staged ${asset} ${version} (checksum verified; not run, this host is $(uname -s)/${host_arch})"
	exit 0
fi
if ! out="$("${dir}/${asset}" version 2>&1)"; then
	echo "${asset} version exited non-zero: ${out}" >&2
	exit 1
fi
# The trailing space stops v0.1.0 accepting a v0.1.0-rc.1 binary.
case "${out}" in
"nkectl ${version} "*) ;;
*)
	echo "asked for ${version}, and the ${tag} binary reports: ${out}" >&2
	exit 1
	;;
esac
echo "staged ${asset}: ${out}"
