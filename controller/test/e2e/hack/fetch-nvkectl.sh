#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stage a published nvkectl for the site lane: pull the artifact SITE_CLI_REF
# names, and check the binary reports the version it was asked for.
#
#   SITE_CLI_REF=<registry>/nvkectl:<version>-linux-<arch> \
#     fetch-nvkectl.sh <version> <arch> <dir>
#
# Writes <dir>/nvkectl-linux-<arch>. `make site-inputs SITE_CLI_BUILD=0` runs
# this, and the Forge workflow is one caller of that.
#
# nvkectl carries no index above its platform tags, so the artifact is tagged
# <version>-<os>-<arch>. The caller supplies the whole reference, which the
# qualification plan resolved.
#
# What the pull guarantees, exactly: oras checks each blob against the manifest
# the registry served for that tag, so a truncated or corrupted transfer fails.
# It does not pin the tag. Nothing here compares that manifest with a digest
# recorded independently of it, so an artifact republished under the same tag
# pulls cleanly, and the binary then runs on the site node. The version check
# below is a separate claim, that the binary is the one asked for rather than
# that it is intact.
#
# oras reads the registry credentials docker login wrote, so a caller that can
# pull the images can pull this.
set -Eeuo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <version> <arch> <dir>" >&2
	exit 2
fi
version="$1"
arch="$2"
dir="$3"

[ -n "${SITE_CLI_REF:-}" ] || {
	echo "SITE_CLI_REF is required: it names the artifact to stage" >&2
	exit 1
}
case "${arch}" in
amd64 | arm64) ;;
*)
	echo "unsupported arch ${arch}; nvkectl publishes linux/amd64 and linux/arm64" >&2
	exit 1
	;;
esac
command -v oras >/dev/null || {
	echo "oras is not installed, so ${SITE_CLI_REF} cannot be pulled" >&2
	exit 1
}

out="nvkectl-linux-${arch}"
mkdir -p "${dir}"

# site.nvkectl points at this path, so a binary that fails any check below must
# not be left there for a later suite run to pick up.
trap '[ "$?" -eq 0 ] || rm -f "${dir}/${out}"' EXIT

if ! oras pull --output "${dir}" "${SITE_CLI_REF}"; then
	echo "no artifact at ${SITE_CLI_REF}" >&2
	exit 1
fi
[ -f "${dir}/${out}" ] || {
	echo "${SITE_CLI_REF} carries no ${out}, it holds:" >&2
	ls -1 "${dir}" >&2
	exit 1
}
chmod 0755 "${dir}/${out}"

# Only a binary for this host can be run. For any other the digest is the whole
# check, and the bootstrap is the first thing to execute it.
host_arch="$(uname -m)"
case "${host_arch}" in
x86_64) host_arch=amd64 ;;
aarch64 | arm64) host_arch=arm64 ;;
esac
if [ "$(uname -s)" != Linux ] || [ "${host_arch}" != "${arch}" ]; then
	echo "staged ${out} ${version} from ${SITE_CLI_REF} (not run, this host is $(uname -s)/${host_arch})"
	exit 0
fi
if ! out_version="$("${dir}/${out}" version 2>&1)"; then
	echo "${out} version exited non-zero: ${out_version}" >&2
	exit 1
fi
# The trailing space stops v0.1.0 accepting a v0.1.0-rc.1 binary.
case "${out_version}" in
"nvkectl ${version} "*) ;;
*)
	echo "asked for ${version}, and ${SITE_CLI_REF} reports: ${out_version}" >&2
	exit 1
	;;
esac
echo "staged ${out}: ${out_version}"
