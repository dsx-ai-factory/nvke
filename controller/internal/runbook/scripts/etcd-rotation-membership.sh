#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Read-only probe: emit no config, credentials, member metadata or tool stderr.
set -euo pipefail
exec 2>/dev/null
(( $# > 0 ))
matches=()
while read -r container; do
  [[ -n "$container" ]] || continue
  if crictl inspect "$container" | jq -e '
    .status.labels["io.kubernetes.pod.namespace"] == "kube-system" and
    (.status.labels["io.kubernetes.pod.name"] | startswith("etcd-"))
  ' >/dev/null; then
    matches+=("$container")
  fi
done < <(crictl ps --label io.kubernetes.container.name=etcd -q)
(( ${#matches[@]} == 1 ))
etcdctl_command=(crictl exec "${matches[0]}" etcdctl
  --endpoints=https://127.0.0.1:2379
  --cacert=/etc/kubernetes/pki/etcd/ca.crt
  --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt
  --key=/etc/kubernetes/pki/etcd/healthcheck-client.key)
expected="$(jq -cn '$ARGS.positional | sort' --args "$@")"
"${etcdctl_command[@]}" member list --write-out=json | jq -e --argjson expected "$expected" '
  (.members | type == "array") and
  ([.members[].name] | sort) == $expected and
  ([.members[] | select(.isLearner == true)] | length) == 0
' >/dev/null
"${etcdctl_command[@]}" endpoint health --cluster >/dev/null
