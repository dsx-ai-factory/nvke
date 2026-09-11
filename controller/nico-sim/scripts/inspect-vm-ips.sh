#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# inspect-vm-ips: compare nico-vms DB IP vs actual IP for running VMs
# Detects IP drift caused by OVS bridge MAC changes
LEASE_FILE="${NICO_VMS_DHCP_LEASE_FILE:-/var/run/nico-vms-dnsmasq.leases}"
HOSTS_DIR="${NICO_VMS_DHCP_HOSTS_DIR:-/var/lib/nico-vms/dhcp-hosts}"
DB_FILE="${NICO_SIM_DB_PATH:-/workspace/dev/nke/nke-dev/.dev/nico-sim.db}"

if [ ! -f "$DB_FILE" ]; then
  echo "nico-vms DB not found at $DB_FILE"
  exit 1
fi

rows=()
for vm in $(virsh list --name 2>/dev/null); do
  instance_id="${vm#nico-vm-}"
  orig_mac=$(virsh domiflist "$vm" 2>/dev/null | awk '/bridge/{print $5}')
  tap=$(virsh domiflist "$vm" 2>/dev/null | awk '/bridge/{print $1}')

  # Get expected IP from nico-vms DB
  db_row=$(sqlite3 "$DB_FILE" "
    SELECT i.name, ii.ip_addresses_json
    FROM instances i
    LEFT JOIN instance_interfaces ii ON ii.instance_id = i.id
    WHERE i.id = '$instance_id';
  " 2>/dev/null)
  db_name=$(echo "$db_row" | cut -d'|' -f1)
  db_ip=$(echo "$db_row" | cut -d'|' -f2 | tr -d '[]"' | cut -d',' -f1)
  [ -z "$db_ip" ] && db_ip="(none)"

  # Get host entry
  host_entry=""
  [ -f "$HOSTS_DIR/$instance_id" ] && host_entry=$(cat "$HOSTS_DIR/$instance_id")

  # Get actual IP from ARP by original MAC
  arp_ip=$(arp -an 2>/dev/null | awk -v mac="$orig_mac" '$4==mac{gsub(/[()]/,"",$2); print $2}')

  # Check for a different MAC on this tap (OVS bridge)
  other_macs=$(bridge fdb show dev "$tap" 2>/dev/null | awk '{print $1}' | grep -v "33:33\|01:00\|ff:ff" | grep -vi "${orig_mac:-xxx}" | sort -u | head -1)
  other_ip=""
  [ -n "$other_macs" ] && other_ip=$(arp -an 2>/dev/null | awk -v mac="$other_macs" '$4==mac{gsub(/[()]/,"",$2); print $2}')

  # Determine actual IP and result
  actual_ip="${arp_ip:-${other_ip:-(unreachable)}}"
  result="OK"
  if [ -n "$arp_ip" ] && [ "$arp_ip" = "$db_ip" ]; then
    result="OK"
  elif [ -z "$arp_ip" ] && [ -n "$other_ip" ]; then
    actual_ip="$other_ip (MAC: $other_macs)"
    if [ "$other_ip" = "$db_ip" ]; then
      result="MAC_CHANGED"
    else
      result="DRIFTED"
    fi
  elif [ -z "$arp_ip" ] && [ -z "$other_ip" ]; then
    result="DOWN"
  elif [ -n "$arp_ip" ] && [ "$arp_ip" != "$db_ip" ]; then
    result="DRIFTED"
  fi

  rows+=("${db_name:-(unknown)}|${tap}|${orig_mac}|${db_ip}|${actual_ip}|${result}|${host_entry:-(none)}")
done

if [ ${#rows[@]} -eq 0 ]; then
  echo "No running NICo VMs found."
  exit 0
fi

# Compute column widths
headers="NAME|TAP|ORIG_MAC|DB_IP|ACTUAL_IP|RESULT|HOST_ENTRY"
all_lines=("$headers" "${rows[@]}")

for col in 0 1 2 3 4 5 6; do
  max=0
  for line in "${all_lines[@]}"; do
    val=$(echo "$line" | cut -d'|' -f$((col+1)))
    len=${#val}
    (( len > max )) && max=$len
  done
  widths[$col]=$max
done

fmt="%-${widths[0]}s  %-${widths[1]}s  %-${widths[2]}s  %-${widths[3]}s  %-${widths[4]}s  %-${widths[5]}s  %s\n"

printf "$fmt" "NAME" "TAP" "ORIG_MAC" "DB_IP" "ACTUAL_IP" "RESULT" "HOST_ENTRY"
total=0; for w in "${widths[@]}"; do total=$((total + w)); done
printf "%0.s-" $(seq 1 $((total + 12))); echo

for row in "${rows[@]}"; do
  IFS='|' read -r c0 c1 c2 c3 c4 c5 c6 <<< "$row"
  printf "$fmt" "$c0" "$c1" "$c2" "$c3" "$c4" "$c5" "$c6"
done
