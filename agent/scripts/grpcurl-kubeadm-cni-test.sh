#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Exercise ExecuteKubeadmInit / ExecuteKubeadmJoin and InstallCNI on an agent over gRPC
# via grpcurl. Intended for manual testing when the agent runs with plaintext gRPC
# (--disable-mtls); for TLS/mTLS, set GRPCURL_EXTRA_OPTS (e.g. -cert -key -cacert).
#
# InstallCNI only deploys charts/manifests (helm/kubectl). Install the charts and image Debian
# packages beforehand — e.g. via `InstallPackages` with nvke-cni-ovn-charts-<stream> and
# nvke-cni-ovn-images-<stream> (workers/join nodes typically need images only if not pulling from a
# registry). The agent does not verify container images in containerd so air-gapped pre-load and
# private-registry-only flows both remain valid.
#
# Dependencies: grpcurl, jq
#
# ExecuteKubeadmInit / ExecuteKubeadmJoin return immediately while kubeadm runs in the agent's
# background goroutine. This script waits (polls GetKubeadmStatus) before InstallCNI so e.g.
# InstallCNI sees /etc/kubernetes/admin.conf after init completes.
#
# OVN-CHART POD READINESS AFTER InstallCNI: the image is the public
# ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu (no pull secret) and
# the images deb pre-imports it into containerd, so a flapping pod is not a
# registry-auth problem. Missing /etc/cni/net.d/10-ovn-kubernetes.conf in
# readiness probes is typical until ovnkube-node establishes OVS/control-plane
# SB connections; persistent failure is an OVN/OVS/logs issue, not fixed by
# delaying the grpcurl script further.
#
# Examples:
#   ./scripts/grpcurl-kubeadm-cni-test.sh 192.168.1.10 ovn init \\
#       --control-plane-endpoint 192.168.1.10:6443
#
#   ./scripts/grpcurl-kubeadm-cni-test.sh 192.168.1.11 calico join \\
#       --join-string 'kubeadm join 192.168.1.10:6443 --token abc... --discovery-token-ca-cert-hash sha256:...'

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
  grpcurl-kubeadm-cni-test.sh [options] <node-ip|host> <cni:ovn|calico> <init|join>

Required for join:
  --join-string 'kubeadm join ...'   full kubeadm join command line (Worker or control-plane string)

Options:
  -p, --port <port>                  gRPC port (default: 15005)
      --plaintext                    use insecure gRPC (default: on)
      --tls                          do not pass -plaintext to grpcurl (server TLS without client auth)
      --skip-install-cni             only run kubeadm init/join (no InstallCNI calls)

Async kubeadm → InstallCNI (only when not --skip-install-cni):
      --wait-timeout-sec <n>         max seconds to poll GetKubeadmStatus (default: 900)
      --wait-poll-sec <n>            sleep between polls (default: 5)

Init / kubeadm cluster networking (passed into KubeadmInitRequest):
      --pod-network-cidr <cidr>      default: 10.244.0.0/16
      --service-cidr <cidr>          default: 10.96.0.0/12
      --control-plane-endpoint <hp>   default for init: <node-ip>:6443 if unset
      --kubernetes-version <ver>     optional kubeadm --kubernetes-version (e.g. v1.31.9)

InstallCNI (OVN / Calico Helm --set merges into request cni_config; repeat for multiple keys):
      --helm-set KEY=VALUE           passed through to helm install via the agent (--set KEY=VALUE)

  -h, --help                         show this help

Environment:
  GRPCURL_EXTRA_OPTS                 extra args for every grpcurl invocation (quoted string)

On **init**, runs InstallCNI once (charts Debian package must be installed first — use
InstallPackages or apt outside this script). Pre-install image packages on hosts that cannot
pull from your registry during pod schedule.

On **join**, InstallCNI is not invoked (workers are not eligible). Install image/manifest packages
via InstallPackages or apt before join when offline image load or other host prep is required.

OVN troubleshooting (after InstallCNI returns OK): see header comment on imagePullSecrets and CNI readiness.

EOF
}

NODE_IP=""
CNI=""
OP=""
JOIN_STRING=""
SKIP_INSTALL_CNI=0
POD_CIDR="10.244.0.0/16"
SERVICE_CIDR="10.96.0.0/12"
CONTROL_PLANE_ENDPOINT=""
KUBERNETES_VERSION=""
declare -a HELM_SETS=()

