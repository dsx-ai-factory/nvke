#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu
kubeconfig_path="${E2E_KUBECONFIG_PATH:-/kubeconfig/value}"
termination_log="${E2E_TERMINATION_LOG:-/dev/termination-log}"
# Seconds between two attempts of retry_kubectl. The unit test sets 0.
retry_delay="${E2E_KUBECTL_RETRY_DELAY:-2}"
secret_name="e2e-etcd-encryption-$(date +%s)"
plaintext_marker="nke-e2e-plaintext-$secret_name"
secret_key="/registry/secrets/default/$secret_name"
raw_value_path="${TMPDIR:-/tmp}/raw-etcd-secret-$secret_name"
kubectl_stdout_path="${TMPDIR:-/tmp}/kubectl-stdout-$secret_name"
kubectl_stderr_path="${TMPDIR:-/tmp}/kubectl-stderr-$secret_name"
api_servers_path="${TMPDIR:-/tmp}/kube-apiserver-endpoints-$secret_name"
metrics_raw_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-$secret_name"
metrics_before_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-before-$secret_name"
metrics_after_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-after-$secret_name"
diagnostic_secrets_path="${TMPDIR:-/tmp}/diagnostic-secrets-$secret_name"
etcd_pod="unknown"

# retry_kubectl runs kubectl against the kubeconfig's server, the control-plane
# endpoint, with a bounded request timeout and up to $1 attempts. One call
# through the anycast VIP can fail while the control plane settles after a node
# joins, and the next one succeeds. Only the successful attempt's stdout is
# passed on. Every failed attempt's stderr goes to the container log, and the
# last one's stays in kubectl_stderr_path for fail_verification.
retry_kubectl() {
	attempts="$1"
	shift
	attempt=1
	while :; do
		if kubectl --kubeconfig "$kubeconfig_path" --request-timeout=10s "$@" \
			> "$kubectl_stdout_path" 2> "$kubectl_stderr_path"; then
			cat "$kubectl_stdout_path"
			cat "$kubectl_stderr_path" >&2
			rm -f "$kubectl_stdout_path" "$kubectl_stderr_path"
			return 0
		else
			status=$?
		fi
		printf 'kubectl %s failed on attempt %s of %s:\n' "$1" "$attempt" "$attempts" >&2
		cat "$kubectl_stderr_path" >&2
		if [ "$attempt" -ge "$attempts" ]; then
			rm -f "$kubectl_stdout_path"
			return "$status"
		fi
		attempt=$((attempt + 1))
		sleep "$retry_delay"
	done
}

cleanup() {
	if [ -f "$diagnostic_secrets_path" ]; then
		while IFS= read -r diagnostic_secret; do
			[ -n "$diagnostic_secret" ] || continue
			retry_kubectl 2 delete secret "$diagnostic_secret" \
				-n default --ignore-not-found >/dev/null 2>&1 || true
		done < "$diagnostic_secrets_path"
	fi
	retry_kubectl 2 delete secret "$secret_name" -n default --ignore-not-found >/dev/null 2>&1 || true
	rm -f "$raw_value_path" "$api_servers_path" "$metrics_raw_path" \
		"$metrics_before_path" "$metrics_after_path" "$diagnostic_secrets_path" \
		"$kubectl_stdout_path" "$kubectl_stderr_path"
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
	if ! api_servers="$(kubectl --kubeconfig "$kubeconfig_path" --request-timeout=10s get pods -n kube-system \
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
	# The failed kubectl call's stderr, read before the diagnostics below run
	# kubectl again: its first lines, joined, say why the call failed.
	kubectl_error=""
	if [ -s "$kubectl_stderr_path" ]; then
		kubectl_error="; kubectl error: $(awk 'NR <= 5 { printf "%s%s", separator, substr($0, 1, 400); separator = " | " }' "$kubectl_stderr_path")"
	elif [ -f "$kubectl_stderr_path" ]; then
		kubectl_error="; kubectl error: none on stderr"
	fi
	providers="$(effective_write_providers)"
	printf '%s (etcd pod: %s; effective write providers: %s%s)\n' \
		"$reason" "$etcd_pod" "$providers" "$kubectl_error" > "$termination_log"
	exit 1
}

if ! retry_kubectl 5 create secret generic "$secret_name" \
	-n default --from-literal=probe="$plaintext_marker" >/dev/null; then
	fail_verification "failed to create the verifier Secret"
fi

# items[*], not items[0]: indexing an empty list is an error, so a cluster with
# no etcd pod took the branch below and reported a failed list. Both causes
# reached one message and the "no etcd pod" branch was unreachable.
if ! etcd_pods="$(retry_kubectl 5 get pods -n kube-system \
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
	retry_kubectl 5 exec -n kube-system "$etcd_pod" -c etcd -- \
		etcdctl \
			--endpoints=https://127.0.0.1:2379 \
			--cacert=/etc/kubernetes/pki/etcd/ca.crt \
			--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
			--key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
			get "$secret_key" "$@"
}

# Keep the binary protobuf value only in temporary files so the etcdctl exit
# status can be checked independently from the content checks. The EXIT trap
# removes them and the value is never logged: stderr carries error text only.
if ! etcdctl_for_secret --print-value-only > "$raw_value_path"; then
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
