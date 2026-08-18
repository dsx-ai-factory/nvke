#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Script to configure Open vSwitch bridge
# Handles netplan-managed and ifupdown-managed networking.

set -e

NETPLAN_DIR="${NETPLAN_DIR:-/etc/netplan}"
IFUPDOWN_DIR="${IFUPDOWN_DIR:-/etc/network/interfaces.d}"
# SYS_CLASS_NET_DIR is overridable for tests; production always reads /sys.
SYS_CLASS_NET_DIR="${SYS_CLASS_NET_DIR:-/sys/class/net}"
UPLINK_MTU="${UPLINK_MTU:-}"
# UPLINK_BRIDGE_NAME pins the OVS bridge name. CWE leaves this unset; the
# default br<kernel-iface> derivation below matches ovn-kubernetes's
# strip-br convention. Set this only to override the derivation.
UPLINK_BRIDGE_NAME="${UPLINK_BRIDGE_NAME:-}"
# UPLINK_INTERFACE explicitly names the kernel NIC to bridge (wins over
# UPLINK_INTERFACE_SELECTOR). When neither is set, the default-route NIC is used.
UPLINK_INTERFACE="${UPLINK_INTERFACE:-}"
# UPLINK_INTERFACE_SELECTOR picks the NIC by role. Allowed values:
# default-route-nic (Forge and single NIC instances) or first-non-default-nic
# (the first physical NIC that isn't the default-route NIC, in lexical order).
UPLINK_INTERFACE_SELECTOR="${UPLINK_INTERFACE_SELECTOR:-}"
# UPLINK_DHCP: "true" keeps DHCP on the bridge (default), "false" switches to
# static. When unset, the presence of UPLINK_ADDRESS implies static mode.
UPLINK_DHCP="${UPLINK_DHCP:-}"
# UPLINK_ADDRESS / UPLINK_PREFIX: static IPv4 to assign to the bridge.
UPLINK_ADDRESS="${UPLINK_ADDRESS:-}"
UPLINK_PREFIX="${UPLINK_PREFIX:-}"
# UPLINK_ROUTE_DESTINATION: optional extra static route CIDR for the bridge.
UPLINK_ROUTE_DESTINATION="${UPLINK_ROUTE_DESTINATION:-}"
# UPLINK_ROUTE_VIA_MODE: how to resolve the route next hop. Allowed values:
# "ip" (use UPLINK_ROUTE_VIA_IP verbatim) or "subnet-first-host" (derive
# <network>.1 from UPLINK_ADDRESS/UPLINK_PREFIX).
UPLINK_ROUTE_VIA_MODE="${UPLINK_ROUTE_VIA_MODE:-}"
UPLINK_ROUTE_VIA_IP="${UPLINK_ROUTE_VIA_IP:-}"

error_exit() {
  echo "Error: $1" >&2
  exit 1
}

generate_explicit_config() {
  local interface="$1"
  local netplan_file="$2"
  local mtu="$3"
  echo "Extracting network settings from running interface $interface..."
  
  local dns_servers=$(resolvectl status "$interface" 2>/dev/null | awk '/DNS Servers:/ {for(i=3;i<=NF;i++) print $i}')
  local search_domains=$(resolvectl status "$interface" 2>/dev/null | awk '/DNS Domain:/ {for(i=3;i<=NF;i++) print $i}')

  cat > "$netplan_file" << EOF  
network:
  version: 2
  ethernets:
    $interface:
      mtu: ${mtu}
      dhcp4: true
      dhcp-identifier: mac
EOF
  
  if [ -n "$dns_servers" ]; then
    echo "      nameservers:" >> "$netplan_file"
    echo "        addresses:" >> "$netplan_file"
    while IFS= read -r server; do
      echo "          - $server" >> "$netplan_file"
    done <<< "$dns_servers"
    
    if [ -n "$search_domains" ]; then
      echo "        search:" >> "$netplan_file"
      while IFS= read -r domain; do
        echo "          - $domain" >> "$netplan_file"
      done <<< "$search_domains"
    fi
  fi
}

check_ovs_installed() {
  if ! dpkg -l | grep -qw -e "^ii.*openvswitch-switch"; then
    echo "openvswitch-switch is not installed."
    exit 1
  fi
  systemctl enable --now openvswitch-switch
}

