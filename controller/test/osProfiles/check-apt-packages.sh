#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
#
# Generic apt-repository consumer check. Configures an apt repo the same way an
# NKE node does (signed-by keyring + sources.list.d), runs `apt-get update`, and
# verifies every package spec in a manifest is installable via a dry-run
# `apt-get install -s`. This validates: repo reachable, index GPG-signed and
# parseable, package present, exact pinned version present, and dependencies
# satisfiable.
#
# It knows nothing about NKE — pass it any repo + any manifest and it works.
# Meant to run inside an ubuntu:<distro> container matching the target distro.
#
# Usage:
#   check-apt-packages.sh <manifest-file>
#
# Required env:
#   APT_REPO_URL   e.g. https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-noble
#   APT_SUITE      e.g. nke-noble   (NOT plain "noble")
#   APT_GPG_URL    e.g. https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-public.key
# Optional env:
#   APT_COMPONENT  default: main
#   APT_KEYRING    default: /etc/apt/keyrings/apt-check.gpg
#   APT_LIST_NAME  default: apt-check   (-> /etc/apt/sources.list.d/<name>.list)

set -euo pipefail

MANIFEST="${1:?usage: check-apt-packages.sh <manifest-file>}"
: "${APT_REPO_URL:?APT_REPO_URL must be set}"
: "${APT_SUITE:?APT_SUITE must be set}"
: "${APT_GPG_URL:?APT_GPG_URL must be set}"
APT_COMPONENT="${APT_COMPONENT:-main}"
APT_KEYRING="${APT_KEYRING:-/etc/apt/keyrings/apt-check.gpg}"
APT_LIST_NAME="${APT_LIST_NAME:-apt-check}"

export DEBIAN_FRONTEND=noninteractive

# Base tools — a fresh ubuntu image may lack curl/gpg/ca-certificates.
if ! command -v curl >/dev/null || ! command -v gpg >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq curl gnupg ca-certificates >/dev/null
fi

# Configure the repo exactly as an NKE node does.
install -d -m 0755 /etc/apt/keyrings
curl -fsSL "${APT_GPG_URL}" | gpg --dearmor -o "${APT_KEYRING}"
echo "deb [signed-by=${APT_KEYRING}] ${APT_REPO_URL} ${APT_SUITE} ${APT_COMPONENT}" \
  > "/etc/apt/sources.list.d/${APT_LIST_NAME}.list"

echo "Configured apt repo: ${APT_REPO_URL} ${APT_SUITE} ${APT_COMPONENT}"
# `apt-get update` also proves the repo is reachable and its index GPG-signed.
apt-get update

if [[ ! -s "${MANIFEST}" ]]; then
  echo "No packages to check (empty manifest) — nothing required for this arch/distro. Skipping."
  exit 0
fi

echo ""
echo "Checking $(grep -cvE '^\s*$' "${MANIFEST}") package spec(s) against ${APT_SUITE}..."

ok=0 fail=0
missing=()
err_file="$(mktemp)"
while IFS= read -r spec; do
  [[ -z "${spec}" ]] && continue
  if apt-get install -s -y "${spec}" >/dev/null 2>"${err_file}"; then
    echo "  [OK]   ${spec}"
    ok=$((ok + 1))
  else
    echo "  [FAIL] ${spec}"
    grep -iE 'unable to locate|no installation candidate|was not found|unmet dependencies|has no installation' \
      "${err_file}" | head -1 | sed 's/^/           /' || true
    missing+=("${spec}")
    fail=$((fail + 1))
  fi
done < "${MANIFEST}"
rm -f "${err_file}"

echo ""
echo "apt package check (${APT_SUITE}): ${ok} available, ${fail} missing"

if (( fail > 0 )); then
  echo ""
  echo "MISSING from ${APT_REPO_URL} ${APT_SUITE} — required by CWE osProfiles:"
  printf '  - %s\n' "${missing[@]}"
  echo ""
  echo "Each missing spec is installed by an osProfile but is not available in the apt index."
  echo "Likely a dropped/never-published .deb. Recover via the publisher:"
  echo "  - restore a specific deb:  PUBLISHER_RESTORE_DEBS=<name>_<version>_<arch>.deb"
  echo "  - or re-run the publisher pipeline to republish, then re-run this job."
  exit 1
fi

echo "All required packages are available in ${APT_SUITE}."
