#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu
kubeconfig_path="${E2E_KUBECONFIG_PATH:-/kubeconfig/value}"
termination_log="${E2E_TERMINATION_LOG:-/dev/termination-log}"
secret_name="e2e-etcd-encryption-$(date +%s)"
plaintext_marker="nke-e2e-plaintext-$secret_name"
secret_key="/registry/secrets/default/$secret_name"
raw_value_path="${TMPDIR:-/tmp}/raw-etcd-secret-$secret_name"
api_servers_path="${TMPDIR:-/tmp}/kube-apiserver-endpoints-$secret_name"
metrics_raw_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-$secret_name"
metrics_before_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-before-$secret_name"
metrics_after_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-after-$secret_name"
diagnostic_secrets_path="${TMPDIR:-/tmp}/diagnostic-secrets-$secret_name"
etcd_pod="unknown"
cleanup() {
	if [ -f "$diagnostic_secrets_path" ]; then
		while IFS= read -r diagnostic_secret; do
			[ -n "$diagnostic_secret" ] || continue
			kubectl --kubeconfig "$kubeconfig_path" delete secret "$diagnostic_secret" \
				-n default --ignore-not-found >/dev/null 2>&1 || true
		done < "$diagnostic_secrets_path"
	fi
	rm -f "$raw_value_path" "$api_servers_path" "$metrics_raw_path" \
		"$metrics_before_path" "$metrics_after_path" "$diagnostic_secrets_path"
	kubectl --kubeconfig "$kubeconfig_path" delete secret "$secret_name" -n default --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

secret_write_counts() {
	api_server="$1"
	output_path="$2"
	if ! kubectl --kubeconfig "$kubeconfig_path" --server="$api_server" \
		--request-timeout=10s get --raw /metrics > "$metrics_raw_path" 2>/dev/null; then
		rm -f "$metrics_raw_path"
		return 1
	fi

	# Kubernetes records the transformer used for each write. Reduce the
	# provider-specific prefix to a provider name so key names never reach logs.
	if ! awk '
		/^apiserver_storage_transformation_operations_total\{/ &&
		/transformation_type="to_storage"/ &&
		/status="OK"/ {
			if ($0 ~ /resource="/ && $0 !~ /resource="secrets"/) {
				next
			}
			marker = "transformer_prefix=\""
			start = index($0, marker)
			if (start == 0) {
				next
			}
			rest = substr($0, start + length(marker))
			finish = index(rest, "\"")
			if (finish == 0) {
				next
			}
			prefix = substr(rest, 1, finish - 1)
			if (prefix == "") {
				identity += $2
			} else if (index(prefix, "k8s:enc:secretbox:v1:") == 1) {
				secretbox += $2
			} else if (index(prefix, "k8s:enc:") == 1) {
				other += $2
			}
		}
		END {
			printf "identity %.17g\n", identity
			printf "secretbox %.17g\n", secretbox
			printf "other %.17g\n", other
		}
	' "$metrics_raw_path" > "$output_path"; then
		rm -f "$metrics_raw_path" "$output_path"
		return 1
	fi
	rm -f "$metrics_raw_path"
}

provider_delta() {
	awk '
		NR == FNR {
			before[$1] = $2
			next
		}
		$2 > before[$1] {
			if (providers != "") {
				providers = providers "+"
			}
			providers = providers $1
		}
		END {
			if (providers == "") {
				providers = "no-observed-transform(config-may-be-absent)"
			}
			printf "%s", providers
		}
	' "$metrics_before_path" "$metrics_after_path"
}

effective_write_providers() {
	api_servers=""
	if ! api_servers="$(kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system \
		-l component=kube-apiserver \
		-o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.status.podIP}{"\n"}{end}' 2>/dev/null)"; then
		printf 'unknown'
		return
	fi
	if [ -z "$api_servers" ]; then
		printf 'unknown'
		return
	fi

	printf '%s\n' "$api_servers" | sort > "$api_servers_path"
	providers=""
	probe_index=0
	while IFS='|' read -r api_pod api_ip; do
		[ -n "$api_pod" ] || continue
		probe_index=$((probe_index + 1))
		provider="unknown"
		if [ -z "$api_ip" ]; then
			provider="missing-pod-ip"
		else
			case "$api_ip" in
				*:*) api_server="https://[$api_ip]:6443" ;;
				*) api_server="https://$api_ip:6443" ;;
			esac
			diagnostic_secret="${secret_name}-api-${probe_index}"
			if ! secret_write_counts "$api_server" "$metrics_before_path"; then
				provider="metrics-before-error"
			elif ! kubectl --kubeconfig "$kubeconfig_path" --server="$api_server" \
				--request-timeout=10s create secret generic "$diagnostic_secret" \
				-n default --from-literal=probe="provider-diagnostic" >/dev/null 2>&1; then
				provider="write-error"
			else
				printf '%s\n' "$diagnostic_secret" >> "$diagnostic_secrets_path"
				if ! secret_write_counts "$api_server" "$metrics_after_path"; then
					provider="metrics-after-error"
				else
					provider="$(provider_delta)"
				fi
			fi
		fi
		if [ -n "$providers" ]; then
			providers="$providers; "
		fi
		providers="${providers}${api_pod}=${provider}"
	done < "$api_servers_path"
	printf '%s' "${providers:-unknown}"
}

fail_verification() {
	reason="$1"
	providers="$(effective_write_providers)"
	printf '%s (etcd pod: %s; effective write providers: %s)\n' \
		"$reason" "$etcd_pod" "$providers" > "$termination_log"
	exit 1
}

if ! kubectl --kubeconfig "$kubeconfig_path" create secret generic "$secret_name" \
	-n default --from-literal=probe="$plaintext_marker" >/dev/null; then
	fail_verification "failed to create the verifier Secret"
fi

# items[*], not items[0]: indexing an empty list is an error, so a cluster with
# no etcd pod took the branch below and reported a failed list. Both causes
# reached one message and the "no etcd pod" branch was unreachable.
if ! etcd_pods="$(kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system \
	-l component=etcd -o jsonpath='{.items[*].metadata.name}')"; then
	etcd_pod="unknown"
	fail_verification "failed to list etcd pods"
fi
etcd_pod="${etcd_pods%% *}"
if [ -z "$etcd_pod" ]; then
	etcd_pod="unknown"
	fail_verification "no etcd pod found"
fi

etcdctl_for_secret() {
	kubectl --kubeconfig "$kubeconfig_path" exec -n kube-system "$etcd_pod" -c etcd -- \
		etcdctl \
			--endpoints=https://127.0.0.1:2379 \
			--cacert=/etc/kubernetes/pki/etcd/ca.crt \
			--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
			--key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
			get "$secret_key" "$@"
}

# Keep the binary protobuf value only in a temporary file so the etcdctl exit
# status can be checked independently from the content checks. The EXIT trap
# removes the file, and neither the value nor etcdctl stderr is ever logged.
if ! etcdctl_for_secret --print-value-only > "$raw_value_path" 2>/dev/null; then
	fail_verification "failed to read the raw etcd Secret value"
fi
if [ ! -s "$raw_value_path" ]; then
	fail_verification "raw etcd Secret key is missing or its value is empty"
fi
if grep -a -F -q "$plaintext_marker" "$raw_value_path"; then
	fail_verification "raw etcd Secret contains the plaintext marker"
fi
if ! grep -a -q 'k8s:enc:secretbox:v1:' "$raw_value_path"; then
	fail_verification "raw etcd Secret does not contain the secretbox encryption prefix"
fi

echo "raw etcd Secret encryption verified" > "$termination_log"
