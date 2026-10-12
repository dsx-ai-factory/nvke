#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -e

# Keep old paths as aliases for one release, including rollback to an old deb.
# Refuse two independent state trees rather than overwrite either one.
migrate_directory() {
  local old="$1" new="$2"
  if [ -L "$old" ]; then
    [ "$(readlink "$old")" = "$new" ] || { echo "Unexpected alias: $old" >&2; exit 1; }
    return
  fi
  if [ -d "$old" ]; then
    if [ -e "$new" ]; then
      rmdir "$new" || { echo "Cannot merge existing state: $old and $new" >&2; exit 1; }
    fi
    mv "$old" "$new"
  fi
  mkdir -p "$new"
  ln -s "$new" "$old"
}

migrate_directory /var/lib/nke /var/lib/nvke
migrate_directory /usr/local/nke /usr/local/nvke
migrate_directory /etc/systemd/system/nke-agent.service.d /etc/systemd/system/nvke-agent.service.d

# The controller owns /etc/nke and chooses its configuration paths per node.
# Preserve the existing arguments and let old controllers keep writing the alias.
if [ -f /etc/default/nke-agent ] && [ -f /etc/default/nvke-agent ] && ! [ /etc/default/nke-agent -ef /etc/default/nvke-agent ]; then
  echo "Both agent env files exist independently; choose the active configuration before installing" >&2
  exit 1
fi
if [ ! -e /etc/default/nvke-agent ]; then
  if [ -f /etc/default/nke-agent ]; then
    mv /etc/default/nke-agent /etc/default/nvke-agent
  else
    echo 'AGENT_ARGS=""' >/etc/default/nvke-agent
  fi
fi
if [ ! -e /etc/default/nke-agent ]; then
  ln -s /etc/default/nvke-agent /etc/default/nke-agent
fi
