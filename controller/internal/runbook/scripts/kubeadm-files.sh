#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Purpose: Stage rendered kubeadm prerequisite files for agents that cannot
# write the files carried by KubeadmInitRequest or KubeadmJoinRequest.

set -euo pipefail

error() {
  echo "Error: $1" >&2
  exit 1
}

decode_base64() {
  if base64 --decode </dev/null >/dev/null 2>&1; then
    base64 --decode
  else
    base64 -D
  fi
}

stage_file() {
  local path="$1"
  local encoded_content="$2"
  local mode="$3"
  local owner="$4"

  if [[ "$path" != /* ]]; then
    error "file path must be absolute: $path"
  fi
  if ! [[ "$mode" =~ ^[0-7]{3,4}$ ]]; then
    error "file mode must be octal: $mode"
  fi
  if [[ -z "$owner" ]]; then
    error "file owner must not be empty: $path"
  fi

  local directory
  directory="$(dirname -- "$path")"
  mkdir -p -- "$directory"

  local temporary_file
  temporary_file="$(mktemp "${directory}/.nke-kubeadm-file.XXXXXX")"
  if ! printf '%s' "$encoded_content" | decode_base64 >"$temporary_file"; then
    rm -f -- "$temporary_file"
    error "failed to decode content for $path"
  fi
  chmod "$mode" "$temporary_file" || {
    rm -f -- "$temporary_file"
    error "failed to set mode on $path"
  }
  chown "$owner" "$temporary_file" || {
    rm -f -- "$temporary_file"
    error "failed to set owner on $path"
  }
  mv -f -- "$temporary_file" "$path" || {
    rm -f -- "$temporary_file"
    error "failed to install $path"
  }
}

if (( $# == 0 || $# % 4 != 0 )); then
  error "expected one or more path/content/mode/owner argument groups"
fi

while (( $# > 0 )); do
  stage_file "$1" "$2" "$3" "$4"
  shift 4
done
