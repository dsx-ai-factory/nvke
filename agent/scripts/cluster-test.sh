#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# End-to-end manual cluster test against agent VMs over grpcurl (plaintext by default).
#
# Orchestrates the recommended NVKE controller-style flow:
#   1. Wait for agents (optional preflight / package install)
#   2. kubeadm init on the first control-plane node (noCniInstall=true)
#   3. Join additional control-plane nodes, then workers
#   4. InstallCNI once on the bootstrap control-plane
#
# Assumes VMs already run nvke-agent with --disable-mtls (or equivalent) and that apt is
# configured (e.g. via cloud-init). Use --install-cni-packages when chart/image debs are not
# pre-installed.
#
# Dependencies: grpcurl, jq
#
# Quick start (1 CP + 1 worker):
#   ./scripts/cluster-test.sh \
#       --control-plane 192.168.1.10 \
#       --worker 192.168.1.11 \
#       --cni ovn \
#       --install-cni-packages
#
# HA control plane (3 CP + workers):
#   ./scripts/cluster-test.sh --config samples/cluster-test.example.json
#
# See also: scripts/grpcurl-kubeadm-cni-test.sh for single-node init/join debugging.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=grpcurl-lib.sh
source "${SCRIPT_DIR}/grpcurl-lib.sh"

die() {
	grpcurl_die "$@"
}

usage() {
	cat <<'EOF'
Usage:
  cluster-test.sh [options]

Node lists (repeat flags or comma-separate values):
  --control-plane <ip>             control-plane node (first entry is bootstrap/init node)
  --worker <ip>                    worker node (repeat for each worker)

Cluster / CNI:
  --cni <ovn|calico>               CNI variant (default: ovn)
  --cni-version <stream>           CNI deb stream suffix (default: agent defaults)
  --control-plane-endpoint <host:port>
                                   API server endpoint (default: <bootstrap-cp>:6443)
  --pod-network-cidr <cidr>        default: 10.244.0.0/16
  --service-cidr <cidr>            default: 10.96.0.0/12
  --kubernetes-version <ver>       optional kubeadm version (e.g. v1.31.9)
  --helm-set KEY=VALUE             extra InstallCNI helm --set values (repeatable)

Packages (optional — skip when cloud-init already installed debs):
  --skip-packages                  do not run InstallCollection / InstallPackages
  --install-cni-packages           install CNI image debs on all nodes and charts deb on bootstrap CP
  --collection-url <url>           InstallCollection on every node before kubeadm
  --collection-staging             pass staging=true to InstallCollection
  --package <deb-spec>             extra InstallPackages entry on every node (repeatable)

gRPC / timing:
  -p, --port <port>                gRPC port (default: 15005)
      --plaintext                  insecure gRPC (default)
      --tls                        omit -plaintext
      --agent-ready-timeout <sec>  wait for agent RPC (default: 300)
      --wait-timeout-sec <sec>     kubeadm poll timeout (default: 900)
      --wait-poll-sec <sec>        poll interval (default: 5)
      --skip-wait-agents           do not wait for grpcurl list before starting

Config file:
  --config <file.json>             JSON config (CLI flags override file values)

Other:
      --dry-run                      print planned steps and exit
  -h, --help                       show this help

Environment:
  GRPCURL_EXTRA_OPTS               extra args for every grpcurl invocation

Multi-control-plane: list every CP with --control-plane in order. The first IP is the
bootstrap node (init + InstallCNI). Remaining CPs join with the control-plane join string.

EOF
}

CONFIG_FILE=""
DRY_RUN=0
SKIP_WAIT_AGENTS=0
SKIP_PACKAGES=0
INSTALL_CNI_PACKAGES=0
COLLECTION_URL=""
COLLECTION_STAGING=0
POD_CIDR="10.244.0.0/16"
SERVICE_CIDR="10.96.0.0/12"
CONTROL_PLANE_ENDPOINT=""
KUBERNETES_VERSION=""
CNI="ovn"
CNI_VERSION=""
AGENT_READY_TIMEOUT=300
declare -a CONTROL_PLANES=()
declare -a WORKERS=()
declare -a EXTRA_PACKAGES=()
declare -a HELM_SETS=()

append_csv_or_value() {
	local -n _arr=$1
	local value="$2"
	local part
	IFS=',' read -ra parts <<<"$value"
	for part in "${parts[@]}"; do
		part="$(echo "$part" | xargs)"
		[[ -n "$part" ]] && _arr+=("$part")
	done
}