while [[ $# -gt 0 ]]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-p | --port)
		GRPC_PORT="${2:-}"
		[[ -n "$GRPC_PORT" ]] || die "--port needs a value"
		shift 2
		;;
	--join-string)
		JOIN_STRING="${2:-}"
		[[ -n "$JOIN_STRING" ]] || die "--join-string needs a value"
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
	--skip-install-cni)
		SKIP_INSTALL_CNI=1
		shift
		;;
	--pod-network-cidr)
		POD_CIDR="${2:-}"
		[[ -n "$POD_CIDR" ]] || die "--pod-network-cidr needs a value"
		shift 2
		;;
	--service-cidr)
		SERVICE_CIDR="${2:-}"
		[[ -n "$SERVICE_CIDR" ]] || die "--service-cidr needs a value"
		shift 2
		;;
	--control-plane-endpoint)
		CONTROL_PLANE_ENDPOINT="${2:-}"
		[[ -n "$CONTROL_PLANE_ENDPOINT" ]] || die "--control-plane-endpoint needs a value"
		shift 2
		;;
	--kubernetes-version)
		KUBERNETES_VERSION="${2:-}"
		[[ -n "$KUBERNETES_VERSION" ]] || die "--kubernetes-version needs a value"
		shift 2
		;;
	--wait-timeout-sec)
		GRPC_WAIT_TIMEOUT_SEC="${2:-}"
		[[ -n "$GRPC_WAIT_TIMEOUT_SEC" ]] || die "--wait-timeout-sec needs a value"
		[[ "$GRPC_WAIT_TIMEOUT_SEC" =~ ^[0-9]+$ ]] || die "--wait-timeout-sec must be a non-negative integer"
		shift 2
		;;
	--wait-poll-sec)
		GRPC_WAIT_POLL_SEC="${2:-}"
		[[ -n "$GRPC_WAIT_POLL_SEC" ]] || die "--wait-poll-sec needs a value"
		[[ "$GRPC_WAIT_POLL_SEC" =~ ^[0-9]+$ ]] || die "--wait-poll-sec must be > 0"
		((GRPC_WAIT_POLL_SEC > 0)) || die "--wait-poll-sec must be > 0"
		shift 2
		;;
	--helm-set)
		[[ -n "${2:-}" ]] || die "--helm-set requires KEY=VALUE"
		[[ "$2" == *"="* ]] || die "--helm-set must look like KEY=VALUE (got: $2)"
		HELM_SETS+=("$2")
		shift 2
		;;
	--)
		shift
		break
		;;
	-*)
		die "unknown option: $1"
		;;
	*)
		break
		;;
	esac
done

grpcurl_require_tools

[[ $# -ge 3 ]] || {
	usage >&2
	exit 1
}

NODE_IP="$1"
CNI="$2"
OP="$3"

case "$(echo "$OP" | tr '[:upper:]' '[:lower:]')" in
init) ;;
join) ;;
*)
	die "operation must be init or join, got: $OP"
	;;
esac

TARGET="$(grpcurl_node_target "$NODE_IP")"

if [[ "$(echo "$OP" | tr '[:upper:]' '[:lower:]')" == "init" ]]; then
	[[ -z "$CONTROL_PLANE_ENDPOINT" ]] && CONTROL_PLANE_ENDPOINT="${NODE_IP}:6443"
	grpcurl_run_kubeadm_init "$TARGET" "$POD_CIDR" "$SERVICE_CIDR" "$CONTROL_PLANE_ENDPOINT" "$KUBERNETES_VERSION"
elif [[ "$(echo "$OP" | tr '[:upper:]' '[:lower:]')" == "join" ]]; then
	[[ -n "$JOIN_STRING" ]] || die "join requires --join-string 'kubeadm join ...'"
	grpcurl_run_kubeadm_join "$TARGET" "$JOIN_STRING"
fi

if [[ "$SKIP_INSTALL_CNI" -eq 1 ]]; then
	echo ">>> skipping InstallCNI (--skip-install-cni)"
	exit 0
fi

case "$(echo "$OP" | tr '[:upper:]' '[:lower:]')" in
init) grpcurl_wait_for_async_kubeadm "$TARGET" initialize ;;
join) grpcurl_wait_for_async_kubeadm "$TARGET" join ;;
esac

if [[ "$(echo "$OP" | tr '[:upper:]' '[:lower:]')" == "init" ]]; then
	echo ">>> InstallCNI (deploy charts/manifests — prerequisite: charts deb installed on this node)"
	grpcurl_run_install_cni "$TARGET" "$CNI" "$POD_CIDR" "$SERVICE_CIDR" HELM_SETS
else
	echo ">>> skipping InstallCNI on join workers (deploy CNI from a control plane only)"
fi

echo ">>> done"
