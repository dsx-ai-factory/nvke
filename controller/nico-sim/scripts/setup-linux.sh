#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# setup-linux.sh — Set up a Linux machine for nico-vms development.
# Installs QEMU/libvirt, configures bridge networking, dnsmasq, iptables, etc.
#
# Usage: bash scripts/setup-linux.sh
#
# This script is idempotent — safe to run multiple times.
# Runs as the current user; sudo is used only for privileged operations.

set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }
check() { command -v "$1" >/dev/null 2>&1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Pin the host toolchain to controller/go.mod, not go.dev's latest.
# Override with GO_VERSION=go1.x.y if needed.
required_go_version() {
    if [[ -n "${GO_VERSION:-}" ]]; then
        echo "${GO_VERSION}"
        return
    fi
    local gomod="${SCRIPT_DIR}/../../go.mod"
    local ver
    if [[ ! -f "$gomod" ]]; then
        fail "controller/go.mod not found at ${gomod}; set GO_VERSION=go1.x.y"
    fi
    ver="$(awk '/^go / { print "go" $2; exit }' "$gomod")"
    if [[ -z "$ver" ]]; then
        fail "no go directive in ${gomod}; set GO_VERSION=go1.x.y"
    fi
    echo "$ver"
}

install_go() {
    local version="$1"
    info "Installing Go ${version} from go.dev..."
    local go_tarball_tmp
    go_tarball_tmp="$(mktemp)"
    curl -fsSLo "$go_tarball_tmp" "https://go.dev/dl/${version}.linux-${DEB_ARCH}.tar.gz"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$go_tarball_tmp"
    rm -f "$go_tarball_tmp"
    export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
    info "Go ${version} installed"
}

# Host architecture, normalized so this script works on both amd64 and arm64.
# DEB_ARCH matches Debian/Go/kubectl/kind download-URL naming (amd64|arm64);
# UNAME_ARCH matches QEMU's per-target binary suffix (x86_64|aarch64).
DEB_ARCH="$(dpkg --print-architecture)"
UNAME_ARCH="$(uname -m)"
# Native QEMU system package + binary for this arch. Fail fast on anything other
# than the two supported arches rather than silently defaulting to x86.
case "$DEB_ARCH" in
    arm64)
        QEMU_SYSTEM_PKG="qemu-system-arm"
        QEMU_SYSTEM_BIN="qemu-system-aarch64"
        ;;
    amd64)
        QEMU_SYSTEM_PKG="qemu-system-x86"
        QEMU_SYSTEM_BIN="qemu-system-x86_64"
        ;;
    *)
        fail "Unsupported architecture: $DEB_ARCH (supported: amd64, arm64)"
        ;;
esac

# ── 0. Pre-flight checks ───────────────────────────────────────────────────
# Verify tools that this script does NOT install are present before doing
# anything else. Packages installed via apt (qemu, libvirt, dnsmasq, etc.)
# are handled in step 1 — no need to check them here.

echo ""
printf "${BOLD}Pre-flight checks${NC}\n"
echo "-----------------"
echo ""

# Verify sudo access upfront (caches credentials for subsequent calls)
if ! sudo -v; then
    fail "sudo access is required for package installation and network setup"
fi

