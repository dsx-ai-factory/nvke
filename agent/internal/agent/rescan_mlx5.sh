#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


#
# Mellanox DPU PCI Rescan Script
# Rescans DPU devides that are not fully initialized during boot
# Version: 1.0
#

set -euo pipefail

# Configuration
readonly SCRIPT_NAME="$(basename "$0")"
readonly CONFIG_FILE="/etc/mlx5-rescan.conf"
readonly LOCK_FILE="/var/run/${SCRIPT_NAME}.lock"
readonly LOG_FILE="/var/log/mlx5-rescan.log"

# Default configuration
BLUEFIELD_ADDRESSES=()
NETWORK_CHECK_TIMEOUT=300     # 300 seconds to wait for network interfaces
LOG_LEVEL="INFO"              # DEBUG, INFO, WARN, ERROR
REQUIRED_NETWORK_INTERFACES=1 # Minimum non-loopback interfaces expected
RUN_DHCP_CLIENT=true          # Run dhclient to configure network interfaces after reset

# Load optional configuration
load_config() {
  if [[ -f "$CONFIG_FILE" ]]; then
    log_debug "Loading optional configuration from: $CONFIG_FILE"
    # shellcheck source=/dev/null
    source "$CONFIG_FILE"
    log_debug "Configuration loaded successfully"
  else
    log_debug "No configuration file found, using defaults"
  fi
}

# Logging functions
log() {
  local level="$1"
  shift
  local message="$*"
  local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
  echo "[$timestamp] [$level] $message" | tee -a "$LOG_FILE"
}

log_debug() { [[ "$LOG_LEVEL" == "DEBUG" ]] && log "DEBUG" "$@" || true; }
log_info() { log "INFO" "$@"; }
log_warn() { log "WARN" "$@"; }
log_error() { log "ERROR" "$@"; }

# Error handling
error_exit() {
  log_error "$1"
  cleanup
  exit "${2:-1}"
}

# Cleanup function
cleanup() {
  log_debug "Performing cleanup..."
  if [[ -f "$LOCK_FILE" ]]; then
    rm -f "$LOCK_FILE" || log_warn "Failed to remove lock file: $LOCK_FILE"
  fi
}

# Signal handlers
trap 'error_exit "Script interrupted by signal" 130' INT TERM
trap 'cleanup' EXIT

# Check if running as root
check_root() {
  if [[ $EUID -ne 0 ]]; then
    error_exit "This script must be run as root" 2
  fi

  log_debug "Running as root (UID=$EUID, USER=${USER:-unset})"
}

# Test /sys filesystem access
test_sys_access() {
  log_debug "Testing /sys filesystem write access..."

  # Test basic /sys write access
  local test_file="/sys/bus/pci/rescan"
  if [[ ! -f "$test_file" ]]; then
    error_exit "/sys/bus/pci/rescan not found - PCI subsystem not available" 10
  fi

  # Test if we can write to the file (without actually writing)
  if [[ ! -w "$test_file" ]]; then
    log_error "/sys filesystem appears to be read-only or permission denied"
    log_error "Current user: ${USER:-unknown}, UID: $EUID"
    log_error "File permissions: $(ls -l "$test_file" 2>/dev/null || echo 'cannot read')"
    log_error "Mount info: $(mount | grep '/sys' | head -1 || echo 'no /sys mount found')"
    error_exit "Cannot write to /sys filesystem - check permissions" 11
  fi

  log_debug "/sys filesystem write access confirmed"
}

# Create lock file to prevent multiple instances
create_lock() {
  if [[ -f "$LOCK_FILE" ]]; then
    local lock_pid
    lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "unknown")
    if kill -0 "$lock_pid" 2>/dev/null; then
      error_exit "Another instance is already running (PID: $lock_pid)" 3
    else
      log_warn "Stale lock file found, removing it"
      rm -f "$LOCK_FILE"
    fi
  fi

  echo $$ >"$LOCK_FILE" || error_exit "Failed to create lock file: $LOCK_FILE" 4
  log_debug "Lock file created: $LOCK_FILE"
}

# Check for network interfaces (excluding loopback)
check_network_interfaces() {
  local interface_count
  interface_count=$(ip link show | grep -E '^[0-9]+:' | grep -v 'lo:' | wc -l)
  log_debug "Found $interface_count non-loopback network interfaces"
  echo "$interface_count"
}

