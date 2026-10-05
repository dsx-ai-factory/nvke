---
type: result
scenario: ../../workloads/scalability/node-density/scenario.yaml
workload: scalability
k8s_version: 1.34.2
date: 2026-09-21
authors: [NVIDIA NKE Scale Team]
---

# node-density — k8s 1.34.2

Scenario: [node-density](../../workloads/scalability/node-density/README.md)

## TL;DR

4,120 iterations filled 42 workers to an average of 105 pods per node, creating 4,120 pause pods in
212 s. With no probe and no Service connectivity gating readiness, the headline figure
is a pod-startup floor: **pod-ready p99 of 2 s**. Peak Pending was 23 pods, every one
of the 4,120 pods reached Ready, and latency coverage was complete. The calculated
iteration count equals the observed iteration count.

The engine reported **`passed: true`** with no execution errors. Measured directly,
the slowest pod this workload created reached Ready in 2 s.

| | Outcome |
|---|---|
| Pod-ready p99 vs the 15 s bar | **Met** — 2 s |
| Latency measurement coverage | **Complete** — 4,120 of 4,120 pods |
| Scheduling headroom | **Held** — peak Pending 23, every pod reached Ready |
| Slowest pod this workload created | **2 s** |
| Prometheus alerting | **`passed: true`** — no `error` rule fired |

**Not a maximum-density claim.** See
[What is tested](../../workloads/scalability/node-density/README.md#what-is-tested)
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
| CNI | OVN-Kubernetes, Helm chart 1.4.0 (app 2026-02.0) |
| OVN | 25.09.1 |
| Open vSwitch | 3.6.1 |
| CoreDNS | 1.12.1 |
| etcd | 3.6.5 |
| kube-proxy mode | n/a — not installed |
| Engine | kube-burner 2.5.0 |

## Results summary

| Size | Scenario | Duration (s) | Pod ready p50 (ms) | Pod ready p95 (ms) | Pod ready p99 (ms) | Pod scheduled p99 (ms) | Containers ready p99 (ms) | Containers started p99 (ms) |
|------|----------|--------------|--------------------|--------------------|--------------------|------------------------|---------------------------|-----------------------------|
| L | 105 pods/node across 42 workers, 4120 iterations | 212 | 1000 | 2000 | 2000 | 895 | 2000 | 1562 |

## Per-size detail

### L
```yaml
size: L
scenario: 105 pods/node across 42 workers, 4120 iterations
duration_s: 212
pod_ready_p50_ms: 1000
pod_ready_p95_ms: 2000
pod_ready_p99_ms: 2000
pod_scheduled_p99_ms: 895
containers_ready_p99_ms: 2000
containers_started_p99_ms: 1562
pods_per_node: 105
nodes_targeted: 42
existing_pods_at_derivation: 290
iterations: 4120
pods_created: 4120
qps: 20
burst: 20
pod_ready_threshold_s: 15
max_apiserver_cpu_cores: 1.67
max_apiserver_memory_gib: 12.55
max_etcd_cpu_cores: 0.49
max_etcd_memory_mib: 433
max_readonly_inflight: 5
max_mutating_inflight: 9
etcd_db_in_use_mib: 144
etcd_wal_fsync_p99_ms: 0.99
etcd_backend_commit_p99_ms: 0.99
peak_pending_pods: 23
```

**Observations:** The calculated iteration count equals the observed iteration count —
42 workers x 105 pods = 4,410, minus the 290 pods already resident, is 4,120, and the
run's configuration carried `jobIterations: 4120`. The pod latency measurement
produced 4,120 documents, one per iteration, spread across all 42 targeted nodes, so
the quantiles cover the whole run. Every one of those pods reached Ready, with a
measured maximum of 2 s. Control-plane headroom was ample throughout: read-only
inflight peaked at 5 and mutating at 9 across all three API servers, well under one
server's APF limit of 600, and etcd disk
latency stayed an order of magnitude inside conventional bars. Because the derivation
subtracts pods already resident, the iteration count is a property of the cluster at
the moment the run started, not a fixed number for this scenario.

## Scope of this publication

This result covers one density point on one cluster: 105 pods per node, below that
cluster's max-pods setting of 110.

## Gate outcome

The engine reported `passed: true` and `executionErrors` was empty. The flag combines
two things, and both were satisfied: the configured pod-ready p99 bar of 15 s against
a measured 2 s, and no `severity: error` alerting rule firing in the run window.

That bar is not a timeout. The engine computes the pod-ready quantiles when the run
finishes and compares the p99 against it — nothing is cancelled, and no pod is affected
by the comparison. The pause pod this workload creates carries no probe and no
deadline, so nothing in the workload can fail a pod on a timer.

## Reading the numbers

Pod-ready values are whole seconds, because the Ready condition timestamp is
second-granular: p95, p99 and the maximum all read 2 s, meaningful to +/- 1 s.
Container-start latency has sub-second precision — 1,562 ms at p99 — and is the
better figure to compare between runs.

Pod-scheduled values are imprecise. 91% of pods read 0 ms, a resolution floor rather
than a measurement, which is why the p50 is 0; the 895 ms p99 comes from the rest and
reads up to 1 s high. See
[How it is tested](../../workloads/scalability/node-density/README.md#how-it-is-tested).

Peak Pending was 23 pods, about 0.6% of those created, and nothing was backlogged:
every pod reached Ready. The Pending series ends at its peak only because sampling
stops with the job.
