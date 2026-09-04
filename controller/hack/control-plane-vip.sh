#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

first_available_control_plane_vip() {
  local vip_range="${1}"
  local clusters="${2}"
  local first_vip="${3}"
  local prefix
  local candidate
  local used
  local host

  if [[ ! "${vip_range}" =~ ^([0-9]{1,3}\.){3}0/24$ ]]; then
    echo "control-plane VIP range must be an IPv4 /24 ending in .0" >&2
    return 1
  fi
  prefix="${vip_range%0/24}"
  host="${first_vip##*.}"
  if [[ "${first_vip%.*}." != "${prefix}" || ! "${host}" =~ ^[0-9]{1,3}$ ]]; then
    echo "first control-plane VIP must be in ${vip_range}" >&2
    return 1
  fi
  host=$((10#${host}))
  if ((host < 1 || host > 254)); then
    echo "first control-plane VIP must be a usable host address" >&2
    return 1
  fi

  used="$(jq -r '.clusters[]?.controlPlaneVIP // empty' <<<"${clusters}")"

  for ((; host <= 254; host++)); do
    candidate="${prefix}${host}"
    if ! grep -Fxq "${candidate}" <<<"${used}"; then
      echo "${candidate}"
      return
    fi
  done

  echo "no unused control-plane VIP remains in ${vip_range}" >&2
  return 1
}
