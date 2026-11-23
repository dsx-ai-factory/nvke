#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

export LC_ALL=C
marker=/var/lib/dsxos/.cidata-consumed

if [ -L "$marker" ]; then
    echo 'DSX CIDATA completion marker is not a regular file' >&2
    exit 1
fi
if detail=$(stat -L -- "$marker" 2>&1 >/dev/null); then
    if [ ! -f "$marker" ]; then
        echo 'DSX CIDATA completion marker is not a regular file' >&2
        exit 1
    fi
    echo ready
    exit 0
fi
case "$detail" in
    *': No such file or directory') ;;
    *) printf '%s\n' "$detail" >&2; exit 1 ;;
esac

report=$(cloud-init status --long 2>&1)
result=$?
if [ "$result" -ne 0 ] && [ "$result" -ne 2 ]; then
    printf 'cloud-init exited %s: %s\n' "$result" "$report" >&2
    exit 1
fi
state=$(printf '%s\n' "$report" | sed -n 's/^[[:space:]]*[Ss][Tt][Aa][Tt][Uu][Ss]:[[:space:]]*//p' | head -n 1 | tr '[:upper:]' '[:lower:]')
state="${state%"${state##*[![:space:]]}"}"
case "$state" in
    done) echo ready ;;
    error) printf 'cloud-init error status: %s\n' "$report" >&2; exit 1 ;;
    *) echo notready ;;
esac
