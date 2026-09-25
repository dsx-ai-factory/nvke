#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/../.."

start_container() {
  local name=$1
  shift
  if docker container inspect "$name" >/dev/null 2>&1; then
    if [[ $(docker inspect --format '{{index .Config.Labels "cwe.tracing-poc"}}' "$name") != true ]]; then
      echo "Refusing to use unrelated container $name" >&2
      exit 1
    fi
    docker start "$name" >/dev/null
  else
    docker run -d --name "$name" --label cwe.tracing-poc=true "$@"
  fi
}

case "${1:-test}" in
  up)
    start_container cwe-tracing-poc-postgres -p 127.0.0.1:15432:5432 \
      -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=cwe_tracing_poc postgres:17.4
    start_container cwe-tracing-poc-temporal -p 127.0.0.1:17233:7233 -p 127.0.0.1:18233:8233 \
      temporalio/temporal:1.4.1 server start-dev --ip 0.0.0.0 --ui-ip 0.0.0.0
    start_container cwe-tracing-poc-jaeger -p 127.0.0.1:14317:4317 -p 127.0.0.1:16686:16686 \
      cr.jaegertracing.io/jaegertracing/jaeger:2.10.0
    ready=false
    for ((i=0; i<60; i++)); do
      if docker exec cwe-tracing-poc-postgres pg_isready -U postgres -d cwe_tracing_poc >/dev/null 2>&1 && \
         docker exec cwe-tracing-poc-temporal temporal operator cluster health --address 127.0.0.1:7233 >/dev/null 2>&1 && \
         curl -fsS http://127.0.0.1:16686/ >/dev/null; then
        ready=true
        break
      fi
      sleep 1
    done
    [[ $ready == true ]] || { echo 'POC services did not become ready' >&2; exit 1; }
    echo 'Ready: Jaeger http://localhost:16686 | Temporal http://localhost:18233'
    ;;
  test)
    mkdir -p .local/tracing-poc
    export POC_LOG_PATH="$PWD/.local/tracing-poc/logs.jsonl"
    export GOTOOLCHAIN="go$(awk '/^go / {print $2; exit}' ../go.work)"
    GOPROXY=off go test -tags tracing_poc ./internal/temporal -run '^TestTracingPOC$' -count=1 -v -timeout=3m
    ;;
  stop)
    for name in cwe-tracing-poc-postgres cwe-tracing-poc-temporal cwe-tracing-poc-jaeger; do
      if [[ $(docker inspect --format '{{index .Config.Labels "cwe.tracing-poc"}}' "$name" 2>/dev/null) == true ]]; then
        docker stop "$name"
      fi
    done
    ;;
  *) echo "Usage: bash $0 {up|test|stop}" >&2; exit 1 ;;
esac
