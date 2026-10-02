#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Print a git ref for the CAPNICo version pinned in controller/go.mod: the tag
# itself, or, for a pseudo-version (...yyyymmddhhmmss-abcdef123456), the full
# sha of the commit it names. actions/checkout and the contents API take either.
# Needs GH_TOKEN; uses gh when present and curl otherwise (not every runner has gh).
set -euo pipefail
repo=dsx-ai-factory/cluster-api-provider-nico
pin="$(awk -v m="github.com/${repo}" '$1 == m { print $2; exit }' "$(dirname "$0")/../../go.mod")"
[[ -n "${pin}" ]] || { echo "controller/go.mod does not pin ${repo}" >&2; exit 1; }
if [[ "${pin}" =~ [.-][0-9]{14}-([0-9a-f]{12})$ ]]; then
  if command -v gh >/dev/null; then
    gh api "repos/${repo}/commits/${BASH_REMATCH[1]}" --jq .sha
  else
    curl -fsSL -H "Authorization: Bearer ${GH_TOKEN:?}" "https://api.github.com/repos/${repo}/commits/${BASH_REMATCH[1]}" | jq -r .sha
  fi
else
  printf '%s\n' "${pin}"
fi