if check docker; then
    ver=$(docker --version 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "docker" "$ver"
    if ! docker info &>/dev/null; then
        printf "  ${YELLOW}!!${NC}  docker is installed but the daemon is not running\n"
        info "Starting docker daemon..."
        sudo systemctl enable --now docker 2>/dev/null || true
        sudo systemctl start docker 2>/dev/null || true
    fi
else
    printf "  ${RED}--${NC}  %-20s ${RED}missing${NC} -- installing...\n" "docker"
    sudo apt-get update -qq
    sudo apt-get install -y -qq -o Dpkg::Options::="--force-confold" docker.io
    sudo systemctl enable --now docker 2>/dev/null || true
    sudo usermod -aG docker "$USER" 2>/dev/null || true
    info "docker installed"
fi

# docker.io does not ship the buildx CLI plugin. install-capnico.sh builds the
# local CAPNICo chart with DOCKER_BUILDKIT=1, and the CLI rejects that build
# when the plugin is missing.
if ! docker buildx version >/dev/null 2>&1; then
    info "Installing docker-buildx..."
    sudo apt-get update -qq
    sudo apt-get install -y -qq -o Dpkg::Options::="--force-confold" docker-buildx
fi

# Ensure the current shell can talk to docker without re-login.
if ! docker info &>/dev/null 2>&1; then
    sudo chmod 666 /var/run/docker.sock
    info "Opened docker socket permissions for current session"
fi

# KVM — fix permissions automatically if possible.
if [[ -e /dev/kvm ]]; then
    if [[ -r /dev/kvm ]] && [[ -w /dev/kvm ]]; then
        printf "  ${GREEN}ok${NC}  %-20s accessible\n" "/dev/kvm"
    else
        printf "  ${YELLOW}!!${NC}  /dev/kvm exists but is not accessible by your user -- fixing...\n"
        sudo usermod -aG kvm "$(whoami)"
        info "Added $USER to kvm group (log out/in or run 'newgrp kvm' to activate)"
    fi
else
    printf "  ${YELLOW}!!${NC}  /dev/kvm not found -- attempting to load KVM modules...\n"
    sudo modprobe kvm 2>/dev/null || true
    sudo modprobe kvm_intel 2>/dev/null || true
    sudo modprobe kvm_amd 2>/dev/null || true
    if [[ -e /dev/kvm ]]; then
        info "KVM modules loaded successfully"
        sudo usermod -aG kvm "$(whoami)" 2>/dev/null || true
        sudo chmod 666 /dev/kvm
    else
        warn "/dev/kvm still not available -- VMs will use TCG software emulation (slow)"
        warn "Ensure your CPU supports virtualization and the kvm module is loaded"
    fi
fi

echo ""

info "Installing for user: $USER (home: $HOME)"

# ── 1. System packages ──────────────────────────────────────────────────────

info "Updating package lists..."
sudo apt-get update -qq

info "Installing QEMU, libvirt, and utilities..."
sudo apt-get install -y -qq -o Dpkg::Options::="--force-confold" \
    "$QEMU_SYSTEM_PKG" \
    qemu-utils \
    qemu-efi-aarch64 \
    ovmf \
    libvirt-daemon-system \
    libvirt-clients \
    virtinst \
    bridge-utils \
    cloud-image-utils \
    genisoimage \
    e2fsprogs \
    curl \
    git \
    build-essential \
    iptables \
    dnsmasq-base \
    net-tools \
    swtpm \
    swtpm-tools

# ── 1b. Dev tooling (tilt, kubectl, kind, helm, go, ko) ───────────────────

if check tilt; then
    ver=$(tilt version 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "tilt" "$ver"
else
    info "Installing tilt..."
    curl -fsSL https://raw.githubusercontent.com/tilt-dev/tilt/master/scripts/install.sh | bash
    info "tilt installed"
fi

if check kubectl; then
    ver=$(kubectl version --client -o json 2>/dev/null | grep gitVersion | head -1 || kubectl version --client 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "kubectl" "$ver"
else
    info "Installing kubectl..."
    KUBECTL_VERSION=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
    kubectl_tmp="$(mktemp)"
    curl -fsSLo "$kubectl_tmp" "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${DEB_ARCH}/kubectl"
    sudo install -o root -g root -m 0755 "$kubectl_tmp" /usr/local/bin/kubectl
    rm -f "$kubectl_tmp"
    info "kubectl ${KUBECTL_VERSION} installed"
fi

if check kind; then
    ver=$(kind version 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "kind" "$ver"
else
    info "Installing kind..."
    kind_tmp="$(mktemp)"
    curl -fsSLo "$kind_tmp" "https://github.com/kubernetes-sigs/kind/releases/latest/download/kind-linux-${DEB_ARCH}"
    sudo install -o root -g root -m 0755 "$kind_tmp" /usr/local/bin/kind
    rm -f "$kind_tmp"
    info "kind $(kind version 2>&1 | head -1) installed"
fi

# install-capnico.sh passes --server-side, which Helm 3 does not have.
# get-helm-4 installs the latest Helm 4. A Helm 3 binary already on PATH is replaced.
helm_major=""
if check helm; then
    helm_major="$(helm version --short 2>/dev/null | awk '{print $1}')"
    helm_major="${helm_major%%.*}"
    helm_major="${helm_major#v}"
fi
if [[ "$helm_major" == "4" ]]; then
    ver=$(helm version --short 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "helm" "$ver"
else
    if [[ -n "$helm_major" ]]; then
        warn "Helm ${helm_major} is installed; replacing with Helm 4..."
    else
        info "Installing Helm 4..."
    fi
    curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
    hash -r
    info "helm $(helm version --short 2>&1 | head -1) installed"
fi

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
REQUIRED_GO="$(required_go_version)"
if check go; then
    installed="$(go version | awk '{print $3}')"
    if [[ "$installed" == "$REQUIRED_GO" ]]; then
        ver=$(go version 2>&1 | head -1)
        printf "  ${GREEN}ok${NC}  %-20s %s\n" "go" "$ver"
    else
        warn "Go ${installed} does not match ${REQUIRED_GO} from go.mod; replacing..."
        install_go "$REQUIRED_GO"
    fi
else
    install_go "$REQUIRED_GO"
fi

# Persist Go PATH for all future shells (Tilt spawns subprocesses that need this).
PROFILE_SNIPPET='export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"'
PROFILE_FILE="$HOME/.profile"
if ! grep -qF '/usr/local/go/bin' "$PROFILE_FILE" 2>/dev/null; then
    echo "$PROFILE_SNIPPET" >> "$PROFILE_FILE"
    info "Added Go paths to $PROFILE_FILE"
fi
if ! grep -qF '/usr/local/go/bin' "$HOME/.bashrc" 2>/dev/null; then
    echo "$PROFILE_SNIPPET" >> "$HOME/.bashrc"
    info "Added Go paths to ~/.bashrc"
fi

if check ko; then
    ver=$(ko version 2>&1 | head -1)
    printf "  ${GREEN}ok${NC}  %-20s %s\n" "ko" "$ver"
else
    info "Installing ko..."
    go install github.com/google/ko@latest
    info "ko installed"
fi

# ── 2. KVM check ────────────────────────────────────────────────────────────

info "Checking KVM availability..."
if [ -e /dev/kvm ]; then
    info "KVM is available (/dev/kvm exists)"
    KVM_AVAILABLE=true
else
    # Try loading modules
    sudo modprobe kvm 2>/dev/null || true
    sudo modprobe kvm_intel 2>/dev/null || true
    sudo modprobe kvm_amd 2>/dev/null || true

    if [ -e /dev/kvm ]; then
        info "KVM modules loaded successfully"
        KVM_AVAILABLE=true
    else
        warn "KVM is NOT available (no hardware virtualization)"
        warn "QEMU will use TCG (software emulation) — tests will be slow"
        KVM_AVAILABLE=false
    fi
fi

# ── 3. User permissions ────────────────────────────────────────────────────

info "Adding $USER to libvirt and kvm groups..."
sudo usermod -aG libvirt "$USER" 2>/dev/null || true
sudo usermod -aG kvm "$USER" 2>/dev/null || true

# ── 3b. Sudoers rule for setcap (passwordless CAP_NET_ADMIN) ─────────────
# The BGP speaker requires CAP_NET_ADMIN for netlink route management.
# This sudoers rule allows the dev user to run setcap without a password,
# so `make run-vm` works seamlessly after each rebuild.
# Restricted to the specific binary path (resolved via realpath to handle symlinks).
NICO_SIM_BINARY="$(realpath -m "${REPO_ROOT}/bin/nico-sim")"

SUDOERS_FILE="/etc/sudoers.d/nico-sim-setcap"
SUDOERS_RULE="$USER ALL=(root) NOPASSWD: /usr/sbin/setcap cap_net_admin+ep ${NICO_SIM_BINARY}"
if [ ! -f "$SUDOERS_FILE" ] || ! grep -qF "${NICO_SIM_BINARY}" "$SUDOERS_FILE" 2>/dev/null; then
    echo "$SUDOERS_RULE" | sudo tee "$SUDOERS_FILE" > /dev/null
    sudo chmod 0440 "$SUDOERS_FILE"
    info "Sudoers rule added: passwordless setcap for $USER on ${NICO_SIM_BINARY}"
else
    info "Sudoers rule for setcap already configured"
fi

# ── 3c. swtpm_localca state directory ─────────────────────────────────────
# swtpm_localca needs read/write access to /var/lib/swtpm-localca. When
# nico-sim runs as a non-root user, the default root-owned directory is
# not writable and swtpm_setup fails with "Need read/write rights on
# statedir". Grant ownership to the current user.
SWTPM_LOCALCA_DIR="/var/lib/swtpm-localca"
if [ -d "$SWTPM_LOCALCA_DIR" ]; then
    sudo chown -R "$(id -u):$(id -g)" "$SWTPM_LOCALCA_DIR"
    info "swtpm_localca statedir ownership set to $(id -un)"
else
    sudo mkdir -p "$SWTPM_LOCALCA_DIR"
    sudo chown -R "$(id -u):$(id -g)" "$SWTPM_LOCALCA_DIR"
    info "Created swtpm_localca statedir with ownership $(id -un)"
fi

# ── 3d. AppArmor: allow swtpm to access paths outside /home ───────────────
# swtpm's AppArmor profile uses "owner @{HOME}/**" which resolves to
# /home/*/**.  If the user's actual $HOME is elsewhere (e.g. /localhome/,
# /workspace/, NFS mounts) or XDG_CACHE_HOME points outside /home, swtpm
# will be denied and VMs fail with "swtpm died" / status 256.
# Add any non-/home paths to the local override.
SWTPM_LOCAL="/etc/apparmor.d/local/usr.bin.swtpm"
if [ -f /etc/apparmor.d/usr.bin.swtpm ]; then
    RELOAD_APPARMOR=false

    # If $HOME is not under /home, add it.
    if [[ "$HOME" != /home/* ]]; then
        RULE="owner ${HOME}/** rwk,"
        if ! grep -qF "$RULE" "$SWTPM_LOCAL" 2>/dev/null; then
            echo "$RULE" | sudo tee -a "$SWTPM_LOCAL" > /dev/null
            RELOAD_APPARMOR=true
            info "AppArmor: added swtpm access to $HOME"
        fi
    fi

    # If XDG_CACHE_HOME is outside $HOME, add it too.
    CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}"
    if [[ "$CACHE_DIR" != "$HOME"* ]]; then
        RULE="owner ${CACHE_DIR}/** rwk,"
        if ! grep -qF "$RULE" "$SWTPM_LOCAL" 2>/dev/null; then
            echo "$RULE" | sudo tee -a "$SWTPM_LOCAL" > /dev/null
            RELOAD_APPARMOR=true
            info "AppArmor: added swtpm access to $CACHE_DIR"
        fi
    fi

    if $RELOAD_APPARMOR; then
        sudo apparmor_parser -r /etc/apparmor.d/usr.bin.swtpm
        info "AppArmor: reloaded swtpm profile"
    else
        info "AppArmor: swtpm profile already up to date"
    fi
fi

# ── 4. libvirt service ──────────────────────────────────────────────────────

info "Ensuring libvirtd is running..."
sudo systemctl enable --now libvirtd 2>/dev/null || true
sudo systemctl start libvirtd 2>/dev/null || true

# ── 4b. qemu-bridge-helper for qemu:///session ──────────────────────────────

# qemu:///session mode runs QEMU as the current user (no root needed, all files
# user-owned). Bridge networking requires qemu-bridge-helper with the setuid bit
# and a bridge.conf listing allowed bridges.
BRIDGE_HELPER=$(find /usr/lib/qemu /usr/libexec -name qemu-bridge-helper 2>/dev/null | head -1)
if [ -n "$BRIDGE_HELPER" ]; then
    info "Configuring qemu-bridge-helper for session mode..."
    sudo mkdir -p /etc/qemu
    if ! grep -q "allow cwebr0" /etc/qemu/bridge.conf 2>/dev/null; then
        echo "allow cwebr0" | sudo tee -a /etc/qemu/bridge.conf >/dev/null
        info "Added 'allow cwebr0' to /etc/qemu/bridge.conf"
    fi
    sudo chmod u+s "$BRIDGE_HELPER"
    info "Set setuid on $BRIDGE_HELPER"
else
    warn "qemu-bridge-helper not found — qemu:///session bridge networking may not work"
fi

# ── 5. Bridge networking ────────────────────────────────────────────────────

BRIDGE_NAME="cwebr0"
BRIDGE_CIDR="172.30.0.0/24"
BRIDGE_IP="172.30.0.1/24"

info "Setting up bridge $BRIDGE_NAME ($BRIDGE_CIDR)..."
if ! ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then
    sudo ip link add name "$BRIDGE_NAME" type bridge
fi
if ! ip addr show "$BRIDGE_NAME" | grep -q "172.30.0.1"; then
    sudo ip addr add "$BRIDGE_IP" dev "$BRIDGE_NAME" 2>/dev/null || true
fi
sudo ip link set "$BRIDGE_NAME" up

# Add link-local IP for cloud-init metadata server.
# Production VM images poll 169.254.169.254 for EC2-style metadata.
METADATA_IP="169.254.169.254/32"
if ! ip addr show "$BRIDGE_NAME" | grep -q "169.254.169.254"; then
    sudo ip addr add "$METADATA_IP" dev "$BRIDGE_NAME" 2>/dev/null || true
    info "Added metadata IP $METADATA_IP to $BRIDGE_NAME"
fi


# ── 5b. DHCP server (dnsmasq) for production images ──────────────────────
# Production FDE images need DHCP to get an initial IP before cloud-init
# can reach the HTTP metadata server at 169.254.169.254:7777.
# dnsmasq uses --dhcp-hostsdir to pick up per-instance MAC→IP reservations
# written by the nico-vms engine (inotify, no reload needed).
DHCP_HOSTS_DIR="/var/lib/nico-vms/dhcp-hosts"
DNSMASQ_PID="/var/run/nico-vms-dnsmasq.pid"
DNSMASQ_LEASE="/var/run/nico-vms-dnsmasq.leases"

sudo mkdir -p "$DHCP_HOSTS_DIR"
sudo chmod 777 "$DHCP_HOSTS_DIR"
# Lease file must be writable by the test user so the engine can purge stale leases.
sudo touch "$DNSMASQ_LEASE"
sudo chmod 666 "$DNSMASQ_LEASE"

# Kill any existing nico-vms dnsmasq
if [ -f "$DNSMASQ_PID" ]; then
    sudo kill "$(cat "$DNSMASQ_PID")" 2>/dev/null || true
    sudo rm -f "$DNSMASQ_PID"
fi

# Host firewalls commonly default INPUT to DROP. Allow only the services that
# nico-vms exposes to guests on its dedicated bridge.
ensure_bridge_input_rule() {
    if ! sudo iptables -C INPUT -i "$BRIDGE_NAME" "$@" -j ACCEPT 2>/dev/null; then
        sudo iptables -I INPUT -i "$BRIDGE_NAME" "$@" -j ACCEPT
        info "INPUT rule added on $BRIDGE_NAME: $*"
    fi
}
ensure_bridge_input_rule -p udp --dport 67
ensure_bridge_input_rule -p udp --dport 53
ensure_bridge_input_rule -p tcp --dport 53
ensure_bridge_input_rule -p tcp --dport 7777
ensure_bridge_input_rule -p tcp --dport 10179

info "Starting dnsmasq DHCP+DNS server on $BRIDGE_NAME..."
# DNS is enabled so VMs can resolve carbide-pxe.forge to the bridge gateway.
# Production images have seedfrom: http://carbide-pxe.forge/api/v0/cloud-init/
# which cloud-init uses to fetch NoCloud metadata. By resolving it locally,
# we avoid the 120s timeout waiting for the unreachable production Forge server.
#
# Detect host's upstream DNS — avoids hardcoding 8.8.8.8 which is blocked on
# networks that enforce internal DNS (e.g. corporate/lab environments).
UPSTREAM_DNS_ARGS=()
if command -v resolvectl &>/dev/null; then
    while IFS= read -r dns_ip; do
        UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
    done < <(resolvectl status 2>/dev/null | awk '/DNS Servers:/{for(i=3;i<=NF;i++) print $i}' | grep -v '^$' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
    while IFS= read -r dns_ip; do
        UPSTREAM_DNS_ARGS+=("--server=${dns_ip}")
    done < <(grep '^nameserver' /etc/resolv.conf 2>/dev/null | awk '{print $2}' | grep -v '^127\.' | head -3)
fi
if [[ ${#UPSTREAM_DNS_ARGS[@]} -eq 0 ]]; then
    UPSTREAM_DNS_ARGS=("--server=127.0.0.53")
fi
info "dnsmasq upstream DNS: ${UPSTREAM_DNS_ARGS[*]}"

sudo dnsmasq \
    --interface="$BRIDGE_NAME" \
    --bind-interfaces \
    --dhcp-range=172.30.0.10,172.30.0.254,255.255.255.0,30m \
    --dhcp-hostsdir="$DHCP_HOSTS_DIR" \
    --dhcp-option=option:router,172.30.0.1 \
    --dhcp-option=option:dns-server,172.30.0.1 \
    --except-interface=lo \
    --no-resolv \
    --no-hosts \
    "${UPSTREAM_DNS_ARGS[@]}" \
    --address=/carbide-pxe.forge/172.30.0.1 \
    --dhcp-leasefile="$DNSMASQ_LEASE" \
    --pid-file="$DNSMASQ_PID" \
    --log-dhcp \
    --dhcp-ignore-clid
info "dnsmasq started (pid=$(cat "$DNSMASQ_PID"))"

# Export env vars for the nico-vms engine to discover DHCP configuration.
# These are read by config.Parse() at engine startup.
cat <<ENVEOF

  Set these environment variables when running nico-vms or integration tests:

    export NICO_VMS_DHCP_HOSTS_DIR=$DHCP_HOSTS_DIR
    export NICO_VMS_DHCP_LEASE_FILE=$DNSMASQ_LEASE

ENVEOF

# Enable IP forwarding and use Layer 4 ECMP hashing so separate connections
# from the same client can be distributed across control-plane nexthops.
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo sysctl -w net.ipv4.fib_multipath_hash_policy=1 >/dev/null
if ! sudo iptables -t nat -C POSTROUTING -s "$BRIDGE_CIDR" ! -d "$BRIDGE_CIDR" -j MASQUERADE 2>/dev/null; then
    sudo iptables -t nat -A POSTROUTING -s "$BRIDGE_CIDR" ! -d "$BRIDGE_CIDR" -j MASQUERADE
    info "NAT rule added for $BRIDGE_CIDR"
fi

# Allow forwarding for bridge traffic. Docker and libvirt set the FORWARD chain
# policy to DROP, so without explicit ACCEPT rules VM traffic is silently dropped
# even though MASQUERADE is configured.
if ! sudo iptables -C FORWARD -i "$BRIDGE_NAME" -s "$BRIDGE_CIDR" -j ACCEPT 2>/dev/null; then
    sudo iptables -I FORWARD -i "$BRIDGE_NAME" -s "$BRIDGE_CIDR" -j ACCEPT
    info "FORWARD rule added: allow outbound from $BRIDGE_NAME"
fi
if ! sudo iptables -C FORWARD -o "$BRIDGE_NAME" -d "$BRIDGE_CIDR" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
    sudo iptables -I FORWARD -o "$BRIDGE_NAME" -d "$BRIDGE_CIDR" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    info "FORWARD rule added: allow return traffic to $BRIDGE_NAME"
fi

# Redirect port 80 on the bridge to port 7777 where the nico-vms metadata server listens.
# Production images reach http://carbide-pxe.forge/api/v0/cloud-init/ (port 80 implicit).
# dnsmasq resolves carbide-pxe.forge to the bridge gateway; this rule sends it to our server.
# IMPORTANT: Only redirect traffic destined for the gateway IP, NOT all port 80 traffic —
# otherwise VMs cannot reach external HTTP servers (apt repos, etc.).
BRIDGE_GW="${BRIDGE_IP%%/*}"  # strip /24 -> 172.30.0.1
if ! sudo iptables -t nat -C PREROUTING -i "$BRIDGE_NAME" -d "$BRIDGE_GW" -p tcp --dport 80 -j REDIRECT --to-port 7777 2>/dev/null; then
    sudo iptables -t nat -A PREROUTING -i "$BRIDGE_NAME" -d "$BRIDGE_GW" -p tcp --dport 80 -j REDIRECT --to-port 7777
    info "iptables REDIRECT: port 80 -> 7777 on $BRIDGE_NAME (dest=$BRIDGE_GW only)"
fi

# Redirect BGP port 179 → 10179 on bridge gateway for nico-vms BGP speaker.
# kube-vip hardcodes peer port to 179; this mirrors the existing 80→7777 metadata redirect.
# Scope to bridge gateway IP (-d) to avoid intercepting unrelated port-179 traffic.
if ! sudo iptables -t nat -C PREROUTING -i "$BRIDGE_NAME" -d "$BRIDGE_GW" -p tcp --dport 179 -j REDIRECT --to-port 10179 2>/dev/null; then
    sudo iptables -t nat -A PREROUTING -i "$BRIDGE_NAME" -d "$BRIDGE_GW" -p tcp --dport 179 -j REDIRECT --to-port 10179
    info "iptables REDIRECT: port 179 -> 10179 on $BRIDGE_NAME (dest=$BRIDGE_GW only)"
fi

# ── 5c. Increase virtlogd max log size ────────────────────────────────────────
# virtlogd manages serial/console log files for libvirt VMs. The default
# max_size is 2MiB, which truncates production cloud-init logs (cloud-init
# retry + bundle downloads can exceed 2MiB easily). Increase to 50MiB and
# keep 3 rotated backups so we never lose boot history.
#
# IMPORTANT: virtlogd can run in system mode (/etc/libvirt/virtlogd.conf) or
# session mode (~/.config/libvirt/virtlogd.conf). nico-vms uses session mode
# (qemu:///session or virsh as non-root), so we must configure BOTH locations.
apply_virtlogd_config() {
    local conf="$1"
    local use_sudo="$2"
    local changed=false
    if ! grep -q '^max_size = 52428800' "$conf" 2>/dev/null; then
        $use_sudo sed -i 's/^#\s*max_size\s*=.*/max_size = 52428800/' "$conf"
        changed=true
    fi
    if ! grep -q '^max_backups = 3' "$conf" 2>/dev/null; then
        $use_sudo sed -i 's/^#\s*max_backups\s*=.*/max_backups = 3/' "$conf"
        changed=true
    fi
    $changed && return 0 || return 1
}

NEEDS_RESTART=false

# System config
if [ -f "/etc/libvirt/virtlogd.conf" ]; then
    if apply_virtlogd_config "/etc/libvirt/virtlogd.conf" sudo; then
        info "virtlogd system config updated (max_size=50MiB)"
        NEEDS_RESTART=true
    fi
fi

# Session config (for non-root libvirt usage)
USER_VIRTLOGD_DIR="$HOME/.config/libvirt"
USER_VIRTLOGD_CONF="$USER_VIRTLOGD_DIR/virtlogd.conf"
if [ ! -f "$USER_VIRTLOGD_CONF" ] && [ -f "/etc/libvirt/virtlogd.conf" ]; then
    mkdir -p "$USER_VIRTLOGD_DIR"
    cp /etc/libvirt/virtlogd.conf "$USER_VIRTLOGD_CONF"
    info "Copied virtlogd.conf to session config: $USER_VIRTLOGD_CONF"
    NEEDS_RESTART=true
fi
if [ -f "$USER_VIRTLOGD_CONF" ]; then
    if apply_virtlogd_config "$USER_VIRTLOGD_CONF" ""; then
        info "virtlogd session config updated (max_size=50MiB)"
        NEEDS_RESTART=true
    fi
fi

if $NEEDS_RESTART; then
    # Kill any running virtlogd and restart the socket cleanly
    sudo killall virtlogd 2>/dev/null || true
    sudo systemctl stop virtlogd.socket virtlogd.service 2>/dev/null || true
    sudo systemctl start virtlogd.socket 2>/dev/null || true
    info "virtlogd restarted with new config (system + session)"
else
    info "virtlogd already configured (max_size=50MiB, max_backups=3)"
fi

# ── 6. OVMF firmware check ─────────────────────────────────────────────────

info "Checking OVMF firmware..."
OVMF_PATHS=(
    /usr/share/OVMF/OVMF_CODE.fd
    /usr/share/OVMF/OVMF_CODE_4M.fd
    /usr/share/ovmf/OVMF.fd
    /usr/share/qemu/ovmf-x86_64-code.fd
    # aarch64 UEFI firmware (AAVMF); libvirt auto-selects it via firmware='efi'.
    /usr/share/AAVMF/AAVMF_CODE.fd
    /usr/share/qemu-efi-aarch64/QEMU_EFI.fd
)
OVMF_FOUND=false
for p in "${OVMF_PATHS[@]}"; do
    if [ -f "$p" ]; then
        info "OVMF found: $p"
        OVMF_FOUND=true
        break
    fi
done
if ! $OVMF_FOUND; then
    warn "OVMF firmware not found — UEFI boot may not work"
fi

# ── 7. Verify everything ───────────────────────────────────────────────────

echo ""
info "=== Verification ==="
for cmd in "$QEMU_SYSTEM_BIN" qemu-img qemu-io virsh cloud-localds mkisofs debugfs; do
    if check "$cmd"; then
        echo -e "  ${GREEN}OK${NC}  $cmd ($(command -v $cmd))"
    else
        echo -e "  ${RED}MISSING${NC}  $cmd"
    fi
done

echo ""
if [ -e /dev/kvm ]; then
    echo -e "  ${GREEN}OK${NC}  KVM (/dev/kvm)"
else
    echo -e "  ${YELLOW}WARN${NC}  No KVM — will use TCG (slow)"
fi

if ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then
    echo -e "  ${GREEN}OK${NC}  Bridge $BRIDGE_NAME"
else
    echo -e "  ${RED}MISSING${NC}  Bridge $BRIDGE_NAME"
fi

# Apply CAP_NET_ADMIN to pre-built binary (if present)
if [ -f "${NICO_SIM_BINARY}" ]; then
    sudo setcap cap_net_admin+ep "${NICO_SIM_BINARY}"
    info "Applied CAP_NET_ADMIN to ${NICO_SIM_BINARY}"
fi

echo ""
info "Setup complete. Log out and back in for group changes to take effect."
echo ""
if ! $KVM_AVAILABLE; then
    warn "Without KVM, real VM tests will use TCG software emulation."
    warn "Set NICO_VMS_READINESS_TIMEOUT=30m for production image tests."
fi
