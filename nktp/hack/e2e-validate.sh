#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Installs the NKTP chart on a Kind cluster and asserts that telemetry actually
# flows, rather than only that the chart renders.
#
# The merge gate lints, renders and kubeconforms the chart. All of that passes on
# a chart that installs and then sends nothing, which is the failure this script
# exists to catch: a Collector that crash-loops, an exporter that cannot reach the
# backend, a Target Allocator that discovers nothing.
#
# The assertions are the ones docs/nktp/kind-validation.md lists, run against the
# bundled development backend. They are queries, not `kubectl get`: a Collector
# can be Ready and still export nothing.
#
# Usage:
#   nktp/hack/e2e-validate.sh              # create a cluster, validate, delete it
#   KEEP_CLUSTER=1 nktp/hack/e2e-validate.sh   # leave it up to poke at
#   REUSE_CLUSTER=1 nktp/hack/e2e-validate.sh  # use the current kube context
#
# Needs kind, kubectl, helm and jq. The whole thing is one Kind node, but not a
# small one: the chart requests about 2 CPU across its pods, so a 4-vCPU host
# leaves several Collectors Pending on "Insufficient cpu".

set -euo pipefail

CLUSTER="${CLUSTER:-nktp-e2e}"
NAMESPACE="${NAMESPACE:-nktp}"
CHART="${CHART:-nktp/deploy/charts/nktp}"
VALUES="${VALUES:-nktp/hack/e2e-values.yaml}"
# Pinned in make/tool-versions.mk: the version clusterctl init installs on a site.
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-$(make --no-print-directory print-CERT_MANAGER_VERSION)}"
KIND_CONFIG="${KIND_CONFIG:-controller/hack/cwe-capi-kind.yaml}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
REUSE_CLUSTER="${REUSE_CLUSTER:-0}"
# How long a signal gets to show up. Logs and traces are batched, so the first
# query after install legitimately returns nothing.
SIGNAL_TIMEOUT="${SIGNAL_TIMEOUT:-300}"

failures=0
pf_pid=""

log()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
pass() { printf '    \033[32mPASS\033[0m  %s\n' "$*"; }
fail() { printf '    \033[31mFAIL\033[0m  %s\n' "$*"; failures=$((failures + 1)); }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }

for tool in kind kubectl helm jq curl make; do
  command -v "$tool" >/dev/null || die "$tool is required"
done

