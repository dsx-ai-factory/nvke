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
  local binary_path="/usr/local/bin/nke-agent"
  local backup_path="/usr/local/bin/nke-agent.bak"

  if [ -f "$binary_path" ]; then
    echo "stopping the currently running agent service..."
    systemctl stop nke-agent

    echo "Backing up the current agent binary..."
    mv "$binary_path" "$backup_path"
  else
    echo "No agent binary found in /usr/bin, checking for legacy install..."
    archive_legacy_agent
  fi

  echo "Installing the agent..."
  cp ./nke-agent "$binary_path"
  chmod +x $binary_path
}

install_updater() {
  if [ ! -x ./nke-agent-updater ]; then
    echo "nke-agent-updater is missing from the agent bundle" >&2
    exit 1
  fi
  if [ ! -f ./nke-agent-updater@.service ]; then
    echo "nke-agent-updater@.service is missing from the agent bundle" >&2
    exit 1
  fi

  install -m 0755 ./nke-agent-updater /usr/local/bin/nke-agent-updater
  install -m 0644 ./nke-agent-updater@.service /etc/systemd/system/nke-agent-updater@.service
}

create_agent_service_file() {
  # Use an env file for arguments rather than editing the systemd service file
  echo "AGENT_ARGS=\"$AGENT_ARGS\"" >/etc/default/nke-agent

  # Now the actual systemd file
  local service_file="/etc/systemd/system/nke-agent.service"
  if [ -f "$service_file" ]; then
    systemctl stop nke-agent
    rm $service_file
  fi

  echo "Creating systemd service file..."

  cat <<'EOL' >"$service_file"
[Unit]
Description=NVKE Node Agent
After=multi-user.target
StartLimitIntervalSec=0

[Service]
EnvironmentFile=/etc/default/nke-agent
ExecStart=/usr/local/bin/nke-agent $AGENT_ARGS 
ExecReload=/bin/kill -HUP $MAINPID
Type=simple
Restart=always
RestartSec=5s

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOL
  echo "Systemd agent service file created at $service_file"
}

install_script_logrotate() {
  local logrotate_dest="/etc/logrotate.d/nke-execute-scripts"
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local logrotate_src=""
  if [ -f "${script_dir}/nke-execute-scripts.logrotate" ]; then
    logrotate_src="${script_dir}/nke-execute-scripts.logrotate"
  elif [ -f "./nke-execute-scripts.logrotate" ]; then
    logrotate_src="./nke-execute-scripts.logrotate"
  fi

  if [ -z "$logrotate_src" ]; then
    echo "Warning: nke-execute-scripts.logrotate not found (checked script dir and cwd), skipping logrotate config."
    return 0
  fi

  echo "Installing logrotate config for nke-execute-scripts.log..."
  cp "$logrotate_src" "$logrotate_dest"
  chmod 644 "$logrotate_dest"
  echo "Logrotate config installed at $logrotate_dest"
}

# Function to enable and start the agent service
enable_and_start_agent() {
  echo "Enabling and starting the agent service..."
  systemctl enable nke-agent
  systemctl start nke-agent

  if systemctl is-active --quiet nke-agent; then
    echo "nke-agent is running."
  else
    echo "nke-agent is not running."
    exit 1
  fi
}

# Function to check if the upgrade was successful
check_upgrade_success() {
  if diff <(/usr/local/bin/nke-agent -version) <(./nke-agent -version) >/dev/null; then
    echo "Agent upgrade successful."
  else
    echo "Agent upgrade failed. Restoring backup..."
    cp /usr/local/bin/nke-agent.bak /usr/local/bin/nke-agent
    systemctl start nke-agent
    echo "Backup restored. Please check the logs for more details."
    exit 1
  fi
}

archive_legacy_agent() {
  if [ -f /usr/local/nke/.nke-state.json ]; then
    echo "legacy agent state file found, relocating to /var/lib/nke/state.json"
    cp /usr/local/nke/.nke-state.json /var/lib/nke/state.json
  fi

  if [ -d /usr/local/nke ]; then
    echo "legacy agent install (/usr/local/nke) found, archiving to /usr/local/nke-bak.tgz"
    tar czf /usr/local/nke-bak.tgz /usr/local/nke
    if rm /usr/local/nke/.nke-state.json; then
      echo "Removed legacy state file"
    else
      echo "Warning: Failed to remove legacy state file"
    fi

    if [ -f /var/lib/nke/state.json ] && ln -s /var/lib/nke/state.json /usr/local/nke/.nke-state.json; then
      echo "Created symlink from legacy state path to new location"
    else
      echo "Warning: Failed to create state symlink - legacy applications may not function properly"
    fi
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

install_updater
install_agent_binary
create_agent_service_file
install_script_logrotate

systemctl daemon-reload

enable_and_start_agent
echo "Agent upgrade process completed."
echo "Please check the logs for any errors or warnings."
echo "You can view the logs using journalctl -u nke-agent."
