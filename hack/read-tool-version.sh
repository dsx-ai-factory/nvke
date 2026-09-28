#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Read a pin out of make/tool-versions.mk, for the callers that cannot include
# it. A shell script that restates a pin is a second declaration, and
# hack/tool-version-check.sh rejects it; this is how such a script gets the
# value instead.
#
# Source it and call read_tool_version:
#
#     . "$(git rev-parse --show-toplevel)/hack/read-tool-version.sh"
#     SPIRE_AGENT_VERSION="$(read_tool_version SPIRE_VERSION)"
#
# An unknown name is a hard error rather than an empty string, so a pin that is
# renamed in the owner file fails here instead of downloading ""-tagged bytes.

# Resolved from this file's own location, so a caller at any depth works and no
# caller has to count "..". Not readonly: sourcing twice in one shell is a
# caller's business, not an error.
TOOL_VERSIONS_MK="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/make/tool-versions.mk"

read_tool_version() {
  local name="${1:?usage: read_tool_version NAME}" value
  value="$(sed -nE "s/^${name}[[:space:]]*\\??=[[:space:]]*([^[:space:]#]+).*/\\1/p" "${TOOL_VERSIONS_MK}" | tail -1)"
  if [[ -z "${value}" ]]; then
    printf '%s is not declared in %s\n' "${name}" "${TOOL_VERSIONS_MK}" >&2
    return 1
  fi
  printf '%s' "${value}"
}
