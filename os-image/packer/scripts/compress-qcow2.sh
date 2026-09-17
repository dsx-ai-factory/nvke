#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Compresses the qcow2 Packer just produced with a manual `qemu-img convert
# -c`, in place. This exists instead of the qemu builder's own
# disk_compression option: as of packer-plugin-qemu v1.1.4, disk_compression
# = true breaks the builder's own "Converting hard drive..." step (it tries
# to open the destination file without creating it first, so the whole build
# fails with "No such file or directory") -- confirmed by reproducing it
# against a real build. This runs the same `qemu-img convert -c` the builder
# would have used, just after the fact and outside the broken codepath.
set -euo pipefail

QCOW2="$1"
TMP="${QCOW2%.qcow2}.compressed.qcow2"
PYTHON="${PYTHON:-python3}"

cleanup() {
  rm -f -- "${TMP}"
}
trap cleanup EXIT

file_size() {
  stat -c %s "$1" 2>/dev/null || stat -f %z "$1"
}

echo "Compressing ${QCOW2}..."
BEFORE=$(file_size "${QCOW2}")
qemu-img convert -c -O qcow2 "${QCOW2}" "${TMP}"

# Validate the converted file before it replaces Packer's output. Parsing the
# authoritative qemu-img JSON avoids accepting a backing file, container-level
# encryption, or a non-QCOW2 result because a human-readable line happened to
# contain an expected word.
qemu-img info --output=json "${TMP}" | "${PYTHON}" -c '
import json
import sys


def fail(message):
    print(f"compress-qcow2: {message}", file=sys.stderr)
    raise SystemExit(1)


try:
    info = json.load(sys.stdin)
except (UnicodeError, json.JSONDecodeError) as exc:
    fail(f"qemu-img returned invalid JSON: {exc}")

if not isinstance(info, dict):
    fail("qemu-img metadata is not a JSON object")
actual_format = info.get("format")
if actual_format != "qcow2":
    fail(f"qemu-img reported format {actual_format!r}, expected qcow2")
virtual_size = info.get("virtual-size")
if isinstance(virtual_size, bool) or not isinstance(virtual_size, int) or virtual_size <= 0:
    fail("qemu-img virtual-size must be a positive integer")
if "encrypted" in info and info["encrypted"] is not False:
    fail("QCOW2 container encryption is unsupported")
for field in ("backing-filename", "full-backing-filename"):
    if field in info and info[field] != "":
        fail(f"QCOW2 must be self-contained; qemu-img reported {field}")
'
mv "${TMP}" "${QCOW2}"
trap - EXIT
AFTER=$(file_size "${QCOW2}")

echo "Before: $((BEFORE / 1024 / 1024)) MiB"
echo "After:  $((AFTER / 1024 / 1024)) MiB"
