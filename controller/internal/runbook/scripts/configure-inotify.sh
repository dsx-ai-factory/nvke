#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


MAX_USER_WATCHES="${MAX_USER_WATCHES:-1048576}"
MAX_USER_INSTANCES="${MAX_USER_INSTANCES:-1024}"
MAX_QUEUED_EVENTS="${MAX_QUEUED_EVENTS:-65536}"
SYSCTL_CONF="${SYSCTL_CONF:-/etc/sysctl.d/99-inotify.conf}"

configure_inotify() {
  tee $SYSCTL_CONF > /dev/null <<EOF
fs.inotify.max_user_watches=$MAX_USER_WATCHES
fs.inotify.max_user_instances=$MAX_USER_INSTANCES
fs.inotify.max_queued_events=$MAX_QUEUED_EVENTS
EOF

  sysctl --system
  echo "Inotify configuration completed"
}


if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  configure_inotify
fi