load_config_file() {
	local file="$1"
	[[ -f "$file" ]] || die "config not found: $file"
	jq -e . "$file" >/dev/null || die "invalid JSON config: $file"

	if [[ "$GRPC_PORT" == "15005" ]]; then
		local p
		p="$(jq -r '.grpc.port // empty' "$file")"
		[[ -n "$p" ]] && GRPC_PORT="$p"
	fi
	if [[ "$GRPC_USE_PLAINTEXT" -eq 1 ]]; then
		local plain
		plain="$(jq -r '.grpc.plaintext // empty' "$file")"
		[[ "$plain" == "false" ]] && GRPC_USE_PLAINTEXT=0
	fi

	local cpe pod svc kv cni variant ver
	cpe="$(jq -r '.cluster.control_plane_endpoint // empty' "$file")"
	[[ -z "$CONTROL_PLANE_ENDPOINT" && -n "$cpe" ]] && CONTROL_PLANE_ENDPOINT="$cpe"
	pod="$(jq -r '.cluster.pod_network_cidr // empty' "$file")"
	[[ -n "$pod" ]] && POD_CIDR="$pod"
	svc="$(jq -r '.cluster.service_cidr // empty' "$file")"
	[[ -n "$svc" ]] && SERVICE_CIDR="$svc"
	kv="$(jq -r '.cluster.kubernetes_version // empty' "$file")"
	[[ -z "$KUBERNETES_VERSION" && -n "$kv" ]] && KUBERNETES_VERSION="$kv"

	variant="$(jq -r '.cni.variant // empty' "$file")"
	[[ -n "$variant" ]] && CNI="$variant"
	ver="$(jq -r '.cni.version // empty' "$file")"
	[[ -z "$CNI_VERSION" && -n "$ver" ]] && CNI_VERSION="$ver"
	while IFS= read -r hs; do
		[[ -n "$hs" ]] && HELM_SETS+=("$hs")
	done < <(jq -r '.cni.helm_sets[]? // empty' "$file")

	if [[ ${#CONTROL_PLANES[@]} -eq 0 ]]; then
		while IFS= read -r ip; do
			[[ -n "$ip" ]] && CONTROL_PLANES+=("$ip")
		done < <(jq -r '.nodes.control_plane[]? // empty' "$file")
	fi
	if [[ ${#WORKERS[@]} -eq 0 ]]; then
		while IFS= read -r ip; do
			[[ -n "$ip" ]] && WORKERS+=("$ip")
		done < <(jq -r '.nodes.workers[]? // empty' "$file")
	fi

	local skip pkg coll staging
	skip="$(jq -r '.packages.skip // false' "$file")"
	[[ "$skip" == "true" && "$SKIP_PACKAGES" -eq 0 ]] && SKIP_PACKAGES=1
	pkg="$(jq -r '.packages.install_cni_packages // false' "$file")"
	[[ "$pkg" == "true" && "$INSTALL_CNI_PACKAGES" -eq 0 ]] && INSTALL_CNI_PACKAGES=1
	coll="$(jq -r '.packages.collection_url // empty' "$file")"
	[[ -z "$COLLECTION_URL" && -n "$coll" ]] && COLLECTION_URL="$coll"
	staging="$(jq -r '.packages.collection_staging // false' "$file")"
	[[ "$staging" == "true" && "$COLLECTION_STAGING" -eq 0 ]] && COLLECTION_STAGING=1
	while IFS= read -r pkg_name; do
		[[ -n "$pkg_name" ]] && EXTRA_PACKAGES+=("$pkg_name")
	done < <(jq -r '.packages.extra[]? // empty' "$file")

	local art kwt ps
	art="$(jq -r '.timing.agent_ready_timeout_sec // empty' "$file")"
	[[ -n "$art" ]] && AGENT_READY_TIMEOUT="$art"
	kwt="$(jq -r '.timing.kubeadm_timeout_sec // empty' "$file")"
	[[ -n "$kwt" ]] && GRPC_WAIT_TIMEOUT_SEC="$kwt"
	ps="$(jq -r '.timing.poll_sec // empty' "$file")"
	[[ -n "$ps" ]] && GRPC_WAIT_POLL_SEC="$ps"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--config)
		CONFIG_FILE="${2:-}"
		[[ -n "$CONFIG_FILE" ]] || die "--config needs a value"
		shift 2
		;;
	--control-plane)
		append_csv_or_value CONTROL_PLANES "${2:-}"
		[[ ${#CONTROL_PLANES[@]} -gt 0 ]] || die "--control-plane needs a value"
		shift 2
		;;
	--worker)
		append_csv_or_value WORKERS "${2:-}"
		[[ ${#WORKERS[@]} -gt 0 ]] || die "--worker needs a value"
		shift 2
		;;
	--cni)
		CNI="${2:-}"
		[[ -n "$CNI" ]] || die "--cni needs a value"
		shift 2
		;;
	--cni-version)
		CNI_VERSION="${2:-}"
		shift 2
		;;
	--control-plane-endpoint)
		CONTROL_PLANE_ENDPOINT="${2:-}"
		[[ -n "$CONTROL_PLANE_ENDPOINT" ]] || die "--control-plane-endpoint needs a value"
		shift 2
		;;
	--pod-network-cidr)
		POD_CIDR="${2:-}"
		shift 2
		;;
	--service-cidr)
		SERVICE_CIDR="${2:-}"
		shift 2
		;;
	--kubernetes-version)
		KUBERNETES_VERSION="${2:-}"
		shift 2
		;;
	--helm-set)
		[[ -n "${2:-}" ]] || die "--helm-set requires KEY=VALUE"
		HELM_SETS+=("$2")
		shift 2
		;;
	--skip-packages)
		SKIP_PACKAGES=1
		shift
		;;
	--install-cni-packages)
		INSTALL_CNI_PACKAGES=1
		shift
		;;
	--collection-url)
		COLLECTION_URL="${2:-}"
		[[ -n "$COLLECTION_URL" ]] || die "--collection-url needs a value"
		shift 2
		;;
	--collection-staging)
		COLLECTION_STAGING=1
		shift
		;;
	--package)
		EXTRA_PACKAGES+=("${2:-}")
		[[ -n "${EXTRA_PACKAGES[$((${#EXTRA_PACKAGES[@]} - 1))]}" ]] || die "--package needs a value"
		shift 2
		;;
	-p | --port)
		GRPC_PORT="${2:-}"
		shift 2
		;;
	--plaintext)
		GRPC_USE_PLAINTEXT=1
		shift
		;;
	--tls)
		GRPC_USE_PLAINTEXT=0
		shift
		;;
	--agent-ready-timeout)
		AGENT_READY_TIMEOUT="${2:-}"
		shift 2
		;;
	--wait-timeout-sec)
		GRPC_WAIT_TIMEOUT_SEC="${2:-}"
		shift 2
		;;
	--wait-poll-sec)
		GRPC_WAIT_POLL_SEC="${2:-}"
		shift 2
		;;
	--skip-wait-agents)
		SKIP_WAIT_AGENTS=1
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--)
		shift
		break
		;;
	-*)
		die "unknown option: $1"
		;;
	*)
		die "unexpected argument: $1 (use --help)"
		;;
	esac
