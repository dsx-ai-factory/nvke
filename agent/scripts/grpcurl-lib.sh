#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Shared grpcurl helpers for agent manual / VM cluster testing.
# Source from other scripts in this directory; do not run directly.

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	echo "error: source grpcurl-lib.sh from another script" >&2
	exit 1
fi

GRPC_LIB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GRPC_PROTO_DIR="${GRPC_PROTO_DIR:-${GRPC_LIB_ROOT}/pkg/agent/proto}"
GRPC_PROTO_FILE="${GRPC_PROTO_DIR}/node_agent.proto"
GRPC_SERVICE="${GRPC_SERVICE:-proto.NodeAgent}"

GRPC_PORT="${GRPC_PORT:-15005}"
GRPC_USE_PLAINTEXT="${GRPC_USE_PLAINTEXT:-1}"
GRPCURL_EXTRA_OPTS="${GRPCURL_EXTRA_OPTS:-}"
GRPC_WAIT_TIMEOUT_SEC="${GRPC_WAIT_TIMEOUT_SEC:-900}"
GRPC_WAIT_POLL_SEC="${GRPC_WAIT_POLL_SEC:-5}"

grpcurl_die() {
	echo "error: $*" >&2
	exit 1
}

grpcurl_require_tools() {
	[[ -f "$GRPC_PROTO_FILE" ]] || grpcurl_die "proto not found: $GRPC_PROTO_FILE (run from agent tree)"
	command -v grpcurl >/dev/null 2>&1 || grpcurl_die "grpcurl not found in PATH"
	command -v jq >/dev/null 2>&1 || grpcurl_die "jq not found in PATH"
}

grpcurl_node_target() {
	local node_ip="$1"
	echo "${node_ip}:${GRPC_PORT}"
}

grpcurl_tls_opts() {
	local -n _out=$1
	_out=()
	if [[ "$GRPC_USE_PLAINTEXT" -eq 1 ]]; then
		_out=(-plaintext)
	fi
}

# shellcheck disable=2086
grpcurl_call() {
	local target="$1"
	local method="$2"
	local json="${3:-{}}"
	local -a tls_opts=()
	grpcurl_tls_opts tls_opts
	grpcurl "${tls_opts[@]}" $GRPCURL_EXTRA_OPTS \
		-import-path "$GRPC_PROTO_DIR" \
		-proto "$(basename "$GRPC_PROTO_FILE")" \
		-d "$json" \
		"$target" "${GRPC_SERVICE}/${method}"
}

grpcurl_wait_for_agent() {
	local target="$1"
	local timeout_sec="${2:-120}"
	local poll_sec="${3:-3}"
	local deadline=$((SECONDS + timeout_sec))
	echo ">>> waiting for agent at ${target} (timeout ${timeout_sec}s)..."
	while true; do
		if ((SECONDS >= deadline)); then
			grpcurl_die "timed out waiting for agent at ${target}"
		fi
		local -a tls_opts=()
		grpcurl_tls_opts tls_opts
		if grpcurl "${tls_opts[@]}" $GRPCURL_EXTRA_OPTS "$target" list >/dev/null 2>&1; then
			echo ">>> agent ready at ${target}"
			return 0
		fi
		sleep "$poll_sec"
	done
}

grpcurl_cni_variant_json() {
	case "$(echo "$1" | tr '[:upper:]' '[:lower:]')" in
	ovn) echo '"CNI_VARIANT_OVN"' ;;
	calico) echo '"CNI_VARIANT_CALICO"' ;;
	*) grpcurl_die "cni must be ovn or calico, got: $1" ;;
	esac
}

grpcurl_cni_stream() {
	local variant="$1"
	local version="$2"
	version="${version#v}"
	if [[ -n "$version" ]]; then
		echo "$version"
		return 0
	fi
	case "$(echo "$variant" | tr '[:upper:]' '[:lower:]')" in
	ovn) echo "1.4.0" ;;
	calico) echo "3.30.1" ;;
	*) grpcurl_die "unknown cni variant: $variant" ;;
	esac
}

grpcurl_cni_images_package() {
	local variant="$1"
	local version
	version="$(grpcurl_cni_stream "$variant" "$2")"
	case "$(echo "$variant" | tr '[:upper:]' '[:lower:]')" in
	ovn) echo "nvke-cni-ovn-images-${version}" ;;
	calico) echo "nvke-cni-calico-images-${version}" ;;
	esac
}

grpcurl_cni_charts_package() {
	local variant="$1"
	local version
	version="$(grpcurl_cni_stream "$variant" "$2")"
	case "$(echo "$variant" | tr '[:upper:]' '[:lower:]')" in
	ovn) echo "nvke-cni-ovn-charts-${version}" ;;
	calico) echo "nvke-cni-calico-manifests-${version}" ;;
	esac
}

