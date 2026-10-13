#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  hack/bgp-containers/capture-cwe-delete.sh start RESULT_DIR TARGET_CP WORKER_NODE WORKER_IP VIP [PORT] [TOR_CONTAINER]
  hack/bgp-containers/capture-cwe-delete.sh stop RESULT_DIR

Starts the capture set needed before triggering a controller node deletion:
  - tcpdump -U -l on every capd-cp-* container and the worker
  - /var/log/nvke-execute-scripts.log on the target CP
  - nvke-agent.service journal on the target CP
  - kubelet and kube-proxy logs on the target CP and worker
  - worker and CP socket state monitors
  - FRR VIP route monitor

The script writes PID metadata to RESULT_DIR/capture-pids.txt. Run stop after
the test to clean up local docker exec wrappers and tcpdump processes inside
the CAPD containers.
EOF
}

timestamp() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

start_job() {
  local name="$1"
  local output_file="$2"
  shift 2

  (
    trap '' HUP
    echo "$(timestamp) START ${name}"
    exec "$@"
  ) >"${output_file}" 2>&1 &

  local pid=$!
  echo "${pid} ${name} ${output_file}" >>"${PID_FILE}"
}

require_container() {
  local container="$1"
  docker inspect "${container}" >/dev/null 2>&1 || die "container not found: ${container}"
}

require_cmd() {
  local container="$1"
  local command="$2"
  docker exec "${container}" sh -c "command -v ${command} >/dev/null 2>&1" || die "${command} missing in ${container}"
}

require_file() {
  local container="$1"
  local path="$2"
  docker exec "${container}" test -f "${path}" || die "${path} missing in ${container}"
}

discover_cp_nodes() {
  docker ps --format '{{.Names}}' | grep -E '(^|-)cp-[0-9]+$' | sort
}

stop_capture() {
  local result_dir="$1"
  local pid_file="${result_dir}/capture-pids.txt"
  local cp_nodes_file="${result_dir}/cp-nodes.txt"
  local worker_file="${result_dir}/worker-node.txt"

  if [[ -f "${pid_file}" ]]; then
    while read -r pid _; do
      [[ -n "${pid}" ]] || continue
      kill "${pid}" >/dev/null 2>&1 || true
    done <"${pid_file}"
  fi

  if [[ -f "${worker_file}" ]]; then
    local worker
    worker="$(cat "${worker_file}")"
    docker exec "${worker}" sh -c 'pkill -f "tcpdump -U -l -i any -nn -tttt" 2>/dev/null || true' >/dev/null 2>&1 || true
  fi

  if [[ -f "${cp_nodes_file}" ]]; then
    while read -r cp; do
      [[ -n "${cp}" ]] || continue
      docker exec "${cp}" sh -c 'pkill -f "tcpdump -U -l -i any -nn -tttt" 2>/dev/null || true' >/dev/null 2>&1 || true
    done <"${cp_nodes_file}"
  fi
}

verify_jobs_alive() {
  local failed=0

  sleep 2

  while read -r pid name output_file; do
    [[ -n "${pid}" ]] || continue
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      echo "capture exited early: ${name} (${output_file})" >&2
      failed=1
    fi
  done <"${PID_FILE}"

  if [[ "${failed}" -ne 0 ]]; then
    stop_capture "${RESULT_DIR}"
    die "one or more captures failed to stay running"
  fi
}

