#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# Extend logical volume
# Capture output and exit code
set +e
LVEXTEND_OUTPUT=$(lvextend -l +100%FREE /dev/ubuntu-vg/ubuntu-lv 2>&1)
LVEXTEND_EXIT=$?
set -e

if [ $LVEXTEND_EXIT -eq 0 ]; then
    echo "Logical volume extended successfully"
elif echo "$LVEXTEND_OUTPUT" | grep -q "matches existing size"; then
    # This is the specific "no-op" case - already at maximum size
    echo "Logical volume already at maximum size"
else
    # Real error - log and fail
    echo "ERROR: lvextend failed with exit code $LVEXTEND_EXIT"
    echo "$LVEXTEND_OUTPUT"
    exit $LVEXTEND_EXIT
fi

# Resize filesystem
# Capture output and exit code
set +e
RESIZE2FS_OUTPUT=$(resize2fs /dev/ubuntu-vg/ubuntu-lv 2>&1)
RESIZE2FS_EXIT=$?
set -e

if [ $RESIZE2FS_EXIT -eq 0 ]; then
    echo "Filesystem resized successfully"
elif echo "$RESIZE2FS_OUTPUT" | grep -q "Nothing to do\|already.*blocks long"; then
    # This is the specific "no-op" case - filesystem already at correct size
    echo "Filesystem already at correct size"
else
    # Real error - log and fail
    echo "ERROR: resize2fs failed with exit code $RESIZE2FS_EXIT"
    echo "$RESIZE2FS_OUTPUT"
    exit $RESIZE2FS_EXIT
fi

# lvdisplay
# pvdisplay
# vgdisplay
