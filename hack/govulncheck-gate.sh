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
# <module>=<OSV-id>[,<OSV-id>...], space-separated across modules.
waived_spec="${GOVULNCHECK_WAIVED:-}"
waiver_review="${GOVULNCHECK_WAIVER_REVIEW:-}"
waiver_owner="${GOVULNCHECK_WAIVER_OWNER:-}"

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

# Captured, not streamed, because the waiver is scoped to an advisory now and
# the report is the only place the advisory IDs appear. Echoed back either way,
# so a human still sees exactly what govulncheck said.
report="$(mktemp)"
trap 'rm -f "${report}"' EXIT
set +e
(cd "${repo_root}/${module}" && "${scanner}" ./...) >"${report}" 2>&1
rc=$?
set -e
cat "${report}"

if [[ ${rc} -ne 0 && ${rc} -ne 3 ]]; then
  error "govulncheck failed to run in ${module} (exit ${rc})"
  exit "${rc}"
fi

# The advisories waived for THIS module, if any.
allowed=""
for entry in ${waived_spec}; do
  [[ "${entry%%=*}" == "${module}" ]] || continue
  allowed="${entry#*=}"
  allowed="${allowed//,/ }"
done

if [[ -z "${allowed}" ]]; then
  [[ ${rc} -eq 0 ]] && exit 0
  error "${module} calls a vulnerable symbol"
  exit 3
fi

# Only the "Symbol Results" section lists advisories this code actually calls.
# Anything below it is imported-but-not-called and is not what the gate is for.
called="$(awk '/^=== Symbol Results ===/{s=1; next} /^=== /{s=0} s' "${report}" |
  grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u || true)"

fail=0
for id in ${called}; do
  case " ${allowed} " in
    *" ${id} "*) ;;
    *) error "${module} calls ${id}, which is not waived"; fail=1 ;;
  esac
done
for id in ${allowed}; do
  case " ${called} " in
    *" ${id} "*) ;;
    *) error "${module} no longer calls ${id}; remove it from GOVULNCHECK_WAIVED"; fail=1 ;;
  esac
done

if [[ ${fail} -ne 0 ]]; then
  [[ -n "${waiver_owner}" ]] && error "waiver owner: ${waiver_owner}"
  exit 3
fi

# A date that fails the build would get the whole mechanism deleted the first
# time it fired on a Monday. Warn loudly instead; the un-waive check above is
# what actually stops rot.
if [[ -n "${waiver_review}" ]] && [[ "$(date -u +%Y-%m-%d)" > "${waiver_review}" ]]; then
  warn "the ${module} waiver passed its review date ${waiver_review} (owner: ${waiver_owner:-unassigned})"
fi

warn "${module} is waived for ${allowed// /, } and still calls it"
exit 0