cleanup() {
  local rc=$?
  [ -n "$pf_pid" ] && kill "$pf_pid" 2>/dev/null || true
  if [ "$rc" != 0 ] || [ "$failures" != 0 ]; then
    log "Diagnostics"
    kubectl -n "$NAMESPACE" get pods -o wide 2>/dev/null || true
    kubectl -n "$NAMESPACE" get opentelemetrycollectors 2>/dev/null || true
    kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>/dev/null | tail -30 || true
  fi
  if [ "$REUSE_CLUSTER" = 0 ] && [ "$KEEP_CLUSTER" = 0 ]; then
    log "Deleting cluster ${CLUSTER}"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- the cluster

if [ "$REUSE_CLUSTER" = 0 ]; then
  log "Creating Kind cluster ${CLUSTER}"
  # The same node config the controller's own e2e uses, so this validates the
  # chart on the shape a site controller actually runs on.
  kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --wait 300s
fi

kubectl cluster-info >/dev/null || die "no reachable cluster"

# ---------------------------------------------------------------- the install

log "Installing the chart"
# `make nktp-chart-deps` rather than a bare `helm dependency build`: it derives
# the repository list from Chart.yaml, so adding a dependency needs no change
# here, and it registers them in a temporary HELM_REPOSITORY_CONFIG so running
# this does not edit the caller's helm repo list.
make --no-print-directory nktp-chart-deps

# The chart's certificates come from cert-manager: the Operator's webhook
# certificate, and the mTLS between each Collector and its Target Allocator.
helm upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --version "$CERT_MANAGER_VERSION" \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --wait --timeout 5m

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
# The node Collector reads /var/log/pods and /proc from the host, so
# the namespace has to admit a privileged pod or its DaemonSet never starts.
kubectl label namespace "$NAMESPACE" \
  pod-security.kubernetes.io/enforce=privileged --overwrite

# The chart's defaults are sized for a multi-node cluster. VALUES sizes it for
# one Kind node and turns on metrics-server for Kind; see that file.
helm install nktp "$CHART" \
  --namespace "$NAMESPACE" \
  --wait --timeout 15m \
  --values "$VALUES"

log "Waiting for the Operator-built Collectors"
# helm --wait covers the Helm-managed workloads. The Operator builds the
# Collectors after that, so they need their own wait.
kubectl -n "$NAMESPACE" wait --for=condition=Ready pod --all --timeout=10m

# ------------------------------------------------------------ the backend port

log "Port-forwarding the development backend"
kubectl -n "$NAMESPACE" port-forward deploy/lgtm 3000:3000 9090:9090 3100:3100 3200:3200 \
  >/dev/null 2>&1 &
pf_pid=$!

for _ in $(seq 1 30); do
  if curl -fsS 'http://127.0.0.1:9090/-/ready' >/dev/null 2>&1; then break; fi
  sleep 2
done
curl -fsS 'http://127.0.0.1:9090/-/ready' >/dev/null 2>&1 ||
  die "Prometheus in the lgtm pod never became ready"

# --------------------------------------------------------------- the assertions
#
# Every signal assertion retries. A Collector exports on a batch timer, so an
# immediate query says nothing about whether the pipeline works.

# retry_until <description> <command...>
retry_until() {
  local what=$1; shift
  local deadline=$((SECONDS + SIGNAL_TIMEOUT))
  while :; do
    if "$@" >/dev/null 2>&1; then pass "$what"; return 0; fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "$what (still failing after ${SIGNAL_TIMEOUT}s)"
      return 1
    fi
    sleep 10
  done
}

# promql_nonempty <query> -- succeeds when the query returns at least one series
promql_nonempty() {
  local n
  n=$(curl -fsS --get 'http://127.0.0.1:9090/api/v1/query' \
        --data-urlencode "query=$1" 2>/dev/null |
        jq -r '.data.result | length' 2>/dev/null) || return 1
  [ "${n:-0}" -gt 0 ]
}

# promql_empty <query> -- succeeds when the query returns nothing
promql_empty() {
  local n
  n=$(curl -fsS --get 'http://127.0.0.1:9090/api/v1/query' \
        --data-urlencode "query=$1" 2>/dev/null |
        jq -r '.data.result | length' 2>/dev/null) || return 1
  [ "${n:-0}" -eq 0 ]
}

# logql_nonempty <query> -- Loki, over the last 15 minutes
logql_nonempty() {
  local n
  n=$(curl -fsS --get 'http://127.0.0.1:3100/loki/api/v1/query' \
        --data-urlencode "query=$1" 2>/dev/null |
        jq -r '.data.result | length' 2>/dev/null) || return 1
  [ "${n:-0}" -gt 0 ]
}

log "Asserting the signals"

# Every scrape job is up. min() catches one target down among many, which a
# plain up==1 query would hide.
retry_until "scrape targets are up" \
  promql_nonempty 'min by (job) (up{job=~"kubelet|kubelet-cadvisor|nktp-operator|node-exporter|.*-targetallocator|nktp/nktp-ksm-.*"}) == 1'

# The inverse assertion: no exporter is failing to send. This is the one that
# catches a backend the gateways cannot reach.
retry_until "no exporter send failures" \
  promql_empty 'sum by (service_name) ({__name__=~"otelcol_exporter_send_failed.*"}) > 0'

# The API server and CoreDNS are scraped through the kube-stack monitors the
# large-target pool selects. The scheduler, controller manager, etcd and
# kube-proxy serve metrics on 127.0.0.1 on Kind, so their targets are down here.
retry_until "API server metrics are scraped" \
  promql_nonempty 'up{job="apiserver"} == 1'
retry_until "CoreDNS metrics are scraped" \
  promql_nonempty 'up{job="coredns"} == 1'

# kube-state-metrics object state arrives through the large-target pool.
retry_until "object-state metrics arrive" \
  promql_nonempty 'count(kube_pod_info) > 0 and count(kube_deployment_status_replicas_available) > 0'

# Pod logs reach Loki through the node Collector's filelog receiver.
retry_until "pod logs reach Loki" \
  logql_nonempty 'sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~"kube-system|nktp"}[15m]))'

# The node's systemd journal reaches Loki through the node Collector's journald
# receiver, which runs the node's own journalctl.
retry_until "node journal logs reach Loki" \
  logql_nonempty 'sum(count_over_time({service_name="journald"}[15m]))'

# Kubernetes events reach Loki through the cluster-events Collector.
retry_until "Kubernetes events reach Loki" \
  logql_nonempty 'sum(count_over_time({service_name="unknown_service"} | k8s_event_reason=~".+" [15m]))'

# The Collectors' own traces reach Tempo.
tempo_has_traces() {
  local n
  n=$(curl -fsS --get 'http://127.0.0.1:3200/api/search' \
        --data-urlencode 'q={ resource.service.name =~ "nktp-.*" }' 2>/dev/null |
        jq -r '.traces | length' 2>/dev/null) || return 1
  [ "${n:-0}" -gt 0 ]
}
retry_until "Collector traces reach Tempo" tempo_has_traces

# metrics-server serves metrics.k8s.io, so the autoscalers can read utilisation.
# An HPA with no currentMetrics means the API is there but empty.
hpas_have_metrics() {
  kubectl -n "$NAMESPACE" get hpa -o json |
    jq -e '[.items[] | select((.status.currentMetrics // []) | length == 0)] | length == 0'
}
retry_until "every HPA reports metrics" hpas_have_metrics

# metrics-server itself, independently of the HPAs.
retry_until "metrics-server serves metrics.k8s.io" \
  kubectl top nodes

# ------------------------------------------------------------------- the report

log "Result"
if [ "$failures" -eq 0 ]; then
  info "all assertions passed"
else
  info "${failures} assertion(s) failed"
  exit 1
fi