done

[[ -n "$CONFIG_FILE" ]] && load_config_file "$CONFIG_FILE"

grpcurl_require_tools

[[ ${#CONTROL_PLANES[@]} -gt 0 ]] || die "at least one --control-plane node is required"

BOOTSTRAP_CP="${CONTROL_PLANES[0]}"
if [[ -z "$CONTROL_PLANE_ENDPOINT" ]]; then
	CONTROL_PLANE_ENDPOINT="${BOOTSTRAP_CP}:6443"
fi

all_nodes() {
	local ip
	for ip in "${CONTROL_PLANES[@]}" "${WORKERS[@]}"; do
		echo "$ip"
	done
}

log_plan() {
	echo "=== cluster-test plan ==="
	echo "bootstrap control-plane: ${BOOTSTRAP_CP}"
	echo "control-plane nodes:     ${CONTROL_PLANES[*]}"
	echo "workers:                 ${WORKERS[*]:-(none)}"
	echo "API endpoint:            ${CONTROL_PLANE_ENDPOINT}"
	echo "CNI:                     ${CNI} (version stream: ${CNI_VERSION:-default})"
	echo "pod/service CIDR:        ${POD_CIDR} / ${SERVICE_CIDR}"
	echo "skip packages:           ${SKIP_PACKAGES}"
	echo "install CNI packages:    ${INSTALL_CNI_PACKAGES}"
	echo "collection URL:          ${COLLECTION_URL:-(none)}"
	echo "========================="
}

install_node_packages() {
	local node_ip="$1"
	local target
	target="$(grpcurl_node_target "$node_ip")"
	local -a pkgs=()

	if [[ "$SKIP_PACKAGES" -eq 0 && -n "$COLLECTION_URL" ]]; then
		grpcurl_install_collection_async "$target" "$COLLECTION_URL" "$([[ "$COLLECTION_STAGING" -eq 1 ]] && echo true || echo false)"
	fi

	if [[ "$INSTALL_CNI_PACKAGES" -eq 1 ]]; then
		pkgs+=("$(grpcurl_cni_images_package "$CNI" "$CNI_VERSION")")
	fi
	if [[ "$SKIP_PACKAGES" -eq 0 && ${#EXTRA_PACKAGES[@]} -gt 0 ]]; then
		pkgs+=("${EXTRA_PACKAGES[@]}")
	fi
	if [[ ${#pkgs[@]} -gt 0 ]]; then
		grpcurl_install_packages_sync "$target" "${pkgs[@]}"
	fi
}

install_bootstrap_charts_package() {
	[[ "$INSTALL_CNI_PACKAGES" -eq 1 ]] || return 0
	local target charts_pkg
	target="$(grpcurl_node_target "$BOOTSTRAP_CP")"
	charts_pkg="$(grpcurl_cni_charts_package "$CNI" "$CNI_VERSION")"
	grpcurl_install_packages_sync "$target" "$charts_pkg"
}

run_cluster() {
	log_plan

	local node_ip target
	if [[ "$SKIP_WAIT_AGENTS" -eq 0 ]]; then
		while IFS= read -r node_ip; do
			grpcurl_wait_for_agent "$(grpcurl_node_target "$node_ip")" "$AGENT_READY_TIMEOUT" "$GRPC_WAIT_POLL_SEC"
		done < <(all_nodes | sort -u)
	fi

	if [[ "$SKIP_PACKAGES" -eq 0 || "$INSTALL_CNI_PACKAGES" -eq 1 || -n "$COLLECTION_URL" || ${#EXTRA_PACKAGES[@]} -gt 0 ]]; then
		echo ">>> installing packages on all nodes"
		while IFS= read -r node_ip; do
			install_node_packages "$node_ip"
		done < <(all_nodes | sort -u)
	fi

	target="$(grpcurl_node_target "$BOOTSTRAP_CP")"
	echo ">>> kubeadm init on bootstrap control-plane ${BOOTSTRAP_CP}"
	grpcurl_run_kubeadm_init "$target" "$POD_CIDR" "$SERVICE_CIDR" "$CONTROL_PLANE_ENDPOINT" "$KUBERNETES_VERSION"
	grpcurl_wait_for_async_kubeadm "$target" initialize

	local cp_join worker_join
	grpcurl_fetch_join_strings "$target" cp_join worker_join
	echo ">>> fetched join strings from ${BOOTSTRAP_CP}"

	local cp_ip
	for cp_ip in "${CONTROL_PLANES[@]:1}"; do
		target="$(grpcurl_node_target "$cp_ip")"
		echo ">>> joining control-plane ${cp_ip}"
		grpcurl_run_kubeadm_join "$target" "$cp_join"
		grpcurl_wait_for_async_kubeadm "$target" join
	done

	local worker_ip
	for worker_ip in "${WORKERS[@]}"; do
		target="$(grpcurl_node_target "$worker_ip")"
		echo ">>> joining worker ${worker_ip}"
		grpcurl_run_kubeadm_join "$target" "$worker_join"
		grpcurl_wait_for_async_kubeadm "$target" join
	done

	install_bootstrap_charts_package
	target="$(grpcurl_node_target "$BOOTSTRAP_CP")"
	grpcurl_run_install_cni "$target" "$CNI" "$POD_CIDR" "$SERVICE_CIDR" HELM_SETS "$CNI_VERSION"

	echo ">>> cluster-test finished"
	echo "    bootstrap CP: ${BOOTSTRAP_CP}"
	echo "    kubectl: grpcurl -plaintext ${BOOTSTRAP_CP}:${GRPC_PORT} ${GRPC_SERVICE}/GetKubeconfig"
}

if [[ "$DRY_RUN" -eq 1 ]]; then
	log_plan
	echo ">>> dry-run: no RPCs sent"
	exit 0
fi

run_cluster
