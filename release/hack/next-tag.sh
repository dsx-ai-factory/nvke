#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Prints the next release tag for a branch or commit without creating it.
# usage: release/hack/next-tag.sh <BRANCH> [<PRE-RELEASE>]
#
# The version is read from the conventional commits since the latest GA tag. A
# feat, or a breaking change of any type, bumps the minor; a fix bumps the patch.
# A breaking change is a ! after the type, or a BREAKING CHANGE footer in the
# body outside a fenced code block. The major is never bumped: 1.0 is cut by
# hand. With neither, nothing is releasable, and the script exits 3 having
# printed nothing on stdout.
#
# next_version prints the next release tag for a branch or commit.
# Args:
#   $1 - branch or other committish to inspect
#   $2 - prerelease label (e.g. "beta" or "rc") or empty for GA.
function next_version() {
    local branch="$1"
    local prerelease="$2"

    local prefix="v"
    local latest_ga="$(git tag -l | grep "^${prefix}[0-9]\+\.[0-9]\+\.[0-9]\+$" | sort -V | tail -1)"
    local range="${latest_ga}..${branch}"

    local major="$(cut -d. -f1 <<< "${latest_ga#"${prefix}"}")"
    local minor="$(cut -d. -f2 <<< "${latest_ga#"${prefix}"}")"
    local patch="$(cut -d. -f3 <<< "${latest_ga#"${prefix}"}")"

    # Read into variables rather than piped: grep -q exits on the first match,
    # and git log, killed by SIGPIPE, would fail the pipeline under pipefail.
    # The footer token is uppercase, and BREAKING-CHANGE is its synonym.
    #
    # The footer is looked for on any line of a body, not only in its last
    # paragraph. Git does not read BREAKING CHANGE as a trailer, so commit -s
    # puts the sign-off in a paragraph after it, and a squash merge joins every
    # commit's message into one body. A line inside a fenced code block is an
    # example rather than a footer, and each commit starts outside a fence, so
    # one left open cannot hide the next commit's footer. As in CommonMark, a
    # fence closes only on the opener's character, at least as many of it, and
    # nothing after them, so an example of a fence does not end the block. A
    # line indented four spaces or more is indented code, never a fence.
    local subjects bodies
    subjects="$(git log --format='%s' "${range}")"
    bodies="$(git log --format='%x1e%n%b' "${range}" | awk -v commit=$'\036' '
        $0 == commit { fenced = 0; next }
        /^ ? ? ?(```|~~~)/ {
            line = $0; sub(/^ */, "", line)
            ch = substr(line, 1, 1); n = 0
            while (substr(line, n + 1, 1) == ch) n++
            if (!fenced) { fenced = 1; fch = ch; flen = n }
            else if (ch == fch && n >= flen && substr(line, n + 1) ~ /^[ \t]*$/) fenced = 0
            next
        }
        !fenced')"
    if grep -Eq '^(feat(\([^)]*\))?!?|[a-z]+(\([^)]*\))?!):' <<< "${subjects}" \
        || grep -Eq '^BREAKING[ -]CHANGE: ' <<< "${bodies}"; then
        minor=$((minor+1))
        patch=0
    elif grep -Eq '^fix(\([^)]*\))?:' <<< "${subjects}"; then
        patch=$((patch+1))
    else
        echo "nothing releasable since ${latest_ga}: no feat or fix commit" >&2
        exit 3
    fi

    local base="${prefix}${major}.${minor}.${patch}"
    if [[ -z "${prerelease}" ]]; then
        echo "${base}"
        return
    fi

    local latest_prerelease=$(git tag -l | grep "^${base}-${prerelease}\\.[0-9]\+$" | sort -V | tail -1)
    local n=1
    if [[ -n "${latest_prerelease}" ]]; then
        n=$(( $(cut -d. -f4 <<< "${latest_prerelease}") + 1 ))
    fi

    echo "${base}-${prerelease}.${n}"
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
    echo "usage: release/hack/next-tag.sh <BRANCH> [<PRE-RELEASE>]" >&2
    exit 1
fi

next_version "$1" "${2:-}"
