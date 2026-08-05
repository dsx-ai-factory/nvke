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
# Emit the apt package specs (name[=version]) that the *active* osProfiles for a
# given architecture + ubuntu_version install from the NKE apt repository, one
# spec per line. The output is a manifest consumed by check-apt-packages.sh.
#
# Usage:
#   extract-osprofile-packages.sh <architecture> <ubuntu_version>
#   e.g. extract-osprofile-packages.sh amd64 24.04
#
# Sources of truth:
#   - metadata.yaml                -> which profiles match arch + ubuntu_version
#                                     (deprecated profiles live in deprecated.yaml
#                                     and are therefore excluded automatically)
#   - <profile>/userData.tmpl      -> the apt-get install commands
#   - apt-validation-k8s-versions.txt (or $K8S_VERSIONS) -> versions used to
#                                     expand injected nke-kubeadm-distro-<ver>
#
# Deliberately excluded (not served by the apt repo, so not our concern here):
#   - local .deb installs (any token containing "/", e.g. /tmp/nke-agent.deb)
#   - bundle/tarball installs (fetch-and-install-bundle.sh — not apt-get install)
#   - unresolved Go-template package tokens (anything still containing {{ }})

set -euo pipefail

ARCH="${1:?usage: extract-osprofile-packages.sh <architecture> <ubuntu_version>}"
UBUNTU_VERSION="${2:?usage: extract-osprofile-packages.sh <architecture> <ubuntu_version>}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The osProfiles live in the repo's profiles/ tree (this script lives under
# test/osProfiles/). Override with OSPROFILES_DIR if the layout changes.
OSPROFILES_DIR="${OSPROFILES_DIR:-$(cd "${SCRIPT_DIR}/../../profiles/osProfiles" && pwd)}"
METADATA_FILE="${OSPROFILES_DIR}/metadata.yaml"
K8S_VERSIONS_FILE="${SCRIPT_DIR}/apt-validation-k8s-versions.txt"

command -v yq >/dev/null || { echo "error: yq is required" >&2; exit 1; }

# Kubernetes versions used to expand injected nke-kubeadm-distro-<ver>.
if [[ -n "${K8S_VERSIONS:-}" ]]; then
  read -r -a K8S_VERS <<< "${K8S_VERSIONS}"
else
  mapfile -t K8S_VERS < <(grep -vE '^\s*#|^\s*$' "${K8S_VERSIONS_FILE}" | tr -d ' \t\r')
fi
if [[ ${#K8S_VERS[@]} -eq 0 ]]; then
  echo "error: no Kubernetes versions loaded (checked K8S_VERSIONS env var and ${K8S_VERSIONS_FILE})" >&2
  exit 1
fi

# Active profiles matching arch + ubuntu_version, from metadata.yaml.
mapfile -t PROFILES < <(
  yq eval -r \
    ".profiles | to_entries | .[] | select(.value.architecture == \"${ARCH}\" and .value.ubuntu_version == \"${UBUNTU_VERSION}\") | .key" \
    "${METADATA_FILE}"
)

# Pull package tokens from a profile's userData.tmpl. Handles backslash line
# continuations, collapses Go-template expressions (whose inner whitespace would
# otherwise break tokenizing), and keeps the injected K8s version as a __K8SVER__
# marker for the caller to expand.
emit_tokens_for_profile() {
  local profile="$1"
  local tmpl="${OSPROFILES_DIR}/${profile}/userData.tmpl"
  [[ -f "${tmpl}" ]] || return 0

  awk '
    function flush(s,   seg, arr, n, i, tok) {
      gsub(/\{\{[^}]*KubernetesVersion[^}]*\}\}/, "__K8SVER__", s)
      gsub(/\{\{[^}]*\}\}/, "__TMPL__", s)
      while (match(s, /apt-get install /)) {
        seg = substr(s, RSTART + RLENGTH)
        sub(/[&|;].*$/, "", seg)          # stop at a shell separator
        n = split(seg, arr, /[ \t]+/)
        for (i = 1; i <= n; i++) {
          tok = arr[i]
          if (tok == "")            continue
          if (tok ~ /^-/)           continue   # flags like -y
          if (tok ~ /\//)           continue   # local file installs (/tmp/*.deb)
          if (tok ~ /__TMPL__/)     continue   # unresolved Go-template token
          if (tok ~ /\$/)           continue   # shell var (e.g. ${TARGET_KERNEL}) — runtime-only
          print tok
        }
        s = substr(s, RSTART + RLENGTH)
      }
    }
    {
      cur = cur $0
      if (cur ~ /\\[ \t]*$/) { sub(/\\[ \t]*$/, " ", cur); next }   # join continuation
      flush(cur); cur = ""
    }
    END { if (cur != "") flush(cur) }
  ' "${tmpl}"
}

{
  for p in "${PROFILES[@]:-}"; do
    [[ -z "${p}" ]] && continue
    emit_tokens_for_profile "${p}"
  done
} | while IFS= read -r tok; do
  [[ -z "${tok}" ]] && continue
  if [[ "${tok}" == *"__K8SVER__"* ]]; then
    for v in "${K8S_VERS[@]:-}"; do
      [[ -z "${v}" ]] && continue
      echo "${tok/__K8SVER__/${v}}"
    done
  else
    echo "${tok}"
  fi
done | sort -u
