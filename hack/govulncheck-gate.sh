#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Gate one workspace module on called vulnerabilities. CI and make use this
# script so the offline scan and waiver behavior stay identical.

set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 <module>" >&2; exit 2; }

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module="$1"
scanner="${GOVULNCHECK:-govulncheck}"
waived_modules="${GOVULNCHECK_WAIVED_MODULES:-}"

[[ -f "${repo_root}/${module}/go.mod" ]] || {
  echo "ERROR: ${module} is not a Go module in this workspace" >&2
  exit 2
}

error() {
  if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
    echo "::error::$*" >&2
  else
    echo "ERROR: $*" >&2
  fi
}

warn() {
  if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
    echo "::warning::$*" >&2
  else
    echo "WARNING: $*" >&2
  fi
}

set +e
(cd "${repo_root}/${module}" && GOPROXY=off "${scanner}" ./...)
rc=$?
set -e

if [[ ${rc} -ne 0 && ${rc} -ne 3 ]]; then
  error "govulncheck failed to run in ${module} (exit ${rc})"
  exit "${rc}"
fi

waived=false
for candidate in ${waived_modules}; do
  [[ "${candidate}" == "${module}" ]] && waived=true
done

if [[ "${waived}" == "false" ]]; then
  [[ ${rc} -eq 0 ]] && exit 0
  error "${module} calls a vulnerable symbol"
  exit 3
fi

if [[ ${rc} -eq 3 ]]; then
  warn "${module} is waived and still has a reachable vulnerability"
  exit 0
fi

error "${module} is clean but still waived; remove its waiver"
exit 1