detect_default_interface() {
  DEFAULT_IF=$(ip route show default | awk '/default/ {print $5}' | head -n1)
  if [ -z "$DEFAULT_IF" ]; then
    error_exit "Could not determine the default network interface."
  fi
  echo "Default interface: $DEFAULT_IF"

  DEFAULT_MAC=$(ip link show "$DEFAULT_IF" | awk '/link\/ether/ {print $2}')
  echo "Default interface MAC: $DEFAULT_MAC"
}

detect_first_non_default_nic() {
  local default_route_if="$1"
  local candidates=()
  while read -r iface; do
    [ -z "$iface" ] && continue
    [ "$iface" = "lo" ] && continue
    [ "$iface" = "$default_route_if" ] && continue
    # Require a /device entry so virtual interfaces (docker0, veth, br-*) are
    # filtered out — only real physical NICs are eligible.
    [ -e "${SYS_CLASS_NET_DIR}/${iface}/device" ] || continue
    candidates+=("$iface")
  done < <(ip -o link show | awk -F': ' '{print $2}' | cut -d'@' -f1)
  if [ ${#candidates[@]} -eq 0 ]; then
    return 1
  fi
  # Lexical sort gives a stable pick across hardware where kernel ifindex
  # order may shift after PCI probe-order changes. See NKX deferred-decision
  # note in .local/agents/ovn-multi-nic.md.
  printf '%s\n' "${candidates[@]}" | LC_ALL=C sort | head -n1
}

resolve_uplink_interface() {
  local uplink_if=""

  if [ -n "${UPLINK_INTERFACE:-}" ]; then
    uplink_if="$UPLINK_INTERFACE"
  else
    case "${UPLINK_INTERFACE_SELECTOR:-}" in
      ""|default-route-nic)
        # Stay on the default-route NIC; DEFAULT_IF already holds it.
        return 0
        ;;
      first-non-default-nic)
        uplink_if=$(detect_first_non_default_nic "$DEFAULT_IF") || \
          error_exit "Could not determine the first non-default NIC"
        ;;
      *)
        error_exit "Unsupported UPLINK_INTERFACE_SELECTOR: ${UPLINK_INTERFACE_SELECTOR}"
        ;;
    esac
  fi

  if [ "$uplink_if" = "$DEFAULT_IF" ]; then
    return 0
  fi

  DEFAULT_IF="$uplink_if"
  DEFAULT_MAC=$(ip link show "$DEFAULT_IF" | awk '/link\/ether/ {print $2}')
  echo "Selected uplink interface: $DEFAULT_IF (MAC: ${DEFAULT_MAC:-unknown})"
}

detect_mtu() {
  if [ -n "$UPLINK_MTU" ]; then
    echo "Using MTU from environment: $UPLINK_MTU"
  else
    DETECTED_MTU=$(ip link show "$DEFAULT_IF" 2>/dev/null | awk '/mtu/ {print $5}')
    if [ -n "$DETECTED_MTU" ]; then
      UPLINK_MTU="$DETECTED_MTU"
      echo "Detected MTU from $DEFAULT_IF: $UPLINK_MTU"
    else
      UPLINK_MTU=1500
      echo "Using default MTU: $UPLINK_MTU"
    fi
  fi
}

check_already_configured() {
  # $1 = resolved target bridge name. Short-circuit if either:
  #   - the target bridge already exists (multi-NIC reruns, where DEFAULT_IF
  #     is the kernel uplink not a bridge), or
  #   - DEFAULT_IF is itself an existing bridge (single-NIC reruns after a
  #     reboot, where the default route now lives on the bridge we created
  #     in a prior run — otherwise resolve_bridge_name would compose
  #     br<bridge> and we'd build a double-prefixed bridge on top).
  local target_bridge_name="$1"
  local bridges
  bridges=$(ovs-vsctl list-br 2>/dev/null || true)
  if echo "$bridges" | grep -qx "$target_bridge_name"; then
    echo "Open vSwitch bridge $target_bridge_name has already been configured."
    exit 0
  fi
  if [[ "$DEFAULT_IF" =~ ^br.*$ ]] && echo "$bridges" | grep -qx "$DEFAULT_IF"; then
    echo "Open vSwitch bridge $DEFAULT_IF has already been configured."
    exit 0
  fi
}

regenerate_ovs_system_id() {
  echo "Removing existing OpenVSwitch system ID files to ensure uniqueness..."
  rm -f /etc/openvswitch/system-id.conf /etc/openvswitch/install_uuid.conf
  echo "Restarting ovs-vswitchd to generate new system ID..."
  systemctl restart ovs-vswitchd
}

