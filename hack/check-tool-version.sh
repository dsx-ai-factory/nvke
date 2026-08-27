#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 <tool> <version>" >&2; exit 2; }

tool="$1"
expected="$2"

install_hint() {
  case "${tool}" in
    actionlint) echo "go install github.com/rhysd/actionlint/cmd/actionlint@v${expected#v}" ;;
    kubeconform) echo "https://github.com/yannh/kubeconform/releases/tag/${expected}" ;;
    govulncheck) echo "go install golang.org/x/vuln/cmd/govulncheck@${expected}" ;;
    helm) echo "https://github.com/helm/helm/releases/tag/${expected}" ;;
    shellcheck) echo "https://github.com/koalaman/shellcheck/releases/tag/v${expected#v}" ;;
    *) echo "install ${tool} ${expected}" ;;
  esac
}

fail() {
  echo "ERROR: $*" >&2
  echo "Use 'flox activate', or install: $(install_hint)" >&2
  exit 1
}

command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is missing; expected ${expected}"

case "${tool}" in
  actionlint)
    actual="$(actionlint -version 2>&1 | awk 'NR == 1 { print $1 }')"
    actual="${actual#v}"
    ;;
  shellcheck)
    actual="$(shellcheck --version | awk '$1 == "version:" { print $2 }')"
    ;;
  helm)
    actual="$(helm version --short | awk -F+ '{ print $1 }')"
    ;;
  kubeconform)
    actual="$(kubeconform -v | tr -d '[:space:]')"
    ;;
  govulncheck)
    actual="$(govulncheck -version 2>&1 | awk -F@ '$1 == "Scanner: govulncheck" { print $2 }')"
    ;;
  *)
    fail "no version parser for ${tool}"
    ;;
esac

[[ -n "${actual}" ]] || fail "could not read the ${tool} version"
[[ "${actual}" == "${expected}" ]] || fail "${tool} is ${actual}; expected ${expected}"

echo "  ${tool} ${actual}"
