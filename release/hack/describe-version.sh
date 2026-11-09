#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#
# Print the version every artifact built from a commit is published under. A
# bare v* tag run is published under that tag; any other run under the nearest
# v* tag, described. The publish workflows resolve their version here rather
# than calling git describe themselves, so each one gets the same checks before
# it pushes anything.
#
# Usage: release/hack/describe-version.sh [commit]   (default HEAD)
#
# Refuses, rather than prints:
#   - a describe that is not a v* release, RC or beta version, its describe
#     suffix, or a bare commit. A tag name reaches make and the shell, and the
#     Makefiles describe the commit themselves, so nothing else may pass
#     through, whichever tag started the run.
#   - on a v* tag run, a tag that is not a version, or that does not name the
#     commit being built.
#   - on a component tag run (agent/v*, cli/v*, ...), a commit a v* tag already
#     names. Its describe is that tag, and the run would overwrite the
#     artifacts that tag published.
#
# Naming a v* tag run by its own tag rather than the describe matters when the
# commit carries several: git describe prefers an annotated release over a
# beta, and of two lightweight betas the first by name, so a beta re-run on a
# released commit, or a second beta on one commit, would otherwise publish
# under another tag's version.
#
# GITHUB_REF is the ref that started the run; unset outside Actions, which
# skips the tag checks.

set -euo pipefail

commit="${1:-HEAD}"
version="$(git describe --tags --always --match 'v*' "${commit}")"

tag_re='^v[0-9]+\.[0-9]+\.[0-9]+(-(beta|rc)\.[0-9]+)?$'
version_re='^(v[0-9]+\.[0-9]+\.[0-9]+(-(beta|rc)\.[0-9]+)?(-[0-9]+-g[0-9a-f]+)?|[0-9a-f]{7,40})$'

if ! [[ "${version}" =~ ${version_re} ]]; then
  echo "::error::${commit} describes to '${version}', which is not a version this repository publishes" >&2
  exit 1
fi

ref="${GITHUB_REF:-}"
case "${ref}" in
  refs/tags/v*)
    tag="${ref#refs/tags/}"
    if ! [[ "${tag}" =~ ${tag_re} ]]; then
      echo "::error::${tag} is not a version this repository publishes" >&2
      exit 1
    fi
    tagged="$(git rev-parse --verify --quiet "refs/tags/${tag}^{commit}" || true)"
    if [[ "${tagged}" != "$(git rev-parse --verify "${commit}^{commit}")" ]]; then
      echo "::error::${tag} does not name ${commit}; refusing to publish ${commit} under it" >&2
      exit 1
    fi
    version="${tag}"
    ;;
  refs/tags/*/v*)
    if [[ "${version}" =~ ${tag_re} ]]; then
      echo "::error::${ref#refs/tags/} is on a commit ${version} already names; refusing to republish ${version}" >&2
      exit 1
    fi
    ;;
esac

echo "${version}"
