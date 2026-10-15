#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -e
umask 077
MAX_ATTEMPTS=12
printf '%s' "$SPIRE_KUBECONFIG" > /work/kubeconfig
set +e
attempt=0
minted=no
status=none
command_status=1
failure=none
while [ "$attempt" -lt "$MAX_ATTEMPTS" ]; do
  attempt=$((attempt + 1))
  minted=no
  status=none
  command_status=1
  token=
  failure=none
  if [ ! -S /spiffe-workload-api/spire-agent.sock ]; then
    failure=socket_missing
  elif [ ! -x "$PLUGIN_INSTALL_PATH" ]; then
    failure=plugin_missing
  else
    SPIFFE_ENDPOINT_SOCKET=unix:///spiffe-workload-api/spire-agent.sock SPIFFE_JWT_AUDIENCE="$CLUSTER" \
      "$PLUGIN_INSTALL_PATH" -timeout 5s >/work/credential.yaml 2>/work/mint.log
    command_status=$?
    if [ "$command_status" -ne 0 ]; then
      failure=mint_failed
    else
      token=$(awk '$1 == "token:" { gsub(/^"|"$/, "", $2); print $2; exit }' /work/credential.yaml)
      [ -n "$token" ] || failure=empty_token
    fi
  fi
  if [ "$failure" != none ]; then
    [ "$attempt" -lt "$MAX_ATTEMPTS" ] && sleep 5
    continue
  fi
  kubectl --kubeconfig /work/kubeconfig --token="$token" get --raw=/api -v=8 >/dev/null 2>/work/request.log
  command_status=$?
  minted=no
  status=none
  grep -Eiq 'Authorization.*[Bb]earer <masked>' /work/request.log && minted=yes
  parsed_status=$(sed -n 's/.*status="\([0-9][0-9][0-9]\)[^"]*".*/\1/p' /work/request.log | tail -n 1)
  [ -n "$parsed_status" ] && status="$parsed_status"
  if [ "$minted" = yes ]; then
    case "$status" in
      200|401|403) break ;;
    esac
  fi
  failure=request_failed
  [ "$attempt" -lt "$MAX_ATTEMPTS" ] && sleep 5
done
printf 'MINTED %s\nHTTP %s\nCOMMAND %s\nFAILURE %s\n' "$minted" "$status" "$command_status" "$failure" > /dev/termination-log
exit 0
