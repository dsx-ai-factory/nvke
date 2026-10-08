---
type: result
scenario: ../../workloads/scalability/cluster-density/scenario.yaml
workload: scalability
k8s_version: 1.34.2
date: 2026-09-18
authors: [NVIDIA NKE Scale Team]
---

# cluster-density — k8s 1.34.2

Scenario: [cluster-density](../../workloads/scalability/cluster-density/README.md)

## TL;DR

378 iterations created 3,780 pods across 378 namespaces on 42 workers in 710 s,
alongside 12,474 other namespaced objects — 33 per iteration. **Pod-ready p99 was
2 s against the scenario's 13 s bar.** Peak Pending never exceeded 10 pods, so the
cluster absorbed the creation burst without a scheduling backlog, and the control plane
stayed far from saturation — read-only inflight peaked at 18 and mutating at 36
across all three API servers, well under one server's APF limit of 600.

This is creation pressure, not churn: the engine's churn feature, which deletes and
recreates a proportion of namespaces mid-run, was disabled for this run.

The engine reported **`passed: true`** with no execution errors and no alerting rule
firing at any severity.

| | Outcome |
|---|---|
| Pod-ready p99 vs the 13 s bar | **Met** — 2 s |
| Latency measurement coverage | **Complete** — 3,780 of 3,780 pods |
| Scheduling headroom | **Held** — peak Pending 10 |
| etcd disk bars | **Met** — WAL fsync 0.99 ms, commit 3.07 ms |
| Slowest pod this workload created | **3 s** |
| Prometheus alerting | **`passed: true`** — no rule fired |

**Not a maximum-density claim.** See
[What is tested](../../workloads/scalability/cluster-density/README.md#what-is-tested)
in the scenario README.

## Environment

| Field | Value |
|-------|-------|
| Nodes | 47 — 3 control-plane, 2 monitoring, 42 worker |
| Worker node | 128 cores, ~1007 GiB memory |
| Worker kubelet max-pods | 110 |
| Nodes targeted | 42, selected explicitly by worker role |

## Software configuration

| Component | Version |
|-----------|---------|
| Kubernetes | 1.34.2 |
| Containerd | 1.7.28 |
| OS image | Ubuntu 22.04.4 LTS |
| Kernel | 5.15.0-113-generic |
| CNI | OVN-Kubernetes, downstream build of upstream commit d5f19be |
| OVN | 25.09 |
| Open vSwitch | 2.17 |
| CoreDNS | 1.12.1 |
| etcd | 3.6.5 |
| kube-proxy mode | n/a — not installed |
| Engine | kube-burner 2.5.0 |

## Results summary

| Size | Scenario | Duration (s) | Pod ready p50 (ms) | Pod ready p95 (ms) | Pod ready p99 (ms) | Pod scheduled p99 (ms) | Containers ready p99 (ms) | Svc ready p99 (ms) |
|------|----------|--------------|--------------------|--------------------|--------------------|------------------------|---------------------------|--------------------|
| L | 378 iterations, 10 pods each, 42 workers | 710 | 1000 | 2000 | 2000 | 975 | 2000 | 2971.3 |

## Per-size detail

### L
```yaml
size: L
scenario: 378 iterations, 10 pods each, 42 workers
duration_s: 710
pod_ready_p50_ms: 1000
pod_ready_p95_ms: 2000
pod_ready_p99_ms: 2000
pod_scheduled_p99_ms: 975
containers_ready_p99_ms: 2000
svc_ready_p99_ms: 2971.3
iterations: 378
pods_created: 3780
namespaces_created: 378
qps: 20
burst: 20
pod_ready_threshold_s: 120
max_apiserver_cpu_cores: 2.62
max_apiserver_memory_gib: 14.22
max_etcd_cpu_cores: 1.06
max_etcd_memory_mib: 563
max_readonly_inflight: 18
max_mutating_inflight: 36
etcd_db_in_use_mib: 467
etcd_wal_fsync_p99_ms: 0.99
etcd_backend_commit_p99_ms: 3.07
peak_pending_pods: 10
```

**Observations:** Pod-ready p99 sits far inside the scenario's bar, and the supporting
signals agree that the cluster was not under pressure — etcd WAL fsync p99 of 0.99 ms
against a 10 ms bar, backend commit 3.07 ms against 30 ms, and inflight request counts
an order of magnitude below the API Priority and Fairness limit. The pod latency measurement produced exactly
3,780 documents, one per pod created, so the quantiles cover the whole run rather than
a subset.

Service-ready latency is the one figure with a visible tail: p50 1,124 ms and p95
1,192 ms, but p99 2,971 ms and a slowest of 6,064 ms. An earlier execution of this same
scenario on the same cluster recorded a p99 of 1,288 ms with a slowest of 1,424 ms, on a
p50 and p95 within a few percent of these. The bulk of Service programming is therefore
stable run to run and the tail is not — worth knowing before treating a single
`svc_ready_p99_ms` as characteristic. Both runs sit inside the 10 s `svcTimeout` this
workload allows, and the scenario declares no threshold on this metric.

## Scope of this publication

This result covers one density point on one cluster. Two limits on what the pod-ready
figure means are set out in the
[scenario README](../../workloads/scalability/cluster-density/README.md) and apply to
any run of it: NetworkPolicy programming latency is not measured, and the client pods'
readiness probe puts some Service-programming time inside the figure.

## Gate outcome

The engine reported `passed: true`, `executionErrors` was empty, and no alerting rule
fired at any severity. The flag combines the configured latency threshold with the
alerting rules loaded from the alerts file; the configured pod-ready p99 bar was 2 minutes against a
measured 2 s.

That bar is not a timeout. The engine computes the pod-ready quantiles when the run
finishes and compares the p99 against it — nothing is cancelled, and no pod is affected
by the comparison.

## Reading the numbers

The Ready-family quantiles are quantized to whole seconds, because they derive from pod
condition timestamps that are second-granularity in the Kubernetes API. Container-start
latency and service-ready latency come from different measurements and do carry
sub-second precision.
