#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Render the seed and site overlays offline with aicr and check each recipe and bundle has exactly the
# components the target is meant to deploy, in dependency order. Needs aicr on PATH (or AICR=/path/to/aicr).
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AICR="${AICR:-aicr}"
out="$(mktemp -d)"
trap 'rm -rf "${out}"' EXIT

expect_seed="postgres spire-crds temporal spire provisioned-provider capnico nvke-seed-config nke-controller"
expect_site="cert-manager nvke-base-config capnico nktp postgres spire-crds temporal spire nke-controller"

check() {
  local target=$1 expect=$2 got
  "${AICR}" recipe --service "nvke-${target}" --data "${HERE}" --output "${out}/${target}.yaml" 2>"${out}/err" >/dev/null || { cat "${out}/err" >&2; exit 1; }
  "${AICR}" bundle --recipe "${out}/${target}.yaml" --data "${HERE}" --deployer helm --output "${out}/${target}-bundle" 2>"${out}/err" >/dev/null || { cat "${out}/err" >&2; exit 1; }
  got="$(sed -n '/^componentRefs:/,$ s/^    name: //p' "${out}/${target}.yaml" | sort | tr '\n' ' ')"
  want="$(printf '%s\n' ${expect} | sort | tr '\n' ' ')"
  [ "${got}" = "${want}" ] || { printf 'nvke-%s: components differ\n  got  %s\n  want %s\n' "${target}" "${got}" "${want}" >&2; exit 1; }
  local n; n="$(find "${out}/${target}-bundle" -maxdepth 1 -name '[0-9][0-9][0-9]-*' | wc -l | tr -d ' ')"
  [ "${n}" = "$(printf '%s\n' ${expect} | wc -l | tr -d ' ')" ] || { printf 'nvke-%s: bundle has %s component dirs\n' "${target}" "${n}" >&2; exit 1; }
  printf 'nvke-%s: ok (%s)\n' "${target}" "${expect}"
}
check seed "${expect_seed}"
check site "${expect_site}"
