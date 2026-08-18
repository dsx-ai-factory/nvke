#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Update a single KEY=value entry in an env file while keeping the result
# portable across macOS and Linux.
#
# We intentionally avoid `sed -i` here because BSD `sed` (macOS) and GNU `sed`
# (Linux) behave differently for in-place edits. We also need to collapse any
# duplicate entries for the same key into exactly one line; we previously hit a
# duplicate-key failure for `FORGE_ENVIRONMENT` when generating the ConfigMap.

if [[ "$#" -ne 3 ]]; then
  echo "usage: $0 <path> <key> <value>" >&2
  exit 1
fi

path="$1"
key="$2"
value="$3"
tmp="${path}.tmp.$$"

input="/dev/null"
if [[ -f "${path}" ]]; then
  input="${path}"
fi

awk -v key="${key}" -v value="${value}" '
BEGIN {
  prefix = key "="
  updated = 0
}
index($0, prefix) == 1 {
  if (!updated) {
    print prefix value
    updated = 1
  }
  next
}
{
  print
}
END {
  if (!updated) {
    print prefix value
  }
}
' "${input}" > "${tmp}"

mv "${tmp}" "${path}"