grpcurl_wait_for_async_kubeadm() {
	local target="$1"
	local want_phase="$2"
	local deadline=$((SECONDS + GRPC_WAIT_TIMEOUT_SEC))
	echo ">>> waiting for kubeadm on ${target} (want phase ${want_phase}, timeout ${GRPC_WAIT_TIMEOUT_SEC}s, poll ${GRPC_WAIT_POLL_SEC}s)..."
	while true; do
		if ((SECONDS >= deadline)); then
			grpcurl_die "timed out after ${GRPC_WAIT_TIMEOUT_SEC}s waiting for kubeadm on ${target}"
		fi
		local out ks kp msg
		out="$(grpcurl_call "$target" GetKubeadmStatus '{}')" || grpcurl_die "GetKubeadmStatus failed for ${target}"
		ks="$(echo "$out" | jq -r '.kubeadmStatus // empty')"
		kp="$(echo "$out" | jq -r '.kubeadmPhase // empty')"
		msg="$(echo "$out" | jq -r '.kubeadmCurrentMessage // empty')"
		if [[ "$ks" == "error" ]]; then
			grpcurl_die "kubeadm reported error on ${target} (phase=${kp} message=${msg})"
		fi
		if [[ "$ks" == "ready" && "$kp" == "$want_phase" ]]; then
			if ! echo "$out" | jq -e --arg p "$want_phase" '((.kubeadmCompletedPhases // []) | index($p)) != null' >/dev/null; then
				grpcurl_die "kubeadm ready on ${target} but phase ${want_phase} not in kubeadmCompletedPhases: ${out}"
			fi
			echo ">>> kubeadm finished on ${target} (status=ready phase=${want_phase})"
			return 0
		fi
		echo ">>> still waiting on ${target} (status=${ks} phase=${kp} message=${msg})"
		sleep "$GRPC_WAIT_POLL_SEC"
	done
}

grpcurl_assert_execute_kubeadm_init_response() {
	local body="$1"
	local st msg
	st="$(echo "$body" | jq -r '.status // empty')"
	msg="$(echo "$body" | jq -r '.errorMessage // empty')"
	if [[ "$st" == "error" ]]; then
		grpcurl_die "ExecuteKubeadmInit rejected (status=error): ${msg:-unknown}"
	fi
	if [[ "$st" != "pending" ]]; then
		grpcurl_die "unexpected ExecuteKubeadmInit status (want pending, got '${st}'): ${body}"
	fi
}

grpcurl_assert_execute_kubeadm_join_response() {
	local body="$1"
	local st msg
	st="$(echo "$body" | jq -r '.status // empty')"
	msg="$(echo "$body" | jq -r '.errorMessage // empty')"
	if [[ "$st" == "error" ]]; then
		grpcurl_die "ExecuteKubeadmJoin rejected (status=error): ${msg:-unknown}"
	fi
	if [[ "$st" != "pending" ]]; then
		grpcurl_die "unexpected ExecuteKubeadmJoin status (want pending, got '${st}'): ${body}"
	fi
}

# Optional 4th arg: kubernetes_version (e.g. v1.31.9).
grpcurl_run_kubeadm_init() {
	local target="$1"
	local pod_cidr="$2"
	local service_cidr="$3"
	local control_plane_endpoint="$4"
	local kubernetes_version="${5:-}"
	local init_json
	if [[ -n "$kubernetes_version" ]]; then
		init_json="$(jq -n \
			--arg pod "$pod_cidr" \
			--arg svc "$service_cidr" \
			--arg cpe "$control_plane_endpoint" \
			--arg kv "$kubernetes_version" \
			'{podNetworkCidr: $pod, serviceCidr: $svc, controlPlaneEndpoint: $cpe, noCniInstall: true, kubernetesVersion: $kv}')"
	else
		init_json="$(jq -n \
			--arg pod "$pod_cidr" \
			--arg svc "$service_cidr" \
			--arg cpe "$control_plane_endpoint" \
			'{podNetworkCidr: $pod, serviceCidr: $svc, controlPlaneEndpoint: $cpe, noCniInstall: true}')"
	fi
	echo ">>> ExecuteKubeadmInit on ${target}"
	local init_resp
	init_resp="$(grpcurl_call "$target" ExecuteKubeadmInit "$init_json")" || grpcurl_die "ExecuteKubeadmInit failed for ${target}"
	echo "$init_resp"
	grpcurl_assert_execute_kubeadm_init_response "$init_resp"
}

grpcurl_run_kubeadm_join() {
	local target="$1"
	local join_string="$2"
	local join_json
	join_json="$(jq -n --arg js "$join_string" '{kubeadmJoinString: $js}')"
	echo ">>> ExecuteKubeadmJoin on ${target}"
	local join_resp
	join_resp="$(grpcurl_call "$target" ExecuteKubeadmJoin "$join_json")" || grpcurl_die "ExecuteKubeadmJoin failed for ${target}"
	echo "$join_resp"
	grpcurl_assert_execute_kubeadm_join_response "$join_resp"
}