start_capture() {
  RESULT_DIR="$1"
  local target_cp="$2"
  local worker_node="$3"
  local worker_ip="$4"
  local vip="$5"
  local port="${6:-6443}"
  local tor_container="${7:-bgp-containers}"
  PID_FILE="${RESULT_DIR}/capture-pids.txt"

  mkdir -p "${RESULT_DIR}"
  : >"${PID_FILE}"

  require_container "${target_cp}"
  require_container "${worker_node}"
  require_container "${tor_container}"

  local cp_nodes
  cp_nodes="$(discover_cp_nodes)"
  [[ -n "${cp_nodes}" ]] || die "no capd-cp-* containers found"

  echo "${cp_nodes}" >"${RESULT_DIR}/cp-nodes.txt"
  echo "${worker_node}" >"${RESULT_DIR}/worker-node.txt"

  require_cmd "${worker_node}" tcpdump
  require_cmd "${worker_node}" journalctl
  require_cmd "${worker_node}" tail
  require_cmd "${worker_node}" ss
  docker exec "${worker_node}" sh -c 'ls /var/log/containers/kube-proxy-*_kube-system_kube-proxy-*.log >/dev/null 2>&1' \
    || die "kube-proxy container log not found in ${worker_node}"

  while read -r cp; do
    [[ -n "${cp}" ]] || continue
    require_container "${cp}"
    require_cmd "${cp}" tcpdump
    require_cmd "${cp}" journalctl
    require_cmd "${cp}" tail
    require_cmd "${cp}" ss
  done <"${RESULT_DIR}/cp-nodes.txt"

  require_file "${target_cp}" /var/log/nvke-execute-scripts.log
  docker exec "${target_cp}" systemctl is-active --quiet nvke-agent.service \
    || die "nvke-agent.service is not active in ${target_cp}"
  docker exec "${target_cp}" sh -c 'ls /var/log/containers/kube-proxy-*_kube-system_kube-proxy-*.log >/dev/null 2>&1' \
    || die "kube-proxy container log not found in ${target_cp}"

  start_job worker-tcpdump "${RESULT_DIR}/worker-tcpdump.txt" \
    docker exec "${worker_node}" tcpdump -U -l -i any -nn -tttt "host ${vip} and tcp port ${port}"

  while read -r cp; do
    [[ -n "${cp}" ]] || continue
    start_job "${cp}-tcpdump" "${RESULT_DIR}/${cp}-tcpdump.txt" \
      docker exec "${cp}" tcpdump -U -l -i any -nn -tttt "host ${worker_ip} and host ${vip} and tcp port ${port}"
    start_job "${cp}-ss" "${RESULT_DIR}/${cp}-ss-live.txt" \
      docker exec "${cp}" sh -c "while true; do date -u +%Y-%m-%dT%H:%M:%SZ; ss -Htoni state established '( sport = :${port} and dst = ${worker_ip} )'; sleep 1; done"
  done <"${RESULT_DIR}/cp-nodes.txt"

  start_job target-nvke-execute-scripts "${RESULT_DIR}/target-nvke-execute-scripts-live.log" \
    docker exec "${target_cp}" tail -n0 -F /var/log/nvke-execute-scripts.log

  start_job target-nvke-agent-journal "${RESULT_DIR}/target-nvke-agent-journal-live.log" \
    docker exec "${target_cp}" journalctl -u nvke-agent.service -f --since now --no-pager -o short-iso-precise

  start_job target-kubelet "${RESULT_DIR}/target-kubelet-live.log" \
    docker exec "${target_cp}" journalctl -u kubelet.service -f --since now --no-pager -o short-iso-precise

  start_job target-kube-proxy "${RESULT_DIR}/target-kube-proxy-live.log" \
    docker exec "${target_cp}" sh -c 'tail -n0 -F /var/log/containers/kube-proxy-*_kube-system_kube-proxy-*.log'

  start_job worker-kubelet "${RESULT_DIR}/worker-kubelet-live.log" \
    docker exec "${worker_node}" journalctl -u kubelet.service -f --since now --no-pager -o short-iso-precise

  start_job worker-kube-proxy "${RESULT_DIR}/worker-kube-proxy-live.log" \
    docker exec "${worker_node}" sh -c 'tail -n0 -F /var/log/containers/kube-proxy-*_kube-system_kube-proxy-*.log'

  start_job worker-ss "${RESULT_DIR}/worker-ss-live.txt" \
    docker exec "${worker_node}" sh -c "while true; do date -u +%Y-%m-%dT%H:%M:%SZ; ss -Htanp state established '( dst = ${vip} and dport = :${port} )'; sleep 1; done"

  start_job frr-route "${RESULT_DIR}/frr-route-live.txt" \
    docker exec "${tor_container}" sh -c "while true; do date -u +%Y-%m-%dT%H:%M:%SZ; vtysh -c 'show bgp ipv4 unicast ${vip}/32'; sleep 1; done"

  verify_jobs_alive

  echo "$(timestamp) capture set ready"
  echo "result_dir=${RESULT_DIR}"

  if [[ "${CAPTURE_HOLD:-}" == "1" ]]; then
    trap 'stop_capture "${RESULT_DIR}"; exit 0' INT TERM
    while true; do
      sleep 3600
    done
  fi
}

main() {
  local action="${1:-}"
  case "${action}" in
    start)
      [[ "$#" -ge 6 ]] || {
        usage
        exit 2
      }
      shift
      start_capture "$@"
      ;;
    stop)
      [[ "$#" -eq 2 ]] || {
        usage
        exit 2
      }
      stop_capture "$2"
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

main "$@"
