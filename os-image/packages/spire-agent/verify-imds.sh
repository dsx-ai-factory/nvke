#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Verify a local IMDS executable without executing code for the target machine.
set -euo pipefail
die() { echo "IMDS agent: $*" >&2; exit 1; }
[[ $# == 4 ]] || die "expected FILE SHA256 INPUT_ARCH TARGET_ARCH"
file=$1 expected=$2 input_arch=$3 target_arch=$4
[[ "$input_arch" == "$target_arch" ]] || die "input architecture $input_arch does not match target $target_arch"
case "$target_arch" in
  amd64) machine=3e00 ;;
  arm64) machine=b700 ;;
  *) die "unsupported architecture $target_arch" ;;
esac
[[ -f "$file" && ! -L "$file" ]] || die "missing regular input file: $file"
[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "expected SHA-256 must be 64 lowercase hexadecimal characters"
actual=$(sha256sum "$file" | cut -d' ' -f1)
[[ "$actual" == "$expected" ]] || die "SHA-256 mismatch for $file: expected $expected, got $actual"
# ELF64, little endian, current ELF version; e_type EXEC or DYN (PIE), and
# e_machine x86-64 or AArch64. Do not trust the caller's architecture label.
header=$(od -An -v -tx1 -N64 "$file" | tr -d ' \n')
[[ ${#header} == 128 && "${header:0:14}" == 7f454c46020101 &&
   ( "${header:14:2}" == 00 || "${header:14:2}" == 03 ) &&
   ( "${header:32:4}" == 0200 || "${header:32:4}" == 0300 ) &&
   "${header:36:4}" == "$machine" ]] || die "expected a Linux $target_arch ELF64 executable: $file"
