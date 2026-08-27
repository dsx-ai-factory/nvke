#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

modules="$(awk '
  /^use \($/ { inside = 1; next }
  inside && /^\)$/ { inside = 0; next }
  inside {
    sub(/^[[:space:]]*\.\//, "")
    sub(/[[:space:]]*$/, "")
    if (length > 0) print
  }
' "${repo_root}/go.work" | LC_ALL=C sort)"

[[ -n "${modules}" ]] || { echo "ERROR: no modules found in go.work" >&2; exit 1; }

while IFS= read -r module; do
  [[ -f "${repo_root}/${module}/go.mod" ]] || {
    echo "ERROR: go.work module ${module} has no go.mod" >&2
    exit 1
  }
done <<<"${modules}"

printf '%s\n' "${modules}"
