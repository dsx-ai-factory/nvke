#!/bin/sh
set -eu
args="$*"
secret_name="unknown"
api_server=""
for arg in "$@"; do
	case "$arg" in
		/registry/secrets/default/*)
			secret_name="${arg##*/}"
			;;
		--server=*)
			api_server="${arg#--server=}"
			;;
	esac
done

case "$args" in
	*" delete secret "*)
		exit 0
		;;
	*" create secret generic "*)
		if [ -n "$api_server" ]; then
			case "$api_server" in
				*10.0.0.10*) server_index=0 ;;
				*10.0.0.11*) server_index=1 ;;
				*) exit 98 ;;
			esac
			: > "${FAKE_STATE_DIR}/server-${server_index}"
		fi
		exit "${FAKE_CREATE_STATUS:-0}"
		;;
	*" get --raw /metrics"*)
		case "$api_server" in
			*10.0.0.10*)
				server_index=0
				provider="${FAKE_PROVIDER_0:-secretbox}"
				;;
			*10.0.0.11*)
				server_index=1
				provider="${FAKE_PROVIDER_1:-identity}"
				;;
			*)
				exit 97
				;;
		esac
		if [ ! -f "${FAKE_STATE_DIR}/server-${server_index}" ]; then
			exit 0
		fi
		case "$provider" in
			identity)
				printf '%s\n' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix=""} 1'
				;;
			secretbox)
				printf '%s' "$FAKE_SECRETBOX_METRICS"
				;;
			secretbox-legacy)
				printf '%s' "$FAKE_SECRETBOX_LEGACY_METRICS"
				;;
			other)
				printf '%s\n' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix="k8s:enc:aescbc:v1:"} 1' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix="nvke-key-1:"} 1'
				;;
		esac
		;;
	*" get pods -n kube-system -l component=etcd "*)
		if [ "${FAKE_ETCD_LIST_STATUS:-0}" -ne 0 ]; then
			exit "$FAKE_ETCD_LIST_STATUS"
		fi
		# No colon, so an empty FAKE_ETCD_POD models a cluster with no etcd pod
		# rather than falling back to the default.
		etcd_pod_out="${FAKE_ETCD_POD-etcd-0}"
		# Real kubectl treats an out-of-range index as an error, so items[0]
		# against an empty list exits non-zero instead of printing nothing.
		# Without this the empty case is indistinguishable from a healthy one.
		if [ -z "$etcd_pod_out" ]; then
			case "$args" in
				*'items[0]'*)
					printf '%s\n' 'error: array index out of bounds' >&2
					exit 1
					;;
			esac
		fi
		printf '%s' "$etcd_pod_out"
		;;
	*" get pods -n kube-system -l component=kube-apiserver "*)
		printf '%s\n' 'kube-apiserver-1|10.0.0.11' 'kube-apiserver-0|10.0.0.10'
		;;
	*" --count-only"*)
		printf '%s\n' 'Error: --count-only is only for --write-out=fields' >&2
		exit 128
		;;
	*" --print-value-only"*)
		if [ "${FAKE_ETCD_READ_STATUS:-0}" -ne 0 ]; then
			exit "$FAKE_ETCD_READ_STATUS"
		fi
		case "${FAKE_ETCD_VALUE:-encrypted}" in
			empty)
				;;
			encrypted)
				printf 'binary-prefix-k8s:enc:secretbox:v1:encrypted-payload'
				;;
			plaintext)
				printf 'RAW_SECRET_SENTINEL-nke-e2e-plaintext-%s' "$secret_name"
				;;
			unrecognized)
				printf 'RAW_SECRET_SENTINEL-unrecognized-encoding'
				;;
		esac
		;;
	*)
		printf 'unexpected fake kubectl invocation: %s\n' "$args" >&2
		exit 99
		;;
esac
