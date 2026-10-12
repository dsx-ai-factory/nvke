#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Prints the next release tag for a branch or commit without creating it.
# usage: hack/next-tag.sh <BRANCH> [<PRE-RELEASE>]
#
# next_version prints the next release tag for a branch or commit.
# Args:
#   $1 - branch or other committish to inspect
#   $2 - prerelease label (e.g. "beta") or empty for GA.
# Bumps minor if any commit since the latest GA tag starts with "feat", else patch.
function next_version() {
    local branch="$1"
    local prerelease="$2"

    local prefix="v"
    local latest_ga="$(git tag -l | grep "^${prefix}[0-9]\+\.[0-9]\+\.[0-9]\+$" | sort -V | tail -1)"
    local range="${latest_ga}..${branch}"

    local major="$(cut -d. -f1 <<< "${latest_ga#"${prefix}"}")"
    local minor="$(cut -d. -f2 <<< "${latest_ga#"${prefix}"}")"
    local patch="$(cut -d. -f3 <<< "${latest_ga#"${prefix}"}")"

    if git log --format='%s' "${range}" | grep '^feat' &>/dev/null; then
        minor=$((minor+1))
        patch=0
    else
        patch=$((patch+1))
    fi

    local base="${prefix}${major}.${minor}.${patch}"
    if [[ -z "${prerelease}" ]]; then
        echo "${base}"
        return
    fi

    local latest_prerelease=$(git tag -l | grep "^${base}-${prerelease}\\.[0-9]\+$" | sort -V | tail -1)
    local n=1
    if [[ -n "${latest_prerelease}" ]]; then
        n=$(("$(cut -d. -f4 <<< "${latest_prerelease}")"+1))
    fi

    echo "${base}-${prerelease}.${n}"
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
    echo "usage: hack/next-tag.sh <BRANCH> [<PRE-RELEASE>]" >&2
    exit 1
fi

next_version "$1" "${2:-}"
