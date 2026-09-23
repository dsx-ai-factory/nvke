#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Check the loader, dependencies, and maximum required Ubuntu glibc ABI.
set -euo pipefail
[[ "${1:-}" == --arch && $# -ge 3 ]] || { echo 'Usage: check-ubuntu-elf --arch amd64|arm64 BINARY...' >&2; exit 2; }
case "$2" in
  amd64) loader=/lib64/ld-linux-x86-64.so.2 ;;
  arm64) loader=/lib/ld-linux-aarch64.so.1 ;;
  *) echo "Unsupported architecture: $2" >&2; exit 2 ;;
esac
shift 2
fail() { echo "FAIL: $binary: $*" >&2; exit 1; }
for binary in "$@"; do
  headers=$(readelf -lW "$binary")
  dynamic=$(readelf -dW "$binary")
  versions=$(readelf -VW "$binary")
  [[ "$headers" == *"[Requesting program interpreter: $loader]"* ]] || fail "expected Ubuntu loader $loader"
  if grep -Eq '\((RPATH|RUNPATH)\)' <<< "$dynamic"; then fail 'unexpected runtime library search path'; fi
  needed=$(sed -n 's/.*(NEEDED).*\[\(.*\)\].*/\1/p' <<< "$dynamic")
  grep -Fxq libc.so.6 <<< "$needed" || fail 'missing libc.so.6 dependency'
  [[ "$needed" != */* ]] || fail 'path-qualified library dependency'
  while IFS= read -r version; do
    IFS=. read -r major minor patch <<< "${version#GLIBC_}"
    if ((major > 2 || (major == 2 && minor > 39) || (major == 2 && minor == 39 && ${patch:-0} > 0))); then
      fail "requires $version, newer than 2.39"
    fi
  done < <(grep -Eo 'GLIBC_[0-9]+\.[0-9]+(\.[0-9]+)?' <<< "$versions" || true)
  echo "PASS: $binary: standard loader, no RPATH, glibc <= 2.39"
done