grpcurl_build_cni_config_json() {
	local -n _helm_sets=$1
	local j='{}'
	local kv k v
	for kv in "${_helm_sets[@]}"; do
		k="${kv%%=*}"
		v="${kv#*=}"
		[[ -n "$k" ]] || grpcurl_die "--helm-set key empty in: ${kv}"
		j="$(echo "$j" | jq --arg k "$k" --arg v "$v" '. + {($k): $v}')"
	done
	echo "$j"
}

# Optional 6th arg: cni version stream (empty = agent defaults).
grpcurl_run_install_cni() {
	local target="$1"
	local cni="$2"
	local pod_cidr="$3"
	local service_cidr="$4"
	local -n _helm_sets=$5
	local cni_version="${6:-}"
	local cni_variant_json cni_config_json install_json
	cni_variant_json="$(grpcurl_cni_variant_json "$cni")"
	cni_config_json="$(grpcurl_build_cni_config_json _helm_sets)"
	install_json="$(jq -n \
		--argjson variant "$cni_variant_json" \
		--arg pod "$pod_cidr" \
		--arg svc "$service_cidr" \
		--arg ver "$cni_version" \
		--argjson cconf "$cni_config_json" \
		'{cniVariant: $variant, version: $ver, podNetworkCidr: $pod, serviceCidr: $svc, cniConfig: $cconf}')"
	echo ">>> InstallCNI on ${target}"
	grpcurl_call "$target" InstallCNI "$install_json"
}

# Writes trimmed join strings to named variables passed as 2nd and 3rd arg names.
grpcurl_fetch_join_strings() {
	local target="$1"
	local -n _cp_out=$2
	local -n _worker_out=$3
	local out
	out="$(grpcurl_call "$target" GetKubeadmJoinString '{}')" || grpcurl_die "GetKubeadmJoinString failed for ${target}"
	_cp_out="$(echo "$out" | jq -r '.controlplaneJoinString // empty' | xargs)"
	_worker_out="$(echo "$out" | jq -r '.workerJoinString // empty' | xargs)"
	[[ -n "$_cp_out" && -n "$_worker_out" ]] || grpcurl_die "empty join strings from ${target}: ${out}"
}

grpcurl_install_packages_sync() {
	local target="$1"
	shift
	local -a packages=("$@")
	[[ ${#packages[@]} -gt 0 ]] || return 0
	local req out
	req="$(jq -n --argjson pkgs "$(printf '%s\n' "${packages[@]}" | jq -R . | jq -s .)" '{packages: $pkgs, update: true, async: false}')"
	echo ">>> InstallPackages on ${target}: ${packages[*]}"
	out="$(grpcurl_call "$target" InstallPackages "$req")" || grpcurl_die "InstallPackages failed for ${target}"
	echo "$out"
}

grpcurl_install_collection_async() {
	local target="$1"
	local collection_url="$2"
	local staging="${3:-false}"
	local req out request_id
	req="$(jq -n --arg url "$collection_url" --argjson staging "$staging" '{collectionURL: $url, staging: $staging}')"
	echo ">>> InstallCollection on ${target}: ${collection_url}"
	out="$(grpcurl_call "$target" InstallCollection "$req")" || grpcurl_die "InstallCollection failed for ${target}"
	request_id="$(echo "$out" | jq -r '.requestId // empty')"
	[[ -n "$request_id" ]] || grpcurl_die "InstallCollection returned no requestId: ${out}"
	grpcurl_wait_for_bundle_status "$target" "$request_id"
}

grpcurl_wait_for_bundle_status() {
	local target="$1"
	local request_id="$2"
	local timeout_sec="${3:-1800}"
	local poll_sec="${4:-5}"
	local deadline=$((SECONDS + timeout_sec))
	echo ">>> waiting for bundle/collection ${request_id} on ${target}..."
	while true; do
		if ((SECONDS >= deadline)); then
			grpcurl_die "timed out waiting for request ${request_id} on ${target}"
		fi
		local out statuses pending failed
		out="$(grpcurl_call "$target" GetBundleStatus "$(jq -n --arg id "$request_id" '{requestId: $id}')")" || grpcurl_die "GetBundleStatus failed for ${target}"
		statuses="$(echo "$out" | jq -r '.statuses[]?.status // empty')"
		if [[ -z "$statuses" ]]; then
			echo ">>> no bundle status yet for ${request_id}"
			sleep "$poll_sec"
			continue
		fi
		pending="$(echo "$out" | jq -r '[.statuses[]?.status] | map(select(. == "installing" or . == "pending")) | length')"
		failed="$(echo "$out" | jq -r '[.statuses[]?.status] | map(select(. == "error")) | length')"
		if [[ "$failed" != "0" ]]; then
			grpcurl_die "bundle/collection ${request_id} failed on ${target}: ${out}"
		fi
		if [[ "$pending" == "0" ]]; then
			echo ">>> bundle/collection ${request_id} finished on ${target}"
			return 0
		fi
		echo ">>> bundle/collection ${request_id} still running on ${target}"
		sleep "$poll_sec"
	done
}
