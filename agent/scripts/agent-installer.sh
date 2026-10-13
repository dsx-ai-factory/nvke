#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# This script is used to upgrade the agent on a system.
# It is designed to be run as a root user or with sudo privileges.

# Check if the script is run as root
if [ "$(id -u)" -ne 0 ]; then
  echo "This script must be run as root or with sudo privileges."
  exit 1
fi

AGENT_ARGS=""

# swap the binary
install_agent_binary() {
  local binary_path="/usr/local/bin/nvke-agent"
  local backup_path="/usr/local/bin/nvke-agent.bak"

  if [ -f "$binary_path" ]; then
    echo "stopping the currently running agent service..."
    systemctl stop nvke-agent

    echo "Backing up the current agent binary..."
    mv "$binary_path" "$backup_path"
  else
    echo "No agent binary found in /usr/bin, checking for legacy install..."
    archive_legacy_agent
  fi

  echo "Installing the agent..."
  cp ./nvke-agent "$binary_path"
  chmod +x $binary_path
}

install_updater() {
  if [ ! -x ./nvke-agent-updater ]; then
    echo "nvke-agent-updater is missing from the agent bundle" >&2
    exit 1
  fi
  if [ ! -f ./nvke-agent-updater@.service ]; then
    echo "nvke-agent-updater@.service is missing from the agent bundle" >&2
    exit 1
  fi

  install -m 0755 ./nvke-agent-updater /usr/local/bin/nvke-agent-updater
  install -m 0644 ./nvke-agent-updater@.service /etc/systemd/system/nvke-agent-updater@.service
}

create_agent_service_file() {
  # Keep the canonical unit and its legacy alias identical to the Debian package.
  install -m 0644 ./nvke-agent.service /etc/systemd/system/nvke-agent.service
  if [ -n "$AGENT_ARGS" ]; then
    echo "AGENT_ARGS=\"$AGENT_ARGS\"" >/etc/default/nvke-agent
  fi
}

install_script_logrotate() {
  local logrotate_dest="/etc/logrotate.d/nvke-execute-scripts"
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local logrotate_src=""
  if [ -f "${script_dir}/nvke-execute-scripts.logrotate" ]; then
    logrotate_src="${script_dir}/nvke-execute-scripts.logrotate"
  elif [ -f "./nvke-execute-scripts.logrotate" ]; then
    logrotate_src="./nvke-execute-scripts.logrotate"
  fi

  if [ -z "$logrotate_src" ]; then
    echo "Warning: nvke-execute-scripts.logrotate not found (checked script dir and cwd), skipping logrotate config."
    return 0
  fi

  echo "Installing logrotate config for nvke-execute-scripts.log..."
  cp "$logrotate_src" "$logrotate_dest"
  chmod 644 "$logrotate_dest"
  echo "Logrotate config installed at $logrotate_dest"
}

# Function to enable and start the agent service
enable_and_start_agent() {
  echo "Enabling and starting the agent service..."
  systemctl enable --force nvke-agent
  systemctl start nvke-agent

  if systemctl is-active --quiet nvke-agent; then
    echo "nvke-agent is running."
  else
    echo "nvke-agent is not running."
    exit 1
  fi
}

# Function to check if the upgrade was successful
check_upgrade_success() {
  if diff <(/usr/local/bin/nvke-agent -version) <(./nvke-agent -version) >/dev/null; then
    echo "Agent upgrade successful."
  else
    echo "Agent upgrade failed. Restoring backup..."
    cp /usr/local/bin/nvke-agent.bak /usr/local/bin/nvke-agent
    systemctl start nvke-agent
    echo "Backup restored. Please check the logs for more details."
    exit 1
  fi
}

archive_legacy_agent() {
  if [ -f /usr/local/nke/.nke-state.json ] && [ ! -f /var/lib/nvke/state.json ]; then
    cp /usr/local/nke/.nke-state.json /var/lib/nvke/state.json
  fi
}

# Main script execution
while [[ $# -gt 0 ]]; do
  case "$1" in
  -u | --src-url)
    SRC_URL=$2
    AGENT_ARGS="$AGENT_ARGS --bundle-server-url $SRC_URL"
    shift 2
    ;;
  -d | --disable-mtls)
    # No need for a separate DISABLE_MTLS variable; the flag is passed via AGENT_ARGS.
    AGENT_ARGS="$AGENT_ARGS --disable-mtls"
    shift
    ;;
  *)
    echo "Warning: ignoring unknown option: $1" >&2
    shift
    ;;
  esac
done

# Stop both names before moving state or replacing the service unit.
systemctl stop nke-agent.service nvke-agent.service || true
bash ./migrate-agent-paths.sh
install_updater
install_agent_binary
create_agent_service_file
# The old regular unit would prevent creation of the canonical unit's alias.
rm -f /etc/systemd/system/nke-agent.service
ln -sf /usr/local/bin/nvke-agent /usr/local/bin/nke-agent
install_script_logrotate

systemctl daemon-reload

enable_and_start_agent
echo "Agent upgrade process completed."
echo "Please check the logs for any errors or warnings."
echo "You can view the logs using journalctl -u nvke-agent."