# Discover Bluefield devices dynamically
discover_bluefield_devices() {
  log_info "Scanning for Bluefield devices..."

  # Use mapfile to properly handle the array
  local devices_raw
  devices_raw=$(lspci | grep -i bluefield | awk '{ print $1 }' || true)

  BLUEFIELD_ADDRESSES=()
  if [[ -n "$devices_raw" ]]; then
    while IFS= read -r device; do
      if [[ -n "$device" ]]; then
        # Convert to full PCI address format (0000:xx:xx.x)
        if [[ "$device" =~ ^[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$ ]]; then
          device="0000:$device"
        fi
        BLUEFIELD_ADDRESSES+=("$device")
      fi
    done <<<"$devices_raw"
  fi

  if [[ ${#BLUEFIELD_ADDRESSES[@]} -eq 0 ]]; then
    log_info "No Bluefield devices found in system"
    return 1
  fi

  log_info "Found ${#BLUEFIELD_ADDRESSES[@]} Bluefield device(s): ${BLUEFIELD_ADDRESSES[*]}"
  return 0
}

# Check if there are any valid network interfaces
# In failure conditions only the loopback interface is present
network_interfaces_sufficient() {
  local current_interfaces
  current_interfaces=$(check_network_interfaces)

  if [[ "$current_interfaces" -ge "$REQUIRED_NETWORK_INTERFACES" ]]; then
    log_info "Sufficient network interfaces found ($current_interfaces >= $REQUIRED_NETWORK_INTERFACES)"
    return 0
  else
    log_warn "Only loopback interface found ($current_interfaces < $REQUIRED_NETWORK_INTERFACES)"
    return 1
  fi
}

# Check if PCI device exists
device_exists() {
  local pci_addr="$1"
  [[ -d "/sys/bus/pci/devices/$pci_addr" ]]
}

# Validate Bluefield device
validate_bluefield_device() {
  local pci_addr="$1"
  local vendor_file="/sys/bus/pci/devices/$pci_addr/vendor"
  local device_file="/sys/bus/pci/devices/$pci_addr/device"

  if [[ ! -f "$vendor_file" ]] || [[ ! -f "$device_file" ]]; then
    return 1
  fi

  local vendor
  local device
  vendor=$(cat "$vendor_file" 2>/dev/null)
  device=$(cat "$device_file" 2>/dev/null)

  # Mellanox vendor ID is 0x15b3
  if [[ "$vendor" != "0x15b3" ]]; then
    log_error "Device $pci_addr is not a Mellanox device (vendor: $vendor)"
    return 1
  fi

  log_debug "Validated Bluefield device: $pci_addr (vendor: $vendor, device: $device)"
  return 0
}

# Remove PCI device
remove_device() {
  local pci_addr="$1"
  local remove_file="/sys/bus/pci/devices/$pci_addr/remove"

  if [[ ! -f "$remove_file" ]]; then
    log_error "Remove interface not found for device: $pci_addr"
    return 1
  fi

  log_info "Removing PCI device: $pci_addr"
  echo 1 >"$remove_file" || {
    log_error "Failed to remove device: $pci_addr"
    return 1
  }

  log_info "Device removed successfully: $pci_addr"
  return 0
}

# Wait for network interfaces to appear
wait_for_network_interfaces() {
  local elapsed=0

  log_info "Waiting for network interfaces to appear (timeout: ${NETWORK_CHECK_TIMEOUT}s)"

  while [[ $elapsed -lt $NETWORK_CHECK_TIMEOUT ]]; do
    if network_interfaces_sufficient; then
      log_info "Network interfaces are now available (waited ${elapsed}s)"
      return 0
    fi

    sleep 1
    elapsed=$((elapsed + 1))

    if ((elapsed % 5 == 0)); then
      log_debug "Still waiting for network interfaces... (${elapsed}s elapsed)"
    fi
  done

  log_warn "Timeout waiting for network interfaces to appear after ${NETWORK_CHECK_TIMEOUT}s"
  return 1
}

# Rescan PCI bus
rescan_pci_bus() {
  local rescan_file="/sys/bus/pci/rescan"

  if [[ ! -f "$rescan_file" ]]; then
    log_error "PCI rescan interface not found: $rescan_file"
    return 1
  fi

  log_info "Rescanning PCI bus..."
  echo 1 >"$rescan_file" || {
    log_error "Failed to rescan PCI bus"
    return 1
  }

  log_info "PCI bus rescan completed"
  return 0
}

# Run DHCP client to configure network interfaces
run_dhcp_client() {
  log_info "Running DHCP client to configure network interfaces..."

  if dhclient; then
    log_info "DHCP client completed successfully"
    return 0
  else
    log_warn "DHCP client failed"
    return 1
  fi
}

# Main function
main() {
  log_info "Starting $SCRIPT_NAME (PID: $$) - Bluefield Reset"

  # Initialize
  check_root
  create_lock
  load_config
  test_sys_access

  # Check current network interface status
  log_info "Checking current network interface status..."
  if network_interfaces_sufficient; then
    log_info "Network interfaces are already available - no reset needed"
    log_info "$SCRIPT_NAME completed successfully (no action required)"
    exit 0  # Success: no action needed
  fi

  # Network interfaces are insufficient, need to reset Bluefield devices
  log_warn "Network interfaces insufficient, attempting Bluefield device reset..."

  # Discover Bluefield devices dynamically
  if ! discover_bluefield_devices; then
    error_exit "No Bluefield devices found to reset" 5
  fi

  local failed_devices=()
  local success_count=0

  # Process each discovered Bluefield device
  for pci_addr in "${BLUEFIELD_ADDRESSES[@]}"; do
    log_info "Processing Bluefield device: $pci_addr"

    # Check if device exists
    if ! device_exists "$pci_addr"; then
      log_warn "Device not found, skipping: $pci_addr"
      continue
    fi

    # Validate it's a Bluefield device
    if ! validate_bluefield_device "$pci_addr"; then
      log_error "Device validation failed: $pci_addr"
      failed_devices+=("$pci_addr")
      continue
    fi

    # Remove the device
    if ! remove_device "$pci_addr"; then
      failed_devices+=("$pci_addr")
      continue
    fi

    success_count=$((success_count + 1))
    log_info "Successfully removed device: $pci_addr"
  done

  if [[ $success_count -eq 0 ]]; then
    error_exit "Failed to remove any Bluefield devices" 6
  fi

  # Rescan PCI bus after all devices are removed
  log_info "All devices removed, rescanning PCI bus..."
  if ! rescan_pci_bus; then
    error_exit "Failed to rescan PCI bus" 7
  fi

  # Wait for network interfaces to appear
  log_info "Waiting for network interfaces to initialize..."
  if wait_for_network_interfaces; then
    log_info "Network interfaces are now available after reset"

    # Run DHCP client to configure the network interfaces
    if [[ "$RUN_DHCP_CLIENT" == "true" ]]; then
      if run_dhcp_client; then
        log_info "Network configuration completed successfully"
      else
        log_warn "DHCP configuration failed, but interfaces are available"
      fi
    else
      log_debug "DHCP client execution skipped (RUN_DHCP_CLIENT=false)"
    fi

    log_info "$SCRIPT_NAME completed successfully"
  else
    log_error "Network interfaces did not appear within timeout period"
    log_error "Manual investigation may be required"
    error_exit "Network interfaces still not available after reset" 8
  fi

  # Final status report
  log_info "Reset completed: $success_count devices processed, ${#failed_devices[@]} failed"
  if [[ ${#failed_devices[@]} -gt 0 ]]; then
    log_warn "Failed devices: ${failed_devices[*]}"
  fi
  
  exit 60  # Success: reset was performed
}

# Usage information
usage() {
  cat <<EOF
Usage: $SCRIPT_NAME [OPTIONS]

Bluefield DPU Reset Script

This script automatically detects when network interfaces are missing and 
dynamically discovers and resets Bluefield DPU devices to restore connectivity.

BEHAVIOR:
    1. Checks for non-loopback network interfaces
    2. If sufficient interfaces exist, exits successfully
    3. If not, discovers Bluefield devices using lspci
    4. Removes all discovered Bluefield devices
    5. Rescans PCI bus
    6. Waits up to 300 seconds for network interfaces to appear
    7. Runs DHCP client to configure network interfaces (if enabled)

OPTIONS:
    -h, --help      Show this help message
    -v, --version   Show version information
    -d, --debug     Enable debug logging
    -t, --timeout   Set network interface timeout (default: 120s)
    -r, --required  Set required interface count (default: 1)
    --no-dhcp       Skip running DHCP client after reset

EXIT CODES:
    0               Success - network interfaces already available (no reset needed)
    60              Success - reset was performed
    1-59, 61+       Error occurred during execution

FILES:
    Log file: $LOG_FILE
    Lock file: $LOCK_FILE

EXAMPLES:
    $SCRIPT_NAME                    # Run automatic detection and reset with DHCP
    $SCRIPT_NAME --debug           # Run with debug logging  
    $SCRIPT_NAME -t 30 -r 2        # Wait 30s, require 2 interfaces
    $SCRIPT_NAME --no-dhcp         # Reset without running DHCP client

EOF
}

# Command line argument parsing
while [[ $# -gt 0 ]]; do
  case $1 in
  -h | --help)
    usage
    exit 0
    ;;
  -v | --version)
    echo "$SCRIPT_NAME version 1.0 - Bluefield Reset"
    exit 0
    ;;
  -d | --debug)
    LOG_LEVEL="DEBUG"
    shift
    ;;
  -t | --timeout)
    if [[ -n "${2:-}" ]] && [[ "$2" =~ ^[0-9]+$ ]]; then
      NETWORK_CHECK_TIMEOUT="$2"
      shift 2
    else
      log_error "Invalid timeout value: ${2:-}"
      exit 1
    fi
    ;;
  -r | --required)
    if [[ -n "${2:-}" ]] && [[ "$2" =~ ^[0-9]+$ ]]; then
      REQUIRED_NETWORK_INTERFACES="$2"
      shift 2
    else
      log_error "Invalid required interface count: ${2:-}"
      exit 1
    fi
    ;;
  --no-dhcp)
    RUN_DHCP_CLIENT=false
    shift
    ;;
  *)
    log_error "Unknown option: $1"
    usage
    exit 1
    ;;
  esac
done

# Run main function
main "$@"