detect_network_backend() {
  if grep -rq "iface ${DEFAULT_IF}" "${IFUPDOWN_DIR}" 2>/dev/null || \
     grep -q "iface ${DEFAULT_IF}" /etc/network/interfaces 2>/dev/null; then
    NETWORK_BACKEND="ifupdown"
    echo "Detected network backend: ifupdown"
  elif ls "${NETPLAN_DIR}"/*.yaml &>/dev/null; then
    NETWORK_BACKEND="netplan"
    echo "Detected network backend: netplan"
  else
    error_exit "Could not detect network backend for $DEFAULT_IF"
  fi
}

find_netplan_config() {
  KERNEL_IF="$DEFAULT_IF"
  NETPLAN_FILE="$(grep -rl "${DEFAULT_IF}" "${NETPLAN_DIR}" || true)"
  if [ -z "$NETPLAN_FILE" ] && [ -n "$DEFAULT_MAC" ]; then
    echo "No netplan file found by interface name, searching by MAC address $DEFAULT_MAC..."
    NETPLAN_FILE="$(grep -ril "${DEFAULT_MAC}" "${NETPLAN_DIR}" || true)"

    # Resolve the logical interface name from the netplan YAML.
    # Walk backwards from the MAC match to find the nearest interface name key.
    if [ -n "$NETPLAN_FILE" ]; then
      NETPLAN_IF=$(grep -B10 -i "${DEFAULT_MAC}" "$NETPLAN_FILE" \
        | grep -E '^    [a-zA-Z][a-zA-Z0-9_-]+:$' \
        | tail -1 | sed 's/[: ]//g')
      if [ -n "$NETPLAN_IF" ]; then
        echo "Netplan uses logical name '$NETPLAN_IF' for kernel interface '$DEFAULT_IF' (MAC: $DEFAULT_MAC)"
        DEFAULT_IF="$NETPLAN_IF"
      fi
    fi
  fi
  if [ -z "$NETPLAN_FILE" ]; then
    echo "No explicit configuration found for $DEFAULT_IF, checking for all-en section..."
    ALL_EN_CONFIG=$(netplan get "network.ethernets.all-en" 2>/dev/null || echo "")
    
    if [ -n "$ALL_EN_CONFIG" ] && [ "$ALL_EN_CONFIG" != "null" ]; then
      echo "Found all-en match pattern, regenerating explicit config from running interface..."
      NETPLAN_FILE=$(grep -rl "^[[:space:]]*all-en:" "$NETPLAN_DIR" | head -1)
      
      if [ -z "$NETPLAN_FILE" ]; then
        error_exit "Could not find netplan file for all-en"
      fi
     
      generate_explicit_config "$DEFAULT_IF" "$NETPLAN_FILE" "$UPLINK_MTU"
    else
      error_exit "Could not find netplan configuration for interface $DEFAULT_IF"
    fi
  elif [ "$(echo "$NETPLAN_FILE" | wc -l)" -gt 1 ]; then
    error_exit "Found multiple netplan config files for $DEFAULT_IF:\n$NETPLAN_FILE"
  fi
}

resolve_bridge_name() {
  local kernel_if="$1"
  if [ -n "${UPLINK_BRIDGE_NAME:-}" ]; then
    echo "$UPLINK_BRIDGE_NAME"
  else
    echo "br${kernel_if}"
  fi
}

ip_to_int() {
  local ip="$1"
  local a b c d
  IFS=. read -r a b c d <<< "$ip"
  echo $(( (a << 24) | (b << 16) | (c << 8) | d ))
}

int_to_ip() {
  local value="$1"
  printf "%d.%d.%d.%d" \
    $(( (value >> 24) & 255 )) \
    $(( (value >> 16) & 255 )) \
    $(( (value >> 8) & 255 )) \
    $(( value & 255 ))
}

cidr_first_host() {
  local ip="$1"
  local prefix="$2"
  # /31 (RFC 3021) only has two addresses; network+1 collides with the local
  # address when it owns the odd IP, which would install a route via the
  # bridge itself. Use UPLINK_ROUTE_VIA_MODE=ip for point-to-point links.
  if [ "$prefix" -lt 1 ] || [ "$prefix" -gt 30 ]; then
    error_exit "subnet-first-host requires a prefix between 1 and 30 (got $prefix; use UPLINK_ROUTE_VIA_MODE=ip for /31)"
  fi
  local ip_int mask network
  ip_int=$(ip_to_int "$ip")
  mask=$(( (0xFFFFFFFF << (32 - prefix)) & 0xFFFFFFFF ))
  network=$(( ip_int & mask ))
  int_to_ip $(( network + 1 ))
}

resolve_route_via() {
  case "${UPLINK_ROUTE_VIA_MODE:-}" in
    "")
      echo ""
      ;;
    ip)
      if [ -z "${UPLINK_ROUTE_VIA_IP:-}" ]; then
        error_exit "UPLINK_ROUTE_VIA_MODE=ip requires UPLINK_ROUTE_VIA_IP"
      fi
      echo "${UPLINK_ROUTE_VIA_IP}"
      ;;
    subnet-first-host)
      if [ -z "${UPLINK_ADDRESS:-}" ] || [ -z "${UPLINK_PREFIX:-}" ]; then
        error_exit "UPLINK_ROUTE_VIA_MODE=subnet-first-host requires UPLINK_ADDRESS and UPLINK_PREFIX"
      fi
      cidr_first_host "${UPLINK_ADDRESS}" "${UPLINK_PREFIX}"
      ;;
    *)
      error_exit "Unsupported UPLINK_ROUTE_VIA_MODE: ${UPLINK_ROUTE_VIA_MODE}"
      ;;
  esac
}

write_static_bridge_netplan() {
  local uplink_if="$1"
  local bridge_name="$2"
  local netplan_file="$3"
  local uplink_mtu="$4"
  local uplink_address="$5"
  local uplink_prefix="$6"
  local route_destination="$7"
  local route_via="$8"

  cat > "$netplan_file" <<EOF
network:
  version: 2
  ethernets:
    $uplink_if:
      mtu: ${uplink_mtu}
      dhcp4: false
  bridges:
    $bridge_name:
      mtu: ${uplink_mtu}
      interfaces:
        - $uplink_if
      openvswitch: {}
      addresses:
        - ${uplink_address}/${uplink_prefix}
EOF

  if [ -n "$route_destination" ] && [ -n "$route_via" ]; then
    cat >> "$netplan_file" <<EOF
      routes:
        - to: ${route_destination}
          via: ${route_via}
          metric: 50
EOF
  fi
}

remove_stale_netplan_for_iface() {
  # netplan merges files at apply time, so a pre-existing static-L3 claim
  # on $kernel_if would coexist with the bridge config we write and
  # duplicate the IP. Match by literal kernel iface name only (stanza key
  # or match.name); MAC-based matches are out of scope.
  local kernel_if="$1" our_file="$2" f
  for f in "${NETPLAN_DIR}"/*.yaml; do
    [ -f "$f" ] || continue
    [ "$f" = "$our_file" ] && continue
    if iface_stanza_has_static_l3 "$kernel_if" "$f"; then
      echo "Removing pre-existing netplan file $f (declares static L3 for $kernel_if)"
      rm -f "$f"
    fi
  done
}

# Returns 0 iff $file contains an ethernets stanza matching $iface (by
# stanza key or match.name) whose body declares addresses: or routes:.
# Scoping the L3 check to the matching stanza avoids deleting a shared
# netplan file that carries static L3 only for an unrelated interface.
iface_stanza_has_static_l3() {
  local iface="$1" file="$2"
  awk -v iface="$iface" '
    function indent_of(s) { match(s, /^[[:space:]]*/); return RLENGTH }
    function close_stanza() {
      if (matched && has_l3) found = 1
      matched = 0; has_l3 = 0
    }
    BEGIN { in_eth=0; eth_indent=-1; key_indent=-1; found=0 }
    {
      ind = indent_of($0); body = substr($0, ind+1)
      if (body == "" || body ~ /^#/) next
      if (in_eth && ind <= eth_indent && body !~ /^ethernets:[[:space:]]*$/) {
        close_stanza(); in_eth = 0; key_indent = -1
      }
      if (body ~ /^ethernets:[[:space:]]*$/) {
        close_stanza(); in_eth = 1; eth_indent = ind; key_indent = -1; next
      }
      if (!in_eth) next
      if (key_indent < 0 && ind > eth_indent) key_indent = ind
      if (ind == key_indent && body ~ /:[[:space:]]*$/) {
        close_stanza()
        key = body; sub(/:[[:space:]]*$/, "", key)
        if (key == iface) matched = 1
        next
      }
      if (ind > key_indent) {
        if (body ~ /^name:[[:space:]]+/) {
          val = body; sub(/^name:[[:space:]]+/, "", val); gsub(/[\047"[:space:]]/, "", val)
          if (val == iface) matched = 1
        }
        if (body ~ /^addresses:/ || body ~ /^routes:/) has_l3 = 1
      }
    }
    END { close_stanza(); exit (found ? 0 : 1) }
  ' "$file"
}

configure_static_uplink() {
  # In static mode the kernel name IS the netplan name — no MAC-fallback
  # resolution against an existing netplan, because we are writing a fresh
  # file owned entirely by this script. netplan merges files at apply
  # time; displace any pre-existing static-L3 claim on $KERNEL_IF
  # (typically cloud-init) before writing ours.
  KERNEL_IF="$DEFAULT_IF"
  BRIDGE_NAME=$(resolve_bridge_name "$KERNEL_IF")
  local route_via
  route_via=$(resolve_route_via)

  local netplan_file="${NETPLAN_DIR}/90-openvswitch-${BRIDGE_NAME}.yaml"
  remove_stale_netplan_for_iface "$KERNEL_IF" "$netplan_file"
  echo "Writing static Open vSwitch bridge config for $KERNEL_IF to $netplan_file"
  write_static_bridge_netplan \
    "$KERNEL_IF" "$BRIDGE_NAME" "$netplan_file" "$UPLINK_MTU" \
    "$UPLINK_ADDRESS" "$UPLINK_PREFIX" \
    "${UPLINK_ROUTE_DESTINATION:-}" "${route_via:-}"

  # Strip any pre-existing address/route from the bare NIC so netplan apply
  # can re-attach them under the bridge without "RTNETLINK answers: File
  # exists" failures.
  ip addr del "${UPLINK_ADDRESS}/${UPLINK_PREFIX}" dev "$KERNEL_IF" 2>/dev/null || true
  if [ -n "${UPLINK_ROUTE_DESTINATION:-}" ] && [ -n "${route_via:-}" ]; then
    ip route del "$UPLINK_ROUTE_DESTINATION" via "$route_via" dev "$KERNEL_IF" 2>/dev/null || true
  fi

  # Defensive: the YAML above already declares these, but match the abandoned
  # branch's pattern of asserting them through netplan set so the openvswitch
  # backend re-reads them on apply.
  netplan set "bridges.${BRIDGE_NAME}.interfaces=[$KERNEL_IF]"
  netplan set "bridges.${BRIDGE_NAME}.openvswitch={}"
}

transform_netplan_to_bridge() {
  BRIDGE_NAME=$(resolve_bridge_name "$KERNEL_IF")
  echo "Using netplan config: $NETPLAN_FILE"

  # Defensive: even though this path reuses NETPLAN_FILE rather than
  # writing a fresh one, displace any *other* static-L3 file claiming
  # $KERNEL_IF so netplan apply doesn't merge a stale claim back in.
  remove_stale_netplan_for_iface "$KERNEL_IF" "$NETPLAN_FILE"

  sed -i 's/ethernets/bridges/' "$NETPLAN_FILE"
  sed -i 's/'"$DEFAULT_IF"'/'"$BRIDGE_NAME"'/' "$NETPLAN_FILE"
  # Remove match/macaddress from bridge section -- netplan only allows these under ethernets
  sed -i '/^[[:space:]]*match:$/d' "$NETPLAN_FILE"
  sed -i '/^[[:space:]]*macaddress:.*$/d' "$NETPLAN_FILE"
  if [ -n "$DEFAULT_MAC" ]; then
cat >> "$NETPLAN_FILE" << EOF
  ethernets:
    $KERNEL_IF:
      mtu: $UPLINK_MTU
      match:
        macaddress: $DEFAULT_MAC
EOF
  else
cat >> "$NETPLAN_FILE" << EOF
  ethernets:
    $KERNEL_IF:
      mtu: $UPLINK_MTU
EOF
  fi
  netplan set "bridges.$BRIDGE_NAME.interfaces=[$KERNEL_IF]"
  netplan set "bridges.$BRIDGE_NAME.openvswitch={}"
}

verify_bridge() {
  # default_route_if (optional): the NIC that originally held the default
  # route, captured before any uplink selector ran. When provided AND not
  # equal to KERNEL_IF, we skip the default-route migration check (multi-NIC
  # mode: we deliberately bridged a non-default NIC). When omitted (ifupdown
  # configure_ovs_direct path), we conservatively assert the bridge owns the
  # default route, matching the single-NIC legacy behavior.
  local default_route_if="${1:-}"

  if ! ovs-vsctl list-br | grep -q "^${BRIDGE_NAME}$"; then
    error_exit "Bridge $BRIDGE_NAME was not created successfully"
  fi

  local check_default_route="true"
  if [ -n "$default_route_if" ] && [ -n "${KERNEL_IF:-}" ] && [ "$KERNEL_IF" != "$default_route_if" ]; then
    check_default_route="false"
  fi

  if [ "$check_default_route" = "true" ]; then
    NEW_DEFAULT_IF=$(ip route show default | awk '/default/ {print $5}' | head -n1)
    if [ "$NEW_DEFAULT_IF" != "$BRIDGE_NAME" ]; then
      error_exit "Default route is not using bridge $BRIDGE_NAME (currently using $NEW_DEFAULT_IF)"
    fi
    echo "Default route is now using bridge interface: $BRIDGE_NAME"
  else
    echo "Bridge $BRIDGE_NAME owns non-default-route NIC $KERNEL_IF; default route remains on ${default_route_if:-mgmt NIC}"
  fi

  # Static-mode verifications: the IP we asked for is on the bridge, and the
  # optional extra route resolves through the bridge.
  if [ -n "${UPLINK_ADDRESS:-}" ] && [ -n "${UPLINK_PREFIX:-}" ]; then
    if ! ip -o -4 addr show dev "$BRIDGE_NAME" 2>/dev/null | grep -q "${UPLINK_ADDRESS}/${UPLINK_PREFIX}"; then
      error_exit "Bridge $BRIDGE_NAME is missing address ${UPLINK_ADDRESS}/${UPLINK_PREFIX}"
    fi
  fi
  if [ -n "${UPLINK_ROUTE_DESTINATION:-}" ]; then
    if ! ip route show "${UPLINK_ROUTE_DESTINATION}" 2>/dev/null | grep -q "dev ${BRIDGE_NAME}"; then
      error_exit "Route ${UPLINK_ROUTE_DESTINATION} is not using bridge $BRIDGE_NAME"
    fi
  fi

  echo "Open vSwitch bridge $BRIDGE_NAME has been created and configured."
}

apply_and_verify() {
  local default_route_if="${1:-}"

  echo "Validating netplan configuration..."
  if ! netplan generate; then
    echo "Netplan configuration validation failed."
    error_exit "Invalid netplan configuration generated."
  fi

  echo "Applying netplan configuration..."
  netplan apply

  sleep 2

  verify_bridge "$default_route_if"
}

netmask_to_prefix() {
  local mask="$1"
  local bits=0
  IFS='.' read -r a b c d <<< "$mask"
  for octet in $a $b $c $d; do
    while [ "$octet" -gt 0 ]; do
      bits=$(( bits + (octet & 1) ))
      octet=$(( octet >> 1 ))
    done
  done
  echo "$bits"
}

configure_ovs_direct() {
  BRIDGE_NAME=$(resolve_bridge_name "$DEFAULT_IF")

  local ifcfg_file
  ifcfg_file=$(grep -rl "iface ${DEFAULT_IF}" "${IFUPDOWN_DIR}" | head -1)
  if [ -z "$ifcfg_file" ]; then
    error_exit "Could not find ifupdown config for $DEFAULT_IF in $IFUPDOWN_DIR"
  fi

  local address netmask gateway
  address=$(awk '/^[[:space:]]*address[[:space:]]+/ {print $2; exit}' "$ifcfg_file")
  netmask=$(awk '/^[[:space:]]*netmask[[:space:]]+/ {print $2; exit}' "$ifcfg_file")
  gateway=$(awk '/^[[:space:]]*gateway[[:space:]]+/ {print $2; exit}' "$ifcfg_file")

  if [ -z "$address" ] || [ -z "$netmask" ]; then
    error_exit "Could not parse address/netmask from $ifcfg_file"
  fi

  if [ -z "$gateway" ]; then
    gateway=$(ip route show default | awk '/default/ {print $3; exit}')
  fi
  if [ -z "$gateway" ]; then
    error_exit "Could not determine default gateway for $DEFAULT_IF"
  fi

  local prefix
  prefix=$(netmask_to_prefix "$netmask")

  echo "Configuring OVS bridge $BRIDGE_NAME (direct mode)..."
  echo "  interface=$DEFAULT_IF addr=$address/$prefix gw=$gateway mtu=$UPLINK_MTU"

  ovs-vsctl --may-exist add-br "$BRIDGE_NAME"

  # Pin the bridge MAC when the NIC has a locally-administered address.
  # OVS only auto-inherits globally-unique MACs (LA bit clear).
  if [ -n "$DEFAULT_MAC" ]; then
    local first_octet
    first_octet=$(printf '%d' "0x$(echo "$DEFAULT_MAC" | cut -d: -f1)")
    if (( first_octet & 2 )); then
      echo "MAC $DEFAULT_MAC is locally administered; pinning bridge MAC..."
      ovs-vsctl set bridge "$BRIDGE_NAME" other-config:hwaddr="$DEFAULT_MAC"
    fi
  fi

  ovs-vsctl --may-exist add-port "$BRIDGE_NAME" "$DEFAULT_IF"

  ip addr add "$address/$prefix" dev "$BRIDGE_NAME"
  ip link set "$BRIDGE_NAME" up
  ip link set "$BRIDGE_NAME" mtu "$UPLINK_MTU"
  ip route replace default via "$gateway" dev "$BRIDGE_NAME" onlink
  ip addr flush dev "$DEFAULT_IF"

  # Rewrite ifupdown configs so the bridge survives reboot.
  cat > "$ifcfg_file" <<EOF
auto $DEFAULT_IF
iface $DEFAULT_IF inet manual
	mtu $UPLINK_MTU
EOF

  cat > "${IFUPDOWN_DIR}/ifcfg-${BRIDGE_NAME}" <<EOF
auto $BRIDGE_NAME
iface $BRIDGE_NAME inet static
	address $address
	netmask $netmask
	gateway $gateway
	mtu $UPLINK_MTU
	ovs_type OVSBridge
	ovs_ports $DEFAULT_IF
EOF

  verify_bridge
}

configure_openvswitch() {
  check_ovs_installed
  detect_default_interface
  # Capture the default-route NIC before any selector/explicit override so
  # apply_and_verify can decide whether the default-route migration check
  # should fire.
  local default_route_if="$DEFAULT_IF"
  resolve_uplink_interface
  detect_mtu
  local target_bridge_name
  target_bridge_name=$(resolve_bridge_name "$DEFAULT_IF")
  check_already_configured "$target_bridge_name"
  regenerate_ovs_system_id
  if [ "${UPLINK_DHCP:-}" = "false" ] && [ -z "${UPLINK_ADDRESS:-}" ]; then
    error_exit "UPLINK_DHCP=false requires UPLINK_ADDRESS"
  fi

  # API validation already catches these from CWE-driven runs; the checks
  # below are defense-in-depth for direct nkecli / manual invocation. They
  # apply regardless of backend so bad input fails fast.
  if [ -n "${UPLINK_ADDRESS:-}" ]; then
    if [ -z "${UPLINK_PREFIX:-}" ]; then
      error_exit "UPLINK_ADDRESS requires UPLINK_PREFIX"
    fi
    if [ -n "${UPLINK_ROUTE_DESTINATION:-}" ] && [ -z "${UPLINK_ROUTE_VIA_MODE:-}" ]; then
      error_exit "UPLINK_ROUTE_DESTINATION requires UPLINK_ROUTE_VIA_MODE"
    fi
    # Bridging the default-route NIC with a static IP removes DHCP from it,
    # so the host has no default route unless we install one. Require an
    # explicit 0.0.0.0/0 route_destination in that case.
    if [ "$DEFAULT_IF" = "$default_route_if" ] && [ "${UPLINK_ROUTE_DESTINATION:-}" != "0.0.0.0/0" ]; then
      error_exit "Static uplink on the default-route NIC ($DEFAULT_IF) requires UPLINK_ROUTE_DESTINATION=0.0.0.0/0"
    fi
  fi

  detect_network_backend

  if [ "$NETWORK_BACKEND" = "netplan" ]; then
    if [ -n "${UPLINK_ADDRESS:-}" ]; then
      configure_static_uplink
    else
      find_netplan_config
      transform_netplan_to_bridge
    fi
    apply_and_verify "$default_route_if"
  else
    configure_ovs_direct
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  configure_openvswitch
fi
