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

echo "Compressing ${QCOW2}..."
BEFORE=$(stat -c %s "${QCOW2}")
qemu-img convert -c -O qcow2 "${QCOW2}" "${TMP}"
mv "${TMP}" "${QCOW2}"
AFTER=$(stat -c %s "${QCOW2}")

echo "Before: $((BEFORE / 1024 / 1024)) MiB"
echo "After:  $((AFTER / 1024 / 1024)) MiB"
